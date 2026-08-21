##############################################################################
# SOUQ — production, eu-west-1
#
#   terraform init && terraform plan
#
# Read infra/terraform/README.md before applying. Two things about this file
# are deliberate and easy to get wrong:
#
#   1. State is remote with DynamoDB locking. Two engineers applying at once
#      against an EKS cluster produces a half-migrated control plane that is
#      genuinely hard to recover.
#   2. `is_production = true` flips ~20 availability/cost decisions together
#      (multi-AZ, replicas, deletion protection, backup retention, dedicated
#      masters, storage autoscaling). They are not independent choices, so
#      they are not independent variables.
##############################################################################

terraform {
  required_version = ">= 1.9"

  required_providers {
    aws        = { source = "hashicorp/aws", version = "~> 5.60" }
    kubernetes = { source = "hashicorp/kubernetes", version = "~> 2.32" }
    helm       = { source = "hashicorp/helm", version = "~> 2.15" }
    random     = { source = "hashicorp/random", version = "~> 3.6" }
  }

  backend "s3" {
    bucket         = "souq-terraform-state-prod"
    key            = "prod/terraform.tfstate"
    region         = "eu-west-1"
    dynamodb_table = "souq-terraform-locks"
    encrypt        = true
    kms_key_id     = "alias/souq-terraform-state"
  }
}

locals {
  name   = "souq-prod"
  region = "eu-west-1"

  tags = {
    Project     = "souq"
    Environment = "production"
    ManagedBy   = "terraform"
    Repository  = "github.com/souq/platform"
    # Drives the cost dashboard. Untagged spend is spend nobody owns.
    CostCenter = "commerce-platform"
  }
}

provider "aws" {
  region = local.region

  default_tags {
    tags = local.tags
  }

  # Guard against a misconfigured profile applying prod into the wrong
  # account. This has happened to everyone once.
  allowed_account_ids = [var.account_id]
}

# CloudFront certificates and WAF for a global distribution must live in
# us-east-1 regardless of where everything else is.
provider "aws" {
  alias  = "us_east_1"
  region = "us-east-1"

  default_tags {

    tags = local.tags

  }
  allowed_account_ids = [var.account_id]
}

provider "kubernetes" {
  host                   = module.eks.cluster_endpoint
  cluster_ca_certificate = base64decode(module.eks.cluster_ca_certificate)

  exec {
    api_version = "client.authentication.k8s.io/v1beta1"
    command     = "aws"
    args        = ["eks", "get-token", "--cluster-name", module.eks.cluster_name, "--region", local.region]
  }
}

provider "helm" {
  kubernetes {
    host                   = module.eks.cluster_endpoint
    cluster_ca_certificate = base64decode(module.eks.cluster_ca_certificate)

    exec {
      api_version = "client.authentication.k8s.io/v1beta1"
      command     = "aws"
      args        = ["eks", "get-token", "--cluster-name", module.eks.cluster_name, "--region", local.region]
    }
  }
}

data "aws_caller_identity" "current" {}

##############################################################################
# Encryption
#
# One CMK for the data plane. A single key means one rotation schedule, one
# grant policy, and one place to look when an audit asks who can decrypt
# customer data. Separate keys per store sound stricter but in practice
# produce a sprawl of policies nobody reviews.
##############################################################################

resource "aws_kms_key" "data" {
  description             = "SOUQ data-at-rest encryption"
  enable_key_rotation     = true
  deletion_window_in_days = 30
  multi_region            = true # so the DR region can decrypt replicated snapshots

  tags = merge(local.tags, { Name = "${local.name}-data" })
}

resource "aws_kms_alias" "data" {
  name          = "alias/${local.name}-data"
  target_key_id = aws_kms_key.data.key_id
}

##############################################################################
# JWT signing key
#
# ASYMMETRIC and separate from the data CMK, for a reason that is not
# fastidiousness: a symmetric key encrypts, an asymmetric signing key signs, and
# KMS will not let one do the other. More to the point, the two have opposite
# exposure profiles — the data key's ciphertext is private, whereas this key's
# public half is deliberately published at /v1/.well-known/jwks.json so that
# every service in the platform can verify locally without an auth round trip.
#
# The private half never leaves KMS. identity-service signs by API call, so a
# container escape or a heap dump yields the ability to ask KMS to sign while
# the pod lives — not the key itself. Recovery is revoking an IAM role rather
# than rotating a secret somebody already has.
#
# RSA_2048 rather than ECC, and RS256 rather than ES256: KMS returns an ECDSA
# signature in DER, while JWS wants the concatenated (R||S) form, so an EC key
# would need a conversion step in the signing path. RSA_PKCS1_V1_5 needs none —
# what KMS returns is byte-for-byte what JWS expects.
#
# enable_key_rotation is deliberately ABSENT. AWS cannot rotate an asymmetric
# key in place, and setting it here is silently ignored. Rotation means creating
# a second key, publishing both in the JWKS, switching the signer, and removing
# the old one only after every token it signed has expired — see
# docs/runbooks/jwt-key-rotation.md.
##############################################################################

resource "aws_kms_key" "jwt_signing" {
  description              = "SOUQ JWT signing (RS256). Private half never leaves KMS."
  key_usage                = "SIGN_VERIFY"
  customer_master_key_spec = "RSA_2048"
  deletion_window_in_days  = 30

  # Longer than the data key's window would be. Deleting this key invalidates
  # every access token in flight AND makes historical tokens unverifiable, so
  # the recovery window should outlast anyone's holiday.

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid       = "EnableRoot"
        Effect    = "Allow"
        Principal = { AWS = "arn:aws:iam::${var.account_id}:root" }
        Action    = "kms:*"
        Resource  = "*"
      },
      {
        # Sign, and read the public half for the JWKS document. Deliberately
        # NOT kms:Verify — this service verifies locally against the published
        # public key like every other service does, so that the one service
        # minting tokens is not the one service skipping the verification path.
        Sid       = "IdentityServiceMaySign"
        Effect    = "Allow"
        Principal = { AWS = module.eks.irsa_role_arns["identity-service"] }
        Action    = ["kms:Sign", "kms:GetPublicKey", "kms:DescribeKey"]
        Resource  = "*"
      },
      {
        # Everything else in the platform may read the public half only. In
        # practice they fetch it over HTTP from the JWKS endpoint; this exists
        # so a service can fall back to KMS if identity-service is unreachable
        # during a cold start.
        Sid       = "PlatformMayReadThePublicKey"
        Effect    = "Allow"
        Principal = { AWS = [for name, arn in module.eks.irsa_role_arns : arn] }
        Action    = ["kms:GetPublicKey"]
        Resource  = "*"
      },
    ]
  })

  tags = merge(local.tags, { Name = "${local.name}-jwt-signing" })
}

resource "aws_kms_alias" "jwt_signing" {
  name          = "alias/${local.name}-jwt-signing"
  target_key_id = aws_kms_key.jwt_signing.key_id
}

resource "aws_kms_key" "logs" {
  description             = "SOUQ CloudWatch Logs"
  enable_key_rotation     = true
  deletion_window_in_days = 30

  # CloudWatch Logs needs an explicit grant; the default key policy does not
  # cover it and the log group creation fails with an unhelpful error.
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid       = "EnableRoot"
        Effect    = "Allow"
        Principal = { AWS = "arn:aws:iam::${var.account_id}:root" }
        Action    = "kms:*"
        Resource  = "*"
      },
      {
        Sid       = "AllowCloudWatchLogs"
        Effect    = "Allow"
        Principal = { Service = "logs.${local.region}.amazonaws.com" }
        Action    = ["kms:Encrypt*", "kms:Decrypt*", "kms:ReEncrypt*", "kms:GenerateDataKey*", "kms:Describe*"]
        Resource  = "*"
        Condition = {
          ArnLike = { "kms:EncryptionContext:aws:logs:arn" = "arn:aws:logs:${local.region}:${var.account_id}:log-group:*" }
        }
      },
    ]
  })

  tags = merge(local.tags, { Name = "${local.name}-logs" })
}

##############################################################################
# Network
##############################################################################

module "network" {
  source = "../../modules/network"

  name       = local.name
  region     = local.region
  account_id = var.account_id

  vpc_cidr = "10.0.0.0/16"
  az_count = 3

  enable_nat = true
  # Per-AZ in production: no shared egress failure domain, and the cross-AZ
  # data-transfer saving at our volume exceeds the extra gateway cost.
  one_nat_per_az = true

  enable_interface_endpoints = true
  flow_log_retention_days    = 90
  log_kms_key_arn            = aws_kms_key.logs.arn

  tags = local.tags
}

##############################################################################
# EKS
##############################################################################

module "eks" {
  source = "../../modules/eks"

  name       = local.name
  region     = local.region
  account_id = var.account_id

  kubernetes_version = "1.31"

  vpc_id             = module.network.vpc_id
  private_subnet_ids = module.network.private_subnet_ids

  # Private API endpoint. Access is via SSM Session Manager or the VPN — there
  # is no public control-plane endpoint to find and probe.
  endpoint_public_access  = false
  endpoint_private_access = true
  public_access_cidrs     = []

  # A small always-on managed node group runs the things that must exist
  # before Karpenter can schedule anything — CoreDNS, the EBS CSI driver, and
  # Karpenter itself. Everything else lands on Karpenter-provisioned capacity.
  system_node_instance_types = ["m7g.large"]
  system_node_min_size       = 3
  system_node_max_size       = 6

  enable_karpenter = true

  cluster_log_types   = ["api", "audit", "authenticator", "controllerManager", "scheduler"]
  log_kms_key_arn     = aws_kms_key.logs.arn
  secrets_kms_key_arn = aws_kms_key.data.arn

  tags = local.tags
}

##############################################################################
# Data plane
##############################################################################

module "data" {
  source = "../../modules/data"

  name       = local.name
  region     = local.region
  account_id = var.account_id

  vpc_id                     = module.network.vpc_id
  data_subnet_ids            = module.network.data_subnet_ids
  eks_node_security_group_id = module.eks.node_security_group_id

  kms_key_arn     = aws_kms_key.data.arn
  log_kms_key_arn = aws_kms_key.logs.arn

  is_production = true

  msk_broker_count   = 3
  msk_instance_type  = "kafka.m5.large"
  msk_volume_size_gb = 200

  redis_node_type          = "cache.r7g.large"
  opensearch_instance_type = "r6g.large.search"
  docdb_instance_class     = "db.r6g.large"

  search_service_role_arns = [module.eks.irsa_role_arns["search-service"]]

  tags = local.tags
}

##############################################################################
# Edge: S3, CloudFront, WAF
##############################################################################

module "edge" {
  source = "../../modules/edge"

  providers = {
    aws           = aws
    aws.us_east_1 = aws.us_east_1
  }

  name        = local.name
  domain_name = var.domain_name
  # Storefront and admin both run on EKS behind the ALB; CloudFront fronts
  # them for TLS termination, WAF, and static-asset caching.
  alb_dns_name = module.eks.alb_dns_name

  kms_key_arn = aws_kms_key.data.arn

  # Rate limits are per-IP over a 5-minute window. Checkout is deliberately
  # tighter than browsing: a bot scraping the catalogue is annoying, a bot
  # hammering checkout is card testing.
  rate_limit_general  = 2000
  rate_limit_checkout = 100
  rate_limit_auth     = 30

  tags = local.tags
}

##############################################################################
# Amazon Personalize
##############################################################################

module "personalize" {
  source = "../../modules/personalize"

  name        = local.name
  kms_key_arn = aws_kms_key.data.arn

  # Firehose mirrors souq.user.activity.v1 into S3 in the schema Personalize
  # expects, so a dataset import is a scheduled job rather than an ETL project.
  activity_bucket = module.edge.personalize_bucket

  recipes = {
    # "Recommended for you" on the home page.
    user_personalization = "arn:aws:personalize:::recipe/aws-user-personalization-v2"
    # "Customers also viewed" on the PDP.
    similar_items = "arn:aws:personalize:::recipe/aws-similar-items"
    # Reranks search results for a signed-in user.
    personalized_ranking = "arn:aws:personalize:::recipe/aws-personalized-ranking-v2"
  }

  tags = local.tags
}

##############################################################################
# Observability
##############################################################################

module "observability" {
  source = "../../modules/observability"

  name       = local.name
  region     = local.region
  account_id = var.account_id

  cluster_name      = module.eks.cluster_name
  oidc_provider_arn = module.eks.oidc_provider_arn

  # Amazon Managed Prometheus + Managed Grafana rather than self-hosted: the
  # metrics stack must survive the cluster it observes.
  enable_managed_prometheus = true
  enable_managed_grafana    = true

  alert_email_addresses = var.alert_email_addresses
  pagerduty_endpoint    = var.pagerduty_endpoint

  tags = local.tags
}

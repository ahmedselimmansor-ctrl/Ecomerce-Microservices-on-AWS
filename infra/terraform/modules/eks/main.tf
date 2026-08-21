##############################################################################
# EKS
#
# Two capacity tiers, deliberately:
#
#   system     a small managed node group that is always on. It runs the
#              things that must exist BEFORE anything can be scheduled —
#              CoreDNS, the EBS CSI driver, and Karpenter itself. Putting
#              Karpenter on Karpenter-provisioned capacity is a bootstrap
#              deadlock that only shows up when the cluster is empty, i.e.
#              during disaster recovery.
#
#   karpenter  everything else. Provisions the right instance type for the
#              actual pending pods rather than guessing a node group shape in
#              advance, and consolidates aggressively when demand falls.
#
# Access is IAM-only via EKS access entries. aws-auth ConfigMap editing is not
# used: a typo in that ConfigMap locks everyone out of the cluster including
# the person who made it, and recovery requires the cluster creator's
# credentials.
##############################################################################

terraform {
  required_version = ">= 1.9"
  required_providers {
    aws        = { source = "hashicorp/aws", version = "~> 5.60" }
    kubernetes = { source = "hashicorp/kubernetes", version = "~> 2.32" }
    helm       = { source = "hashicorp/helm", version = "~> 2.15" }
    tls        = { source = "hashicorp/tls", version = "~> 4.0" }
  }
}

locals {
  tags = merge(var.tags, { Module = "eks" })

  # One IRSA role per service. A single shared role would mean search-service
  # could read the payments secret, which is exactly the blast radius IRSA
  # exists to prevent.
  services = [
    "order-service", "inventory-service", "payment-service",
    "identity-service", "catalog-service", "cart-service",
    "search-service", "recommendation-service", "pricing-engine",
    "review-service", "notification-service",
  ]
}

##############################################################################
# Cluster
##############################################################################

resource "aws_eks_cluster" "this" {
  name     = var.name
  role_arn = aws_iam_role.cluster.arn
  version  = var.kubernetes_version

  vpc_config {
    subnet_ids              = var.private_subnet_ids
    endpoint_private_access = var.endpoint_private_access
    endpoint_public_access  = var.endpoint_public_access
    public_access_cidrs     = var.endpoint_public_access ? var.public_access_cidrs : null
    security_group_ids      = [aws_security_group.cluster.id]
  }

  access_config {
    # API, not the aws-auth ConfigMap. Access is granted with IAM access
    # entries below, which are auditable in CloudTrail and cannot lock the
    # cluster out with a YAML typo.
    authentication_mode                         = "API"
    bootstrap_cluster_creator_admin_permissions = false
  }

  # Envelope encryption for Secrets. Without it, Kubernetes Secrets are stored
  # base64-encoded — that is encoding, not encryption, and an etcd snapshot
  # hands over every credential in the cluster.
  encryption_config {
    provider {
      key_arn = var.secrets_kms_key_arn
    }
    resources = ["secrets"]
  }

  enabled_cluster_log_types = var.cluster_log_types

  # `audit` in particular: without it there is no record of who did what in
  # the cluster, which makes any security question unanswerable after the fact.
  depends_on = [
    aws_iam_role_policy_attachment.cluster_policy,
    aws_cloudwatch_log_group.cluster,
  ]

  tags = local.tags
}

resource "aws_cloudwatch_log_group" "cluster" {
  # EKS writes to this exact path. Creating it here rather than letting EKS do
  # it is the only way to set retention and a CMK.
  name              = "/aws/eks/${var.name}/cluster"
  retention_in_days = var.log_retention_days
  kms_key_id        = var.log_kms_key_arn
  tags              = local.tags
}

resource "aws_security_group" "cluster" {
  name_prefix = "${var.name}-cluster-"
  description = "EKS control plane"
  vpc_id      = var.vpc_id

  egress {
    description = "control plane to nodes"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = merge(local.tags, { Name = "${var.name}-cluster" })
  lifecycle {
    create_before_destroy = true
  }
}

##############################################################################
# OIDC provider — the foundation of IRSA
##############################################################################

data "tls_certificate" "oidc" {
  url = aws_eks_cluster.this.identity[0].oidc[0].issuer
}
resource "aws_iam_openid_connect_provider" "this" {
  url             = aws_eks_cluster.this.identity[0].oidc[0].issuer
  client_id_list  = ["sts.amazonaws.com"]
  thumbprint_list = [data.tls_certificate.oidc.certificates[0].sha1_fingerprint]
  tags            = local.tags
}

locals {
  oidc_issuer = replace(aws_eks_cluster.this.identity[0].oidc[0].issuer, "https://", "")
}

##############################################################################
# IRSA roles
#
# One per service. The trust policy pins BOTH the namespace and the service
# account name — omitting the sub condition would let any pod in any namespace
# assume the role, which is the most common IRSA misconfiguration.
##############################################################################

data "aws_iam_policy_document" "irsa_trust" {
  for_each = toset(local.services)

  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRoleWithWebIdentity"]

    principals {
      type        = "Federated"
      identifiers = [aws_iam_openid_connect_provider.this.arn]
    }

    condition {
      test     = "StringEquals"
      variable = "${local.oidc_issuer}:sub"
      values   = ["system:serviceaccount:souq:${each.value}"]
    }

    # Without this, a token minted for a different audience is accepted.
    condition {
      test     = "StringEquals"
      variable = "${local.oidc_issuer}:aud"
      values   = ["sts.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "irsa" {
  for_each = toset(local.services)

  name               = "${var.name}-${each.value}"
  assume_role_policy = data.aws_iam_policy_document.irsa_trust[each.value].json
  tags               = merge(local.tags, { Service = each.value })
}

##############################################################################
# System node group
##############################################################################

resource "aws_eks_node_group" "system" {
  cluster_name    = aws_eks_cluster.this.name
  node_group_name = "${var.name}-system"
  node_role_arn   = aws_iam_role.node.arn
  subnet_ids      = var.private_subnet_ids

  # Graviton. Roughly 20% cheaper per vCPU and every image in this platform is
  # built multi-arch, so there is no reason to pay for x86 here.
  instance_types = var.system_node_instance_types
  ami_type       = "AL2023_ARM_64_STANDARD"
  capacity_type  = "ON_DEMAND" # never spot: these nodes run CoreDNS

  scaling_config {
    desired_size = var.system_node_min_size
    min_size     = var.system_node_min_size
    max_size     = var.system_node_max_size
  }

  update_config {
    # One at a time. These nodes host cluster-critical add-ons; taking two out
    # of three simultaneously during an upgrade is a DNS outage.
    max_unavailable = 1
  }

  # Only system workloads. Application pods without a toleration cannot land
  # here, which keeps the always-on tier small and predictable.
  taint {
    key    = "CriticalAddonsOnly"
    value  = "true"
    effect = "NO_SCHEDULE"
  }

  labels = { "souq.dev/pool" = "system" }

  lifecycle {
    # The desired size drifts as the cluster autoscaler moves it; Terraform
    # fighting that produces a diff on every plan.
    ignore_changes = [scaling_config[0].desired_size]
  }

  depends_on = [
    aws_iam_role_policy_attachment.node_worker,
    aws_iam_role_policy_attachment.node_cni,
    aws_iam_role_policy_attachment.node_ecr,
  ]

  tags = merge(local.tags, { Name = "${var.name}-system" })
}

##############################################################################
# Add-ons
#
# Managed add-ons rather than Helm charts, so EKS handles version
# compatibility with the control plane on upgrade.
##############################################################################

resource "aws_eks_addon" "vpc_cni" {
  cluster_name  = aws_eks_cluster.this.name
  addon_name    = "vpc-cni"
  addon_version = var.addon_versions.vpc_cni

  # PRESERVE, not OVERWRITE. An add-on update silently reverting a hand-tuned
  # setting is a very confusing outage.
  resolve_conflicts_on_update = "PRESERVE"
  service_account_role_arn    = aws_iam_role.vpc_cni.arn

  configuration_values = jsonencode({
    env = {
      # Prefix delegation. Without it, a node's pod capacity is capped by its
      # ENI count — an m7g.large tops out at 29 pods, which is wasteful for a
      # fleet of small Go services.
      ENABLE_PREFIX_DELEGATION = "true"
      WARM_PREFIX_TARGET       = "1"
      # Required for Kubernetes NetworkPolicy to be enforced at all. Without
      # it the policies in each service's k8s/ directory are decoration.
      ENABLE_NETWORK_POLICY = "true"
    }
  })

  tags = local.tags
}

resource "aws_eks_addon" "coredns" {
  cluster_name                = aws_eks_cluster.this.name
  addon_name                  = "coredns"
  addon_version               = var.addon_versions.coredns
  resolve_conflicts_on_update = "PRESERVE"

  configuration_values = jsonencode({
    replicaCount = 3
    tolerations  = [{ key = "CriticalAddonsOnly", operator = "Exists", effect = "NoSchedule" }]
    # Anti-affinity across zones: all three CoreDNS pods on one node makes DNS
    # a single point of failure for the whole cluster.
    affinity = {
      podAntiAffinity = {
        requiredDuringSchedulingIgnoredDuringExecution = [{
          labelSelector = { matchExpressions = [{ key = "k8s-app", operator = "In", values = ["kube-dns"] }] }
          topologyKey   = "kubernetes.io/hostname"
        }]
      }
    }
  })

  depends_on = [aws_eks_node_group.system]
  tags       = local.tags
}

resource "aws_eks_addon" "kube_proxy" {
  cluster_name                = aws_eks_cluster.this.name
  addon_name                  = "kube-proxy"
  addon_version               = var.addon_versions.kube_proxy
  resolve_conflicts_on_update = "PRESERVE"
  tags                        = local.tags
}

resource "aws_eks_addon" "ebs_csi" {
  cluster_name                = aws_eks_cluster.this.name
  addon_name                  = "aws-ebs-csi-driver"
  addon_version               = var.addon_versions.ebs_csi
  resolve_conflicts_on_update = "PRESERVE"
  service_account_role_arn    = aws_iam_role.ebs_csi.arn
  depends_on                  = [aws_eks_node_group.system]
  tags                        = local.tags
}

resource "aws_eks_addon" "pod_identity" {
  cluster_name  = aws_eks_cluster.this.name
  addon_name    = "eks-pod-identity-agent"
  addon_version = var.addon_versions.pod_identity
  tags          = local.tags
}

##############################################################################
# Karpenter
##############################################################################

resource "aws_iam_role" "karpenter_node" {
  count = var.enable_karpenter ? 1 : 0

  name = "${var.name}-karpenter-node"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "ec2.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })
  tags = local.tags
}

resource "aws_iam_role_policy_attachment" "karpenter_node" {
  for_each = var.enable_karpenter ? toset([
    "arn:aws:iam::aws:policy/AmazonEKSWorkerNodePolicy",
    "arn:aws:iam::aws:policy/AmazonEKS_CNI_Policy",
    "arn:aws:iam::aws:policy/AmazonEC2ContainerRegistryReadOnly",
    "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore",
  ]) : toset([])

  role       = aws_iam_role.karpenter_node[0].name
  policy_arn = each.value
}

# Karpenter-provisioned nodes need to be able to join the cluster.
resource "aws_eks_access_entry" "karpenter_node" {
  count = var.enable_karpenter ? 1 : 0

  cluster_name  = aws_eks_cluster.this.name
  principal_arn = aws_iam_role.karpenter_node[0].arn
  type          = "EC2_LINUX"
}

resource "aws_iam_role" "karpenter_controller" {
  count = var.enable_karpenter ? 1 : 0

  name = "${var.name}-karpenter-controller"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Federated = aws_iam_openid_connect_provider.this.arn }
      Action    = "sts:AssumeRoleWithWebIdentity"
      Condition = {
        StringEquals = {
          "${local.oidc_issuer}:sub" = "system:serviceaccount:karpenter:karpenter"
          "${local.oidc_issuer}:aud" = "sts.amazonaws.com"
        }
      }
    }]
  })
  tags = local.tags
}

resource "aws_iam_role_policy" "karpenter_controller" {
  count = var.enable_karpenter ? 1 : 0

  role = aws_iam_role.karpenter_controller[0].id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid    = "ProvisionCapacity"
        Effect = "Allow"
        Action = [
          "ec2:CreateLaunchTemplate", "ec2:CreateFleet", "ec2:RunInstances",
          "ec2:CreateTags", "ec2:DescribeLaunchTemplates", "ec2:DescribeInstances",
          "ec2:DescribeSecurityGroups", "ec2:DescribeSubnets",
          "ec2:DescribeInstanceTypes", "ec2:DescribeInstanceTypeOfferings",
          "ec2:DescribeAvailabilityZones", "ec2:DescribeSpotPriceHistory",
          "ec2:DescribeImages",
          "pricing:GetProducts", "ssm:GetParameter",
        ]
        Resource = "*"
      },
      {
        Sid      = "TerminateOnlyOurOwnNodes"
        Effect   = "Allow"
        Action   = ["ec2:TerminateInstances", "ec2:DeleteLaunchTemplate"]
        Resource = "*"
        # Without this condition Karpenter can terminate ANY instance in the
        # account, including the bastion and anything else that happens to
        # share the region.
        Condition = {
          StringEquals = { "aws:ResourceTag/karpenter.sh/nodepool" = "*" }
        }
      },
      {
        Sid      = "PassNodeRole"
        Effect   = "Allow"
        Action   = "iam:PassRole"
        Resource = aws_iam_role.karpenter_node[0].arn
      },
      {
        Sid      = "ReadCluster"
        Effect   = "Allow"
        Action   = ["eks:DescribeCluster"]
        Resource = aws_eks_cluster.this.arn
      },
      {
        Sid      = "InterruptionQueue"
        Effect   = "Allow"
        Action   = ["sqs:DeleteMessage", "sqs:GetQueueUrl", "sqs:ReceiveMessage"]
        Resource = aws_sqs_queue.karpenter_interruption[0].arn
      },
    ]
  })
}

# Spot interruption notices land here, giving Karpenter ~2 minutes to drain a
# node gracefully instead of having pods killed with it.
resource "aws_sqs_queue" "karpenter_interruption" {
  count = var.enable_karpenter ? 1 : 0

  name                      = "${var.name}-karpenter-interruption"
  message_retention_seconds = 300
  sqs_managed_sse_enabled   = true
  tags                      = local.tags
}

resource "aws_cloudwatch_event_rule" "spot_interruption" {
  count = var.enable_karpenter ? 1 : 0

  name        = "${var.name}-spot-interruption"
  description = "EC2 spot interruption warnings for Karpenter"
  event_pattern = jsonencode({
    source      = ["aws.ec2"]
    detail-type = ["EC2 Spot Instance Interruption Warning", "EC2 Instance Rebalance Recommendation"]
  })
  tags = local.tags
}

resource "aws_cloudwatch_event_target" "spot_interruption" {
  count = var.enable_karpenter ? 1 : 0

  rule = aws_cloudwatch_event_rule.spot_interruption[0].name
  arn  = aws_sqs_queue.karpenter_interruption[0].arn
}

##############################################################################
# Cluster / node IAM
##############################################################################

resource "aws_iam_role" "cluster" {
  name = "${var.name}-cluster"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "eks.amazonaws.com" }
      Action    = ["sts:AssumeRole", "sts:TagSession"]
    }]
  })
  tags = local.tags
}

resource "aws_iam_role_policy_attachment" "cluster_policy" {
  role       = aws_iam_role.cluster.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonEKSClusterPolicy"
}

resource "aws_iam_role" "node" {
  name = "${var.name}-node"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "ec2.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })
  tags = local.tags
}

resource "aws_iam_role_policy_attachment" "node_worker" {
  role       = aws_iam_role.node.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonEKSWorkerNodePolicy"
}

resource "aws_iam_role_policy_attachment" "node_cni" {
  role       = aws_iam_role.node.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonEKS_CNI_Policy"
}

resource "aws_iam_role_policy_attachment" "node_ecr" {
  role       = aws_iam_role.node.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonEC2ContainerRegistryReadOnly"
}

# SSM rather than SSH. No bastion, no key pairs, no port 22 anywhere, and
# every session is recorded in CloudTrail.
resource "aws_iam_role_policy_attachment" "node_ssm" {
  role       = aws_iam_role.node.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
}

resource "aws_iam_role" "vpc_cni" {
  name = "${var.name}-vpc-cni"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Federated = aws_iam_openid_connect_provider.this.arn }
      Action    = "sts:AssumeRoleWithWebIdentity"
      Condition = {
        StringEquals = {
          "${local.oidc_issuer}:sub" = "system:serviceaccount:kube-system:aws-node"
          "${local.oidc_issuer}:aud" = "sts.amazonaws.com"
        }
      }
    }]
  })
  tags = local.tags
}

resource "aws_iam_role_policy_attachment" "vpc_cni" {
  role       = aws_iam_role.vpc_cni.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonEKS_CNI_Policy"
}

resource "aws_iam_role" "ebs_csi" {
  name = "${var.name}-ebs-csi"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Federated = aws_iam_openid_connect_provider.this.arn }
      Action    = "sts:AssumeRoleWithWebIdentity"
      Condition = {
        StringEquals = {
          "${local.oidc_issuer}:sub" = "system:serviceaccount:kube-system:ebs-csi-controller-sa"
          "${local.oidc_issuer}:aud" = "sts.amazonaws.com"
        }
      }
    }]
  })
  tags = local.tags
}

resource "aws_iam_role_policy_attachment" "ebs_csi" {
  role       = aws_iam_role.ebs_csi.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonEBSCSIDriverPolicy"
}

##############################################################################
# Cluster access
##############################################################################

resource "aws_eks_access_entry" "admins" {
  for_each = toset(var.cluster_admin_role_arns)

  cluster_name  = aws_eks_cluster.this.name
  principal_arn = each.value
  type          = "STANDARD"
}

resource "aws_eks_access_policy_association" "admins" {
  for_each = toset(var.cluster_admin_role_arns)

  cluster_name  = aws_eks_cluster.this.name
  principal_arn = each.value
  policy_arn    = "arn:aws:eks::aws:cluster-access-policy/AmazonEKSClusterAdminPolicy"

  access_scope {

    type = "cluster"

  }
  depends_on = [aws_eks_access_entry.admins]
}

# Read-only for on-call. Enough to diagnose an incident, not enough to make it
# worse at 3am.
resource "aws_eks_access_entry" "viewers" {
  for_each = toset(var.cluster_viewer_role_arns)

  cluster_name  = aws_eks_cluster.this.name
  principal_arn = each.value
  type          = "STANDARD"
}

resource "aws_eks_access_policy_association" "viewers" {
  for_each = toset(var.cluster_viewer_role_arns)

  cluster_name  = aws_eks_cluster.this.name
  principal_arn = each.value
  policy_arn    = "arn:aws:eks::aws:cluster-access-policy/AmazonEKSViewPolicy"

  access_scope {

    type = "cluster"

  }
  depends_on = [aws_eks_access_entry.viewers]
}

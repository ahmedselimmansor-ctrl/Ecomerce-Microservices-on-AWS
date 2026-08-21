##############################################################################
# Network
#
# Three tiers across three AZs. The tier split is not ceremony: it is what
# makes the "a compromised pod cannot reach the internet or the database
# directly" claim enforceable rather than aspirational.
#
#   public   ALB and NAT only. Nothing else is ever placed here.
#   private  EKS nodes and pods. Egress through NAT, no inbound from anywhere.
#   data     Aurora, MSK, ElastiCache, DocumentDB, OpenSearch. NO route to a
#            NAT gateway at all, so a database host physically cannot reach the
#            internet even if something inside it wants to.
##############################################################################

terraform {
  required_version = ">= 1.9"
  required_providers {
    aws = { source = "hashicorp/aws", version = "~> 5.60" }
  }
}

locals {
  azs = slice(data.aws_availability_zones.available.names, 0, var.az_count)

  # /16 carved so each tier gets contiguous space and the CIDRs are readable
  # in a security group rule at 3am:
  #   10.0.0.0/20    public   (4094 usable per AZ block)
  #   10.0.16.0/18   private  large, because pod ENIs consume addresses fast
  #   10.0.80.0/20   data
  public_cidrs  = [for i in range(var.az_count) : cidrsubnet(var.vpc_cidr, 6, i)]
  private_cidrs = [for i in range(var.az_count) : cidrsubnet(var.vpc_cidr, 4, i + 1)]
  data_cidrs    = [for i in range(var.az_count) : cidrsubnet(var.vpc_cidr, 6, i + 20)]

  tags = merge(var.tags, {
    Module = "network"
  })
}

data "aws_availability_zones" "available" {
  state = "available"
}
resource "aws_vpc" "this" {
  cidr_block           = var.vpc_cidr
  enable_dns_support   = true
  enable_dns_hostnames = true # required by every AWS-managed data service here

  tags = merge(local.tags, { Name = "${var.name}-vpc" })
}

resource "aws_internet_gateway" "this" {
  vpc_id = aws_vpc.this.id
  tags   = merge(local.tags, { Name = "${var.name}-igw" })
}

##############################################################################
# Subnets
##############################################################################

resource "aws_subnet" "public" {
  count = var.az_count

  vpc_id                  = aws_vpc.this.id
  cidr_block              = local.public_cidrs[count.index]
  availability_zone       = local.azs[count.index]
  map_public_ip_on_launch = false # ALB gets its own EIPs; nothing else belongs here

  tags = merge(local.tags, {
    Name                     = "${var.name}-public-${local.azs[count.index]}"
    Tier                     = "public"
    "kubernetes.io/role/elb" = "1"
    # Cluster ownership tag, required for the AWS Load Balancer Controller to
    # discover these subnets when it provisions an ALB from an Ingress.
    "kubernetes.io/cluster/${var.name}" = "shared"
  })
}

resource "aws_subnet" "private" {
  count = var.az_count

  vpc_id            = aws_vpc.this.id
  cidr_block        = local.private_cidrs[count.index]
  availability_zone = local.azs[count.index]

  tags = merge(local.tags, {
    Name                                = "${var.name}-private-${local.azs[count.index]}"
    Tier                                = "private"
    "kubernetes.io/role/internal-elb"   = "1"
    "kubernetes.io/cluster/${var.name}" = "shared"
    # Karpenter finds capacity to provision into by this tag.
    "karpenter.sh/discovery" = var.name
  })
}

resource "aws_subnet" "data" {
  count = var.az_count

  vpc_id            = aws_vpc.this.id
  cidr_block        = local.data_cidrs[count.index]
  availability_zone = local.azs[count.index]

  tags = merge(local.tags, {
    Name = "${var.name}-data-${local.azs[count.index]}"
    Tier = "data"
  })
}

##############################################################################
# NAT
#
# one_nat_per_az is a deliberate cost/availability decision, not a default:
#
#   false  one NAT (~$33/mo + data). If its AZ fails, every private subnet
#          loses egress. Correct for dev.
#   true   one per AZ (~$100/mo + data, and cross-AZ data charges disappear
#          because traffic stays in-zone). Correct for prod.
#
# The cross-AZ data transfer saving at production volume usually exceeds the
# extra gateway cost, so this is rarely the trade it first appears to be.
##############################################################################

resource "aws_eip" "nat" {
  count = var.enable_nat ? (var.one_nat_per_az ? var.az_count : 1) : 0

  domain = "vpc"
  tags   = merge(local.tags, { Name = "${var.name}-nat-${count.index}" })

  depends_on = [aws_internet_gateway.this]
}

resource "aws_nat_gateway" "this" {
  count = var.enable_nat ? (var.one_nat_per_az ? var.az_count : 1) : 0

  allocation_id = aws_eip.nat[count.index].id
  subnet_id     = aws_subnet.public[count.index].id

  tags = merge(local.tags, { Name = "${var.name}-nat-${local.azs[count.index]}" })

  depends_on = [aws_internet_gateway.this]
}

##############################################################################
# Routing
##############################################################################

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.this.id
  tags   = merge(local.tags, { Name = "${var.name}-rt-public" })
}

resource "aws_route" "public_internet" {
  route_table_id         = aws_route_table.public.id
  destination_cidr_block = "0.0.0.0/0"
  gateway_id             = aws_internet_gateway.this.id
}

resource "aws_route_table_association" "public" {
  count          = var.az_count
  subnet_id      = aws_subnet.public[count.index].id
  route_table_id = aws_route_table.public.id
}

# One route table per private subnet so each can point at its own AZ's NAT.
resource "aws_route_table" "private" {
  count = var.az_count

  vpc_id = aws_vpc.this.id
  tags   = merge(local.tags, { Name = "${var.name}-rt-private-${local.azs[count.index]}" })
}

resource "aws_route" "private_nat" {
  count = var.enable_nat ? var.az_count : 0

  route_table_id         = aws_route_table.private[count.index].id
  destination_cidr_block = "0.0.0.0/0"
  nat_gateway_id         = aws_nat_gateway.this[var.one_nat_per_az ? count.index : 0].id
}

resource "aws_route_table_association" "private" {
  count          = var.az_count
  subnet_id      = aws_subnet.private[count.index].id
  route_table_id = aws_route_table.private[count.index].id
}

# The data tier's route table has NO default route. That absence is the
# control: a compromised database host has nowhere to exfiltrate to, and it
# cannot be undone by a security group change.
resource "aws_route_table" "data" {
  vpc_id = aws_vpc.this.id
  tags   = merge(local.tags, { Name = "${var.name}-rt-data" })
}

resource "aws_route_table_association" "data" {
  count          = var.az_count
  subnet_id      = aws_subnet.data[count.index].id
  route_table_id = aws_route_table.data.id
}

##############################################################################
# VPC endpoints
#
# S3 and DynamoDB gateway endpoints are free and cut NAT data charges
# substantially — product media and Personalize datasets are the bulk of our
# S3 traffic. The interface endpoints cost ~$7/mo each but keep ECR pulls,
# Secrets Manager reads and CloudWatch writes off the public internet
# entirely, which is both cheaper at volume and a smaller attack surface.
##############################################################################

resource "aws_vpc_endpoint" "s3" {
  vpc_id            = aws_vpc.this.id
  service_name      = "com.amazonaws.${var.region}.s3"
  vpc_endpoint_type = "Gateway"

  route_table_ids = concat(
    aws_route_table.private[*].id,
    [aws_route_table.data.id], # so Aurora can reach S3 for backups
  )

  tags = merge(local.tags, { Name = "${var.name}-vpce-s3" })
}

resource "aws_vpc_endpoint" "dynamodb" {
  vpc_id            = aws_vpc.this.id
  service_name      = "com.amazonaws.${var.region}.dynamodb"
  vpc_endpoint_type = "Gateway"
  route_table_ids   = aws_route_table.private[*].id

  tags = merge(local.tags, { Name = "${var.name}-vpce-dynamodb" })
}

resource "aws_security_group" "vpc_endpoints" {
  name_prefix = "${var.name}-vpce-"
  description = "HTTPS from within the VPC to interface endpoints"
  vpc_id      = aws_vpc.this.id

  ingress {
    description = "HTTPS from the VPC"
    from_port   = 443
    to_port     = 443
    protocol    = "tcp"
    cidr_blocks = [var.vpc_cidr]
  }

  # No egress rule at all. An interface endpoint only ever responds to
  # requests; it never initiates one.

  tags = merge(local.tags, { Name = "${var.name}-vpce-sg" })

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_vpc_endpoint" "interface" {
  for_each = var.enable_interface_endpoints ? toset([
    "ecr.api", "ecr.dkr",                # image pulls without egressing to the internet
    "secretsmanager",                    # database credentials
    "kms",                               # JWT signing keys, envelope encryption
    "logs", "monitoring",                # CloudWatch Logs and metrics
    "sts",                               # IRSA token exchange
    "ssm", "ssmmessages", "ec2messages", # SSM Session Manager, so no bastion
    "sqs", "sns",
    "elasticloadbalancing",
    "autoscaling",
  ]) : toset([])

  vpc_id              = aws_vpc.this.id
  service_name        = "com.amazonaws.${var.region}.${each.value}"
  vpc_endpoint_type   = "Interface"
  subnet_ids          = aws_subnet.private[*].id
  security_group_ids  = [aws_security_group.vpc_endpoints.id]
  private_dns_enabled = true

  tags = merge(local.tags, { Name = "${var.name}-vpce-${each.value}" })
}

##############################################################################
# Flow logs
#
# Rejected traffic only. Accepting everything on a busy cluster produces
# terabytes a month and the signal is entirely in the rejects — a pod trying to
# reach something it should not is exactly what a NetworkPolicy violation looks
# like from the outside.
##############################################################################

resource "aws_cloudwatch_log_group" "flow" {
  name              = "/aws/vpc/${var.name}/flowlogs"
  retention_in_days = var.flow_log_retention_days
  kms_key_id        = var.log_kms_key_arn
  tags              = local.tags
}

resource "aws_iam_role" "flow" {
  name_prefix = "${var.name}-flow-"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "vpc-flow-logs.amazonaws.com" }
      Action    = "sts:AssumeRole"
      Condition = {
        # Confused-deputy guard: without these, any account's flow log service
        # could assume this role.
        StringEquals = { "aws:SourceAccount" = var.account_id }
        ArnLike      = { "aws:SourceArn" = "arn:aws:ec2:${var.region}:${var.account_id}:vpc-flow-log/*" }
      }
    }]
  })

  tags = local.tags
}

resource "aws_iam_role_policy" "flow" {
  role = aws_iam_role.flow.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Action = [
        "logs:CreateLogStream",
        "logs:PutLogEvents",
        "logs:DescribeLogGroups",
        "logs:DescribeLogStreams",
      ]
      Resource = "${aws_cloudwatch_log_group.flow.arn}:*"
    }]
  })
}

resource "aws_flow_log" "this" {
  vpc_id                   = aws_vpc.this.id
  traffic_type             = "REJECT"
  log_destination_type     = "cloud-watch-logs"
  log_destination          = aws_cloudwatch_log_group.flow.arn
  iam_role_arn             = aws_iam_role.flow.arn
  max_aggregation_interval = 60

  tags = merge(local.tags, { Name = "${var.name}-flowlogs" })
}

##############################################################################
# Default security group lockdown
#
# AWS creates a default SG that allows all traffic between anything using it.
# It cannot be deleted, so it is emptied instead — otherwise a resource created
# without an explicit SG silently joins a permissive mesh.
##############################################################################

resource "aws_default_security_group" "this" {
  vpc_id = aws_vpc.this.id

  # No ingress, no egress blocks: both are empty by omission.

  tags = merge(local.tags, { Name = "${var.name}-default-DO-NOT-USE" })
}

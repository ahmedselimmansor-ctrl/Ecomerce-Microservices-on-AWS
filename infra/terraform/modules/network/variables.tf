variable "name" {
  description = "Cluster/environment name. Used as a prefix and in Kubernetes discovery tags."
  type        = string

  validation {
    # EKS cluster names and the tags derived from them have this constraint;
    # catching it here beats a failure 20 minutes into an apply.
    condition     = can(regex("^[a-z][a-z0-9-]{1,38}$", var.name))
    error_message = "name must be lowercase alphanumeric with hyphens, 2-39 characters, starting with a letter."
  }
}

variable "region" {
  type = string
}
variable "account_id" {
  description = "Used in the flow-log role's confused-deputy conditions."
  type        = string
}

variable "vpc_cidr" {
  description = "A /16. Smaller ranges run out of pod IPs quickly — the VPC CNI assigns a real VPC address per pod."
  type        = string
  default     = "10.0.0.0/16"

  validation {
    condition     = can(cidrnetmask(var.vpc_cidr)) && tonumber(split("/", var.vpc_cidr)[1]) <= 16
    error_message = "vpc_cidr must be a valid CIDR of /16 or larger."
  }
}

variable "az_count" {
  description = "Availability zones to span. Three is the minimum for a quorum-based service like MSK."
  type        = number
  default     = 3

  validation {
    condition     = var.az_count >= 2 && var.az_count <= 4
    error_message = "az_count must be between 2 and 4; MSK and Aurora multi-AZ both want at least 3 in production."
  }
}

variable "enable_nat" {
  type    = bool
  default = true
}

variable "one_nat_per_az" {
  description = <<-EOT
    false: one NAT gateway. Cheaper (~$33/mo), but its AZ failing removes egress
    from every private subnet.
    true: one per AZ (~$100/mo), no cross-AZ data transfer charges, no shared
    failure domain. At production traffic the data-transfer saving usually
    exceeds the extra gateway cost.
  EOT
  type        = bool
  default     = false
}

variable "enable_interface_endpoints" {
  description = "~$7/mo each, ~14 of them. Keeps ECR, Secrets Manager, KMS and CloudWatch traffic off the public internet and cuts NAT data charges."
  type        = bool
  default     = true
}

variable "flow_log_retention_days" {
  type    = number
  default = 30
}

variable "log_kms_key_arn" {
  description = "Optional CMK for the flow log group. Null uses the AWS-managed key."
  type        = string
  default     = null
}

variable "tags" {
  type    = map(string)
  default = {}
}

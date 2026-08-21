variable "name" {
  type = string
}
variable "region" {
  type = string
}
variable "account_id" {
  type = string
}
variable "vpc_id" {
  type = string
}
variable "data_subnet_ids" {
  description = "Data-tier subnets. These have no route to a NAT gateway."
  type        = list(string)
}

variable "eks_node_security_group_id" {
  description = "The only source allowed to reach any of these stores."
  type        = string
}

variable "kms_key_arn" {
  description = "CMK for encryption at rest across every store. A single key keeps the grant policy and the rotation schedule in one place."
  type        = string
}

variable "log_kms_key_arn" {
  type    = string
  default = null
}

variable "search_service_role_arns" {
  description = "IRSA role ARNs permitted on the OpenSearch domain."
  type        = list(string)
  default     = []
}

variable "is_production" {
  description = <<-EOT
    Drives every availability/cost trade at once: multi-AZ, read replicas,
    dedicated masters, deletion protection, backup retention, Performance
    Insights retention, and MSK storage autoscaling.

    A single flag rather than twenty variables, because these decisions are
    not independent — a dev environment with production backup retention and
    no replicas is nobody's intent.
  EOT
  type        = bool
  default     = false
}

variable "postgres_engine_version" {
  type    = string
  default = "16.4"
}

variable "mysql_engine_version" {
  type    = string
  default = "8.0.mysql_aurora.3.07.1"
}

variable "kafka_version" {
  type    = string
  default = "3.6.0"
}

variable "msk_broker_count" {
  description = "Must be a multiple of the AZ count. Three is the minimum that supports min.insync.replicas=2 while tolerating a broker loss."
  type        = number
  default     = 3

  validation {
    condition     = var.msk_broker_count >= 3 && var.msk_broker_count % 3 == 0
    error_message = "msk_broker_count must be at least 3 and a multiple of 3 to spread evenly across AZs."
  }
}

variable "msk_instance_type" {
  type    = string
  default = "kafka.m5.large"
}

variable "msk_volume_size_gb" {
  type    = number
  default = 100
}

variable "redis_version" {
  type    = string
  default = "7.1"
}

variable "redis_node_type" {
  type    = string
  default = "cache.t4g.micro"
}

variable "opensearch_version" {
  type    = string
  default = "OpenSearch_2.15"
}

variable "opensearch_instance_type" {
  type    = string
  default = "t3.small.search"
}

variable "docdb_instance_class" {
  type    = string
  default = "db.t4g.medium"
}

variable "tags" {
  type    = map(string)
  default = {}
}

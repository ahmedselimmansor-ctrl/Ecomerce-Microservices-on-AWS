variable "name"       { type = string }
variable "region"     { type = string }
variable "account_id" { type = string }
variable "vpc_id"     { type = string }

variable "private_subnet_ids" {
  description = "Nodes and pods. Never public subnets: a pod with a public IP is one security-group mistake from being on the internet."
  type        = list(string)
}

variable "kubernetes_version" {
  type    = string
  default = "1.31"
}

variable "endpoint_public_access" {
  description = "false in production. Access is via SSM Session Manager or the VPN, so there is no public control-plane endpoint to find and probe."
  type        = bool
  default     = false
}

variable "endpoint_private_access" {
  type    = bool
  default = true
}

variable "public_access_cidrs" {
  description = "Only consulted when endpoint_public_access is true. 0.0.0.0/0 here is the single most common EKS misconfiguration."
  type        = list(string)
  default     = []
}

variable "system_node_instance_types" {
  description = "Graviton. ~20% cheaper per vCPU, and every image here is built multi-arch."
  type        = list(string)
  default     = ["m7g.large"]
}

variable "system_node_min_size" {
  description = "3, so CoreDNS can spread one replica per AZ."
  type        = number
  default     = 3
}

variable "system_node_max_size" {
  type    = number
  default = 6
}

variable "enable_karpenter" {
  type    = bool
  default = true
}

variable "cluster_log_types" {
  description = "`audit` is the one that matters: without it, who-did-what in the cluster is unanswerable after the fact."
  type        = list(string)
  default     = ["api", "audit", "authenticator", "controllerManager", "scheduler"]
}

variable "log_retention_days" {
  type    = number
  default = 90
}

variable "log_kms_key_arn" {
  type    = string
  default = null
}

variable "secrets_kms_key_arn" {
  description = "Envelope encryption for Kubernetes Secrets. Without it they are base64-encoded, which is encoding, not encryption."
  type        = string
}

variable "addon_versions" {
  description = "Pinned. An add-on auto-updating during an incident is not a variable you want in play."
  type = object({
    vpc_cni      = string
    coredns      = string
    kube_proxy   = string
    ebs_csi      = string
    pod_identity = string
  })
  default = {
    vpc_cni      = "v1.18.5-eksbuild.1"
    coredns      = "v1.11.3-eksbuild.1"
    kube_proxy   = "v1.31.0-eksbuild.5"
    ebs_csi      = "v1.36.0-eksbuild.1"
    pod_identity = "v1.3.4-eksbuild.1"
  }
}

variable "cluster_admin_role_arns" {
  type    = list(string)
  default = []
}

variable "cluster_viewer_role_arns" {
  description = "On-call. Enough to diagnose an incident, not enough to make it worse."
  type        = list(string)
  default     = []
}

variable "tags" {
  type    = map(string)
  default = {}
}

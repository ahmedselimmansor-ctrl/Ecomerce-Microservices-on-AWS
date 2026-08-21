variable "name" {
  type = string
}
variable "region" {
  type = string
}
variable "account_id" {
  type = string
}
variable "cluster_name" {
  type = string
}
variable "oidc_provider_arn" {
  type = string
}
variable "enable_managed_prometheus" {
  description = "Managed, not self-hosted: the metrics stack must survive the cluster it observes."
  type        = bool
  default     = true
}

variable "enable_managed_grafana" {
  type    = bool
  default = true
}

variable "alert_email_addresses" {
  description = "Ticket-severity only. Pages go to PagerDuty; email is not a paging channel."
  type        = list(string)
  default     = []
}

variable "pagerduty_endpoint" {
  type      = string
  default   = ""
  sensitive = true
}

variable "tags" {
  type    = map(string)
  default = {}
}

variable "account_id" {
  description = "AWS account this environment lives in. Pinned in the provider's allowed_account_ids so a misconfigured profile cannot apply prod somewhere else."
  type        = string
}

variable "domain_name" {
  description = "Apex domain, e.g. souq.dev. CloudFront serves the apex and www; the API is on api.<domain>."
  type        = string
}

variable "alert_email_addresses" {
  description = "Non-paging alerts (error-budget burn warnings, cost anomalies)."
  type        = list(string)
  default     = []
}

variable "pagerduty_endpoint" {
  description = "Events API v2 integration URL for paging alerts: stuck sagas, illegal saga transitions, ledger imbalance."
  type        = string
  default     = ""
  sensitive   = true
}

variable "name" {
  type = string
}
variable "domain_name" {
  type = string
}
variable "alb_dns_name" {
  description = "The internal ALB. Reachable only via CloudFront, enforced by the X-Origin-Verify header."
  type        = string
}

variable "kms_key_arn" {
  type = string
}
variable "rate_limit_general" {
  description = "Requests per IP per 5-minute window across the whole site."
  type        = number
  default     = 2000
}

variable "rate_limit_checkout" {
  description = "Twenty times tighter than general. A bot scraping the catalogue is annoying; a bot hammering checkout is card testing, and that costs per-attempt fees and chargebacks."
  type        = number
  default     = 100
}

variable "rate_limit_auth" {
  description = "Credential stuffing. Low enough to matter, high enough that a shared office NAT is not locked out."
  type        = number
  default     = 30
}

variable "enable_bot_control" {
  description = "Starts in COUNT mode. Blocking on day one always catches a price-comparison partner or the mobile app's own user agent; watch the counts for a fortnight first."
  type        = bool
  default     = true
}

variable "price_class" {
  description = "PriceClass_200 covers Europe, the Middle East and North America. PriceClass_All adds South America and Oceania at roughly 30% more."
  type        = string
  default     = "PriceClass_200"
}

variable "log_retention_days" {
  type    = number
  default = 90
}

variable "tags" {
  type    = map(string)
  default = {}
}

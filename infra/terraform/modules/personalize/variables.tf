variable "name" {
  type = string
}
variable "kms_key_arn" {
  type = string
}
variable "activity_bucket" {
  description = "Where Firehose lands the interaction files Personalize imports from."
  type        = string
}

variable "recipes" {
  description = <<-EOT
    Recipe ARN per use case. Solutions and campaigns are NOT created here —
    the AWS provider has no resource for them because training is a
    long-running job that does not fit an apply. See
    docs/runbooks/personalize-retrain.md.
  EOT
  type        = map(string)
  default     = {}
}

variable "tags" {
  type    = map(string)
  default = {}
}

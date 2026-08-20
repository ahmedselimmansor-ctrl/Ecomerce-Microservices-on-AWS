output "distribution_id"     { value = aws_cloudfront_distribution.this.id }
output "distribution_domain" { value = aws_cloudfront_distribution.this.domain_name }
output "distribution_arn"    { value = aws_cloudfront_distribution.this.arn }

output "certificate_arn" {
  description = "Needs DNS validation before the distribution serves the alias."
  value       = aws_acm_certificate.this.arn
}

output "certificate_validation_records" {
  description = "Create these in Route 53 (or your registrar) to validate the certificate."
  value = [for o in aws_acm_certificate.this.domain_validation_options : {
    name = o.resource_record_name, type = o.resource_record_type, value = o.resource_record_value
  }]
}

output "media_bucket"       { value = aws_s3_bucket.media.id }
output "invoices_bucket"    { value = aws_s3_bucket.invoices.id }
output "personalize_bucket" { value = aws_s3_bucket.personalize.id }
output "logs_bucket"        { value = aws_s3_bucket.logs.id }

output "waf_arn" { value = aws_wafv2_web_acl.this.arn }

output "origin_verify_secret_arn" {
  description = "The ALB listener rule must require this header value, or the WAF can be bypassed by hitting the ALB directly."
  value       = aws_secretsmanager_secret.origin_secret.arn
}

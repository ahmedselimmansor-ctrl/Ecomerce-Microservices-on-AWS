##############################################################################
# Outputs
#
# The values a deploy needs and cannot derive. Everything here is either a
# placeholder the Kubernetes manifests substitute, or an identifier an operator
# needs during an incident.
#
# Nothing secret. A Terraform output lands in the state file and in whatever CI
# job printed it; credentials belong in Secrets Manager, reaching the pods via
# ExternalSecret.
##############################################################################

output "cluster_name" {
  description = "For `aws eks update-kubeconfig`."
  value       = module.eks.cluster_name
}

output "cluster_endpoint" {
  value = module.eks.cluster_endpoint
}

output "jwt_signing_key_arn" {
  description = <<-EOT
    Substituted into the souq-platform ConfigMap as jwt.kms.key.id.

    identity-service refuses to start without it in any environment other than
    local or test — a generated in-memory key would mean every pod signs with a
    different one, so roughly (n-1)/n of verifications across the platform would
    fail. That presents as an intermittent auth bug rather than as an outage,
    which is why the refusal is deliberate.
  EOT
  value       = aws_kms_key.jwt_signing.arn
}

output "jwt_signing_key_alias" {
  description = "Stable across a key replacement; the ARN is not."
  value       = aws_kms_alias.jwt_signing.name
}

output "data_kms_key_arn" {
  value = aws_kms_key.data.arn
}

output "irsa_role_arns" {
  description = "Per-service IAM roles, annotated onto each ServiceAccount."
  value       = module.eks.irsa_role_arns
}

output "cluster_name" {
  value = aws_eks_cluster.this.name
}
output "cluster_endpoint" {
  value = aws_eks_cluster.this.endpoint
}
output "cluster_arn" {
  value = aws_eks_cluster.this.arn
}
output "cluster_ca_certificate" {
  value     = aws_eks_cluster.this.certificate_authority[0].data
  sensitive = true
}

output "oidc_provider_arn" {
  value = aws_iam_openid_connect_provider.this.arn
}
output "oidc_issuer" {
  value = local.oidc_issuer
}
output "node_security_group_id" {
  description = "The only source the data-tier security groups admit."
  value       = aws_eks_cluster.this.vpc_config[0].cluster_security_group_id
}

output "irsa_role_arns" {
  description = "One role per service. Annotate each ServiceAccount with its own; a shared role means search-service can read the payments secret."
  value       = { for k, v in aws_iam_role.irsa : k => v.arn }
}

output "karpenter_node_role_arn" {
  value = var.enable_karpenter ? aws_iam_role.karpenter_node[0].arn : null
}
output "karpenter_controller_role_arn" {
  value = var.enable_karpenter ? aws_iam_role.karpenter_controller[0].arn : null
}
output "karpenter_interruption_queue" {
  value = var.enable_karpenter ? aws_sqs_queue.karpenter_interruption[0].name : null
}
output "alb_dns_name" {
  description = "Populated by the AWS Load Balancer Controller from the Ingress; a placeholder until the first Ingress is reconciled."
  value       = ""
}

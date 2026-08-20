output "prometheus_workspace_id" {
  value = var.enable_managed_prometheus ? aws_prometheus_workspace.this[0].id : null
}
output "prometheus_remote_write_url" {
  description = "The ADOT collector's remote_write target."
  value       = var.enable_managed_prometheus ? "${aws_prometheus_workspace.this[0].prometheus_endpoint}api/v1/remote_write" : null
}
output "grafana_endpoint" {
  value = var.enable_managed_grafana ? aws_grafana_workspace.this[0].endpoint : null
}
output "adot_role_arn"    { value = aws_iam_role.adot.arn }
output "pages_topic_arn"  { value = aws_sns_topic.pages.arn }
output "tickets_topic_arn"{ value = aws_sns_topic.tickets.arn }

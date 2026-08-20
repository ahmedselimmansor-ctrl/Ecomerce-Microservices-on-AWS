output "dataset_group_arn" { value = aws_personalize_dataset_group.this.arn }
output "interactions_dataset_arn" { value = aws_personalize_dataset.interactions.arn }
output "items_dataset_arn"        { value = aws_personalize_dataset.items.arn }
output "users_dataset_arn"        { value = aws_personalize_dataset.users.arn }
output "firehose_stream_name"     { value = aws_kinesis_firehose_delivery_stream.activity.name }
output "personalize_role_arn"     { value = aws_iam_role.personalize.arn }

output "recipes" {
  description = "Passed through for the retrain runbook to consume."
  value       = var.recipes
}

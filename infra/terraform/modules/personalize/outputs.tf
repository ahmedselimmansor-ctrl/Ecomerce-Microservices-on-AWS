output "dataset_group_arn" {
  value = awscc_personalize_dataset_group.this.dataset_group_arn
}
output "interactions_dataset_arn" {
  value = awscc_personalize_dataset.interactions.dataset_arn
}
output "items_dataset_arn" {
  value = awscc_personalize_dataset.items.dataset_arn
}
output "users_dataset_arn" {
  value = awscc_personalize_dataset.users.dataset_arn
}
output "firehose_stream_name" {
  value = aws_kinesis_firehose_delivery_stream.activity.name
}
output "personalize_role_arn" {
  value = aws_iam_role.personalize.arn
}
output "recipes" {
  description = "Passed through for the retrain runbook to consume."
  value       = var.recipes
}

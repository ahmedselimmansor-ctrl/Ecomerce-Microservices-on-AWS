output "postgres_endpoints" {
  description = "Writer endpoint per service."
  value       = { for k, v in aws_rds_cluster.postgres : k => v.endpoint }
}

output "postgres_reader_endpoints" {
  value = { for k, v in aws_rds_cluster.postgres : k => v.reader_endpoint }
}

output "postgres_secret_arns" {
  description = "External Secrets projects these into the pods. The connection string never exists in a manifest."
  value       = { for k, v in aws_secretsmanager_secret.postgres : k => v.arn }
}

output "mysql_endpoint" {
  value = aws_rds_cluster.mysql.endpoint
}

output "kafka_bootstrap_brokers" {
  description = "IAM SASL over TLS, port 9098. There is no plaintext listener."
  value       = aws_msk_cluster.this.bootstrap_brokers_sasl_iam
}

output "kafka_cluster_arn" {
  description = "Needed by the IRSA policies that grant per-topic access."
  value       = aws_msk_cluster.this.arn
}

output "redis_endpoint" {
  value = aws_elasticache_replication_group.redis.configuration_endpoint_address
}

output "redis_secret_arn" {
  value = aws_secretsmanager_secret.redis.arn
}

output "opensearch_endpoint" {
  value = aws_opensearch_domain.search.endpoint
}

output "opensearch_arn" {
  value = aws_opensearch_domain.search.arn
}

output "documentdb_endpoint" {
  value = aws_docdb_cluster.reviews.endpoint
}

output "documentdb_secret_arn" {
  value = aws_secretsmanager_secret.docdb.arn
}

output "keyspaces_keyspace" {
  value = aws_keyspaces_keyspace.notifications.name
}

output "security_group_ids" {
  description = "Per-engine, so a NetworkPolicy or an audit can reason about them individually."
  value = {
    postgres   = aws_security_group.postgres.id
    redis      = aws_security_group.redis.id
    kafka      = aws_security_group.kafka.id
    opensearch = aws_security_group.search.id
    documentdb = aws_security_group.documentdb.id
  }
}

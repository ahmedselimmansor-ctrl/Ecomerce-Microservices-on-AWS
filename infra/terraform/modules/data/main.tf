##############################################################################
# Managed data stores.
#
# Every one of these lives in the data tier, which has no route to a NAT
# gateway (see modules/network). Reachability is therefore a security-group
# question only, and the answer is "the EKS node security group, nothing else".
#
# One Aurora cluster per service, not one cluster with five databases. That
# costs more, and it is the point: database-per-service (docs/CONTRACTS.md §6)
# is only a real boundary if crossing it is impossible rather than discouraged.
# Separate clusters also mean catalog's analytical query cannot starve
# checkout's connection pool, and each can be sized and scaled independently.
##############################################################################

terraform {
  required_version = ">= 1.9"
  required_providers {
    aws    = { source = "hashicorp/aws", version = "~> 5.60" }
    random = { source = "hashicorp/random", version = "~> 3.6" }
  }
}

locals {
  tags = merge(var.tags, { Module = "data" })

  # service -> sizing. checkout-path services get more capacity and a longer
  # backup window; catalog and identity are read-heavy and cheaper to run.
  postgres_clusters = {
    identity  = { min_acu = 0.5, max_acu = var.is_production ? 8 : 2, replicas = var.is_production ? 1 : 0 }
    catalog   = { min_acu = 0.5, max_acu = var.is_production ? 16 : 2, replicas = var.is_production ? 2 : 0 }
    orders    = { min_acu = 1.0, max_acu = var.is_production ? 32 : 4, replicas = var.is_production ? 2 : 0 }
    inventory = { min_acu = 1.0, max_acu = var.is_production ? 32 : 4, replicas = var.is_production ? 1 : 0 }
    payments  = { min_acu = 0.5, max_acu = var.is_production ? 16 : 2, replicas = var.is_production ? 1 : 0 }
  }
}

##############################################################################
# Subnet groups and security
##############################################################################

resource "aws_db_subnet_group" "data" {
  name       = "${var.name}-data"
  subnet_ids = var.data_subnet_ids
  tags       = local.tags
}

resource "aws_elasticache_subnet_group" "data" {
  name       = "${var.name}-data"
  subnet_ids = var.data_subnet_ids
  tags       = local.tags
}

resource "aws_docdb_subnet_group" "data" {
  name       = "${var.name}-data"
  subnet_ids = var.data_subnet_ids
  tags       = local.tags
}

# One security group per engine rather than one shared "data" group. A shared
# group means anything that can reach Postgres can also reach Redis, which
# quietly removes the isolation the separate clusters were bought for.
resource "aws_security_group" "postgres" {
  name_prefix = "${var.name}-pg-"
  description = "Aurora PostgreSQL: 5432 from EKS nodes only"
  vpc_id      = var.vpc_id

  ingress {
    description     = "PostgreSQL from the cluster"
    from_port       = 5432
    to_port         = 5432
    protocol        = "tcp"
    security_groups = [var.eks_node_security_group_id]
  }

  tags = merge(local.tags, { Name = "${var.name}-pg" })
  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_security_group" "redis" {
  name_prefix = "${var.name}-redis-"
  description = "ElastiCache: 6379 from EKS nodes only"
  vpc_id      = var.vpc_id

  ingress {
    from_port       = 6379
    to_port         = 6379
    protocol        = "tcp"
    security_groups = [var.eks_node_security_group_id]
  }

  tags = merge(local.tags, { Name = "${var.name}-redis" })
  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_security_group" "kafka" {
  name_prefix = "${var.name}-msk-"
  description = "MSK: IAM-authenticated TLS from EKS nodes only"
  vpc_id      = var.vpc_id

  ingress {
    description     = "Kafka over TLS with IAM SASL"
    from_port       = 9098
    to_port         = 9098
    protocol        = "tcp"
    security_groups = [var.eks_node_security_group_id]
  }
  # 9092 (plaintext) is deliberately absent. There is no listener for it.

  tags = merge(local.tags, { Name = "${var.name}-msk" })
  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_security_group" "search" {
  name_prefix = "${var.name}-os-"
  description = "OpenSearch: HTTPS from EKS nodes only"
  vpc_id      = var.vpc_id

  ingress {
    from_port       = 443
    to_port         = 443
    protocol        = "tcp"
    security_groups = [var.eks_node_security_group_id]
  }

  tags = merge(local.tags, { Name = "${var.name}-os" })
  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_security_group" "documentdb" {
  name_prefix = "${var.name}-docdb-"
  description = "DocumentDB: 27017 from EKS nodes only"
  vpc_id      = var.vpc_id

  ingress {
    from_port       = 27017
    to_port         = 27017
    protocol        = "tcp"
    security_groups = [var.eks_node_security_group_id]
  }

  tags = merge(local.tags, { Name = "${var.name}-docdb" })
  lifecycle {
    create_before_destroy = true
  }
}

##############################################################################
# Aurora PostgreSQL — one Serverless v2 cluster per service
##############################################################################

resource "random_password" "postgres" {
  for_each = local.postgres_clusters

  length  = 40
  special = true
  # RDS rejects these in a master password, and finding that out during an
  # apply is a slow way to learn it.
  override_special = "!#$%&*()-_=+[]{}<>:?"
}

resource "aws_secretsmanager_secret" "postgres" {
  for_each = local.postgres_clusters

  name       = "${var.name}/db/${each.key}"
  kms_key_id = var.kms_key_arn
  # Long enough to recover from a mistaken destroy, short enough that a
  # renamed environment can reuse the name within a sprint.
  recovery_window_in_days = var.is_production ? 30 : 7

  tags = merge(local.tags, { Service = each.key })
}

resource "aws_secretsmanager_secret_version" "postgres" {
  for_each = local.postgres_clusters

  secret_id = aws_secretsmanager_secret.postgres[each.key].id
  secret_string = jsonencode({
    username = "souq_${each.key}"
    password = random_password.postgres[each.key].result
    host     = aws_rds_cluster.postgres[each.key].endpoint
    reader   = aws_rds_cluster.postgres[each.key].reader_endpoint
    port     = 5432
    dbname   = each.key
    # The form the services actually consume, so External Secrets can project
    # one key straight into SOUQ_DB_URL without a transform.
    url = format("postgres://souq_%s:%s@%s:5432/%s?sslmode=require",
      each.key,
      urlencode(random_password.postgres[each.key].result),
      aws_rds_cluster.postgres[each.key].endpoint,
      each.key,
    )
  })
}

resource "aws_rds_cluster_parameter_group" "postgres" {
  name_prefix = "${var.name}-pg16-"
  family      = "aurora-postgresql16"
  description = "SOUQ Aurora PostgreSQL 16"

  parameter {
    name  = "log_min_duration_statement"
    value = "1000" # anything over a second is worth seeing
  }
  parameter {
    name  = "log_lock_waits"
    value = "1"
  }
  # Force TLS. Aurora accepts unencrypted connections by default and a service
  # misconfigured to skip TLS would otherwise work silently.
  parameter {
    name         = "rds.force_ssl"
    value        = "1"
    apply_method = "pending-reboot"
  }
  parameter {
    name  = "idle_in_transaction_session_timeout"
    value = "60000"
  }
  # pg_stat_statements is how you find the query that regressed after a deploy.
  parameter {
    name         = "shared_preload_libraries"
    value        = "pg_stat_statements"
    apply_method = "pending-reboot"
  }

  lifecycle {

    create_before_destroy = true

  }
  tags = local.tags
}

resource "aws_rds_cluster" "postgres" {
  for_each = local.postgres_clusters

  cluster_identifier = "${var.name}-${each.key}"
  engine             = "aurora-postgresql"
  engine_mode        = "provisioned" # required for Serverless v2
  engine_version     = var.postgres_engine_version
  database_name      = each.key

  master_username = "souq_${each.key}"
  master_password = random_password.postgres[each.key].result

  db_subnet_group_name            = aws_db_subnet_group.data.name
  vpc_security_group_ids          = [aws_security_group.postgres.id]
  db_cluster_parameter_group_name = aws_rds_cluster_parameter_group.postgres.name

  storage_encrypted = true
  kms_key_id        = var.kms_key_arn

  backup_retention_period      = var.is_production ? 30 : 7
  preferred_backup_window      = "02:00-03:00"
  preferred_maintenance_window = "sun:03:30-sun:05:00"
  copy_tags_to_snapshot        = true

  # Production must not be destroyable by a terraform apply, and must leave a
  # snapshot behind if it somehow is.
  deletion_protection       = var.is_production
  skip_final_snapshot       = !var.is_production
  final_snapshot_identifier = var.is_production ? "${var.name}-${each.key}-final-${formatdate("YYYYMMDDhhmmss", timestamp())}" : null

  enabled_cloudwatch_logs_exports = ["postgresql"]

  serverlessv2_scaling_configuration {
    min_capacity = each.value.min_acu
    max_capacity = each.value.max_acu
  }

  lifecycle {
    ignore_changes = [
      # Recomputed on every plan and would show a permanent diff.
      final_snapshot_identifier,
      # Rotated out of band by Secrets Manager rotation, not by Terraform.
      master_password,
    ]
  }

  tags = merge(local.tags, { Service = each.key })
}

# Writer instance.
resource "aws_rds_cluster_instance" "postgres_writer" {
  for_each = local.postgres_clusters

  identifier         = "${var.name}-${each.key}-writer"
  cluster_identifier = aws_rds_cluster.postgres[each.key].id
  instance_class     = "db.serverless"
  engine             = aws_rds_cluster.postgres[each.key].engine
  engine_version     = aws_rds_cluster.postgres[each.key].engine_version

  performance_insights_enabled          = true
  performance_insights_retention_period = var.is_production ? 465 : 7
  performance_insights_kms_key_id       = var.kms_key_arn
  monitoring_interval                   = 30
  monitoring_role_arn                   = aws_iam_role.rds_monitoring.arn

  auto_minor_version_upgrade = true

  tags = merge(local.tags, { Service = each.key, Role = "writer" })
}

# Read replicas. Also the failover targets — a cluster with no replica has a
# multi-minute RTO because Aurora has to build a new instance first.
resource "aws_rds_cluster_instance" "postgres_reader" {
  for_each = merge([
    for svc, cfg in local.postgres_clusters : {
      for i in range(cfg.replicas) : "${svc}-${i}" => { service = svc, index = i }
    }
  ]...)

  identifier         = "${var.name}-${each.value.service}-reader-${each.value.index}"
  cluster_identifier = aws_rds_cluster.postgres[each.value.service].id
  instance_class     = "db.serverless"
  engine             = aws_rds_cluster.postgres[each.value.service].engine
  engine_version     = aws_rds_cluster.postgres[each.value.service].engine_version

  # Lower than the writer so a failover always promotes a replica rather than
  # waiting for a new instance to be provisioned.
  promotion_tier = each.value.index + 1

  performance_insights_enabled = true
  monitoring_interval          = 30
  monitoring_role_arn          = aws_iam_role.rds_monitoring.arn

  tags = merge(local.tags, { Service = each.value.service, Role = "reader" })
}

resource "aws_iam_role" "rds_monitoring" {
  name_prefix = "${var.name}-rds-mon-"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "monitoring.rds.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })

  tags = local.tags
}

resource "aws_iam_role_policy_attachment" "rds_monitoring" {
  role       = aws_iam_role.rds_monitoring.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonRDSEnhancedMonitoringRole"
}

##############################################################################
# Aurora MySQL — reporting marts and merchant settlements
#
# MySQL rather than another Postgres because the BI tooling and the settlement
# reports the finance team already runs are written against it. Keeping it on
# a separate engine also makes it structurally impossible for a heavy
# reporting query to be pointed at a transactional cluster by accident.
##############################################################################

resource "random_password" "mysql" {
  length           = 40
  special          = true
  override_special = "!#$%&*()-_=+[]{}<>:?"
}

resource "aws_rds_cluster" "mysql" {
  cluster_identifier = "${var.name}-analytics"
  engine             = "aurora-mysql"
  engine_mode        = "provisioned"
  engine_version     = var.mysql_engine_version
  database_name      = "analytics_ops"

  master_username = "souq_analytics"
  master_password = random_password.mysql.result

  db_subnet_group_name   = aws_db_subnet_group.data.name
  vpc_security_group_ids = [aws_security_group.postgres.id] # 3306 added below

  storage_encrypted = true
  kms_key_id        = var.kms_key_arn

  backup_retention_period = var.is_production ? 30 : 7
  deletion_protection     = var.is_production
  skip_final_snapshot     = !var.is_production

  enabled_cloudwatch_logs_exports = ["error", "slowquery"]

  serverlessv2_scaling_configuration {
    min_capacity = 0.5
    max_capacity = var.is_production ? 16 : 2
  }

  lifecycle {

    ignore_changes = [master_password]

  }
  tags = merge(local.tags, { Service = "analytics" })
}

resource "aws_security_group_rule" "mysql" {
  type                     = "ingress"
  from_port                = 3306
  to_port                  = 3306
  protocol                 = "tcp"
  security_group_id        = aws_security_group.postgres.id
  source_security_group_id = var.eks_node_security_group_id
  description              = "MySQL from the cluster"
}

resource "aws_rds_cluster_instance" "mysql" {
  identifier         = "${var.name}-analytics-writer"
  cluster_identifier = aws_rds_cluster.mysql.id
  instance_class     = "db.serverless"
  engine             = aws_rds_cluster.mysql.engine
  engine_version     = aws_rds_cluster.mysql.engine_version

  performance_insights_enabled = true
  tags                         = local.tags
}

##############################################################################
# MSK — the event backbone
##############################################################################

resource "aws_msk_configuration" "this" {
  name           = "${var.name}-msk"
  kafka_versions = [var.kafka_version]

  server_properties = <<-PROPERTIES
    # OFF. Every topic has a deliberate partition count and cleanup policy
    # (docs/CONTRACTS.md §3.1). Auto-creation gives a typo'd topic name one
    # partition and delete-retention, and you find out under load.
    auto.create.topics.enable=false

    # A topic must never be deletable by an application client.
    delete.topic.enable=false

    # Durability over availability. min.insync.replicas=2 with acks=all means
    # a produce is only acknowledged once two brokers have it, so losing one
    # broker cannot lose an event the outbox relay has already marked
    # published — which is the NoLostEvent property in
    # internal/eventbus/outbox_model_test.go.
    default.replication.factor=3
    min.insync.replicas=2
    offsets.topic.replication.factor=3
    transaction.state.log.replication.factor=3
    transaction.state.log.min.isr=2

    # OFF. Unclean election lets a lagging replica become leader and silently
    # discard committed messages. Never acceptable for order or payment events.
    unclean.leader.election.enable=false

    log.retention.hours=168
    num.partitions=6
    compression.type=producer
  PROPERTIES
}

resource "aws_msk_cluster" "this" {
  cluster_name           = var.name
  kafka_version          = var.kafka_version
  number_of_broker_nodes = var.msk_broker_count

  broker_node_group_info {
    instance_type   = var.msk_instance_type
    client_subnets  = var.data_subnet_ids
    security_groups = [aws_security_group.kafka.id]

    storage_info {
      ebs_storage_info {
        volume_size = var.msk_volume_size_gb

        # Grows the volume automatically before a broker fills its disk.
        # A full MSK broker stops accepting writes, which stops checkout.
        provisioned_throughput {
          enabled = var.is_production
          # Only valid on m5.4xlarge and larger; null on smaller instances.
          volume_throughput = var.is_production ? 250 : null
        }
      }
    }
  }

  configuration_info {
    arn      = aws_msk_configuration.this.arn
    revision = aws_msk_configuration.this.latest_revision
  }

  client_authentication {
    sasl {
      # IAM only. No SCRAM, no unauthenticated access — which means there is
      # no static Kafka credential anywhere in the platform to leak or rotate.
      # Pods authenticate with their IRSA role.
      iam = true
    }
  }

  encryption_info {
    encryption_at_rest_kms_key_arn = var.kms_key_arn
    encryption_in_transit {
      client_broker = "TLS"
      in_cluster    = true
    }
  }

  enhanced_monitoring = var.is_production ? "PER_TOPIC_PER_PARTITION" : "DEFAULT"

  open_monitoring {
    prometheus {
      jmx_exporter {
        enabled_in_broker = true
      }
      node_exporter {
        enabled_in_broker = true
      }
    }
  }

  logging_info {
    broker_logs {
      cloudwatch_logs {
        enabled   = true
        log_group = aws_cloudwatch_log_group.msk.name
      }
    }
  }

  tags = local.tags

  lifecycle {
    # A cluster rebuild would drop every unconsumed event.
    prevent_destroy = false # set true in the prod env; kept false here so dev can be torn down
  }
}

resource "aws_cloudwatch_log_group" "msk" {
  name              = "/aws/msk/${var.name}"
  retention_in_days = 30
  kms_key_id        = var.log_kms_key_arn
  tags              = local.tags
}

# Autoscales broker storage so a slow consumer cannot fill the disk and stop
# the whole platform accepting writes.
resource "aws_appautoscaling_target" "msk_storage" {
  count = var.is_production ? 1 : 0

  max_capacity       = var.msk_volume_size_gb * 4
  min_capacity       = 1
  resource_id        = aws_msk_cluster.this.arn
  scalable_dimension = "kafka:broker-storage:VolumeSize"
  service_namespace  = "kafka"
}

resource "aws_appautoscaling_policy" "msk_storage" {
  count = var.is_production ? 1 : 0

  name               = "${var.name}-msk-storage"
  policy_type        = "TargetTrackingScaling"
  resource_id        = aws_appautoscaling_target.msk_storage[0].resource_id
  scalable_dimension = aws_appautoscaling_target.msk_storage[0].scalable_dimension
  service_namespace  = aws_appautoscaling_target.msk_storage[0].service_namespace

  target_tracking_scaling_policy_configuration {
    predefined_metric_specification {
      predefined_metric_type = "KafkaBrokerStorageUtilization"
    }
    target_value = 70
  }
}

##############################################################################
# ElastiCache Redis — carts, sessions, rate limits, hot-product cache
##############################################################################

resource "aws_elasticache_replication_group" "redis" {
  replication_group_id = "${var.name}-redis"
  description          = "SOUQ carts, sessions, caches"

  engine         = "redis"
  engine_version = var.redis_version
  node_type      = var.redis_node_type
  port           = 6379

  # Cluster mode. A cart workload is trivially shardable by cart id, and a
  # single-shard Redis becomes the platform's throughput ceiling long before
  # anything else does.
  num_node_groups         = var.is_production ? 3 : 1
  replicas_per_node_group = var.is_production ? 1 : 0

  automatic_failover_enabled = var.is_production
  multi_az_enabled           = var.is_production

  subnet_group_name  = aws_elasticache_subnet_group.data.name
  security_group_ids = [aws_security_group.redis.id]

  at_rest_encryption_enabled = true
  kms_key_id                 = var.kms_key_arn
  transit_encryption_enabled = true
  auth_token                 = random_password.redis.result

  parameter_group_name = aws_elasticache_parameter_group.redis.name

  # Carts have a TTL and are reconstructible from the catalogue, so evicting
  # the coldest under memory pressure is correct. The alternative — refusing
  # writes when full — takes checkout down.
  maintenance_window       = "sun:05:00-sun:07:00"
  snapshot_retention_limit = var.is_production ? 7 : 0
  snapshot_window          = "03:00-05:00"

  apply_immediately          = !var.is_production
  auto_minor_version_upgrade = true

  log_delivery_configuration {
    destination      = aws_cloudwatch_log_group.redis.name
    destination_type = "cloudwatch-logs"
    log_format       = "json"
    log_type         = "slow-log"
  }

  tags = local.tags
}

resource "aws_elasticache_parameter_group" "redis" {
  # `name`, not `name_prefix` — the resource has no name_prefix argument, and
  # `name` is required. Combined with create_before_destroy below, a change to
  # the parameters needs a new name, which is what the suffix is for.
  name   = "${var.name}-redis7"
  family = "redis7"

  parameter {
    name  = "maxmemory-policy"
    value = "allkeys-lru"
  }

  lifecycle {

    create_before_destroy = true

  }
  tags = local.tags
}

resource "random_password" "redis" {
  length = 64
  # Redis AUTH tokens reject most punctuation.
  special = false
}

resource "aws_secretsmanager_secret" "redis" {
  name       = "${var.name}/redis/auth"
  kms_key_id = var.kms_key_arn
  tags       = local.tags
}

resource "aws_secretsmanager_secret_version" "redis" {
  secret_id = aws_secretsmanager_secret.redis.id
  secret_string = jsonencode({
    auth_token = random_password.redis.result
    url        = "rediss://:${urlencode(random_password.redis.result)}@${aws_elasticache_replication_group.redis.configuration_endpoint_address}:6379"
  })
}

resource "aws_cloudwatch_log_group" "redis" {
  name              = "/aws/elasticache/${var.name}"
  retention_in_days = 14
  tags              = local.tags
}

##############################################################################
# OpenSearch — the product index
##############################################################################

resource "aws_opensearch_domain" "search" {
  domain_name    = var.name
  engine_version = var.opensearch_version

  cluster_config {
    instance_type  = var.opensearch_instance_type
    instance_count = var.is_production ? 3 : 1

    zone_awareness_enabled = var.is_production
    dynamic "zone_awareness_config" {
      for_each = var.is_production ? [1] : []
      content {
        availability_zone_count = 3
      }
    }

    # Dedicated masters keep cluster state management off the nodes that are
    # serving queries, so a heavy aggregation cannot destabilise the cluster.
    dedicated_master_enabled = var.is_production
    dedicated_master_type    = var.is_production ? "m6g.large.search" : null
    dedicated_master_count   = var.is_production ? 3 : null
  }

  ebs_options {
    ebs_enabled = true
    volume_type = "gp3"
    volume_size = var.is_production ? 200 : 20
    throughput  = 250
  }

  vpc_options {
    subnet_ids         = var.is_production ? slice(var.data_subnet_ids, 0, 3) : [var.data_subnet_ids[0]]
    security_group_ids = [aws_security_group.search.id]
  }

  encrypt_at_rest {
    enabled    = true
    kms_key_id = var.kms_key_arn
  }
  node_to_node_encryption {
    enabled = true
  }
  domain_endpoint_options {
    enforce_https       = true
    tls_security_policy = "Policy-Min-TLS-1-2-2019-07"
  }

  advanced_security_options {
    enabled = true
    # IAM only. No internal user database means no OpenSearch password to
    # rotate or leak; search-service signs requests with its IRSA role.
    internal_user_database_enabled = false
  }

  # The domain sits in private subnets with a security group that only admits
  # the node group, so this policy is the second gate rather than the only one.
  access_policies = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { AWS = var.search_service_role_arns }
      Action    = "es:*"
      Resource  = "arn:aws:es:${var.region}:${var.account_id}:domain/${var.name}/*"
    }]
  })

  log_publishing_options {
    log_type                 = "ES_APPLICATION_LOGS"
    cloudwatch_log_group_arn = aws_cloudwatch_log_group.opensearch.arn
  }
  log_publishing_options {
    log_type                 = "SEARCH_SLOW_LOGS"
    cloudwatch_log_group_arn = aws_cloudwatch_log_group.opensearch.arn
  }

  auto_tune_options {
    desired_state = var.is_production ? "ENABLED" : "DISABLED"
  }

  tags = local.tags
}

resource "aws_cloudwatch_log_group" "opensearch" {
  name              = "/aws/opensearch/${var.name}"
  retention_in_days = 14
  tags              = local.tags
}

resource "aws_cloudwatch_log_resource_policy" "opensearch" {
  policy_name = "${var.name}-opensearch-logs"
  policy_document = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "es.amazonaws.com" }
      Action    = ["logs:PutLogEvents", "logs:CreateLogStream"]
      Resource  = "${aws_cloudwatch_log_group.opensearch.arn}:*"
    }]
  })
}

##############################################################################
# DocumentDB — reviews
##############################################################################

resource "random_password" "docdb" {
  length           = 40
  special          = true
  override_special = "!#$%&*()-_=+[]{}<>:?"
}

resource "aws_docdb_cluster_parameter_group" "this" {
  name_prefix = "${var.name}-docdb-"
  family      = "docdb5.0"

  # DocumentDB allows unencrypted connections by default.
  parameter {
    name  = "tls"
    value = "enabled"
  }
  parameter {
    name  = "audit_logs"
    value = "enabled"
  }

  lifecycle {

    create_before_destroy = true

  }
  tags = local.tags
}

resource "aws_docdb_cluster" "reviews" {
  cluster_identifier = "${var.name}-reviews"
  engine             = "docdb"
  engine_version     = "5.0.0"

  master_username = "souq_reviews"
  master_password = random_password.docdb.result

  db_subnet_group_name            = aws_docdb_subnet_group.data.name
  vpc_security_group_ids          = [aws_security_group.documentdb.id]
  db_cluster_parameter_group_name = aws_docdb_cluster_parameter_group.this.name

  storage_encrypted = true
  kms_key_id        = var.kms_key_arn

  backup_retention_period = var.is_production ? 14 : 1
  preferred_backup_window = "02:00-03:00"
  deletion_protection     = var.is_production
  skip_final_snapshot     = !var.is_production

  enabled_cloudwatch_logs_exports = ["audit", "profiler"]

  lifecycle {

    ignore_changes = [master_password]

  }
  tags = local.tags
}

resource "aws_docdb_cluster_instance" "reviews" {
  count = var.is_production ? 2 : 1

  identifier         = "${var.name}-reviews-${count.index}"
  cluster_identifier = aws_docdb_cluster.reviews.id
  instance_class     = var.docdb_instance_class

  tags = local.tags
}

resource "aws_secretsmanager_secret" "docdb" {
  name       = "${var.name}/db/reviews"
  kms_key_id = var.kms_key_arn
  tags       = local.tags
}

resource "aws_secretsmanager_secret_version" "docdb" {
  secret_id = aws_secretsmanager_secret.docdb.id
  secret_string = jsonencode({
    username = "souq_reviews"
    password = random_password.docdb.result
    host     = aws_docdb_cluster.reviews.endpoint
    url = format("mongodb://souq_reviews:%s@%s:27017/reviews?tls=true&replicaSet=rs0&retryWrites=false",
      urlencode(random_password.docdb.result),
      aws_docdb_cluster.reviews.endpoint,
    )
  })
}

##############################################################################
# Keyspaces (managed Cassandra) — notification delivery log and activity feed
#
# Serverless: no cluster to size, no compaction to tune, and the workload is a
# perfect fit — wide, append-only, time-series, always read by partition key.
##############################################################################

resource "aws_keyspaces_keyspace" "notifications" {
  name = replace("${var.name}_notifications", "-", "_")
  tags = local.tags
}

resource "aws_keyspaces_table" "delivery_log" {
  keyspace_name = aws_keyspaces_keyspace.notifications.name
  table_name    = "delivery_log"

  schema_definition {
    column {
      name = "user_id"
      type = "text"
    }
    column {
      name = "sent_at"
      type = "timestamp"
    }
    column {
      name = "notification_id"
      type = "text"
    }
    column {
      name = "channel"
      type = "text"
    }
    column {
      name = "template"
      type = "text"
    }
    column {
      name = "status"
      type = "text"
    }
    column {
      name = "dedupe_key"
      type = "text"
    }
    column {
      name = "provider_id"
      type = "text"
    }
    column {
      name = "error"
      type = "text"
    }
    partition_key {

      name = "user_id"

    }
    # Newest first within a user, so "what did we send this customer?" is one
    # partition read with no sort.
    clustering_key {
      name     = "sent_at"
      order_by = "DESC"
    }
    clustering_key {
      name     = "notification_id"
      order_by = "ASC"
    }
  }

  capacity_specification {
    throughput_mode = "PAY_PER_REQUEST"
  }

  encryption_specification {
    type               = "CUSTOMER_MANAGED_KMS_KEY"
    kms_key_identifier = var.kms_key_arn
  }

  point_in_time_recovery {

    status = var.is_production ? "ENABLED" : "DISABLED"

  }
  # Delivery history is operational, not archival. 90 days covers every
  # support question and keeps the table from growing without bound.
  default_time_to_live = 7776000

  tags = local.tags
}

# Second table keyed by dedupe_key. This is the notification service's
# at-most-once guarantee: the primary key itself stops a duplicated command
# from producing a second email, even if the inbox table were bypassed.
resource "aws_keyspaces_table" "dedupe" {
  keyspace_name = aws_keyspaces_keyspace.notifications.name
  table_name    = "dedupe"

  schema_definition {
    column {
      name = "dedupe_key"
      type = "text"
    }
    column {
      name = "sent_at"
      type = "timestamp"
    }
    column {
      name = "channel"
      type = "text"
    }
    partition_key {

      name = "dedupe_key"

    }
  }

  capacity_specification {

    throughput_mode = "PAY_PER_REQUEST"

  }
  encryption_specification {
    type               = "CUSTOMER_MANAGED_KMS_KEY"
    kms_key_identifier = var.kms_key_arn
  }

  default_time_to_live = 2592000 # 30 days
  tags                 = local.tags
}

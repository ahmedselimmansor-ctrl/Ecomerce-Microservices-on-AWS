##############################################################################
# Amazon Personalize.
#
# Terraform's coverage of Personalize is partial: dataset groups, schemas and
# datasets are manageable, but solutions and campaigns are not (the provider
# has no resource for them, because training is a long-running job whose
# lifecycle does not fit an apply). So this module provisions everything up to
# and including the datasets and the import pipeline, and the solution/campaign
# lifecycle is driven by the runbook in docs/runbooks/personalize-retrain.md.
#
# That split is stated plainly rather than papered over with a null_resource
# running the CLI: a `terraform apply` that silently blocks for 90 minutes on a
# model training job is worse than a documented manual step.
##############################################################################

terraform {
  required_version = ">= 1.9"
  required_providers {
    aws = { source = "hashicorp/aws", version = "~> 5.60" }
  }
}

locals { tags = merge(var.tags, { Module = "personalize" }) }

resource "aws_personalize_dataset_group" "this" {
  name        = var.name
  kms_key_arn = var.kms_key_arn
  role_arn    = aws_iam_role.personalize.arn
  tags        = local.tags
}

# The interactions schema. Personalize is strict about this: a field it does
# not recognise fails the import with a message that does not name the field.
resource "aws_personalize_schema" "interactions" {
  name = "${var.name}-interactions"
  schema = jsonencode({
    type      = "record"
    name      = "Interactions"
    namespace = "com.amazonaws.personalize.schema"
    fields = [
      { name = "USER_ID", type = "string" },
      { name = "ITEM_ID", type = "string" },
      { name = "TIMESTAMP", type = "long" },
      # EVENT_TYPE lets one dataset carry views, cart adds and purchases, and
      # lets the recipe weight them differently. Splitting them into separate
      # datasets loses the sequence, which is most of the signal.
      { name = "EVENT_TYPE", type = "string" },
      { name = "EVENT_VALUE", type = ["null", "float"] },
      # Contextual metadata measurably improves recommendations, but ONLY if
      # the same keys are supplied at inference time. A key sent at inference
      # that was not in training is silently ignored.
      { name = "DEVICE_TYPE", type = ["null", "string"], categorical = true },
      { name = "LOCALE", type = ["null", "string"], categorical = true },
      { name = "IMPRESSION", type = ["null", "string"] },
    ]
    version = "1.0"
  })
}

resource "aws_personalize_schema" "items" {
  name = "${var.name}-items"
  schema = jsonencode({
    type      = "record"
    name      = "Items"
    namespace = "com.amazonaws.personalize.schema"
    fields = [
      { name = "ITEM_ID", type = "string" },
      { name = "CATEGORY_L1", type = ["null", "string"], categorical = true },
      { name = "CATEGORY_L2", type = ["null", "string"], categorical = true },
      { name = "BRAND", type = ["null", "string"], categorical = true },
      { name = "PRICE", type = ["null", "float"] },
      # CREATION_TIMESTAMP is what lets Personalize handle cold-start items at
      # all. Omitting it means every new product is invisible to the model
      # until it accumulates interactions it can only get by being visible.
      { name = "CREATION_TIMESTAMP", type = "long" },
    ]
    version = "1.0"
  })
}

resource "aws_personalize_schema" "users" {
  name = "${var.name}-users"
  schema = jsonencode({
    type      = "record"
    name      = "Users"
    namespace = "com.amazonaws.personalize.schema"
    fields = [
      { name = "USER_ID", type = "string" },
      { name = "SEGMENT", type = ["null", "string"], categorical = true },
      { name = "COUNTRY", type = ["null", "string"], categorical = true },
    ]
    version = "1.0"
  })
}

resource "aws_personalize_dataset" "interactions" {
  name              = "${var.name}-interactions"
  dataset_group_arn = aws_personalize_dataset_group.this.arn
  dataset_type      = "Interactions"
  schema_arn        = aws_personalize_schema.interactions.arn
}

resource "aws_personalize_dataset" "items" {
  name              = "${var.name}-items"
  dataset_group_arn = aws_personalize_dataset_group.this.arn
  dataset_type      = "Items"
  schema_arn        = aws_personalize_schema.items.arn
}

resource "aws_personalize_dataset" "users" {
  name              = "${var.name}-users"
  dataset_group_arn = aws_personalize_dataset_group.this.arn
  dataset_type      = "Users"
  schema_arn        = aws_personalize_schema.users.arn
}

##############################################################################
# Firehose: souq.user.activity.v1 -> S3, in the shape Personalize imports.
#
# Firehose rather than a Lambda consumer because the transformation is a field
# rename and Firehose buffers, compresses and partitions for free. A Lambda
# would be more code doing less.
##############################################################################

resource "aws_kinesis_firehose_delivery_stream" "activity" {
  name        = "${var.name}-activity-to-s3"
  destination = "extended_s3"

  extended_s3_configuration {
    role_arn   = aws_iam_role.firehose.arn
    bucket_arn = "arn:aws:s3:::${var.activity_bucket}"
    prefix     = "interactions/dt=!{timestamp:yyyy-MM-dd}/"
    error_output_prefix = "errors/!{firehose:error-output-type}/dt=!{timestamp:yyyy-MM-dd}/"

    # 128MB or 5 minutes. Personalize imports are batch, so latency does not
    # matter; file size does — thousands of tiny objects make an import job
    # slow and expensive.
    buffering_size     = 128
    buffering_interval = 300
    compression_format = "GZIP"

    processing_configuration {
      enabled = true
      processors {
        type = "Lambda"
        parameters {
          parameter_name  = "LambdaArn"
          parameter_value = "${aws_lambda_function.transform.arn}:$LATEST"
        }
      }
    }

    cloudwatch_logging_options {
      enabled         = true
      log_group_name  = aws_cloudwatch_log_group.firehose.name
      log_stream_name = "s3-delivery"
    }
  }

  tags = local.tags
}

resource "aws_cloudwatch_log_group" "firehose" {
  name              = "/aws/firehose/${var.name}-activity"
  retention_in_days = 14
  tags              = local.tags
}

resource "aws_lambda_function" "transform" {
  function_name = "${var.name}-activity-transform"
  role          = aws_iam_role.lambda.arn
  handler       = "index.handler"
  runtime       = "python3.12"
  timeout       = 60
  memory_size   = 256

  filename         = data.archive_file.transform.output_path
  source_code_hash = data.archive_file.transform.output_base64sha256

  tags = local.tags
}

data "archive_file" "transform" {
  type        = "zip"
  output_path = "${path.module}/.build/transform.zip"

  source {
    filename = "index.py"
    content  = <<-PY
      """Reshapes CloudEvents into Personalize's Interactions format.

      Firehose hands us base64 records and expects the same back with a
      result of Ok, Dropped or ProcessingFailed. A record we cannot parse is
      DROPPED, not failed: a single malformed event must not stall the whole
      delivery stream, and the error prefix in S3 keeps a copy either way.
      """
      import base64, json

      EVENT_TYPES = {
          "VIEW": "view", "ADD_TO_CART": "add_to_cart",
          "PURCHASE": "purchase", "SEARCH": "search", "WISHLIST": "wishlist",
      }

      def handler(event, _context):
          out = []
          for record in event["records"]:
              try:
                  envelope = json.loads(base64.b64decode(record["data"]))
                  d = envelope.get("data", {})

                  # Personalize needs a stable user identity. Anonymous
                  # sessions are dropped rather than given a synthetic id:
                  # a per-session "user" teaches the model nothing and
                  # inflates the user count enormously.
                  user_id = d.get("userId")
                  item_id = d.get("itemId")
                  event_type = EVENT_TYPES.get(d.get("eventType", ""))
                  if not (user_id and item_id and event_type):
                      out.append({"recordId": record["recordId"], "result": "Dropped"})
                      continue

                  row = {
                      "USER_ID": user_id,
                      "ITEM_ID": item_id,
                      # Personalize wants epoch SECONDS, not milliseconds and
                      # not ISO-8601. Getting this wrong makes every
                      # interaction look like it happened in 1970.
                      "TIMESTAMP": int(_epoch_seconds(d.get("occurredAt"))),
                      "EVENT_TYPE": event_type,
                      "EVENT_VALUE": (d.get("value") or {}).get("amount", 0) / 100.0,
                      "DEVICE_TYPE": d.get("deviceType"),
                      "LOCALE": d.get("locale"),
                      # Closes the attribution loop: which recommendation led
                      # to this interaction.
                      "IMPRESSION": d.get("recommendationId"),
                  }

                  out.append({
                      "recordId": record["recordId"],
                      "result": "Ok",
                      # Newline-delimited JSON, one object per line, which is
                      # what a Personalize import job expects.
                      "data": base64.b64encode((json.dumps(row) + "\n").encode()).decode(),
                  })
              except Exception:
                  out.append({"recordId": record["recordId"], "result": "Dropped"})
          return {"records": out}

      def _epoch_seconds(iso):
          from datetime import datetime, timezone
          if not iso:
              return datetime.now(timezone.utc).timestamp()
          return datetime.fromisoformat(iso.replace("Z", "+00:00")).timestamp()
    PY
  }
}

##############################################################################
# IAM
##############################################################################

resource "aws_iam_role" "personalize" {
  name = "${var.name}-personalize"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "personalize.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })
  tags = local.tags
}

resource "aws_iam_role_policy" "personalize" {
  role = aws_iam_role.personalize.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect   = "Allow"
        Action   = ["s3:GetObject", "s3:ListBucket", "s3:PutObject"]
        Resource = ["arn:aws:s3:::${var.activity_bucket}", "arn:aws:s3:::${var.activity_bucket}/*"]
      },
      {
        Effect   = "Allow"
        Action   = ["kms:Decrypt", "kms:GenerateDataKey"]
        Resource = var.kms_key_arn
      },
    ]
  })
}

resource "aws_iam_role" "firehose" {
  name = "${var.name}-firehose"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "firehose.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })
  tags = local.tags
}

resource "aws_iam_role_policy" "firehose" {
  role = aws_iam_role.firehose.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect = "Allow"
        Action = ["s3:AbortMultipartUpload", "s3:GetBucketLocation", "s3:GetObject",
                  "s3:ListBucket", "s3:ListBucketMultipartUploads", "s3:PutObject"]
        Resource = ["arn:aws:s3:::${var.activity_bucket}", "arn:aws:s3:::${var.activity_bucket}/*"]
      },
      { Effect = "Allow", Action = ["lambda:InvokeFunction"], Resource = "${aws_lambda_function.transform.arn}:*" },
      { Effect = "Allow", Action = ["logs:PutLogEvents"], Resource = "${aws_cloudwatch_log_group.firehose.arn}:*" },
      { Effect = "Allow", Action = ["kms:Decrypt", "kms:GenerateDataKey"], Resource = var.kms_key_arn },
    ]
  })
}

resource "aws_iam_role" "lambda" {
  name = "${var.name}-activity-transform"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "lambda.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })
  tags = local.tags
}

resource "aws_iam_role_policy_attachment" "lambda_basic" {
  role       = aws_iam_role.lambda.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole"
}

#!/bin/bash
# Provisions the AWS resources the services expect, in LocalStack.
set -euo pipefail
export AWS_ACCESS_KEY_ID=test AWS_SECRET_ACCESS_KEY=test AWS_DEFAULT_REGION=eu-west-1
AWS="awslocal"

echo "S3 buckets"
for b in souq-product-media souq-invoices souq-exports souq-personalize-data; do
  $AWS s3 mb "s3://$b" >/dev/null 2>&1 || true
done
# Product media is public-read behind CloudFront in AWS; locally the storefront
# reads it directly, so it has to be readable without signing.
$AWS s3api put-bucket-policy --bucket souq-product-media --policy '{
  "Version":"2012-10-17",
  "Statement":[{"Sid":"PublicRead","Effect":"Allow","Principal":"*",
                "Action":"s3:GetObject","Resource":"arn:aws:s3:::souq-product-media/*"}]
}' >/dev/null 2>&1 || true

echo "SES identity"
$AWS ses verify-email-identity --email-address no-reply@souq.dev >/dev/null 2>&1 || true

echo "SNS topics"
$AWS sns create-topic --name souq-ops-alerts >/dev/null 2>&1 || true

echo "Secrets Manager"
$AWS secretsmanager create-secret --name souq/payment/psp-key-salt \
  --secret-string 'local-development-salt-not-a-secret-value' >/dev/null 2>&1 || true
$AWS secretsmanager create-secret --name souq/identity/jwt-signing-key \
  --secret-string 'local-development-only' >/dev/null 2>&1 || true

echo "DynamoDB (recommendation cache)"
$AWS dynamodb create-table --table-name souq-recommendation-cache \
  --attribute-definitions AttributeName=cacheKey,AttributeType=S \
  --key-schema AttributeName=cacheKey,KeyType=HASH \
  --billing-mode PAY_PER_REQUEST >/dev/null 2>&1 || true

echo "localstack ready"

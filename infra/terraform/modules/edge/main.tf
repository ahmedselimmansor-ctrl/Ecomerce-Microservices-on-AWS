##############################################################################
# Edge: S3, CloudFront, WAF.
#
# CloudFront is not here for caching alone. It is the single public entry point
# to the platform, which is what makes it the right place to put TLS
# termination, the WAF, and per-path rate limits. Nothing reaches the ALB
# without passing through it — enforced by a shared secret header the ALB
# checks, so an attacker who discovers the ALB's hostname cannot bypass the WAF
# by talking to it directly.
##############################################################################

terraform {
  required_version = ">= 1.9"
  required_providers {
    aws    = { source = "hashicorp/aws", version = "~> 5.60", configuration_aliases = [aws.us_east_1] }
    random = { source = "hashicorp/random", version = "~> 3.6" }
  }
}

locals {
  tags = merge(var.tags, { Module = "edge" })
  s3_origin  = "s3-media"
  alb_origin = "alb-app"
}

##############################################################################
# Buckets
##############################################################################

resource "aws_s3_bucket" "media" {
  bucket = "${var.name}-product-media"
  tags   = merge(local.tags, { Purpose = "product-media" })
}

resource "aws_s3_bucket" "invoices" {
  bucket = "${var.name}-invoices"
  tags   = merge(local.tags, { Purpose = "invoices" })
}

resource "aws_s3_bucket" "personalize" {
  bucket = "${var.name}-personalize-data"
  tags   = merge(local.tags, { Purpose = "personalize-datasets" })
}

resource "aws_s3_bucket" "logs" {
  bucket = "${var.name}-access-logs"
  tags   = merge(local.tags, { Purpose = "access-logs" })
}

# Applied to every bucket. Public access via a bucket policy is how S3 data
# leaks happen; blocking it at the account boundary means an accidental
# `public-read` ACL simply does not take effect.
resource "aws_s3_bucket_public_access_block" "all" {
  for_each = {
    media       = aws_s3_bucket.media.id
    invoices    = aws_s3_bucket.invoices.id
    personalize = aws_s3_bucket.personalize.id
    logs        = aws_s3_bucket.logs.id
  }

  bucket                  = each.value
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_server_side_encryption_configuration" "all" {
  for_each = {
    media       = aws_s3_bucket.media.id
    invoices    = aws_s3_bucket.invoices.id
    personalize = aws_s3_bucket.personalize.id
  }

  bucket = each.value
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm     = "aws:kms"
      kms_master_key_id = var.kms_key_arn
    }
    # Cuts KMS API calls by orders of magnitude on a bucket with many small
    # objects, which is exactly what product media is. Without it, KMS
    # throttling shows up as slow image uploads.
    bucket_key_enabled = true
  }
}

resource "aws_s3_bucket_versioning" "invoices" {
  bucket = aws_s3_bucket.invoices.id
  # Invoices are financial records. Versioning turns an accidental overwrite
  # from a data-loss incident into a restore.
  versioning_configuration { status = "Enabled" }
}

resource "aws_s3_bucket_lifecycle_configuration" "media" {
  bucket = aws_s3_bucket.media.id

  rule {
    id     = "abort-incomplete-uploads"
    status = "Enabled"
    filter {}
    # Failed multipart uploads are invisible in the console and billed
    # forever. Everyone discovers this via a cost review.
    abort_incomplete_multipart_upload { days_after_initiation = 7 }
  }

  rule {
    id     = "archive-old-media"
    status = "Enabled"
    filter { prefix = "archive/" }
    transition {
      days          = 90
      storage_class = "INTELLIGENT_TIERING"
    }
  }
}

resource "aws_s3_bucket_lifecycle_configuration" "logs" {
  bucket = aws_s3_bucket.logs.id
  rule {
    id     = "expire"
    status = "Enabled"
    filter {}
    expiration { days = var.log_retention_days }
    abort_incomplete_multipart_upload { days_after_initiation = 7 }
  }
}

# CloudFront's log delivery uses ACLs, which are otherwise disabled by the
# modern default.
resource "aws_s3_bucket_ownership_controls" "logs" {
  bucket = aws_s3_bucket.logs.id
  rule { object_ownership = "BucketOwnerPreferred" }
}

##############################################################################
# Origin access
##############################################################################

resource "aws_cloudfront_origin_access_control" "media" {
  name                              = "${var.name}-media-oac"
  origin_access_control_origin_type = "s3"
  signing_behavior                  = "always"
  signing_protocol                  = "sigv4"
  description                       = "CloudFront to the media bucket"
}

# OAC, not a public bucket policy. The bucket stays entirely private and only
# this distribution can read it, so a leaked object URL is useless without
# CloudFront's signature.
data "aws_iam_policy_document" "media_bucket" {
  statement {
    sid       = "AllowCloudFrontOnly"
    effect    = "Allow"
    actions   = ["s3:GetObject"]
    resources = ["${aws_s3_bucket.media.arn}/*"]

    principals {
      type        = "Service"
      identifiers = ["cloudfront.amazonaws.com"]
    }
    condition {
      test     = "StringEquals"
      variable = "AWS:SourceArn"
      values   = [aws_cloudfront_distribution.this.arn]
    }
  }
}

resource "aws_s3_bucket_policy" "media" {
  bucket = aws_s3_bucket.media.id
  policy = data.aws_iam_policy_document.media_bucket.json
}

##############################################################################
# The shared secret that stops the ALB being bypassed
##############################################################################

resource "random_password" "origin_secret" {
  length  = 64
  special = false
}

resource "aws_secretsmanager_secret" "origin_secret" {
  name       = "${var.name}/edge/origin-verify"
  kms_key_id = var.kms_key_arn
  tags       = local.tags
}

resource "aws_secretsmanager_secret_version" "origin_secret" {
  secret_id     = aws_secretsmanager_secret.origin_secret.id
  secret_string = random_password.origin_secret.result
}

##############################################################################
# WAF
#
# Must be in us-east-1 for a CloudFront distribution, regardless of where
# everything else lives.
##############################################################################

resource "aws_wafv2_web_acl" "this" {
  provider = aws.us_east_1

  name  = "${var.name}-edge"
  scope = "CLOUDFRONT"

  default_action { allow {} }

  # 1. Managed common rules.
  rule {
    name     = "AWSManagedCommonRuleSet"
    priority = 10
    override_action { none {} }

    statement {
      managed_rule_group_statement {
        vendor_name = "AWS"
        name        = "AWSManagedRulesCommonRuleSet"

        # Product descriptions and reviews legitimately contain long text and
        # HTML-ish characters. Left on, this rule rejects a merchant's
        # perfectly valid product copy with an unexplained 403.
        rule_action_override {
          name = "SizeRestrictions_BODY"
          action_to_use { count {} }
        }
        rule_action_override {
          name = "CrossSiteScripting_BODY"
          action_to_use { count {} }
        }
      }
    }

    visibility_config {
      cloudwatch_metrics_enabled = true
      metric_name                = "common-rules"
      sampled_requests_enabled   = true
    }
  }

  # 2. Known-bad inputs.
  rule {
    name     = "AWSManagedKnownBadInputs"
    priority = 20
    override_action { none {} }
    statement {
      managed_rule_group_statement {
        vendor_name = "AWS"
        name        = "AWSManagedRulesKnownBadInputsRuleSet"
      }
    }
    visibility_config {
      cloudwatch_metrics_enabled = true
      metric_name                = "known-bad-inputs"
      sampled_requests_enabled   = true
    }
  }

  # 3. Checkout rate limit. Tighter than browsing by a factor of twenty,
  # because a bot scraping the catalogue is annoying while a bot hammering
  # checkout is card testing — and card testing costs real money in
  # per-attempt fees and chargebacks.
  rule {
    name     = "CheckoutRateLimit"
    priority = 30
    action { block {} }

    statement {
      rate_based_statement {
        limit              = var.rate_limit_checkout
        aggregate_key_type = "IP"

        scope_down_statement {
          byte_match_statement {
            positional_constraint = "STARTS_WITH"
            search_string         = "/api/bff/orders"
            field_to_match { uri_path {} }
            text_transformation {
              priority = 0
              type     = "LOWERCASE"
            }
          }
        }
      }
    }

    visibility_config {
      cloudwatch_metrics_enabled = true
      metric_name                = "checkout-rate-limit"
      sampled_requests_enabled   = true
    }
  }

  # 4. Auth rate limit — credential stuffing.
  rule {
    name     = "AuthRateLimit"
    priority = 40
    action { block {} }

    statement {
      rate_based_statement {
        limit              = var.rate_limit_auth
        aggregate_key_type = "IP"

        scope_down_statement {
          byte_match_statement {
            positional_constraint = "STARTS_WITH"
            search_string         = "/api/bff/auth"
            field_to_match { uri_path {} }
            text_transformation {
              priority = 0
              type     = "LOWERCASE"
            }
          }
        }
      }
    }

    visibility_config {
      cloudwatch_metrics_enabled = true
      metric_name                = "auth-rate-limit"
      sampled_requests_enabled   = true
    }
  }

  # 5. General rate limit, last so the specific ones win.
  rule {
    name     = "GeneralRateLimit"
    priority = 50
    action { block {} }

    statement {
      rate_based_statement {
        limit              = var.rate_limit_general
        aggregate_key_type = "IP"
      }
    }

    visibility_config {
      cloudwatch_metrics_enabled = true
      metric_name                = "general-rate-limit"
      sampled_requests_enabled   = true
    }
  }

  # 6. Bot control — COUNT only to begin with. Blocking on day one
  # inevitably catches a price-comparison partner, a monitoring probe, or the
  # mobile app's own user agent. Watch the counts for a fortnight first.
  dynamic "rule" {
    for_each = var.enable_bot_control ? [1] : []
    content {
      name     = "AWSManagedBotControl"
      priority = 60
      override_action { count {} }
      statement {
        managed_rule_group_statement {
          vendor_name = "AWS"
          name        = "AWSManagedRulesBotControlRuleSet"
          managed_rule_group_configs {
            aws_managed_rules_bot_control_rule_set { inspection_level = "COMMON" }
          }
        }
      }
      visibility_config {
        cloudwatch_metrics_enabled = true
        metric_name                = "bot-control"
        sampled_requests_enabled   = true
      }
    }
  }

  visibility_config {
    cloudwatch_metrics_enabled = true
    metric_name                = "${var.name}-edge"
    sampled_requests_enabled   = true
  }

  tags = local.tags
}

##############################################################################
# Certificate
##############################################################################

resource "aws_acm_certificate" "this" {
  provider = aws.us_east_1

  domain_name               = var.domain_name
  subject_alternative_names = ["*.${var.domain_name}"]
  validation_method         = "DNS"

  lifecycle { create_before_destroy = true }
  tags = local.tags
}

##############################################################################
# Cache policies
##############################################################################

resource "aws_cloudfront_cache_policy" "static" {
  name        = "${var.name}-static"
  default_ttl = 86400
  max_ttl     = 31536000
  min_ttl     = 1

  parameters_in_cache_key_and_forwarded_to_origin {
    enable_accept_encoding_brotli = true
    enable_accept_encoding_gzip   = true
    cookies_config { cookie_behavior = "none" }
    headers_config { header_behavior = "none" }
    query_strings_config { query_string_behavior = "none" }
  }
}

resource "aws_cloudfront_cache_policy" "html" {
  name = "${var.name}-html"
  # Short. A product page must reflect a price change quickly, and stale-while-
  # revalidate below means a customer never actually waits for the origin.
  default_ttl = 60
  max_ttl     = 300
  min_ttl     = 0

  parameters_in_cache_key_and_forwarded_to_origin {
    enable_accept_encoding_brotli = true
    enable_accept_encoding_gzip   = true

    cookies_config {
      # The session cookie MUST be in the cache key, or one customer's
      # signed-in page is served to the next visitor. This is the single most
      # damaging CDN misconfiguration there is.
      cookie_behavior = "whitelist"
      cookies { items = ["souq_session", "souq_locale", "souq_currency"] }
    }

    headers_config {
      header_behavior = "whitelist"
      headers { items = ["Accept-Language", "CloudFront-Viewer-Country"] }
    }

    query_strings_config { query_string_behavior = "all" }
  }
}

##############################################################################
# Distribution
##############################################################################

resource "aws_cloudfront_distribution" "this" {
  enabled         = true
  is_ipv6_enabled = true
  comment         = "${var.name} edge"
  price_class     = var.price_class
  aliases         = [var.domain_name, "www.${var.domain_name}"]
  web_acl_id      = aws_wafv2_web_acl.this.arn
  http_version    = "http2and3"

  origin {
    origin_id                = local.s3_origin
    domain_name              = aws_s3_bucket.media.bucket_regional_domain_name
    origin_access_control_id = aws_cloudfront_origin_access_control.media.id
  }

  origin {
    origin_id   = local.alb_origin
    domain_name = var.alb_dns_name

    custom_origin_config {
      http_port              = 80
      https_port             = 443
      origin_protocol_policy = "https-only"
      origin_ssl_protocols   = ["TLSv1.2"]
      # Generous, because checkout writes to Postgres and Kafka in one
      # transaction. A 30s ceiling here would turn a slow-but-succeeding order
      # into a 504 and a customer pressing the button again.
      origin_read_timeout    = 60
      origin_keepalive_timeout = 60
    }

    # The header the ALB checks. Without it, anyone who discovers the ALB's
    # hostname bypasses the WAF and every rate limit above.
    custom_header {
      name  = "X-Origin-Verify"
      value = random_password.origin_secret.result
    }
  }

  default_cache_behavior {
    target_origin_id       = local.alb_origin
    viewer_protocol_policy = "redirect-to-https"
    allowed_methods        = ["GET", "HEAD", "OPTIONS", "PUT", "POST", "PATCH", "DELETE"]
    cached_methods         = ["GET", "HEAD"]
    compress               = true
    cache_policy_id        = aws_cloudfront_cache_policy.html.id
    origin_request_policy_id = data.aws_cloudfront_origin_request_policy.all_viewer.id
    response_headers_policy_id = aws_cloudfront_response_headers_policy.security.id
  }

  ordered_cache_behavior {
    path_pattern           = "/media/*"
    target_origin_id       = local.s3_origin
    viewer_protocol_policy = "redirect-to-https"
    allowed_methods        = ["GET", "HEAD", "OPTIONS"]
    cached_methods         = ["GET", "HEAD"]
    compress               = true
    cache_policy_id        = aws_cloudfront_cache_policy.static.id
  }

  ordered_cache_behavior {
    path_pattern           = "/_next/static/*"
    target_origin_id       = local.alb_origin
    viewer_protocol_policy = "redirect-to-https"
    allowed_methods        = ["GET", "HEAD", "OPTIONS"]
    cached_methods         = ["GET", "HEAD"]
    compress               = true
    # Next.js fingerprints these filenames, so they are immutable and can be
    # cached for a year.
    cache_policy_id = aws_cloudfront_cache_policy.static.id
  }

  ordered_cache_behavior {
    path_pattern           = "/api/*"
    target_origin_id       = local.alb_origin
    viewer_protocol_policy = "https-only"
    allowed_methods        = ["GET", "HEAD", "OPTIONS", "PUT", "POST", "PATCH", "DELETE"]
    cached_methods         = ["GET", "HEAD"]
    compress               = true
    # Never cached. A cached cart or order status would be served to the wrong
    # customer, and a cached POST is meaningless.
    cache_policy_id          = data.aws_cloudfront_cache_policy.disabled.id
    origin_request_policy_id = data.aws_cloudfront_origin_request_policy.all_viewer.id
  }

  custom_error_response {
    error_code            = 503
    response_code         = 503
    response_page_path    = "/maintenance.html"
    error_caching_min_ttl = 10
  }

  restrictions {
    geo_restriction { restriction_type = "none" }
  }

  viewer_certificate {
    acm_certificate_arn      = aws_acm_certificate.this.arn
    ssl_support_method       = "sni-only"
    minimum_protocol_version = "TLSv1.2_2021"
  }

  logging_config {
    bucket          = aws_s3_bucket.logs.bucket_domain_name
    prefix          = "cloudfront/"
    include_cookies = false   # cookies carry the session token
  }

  tags = local.tags
  depends_on = [aws_s3_bucket_ownership_controls.logs]
}

resource "aws_cloudfront_response_headers_policy" "security" {
  name = "${var.name}-security-headers"

  security_headers_config {
    strict_transport_security {
      access_control_max_age_sec = 63072000
      include_subdomains         = true
      preload                    = true
      override                   = true
    }
    content_type_options { override = true }
    frame_options {
      frame_option = "DENY"
      override     = true
    }
    referrer_policy {
      referrer_policy = "strict-origin-when-cross-origin"
      override        = true
    }
    xss_protection {
      protection = true
      mode_block = true
      override   = true
    }
  }

  custom_headers_config {
    items {
      header   = "Permissions-Policy"
      value    = "camera=(), microphone=(), geolocation=(self), payment=(self)"
      override = true
    }
    items {
      # payment=(self) above and this CSP both have to allow the Paymob
      # iframe, or card entry silently fails to render.
      header   = "Content-Security-Policy"
      value    = join("; ", [
        "default-src 'self'",
        "img-src 'self' data: https://${var.domain_name} https://*.cloudfront.net",
        "script-src 'self' 'unsafe-inline' https://accept.paymob.com",
        "style-src 'self' 'unsafe-inline'",
        "frame-src https://accept.paymob.com",
        "connect-src 'self' https://accept.paymob.com",
        "frame-ancestors 'none'",
        "base-uri 'self'",
        "form-action 'self' https://accept.paymob.com",
      ])
      override = true
    }
  }
}

data "aws_cloudfront_cache_policy" "disabled" {
  name = "Managed-CachingDisabled"
}

data "aws_cloudfront_origin_request_policy" "all_viewer" {
  name = "Managed-AllViewerExceptHostHeader"
}

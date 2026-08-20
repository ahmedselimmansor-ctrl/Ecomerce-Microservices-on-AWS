##############################################################################
# Observability.
#
# Managed Prometheus and Managed Grafana rather than self-hosted, for one
# reason: the metrics stack must survive the cluster it observes. A
# Prometheus running inside EKS goes down with the outage you need it to
# explain, which is exactly when it matters.
#
# The alerts below are deliberately few. Every one of them is a page, and each
# corresponds to a property the formal models prove — so if one fires, either
# a proof assumption has been violated in production or the code has diverged
# from the model. Both are worth waking someone for. Dashboards carry
# everything else.
##############################################################################

terraform {
  required_version = ">= 1.9"
  required_providers {
    aws = { source = "hashicorp/aws", version = "~> 5.60" }
  }
}

locals { tags = merge(var.tags, { Module = "observability" }) }

resource "aws_prometheus_workspace" "this" {
  count = var.enable_managed_prometheus ? 1 : 0

  alias = var.name
  logging_configuration {
    log_group_arn = "${aws_cloudwatch_log_group.amp[0].arn}:*"
  }
  tags = local.tags
}

resource "aws_cloudwatch_log_group" "amp" {
  count             = var.enable_managed_prometheus ? 1 : 0
  name              = "/aws/prometheus/${var.name}"
  retention_in_days = 30
  tags              = local.tags
}

##############################################################################
# Alerting rules
##############################################################################

resource "aws_prometheus_rule_group_namespace" "souq" {
  count = var.enable_managed_prometheus ? 1 : 0

  name         = "${var.name}-alerts"
  workspace_id = aws_prometheus_workspace.this[0].id

  data = <<-YAML
    groups:
      # ---------------------------------------------------------------
      # PAGE. Each of these means a property the state-space models prove is not
      # holding in production. There is no "monitor and see" response to any
      # of them.
      # ---------------------------------------------------------------
      - name: souq.invariants
        interval: 30s
        rules:
          - alert: IllegalSagaTransition
            expr: increase(souq_saga_illegal_transitions_total[5m]) > 0
            for: 0m
            labels:
              severity: page
              runbook: docs/runbooks/illegal-saga-transition.md
            annotations:
              summary: "The saga took a transition internal/saga/model_test.go says is impossible"
              description: >-
                {{ $value }} illegal transition(s) from {{ $labels.from }} on
                {{ $labels.trigger }}. The implementation has diverged from the
                proven design. Do not restart anything until the affected
                orders have been identified.

          - alert: OversellDetected
            expr: souq_inventory_reserved > souq_inventory_on_hand
            for: 0m
            labels:
              severity: page
              runbook: docs/runbooks/oversell.md
            annotations:
              summary: "SKU {{ $labels.sku }} has more reserved than exists"
              description: >-
                The no_oversell CHECK constraint should make this
                unrepresentable. If it is firing, either the constraint was
                dropped or something is writing outside the application.

          - alert: LedgerImbalance
            expr: souq_ledger_unbalanced_groups > 0
            for: 2m
            labels:
              severity: page
              runbook: docs/runbooks/ledger-imbalance.md
            annotations:
              summary: "{{ $value }} unbalanced double-entry group(s)"
              description: "The books do not balance. Finance reconciliation will fail."

          - alert: SagaStuckPastPointOfNoReturn
            expr: souq_saga_stuck_orders > 0
            for: 5m
            labels:
              severity: page
              runbook: docs/runbooks/stuck-saga.md
            annotations:
              summary: "{{ $value }} order(s) wedged past the point of no return"
              description: >-
                Stock is committed and payment has not settled. These cannot be
                rolled back (docs/DESIGN-INVARIANTS.md §1) and need manual resolution.

          - alert: UnknownPaymentOutcome
            expr: increase(souq_payment_unknown_outcome_total[10m]) > 0
            for: 0m
            labels:
              severity: page
              runbook: docs/runbooks/unknown-payment-outcome.md
            annotations:
              summary: "A payment's outcome is unknown and needs reconciliation"
              description: >-
                We asked the provider and did not learn the answer. The
                customer may or may not have been charged. Reconcile against
                Paymob before doing anything else.

      # ---------------------------------------------------------------
      # PAGE on availability. Burn-rate based, not threshold based: a
      # threshold alert on error rate fires on every deploy blip. Burn rate
      # fires when the monthly error budget is actually at risk.
      # ---------------------------------------------------------------
      - name: souq.slo
        interval: 30s
        rules:
          - alert: CheckoutErrorBudgetBurningFast
            # 14.4x burn over 1h exhausts a 30-day budget in ~2 days.
            expr: |
              (
                sum(rate(http_server_requests_total{route="/v1/orders",status="5xx"}[1h]))
                / sum(rate(http_server_requests_total{route="/v1/orders"}[1h]))
              ) > (14.4 * 0.0005)
            for: 2m
            labels:
              severity: page
              runbook: docs/runbooks/checkout-errors.md
            annotations:
              summary: "Checkout is burning its error budget 14x too fast"

          - alert: CheckoutErrorBudgetBurningSlowly
            expr: |
              (
                sum(rate(http_server_requests_total{route="/v1/orders",status="5xx"}[6h]))
                / sum(rate(http_server_requests_total{route="/v1/orders"}[6h]))
              ) > (6 * 0.0005)
            for: 15m
            labels:
              severity: ticket
            annotations:
              summary: "Checkout error budget burning at 6x"

          - alert: CheckoutLatencyBudgetBreached
            expr: |
              histogram_quantile(0.99,
                sum(rate(http_server_requests_seconds_bucket{route="/v1/orders"}[5m])) by (le)
              ) > 0.8
            for: 10m
            labels:
              severity: ticket
            annotations:
              summary: "Checkout p99 is {{ $value | humanizeDuration }}, SLO is 800ms"

      # ---------------------------------------------------------------
      # TICKET. Degradation that customers feel but that is not an outage.
      # ---------------------------------------------------------------
      - name: souq.degradation
        interval: 1m
        rules:
          - alert: OutboxBacklogGrowing
            # The rate matters more than the depth. A backlog of 5000 that is
            # draining is fine; one of 500 that is growing is not.
            expr: deriv(souq_outbox_unpublished[10m]) > 1
            for: 10m
            labels:
              severity: ticket
              runbook: docs/runbooks/outbox-backlog.md
            annotations:
              summary: "The outbox relay is losing to the write rate on {{ $labels.service }}"
              description: "Events are getting stale even though each publish succeeds."

          - alert: DeadLetterQueueGrowing
            expr: increase(souq_events_consumed_total{outcome="dlq"}[15m]) > 5
            for: 5m
            labels:
              severity: ticket
              runbook: docs/runbooks/dlq.md
            annotations:
              summary: "{{ $value }} messages sent to the DLQ from {{ $labels.topic }}"

          - alert: PricingEngineCircuitOpen
            expr: souq_pricing_calls_total{outcome="circuit_open"} > 0
            for: 5m
            labels:
              severity: ticket
            annotations:
              summary: "Carts are being priced at list price"
              description: >-
                pricing-engine is unreachable and the breaker is open. Customers
                are not seeing their promotions. Not an outage — checkout still
                works — but it is a revenue and trust problem.

          - alert: RecommendationsAllFallback
            expr: |
              sum(rate(souq_recommendations_total{fallback="true"}[10m]))
              / sum(rate(souq_recommendations_total[10m])) > 0.5
            for: 10m
            labels:
              severity: ticket
            annotations:
              summary: "Over half of recommendations are the fallback ranker"

          - alert: ReservationTTLSweeperActive
            # Should be near zero. The saga releases explicitly; the sweeper
            # firing means a saga did not, which points at order-service.
            expr: increase(souq_reservation_ttl_expiries_total[15m]) > 10
            for: 5m
            labels:
              severity: ticket
            annotations:
              summary: "The TTL sweeper is releasing reservations the saga should have"

          - alert: RefreshTokenReuseSpike
            expr: increase(souq_refresh_token_reuse_total[15m]) > 20
            for: 5m
            labels:
              severity: ticket
              runbook: docs/runbooks/token-reuse.md
            annotations:
              summary: "{{ $value }} refresh-token reuses detected"
              description: >-
                Occasional reuse is a client retrying after a network failure.
                A spike is credential theft or a broken client release.
  YAML
}

##############################################################################
# Grafana
##############################################################################

resource "aws_grafana_workspace" "this" {
  count = var.enable_managed_grafana ? 1 : 0

  name                     = var.name
  account_access_type      = "CURRENT_ACCOUNT"
  # IAM Identity Center rather than local users: nobody should have a
  # standalone Grafana password, and offboarding must be one action.
  authentication_providers = ["AWS_SSO"]
  permission_type          = "SERVICE_MANAGED"
  data_sources             = ["PROMETHEUS", "CLOUDWATCH", "XRAY"]
  role_arn                 = aws_iam_role.grafana[0].arn

  tags = local.tags
}

resource "aws_iam_role" "grafana" {
  count = var.enable_managed_grafana ? 1 : 0

  name = "${var.name}-grafana"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "grafana.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })
  tags = local.tags
}

##############################################################################
# Routing
##############################################################################

resource "aws_sns_topic" "pages" {
  name              = "${var.name}-pages"
  kms_master_key_id = "alias/aws/sns"
  tags              = merge(local.tags, { Severity = "page" })
}

resource "aws_sns_topic" "tickets" {
  name              = "${var.name}-tickets"
  kms_master_key_id = "alias/aws/sns"
  tags              = merge(local.tags, { Severity = "ticket" })
}

resource "aws_sns_topic_subscription" "pagerduty" {
  count = var.pagerduty_endpoint != "" ? 1 : 0

  topic_arn              = aws_sns_topic.pages.arn
  protocol               = "https"
  endpoint               = var.pagerduty_endpoint
  endpoint_auto_confirms = true
}

resource "aws_sns_topic_subscription" "email" {
  for_each = toset(var.alert_email_addresses)

  topic_arn = aws_sns_topic.tickets.arn
  protocol  = "email"
  endpoint  = each.value
}

##############################################################################
# ADOT collector IRSA
##############################################################################

resource "aws_iam_role" "adot" {
  name = "${var.name}-adot-collector"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Federated = var.oidc_provider_arn }
      Action    = "sts:AssumeRoleWithWebIdentity"
      Condition = {
        StringEquals = {
          "${replace(var.oidc_provider_arn, "/^.*oidc-provider\\//", "")}:sub" = "system:serviceaccount:observability:adot-collector"
        }
      }
    }]
  })
  tags = local.tags
}

resource "aws_iam_role_policy_attachment" "adot" {
  for_each = toset([
    "arn:aws:iam::aws:policy/AmazonPrometheusRemoteWriteAccess",
    "arn:aws:iam::aws:policy/AWSXrayWriteOnlyAccess",
    "arn:aws:iam::aws:policy/CloudWatchAgentServerPolicy",
  ])
  role       = aws_iam_role.adot.name
  policy_arn = each.value
}

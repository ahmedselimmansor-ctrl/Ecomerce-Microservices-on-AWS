# SOUQ — one entry point for everything.
#
#   make            list every target
#   make check      the whole gate: formal models, builds, tests, linters
#   make up         local stack
#   make smoke      end-to-end checkout against the local stack

SHELL := /bin/bash
.DEFAULT_GOAL := help
.ONESHELL:

COMPOSE     := docker compose
GIT_SHA     := $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)
REGISTRY    ?= ghcr.io/souq
GO_SERVICES   := order-service payment-service inventory-service
JAVA_SERVICES := identity-service catalog-service
NODE_SERVICES := cart-service review-service notification-service
PY_SERVICES   := search-service recommendation-service
CPP_SERVICES  := pricing-engine
ALL_SERVICES  := $(GO_SERVICES) $(JAVA_SERVICES) $(NODE_SERVICES) $(PY_SERVICES) $(CPP_SERVICES)

CYAN := \033[36m
RED  := \033[0;31m
BOLD := \033[1m
DIM  := \033[2m
OFF  := \033[0m

## ---------------------------------------------------------------- help

.PHONY: help
help: ## Show this help
	@printf "$(BOLD)SOUQ$(OFF) — distributed commerce platform\n\n"
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
	  | awk 'BEGIN {FS = ":.*?## "}; {printf "  $(CYAN)%-22s$(OFF) %s\n", $$1, $$2}'
	@printf "\n$(DIM)services: $(ALL_SERVICES)$(OFF)\n"

## ---------------------------------------------------------------- verify

.PHONY: check
check: models contracts test lint frontend image-check tf-check k8s-check ## Everything CI runs, in the order that fails fastest
	@printf "\n$(BOLD)all checks passed$(OFF)\n"

.PHONY: models
models: ## Exhaustive state-space search (several assert a known-bad design still FAILS)
	@printf "$(BOLD)state-space models$(OFF)\n"
	@(cd libs/go-modelcheck && go test ./...) | sed 's/^/  explorer     /'
	@(cd services/order-service && go test ./internal/saga/ ./internal/eventbus/) | sed 's/^/  saga+outbox  /'
	@(cd services/inventory-service && go test ./internal/stock/) | sed 's/^/  reservation  /'

.PHONY: contracts
contracts: ## Validate the AsyncAPI document and build the shared Zod package
	@printf "$(BOLD)contracts$(OFF)\n"
	@python3 -c "import yaml,sys; d=yaml.safe_load(open('contracts/asyncapi/souq-events.v1.yaml')); \
	  print(f\"  asyncapi {d['asyncapi']}: {len(d['channels'])} channels, {len(d['components']['messages'])} messages\")"
	@(cd libs/ts-contracts && (npm ci --silent --no-audit --no-fund 2>/dev/null || npm install --silent))
	@(cd libs/ts-contracts && npx tsc -p tsconfig.json --noEmit) && printf "  ts-contracts typechecks\n"
	@# The TypeScript and Python contracts are one contract written twice.
	@# Nothing generates one from the other, so nothing but this stops them
	@# drifting. Stdlib-only, so it runs without pydantic installed.
	@python3 scripts/contract-parity.py | sed 's/^/  /'
	@python3 scripts/contract-parity.py >/dev/null

.PHONY: test
test: test-go test-node test-py ## Run every unit test suite

.PHONY: test-go
test-go:
	@printf "$(BOLD)go tests$(OFF)\n"
	@for s in $(GO_SERVICES); do \
	  printf "  %s\n" "$$s"; \
	  (cd services/$$s && go test ./... 2>&1 | sed 's/^/    /') || exit 1; \
	done

.PHONY: test-node
test-node:
	@printf "$(BOLD)node tests$(OFF)\n"
	@# Every Node package, not just the contracts. The three services had tests
	@# that nothing local ran — CI found them missing before this did.
	@for pkg in libs/ts-contracts $(addprefix services/,$(NODE_SERVICES)); do \
	  if [ ! -d $$pkg/node_modules ]; then \
	    printf "  $(DIM)$$pkg: dependencies not installed; skipping$(OFF)\n"; continue; \
	  fi; \
	  printf "  %-28s " "$$pkg"; \
	  out=$$(cd $$pkg && npx vitest run 2>&1); \
	  if [ $$? -eq 0 ]; then \
	    printf "%s\n" "$$(echo "$$out" | grep -oE 'Tests +[0-9]+ passed \([0-9]+\)' | tail -1)"; \
	  else \
	    printf "$(RED)FAILED$(OFF)\n"; echo "$$out" | tail -12 | sed 's/^/      /'; exit 1; \
	  fi; \
	done

.PHONY: test-py
test-py:
	@printf "$(BOLD)python tests$(OFF)\n"
	@python3 -c "import pytest" 2>/dev/null || { \
	  printf "  $(DIM)pytest not installed; skipping$(OFF)\n"; exit 0; }
	@for s in $(PY_SERVICES); do \
	  [ -d services/$$s/tests ] || continue; \
	  (cd services/$$s && python3 -m pytest -q 2>&1 | tail -5) || exit 1; \
	done

.PHONY: test-integration
test-integration: ## Cross-service tests; needs the local stack running
	@printf "$(BOLD)integration tests$(OFF)\n"
	@SOUQ_TEST_DB_URL=postgres://souq:souq_local_only@localhost:5432/inventory \
	  bash tools/integration/run.sh

.PHONY: lint
lint: ## Static analysis across every language
	@printf "$(BOLD)lint$(OFF)\n"
	@for s in $(GO_SERVICES); do (cd services/$$s && go vet ./...) || exit 1; done
	@printf "  go vet clean\n"
	@# gofmt is not a style preference in Go, it is the format. A repository
	@# that drifts from it produces diffs full of whitespace, which is how a
	@# real change hides inside a reformat.
	@unformatted=$$(gofmt -l services/*/ libs/go-modelcheck/ 2>/dev/null); \
	  if [ -n "$$unformatted" ]; then \
	    printf "  $(RED)gofmt$(OFF) these files are not formatted:\n"; \
	    echo "$$unformatted" | sed 's/^/    /'; exit 1; \
	  fi
	@printf "  gofmt clean\n"
	@python3 scripts/java-check.py
	@command -v hadolint >/dev/null && find services apps -name Dockerfile -exec hadolint {} + || \
	  printf "  $(DIM)hadolint not installed; skipping Dockerfile lint$(OFF)\n"

.PHONY: java-check
java-check: ## Cross-reference the Java sources without a JDK (see scripts/java-check.py)
	@printf "$(BOLD)java cross-reference$(OFF)\n"
	@python3 scripts/java-check.py

.PHONY: sql-check
sql-check: ## Apply every migration to a throwaway Postgres and assert the invariants
	@bash scripts/sql-check.sh

.PHONY: frontend
frontend: ## Typecheck and build the storefront and the admin app
	@printf "$(BOLD)frontend$(OFF)\n"
	@# `next build` is the only thing that catches a Server/Client Component
	@# boundary violation — tsc does not model it, so a page that calls a hook
	@# from a server component typechecks cleanly and fails at request time.
	@for app in storefront admin; do \
	  if [ ! -d apps/$$app/node_modules ]; then \
	    printf "  $(DIM)$$app: dependencies not installed; skipping$(OFF)\n"; continue; \
	  fi; \
	  (cd apps/$$app && npx tsc --noEmit) || exit 1; \
	  printf "  $$app typechecks\n"; \
	  (cd apps/$$app && npx next build >/dev/null 2>&1) || { \
	    printf "  $(RED)$$app failed to build$(OFF)\n"; \
	    (cd apps/$$app && npx next build 2>&1 | tail -20); exit 1; }; \
	  printf "  $$app builds\n"; \
	done
	@# A dead internal link typechecks, builds, and 404s when clicked. No
	@# other check here can see it.
	@python3 scripts/link-check.py | sed 's/^/  /'
	@python3 scripts/link-check.py >/dev/null

.PHONY: image-check
image-check: ## Assert Dockerfiles, compose and the CI matrix agree (fast; no build)
	@printf "$(BOLD)image consistency$(OFF)\n"
	@python3 scripts/image-check.py

.PHONY: tf-check
tf-check: ## terraform fmt + validate every module and env
	@bash scripts/tf-check.sh

.PHONY: k8s-check
k8s-check: ## Render both overlays and assert the workload policy
	@bash scripts/k8s-check.sh

## ---------------------------------------------------------------- run

.PHONY: up
up: ## Start datastores, Kafka and LocalStack, then wait for health
	@printf "$(BOLD)starting infrastructure$(OFF)\n"
	@$(COMPOSE) up -d postgres redis mysql mongodb cassandra elasticsearch kafka localstack
	@printf "waiting for health "
	@for i in $$(seq 1 120); do \
	  unhealthy=$$($(COMPOSE) ps --format '{{.Health}}' 2>/dev/null | grep -cv '^healthy$$' || true); \
	  [ "$$unhealthy" = "0" ] && break; printf "."; sleep 2; \
	done; printf "\n"
	@$(COMPOSE) up kafka-init
	@$(COMPOSE) ps
	@printf "\n$(BOLD)infrastructure ready$(OFF)\n"
	@printf "  next: $(CYAN)make up-services$(OFF) then $(CYAN)make seed$(OFF)\n"

.PHONY: up-services
up-services: ## Start all 11 backend services
	@$(COMPOSE) --profile services up -d --build
	@$(COMPOSE) --profile services ps

.PHONY: up-frontend
up-frontend: ## Start the storefront (:3000) and admin (:3001)
	@$(COMPOSE) --profile frontend up -d --build

.PHONY: up-all
up-all: up up-services up-frontend ## Everything
	@$(MAKE) --no-print-directory urls

.PHONY: observability
observability: ## Jaeger, Prometheus, Grafana, Kafka UI, Kibana
	@$(COMPOSE) --profile observability up -d
	@$(MAKE) --no-print-directory urls

.PHONY: urls
urls: ## Print every local URL
	@printf "$(BOLD)storefront$(OFF)      http://localhost:3000\n"
	@printf "$(BOLD)admin$(OFF)           http://localhost:3001\n"
	@printf "\n$(DIM)services$(OFF)\n"
	@printf "  identity        http://localhost:8081\n"
	@printf "  catalog         http://localhost:8082\n"
	@printf "  cart            http://localhost:8083\n"
	@printf "  order           http://localhost:8084\n"
	@printf "  inventory       http://localhost:8085\n"
	@printf "  payment         http://localhost:8086\n"
	@printf "  search          http://localhost:8087\n"
	@printf "  recommendation  http://localhost:8088\n"
	@printf "  pricing         localhost:9089 (grpc)\n"
	@printf "  review          http://localhost:8090\n"
	@printf "  notification    http://localhost:8091\n"
	@printf "\n$(DIM)tooling$(OFF)\n"
	@printf "  jaeger          http://localhost:16686\n"
	@printf "  prometheus      http://localhost:9090\n"
	@printf "  grafana         http://localhost:3030\n"
	@printf "  kafka ui        http://localhost:8090\n"
	@printf "  kibana          http://localhost:5601\n"

.PHONY: down
down: ## Stop everything, keep the data
	@$(COMPOSE) --profile services --profile frontend --profile observability down

.PHONY: nuke
nuke: ## Stop everything and delete the volumes
	@printf "this deletes every local volume. ctrl-c to abort, enter to continue: "; read _
	@$(COMPOSE) --profile services --profile frontend --profile observability down -v
	@printf "gone\n"

.PHONY: logs
logs: ## Tail logs; SVC=order-service to narrow
	@$(COMPOSE) logs -f --tail=200 $(SVC)

.PHONY: ps
ps:
	@$(COMPOSE) ps

## ---------------------------------------------------------------- data

.PHONY: seed
seed: ## Load the demo catalogue, stock levels and users
	@printf "$(BOLD)seeding$(OFF)\n"
	@bash scripts/seed.sh

.PHONY: smoke
smoke: ## End-to-end checkout against the local stack
	@printf "$(BOLD)smoke test$(OFF)\n"
	@bash scripts/smoke.sh

.PHONY: chaos
chaos: ## Kill a participant mid-saga and prove the invariants still hold
	@bash scripts/chaos.sh

.PHONY: load
load: ## k6 load test against checkout
	@command -v k6 >/dev/null || { printf "install k6: https://k6.io/docs/get-started/installation/\n"; exit 1; }
	@k6 run tools/load-test/checkout.js

.PHONY: psql
psql: ## psql into a service database; DB=orders
	@$(COMPOSE) exec postgres psql -U souq -d $(or $(DB),orders)

.PHONY: topics
topics: ## List Kafka topics with their configuration
	@$(COMPOSE) exec kafka kafka-topics.sh --bootstrap-server localhost:29092 --describe

.PHONY: dlq
dlq: ## Show the depth of every dead-letter topic
	@bash scripts/dlq-depth.sh

## ---------------------------------------------------------------- build

.PHONY: build
build: ## Build every container image
	@$(COMPOSE) --profile services --profile frontend build --parallel

.PHONY: images
images: ## Build and tag every image for the registry
	@# Driven by the CI matrix rather than by a list here, so the two cannot
	@# disagree about what exists. Context is the repository root for all of
	@# them: ten copy from libs/ or contracts/, which a per-directory context
	@# cannot reach.
	@python3 scripts/image-check.py >/dev/null || { \
	  printf "$(RED)image configuration is inconsistent$(OFF)\n"; \
	  python3 scripts/image-check.py; exit 1; }
	@python3 -c "import yaml,sys; \
	  print('\n'.join(e['image']+' '+e['dockerfile'] \
	    for e in yaml.safe_load(open('.github/workflows/ci.yml'))['jobs']['images']['strategy']['matrix']['include']))" \
	| while read image dockerfile; do \
	    printf "$(BOLD)%s$(OFF)\n" "$$image"; \
	    docker build --build-arg VERSION=$(GIT_SHA) -f "$$dockerfile" \
	      -t $(REGISTRY)/$$image:$(GIT_SHA) -t $(REGISTRY)/$$image:latest . || exit 1; \
	  done

.PHONY: proto
proto: ## Regenerate gRPC stubs from contracts/proto
	@command -v buf >/dev/null || { printf "install buf: https://buf.build/docs/installation\n"; exit 1; }
	@cd contracts && buf generate

## ---------------------------------------------------------------- infra

.PHONY: tf-plan
tf-plan: ## terraform plan; ENV=dev|prod
	@cd infra/terraform/envs/$(or $(ENV),dev) && terraform init -upgrade && terraform plan

.PHONY: tf-apply
tf-apply: ## terraform apply; ENV=dev|prod
	@cd infra/terraform/envs/$(or $(ENV),dev) && terraform apply

.PHONY: tf-fmt
tf-fmt:
	@terraform fmt -recursive infra/terraform

.PHONY: k8s-diff
k8s-diff: ## Render manifests and diff against the cluster; ENV=dev|prod
	@kubectl kustomize infra/k8s/overlays/$(or $(ENV),dev) | kubectl diff -f - || true

.PHONY: k8s-apply
k8s-apply: ## Apply manifests; ENV=dev|prod
	@kubectl apply -k infra/k8s/overlays/$(or $(ENV),dev)

#!/usr/bin/env bash
# Formats and validates every Terraform module and environment.
#
# The subtlety this exists to encode: **a module that declares
# `configuration_aliases` cannot be validated standalone.**
#
# `modules/edge` needs an `aws.us_east_1` provider, because ACM certificates for
# CloudFront and WAFv2 web ACLs for a global distribution must live in
# us-east-1. The module declares the alias and the ROOT supplies it. Running
# `terraform validate` inside the module directory therefore fails with
# "Provider configuration not present" — not because anything is wrong, but
# because there is no caller to pass it.
#
# CI validated every directory the same way and so reported `edge` as broken
# forever. The fix is to validate such modules through the root that supplies
# their providers, which covers them properly, and skip them standalone.
#
# Usage:  scripts/tf-check.sh
# Exit:   0 clean, 1 findings.

set -uo pipefail
cd "$(dirname "$0")/.."

GREEN=$'\033[0;32m'; RED=$'\033[0;31m'; DIM=$'\033[2m'; BOLD=$'\033[1m'; OFF=$'\033[0m'
failures=0

if ! command -v terraform >/dev/null 2>&1; then
  printf "  ${DIM}terraform not installed; skipping${OFF}\n"
  exit 0
fi

# Shared plugin cache. Without it every module re-downloads ~600 MB of
# providers and the job takes twenty minutes.
export TF_PLUGIN_CACHE_DIR="${TF_PLUGIN_CACHE_DIR:-$PWD/.terraform-plugin-cache}"
mkdir -p "$TF_PLUGIN_CACHE_DIR"

echo "${BOLD}terraform fmt${OFF}"
if ! terraform fmt -check -recursive infra/terraform >/dev/null 2>&1; then
  echo "  ${RED}not formatted${OFF}"
  terraform fmt -check -recursive -diff infra/terraform 2>&1 | head -40 | sed 's/^/    /'
  failures=$((failures + 1))
else
  echo "  ${GREEN}clean${OFF}"
fi

echo
echo "${BOLD}terraform validate${OFF}"

for dir in infra/terraform/modules/*/ infra/terraform/envs/*/; do
  name=$(basename "${dir%/}")

  # Declaring configuration_aliases means "my caller supplies this provider".
  # There is no caller here, so standalone validation cannot succeed. The root
  # environment below instantiates the module and validates it in context.
  if grep -rq 'configuration_aliases' "$dir" 2>/dev/null; then
    printf "  %-16s ${DIM}skipped — needs a provider alias from its caller${OFF}\n" "$name"
    continue
  fi

  printf "  %-16s " "$name"

  if ! init_out=$(terraform -chdir="$dir" init -backend=false -input=false -no-color 2>&1); then
    echo "${RED}init failed${OFF}"
    echo "$init_out" | grep -A4 "Error:" | head -12 | sed 's/^/      /'
    failures=$((failures + 1))
    continue
  fi

  if validate_out=$(terraform -chdir="$dir" validate -no-color 2>&1); then
    echo "${GREEN}valid${OFF}"
  else
    echo "${RED}invalid${OFF}"
    echo "$validate_out" | grep -A4 "Error:" | head -16 | sed 's/^/      /'
    failures=$((failures + 1))
  fi
done

echo
if [ "$failures" -eq 0 ]; then
  echo "${GREEN}terraform is valid${OFF}"
else
  echo "${RED}$failures module(s) failed${OFF}"
  exit 1
fi

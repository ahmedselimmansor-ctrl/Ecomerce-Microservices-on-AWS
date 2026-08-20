#!/bin/bash
# Creates one database per service from POSTGRES_MULTIPLE_DATABASES.
#
# Separate DATABASES rather than separate schemas, deliberately: it makes the
# database-per-service boundary (docs/CONTRACTS.md §6) enforceable locally, so
# a service that reaches across it fails on a developer's laptop instead of in
# staging. In AWS each of these is its own Aurora cluster.
set -euo pipefail

if [ -z "${POSTGRES_MULTIPLE_DATABASES:-}" ]; then
  echo "POSTGRES_MULTIPLE_DATABASES not set; nothing to create"
  exit 0
fi

for db in $(echo "$POSTGRES_MULTIPLE_DATABASES" | tr ',' ' '); do
  echo "creating database '$db'"
  psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" <<-EOSQL
    CREATE DATABASE "$db";
    GRANT ALL PRIVILEGES ON DATABASE "$db" TO "$POSTGRES_USER";
EOSQL
done

echo "created: $POSTGRES_MULTIPLE_DATABASES"

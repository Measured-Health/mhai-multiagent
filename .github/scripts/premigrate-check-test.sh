#!/usr/bin/env bash
# Tests for deploy/premigrate-check.sh (Plan 3 T15). Needs Docker and the
# core image (mhai-core:local, `docker build -t mhai-core:local services/core`).
# Synthetic data only; the throwaway Postgres is removed at the end.
#
#   .github/scripts/premigrate-check-test.sh
set -uo pipefail
root=$(cd "$(dirname "$0")/../.." && pwd)
check="$root/deploy/premigrate-check.sh"
dir=$(mktemp -d)
project=mhai-premigrate-test
pw=k$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')
cat > "$dir/compose.yml" <<YAML
name: $project
services:
  postgres:
    image: postgres:16.15-alpine
    environment: { POSTGRES_DB: mhai, POSTGRES_USER: pgadmin, POSTGRES_PASSWORD: "$pw" }
YAML
compose="docker compose -f $dir/compose.yml"
cleanup() { $compose down -v >/dev/null 2>&1; rm -rf "$dir"; }
trap cleanup EXIT
failures=0
expect() { # name, want exit code, then env assignments for the check
  local name=$1 want=$2; shift 2
  out=$(env PREMIGRATE_COMPOSE="${COMPOSE_OVERRIDE:-$compose}" "$@" bash "$check" 2>&1); got=$?
  if [ "$got" = "$want" ]; then echo "ok   $name (exit $got)"; else echo "FAIL $name: exit $got, want $want: $out"; failures=$((failures + 1)); fi
}
psql() { $compose exec -T postgres psql -U pgadmin -d mhai -v ON_ERROR_STOP=1 -qtAc "$1" >/dev/null; }

expect "fresh database: nothing to check" 0
docker run --rm --network "${project}_default" \
  -e DATABASE_URL="postgres://pgadmin:$pw@postgres:5432/mhai?sslmode=disable" mhai-core:local migrate up >/dev/null || exit 1
expect "00007 applied: nothing to check" 0
# The state before 00007: its constraints gone, its version not applied.
psql "ALTER TABLE clinical_queries DROP CONSTRAINT clinical_queries_status_check, DROP CONSTRAINT clinical_queries_error_check;
      UPDATE goose_db_version SET is_applied = false WHERE version_id >= 7;
      INSERT INTO hospitals (id, name, short_code, time_zone) VALUES ('00000000-0000-4000-8000-00000000aa01', 'Synthetic', 'PRE', 'UTC');
      INSERT INTO clinical_queries (id, hospital_id, clinical_question, urgency, status)
        VALUES (gen_random_uuid(), '00000000-0000-4000-8000-00000000aa01', 'synthetic', 'routine', 'pending')"
expect "pre-00007, a status 00007 rejects" 1
psql "UPDATE clinical_queries SET status = 'failed', error = 'Synthetic free text'"
expect "pre-00007, a free-text error" 1
psql "UPDATE clinical_queries SET error = 'internal'"
expect "pre-00007, rows that fit" 0
expect "psql fails (no such database): fails closed" 1 PGDATABASE=no_such_database

# A compose that starts nothing and whose Postgres never answers.
cat > "$dir/down-compose" <<'SH'
#!/bin/sh
case "$*" in *pg_isready*) exit 2;; *psql*) exit 2;; esac
exit 0
SH
# A compose whose psql answers garbage.
cat > "$dir/odd-compose" <<'SH'
#!/bin/sh
case "$*" in *pg_isready*) exit 0;; *psql*) echo "maybe"; exit 0;; esac
exit 0
SH
chmod +x "$dir/down-compose" "$dir/odd-compose"
COMPOSE_OVERRIDE="$dir/down-compose" expect "Postgres never ready: fails closed" 1 PREMIGRATE_TRIES=2
COMPOSE_OVERRIDE="$dir/odd-compose" expect "unexpected answer: fails closed" 1

[ "$failures" = 0 ] && echo "premigrate-check: all passed" || { echo "premigrate-check: $failures failed"; exit 1; }

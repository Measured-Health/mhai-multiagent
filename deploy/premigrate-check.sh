#!/usr/bin/env bash
# Run by .github/workflows/deploy.yml before the migrations, in
# /srv/mhai-backend. Read-only and idempotent.
#
# Migration 00007 (Plan 3) adds CHECK constraints that validate the
# existing clinical_queries rows (status in a closed set; error a code, not
# text). Each migration is its own transaction, so a bad row would stop
# 00007 whole and leave the old core running. This check stops the deploy
# first, by name, before anything migrates. Once 00007 is applied, and on a
# database with no clinical_queries yet, it does nothing.
#
# Fails closed: a query that errors, an answer that isn't the expected
# shape, or a Postgres that never answers stops the deploy (exit 1).
#
# PREMIGRATE_COMPOSE overrides the compose command, PREMIGRATE_TRIES the
# readiness tries (tests).
set -euo pipefail
compose=${PREMIGRATE_COMPOSE:-docker compose}
tries=${PREMIGRATE_TRIES:-30}
fail() { echo "::error::pre-migration check: $1 (deploy/README.md, Plan 3 pre-merge check)." >&2; exit 1; }
q() { $compose exec -T postgres psql -U "${PGUSER:-pgadmin}" -d "${PGDATABASE:-mhai}" -v ON_ERROR_STOP=1 -tAc "$1"; }
count() { # a query that must answer one non-negative integer
  local r
  r=$(q "$1") || fail "could not query Postgres"
  case "$r" in ''|*[!0-9]*) fail "unexpected answer from Postgres" ;; esac
  printf '%s' "$r"
}

# Postgres is normally running already; up -d leaves it alone then.
$compose up -d postgres >/dev/null || fail "could not start postgres"
ready=0
for _ in $(seq 1 "$tries"); do
  if $compose exec -T postgres pg_isready -q -U "${PGUSER:-pgadmin}" -d "${PGDATABASE:-mhai}"; then ready=1; break; fi
  sleep 2
done
[ "$ready" = 1 ] || fail "Postgres never became ready"

r=$(q "SELECT to_regclass('public.clinical_queries') IS NOT NULL AND to_regclass('public.goose_db_version') IS NOT NULL") ||
  fail "could not query Postgres"
case "$r" in
  t) ;;
  f) echo "pre-migration check: no clinical_queries yet"; exit 0 ;;
  *) fail "unexpected answer from Postgres" ;;
esac
# count runs in a subshell: its fail ends only that, so check its status.
applied=$(count "SELECT count(*) FROM goose_db_version WHERE version_id = 7 AND is_applied") || exit 1
if [ "$applied" != 0 ]; then
  echo "pre-migration check: 00007 already applied"
  exit 0
fi
bad=$(count "SELECT count(*) FROM clinical_queries
  WHERE status NOT IN ('queued', 'running', 'completed', 'failed', 'cancelled')
     OR (error IS NOT NULL AND error !~ '^[a-z_]{1,32}\$')") || exit 1
[ "$bad" = 0 ] || fail "clinical_queries has $bad row(s) that migration 00007's constraints reject; nothing was migrated"
echo "pre-migration check: clinical_queries rows fit 00007's constraints"

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
# PREMIGRATE_COMPOSE overrides the compose command (tests).
set -euo pipefail
compose=${PREMIGRATE_COMPOSE:-docker compose}
q() { $compose exec -T postgres psql -U "${PGUSER:-pgadmin}" -d "${PGDATABASE:-mhai}" -v ON_ERROR_STOP=1 -tAc "$1"; }

$compose up -d --wait postgres >/dev/null
if [ "$(q "SELECT to_regclass('public.clinical_queries') IS NOT NULL AND to_regclass('public.goose_db_version') IS NOT NULL")" != t ]; then
  echo "pre-migration check: no clinical_queries yet"
  exit 0
fi
if [ "$(q "SELECT count(*) FROM goose_db_version WHERE version_id = 7 AND is_applied")" != 0 ]; then
  echo "pre-migration check: 00007 already applied"
  exit 0
fi
bad=$(q "SELECT count(*) FROM clinical_queries
  WHERE status NOT IN ('queued', 'running', 'completed', 'failed', 'cancelled')
     OR (error IS NOT NULL AND error !~ '^[a-z_]{1,32}\$')")
if [ "$bad" != 0 ]; then
  echo "::error::clinical_queries has $bad row(s) that migration 00007's constraints reject; nothing was migrated (deploy/README.md, Plan 3 pre-merge check)."
  exit 1
fi
echo "pre-migration check: clinical_queries rows fit 00007's constraints"

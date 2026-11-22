#!/usr/bin/env bash
# The live site (repository variable SITE_MODE=live; deploy/README.md, "Live
# site"). Run by the "Deploy backend demo" workflow instead of
# demo/enable-demo.sh, from /srv/mhai-backend (the workflow ships this file
# with the bundle), twice. Idempotent.
#
#   bash live-site.sh precheck   before core (re)starts, against Postgres
#                                alone (started if needed)
#   bash live-site.sh            after core is healthy on the migrated database
#
# Between the two the workflow stops core on any failure (its EXIT trap).
#
# It runs no demo step. It fails closed:
#   - it writes the marker live-site first: from then on demo/enable-demo.sh
#     and demo/reset-demo.sh refuse on this host, whatever runs them, until
#     an operator removes the marker by hand;
#   - sample-ai (the demo's sample answers) is stopped and removed, and the
#     demo's markers demo/seeded and demo/reset are removed (mhai-web's demo
#     reads demo/seeded);
#   - .env must not turn on the demo profile (COMPOSE_PROFILES);
#   - a demo hospital (is_demo) that is not deactivated stops core and fails
#     the deploy: the seeded demo accounts have published passwords, which
#     must never sign in on the live site. Remove the demo data first
#     (deploy/README.md, "Live site: removing the demo data"). A query that
#     fails or answers anything but a count stops core too. Deactivating the
#     hospital is what ends its accounts' sessions (core refuses their
#     refresh at once); this script writes nothing to the database.
#
# It prints counts only (no names, no ids) and never a secret. It never
# touches the EMR containers, mhai-web or nginx.
#
# MHAI_BACKEND_DIR overrides the folder (tests).
set -euo pipefail

# LIVE_SITE_COMPOSE: a program that stands in for `docker compose` (tests).
dc() { if [ -n "${LIVE_SITE_COMPOSE:-}" ]; then "$LIVE_SITE_COMPOSE" "$@"; else docker compose "$@"; fi; }
DEST=${MHAI_BACKEND_DIR:-/srv/mhai-backend}

say()  { printf '\n==> %s\n' "$*"; }
info() { printf '    %s\n' "$*"; }
fail() { printf '::error::live site: %s (deploy/README.md, "Live site").\n' "$*" >&2; exit 1; }
q() { dc exec -T postgres psql -U "${PGUSER:-pgadmin}" -d "${PGDATABASE:-mhai}" -v ON_ERROR_STOP=1 -tAc "$1"; }

case "${1:-}" in
  precheck) STAGE=precheck ;;
  '') STAGE=check ;;
  *) fail "usage: live-site.sh [precheck]" ;;
esac

cd "$DEST"
[ -f .env ] || fail "$DEST is not set up (deploy/setup-host.sh)"

say "Live-site marker"
# First, so the demo scripts refuse on this host even if a later step fails.
date -u +%Y-%m-%dT%H:%M:%SZ > live-site
chmod 644 live-site
info "$DEST/live-site written: demo/enable-demo.sh and demo/reset-demo.sh refuse here"

say "No demo profile in .env"
profiles=$(sed -n 's/^COMPOSE_PROFILES=//p' .env | tail -n1)
case ",$profiles," in
  *,demo,*) fail ".env has COMPOSE_PROFILES with demo; remove it" ;;
esac
info "none"

say "No sample answers"
# A compose that can't answer fails here: never assume sample-ai is off.
running=$(dc --profile demo ps -q sample-ai) || fail "could not list sample-ai"
if [ -n "$running" ]; then
  dc --profile demo stop sample-ai >/dev/null
  dc --profile demo rm -f sample-ai >/dev/null
  info "sample-ai stopped and removed"
fi
running=$(dc --profile demo ps -q sample-ai) || fail "could not list sample-ai"
[ -z "$running" ] || fail "sample-ai is still there"
info "sample-ai is not running"

say "No demo markers"
rm -f demo/seeded demo/reset
info "demo/seeded and demo/reset removed (if they were there)"

say "No demo hospital that can sign in ($STAGE)"
if [ "$STAGE" = precheck ]; then
  # Postgres alone; up -d leaves a running one as it is.
  dc up -d postgres >/dev/null || { dc stop core >/dev/null 2>&1 || true; fail "could not start postgres; core stopped"; }
  ready=0
  for _ in $(seq 1 "${LIVE_SITE_TRIES:-30}"); do
    if dc exec -T postgres pg_isready -q -U "${PGUSER:-pgadmin}" -d "${PGDATABASE:-mhai}"; then ready=1; break; fi
    sleep 2
  done
  [ "$ready" = 1 ] || { dc stop core >/dev/null 2>&1 || true; fail "Postgres never became ready; core stopped"; }
fi
# A database with no hospitals table yet (a new one, before the first
# migration) holds no demo hospital. Two queries: Postgres resolves every
# table a statement names before it runs, so one CASE over a missing table
# fails ("relation does not exist") even on the branch that never runs.
t=$(q "SELECT to_regclass('public.hospitals') IS NOT NULL") || {
  dc stop core >/dev/null 2>&1 || true
  fail "could not query Postgres; core stopped"
}
case "$t" in
  f) r=0 ;;
  t) r=$(q "SELECT count(*) FROM hospitals WHERE is_demo AND status <> 'deactivated'") || {
       dc stop core >/dev/null 2>&1 || true
       fail "could not query Postgres; core stopped"
     } ;;
  *) dc stop core >/dev/null 2>&1 || true
     fail "unexpected answer from Postgres; core stopped" ;;
esac
case "$r" in
  ''|*[!0-9]*)
    dc stop core >/dev/null 2>&1 || true
    fail "unexpected answer from Postgres; core stopped" ;;
esac
if [ "$r" != 0 ]; then
  dc stop core >/dev/null 2>&1 || true
  fail "$r demo hospital(s) not deactivated: their seeded accounts have published passwords. Core is stopped; remove the demo data, then deploy again"
fi
info "none (demo hospitals that are deactivated, if any, can't sign in)"

say "Live site checks passed ($STAGE)"

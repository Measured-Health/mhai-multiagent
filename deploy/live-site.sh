#!/usr/bin/env bash
# The live site (repository variable SITE_MODE=live; deploy/README.md, "Live
# site"). Run by the "Deploy backend" workflow instead of
# demo/enable-demo.sh, after core is healthy, from /srv/mhai-backend (the
# workflow ships this file with the bundle). Idempotent.
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
#     (deploy/README.md, "Live site: removing the demo data").
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

say "No demo hospital that can sign in"
r=$(q "SELECT count(*) FROM hospitals WHERE is_demo AND status <> 'deactivated'") || {
  dc stop core >/dev/null 2>&1 || true
  fail "could not query Postgres; core stopped"
}
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

say "Live site ready"

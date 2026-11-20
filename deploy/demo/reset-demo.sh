#!/usr/bin/env bash
# The public demo's scheduled reset (deploy/README.md, "Public demo: the
# scheduled reset"). Run by the "Reset backend demo" workflow, never by hand
# on the host, from /srv/mhai-backend, where "Deploy backend demo" ships this
# file with the rest of the bundle. It runs the one-shot core-demo-reset
# (core demo reset): the seed's own code and guards, so it only ever touches
# the demo hospital DEM in a database of demo hospitals.
#
# Nothing to do, and a pass:
#   - the pilot's pipeline is on this host (the demo is off; enable-demo.sh);
#   - the demo was never seeded here (no demo/seeded marker: the seed refused,
#     for example beside a non-demo hospital, and the reset would refuse too).
# A refusal or failure of the reset itself fails the workflow; its message
# names the reason (ids and counts only, nothing secret).
#
# It starts nothing else, pulls nothing, and never touches the EMR
# containers, the web demo or nginx.

set -euo pipefail
umask 077

DEST=${MHAI_BACKEND_DIR:-/srv/mhai-backend}

say()  { printf '\n==> %s\n' "$*"; }
info() { printf '    %s\n' "$*"; }
die()  { printf '::error::%s\n' "$*" >&2; exit 1; }

cd "$DEST"
[ -f .env ] || die "$DEST is not set up (deploy/setup-host.sh)"
env_get() { sed -n "s/^$1=//p" .env | tail -n1; }

say "Not beside the pilot"
case ",$(env_get COMPOSE_PROFILES)," in
  *,pilot,*) info "the pilot profile is on: the demo is off, nothing to reset"; exit 0 ;;
esac
if [ -n "$(docker compose --profile pilot ps -q pipeline 2>/dev/null)" ]; then
  info "the pilot's pipeline is running: the demo is off, nothing to reset"
  exit 0
fi

say "Seeded demo"
if [ ! -f demo/seeded ]; then
  info "no demo/seeded marker: the demo seed has not run here (or refused); nothing to reset"
  exit 0
fi
info "seeded $(cat demo/seeded)"

say "Core is up (the reset needs Postgres and NATS)"
[ -n "$(docker compose ps -q --status running core 2>/dev/null)" ] || die "core is not running: deploy first"

say "core demo reset"
if ! docker compose --profile demo run --rm -T --no-deps core-demo-reset </dev/null; then
  die "core demo reset refused or failed (its message above names the reason; nothing secret)"
fi
mkdir -p demo
chmod 755 demo
date -u +%Y-%m-%dT%H:%M:%SZ > demo/reset
chmod 644 demo/reset
info "reset (marker $DEST/demo/reset)"

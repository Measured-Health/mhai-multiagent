#!/usr/bin/env bash
# The deploy's production-mode check (deploy/README.md, "Production mode"),
# run in /srv/mhai-backend before anything restarts. With compose.production.yml
# in COMPOSE_FILE: every TLS folder the overlay mounts is there (a missing
# bind source would come up as an empty folder Docker made, and the service
# would fail later), the certificates verify against the CA and none has
# expired; it warns under 30 days. Without production mode it checks nothing.
# The production-mode marker (read by mhai-web's deploy) follows the mode.
# Reads public certificates only (tls/public); prints no secret.
set -euo pipefail

DEST=${MHAI_BACKEND_DIR:-$PWD}
cd "$DEST"
die() { printf '::error::production mode: %s (deploy/README.md, "Production mode").\n' "$*" >&2; exit 1; }
[ -f .env ] || die "no .env in $DEST"
files=$(sed -n 's/^COMPOSE_FILE=//p' .env | tail -n1)
case ":$files:" in
  *:compose.production.yml:*) ;;
  *)
    if [ -e production-mode ]; then rm -f production-mode; echo "production mode: off (marker removed)"; else echo "production mode: off"; fi
    exit 0 ;;
esac
env_get() { sed -n "s/^$1=//p" .env | tail -n1; }

need=(nats postgres core)
[ "$(env_get NOTIFY_ENABLED)" != true ] || need+=(notifications)
[ "$(env_get PIPELINE_ENABLED)" != true ] || need+=(pipeline)
[ "$(env_get EMR_ENABLED)" != true ] || need+=(emr)
[ "$(env_get OPENBAO_ENABLED)" != true ] || need+=(openbao)
for f in compose.production.yml nats/tls.conf postgres/pg_hba.production.conf tls/public/ca.pem; do
  [ -f "$f" ] || die "$f is missing: run production/enable-production.sh"
done
for s in "${need[@]}"; do
  [ -d "tls/$s" ] && [ ! -L "tls/$s" ] || die "tls/$s is missing: run production/enable-production.sh"
  [ -f "tls/public/$s.pem" ] || die "tls/public/$s.pem is missing: run production/enable-production.sh"
  openssl verify -CAfile tls/public/ca.pem "tls/public/$s.pem" >/dev/null 2>&1 || die "tls/public/$s.pem is not from tls/public/ca.pem"
  end=$(openssl x509 -in "tls/public/$s.pem" -noout -enddate | sed 's/^notAfter=//')
  left=$(( ($(date -d "$end" +%s) - $(date +%s)) / 86400 ))
  [ "$left" -ge 0 ] || die "the $s certificate has expired: production/enable-production.sh --renew --ca-dir <the CA>"
  [ "$left" -ge 30 ] || echo "::warning::production mode: the $s certificate expires in $left days: renew it (README, \"Production mode: renewal\")"
done
[ -e production-mode ] || { date -u +%Y-%m-%dT%H:%M:%SZ > production-mode; chmod 644 production-mode; }
echo "production mode: on (${need[*]}; certificates good)"

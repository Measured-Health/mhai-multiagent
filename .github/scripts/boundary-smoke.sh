#!/usr/bin/env bash
# The end-to-end smoke test (Plan 3 Task 15, spec §12): start the smoke
# stack (docker-compose.yml + docker-compose.smoke.yml), run
# services/core/internal/smoke as the clinical BFF over NATS, and take the
# stack down again, volumes included.
#
#   .github/scripts/boundary-smoke.sh [--build]
#
# --build builds the images first; without it, compose uses mhai-core:local
# and mhai-intelligence:cpu if they exist (CI builds them with its layer
# cache) and builds them otherwise.
#
# Every secret is generated here for this run, kept in a mode-600 temporary
# env file, and removed at the end. None is printed. Override the host
# ports with POSTGRES_PORT, NATS_PORT and CORE_PORT, and the compose project
# with SMOKE_PROJECT, when the defaults clash on a shared machine.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
cd "$root"

build=()
case "${1:-}" in
  --build) build=(--build) ;;
  "") ;;
  *) echo "usage: $0 [--build]" >&2; exit 2 ;;
esac

project=${SMOKE_PROJECT:-mhai-smoke}
pg_port=${POSTGRES_PORT:-5432}
nats_port=${NATS_PORT:-4222}
core_port=${CORE_PORT:-8080}

umask 077
envfile=$(mktemp "${TMPDIR:-/tmp}/mhai-smoke-env.XXXXXX")

# Letters and digits only, starting with a letter: they go into URLs and
# the NATS config (services/nats/nats.conf).
secret() { printf 'k%s' "$(od -An -N24 -tx1 /dev/urandom | tr -d ' \n')"; }

pg_pw=$(secret)
gateway_pw=$(secret)
bff_clinical_pw=$(secret)
bff_key=$(secret)
cat > "$envfile" <<EOF
POSTGRES_DB=mhai
POSTGRES_USER=mhai
POSTGRES_PASSWORD=$pg_pw
POSTGRES_PORT=$pg_port
NATS_PORT=$nats_port
CORE_PORT=$core_port
CORE_DB_PASSWORD=$(secret)
PROVISION_DB_PASSWORD=$(secret)
INTELLIGENCE_DB_PASSWORD=$(secret)
MFA_ENCRYPTION_KEY=$(secret)
BFF_SERVICE_KEYS=$bff_key
NATS_GATEWAY_PASSWORD=$gateway_pw
NATS_PIPELINE_PASSWORD=$(secret)
NATS_ECHO_PASSWORD=$(secret)
NATS_BFF_CLINICAL_PASSWORD=$bff_clinical_pw
NATS_BFF_PLATFORM_PASSWORD=$(secret)
NATS_JS_KEY=$(secret)$(secret)
EOF

compose=(docker compose -p "$project" --env-file "$envfile"
  -f "$root/docker-compose.yml" -f "$root/docker-compose.smoke.yml")

status=1
cleanup() {
  if [ "$status" != 0 ]; then
    echo "smoke: failed; recent logs" >&2
    "${compose[@]}" logs --no-color --tail=60 core pipeline echo nats core-migrate >&2 || true
  fi
  "${compose[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
  rm -f "$envfile"
}
trap cleanup EXIT

"${compose[@]}" config -q
"${compose[@]}" up -d "${build[@]}" --wait --wait-timeout 300

# core has no compose health check (distroless): wait for readyz.
ready=0
for _ in $(seq 1 60); do
  if [ "$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$core_port/api/v1/readyz" || true)" = 200 ]; then
    ready=1
    break
  fi
  sleep 2
done
if [ "$ready" != 1 ]; then
  echo "smoke: core not ready after 120 s" >&2
  exit 1
fi
echo "smoke: stack up (core ready)"

export SMOKE_NATS_URL="nats://127.0.0.1:$nats_port"
export SMOKE_NATS_BFF_CLINICAL_PASSWORD=$bff_clinical_pw
export SMOKE_NATS_GATEWAY_PASSWORD=$gateway_pw
export SMOKE_DATABASE_URL="postgres://mhai:$pg_pw@127.0.0.1:$pg_port/mhai?sslmode=disable"
export SMOKE_CORE_URL="http://127.0.0.1:$core_port"
export SMOKE_BFF_SERVICE_KEY=$bff_key
export SMOKE_CORE_CLI="${compose[*]} run --rm --no-deps -T core-cli"

(cd services/core && go test -tags smoke -count=1 -v ./internal/smoke/...)
status=0
echo "smoke: passed"

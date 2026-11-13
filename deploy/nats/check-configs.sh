#!/usr/bin/env bash
# Starts nats-server -t on the NATS configs exactly as they are deployed,
# with the default variables, so a config or a default key nats-server
# refuses fails CI instead of stopping NATS (and with it core and the demo):
#   - deploy/nats/nats.conf with the nats service's environment as
#     `docker compose config` renders it from deploy/compose.yml (the
#     defaults, plus the three public keys setup-host.sh writes to .env);
#   - services/nats/nats.conf as dev compose mounts it, with the emr user
#     off (emr-off.conf) and on (emr.conf).
# The image is the deploy's pinned NATS image. Needs docker and docker
# compose v2; run from the repository root. Prints no secret: every value
# here is a test value.
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
cd "$ROOT"
# The checkout as the Docker daemon sees it, for the bind mounts: the same
# path in CI; set NATS_CHECK_HOST_ROOT when docker runs on another host's
# daemon (a container with the host's socket).
SRCROOT=${NATS_CHECK_HOST_ROOT:-$ROOT}
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# Valid public user NKeys whose seeds were thrown away: stand-ins for the
# keys setup-host.sh puts in .env (test values, nobody can sign in).
cat > "$work/deploy.env" <<'E'
POSTGRES_PASSWORD=test0only0postgres0password
CORE_DB_PASSWORD=test0only0core0password0
PROVISION_DB_PASSWORD=test0only0provision0pw
MFA_ENCRYPTION_KEY=test0only0mfa0key0test0only0mfa0
BFF_SERVICE_KEYS=test0only0bff0key0test0only0bff0
NATS_GATEWAY_NKEY=UBX5TN23JN3AGNKAN4MABJFON37GDBKA6AII3RCKVWKTGSBE6WUTTPL4
NATS_BFF_NKEY=UDSUWROES6WBAXF6XZO7TXQ2YM2YMJPIPSGOHF3OO3JUFOYQKVXWGKRV
NATS_BFF_PLATFORM_NKEY=UCAAJGKAIDMU5SIXQWLWPWO5MJJMRUCMVTZ2MTVTXH75WFBW4TDX4TT7
E
rendered=$(docker compose -p mhai-backend -f deploy/compose.yml --env-file "$work/deploy.env" config --format json)
image=$(printf '%s' "$rendered" | jq -r '.services.nats.image')
case "$image" in nats:[0-9]*.[0-9]*.[0-9]*-alpine) ;; *) echo "the deploy's NATS image is not pinned: $image" >&2; exit 1 ;; esac
printf '%s' "$rendered" | jq -r '.services.nats.environment | to_entries[] | "\(.key)=\(.value)"' > "$work/nats.env"
grep -q '^NATS_PIPELINE_NKEY=U' "$work/nats.env" || { echo "the nats service has no NATS_PIPELINE_NKEY default" >&2; exit 1; }

check() {
  local name=$1; shift
  if out=$(docker run --rm --network none "$@" "$image" -t -c /etc/nats/nats.conf 2>&1); then
    echo "ok   $name"
  else
    echo "FAIL $name" >&2
    printf '%s\n' "$out" >&2
    return 1
  fi
}

check "deploy/nats/nats.conf (default deploy, $image)" --env-file "$work/nats.env" \
  -v "$SRCROOT/deploy/nats/nats.conf:/etc/nats/nats.conf:ro"

cat > "$work/dev.env" <<'E'
NATS_GATEWAY_PASSWORD=testonlygateway0001
NATS_PIPELINE_PASSWORD=testonlypipeline0001
NATS_ECHO_PASSWORD=testonlyecho0001
NATS_BFF_CLINICAL_PASSWORD=testonlyclinical0001
NATS_BFF_PLATFORM_PASSWORD=testonlyplatform0001
NATS_EMR_PASSWORD=testonlyemr0001
NATS_JS_KEY=testonlyjetstreamkey0000000000000001
E
for emr in emr-off.conf emr.conf; do
  check "services/nats/nats.conf (dev, $emr)" --env-file "$work/dev.env" \
    -v "$SRCROOT/services/nats/nats.conf:/etc/nats/nats.conf:ro" \
    -v "$SRCROOT/services/nats/bff-clinical.test.conf:/etc/nats/bff-clinical.test.conf:ro" \
    -v "$SRCROOT/services/nats/$emr:/etc/nats/emr.conf:ro"
done

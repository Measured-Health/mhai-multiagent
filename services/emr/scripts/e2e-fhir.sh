#!/usr/bin/env bash
# The FHIR path end to end on synthetic patients (EMR plan Task 25):
# Postgres with core's migrations, the real egress-emr, the synthetic FHIR
# server (emr synth-fhir, development only) on the proxy's outside network,
# and a test container on exactly the emr service's networks (backend and
# emr-proxy: no route out) that saves a connection, writes the allow-list,
# signs in and syncs through egress-emr, normalises, and checks that the
# records reached the canonical tables and that no synthetic name, MRN or
# value is in any log or message.
#
# Run on a Docker host with Go (the build server, inside the remote runner),
# never in CI and never against a real FHIR server:
#
#   bash services/emr/scripts/e2e-fhir.sh
#
# KEEP_FOR_DEBUG=1 leaves the project up for a person to inspect.
set -euo pipefail
exec </dev/null

here=$(cd "$(dirname "$0")" && pwd)
svc=$(cd "$here/../.." && pwd)
project=mhai-emr-e2e-$$
image=mhai-emr:$project
runner=mhai-emr-e2e-runner:$project
work=$(mktemp -d)
seed=${E2E_SEED:-7}
patients=${E2E_PATIENTS:-12}

cleanup() {
  if [ -n "${KEEP_FOR_DEBUG:-}" ]; then echo "kept: project $project, work $work"; return; fi
  docker compose -p "$project" -f "$work/compose.yml" down -v --remove-orphans >/dev/null 2>&1 || true
  docker rmi -f "$runner" "$image" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT

echo "building the emr image and the test binary"
docker build -q -t "$image" -f "$svc/emr/Dockerfile" "$svc" >/dev/null
(cd "$svc/emr" && CGO_ENABLED=0 go test -c -tags e2e -o "$work/e2e.test" ./internal/e2e/)
cp -r "$svc/core/internal/db/migrations" "$work/migrations"
cp -r "$svc/emr/codes" "$work/codes"
cp -r "$svc/emr/maps" "$work/maps"
cat > "$work/Dockerfile" <<'DOCKER'
FROM alpine:3.20
COPY e2e.test /e2e.test
COPY migrations /migrations
COPY codes /codes
COPY maps /maps
ENTRYPOINT ["/e2e.test", "-test.v", "-test.count=1", "-test.timeout=10m"]
DOCKER
docker build -q -t "$runner" "$work" >/dev/null

# Fixed subnets: backend and emr-proxy are internal; the synthetic server
# sits on the proxy's outside network, which development's
# EGRESS_EMR_DEV_EXEMPT takes out of the proxy's own networks, and which
# the connection declares as its private range.
cat > "$work/compose.yml" <<YML
name: $project
services:
  postgres:
    image: postgres:16-alpine
    environment:
      POSTGRES_DB: mhai
      POSTGRES_USER: mhai_super
      POSTGRES_PASSWORD: synthetic-e2e-only
    networks: [backend]
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U mhai_super -d mhai"]
      interval: 2s
      timeout: 3s
      retries: 30
  proxy-key:
    image: $image
    command: ["keys", "proxy-init", "--out", "/emr-proxy-key"]
    volumes: [emr-proxy-key:/emr-proxy-key]
    network_mode: none
  synth-fhir:
    image: $image
    user: "0:0"
    command: ["synth-fhir", "--addr", ":443", "--seed", "$seed", "--patients", "$patients",
              "--name", "fhir.synthetic.example", "--out", "/synth"]
    environment:
      APP_ENV: development
    volumes: [synth:/synth]
    networks:
      egress:
        ipv4_address: 10.233.5.10
        aliases: [fhir.synthetic.example]
  egress-emr:
    image: $image
    entrypoint: ["/egress-emr"]
    command: []
    environment:
      EGRESS_EMR_ALLOW_LIST: /emr-egress/allow.json
      EGRESS_EMR_KEY_PATH: /emr-proxy-key/k_proxy
      EGRESS_EMR_ENV: development
      EGRESS_EMR_INTERNAL_CIDRS: "10.233.0.0/24,10.233.1.0/24"
      EGRESS_EMR_DEV_EXEMPT: "10.233.5.0/24"
    volumes:
      - emr-egress:/emr-egress:ro
      - emr-proxy-key:/emr-proxy-key:ro
    read_only: true
    cap_drop: [ALL]
    networks: [emr-proxy, egress]
    depends_on:
      proxy-key: { condition: service_completed_successfully }
  runner:
    image: $runner
    environment:
      E2E_DATABASE_URL: postgres://mhai_super:synthetic-e2e-only@postgres:5432/mhai?sslmode=disable
      E2E_MIGRATIONS: /migrations
      E2E_CODES: /codes/testdata
      E2E_MAPS: /maps
      E2E_PROXY_URL: http://egress-emr:3128
      E2E_PROXY_KEY: /emr-proxy-key/k_proxy
      E2E_EGRESS_DIR: /emr-egress
      E2E_SYNTH_DIR: /synth
      E2E_FHIR_NAME: fhir.synthetic.example
      E2E_FHIR_CIDR: 10.233.5.0/24
      E2E_SEED: "$seed"
      E2E_PATIENTS: "$patients"
    volumes:
      - emr-egress:/emr-egress
      - emr-proxy-key:/emr-proxy-key:ro
      - synth:/synth:ro
    networks: [backend, emr-proxy]
    depends_on:
      postgres: { condition: service_healthy }
      egress-emr: { condition: service_started }
      synth-fhir: { condition: service_started }
volumes:
  emr-egress: {}
  emr-proxy-key: {}
  synth: {}
networks:
  backend:
    internal: true
    ipam: { config: [{ subnet: 10.233.0.0/24 }] }
  emr-proxy:
    internal: true
    ipam: { config: [{ subnet: 10.233.1.0/24 }] }
  egress:
    internal: true
    ipam: { config: [{ subnet: 10.233.5.0/24 }] }
YML

compose() { docker compose -p "$project" -f "$work/compose.yml" "$@"; }
compose up -d postgres egress-emr synth-fhir >/dev/null
# Wait for the synthetic server's files.
for _ in $(seq 1 60); do
  if compose logs synth-fhir 2>/dev/null | grep -q "serving"; then break; fi
  sleep 1
done
compose logs synth-fhir 2>/dev/null | grep -q "serving" || { echo "FAIL the synthetic server didn't start"; compose logs synth-fhir; exit 1; }

set +e
compose run --rm runner
rc=$?
set -e
echo "--- egress-emr log (codes only):"
compose logs --no-log-prefix egress-emr 2>/dev/null | tail -n 20
if compose logs --no-log-prefix egress-emr 2>/dev/null | grep -q "fhir.synthetic.example"; then
  echo "FAIL egress-emr logged a host name"; rc=1
fi
if [ "$rc" -eq 0 ]; then echo "PASS synthetic FHIR end to end through egress-emr"; else echo "FAIL synthetic FHIR end to end ($rc)"; fi
exit "$rc"

#!/usr/bin/env bash
# The pipeline worker on every deploy (deploy/README.md, "Answering with our own model
# only"). The deploy workflow runs this in /srv/mhai-backend after core is healthy and
# after gpu-model/gpu-model.sh, so our own model is already serving (or the deploy has
# already failed) when the worker starts.
#
#   - PIPELINE_ENABLED=true in .env (deploy/pilot/enable-pilot.sh writes it): checks the
#     files and settings the worker needs, pulls PIPELINE_IMAGE, starts or refreshes the
#     pipeline (and the egress proxy when a cloud provider is set), and waits for the
#     worker's start line. Anything missing fails the deploy, naming what to run.
#   - Anything else: nothing is touched. A pipeline started by hand (the demo host's
#     pilot) keeps running as it is.
#
# Which hospitals are answered by which route is each hospital's model policy, never
# this script. A route this host doesn't serve escalates every question on it.
#
# It prints no secret (it reads only whether the secret files and values exist) and
# touches only the mhai-backend compose project.
set -euo pipefail

DEST=${MHAI_BACKEND_DIR:-$PWD}
START_TIMEOUT=${PIPELINE_START_TIMEOUT:-180}
INTERVAL=${PIPELINE_POLL_INTERVAL:-5}
# The pipeline's cap is 1600 MiB (compose.yml), plus some room.
MIN_AVAILABLE_MIB=${PIPELINE_MIN_AVAILABLE_MIB:-1700}
MEMINFO=${MEMINFO:-/proc/meminfo}

die() { printf '::error::pipeline: %s\n' "$*" >&2; exit 1; }
info() { printf 'pipeline: %s\n' "$*"; }

cd "$DEST"
[ -f .env ] || die "no .env in $DEST"
env_get() { sed -n "s/^$1=\(.*\)\$/\1/p" .env | tail -n1; }

if [ "$(env_get PIPELINE_ENABLED)" != true ]; then
  info "not managed by the deploy (PIPELINE_ENABLED is not true)"
  exit 0
fi

provider=$(env_get PILOT_CLOUD_PROVIDER)
gpu=$(env_get GPU_MODEL_ENABLED)
roles=$(env_get PILOT_MODEL_ROLES)

# The routes this worker can answer. None: every question would escalate, which is
# never what PIPELINE_ENABLED=true means.
routes=()
[ -z "$provider" ] || routes+=("cloud:$provider")
[ "$gpu" != true ] || routes+=(own)
[ -z "$roles" ] || routes+=(local)
[ "${#routes[@]}" -gt 0 ] || die "PIPELINE_ENABLED=true but no answer route: set GPU_MODEL_ENABLED=true (our own model) or run pilot/enable-pilot.sh (deploy/README.md, \"Answering with our own model only\")"

# The files enable-pilot.sh makes (each mode 0400, owned by the image's user). A path
# Docker created as a folder (a mount of a missing file) is refused too.
need_file() { # need_file FILE WHAT: secrets/FILE is a regular, non-empty file
  if [ -L "secrets/$1" ] || [ ! -f "secrets/$1" ] || [ ! -s "secrets/$1" ]; then
    die "secrets/$1 ($2) is missing or empty: run pilot/enable-pilot.sh (deploy/README.md, \"Answering with our own model only\")"
  fi
}
need_file nats-pipeline.nk "the pipeline's NATS NKey"
need_file intelligence-db-password "the pipeline's database password"
key=secrets/pilot-cloud-api-key
if [ -L "$key" ] || [ ! -f "$key" ]; then
  die "$key is missing or not a file (with no cloud provider, an empty placeholder): run pilot/enable-pilot.sh"
fi
if [ -n "$provider" ] && [ ! -s "$key" ]; then
  die "PILOT_CLOUD_PROVIDER is set but $key is empty: run pilot/enable-pilot.sh --provider $provider --replace-key"
fi
[[ $(env_get NATS_PIPELINE_NKEY) =~ ^U[A-Z2-7]{55}$ ]] || die "NATS_PIPELINE_NKEY in .env is not a NATS user public key: run pilot/enable-pilot.sh"
[ -n "$(env_get INTELLIGENCE_DB_PASSWORD)" ] || die "INTELLIGENCE_DB_PASSWORD is not in .env: run pilot/enable-pilot.sh"
if [ -n "$provider" ]; then
  for f in egress-proxy/squid.conf egress-proxy/allowed-hosts; do
    [ -f "$f" ] || die "$f is missing (the cloud route's egress proxy): run pilot/enable-pilot.sh"
  done
fi

profiles=(--profile pilot)
[ -z "$provider" ] || profiles+=(--profile cloud)
[ "$gpu" != true ] || profiles+=(--profile gpu-model)
[ -z "$roles" ] || profiles+=(--profile ownmodel)
dc() { docker compose "${profiles[@]}" "$@"; }

# The demo's sample answers are the same NATS user and consumer: never both.
if [ -n "$(docker compose --profile demo ps -q sample-ai 2>/dev/null)" ]; then
  docker compose --profile demo stop sample-ai >/dev/null
  docker compose --profile demo rm -f sample-ai >/dev/null
  info "the demo's sample-ai stopped (the pipeline takes its jobs)"
fi

if [ -z "$(dc ps -q pipeline 2>/dev/null)" ]; then
  avail=$(awk '/^MemAvailable:/ {print int($2/1024)}' "$MEMINFO")
  [ "${avail:-0}" -ge "$MIN_AVAILABLE_MIB" ] || die "not enough memory for the pipeline (${avail:-0} < ${MIN_AVAILABLE_MIB} MiB available)"
fi

if ! dc pull -q pipeline; then
  image=$(env_get PIPELINE_IMAGE)
  image=${image:-ghcr.io/measured-health/mhai-multiagent-pipeline-pilot:latest}
  docker image inspect "$image" >/dev/null 2>&1 \
    || die "could not pull the pipeline image ($image): run the \"Pilot pipeline image\" workflow (deploy/pilot/README.md)"
  info "could not pull the pipeline image; using the copy on this host"
fi

if [ -n "$provider" ] && ! dc up -d --no-deps egress-proxy; then
  dc logs --tail=40 egress-proxy || true
  die "the egress proxy did not start"
fi
# --no-deps: core, its migrations (which make the intelligence_app login) and the
# request key have already run in this deploy.
if ! dc up -d --no-deps pipeline; then
  dc logs --tail=60 pipeline || true
  die "the pipeline did not start"
fi

waited=0
state=unknown
while :; do
  if dc logs --no-log-prefix pipeline 2>/dev/null | grep 'worker started service=pipeline' >/dev/null; then break; fi
  id=$(dc ps -q pipeline 2>/dev/null || true)
  state=$(docker inspect -f '{{.State.Status}} {{.RestartCount}}' "$id" 2>/dev/null || echo "missing 0")
  case "$state" in
    "running 0"|"created 0") ;;
    *) dc logs --tail=60 pipeline || true; die "the pipeline is not running ($state); its settings errors name variables only" ;;
  esac
  if [ "$waited" -ge "$START_TIMEOUT" ]; then
    dc logs --tail=60 pipeline || true
    die "the pipeline did not start within ${START_TIMEOUT}s ($state)"
  fi
  sleep "$INTERVAL"
  waited=$((waited + INTERVAL))
done
info "running (routes: ${routes[*]}; every other route escalates)"

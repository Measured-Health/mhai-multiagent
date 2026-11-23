#!/usr/bin/env bash
# Our own model on the GPU (deploy/README.md, "Our own model on the GPU"): the deploy
# workflow runs this on every deploy, in /srv/mhai-backend, after core is healthy.
#
#   - GPU_MODEL_ENABLED=true in .env: checks the settings, hashes GPU_MODEL_FILE against
#     GPU_MODEL_SHA256, and only then starts the gpu-model service (compose profile
#     gpu-model) and waits until it is healthy and serves exactly
#     <GPU_MODEL_NAME>@<GPU_MODEL_VERSION>#<first 12 hex of the hash>. A file that is
#     missing, a link, outside GPU_MODEL_ROOT (/srv/mhai-models) or does not match the
#     hash stops the gpu-model service and fails the deploy: a changed file is never
#     served.
#   - Anything else: the gpu-model service is stopped and removed if it was running.
#   - Either way, a running pipeline worker (profile pilot) is brought up to date with
#     the settings (compose recreates it only when they changed), so it asks the own
#     model exactly when it runs. Which hospitals use it is each hospital's model
#     policy (own_model, off for every hospital by default), never this script.
#
# It prints no secret (the .env keys it reads hold none) and touches only the
# mhai-backend compose project.
set -euo pipefail

DEST=${MHAI_BACKEND_DIR:-$PWD}
ROOT=${GPU_MODEL_ROOT:-/srv/mhai-models}
START_TIMEOUT=${GPU_MODEL_START_TIMEOUT:-600}
INTERVAL=${GPU_MODEL_POLL_INTERVAL:-5}

die() { printf '::error::gpu-model: %s\n' "$*" >&2; exit 1; }
info() { printf 'gpu-model: %s\n' "$*"; }

cd "$DEST"
[ -f .env ] || die "no .env in $DEST"
env_get() { sed -n "s/^$1=\(.*\)\$/\1/p" .env | tail -n1; }

dc() { docker compose --profile gpu-model "$@"; }

stop_gpu_model() {
  if [ -n "$(dc ps -a -q gpu-model 2>/dev/null)" ]; then
    dc stop gpu-model >/dev/null 2>&1 || true
    dc rm -f gpu-model >/dev/null 2>&1 || true
    info "stopped"
  fi
}

# The pipeline worker reads the GPU_MODEL_* settings at start (deploy/compose.yml):
# recreate it, if it runs, when they changed.
refresh_pipeline() {
  if [ -n "$(docker compose --profile pilot ps -q pipeline 2>/dev/null)" ]; then
    docker compose --profile pilot --profile gpu-model up -d --no-deps pipeline
    info "pipeline up to date"
  fi
}

# A failure stops the service and leaves a running pipeline as it is: its own
# questions then escalate (the service is gone), and settings that failed here never
# reach it (a worker refuses to start on malformed ones, which would stop every route).
fail() { stop_gpu_model; die "$*"; }

if [ "$(env_get GPU_MODEL_ENABLED)" != true ]; then
  stop_gpu_model
  refresh_pipeline
  info "off (GPU_MODEL_ENABLED is not true)"
  exit 0
fi

file=$(env_get GPU_MODEL_FILE)
name=$(env_get GPU_MODEL_NAME)
version=$(env_get GPU_MODEL_VERSION)
sha=$(env_get GPU_MODEL_SHA256)
profile=$(env_get GPU_MODEL_PROFILE)
num='(0|[1-9][0-9]{0,8})'
[[ $name =~ ^[a-z0-9][a-z0-9-]{1,62}$ ]] || fail "GPU_MODEL_NAME must be 2 to 63 of a-z 0-9 - (a letter or digit first)"
[[ $version =~ ^$num\.$num\.$num$ ]] || fail "GPU_MODEL_VERSION must be MAJOR.MINOR.PATCH"
[[ $sha =~ ^[0-9a-f]{64}$ ]] || fail "GPU_MODEL_SHA256 must be the GGUF's SHA-256 (64 lower-case hex)"
# The answer profiles the worker knows (agents/answer_profile.py PROFILE_IDS; a test
# keeps the two lists the same): any other value would stop the pipeline worker.
case "$profile" in
  default|gemma4-e4b-cca-v1) ;;
  *) fail "GPU_MODEL_PROFILE must name an answer profile (deploy/README.md)" ;;
esac
case "$file" in
  "$ROOT"/*.gguf) ;;
  *) fail "GPU_MODEL_FILE must be a .gguf file under $ROOT" ;;
esac
case "$file/" in */../*|*/./*|*//*) fail "GPU_MODEL_FILE must be a plain path" ;; esac
[[ $file =~ ^[A-Za-z0-9/._-]+$ ]] || fail "GPU_MODEL_FILE must be a plain path (A-Z a-z 0-9 / . _ -)"
[ ! -L "$file" ] || fail "GPU_MODEL_FILE is a link"
[ -f "$file" ] || fail "GPU_MODEL_FILE is not a file on this host"
[ -r "$file" ] || fail "GPU_MODEL_FILE is not readable"

info "hashing the GGUF"
got=$(sha256sum -- "$file" | cut -d' ' -f1)
[ "$got" = "$sha" ] || fail "GPU_MODEL_FILE's SHA-256 is not GPU_MODEL_SHA256: not served (deploy/README.md, \"Our own model on the GPU\")"
alias="$name@$version#${sha:0:12}"

dc pull -q gpu-model
if ! dc up -d gpu-model; then
  dc logs --tail=60 gpu-model || true
  fail "the gpu-model service did not start"
fi

id=$(dc ps -q gpu-model)
[ -n "$id" ] || fail "the gpu-model service is not running"
waited=0
while :; do
  health=$(docker inspect -f '{{.State.Health.Status}}' "$id" 2>/dev/null || echo unknown)
  [ "$health" = healthy ] && break
  if [ "$health" = unhealthy ] || [ "$waited" -ge "$START_TIMEOUT" ]; then
    dc logs --tail=60 gpu-model || true
    fail "the gpu-model service is not healthy ($health after ${waited}s)"
  fi
  sleep "$INTERVAL"
  waited=$((waited + INTERVAL))
done

models=$(dc exec -T gpu-model curl -fsS --max-time 10 http://127.0.0.1:8080/v1/models) \
  || fail "could not read the served models"
served=$(printf '%s' "$models" | { grep -o '"id":"[^"]*"' || true; } | sed 's/^"id":"//; s/"$//' | sort -u)
[ "$served" = "$alias" ] || fail "the gpu-model service does not serve exactly $alias"
info "serving $alias"

refresh_pipeline

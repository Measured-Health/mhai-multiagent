#!/usr/bin/env bash
# The on-host evaluation before a hospital is switched to the own answer model
# (deploy/pilot/README.md, "Switch a hospital to your own model"; own answer
# model plan, Task 12). Run by the operator, as the deploy user, on the demo
# host, after enable-ownmodel.sh, from a copy of this repo's deploy/ folder at
# the release the host runs:
#
#   bash deploy/pilot/eval-local.sh [--max-seconds 90] [--preflight-only]
#
# 1. Pre-flight, before anything starts: the host's memory against the model's
#    cap (PILOT_LOCAL_MODEL_MEM), the pinned lock (models/models.lock pins cca),
#    and the images (the llama sidecar's llama.cpp commit is the one the answer
#    gate scored with; a running pipeline is PIPELINE_IMAGE).
# 2. Brings up the ownmodel profile (the llama sidecar and the pipeline).
# 3. Scores the locked clinical_answer split (models/testsets.lock), or, until
#    one is built, the dev split (said so, loudly), against the sidecar through
#    the worker's own client (`mhai-models eval-served`): the sidecar must
#    serve exactly the alias pinned in models.lock first.
# 4. Prints PASS or FAIL with the totals, and keeps the summary in
#    $DEST/local-eval/. Exit 0 on PASS only: never switch a hospital otherwise.
#
# It takes no secret: the store reader's credentials are the files
# enable-ownmodel.sh wrote, used by the model-fetch container only. Nothing it
# prints holds a question, an answer or a credential.

set -euo pipefail
umask 077

DEST=${MHAI_BACKEND_DIR:-/srv/mhai-backend}
SRC=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=deploy/pilot/ownmodel-lib.sh
. "$SRC/pilot/ownmodel-lib.sh"
DEFAULT_IMAGE=ghcr.io/measured-health/mhai-multiagent-pipeline-pilot:latest
SPLIT_VOLUME=mhai-eval-split

usage() {
  cat >&2 <<'EOF'
usage: bash eval-local.sh [--max-seconds N] [--preflight-only]
It takes no key or other secret, on the command line or on stdin.
EOF
  exit 2
}

MAX_SECONDS=${PILOT_LOCAL_MAX_SECONDS:-90} PREFLIGHT_ONLY=0
while [ $# -gt 0 ]; do
  case "$1" in
    --max-seconds) [ $# -ge 2 ] || usage; MAX_SECONDS=$2; shift 2 ;;
    --preflight-only) PREFLIGHT_ONLY=1; shift ;;
    *) usage ;;
  esac
done
[[ $MAX_SECONDS =~ ^[1-9][0-9]{0,3}$ ]] || die "--max-seconds must be 1 to 9999"

say "Prerequisites"
command -v docker >/dev/null || die "docker is not installed"
docker compose version >/dev/null 2>&1 || die "docker compose (v2) is not available"
[ -f "$DEST/.env" ] || die "$DEST is not set up (deploy/setup-host.sh, then enable-pilot.sh)"
ENV_FILE="$DEST/.env"
env_get() { sed -n "s/^$1=//p" "$ENV_FILE" | tail -n1; }
[ "$(env_get PILOT_MODEL_ROLES)" = cca ] \
  || die "PILOT_MODEL_ROLES is not cca in $ENV_FILE: run enable-ownmodel.sh first"
case ",$(env_get COMPOSE_PROFILES)," in
  *,ownmodel,*) ;;
  *) die "COMPOSE_PROFILES in $ENV_FILE has no ownmodel: run enable-ownmodel.sh first" ;;
esac
cd "$DEST"
compose() { docker compose --profile cloud --profile pilot --profile ownmodel "$@"; }
running() { [ -n "$(compose ps -q "$1" 2>/dev/null)" ] && echo 1 || echo 0; }

say "The pinned lock"
check_lock "$DEST/models"

say "Memory"
MODEL_MEM=$(env_get PILOT_LOCAL_MODEL_MEM); MODEL_MEM=${MODEL_MEM:-4g}
check_memory "$MODEL_MEM" "$(running pipeline)" "$(running llama)" "$PIPELINE_MIB"

say "The images"
IMAGE=$(env_get PIPELINE_IMAGE); IMAGE=${IMAGE:-$DEFAULT_IMAGE}
LLAMA_IMAGE=$(env_get LLAMA_IMAGE); LLAMA_IMAGE=${LLAMA_IMAGE:-mhai-llama:local}
image_id=$(docker image inspect -f '{{.Id}}' "$IMAGE" 2>/dev/null) \
  || die "$IMAGE is not on this host: run enable-pilot.sh (it pulls it)"
docker image inspect -f '{{.Id}}' "$LLAMA_IMAGE" >/dev/null 2>&1 \
  || die "$LLAMA_IMAGE is not built: run enable-ownmodel.sh (it builds it from deploy/llama)"
pid=$(compose ps -q pipeline 2>/dev/null || true)
if [ -n "$pid" ]; then
  [ "$(docker inspect -f '{{.Image}}' "$pid")" = "$image_id" ] \
    || die "the running pipeline is not $IMAGE: run enable-ownmodel.sh again (it recreates it), then this"
fi
gate_commit=$(docker run --rm --network none --entrypoint python "$IMAGE" -c \
  'from intelligence.doceval.answers import llama_runtime as r; print(r.LLAMA_CPP_COMMIT)' 2>/dev/null) \
  || die "$IMAGE has no answer gate: deploy a pipeline image with own model PR 2 or later"
[[ $gate_commit =~ ^[0-9a-f]{40}$ ]] || die "unexpected llama.cpp commit from $IMAGE"
served=$(docker run --rm --network none --entrypoint /usr/local/bin/llama-server "$LLAMA_IMAGE" --version 2>&1 || true)
llama_commit_ok "$served" "$gate_commit" \
  || die "$LLAMA_IMAGE is not llama.cpp ${gate_commit:0:12}, the engine the gate scored with: rebuild it (enable-ownmodel.sh)"
info "$IMAGE; $LLAMA_IMAGE at llama.cpp ${gate_commit:0:12}, as the gate"

if [ "$PREFLIGHT_ONLY" = 1 ]; then
  say "Pre-flight passed (--preflight-only: nothing was started)"
  exit 0
fi

say "The ownmodel profile"
compose up -d llama pipeline
wait_cca_served ""

SET_ARGS=()
cleanup() { docker volume rm -f "$SPLIT_VOLUME" >/dev/null 2>&1 || true; }
if has_locked_split "$DEST/models"; then
  SPLIT='test'
  say "The locked clinical_answer split (models/testsets.lock)"
  cleanup
  trap cleanup EXIT
  docker volume create "$SPLIT_VOLUME" >/dev/null
  # As the store's reader, in model-fetch (the only container with a route to
  # the store); its signature is checked there, its hash again in eval-served.
  compose run --rm --no-deps -T -v "$SPLIT_VOLUME:/models" model-fetch \
    testset-fetch --testsets /lock/testsets.lock --trust-policy /lock/trust-policy.json --to /models </dev/null \
    || die "could not fetch the locked split (above: a fixed code; check the reader's credentials and the store)"
  SET_ARGS=(-v "$SPLIT_VOLUME:/split:ro")
  EXTRA=(--set /split/set)
else
  SPLIT='dev'
  say "THE DEV SPLIT: no locked clinical_answer split is built yet"
  info "models/testsets.lock names none, so this scores the dev split (public, in the"
  info "repository). A dev PASS shows the host serves the model well enough; it is NOT"
  info "the locked gate. Record it as 'dev split' wherever you report it."
  EXTRA=()
fi

say "Scoring against the sidecar through the worker's client (one question at a time)"
set +e
out=$(compose run --rm --no-deps -T "${SET_ARGS[@]}" -e INTELLIGENCE_LOCAL_LLM_START_TIMEOUT=900 \
  --entrypoint mhai-models pipeline eval-served --lock /lock/models.lock \
  --testsets /lock/testsets.lock --split "$SPLIT" "${EXTRA[@]}" --max-seconds "$MAX_SECONDS" </dev/null)
rc=$?
set -e
summary=$(printf '%s\n' "$out" | grep -E '^\{"(eval_served|refused|error)"' | tail -n1)
[ -n "$summary" ] || die "eval-served printed no result (exit $rc)"

mkdir -p "$DEST/local-eval"
chmod 700 "$DEST/local-eval"
record="$DEST/local-eval/local-eval-$(date -u +%Y%m%dT%H%M%SZ).json"
printf '{"host":"%s","split":"%s","exit":%d,"result":%s}\n' \
  "$(hostname -s | tr -cd 'A-Za-z0-9._-')" "$SPLIT" "$rc" "$summary" > "$record"
if command -v python3 >/dev/null; then
  printf '%s\n' "$summary" | python3 -m json.tool | sed 's/^/    /'
else
  info "$summary"
fi
info "kept in $record"

case "$rc" in
  0) say "PASS ($SPLIT split): you may switch one hospital (README, 'Switch one hospital')"; exit 0 ;;
  1) say "FAIL ($SPLIT split): do not switch. The failed checks are under \"failed\" above"; exit 1 ;;
  *) say "NOT EVALUATED: refused or failed before scoring (the code above). Do not switch"; exit 2 ;;
esac

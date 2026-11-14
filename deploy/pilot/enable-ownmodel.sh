#!/usr/bin/env bash
# Turn on the own answer model's services on the demo host (deploy/pilot/README.md,
# "Switch a hospital to your own model", steps 6 and 7). It switches no hospital:
# every hospital stays on its route until `core hospitals model-policy set`.
# Run by the operator, as the deploy user, after enable-pilot.sh, from a checkout
# of this repository at the release the host runs (it copies models.lock and
# models/ from there):
#
#   bash deploy/pilot/enable-ownmodel.sh --store-endpoint https://<store host> \
#     --store-bucket <bucket> [--store-region us-east-1] \
#     [--mem 3g] [--threads 2] [--ctx 4096] [--replace-key]
#
# The model store reader's credentials are read from standard input, two lines:
# the access key, then the secret key (hidden prompts on a terminal, or piped from
# a password manager). They go into two 0400 files owned by the image's user,
# never an argument, never .env, never printed. Files already in place are kept
# unless --replace-key is given.
#
# Safe to run again: every step checks what is there first and keeps it. It
# touches only the mhai-backend compose project: never the EMR containers, the
# web demo or nginx.

set -euo pipefail
umask 077

DEST=${MHAI_BACKEND_DIR:-/srv/mhai-backend}
SRC=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
REPO=$(cd "$SRC/.." && pwd)
# shellcheck source=deploy/pilot/ownmodel-lib.sh
. "$SRC/pilot/ownmodel-lib.sh"
NATS_BOX_IMAGE=${NATS_BOX_IMAGE:-natsio/nats-box:0.19.0}
DEFAULT_IMAGE=ghcr.io/measured-health/mhai-multiagent-pipeline-pilot:latest

usage() {
  cat >&2 <<'EOF'
usage: bash enable-ownmodel.sh --store-endpoint https://HOST[:PORT] --store-bucket NAME
         [--store-region REGION] [--mem 3g] [--threads 2] [--ctx 4096] [--replace-key]
The store reader's access key and secret key are read from standard input, one per line.
EOF
  exit 2
}

ENDPOINT='' BUCKET='' REGION=us-east-1 MEM=4g THREADS=2 CTX=4096 REPLACE_KEY=0
while [ $# -gt 0 ]; do
  case "$1" in
    --store-endpoint) [ $# -ge 2 ] || usage; ENDPOINT=$2; shift 2 ;;
    --store-bucket) [ $# -ge 2 ] || usage; BUCKET=$2; shift 2 ;;
    --store-region) [ $# -ge 2 ] || usage; REGION=$2; shift 2 ;;
    --mem) [ $# -ge 2 ] || usage; MEM=$2; shift 2 ;;
    --threads) [ $# -ge 2 ] || usage; THREADS=$2; shift 2 ;;
    --ctx) [ $# -ge 2 ] || usage; CTX=$2; shift 2 ;;
    --replace-key) REPLACE_KEY=1; shift ;;
    *) usage ;;
  esac
done

# --- the arguments, checked before anything is touched ----------------------
[[ $ENDPOINT =~ ^https://[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?(:[0-9]{1,5})?$ ]] \
  || die "--store-endpoint must be https://<host>[:port] (no path, no credentials)"
[[ $BUCKET =~ ^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$ ]] || die "--store-bucket must be an S3 bucket name"
[[ $REGION =~ ^[a-z0-9-]{1,32}$ ]] || die "--store-region must be a region name such as us-east-1"
mem_mib "$MEM" >/dev/null || die "--mem must be a size such as 3g or 3500m"
[[ $THREADS =~ ^[1-9][0-9]?$ ]] || die "--threads must be 1 to 99"
[[ $CTX =~ ^[1-9][0-9]{2,5}$ ]] || die "--ctx must be 100 to 999999"

say "Prerequisites"
command -v docker >/dev/null || die "docker is not installed"
docker compose version >/dev/null 2>&1 || die "docker compose (v2) is not available"
[ "$(id -u)" -ne 0 ] || die "run as the deploy user (ubuntu), not root"
[ -f "$DEST/.env" ] || die "$DEST is not set up (deploy/setup-host.sh)"
ENV_FILE="$DEST/.env"
env_get() { sed -n "s/^$1=//p" "$ENV_FILE" | tail -n1; }
env_set() {
  if [ "$(env_get "$1")" = "$2" ] && grep -q "^$1=" "$ENV_FILE"; then
    info "$1 unchanged"
  else
    sed -i "/^$1=/d" "$ENV_FILE"
    printf '%s=%s\n' "$1" "$2" >> "$ENV_FILE"
    info "$1 written"
  fi
}
case ",$(env_get COMPOSE_PROFILES)," in
  *,pilot,*) ;;
  *) die "the pilot is not enabled here: run enable-pilot.sh first" ;;
esac
if [ ! -f "$REPO/models.lock" ] || [ ! -d "$REPO/models" ] || [ ! -f "$SRC/llama/Dockerfile" ]; then
  die "run this from a checkout of the repository (it copies models.lock, models/ and deploy/llama)"
fi
cd "$DEST"
compose() { docker compose --profile cloud --profile pilot --profile ownmodel "$@"; }
running() { [ -n "$(compose ps -q "$1" 2>/dev/null)" ] && echo 1 || echo 0; }

say "The pinned lock (from $REPO)"
mkdir -p "$DEST/models"
chmod 755 "$DEST/models"
LOCK_CHANGED=0
copy_in() { # copy_in SRC NAME: DEST/models/NAME, 0644, kept when unchanged
  if cmp -s "$1" "$DEST/models/$2"; then
    info "$2 unchanged"
  else
    install -m 644 "$1" "$DEST/models/.$2.new"
    mv -f "$DEST/models/.$2.new" "$DEST/models/$2"
    [ "$2" != models.lock ] || LOCK_CHANGED=1
    info "$2 copied"
  fi
}
copy_in "$REPO/models.lock" models.lock
copy_in "$REPO/models/trust-policy.json" trust-policy.json
copy_in "$REPO/models/testsets.lock" testsets.lock
check_lock "$DEST/models"

say "Memory"
check_memory "$MEM" "$(running pipeline)" "$(running llama)" 0

say "The store reader's credentials"
IMAGE=$(env_get PIPELINE_IMAGE); IMAGE=${IMAGE:-$DEFAULT_IMAGE}
docker image inspect "$IMAGE" >/dev/null 2>&1 || die "$IMAGE is not on this host: run enable-pilot.sh"
APP_UID=$(docker run --rm --network none --entrypoint id "$IMAGE" -u)
case "$APP_UID" in ''|0|*[!0-9]*) die "the image's user is not a non-root uid" ;; esac
mkdir -p "$DEST/secrets"
chmod 700 "$DEST/secrets"
put_secret() { # stdin -> secrets/FILE, owned by the image's user, 0400, in a container with no network
  docker run --rm -i --network none --user 0 --entrypoint sh -e F="$1" -e U="$APP_UID" \
    -v "$DEST/secrets:/s" "$NATS_BOX_IMAGE" -c '
      set -eu; umask 077
      t="/s/.$F.new"; trap "rm -f $t" EXIT
      cat > "$t"; [ -s "$t" ] || { echo "empty secret" >&2; exit 1; }
      chown "$U:$U" "$t"; chmod 400 "$t"; mv -f "$t" "/s/$F"'
}
owned_ok() { [ "$(stat -c '%u %a' "$DEST/secrets/$1" 2>/dev/null)" = "$APP_UID 400" ]; }
AK=model-store-reader-access-key SK=model-store-reader-secret-key
if [ -s "$DEST/secrets/$AK" ] && [ -s "$DEST/secrets/$SK" ] && [ "$REPLACE_KEY" = 0 ]; then
  info "credential files exist, kept (--replace-key to change them)"
else
  ACCESS='' SECRET=''
  if [ -t 0 ]; then
    read -r -s -p "    The reader's access key (hidden): " ACCESS || true; printf '\n'
    read -r -s -p "    The reader's secret key (hidden): " SECRET || true; printf '\n'
  else
    IFS= read -r ACCESS || true
    IFS= read -r SECRET || true
  fi
  for v in "$ACCESS" "$SECRET"; do
    [ -n "$v" ] || { ACCESS='' SECRET=''; die "two lines expected on standard input: the access key, then the secret key"; }
    case "$v" in *[[:space:]]*) ACCESS='' SECRET=''; die "a credential has whitespace in it" ;; esac
    case "$(printf '%s' "$v" | tr '[:upper:]' '[:lower:]')" in
      *changeme*|*change-me*|*placeholder*|*example*) ACCESS='' SECRET=''; die "a credential is a placeholder" ;;
    esac
  done
  printf '%s' "$ACCESS" | put_secret "$AK"
  printf '%s' "$SECRET" | put_secret "$SK"
  ACCESS='' SECRET=''
  info "credential files written (not shown)"
fi
unset ACCESS SECRET
if ! { owned_ok "$AK" && owned_ok "$SK"; }; then
  die "the reader's credential files have the wrong owner or mode (run again with --replace-key)"
fi

say "Settings in $DEST/.env (none of these is a secret)"
env_set PILOT_MODEL_ROLES cca
env_set PILOT_LOCAL_MODEL_MEM "$MEM"
env_set PILOT_LOCAL_MODEL_THREADS "$THREADS"
env_set PILOT_LOCAL_MODEL_CTX "$CTX"
env_set PILOT_MODEL_STORE_ENDPOINT "$ENDPOINT"
env_set PILOT_MODEL_STORE_BUCKET "$BUCKET"
env_set PILOT_MODEL_STORE_REGION "$REGION"
env_set COMPOSE_PROFILES cloud,pilot,ownmodel
docker compose config --quiet || die "compose.yml and .env don't validate"

say "The llama sidecar image (deploy/llama, llama.cpp at the gate's commit)"
mkdir -p "$DEST/llama"
chmod 755 "$DEST/llama"
LLAMA_CHANGED=0
for f in Dockerfile entrypoint.sh; do
  if ! cmp -s "$SRC/llama/$f" "$DEST/llama/$f"; then
    install -m 644 "$SRC/llama/$f" "$DEST/llama/$f"; LLAMA_CHANGED=1; info "llama/$f copied"
  fi
done
LLAMA_IMAGE=$(env_get LLAMA_IMAGE); LLAMA_IMAGE=${LLAMA_IMAGE:-mhai-llama:local}
if [ "$LLAMA_CHANGED" = 1 ] || ! docker image inspect "$LLAMA_IMAGE" >/dev/null 2>&1; then
  info "building $LLAMA_IMAGE (llama.cpp from source: several minutes on 2 cores)"
  compose build llama
else
  info "$LLAMA_IMAGE up to date"
fi

say "Fetching the pinned models (model-fetch, as the store's reader)"
compose run --rm --no-deps -T model-fetch </dev/null \
  || die "model-fetch failed (above: a fixed code): check the endpoint, the bucket and the reader's credentials"

say "Starting llama and the pipeline"
# The pipeline is recreated when its settings changed (PILOT_MODEL_ROLES): it
# verifies the pin offline from the mirror, writes active.json, and waits for
# the sidecar to serve the alias.
# The worker reads models.lock once at start-up: a new pin (or a rollback) needs
# a restart of a running pipeline.
PIPELINE_WAS_RUNNING=$(running pipeline)
compose up -d llama pipeline
SINCE=''
if [ "$LOCK_CHANGED" = 1 ] && [ "$PIPELINE_WAS_RUNNING" = 1 ]; then
  info "models.lock changed: restarting the pipeline to verify the new pin"
  SINCE=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  compose restart pipeline >/dev/null
fi
wait_cca_served "$SINCE"

say "Done"
info "The own model is served; no hospital was switched."
info "Next: bash deploy/pilot/eval-local.sh (README, 'Evaluate on this host')."

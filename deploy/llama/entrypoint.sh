#!/usr/bin/env bash
# The llama sidecar's entrypoint (own answer model plan, PR 3, Decision 6).
#
# The pipeline worker verifies the pinned cca model (signature, gate and every file's
# hash) into the cache and writes active.json there:
#   {"gguf":"<name>/<version>-<hash12>/<file>.gguf","sha256":"<64 hex>","version_string":"<name>@<version>#<hash12>"}
# This script waits for it, re-hashes the named GGUF, refuses to start on any mismatch,
# and only then runs llama-server on that file with --alias <version_string>. The worker
# checks before every job that /v1/models lists exactly that alias.
#
# Settings: LLAMA_MODELS_DIR (/models, the cache mounted read-only), LLAMA_THREADS (2),
# LLAMA_CTX (4096), LLAMA_PORT (8080), LLAMA_ACTIVE_TIMEOUT (seconds to wait for
# active.json, 900). It never names a path or a hash it was not given in its messages.
set -euo pipefail

die() { printf 'llama-entrypoint: %s\n' "$*" >&2; exit 1; }

MODELS=${LLAMA_MODELS_DIR:-/models}
THREADS=${LLAMA_THREADS:-2}
CTX=${LLAMA_CTX:-4096}
PORT=${LLAMA_PORT:-8080}
WAIT=${LLAMA_ACTIVE_TIMEOUT:-900}
SERVER=${LLAMA_SERVER:-/usr/local/bin/llama-server}

int() {
  if ! [[ $2 =~ ^[1-9][0-9]{0,6}$ ]] || (( $2 < $3 || $2 > $4 )); then
    die "$1 must be a whole number from $3 to $4"
  fi
}
int LLAMA_THREADS "$THREADS" 1 256
int LLAMA_CTX "$CTX" 256 131072
int LLAMA_PORT "$PORT" 1 65535
int LLAMA_ACTIVE_TIMEOUT "$WAIT" 1 86400

active="$MODELS/active.json"
waited=0
until [ -f "$active" ] && [ ! -L "$active" ]; do
  (( waited < WAIT )) || die "no active.json after ${WAIT}s: the worker has not verified a cca pin"
  sleep 2
  waited=$((waited + 2))
done

size=$(stat -c %s -- "$active")
(( size > 0 && size <= 4096 )) || die "active.json is empty or too large"
raw=$(<"$active")
seg='[a-z0-9][a-z0-9._-]{0,127}'
re="^\\{\"gguf\":\"(${seg}/${seg}/${seg}\\.gguf)\",\"sha256\":\"([0-9a-f]{64})\",\"version_string\":\"([a-z0-9][a-z0-9-]{1,62}@[0-9]{1,9}\\.[0-9]{1,9}\\.[0-9]{1,9}#[0-9a-f]{12})\"\\}$"
[[ $raw =~ $re ]] || die "active.json is not in the expected form"
rel=${BASH_REMATCH[1]}
want=${BASH_REMATCH[2]}
alias=${BASH_REMATCH[3]}
case "/$rel/" in */../*|*/./*) die "active.json names a path outside the cache" ;; esac

# Every component a real directory or file, never a link.
path=$MODELS
IFS=/ read -r -a parts <<<"$rel"
for p in "${parts[@]}"; do
  path="$path/$p"
  [ ! -L "$path" ] || die "the GGUF's path holds a link"
done
[ -f "$path" ] || die "the GGUF named in active.json is not a regular file"

got=$(sha256sum -- "$path" | cut -d' ' -f1)
[ "$got" = "$want" ] || die "the GGUF's SHA-256 is not the verified one: refusing to start"

printf 'llama-entrypoint: serving %s (sha256 verified)\n' "$alias" >&2
exec "$SERVER" -m "$path" --alias "$alias" --host 0.0.0.0 --port "$PORT" \
  -t "$THREADS" -c "$CTX" -np 1 --jinja --offline --no-webui

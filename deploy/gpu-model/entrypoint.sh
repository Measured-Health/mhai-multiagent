#!/usr/bin/env bash
# The gpu-model service's entrypoint (profile gpu-model; deploy/README.md, "Our own model
# on the GPU"): our own fine-tuned answer model, one GGUF from the host, on the GPU, in
# the official llama.cpp server-cuda image (pinned by digest in compose.yml).
#
# The deploy (gpu-model/gpu-model.sh) hashes the host's file before it starts this
# service; this script hashes it again, inside the container, before serving: it refuses
# to start on any mismatch, then runs llama-server on that file with
# --alias <name>@<version>#<first 12 hex of the SHA-256>, offline, without the web UI.
# The pipeline worker checks before every job that /v1/models lists exactly that alias.
#
# Settings (compose passes them from the host's .env): GPU_MODEL_NAME, GPU_MODEL_VERSION,
# GPU_MODEL_SHA256 (required), GPU_MODEL_CTX (8192), GPU_MODEL_GPU_LAYERS (999: every
# layer on the GPU), GPU_MODEL_THREADS (4), GPU_MODEL_PORT (8080). It names no path or
# hash it was not given in its messages.
set -euo pipefail

die() { printf 'gpu-model-entrypoint: %s\n' "$*" >&2; exit 1; }

MODEL=${GPU_MODEL_PATH:-/model/model.gguf}
SERVER=${GPU_MODEL_SERVER:-/app/llama-server}
NAME=${GPU_MODEL_NAME:-}
VERSION=${GPU_MODEL_VERSION:-}
SHA=${GPU_MODEL_SHA256:-}
CTX=${GPU_MODEL_CTX:-8192}
LAYERS=${GPU_MODEL_GPU_LAYERS:-999}
THREADS=${GPU_MODEL_THREADS:-4}
PORT=${GPU_MODEL_PORT:-8080}

int() {
  if ! [[ $2 =~ ^[0-9]{1,7}$ ]] || [[ $2 =~ ^0[0-9] ]] || (( $2 < $3 || $2 > $4 )); then
    die "$1 must be a whole number from $3 to $4"
  fi
}
[[ $NAME =~ ^[a-z0-9][a-z0-9-]{1,62}$ ]] || die "GPU_MODEL_NAME must be 2 to 63 of a-z 0-9 - (a letter or digit first)"
num='(0|[1-9][0-9]{0,8})'
[[ $VERSION =~ ^$num\.$num\.$num$ ]] || die "GPU_MODEL_VERSION must be MAJOR.MINOR.PATCH"
[[ $SHA =~ ^[0-9a-f]{64}$ ]] || die "GPU_MODEL_SHA256 must be 64 lower-case hex"
int GPU_MODEL_CTX "$CTX" 256 131072
int GPU_MODEL_GPU_LAYERS "$LAYERS" 0 999
int GPU_MODEL_THREADS "$THREADS" 1 256
int GPU_MODEL_PORT "$PORT" 1 65535

[ ! -L "$MODEL" ] || die "the GGUF is a link"
[ -f "$MODEL" ] || die "no GGUF at the mounted path (GPU_MODEL_FILE on the host)"
got=$(sha256sum -- "$MODEL" | cut -d' ' -f1)
[ "$got" = "$SHA" ] || die "the GGUF's SHA-256 is not GPU_MODEL_SHA256: refusing to start"

alias="$NAME@$VERSION#${SHA:0:12}"
printf 'gpu-model-entrypoint: serving %s (sha256 verified)\n' "$alias" >&2
exec "$SERVER" -m "$MODEL" --alias "$alias" --host 0.0.0.0 --port "$PORT" \
  -c "$CTX" -np 1 -ngl "$LAYERS" -t "$THREADS" --jinja --offline --no-webui

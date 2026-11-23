#!/usr/bin/env bash
# Tests for entrypoint.sh, with a fake llama-server that prints its arguments: no Docker,
# no GPU, no model. Every file is synthetic. Run: bash deploy/gpu-model/test-entrypoint.sh
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
fail=0

cat >"$tmp/fake-server" <<'FAKE'
#!/usr/bin/env bash
printf 'SERVED %s\n' "$*"
FAKE
chmod +x "$tmp/fake-server"

gguf="$tmp/model.gguf"
setup() {
  rm -f "$gguf" "$tmp/real.gguf"
  printf 'GGUF synthetic weights' >"$gguf"
  sha=$(sha256sum "$gguf" | cut -d' ' -f1)
}

run() {
  GPU_MODEL_PATH="$gguf" GPU_MODEL_SERVER="$tmp/fake-server" \
    GPU_MODEL_NAME="${NAME-gemma4-e4b-cca}" GPU_MODEL_VERSION="${VERSION-1.0.0}" \
    GPU_MODEL_SHA256="${SHA-$sha}" GPU_MODEL_CTX="${CTX:-8192}" GPU_MODEL_GPU_LAYERS="${LAYERS:-999}" \
    GPU_MODEL_THREADS="${THREADS:-4}" bash "$here/entrypoint.sh" 2>"$tmp/err"
}

expect_ok() {
  local out want
  want="SERVED -m $gguf --alias gemma4-e4b-cca@1.0.0#${sha:0:12} --host 0.0.0.0 --port 8080 -c ${CTX:-8192} -np 1 -ngl ${LAYERS:-999} -t ${THREADS:-4} --jinja --offline --no-webui"
  if ! out=$(run); then echo "FAIL $1: refused ($(cat "$tmp/err"))"; fail=1; return; fi
  if [ "$out" = "$want" ]; then echo "ok   $1"; else echo "FAIL $1: $out"; fail=1; fi
}

expect_refused() {
  local out
  if out=$(run); then echo "FAIL $1: started ($out)"; fail=1; return; fi
  if [ -n "$out" ]; then echo "FAIL $1: the server ran ($out)"; fail=1; return; fi
  if ! grep -q -- "$2" "$tmp/err"; then echo "FAIL $1: wrong reason ($(cat "$tmp/err"))"; fail=1; return; fi
  if grep -q -- "$sha" "$tmp/err"; then echo "FAIL $1: the message names the hash"; fail=1; return; fi
  echo "ok   $1"
}

setup; expect_ok "the configured GGUF starts with its alias, every layer on the GPU"
setup; CTX=4096 LAYERS=0 THREADS=2 expect_ok "the settings are passed through"

setup; printf 'X' >>"$gguf"
expect_refused "a changed GGUF is refused" "not GPU_MODEL_SHA256"
setup; printf 'gGUF synthetic weights' >"$gguf"
expect_refused "a flipped bit is refused" "not GPU_MODEL_SHA256"
setup; rm "$gguf"
expect_refused "no GGUF: never starts" "no GGUF"
setup; mv "$gguf" "$tmp/real.gguf"; ln -s "$tmp/real.gguf" "$gguf"
expect_refused "a linked GGUF is refused" "link"

setup; SHA="" expect_refused "no hash: never starts" "GPU_MODEL_SHA256"
setup; SHA="${sha^^}" expect_refused "an upper-case hash is refused" "GPU_MODEL_SHA256"
setup; SHA="${sha:0:63}" expect_refused "a short hash is refused" "GPU_MODEL_SHA256"
setup; NAME="" expect_refused "no name: never starts" "GPU_MODEL_NAME"
setup; NAME="Gemma --host 1.2.3.4" expect_refused "a name with arguments is refused" "GPU_MODEL_NAME"
setup; VERSION="1.0" expect_refused "a short version is refused" "GPU_MODEL_VERSION"
setup; VERSION="1.0.0 --port 1" expect_refused "a version with arguments is refused" "GPU_MODEL_VERSION"
setup; CTX="8192 --host 0.0.0.0" expect_refused "a malformed context is refused" "GPU_MODEL_CTX"
setup; LAYERS=1000 expect_refused "too many layers are refused" "GPU_MODEL_GPU_LAYERS"
setup; THREADS=0 expect_refused "no threads are refused" "GPU_MODEL_THREADS"
setup; THREADS=04 expect_refused "a leading zero is refused" "GPU_MODEL_THREADS"

exit "$fail"

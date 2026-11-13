#!/usr/bin/env bash
# Tests for entrypoint.sh, with a fake llama-server that prints its arguments: no Docker,
# no model. Every file is synthetic. Run: bash deploy/llama/test-entrypoint.sh
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

name=mhai-cca-synthetic
dir=1.0.0-0123456789ab
alias="$name@1.0.0#0123456789ab"
gguf="$tmp/models/$name/$dir/model-q4_k_m.gguf"

setup() { # a fresh cache with one GGUF and an active.json naming it
  rm -rf "$tmp/models"
  mkdir -p "$tmp/models/$name/$dir"
  printf 'GGUF synthetic weights' >"$gguf"
  sha=$(sha256sum "$gguf" | cut -d' ' -f1)
  printf '{"gguf":"%s/%s/model-q4_k_m.gguf","sha256":"%s","version_string":"%s"}' \
    "$name" "$dir" "$sha" "$alias" >"$tmp/models/active.json"
}

run() {
  LLAMA_MODELS_DIR="$tmp/models" LLAMA_SERVER="$tmp/fake-server" LLAMA_ACTIVE_TIMEOUT=2 \
    LLAMA_THREADS="${THREADS:-2}" bash "$here/entrypoint.sh" 2>"$tmp/err"
}

expect_ok() {
  local out
  if ! out=$(run); then echo "FAIL $1: refused ($(cat "$tmp/err"))"; fail=1; return; fi
  case "$out" in
    "SERVED -m $gguf --alias $alias --host 0.0.0.0 --port 8080 -t 2 -c 4096 -np 1 --jinja --offline --no-webui")
      echo "ok   $1" ;;
    *) echo "FAIL $1: $out"; fail=1 ;;
  esac
}

expect_refused() {
  local out
  if out=$(run); then echo "FAIL $1: started ($out)"; fail=1; return; fi
  if [ -n "$out" ]; then echo "FAIL $1: the server ran ($out)"; fail=1; return; fi
  if [ -n "${2:-}" ] && ! grep -q -- "$2" "$tmp/err"; then
    echo "FAIL $1: wrong reason ($(cat "$tmp/err"))"; fail=1; return
  fi
  echo "ok   $1"
}

setup; expect_ok "the verified GGUF starts with its alias"

setup; printf 'X' >>"$gguf"
expect_refused "a changed GGUF is refused" "not the verified one"

setup; printf 'gGUF synthetic weights' >"$gguf" # one flipped bit in the first byte
expect_refused "a flipped bit is refused" "not the verified one"

setup; rm "$tmp/models/active.json"
expect_refused "no active.json: never starts" "no active.json"

setup; sed -i 's/"version_string"/"alias"/' "$tmp/models/active.json"
expect_refused "an unexpected key is refused" "expected form"

setup; sed -i "s|$name/$dir/model-q4_k_m.gguf|$name/../$dir/model-q4_k_m.gguf|" "$tmp/models/active.json"
expect_refused "a path out of the cache is refused" ""

setup; sed -i 's|#0123456789ab"|#0123456789ab --host 1.2.3.4"|' "$tmp/models/active.json"
expect_refused "an alias with extra arguments is refused" "expected form"

setup; mv "$gguf" "$tmp/real.gguf"; ln -s "$tmp/real.gguf" "$gguf"
expect_refused "a linked GGUF is refused" "link"

setup; mv "$tmp/models/active.json" "$tmp/active.json"; ln -s "$tmp/active.json" "$tmp/models/active.json"
expect_refused "a linked active.json is never read" "no active.json"

setup; THREADS="2 --host 0.0.0.0" expect_refused "a malformed setting is refused" "LLAMA_THREADS"

exit "$fail"

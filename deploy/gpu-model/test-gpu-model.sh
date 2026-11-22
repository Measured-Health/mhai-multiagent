#!/usr/bin/env bash
# Tests for gpu-model.sh with a fake docker on PATH: no Docker, no GPU, no model. Every
# file is synthetic. Run: bash deploy/gpu-model/test-gpu-model.sh
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
fail=0
mkdir -p "$tmp/bin" "$tmp/models/cca-v1" "$tmp/dest"

# The fake docker logs every call and answers as FAKE_* say: the gpu-model and pipeline
# containers (running or not), the health, and the served alias.
cat >"$tmp/bin/docker" <<'FAKE'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$FAKE_LOG"
case "$*" in
  *"ps -a -q gpu-model"*|*"ps -q gpu-model"*)
    if [ "${FAKE_GPU_RUNNING:-0}" = 1 ] || [ -e "$FAKE_LOG.up" ]; then echo gpu1; fi; exit 0 ;;
  *"ps -q pipeline"*) if [ "${FAKE_PIPELINE:-0}" = 1 ]; then echo pipe1; fi; exit 0 ;;
  *"up -d gpu-model"*) if [ "${FAKE_UP_FAILS:-0}" = 1 ]; then exit 1; fi; touch "$FAKE_LOG.up"; exit 0 ;;
  inspect*) echo "${FAKE_HEALTH:-healthy}"; exit 0 ;;
  *"exec -T gpu-model curl"*) printf '{"object":"list","data":[%s]}' "${FAKE_SERVED-}"; exit 0 ;;
esac
exit 0
FAKE
chmod +x "$tmp/bin/docker"

gguf="$tmp/models/cca-v1/model-q4_k_m.gguf"
printf 'GGUF synthetic weights' >"$gguf"
sha=$(sha256sum "$gguf" | cut -d' ' -f1)
alias="gemma4-e4b-cca@1.0.0#${sha:0:12}"

write_env() { # the host's .env, with any line replaced by an argument KEY=VALUE
  {
    echo "POSTGRES_PASSWORD=synthetic"
    echo "GPU_MODEL_ENABLED=true"
    echo "GPU_MODEL_FILE=$gguf"
    echo "GPU_MODEL_NAME=gemma4-e4b-cca"
    echo "GPU_MODEL_VERSION=1.0.0"
    echo "GPU_MODEL_SHA256=$sha"
    echo "GPU_MODEL_PROFILE=gemma4-e4b-cca-v1"
    for kv in "$@"; do echo "$kv"; done
  } >"$tmp/dest/.env"
}

run() {
  : >"$tmp/log"; rm -f "$tmp/log.up"
  PATH="$tmp/bin:$PATH" FAKE_LOG="$tmp/log" MHAI_BACKEND_DIR="$tmp/dest" GPU_MODEL_ROOT="$tmp/models" \
    GPU_MODEL_START_TIMEOUT=2 GPU_MODEL_POLL_INTERVAL=1 FAKE_SERVED="${SERVED-\"id\":\"$alias\"}" \
    bash "$here/gpu-model.sh" >"$tmp/out" 2>&1
}

expect_started() {
  if ! run; then echo "FAIL $1: refused ($(cat "$tmp/out"))"; fail=1; return; fi
  if ! grep -q -- "--profile gpu-model up -d gpu-model" "$tmp/log"; then echo "FAIL $1: not started"; fail=1; return; fi
  if ! grep -q "serving $alias" "$tmp/out"; then echo "FAIL $1: $(cat "$tmp/out")"; fail=1; return; fi
  echo "ok   $1"
}

expect_refused() {
  if run; then echo "FAIL $1: passed ($(cat "$tmp/out"))"; fail=1; return; fi
  if ! grep -q -- "$2" "$tmp/out"; then echo "FAIL $1: wrong reason ($(cat "$tmp/out"))"; fail=1; return; fi
  if [ "${3:-}" != started ] && grep -q -- "up -d gpu-model" "$tmp/log"; then echo "FAIL $1: the service was started"; fail=1; return; fi
  if grep -q -- "$sha" "$tmp/out"; then echo "FAIL $1: the output names the hash"; fail=1; return; fi
  echo "ok   $1"
}

write_env; expect_started "the configured GGUF is hashed, started and checked"
write_env; FAKE_PIPELINE=1 expect_started "a running pipeline is brought up to date"
if ! grep -q -- "--profile pilot --profile gpu-model up -d --no-deps pipeline" "$tmp/log"; then echo "FAIL the pipeline was not refreshed"; fail=1; fi

# Off: stopped if running, never started.
write_env "GPU_MODEL_ENABLED=false"
if FAKE_GPU_RUNNING=1 run && grep -q -- "stop gpu-model" "$tmp/log" && ! grep -q -- "up -d gpu-model" "$tmp/log"; then
  echo "ok   off stops a running gpu-model"
else echo "FAIL off: $(cat "$tmp/out")"; fail=1; fi
sed -i '/GPU_MODEL_ENABLED/d' "$tmp/dest/.env"
if run && ! grep -q -- "up -d gpu-model" "$tmp/log"; then echo "ok   unset is off"; else echo "FAIL unset"; fail=1; fi
write_env "GPU_MODEL_ENABLED=yes"
if run && ! grep -q -- "up -d gpu-model" "$tmp/log"; then echo "ok   anything but true is off"; else echo "FAIL yes"; fail=1; fi

# The file and the hash: fail closed, and a running service is stopped.
printf 'X' >>"$gguf"
write_env; FAKE_GPU_RUNNING=1 expect_refused "a changed GGUF is refused" "is not GPU_MODEL_SHA256"
grep -q -- "stop gpu-model" "$tmp/log" || { echo "FAIL a changed GGUF left the service running"; fail=1; }
printf 'GGUF synthetic weights' >"$gguf"
write_env "GPU_MODEL_SHA256=${sha:0:63}0"; expect_refused "another hash is refused" "is not GPU_MODEL_SHA256"
write_env "GPU_MODEL_SHA256=${sha^^}"; expect_refused "an upper-case hash is refused" "GPU_MODEL_SHA256 must"
write_env "GPU_MODEL_SHA256="; expect_refused "no hash is refused" "GPU_MODEL_SHA256 must"
write_env "GPU_MODEL_FILE=$tmp/models/cca-v1/missing.gguf"; expect_refused "a missing file is refused" "not a file"
write_env "GPU_MODEL_FILE=$tmp/elsewhere.gguf"; expect_refused "a file outside the root is refused" "under"
write_env "GPU_MODEL_FILE=$tmp/models/../models/cca-v1/model-q4_k_m.gguf"; expect_refused "a path with .. is refused" "plain path"
write_env "GPU_MODEL_FILE=$tmp/models/cca-v1/model.bin"; expect_refused "not a .gguf is refused" "under"
ln -s "$gguf" "$tmp/models/link.gguf"
write_env "GPU_MODEL_FILE=$tmp/models/link.gguf"; expect_refused "a linked file is refused" "link"
write_env "GPU_MODEL_NAME=Gemma"; expect_refused "a bad name is refused" "GPU_MODEL_NAME"
write_env "GPU_MODEL_VERSION=1"; expect_refused "a bad version is refused" "GPU_MODEL_VERSION"
write_env "GPU_MODEL_PROFILE=gemma"; expect_refused "an unknown profile is refused" "GPU_MODEL_PROFILE"

# The service: unhealthy, not started, or serving anything but exactly the alias.
write_env; FAKE_HEALTH=unhealthy expect_refused "an unhealthy service fails the deploy" "not healthy" started
write_env; FAKE_HEALTH=starting expect_refused "a service never healthy fails the deploy" "not healthy" started
write_env; FAKE_UP_FAILS=1 expect_refused "a service that does not start fails the deploy" "did not start" started
write_env; SERVED="\"id\":\"gemma4-e4b-cca@1.0.0#ffffffffffff\"" expect_refused "another alias fails the deploy" "does not serve exactly" started
write_env; SERVED="\"id\":\"$alias\"},{\"id\":\"other@1.0.0#aaaaaaaaaaaa\"" expect_refused "a second model fails the deploy" "does not serve exactly" started
write_env; SERVED="" expect_refused "no model fails the deploy" "does not serve exactly" started

exit "$fail"

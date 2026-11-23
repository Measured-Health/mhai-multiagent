#!/usr/bin/env bash
# Tests for swap-model.sh with a fake docker and a fake nvidia-smi on PATH: no Docker, no
# GPU, no model. The real gpu-model.sh does the switch against the fake. Every file is
# synthetic. Run: bash deploy/gpu-model/test-swap-model.sh
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
tmp=$(mktemp -d)
trap 'chmod -R u+w "$tmp" 2>/dev/null; rm -rf "$tmp"' EXIT
fail=0
mkdir -p "$tmp/bin" "$tmp/gm/builder" "$tmp/gm/lock" "$tmp/dest" "$tmp/adapter"
cp "$here/swap-model.sh" "$here/gpu-model.sh" "$here/entrypoint.sh" "$tmp/gm/"
cp "$here/builder/Dockerfile" "$here/builder/merge.py" "$tmp/gm/builder/"
cp "$repo/models/licences.json" "$repo/models/approved-datasets.json" "$tmp/gm/lock/"
grep '^#' "$repo/models/testsets.lock" >"$tmp/gm/lock/testsets.lock"

# The fake docker logs every call and answers as FAKE_* say. The gpu-model service serves
# the alias of whatever .env holds (unless FAKE_SERVE_BAD names that version).
cat >"$tmp/bin/docker" <<'FAKE'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$FAKE_LOG"
envget() { sed -n "s/^$1=\(.*\)\$/\1/p" "$FAKE_DEST/.env" | tail -n1; }
mount_of() { # the host side of -v X:$1
  local prev='' a
  for a in "${ARGS[@]}"; do
    if [ "$prev" = -v ]; then case "$a" in *:"$1"|*:"$1":*) printf '%s' "${a%%:"$1"*}"; return ;; esac; fi
    prev=$a
  done
}
ARGS=("$@")
case "$*" in
  "compose version") exit 0 ;;
  *"config --images gpu-model"*) echo "ghcr.io/ggml-org/llama.cpp:server-cuda-b12627@sha256:$(printf 'a%.0s' {1..64})"; exit 0 ;;
  "image inspect -f {{.Id}} "*) echo "sha256:fakebuilder"; exit 0 ;;
  "image inspect "*) exit "${FAKE_NO_IMAGE:-0}" ;;
  *" swap --help") exit "${FAKE_NO_SWAP:-0}" ;;
  *" swap check-adapter "*)
    if [ -n "${FAKE_ADAPTER_REFUSED:-}" ]; then echo "{\"refused\":\"$FAKE_ADAPTER_REFUSED\"}"; exit 2; fi
    echo '{"adapter":{"architecture":"gemma4","base":"google/gemma-4-E4B-it","datasets":["medqa-usmle-4-options@1.0.0"],"licence":"Apache-2.0","revision":"4ecc2ba63bcac23ba4a767fc87025b1450f84c9c","trained_by":"synthetic-trainer"}}'
    exit 0 ;;
  *" swap check-gguf "*)
    if [ -n "${FAKE_GGUF_REFUSED:-}" ]; then echo "{\"refused\":\"$FAKE_GGUF_REFUSED\"}"; exit 2; fi
    echo '{"gguf":{"architecture":"gemma4","size":1}}'; exit 0 ;;
  *" swap eval-shadow "*)
    v=${FAKE_GATE:-pass}
    echo "loading..."
    echo "{\"eval_shadow\":{\"alias\":\"x\",\"metrics\":{\"escalation_recall\":\"1.000000\"},\"split\":\"dev\",\"verdict\":\"$v\"}}"
    [ "$v" = pass ] && exit 0; exit 1 ;;
  "build "*) exit 0 ;;
  *" fetch --repo "*) exit "${FAKE_FETCH_FAILS:-0}" ;;
  *" build --repo "*)
    [ "${FAKE_BUILD_FAILS:-0}" = 0 ] || exit 1
    out=$(mount_of /out)
    printf 'GGUF synthetic candidate %s' "${FAKE_GGUF_SEED:-1}" >"$out/model-q4_k_m.gguf"
    sha=$(sha256sum "$out/model-q4_k_m.gguf" | cut -d' ' -f1)
    printf '{\n  "gguf": {\n    "sha256": "%s"\n  }\n}\n' "${FAKE_MANIFEST_SHA:-$sha}" >"$out/manifest.json"
    exit 0 ;;
  *"run -d --name mhai-gpu-model-shadow"*) exit "${FAKE_SHADOW_FAILS:-0}" ;;
  "exec mhai-gpu-model-shadow curl"*) exit "${FAKE_SHADOW_UNHEALTHY:-0}" ;;
  "inspect -f {{.State.Running}}"*) echo true; exit 0 ;;
  *"ps -a -q gpu-model"*|*"ps -q gpu-model"*) echo gpu1; exit 0 ;;
  *"ps -q pipeline"*) exit 0 ;;
  inspect*) echo healthy; exit 0 ;;
  *"exec -T gpu-model curl"*)
    v=$(envget GPU_MODEL_VERSION)
    if [ "$v" = "${FAKE_SERVE_BAD:-none}" ]; then echo '{"data":[{"id":"other@0.0.1#000000000000"}]}'; exit 0; fi
    sha=$(envget GPU_MODEL_SHA256)
    echo "{\"data\":[{\"id\":\"$(envget GPU_MODEL_NAME)@$v#${sha:0:12}\"}]}"; exit 0 ;;
esac
exit 0
FAKE
cat >"$tmp/bin/nvidia-smi" <<'FAKE'
#!/usr/bin/env bash
echo "${FAKE_GPU_FREE:-20000}"
FAKE
chmod +x "$tmp/bin/docker" "$tmp/bin/nvidia-smi"

cur_gguf="$tmp/models/cca-v1/model-q4_k_m.gguf"
cand_gguf="$tmp/candidate.gguf"
reset() { # a fresh host: the current model served, no swap yet
  chmod -R u+w "$tmp/models" 2>/dev/null || true
  rm -rf "$tmp/models" "$tmp/adapter"
  mkdir -p "$tmp/models/cca-v1" "$tmp/adapter"
  printf 'GGUF synthetic current' >"$cur_gguf"
  printf 'GGUF synthetic from a file' >"$cand_gguf"
  echo '{}' >"$tmp/adapter/adapter_config.json"
  cur_sha=$(sha256sum "$cur_gguf" | cut -d' ' -f1)
  {
    echo "POSTGRES_PASSWORD=synthetic-secret-value"
    echo "PIPELINE_IMAGE=example.invalid/pipeline:test"
    echo "GPU_MODEL_ENABLED=true"
    echo "GPU_MODEL_FILE=$cur_gguf"
    echo "GPU_MODEL_NAME=gemma4-e4b-cca"
    echo "GPU_MODEL_VERSION=1.0.0"
    echo "GPU_MODEL_SHA256=$cur_sha"
    echo "GPU_MODEL_PROFILE=gemma4-e4b-cca-v1"
  } >"$tmp/dest/.env"
  chmod 600 "$tmp/dest/.env"
}

run() { # args to swap-model.sh; stdin is the typed confirmation
  : >"$tmp/log"
  PATH="$tmp/bin:$PATH" FAKE_LOG="$tmp/log" FAKE_DEST="$tmp/dest" MHAI_BACKEND_DIR="$tmp/dest" \
    GPU_MODEL_ROOT="$tmp/models" GPU_MODEL_START_TIMEOUT=2 GPU_MODEL_POLL_INTERVAL=1 \
    GPU_MODEL_BUILD_MIN_FREE_GB=0 bash "$tmp/gm/swap-model.sh" "$@" >"$tmp/out" 2>&1
}
env_get() { sed -n "s/^$1=\(.*\)\$/\1/p" "$tmp/dest/.env" | tail -n1; }
ok() { echo "ok   $1"; }
bad() { echo "FAIL $1: $(tail -n 5 "$tmp/out")"; fail=1; }
no_secret() { if grep -q synthetic-secret-value "$tmp/out" "$tmp/models/swaps.log" 2>/dev/null; then echo "FAIL $1: a secret was printed"; fail=1; fi; }
ADAPTER=(--adapter "$tmp/adapter" --name gemma4-e4b-cca --version 2.0.0 --profile gemma4-e4b-cca-v1)

expect_refused() { # name reason args...
  local name=$1 reason=$2; shift 2
  local before; before=$(cat "$tmp/dest/.env")
  if run "$@" </dev/null; then bad "$name (passed)"; return; fi
  if ! grep -q -- "$reason" "$tmp/out"; then bad "$name (wrong reason)"; return; fi
  if [ "$before" != "$(cat "$tmp/dest/.env")" ]; then bad "$name (.env changed)"; return; fi
  if grep -q -- "up -d gpu-model" "$tmp/log"; then bad "$name (the service was restarted)"; return; fi
  no_secret "$name"
  ok "$name"
}

# ---- refusals before anything is built
reset
expect_refused "a bad name" "--name must" --gguf "$cand_gguf" --name Bad --version 2.0.0 --profile default
expect_refused "a bad version" "--version must" --gguf "$cand_gguf" --name gemma4-e4b-cca --version 2.0 --profile default
expect_refused "an unknown profile" "--profile must" --gguf "$cand_gguf" --name gemma4-e4b-cca --version 2.0.0 --profile gemma
expect_refused "two sources" "usage" --gguf "$cand_gguf" --adapter "$tmp/adapter" --name x1 --version 1.0.0 --profile default
expect_refused "rollback with other options" "usage" --rollback --name x1
expect_refused "no source" "usage" --name x1 --version 1.0.0 --profile default
expect_refused "a version not above the served one" "must be above" --gguf "$cand_gguf" --name gemma4-e4b-cca --version 1.0.0 --profile default
mkdir -p "$tmp/models/gemma4-e4b-cca/3.0.0"
expect_refused "an existing version folder" "every swap is a new version" --gguf "$cand_gguf" --name gemma4-e4b-cca --version 3.0.0 --profile default
reset
echo '{"to":{"alias":"other-model@1.2.0#abc"}}' >"$tmp/models/swaps.log"
expect_refused "a version swapped to before" "used before" --gguf "$cand_gguf" --name other-model --version 1.2.0 --profile default
reset
FAKE_NO_SWAP=1 expect_refused "an image without the checks" "no 'mhai-models swap'" "${ADAPTER[@]}"
FAKE_ADAPTER_REFUSED=pickle_refused expect_refused "a refused adapter" "pickle_refused" "${ADAPTER[@]}"
if [ -e "$tmp/models/gemma4-e4b-cca/2.0.0" ]; then echo "FAIL a refused adapter left its version folder"; fail=1; fi
if grep -q -- " build --repo" "$tmp/log"; then echo "FAIL a refused adapter was built"; fail=1; fi
ln -s "$tmp/adapter" "$tmp/adapter-link"
expect_refused "a linked adapter folder" "must be a folder" --adapter "$tmp/adapter-link" --name gemma4-e4b-cca --version 2.0.0 --profile default
ln -s "$cand_gguf" "$tmp/link.gguf"
expect_refused "a linked GGUF" "regular file" --gguf "$tmp/link.gguf" --name gemma4-e4b-cca --version 2.0.0 --profile default
FAKE_GGUF_REFUSED=gguf_invalid expect_refused "a refused GGUF" "gguf_invalid" "${ADAPTER[@]}"
FAKE_BUILD_FAILS=1 expect_refused "a failed build" "the build failed" "${ADAPTER[@]}"
FAKE_MANIFEST_SHA=$(printf 'f%.0s' {1..64}) expect_refused "a GGUF that is not the one the build recorded" "not the one the build recorded" "${ADAPTER[@]}"
FAKE_GPU_FREE=1000 expect_refused "too little GPU memory for the shadow" "MiB free" "${ADAPTER[@]}"
grep -q '"action":"refused"' "$tmp/models/swaps.log" || { echo "FAIL the GPU refusal was not logged"; fail=1; }
reset
FAKE_SHADOW_UNHEALTHY=1 expect_refused "a shadow that never becomes healthy" "did not become healthy" "${ADAPTER[@]}"
reset
exec 8>>"$tmp/models/.swap.lock"; flock -n 8
expect_refused "a second swap at the same time" "another swap is running" "${ADAPTER[@]}"
exec 8>&-

# ---- a gate that fails: no switch, logged, the model kept for the record
reset
FAKE_GATE=fail expect_refused "a failing gate does not switch" "THE GATE DID NOT PASS" "${ADAPTER[@]}"
if grep -q '"action":"refused".*"split":"dev","gate":"fail","forced":false' "$tmp/models/swaps.log" \
  && [ -f "$tmp/models/gemma4-e4b-cca/2.0.0/model-q4_k_m.gguf" ]; then ok "the failed gate is logged and the model kept"
else echo "FAIL the failed gate's record"; fail=1; fi
if grep -q "DEV SPLIT, NOT A LOCKED TEST" "$tmp/out"; then ok "the dev split is said loudly"; else echo "FAIL no dev split banner"; fail=1; fi

# ---- a passing adapter swap
reset
if run "${ADAPTER[@]}" </dev/null; then
  new_sha=$(sha256sum "$tmp/models/gemma4-e4b-cca/2.0.0/model-q4_k_m.gguf" | cut -d' ' -f1)
  if [ "$(env_get GPU_MODEL_SHA256)" = "$new_sha" ] && [ "$(env_get GPU_MODEL_VERSION)" = 2.0.0 ] \
    && [ "$(env_get GPU_MODEL_FILE)" = "$tmp/models/gemma4-e4b-cca/2.0.0/model-q4_k_m.gguf" ] \
    && [ "$(env_get GPU_MODEL_PREVIOUS_SHA256)" = "$cur_sha" ] && [ "$(env_get GPU_MODEL_PREVIOUS_VERSION)" = 1.0.0 ] \
    && [ "$(env_get POSTGRES_PASSWORD)" = synthetic-secret-value ] && [ "$(stat -c %a "$tmp/dest/.env")" = 600 ]; then
    ok "a passing adapter is switched, the old settings kept as PREVIOUS"
  else bad "the .env after the swap"; fi
  grep -q "serving gemma4-e4b-cca@2.0.0#${new_sha:0:12}" "$tmp/out" || bad "the new alias was not waited for"
  { [ "$(stat -c %a "$tmp/models/gemma4-e4b-cca/2.0.0/model-q4_k_m.gguf")" = 444 ] \
    && [ "$(stat -c %a "$tmp/models/gemma4-e4b-cca/2.0.0")" = 555 ]; } || bad "the model is not read-only"
  grep -q '"action":"swap".*"to":{"alias":"gemma4-e4b-cca@2.0.0#'"${new_sha:0:12}"'","sha256":"'"$new_sha"'"},"split":"dev","gate":"pass","forced":false,"summary":{"eval_shadow"' "$tmp/models/swaps.log" \
    || bad "the swap's log line"
  build_line=$(grep -- " build --repo" "$tmp/log")
  case "$build_line" in *"--network none"*) ;; *) bad "the build had a network" ;; esac
  case "$build_line" in *secrets*|*.env*|*docker.sock*) bad "the build saw a secret" ;; esac
  shadow_line=$(grep -- "run -d --name mhai-gpu-model-shadow" "$tmp/log")
  case "$shadow_line" in *"--network mhai-gpu-model-shadow --network-alias gpu-model"*) ;; *) bad "the shadow's network" ;; esac
  grep -q -- "network create --internal mhai-gpu-model-shadow" "$tmp/log" || bad "the shadow's network is not internal"
  grep -q -- "swap eval-shadow .*--split dev" "$tmp/log" || bad "the gate's split"
  if grep -q "model-policy\|core-cli" "$tmp/log"; then bad "a hospital's own_model was touched"; fi
  [ ! -e "$tmp/models/.work/gemma4-e4b-cca-2.0.0" ] || bad "the work folder was left"
  no_secret "the swap"
  ok "the swap ran the checks, the shadow and the gate, and logged them"
else bad "a passing adapter"; fi

# ---- rollback, and rollback of the rollback
if run --rollback </dev/null && [ "$(env_get GPU_MODEL_SHA256)" = "$cur_sha" ] \
  && [ "$(env_get GPU_MODEL_PREVIOUS_VERSION)" = 2.0.0 ] && grep -q "serving gemma4-e4b-cca@1.0.0#${cur_sha:0:12}" "$tmp/out" \
  && grep -q '"action":"rollback"' "$tmp/models/swaps.log"; then ok "rollback serves the previous model"
else bad "rollback"; fi
if run --rollback </dev/null && [ "$(env_get GPU_MODEL_VERSION)" = 2.0.0 ]; then ok "a second rollback undoes the first"
else bad "rollback twice"; fi

# ---- a switch that does not come up is undone
reset
if FAKE_SERVE_BAD=2.0.0 run "${ADAPTER[@]}" </dev/null; then bad "a switch that did not come up"
elif [ "$(env_get GPU_MODEL_SHA256)" = "$cur_sha" ] && [ -z "$(env_get GPU_MODEL_PREVIOUS_SHA256)" ] \
  && grep -q "the previous model is served again" "$tmp/out" && grep -q '"action":"swap_failed"' "$tmp/models/swaps.log"; then
  ok "a switch that does not come up puts the old settings back"
else bad "the failed switch's recovery"; fi

reset
expect_refused "rollback with nothing to roll back to" "nothing to roll back to" --rollback

# ---- forcing
reset
if printf '9.9.9\n' | FAKE_GATE=fail run "${ADAPTER[@]}" --force-without-gate; then bad "a wrong confirmation"
elif grep -q "does not match" "$tmp/out" && ! grep -q -- " build --repo" "$tmp/log"; then ok "force needs the version typed again"
else bad "the force confirmation"; fi
reset
if printf '2.0.0\n' | FAKE_GATE=fail run "${ADAPTER[@]}" --force-without-gate \
  && [ "$(env_get GPU_MODEL_VERSION)" = 2.0.0 ] && grep -q "FORCED" "$tmp/out" \
  && grep -q '"action":"swap".*"gate":"fail","forced":true' "$tmp/models/swaps.log"; then ok "a forced swap switches and is logged as forced"
else bad "the forced swap"; fi

# ---- a GGUF, and --check-only
reset
if run --gguf "$cand_gguf" --name gemma4-e4b-cca --version 2.1.0 --profile default </dev/null \
  && [ "$(env_get GPU_MODEL_FILE)" = "$tmp/models/gemma4-e4b-cca/2.1.0/model.gguf" ] \
  && [ "$(env_get GPU_MODEL_PROFILE)" = default ] && grep -q '"source": "gguf"' "$tmp/models/gemma4-e4b-cca/2.1.0/manifest.json" \
  && ! grep -q -- "fetch --repo" "$tmp/log"; then ok "a GGUF is copied, checked, shadow-tested and switched"
else bad "the GGUF swap"; fi
reset
if run --gguf "$cand_gguf" --name gemma4-e4b-cca --version 2.1.0 --profile default --check-only </dev/null \
  && [ "$(env_get GPU_MODEL_VERSION)" = 1.0.0 ] && grep -q '"action":"checked"' "$tmp/models/swaps.log" \
  && ! grep -q -- "up -d gpu-model" "$tmp/log"; then ok "--check-only never switches"
else bad "--check-only"; fi
reset
expect_refused "the served GGUF again" "already served" --gguf "$cur_gguf" --name gemma4-e4b-cca --version 2.2.0 --profile default

# ---- a locked split is fetched and scored instead of the dev split
reset
cp "$tmp/gm/lock/testsets.lock" "$tmp/testsets.keep"
echo "clinical_answer@1.0.0#$(printf 'b%.0s' {1..64})" >>"$tmp/gm/lock/testsets.lock"
if run "${ADAPTER[@]}" </dev/null && grep -q -- "model-fetch testset-fetch" "$tmp/log" \
  && grep -q -- "swap eval-shadow .*--split test --set /split/set" "$tmp/log" && ! grep -q "DEV SPLIT" "$tmp/out"; then
  ok "a locked split is fetched and scored"
else bad "the locked split"; fi
cp "$tmp/testsets.keep" "$tmp/gm/lock/testsets.lock"

exit "$fail"

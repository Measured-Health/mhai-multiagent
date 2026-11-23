#!/usr/bin/env bash
# Swap our own model on the GPU in one command (deploy/README.md, "Swapping our own model").
# Run by the operator, as the deploy user, on the GPU server, in /srv/mhai-backend:
#
#   bash gpu-model/swap-model.sh --adapter DIR --name NAME --version X.Y.Z --profile PROFILE
#   bash gpu-model/swap-model.sh --gguf FILE  --name NAME --version X.Y.Z --profile PROFILE
#   bash gpu-model/swap-model.sh --rollback
#
#   options: --check-only (build, check and shadow-test; never switch)
#            --force-without-gate (switch even if the gate does not pass: you type the
#              version again first, and the swap is logged as forced)
#            --max-seconds N (the gate's median seconds per answer, default 90)
#
# 1. Checks, before anything starts: the name, a version that is new (no folder for it,
#    never swapped to, and above the served one for the same name), the profile, the
#    pipeline image (it holds the checks: `mhai-models swap`), one swap at a time.
# 2. --adapter: the researcher's QLoRA adapter folder is copied, then checked as data only
#    (`mhai-models swap check-adapter`, no network: allowed files only, no pickle or code,
#    a LoRA adapter on an allowed base at its pinned revision, a cca training manifest on
#    that base, its licence in models/licences.json and its datasets approved). The base is
#    downloaded once into $ROOT/.cache/hf (the only step with a network; no token, no
#    secret), then merged and converted in a throwaway builder container with no network
#    and no secret (gpu-model/builder/), into $ROOT/<name>/<version>/model-q4_k_m.gguf with
#    manifest.json (SHA-256, sizes, tool versions). --gguf: the file is copied to
#    $ROOT/<name>/<version>/model.gguf. Either way the folder is read-only afterwards.
# 3. The GGUF's header checks (`mhai-models swap check-gguf`: the promotion's GGUF checks,
#    an allowed architecture, the base's for an adapter, and the adapter's chat template).
# 4. The shadow test: a second gpu-model instance (the same pinned image and entrypoint,
#    which re-hashes the file) serves the candidate on an internal network of its own,
#    as `gpu-model`, beside the live one (the GPU's free memory is checked first); the
#    clinical_answer gate runs against it through the own route's client and the
#    candidate's answer profile (`mhai-models swap eval-shadow`): the locked split when
#    models/testsets.lock names one, else the dev split, said so loudly.
# 5. Only on a pass (or --force-without-gate): the served GPU_MODEL_* settings in .env
#    are kept as GPU_MODEL_PREVIOUS_*, the new ones written, and gpu-model/gpu-model.sh
#    re-hashes, restarts and waits for the new alias. If that fails, the old settings are
#    put back and gpu-model.sh run again. --rollback swaps PREVIOUS and current the same
#    way. Hospitals' own_model switches are never touched.
# 6. Every swap, rollback and refusal is a line in $ROOT/swaps.log: UTC time, operator,
#    from and to (alias and SHA-256), the gate's verdict, split and metrics, forced or not.
#
# It reads no secret and prints none: the .env keys it reads and writes are GPU_MODEL_*
# and PIPELINE_IMAGE, which hold none. Exit 0: swapped (or checked); anything else: not.
set -euo pipefail
umask 022

DEST=${MHAI_BACKEND_DIR:-$PWD}
ROOT=${GPU_MODEL_ROOT:-/srv/mhai-models}
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
DEFAULT_IMAGE=ghcr.io/measured-health/mhai-multiagent-pipeline-pilot:latest
SHADOW=mhai-gpu-model-shadow
SHADOW_NET=mhai-gpu-model-shadow
SPLIT_VOLUME=mhai-swap-split
START_TIMEOUT=${GPU_MODEL_START_TIMEOUT:-600}
INTERVAL=${GPU_MODEL_POLL_INTERVAL:-5}
HEADROOM_MIB=${GPU_MODEL_SHADOW_HEADROOM_MIB:-2048}
BUILD_MEM=${GPU_MODEL_BUILD_MEM:-32g}
BUILD_CPUS=${GPU_MODEL_BUILD_CPUS:-4}
MIN_FREE_GB=${GPU_MODEL_BUILD_MIN_FREE_GB:-60}
EVAL_MEM=${GPU_MODEL_EVAL_MEM:-4g}
KEYS=(ENABLED FILE NAME VERSION SHA256 PROFILE)

die() { printf '::error::swap-model: %s\n' "$*" >&2; exit 1; }
info() { printf 'swap-model: %s\n' "$*"; }
say() { printf '\n== %s ==\n' "$*"; }
warn() { printf '::warning::swap-model: %s\n' "$*" >&2; }

usage() {
  cat >&2 <<'EOF'
usage: bash gpu-model/swap-model.sh --adapter DIR | --gguf FILE
         --name NAME --version X.Y.Z --profile PROFILE
         [--check-only] [--force-without-gate] [--max-seconds N]
       bash gpu-model/swap-model.sh --rollback
EOF
  exit 2
}

MODE='' SRC='' NAME='' VERSION='' PROFILE='' CHECK_ONLY=0 FORCE=0 MAX_SECONDS=90
while [ $# -gt 0 ]; do
  case "$1" in
    --adapter|--gguf)
      { [ $# -ge 2 ] && [ -z "$MODE" ]; } || usage
      MODE=${1#--}; SRC=$2; shift 2 ;;
    --rollback) [ -z "$MODE" ] || usage; MODE=rollback; shift ;;
    --name) [ $# -ge 2 ] || usage; NAME=$2; shift 2 ;;
    --version) [ $# -ge 2 ] || usage; VERSION=$2; shift 2 ;;
    --profile) [ $# -ge 2 ] || usage; PROFILE=$2; shift 2 ;;
    --max-seconds) [ $# -ge 2 ] || usage; MAX_SECONDS=$2; shift 2 ;;
    --check-only) CHECK_ONLY=1; shift ;;
    --force-without-gate) FORCE=1; shift ;;
    *) usage ;;
  esac
done
[ -n "$MODE" ] || usage
if [ "$MODE" = rollback ]; then
  { [ -z "$NAME$VERSION$PROFILE" ] && [ "$CHECK_ONLY$FORCE" = 00 ]; } || usage
fi

num='(0|[1-9][0-9]{0,8})'
valid_name() { [[ $1 =~ ^[a-z0-9][a-z0-9-]{1,62}$ ]]; }
valid_version() { [[ $1 =~ ^$num\.$num\.$num$ ]]; }
valid_sha() { [[ $1 =~ ^[0-9a-f]{64}$ ]]; }
# The answer profiles the worker knows (gpu-model.sh keeps the same list).
valid_profile() { case "$1" in default|gemma4-e4b-cca-v1) return 0 ;; *) return 1 ;; esac; }
# 1 when $1 > $2 (both MAJOR.MINOR.PATCH)
version_gt() {
  local a b i
  IFS=. read -r -a a <<<"$1"; IFS=. read -r -a b <<<"$2"
  for i in 0 1 2; do
    if ((10#${a[i]} > 10#${b[i]})); then return 0; fi
    if ((10#${a[i]} < 10#${b[i]})); then return 1; fi
  done
  return 1
}

if [ "$MODE" != rollback ]; then
  valid_name "$NAME" || die "--name must be 2 to 63 of a-z 0-9 - (a letter or digit first)"
  valid_version "$VERSION" || die "--version must be MAJOR.MINOR.PATCH"
  valid_profile "$PROFILE" || die "--profile must name an answer profile (default or gemma4-e4b-cca-v1)"
  [[ $MAX_SECONDS =~ ^[1-9][0-9]{0,3}$ ]] || die "--max-seconds must be 1 to 9999"
fi

say "Prerequisites"
command -v docker >/dev/null || die "docker is not installed"
docker compose version >/dev/null 2>&1 || die "docker compose (v2) is not available"
cd "$DEST"
[ -f .env ] || die "no .env in $DEST"
{ [ -d "$ROOT" ] && [ ! -L "$ROOT" ]; } || die "$ROOT is not a folder on this host"
command -v flock >/dev/null || die "flock (util-linux) is not installed"
exec 9>>"$ROOT/.swap.lock"
flock -n 9 || die "another swap is running"
LOG="$ROOT/swaps.log"
if [ -d "$HERE/lock" ]; then LOCK_DIR="$HERE/lock"; else LOCK_DIR=$(cd "$HERE/../../models" 2>/dev/null && pwd) || true; fi
for f in testsets.lock licences.json approved-datasets.json; do
  [ -f "${LOCK_DIR:-/nonexistent}/$f" ] || die "no $f next to this script (gpu-model/lock/, shipped by the deploy)"
done

env_get() { sed -n "s/^$1=\(.*\)\$/\1/p" .env | tail -n1; }
declare -A CUR PREV
for k in "${KEYS[@]}"; do
  CUR[$k]=$(env_get "GPU_MODEL_$k")
  PREV[$k]=$(env_get "GPU_MODEL_PREVIOUS_$k")
done
alias_of() { if [ -n "$1" ]; then printf '%s@%s#%s' "$1" "$2" "${3:0:12}"; fi; }
OPERATOR=$(printf '%s' "${SUDO_USER:-$(id -un)}" | tr -cd 'A-Za-z0-9._-')
OPERATOR=${OPERATOR:-unknown}

# One JSON line in swaps.log; every field is a checked token, the summary is the gate's own
# aggregates-only JSON (or null). Nothing in it is a secret.
log_swap() { # action to_alias to_sha split gate forced summary
  local now from_alias summary=${7:-null}
  now=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  from_alias=$(alias_of "${CUR[NAME]}" "${CUR[VERSION]}" "${CUR[SHA256]}")
  summary=$(printf '%s' "$summary" | tr -d '\n' | tr -cd '[:print:]')
  printf '{"at":"%s","operator":"%s","action":"%s","from":{"alias":"%s","sha256":"%s"},"to":{"alias":"%s","sha256":"%s"},"split":"%s","gate":"%s","forced":%s,"summary":%s}\n' \
    "$now" "$OPERATOR" "$1" "$from_alias" "${CUR[SHA256]}" "$2" "$3" "$4" "$5" "$6" "$summary" >>"$LOG"
}

# Write GPU_MODEL_<k>=$2... and GPU_MODEL_PREVIOUS_<k>=$1... into .env, atomically, with
# .env's mode. Arguments: two names of associative arrays (new, then previous).
write_env() {
  local -n new=$1 old=$2
  local tmp=".env.swap.$$" k drop=''
  for k in "${KEYS[@]}"; do drop+="GPU_MODEL_$k GPU_MODEL_PREVIOUS_$k "; done
  (umask 077; awk -v drop="$drop" 'BEGIN { n = split(drop, d, " "); for (i = 1; i <= n; i++) skip[d[i]] = 1 }
    { key = $0; sub(/=.*/, "", key); if (!(key in skip)) print }' .env >"$tmp")
  for k in "${KEYS[@]}"; do printf 'GPU_MODEL_%s=%s\n' "$k" "${new[$k]}" >>"$tmp"; done
  for k in "${KEYS[@]}"; do printf 'GPU_MODEL_PREVIOUS_%s=%s\n' "$k" "${old[$k]}" >>"$tmp"; done
  chmod --reference=.env "$tmp"
  mv -f "$tmp" .env
}

run_switch() { MHAI_BACKEND_DIR="$DEST" GPU_MODEL_ROOT="$ROOT" bash "$HERE/gpu-model.sh"; }

# Serve the settings in array $1, keeping array $2 as the previous ones; on any failure put
# the old .env back and run the switch again, then fail.
apply() { # new prev action to_alias to_sha split gate forced summary
  local saved
  saved=$(cat .env; echo x); saved=${saved%x}
  write_env "$1" "$2"
  if run_switch; then
    log_swap "$3" "$4" "$5" "$6" "$7" "$8" "${9:-}"
    return 0
  fi
  warn "the new settings did not come up: putting the old ones back"
  printf '%s' "$saved" >".env.swap.$$"
  chmod --reference=.env ".env.swap.$$"
  mv -f ".env.swap.$$" .env
  log_swap "${3}_failed" "$4" "$5" "$6" "$7" "$8" "${9:-}"
  if run_switch; then
    die "$3 failed; the previous model is served again"
  fi
  die "$3 failed, and the previous settings did not come up either: see gpu-model.sh's reason above"
}

if [ "$MODE" = rollback ]; then
  say "Rollback"
  for k in FILE NAME VERSION SHA256 PROFILE; do
    [ -n "${PREV[$k]}" ] || die "no GPU_MODEL_PREVIOUS_$k in .env: nothing to roll back to"
  done
  { valid_name "${PREV[NAME]}" && valid_version "${PREV[VERSION]}" && valid_sha "${PREV[SHA256]}" \
    && valid_profile "${PREV[PROFILE]}"; } || die "GPU_MODEL_PREVIOUS_* are malformed: not rolled back"
  declare -A NEW_SET OLD_SET
  for k in "${KEYS[@]}"; do NEW_SET[$k]=${PREV[$k]}; OLD_SET[$k]=${CUR[$k]}; done
  NEW_SET[ENABLED]=true
  to=$(alias_of "${PREV[NAME]}" "${PREV[VERSION]}" "${PREV[SHA256]}")
  info "from $(alias_of "${CUR[NAME]}" "${CUR[VERSION]}" "${CUR[SHA256]}") to $to"
  apply NEW_SET OLD_SET rollback "$to" "${PREV[SHA256]}" none none false
  info "rolled back: serving $to (run --rollback again to undo)"
  exit 0
fi

# ---- a swap --------------------------------------------------------------------------

[ "${CUR[NAME]}" != "$NAME" ] || [ -z "${CUR[VERSION]}" ] || version_gt "$VERSION" "${CUR[VERSION]}" \
  || die "--version must be above the served $NAME@${CUR[VERSION]}"
OUT="$ROOT/$NAME/$VERSION"
{ [ ! -e "$OUT" ] && [ ! -L "$OUT" ]; } || die "$OUT exists: every swap is a new version"
if [ -f "$LOG" ] && grep -qF "\"to\":{\"alias\":\"$NAME@$VERSION#" "$LOG"; then
  die "$NAME@$VERSION was used before (swaps.log): every swap is a new version"
fi

if [ "$FORCE" = 1 ]; then
  warn "--force-without-gate: the model will be served even if the clinical_answer gate fails"
  printf 'Type the version (%s) to confirm a switch without a passing gate: ' "$VERSION" >&2
  typed=''
  read -r typed || true
  [ "$typed" = "$VERSION" ] || die "the version typed does not match: nothing was done"
fi

IMAGE=$(env_get PIPELINE_IMAGE); IMAGE=${IMAGE:-$DEFAULT_IMAGE}
docker image inspect "$IMAGE" >/dev/null 2>&1 || docker pull -q "$IMAGE" >/dev/null \
  || die "$IMAGE is not on this host and could not be pulled"
docker run --rm --network none --entrypoint mhai-models "$IMAGE" swap --help >/dev/null 2>&1 \
  || die "$IMAGE has no 'mhai-models swap': deploy a newer pipeline image first"
GPU_IMAGE=$(docker compose --profile gpu-model config --images gpu-model 2>/dev/null | head -n1)
[[ $GPU_IMAGE == *@sha256:* ]] || die "could not read the gpu-model service's pinned image"

WORK="$ROOT/.work/$NAME-$VERSION"
KEEP_OUT=0
cleanup() {
  docker rm -f "$SHADOW" >/dev/null 2>&1 || true
  docker network rm "$SHADOW_NET" >/dev/null 2>&1 || true
  docker volume rm -f "$SPLIT_VOLUME" >/dev/null 2>&1 || true
  if [ -d "$WORK" ]; then chmod -R u+w "$WORK" 2>/dev/null || true; rm -rf "$WORK"; fi
  if [ "$KEEP_OUT" = 0 ] && [ -d "$OUT" ]; then chmod -R u+w "$OUT" 2>/dev/null || true; rm -rf "$OUT"; fi
}
trap cleanup EXIT

# The checks, as data only, in the pipeline image: no network, read-only, the operator's uid.
helper() { # mounts... -- args...
  local mounts=()
  while [ "$1" != -- ]; do mounts+=(-v "$1"); shift; done
  shift
  docker run --rm --network none --read-only --tmpfs /tmp --cap-drop ALL \
    --security-opt no-new-privileges --user "$(id -u):$(id -g)" --memory 2g \
    "${mounts[@]}" --entrypoint mhai-models "$IMAGE" swap "$@" </dev/null
}
json_field() { sed -n "s/.*\"$1\":\"\([^\"]*\)\".*/\1/p" | head -n1; }

mkdir -p "$ROOT/$NAME" "$ROOT/.work"
if [ "$MODE" = adapter ]; then
  say "The adapter (checked as data; nothing in it is run)"
  { [ -d "$SRC" ] && [ ! -L "$SRC" ]; } || die "--adapter must be a folder (not a link)"
  free_gb=$(df -P -k "$ROOT" | awk 'NR == 2 { print int($4 / 1048576) }')
  [ "${free_gb:-0}" -ge "$MIN_FREE_GB" ] || die "$ROOT has ${free_gb:-0} GB free; the build needs $MIN_FREE_GB"
  rm -rf "$WORK"; mkdir -p "$WORK/adapter" "$WORK/scratch"
  # -P: links are copied as links, and the check refuses them.
  cp -RP -- "$SRC"/. "$WORK/adapter/"
  out=$(helper "$WORK/adapter:/in:ro" "$LOCK_DIR:/lock:ro" -- check-adapter /in \
    --licences /lock/licences.json --datasets /lock/approved-datasets.json) \
    || die "the adapter was refused: $(printf '%s' "$out" | tail -n1)"
  BASE=$(printf '%s' "$out" | json_field base)
  REVISION=$(printf '%s' "$out" | json_field revision)
  ARCH=$(printf '%s' "$out" | json_field architecture)
  { [[ $BASE =~ ^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$ ]] && [[ $REVISION =~ ^[0-9a-f]{40}$ ]] \
    && [[ $ARCH =~ ^[a-z0-9]+$ ]]; } || die "unexpected answer from the adapter check"
  info "adapter on $BASE at ${REVISION:0:12} (licence and datasets approved)"

  say "The GGUF builder (pinned; no network and no secret when it builds)"
  tag="mhai-gguf-builder:$(cat "$HERE/builder/Dockerfile" "$HERE/builder/merge.py" | sha256sum | cut -c1-12)"
  if ! docker image inspect "$tag" >/dev/null 2>&1; then
    docker build -q -t "$tag" "$HERE/builder" >/dev/null || die "the builder image did not build"
  fi
  builder_id=$(docker image inspect -f '{{.Id}}' "$tag")
  run_builder() { # docker run options... -- merge.py args...
    local opts=()
    while [ "$1" != -- ]; do opts+=("$1"); shift; done
    shift
    docker run --rm --read-only --tmpfs /tmp --cap-drop ALL --security-opt no-new-privileges \
      --user "$(id -u):$(id -g)" -e HOME=/tmp -e HF_HOME=/cache "${opts[@]}" "$tag" "$@" </dev/null
  }
  mkdir -p "$ROOT/.cache/hf"
  info "fetching the base (once; cached in $ROOT/.cache/hf)"
  run_builder -v "$ROOT/.cache/hf:/cache" -- fetch --repo "$BASE" --revision "$REVISION" \
    || die "could not download the base"
  mkdir -p "$OUT"
  info "merging and converting (CPU; tens of minutes)"
  run_builder --network none --memory "$BUILD_MEM" --memory-swap "$BUILD_MEM" --cpus "$BUILD_CPUS" \
    -e MHAI_BUILDER_IMAGE="$builder_id" -v "$ROOT/.cache/hf:/cache:ro" -v "$WORK/adapter:/adapter:ro" \
    -v "$WORK/scratch:/work" -v "$OUT:/out" -- build --repo "$BASE" --revision "$REVISION" \
    --adapter /adapter --work /work --out /out --name "$NAME" --version "$VERSION" \
    --profile "$PROFILE" --built-by "$OPERATOR" || die "the build failed"
  GGUF="$OUT/model-q4_k_m.gguf"
  { [ -f "$GGUF" ] && [ ! -L "$GGUF" ] && [ -f "$OUT/manifest.json" ]; } || die "the build wrote no GGUF"
  CHECK=(check-gguf /out/model-q4_k_m.gguf --architecture "$ARCH" --template-from /adapter)
  MOUNTS=("$OUT:/out:ro" "$WORK/adapter:/adapter:ro")
else
  say "The GGUF"
  { [ -f "$SRC" ] && [ ! -L "$SRC" ]; } || die "--gguf must be a regular file (not a link)"
  mkdir -p "$OUT"
  GGUF="$OUT/model.gguf"
  cp -- "$SRC" "$GGUF"
  CHECK=(check-gguf /out/model.gguf)
  MOUNTS=("$OUT:/out:ro")
fi

say "The GGUF's checks"
out=$(helper "${MOUNTS[@]}" -- "${CHECK[@]}") || die "the GGUF was refused: $(printf '%s' "$out" | tail -n1)"
info "architecture $(printf '%s' "$out" | json_field architecture)"
SHA=$(sha256sum -- "$GGUF" | cut -d' ' -f1)
BYTES=$(stat -c %s -- "$GGUF")
if [ "$MODE" = adapter ]; then
  grep -q "\"sha256\": \"$SHA\"" "$OUT/manifest.json" || die "the GGUF's SHA-256 is not the one the build recorded"
else
  printf '{\n  "built_at": "%s",\n  "built_by": "%s",\n  "gguf": {"bytes": %s, "file": "model.gguf", "sha256": "%s"},\n  "name": "%s",\n  "profile": "%s",\n  "schema": "mhai.gpu_model.build/1",\n  "source": "gguf",\n  "version": "%s"\n}\n' \
    "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$OPERATOR" "$BYTES" "$SHA" "$NAME" "$PROFILE" "$VERSION" >"$OUT/manifest.json"
fi
[ "$SHA" != "${CUR[SHA256]}" ] || die "this GGUF is the one already served"
chmod 0444 "$GGUF" "$OUT/manifest.json"
chmod 0555 "$OUT"
TO=$(alias_of "$NAME" "$VERSION" "$SHA")
info "$TO: $BYTES bytes, SHA-256 $SHA (read-only in $OUT)"

say "Shadow test (a second gpu-model, internal network only)"
GATE=not_evaluated SPLIT=none SUMMARY=null
free_mib=$(nvidia-smi --query-gpu=memory.free --format=csv,noheader,nounits 2>/dev/null | head -n1 | tr -cd '0-9' || true)
need_mib=$(( BYTES / 1048576 + HEADROOM_MIB ))
shadow_ok=1
if [ -z "$free_mib" ] || [ "$free_mib" -lt "$need_mib" ]; then
  shadow_ok=0
  [ "$FORCE" = 1 ] || { KEEP_OUT=1; log_swap refused "$TO" "$SHA" none not_evaluated false null
    die "the GPU has ${free_mib:-unknown} MiB free; the shadow needs $need_mib: not switched"; }
  warn "the GPU has ${free_mib:-unknown} MiB free (needs $need_mib): no shadow test (forced)"
  GATE=skipped
fi

if [ "$shadow_ok" = 1 ]; then
  docker rm -f "$SHADOW" >/dev/null 2>&1 || true
  docker network rm "$SHADOW_NET" >/dev/null 2>&1 || true
  docker network create --internal "$SHADOW_NET" >/dev/null
  ctx=$(env_get GPU_MODEL_CTX); layers=$(env_get GPU_MODEL_GPU_LAYERS)
  threads=$(env_get GPU_MODEL_THREADS); mem=$(env_get GPU_MODEL_MEM)
  docker run -d --name "$SHADOW" --network "$SHADOW_NET" --network-alias gpu-model \
    --user 1000:1000 --read-only --tmpfs /tmp --cap-drop ALL --security-opt no-new-privileges \
    --device nvidia.com/gpu=all --memory "${mem:-8g}" --memory-swap "${mem:-8g}" --cpus "${threads:-4}" \
    -e GPU_MODEL_NAME="$NAME" -e GPU_MODEL_VERSION="$VERSION" -e GPU_MODEL_SHA256="$SHA" \
    -e GPU_MODEL_CTX="${ctx:-8192}" -e GPU_MODEL_GPU_LAYERS="${layers:-999}" -e GPU_MODEL_THREADS="${threads:-4}" \
    -v "$GGUF:/model/model.gguf:ro" -v "$HERE/entrypoint.sh:/mhai/entrypoint.sh:ro" \
    --entrypoint /bin/bash "$GPU_IMAGE" /mhai/entrypoint.sh >/dev/null || die "the shadow did not start"
  waited=0
  until docker exec "$SHADOW" curl -fsS --max-time 5 http://127.0.0.1:8080/health >/dev/null 2>&1; do
    if [ "$(docker inspect -f '{{.State.Running}}' "$SHADOW" 2>/dev/null)" != true ] || [ "$waited" -ge "$START_TIMEOUT" ]; then
      docker logs --tail=40 "$SHADOW" 2>&1 || true
      KEEP_OUT=1
      log_swap refused "$TO" "$SHA" none not_evaluated false null
      die "the shadow did not become healthy (${waited}s): not switched"
    fi
    sleep "$INTERVAL"; waited=$((waited + INTERVAL))
  done
  info "the shadow serves the candidate"

  SET_MOUNT=() EXTRA=()
  if grep -qv '^#' "$LOCK_DIR/testsets.lock"; then
    SPLIT='test'
    say "The locked clinical_answer split (models/testsets.lock)"
    docker volume rm -f "$SPLIT_VOLUME" >/dev/null 2>&1 || true
    docker volume create "$SPLIT_VOLUME" >/dev/null
    docker compose --profile ownmodel run --rm --no-deps -T -v "$SPLIT_VOLUME:/models" \
      -v "$LOCK_DIR:/swaplock:ro" model-fetch testset-fetch --testsets /swaplock/testsets.lock \
      --trust-policy /lock/trust-policy.json --to /models </dev/null \
      || die "could not fetch the locked split (the store reader's credentials and the store): not switched"
    SET_MOUNT=(-v "$SPLIT_VOLUME:/split:ro"); EXTRA=(--set /split/set)
  else
    SPLIT=dev
    say "DEV SPLIT, NOT A LOCKED TEST: models/testsets.lock names no clinical_answer split yet"
    info "A dev pass shows the candidate is no worse on the public dev questions; it is NOT"
    info "the locked gate. swaps.log records it as split \"dev\"."
  fi

  say "The clinical_answer gate against the shadow (one question at a time)"
  set +e
  out=$(docker run --rm --network "$SHADOW_NET" --read-only --tmpfs /tmp --cap-drop ALL \
    --security-opt no-new-privileges --user 1000:1000 --memory "$EVAL_MEM" \
    -e APP_ENV=production -e HF_HUB_OFFLINE=1 -e TRANSFORMERS_OFFLINE=1 \
    -e INTELLIGENCE_LOCAL_LLM_START_TIMEOUT=900 \
    -v "$LOCK_DIR/testsets.lock:/lock/testsets.lock:ro" "${SET_MOUNT[@]}" \
    --entrypoint mhai-models "$IMAGE" swap eval-shadow --name "$NAME" --version "$VERSION" \
    --sha256 "$SHA" --profile "$PROFILE" --split "$SPLIT" "${EXTRA[@]}" \
    --testsets /lock/testsets.lock --max-seconds "$MAX_SECONDS" </dev/null)
  rc=$?
  set -e
  SUMMARY=$(printf '%s\n' "$out" | grep -E '^\{"(eval_shadow|refused|error)"' | tail -n1 || true)
  SUMMARY=${SUMMARY:-null}
  if [ "$rc" = 0 ] && printf '%s' "$SUMMARY" | grep -q '"verdict":"pass"'; then GATE=pass
  elif [ "$rc" = 1 ]; then GATE=fail
  else GATE=not_evaluated; fi
  docker rm -f "$SHADOW" >/dev/null 2>&1 || true
  docker network rm "$SHADOW_NET" >/dev/null 2>&1 || true
  info "$SUMMARY"
fi
KEEP_OUT=1

case "$GATE" in
  pass) say "PASS ($SPLIT split)" ;;
  *)
    say "THE GATE DID NOT PASS ($GATE, $SPLIT split)"
    if [ "$FORCE" != 1 ] || [ "$CHECK_ONLY" = 1 ]; then
      log_swap refused "$TO" "$SHA" "$SPLIT" "$GATE" false "$SUMMARY"
      die "not switched (the model stays in $OUT for the record)"
    fi
    warn "FORCED: switching to $TO although the gate did not pass ($GATE); this is logged"
    ;;
esac

if [ "$CHECK_ONLY" = 1 ]; then
  log_swap checked "$TO" "$SHA" "$SPLIT" "$GATE" false "$SUMMARY"
  info "--check-only: passed, not switched"
  exit 0
fi

say "Switching"
# shellcheck disable=SC2034 # read by apply through namerefs
declare -A NEW_SET OLD_SET
for k in "${KEYS[@]}"; do OLD_SET[$k]=${CUR[$k]}; done
NEW_SET=([ENABLED]=true [FILE]="$GGUF" [NAME]="$NAME" [VERSION]="$VERSION" [SHA256]="$SHA" [PROFILE]="$PROFILE")
forced=false
[ "$GATE" = pass ] || forced=true
apply NEW_SET OLD_SET swap "$TO" "$SHA" "$SPLIT" "$GATE" "$forced" "$SUMMARY"
info "swapped: serving $TO (bash gpu-model/swap-model.sh --rollback to go back)"

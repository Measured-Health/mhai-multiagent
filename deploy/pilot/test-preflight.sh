#!/usr/bin/env bash
# Tests for eval-local.sh's pre-flight (and ownmodel-lib.sh), with a fake docker
# that answers only the read-only questions the pre-flight asks and fails on
# anything else: no Docker, no model, no host. Every file is synthetic.
# Run: bash deploy/pilot/test-preflight.sh
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
fail=0
COMMIT=b1b770d90d94cad9e9ea5b993bdbc383a522fb28

mkdir -p "$tmp/bin"
cat >"$tmp/bin/docker" <<'FAKE'
#!/usr/bin/env bash
# The pre-flight's questions only; anything else (up, run of a service, volume,
# build, pull) is a test failure.
printf '%s\n' "$*" >>"$FAKE_LOG"
case "$*" in
  "compose version") exit 0 ;;
  *"compose --profile cloud --profile pilot --profile ownmodel ps -q pipeline") printf '%s' "${FAKE_PIPELINE_ID:-}"; exit 0 ;;
  *"compose --profile cloud --profile pilot --profile ownmodel ps -q llama") printf '%s' "${FAKE_LLAMA_ID:-}"; exit 0 ;;
  "image inspect -f {{.Id}} "*) [ "${FAKE_NO_IMAGE:-}" = "${*: -1}" ] && exit 1; echo "sha256:${*: -1}"; exit 0 ;;
  "inspect -f {{.Image}} "*) echo "${FAKE_RUNNING_IMAGE:-}"; exit 0 ;;
  "run --rm --network none --entrypoint python "*) echo "${FAKE_GATE_COMMIT:-}"; exit 0 ;;
  "run --rm --network none --entrypoint /usr/local/bin/llama-server "*) echo "${FAKE_LLAMA_VERSION:-}" >&2; exit 0 ;;
esac
echo "UNEXPECTED docker $*" >>"$FAKE_LOG"
exit 99
FAKE
chmod +x "$tmp/bin/docker"

GIB=$((1024 * 1024)) # kB per GiB, as /proc/meminfo counts

setup() { # a host with 8 GiB, the pilot and the own model enabled, a cca pin
  rm -rf "$tmp/dest" "$tmp/log"
  mkdir -p "$tmp/dest/models"
  printf 'MemTotal: %d kB\nMemFree: 1 kB\nMemAvailable: %d kB\n' $((8 * GIB)) $((7 * GIB)) >"$tmp/meminfo"
  cat >"$tmp/dest/.env" <<'ENV'
COMPOSE_PROFILES=cloud,pilot,ownmodel
PILOT_MODEL_ROLES=cca
PILOT_LOCAL_MODEL_MEM=3g
PIPELINE_IMAGE=pipeline-synthetic:1
ENV
  printf '{"models":{"cca":{"gate":"answer","manifest_sha256":"%s","name":"mhai-cca-synthetic","version":"1.0.0"}},"schema":"mhai.models.lock/1"}\n' \
    "$(printf 'c%.0s' $(seq 64))" >"$tmp/dest/models/models.lock"
  printf '{}\n' >"$tmp/dest/models/trust-policy.json"
  printf '# no entries\n' >"$tmp/dest/models/testsets.lock"
  export FAKE_PIPELINE_ID='' FAKE_LLAMA_ID='' FAKE_RUNNING_IMAGE='' FAKE_NO_IMAGE=''
  export FAKE_GATE_COMMIT=$COMMIT
  export FAKE_LLAMA_VERSION="version: 1.0.0 (build 12359, commit b1b770d)"
}

run() {
  PATH="$tmp/bin:$PATH" FAKE_LOG="$tmp/log" MHAI_BACKEND_DIR="$tmp/dest" MEMINFO="$tmp/meminfo" \
    bash "$here/eval-local.sh" --preflight-only "$@" </dev/null >"$tmp/out" 2>"$tmp/err"
}

nothing_started() { ! grep -q UNEXPECTED "$tmp/log" 2>/dev/null; }

expect_ok() {
  if ! run; then echo "FAIL $1: refused ($(cat "$tmp/err"))"; fail=1; return; fi
  if ! grep -q 'Pre-flight passed' "$tmp/out"; then echo "FAIL $1: $(cat "$tmp/out")"; fail=1; return; fi
  if ! nothing_started; then echo "FAIL $1: $(grep UNEXPECTED "$tmp/log")"; fail=1; return; fi
  echo "ok   $1"
}

expect_refused() {
  if run "${@:3}"; then echo "FAIL $1: passed"; fail=1; return; fi
  if ! grep -q -- "$2" "$tmp/err"; then echo "FAIL $1: wrong reason ($(cat "$tmp/err"))"; fail=1; return; fi
  if ! nothing_started; then echo "FAIL $1: $(grep UNEXPECTED "$tmp/log")"; fail=1; return; fi
  echo "ok   $1"
}

setup; expect_ok "an 8 GiB host with a 3 GiB model passes"

setup; printf 'MemTotal: 3878000 kB\nMemAvailable: 1500000 kB\n' >"$tmp/meminfo" # the demo host
expect_refused "the demo host (3.7 GiB) is refused: no model fits next to the pilot" "can't fit next to the pilot"
if ! grep -q 'Resize the instance' "$tmp/err"; then echo "FAIL the refusal says to resize"; fail=1; fi

setup; sed -i 's/^PILOT_LOCAL_MODEL_MEM=.*/PILOT_LOCAL_MODEL_MEM=6g/' "$tmp/dest/.env"
expect_refused "a model too large for the host is refused" "can't fit next to the pilot"

setup; printf 'MemTotal: %d kB\nMemAvailable: %d kB\n' $((8 * GIB)) $((3 * GIB)) >"$tmp/meminfo"
expect_refused "too little free memory now is refused" "not enough free memory"

setup; printf 'MemTotal: %d kB\nMemAvailable: %d kB\n' $((8 * GIB)) $((2 * GIB)) >"$tmp/meminfo"
export FAKE_PIPELINE_ID=p1 FAKE_LLAMA_ID=l1 FAKE_RUNNING_IMAGE=sha256:pipeline-synthetic:1
expect_ok "with llama and the pipeline running, only the eval container's memory is needed"

setup; sed -i 's/^PILOT_LOCAL_MODEL_MEM=.*/PILOT_LOCAL_MODEL_MEM=3gb/' "$tmp/dest/.env"
expect_refused "a malformed model size is refused" "PILOT_LOCAL_MODEL_MEM"

setup; rm "$tmp/dest/models/models.lock"
expect_refused "no models.lock: refused" "models.lock is missing"

setup; ln -s "$tmp/meminfo" "$tmp/dest/models/trust-policy.json.x"; rm "$tmp/dest/models/trust-policy.json"
mv "$tmp/dest/models/trust-policy.json.x" "$tmp/dest/models/trust-policy.json"
expect_refused "a linked trust policy is refused" "trust-policy.json is missing (or a link)"

setup; printf '{"models":{},"schema":"mhai.models.lock/1"}\n' >"$tmp/dest/models/models.lock"
expect_refused "a lock without cca is refused" "pins no cca"

setup; sed -i 's/^PILOT_MODEL_ROLES=.*/PILOT_MODEL_ROLES=/' "$tmp/dest/.env"
expect_refused "without PILOT_MODEL_ROLES=cca: refused" "enable-ownmodel.sh first"

setup; sed -i 's/^COMPOSE_PROFILES=.*/COMPOSE_PROFILES=cloud,pilot/' "$tmp/dest/.env"
expect_refused "without the ownmodel profile: refused" "has no ownmodel"

setup; export FAKE_LLAMA_VERSION="version: 1.0.0 (build 12000, commit 0000000)"
expect_refused "a llama image at another llama.cpp commit is refused" "the engine the gate scored with"

setup; export FAKE_NO_IMAGE=mhai-llama:local
expect_refused "no llama image: refused" "is not built"

setup; export FAKE_PIPELINE_ID=p1 FAKE_RUNNING_IMAGE=sha256:another-image
expect_refused "a running pipeline on another image is refused" "the running pipeline is not"

setup; expect_refused "a key on the command line is refused" "usage" --key sk-synthetic-not-a-key
if grep -q 'sk-synthetic' "$tmp/out" "$tmp/err"; then echo "FAIL the argument was echoed"; fail=1; fi

# The library on its own.
# shellcheck source=deploy/pilot/ownmodel-lib.sh
. "$here/ownmodel-lib.sh"
if [ "$(mem_mib 4g)" = 4096 ] && [ "$(mem_mib 3500m)" = 3500 ] && ! mem_mib 0g && ! mem_mib 4; then
  echo "ok   mem_mib"
else
  echo "FAIL mem_mib"; fail=1
fi
if llama_commit_ok "x version: 1.0.0-dev (build 1, commit b1b770d90d) y" "$COMMIT" \
  && ! llama_commit_ok "no version line" "$COMMIT"; then
  echo "ok   llama_commit_ok"
else
  echo "FAIL llama_commit_ok"; fail=1
fi

exit "$fail"

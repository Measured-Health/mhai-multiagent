#!/usr/bin/env bash
# Tests for pipeline.sh (the deploy's pipeline step) with a fake docker on PATH, and for
# enable-pilot.sh's argument checks (--provider none): no Docker, no model, no host.
# Every file and value is synthetic. Run: bash deploy/pilot/test-pipeline.sh
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
fail=0
mkdir -p "$tmp/bin"

# The fake docker logs every call and answers as FAKE_* say.
cat >"$tmp/bin/docker" <<'FAKE'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$FAKE_LOG"
case "$*" in
  *"ps -q sample-ai"*) if [ "${FAKE_SAMPLE:-0}" = 1 ]; then echo sample1; fi; exit 0 ;;
  *"ps -q pipeline"*)
    if [ "${FAKE_PIPELINE_RUNNING:-0}" = 1 ] || [ -e "$FAKE_LOG.up" ]; then echo pipe1; fi; exit 0 ;;
  *"pull -q pipeline"*) exit "${FAKE_PULL_FAILS:-0}" ;;
  "image inspect "*) exit "${FAKE_NO_IMAGE:-0}" ;;
  *"up -d --no-deps pipeline"*) [ "${FAKE_UP_FAILS:-0}" = 1 ] && exit 1; touch "$FAKE_LOG.up"; exit 0 ;;
  *"logs --no-log-prefix pipeline"*)
    if [ "${FAKE_STARTED:-1}" = 1 ]; then echo "worker started service=pipeline"; fi; exit 0 ;;
  "inspect -f "*) echo "${FAKE_STATE:-running 0}"; exit 0 ;;
esac
exit 0
FAKE
chmod +x "$tmp/bin/docker"

NKEY="U$(printf 'A%.0s' $(seq 55))"
DBPW=synthetic-db-password-for-tests

setup() { # a host where enable-pilot.sh --provider none has run; extra .env lines as arguments
  rm -rf "$tmp/dest"
  mkdir -p "$tmp/dest/secrets"
  printf 'SUSYNTHETIC\n' >"$tmp/dest/secrets/nats-pipeline.nk"
  printf '%s' "$DBPW" >"$tmp/dest/secrets/intelligence-db-password"
  : >"$tmp/dest/secrets/pilot-cloud-api-key"
  {
    echo "PIPELINE_ENABLED=true"
    echo "GPU_MODEL_ENABLED=true"
    echo "NATS_PIPELINE_NKEY=$NKEY"
    echo "INTELLIGENCE_DB_PASSWORD=$DBPW"
    echo "CORE_OWN_MODEL_ALLOWED=true"
    for kv in "$@"; do echo "$kv"; done
  } >"$tmp/dest/.env"
  printf 'MemTotal: 33554432 kB\nMemAvailable: 16777216 kB\n' >"$tmp/meminfo"
}

run() {
  : >"$tmp/log"; rm -f "$tmp/log.up"
  PATH="$tmp/bin:$PATH" FAKE_LOG="$tmp/log" MHAI_BACKEND_DIR="$tmp/dest" MEMINFO="$tmp/meminfo" \
    PIPELINE_START_TIMEOUT=2 PIPELINE_POLL_INTERVAL=1 bash "$here/pipeline.sh" >"$tmp/out" 2>&1
}

no_secret_out() { ! grep -q -- "$DBPW" "$tmp/out"; }

expect_started() { # expect_started NAME [PATTERN in the docker log]...
  local name=$1; shift
  if ! run; then echo "FAIL $name: refused ($(cat "$tmp/out"))"; fail=1; return; fi
  if ! grep -q -- "up -d --no-deps pipeline" "$tmp/log"; then echo "FAIL $name: not started"; fail=1; return; fi
  for p in "$@"; do
    if ! grep -q -- "$p" "$tmp/log"; then echo "FAIL $name: no '$p' in $(cat "$tmp/log")"; fail=1; return; fi
  done
  no_secret_out || { echo "FAIL $name: the output holds the password"; fail=1; return; }
  echo "ok   $name"
}

expect_refused() { # expect_refused NAME REASON [started]
  if run; then echo "FAIL $1: passed ($(cat "$tmp/out"))"; fail=1; return; fi
  if ! grep -q -- "$2" "$tmp/out"; then echo "FAIL $1: wrong reason ($(cat "$tmp/out"))"; fail=1; return; fi
  if [ "${3:-}" != started ] && grep -q -- "up -d" "$tmp/log"; then echo "FAIL $1: something was started"; fail=1; return; fi
  no_secret_out || { echo "FAIL $1: the output holds the password"; fail=1; return; }
  echo "ok   $1"
}

# --- off: nothing is touched ---------------------------------------------------------
setup; sed -i '/PIPELINE_ENABLED/d' "$tmp/dest/.env"
if run && [ ! -s "$tmp/log" ] && grep -q "not managed" "$tmp/out"; then echo "ok   unset is off, no docker call"
else echo "FAIL unset: $(cat "$tmp/out")"; fail=1; fi
setup "PIPELINE_ENABLED=yes"; sed -i '0,/PIPELINE_ENABLED=true/{/PIPELINE_ENABLED=true/d}' "$tmp/dest/.env"
if run && [ ! -s "$tmp/log" ]; then echo "ok   anything but true is off"; else echo "FAIL yes: $(cat "$tmp/out")"; fail=1; fi

# --- our own model only ----------------------------------------------------------------
setup; expect_started "own model only: started with no cloud" "--profile pilot --profile gpu-model up -d --no-deps pipeline"
if grep -q -- "egress-proxy" "$tmp/log" || grep -q -- "--profile cloud" "$tmp/log"; then echo "FAIL own model only: the cloud was touched"; fail=1; fi
grep -q "routes: own;" "$tmp/out" || { echo "FAIL own model only: routes $(cat "$tmp/out")"; fail=1; }
setup; FAKE_SAMPLE=1 expect_started "the demo's sample-ai is stopped first" "stop sample-ai"
setup; FAKE_PIPELINE_RUNNING=1 expect_started "a running pipeline is brought up to date"
setup; FAKE_PULL_FAILS=1 expect_started "a failed pull uses the copy on the host"
setup; FAKE_PULL_FAILS=1 FAKE_NO_IMAGE=1 expect_refused "no image at all fails" "Pilot pipeline image"

# --- the cloud stays optional ----------------------------------------------------------
setup "PILOT_CLOUD_PROVIDER=anthropic"
printf 'synthetic-key-for-tests-only-0000' >"$tmp/dest/secrets/pilot-cloud-api-key"
mkdir -p "$tmp/dest/egress-proxy"; : >"$tmp/dest/egress-proxy/squid.conf"; : >"$tmp/dest/egress-proxy/allowed-hosts"
expect_started "cloud and own model: the proxy too" "--profile cloud" "up -d --no-deps egress-proxy"
setup "PILOT_CLOUD_PROVIDER=anthropic"
expect_refused "a provider with the empty placeholder key fails" "is empty"
setup "PILOT_CLOUD_PROVIDER=anthropic"
printf 'synthetic-key-for-tests-only-0000' >"$tmp/dest/secrets/pilot-cloud-api-key"
expect_refused "a provider without the proxy files fails" "egress-proxy/squid.conf"

# --- what the worker needs ---------------------------------------------------------------
setup "GPU_MODEL_ENABLED=false"; sed -i '0,/GPU_MODEL_ENABLED=true/{/GPU_MODEL_ENABLED=true/d}' "$tmp/dest/.env"
expect_refused "no answer route fails" "no answer route"
setup; rm "$tmp/dest/secrets/nats-pipeline.nk"; expect_refused "no NKey fails" "nats-pipeline.nk"
setup; : >"$tmp/dest/secrets/intelligence-db-password"; expect_refused "an empty password file fails" "intelligence-db-password"
setup; rm "$tmp/dest/secrets/pilot-cloud-api-key"; mkdir "$tmp/dest/secrets/pilot-cloud-api-key"
expect_refused "a folder for the key file fails" "not a file"
setup; ln -s /dev/null "$tmp/dest/secrets/link.nk"; rm "$tmp/dest/secrets/nats-pipeline.nk"
ln -s "$tmp/dest/secrets/link.nk" "$tmp/dest/secrets/nats-pipeline.nk"; expect_refused "a linked NKey fails" "nats-pipeline.nk"
setup "NATS_PIPELINE_NKEY=SUSYNTHETIC"; expect_refused "a seed for a public key fails" "NATS_PIPELINE_NKEY"
setup; sed -i '/INTELLIGENCE_DB_PASSWORD/d' "$tmp/dest/.env"; expect_refused "no database password in .env fails" "INTELLIGENCE_DB_PASSWORD"
setup; printf 'MemTotal: 2097152 kB\nMemAvailable: 1048576 kB\n' >"$tmp/meminfo"
expect_refused "too little memory fails" "not enough memory"
setup; printf 'MemTotal: 2097152 kB\nMemAvailable: 1048576 kB\n' >"$tmp/meminfo"
FAKE_PIPELINE_RUNNING=1 expect_started "memory is not checked again while it runs"

# --- the worker must start ---------------------------------------------------------------
setup; FAKE_UP_FAILS=1 expect_refused "a pipeline that does not start fails" "did not start" started
setup; FAKE_STARTED=0 FAKE_STATE="restarting 2" expect_refused "a crashing pipeline fails" "not running" started
setup; FAKE_STARTED=0 expect_refused "a pipeline never ready fails" "within" started

# --- enable-pilot.sh's arguments (nothing runs past them) --------------------------------
args() { ENABLE_PILOT_CHECK_ARGS_ONLY=1 bash "$here/enable-pilot.sh" "$@" </dev/null >"$tmp/out" 2>&1; }
args_ok() { if args "$@"; then echo "ok   enable-pilot $*"; else echo "FAIL enable-pilot $*: $(cat "$tmp/out")"; fail=1; fi; }
args_refused() { # args_refused REASON ARGS...
  local why=$1; shift
  if args "$@"; then echo "FAIL enable-pilot $*: accepted"; fail=1
  elif ! grep -q -- "$why" "$tmp/out"; then echo "FAIL enable-pilot $*: wrong reason ($(cat "$tmp/out"))"; fail=1
  else echo "ok   enable-pilot $* refused"; fi
}
args_ok --provider none
args_ok --provider none --short-code own1
args_ok --provider none --short-code OWN1 --hospital-name "Synthetic Own Hospital"
args_ok --provider anthropic --hospital-name "Synthetic Hospital" --short-code SYN --agreement-ref AGR-2026-01 --zdr-ref SYNTH-ZDR
args_refused "goes with a cloud provider" --provider none --agreement-ref AGR-2026-01
args_refused "goes with a cloud provider" --provider none --zdr-ref SYNTH-ZDR
args_refused "goes with a cloud provider" --provider none --model x
args_refused "goes with a cloud provider" --provider none --budget 5
args_refused "goes with a cloud provider" --provider none --replace-key
args_refused "goes with --short-code" --provider none --hospital-name "Synthetic Own Hospital"
args_refused "--short-code must" --provider none --short-code "a b"
args_refused "--provider must" --provider local
args_refused "--hospital-name is required" --provider anthropic --short-code SYN --agreement-ref AGR-2026-01 --zdr-ref SYNTH-ZDR

exit "$fail"

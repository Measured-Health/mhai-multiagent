#!/usr/bin/env bash
# Tests for deploy/live-site.sh and the live-site refusals of
# deploy/demo/enable-demo.sh and deploy/demo/reset-demo.sh (deploy/README.md,
# "Live site"). No Docker: a fake compose answers and records every call.
# Synthetic values only.
#
#   bash .github/scripts/live-site-test.sh
set -uo pipefail
root=$(cd "$(dirname "$0")/../.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
failures=0

# The fake compose: FAKE_COUNT is psql's answer (FAKE_PSQL_FAIL=1 makes it
# fail), FAKE_SAMPLE a sample-ai container id until it is removed.
cat > "$work/fake-compose" <<'SH'
#!/usr/bin/env bash
echo "$*" >> "$FAKE_LOG"
case "$*" in
  *"exec -T postgres psql"*)
    [ "${FAKE_PSQL_FAIL:-0}" = 1 ] && exit 2
    printf '%s\n' "$FAKE_COUNT" ;;
  *"ps -q sample-ai"*)
    [ -f "$FAKE_STATE/sample-removed" ] || printf '%s\n' "${FAKE_SAMPLE:-}" ;;
  *"rm -f sample-ai"*) touch "$FAKE_STATE/sample-removed" ;;
esac
exit 0
SH
chmod +x "$work/fake-compose"

# A host folder: .env and (optionally) the demo markers.
host() {
  rm -rf "$work/host" "$work/state"; mkdir -p "$work/host/demo" "$work/state"
  printf 'CORE_HOST_PORT=8092\n%s' "${1:-}" > "$work/host/.env"
  date > "$work/host/demo/seeded"; date > "$work/host/demo/reset"
  : > "$work/log"
}
run() { # want-exit name, then env assignments
  local want=$1 name=$2; shift 2
  out=$(env LIVE_SITE_COMPOSE="$work/fake-compose" MHAI_BACKEND_DIR="$work/host" \
    FAKE_LOG="$work/log" FAKE_STATE="$work/state" "$@" bash "$root/deploy/live-site.sh" 2>&1); got=$?
  if [ "$got" = "$want" ]; then echo "ok   $name (exit $got)"; else echo "FAIL $name: exit $got, want $want: $out"; failures=$((failures + 1)); fi
}
check() { # name, then a command that must succeed
  local name=$1; shift
  if "$@"; then echo "ok   $name"; else echo "FAIL $name"; failures=$((failures + 1)); fi
}
logged() { grep -q -- "$1" "$work/log"; }
not_logged() { ! grep -q -- "$1" "$work/log"; }
gone() { [ ! -e "$1" ]; }

host
run 0 "no demo hospital: passes" FAKE_COUNT=0
check "marker written" test -f "$work/host/live-site"
check "demo/seeded removed" gone "$work/host/demo/seeded"
check "demo/reset removed" gone "$work/host/demo/reset"
check "core not stopped" not_logged "stop core"

host
run 0 "sample-ai running: stopped and removed" FAKE_COUNT=0 FAKE_SAMPLE=abc123
check "sample-ai stopped" logged "--profile demo stop sample-ai"
check "sample-ai removed" logged "--profile demo rm -f sample-ai"

host
run 1 "an active demo hospital: refused" FAKE_COUNT=1
check "core stopped (fail closed)" logged "stop core"
check "marker still written" test -f "$work/host/live-site"

host
run 1 "Postgres errors: refused" FAKE_COUNT=0 FAKE_PSQL_FAIL=1
check "core stopped on a query error" logged "stop core"

host
run 1 "an unexpected answer: refused" FAKE_COUNT="not a number"
check "core stopped on a bad answer" logged "stop core"

host "COMPOSE_PROFILES=notifications,demo"
run 1 "the demo profile in .env: refused" FAKE_COUNT=0

rm -rf "$work/host"; mkdir -p "$work/host"
run 1 "a host that is not set up: refused" FAKE_COUNT=0

# The demo scripts refuse on a host with the marker, before any docker call.
host
date > "$work/host/live-site"
mkdir -p "$work/host/secrets"; : > "$work/host/secrets/nats-gateway.nk"
mkdir -p "$work/bin"
printf '#!/bin/sh\necho "docker $*" >> "%s"\nexit 0\n' "$work/log" > "$work/bin/docker"
chmod +x "$work/bin/docker"
for s in enable-demo.sh reset-demo.sh; do
  : > "$work/log"
  out=$(env PATH="$work/bin:$PATH" MHAI_BACKEND_DIR="$work/host" bash "$root/deploy/demo/$s" 2>&1); got=$?
  if [ "$got" != 0 ] && printf '%s' "$out" | grep -q "live site" && [ ! -s "$work/log" ]; then
    echo "ok   $s refuses on a live site"
  else
    echo "FAIL $s on a live site: exit $got, docker calls: $(cat "$work/log"): $out"; failures=$((failures + 1))
  fi
done

[ "$failures" = 0 ] || { echo "$failures failure(s)"; exit 1; }
echo "all live-site tests passed"

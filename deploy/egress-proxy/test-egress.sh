#!/usr/bin/env bash
# A15 (cloud models plan Task 14): the egress proxy allows CONNECT to port
# 443 of the listed hosts only, and a container on `backend` alone has no
# route out. Run on a Docker host (the build server), never in CI and never
# with a key: it starts only the proxy from deploy/compose.yml, under its
# own project name, with api.anthropic.com as the one allowed host, and
# probes it with raw CONNECT lines. The one allowed tunnel is opened and
# closed at once: nothing is sent through it, so no provider API is called.
#
#   bash deploy/egress-proxy/test-egress.sh
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
deploy=$(dirname "$here")
project=mhai-egress-test-$$
work=$(mktemp -d)
image=$(sed -n 's/^    image: \(ubuntu\/squid[^ ]*\)$/\1/p' "$deploy/compose.yml")
[ -n "$image" ] || { echo "no egress-proxy image in compose.yml" >&2; exit 1; }

printf 'api.anthropic.com\n' > "$work/allowed-hosts"
chmod 0644 "$work/allowed-hosts"
chmod 0755 "$work"

# Compose reads the whole file, so the demo's required variables get
# synthetic values; none of those services is started.
compose() {
  env POSTGRES_PASSWORD=synthetic CORE_DB_PASSWORD=synthetic PROVISION_DB_PASSWORD=synthetic \
    NATS_GATEWAY_NKEY=synthetic NATS_BFF_NKEY=synthetic NATS_BFF_PLATFORM_NKEY=synthetic \
    MFA_ENCRYPTION_KEY=synthetic BFF_SERVICE_KEYS=synthetic \
    EGRESS_ALLOWED_HOSTS="$work/allowed-hosts" \
    docker compose -p "$project" -f "$deploy/compose.yml" --profile cloud "$@"
}
cleanup() {
  compose logs --no-log-prefix egress-proxy > "$work/proxy.log" 2>&1 || true
  compose down -v --remove-orphans >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT

compose up -d --no-deps egress-proxy >/dev/null
backend="${project}_backend"

# Probe from a container on backend only, as the pipeline would be.
probe() {
  docker run --rm --network "$backend" --user 65534:65534 --read-only --cap-drop ALL \
    --entrypoint bash "$image" -c "$1"
}

# Wait for Squid to listen.
for _ in $(seq 1 30); do
  if probe 'exec 3<>/dev/tcp/egress-proxy/3128' 2>/dev/null; then break; fi
  sleep 1
done

# The status code Squid answers a CONNECT with; the tunnel, if any, is
# closed without sending a byte through it.
connect_status() {
  probe "exec 3<>/dev/tcp/egress-proxy/3128 || exit 9
         printf 'CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n' '$1' '$1' >&3
         IFS=' ' read -r -t 20 _ code _ <&3 || exit 8
         echo \"\$code\"" 2>/dev/null || echo "none"
}

fail=0
expect() {
  local target=$1 want=$2 got
  got=$(connect_status "$target")
  # want 4xx: any refusal (Squid may call an odd authority a bad request).
  if [ "$got" = "$want" ] || { [ "$want" = 4xx ] && [[ "$got" =~ ^4[0-9][0-9]$ ]]; }; then
    echo "ok   CONNECT $target -> $got"
  else
    echo "FAIL CONNECT $target -> $got, want $want"
    fail=1
  fi
}

expect example.com:443 403
expect api.openai.com:443 403
expect api.anthropic.com:80 403
expect 1.2.3.4:443 403
expect '[2001:db8::1]:443' 403
expect 2001:db8::1:443 4xx
expect api.anthropic.com.evil.example:443 4xx
expect API.ANTHROPIC.COM:80 4xx
expect evil.api.anthropic.com:443 4xx
expect 16843009:443 4xx
expect localhost:443 4xx
expect api.anthropic.com:443 200

# Not CONNECT: a plain request through the proxy is refused too.
got=$(probe "exec 3<>/dev/tcp/egress-proxy/3128 || exit 9
             printf 'GET http://api.anthropic.com/ HTTP/1.1\r\nHost: api.anthropic.com\r\n\r\n' >&3
             IFS=' ' read -r -t 20 _ code _ <&3 || exit 8
             echo \"\$code\"" 2>/dev/null || echo none)
if [ "$got" = 403 ]; then echo "ok   GET http://api.anthropic.com/ -> 403"; else echo "FAIL GET -> $got, want 403"; fail=1; fi

# backend has no route out: a direct TCP connection fails.
if probe 'timeout 5 bash -c "exec 3<>/dev/tcp/1.1.1.1/443"' 2>/dev/null; then
  echo "FAIL a container on backend reached 1.1.1.1:443 directly"
  fail=1
else
  echo "ok   no direct route from backend to 1.1.1.1:443"
fi

# The access log holds method, host, port and status only.
compose logs --no-log-prefix egress-proxy 2>/dev/null | grep -E '^[0-9]+ (CONNECT|GET) ' > "$work/access.log" || true
if [ ! -s "$work/access.log" ] || grep -vqE '^[0-9]+ (CONNECT|GET) [^ ]+ [0-9-]* [0-9]{3}$' "$work/access.log"; then
  echo "FAIL access log lines aren't '<time> <method> <host> <port> <status>':"
  cat "$work/access.log"
  fail=1
else
  echo "ok   access log: time, method, host, port, status"
fi

if [ "$fail" -ne 0 ]; then
  echo "egress proxy checks FAILED"
  exit 1
fi
echo "egress proxy checks passed"

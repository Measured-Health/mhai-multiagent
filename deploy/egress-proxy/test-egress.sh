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

# localhost is listed too, only to show that an allowed name resolving to a
# private address is refused (private_dst).
printf 'api.anthropic.com\nlocalhost\n' > "$work/allowed-hosts"
# The real squid.conf plus DNS and IP-cache debugging (sections 14 and 78)
# on stderr, so the test can see every name Squid resolves. Nothing else
# differs: the override mounts it over /etc/squid/squid.conf.
cp "$here/squid.conf" "$work/squid.conf"
printf '\ndebug_options ALL,1 14,9 78,9\n' >> "$work/squid.conf"
cat > "$work/override.yml" <<YML
services:
  egress-proxy:
    volumes:
      - $work/squid.conf:/etc/squid/squid.conf:ro
YML
chmod 0644 "$work/allowed-hosts" "$work/squid.conf" "$work/override.yml"
chmod 0755 "$work"

# Compose reads the whole file, so the demo's required variables get
# synthetic values; none of those services is started.
compose() {
  env POSTGRES_PASSWORD=synthetic CORE_DB_PASSWORD=synthetic PROVISION_DB_PASSWORD=synthetic \
    NATS_GATEWAY_NKEY=synthetic NATS_BFF_NKEY=synthetic NATS_BFF_PLATFORM_NKEY=synthetic \
    MFA_ENCRYPTION_KEY=synthetic BFF_SERVICE_KEYS=synthetic \
    EGRESS_ALLOWED_HOSTS="$work/allowed-hosts" \
    docker compose -p "$project" -f "$deploy/compose.yml" -f "$work/override.yml" --profile cloud "$@"
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
expect localhost:443 403   # allowed by name, resolves to 127.0.0.1: private_dst
expect api.anthropic.com:443 200

# A name off the list is refused before any DNS lookup, so a compromised
# pipeline can't leak data in the names it asks for (PR E review). The
# allowed name above was resolved, which shows the debug log sees lookups.
nonce="x$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n').invalid"
expect "$nonce:443" 403
expect "$nonce:80" 403
# Squid's stderr only (its cache log); the access log on stdout names
# every host, refused or not. A name lookup is an ipcache or DNS (idns)
# entry; parsing a name as a numeric address is not a lookup.
ctr=$(docker ps -q --filter "label=com.docker.compose.project=$project" --filter "label=com.docker.compose.service=egress-proxy")
docker logs "$ctr" 2>"$work/cache.log" >/dev/null
dnslog() { grep -E 'ipcache_(nb)?gethostbyname|idns[A-Z]' "$work/cache.log"; }
if ! dnslog | grep -q 'api\.anthropic\.com'; then
  echo "FAIL the DNS debug log shows no lookup of the allowed name: the check below would prove nothing"
  fail=1
elif dnslog | grep -q "$nonce"; then
  echo "FAIL Squid looked up $nonce, a name off the list"
  fail=1
else
  echo "ok   a name off the list is refused with no DNS lookup"
fi

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

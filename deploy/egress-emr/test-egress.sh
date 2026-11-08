#!/usr/bin/env bash
# A6, A29, A37 end to end (EMR plan Task 15; design §6.4): the real
# egress-emr, on the real networks of deploy/compose.yml (profile emr),
# probed from a container on exactly the emr service's networks (backend
# and emr-proxy). Run on a Docker host (the build server), never in CI and
# never against a hospital: the one tunnel opened goes to example.com:443
# and is closed without sending a byte. Synthetic ids and a throwaway key.
#
#   EMR_IMAGE=<the emr image, built from services/emr/Dockerfile> \
#     bash deploy/egress-emr/test-egress.sh
#
# KEEP_FOR_DEBUG=1 leaves the project and its work directory up, and
# prints both, for a person to inspect and remove.
#
# Checks: per-connection Proxy-Authorization (407); only the connection's
# own hosts (403), refused before any DNS lookup (a sidecar captures the
# proxy's DNS traffic: a name off the list is never asked for); one lookup,
# and every address checked: metadata, another hospital's range, the
# backend network and the proxy's own network refused even when declared,
# the connection's own range allowed; a stale allow-list refuses all; the
# emr side has no direct route out; the log names no host.
set -euo pipefail
# Nothing here reads input: never take the caller's (docker and compose
# would otherwise consume it).
exec </dev/null

here=$(cd "$(dirname "$0")" && pwd)
deploy=$(dirname "$here")
image=${EMR_IMAGE:?set EMR_IMAGE to the emr image (docker build -f services/emr/Dockerfile services)}
probe_image=${PROBE_IMAGE:-alpine:3.20}
project=mhai-egress-emr-test-$$
work=$(mktemp -d)
chmod 0755 "$work"
mkdir -m 0755 "$work/conf"

# A throwaway K_proxy, readable by the proxy's user.
khex=$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')
printf '%s\n' "$khex" > "$work/k_proxy"
chmod 0444 "$work/k_proxy"

# Synthetic connections: A and A3 of hospital A, B of hospital B.
hosp_a=0b1f0000-0000-4000-8000-00000000000a
hosp_b=0b1f0000-0000-4000-8000-00000000000b
conn_a=c0110000-0000-4000-8000-00000000000a
conn_b=c0110000-0000-4000-8000-00000000000b
conn_a3=c0110000-0000-4000-8000-0000000000a3

token() { printf '%s' "$1" | openssl dgst -sha256 -mac HMAC -macopt "hexkey:$khex" -r | cut -d' ' -f1; }
basic() { printf 'Basic %s' "$(printf '%s:%s' "$1" "$2" | base64 -w0)"; }
auth_a=$(basic "$conn_a" "$(token "$conn_a")")
auth_b=$(basic "$conn_b" "$(token "$conn_b")")
auth_a3=$(basic "$conn_a3" "$(token "$conn_a3")")

# Compose reads the whole file, so the demo's required variables get
# synthetic values; none of those services is started.
compose() {
  env POSTGRES_PASSWORD=synthetic CORE_DB_PASSWORD=synthetic PROVISION_DB_PASSWORD=synthetic \
    NATS_GATEWAY_NKEY=synthetic NATS_BFF_NKEY=synthetic NATS_BFF_PLATFORM_NKEY=synthetic \
    MFA_ENCRYPTION_KEY=synthetic BFF_SERVICE_KEYS=synthetic EMR_IMAGE="$image" \
    docker compose -p "$project" -f "$deploy/compose.yml" -f "$work/override.yml" --profile emr "$@"
}
dnspid=""
cleanup() {
  if [ -n "${KEEP_FOR_DEBUG:-}" ]; then echo "kept: project $project, work $work"; return; fi
  [ -n "$dnspid" ] && docker rm -f "$project-dns" >/dev/null 2>&1 || true
  compose down -v --remove-orphans >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT

# Phase 1: the probe, on exactly the emr service's networks, so both exist
# and their subnets are known before the proxy starts.
probe_service() {
  cat <<YML
  emr-probe:
    image: $probe_image
    profiles: ["emr"]
    command: ["sleep", "900"]
    user: "65534:65534"
    read_only: true
    cap_drop: [ALL]
    networks: [backend, emr-proxy]
YML
}
{ echo "services:"; probe_service; } > "$work/override.yml"
compose up -d emr-probe >/dev/null
subnet() { docker network inspect -f '{{range .IPAM.Config}}{{.Subnet}} {{end}}' "${project}_$1" | tr ' ' '\n' | grep -m1 '\.'; }
backend_net=$(subnet backend)
proxy_net=$(subnet emr-proxy)
# The first /24 of each, and an address in it.
first24() { echo "$1" | awk -F'[./]' '{print $1"."$2"."$3".0/24"}'; }
addr7() { echo "$1" | awk -F'[./]' '{print $1"."$2"."$3".7"}'; }
backend24=$(first24 "$backend_net"); proxy24=$(first24 "$proxy_net")
echo "backend $backend_net, emr-proxy $proxy_net"

# Phase 2: the proxy, with the test's allow-list directory and key, and
# /etc/hosts names for the private-address cases (the Go resolver reads it
# first; public names go to DNS).
cat > "$work/override.yml" <<YML
services:
$(probe_service)
  egress-emr:
    volumes: !override
      - $work/conf:/emr-egress:ro
      - $work/k_proxy:/run/secrets/emr-proxy.key:ro
    environment:
      EGRESS_EMR_INTERNAL_CIDRS: "$backend_net"
    extra_hosts:
      - "inside.hospital-a.example:10.99.1.5"
      - "vpn-b.hospital-a.example:10.99.2.5"
      - "meta.hospital-a.example:169.254.169.254"
      - "internal.hospital-a.example:$(addr7 "$backend_net")"
      - "proxynet.hospital-a.example:$(addr7 "$proxy_net")"
YML

write_list() { # $1: generated_at
  cat > "$work/conf/allow.json.tmp" <<JSON
{"generated_at":"$1","connections":[
 {"id":"$conn_a","hospital_id":"$hosp_a","hosts":["example.com:443","inside.hospital-a.example:443","vpn-b.hospital-a.example:443","meta.hospital-a.example:443"],"cidrs":["10.99.1.0/24"]},
 {"id":"$conn_a3","hospital_id":"$hosp_a","hosts":["internal.hospital-a.example:443","proxynet.hospital-a.example:443"],"cidrs":["$backend24","$proxy24"]},
 {"id":"$conn_b","hospital_id":"$hosp_b","hosts":["fhir.hospital-b.example:443"],"cidrs":["10.99.2.0/24"]}]}
JSON
  chmod 0644 "$work/conf/allow.json.tmp"
  mv "$work/conf/allow.json.tmp" "$work/conf/allow.json"
  sleep 2 # the proxy reloads every second
}
write_list "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
compose up -d --no-deps egress-emr >/dev/null
ctr=$(compose ps -q egress-emr)

# The DNS capture, in the proxy's own network namespace.
docker run -d --name "$project-dns" --network "container:$ctr" "$probe_image" \
  sh -c 'apk add --no-cache -q tcpdump >/dev/null 2>&1 && exec tcpdump -i any -l -n -A udp 2>&1' >/dev/null
dnspid=1
for _ in $(seq 1 60); do
  docker logs "$project-dns" 2>&1 | grep -q 'listening on' && break
  sleep 1
done
docker logs "$project-dns" 2>&1 | grep -q 'listening on' || { echo "FAIL the DNS capture didn't start"; exit 1; }

# Wait for the proxy to listen.
for _ in $(seq 1 30); do
  compose exec -T emr-probe sh -c 'nc -z -w 2 egress-emr 3128' 2>/dev/null && break
  sleep 1
done

# The status the proxy answers; a tunnel is closed without a byte sent.
status() { # $1 method, $2 target, $3 Proxy-Authorization ('' for none), $4 Host (default: $2)
  local hdr=""
  [ -n "$3" ] && hdr="Proxy-Authorization: $3\r\n"
  compose exec -T emr-probe sh -c "printf '$1 $2 HTTP/1.1\r\nHost: ${4:-$2}\r\n$hdr\r\n' | timeout 20 nc -w 15 egress-emr 3128 | head -n1 | cut -d' ' -f2" 2>/dev/null || true
}

fail=0
expect() { # $1 target, $2 auth, $3 want, $4 why
  local got
  got=$(status CONNECT "$1" "$2")
  if [ "$got" = "$3" ]; then echo "ok   CONNECT $1 -> $got ($4)"; else echo "FAIL CONNECT $1 -> ${got:-none}, want $3 ($4)"; fail=1; fi
}
nonce="x$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')"

expect example.com:443 "" 407 "no Proxy-Authorization"
expect example.com:443 "$(basic "$conn_a" "$(printf '0%.0s' $(seq 1 64))")" 407 "forged token"
expect example.com:443 "$(basic "$conn_a" "$(token "$conn_b")")" 407 "A's id with B's token"
expect fhir.hospital-b.example:443 "$auth_a" 403 "A37: B's host with A's connection"
expect example.com:443 "$auth_b" 403 "A37: A's host with B's connection"
expect www.iana.org:443 "$auth_a" 403 "A6: a name off the list"
expect "$nonce.invalid:443" "$auth_a" 403 "A6: a name off the list, never looked up"
expect "$nonce.example.com:443" "$auth_a" 403 "A6: a subdomain of a listed name"
expect example.com:80 "$auth_a" 403 "port 80"
expect 169.254.169.254:443 "$auth_a" 403 "an IP literal"
expect meta.hospital-a.example:443 "$auth_a" 403 "A29: a listed name answering the metadata address"
expect vpn-b.hospital-a.example:443 "$auth_a" 403 "A37: a listed name answering inside B's range"
expect internal.hospital-a.example:443 "$auth_a3" 403 "the backend network, though declared"
expect proxynet.hospital-a.example:443 "$auth_a3" 403 "the proxy's own network, though declared"
expect inside.hospital-a.example:443 "$auth_a" 502 "A's own declared range: checked, dialled, nothing there"
expect inside.hospital-a.example:443 "$auth_a3" 403 "A37: A's host with A3's connection, same hospital"
expect example.com:443 "$auth_a" 200 "A's listed public host"

got=$(status GET http://example.com/ "$auth_a" example.com)
if [ "$got" = 405 ]; then echo "ok   GET through the proxy -> 405"; else echo "FAIL GET -> ${got:-none}, want 405"; fail=1; fi

# The capture saw the allowed name looked up, and never the nonce.
sleep 1
dns=$(docker logs "$project-dns" 2>&1)
if ! printf '%s' "$dns" | grep -q 'example'; then
  echo "FAIL the DNS capture shows no lookup of the allowed name: the check below would prove nothing"
  fail=1
elif printf '%s' "$dns" | grep -q "$nonce"; then
  echo "FAIL the proxy looked up $nonce, a name off the list"
  fail=1
elif printf '%s' "$dns" | grep -q 'iana'; then
  echo "FAIL the proxy looked up www.iana.org, a name off the list"
  fail=1
else
  echo "ok   names off the list are refused with no DNS lookup (the allowed one was looked up)"
fi

# K5: a stale allow-list refuses everything.
write_list "$(date -u -d '-200 seconds' +%Y-%m-%dT%H:%M:%SZ)"
expect example.com:443 "$auth_a" 503 "K5: a stale allow-list"
rm -f "$work/conf/allow.json"
sleep 2
expect example.com:443 "$auth_a" 503 "K5: no allow-list"

# The emr side has no route out but the proxy.
for dst in "1.1.1.1 443" "93.184.215.14 443" "8.8.8.8 53"; do
  if compose exec -T emr-probe sh -c "nc -z -w 5 $dst" 2>/dev/null; then
    echo "FAIL a container on emr's networks reached $dst directly"; fail=1
  else
    echo "ok   no direct route from emr's networks to $dst"
  fi
done

# The log: one JSON line per CONNECT, ids, codes and a host hash; never a
# host, an address or a credential.
logs=$(compose logs --no-log-prefix egress-emr 2>/dev/null)
leak=0
for s in example.com iana hospital-a hospital-b "$nonce" 169.254 10.99. "$(token "$conn_a")" "$khex" Basic; do
  if printf '%s' "$logs" | grep -qF "$s"; then echo "FAIL the proxy log carries $s"; leak=1; fi
done
if [ "$leak" = 0 ] && printf '%s' "$logs" | grep -q '"decision":"not_listed"' && printf '%s' "$logs" | grep -q '"host_hash"'; then
  echo "ok   the proxy log: decision codes and host hashes, no host or credential"
else
  [ "$leak" = 0 ] && echo "FAIL the proxy log lacks its decision lines"
  fail=1
fi

if [ "$fail" -ne 0 ]; then
  # The proxy's own lines carry ids, codes and hashes only.
  printf '%s\n' "$logs" | tail -n 30
  echo "egress-emr checks FAILED"
  exit 1
fi
echo "egress-emr checks passed"

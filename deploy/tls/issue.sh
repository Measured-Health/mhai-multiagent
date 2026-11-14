#!/usr/bin/env bash
# Issue one certificate from the split mode's private CA (make-ca.sh;
# docs/deploy/remote-intelligence.md). ECDSA P-256, 365 days, not a CA, one
# use only (server or client), the SANs given here.
#
#   bash deploy/tls/issue.sh --ca-dir CA --out DIR --name NAME --kind server|client
#        [--dns a,b] [--ip 10.0.0.1,...] [--csr FILE] [--days N] [--renew]
#
# Writes DIR/NAME.pem (0644) and, without --csr, a new key DIR/NAME-key.pem
# (0600). With --csr (a request made on another host, which keeps its key) it
# signs the request's key only: the CSR's subject must be CN=NAME, and the
# SANs and the use come from these arguments, never from the CSR. A client
# certificate always carries DNS:NAME. Refuses to replace a certificate
# without --renew. Never prints a key: only the paths, the fingerprint and
# the expiry date.
set -euo pipefail
umask 077

die() { printf 'issue: %s\n' "$*" >&2; exit 1; }
usage() {
  echo "usage: bash issue.sh --ca-dir CA --out DIR --name NAME --kind server|client" >&2
  echo "         [--dns a,b] [--ip 10.0.0.1,...] [--csr FILE] [--days N] [--renew]" >&2
  exit 2
}

CA='' OUT='' NAME='' KIND='' DNS='' IPS='' CSR='' DAYS=365 RENEW=0
while [ $# -gt 0 ]; do
  case "$1" in
    --ca-dir) [ $# -ge 2 ] || usage; CA=$2; shift 2 ;;
    --out) [ $# -ge 2 ] || usage; OUT=$2; shift 2 ;;
    --name) [ $# -ge 2 ] || usage; NAME=$2; shift 2 ;;
    --kind) [ $# -ge 2 ] || usage; KIND=$2; shift 2 ;;
    --dns) [ $# -ge 2 ] || usage; DNS=$2; shift 2 ;;
    --ip) [ $# -ge 2 ] || usage; IPS=$2; shift 2 ;;
    --csr) [ $# -ge 2 ] || usage; CSR=$2; shift 2 ;;
    --days) [ $# -ge 2 ] || usage; DAYS=$2; shift 2 ;;
    --renew) RENEW=1; shift ;;
    *) usage ;;
  esac
done

command -v openssl >/dev/null || die "openssl is not installed"
[[ $NAME =~ ^[a-z][a-z0-9-]{0,62}$ ]] || die "--name must be lowercase letters, digits and - (a DNS label)"
case "$KIND" in
  server) EKU=serverAuth ;;
  client) EKU=clientAuth ;;
  *) die "--kind must be server or client" ;;
esac
if ! [[ $DAYS =~ ^[1-9][0-9]{0,2}$ ]] || [ "$DAYS" -gt 825 ]; then die "--days must be 1 to 825"; fi
if [ ! -f "$CA/ca.pem" ] || [ ! -f "$CA/ca-key.pem" ]; then
  die "--ca-dir has no ca.pem and ca-key.pem (make-ca.sh)"
fi
[ -n "$OUT" ] || die "--out is required"

label='[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?'
sans=()
if [ -n "$DNS" ]; then
  IFS=, read -r -a dns <<<"$DNS"
  for d in "${dns[@]}"; do
    [[ $d =~ ^$label(\.$label)*$ ]] || die "--dns: not a DNS name"
    sans+=("DNS:$d")
  done
fi
if [ "$KIND" = client ] && [[ " ${sans[*]-} " != *" DNS:$NAME "* ]]; then sans+=("DNS:$NAME"); fi
if [ -n "$IPS" ]; then
  IFS=, read -r -a ips <<<"$IPS"
  for a in "${ips[@]}"; do
    [[ $a =~ ^([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})$ ]] || die "--ip: not an IPv4 address"
    for o in "${BASH_REMATCH[@]:1}"; do [ "$((10#$o))" -le 255 ] || die "--ip: not an IPv4 address"; done
    sans+=("IP:$a")
  done
fi
[ "${#sans[@]}" -gt 0 ] || die "a server certificate needs --dns or --ip"
san=$(IFS=,; printf '%s' "${sans[*]}")

if [ -e "$OUT/$NAME.pem" ] && [ "$RENEW" = 0 ]; then
  die "$OUT/$NAME.pem exists: --renew to replace it (nothing changed)"
fi
mkdir -p "$OUT"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

if [ -n "$CSR" ]; then
  [ -f "$CSR" ] || die "--csr: no such file"
  openssl req -in "$CSR" -noout -verify >/dev/null 2>&1 || die "--csr: the request's signature does not verify"
  cn=$(openssl req -in "$CSR" -noout -subject -nameopt multiline | sed -n 's/^ *commonName *= *//p')
  [ "$cn" = "$NAME" ] || die "--csr: the request is for another name (CN must be $NAME)"
  cp "$CSR" "$tmp/req.csr"
else
  openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$tmp/key.pem" 2>/dev/null
  openssl req -new -key "$tmp/key.pem" -subj "/CN=$NAME" -out "$tmp/req.csr" 2>/dev/null
fi

cat >"$tmp/ext.cnf" <<CNF
[v3]
basicConstraints = critical,CA:FALSE
keyUsage = critical,digitalSignature
extendedKeyUsage = $EKU
subjectAltName = $san
subjectKeyIdentifier = hash
authorityKeyIdentifier = keyid
CNF
openssl x509 -req -in "$tmp/req.csr" -CA "$CA/ca.pem" -CAkey "$CA/ca-key.pem" \
  -set_serial "0x$(openssl rand -hex 16)" -days "$DAYS" -sha256 \
  -extfile "$tmp/ext.cnf" -extensions v3 -out "$tmp/cert.pem" 2>/dev/null \
  || die "signing failed"
openssl verify -CAfile "$CA/ca.pem" "$tmp/cert.pem" >/dev/null || die "the new certificate does not verify"

chmod 644 "$tmp/cert.pem"
if [ -f "$tmp/key.pem" ]; then
  chmod 600 "$tmp/key.pem"
  mv -f "$tmp/key.pem" "$OUT/$NAME-key.pem"
fi
mv -f "$tmp/cert.pem" "$OUT/$NAME.pem"
echo "issued $OUT/$NAME.pem ($KIND; $san)"
openssl x509 -in "$OUT/$NAME.pem" -noout -fingerprint -sha256 -enddate

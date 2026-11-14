#!/usr/bin/env bash
# Tests for make-ca.sh and issue.sh (docs/deploy/remote-intelligence.md): no
# Docker, openssl only. Every key and certificate is made here, in a temporary
# folder, and removed at the end. Run: bash deploy/tls/test-tls.sh
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
fail=0
ok()   { echo "ok   $1"; }
bad()  { echo "FAIL $1"; fail=1; }
check() { local name=$1; shift; if "$@"; then ok "$name"; else bad "$name"; fi; }
mode() { stat -c %a "$1"; }
no_key_in() { ! grep -q 'PRIVATE KEY' "$1"; }

ca=$tmp/ca
bash "$here/make-ca.sh" "$ca" >"$tmp/out" 2>&1 || { cat "$tmp/out"; bad "make-ca runs"; }
check "make-ca: the folder is 0700" test "$(mode "$ca")" = 700
check "make-ca: the CA key is 0600" test "$(mode "$ca/ca-key.pem")" = 600
check "make-ca: the CA certificate is 0644" test "$(mode "$ca/ca.pem")" = 644
check "make-ca: prints no key" no_key_in "$tmp/out"
check "make-ca: a CA certificate" bash -c "openssl x509 -in '$ca/ca.pem' -noout -ext basicConstraints | grep -q 'CA:TRUE'"
if bash "$here/make-ca.sh" "$ca" >/dev/null 2>&1; then bad "make-ca: refuses to replace a CA"; else ok "make-ca: refuses to replace a CA"; fi

out=$tmp/issued
bash "$here/issue.sh" --ca-dir "$ca" --out "$out" --name nats --kind server \
  --dns nats,mhai-nats,mhai-app --ip 10.0.1.10 >"$tmp/out" 2>&1 || { cat "$tmp/out"; bad "issue server runs"; }
check "issue: the key is 0600" test "$(mode "$out/nats-key.pem")" = 600
check "issue: prints no key" no_key_in "$tmp/out"
check "issue: the chain verifies" openssl verify -CAfile "$ca/ca.pem" -purpose sslserver "$out/nats.pem"
check "issue: the SANs" bash -c "openssl x509 -in '$out/nats.pem' -noout -ext subjectAltName | tr -d ' ' | grep -q 'DNS:nats,DNS:mhai-nats,DNS:mhai-app,IPAddress:10.0.1.10'"
check "issue: server use only" bash -c "openssl x509 -in '$out/nats.pem' -noout -ext extendedKeyUsage | grep -q 'TLS Web Server Authentication' && ! openssl x509 -in '$out/nats.pem' -noout -ext extendedKeyUsage | grep -q Client"
check "issue: not a CA" bash -c "openssl x509 -in '$out/nats.pem' -noout -ext basicConstraints | grep -q 'CA:FALSE'"
check "issue: the key matches" test "$(openssl x509 -in "$out/nats.pem" -noout -pubkey)" = "$(openssl pkey -in "$out/nats-key.pem" -pubout)"
if bash "$here/issue.sh" --ca-dir "$ca" --out "$out" --name nats --kind server --dns nats >/dev/null 2>&1; then
  bad "issue: refuses to replace without --renew"
else ok "issue: refuses to replace without --renew"; fi
if bash "$here/issue.sh" --ca-dir "$ca" --out "$out" --name nats --kind server --dns nats --renew >/dev/null 2>&1; then
  ok "issue: --renew replaces"
else bad "issue: --renew replaces"; fi

bash "$here/issue.sh" --ca-dir "$ca" --out "$out" --name core --kind client >/dev/null 2>&1 || bad "issue client runs"
check "issue: client use" openssl verify -CAfile "$ca/ca.pem" -purpose sslclient "$out/core.pem"

# A CSR made elsewhere (the intelligence host): only the certificate comes back.
openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$tmp/p.key" 2>/dev/null
openssl req -new -key "$tmp/p.key" -subj /CN=pipeline -out "$tmp/p.csr" 2>/dev/null
bash "$here/issue.sh" --ca-dir "$ca" --out "$out" --name pipeline --kind client --csr "$tmp/p.csr" >"$tmp/out" 2>&1 \
  || { cat "$tmp/out"; bad "issue: signs a CSR"; }
check "issue: CSR signed, no key written" bash -c "[ -f '$out/pipeline.pem' ] && [ ! -e '$out/pipeline-key.pem' ]"
check "issue: CSR certificate matches its key" test "$(openssl x509 -in "$out/pipeline.pem" -noout -pubkey)" = "$(openssl pkey -in "$tmp/p.key" -pubout)"
openssl req -new -key "$tmp/p.key" -subj /CN=core -out "$tmp/wrong.csr" 2>/dev/null
if bash "$here/issue.sh" --ca-dir "$ca" --out "$tmp/x" --name pipeline --kind client --csr "$tmp/wrong.csr" >/dev/null 2>&1; then
  bad "issue: refuses a CSR for another name"
else ok "issue: refuses a CSR for another name"; fi

for args in "--name ../x --kind client" "--name nats --kind server --dns 'a b'" "--name nats --kind server --ip 999.1.1.1" "--name nats --kind other"; do
  # shellcheck disable=SC2086
  if eval bash "$here/issue.sh" --ca-dir "$ca" --out "$tmp/y" $args >/dev/null 2>&1; then
    bad "issue: refuses $args"
  else ok "issue: refuses $args"; fi
done

if [ "$fail" != 0 ]; then echo "some failed"; exit 1; fi
echo "all passed"

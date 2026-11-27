#!/usr/bin/env bash
# Make a private CA: the split mode's (docs/deploy/remote-intelligence.md,
# decision D3) or production mode's (deploy/production/enable-production.sh):
# one ECDSA P-256 key and a self-signed CA certificate for 10 years, in a new
# folder (mode 0700):
#
#   bash deploy/tls/make-ca.sh DIR [LABEL]
#     DIR/ca-key.pem   the CA key, mode 0600. Never printed; keep DIR on an
#                      encrypted disk, off both hosts once the certificates
#                      are issued.
#     DIR/ca.pem       the CA certificate, mode 0644 (public).
#
# It refuses to touch a folder that already holds a CA. Prints the paths and
# the certificate's fingerprint only.
set -euo pipefail
umask 077

die() { printf 'make-ca: %s\n' "$*" >&2; exit 1; }
[ $# -ge 1 ] && [ $# -le 2 ] || { echo "usage: bash make-ca.sh DIR [LABEL]" >&2; exit 2; }
dir=$1
# LABEL names the CA in its subject: split (the default) or production.
label=${2:-split}
[[ $label =~ ^[a-z][a-z0-9-]{0,30}$ ]] || die "LABEL must be lowercase letters, digits and -"
command -v openssl >/dev/null || die "openssl is not installed"
if [ -e "$dir/ca-key.pem" ] || [ -e "$dir/ca.pem" ]; then
  die "$dir already holds a CA: nothing changed"
fi
mkdir -p "$dir"
chmod 700 "$dir"

tmp=$(mktemp -d "$dir/.new.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$tmp/ca-key.pem" 2>/dev/null
openssl req -new -x509 -key "$tmp/ca-key.pem" -sha256 -days 3650 \
  -subj "/O=Measured Health/CN=MHAI $label CA $(date -u +%Y-%m-%d)" \
  -addext "basicConstraints=critical,CA:TRUE,pathlen:0" \
  -addext "keyUsage=critical,keyCertSign,cRLSign" \
  -addext "subjectKeyIdentifier=hash" \
  -out "$tmp/ca.pem" 2>/dev/null
chmod 600 "$tmp/ca-key.pem"
chmod 644 "$tmp/ca.pem"
mv "$tmp/ca-key.pem" "$dir/ca-key.pem"
mv "$tmp/ca.pem" "$dir/ca.pem"

echo "CA made in $dir (ca-key.pem 0600, ca.pem 0644)"
openssl x509 -in "$dir/ca.pem" -noout -fingerprint -sha256 -enddate

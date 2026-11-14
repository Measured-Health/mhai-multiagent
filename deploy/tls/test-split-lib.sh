#!/usr/bin/env bash
# Tests for split-lib.sh's checks, and for the enable scripts refusing bad
# arguments before they touch anything. No Docker. Run: bash deploy/tls/test-split-lib.sh
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
# shellcheck source=deploy/tls/split-lib.sh
. "$here/split-lib.sh"
fail=0
yes_() { if "$@"; then echo "ok   $*"; else echo "FAIL $*"; fail=1; fi; }
no_()  { if "$@"; then echo "FAIL not $*"; fail=1; else echo "ok   not $*"; fi; }

for a in 10.0.1.10 172.16.0.5 172.31.255.254 192.168.1.20 100.64.0.9 100.127.1.1; do yes_ is_private_ipv4 "$a"; done
for a in 8.8.8.8 172.32.0.1 172.15.0.1 192.169.0.1 100.128.0.1 127.0.0.1 0.0.0.0 10.0.0.0 \
         10.0.0.255 10.0.0.256 010.0.0.1 10.0.0 10.0.0.1/32 "10.0.0.1 " "" ::1 mhai-app; do
  no_ is_private_ipv4 "$a"
done
yes_ is_nkey_public UBEBXXYDPNSDCIC4J52JS4FS4S4YKKXBTUT7TC3FKSKA54XNFUCS2PLV
for k in SUAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA \
         NBEBXXYDPNSDCIC4J52JS4FS4S4YKKXBTUT7TC3FKSKA54XNFUCS2PLV UBEBXX "UBEBXXYDPNSDCIC4J52JS4FS4S4YKKXBTUT7TC3FKSKA54XNFUCS2PL;"; do
  no_ is_nkey_public "$k"
done

# The enable scripts stop on bad arguments before any check of the host.
refuses() { # refuses SCRIPT ARGS...: exits non-zero and says why, touching nothing
  local d out
  d=$(mktemp -d)
  if out=$(MHAI_BACKEND_DIR="$d/b" MHAI_INTELLIGENCE_DIR="$d/i" bash "$@" </dev/null 2>&1); then
    echo "FAIL $* ran"; fail=1
  elif [ -n "$(ls -A "$d")" ]; then
    echo "FAIL $* touched files"; fail=1
  else
    echo "ok   refuses: ${out%%$'\n'*}"
  fi
  rm -rf "$d"
}
split=$here/../enable-split.sh
intel=$here/../intelligence-host/enable-intelligence-host.sh
nk=UBEBXXYDPNSDCIC4J52JS4FS4S4YKKXBTUT7TC3FKSKA54XNFUCS2PLV
refuses "$split" --app-addr 3.3.3.3 --intelligence-addr 10.0.1.20 --ca-dir /x --pipeline-nkey "$nk" --pipeline-csr /x
refuses "$split" --app-addr 10.0.1.10 --intelligence-addr 10.0.1.10 --ca-dir /x --pipeline-nkey "$nk" --pipeline-csr /x
refuses "$split" --app-addr 10.0.1.10 --intelligence-addr 10.0.1.20 --ca-dir /x --pipeline-nkey SUBAD --pipeline-csr /x
refuses "$split" --app-addr 10.0.1.10 --intelligence-addr 10.0.1.20 --ca-dir /x --pipeline-nkey "$nk" --pipeline-csr /x --cloud-provider other
refuses "$intel" up --app-addr 8.8.8.8 --ca /x --client-cert /x --trusted-keys /x
refuses "$intel" up --app-addr 10.0.1.10 --ca /x --client-cert /x --trusted-keys /x --provider other
refuses "$intel" up --app-addr 10.0.1.10 --ca /x --client-cert /x --trusted-keys /x --ownmodel --store-endpoint http://store
refuses "$intel" frobnicate

if [ "$fail" != 0 ]; then echo "some failed"; exit 1; fi
echo "all passed"

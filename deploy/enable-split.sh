#!/usr/bin/env bash
# The app host's half of the split mode (docs/deploy/remote-intelligence.md):
# NATS and Postgres with TLS, published on this host's private address for the
# intelligence host only. Never on the demo host. Run as the deploy user, after
# setup-host.sh and a first deploy, from a checkout of this repository at the
# deployed release:
#
#   bash deploy/enable-split.sh --app-addr 10.0.1.10 --intelligence-addr 10.0.1.20 \
#     --ca-dir ~/mhai-ca --pipeline-nkey U... --pipeline-csr pipeline.csr \
#     [--cloud-provider anthropic|gemini] [--renew]
#
# --pipeline-nkey and --pipeline-csr come from `enable-intelligence-host.sh init`
# on the intelligence host (its keys never leave it). --ca-dir holds the private
# CA (deploy/tls/make-ca.sh); bring it for this run only.
#
# What it does, each step checking first and keeping what is there:
#   1. issues, from the CA: the NATS and Postgres server certificates (SANs: the
#      service name, mhai-app, the app address), and client certificates for
#      core and both BFFs; signs the pipeline's CSR. Keys are 0400 (Postgres
#      0600), each owned by its service's uid, written in a container with no
#      network; nothing secret is printed. Kept unless --renew, or unless the
#      app address changed.
#   2. writes postgres/pg_hba.conf (render-hba.sh) for the intelligence address
#      and this host's Docker networks;
#   3. copies compose.split.yml and nats/tls.conf, and sets in .env:
#      COMPOSE_FILE=compose.yml:compose.split.yml, SPLIT_APP_ADDR,
#      NATS_PIPELINE_NKEY, INTELLIGENCE_DB_PASSWORD (generated once; core-migrate
#      makes the intelligence_app login from it), and CORE_CLOUD_PROVIDERS with
#      --cloud-provider;
#   4. recreates postgres and nats, brings core up (core-migrate runs), waits
#      for readyz;
#   5. puts what the intelligence host needs, all public, in split-out/:
#      ca.pem, pipeline.pem, request-keys.json; prints the firewall rules.
set -euo pipefail
umask 077

DEST=${MHAI_BACKEND_DIR:-/srv/mhai-backend}
SRC=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=deploy/tls/split-lib.sh
. "$SRC/tls/split-lib.sh"
HELPER_IMAGE=${NATS_BOX_IMAGE:-natsio/nats-box:0.19.0}
POSTGRES_UID=70 NATS_UID=1000 CORE_UID=65532 WEB_UID=1001

usage() {
  sed -n '7,10p' "$0" >&2
  exit 2
}

APP='' INTEL='' CA='' NKEY='' CSR='' CLOUD='' RENEW=0
while [ $# -gt 0 ]; do
  case "$1" in
    --app-addr) [ $# -ge 2 ] || usage; APP=$2; shift 2 ;;
    --intelligence-addr) [ $# -ge 2 ] || usage; INTEL=$2; shift 2 ;;
    --ca-dir) [ $# -ge 2 ] || usage; CA=$2; shift 2 ;;
    --pipeline-nkey) [ $# -ge 2 ] || usage; NKEY=$2; shift 2 ;;
    --pipeline-csr) [ $# -ge 2 ] || usage; CSR=$2; shift 2 ;;
    --cloud-provider) [ $# -ge 2 ] || usage; CLOUD=$2; shift 2 ;;
    --renew) RENEW=1; shift ;;
    *) usage ;;
  esac
done

# --- the arguments, checked before anything is touched ----------------------
is_private_ipv4 "$APP" || die "--app-addr must be this host's private IPv4 address (10/8, 172.16/12, 192.168/16 or 100.64/10): NATS and Postgres are never published on a public one"
is_private_ipv4 "$INTEL" || die "--intelligence-addr must be the intelligence host's private IPv4 address"
[ "$APP" != "$INTEL" ] || die "--app-addr and --intelligence-addr are the same address"
is_nkey_public "$NKEY" || die "--pipeline-nkey must be the pipeline's public NKey (U..., from enable-intelligence-host.sh init)"
case "$CLOUD" in ''|anthropic|gemini) ;; *) die "--cloud-provider must be anthropic or gemini" ;; esac

say "Prerequisites"
for bin in docker openssl curl jq; do command -v "$bin" >/dev/null || die "$bin is not installed"; done
docker compose version >/dev/null 2>&1 || die "docker compose (v2) is not available"
[ "$(id -u)" -ne 0 ] || die "run as the deploy user, not root"
if [ ! -f "$CA/ca.pem" ] || [ ! -f "$CA/ca-key.pem" ]; then
  die "--ca-dir has no ca.pem and ca-key.pem (deploy/tls/make-ca.sh)"
fi
[ -f "$CSR" ] || die "--pipeline-csr: no such file"
if [ ! -f "$DEST/.env" ] || [ ! -f "$DEST/secrets/nats-gateway.nk" ]; then
  die "$DEST is not set up (deploy/setup-host.sh, then a first deploy)"
fi
hostname -I 2>/dev/null | tr ' ' '\n' | grep -qx "$APP" || die "$APP is not an address of this host"
ENV_FILE="$DEST/.env"
env_get() { sed -n "s/^$1=//p" "$ENV_FILE" | tail -n1; }
env_set() {
  if [ "$(env_get "$1")" = "$2" ] && grep -q "^$1=" "$ENV_FILE"; then
    info "$1 unchanged"
  else
    sed -i "/^$1=/d" "$ENV_FILE"
    printf '%s=%s\n' "$1" "$2" >> "$ENV_FILE"
    info "$1 written"
  fi
}
case ",$(env_get COMPOSE_PROFILES)," in
  *,pilot,*) die "the single-host pilot pipeline is on here (COMPOSE_PROFILES has pilot): switch it off first (pilot/README.md, 'Switching off'); in split mode it runs on the intelligence host" ;;
esac
cd "$DEST"
docker network inspect mhai-backend_backend mhai-backend_edge >/dev/null 2>&1 \
  || die "the backend's Docker networks are not there: deploy once first (docker compose up -d core)"
info "ok"

say "Certificates from the CA (keys never printed)"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
iss() { bash "$SRC/tls/issue.sh" --ca-dir "$CA" "$@" | sed 's/^/    /'; }
issue_all=$RENEW
if [ ! -f "$DEST/split-out/nats.pem" ]; then
  issue_all=1
elif ! cert_has_ip "$DEST/split-out/nats.pem" "$APP"; then
  info "the app address changed: issuing again"; issue_all=1
fi
if [ "$issue_all" = 1 ]; then
  iss --out "$work/nats" --name server --kind server --dns nats,mhai-nats,mhai-app --ip "$APP"
  iss --out "$work/postgres" --name server --kind server --dns postgres,mhai-app --ip "$APP"
  iss --out "$work/core" --name core --kind client
  iss --out "$work/bff" --name bff-clinical --kind client
  iss --out "$work/bff" --name bff-platform --kind client
  for d in nats core bff; do cp "$CA/ca.pem" "$work/$d/ca.pem"; done
  place_files "$work/nats" "$DEST/tls/nats" "$NATS_UID" 400
  place_files "$work/postgres" "$DEST/tls/postgres" "$POSTGRES_UID" 600
  place_files "$work/core" "$DEST/tls/core" "$CORE_UID" 400
  place_files "$work/bff" "$DEST/secrets/bff/tls" "$WEB_UID" 400
  mkdir -p "$DEST/split-out"
  install -m 644 "$work/nats/server.pem" "$DEST/split-out/nats.pem"
  info "placed in tls/nats, tls/postgres, tls/core and secrets/bff/tls"
else
  info "certificates in place for $APP, kept (--renew to issue again)"
fi
mkdir -p "$DEST/split-out"
chmod 755 "$DEST/split-out"
if [ "$issue_all" = 1 ] || [ ! -f "$DEST/split-out/pipeline.pem" ] \
   || [ "$(openssl req -in "$CSR" -noout -pubkey)" != "$(openssl x509 -in "$DEST/split-out/pipeline.pem" -noout -pubkey)" ]; then
  iss --out "$work/pipeline" --name pipeline --kind client --csr "$CSR" --renew
  install -m 644 "$work/pipeline/pipeline.pem" "$DEST/split-out/pipeline.pem"
else
  info "pipeline.pem in place for this CSR, kept"
fi
install -m 644 "$CA/ca.pem" "$DEST/split-out/ca.pem"
report_expiry "$DEST/split-out/nats.pem" "server certificates"
report_expiry "$DEST/split-out/pipeline.pem" "pipeline.pem"

say "pg_hba.conf for $INTEL"
subnets=()
for n in mhai-backend_backend mhai-backend_edge; do
  subnets+=("$(docker network inspect -f '{{(index .IPAM.Config 0).Subnet}}' "$n")")
done
mkdir -p "$DEST/postgres"
chmod 755 "$DEST/postgres"
bash "$SRC/postgres/render-hba.sh" "$INTEL" "${subnets[@]}" > "$DEST/postgres/pg_hba.conf.new"
chmod 644 "$DEST/postgres/pg_hba.conf.new"
mv -f "$DEST/postgres/pg_hba.conf.new" "$DEST/postgres/pg_hba.conf"
info "intelligence_app over TLS from $INTEL/32 only; core on ${subnets[*]}"

say "Compose files and .env"
install -m 644 "$SRC/compose.split.yml" "$DEST/compose.split.yml"
install -m 644 "$SRC/nats/tls.conf" "$DEST/nats/tls.conf"
if [ -z "$(env_get INTELLIGENCE_DB_PASSWORD)" ]; then
  sed -i '/^INTELLIGENCE_DB_PASSWORD=/d' "$ENV_FILE"
  printf 'INTELLIGENCE_DB_PASSWORD=%s\n' "$(openssl rand -hex 24)" >> "$ENV_FILE"
  info "INTELLIGENCE_DB_PASSWORD generated (not shown)"
else
  info "INTELLIGENCE_DB_PASSWORD already set, kept"
fi
env_set NATS_PIPELINE_NKEY "$NKEY"
env_set SPLIT_APP_ADDR "$APP"
env_set COMPOSE_FILE compose.yml:compose.split.yml
[ -z "$CLOUD" ] || env_set CORE_CLOUD_PROVIDERS "$CLOUD"
docker compose config --quiet || die "compose.yml, compose.split.yml and .env don't validate"

say "Postgres, NATS and core with TLS"
docker compose up -d --force-recreate --no-deps postgres nats
if ! docker compose up -d core; then
  docker compose logs --tail=60 postgres nats core-migrate core
  die "core did not start"
fi
port=$(env_get CORE_HOST_PORT); port=${port:-8092}
ready=000
for _ in $(seq 1 24); do
  ready=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$port/api/v1/readyz" || echo 000)
  [ "$ready" != 200 ] || break
  sleep 5
done
[ "$ready" = 200 ] || { docker compose logs --tail=80 nats core; die "core is not ready (readyz=$ready)"; }
info "core ready over NATS TLS; core-migrate ensured the intelligence_app login"

say "For the intelligence host ($DEST/split-out, all public)"
docker run --rm --network none -v mhai-backend-trustedkeys:/t:ro --entrypoint cat "$HELPER_IMAGE" \
  /t/request-keys.json > "$DEST/split-out/request-keys.json"
chmod 644 "$DEST/split-out/request-keys.json"
info "ca.pem, pipeline.pem, request-keys.json"

say "Done. Before the intelligence host connects:"
cat <<EOF
    1. Cloud firewall: allow TCP 4222 and 5432 to $APP from $INTEL only.
    2. Docker bypasses ufw; on this host, add and persist:
         for p in 4222 5432; do
           sudo iptables -I DOCKER-USER -p tcp ! -s $INTEL \\
             -m conntrack --ctorigdst $APP --ctorigdstport "\$p" -j DROP
         done
    3. The frontend: NATS_URL=tls://mhai-nats:4222 and NATS_TLS_CA_FILE,
       NATS_TLS_CERT_FILE, NATS_TLS_KEY_FILE from $DEST/secrets/bff/tls/
       (until then the BFFs can't reach NATS: verify is on).
    4. Copy $DEST/split-out/* to the intelligence host, then run there
       (the database password piped from this host, never shown):
         ssh <app host> "sed -n 's/^INTELLIGENCE_DB_PASSWORD=//p' $ENV_FILE" | \\
           ssh <intelligence host> "bash <checkout>/deploy/intelligence-host/enable-intelligence-host.sh up \\
             --app-addr $APP --ca split-out/ca.pem --client-cert split-out/pipeline.pem \\
             --trusted-keys split-out/request-keys.json"
    5. Remove the CA directory from this host.
EOF

#!/usr/bin/env bash
# The intelligence host's half of the split mode (docs/deploy/remote-intelligence.md).
# Run as the deploy user, from a checkout of this repository at the release the
# app host runs. Two steps:
#
#   bash deploy/intelligence-host/enable-intelligence-host.sh init [--image IMAGE] [--renew]
#     Makes, on this host, the pipeline's NATS NKey seed and its TLS key (both
#     0400, owned by the image's user; neither ever leaves this host), and a
#     CSR. Prints the NKey's public key and the CSR's path: both public, for
#     enable-split.sh on the app host.
#
#   ... | bash deploy/intelligence-host/enable-intelligence-host.sh up \
#     --app-addr 10.0.1.10 --ca ca.pem --client-cert pipeline.pem \
#     --trusted-keys request-keys.json [--gpu] [--image IMAGE] [--replace-db-password]
#     [--provider anthropic|gemini --zdr-ref REF [--model ID] [--gemini-paid-tier verified]]
#     [--ownmodel --store-endpoint https://HOST --store-bucket NAME [--store-region R]
#      [--mem 8g] [--threads 8] [--ctx 4096]]
#     Reads the intelligence_app password from standard input (piped from the
#     app host, or a hidden prompt) unless it is already in place; checks the
#     certificate (our CA, client use, CN pipeline, the key made by init);
#     installs it with the CA and the trusted keys; writes .env; starts
#     app-link and the pipeline (and the egress proxy, llama and model-fetch
#     when asked) and waits for the worker. The cloud key and the store
#     reader's keys go in first with put-secret.sh.
#
# Safe to run again: every step checks what is there and keeps it.
set -euo pipefail
umask 077

DEST=${MHAI_INTELLIGENCE_DIR:-/srv/mhai-intelligence}
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
SRC=$(cd "$HERE/.." && pwd)
REPO=$(cd "$SRC/.." && pwd)
# shellcheck source=deploy/tls/split-lib.sh
. "$SRC/tls/split-lib.sh"
HELPER_IMAGE=${NATS_BOX_IMAGE:-natsio/nats-box:0.19.0}
DEFAULT_IMAGE=ghcr.io/measured-health/mhai-multiagent-pipeline-pilot:latest

usage() { sed -n '6,20p' "$0" >&2; exit 2; }

CMD=${1:-}
[ $# -gt 0 ] && shift
case "$CMD" in init|up) ;; *) usage ;; esac

APP='' CA='' CERT='' TRUSTED='' GPU=0 IMAGE='' RENEW=0 REPLACE_PW=0
PROVIDER='' ZDR='' MODEL='' GEMINI_TIER=''
OWN=0 ENDPOINT='' BUCKET='' REGION=us-east-1 MEM=8g THREADS=8 CTX=4096
while [ $# -gt 0 ]; do
  case "$1" in
    --app-addr) [ $# -ge 2 ] || usage; APP=$2; shift 2 ;;
    --ca) [ $# -ge 2 ] || usage; CA=$2; shift 2 ;;
    --client-cert) [ $# -ge 2 ] || usage; CERT=$2; shift 2 ;;
    --trusted-keys) [ $# -ge 2 ] || usage; TRUSTED=$2; shift 2 ;;
    --image) [ $# -ge 2 ] || usage; IMAGE=$2; shift 2 ;;
    --gpu) GPU=1; shift ;;
    --renew) RENEW=1; shift ;;
    --replace-db-password) REPLACE_PW=1; shift ;;
    --provider) [ $# -ge 2 ] || usage; PROVIDER=$2; shift 2 ;;
    --zdr-ref) [ $# -ge 2 ] || usage; ZDR=$2; shift 2 ;;
    --model) [ $# -ge 2 ] || usage; MODEL=$2; shift 2 ;;
    --gemini-paid-tier) [ $# -ge 2 ] || usage; GEMINI_TIER=$2; shift 2 ;;
    --ownmodel) OWN=1; shift ;;
    --store-endpoint) [ $# -ge 2 ] || usage; ENDPOINT=$2; shift 2 ;;
    --store-bucket) [ $# -ge 2 ] || usage; BUCKET=$2; shift 2 ;;
    --store-region) [ $# -ge 2 ] || usage; REGION=$2; shift 2 ;;
    --mem) [ $# -ge 2 ] || usage; MEM=$2; shift 2 ;;
    --threads) [ $# -ge 2 ] || usage; THREADS=$2; shift 2 ;;
    --ctx) [ $# -ge 2 ] || usage; CTX=$2; shift 2 ;;
    *) usage ;;
  esac
done

# --- the arguments, checked before anything is touched ----------------------
if [ "$CMD" = up ]; then
  is_private_ipv4 "$APP" || die "--app-addr must be the app host's private IPv4 address (10/8, 172.16/12, 192.168/16 or 100.64/10)"
  case "$PROVIDER" in
    '') [ -z "$ZDR$MODEL$GEMINI_TIER" ] || die "--zdr-ref, --model and --gemini-paid-tier need --provider" ;;
    anthropic) MODEL=${MODEL:-claude-opus-5-5} ;;
    gemini) [ -n "$MODEL" ] || die "--model is required for gemini"
      [ "$GEMINI_TIER" = verified ] || die "--gemini-paid-tier verified is required for gemini (pilot/README.md)" ;;
    *) die "--provider must be anthropic or gemini" ;;
  esac
  if [ -n "$PROVIDER" ]; then
    printf '%s' "$ZDR" | grep -Eq '^[A-Z0-9][A-Z0-9_.-]{2,63}$' || die "--zdr-ref must be 3 to 64 of A-Z 0-9 _ . -"
    [ "$ZDR" != DEMO-SYNTHETIC ] || die "--zdr-ref DEMO-SYNTHETIC is for the synthetic demo only"
    printf '%s' "$MODEL" | grep -Eq '^[A-Za-z0-9._:-]{1,64}$' || die "--model must be 1 to 64 of A-Z a-z 0-9 . _ : -"
  fi
  if [ "$OWN" = 1 ]; then
    [[ $ENDPOINT =~ ^https://[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?(:[0-9]{1,5})?$ ]] \
      || die "--store-endpoint must be https://<host>[:port]"
    [[ $BUCKET =~ ^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$ ]] || die "--store-bucket must be an S3 bucket name"
    [[ $REGION =~ ^[a-z0-9-]{1,32}$ ]] || die "--store-region must be a region name"
    [[ $MEM =~ ^[1-9][0-9]{0,5}[mMgG]$ ]] || die "--mem must be a size such as 8g"
    [[ $THREADS =~ ^[1-9][0-9]?$ ]] || die "--threads must be 1 to 99"
    [[ $CTX =~ ^[1-9][0-9]{2,5}$ ]] || die "--ctx must be 100 to 999999"
  fi
  for f in "$CA" "$CERT" "$TRUSTED"; do [ -f "$f" ] || die "--ca, --client-cert and --trusted-keys must be files"; done
fi

say "Prerequisites"
for bin in docker openssl; do command -v "$bin" >/dev/null || die "$bin is not installed"; done
docker compose version >/dev/null 2>&1 || die "docker compose (v2) is not available"
[ "$(id -u)" -ne 0 ] || die "run as the deploy user, not root"
if [ ! -d "$DEST" ]; then
  mkdir -p "$DEST" 2>/dev/null || die "can't create $DEST: sudo install -d -o $(id -un) -m 750 $DEST, then run again"
fi
chmod 750 "$DEST"
mkdir -p "$DEST/secrets" "$DEST/tls" "$DEST/trusted"
chmod 700 "$DEST/secrets"
chmod 755 "$DEST/tls" "$DEST/trusted"
ENV_FILE="$DEST/.env"
[ -f "$ENV_FILE" ] || { : > "$ENV_FILE"; chmod 600 "$ENV_FILE"; }
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
env_unset() { if grep -q "^$1=" "$ENV_FILE"; then sed -i "/^$1=/d" "$ENV_FILE"; info "$1 removed"; fi; }
! grep -q '^INTELLIGENCE_SINGLE_HOST=' "$ENV_FILE" \
  || die "$ENV_FILE sets INTELLIGENCE_SINGLE_HOST: never on this host (remove it)"

IMAGE=${IMAGE:-$(env_get PIPELINE_IMAGE)}
IMAGE=${IMAGE:-$DEFAULT_IMAGE}
if ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
  docker pull -q "$IMAGE" >/dev/null 2>&1 \
    || die "could not pull $IMAGE ('docker login ghcr.io' with a read:packages token on stdin; an amd64 host needs the multi-arch tag from the 'Pilot pipeline image' workflow (arch both) or 'latest-amd64')"
fi
arch=$(docker image inspect -f '{{.Architecture}}' "$IMAGE")
case "$(uname -m)/$arch" in aarch64/arm64|x86_64/amd64) ;; *) die "$IMAGE is $arch, this host is $(uname -m)" ;; esac
APP_UID=$(docker run --rm --network none --entrypoint id "$IMAGE" -u)
case "$APP_UID" in ''|0|*[!0-9]*) die "the image's user is not a non-root uid" ;; esac
env_set PIPELINE_IMAGE "$IMAGE"
env_set PIPELINE_UID "$APP_UID"

# place_secret NAME: stdin -> secrets/NAME, 0400, owned by the image's user.
place_secret() {
  docker run --rm -i --network none --user 0 --entrypoint sh -e F="$1" -e U="$APP_UID" \
    -v "$DEST/secrets:/s" "$HELPER_IMAGE" -c '
      set -eu; umask 077
      t="/s/.$F.new"; trap "rm -f $t" EXIT
      cat > "$t"; chown "$U:$U" "$t"; chmod 400 "$t"; mv -f "$t" "/s/$F"'
}
owned_ok() { [ "$(stat -c '%u %a' "$DEST/$1" 2>/dev/null)" = "$APP_UID 400" ]; }

if [ "$CMD" = init ]; then
  say "The pipeline's NATS NKey (made here; the seed never leaves this host)"
  pub=$(docker run --rm --network none --user 0 --entrypoint sh -e U="$APP_UID" \
    -v "$DEST/secrets:/s" "$HELPER_IMAGE" -c '
      set -eu; umask 077
      if [ ! -s /s/nats-pipeline.nk ]; then
        out=$(nsc generate nkey --user 2>/dev/null); printf "%s\n" "$out" | sed -n 1p > /s/nats-pipeline.nk; unset out
        echo "generated" >&2
      else echo "exists, kept" >&2; fi
      case "$(head -c 2 /s/nats-pipeline.nk)" in SU) ;; *) echo "not a user seed" >&2; exit 1;; esac
      chown "$U:$U" /s/nats-pipeline.nk; chmod 400 /s/nats-pipeline.nk
      nats auth nkey show /s/nats-pipeline.nk' 2> >(sed 's/^/    /' >&2))
  is_nkey_public "$pub" || die "could not read the pipeline's public key"

  say "The pipeline's TLS key and CSR (the key never leaves this host)"
  if [ -f "$DEST/pipeline.csr" ] && [ "$RENEW" = 0 ]; then
    info "key and CSR exist, kept (--renew for a new pair)"
  else
    work=$(mktemp -d)
    trap 'rm -rf "$work"' EXIT
    openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$work/pipeline-key.pem" 2>/dev/null
    openssl req -new -key "$work/pipeline-key.pem" -subj /CN=pipeline -out "$work/pipeline.csr" 2>/dev/null
    place_secret pipeline-key.pem < "$work/pipeline-key.pem"
    docker run --rm --network none --user 0 --entrypoint sh -v "$DEST/secrets:/s" -v "$DEST/tls:/t" "$HELPER_IMAGE" \
      -c 'mv -f /s/pipeline-key.pem /t/pipeline-key.pem'
    install -m 644 "$work/pipeline.csr" "$DEST/pipeline.csr"
    rm -rf "$work"
    info "key made (0400, uid $APP_UID); a new key needs a new certificate (enable-split.sh)"
  fi
  owned_ok tls/pipeline-key.pem || die "tls/pipeline-key.pem has the wrong owner or mode (init --renew)"

  say "Done. Copy these two (public) to the app host for enable-split.sh:"
  info "--pipeline-nkey $pub"
  info "--pipeline-csr $DEST/pipeline.csr"
  exit 0
fi

# --- up ------------------------------------------------------------------------
if [ ! -f "$DEST/pipeline.csr" ] || ! owned_ok secrets/nats-pipeline.nk || ! owned_ok tls/pipeline-key.pem; then
  die "run init first (the NKey, the TLS key and the CSR)"
fi

say "The certificate"
openssl verify -CAfile "$CA" -purpose sslclient "$CERT" >/dev/null 2>&1 \
  || die "--client-cert is not a client certificate from --ca"
cn=$(openssl x509 -in "$CERT" -noout -subject -nameopt multiline | sed -n 's/^ *commonName *= *//p')
[ "$cn" = pipeline ] || die "--client-cert is not the pipeline's (CN $cn)"
[ "$(openssl x509 -in "$CERT" -noout -pubkey)" = "$(openssl req -in "$DEST/pipeline.csr" -noout -pubkey)" ] \
  || die "--client-cert is not for the key init made here (sign this host's pipeline.csr)"
report_expiry "$CERT" "pipeline.pem"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
cp "$CA" "$work/ca.pem"
cp "$CERT" "$work/pipeline.pem"
# The key stays: place_files only adds and replaces the files it is given.
place_files "$work" "$DEST/tls" "$APP_UID" 400 keepdir
rm -f "$work"/*
python_ok=$(docker run --rm -i --network none --entrypoint python "$IMAGE" -c \
  'import json,sys; d=json.load(sys.stdin); print("ok" if set(d) == {"keys"} and isinstance(d["keys"], dict) and d["keys"] else "bad")' \
  < "$TRUSTED" 2>/dev/null || echo bad)
[ "$python_ok" = ok ] || die "--trusted-keys is not a request-keys.json"
cp "$TRUSTED" "$work/request-keys.json"
place_files "$work" "$DEST/trusted" "$APP_UID" 400 keepdir
rm -f "$work"/*
info "ca.pem, pipeline.pem and request-keys.json installed"

say "The database password (intelligence_app)"
if owned_ok secrets/intelligence-db-password && [ "$REPLACE_PW" = 0 ]; then
  info "in place, kept (--replace-db-password to change it)"
else
  PW=''
  if [ -t 0 ]; then read -r -s -p "    intelligence_app password (hidden): " PW || true; printf '\n'
  else IFS= read -r PW || true; fi
  [ "${#PW}" -ge 16 ] || { PW=''; die "the password on standard input is missing or shorter than 16 characters"; }
  case "$PW" in *[[:space:]]*) PW=''; die "the password has whitespace in it" ;; esac
  printf '%s' "$PW" | place_secret intelligence-db-password
  PW=''
  info "written (0400; not shown)"
fi
unset PW

say "Files and settings"
install -m 644 "$HERE/compose.yml" "$DEST/compose.yml"
install -m 644 "$HERE/compose.gpu.yml" "$DEST/compose.gpu.yml"
install -m 644 "$HERE/app-link.cfg" "$DEST/app-link.cfg"
mkdir -p "$DEST/egress-proxy" "$DEST/models"
chmod 755 "$DEST/egress-proxy" "$DEST/models"
install -m 644 "$SRC/egress-proxy/squid.conf" "$DEST/egress-proxy/squid.conf"
# Bind mounts need the files: an empty key and allow-list when the route is off.
owned_ok secrets/pilot-cloud-api-key || : | place_secret pilot-cloud-api-key
[ -f "$DEST/egress-proxy/allowed-hosts" ] || install -m 644 /dev/null "$DEST/egress-proxy/allowed-hosts"
env_set APP_HOST_ADDR "$APP"
profiles=()
if [ -n "$PROVIDER" ]; then
  if [ ! -s "$DEST/secrets/pilot-cloud-api-key" ] || ! owned_ok secrets/pilot-cloud-api-key; then
    die "no cloud key: put-secret.sh pilot-cloud-api-key first"
  fi
  HOST=$(docker run --rm --network none --entrypoint python "$IMAGE" -c \
    'import sys; from intelligence.boundary.cloud import vocab; print(vocab.HOSTS[sys.argv[1]])' "$PROVIDER" 2>/dev/null) \
    || die "$IMAGE does not support the provider $PROVIDER"
  printf '%s' "$HOST" | grep -Eq '^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$' || die "unexpected API host"
  printf '# %s only (enable-intelligence-host.sh).\n%s\n' "$PROVIDER" "$HOST" > "$DEST/egress-proxy/allowed-hosts"
  chmod 644 "$DEST/egress-proxy/allowed-hosts"
  P=$(printf '%s' "$PROVIDER" | tr '[:lower:]' '[:upper:]')
  for other in ANTHROPIC GEMINI; do
    [ "$other" = "$P" ] || for v in MODEL KEY_FILE ZDR_REF; do env_unset "PILOT_${other}_$v"; done
  done
  env_set PILOT_CLOUD_PROVIDER "$PROVIDER"
  env_set "PILOT_${P}_MODEL" "$MODEL"
  env_set "PILOT_${P}_KEY_FILE" /run/secrets/cloud-api-key
  env_set "PILOT_${P}_ZDR_REF" "$ZDR"
  if [ "$PROVIDER" = gemini ]; then env_set PILOT_GEMINI_PAID_TIER verified; else env_unset PILOT_GEMINI_PAID_TIER; fi
  profiles+=(cloud)
  info "cloud route: $PROVIDER via the egress proxy, $HOST only"
fi
if [ "$OWN" = 1 ]; then
  # shellcheck source=deploy/pilot/ownmodel-lib.sh
  . "$SRC/pilot/ownmodel-lib.sh"
  running() { [ -n "$(cd "$DEST" && docker compose ps -q "$1" 2>/dev/null)" ] && echo 1 || echo 0; }
  for f in models.lock models/trust-policy.json models/testsets.lock; do
    [ -f "$REPO/$f" ] || die "run this from a checkout of the repository ($f is missing)"
  done
  install -m 644 "$REPO/models.lock" "$DEST/models/models.lock"
  install -m 644 "$REPO/models/trust-policy.json" "$DEST/models/trust-policy.json"
  install -m 644 "$REPO/models/testsets.lock" "$DEST/models/testsets.lock"
  check_lock "$DEST/models"
  # A host of its own: only the pipeline (its 2 GiB cap) and the sidecar, and
  # 1 GiB for the rest.
  RESERVE_MIB=${PILOT_HOST_RESERVE_MIB:-1024}
  PIPELINE_MIB=2048
  check_memory "$MEM" "$(running pipeline)" "$(running llama)" 0
  for k in model-store-reader-access-key model-store-reader-secret-key; do
    owned_ok "secrets/$k" || die "no secrets/$k: put-secret.sh $k first"
  done
  mkdir -p "$DEST/llama"
  chmod 755 "$DEST/llama"
  install -m 644 "$SRC/llama/Dockerfile" "$DEST/llama/Dockerfile"
  install -m 644 "$SRC/llama/entrypoint.sh" "$DEST/llama/entrypoint.sh"
  env_set PILOT_MODEL_ROLES cca
  env_set PILOT_LOCAL_MODEL_MEM "$MEM"
  env_set PILOT_LOCAL_MODEL_THREADS "$THREADS"
  env_set PILOT_LOCAL_MODEL_CTX "$CTX"
  env_set PILOT_MODEL_STORE_ENDPOINT "$ENDPOINT"
  env_set PILOT_MODEL_STORE_BUCKET "$BUCKET"
  env_set PILOT_MODEL_STORE_REGION "$REGION"
  profiles+=(ownmodel)
fi
if [ "$GPU" = 1 ]; then
  docker info --format '{{json .Runtimes}}' | grep -q nvidia \
    || die "--gpu: the NVIDIA Container Toolkit is not installed (no nvidia runtime)"
  env_set COMPOSE_FILE compose.yml:compose.gpu.yml
  env_set LLAMA_IMAGE mhai-llama:cuda
else
  env_set COMPOSE_FILE compose.yml
  env_unset LLAMA_IMAGE
fi
env_set COMPOSE_PROFILES "$(IFS=,; printf '%s' "${profiles[*]-}")"
cd "$DEST"
docker compose config --quiet || die "compose.yml and .env don't validate"

say "Starting"
if [ "$OWN" = 1 ]; then
  docker compose build llama
  docker compose run --rm --no-deps -T model-fetch </dev/null || die "model-fetch failed (a fixed code above)"
fi
SINCE=$(date -u +%Y-%m-%dT%H:%M:%SZ)
docker compose up -d
docker compose restart pipeline >/dev/null
started=0
for _ in $(seq 1 36); do
  if docker compose logs --no-log-prefix --since "$SINCE" pipeline 2>/dev/null | grep -q 'worker started service=pipeline'; then
    started=1; break
  fi
  sleep 5
done
if [ "$started" != 1 ]; then
  docker compose logs --tail=60 app-link pipeline
  die "the pipeline did not start: its errors name variables only (check the firewall, the certificate and the password)"
fi
info "pipeline started: NATS over TLS, Postgres verify-full, through app-link to $APP"
if [ "$OWN" = 1 ]; then
  compose() { docker compose "$@"; }
  wait_cca_served "$SINCE"
fi

say "Done"
info "Next: on the app host, set each hospital's model policy (pilot/README.md, steps 7 and 8)."

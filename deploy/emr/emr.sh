#!/usr/bin/env bash
# The emr service on every deploy (deploy/README.md, "EMR integration"). The
# deploy workflow runs this in /srv/mhai-backend after core is healthy, as it
# runs pilot/pipeline.sh and the notifications step; enable-emr.sh and
# enable-production.sh run it too.
#
#   - EMR_ENABLED=true in .env (deploy/emr/enable-emr.sh writes it): checks
#     that production mode is on and that every file and setting the service
#     needs is there, pulls EMR_IMAGE, starts or refreshes egress-emr and
#     emr, and waits for the service's start line. Anything missing fails the
#     deploy, naming what to run.
#   - OPENBAO_ENABLED=true too (deploy/openbao/enable-openbao.sh): the
#     OpenBao overlay and the emr service's credentials must be there;
#     openbao is started with emr (as production). OpenBao starts SEALED
#     after a restart: that never fails the deploy. emr then runs, fails
#     closed (its connections show the store unavailable), and this prints
#     a warning to unseal it (openbao/enable-openbao.sh --unseal).
#   - anything else: emr and egress-emr (and openbao) are stopped and
#     removed if they run.
#
# It prints no secret (it checks only that the secret files and values
# exist) and touches only this backend's compose project.
set -euo pipefail

DEST=${MHAI_BACKEND_DIR:-$PWD}
TRIES=${EMR_START_TRIES:-36}
export PROD_POLL=${EMR_POLL_INTERVAL:-5}

die() { printf '::error::emr: %s\n' "$*" >&2; exit 1; }
info() { printf 'emr: %s\n' "$*"; }

cd "$DEST"
[ -f .env ] || die "no .env in $DEST"
env_get() { sed -n "s/^$1=\(.*\)\$/\1/p" .env | tail -n1; }
dc() { docker compose --profile emr "$@"; }

SVCS=(egress-emr emr)
if [ "$(env_get EMR_ENABLED)" != true ]; then
  if [ -n "$(dc ps -q emr egress-emr 2>/dev/null)" ] || [ -n "$(dc ps -q openbao 2>/dev/null)" ]; then
    dc stop emr egress-emr >/dev/null 2>&1 || true
    dc rm -f emr egress-emr >/dev/null 2>&1 || true
    dc stop openbao >/dev/null 2>&1 || true
    dc rm -f openbao >/dev/null 2>&1 || true
    info "EMR_ENABLED is not true: emr and egress-emr (and openbao) stopped"
  else
    info "off (EMR_ENABLED is not true)"
  fi
  exit 0
fi

files=$(env_get COMPOSE_FILE)
case ":$files:" in *:compose.production.yml:*) ;; *)
  die "EMR_ENABLED=true but production mode is off (COMPOSE_FILE has no compose.production.yml): the emr service speaks TLS only. Run production/enable-production.sh, or emr/enable-emr.sh --disable" ;;
esac
case ":$files:" in *:compose.emr.yml:*) ;; *)
  die "EMR_ENABLED=true but COMPOSE_FILE has no compose.emr.yml (core's EMR settings): run emr/enable-emr.sh" ;;
esac
# Each a regular, non-empty file (a path Docker made as a folder, a mount of
# a missing file, is refused too).
for f in secrets/nats-emr.nk secrets/emr-proxy.key secrets/emr-seal.pub secrets/core-idempotency-hmac.key \
         tls/public/emr.pem compose.emr.yml; do
  if [ -L "$f" ] || [ ! -f "$f" ] || [ ! -s "$f" ]; then
    die "$f is missing or empty: run emr/enable-emr.sh (deploy/README.md, \"EMR integration\")"
  fi
done
for d in emr/seal emr/store tls/emr; do
  [ -d "$d" ] && [ ! -L "$d" ] || die "$d is missing: run emr/enable-emr.sh"
done
OPENBAO=0
if [ "$(env_get OPENBAO_ENABLED)" = true ]; then
  case ":$files:" in *:compose.openbao.yml:*) ;; *)
    die "OPENBAO_ENABLED=true but COMPOSE_FILE has no compose.openbao.yml: run openbao/enable-openbao.sh (or its --disable)" ;;
  esac
  for f in compose.openbao.yml secrets/openbao-emr.json tls/public/openbao.pem openbao/openbao.hcl openbao/emr-policy.hcl; do
    if [ -L "$f" ] || [ ! -f "$f" ] || [ ! -s "$f" ]; then die "$f is missing or empty: run openbao/enable-openbao.sh"; fi
  done
  [ -d tls/openbao ] && [ ! -L tls/openbao ] || die "tls/openbao is missing: run openbao/enable-openbao.sh --prepare"
  OPENBAO=1
  SVCS=(openbao egress-emr emr)
else
  case ":$files:" in *:compose.openbao.yml:*)
    die "COMPOSE_FILE has compose.openbao.yml but OPENBAO_ENABLED is not true: run openbao/enable-openbao.sh (or its --disable)" ;;
  esac
fi
[[ $(env_get NATS_EMR_NKEY) =~ ^U[A-Z2-7]{55}$ ]] || die "NATS_EMR_NKEY in .env is not a NATS user public key: run emr/enable-emr.sh"
[ -n "$(env_get EMR_DB_PASSWORD)" ] || die "EMR_DB_PASSWORD is not in .env: run emr/enable-emr.sh"
[ -n "$(env_get EGRESS_EMR_INTERNAL_CIDRS)" ] || die "EGRESS_EMR_INTERNAL_CIDRS is not in .env: run emr/enable-emr.sh"
grep -F 'nkey: $NATS_EMR_NKEY' nats/nats.conf >/dev/null || die "nats/nats.conf has no emr user: deploy the release with it first"

dc pull -q "${SVCS[@]}" >/dev/null 2>&1 || info "pull skipped (images already here, or no registry login)"
if ! dc up -d "${SVCS[@]}"; then
  dc logs --tail=60 "${SVCS[@]}"
  die "emr did not start"
fi
started=0
for _ in $(seq 1 "$TRIES"); do
  # grep without -q: an early exit would cut the log stream (PR #289).
  if dc logs --no-log-prefix emr 2>/dev/null | grep 'emr serving' >/dev/null; then started=1; break; fi
  sleep "$PROD_POLL"
done
if [ "$started" != 1 ]; then
  dc logs --tail=60 emr egress-emr
  die "the emr service did not start (its log is above; it names a setting, never a value)"
fi
state=$(docker inspect -f '{{.State.Status}} {{.RestartCount}}' "$(dc ps -q egress-emr)" 2>/dev/null || echo "missing 0")
case "$state" in "running "*) ;; *) dc logs --tail=40 egress-emr; die "egress-emr is not running ($state)" ;; esac
if [ "$OPENBAO" = 1 ]; then
  # Sealed is not a deploy failure: the store fails closed until unsealed.
  ob=$(dc exec -T openbao bao status -format=json 2>/dev/null || true)
  case "$(printf '%s' "$ob" | jq -r '"\(.initialized) \(.sealed)"' 2>/dev/null)" in
    "true false") info "OpenBao unsealed" ;;
    "true true")
      printf '::warning::emr: OpenBao is sealed: the EMR connections fail closed until it is unsealed. Run: bash %s/openbao/enable-openbao.sh --unseal (3 of the 5 key shares)\n' "$DEST"
      info "OpenBao is SEALED: run openbao/enable-openbao.sh --unseal" ;;
    *) printf '::warning::emr: OpenBao does not answer: the EMR connections fail closed (openbao/enable-openbao.sh --status)\n' ;;
  esac
fi
info "running (emr serving; egress-emr $state)"

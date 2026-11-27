#!/usr/bin/env bash
# Production mode for the single-host backend (deploy/README.md, "Production
# mode"; docs/deploy/production-tls-emr.md): TLS for NATS (client
# certificates for every client) and Postgres (verify-full for every client),
# core as production, the pipeline off its single-host plaintext mode, the
# notifications service as production, and the BFFs (mhai-web) over TLS. Run
# by the operator, as the deploy user, on the host:
#
#   bash /srv/mhai-backend/production/enable-production.sh --check
#   bash /srv/mhai-backend/production/enable-production.sh --prepare [--ca-dir DIR]
#   bash /srv/mhai-backend/production/enable-production.sh [--ca-dir DIR]
#   bash /srv/mhai-backend/production/enable-production.sh --rollback
#
# (the deploy workflow copies it there; a checkout's deploy/ works the same).
#
#   --check     writes nothing: what is ready, what would change, what is
#               missing, the certificates' expiry. Exit 1 when the switch
#               would refuse.
#   --prepare   everything that needs no restart: the private CA (made in
#               --ca-dir if it has none), every certificate, the compose
#               overlay and config files, the rollback copy, the images, a
#               validation of the new compose config. Nothing running changes.
#   (none)      --prepare, then the switch: one restart sequence (Postgres
#               and NATS, core with its migrations, the pipeline,
#               notifications, emr, the BFFs), then the checks. Any failure
#               during the switch rolls back by itself (--no-auto-rollback
#               to keep the failed state for a look).
#   --rollback  back to the mode before (COMPOSE_FILE as it was), the EMR
#               switched off first (it speaks TLS only), every service
#               recreated, the BFFs back on their plain settings. The
#               certificates stay (a re-run reuses them).
#   --renew     issue every certificate again (needs the CA in --ca-dir):
#               yearly, or at once if a key may have leaked.
#   --ca-dir    the private CA (deploy/tls/make-ca.sh), default ~/mhai-ca.
#               Needed only to issue. After issuing: move it OFF this host
#               (README, "The private CA").
#
# Idempotent: a second run changes nothing that is already right and keeps
# every certificate that is still good. It prints no secret and no key, and
# touches only this backend's compose project and, for the BFFs, the
# web-live and platform-live services of mhai-web.
set -euo pipefail
umask 077

DEST=${MHAI_BACKEND_DIR:-/srv/mhai-backend}
SRC=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
WEB_DIR=${MHAI_WEB_DIR:-/srv/mhai-web}
PLATFORM_DIR=${MHAI_WEB_PLATFORM_DIR:-/srv/mhai-web-platform}
WEB_PORT=${WEB_LIVE_PORT:-8093}
PLATFORM_PORT=${PLATFORM_LIVE_PORT:-8094}
# shellcheck source=deploy/tls/split-lib.sh
. "$SRC/tls/split-lib.sh"
# shellcheck source=deploy/production/prod-lib.sh
. "$SRC/production/prod-lib.sh"

usage() { sed -n '9,12p' "$0" >&2; exit 2; }
MODE=apply CA=${HOME:-/nonexistent}/mhai-ca RENEW=0 AUTO_ROLLBACK=1
while [ $# -gt 0 ]; do
  case "$1" in
    --check) MODE=check; shift ;;
    --prepare) MODE=prepare; shift ;;
    --rollback) MODE=rollback; shift ;;
    --renew) RENEW=1; shift ;;
    --ca-dir) [ $# -ge 2 ] || usage; CA=$2; shift 2 ;;
    --no-auto-rollback) AUTO_ROLLBACK=0; shift ;;
    *) usage ;;
  esac
done

PROBLEMS=0 SWITCHING=0 work=''
# On any exit: the temporary files go; a failure during the switch rolls
# back (unless --no-auto-rollback).
on_exit() {
  local rc=$?
  [ -z "$work" ] || rm -rf "$work"
  if [ "$SWITCHING" = 1 ] && [ "$rc" != 0 ]; then
    SWITCHING=0
    warn "the switch failed"
    if [ "$AUTO_ROLLBACK" = 1 ]; then
      rollback || true
      printf 'error: the switch failed and was rolled back (the logs above say why)\n' >&2
    else
      printf 'error: the switch failed; --rollback to go back\n' >&2
    fi
  fi
  exit "$rc"
}
trap on_exit EXIT
problem() { # in --check: count and go on; otherwise stop
  if [ "$MODE" = check ]; then printf '    PROBLEM: %s\n' "$*" >&2; PROBLEMS=$((PROBLEMS + 1)); else die "$*"; fi
}

# --- prerequisites ------------------------------------------------------------
say "Prerequisites"
for bin in docker openssl curl jq; do command -v "$bin" >/dev/null || die "$bin is not installed"; done
docker compose version >/dev/null 2>&1 || die "docker compose (v2) is not available"
[ "$(id -u)" -ne 0 ] || die "run as the deploy user, not root"
if [ ! -f "$DEST/.env" ] || [ ! -f "$DEST/secrets/nats-gateway.nk" ]; then
  die "$DEST is not set up (deploy/setup-host.sh, then a first deploy)"
fi
cd "$DEST"
PROJECT=$(project)
[ -n "$PROJECT" ] && [ "$PROJECT" != null ] || die "docker compose config does not validate in $DEST"
info "project $PROJECT, compose files $(cf_get)"
cf_has compose.split.yml && die "the split mode is on here (COMPOSE_FILE has compose.split.yml): production mode is for the single-host backend; the split mode already runs NATS with TLS"
SNAP="$DEST/rollback/pre-production"
FILES_BEFORE=$(cf_get)

# The web services, when this host runs them (the live site).
web_targets=() # "dir|base file|override|service|port"
add_web() { # add_web DIR BASE OVERRIDE SERVICE PORT
  [ -f "$1/$2" ] || return 0
  if [ -z "$(cd "$1" && docker compose -f "$2" --profile live ps -q "$4" 2>/dev/null)" ]; then
    info "$4 is not running in $1: not switched (start it with mhai-web's deploy after this)"
    return 0
  fi
  [ -f "$1/$3" ] || problem "$1/$3 is missing: deploy mhai-web with its NATS TLS override first (its README, \"Production mode\"); without it $4 can't reach NATS once TLS is on"
  web_targets+=("$1|$2|$3|$4|$5")
}

rollback() {
  say "Rollback: back to the mode before production"
  local before
  if [ -f "$SNAP/compose-file" ]; then before=$(cat "$SNAP/compose-file"); else before=compose.yml; fi
  before=$(cf_without "$before" "$PROD_OVERLAY")
  before=$(cf_without "$before" "$EMR_OVERLAY")
  if [ "$(env_get EMR_ENABLED)" = true ] || running emr emr || running egress-emr emr; then
    info "the EMR speaks TLS only: switching it off first"
    env_set EMR_ENABLED false
    docker compose --profile emr stop emr egress-emr >/dev/null 2>&1 || true
    docker compose --profile emr rm -f emr egress-emr >/dev/null 2>&1 || true
  fi
  if [ "$before" = compose.yml ]; then env_unset COMPOSE_FILE; else env_set COMPOSE_FILE "$before"; fi
  rm -f "$DEST/production-mode"
  local t0; t0=$(now_s)
  docker compose up -d --force-recreate --no-deps postgres nats
  docker compose up -d core || { docker compose logs --tail=80 postgres nats core-migrate core; die "rollback: core did not start"; }
  core_ready || { docker compose logs --tail=80 core; die "rollback: core is not ready"; }
  info "core ready ($(elapsed "$t0" "$(now_s)") s after the restart began)"
  restart_workers
  local w dir base ovr svc port
  for w in "${web_targets[@]}"; do
    IFS='|' read -r dir base ovr svc port <<<"$w"
    (cd "$dir" && docker compose -f "$base" --profile live up -d --no-deps "$svc") || warn "$svc did not restart in $dir"
    web_ok "$port" || warn "$svc is not healthy on 127.0.0.1:$port"
  done
  info "back to $(cf_get) in $(elapsed "$t0" "$(now_s)") s"
}

web_ok() { # web_ok PORT: the app answers /api/health (it reaches NATS)
  for _ in $(seq 1 36); do
    curl -fsS --max-time 5 "http://127.0.0.1:$1/api/health" 2>/dev/null | grep '"ok"' >/dev/null && return 0
    sleep "${PROD_POLL:-5}"
  done
  return 1
}

# restart_workers: the pipeline, notifications and emr, as the deploy starts
# them, each only if it is on.
restart_workers() {
  if [ "$(env_get PIPELINE_ENABLED)" = true ] && [ -f pilot/pipeline.sh ]; then
    info "pipeline (PIPELINE_ENABLED=true): pilot/pipeline.sh"
    bash pilot/pipeline.sh
  elif running pipeline pilot; then
    info "pipeline (started by hand): recreated"
    docker compose --profile pilot up -d --no-deps --force-recreate pipeline
    log_has pipeline "worker started" 36 pilot || { docker compose --profile pilot logs --tail=60 pipeline; die "the pipeline did not start"; }
  fi
  if [ "$(env_get NOTIFY_ENABLED)" = true ]; then
    info "notifications (NOTIFY_ENABLED=true): recreated"
    docker compose --profile notifications up -d --force-recreate mail-relay notifications
    log_has notifications 'notifications serving' 36 notifications \
      || { docker compose --profile notifications logs --tail=60 notifications; die "the notifications service did not start"; }
  fi
  if [ "$(env_get EMR_ENABLED)" = true ] && [ -f emr/emr.sh ]; then
    info "emr (EMR_ENABLED=true): emr/emr.sh"
    bash emr/emr.sh
  fi
}

if [ "$MODE" = rollback ]; then
  add_web "$WEB_DIR" compose.yml compose.nats-tls.yml web-live "$WEB_PORT"
  add_web "$PLATFORM_DIR" compose.platform.yml compose.platform.nats-tls.yml platform-live "$PLATFORM_PORT"
  production_on || [ -e "$DEST/production-mode" ] || { info "production mode is not on: nothing to roll back"; exit 0; }
  rollback
  say "Rolled back. Production mode is off; the certificates are kept in $DEST/tls."
  exit 0
fi

# --- the settings core and the services check outside development ----------
say "Settings production refuses (.env)"
delivery=$(env_get CORE_DELIVERY)
case "$delivery" in
  log) problem "CORE_DELIVERY=log: production refuses it (it sends nothing). Set notify (email on) or none" ;;
  notify)
    [ "$(env_get CORE_NOTIFY_ENABLED)" = true ] || problem "CORE_DELIVERY=notify needs CORE_NOTIFY_ENABLED=true"
    [ -n "$(env_get CORE_APP_URL)" ] || problem "CORE_DELIVERY=notify needs CORE_APP_URL" ;;
  ''|none) warn "CORE_DELIVERY is ${delivery:-unset}: in production nothing is sent and invite and reset actions fail closed (dev links are never shown)" ;;
  *) problem "CORE_DELIVERY must be notify, none or unset" ;;
esac
case "$(env_get CORE_APP_URL)" in ''|https://?*) ;; *) problem "CORE_APP_URL must be the app's https:// address in production" ;; esac
if [ -z "$(env_get SECURITY_CONTACT_NAME)" ] || [ -z "$(env_get SECURITY_CONTACT_EMAIL)" ]; then
  problem "SECURITY_CONTACT_NAME and SECURITY_CONTACT_EMAIL are required in production"
fi
case "$(env_get NATS_JS_REPLICAS)" in ''|1) ;; *) problem "NATS_JS_REPLICAS must be 1 (or unset) on one host" ;; esac
[ "$(env_get OTEL_EXPORTER_OTLP_INSECURE)" = true ] && warn "OTEL_EXPORTER_OTLP_INSECURE=true: traces would leave in plaintext (core warns at start)"
case ",$(env_get COMPOSE_PROFILES)," in *,demo,*) problem "COMPOSE_PROFILES has demo: production mode never runs the demo" ;; esac
running sample-ai demo && problem "sample-ai (the demo's sample answers) is running: production mode never runs the demo"
if [ "$(env_get NOTIFY_ENABLED)" = true ]; then
  case ",$(env_get NOTIFY_CHANNELS)," in *,log,*) problem "NOTIFY_CHANNELS has log: production refuses it" ;; esac
fi
info "checked"

add_web "$WEB_DIR" compose.yml compose.nats-tls.yml web-live "$WEB_PORT"
add_web "$PLATFORM_DIR" compose.platform.yml compose.platform.nats-tls.yml platform-live "$PLATFORM_PORT"
for need in compose.production.yml nats/tls.conf postgres/pg_hba.production.conf; do
  [ -f "$SRC/$need" ] || die "$SRC/$need is missing: run this from the release's deploy/ (the deploy bundle or a checkout)"
done

# --- certificates ---------------------------------------------------------------
say "Certificates (private CA; keys never printed)"
PUB="$DEST/tls/public"
PIPELINE_IMAGE_=$(env_get PIPELINE_IMAGE); PIPELINE_IMAGE_=${PIPELINE_IMAGE_:-ghcr.io/measured-health/mhai-multiagent-pipeline-pilot:latest}
PIPELINE_UID=1000
if docker image inspect "$PIPELINE_IMAGE_" >/dev/null 2>&1; then
  PIPELINE_UID=$(docker run --rm --network none --entrypoint id "$PIPELINE_IMAGE_" -u)
  case "$PIPELINE_UID" in ''|0|*[!0-9]*) die "the pipeline image's user is not a non-root uid" ;; esac
fi
# name|kind|dns|folder|uid|keymode
CERTS=(
  "nats|server|nats,mhai-nats|nats|$NATS_UID|400"
  "postgres|server|postgres|postgres|$POSTGRES_UID|600"
  "core|client||core|$CORE_UID|400"
  "pipeline|client||pipeline|$PIPELINE_UID|400"
  "notifications|client||notifications|$NOTIFY_UID|400"
  "emr|client||emr|$EMR_UID|400"
  "bff-clinical|client||bff|$WEB_UID|400"
  "bff-platform|client||bff|$WEB_UID|400"
)
need_issue=$RENEW
if [ ! -f "$PUB/ca.pem" ]; then
  need_issue=1
else
  for c in "${CERTS[@]}"; do
    IFS='|' read -r name _ <<<"$c"
    if [ ! -f "$PUB/$name.pem" ] || ! openssl verify -CAfile "$PUB/ca.pem" "$PUB/$name.pem" >/dev/null 2>&1; then
      info "$name.pem missing or not from tls/public/ca.pem"; need_issue=1
    elif [ "$(cert_days_left "$PUB/$name.pem")" -lt 30 ]; then
      info "$name.pem expires in under 30 days"; need_issue=1
    fi
  done
fi
if [ "$need_issue" = 0 ]; then
  info "every certificate is in place and good, kept (--renew to issue again)"
  [ ! -f "$CA/ca-key.pem" ] || warn "the CA key is still on this host ($CA): move it off (README, \"The private CA\")"
elif [ "$MODE" = check ]; then
  if [ -f "$CA/ca-key.pem" ]; then info "would issue every certificate from $CA"
  elif [ -e "$CA" ]; then problem "$CA exists but holds no CA"
  else info "would make a new private CA in $CA and issue every certificate"; fi
else
  if [ ! -f "$CA/ca-key.pem" ]; then
    [ ! -f "$PUB/ca.pem" ] || [ "$RENEW" = 1 ] || die "the certificates need issuing again but $CA has no CA key: bring the CA back (--ca-dir), or --renew with a new one (every certificate is issued again)"
    bash "$SRC/tls/make-ca.sh" "$CA" "production" | sed 's/^/    /'
  fi
  if [ -f "$PUB/ca.pem" ] && ! cmp -s "$PUB/ca.pem" "$CA/ca.pem"; then
    [ "$RENEW" = 1 ] || die "$CA is not the CA these certificates came from: --renew to issue every certificate from it (a CA rotation)"
    info "a new CA: every certificate is issued from it"
  fi
  work=$(mktemp -d)
  for c in "${CERTS[@]}"; do
    IFS='|' read -r name kind dns folder _ _ <<<"$c"
    mkdir -p "$work/$folder"
    if [ "$kind" = server ]; then
      bash "$SRC/tls/issue.sh" --ca-dir "$CA" --out "$work/$folder" --name server --kind server --dns "$dns" | sed 's/^/    /'
      cp "$work/$folder/server.pem" "$work/$name.pub.pem"
    else
      bash "$SRC/tls/issue.sh" --ca-dir "$CA" --out "$work/$folder" --name "$name" --kind client | sed 's/^/    /'
      cp "$work/$folder/$name.pem" "$work/$name.pub.pem"
    fi
    cp "$CA/ca.pem" "$work/$folder/ca.pem"
  done
  mkdir -p "$DEST/tls"; chmod 755 "$DEST/tls"
  for c in "${CERTS[@]}"; do
    IFS='|' read -r name _ _ folder uid mode <<<"$c"
    [ -d "$work/$folder" ] || continue
    if [ "$folder" = bff ]; then
      place_files "$work/bff" "$DEST/secrets/bff/tls" "$uid" "$mode"
    else
      place_files "$work/$folder" "$DEST/tls/$folder" "$uid" "$mode"
    fi
    rm -rf "${work:?}/$folder"
  done
  mkdir -p "$PUB"; chmod 755 "$PUB"
  for c in "${CERTS[@]}"; do IFS='|' read -r name _ <<<"$c"; install -m 644 "$work/$name.pub.pem" "$PUB/$name.pem"; done
  install -m 644 "$CA/ca.pem" "$PUB/ca.pem"
  info "placed in tls/<service> and secrets/bff/tls (public copies in tls/public)"
fi
if [ -f "$PUB/nats.pem" ]; then report_expiry "$PUB/nats.pem" "the certificates"; fi

# --- the files the overlay names -------------------------------------------------
copy_in() { # copy_in REL: from the release into DEST, unless it is the same file
  if [ "$MODE" = check ]; then
    if [ -f "$DEST/$1" ] && cmp -s "$SRC/$1" "$DEST/$1"; then info "$1 in place"; else info "would install $1"; fi
    return
  fi
  mkdir -p "$DEST/$(dirname "$1")"; chmod 755 "$DEST/$(dirname "$1")"
  [ "$SRC/$1" -ef "$DEST/$1" ] || install -m 644 "$SRC/$1" "$DEST/$1"
  # Read by the containers' own users (Postgres 70, NATS 1000): world-readable.
  chmod 644 "$DEST/$1"
}
say "Compose overlay and config files"
copy_in compose.production.yml
copy_in nats/tls.conf
copy_in postgres/pg_hba.production.conf
[ "$(env_get EMR_ENABLED)" != true ] || copy_in compose.emr.yml
FILES_AFTER=$(cf_with "$(cf_get)" "$PROD_OVERLAY")
if [ "$MODE" != check ] || [ -f "$PUB/ca.pem" ]; then
  COMPOSE_FILE=$FILES_AFTER docker compose config --quiet || problem "the compose files with $PROD_OVERLAY don't validate"
  info "COMPOSE_FILE=$FILES_AFTER validates"
fi

if [ "$MODE" = check ]; then
  say "Web BFFs"
  for w in "${web_targets[@]}"; do IFS='|' read -r dir _ ovr svc port <<<"$w"; info "$svc in $dir: switched with $ovr (127.0.0.1:$port)"; done
  [ "${#web_targets[@]}" -gt 0 ] || info "none running here"
  if production_on; then info "production mode is ON ($(cf_get))"; else info "production mode is off: the switch would restart Postgres, NATS, core and the workers (expect 30 to 90 s of downtime)"; fi
  [ "$PROBLEMS" = 0 ] || { say "$PROBLEMS problem(s): the switch would refuse"; exit 1; }
  say "Ready"
  exit 0
fi

# --- the rollback copy and the images --------------------------------------------
say "Rollback copy and images"
if [ ! -d "$SNAP" ] && ! production_on; then
  mkdir -p "$SNAP"; chmod 700 "$DEST/rollback" "$SNAP"
  cp -p "$ENV_FILE" "$SNAP/env"; chmod 600 "$SNAP/env"
  printf '%s\n' "$FILES_BEFORE" > "$SNAP/compose-file"
  date -u +%Y-%m-%dT%H:%M:%SZ > "$SNAP/taken"
  info "saved: rollback/pre-production (COMPOSE_FILE was $FILES_BEFORE; .env copy, mode 600)"
else
  info "rollback/pre-production kept (taken $(cat "$SNAP/taken" 2>/dev/null || echo earlier))"
fi
COMPOSE_FILE=$FILES_AFTER docker compose pull -q postgres nats core >/dev/null 2>&1 || info "pull skipped (images already here, or no registry login)"

if [ "$MODE" = prepare ]; then
  say "Prepared. Nothing running changed. Run again without --prepare to switch."
  exit 0
fi

# --- the switch ---------------------------------------------------------------------
say "Switch (one restart sequence)"
T0=$(now_s)
SWITCHING=1
env_set COMPOSE_FILE "$FILES_AFTER"
date -u +%Y-%m-%dT%H:%M:%SZ > "$DEST/production-mode"; chmod 644 "$DEST/production-mode"
docker compose up -d --force-recreate --no-deps postgres nats
if ! docker compose up -d core; then
  docker compose logs --tail=80 postgres nats core-migrate core; die "core did not start"
fi
core_ready || { docker compose logs --tail=80 nats core; die "core is not ready"; }
T_CORE=$(now_s)
info "core ready over TLS in $(elapsed "$T0" "$T_CORE") s"
restart_workers
for w in "${web_targets[@]}"; do
  IFS='|' read -r dir base ovr svc port <<<"$w"
  (cd "$dir" && docker compose -f "$base" -f "$ovr" --profile live up -d --no-deps "$svc")
  if ! web_ok "$port"; then
    (cd "$dir" && docker compose -f "$base" -f "$ovr" --profile live logs --tail=60 "$svc"); die "$svc did not reconnect"
  fi
  info "$svc reconnected over TLS ($(elapsed "$T0" "$(now_s)") s)"
done
T1=$(now_s)
SWITCHING=0

say "Checks"
info "NATS clients (name, TLS, user):"
nats_connz | sed 's/^/      /'
if nats_connz | awk -F'\t' '$2 == "none"' | grep . >/dev/null; then warn "a NATS client is without TLS (above)"; fi
info "Postgres network sessions (user, TLS, count):"
pg_tls_report | sed 's/^/      /'
if pg_tls_report | awk '$2 == "f"' | grep . >/dev/null; then warn "a Postgres session is without TLS (above)"; fi
say "Production mode on: $(cf_get). Downtime: about $(elapsed "$T0" "$T_CORE") s for core, $(elapsed "$T0" "$T1") s until every client was back."
[ ! -f "$CA/ca-key.pem" ] || warn "now move the CA off this host: $CA (README, \"The private CA\")"

#!/usr/bin/env bash
# OpenBao, the emr service's secret store (deploy/README.md, "OpenBao (the
# EMR secret store)"; H3). Production mode and the EMR first
# (production/enable-production.sh, emr/enable-emr.sh). Run by the
# operator, as the deploy user, on the host:
#
#   bash /srv/mhai-backend/openbao/enable-openbao.sh --check
#   bash /srv/mhai-backend/openbao/enable-openbao.sh --prepare [--ca-dir DIR]
#   bash /srv/mhai-backend/openbao/enable-openbao.sh [--ca-dir DIR]
#   bash /srv/mhai-backend/openbao/enable-openbao.sh --unseal
#   bash /srv/mhai-backend/openbao/enable-openbao.sh --status
#   bash /srv/mhai-backend/openbao/enable-openbao.sh --rotate-credentials
#   bash /srv/mhai-backend/openbao/enable-openbao.sh --disable
#   bash /srv/mhai-backend/openbao/enable-openbao.sh --purge-file-store
#   sudo bash /srv/mhai-backend/openbao/enable-openbao.sh --snapshot FILE   (deploy/ops/backup.sh)
#
#   --check     writes nothing: what is ready, what would be made.
#   --prepare   what needs no restart: OpenBao's server certificate from the
#               production CA (tls/openbao; the CA in --ca-dir, default
#               ~/mhai-ca, is needed this once: bring it back, then move it
#               off again), its config in openbao/, the data volume, the
#               image, a validation of the compose files.
#   (none)      --prepare, then: OpenBao started; initialised once with 5
#               Shamir key shares, 3 to unseal, written ONLY to a new 0600
#               file in ~/mhai-openbao-init (0700) for the owner to move
#               offline (never printed, never in .env or a log); unsealed;
#               the KV v2 mount mhai/, the emr-production and backup
#               policies and AppRoles (openbao/configure.sh); the emr
#               service's credentials (secrets/openbao-emr.json, 0400, uid
#               65532) and the backup's (secrets/openbao-backup.json, 0400,
#               root); the root token revoked and taken out of the file;
#               then the switch: compose.openbao.yml in COMPOSE_FILE,
#               OPENBAO_ENABLED=true, emr recreated as production with the
#               OpenBao store, checked (emr secrets ready), and the file
#               store's credentials moved in (emr secrets move-from-file;
#               idempotent). A failure in the switch puts emr back as it was.
#   --unseal    after any restart of OpenBao (a deploy that recreates it, a
#               reboot): asks for 3 key shares (hidden), or reads them one
#               per line from stdin, then checks emr reaches the store.
#   --status    sealed or not, the emr service's view, the certificate.
#   --rotate-credentials
#               new AppRole secret_ids for emr and the backup, the old ones
#               destroyed, emr restarted. Needs 3 key shares (a short-lived
#               root token is made from them and revoked at the end).
#   --disable   emr back to staging with the file store (compose.openbao.yml
#               out of COMPOSE_FILE, OPENBAO_ENABLED=false), OpenBao
#               stopped. Its volume, certificate and credentials stay.
#               Credentials set while on OpenBao are not in the file store:
#               those connections need them entered again.
#   --purge-file-store
#               after the switch is checked: the move once more, then the
#               file store's copies removed (emr/store emptied).
#   --snapshot FILE|-
#               a raft snapshot (encrypted by OpenBao's barrier) to FILE or
#               stdout, by the backup AppRole (root; deploy/ops/backup.sh).
#
# It prints no share, token or secret_id, and touches only this backend's
# compose project.
set -euo pipefail
umask 077

DEST=${MHAI_BACKEND_DIR:-/srv/mhai-backend}
SRC=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
INIT_DIR=${OPENBAO_INIT_DIR:-${HOME:-/nonexistent}/mhai-openbao-init}
# shellcheck source=deploy/tls/split-lib.sh
. "$SRC/tls/split-lib.sh"
# shellcheck source=deploy/production/prod-lib.sh
. "$SRC/production/prod-lib.sh"
APP_ENV_=production
SHARES=5 THRESHOLD=3

usage() { sed -n '7,15p' "$0" >&2; exit 2; }
MODE=apply CA=${HOME:-/nonexistent}/mhai-ca SNAP_OUT=''
while [ $# -gt 0 ]; do
  case "$1" in
    --check) MODE=check; shift ;;
    --prepare) MODE=prepare; shift ;;
    --unseal) MODE=unseal; shift ;;
    --status) MODE=status; shift ;;
    --rotate-credentials) MODE=rotate; shift ;;
    --disable) MODE=disable; shift ;;
    --purge-file-store) MODE=purge; shift ;;
    --snapshot) [ $# -ge 2 ] || usage; MODE=snapshot; SNAP_OUT=$2; shift 2 ;;
    --ca-dir) [ $# -ge 2 ] || usage; CA=$2; shift 2 ;;
    *) usage ;;
  esac
done

PROBLEMS=0
problem() { if [ "$MODE" = check ]; then printf '    PROBLEM: %s\n' "$*" >&2; PROBLEMS=$((PROBLEMS + 1)); else die "$*"; fi; }

for bin in docker openssl jq; do command -v "$bin" >/dev/null || die "$bin is not installed"; done
if [ "$MODE" = snapshot ]; then
  [ "$(id -u)" -eq 0 ] || die "--snapshot runs as root (deploy/ops/backup.sh): the backup credentials are root's"
else
  [ "$(id -u)" -ne 0 ] || die "run as the deploy user, not root"
fi
[ -f "$DEST/.env" ] && [ -f "$DEST/secrets/nats-gateway.nk" ] || die "$DEST is not set up (deploy/setup-host.sh)"
cd "$DEST"

FILES_WITH=$(cf_with "$(cf_get)" "$OPENBAO_OVERLAY")
# Every compose call names the overlay, so OpenBao is reachable before the
# switch (COMPOSE_FILE in .env still without it) and after.
dcb() { COMPOSE_FILE=$FILES_WITH docker compose --profile emr "$@"; }
# ob ARGS...: a command in the openbao container (the loopback listener).
ob() { dcb exec -T openbao "$@"; }
# ob_state: "uninitialised", "sealed", "unsealed" or "down".
ob_state() {
  local j
  j=$(ob bao status -format=json </dev/null 2>/dev/null) || [ -n "$j" ] || { echo down; return; }
  case "$(jq -r '"\(.initialized) \(.sealed)"' <<<"$j" 2>/dev/null)" in
    "false "*) echo uninitialised ;;
    "true true") echo sealed ;;
    "true false") echo unsealed ;;
    *) echo down ;;
  esac
}
wait_state() { # wait_state: until OpenBao answers (any state but down)
  local s
  for _ in $(seq 1 30); do s=$(ob_state); [ "$s" = down ] || { echo "$s"; return 0; }; sleep 2; done
  echo down; return 1
}
# emr_ready: the emr service's own check, in a one-off container with its
# settings (the store it names, its credentials file).
emr_ready() { dcb run --rm --no-deps -T emr secrets ready </dev/null > >(sed 's/^/    /') 2>&1; }
wait_emr_ready() { # wait_emr_ready TRIES
  for _ in $(seq 1 "${1:-12}"); do emr_ready >/dev/null 2>&1 && { emr_ready; return 0; }; sleep "${PROD_POLL:-5}"; done
  emr_ready; return 1
}
volume_name() { local v; v=$(env_get OPENBAO_VOLUME); printf '%s' "${v:-$(project)-openbao}"; }

# unseal_from: reads shares on stdin (one per line) and sends each to
# sys/unseal until OpenBao is unsealed. Nothing of a share is printed.
unseal_from() {
  local share n=0
  # A half-finished unseal (a wrong share earlier) is started again.
  printf '{"reset":true}\n' | ob bao write -format=json sys/unseal - >/dev/null 2>&1 || true
  while IFS= read -r share; do
    [ -n "$share" ] || continue
    printf '%s\n' "$share" | jq -Rc '{key: .}' | ob bao write -format=json sys/unseal - >/dev/null 2>&1 \
      || { share=''; warn "the shares so far did not unseal it (one is not this OpenBao's): progress reset, give $THRESHOLD again"
           printf '{"reset":true}\n' | ob bao write -format=json sys/unseal - >/dev/null 2>&1 || true; n=0; continue; }
    share=''
    n=$((n + 1))
    [ "$(ob_state)" != unsealed ] || { info "unsealed ($n share(s))"; return 0; }
  done
  [ "$(ob_state)" = unsealed ]
}
# prompt_shares: THRESHOLD shares from the terminal (hidden) or stdin.
prompt_shares() {
  local s i
  if [ -t 0 ]; then
    for i in $(seq 1 "$THRESHOLD"); do
      IFS= read -rs -p "    key share $i of $THRESHOLD (hidden): " s </dev/tty; printf '\n' >&2
      printf '%s\n' "$s"; s=''
    done
  else
    cat
  fi
}
init_file() { ls -1 "$INIT_DIR"/init-*.json 2>/dev/null | tail -n1; }
shares_source() { # the shares: from the init file while it is still here, else asked for
  local f; f=$(init_file)
  if [ -n "$f" ]; then jq -r '.unseal_keys_b64[]' "$f"; else prompt_shares; fi
}

# otp_decode ENCODED OTP: the root token from sys/generate-root (the OTP
# XORed with the token, base64 without padding). Pure bash: nothing reaches
# an argument of another process.
otp_decode() {
  local enc=$1 otp=$2 pad i out='' b
  pad=$(( (4 - ${#enc} % 4) % 4 ))
  enc=$enc$(printf '%*s' "$pad" '' | tr ' ' '=')
  local -a bytes
  read -r -a bytes < <(printf '%s' "$enc" | base64 -d | od -An -v -tu1 | tr -s ' \n' ' ')
  [ "${#bytes[@]}" = "${#otp}" ] || return 1
  for ((i = 0; i < ${#otp}; i++)); do
    printf -v b '%d' "'${otp:i:1}"
    printf -v b '%02x' $(( bytes[i] ^ b ))
    out+="\\x$b"
  done
  printf '%b' "$out"
}
# root_from: reads shares on stdin; sets ROOT to a new root token made from
# them (sys/generate-root, on the loopback listener only). revoke_root ends
# it.
ROOT=''
root_from() {
  local g nonce otp share r='' enc
  ob bao delete sys/generate-root/attempt </dev/null >/dev/null 2>&1 || true
  g=$(ob bao write -format=json -f sys/generate-root/attempt </dev/null) || die "could not start a root token generation"
  nonce=$(jq -r '.data.nonce' <<<"$g"); otp=$(jq -r '.data.otp' <<<"$g"); g=''
  [ -n "$otp" ] && [ "$otp" != null ] || die "no one-time password from OpenBao"
  while IFS= read -r share; do
    [ -n "$share" ] || continue
    r=$(printf '%s\n' "$share" | jq -Rc --arg n "$nonce" '{key: ., nonce: $n}' | ob bao write -format=json sys/generate-root/update - 2>/dev/null) \
      || { share=''; warn "a share was refused"; continue; }
    share=''
    [ "$(jq -r '.data.complete' <<<"$r")" != true ] || break
  done
  enc=$(jq -r '.data.encoded_token // empty' <<<"$r"); r=''
  [ -n "$enc" ] || { ob bao delete sys/generate-root/attempt </dev/null >/dev/null 2>&1 || true; die "not enough key shares for a root token"; }
  ROOT=$(otp_decode "$enc" "$otp") || die "could not decode the root token"
  otp='' enc=''
  [[ $ROOT =~ ^s\.[A-Za-z0-9]{20,}$ ]] || { ROOT=''; die "the decoded root token is not a token"; }
  info "a short-lived root token made from the key shares (revoked at the end)"
}
as_root() { printf '%s\n' "$ROOT" | ob env MHAI_ENV="$APP_ENV_" sh "$@"; }
revoke_root() {
  [ -n "$ROOT" ] || return 0
  if printf '%s\n' "$ROOT" | ob sh -c 'IFS= read -r BAO_TOKEN; export BAO_TOKEN; bao token revoke -self' >/dev/null 2>&1; then
    info "root token revoked"
  else
    warn "the root token could not be revoked: revoke it (bao token revoke) before leaving"
  fi
  ROOT=''
}
trap 'revoke_root' EXIT

# place_creds MODE FILE UID: new credentials from configure.sh MODE into
# secrets/FILE (0400, UID), through a temp file in secrets/ (0600); the
# role's other secret_ids are destroyed after.
place_creds() {
  local mode=$1 file=$2 uid=$3 tmp acc role
  tmp=$(mktemp "$DEST/secrets/.openbao.XXXXXX")
  as_root /openbao/config/configure.sh "$mode" > "$tmp" 2> >(sed 's/^/    /' >&2) || { rm -f "$tmp"; die "configure.sh $mode failed"; }
  jq -e 'has("role_id") and has("secret_id") and has("secret_id_accessor")' "$tmp" >/dev/null || { rm -f "$tmp"; die "configure.sh $mode gave no credentials"; }
  acc=$(jq -r .secret_id_accessor "$tmp")
  helper -e U="$uid" -e T="$(basename "$tmp")" -e F="$file" -v "$DEST/secrets:/s" "$HELPER_IMAGE" -c 'set -eu; chown "$U:$U" "/s/$T"; chmod 400 "/s/$T"; mv -f "/s/$T" "/s/$F"'
  role="emr-$APP_ENV_"
  [ "$mode" != backup-credentials ] || role=backup
  as_root /openbao/config/configure.sh prune "$role" "$acc" 2> >(sed 's/^/    /' >&2) >/dev/null
  info "secrets/$file (0400, uid $uid) written"
}

# ---------------------------------------------------------------------------
if [ "$MODE" = snapshot ]; then
  # FILE, or - for stdout (backup.sh encrypts the stream with age). Only
  # errors are printed, on stderr.
  [ -s secrets/openbao-backup.json ] || die "no secrets/openbao-backup.json (enable-openbao.sh first)"
  [ "$(ob_state)" = unsealed ] || die "OpenBao is not unsealed: no snapshot (enable-openbao.sh --unseal)"
  snap() {
    jq -c '{role_id, secret_id}' secrets/openbao-backup.json | ob sh -c '
      set -eu; umask 077
      IFS= read -r c
      BAO_TOKEN=$(printf "%s" "$c" | bao write -field=token auth/approle/login -); c=""; export BAO_TOKEN
      bao operator raft snapshot save /tmp/s.snap >/dev/null
      cat /tmp/s.snap; rm -f /tmp/s.snap; bao token revoke -self >/dev/null'
  }
  if [ "$SNAP_OUT" = - ]; then snap; exit $?; fi
  tmp_out="$SNAP_OUT.tmp.$$"
  snap > "$tmp_out" || { rm -f "$tmp_out"; die "the snapshot failed"; }
  [ -s "$tmp_out" ] || { rm -f "$tmp_out"; die "the snapshot is empty"; }
  mv -f "$tmp_out" "$SNAP_OUT"
  info "OpenBao snapshot: $SNAP_OUT ($(stat -c %s "$SNAP_OUT") bytes)" >&2
  exit 0
fi

if [ "$MODE" = status ]; then
  say "OpenBao"
  info "OPENBAO_ENABLED=$(env_get OPENBAO_ENABLED); COMPOSE_FILE=$(cf_get)"
  st=$(ob_state); info "state: $st"
  [ ! -f tls/public/openbao.pem ] || report_expiry tls/public/openbao.pem "the OpenBao certificate"
  [ -z "$(init_file)" ] || warn "the key shares are still on this host ($(init_file)): move them offline (README, \"OpenBao\")"
  if cf_has "$OPENBAO_OVERLAY"; then
    if emr_ready; then info "the emr service reaches the store"; else warn "the emr service cannot use the store (sealed or down): --unseal"; fi
  fi
  exit 0
fi

if [ "$MODE" = unseal ]; then
  say "Unseal"
  st=$(wait_state) || die "OpenBao is not running (docker compose --profile emr up -d openbao, with COMPOSE_FILE naming $OPENBAO_OVERLAY)"
  case "$st" in
    unsealed) info "already unsealed" ;;
    uninitialised) die "OpenBao is not initialised: run enable-openbao.sh (the setup)" ;;
    sealed) t0=$(now_s); prompt_shares | unseal_from || die "still sealed: give $THRESHOLD of the $SHARES key shares"; info "unsealed in $(elapsed "$t0" "$(now_s)") s" ;;
  esac
  if cf_has "$OPENBAO_OVERLAY"; then
    t0=$(now_s)
    wait_emr_ready 12 || die "OpenBao is unsealed but the emr service can't use it (its log; enable-openbao.sh --status)"
    info "the emr service reaches the store again ($(elapsed "$t0" "$(now_s)") s)"
  fi
  exit 0
fi

if [ "$MODE" = disable ]; then
  say "Switching the emr service back to the file store (staging)"
  env_set OPENBAO_ENABLED false
  env_set COMPOSE_FILE "$(cf_without "$(cf_get)" "$OPENBAO_OVERLAY")"
  t0=$(now_s)
  if [ "$(env_get EMR_ENABLED)" = true ]; then
    docker compose --profile emr up -d --no-deps emr
    log_has emr 'emr serving' 36 emr || { docker compose --profile emr logs --tail=40 emr; die "emr did not start on the file store"; }
  fi
  dcb stop openbao >/dev/null 2>&1 || true
  dcb rm -f openbao >/dev/null 2>&1 || true
  say "OpenBao off; emr on the file store as staging ($(elapsed "$t0" "$(now_s)") s). Kept: the volume $(volume_name), tls/openbao, secrets/openbao-*.json."
  warn "credentials set while on OpenBao are not in the file store: those connections need them entered again"
  exit 0
fi

# --- check / prepare / apply / rotate / purge: the prerequisites ----------
say "Prerequisites"
production_on && [ -f tls/public/emr.pem ] || problem "production mode is not on (production/enable-production.sh first)"
[ "$(env_get EMR_ENABLED)" = true ] && cf_has "$EMR_OVERLAY" || problem "the EMR is not on (emr/enable-emr.sh first)"
[ -d tls/emr ] || problem "tls/emr is missing (emr's client certificate, production/enable-production.sh)"
for f in compose.openbao.yml openbao/openbao.hcl openbao/emr-policy.hcl openbao/backup-policy.hcl openbao/configure.sh; do
  [ -f "$SRC/$f" ] || die "$SRC/$f is missing: run this from the release's deploy/"
done
OB_IMAGE=$(env_get OPENBAO_IMAGE)
OB_IMAGE=${OB_IMAGE:-$(sed -n 's/^ *image: \${OPENBAO_IMAGE:-\(.*\)}$/\1/p' "$SRC/compose.openbao.yml")}
[[ $OB_IMAGE == *@sha256:* ]] || problem "the OpenBao image is not pinned by digest"
VOL=$(volume_name)
avail=$(awk '/^MemAvailable:/ {print int($2/1024)}' "${MEMINFO:-/proc/meminfo}")
[ "$avail" -ge 300 ] || problem "only $avail MiB available: OpenBao takes up to 256 MiB"
info "checked (image $OB_IMAGE, volume $VOL)"

if [ "$MODE" = rotate ]; then
  say "Rotating the AppRole credentials"
  [ "$(ob_state)" = unsealed ] || die "OpenBao is not unsealed (--unseal first)"
  root_from < <(shares_source)
  place_creds emr-credentials openbao-emr.json "$EMR_UID"
  place_creds backup-credentials openbao-backup.json 0
  revoke_root
  t0=$(now_s)
  dcb up -d --no-deps --force-recreate emr
  log_has emr 'emr serving' 36 emr || die "emr did not restart"
  wait_emr_ready 12 || die "emr does not reach the store with the new credentials"
  say "Rotated: the old secret_ids are destroyed; emr restarted ($(elapsed "$t0" "$(now_s)") s)."
  exit 0
fi

if [ "$MODE" = purge ]; then
  say "Removing the file store's copies"
  cf_has "$OPENBAO_OVERLAY" || die "the emr service is not on OpenBao: nothing to purge"
  emr_ready || die "the store is not ready: unseal first"
  dcb run --rm --no-deps -T emr secrets move-from-file --dir /emr-secrets --env staging > >(sed 's/^/    /') \
    || die "the move failed: nothing removed"
  helper -v "$DEST/emr:/e" "$HELPER_IMAGE" -c 'set -eu; rm -rf /e/store/mhai; ls -A /e/store | wc -l' | sed 's/^/    entries left in emr\/store: /'
  say "emr/store emptied (the folder stays)."
  exit 0
fi

# --- the certificate ----------------------------------------------------------
say "Certificate (tls/openbao; key never printed)"
need_cert=0
if [ ! -f tls/public/openbao.pem ] || [ ! -d tls/openbao ] || ! openssl verify -CAfile tls/public/ca.pem tls/public/openbao.pem >/dev/null 2>&1; then
  need_cert=1
elif [ "$(cert_days_left tls/public/openbao.pem)" -lt 30 ]; then
  info "the OpenBao certificate expires in under 30 days"; need_cert=1
fi
if [ "$need_cert" = 0 ]; then
  info "in place and good, kept (renewed with the others: production/enable-production.sh --renew)"
elif [ "$MODE" = check ]; then
  if [ -f "$CA/ca-key.pem" ]; then info "would issue it from $CA"; else problem "no OpenBao certificate yet and no CA in $CA: bring the production CA back for --prepare (README, \"The private CA\")"; fi
else
  [ -f "$CA/ca-key.pem" ] || die "the OpenBao certificate must be issued from the production CA: bring it back to $CA (or --ca-dir), run --prepare, then move it off again"
  cmp -s "$CA/ca.pem" tls/public/ca.pem || die "$CA is not the CA of this host's certificates (tls/public/ca.pem)"
  work=$(mktemp -d); trap 'rm -rf "$work"; revoke_root' EXIT
  bash "$SRC/tls/issue.sh" --ca-dir "$CA" --out "$work/openbao" --name server --kind server --dns openbao --renew | sed 's/^/    /'
  cp "$CA/ca.pem" "$work/openbao/ca.pem"
  cp "$work/openbao/server.pem" "$work/openbao.pub.pem"
  place_files "$work/openbao" "$DEST/tls/openbao" "$OPENBAO_UID" 400
  install -m 644 "$work/openbao.pub.pem" tls/public/openbao.pem
  rm -rf "$work"
  info "issued: tls/openbao (uid $OPENBAO_UID, key 0400), tls/public/openbao.pem"
  [ "$(ob_state)" = down ] || { dcb kill -s HUP openbao >/dev/null && info "OpenBao told to reload its certificate (SIGHUP; it stays unsealed)"; }
  warn "now move the CA off this host again: $CA"
fi

# --- config, volume, image ------------------------------------------------------
say "Config, volume and image"
if [ "$MODE" = check ]; then
  for f in openbao.hcl emr-policy.hcl backup-policy.hcl configure.sh; do
    if [ -f "openbao/$f" ] && cmp -s "$SRC/openbao/$f" "openbao/$f"; then info "openbao/$f in place"; else info "would install openbao/$f"; fi
  done
  if docker volume inspect "$VOL" >/dev/null 2>&1; then info "volume $VOL present"; else info "would make the volume $VOL"; fi
  docker image inspect "$OB_IMAGE" >/dev/null 2>&1 && info "image present" || info "would pull $OB_IMAGE"
  [ "$(env_get OPENBAO_ENABLED)" = true ] && info "OpenBao is ON ($(ob_state))" || info "OpenBao is off: the switch would recreate emr (about 10 s without the EMR; the rest of the site is untouched)"
  [ "$PROBLEMS" = 0 ] || { say "$PROBLEMS problem(s): the switch would refuse"; exit 1; }
  say "Ready"
  exit 0
fi
mkdir -p openbao; chmod 755 openbao
for f in openbao.hcl emr-policy.hcl backup-policy.hcl configure.sh enable-openbao.sh; do
  [ "$SRC/openbao/$f" -ef "openbao/$f" ] || install -m 644 "$SRC/openbao/$f" "openbao/$f"
  chmod 644 "openbao/$f"
done
[ "$SRC/compose.openbao.yml" -ef compose.openbao.yml ] || install -m 644 "$SRC/compose.openbao.yml" compose.openbao.yml
chmod 644 compose.openbao.yml
env_set OPENBAO_VOLUME "$VOL"
docker volume inspect "$VOL" >/dev/null 2>&1 || { docker volume create "$VOL" >/dev/null; info "volume $VOL made"; }
docker image inspect "$OB_IMAGE" >/dev/null 2>&1 || docker pull -q "$OB_IMAGE" >/dev/null || die "could not pull $OB_IMAGE"
dcb config --quiet || die "the compose files with $OPENBAO_OVERLAY don't validate (nothing restarted)"
info "COMPOSE_FILE=$FILES_WITH validates"
if [ "$MODE" = prepare ]; then
  say "Prepared. Nothing running changed. Run again without --prepare to switch."
  exit 0
fi

# --- start, initialise, unseal, configure -----------------------------------------
say "OpenBao"
T0=$(now_s)
dcb up -d --no-deps openbao
st=$(wait_state) || { dcb logs --tail=40 openbao; die "OpenBao did not start"; }
info "running ($st)"
if [ "$st" = uninitialised ]; then
  [ -z "$(init_file)" ] || die "$INIT_DIR already holds key shares, but this OpenBao is new: move them away first (they belong to another OpenBao)"
  mkdir -p "$INIT_DIR"; chmod 700 "$INIT_DIR"
  INIT="$INIT_DIR/init-$(date -u +%Y%m%dT%H%M%SZ).json"
  ( umask 077; ob bao operator init -format=json -key-shares="$SHARES" -key-threshold="$THRESHOLD" </dev/null > "$INIT" ) \
    || { rm -f "$INIT"; die "initialisation failed"; }
  chmod 600 "$INIT"
  [ "$(jq '.unseal_keys_b64 | length' "$INIT")" = "$SHARES" ] || die "initialisation gave no key shares (see $INIT)"
  info "initialised: $SHARES key shares ($THRESHOLD to unseal) and the root token written to $INIT (0600; not shown)"
  st=sealed
fi
if [ "$st" = sealed ]; then shares_source | unseal_from || die "still sealed"; fi
f=$(init_file)
if [ -n "$f" ] && [ "$(jq -r '.root_token // empty' "$f")" != "" ]; then
  ROOT=$(jq -r .root_token "$f")
  info "the initial root token (from $f)"
else
  root_from < <(shares_source)
fi
as_root /openbao/config/configure.sh setup 2> >(sed 's/^/    /' >&2)
if [ -s secrets/openbao-emr.json ] && [ -s secrets/openbao-backup.json ]; then
  info "secrets/openbao-emr.json and openbao-backup.json present, kept (--rotate-credentials for new ones)"
else
  place_creds emr-credentials openbao-emr.json "$EMR_UID"
  place_creds backup-credentials openbao-backup.json 0
fi
revoke_root
if [ -n "$f" ] && [ "$(jq -r '.root_token // empty' "$f")" != "" ]; then
  t=$(mktemp "$INIT_DIR/.init.XXXXXX"); jq 'del(.root_token)' "$f" > "$t"; chmod 600 "$t"; mv -f "$t" "$f"
  info "the revoked root token taken out of $f: it holds the key shares only"
fi

# --- the switch ---------------------------------------------------------------------
say "Switch: emr to production with OpenBao"
SAVED_FILES=$(cf_get) SAVED_ENABLED=$(env_get OPENBAO_ENABLED)
revert() {
  warn "putting emr back as it was"
  env_set COMPOSE_FILE "$SAVED_FILES"
  if [ -n "$SAVED_ENABLED" ]; then env_set OPENBAO_ENABLED "$SAVED_ENABLED"; else env_unset OPENBAO_ENABLED; fi
  docker compose --profile emr up -d --no-deps emr || true
}
T1=$(now_s)
env_set COMPOSE_FILE "$FILES_WITH"
env_set OPENBAO_ENABLED true
if ! docker compose --profile emr up -d --no-deps emr; then revert; die "emr did not start (settings put back)"; fi
if ! log_has emr 'emr serving' 36 emr; then docker compose --profile emr logs --tail=40 emr; revert; die "emr did not start as production (settings put back)"; fi
if ! wait_emr_ready 12; then revert; die "emr does not reach OpenBao (settings put back)"; fi
T2=$(now_s)
info "emr serving as production with OpenBao ($(elapsed "$T1" "$T2") s without the EMR)"
say "Moving the file store's credentials in (idempotent)"
docker compose --profile emr run --rm --no-deps -T emr secrets move-from-file --dir /emr-secrets --env staging > >(sed 's/^/    /') \
  || warn "the move stopped (above); run enable-openbao.sh again to retry (it moves only what is left)"
say "OpenBao on: $(elapsed "$T0" "$(now_s)") s in all; the emr service was away $(elapsed "$T1" "$T2") s."
cat <<EOF
    The owner, now: move $INIT_DIR OFF this host (README, "OpenBao: the key
    shares"): its file holds the $SHARES key shares; $THRESHOLD unseal OpenBao
    after every restart. Keep them apart (no one person, no one place with
    $THRESHOLD), never in chat, email or the repository. Then:
      shred -u $INIT_DIR/init-*.json && rmdir $INIT_DIR
    After any restart of OpenBao (a reboot, a deploy that recreates it):
      bash $DEST/openbao/enable-openbao.sh --unseal
EOF

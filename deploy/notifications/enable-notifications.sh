#!/usr/bin/env bash
# Turn on email for the demo host (deploy/README.md, "Turn on email for the
# demo"). Run by the product owner, as the deploy user, on the instance:
#
#   bash /srv/mhai-backend/notifications/enable-notifications.sh
#
# (the deploy workflow copies this file there). Before it, the NOTIFY_SMTP_*,
# CORE_* and NOTIFY_CHANNELS lines are in .env and the SMTP password is in
# secrets/smtp_password (the README's two steps). This script:
#   - makes the notifications service's NATS NKey (secrets/nats-notifications.nk,
#     mode 0400, uid 65532) inside a container with no network, and writes
#     only its public key to .env (NATS_NOTIFICATIONS_NKEY);
#   - generates NOTIFY_DB_PASSWORD in .env if it isn't there (core-migrate
#     makes the notify_app login from it);
#   - checks the password file's owner and mode, never its content;
#   - sets NOTIFY_ENABLED=true, so every later deploy starts the profile;
#   - recreates nats when its key is new, restarts core (migrations, the
#     login, CORE_* changes) and starts the notifications profile, then
#     waits for the service's start line.
# Run it again at any time: each step keeps what is there. It prints no
# secret. It touches only the mhai-backend compose project.

set -euo pipefail
umask 077

DEST=${MHAI_BACKEND_DIR:-/srv/mhai-backend}
NATS_BOX_IMAGE=${NATS_BOX_IMAGE:-natsio/nats-box:0.19.0}
APP_UID=65532   # distroless nonroot: the notifications image's user

say()  { printf '\n==> %s\n' "$*"; }
info() { printf '    %s\n' "$*"; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }

[ $# -eq 0 ] || die "no arguments: the settings are in $DEST/.env (deploy/README.md)"

say "Checking prerequisites"
for bin in docker openssl curl; do
  command -v "$bin" >/dev/null || die "$bin is not installed"
done
docker compose version >/dev/null 2>&1 || die "docker compose (v2) is not available"
[ "$(id -u)" -ne 0 ] || die "run as the deploy user (ubuntu), not root"
if [ ! -f "$DEST/.env" ] || [ ! -f "$DEST/secrets/nats-gateway.nk" ]; then
  die "$DEST is not set up (deploy/setup-host.sh)"
fi
cd "$DEST"
ENV_FILE="$DEST/.env"
compose() { docker compose --profile notifications "$@"; }
compose config --services 2>/dev/null | grep -qx notifications \
  || die "$DEST/compose.yml has no notifications service: deploy the release with it first"
grep -qF "nkey: \$NATS_NOTIFICATIONS_NKEY" "$DEST/nats/nats.conf" \
  || die "$DEST/nats/nats.conf has no notifications user: deploy the release with it first"

# env_get NAME: the value of NAME in .env (the last one), or empty.
env_get() { sed -n "s/^$1=//p" "$ENV_FILE" | tail -n1; }
# env_set NAME VALUE: write NAME=VALUE (not a secret: it is printed).
env_set() {
  if [ "$(env_get "$1")" = "$2" ] && grep -q "^$1=" "$ENV_FILE"; then
    info "$1 unchanged"
  else
    sed -i "/^$1=/d" "$ENV_FILE"
    printf '%s=%s\n' "$1" "$2" >> "$ENV_FILE"
    info "$1=$2 written"
  fi
}

say "The settings in .env"
CHANNELS=$(env_get NOTIFY_CHANNELS)
case "$CHANNELS" in
  email) ;;
  "") info "NOTIFY_CHANNELS is empty: the service starts and sends nothing" ;;
  *) die "NOTIFY_CHANNELS must be email or empty on the demo" ;;
esac
if [ "$CHANNELS" = email ]; then
  for v in NOTIFY_SMTP_HOST NOTIFY_SMTP_PORT NOTIFY_SMTP_USERNAME NOTIFY_SMTP_FROM NOTIFY_SMTP_REPLY_TO CORE_APP_URL; do
    [ -n "$(env_get "$v")" ] || die "$v is not set in $ENV_FILE (deploy/README.md, \"Turn on email for the demo\")"
  done
  case "$(env_get NOTIFY_SMTP_PORT)" in 465|587) ;; *) die "NOTIFY_SMTP_PORT must be 465 or 587" ;; esac
  case "$(env_get CORE_APP_URL)" in https://?*) ;; *) die "CORE_APP_URL must be the app's https address" ;; esac
fi
grep -q '^NOTIFY_SMTP_PASSWORD=' "$ENV_FILE" \
  && die "NOTIFY_SMTP_PASSWORD is in .env: delete that line; the password goes only in secrets/smtp_password"
for v in NOTIFY_SMTP_HOST NOTIFY_SMTP_PORT NOTIFY_SMTP_USERNAME NOTIFY_SMTP_FROM NOTIFY_SMTP_FROM_NAME NOTIFY_SMTP_REPLY_TO \
         CORE_APP_URL CORE_DELIVERY CORE_NOTIFY_ENABLED; do
  info "$v=$(env_get "$v")"
done
if [ "$(env_get CORE_DELIVERY)" = notify ] && [ "$(env_get CORE_NOTIFY_ENABLED)" != true ]; then
  die "CORE_DELIVERY=notify needs CORE_NOTIFY_ENABLED=true (core refuses to start otherwise)"
fi

say "Secrets in $DEST/secrets (each mode 400, owned by uid $APP_UID)"
mkdir -p "$DEST/secrets"
chmod 700 "$DEST/secrets"
# owned_ok FILE: secrets/FILE is a regular file owned by the service's user, mode 400.
owned_ok() { [ -f "$DEST/secrets/$1" ] && [ "$(stat -c '%u %a' "$DEST/secrets/$1" 2>/dev/null)" = "$APP_UID 400" ]; }

if [ -d "$DEST/secrets/smtp_password" ]; then
  die "secrets/smtp_password is a folder (Docker made it when the file was missing): sudo rmdir it, then write the password (README)"
fi
if owned_ok smtp_password; then
  info "smtp_password: present, mode 400, uid $APP_UID"
elif [ "$CHANNELS" = email ]; then
  die "secrets/smtp_password is missing or not mode 400 owned by uid $APP_UID: write it as deploy/README.md says"
else
  die "secrets/smtp_password is missing: the service mounts it even with email off. Write it (README), or an unused placeholder line with sudo install -m 0400 -o $APP_UID"
fi

# The NATS NKey: made inside nats-box with no network; only the public key
# comes back. An existing seed is kept.
pub=$(docker run --rm --network none --user 0 --entrypoint sh -e U="$APP_UID" \
  -v "$DEST/secrets:/s" "$NATS_BOX_IMAGE" -c '
    set -eu; umask 077
    if [ -s /s/nats-notifications.nk ]; then
      echo "notifications NKey exists, kept" >&2
    else
      out=$(nsc generate nkey --user 2>/dev/null)
      printf "%s\n" "$out" | sed -n 1p > /s/nats-notifications.nk
      unset out
      echo "notifications NKey generated" >&2
    fi
    case "$(head -c 2 /s/nats-notifications.nk)" in SU) ;; *) echo "not a user seed" >&2; exit 1;; esac
    chown "$U:$U" /s/nats-notifications.nk; chmod 400 /s/nats-notifications.nk
    nats auth nkey show /s/nats-notifications.nk' 2> >(sed 's/^/    /' >&2))
case "$pub" in U?*) ;; *) die "could not read the notifications service's public key" ;; esac
owned_ok nats-notifications.nk || die "secrets/nats-notifications.nk has the wrong owner or mode"
NATS_KEY_CHANGED=0
[ "$(env_get NATS_NOTIFICATIONS_NKEY)" = "$pub" ] || NATS_KEY_CHANGED=1
env_set NATS_NOTIFICATIONS_NKEY "$pub"

# The database login's password: in .env, for core-migrate (which makes the
# login) and the service's NOTIFY_DATABASE_URL. Never printed.
if [ -z "$(env_get NOTIFY_DB_PASSWORD)" ]; then
  sed -i '/^NOTIFY_DB_PASSWORD=/d' "$ENV_FILE"
  printf 'NOTIFY_DB_PASSWORD=%s\n' "$(openssl rand -hex 24)" >> "$ENV_FILE"
  info "NOTIFY_DB_PASSWORD generated"
else
  info "NOTIFY_DB_PASSWORD already set, kept"
fi
env_set NOTIFY_ENABLED true
docker compose config --quiet || die "compose.yml and .env don't validate"

say "NATS and core"
if [ "$NATS_KEY_CHANGED" = 1 ] && [ -n "$(docker compose ps -q nats)" ]; then
  docker compose up -d --force-recreate --no-deps nats
  info "nats recreated with the notifications service's public key"
fi
# Runs core-migrate (which makes notify_app) and recreates core when its
# CORE_* settings changed, as a deploy does.
if ! docker compose up -d core; then
  docker compose logs --tail=60 core-migrate core
  die "core did not start"
fi
port=$(env_get CORE_HOST_PORT); port=${port:-8092}
ok=0
for _ in $(seq 1 24); do
  ready=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$port/api/v1/readyz" || echo 000)
  if [ "$ready" = 200 ]; then ok=1; break; fi
  sleep 5
done
[ "$ok" = 1 ] || { docker compose logs --tail=80 core; die "core is not ready (readyz=$ready)"; }
info "core ready (CORE_DELIVERY=$(env_get CORE_DELIVERY), CORE_NOTIFY_ENABLED=$(env_get CORE_NOTIFY_ENABLED))"

say "Starting the notifications service and its mail relay"
compose pull notifications mail-relay >/dev/null 2>&1 || info "pull skipped (images already here, or no registry login)"
compose up -d --force-recreate mail-relay notifications
started=0
for _ in $(seq 1 24); do
  if compose logs --no-log-prefix notifications 2>/dev/null | grep 'notifications serving' >/dev/null; then started=1; break; fi
  sleep 5
done
if [ "$started" != 1 ]; then
  compose logs --tail=60 notifications mail-relay
  die "the notifications service did not start (its log is above; it names a setting, never a value)"
fi
compose logs --no-log-prefix notifications 2>/dev/null | grep -E 'notifications serving|email by SMTP' | tail -n 2 | sed 's/^/    /'
say "Done. Send a test as deploy/README.md says (\"Send a test\")."

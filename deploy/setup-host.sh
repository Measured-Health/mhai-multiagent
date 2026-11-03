#!/usr/bin/env bash
# One-time setup of the backend demo on the preview instance. Safe to run
# again: it never replaces a secret, a key or a volume that is already there,
# and it says what it does at every step.
#
# Run as the deploy user (ubuntu), from a copy of this repo's deploy/ folder:
#   bash deploy/setup-host.sh
#
# It does not start anything, touch nginx, or touch any other container.
# What it makes, under /srv/mhai-backend:
#   compose.yml, nats/nats.conf   copied from deploy/
#   .env                     generated secrets, mode 600
#   secrets/nats-gateway.nk  the core service's NATS NKey seed, mode 400,
#                            owned by the container user (65532)
#   secrets/nats-gateway.pub its public key (also in .env as NATS_GATEWAY_NKEY)
# It needs sudo only to create /srv/mhai-backend; file ownership for the
# containers' users is set from throwaway containers (no network).
#   secrets/bff/nats-bff-clinical.nk, nats-bff-platform.nk
#                            each frontend BFF's NATS NKey seed, mode 400,
#                            owned by uid 1001 (mhai-web's container user);
#                            the folder is mode 750. The public keys are in
#                            .env (NATS_BFF_NKEY, NATS_BFF_PLATFORM_NKEY).
#                            An older NATS_BFF_NKEY_SEED in .env is moved
#                            into its file (same pair) and removed.
#   --rotate-bff clinical|platform|all   makes a new pair for that BFF
#                            (restart nats and the app afterwards)
# the Docker volumes mhai-backend-pgdata, -natsdata, -corekeys and
# -trustedkeys, and the
# internal Docker network mhai-bff that the frontend's containers join.

set -euo pipefail

DEST=${MHAI_BACKEND_DIR:-/srv/mhai-backend}
# Host ports: from the environment, else from .env, else the defaults.
CORE_HOST_PORT=${CORE_HOST_PORT:-}
NATS_HOST_PORT=${NATS_HOST_PORT:-}
NATS_IMAGE=${NATS_IMAGE:-nats:2.10.29-alpine}
NATS_BOX_IMAGE=${NATS_BOX_IMAGE:-natsio/nats-box:0.19.0}
POSTGRES_IMAGE=${POSTGRES_IMAGE:-postgres:16.15-alpine}
CORE_UID=65532   # distroless nonroot
NATS_UID=1000    # compose.yml: nats user
PG_UID=70        # postgres alpine image's postgres user
WEB_UID=1001     # mhai-web's containers: the nextjs user in both Dockerfiles
WEB_GID=1001     # its group (nodejs)
ROTATE_BFF=      # clinical, platform or all: --rotate-bff
ROTATED=0

usage() { echo "usage: bash setup-host.sh [--rotate-bff clinical|platform|all]" >&2; exit 2; }
while [ $# -gt 0 ]; do
  case "$1" in
    --rotate-bff) [ $# -ge 2 ] || usage; ROTATE_BFF=$2; shift 2 ;;
    -h|--help) usage ;;
    *) usage ;;
  esac
done
case "$ROTATE_BFF" in ""|clinical|platform|all) ;; *) usage ;; esac

SRC=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
OWNER=$(id -un)

say()  { printf '\n==> %s\n' "$*"; }
info() { printf '    %s\n' "$*"; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }

# A random value of letters and digits only: safe in a URL, in .env and in
# the NATS config, and over every minimum length the services check.
rand_hex() { openssl rand -hex "$1"; }

say "Checking prerequisites"
for bin in docker openssl ss; do
  command -v "$bin" >/dev/null || die "$bin is not installed"
done
docker compose version >/dev/null 2>&1 || die "docker compose (v2) is not available"
for f in compose.yml nats/nats.conf; do
  [ -f "$SRC/$f" ] || die "$SRC/$f is missing: run this from a copy of the repo's deploy/ folder"
done
[ "$(id -u)" -ne 0 ] || die "run as the deploy user (ubuntu), not root; it uses sudo only to create $DEST"
info "docker $(docker version --format '{{.Server.Version}}'), $(docker compose version --short)"

say "Folder $DEST"
if [ -d "$DEST" ]; then
  info "exists, kept"
else
  if ! mkdir -p "$DEST" 2>/dev/null; then
    sudo mkdir -p "$DEST"
    sudo chown "$OWNER:$OWNER" "$DEST"
  fi
  info "created, owner $OWNER"
fi
[ -w "$DEST" ] || die "$DEST is not writable by $OWNER"
chmod 750 "$DEST"
mkdir -p "$DEST/nats"
info "mode 750"

say "Config files (the deploy workflow refreshes these on every deploy)"
copy() {
  local from=$1 to=$2 mode=$3
  if [ -f "$to" ] && cmp -s "$from" "$to"; then
    info "$to unchanged"
  else
    install -m "$mode" "$from" "$to"
    info "$to written"
  fi
}
copy "$SRC/compose.yml"                   "$DEST/compose.yml"                   644
copy "$SRC/nats/nats.conf"                "$DEST/nats/nats.conf"                644

say "Secrets in $DEST/.env"
umask 077
ENV_FILE="$DEST/.env"
[ -f "$ENV_FILE" ] || { : > "$ENV_FILE"; info "created $ENV_FILE"; }
chmod 600 "$ENV_FILE"
add_secret() {
  local name=$1 value=$2
  if grep -q "^${name}=." "$ENV_FILE"; then
    info "$name already set, kept"
  else
    sed -i "/^${name}=/d" "$ENV_FILE"
    printf '%s=%s\n' "$name" "$value" >> "$ENV_FILE"
    info "$name generated"
  fi
}
add_secret POSTGRES_PASSWORD     "$(rand_hex 24)"
add_secret CORE_DB_PASSWORD      "$(rand_hex 24)"
add_secret PROVISION_DB_PASSWORD "$(rand_hex 24)"
add_secret MFA_ENCRYPTION_KEY    "$(rand_hex 32)"
# One key per BFF, comma-separated. The web BFF gets this one when it is
# wired to the backend (deploy/README.md).
add_secret BFF_SERVICE_KEYS      "$(rand_hex 32)"
info "mode $(stat -c '%a %U' "$ENV_FILE")"

say "NATS NKey for the core service ($DEST/secrets/nats-gateway.nk)"
mkdir -p "$DEST/secrets"
chmod 700 "$DEST/secrets"
# Inside a throwaway container (no network), so the seed is written straight
# to the file, owned by the core container's user, and never printed. nsc
# prints the seed, then the public key; the public key is kept beside it
# (it is not a secret) and is all that comes back. An existing seed is kept.
pub=$(docker run --rm --network none --user 0 --entrypoint sh \
  -v "$DEST/secrets:/s" "$NATS_BOX_IMAGE" -c '
    set -eu; umask 077
    if [ -s /s/nats-gateway.nk ]; then
      [ -s /s/nats-gateway.pub ] || { echo "seed without its public key: delete both files to make a new pair" >&2; exit 1; }
      echo "seed exists, kept" >&2
    else
      out=$(nsc generate nkey --user 2>/dev/null)
      printf "%s\n" "$out" | sed -n 1p > /s/nats-gateway.nk
      printf "%s\n" "$out" | sed -n 2p > /s/nats-gateway.pub
      echo "seed generated" >&2
    fi
    chown '"$CORE_UID:$CORE_UID"' /s/nats-gateway.nk
    chmod 400 /s/nats-gateway.nk
    chmod 444 /s/nats-gateway.pub
    case "$(head -c 2 /s/nats-gateway.nk)" in SU) ;; *) echo "the seed is not a user seed" >&2; exit 1;; esac
    cat /s/nats-gateway.pub' 2> >(sed 's/^/    /' >&2))
case "$pub" in U?*) ;; *) die "could not read the public key from the seed" ;; esac
if grep -qx "NATS_GATEWAY_NKEY=$pub" "$ENV_FILE"; then
  info "NATS_GATEWAY_NKEY matches the seed, kept"
else
  sed -i '/^NATS_GATEWAY_NKEY=/d' "$ENV_FILE"
  printf 'NATS_GATEWAY_NKEY=%s\n' "$pub" >> "$ENV_FILE"
  info "NATS_GATEWAY_NKEY (the public key) written to .env"
fi
info "seed: mode 400, owner uid $CORE_UID; the folder is mode 700"

say "NATS NKeys for the frontend's BFFs ($DEST/secrets/bff/)"
# One pair per BFF app. The public key goes in .env (NATS reads it); the seed
# goes only in its own file, mode 400, owned by uid $WEB_UID (the frontend
# container's user), and mhai-web's compose mounts that file as a secret. The
# seed is never in .env, an environment variable, an image or this output:
# it is made, moved and checked inside a throwaway container with no network,
# and only the public key comes back. The folder is mode 750.
mkdir -p "$DEST/secrets/bff"
bff_key() {
  local app=$1 pubvar=$2 legacyvar=$3 rotate=0 legacy="" pub
  if [ "$ROTATE_BFF" = all ] || [ "$ROTATE_BFF" = "$app" ]; then rotate=1; fi
  if [ -n "$legacyvar" ]; then
    legacy=$(sed -n "s/^${legacyvar}=\(SU.*\)$/\1/p" "$ENV_FILE" | tail -n1)
  fi
  pub=$(printf '%s' "$legacy" | docker run --rm -i --network none --user 0 --entrypoint sh \
    -e APP="$app" -e ROTATE="$rotate" -e DIR_OWNER="$(id -u):$WEB_GID" -e WEB_UID="$WEB_UID" -e WEB_GID="$WEB_GID" \
    -v "$DEST/secrets/bff:/s" "$NATS_BOX_IMAGE" -c '
      set -eu; umask 077
      f=/s/nats-bff-$APP.nk
      incoming=$(cat)
      if [ "$ROTATE" = 1 ] || [ ! -s "$f" ]; then
        if [ "$ROTATE" != 1 ] && [ -n "$incoming" ]; then
          printf "%s\n" "$incoming" > "$f"
          echo "seed moved from .env into its file (same key pair)" >&2
        else
          out=$(nsc generate nkey --user 2>/dev/null)
          printf "%s\n" "$out" | sed -n 1p > "$f"
          [ "$ROTATE" = 1 ] && echo "new pair made (rotated)" >&2 || echo "new pair made" >&2
          unset out
        fi
      else
        echo "seed file exists, kept" >&2
      fi
      unset incoming
      case "$(head -c 2 "$f")" in SU) ;; *) echo "the seed is not a user seed" >&2; exit 1;; esac
      chown "$WEB_UID:$WEB_GID" "$f"; chmod 400 "$f"
      chown "$DIR_OWNER" /s; chmod 750 /s
      nats auth nkey show "$f"' 2> >(sed 's/^/    /' >&2))
  case "$pub" in U?*) ;; *) die "could not read the $app BFF public key from its seed" ;; esac
  if grep -qx "${pubvar}=$pub" "$ENV_FILE"; then
    info "$pubvar matches the seed, kept"
  else
    sed -i "/^${pubvar}=/d" "$ENV_FILE"
    printf '%s=%s\n' "$pubvar" "$pub" >> "$ENV_FILE"
    info "$pubvar (the public key) written to .env"
    if [ "$rotate" = 1 ]; then ROTATED=1; fi
  fi
  if [ -n "$legacyvar" ] && grep -q "^${legacyvar}=" "$ENV_FILE"; then
    sed -i "/^${legacyvar}=/d" "$ENV_FILE"
    info "$legacyvar removed from .env"
  fi
  info "$app: seed mode 400, owner uid $WEB_UID"
}
bff_key clinical NATS_BFF_NKEY          NATS_BFF_NKEY_SEED
bff_key platform NATS_BFF_PLATFORM_NKEY ""
info "folder: mode 750"
if [ "$ROTATED" = 1 ]; then
  info "Rotated: restart nats and the BFF's app so both use the new pair:"
  info "  (cd $DEST && docker compose up -d --force-recreate nats), then restart the app"
fi

say "Docker network mhai-bff (shared with the frontend's containers)"
if docker network inspect mhai-bff >/dev/null 2>&1; then
  if [ "$(docker network inspect -f '{{.Internal}}' mhai-bff)" = true ]; then
    info "exists, internal, kept"
  else
    die "network mhai-bff exists but is not internal; remove it (docker network rm mhai-bff) and re-run"
  fi
else
  docker network create --internal --label org.measured-health.stack=mhai-backend mhai-bff >/dev/null
  info "created (internal: no route out)"
fi

say "Docker volumes"
volume() {
  local name=$1 uid=$2 image=$3
  if docker volume inspect "$name" >/dev/null 2>&1; then
    info "$name exists, kept"
  else
    docker volume create --label org.measured-health.stack=mhai-backend "$name" >/dev/null
    info "$name created"
  fi
  if [ -n "$uid" ]; then
    # Owned by the container's user, so the container needs no root.
    docker run --rm --network none --user 0 --entrypoint chown -v "$name:/v" "$image" "$uid:$uid" /v
    docker run --rm --network none --user 0 --entrypoint chmod -v "$name:/v" "$image" 700 /v
    info "$name owned by uid $uid, mode 700"
  fi
}
volume mhai-backend-pgdata   "$PG_UID"   "$POSTGRES_IMAGE"
volume mhai-backend-natsdata "$NATS_UID" "$NATS_IMAGE"
# Filled from the image (owned by 65532) on the core-keys container's first start.
volume mhai-backend-corekeys ""          ""
# The request-token trusted-keys file (public keys), filled the same way by
# the core-request-key container.
volume mhai-backend-trustedkeys ""       ""

say "Loopback ports"
check_port() {
  local var=$1 port=$2 def=$3
  [ -n "$port" ] || port=$(sed -n "s/^$var=\([0-9][0-9]*\)$/\1/p" "$ENV_FILE" | tail -n1)
  port=${port:-$def}
  case "$port" in *[!0-9]*|'') die "$var must be a port number" ;; esac
  if ! ss -Hltn "sport = :$port" | grep -q .; then
    info "127.0.0.1:$port ($var) free"
  elif docker ps --filter name='^mhai-backend-' --format '{{.Ports}}' | grep -q "127.0.0.1:$port->"; then
    info "127.0.0.1:$port ($var) held by this stack, fine"
  else
    die "port $port is in use on this host; re-run with $var=<free port>"
  fi
  if [ "$port" != "$def" ] && ! grep -qx "$var=$port" "$ENV_FILE"; then
    sed -i "/^$var=/d" "$ENV_FILE"
    printf '%s=%s\n' "$var" "$port" >> "$ENV_FILE"
    info "$var=$port written to .env"
  fi
}
check_port CORE_HOST_PORT "$CORE_HOST_PORT" 8092
check_port NATS_HOST_PORT "$NATS_HOST_PORT" 4222

say "Checking the compose file"
(cd "$DEST" && docker compose config --quiet)
info "compose.yml and .env are valid"

say "Done"
info "Nothing was started. Next: set the repository variable DEPLOY_ENABLED=true"
info "and run the Deploy backend demo workflow (deploy/README.md)."

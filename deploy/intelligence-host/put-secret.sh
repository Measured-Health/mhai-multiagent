#!/usr/bin/env bash
# Put one secret in place on the intelligence host (docs/deploy/remote-intelligence.md):
#
#   bash deploy/intelligence-host/put-secret.sh NAME < value
#
# NAME is one of pilot-cloud-api-key, model-store-reader-access-key,
# model-store-reader-secret-key, intelligence-db-password. The value is read
# from standard input (a hidden prompt on a terminal, or one line piped from a
# password manager), written into secrets/NAME inside a container with no
# network, mode 0400, owned by the pipeline image's user. Never an argument,
# never .env, never printed. Replaces a file already there.
set -euo pipefail
umask 077

DEST=${MHAI_INTELLIGENCE_DIR:-/srv/mhai-intelligence}
SRC=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=deploy/tls/split-lib.sh
. "$SRC/tls/split-lib.sh"
HELPER_IMAGE=${NATS_BOX_IMAGE:-natsio/nats-box:0.19.0}

[ $# -eq 1 ] || { echo "usage: bash put-secret.sh NAME < value" >&2; exit 2; }
NAME=$1
case "$NAME" in
  pilot-cloud-api-key|model-store-reader-access-key|model-store-reader-secret-key|intelligence-db-password) ;;
  *) die "NAME must be pilot-cloud-api-key, model-store-reader-access-key, model-store-reader-secret-key or intelligence-db-password" ;;
esac
[ -f "$DEST/.env" ] || die "$DEST is not set up: run enable-intelligence-host.sh init first"
APP_UID=$(sed -n 's/^PIPELINE_UID=//p' "$DEST/.env" | tail -n1)
[[ $APP_UID =~ ^[1-9][0-9]*$ ]] || die "PIPELINE_UID is missing from $DEST/.env: run init again"

V=''
if [ -t 0 ]; then
  read -r -s -p "    $NAME (hidden): " V || true
  printf '\n'
else
  IFS= read -r V || true
fi
[ -n "$V" ] || die "nothing on standard input"
case "$V" in *[[:space:]]*) V=''; die "the value has whitespace in it" ;; esac
case "$(printf '%s' "$V" | tr '[:upper:]' '[:lower:]')" in
  *changeme*|*change-me*|*placeholder*|*example*) V=''; die "the value is a placeholder" ;;
esac
if [ "$NAME" = intelligence-db-password ] && [ "${#V}" -lt 16 ]; then V=''; die "the password is shorter than 16 characters"; fi
[ "${#V}" -ge 16 ] || [ "$NAME" = model-store-reader-access-key ] || { V=''; die "the value is shorter than 16 characters"; }

mkdir -p "$DEST/secrets"
chmod 700 "$DEST/secrets"
printf '%s' "$V" | docker run --rm -i --network none --user 0 --entrypoint sh -e F="$NAME" -e U="$APP_UID" \
  -v "$DEST/secrets:/s" "$HELPER_IMAGE" -c '
    set -eu; umask 077
    t="/s/.$F.new"; trap "rm -f $t" EXIT
    cat > "$t"; [ -s "$t" ] || { echo "empty secret" >&2; exit 1; }
    chown "$U:$U" "$t"; chmod 400 "$t"; mv -f "$t" "/s/$F"'
V=''
[ "$(stat -c '%u %a' "$DEST/secrets/$NAME")" = "$APP_UID 400" ] || die "secrets/$NAME has the wrong owner or mode"
info "secrets/$NAME written (0400, uid $APP_UID; not shown)"

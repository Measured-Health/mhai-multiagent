#!/bin/sh
# Provision the model store on MinIO (tests and docker-compose.models.yml; test-only, never
# shipped). Runs in the pinned MinIO client image, which has mc and a POSIX shell.
#
#   - the bucket, with versioning turned on (the store works without it too: a bucket
#     without versioning records the version id "null", and every file's SHA-256 is
#     checked either way);
#   - two users, writer (promote, testset-build) and reader (the locked split, pin,
#     lock-check, rollback, the AI service), each with the policy of the same name from
#     policies/ (the bucket name filled in). Neither may delete an object;
#   - each user's random password, written to MODEL_STORE_SECRETS_DIR/<user> with mode
#     0600. A password already there is kept, so a re-run changes nothing.
#
# Environment: MINIO_URL, MINIO_ROOT_USER and MINIO_ROOT_PASSWORD (required);
# MODEL_STORE_BUCKET (mhai-models), MODEL_STORE_SECRETS_DIR (/secrets),
# MODEL_STORE_POLICY_DIR (policies/ next to this file).
#
# No password or key is ever echoed or put on a command line: they reach mc on stdin.
set -eu
umask 077

: "${MINIO_URL:?set MINIO_URL}"
: "${MINIO_ROOT_USER:?set MINIO_ROOT_USER}"
: "${MINIO_ROOT_PASSWORD:?set MINIO_ROOT_PASSWORD}"
BUCKET="${MODEL_STORE_BUCKET:-mhai-models}"
SECRETS="${MODEL_STORE_SECRETS_DIR:-/secrets}"
POLICIES="${MODEL_STORE_POLICY_DIR:-$(dirname "$0")/policies}"
ROLES="writer reader"

fail() {
  printf 'init-model-store: %s\n' "$1" >&2
  exit 1
}

printf '%s' "$BUCKET" | grep -Eqx '[a-z0-9][a-z0-9-]{1,61}[a-z0-9]' || fail "bad MODEL_STORE_BUCKET"
printf '%s' "$MINIO_URL" | grep -Eqx 'https?://[A-Za-z0-9.-]+(:[0-9]{1,5})?' || fail "bad MINIO_URL"
[ -d "$POLICIES" ] || fail "no policy directory"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
export MC_CONFIG_DIR="$WORK/mc"
mkdir -p "$MC_CONFIG_DIR" "$WORK/policies"

mkdir -p "$SECRETS"
chmod 700 "$SECRETS"
[ -w "$SECRETS" ] || fail "secrets directory not writable"

# The root credentials go to mc on stdin (mc asks for the access and secret key).
printf '%s\n%s\n' "$MINIO_ROOT_USER" "$MINIO_ROOT_PASSWORD" |
  mc alias set store "$MINIO_URL" >/dev/null || fail "cannot reach MinIO"

mc mb --ignore-existing "store/$BUCKET" >/dev/null || fail "cannot make the bucket"
mc version enable "store/$BUCKET" >/dev/null || fail "cannot enable versioning"

for role in $ROLES; do
  secret="$SECRETS/$role"
  if [ -L "$secret" ] || [ -L "$secret.tmp" ]; then
    fail "secret file for $role is a link"
  fi
  rm -f "$secret.tmp"
  if [ ! -s "$secret" ]; then
    # 20 random bytes as 40 hex characters (MinIO's secret key limit is 40).
    pw="$(head -c 20 /dev/urandom | od -An -tx1 | tr -d ' \n')"
    [ "${#pw}" -eq 40 ] || fail "cannot make a password"
    printf '%s' "$pw" >"$secret.tmp"
    chmod 600 "$secret.tmp"
    mv "$secret.tmp" "$secret"
  fi
  chmod 600 "$secret"
  grep -Eqx '[0-9a-f]{40}' "$secret" || fail "secret file for $role is not 40 hex characters"
  pw="$(cat "$secret")"
  printf '%s\n' "$pw" | mc admin user add store "$role" >/dev/null || fail "cannot add user $role"
  pw=""

  sed -e "s/@BUCKET@/$BUCKET/g" "$POLICIES/$role.json" >"$WORK/policies/$role.json"
  mc admin policy create store "mhai-$role" "$WORK/policies/$role.json" >/dev/null ||
    fail "cannot create policy $role"
  mc admin policy attach store "mhai-$role" --user "$role" >/dev/null 2>&1 ||
    mc admin user info store "$role" | grep -q "mhai-$role" ||
    fail "cannot attach policy $role"
done

printf 'init-model-store: bucket %s ready (versioning on, %s users)\n' "$BUCKET" "2"

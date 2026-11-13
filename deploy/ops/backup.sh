#!/usr/bin/env bash
# The nightly backup of the mhai database (deploy/README.md, "Operations").
# Run as root by mhai-backup.timer (install-ops.sh), or by hand:
#
#   sudo /usr/local/lib/mhai-ops/backup.sh
#
# What it does, in order, and it stops at the first failure:
#   1. checks the recipients file: age public keys only (age1... or ssh-...);
#      a private key there is refused, and never printed;
#   2. opens one read-only snapshot in the mhai Postgres (docker compose exec,
#      the mhai-backend project only) and, from that snapshot, dumps the
#      database (pg_dump -Fc) and counts every table's rows; dumps the roles
#      without their passwords (pg_dumpall --globals-only --no-role-passwords;
#      core-migrate resets the logins' passwords from .env);
#   3. encrypts each stream with age to the recipients as it is written
#      (nothing in plain text touches the disk), into BACKUP_DIR/.tmp-<ts>,
#      then SHA256SUMS, then renames it to BACKUP_DIR/<ts> (atomic);
#   4. rotates: KEEP_DAILY newest plus the newest of KEEP_WEEKLY ISO weeks;
#   5. copies the backup offsite when OFFSITE_ENV_FILE sets OFFSITE_BUCKET
#      (S3-compatible, rclone in a container; off by default);
#   6. writes STATE_DIR/backup.status (status=ok|failed, finished_at, ...),
#      which healthcheck.sh alerts on.
#
# The NATS JetStream store is not backed up: it holds nothing that isn't
# rebuilt or short-lived (deploy/README.md, "What is backed up").
# Exit 0 only when every step, offsite included when on, succeeded.
set -euo pipefail
umask 077
# shellcheck source-path=SCRIPTDIR source=lib.sh
. "$(dirname "$(readlink -f "$0")")/lib.sh"
ops_load_config

STATUS="$STATE_DIR/backup.status"
STEP=start
TS=$(date -u +%Y%m%dT%H%M%SZ)
TMP=''
DONE=0
OFFSITE=off
PSQL_PID='' FIFO_DIR=''

on_exit() {
  local rc=$?
  if [ -n "$PSQL_PID" ]; then kill "$PSQL_PID" 2>/dev/null || true; fi
  if [ -n "$FIFO_DIR" ]; then rm -rf -- "$FIFO_DIR"; fi
  if [ -n "$TMP" ] && [ -d "$TMP" ]; then rm -rf -- "$TMP"; fi
  if [ "$DONE" != 1 ]; then
    ops_write_status "$STATUS" status=failed "finished_at=$(date +%s)" "backup=" \
      "offsite=$OFFSITE" "message=failed at: $STEP"
    ops_log "BACKUP FAILED at: $STEP"
    [ "$rc" != 0 ] || rc=1
  fi
  exit "$rc"
}
die() { ops_log "error: $*"; exit 1; }

mkdir -p "$STATE_DIR"
chmod 700 "$STATE_DIR"
trap on_exit EXIT

STEP="lock"
exec 9>"$STATE_DIR/backup.lock"
flock -n 9 || die "another backup is running"

STEP="prerequisites"
for bin in docker age sha256sum flock; do
  command -v "$bin" >/dev/null || die "$bin is not installed"
done
[ -f "$MHAI_BACKEND_DIR/compose.yml" ] || die "no compose.yml in $MHAI_BACKEND_DIR"

STEP="recipients file"
[ -s "$BACKUP_RECIPIENTS_FILE" ] || die "no recipients file $BACKUP_RECIPIENTS_FILE (the age public key; deploy/README.md, 'Operations')"
if grep -qi 'AGE-SECRET-KEY' "$BACKUP_RECIPIENTS_FILE"; then
  die "$BACKUP_RECIPIENTS_FILE holds a private key: put only the public key (age1...) there, and keep the private key off this host"
fi
grep -Eq '^(age1[0-9a-z]+|ssh-(ed25519|rsa) )' "$BACKUP_RECIPIENTS_FILE" \
  || die "$BACKUP_RECIPIENTS_FILE has no age public key (age1...)"

STEP="backup directory"
mkdir -p "$BACKUP_DIR"
chmod 700 "$BACKUP_DIR"
# A run that crashed earlier left its temp directory; it is never a backup.
rm -rf -- "${BACKUP_DIR:?}"/.tmp-*
TMP="$BACKUP_DIR/.tmp-$TS"
mkdir "$TMP"

enc() { age -R "$BACKUP_RECIPIENTS_FILE" -o "$1"; }
pgx() { ops_compose exec -T postgres "$@"; }

STEP="snapshot"
# One psql session holds a REPEATABLE READ snapshot open; pg_dump uses it
# (--snapshot) and the counts run in it, so both see the same rows.
# SIGPIPE ignored: a write to a psql that has gone fails (and the run with
# it, through the EXIT trap) instead of killing the script silently.
trap '' PIPE
# psql runs in the background between two FIFOs (in STATE_DIR, 0700); our
# ends stay open even if it exits early, so a failure is a read that ends.
FIFO_DIR=$(mktemp -d "$STATE_DIR/.psql.XXXXXX")
mkfifo "$FIFO_DIR/in" "$FIFO_DIR/out"
ops_compose exec -T postgres psql -U pgadmin -d mhai -qtAX -v ON_ERROR_STOP=1 <"$FIFO_DIR/in" >"$FIFO_DIR/out" 2>&1 &
PSQL_PID=$!
exec {TO_PSQL}>"$FIFO_DIR/in" {FROM_PSQL}<"$FIFO_DIR/out"
printf 'BEGIN ISOLATION LEVEL REPEATABLE READ, READ ONLY;\nSELECT pg_export_snapshot();\n' >&"$TO_PSQL"
SNAP=''
read -r -t 60 SNAP <&"$FROM_PSQL" || true
printf '%s' "$SNAP" | grep -Eq '^[0-9A-F]+-[0-9A-F]+(-[0-9]+)?$' || die "could not open a snapshot in Postgres (is it up?)"

STEP="dump"
pgx pg_dump -U pgadmin -d mhai -Fc --snapshot="$SNAP" | enc "$TMP/mhai.dump.age"
pgx pg_dumpall -U pgadmin --globals-only --no-role-passwords | enc "$TMP/globals.sql.age"

STEP="row counts"
printf "%s\nSELECT version_id FROM (%s) g(version_id);\nSELECT current_setting('server_version');\nSELECT '__END__';\n" \
  "$OPS_COUNTS_SQL" "${OPS_GOOSE_SQL%;}" >&"$TO_PSQL"
LINES=()
while IFS= read -r -t 600 line <&"$FROM_PSQL"; do
  [ "$line" = __END__ ] && break
  LINES+=("$line")
done
[ "${line:-}" = __END__ ] || die "the row counts did not finish"
n=${#LINES[@]}
[ "$n" -ge 3 ] || die "the row counts came back empty"
GOOSE=${LINES[n-2]}
PGVER=${LINES[n-1]}
printf '%s' "$GOOSE" | grep -Eq '^[0-9]+$' || die "could not read the migration version"
printf 'COMMIT;\n\\q\n' >&"$TO_PSQL"
exec {TO_PSQL}>&- {FROM_PSQL}<&-
wait "$PSQL_PID" 2>/dev/null || true
PSQL_PID=''
rm -rf -- "$FIFO_DIR"; FIFO_DIR=''

STEP="manifest"
{
  printf 'format=1\nbackup=%s\ncreated_at=%s\ngoose_version=%s\nserver_version=%s\n' \
    "$TS" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$GOOSE" "$PGVER"
  printf 'count %s\n' "${LINES[@]:0:n-2}"
} | enc "$TMP/manifest.age"

STEP="checksums"
(cd "$TMP" && sha256sum globals.sql.age mhai.dump.age manifest.age > SHA256SUMS)
chmod 600 "$TMP"/*
mv "$TMP" "$BACKUP_DIR/$TS"
TMP=''
SIZE=$(du -sb "$BACKUP_DIR/$TS" | cut -f1)
ops_log "backup $TS written ($SIZE bytes, migration $GOOSE, $((n - 2)) tables)"

STEP="rotation"
ops_rotate "$BACKUP_DIR" "$KEEP_DAILY" "$KEEP_WEEKLY"

# The offsite copy: on only when OFFSITE_ENV_FILE sets OFFSITE_BUCKET. Its
# values are read in a subshell into a 0600 env file for rclone, never into
# this script's environment or onto a command line.
offsite_env() (
  for v in OFFSITE_ENDPOINT OFFSITE_BUCKET OFFSITE_PREFIX OFFSITE_REGION OFFSITE_PROVIDER \
           OFFSITE_ACCESS_KEY_ID OFFSITE_SECRET_ACCESS_KEY OFFSITE_DOCKER_NETWORK; do unset "$v"; done
  ops_read_env "$OFFSITE_ENV_FILE"
  case "${1:-}" in
    bucket) printf '%s' "${OFFSITE_BUCKET:-}" ;;
    target) printf '%s/%s' "$OFFSITE_BUCKET" "${OFFSITE_PREFIX:-}" ;;
    network) printf '%s' "${OFFSITE_DOCKER_NETWORK:-bridge}" ;;
    rclone)
      : "${OFFSITE_ENDPOINT:?OFFSITE_ENDPOINT is not set}" "${OFFSITE_ACCESS_KEY_ID:?OFFSITE_ACCESS_KEY_ID is not set}" \
        "${OFFSITE_SECRET_ACCESS_KEY:?OFFSITE_SECRET_ACCESS_KEY is not set}"
      printf 'RCLONE_CONFIG_OFFSITE_TYPE=s3\nRCLONE_CONFIG_OFFSITE_PROVIDER=%s\n' "${OFFSITE_PROVIDER:-Other}"
      printf 'RCLONE_CONFIG_OFFSITE_ENDPOINT=%s\nRCLONE_CONFIG_OFFSITE_REGION=%s\n' "$OFFSITE_ENDPOINT" "${OFFSITE_REGION:-us-east-1}"
      printf 'RCLONE_CONFIG_OFFSITE_ACCESS_KEY_ID=%s\n' "$OFFSITE_ACCESS_KEY_ID"
      printf 'RCLONE_CONFIG_OFFSITE_SECRET_ACCESS_KEY=%s\n' "$OFFSITE_SECRET_ACCESS_KEY"
      # Write-only keys work: rclone doesn't list or create the bucket.
      printf 'RCLONE_CONFIG_OFFSITE_NO_CHECK_BUCKET=true\n' ;;
  esac
)

if [ -f "$OFFSITE_ENV_FILE" ] && [ -n "$(offsite_env bucket)" ]; then
  STEP="offsite copy"
  OFFSITE=failed
  RENV=$(mktemp "$STATE_DIR/.rclone-env.XXXXXX")
  if offsite_env rclone > "$RENV" \
     && docker run --rm --network "$(offsite_env network)" --env-file "$RENV" \
          -v "$BACKUP_DIR/$TS:/data:ro" "$RCLONE_IMAGE" \
          copy /data "offsite:$(offsite_env target)$TS" --retries 3 --low-level-retries 3 -q 2>&1 | tail -n 5 >&2; then
    OFFSITE=ok
  fi
  rm -f "$RENV"
  if [ "$OFFSITE" != ok ]; then
    ops_write_status "$STATUS" status=failed "finished_at=$(date +%s)" "backup=$TS" "size_bytes=$SIZE" \
      offsite=failed "message=offsite copy failed (the local backup $TS is kept)"
    DONE=1
    ops_log "BACKUP FAILED: offsite copy failed; the local backup $TS is kept"
    exit 1
  fi
  ops_log "offsite copy done"
fi

ops_write_status "$STATUS" status=ok "finished_at=$(date +%s)" "backup=$TS" "size_bytes=$SIZE" \
  "offsite=$OFFSITE" "goose_version=$GOOSE" "message=ok"
DONE=1
ops_log "backup ok"

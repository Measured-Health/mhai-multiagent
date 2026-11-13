#!/usr/bin/env bash
# The weekly restore test (deploy/README.md, "Operations"): restore the
# newest backup into a throwaway Postgres and check it. Run by the operator,
# as root, with the age private key on standard input (it is read into
# memory only: never written to disk, never an argument, never printed):
#
#   pass show mhai/backup-age-key | sudo /usr/local/lib/mhai-ops/restore-test.sh --identity-stdin
#   sudo /usr/local/lib/mhai-ops/restore-test.sh --identity-stdin     # paste it (hidden)
#
# Off the host (a copy of one backup directory, on the operator's machine):
#   restore-test.sh --identity /path/to/key.txt --backup ./20261113T013000Z
#
# Checks, and the first failure stops it:
#   1. SHA256SUMS of the encrypted files (a damaged or altered backup);
#   2. every file decrypts with the key;
#   3. the roles and the database restore into a fresh Postgres
#      (RESTORE_POSTGRES_IMAGE, on an internal network with no route out,
#      removed at the end with its volume) with pg_restore --exit-on-error;
#   4. every table's row count equals the count taken in the backup's snapshot;
#   5. goose: the applied version equals the backup's, and `core migrate
#      status` (CORE_IMAGE) reads the restored database.
# Prints PASS or FAIL, writes STATE_DIR/restore.status (healthcheck.sh
# alerts when it is older than RESTORE_MAX_AGE_DAYS), exits 0 or 1.
# Nothing from the restored data is printed: only counts of tables.
set -euo pipefail
umask 077
# shellcheck source-path=SCRIPTDIR source=lib.sh
. "$(dirname "$(readlink -f "$0")")/lib.sh"
ops_load_config

usage() { echo "usage: restore-test.sh (--identity-stdin | --identity FILE) [--backup DIR]" >&2; exit 2; }
IDENT_FILE='' FROM_STDIN=0 BACKUP=''
while [ $# -gt 0 ]; do
  case "$1" in
    --identity) [ $# -ge 2 ] || usage; IDENT_FILE=$2; shift 2 ;;
    --identity-stdin) FROM_STDIN=1; shift ;;
    --backup) [ $# -ge 2 ] || usage; BACKUP=$2; shift 2 ;;
    *) usage ;;
  esac
done
[ "$FROM_STDIN" = 1 ] || [ -n "$IDENT_FILE" ] || usage

STATUS="$STATE_DIR/restore.status"
NAME=restore-test-$$-$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')
CTR="mhai-$NAME"
NET="mhai-$NAME"
WORK=''
RESULT=failed
REASON='did not finish'
BNAME=''

cleanup() {
  docker rm -fv "$CTR" >/dev/null 2>&1 || true
  docker network rm "$NET" >/dev/null 2>&1 || true
  if [ -n "$WORK" ]; then rm -rf -- "$WORK"; fi
  if [ "$RESULT" = ok ]; then
    echo "PASS: backup $BNAME restored and checked"
  else
    echo "FAIL: $REASON"
  fi
  if mkdir -p "$STATE_DIR" 2>/dev/null; then
    ops_write_status "$STATUS" "status=$RESULT" "finished_at=$(date +%s)" "backup=$BNAME" "message=${REASON//$'\n'/ }" || true
  fi
  [ "$RESULT" = ok ] || exit 1
}
trap cleanup EXIT
fail() { REASON=$*; exit 1; }

for bin in docker age sha256sum; do command -v "$bin" >/dev/null || fail "$bin is not installed"; done

# The key, into memory only.
IDENT=''
if [ "$FROM_STDIN" = 1 ]; then
  if [ -t 0 ]; then
    read -r -s -p "Paste the age private key (AGE-SECRET-KEY-..., hidden): " IDENT || true
    printf '\n' >&2
  else
    IDENT=$(cat)
  fi
else
  [ -r "$IDENT_FILE" ] || fail "cannot read the identity file"
  IDENT=$(cat -- "$IDENT_FILE")
fi
IDENT=$(printf '%s\n' "$IDENT" | grep -v '^#' | grep -E '^AGE-SECRET-KEY-' || true)
[ -n "$IDENT" ] || fail "no age private key (AGE-SECRET-KEY-...) given"
dec() { age -d -i <(printf '%s\n' "$IDENT") -- "$1"; }

# The backup: the newest in BACKUP_DIR unless --backup names one.
if [ -z "$BACKUP" ]; then
  BNAME=$(ops_list_backups "$BACKUP_DIR" | tail -n1)
  [ -n "$BNAME" ] || fail "no backup in $BACKUP_DIR"
  BACKUP="$BACKUP_DIR/$BNAME"
else
  BNAME=$(basename "$BACKUP")
fi
echo "backup: $BNAME"

(cd "$BACKUP" && sha256sum -c --quiet SHA256SUMS >/dev/null 2>&1) || fail "checksum mismatch or missing file in $BNAME (SHA256SUMS)"
echo "checksums: ok"

MANIFEST=$(dec "$BACKUP/manifest.age" 2>/dev/null) || fail "could not decrypt the manifest (wrong key?)"
WANT_GOOSE=$(printf '%s\n' "$MANIFEST" | sed -n 's/^goose_version=//p')
WANT_COUNTS=$(printf '%s\n' "$MANIFEST" | sed -n 's/^count //p')
if [ -z "$WANT_GOOSE" ] || [ -z "$WANT_COUNTS" ]; then fail "the manifest is incomplete"; fi
echo "manifest: migration $WANT_GOOSE, $(printf '%s\n' "$WANT_COUNTS" | wc -l) tables"

# The throwaway Postgres, on its own internal network.
WORK=$(mktemp -d)
PW=r$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')
printf 'POSTGRES_PASSWORD=%s\n' "$PW" > "$WORK/pg.env"
printf 'DATABASE_URL=postgres://pgadmin:%s@restore-pg:5432/mhai?sslmode=disable\n' "$PW" > "$WORK/core.env"
docker network create --internal --label org.measured-health.stack=mhai-restore-test "$NET" >/dev/null \
  || fail "could not create the restore network"
docker run -d --name "$CTR" --label org.measured-health.stack=mhai-restore-test \
  --network "$NET" --network-alias restore-pg --memory 512m --shm-size 64m \
  --env-file "$WORK/pg.env" -e POSTGRES_DB=mhai -e POSTGRES_USER=pgadmin \
  "$RESTORE_POSTGRES_IMAGE" >/dev/null || fail "could not start the throwaway Postgres"
ready=0
for _ in $(seq 1 90); do
  if docker exec "$CTR" pg_isready -h 127.0.0.1 -U pgadmin -d mhai >/dev/null 2>&1; then ready=1; break; fi
  sleep 1
done
[ "$ready" = 1 ] || fail "the throwaway Postgres did not start"

# The roles first (the dump's owners and grants name them). The owner
# exists already, so "already exists" is the one error allowed.
errs=$(dec "$BACKUP/globals.sql.age" | docker exec -i "$CTR" psql -X -q -U pgadmin -d postgres 2>&1 >/dev/null \
  | grep 'ERROR' | grep -v 'already exists' || true)
[ -z "$errs" ] || fail "restoring the roles failed: $(printf '%s' "$errs" | head -n 3)"
set +e
dec "$BACKUP/mhai.dump.age" | docker exec -i "$CTR" pg_restore -U pgadmin -d mhai --exit-on-error 2>"$WORK/restore.err"
rc=$?
set -e
[ "$rc" = 0 ] || fail "pg_restore failed: $(head -n 3 "$WORK/restore.err")"
echo "restore: ok"

GOT_COUNTS=$(docker exec "$CTR" psql -X -qtA -U pgadmin -d mhai -v ON_ERROR_STOP=1 -c "$OPS_COUNTS_SQL") \
  || fail "could not count the restored rows"
if [ "$GOT_COUNTS" != "$WANT_COUNTS" ]; then
  diffs=$(diff <(printf '%s\n' "$WANT_COUNTS") <(printf '%s\n' "$GOT_COUNTS") | grep '^[<>]' | head -n 5 | tr '\n' ' ')
  fail "row counts differ from the backup's snapshot: $diffs"
fi
total=$(printf '%s\n' "$GOT_COUNTS" | awk '{s += $2} END {print s + 0}')
echo "row counts: $(printf '%s\n' "$GOT_COUNTS" | wc -l) tables, $total rows, all equal to the backup's"

GOT_GOOSE=$(docker exec "$CTR" psql -X -qtA -U pgadmin -d mhai -c "$OPS_GOOSE_SQL") || fail "no goose_db_version table"
[ "$GOT_GOOSE" = "$WANT_GOOSE" ] || fail "migration version $GOT_GOOSE, the backup says $WANT_GOOSE"
st=$(docker run --rm --network "$NET" --env-file "$WORK/core.env" "$CORE_IMAGE" migrate status 2>&1) \
  || fail "core migrate status failed on the restored database: $(printf '%s' "$st" | tail -n 2)"
applied=$(printf '%s\n' "$st" | grep -c ' applied ' || true)
pending=$(printf '%s\n' "$st" | grep -c ' pending ' || true)
echo "goose status: $applied applied, $pending pending in $CORE_IMAGE (version $GOT_GOOSE)"
[ "$applied" -gt 0 ] || fail "goose status shows no applied migration"

RESULT=ok
REASON=ok

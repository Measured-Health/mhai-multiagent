#!/usr/bin/env bash
# Tests for deploy/ops/ (the operations baseline): backup -> restore test
# against throwaway Postgres containers, the rotation, the offsite copy (to
# a throwaway MinIO, test-only), and the health check's alert to a fake
# webhook. Needs Docker, age (age-keygen), curl and the core image
# (mhai-core:local, `docker build -t mhai-core:local -f services/core/Dockerfile services`).
# Synthetic data only; every container, network and file is removed at the end.
# The age key pair is a throwaway one made here, never printed.
#
#   .github/scripts/ops-test.sh
set -uo pipefail
root=$(cd "$(dirname "$0")/../.." && pwd)
ops="$root/deploy/ops"
CORE_IMAGE=${CORE_IMAGE:-mhai-core:local}
PG_IMAGE=postgres:16.15-alpine
# Test-only (AGPL, never shipped); the same digests as docker-compose.models.yml.
MINIO_IMAGE=bitnamilegacy/minio@sha256:6dabb4a2088c9a79908de3bc05f4586c23ad2182c8908e7e3acbf61c1467fb20
MC_IMAGE=bitnamilegacy/minio-client@sha256:73bd39f7899a0cef12b8dd5df13aa93a3ed1aaa44236542442e9ac76819ac158
PY_IMAGE=python:3.12-alpine

dir=$(mktemp -d)
chmod 700 "$dir"
# A random run id, not $$: inside a container the PID is small and repeats,
# so two runs on one Docker host could share names and remove each other's
# containers on exit.
run_id=$(od -An -N6 -tx1 /dev/urandom | tr -d ' \n')
project=mhai-ops-test-$run_id
net=mhai-ops-test-net-$run_id
pw=k$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')
cat > "$dir/compose.yml" <<YAML
name: $project
services:
  postgres:
    image: $PG_IMAGE
    environment: { POSTGRES_DB: mhai, POSTGRES_USER: pgadmin, POSTGRES_PASSWORD: "$pw" }
YAML
compose="docker compose -f $dir/compose.yml"
cleanup() {
  $compose down -v >/dev/null 2>&1
  docker rm -fv "$project-minio" "$project-hook" >/dev/null 2>&1
  docker network rm "$net" >/dev/null 2>&1
  rm -rf "$dir"
}
trap cleanup EXIT
failures=0
ok()   { echo "ok   $*"; }
fail() { echo "FAIL $*"; failures=$((failures + 1)); }
# backups: the finished backups' names, oldest first. tmp_left: a temp dir is left.
backups() { local p; for p in "$dir/backups"/*; do if [[ ${p##*/} =~ ^[0-9]{8}T[0-9]{6}Z$ ]]; then echo "${p##*/}"; fi; done; }
tmp_left() { compgen -G "$dir/backups/.tmp-*" >/dev/null; }
# expect_grep NAME PATTERN TEXT: ok when TEXT matches PATTERN (grep -E).
expect_grep() { if printf '%s' "$3" | grep -Eq -- "$2"; then ok "$1"; else fail "$1: $3"; fi; }
check() { # name, then a command that must succeed
  local name=$1; shift
  if "$@"; then ok "$name"; else fail "$name"; fi
}

# --- the config every script reads (OPS_CONFIG) -----------------------------
mkdir -p "$dir/etc" "$dir/backups" "$dir/state"
age-keygen -o "$dir/etc/identity.txt" 2>/dev/null
grep -v '^#' "$dir/etc/identity.txt" | age-keygen -y > "$dir/etc/backup-recipients.txt"
cat > "$dir/etc/ops.env" <<ENV
MHAI_BACKEND_DIR=$dir
BACKUP_DIR=$dir/backups
STATE_DIR=$dir/state
BACKUP_RECIPIENTS_FILE=$dir/etc/backup-recipients.txt
OFFSITE_ENV_FILE=$dir/etc/offsite.env
ALERT_WEBHOOK_FILE=$dir/etc/alert-webhook-url
CORE_IMAGE=$CORE_IMAGE
RESTORE_POSTGRES_IMAGE=$PG_IMAGE
HOST_LABEL=ops-test
ENV
export OPS_CONFIG="$dir/etc/ops.env"

# --- 1. rotation (no Docker) ---------------------------------------------------
rot="$dir/rot"; mkdir -p "$rot"
for i in $(seq 0 39); do mkdir "$rot/$(date -u -d "2026-11-13 - $i days" +%Y%m%dT013000Z)"; done
mkdir "$rot/not-a-backup" "$rot/.tmp-20261113T013000Z"
bash -c '. "$1"; ops_rotate "$2" 7 4 >/dev/null 2>&1' _ "$ops/lib.sh" "$rot"
got=$(for p in "$rot"/*; do printf '%s ' "${p##*/}"; done)
want="20261025T013000Z 20261101T013000Z 20261107T013000Z 20261108T013000Z 20261109T013000Z 20261110T013000Z 20261111T013000Z 20261112T013000Z 20261113T013000Z not-a-backup "
if [ "$got" = "$want" ]; then ok "rotation keeps 7 daily + the newest of 4 ISO weeks, leaves other names"; else fail "rotation: got [$got] want [$want]"; fi

# --- 2. a database with core's migrations and synthetic rows ---------------
$compose up -d postgres >/dev/null 2>&1 || { echo "FAIL could not start postgres"; exit 1; }
for _ in $(seq 1 60); do $compose exec -T postgres pg_isready -h 127.0.0.1 -U pgadmin -d mhai >/dev/null 2>&1 && break; sleep 1; done
docker run --rm --network "${project}_default" \
  -e DATABASE_URL="postgres://pgadmin:$pw@postgres:5432/mhai?sslmode=disable" "$CORE_IMAGE" migrate up >/dev/null \
  || { echo "FAIL migrations"; exit 1; }
psql() { $compose exec -T postgres psql -U pgadmin -d mhai -v ON_ERROR_STOP=1 -qtAc "$1"; }
marker=SyntheticOpsMarker$$
psql "INSERT INTO hospitals (id, name, short_code, time_zone) VALUES
        ('00000000-0000-4000-8000-00000000ab01', '$marker Hospital', 'OPS', 'UTC'),
        ('00000000-0000-4000-8000-00000000ab02', 'Synthetic Two', 'OPS2', 'UTC')" >/dev/null \
  || { echo "FAIL synthetic rows"; exit 1; }

# --- 3. backup ------------------------------------------------------------------
out=$(bash "$ops/backup.sh" 2>&1); rc=$?
if [ "$rc" = 0 ]; then ok "backup exits 0"; else fail "backup exit $rc: $out"; fi
latest=$(backups | tail -n1)
check "backup directory made" test -n "$latest"
for f in globals.sql.age mhai.dump.age manifest.age SHA256SUMS; do
  check "backup has $f" test -s "$dir/backups/$latest/$f"
done
check "files are age-encrypted" sh -c "head -c 21 '$dir/backups/$latest/mhai.dump.age' | grep -q 'age-encryption.org/v1'"
if grep -rqa "$marker" "$dir/backups"; then fail "plaintext synthetic data found in the backup directory"; else ok "no plaintext data in the backup directory"; fi
check "backup files are not world-readable" sh -c "[ -z \"\$(find '$dir/backups/$latest' -perm /077 -type f)\" ]"
check "status file says ok" grep -qx 'status=ok' "$dir/state/backup.status"
check "status names the backup" grep -qx "backup=$latest" "$dir/state/backup.status"
if tmp_left; then fail "temp directory left"; else ok "no leftover temp directory"; fi
if ( cd "$dir/backups/$latest" && sha256sum -c --quiet SHA256SUMS ); then ok "checksums verify"; else fail "checksums"; fi

# --- 4. restore test ------------------------------------------------------------
out=$(bash "$ops/restore-test.sh" --identity-stdin < "$dir/etc/identity.txt" 2>&1); rc=$?
if [ "$rc" = 0 ] && printf '%s' "$out" | grep -q '^PASS'; then ok "restore test passes"; else fail "restore test exit $rc: $out"; fi
expect_grep "restore test reports goose status" 'goose status: [0-9]+ applied' "$out"
if printf '%s' "$out" | grep -q 'AGE-SECRET-KEY'; then fail "restore test printed the identity"; else ok "identity not printed"; fi
check "restore status ok" grep -qx 'status=ok' "$dir/state/restore.status"
check "no restore container left" sh -c "[ -z \"\$(docker ps -aq --filter label=org.measured-health.stack=mhai-restore-test)\" ]"

# The wrong key fails.
age-keygen -o "$dir/other-identity.txt" 2>/dev/null
out=$(bash "$ops/restore-test.sh" --identity-stdin < "$dir/other-identity.txt" 2>&1); rc=$?
if [ "$rc" != 0 ] && printf '%s' "$out" | grep -q '^FAIL'; then ok "restore test fails with the wrong key"; else fail "wrong key: exit $rc: $out"; fi
check "restore status failed" grep -qx 'status=failed' "$dir/state/restore.status"
rm -f "$dir/other-identity.txt"

# A tampered backup fails before anything is decrypted.
second=$(date -u -d '+1 minute' +%Y%m%dT%H%M%SZ)
cp -a "$dir/backups/$latest" "$dir/backups/$second"
printf 'x' >> "$dir/backups/$second/mhai.dump.age"
out=$(bash "$ops/restore-test.sh" --identity-stdin < "$dir/etc/identity.txt" 2>&1); rc=$?
if [ "$rc" != 0 ] && printf '%s' "$out" | grep -qi 'checksum'; then ok "restore test fails on a tampered backup"; else fail "tampered: exit $rc: $out"; fi
rm -rf "${dir:?}/backups/$second"
# A good run last, so the health check below sees a passing restore test.
bash "$ops/restore-test.sh" --identity-stdin < "$dir/etc/identity.txt" >/dev/null 2>&1 || fail "restore test after the failure cases"

# --- 5. backup failures are loud ------------------------------------------------
mv "$dir/etc/backup-recipients.txt" "$dir/etc/recipients.bak"
out=$(bash "$ops/backup.sh" 2>&1); rc=$?
if [ "$rc" != 0 ]; then ok "backup without a recipients file fails"; else fail "no recipients: exit 0"; fi
check "status failed (no recipients)" grep -qx 'status=failed' "$dir/state/backup.status"
grep -v '^#' "$dir/etc/identity.txt" > "$dir/etc/backup-recipients.txt"
out=$(bash "$ops/backup.sh" 2>&1); rc=$?
if [ "$rc" != 0 ] && printf '%s' "$out" | grep -qi 'private key'; then ok "backup refuses a private key as recipients"; else fail "private key accepted: exit $rc"; fi
if printf '%s' "$out" | grep -q 'AGE-SECRET-KEY'; then fail "backup printed the private key"; else ok "private key not printed"; fi
mv -f "$dir/etc/recipients.bak" "$dir/etc/backup-recipients.txt"
$compose stop postgres >/dev/null 2>&1
rm -f "$dir/state/backup.status"
out=$(bash "$ops/backup.sh" 2>&1); rc=$?
if [ "$rc" != 0 ]; then ok "backup with Postgres down fails"; else fail "postgres down: exit 0"; fi
check "status failed (postgres down)" grep -qx 'status=failed' "$dir/state/backup.status"
if tmp_left; then fail "a failed run left a temp directory"; else ok "a failed run leaves no temp directory"; fi
$compose start postgres >/dev/null 2>&1
for _ in $(seq 1 60); do $compose exec -T postgres pg_isready -h 127.0.0.1 -U pgadmin -d mhai >/dev/null 2>&1 && break; sleep 1; done

# --- 6. offsite copy (S3-compatible; a throwaway MinIO) -----------------------
docker network create "$net" >/dev/null
mpw=m$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')
docker run -d --name "$project-minio" --network "$net" --network-alias minio \
  -e MINIO_ROOT_USER=opstest -e MINIO_ROOT_PASSWORD="$mpw" --entrypoint minio \
  "$MINIO_IMAGE" server /tmp/data --address :9000 >/dev/null
mc() { docker run --rm --network "$net" -e MC_HOST_t="http://opstest:$mpw@minio:9000" --entrypoint mc "$MC_IMAGE" "$@"; }
for _ in $(seq 1 60); do mc mb -p t/mhai-backups-test >/dev/null 2>&1 && break; sleep 1; done
cat > "$dir/etc/offsite.env" <<ENV
OFFSITE_ENDPOINT=http://minio:9000
OFFSITE_BUCKET=mhai-backups-test
OFFSITE_PREFIX=pilot/
OFFSITE_REGION=us-east-1
OFFSITE_ACCESS_KEY_ID=opstest
OFFSITE_SECRET_ACCESS_KEY=$mpw
OFFSITE_DOCKER_NETWORK=$net
ENV
chmod 600 "$dir/etc/offsite.env"
out=$(bash "$ops/backup.sh" 2>&1); rc=$?
if [ "$rc" = 0 ]; then ok "backup with offsite exits 0"; else fail "offsite backup exit $rc: $out"; fi
if printf '%s' "$out" | grep -q "$mpw"; then fail "offsite secret printed"; else ok "offsite secret not printed"; fi
check "status says offsite ok" grep -qx 'offsite=ok' "$dir/state/backup.status"
latest=$(backups | tail -n1)
listing=$(mc ls -r "t/mhai-backups-test/pilot/$latest/" 2>&1)
if printf '%s' "$listing" | grep -q 'mhai.dump.age' && printf '%s' "$listing" | grep -q 'SHA256SUMS'; then ok "offsite has the backup"; else fail "offsite listing: $listing"; fi
# A wrong secret fails loudly; the local backup is kept.
sed -i "s/^OFFSITE_SECRET_ACCESS_KEY=.*/OFFSITE_SECRET_ACCESS_KEY=wrong$$/" "$dir/etc/offsite.env"
before=$(backups | wc -l)
sleep 1
out=$(bash "$ops/backup.sh" 2>&1); rc=$?
after=$(backups | wc -l)
if [ "$rc" != 0 ]; then ok "offsite failure fails the run"; else fail "offsite failure: exit 0"; fi
check "status failed, offsite failed" sh -c "grep -qx 'status=failed' '$dir/state/backup.status' && grep -qx 'offsite=failed' '$dir/state/backup.status'"
if [ "$after" -gt "$before" ]; then ok "local backup kept when offsite fails"; else fail "local backup lost ($before -> $after)"; fi
rm -f "$dir/etc/offsite.env"
# A good run again, so the health check below sees a fresh ok backup.
bash "$ops/backup.sh" >/dev/null 2>&1 || fail "backup after offsite tests"

# --- 7. health check and the alert (fake webhook) --------------------------
cat > "$dir/hook.py" <<'PY'
import http.server, sys
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0))
        with open("/out/posts.log", "ab") as f:
            f.write(self.headers.get("Content-Type", "").encode() + b" " + self.rfile.read(n) + b"\n")
        self.send_response(200); self.end_headers()
    def log_message(self, *a): pass
http.server.HTTPServer(("0.0.0.0", 8080), H).serve_forever()
PY
mkdir -p "$dir/hook"; chmod 777 "$dir/hook"
docker run -d --name "$project-hook" -p 127.0.0.1::8080 -v "$dir/hook.py:/hook.py:ro" -v "$dir/hook:/out" \
  "$PY_IMAGE" python /hook.py >/dev/null
hport=$(docker port "$project-hook" 8080/tcp | head -n1 | sed 's/.*://')
for _ in $(seq 1 30); do curl -s -o /dev/null "http://127.0.0.1:$hport/" && break; sleep 1; done
posts() { if [ -f "$dir/hook/posts.log" ]; then wc -l < "$dir/hook/posts.log" | tr -d ' '; else echo 0; fi; }

# Alerting off (no webhook file): the check still fails, nothing is sent.
hc() { env OPS_SERVICES="${SERVICES:-postgres}" CORE_HEALTH_URL="${HEALTH_URL:-}" RESTORE_MAX_AGE_DAYS=8 \
  DISK_MIN_FREE_PCT="${DISK:-1}" bash "$ops/healthcheck.sh" 2>&1; }
out=$(SERVICES="postgres core" hc); rc=$?
if [ "$rc" = 1 ] && printf '%s' "$out" | grep -qi 'alerting is off'; then ok "missing container fails; alerting off without a webhook"; else fail "alerting off: exit $rc: $out"; fi
check "nothing posted while off" test "$(posts)" = 0

printf 'http://127.0.0.1:%s/hook-secret-path\n' "$hport" > "$dir/etc/alert-webhook-url"
chmod 600 "$dir/etc/alert-webhook-url"
out=$(SERVICES="postgres core" HEALTH_URL="http://127.0.0.1:1/api/v1/readyz" DISK=101 hc); rc=$?
if [ "$rc" = 1 ]; then ok "failed checks exit 1"; else fail "failed checks: exit $rc: $out"; fi
check "one alert sent" test "$(posts)" = 1
body=$(cat "$dir/hook/posts.log" 2>/dev/null)
expect_grep "alert is JSON with text" '^application/json \{"text":' "$body"
for want in 'core' 'readyz' 'disk' 'ops-test'; do
  expect_grep "alert names $want" "$want" "$body"
done
if printf '%s' "$out" | grep -q 'hook-secret-path'; then fail "webhook URL printed"; else ok "webhook URL not printed"; fi
# The same failures again: no repeat within ALERT_REPEAT_MINUTES.
SERVICES="postgres core" HEALTH_URL="http://127.0.0.1:1/api/v1/readyz" DISK=101 hc >/dev/null
check "same failures within the window: no second alert" test "$(posts)" = 1
# All pass (postgres up, fresh backup and restore test, disk ok): recovered.
out=$(hc); rc=$?
if [ "$rc" = 0 ]; then ok "all checks pass"; else fail "passing run exit $rc: $out"; fi
check "a recovery message is sent" test "$(posts)" = 2
expect_grep "recovery message text" 'recovered' "$(tail -n1 "$dir/hook/posts.log")"
hc >/dev/null
check "still passing: nothing more sent" test "$(posts)" = 2
# A stale backup alerts.
sed -i "s/^finished_at=.*/finished_at=$(( $(date +%s) - 3 * 86400 ))/" "$dir/state/backup.status"
out=$(hc); rc=$?
if [ "$rc" = 1 ] && tail -n1 "$dir/hook/posts.log" | grep -q 'backup'; then ok "stale backup alerts"; else fail "stale backup: exit $rc: $out"; fi
# A missing restore test alerts.
bash "$ops/backup.sh" >/dev/null 2>&1
rm -f "$dir/state/restore.status"
out=$(hc); rc=$?
if [ "$rc" = 1 ] && tail -n1 "$dir/hook/posts.log" | grep -q 'restore'; then ok "missing restore test alerts"; else fail "restore missing: exit $rc: $out"; fi

echo
if [ "$failures" = 0 ]; then echo "all ops tests passed"; else echo "$failures ops test(s) failed"; exit 1; fi

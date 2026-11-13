# shellcheck shell=bash
# Shared by backup.sh, restore-test.sh and healthcheck.sh (deploy/ops/).
# Sourced, never run. No secret is ever echoed or put on a command line.

OPS_CONFIG=${OPS_CONFIG:-/etc/mhai-ops/ops.env}

ops_log() { printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" >&2; }

# ops_read_env FILE: export each KEY=VALUE line of FILE whose key is upper
# case. The file is parsed, never sourced: a value is taken as it is
# (no quotes, no expansion), so a secret with $ or a quote in it is safe.
ops_read_env() {
  local k v
  [ -f "$1" ] || return 0
  while IFS= read -r line || [ -n "$line" ]; do
    case "$line" in ''|'#'*) continue ;; esac
    k=${line%%=*}; v=${line#*=}
    printf '%s' "$k" | grep -Eq '^[A-Z][A-Z0-9_]*$' || continue
    # A value already in the environment wins (tests, one-off runs).
    [ -n "${!k+x}" ] && continue
    export "$k=$v"
  done < "$1"
}

# ops_load_config: the settings, from OPS_CONFIG, then the defaults.
ops_load_config() {
  ops_read_env "$OPS_CONFIG"
  : "${MHAI_BACKEND_DIR:=/srv/mhai-backend}"
  : "${BACKUP_DIR:=/var/backups/mhai}"
  : "${STATE_DIR:=/var/lib/mhai-ops}"
  : "${BACKUP_RECIPIENTS_FILE:=/etc/mhai-ops/backup-recipients.txt}"
  : "${OFFSITE_ENV_FILE:=/etc/mhai-ops/offsite.env}"
  : "${ALERT_WEBHOOK_FILE:=/etc/mhai-ops/alert-webhook-url}"
  : "${KEEP_DAILY:=7}"
  : "${KEEP_WEEKLY:=4}"
  : "${CORE_IMAGE:=ghcr.io/measured-health/mhai-multiagent-core:latest}"
  : "${RESTORE_POSTGRES_IMAGE:=postgres:16.15-alpine}"
  : "${RCLONE_IMAGE:=rclone/rclone:1.68.2}"
  : "${BACKUP_MAX_AGE_HOURS:=26}"
  : "${RESTORE_MAX_AGE_DAYS:=8}"
  : "${DISK_MIN_FREE_PCT:=15}"
  : "${ALERT_REPEAT_MINUTES:=60}"
  : "${HOST_LABEL:=$(hostname)}"
  # CORE_HEALTH_URL: empty turns the check off; unset is the default.
  CORE_HEALTH_URL=${CORE_HEALTH_URL-http://127.0.0.1:8092/api/v1/readyz}
  export MHAI_BACKEND_DIR BACKUP_DIR STATE_DIR CORE_HEALTH_URL
}

# ops_compose ARGS: docker compose for the mhai-backend project only.
ops_compose() {
  (cd "$MHAI_BACKEND_DIR" && docker compose "$@")
}

# ops_write_status FILE KEY=VALUE...: replace FILE atomically.
ops_write_status() {
  local f=$1; shift
  mkdir -p "$(dirname "$f")"
  printf '%s\n' "$@" > "$f.new"
  chmod 600 "$f.new"
  mv -f "$f.new" "$f"
}

# ops_status_get FILE KEY: a value from a status file, or empty.
ops_status_get() { sed -n "s/^$2=//p" "$1" 2>/dev/null | tail -n1; }

# ops_is_backup_name NAME: a finished backup's directory name.
ops_is_backup_name() { printf '%s' "$1" | grep -Eq '^[0-9]{8}T[0-9]{6}Z$'; }

# ops_list_backups DIR: finished backups, oldest first.
ops_list_backups() {
  local p n
  for p in "$1"/*; do
    n=${p##*/}
    if ops_is_backup_name "$n" && [ -d "$p" ]; then printf '%s\n' "$n"; fi
  done | LC_ALL=C sort
}

# ops_rotate DIR DAILY WEEKLY: keep the newest DAILY backups, and the newest
# backup of each of the WEEKLY most recent ISO weeks; delete the other
# backups. Anything that isn't a backup's name is left alone.
ops_rotate() {
  local dir=$1 daily=$2 weekly=$3 all keep n week weeks='' count=0
  all=$(ops_list_backups "$dir")
  [ -n "$all" ] || return 0
  keep=$(printf '%s\n' "$all" | tail -n "$daily")
  for n in $(printf '%s\n' "$all" | LC_ALL=C sort -r); do
    [ "$count" -lt "$weekly" ] || break
    week=$(date -u -d "${n:0:4}-${n:4:2}-${n:6:2}" +%G-W%V)
    case " $weeks " in *" $week "*) continue ;; esac
    weeks="$weeks $week"; count=$((count + 1))
    keep=$(printf '%s\n%s\n' "$keep" "$n")
  done
  for n in $all; do
    if ! printf '%s\n' "$keep" | grep -qx "$n"; then
      rm -rf -- "${dir:?}/$n"
      ops_log "rotated out $n"
    fi
  done
}

# The query that counts every table's rows, in one snapshot; the backup
# records it and the restore test compares. Superuser: row-level security
# doesn't hide rows from it.
# shellcheck disable=SC2034  # used by backup.sh and restore-test.sh
OPS_COUNTS_SQL="SELECT n.nspname || '.' || c.relname || ' ' ||
  (xpath('/row/c/text()', query_to_xml(format('SELECT count(*) AS c FROM %I.%I', n.nspname, c.relname), false, true, '')))[1]::text
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r', 'p') AND n.nspname NOT IN ('pg_catalog', 'information_schema') AND n.nspname NOT LIKE 'pg_toast%'
ORDER BY 1;"

# shellcheck disable=SC2034
OPS_GOOSE_SQL="SELECT coalesce(max(version_id), 0) FROM goose_db_version WHERE is_applied;"

# ops_json_string TEXT: TEXT as a JSON string (quotes, backslashes and
# control characters escaped).
ops_json_string() {
  local s=$1
  s=${s//\\/\\\\}; s=${s//\"/\\\"}
  s=${s//$'\n'/\\n}; s=${s//$'\r'/\\r}; s=${s//$'\t'/\\t}
  printf '"%s"' "$s"
}

# ops_send_alert TEXT: POST {"text": TEXT} to the URL in ALERT_WEBHOOK_FILE
# (Slack, Mattermost, an email relay, or Telegram's sendMessage with
# chat_id in the URL). Returns 3 when alerting is off (no file), 1 when
# the post failed. The URL goes to curl through its config on stdin, never
# as an argument, and is never printed.
ops_send_alert() {
  local url
  if [ ! -s "$ALERT_WEBHOOK_FILE" ]; then
    ops_log "alerting is off (no $ALERT_WEBHOOK_FILE)"
    return 3
  fi
  url=$(head -n1 "$ALERT_WEBHOOK_FILE")
  if ! printf '%s' "$url" | grep -Eq '^https?://[^"\\[:space:]]+$'; then
    ops_log "the webhook URL in $ALERT_WEBHOOK_FILE is not an http(s) URL; alert not sent"
    return 1
  fi
  if printf 'url = "%s"\n' "$url" | curl -fsS --max-time 20 --retry 2 -K - \
      -H 'Content-Type: application/json' \
      --data-binary "{\"text\":$(ops_json_string "$1")}" -o /dev/null 2>/dev/null; then
    ops_log "alert sent"
    return 0
  fi
  ops_log "the alert could not be sent (webhook unreachable or refused)"
  return 1
}

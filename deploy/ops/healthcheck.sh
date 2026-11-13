#!/usr/bin/env bash
# The health check (deploy/README.md, "Operations"). Run as root every five
# minutes by mhai-healthcheck.timer (install-ops.sh), or by hand:
#
#   sudo /usr/local/lib/mhai-ops/healthcheck.sh
#
# Checks:
#   - containers: each service in OPS_SERVICES (default: postgres, nats,
#     core, and pipeline and egress-proxy when their profiles are on) is
#     running, not restarting, and not unhealthy;
#   - core: CORE_HEALTH_URL (GET /api/v1/readyz on loopback) answers 200;
#   - disk: at least DISK_MIN_FREE_PCT free where the backups, the state and
#     Docker's data are;
#   - the last backup finished ok less than BACKUP_MAX_AGE_HOURS ago;
#   - the last restore test passed less than RESTORE_MAX_AGE_DAYS ago (0 off).
# On failure it posts one alert (ops_send_alert: the URL in
# ALERT_WEBHOOK_FILE, off when there is none) naming the failed checks,
# again only when the set of failures changes or ALERT_REPEAT_MINUTES have
# passed, and one "recovered" message when everything passes again.
# Exits 0 when everything passes, 1 otherwise. It reads no .env and sends
# no patient data: only check names, service names and numbers.
set -uo pipefail
umask 077
# shellcheck source-path=SCRIPTDIR source=lib.sh
. "$(dirname "$(readlink -f "$0")")/lib.sh"
ops_load_config

mkdir -p "$STATE_DIR" && chmod 700 "$STATE_DIR"
exec 9>"$STATE_DIR/healthcheck.lock"
flock -n 9 || { ops_log "another health check is running"; exit 0; }

FAILS=()
bad() { FAILS+=("$*"); ops_log "FAIL $*"; }
now=$(date +%s)

# --- containers ---------------------------------------------------------------
services=${OPS_SERVICES:-}
if [ -z "$services" ]; then
  services="postgres nats core"
  # Compose reads COMPOSE_PROFILES from the project's .env: the pilot's
  # services are listed only when enable-pilot.sh turned their profiles on.
  active=$(ops_compose config --services 2>/dev/null || true)
  for s in pipeline egress-proxy; do
    if printf '%s\n' "$active" | grep -qx "$s"; then services="$services $s"; fi
  done
fi
for s in $services; do
  id=$(ops_compose ps -q "$s" 2>/dev/null | head -n1)
  if [ -z "$id" ]; then bad "container $s: not running (no container)"; continue; fi
  read -r state health restarting <<<"$(docker inspect -f '{{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}} {{.State.Restarting}}' "$id" 2>/dev/null || echo 'missing none false')"
  if [ "$state" != running ] || [ "$restarting" = true ]; then
    bad "container $s: $state"
  elif [ "$health" = unhealthy ]; then
    bad "container $s: unhealthy"
  fi
done

# --- core ---------------------------------------------------------------------
if [ -n "$CORE_HEALTH_URL" ]; then
  code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 "$CORE_HEALTH_URL" 2>/dev/null)
  [ "$code" = 200 ] || bad "core readyz: HTTP ${code:-000} (${CORE_HEALTH_URL##*/})"
fi

# --- disk ---------------------------------------------------------------------
paths="$BACKUP_DIR $STATE_DIR"
droot=$(docker info -f '{{.DockerRootDir}}' 2>/dev/null || true)
[ -z "$droot" ] || paths="$paths $droot"
seen=''
for p in $paths; do
  [ -e "$p" ] || continue
  read -r fs pct <<<"$(df -P "$p" 2>/dev/null | awk 'NR == 2 {print $1, $5}')"
  case " $seen " in *" $fs "*) continue ;; esac
  seen="$seen $fs"
  used=${pct%\%}
  case "$used" in ''|*[!0-9]*) bad "disk $p: df failed"; continue ;; esac
  free=$((100 - used))
  [ "$free" -ge "$DISK_MIN_FREE_PCT" ] || bad "disk $p: ${free}% free (minimum ${DISK_MIN_FREE_PCT}%)"
done

# --- the last backup ----------------------------------------------------------
bs="$STATE_DIR/backup.status"
if [ ! -f "$bs" ]; then
  bad "backup: none has run yet ($bs missing)"
else
  st=$(ops_status_get "$bs" status); at=$(ops_status_get "$bs" finished_at)
  case "$at" in ''|*[!0-9]*) at=0 ;; esac
  age_h=$(( (now - at) / 3600 ))
  if [ "$st" != ok ]; then
    bad "backup: last run failed ($(ops_status_get "$bs" message))"
  elif [ "$age_h" -ge "$BACKUP_MAX_AGE_HOURS" ]; then
    bad "backup: the last good backup is ${age_h}h old (maximum ${BACKUP_MAX_AGE_HOURS}h)"
  fi
fi

# --- the last restore test ----------------------------------------------------
if [ "$RESTORE_MAX_AGE_DAYS" -gt 0 ] 2>/dev/null; then
  rs="$STATE_DIR/restore.status"
  if [ ! -f "$rs" ]; then
    bad "restore test: never run on this host (run restore-test.sh weekly)"
  else
    st=$(ops_status_get "$rs" status); at=$(ops_status_get "$rs" finished_at)
    case "$at" in ''|*[!0-9]*) at=0 ;; esac
    age_d=$(( (now - at) / 86400 ))
    if [ "$st" != ok ]; then
      bad "restore test: the last one failed"
    elif [ "$age_d" -ge "$RESTORE_MAX_AGE_DAYS" ]; then
      bad "restore test: the last pass is ${age_d} days old (run restore-test.sh weekly)"
    fi
  fi
fi

# --- alert ----------------------------------------------------------------------
state="$STATE_DIR/health.state"
prev_fp=$(ops_status_get "$state" fingerprint)
prev_at=$(ops_status_get "$state" alerted_at)
case "$prev_at" in ''|*[!0-9]*) prev_at=0 ;; esac

if [ "${#FAILS[@]}" -eq 0 ]; then
  ops_log "all checks pass"
  if [ -n "$prev_fp" ]; then
    if ops_send_alert "[mhai-ops] $HOST_LABEL: recovered, all checks pass again"; then rm -f "$state"
    elif [ $? = 3 ]; then rm -f "$state"; fi
  fi
  exit 0
fi

fp=$(printf '%s\n' "${FAILS[@]}" | sed 's/[0-9]\+\(h\|%\| days\)/N\1/g' | sha256sum | cut -c1-16)
if [ "$fp" != "$prev_fp" ] || [ $((now - prev_at)) -ge $((ALERT_REPEAT_MINUTES * 60)) ]; then
  msg="[mhai-ops] $HOST_LABEL: ${#FAILS[@]} check(s) failed"
  for f in "${FAILS[@]}"; do msg="$msg"$'\n'"- $f"; done
  ops_send_alert "$msg"
  case $? in
    0) ops_write_status "$state" "fingerprint=$fp" "alerted_at=$now" ;;
    *) : ;;  # off or not sent: nothing recorded, so the next run tries again
  esac
else
  ops_log "same failures as the last alert; not sent again yet"
fi
exit 1

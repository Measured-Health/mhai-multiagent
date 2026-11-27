# shellcheck shell=bash
# Shared by deploy/production/enable-production.sh, deploy/emr/enable-emr.sh
# and deploy/emr/emr.sh (deploy/README.md, "Production mode" and "EMR
# integration"). Sourced, never run. Nothing here reads or prints a secret:
# .env values that are secrets are only ever tested for being set.
#
# Needs DEST (the backend folder, /srv/mhai-backend) set before sourcing, and
# deploy/tls/split-lib.sh (say, info, warn, die, place_files, report_expiry).

ENV_FILE="$DEST/.env"
# The uids the files are made for: the images' users.
POSTGRES_UID=70 NATS_UID=1000 CORE_UID=65532 NOTIFY_UID=65532 EMR_UID=65532 WEB_UID=1001
HELPER_IMAGE=${NATS_BOX_IMAGE:-natsio/nats-box:0.19.0}
PROD_OVERLAY=compose.production.yml
EMR_OVERLAY=compose.emr.yml

# env_get NAME: the value of NAME in .env (the last one), or empty.
env_get() { sed -n "s/^$1=//p" "$ENV_FILE" | tail -n1; }
# env_has NAME: .env has a line for NAME.
env_has() { grep -q "^$1=" "$ENV_FILE"; }
# env_set NAME VALUE: writes NAME=VALUE (never a secret: it is printed).
env_set() {
  if [ "$(env_get "$1")" = "$2" ] && env_has "$1"; then
    info "$1 unchanged"
  else
    sed -i "/^$1=/d" "$ENV_FILE"
    printf '%s=%s\n' "$1" "$2" >> "$ENV_FILE"
    info "$1=$2 written"
  fi
}
# env_unset NAME: removes NAME's lines.
env_unset() { if env_has "$1"; then sed -i "/^$1=/d" "$ENV_FILE"; info "$1 removed"; fi; }
# env_secret NAME: generates NAME (48 hex characters) unless it is set; never printed.
env_secret() {
  if [ -z "$(env_get "$1")" ]; then
    sed -i "/^$1=/d" "$ENV_FILE"
    printf '%s=%s\n' "$1" "$(openssl rand -hex 24)" >> "$ENV_FILE"
    info "$1 generated (not shown)"
  else
    info "$1 already set, kept"
  fi
}

# The compose file list (COMPOSE_FILE in .env, colon-separated).
cf_get() { local v; v=$(env_get COMPOSE_FILE); printf '%s' "${v:-compose.yml}"; }
cf_has() { case ":$(cf_get):" in *":$1:"*) return 0 ;; esac; return 1; }
# cf_with LIST FILE: LIST with FILE appended (once).
cf_with() { case ":$1:" in *":$2:"*) printf '%s' "$1" ;; *) printf '%s:%s' "$1" "$2" ;; esac; }
# cf_without LIST FILE: LIST without FILE.
cf_without() {
  local out='' f
  IFS=: read -r -a parts <<<"$1"
  for f in "${parts[@]}"; do [ "$f" = "$2" ] || out=${out:+$out:}$f; done
  printf '%s' "${out:-compose.yml}"
}
production_on() { cf_has "$PROD_OVERLAY"; }

# project: the compose project's name (COMPOSE_PROJECT_NAME or compose.yml's
# name), so a rehearsal under another name never touches mhai-backend's.
project() { (cd "$DEST" && docker compose config --format json 2>/dev/null | jq -r '.name'); }

# core_ready [TRIES]: core answers readyz 200 on its loopback port.
core_ready() {
  local port tries=${1:-36} code=000
  port=$(env_get CORE_HOST_PORT); port=${port:-8092}
  for _ in $(seq 1 "$tries"); do
    code=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$port/api/v1/readyz" || echo 000)
    [ "$code" != 200 ] || return 0
    sleep "${PROD_POLL:-5}"
  done
  return 1
}

# log_has SERVICE PATTERN [TRIES] [PROFILE...]: the service's log since its
# start has PATTERN. grep without -q: a -q exit would close the pipe on
# docker compose logs (PR #289).
log_has() {
  local svc=$1 pat=$2 tries=${3:-36}; shift 3 || shift $#
  local prof=()
  for p in "$@"; do prof+=(--profile "$p"); done
  for _ in $(seq 1 "$tries"); do
    if (cd "$DEST" && docker compose "${prof[@]}" logs --no-log-prefix "$svc" 2>/dev/null) | grep -- "$pat" >/dev/null; then
      return 0
    fi
    sleep "${PROD_POLL:-5}"
  done
  return 1
}

# running SERVICE [PROFILE...]: the service has a running container.
running() {
  local svc=$1; shift
  local prof=()
  for p in "$@"; do prof+=(--profile "$p"); done
  [ -n "$(cd "$DEST" && docker compose "${prof[@]}" ps -q --status running "$svc" 2>/dev/null)" ]
}

# helper ARGS...: a throwaway root container with no network (the files it
# writes get their owner and mode there; nothing reaches an argument or the
# output).
helper() { docker run --rm --network none --user 0 --entrypoint sh "$@"; }

# nats_connz: the NATS server's view of its clients, for the checks: name,
# TLS version and the NKey user's first characters only.
nats_connz() {
  (cd "$DEST" && docker compose exec -T nats wget -q -O - 'http://127.0.0.1:8222/connz?auth=1&limit=200' 2>/dev/null) \
    | jq -r '.connections[]? | "\(.name // "-")\t\(.tls_version // "none")\t\((.authorized_user // "-")[0:8])"'
}

# pg_tls_report: each client connection to Postgres over the network, and
# whether it is TLS (local socket sessions, the checks themselves, are left out).
pg_tls_report() {
  (cd "$DEST" && docker compose exec -T postgres psql -U pgadmin -d mhai -tA -F ' ' -c \
    "SELECT a.usename, s.ssl, count(*) FROM pg_stat_activity a JOIN pg_stat_ssl s USING (pid)
     WHERE a.client_addr IS NOT NULL GROUP BY 1, 2 ORDER BY 1" 2>/dev/null)
}

# now_s: seconds since the epoch, with tenths.
now_s() { date +%s.%1N; }
elapsed() { awk -v a="$1" -v b="$2" 'BEGIN { printf "%.1f", b - a }'; }

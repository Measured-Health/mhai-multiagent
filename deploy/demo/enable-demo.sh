#!/usr/bin/env bash
# The public demo's backend half (deploy/README.md, "Public demo: sample
# answers"): the seeded synthetic demo hospital and accounts, and the sample
# answer worker. Run by the "Deploy backend demo" workflow on every deploy,
# after core is healthy, from /srv/mhai-backend (the workflow ships this
# folder there). Idempotent: every step checks first and keeps what is there.
#
#   bash demo/enable-demo.sh          seed, then start or update sample-ai
#   DEMO_SAMPLE_AI=off bash demo/enable-demo.sh
#                                     stop sample-ai (the seed still runs)
#
# Fail closed:
#   - never beside the pilot: a pipeline container, or the pilot profile in
#     .env, stops here and sample-ai stays off (both would take the same jobs);
#   - core demo seed refuses a database with a real cloud agreement, and
#     MHAI_DEMO_SEED=on is set only on its one-shot (compose.yml);
#   - sample-ai answers only the hospitals the database marks is_demo
#     (DEMO_SAMPLE_HOSPITALS), and the worker refuses the sample client
#     outside APP_ENV=demo/development;
#   - too little free memory: sample-ai is not started (a warning, the deploy
#     still passes; the demo then shows questions that wait and fail).
#
# Secrets are made on the host only, inside a container with no network, as
# deploy/pilot/enable-pilot.sh does (the same files, so the pilot can later
# take them over): the pipeline's NATS NKey seed and the intelligence_app
# password. Nothing secret is printed or passed as an argument. It never
# touches the EMR containers, the web demo or nginx.

set -euo pipefail
umask 077

DEST=${MHAI_BACKEND_DIR:-/srv/mhai-backend}
NATS_BOX_IMAGE=${NATS_BOX_IMAGE:-natsio/nats-box:0.19.0}
# sample-ai's cap (compose.yml) plus room for the host.
MIN_AVAILABLE_MIB=${DEMO_MIN_AVAILABLE_MIB:-320}
APP_UID=1000   # the intelligence image's appuser (its Dockerfile checks it)

say()  { printf '\n==> %s\n' "$*"; }
info() { printf '    %s\n' "$*"; }
warn() { printf '::warning::%s\n' "$*"; }
die()  { printf '::error::%s\n' "$*" >&2; exit 1; }

cd "$DEST"
ENV_FILE="$DEST/.env"
if [ ! -f "$ENV_FILE" ] || [ ! -f secrets/nats-gateway.nk ]; then die "$DEST is not set up (deploy/setup-host.sh)"; fi
compose() { docker compose --profile demo "$@"; }
env_get() { sed -n "s/^$1=//p" "$ENV_FILE" | tail -n1; }
env_set() {
  if [ "$(env_get "$1")" = "$2" ] && grep -q "^$1=" "$ENV_FILE"; then
    info "$1 unchanged"
  else
    sed -i "/^$1=/d" "$ENV_FILE"
    printf '%s=%s\n' "$1" "$2" >> "$ENV_FILE"
    info "$1 written"
  fi
}
pg() { docker compose exec -T postgres psql -U pgadmin -d mhai -v ON_ERROR_STOP=1 -tA; }
stop_sample() {
  if [ -n "$(compose ps -q sample-ai 2>/dev/null)" ]; then
    compose stop sample-ai >/dev/null && compose rm -f sample-ai >/dev/null
    info "sample-ai stopped"
  fi
}

say "Memory and disk (for the record)"
free -m | sed 's/^/    /'
df -h / | sed 's/^/    /'

say "Not beside the pilot"
PILOT=0
case ",$(env_get COMPOSE_PROFILES)," in *,pilot,*) PILOT=1 ;; esac
[ -z "$(docker compose --profile pilot ps -q pipeline 2>/dev/null)" ] || PILOT=1
if [ "$PILOT" = 1 ]; then
  stop_sample
  # The web deploy then keeps the public demo on sample data (mock mode).
  rm -f "$DEST/demo/seeded"
  warn "the pilot's pipeline is on this host: no demo seed and no sample answers (deploy/README.md)"
  exit 0
fi
info "no pilot pipeline"

say "The demo seed (core demo seed)"
if ! compose run --rm -T core-demo-seed </dev/null; then
  rm -f "$DEST/demo/seeded"
  stop_sample
  die "core demo seed refused or failed (its message above names the reason; nothing secret)"
fi
mkdir -p "$DEST/demo"
chmod 755 "$DEST/demo"
# A marker the web deploy reads before it switches the public demo to live
# mode (mhai-web deploy/README.md). Not a secret.
date -u +%Y-%m-%dT%H:%M:%SZ > "$DEST/demo/seeded"
chmod 644 "$DEST/demo/seeded"
info "seeded (marker $DEST/demo/seeded)"

if [ "${DEMO_SAMPLE_AI:-on}" = off ]; then
  say "Sample answers"
  stop_sample
  info "off (repository variable DEMO_SAMPLE_AI=off)"
  exit 0
fi

say "The demo hospitals sample-ai may answer"
ids=$(printf 'SELECT string_agg(id::text, %s ORDER BY id) FROM hospitals WHERE is_demo;\n' "','" | pg)
printf '%s' "$ids" | grep -Eq '^[0-9a-f-]{36}(,[0-9a-f-]{36}){0,15}$' || die "no demo hospital found after the seed"
env_set DEMO_SAMPLE_HOSPITALS "$ids"

say "Memory for sample-ai"
if [ -n "$(compose ps -q sample-ai 2>/dev/null)" ]; then
  info "already running; not checked again"
else
  avail=$(awk '/^MemAvailable:/ {print int($2/1024)}' /proc/meminfo)
  info "available: ${avail} MiB; sample-ai needs ${MIN_AVAILABLE_MIB} MiB"
  if [ "$avail" -lt "$MIN_AVAILABLE_MIB" ]; then
    warn "not enough memory for sample-ai (${avail} < ${MIN_AVAILABLE_MIB} MiB): not started; questions wait and fail at their deadline"
    exit 0
  fi
fi

say "Its secrets in $DEST/secrets (each mode 400, uid $APP_UID)"
mkdir -p secrets
chmod 700 secrets
put_secret() {
  docker run --rm -i --network none --user 0 --entrypoint sh -e F="$1" -e U="$APP_UID" \
    -v "$DEST/secrets:/s" "$NATS_BOX_IMAGE" -c '
      set -eu; umask 077
      t="/s/.$F.new"; trap "rm -f $t" EXIT
      cat > "$t"; [ -s "$t" ] || { echo "empty secret" >&2; exit 1; }
      chown "$U:$U" "$t"; chmod 400 "$t"; mv -f "$t" "/s/$F"'
}
pub=$(docker run --rm --network none --user 0 --entrypoint sh -e U="$APP_UID" \
  -v "$DEST/secrets:/s" "$NATS_BOX_IMAGE" -c '
    set -eu; umask 077
    if [ -s /s/nats-pipeline.nk ]; then
      echo "pipeline NKey exists, kept" >&2
    else
      out=$(nsc generate nkey --user 2>/dev/null)
      printf "%s\n" "$out" | sed -n 1p > /s/nats-pipeline.nk
      unset out
      echo "pipeline NKey generated" >&2
    fi
    case "$(head -c 2 /s/nats-pipeline.nk)" in SU) ;; *) echo "not a user seed" >&2; exit 1;; esac
    chown "$U:$U" /s/nats-pipeline.nk; chmod 400 /s/nats-pipeline.nk
    nats auth nkey show /s/nats-pipeline.nk' 2> >(sed 's/^/    /' >&2))
case "$pub" in U?*) ;; *) die "could not read the pipeline's public key" ;; esac
NATS_KEY_CHANGED=0
[ "$(env_get NATS_PIPELINE_NKEY)" = "$pub" ] || NATS_KEY_CHANGED=1
env_set NATS_PIPELINE_NKEY "$pub"
DB_NEW=0
if [ -z "$(env_get INTELLIGENCE_DB_PASSWORD)" ]; then
  sed -i '/^INTELLIGENCE_DB_PASSWORD=/d' "$ENV_FILE"
  printf 'INTELLIGENCE_DB_PASSWORD=%s\n' "$(openssl rand -hex 24)" >> "$ENV_FILE"
  DB_NEW=1
  info "INTELLIGENCE_DB_PASSWORD generated"
fi
env_get INTELLIGENCE_DB_PASSWORD | tr -d '\n' | put_secret intelligence-db-password
[ "$(stat -c '%u %a' secrets/intelligence-db-password)" = "$APP_UID 400" ] || die "secrets/intelligence-db-password has the wrong owner or mode"
info "intelligence-db-password written from .env"
docker compose config --quiet || die "compose.yml and .env don't validate"

if [ "$NATS_KEY_CHANGED" = 1 ] || [ "$DB_NEW" = 1 ]; then
  say "NATS and core pick up the pipeline user"
  [ "$NATS_KEY_CHANGED" = 0 ] || { docker compose up -d --force-recreate --no-deps nats; info "nats recreated"; }
  # core-migrate makes the intelligence_app login from INTELLIGENCE_DB_PASSWORD.
  docker compose up -d core || { docker compose logs --tail=60 core-migrate core; die "core did not start"; }
  port=$(env_get CORE_HOST_PORT); port=${port:-8092}
  ok=0
  for _ in $(seq 1 24); do
    if [ "$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$port/api/v1/readyz" || echo 000)" = 200 ]; then ok=1; break; fi
    sleep 5
  done
  [ "$ok" = 1 ] || { docker compose logs --tail=80 core; die "core is not ready after the NATS change"; }
  info "core ready"
fi

say "Starting sample-ai"
# The image carries the intelligence package's libraries (a few GB, though
# the sample path loads none of the model ones): never fill the disk the EMR
# shares.
img=$(compose config --images sample-ai 2>/dev/null | head -n1)
if [ -n "$img" ] && ! docker image inspect "$img" >/dev/null 2>&1; then
  free_gib=$(df -BG --output=avail "$(docker info -f '{{.DockerRootDir}}' 2>/dev/null || echo /)" | tail -n1 | tr -dc 0-9)
  if [ "${free_gib:-0}" -lt "${DEMO_MIN_DISK_GIB:-8}" ]; then
    warn "only ${free_gib:-0} GiB free disk for the sample-ai image (needs ${DEMO_MIN_DISK_GIB:-8}): not started"
    exit 0
  fi
fi
compose pull -q sample-ai
compose up -d --no-deps --force-recreate sample-ai
started=0
for _ in $(seq 1 36); do
  if compose logs --no-log-prefix sample-ai 2>/dev/null | grep -q 'worker started service=pipeline'; then started=1; break; fi
  state=$(docker inspect -f '{{.State.Status}} {{.RestartCount}}' "$(compose ps -q sample-ai)" 2>/dev/null || echo "missing 0")
  case "$state" in running\ 0|created\ 0) ;; *) break ;; esac
  sleep 5
done
if [ "$started" != 1 ]; then
  compose logs --tail=60 sample-ai
  die "sample-ai did not start (its settings errors name variables only)"
fi
info "sample-ai started (labelled sample answers for $(printf '%s' "$ids" | tr ',' '\n' | wc -l) demo hospital(s))"

say "Memory in use (MiB)"
sleep 5
mapfile -t running < <(docker compose --profile demo ps -q)
docker stats --no-stream --format '{{.Name}} {{.MemUsage}}' "${running[@]}" | sed 's/^/    /'
free -m | sed 's/^/    /'

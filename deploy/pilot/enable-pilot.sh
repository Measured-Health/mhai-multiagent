#!/usr/bin/env bash
# Switch on cloud answers for one pilot hospital on the demo host
# (deploy/pilot/README.md). Run by the product owner, as the deploy user, on
# the instance, from a copy of this repo's deploy/ folder at the release
# the instance runs:
#
#   bash deploy/pilot/enable-pilot.sh --provider anthropic \
#     --hospital-name "Pilot Hospital" --short-code PILOT \
#     --agreement-ref AGR-2026-01 --zdr-ref <provider ZDR reference>
#
# The provider's API key is read from standard input (a hidden prompt on a
# terminal, or one line piped from a password manager) into a 0400 file. It
# is never an argument, never in .env, never printed. Run it again at any
# time: every step checks what is there first and keeps it. A key file
# already in place is kept unless --replace-key is given.
#
# It touches only the mhai-backend compose project in /srv/mhai-backend:
# never the EMR containers, the web demo or nginx. It recreates nats (a
# few seconds) when the pipeline's NATS key is new, and core when the
# enabled providers change, as a deploy does.

set -euo pipefail
umask 077

DEST=${MHAI_BACKEND_DIR:-/srv/mhai-backend}
NATS_BOX_IMAGE=${NATS_BOX_IMAGE:-natsio/nats-box:0.19.0}
DEFAULT_IMAGE=ghcr.io/measured-health/mhai-multiagent-pipeline-pilot:latest
# The pipeline's cap is 1600 MiB (compose.yml); the egress proxy's 128.
MIN_AVAILABLE_MIB=${PILOT_MIN_AVAILABLE_MIB:-1800}
SRC=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

say()  { printf '\n==> %s\n' "$*"; }
info() { printf '    %s\n' "$*"; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }

usage() {
  cat >&2 <<'EOF'
usage: bash enable-pilot.sh --provider anthropic|gemini
         --hospital-name NAME --short-code CODE [--time-zone TZ]
         --agreement-ref AGR-YYYY-NN|DPA-YYYY-NN --zdr-ref REF
         [--model MODEL] [--budget TOKENS] [--image IMAGE]
         [--replace-key] [--seed-reference-guidelines]
The API key is read from standard input.
EOF
  exit 2
}

PROVIDER='' NAME='' CODE='' TZONE=Africa/Lagos AGREEMENT='' ZDR='' MODEL='' BUDGET='' IMAGE=''
REPLACE_KEY=0 SEED_GUIDELINES=0
while [ $# -gt 0 ]; do
  case "$1" in
    --provider) [ $# -ge 2 ] || usage; PROVIDER=$2; shift 2 ;;
    --hospital-name) [ $# -ge 2 ] || usage; NAME=$2; shift 2 ;;
    --short-code) [ $# -ge 2 ] || usage; CODE=$2; shift 2 ;;
    --time-zone) [ $# -ge 2 ] || usage; TZONE=$2; shift 2 ;;
    --agreement-ref) [ $# -ge 2 ] || usage; AGREEMENT=$2; shift 2 ;;
    --zdr-ref) [ $# -ge 2 ] || usage; ZDR=$2; shift 2 ;;
    --model) [ $# -ge 2 ] || usage; MODEL=$2; shift 2 ;;
    --budget) [ $# -ge 2 ] || usage; BUDGET=$2; shift 2 ;;
    --image) [ $# -ge 2 ] || usage; IMAGE=$2; shift 2 ;;
    --replace-key) REPLACE_KEY=1; shift ;;
    --seed-reference-guidelines) SEED_GUIDELINES=1; shift ;;
    *) usage ;;
  esac
done

# --- the arguments, checked before anything is touched ----------------------
case "$PROVIDER" in
  anthropic) MODEL=${MODEL:-claude-opus-5-5} ;;
  gemini) [ -n "$MODEL" ] || die "--model is required for gemini (the exact model id the gemini adapter allows)" ;;
  *) die "--provider must be anthropic or gemini" ;;
esac
P_UPPER=$(printf '%s' "$PROVIDER" | tr '[:lower:]' '[:upper:]')
[ -n "$NAME" ] || die "--hospital-name is required"
case "$NAME" in *[\$\"\'\\\`]*|*$'\n'*) die "--hospital-name must not hold quotes, \$, backslashes or newlines" ;; esac
printf '%s' "$CODE" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9_-]{1,31}$' || die "--short-code must be 2 to 32 of A-Z a-z 0-9 _ - (starting with a letter or digit)"
CODE=$(printf '%s' "$CODE" | tr '[:lower:]' '[:upper:]')
printf '%s' "$TZONE" | grep -Eq '^[A-Za-z_]+(/[A-Za-z0-9_+-]+)*$' || die "--time-zone must be an IANA zone such as Africa/Lagos"
# Core's format (00018's CHECK): AGR-YYYY-NN.. or DPA-YYYY-NN.. A real
# hospital never takes DEMO-SYNTHETIC.
printf '%s' "$AGREEMENT" | grep -Eq '^(AGR|DPA)-[0-9]{4}-[0-9]{2,8}$' || die "--agreement-ref must be AGR-YYYY-NN or DPA-YYYY-NN (2 to 8 digits; see README: pilot placeholder)"
# The worker's format; production refuses DEMO-SYNTHETIC.
printf '%s' "$ZDR" | grep -Eq '^[A-Z0-9][A-Z0-9_.-]{2,63}$' || die "--zdr-ref must be 3 to 64 of A-Z 0-9 _ . - (see README)"
[ "$ZDR" != DEMO-SYNTHETIC ] || die "--zdr-ref DEMO-SYNTHETIC is for the synthetic demo only"
printf '%s' "$MODEL" | grep -Eq '^[A-Za-z0-9._:-]{1,64}$' || die "--model must be 1 to 64 of A-Z a-z 0-9 . _ : -"
if [ -n "$BUDGET" ]; then
  if ! printf '%s' "$BUDGET" | grep -Eq '^[1-9][0-9]{0,9}$' || [ "$BUDGET" -gt 1000000000 ]; then
    die "--budget must be 1 to 1000000000"
  fi
fi

say "Checking prerequisites"
for bin in docker openssl curl; do
  command -v "$bin" >/dev/null || die "$bin is not installed"
done
docker compose version >/dev/null 2>&1 || die "docker compose (v2) is not available"
[ "$(id -u)" -ne 0 ] || die "run as the deploy user (ubuntu), not root"
if [ ! -f "$DEST/.env" ] || [ ! -f "$DEST/secrets/nats-gateway.nk" ]; then
  die "$DEST is not set up (deploy/setup-host.sh)"
fi
[ -f "$SRC/egress-proxy/squid.conf" ] || die "$SRC/egress-proxy/squid.conf is missing: run this from a copy of the repo's deploy/ folder"
cd "$DEST"
ENV_FILE="$DEST/.env"
compose() { docker compose --profile cloud --profile pilot "$@"; }
compose config --services 2>/dev/null | grep -qx pipeline \
  || die "$DEST/compose.yml has no pipeline service: deploy the release with the pilot first (the deploy workflow copies compose.yml)"
grep -qF "nkey: \$NATS_PIPELINE_NKEY" "$DEST/nats/nats.conf" \
  || die "$DEST/nats/nats.conf has no pipeline user: deploy the release with the pilot first"
info "release with the pilot is deployed"

# env_get NAME: the value of NAME in .env (the last one), or empty.
env_get() { sed -n "s/^$1=//p" "$ENV_FILE" | tail -n1; }
# env_set NAME VALUE: write NAME=VALUE (not a secret: it is printed).
env_set() {
  if [ "$(env_get "$1")" = "$2" ] && grep -q "^$1=" "$ENV_FILE"; then
    info "$1 unchanged"
  else
    sed -i "/^$1=/d" "$ENV_FILE"
    printf '%s=%s\n' "$1" "$2" >> "$ENV_FILE"
    info "$1 written"
  fi
}
env_unset() {
  if grep -q "^$1=" "$ENV_FILE"; then sed -i "/^$1=/d" "$ENV_FILE"; info "$1 removed"; fi
}

say "The API key ($PROVIDER)"
KEY_FILE="$DEST/secrets/pilot-cloud-api-key"
KEY_PROVIDER_FILE="$DEST/secrets/pilot-cloud-api-key.provider"
KEY=
if [ -s "$KEY_FILE" ] && [ "$REPLACE_KEY" = 0 ]; then
  [ "$(cat "$KEY_PROVIDER_FILE" 2>/dev/null)" = "$PROVIDER" ] \
    || die "the key file in place is not a $PROVIDER key: run again with --replace-key and the $PROVIDER key on stdin"
  info "key file exists, kept (--replace-key to change it)"
else
  if [ -t 0 ]; then
    read -r -s -p "    Paste the $PROVIDER API key (hidden): " KEY || true
    printf '\n'
  else
    IFS= read -r KEY || true
  fi
  [ -n "$KEY" ] || die "no key on standard input"
  case "$KEY" in *[[:space:]]*) KEY=; die "the key has whitespace in it" ;; esac
  [ "${#KEY}" -ge 20 ] || { KEY=; die "the key is shorter than 20 characters"; }
  case "$(printf '%s' "$KEY" | tr '[:upper:]' '[:lower:]')" in
    *changeme*|*change-me*|*placeholder*|*example*) KEY=; die "the key is a placeholder" ;;
  esac
  info "key read (not shown)"
fi

say "Memory"
if [ -n "$(compose ps -q pipeline 2>/dev/null)" ]; then
  info "the pipeline is already running; not checked again"
else
  avail=$(awk '/^MemAvailable:/ {print int($2/1024)}' /proc/meminfo)
  info "available: ${avail} MiB; the pipeline and the egress proxy need ${MIN_AVAILABLE_MIB} MiB"
  [ "$avail" -ge "$MIN_AVAILABLE_MIB" ] || die "not enough memory for the pipeline (${avail} < ${MIN_AVAILABLE_MIB} MiB): resize the instance (README, 'Memory'), nothing was changed"
fi

say "The pipeline image"
IMAGE=${IMAGE:-$(env_get PIPELINE_IMAGE)}
IMAGE=${IMAGE:-$DEFAULT_IMAGE}
if ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
  docker pull -q "$IMAGE" >/dev/null 2>&1 \
    || die "could not pull $IMAGE: run the 'Pilot pipeline image' workflow, then 'docker login ghcr.io' with a read:packages token on stdin (README)"
fi
arch=$(docker image inspect -f '{{.Architecture}}' "$IMAGE")
case "$(uname -m)/$arch" in aarch64/arm64|x86_64/amd64) ;; *) die "$IMAGE is $arch, this host is $(uname -m)" ;; esac
APP_UID=$(docker run --rm --network none --entrypoint id "$IMAGE" -u)
case "$APP_UID" in ''|0|*[!0-9]*) die "the image's user is not a non-root uid" ;; esac
# The provider's API host, from the image's own vocabulary: an image without
# the provider fails here, before anything changes.
HOST=$(docker run --rm --network none --entrypoint python "$IMAGE" -c \
  'import sys; from intelligence.boundary.cloud import vocab; print(vocab.HOSTS[sys.argv[1]])' "$PROVIDER" 2>/dev/null) \
  || die "$IMAGE does not support the provider $PROVIDER"
printf '%s' "$HOST" | grep -Eq '^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$' || die "unexpected API host for $PROVIDER"
info "$IMAGE ($arch), user $APP_UID, API host $HOST"

say "Core knows the provider"
# Every check of the CLI runs before it connects: with the provider enabled
# and no agreement reference, a core that knows the provider answers "needs
# --agreement-ref" and writes nothing. A core that doesn't would refuse to
# start with it in CORE_CLOUD_PROVIDERS, so .env is not touched until here.
out=$(compose --profile tools run --rm --no-deps -T -e CORE_CLOUD_PROVIDERS="$PROVIDER" core-cli \
  hospitals model-policy set "$CODE" --provider "$PROVIDER" </dev/null 2>&1 || true)
case "$out" in
  *"needs --agreement-ref"*) info "core accepts $PROVIDER" ;;
  *) die "the core image does not accept the provider $PROVIDER (deploy a core with it first)" ;;
esac

say "Secrets in $DEST/secrets (each mode 400, owned by the image's user $APP_UID)"
mkdir -p "$DEST/secrets"
chmod 700 "$DEST/secrets"
# put_secret FILE: write stdin to secrets/FILE inside a throwaway container
# with no network (a temp file, then a rename), owned by the pipeline's
# user, mode 400. The value never reaches an argument or the output.
put_secret() {
  docker run --rm -i --network none --user 0 --entrypoint sh -e F="$1" -e U="$APP_UID" \
    -v "$DEST/secrets:/s" "$NATS_BOX_IMAGE" -c '
      set -eu; umask 077
      t="/s/.$F.new"; trap "rm -f $t" EXIT
      cat > "$t"; [ -s "$t" ] || { echo "empty secret" >&2; exit 1; }
      chown "$U:$U" "$t"; chmod 400 "$t"; mv -f "$t" "/s/$F"'
}
# owned_ok FILE: secrets/FILE is owned by the image's user, mode 400.
owned_ok() { [ "$(stat -c '%u %a' "$DEST/secrets/$1" 2>/dev/null)" = "$APP_UID 400" ]; }

# The NATS NKey: made inside nats-box with no network; only the public key
# comes back. An existing pair is kept.
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

# The database login's password: in .env for core-migrate (which makes the
# login), and the same value in a file for the pipeline.
if [ -z "$(env_get INTELLIGENCE_DB_PASSWORD)" ]; then
  sed -i '/^INTELLIGENCE_DB_PASSWORD=/d' "$ENV_FILE"
  printf 'INTELLIGENCE_DB_PASSWORD=%s\n' "$(openssl rand -hex 24)" >> "$ENV_FILE"
  info "INTELLIGENCE_DB_PASSWORD generated"
else
  info "INTELLIGENCE_DB_PASSWORD already set, kept"
fi
env_get INTELLIGENCE_DB_PASSWORD | tr -d '\n' | put_secret intelligence-db-password
owned_ok intelligence-db-password || die "secrets/intelligence-db-password has the wrong owner or mode"
info "intelligence-db-password written from .env"

# The API key.
if [ -n "$KEY" ]; then
  printf '%s' "$KEY" | put_secret pilot-cloud-api-key
  KEY=
  printf '%s\n' "$PROVIDER" > "$KEY_PROVIDER_FILE"
  chmod 600 "$KEY_PROVIDER_FILE"
  info "pilot-cloud-api-key written"
fi
unset KEY
owned_ok pilot-cloud-api-key || die "secrets/pilot-cloud-api-key has the wrong owner or mode (run again with --replace-key)"

say "Settings in $DEST/.env (none of these is a secret)"
for other in ANTHROPIC GEMINI; do
  [ "$other" = "$P_UPPER" ] && continue
  for v in MODEL KEY_FILE ZDR_REF; do env_unset "PILOT_${other}_$v"; done
done
env_set PILOT_CLOUD_PROVIDER "$PROVIDER"
env_set "PILOT_${P_UPPER}_MODEL" "$MODEL"
env_set "PILOT_${P_UPPER}_KEY_FILE" /run/secrets/cloud-api-key
env_set "PILOT_${P_UPPER}_ZDR_REF" "$ZDR"
env_set PIPELINE_IMAGE "$IMAGE"
CORE_CHANGED=0
[ "$(env_get CORE_CLOUD_PROVIDERS)" = "$PROVIDER" ] || CORE_CHANGED=1
env_set CORE_CLOUD_PROVIDERS "$PROVIDER"
# So every later `docker compose` here, the deploy workflow's included,
# keeps the proxy and the pipeline in the project.
env_set COMPOSE_PROFILES cloud,pilot
docker compose config --quiet || die "compose.yml and .env don't validate"

say "The egress proxy allows $HOST only"
mkdir -p "$DEST/egress-proxy"
chmod 755 "$DEST/egress-proxy"
install -m 644 "$SRC/egress-proxy/squid.conf" "$DEST/egress-proxy/squid.conf.new"
if cmp -s "$DEST/egress-proxy/squid.conf.new" "$DEST/egress-proxy/squid.conf"; then
  rm -f "$DEST/egress-proxy/squid.conf.new"; PROXY_CHANGED=0; info "squid.conf unchanged"
else
  mv -f "$DEST/egress-proxy/squid.conf.new" "$DEST/egress-proxy/squid.conf"; PROXY_CHANGED=1; info "squid.conf written"
fi
printf '# The pilot: %s only (enable-pilot.sh).\n%s\n' "$PROVIDER" "$HOST" > "$DEST/egress-proxy/allowed-hosts.new"
chmod 644 "$DEST/egress-proxy/allowed-hosts.new"
if cmp -s "$DEST/egress-proxy/allowed-hosts.new" "$DEST/egress-proxy/allowed-hosts"; then
  rm -f "$DEST/egress-proxy/allowed-hosts.new"; info "allowed-hosts unchanged"
else
  mv -f "$DEST/egress-proxy/allowed-hosts.new" "$DEST/egress-proxy/allowed-hosts"; PROXY_CHANGED=1; info "allowed-hosts written"
fi

say "NATS and core"
if [ "$NATS_KEY_CHANGED" = 1 ]; then
  compose up -d --force-recreate --no-deps nats
  info "nats recreated with the pipeline's public key"
fi
# Runs core-migrate (which makes intelligence_app) and recreates core if
# CORE_CLOUD_PROVIDERS changed, as a deploy does.
if ! compose up -d core; then
  compose logs --tail=60 core-migrate core
  die "core did not start"
fi
info "core-migrate ran: it ensures the intelligence_app login from INTELLIGENCE_DB_PASSWORD"
port=$(env_get CORE_HOST_PORT); port=${port:-8092}
ok=0
for _ in $(seq 1 24); do
  ready=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$port/api/v1/readyz" || echo 000)
  if [ "$ready" = 200 ]; then ok=1; break; fi
  sleep 5
done
[ "$ok" = 1 ] || { compose logs --tail=80 core; die "core is not ready (readyz=$ready)"; }
info "core ready (CORE_CLOUD_PROVIDERS=$PROVIDER; recreated: $CORE_CHANGED)"

say "Starting the egress proxy and the pipeline"
if [ "$PROXY_CHANGED" = 1 ] && [ -n "$(compose ps -q egress-proxy)" ]; then
  compose up -d --force-recreate --no-deps egress-proxy
else
  compose up -d --no-deps egress-proxy
fi
compose up -d pipeline
started=0
for _ in $(seq 1 36); do
  if compose logs --no-log-prefix pipeline 2>/dev/null | grep -q 'worker started service=pipeline'; then started=1; break; fi
  state=$(docker inspect -f '{{.State.Status}} {{.RestartCount}}' "$(compose ps -q pipeline)" 2>/dev/null || echo "missing 0")
  case "$state" in running\ 0|created\ 0) ;; *) break ;; esac
  sleep 5
done
if [ "$started" != 1 ]; then
  compose logs --tail=60 pipeline
  die "the pipeline did not start (state: ${state:-unknown}); its settings errors name variables only"
fi
info "pipeline started"

say "The pilot hospital ($CODE)"
# psql as the owner, the short code passed as a psql variable (quoted by
# psql), never spliced into SQL.
pg() { docker compose exec -T postgres psql -U pgadmin -d mhai -v ON_ERROR_STOP=1 -tA -v code="$CODE" "$@"; }
row=$(printf "SELECT id, status, is_demo FROM hospitals WHERE short_code = :'code';\n" | pg -F ' ')
if [ -z "$row" ]; then
  compose --profile tools run --rm -T core-cli hospitals create --name "$NAME" --short-code "$CODE" \
    --time-zone "$TZONE" --require-mfa-for-clinicians </dev/null
  row=$(printf "SELECT id, status, is_demo FROM hospitals WHERE short_code = :'code';\n" | pg -F ' ')
else
  info "exists, kept (name and settings are not changed here)"
fi
read -r HID HSTATUS HDEMO <<<"$row"
[ "$HDEMO" = f ] || die "$CODE is the synthetic demo hospital: the pilot needs a real (non-demo) hospital"
if [ "$HSTATUS" = active ]; then
  info "active"
else
  compose --profile tools run --rm -T core-cli hospitals activate "$CODE" </dev/null
fi

say "Its model policy: $PROVIDER, agreement $AGREEMENT"
cur=$(printf "SELECT p.cloud_provider, p.agreement_ref, p.monthly_token_budget FROM hospital_model_policy p JOIN hospitals h ON h.id = p.hospital_id WHERE h.short_code = :'code';\n" | pg -F ' ')
read -r CUR_P CUR_REF CUR_BUDGET <<<"${cur:-}"
if [ "${CUR_P:-}" = "$PROVIDER" ] && [ "${CUR_REF:-}" = "$AGREEMENT" ] && { [ -z "$BUDGET" ] || [ "$BUDGET" = "${CUR_BUDGET:-}" ]; }; then
  info "already set, kept (no new audit entry)"
else
  set -- hospitals model-policy set "$CODE" --provider "$PROVIDER" --agreement-ref "$AGREEMENT"
  [ -z "$BUDGET" ] || set -- "$@" --budget "$BUDGET"
  compose --profile tools run --rm -T core-cli "$@" </dev/null
  info "set and audited (hospital.model_policy_changed, via cli)"
fi

if [ "$SEED_GUIDELINES" = 1 ]; then
  say "Shared reference guidelines (services/intelligence/init-db/seed_kb.py)"
  # The six shared reference guidelines (hospital_id NULL: every hospital
  # reads them, none can change them; no patient data). Kept if present.
  pg >/dev/null <<'SQL'
INSERT INTO kb_chunks (chunk_id, hospital_id, content, chunk_type) VALUES
 ('KB#0001', NULL, 'Gestational Diabetes Mellitus (GDM) diagnosed when fasting glucose >= 92 mg/dL, 1-hr >= 180, or 2-hr >= 153 on 75g OGTT.', 'guideline'),
 ('KB#0002', NULL, 'Malaria requires parasitological confirmation by microscopy or RDT. Severe features: Hb < 5, respiratory distress, parasitemia > 10%.', 'guideline'),
 ('KB#0003', NULL, 'Hypertension defined as systolic >= 140 or diastolic >= 90 on two occasions. First-line: thiazides, ACEi, ARBs, CCBs.', 'guideline'),
 ('KB#0004', NULL, 'Neonatal jaundice: physiological onset > 24hr, peaks day 3-5, total bili < 15. Pathological: onset < 24hr, total > 17.', 'guideline'),
 ('KB#0005', NULL, 'URTI: viral majority. Supportive: paracetamol, hydration. Antibiotics only if bacterial superinfection.', 'guideline'),
 ('KB#0006', NULL, 'T2DM: fasting glucose >= 126, 2-hr OGTT >= 200, HbA1c >= 6.5%. First-line: Metformin 500mg BID.', 'guideline')
ON CONFLICT (hospital_id, chunk_id) DO NOTHING;
SQL
  info "$(printf 'SELECT count(*) FROM kb_chunks WHERE hospital_id IS NULL;\n' | pg) shared guideline chunks"
fi

say "Done"
info "Hospital $CODE ($HID): cloud answers from $PROVIDER ($MODEL), agreement $AGREEMENT, ZDR $ZDR."
info "Next: create its users (README, step 3) and run the checklist (README, 'Check it')."

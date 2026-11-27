#!/usr/bin/env bash
# Switch the EMR integration on (deploy/README.md, "EMR integration";
# docs/deploy/production-tls-emr.md). Production mode first
# (production/enable-production.sh): the emr service speaks TLS only. Run by
# the operator, as the deploy user, on the host:
#
#   bash /srv/mhai-backend/emr/enable-emr.sh --check
#   bash /srv/mhai-backend/emr/enable-emr.sh [--connector-types fhir_r4,file_export]
#        [--internal-cidrs CIDR,...]
#   bash /srv/mhai-backend/emr/enable-emr.sh --disable
#
#   --check            writes nothing: what is ready and what would be made.
#   (none)             makes, each kept when it is already there:
#                      - the emr NATS NKey (secrets/nats-emr.nk, 0400, uid
#                        65532; only its public key goes to .env,
#                        NATS_EMR_NKEY), made in a container with no network;
#                      - EMR_DB_PASSWORD in .env (core-migrate makes the
#                        emr_app login, a member of mhai_emr_app only);
#                      - the sealing key pair (emr/seal: seal-<kid>.key 0400,
#                        emr keys seal-init) and its PUBLIC half for core
#                        (secrets/emr-seal.pub);
#                      - the file secret store folder (emr/store, 0700);
#                      - K_proxy (secrets/emr-proxy.key, emr keys proxy-init);
#                      - core's idempotency HMAC key
#                        (secrets/core-idempotency-hmac.key, 32 random bytes);
#                      - EGRESS_EMR_INTERNAL_CIDRS: every Docker network on
#                        this host and the host's own addresses (/32), plus
#                        --internal-cidrs;
#                      then appends compose.emr.yml to COMPOSE_FILE, sets
#                      CORE_EMR_CONNECTOR_TYPES and EMR_ENABLED=true, and
#                      restarts: NATS (only if the emr key is new), core
#                      (migrations: the login; EMR on), then egress-emr and
#                      emr, and checks the service's start line. A failure
#                      puts the settings back and restarts core without EMR.
#   --disable          EMR_ENABLED=false, compose.emr.yml out of COMPOSE_FILE,
#                      emr and egress-emr stopped, core restarted without EMR.
#                      The keys, the stored credentials and the login stay.
#   --connector-types  the types the platform may offer, from fhir_r4,
#                      file_export and hl7v2_file (default fhir_r4,file_export;
#                      kept in .env as CORE_EMR_CONNECTOR_TYPES, so a re-run
#                      without it keeps the list). hl7v2_file (ADR 0019) is
#                      off until it is listed here.
#
# It prints no secret and no key, and touches only this backend's compose
# project.
set -euo pipefail
umask 077

DEST=${MHAI_BACKEND_DIR:-/srv/mhai-backend}
SRC=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=deploy/tls/split-lib.sh
. "$SRC/tls/split-lib.sh"
# shellcheck source=deploy/production/prod-lib.sh
. "$SRC/production/prod-lib.sh"
KNOWN_TYPES=" fhir_r4 file_export hl7v2_file "

usage() { sed -n '7,11p' "$0" >&2; exit 2; }
MODE=apply TYPES='' EXTRA=''
while [ $# -gt 0 ]; do
  case "$1" in
    --check) MODE=check; shift ;;
    --disable) MODE=disable; shift ;;
    --connector-types) [ $# -ge 2 ] || usage; TYPES=$2; shift 2 ;;
    --internal-cidrs) [ $# -ge 2 ] || usage; EXTRA=$2; shift 2 ;;
    *) usage ;;
  esac
done

PROBLEMS=0
problem() { if [ "$MODE" = check ]; then printf '    PROBLEM: %s\n' "$*" >&2; PROBLEMS=$((PROBLEMS + 1)); else die "$*"; fi; }

say "Prerequisites"
for bin in docker openssl curl jq; do command -v "$bin" >/dev/null || die "$bin is not installed"; done
[ "$(id -u)" -ne 0 ] || die "run as the deploy user, not root"
[ -f "$DEST/.env" ] && [ -f "$DEST/secrets/nats-gateway.nk" ] || die "$DEST is not set up (deploy/setup-host.sh)"
cd "$DEST"
dc() { docker compose --profile emr "$@"; }

restart_core() { # core with the settings now in .env; readyz
  docker compose up -d core || { docker compose logs --tail=60 core-migrate core; return 1; }
  core_ready || { docker compose logs --tail=80 core; return 1; }
}

if [ "$MODE" = disable ]; then
  say "Switching the EMR off"
  env_set EMR_ENABLED false
  env_set COMPOSE_FILE "$(cf_without "$(cf_get)" "$EMR_OVERLAY")"
  dc stop emr egress-emr >/dev/null 2>&1 || true
  dc rm -f emr egress-emr >/dev/null 2>&1 || true
  t0=$(now_s)
  restart_core || die "core did not come back without EMR"
  say "EMR off (core back in $(elapsed "$t0" "$(now_s)") s). Keys, stored credentials and the emr_app login are kept."
  exit 0
fi

production_on && [ -f tls/public/emr.pem ] \
  || problem "production mode is not on (production/enable-production.sh first): the emr service speaks TLS only"
dc config --services 2>/dev/null | grep -x emr >/dev/null || problem "compose.yml has no emr service: deploy the release with it first"
grep -F 'nkey: $NATS_EMR_NKEY' nats/nats.conf >/dev/null || problem "nats/nats.conf has no emr user: deploy the release with it first"
for f in compose.emr.yml emr/emr.sh; do
  [ -f "$SRC/$f" ] || die "$SRC/$f is missing: run this from the release's deploy/"
done
[ "$(env_get ANALYSIS_NAMED_IDENTITY)" != demo-fixture ] || problem "ANALYSIS_NAMED_IDENTITY=demo-fixture: core refuses it with the EMR on"
TYPES=${TYPES:-$(env_get CORE_EMR_CONNECTOR_TYPES)}
TYPES=${TYPES:-fhir_r4,file_export}
[[ $TYPES =~ ^[a-z0-9_]+(,[a-z0-9_]+)*$ ]] || problem "--connector-types must be a comma-separated list"
IFS=, read -r -a tl <<<"$TYPES"
for t in "${tl[@]}"; do
  case "$KNOWN_TYPES" in *" $t "*) ;; *) problem "--connector-types: $t is not a production connector type (fhir_r4, file_export, hl7v2_file)" ;; esac
done
EMR_IMAGE=$(env_get EMR_IMAGE); EMR_IMAGE=${EMR_IMAGE:-ghcr.io/measured-health/mhai-multiagent-emr:latest}
if ! docker image inspect "$EMR_IMAGE" >/dev/null 2>&1; then
  if [ "$MODE" = check ]; then info "would pull $EMR_IMAGE"
  else docker pull -q "$EMR_IMAGE" >/dev/null 2>&1 || die "could not pull $EMR_IMAGE (deploy a release that publishes it, or docker login ghcr.io)"; fi
fi
avail=$(awk '/^MemAvailable:/ {print int($2/1024)}' "${MEMINFO:-/proc/meminfo}")
[ "$avail" -ge 400 ] || problem "only $avail MiB available: emr and egress-emr take up to 320 MiB"
info "checked (connector types $TYPES)"

# EGRESS_EMR_INTERNAL_CIDRS: never reached, even when a connection declares
# them. Every Docker network here (this backend's, the web apps', any other
# stack's) and the host's own addresses; egress-emr adds its own networks,
# its gateways and the default bridge at start.
say "Addresses egress-emr never reaches"
cidrs=()
while IFS= read -r n; do
  while IFS= read -r sn; do [ -z "$sn" ] || cidrs+=("$sn"); done < <(docker network inspect -f '{{range .IPAM.Config}}{{println .Subnet}}{{end}}' "$n" 2>/dev/null)
done < <(docker network ls -q)
for a in $(hostname -I 2>/dev/null); do
  case "$a" in *:*) cidrs+=("$a/128") ;; *.*) cidrs+=("$a/32") ;; esac
done
if [ -n "$EXTRA" ]; then IFS=, read -r -a ex <<<"$EXTRA"; cidrs+=("${ex[@]}"); fi
for c in "${cidrs[@]}"; do
  [[ $c =~ ^[0-9a-fA-F:.]+/[0-9]{1,3}$ ]] || problem "not a CIDR: $c"
done
CIDRS=$(printf '%s\n' "${cidrs[@]}" | awk 'NF && !seen[$0]++' | paste -sd, -)
[ -n "$CIDRS" ] || problem "no internal network found"
info "$(printf '%s' "$CIDRS" | tr ',' '\n' | wc -l) networks and addresses"

if [ "$MODE" = check ]; then
  for f in secrets/nats-emr.nk secrets/emr-proxy.key secrets/emr-seal.pub secrets/core-idempotency-hmac.key; do
    if [ -s "$f" ]; then info "$f present"; else info "would make $f"; fi
  done
  [ -n "$(env_get EMR_DB_PASSWORD)" ] && info "EMR_DB_PASSWORD set" || info "would generate EMR_DB_PASSWORD"
  if [ "$(env_get EMR_ENABLED)" = true ]; then info "EMR is ON"; else info "EMR is off: the switch would restart core (about 10 s) and NATS if its key is new (a few seconds)"; fi
  [ "$PROBLEMS" = 0 ] || { say "$PROBLEMS problem(s): the switch would refuse"; exit 1; }
  say "Ready"
  exit 0
fi

say "Keys and secrets (each kept when present; none shown)"
mkdir -p "$DEST/secrets" "$DEST/emr"; chmod 700 "$DEST/secrets"; chmod 755 "$DEST/emr"
# The NKey: made in nats-box with no network; only the public key comes back.
pub=$(helper -e U="$EMR_UID" -v "$DEST/secrets:/s" "$HELPER_IMAGE" -c '
  set -eu; umask 077
  if [ -s /s/nats-emr.nk ]; then echo "emr NKey exists, kept" >&2
  else out=$(nsc generate nkey --user 2>/dev/null); printf "%s\n" "$out" | sed -n 1p > /s/nats-emr.nk; unset out; echo "emr NKey generated" >&2; fi
  case "$(head -c 2 /s/nats-emr.nk)" in SU) ;; *) echo "not a user seed" >&2; exit 1;; esac
  chown "$U:$U" /s/nats-emr.nk; chmod 400 /s/nats-emr.nk
  nats auth nkey show /s/nats-emr.nk' 2> >(sed 's/^/    /' >&2))
[[ $pub =~ ^U[A-Z2-7]{55}$ ]] || die "could not read the emr service's public key"
NATS_KEY_CHANGED=0
[ "$(env_get NATS_EMR_NKEY)" = "$pub" ] || NATS_KEY_CHANGED=1

# The folders the service owns (0700, uid 65532), the K_proxy and idempotency
# keys (0400, uid 65532), made by root in a container with no network.
helper -e U="$EMR_UID" -v "$DEST/emr:/e" -v "$DEST/secrets:/s" "$HELPER_IMAGE" -c '
  set -eu; umask 077
  for d in /e/seal /e/store /s/.emr-proxy-init; do mkdir -p "$d"; chown "$U:$U" "$d"; chmod 700 "$d"; done
  if [ ! -s /s/core-idempotency-hmac.key ]; then
    t=$(mktemp /s/.idem.XXXXXX); head -c 32 /dev/urandom | base64 > "$t"
    chown "$U:$U" "$t"; chmod 400 "$t"; mv -f "$t" /s/core-idempotency-hmac.key; echo "idempotency HMAC key made" >&2
  else echo "idempotency HMAC key exists, kept" >&2; fi' 2> >(sed 's/^/    /' >&2)
# The sealing pair and K_proxy: the emr image's own commands, as its user.
docker run --rm --network none --user "$EMR_UID" -v "$DEST/emr/seal:/out" "$EMR_IMAGE" keys seal-init --out /out | sed 's/^/    /'
if [ ! -s "$DEST/secrets/emr-proxy.key" ]; then
  docker run --rm --network none --user "$EMR_UID" -v "$DEST/secrets/.emr-proxy-init:/out" "$EMR_IMAGE" keys proxy-init --out /out | sed 's/^/    /'
fi
helper -e U="$EMR_UID" -v "$DEST/emr:/e" -v "$DEST/secrets:/s" "$HELPER_IMAGE" -c '
  set -eu; umask 077
  if [ -s /s/.emr-proxy-init/k_proxy ]; then mv -f /s/.emr-proxy-init/k_proxy /s/emr-proxy.key; fi
  rmdir /s/.emr-proxy-init
  chown "$U:$U" /s/emr-proxy.key; chmod 400 /s/emr-proxy.key
  n=$(ls /e/seal/seal-*.pub 2>/dev/null | wc -l)
  [ "$n" = 1 ] || { echo "emr/seal holds $n public keys: expected one (a rotation is in progress; see the README)" >&2; exit 1; }
  t=$(mktemp /s/.seal.XXXXXX); cat /e/seal/seal-*.pub > "$t"; chmod 444 "$t"; mv -f "$t" /s/emr-seal.pub'
info "nats-emr.nk, emr-proxy.key, core-idempotency-hmac.key (0400, uid $EMR_UID); emr/seal, emr/store (0700); emr-seal.pub (public)"

# What to put back if the switch fails.
SAVED_FILES=$(cf_get) SAVED_ENABLED=$(env_get EMR_ENABLED) SAVED_TYPES=$(env_get CORE_EMR_CONNECTOR_TYPES)
say ".env"
env_set NATS_EMR_NKEY "$pub"
env_secret EMR_DB_PASSWORD
env_set EGRESS_EMR_INTERNAL_CIDRS "$CIDRS"
env_set CORE_EMR_CONNECTOR_TYPES "$TYPES"
[ "$SRC/compose.emr.yml" -ef "$DEST/compose.emr.yml" ] || install -m 644 "$SRC/compose.emr.yml" "$DEST/compose.emr.yml"
chmod 644 "$DEST/compose.emr.yml"
mkdir -p "$DEST/emr"
[ "$SRC/emr/emr.sh" -ef "$DEST/emr/emr.sh" ] || install -m 644 "$SRC/emr/emr.sh" "$DEST/emr/emr.sh"
COMPOSE_FILE=$(cf_with "$(cf_get)" "$EMR_OVERLAY") docker compose config --quiet || die "the compose files with $EMR_OVERLAY don't validate (nothing restarted)"

revert() {
  warn "putting the settings back"
  env_set COMPOSE_FILE "$SAVED_FILES"
  if [ -n "$SAVED_ENABLED" ]; then env_set EMR_ENABLED "$SAVED_ENABLED"; else env_unset EMR_ENABLED; fi
  if [ -n "$SAVED_TYPES" ]; then env_set CORE_EMR_CONNECTOR_TYPES "$SAVED_TYPES"; fi
  dc stop emr egress-emr >/dev/null 2>&1 || true
  dc rm -f emr egress-emr >/dev/null 2>&1 || true
  restart_core || warn "core did not come back: look at its log"
}
say "Switch"
t0=$(now_s)
env_set COMPOSE_FILE "$(cf_with "$(cf_get)" "$EMR_OVERLAY")"
env_set EMR_ENABLED true
if [ "$NATS_KEY_CHANGED" = 1 ]; then
  docker compose up -d --force-recreate --no-deps nats
  info "nats recreated with the emr service's public key"
fi
if ! restart_core; then revert; die "core did not start with the EMR on (settings put back)"; fi
t_core=$(now_s)
info "core ready with the EMR on ($(elapsed "$t0" "$t_core") s)"
if ! EMR_START_TRIES=36 bash "$DEST/emr/emr.sh"; then revert; die "the emr service did not start (settings put back)"; fi
say "EMR on: core was back in $(elapsed "$t0" "$t_core") s; emr serving after $(elapsed "$t0" "$(now_s)") s."
cat <<EOF
    Next (platform admin, in the platform app): Records connections, add one
    per hospital (types: $TYPES). Nothing is polled yet: the code tables are
    synthetic until H6 (deploy/README.md, "EMR integration: what is not on").
EOF

#!/usr/bin/env bash
# Two hosts on one machine (docs/deploy/remote-intelligence.md, "Abuse-case
# tests"): Docker networks stand in for the app host, the private network
# and the intelligence host, and the real files do the work:
#
#   split-app   internal: the app host's Docker network (Postgres, NATS, core's
#               one-shots)
#   split-lan   10.213.47.0/24, the private network: the app host at .10
#               (Postgres and NATS share one network namespace, so one address),
#               the intelligence host's app-link at .20, a rogue machine at .30
#   split-intel internal: the intelligence host's backend (the worker, app-link)
#
# Postgres and NATS run the commands deploy/compose.split.yml gives them, with
# nats/tls.conf and pg_hba.conf rendered by postgres/render-hba.sh; app-link
# runs intelligence-host/app-link.cfg; the worker check runs in the
# intelligence image with the pipeline's environment from
# intelligence-host/compose.yml, through the worker's own code (settings,
# open_database, TLS-first connect). Every certificate, key and password is
# made here for this run and removed at the end; none is printed.
#
#   bash deploy/intelligence-host/test-split.sh
#
# Needs docker, docker compose v2, jq and openssl. Images: CORE_IMAGE
# (mhai-core:local) and INTEL_IMAGE (mhai-intelligence:cpu), built by CI's
# boundary-smoke job; the rest are pulled. Every Docker object is named split-*.
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
CORE_IMAGE=${CORE_IMAGE:-mhai-core:local}
INTEL_IMAGE=${INTEL_IMAGE:-mhai-intelligence:cpu}
PG_IMAGE=postgres:16.15-alpine
NATS_IMAGE=nats:2.10.29-alpine
BOX_IMAGE=natsio/nats-box:0.19.0
ALPINE=alpine:3.20
LAN=10.213.47
APP=$LAN.10 INTEL=$LAN.20 ROGUE=$LAN.30

umask 077
work=$(mktemp -d)
fail=0
ok()  { echo "ok   $1"; }
bad() { echo "FAIL $1"; fail=1; }

cleanup() {
  if [ "$fail" != 0 ]; then
    for c in split-pg split-nats split-applink; do
      echo "--- $c" >&2; docker logs --tail 30 "$c" >&2 2>/dev/null || true
    done
  fi
  docker rm -f split-pg split-nats split-applink >/dev/null 2>&1 || true
  docker network rm split-app split-lan split-intel >/dev/null 2>&1 || true
  docker volume rm split-pgtls split-natsconf split-corekeys split-trusted split-inteltls \
    split-intelsecrets split-applinkcfg split-boxsecrets split-boxtls >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT
cleanup_quiet() { fail=0; cleanup; work=$(mktemp -d); }
cleanup_quiet

# fill VOLUME UID: the files in $work/fill (a tar stream, no bind mount, so it
# works when docker is another host's daemon), owned by UID, keys 0400.
fill() {
  docker volume create "$1" >/dev/null
  tar -C "$work/fill" -cf - . | docker run --rm -i --network none -v "$1:/v" "$ALPINE" sh -c "
    set -e; tar -xf - -C /v; chown -R $2:$2 /v; chmod 755 /v; find /v -type f -exec chmod 444 {} +
    find /v -type f \( -name '*key*' -o -name '*.nk' -o -name '*password*' \) -exec chmod $3 {} +"
  rm -rf "$work/fill"; mkdir -p "$work/fill"
}
mkdir -p "$work/fill"
secret() { openssl rand -hex 24; }

echo "== certificates (a CA for this run, and a wrong one)"
bash "$ROOT/deploy/tls/make-ca.sh" "$work/ca" >/dev/null
bash "$ROOT/deploy/tls/make-ca.sh" "$work/wrongca" >/dev/null
iss() { bash "$ROOT/deploy/tls/issue.sh" --ca-dir "$work/ca" --out "$work/certs" "$@" >/dev/null; }
iss --name nats --kind server --dns nats,mhai-nats,mhai-app --ip "$APP"
iss --name postgres --kind server --dns postgres,mhai-app --ip "$APP"
iss --name gateway --kind client
# The pipeline's key and CSR, as the intelligence host makes them.
openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$work/pipeline-key.pem" 2>/dev/null
openssl req -new -key "$work/pipeline-key.pem" -subj /CN=pipeline -out "$work/pipeline.csr" 2>/dev/null
iss --name pipeline --kind client --csr "$work/pipeline.csr"

echo "== NKeys (made in nats-box; only the public keys come back)"
docker volume create split-boxsecrets >/dev/null
nkey() {
  docker run --rm --network none -v split-boxsecrets:/s --entrypoint sh "$BOX_IMAGE" -c "
    set -eu; umask 077; nsc generate nkey --user 2>/dev/null | sed -n 1p > /s/$1.nk; nats auth nkey show /s/$1.nk"
}
PIPE_PUB=$(nkey pipeline) GW_PUB=$(nkey gateway)
case "$PIPE_PUB$GW_PUB" in U*U*) ;; *) echo "no NKeys" >&2; exit 1 ;; esac
# The BFFs' public keys: valid keys whose seeds were thrown away (check-configs.sh).
BFF_PUB=UDSUWROES6WBAXF6XZO7TXQ2YM2YMJPIPSGOHF3OO3JUFOYQKVXWGKRV
BFF_PLATFORM_PUB=UCAAJGKAIDMU5SIXQWLWPWO5MJJMRUCMVTZ2MTVTXH75WFBW4TDX4TT7

PG_PW=$(secret) CORE_PW=$(secret) PROV_PW=$(secret) INTEL_PW=$(secret)

echo "== the commands compose.split.yml gives Postgres and NATS"
cat > "$work/deploy.env" <<E
POSTGRES_PASSWORD=x
CORE_DB_PASSWORD=x
PROVISION_DB_PASSWORD=x
MFA_ENCRYPTION_KEY=x
BFF_SERVICE_KEYS=x
NATS_GATEWAY_NKEY=x
NATS_BFF_NKEY=x
NATS_BFF_PLATFORM_NKEY=x
SPLIT_APP_ADDR=$APP
E
app_cfg=$(docker compose -p split-app -f "$ROOT/deploy/compose.yml" -f "$ROOT/deploy/compose.split.yml" \
  --env-file "$work/deploy.env" config --format json)
mapfile -t PG_CMD < <(jq -r '.services.postgres.command[]' <<<"$app_cfg")
mapfile -t NATS_CMD < <(jq -r '.services.nats.command[]' <<<"$app_cfg")
if [ "$(jq -r '.services.core.environment.NATS_URL' <<<"$app_cfg")" = tls://nats:4222 ]; then
  ok "compose.split.yml: core reaches NATS over TLS"
else bad "compose.split.yml: core reaches NATS over TLS"; fi
intel_cfg=$(APP_HOST_ADDR=$APP docker compose -p split-intel -f "$ROOT/deploy/intelligence-host/compose.yml" config --format json)
mapfile -t PIPE_ENV < <(jq -r '.services.pipeline.environment | to_entries[] | "\(.key)=\(.value)"' <<<"$intel_cfg")
mapfile -t LINK_CMD < <(jq -r '.services."app-link".command[]' <<<"$intel_cfg")

echo "== networks"
docker network create --internal split-app >/dev/null
docker network create --subnet "$LAN.0/24" split-lan >/dev/null
docker network create --internal split-intel >/dev/null
APP_SUBNET=$(docker network inspect -f '{{(index .IPAM.Config 0).Subnet}}' split-app)

echo "== the app host: Postgres with TLS and the split pg_hba"
cp "$work/certs/postgres.pem" "$work/fill/server.pem"
cp "$work/certs/postgres-key.pem" "$work/fill/server-key.pem"
bash "$ROOT/deploy/postgres/render-hba.sh" "$INTEL" "$APP_SUBNET" > "$work/fill/pg_hba.conf"
fill split-pgtls 70 600
docker run -d --name split-pg --network split-app --user 70:70 \
  -e POSTGRES_DB=mhai -e POSTGRES_USER=pgadmin -e POSTGRES_PASSWORD="$PG_PW" \
  -e POSTGRES_INITDB_ARGS=--auth-host=scram-sha-256 \
  -v split-pgtls:/etc/postgresql/tls:ro \
  --mount type=volume,src=split-pgtls,dst=/etc/postgresql/pg_hba.conf,volume-subpath=pg_hba.conf,readonly \
  "$PG_IMAGE" "${PG_CMD[@]}" >/dev/null
docker network connect --ip "$APP" split-lan split-pg
# Over TCP, not the Unix socket: the image's entrypoint first runs a temporary
# server for initdb that listens on the socket only, then stops it and starts
# the real one. A socket check could pass in that first window and the next
# command land in the restart (boundary-smoke flake, 14 Nov).
for _ in $(seq 1 60); do
  docker exec split-pg pg_isready -h 127.0.0.1 -U pgadmin -d mhai >/dev/null 2>&1 && break; sleep 1
done
docker exec split-pg pg_isready -h 127.0.0.1 -U pgadmin -d mhai >/dev/null || { bad "postgres starts"; exit 1; }

echo "== the app host: NATS with nats/tls.conf, in the same network namespace"
cp "$ROOT/deploy/nats/nats.conf" "$ROOT/deploy/nats/tls.conf" "$work/fill/"
mkdir -p "$work/fill/tls"
cp "$work/ca/ca.pem" "$work/fill/tls/ca.pem"
cp "$work/certs/nats.pem" "$work/fill/tls/server.pem"
cp "$work/certs/nats-key.pem" "$work/fill/tls/server-key.pem"
fill split-natsconf 1000 400
docker run -d --name split-nats --network container:split-pg --user 1000:1000 \
  -e NATS_GATEWAY_NKEY="$GW_PUB" -e NATS_BFF_NKEY="$BFF_PUB" -e NATS_BFF_PLATFORM_NKEY="$BFF_PLATFORM_PUB" \
  -e NATS_PIPELINE_NKEY="$PIPE_PUB" -e NATS_NOTIFICATIONS_NKEY=UANAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABKPD \
  -e NATS_EMR_NKEY=UAEV5RWVELI3WEITFTWOF7WXDBONZP4YB5NR5KS2IHRE2MBLRID4XPIV -v split-natsconf:/etc/nats:ro --tmpfs /data:uid=1000 \
  "$NATS_IMAGE" "${NATS_CMD[@]}" >/dev/null
for _ in $(seq 1 30); do docker logs split-nats 2>&1 | grep 'Server is ready' >/dev/null && break; sleep 1; done
docker logs split-nats 2>&1 | grep 'Server is ready' >/dev/null || { bad "nats starts with tls.conf"; exit 1; }
ok "nats starts with tls.conf"

echo "== core's one-shots: migrations (they make intelligence_app) and the request key"
docker run --rm --network split-app -e DATABASE_URL="postgres://pgadmin:$PG_PW@split-pg:5432/mhai?sslmode=disable" \
  -e CORE_DB_USER=core_app -e CORE_DB_PASSWORD="$CORE_PW" -e PROVISION_DB_USER=core_provision \
  -e PROVISION_DB_PASSWORD="$PROV_PW" -e INTELLIGENCE_DB_PASSWORD="$INTEL_PW" \
  "$CORE_IMAGE" migrate up >/dev/null 2>&1 || { bad "core-migrate on the app network"; exit 1; }
ok "core-migrate on the app network (the Docker subnet line)"
for v in split-corekeys split-trusted; do
  docker volume create "$v" >/dev/null
  docker run --rm --network none -v "$v:/v" "$ALPINE" chown 65532:65532 /v
done
docker run --rm --network none -v split-corekeys:/keys -v split-trusted:/trusted "$CORE_IMAGE" \
  keys ensure --kind request --out /keys/request.pem --trusted-out /trusted/request-keys.json >/dev/null

echo "== the intelligence host: app-link at $INTEL, the worker on split-intel"
cp "$ROOT/deploy/intelligence-host/app-link.cfg" "$work/fill/haproxy.cfg"
fill split-applinkcfg 0 444
docker run -d --name split-applink --network split-intel --network-alias mhai-app \
  --read-only --cap-drop ALL --tmpfs /tmp -e APP_HOST_ADDR="$APP" \
  -v split-applinkcfg:/usr/local/etc/haproxy:ro haproxy:3.0.12-alpine "${LINK_CMD[@]}" >/dev/null
docker network connect --ip "$INTEL" split-lan split-applink
APP_UID=$(docker run --rm --network none --entrypoint id "$INTEL_IMAGE" -u)
cp "$work/ca/ca.pem" "$work/fill/ca.pem"
cp "$work/wrongca/ca.pem" "$work/fill/wrong-ca.pem"
cp "$work/certs/pipeline.pem" "$work/fill/pipeline.pem"
cp "$work/pipeline-key.pem" "$work/fill/pipeline-key.pem"
fill split-inteltls "$APP_UID" 400
printf '%s' "$INTEL_PW" > "$work/fill/intelligence-db-password"
fill split-intelsecrets "$APP_UID" 400
docker run --rm --network none -v split-boxsecrets:/b -v split-intelsecrets:/s "$ALPINE" \
  sh -c "cp /b/pipeline.nk /s/nats-pipeline.nk && chown $APP_UID:$APP_UID /s/nats-pipeline.nk && chmod 400 /s/nats-pipeline.nk"

check_py='
import asyncio, sys
from intelligence.boundary.config import WorkerSettings
from intelligence.boundary.services import pipeline
from intelligence.boundary.worker import Worker, _is_tls, build_verifier, open_database

async def main() -> None:
    s = WorkerSettings.from_env(service="pipeline")
    db = await asyncio.wait_for(open_database(s), 20)
    print("database ok (verify-full, intelligence_app, login checked)")
    v, _ = build_verifier(s)
    w = Worker(s, pipeline.handlers(), v)
    try:
        await asyncio.wait_for(w.start(), 15)
        assert _is_tls(w.connection), "not TLS"
        print("nats ok (TLS first, client certificate, NKey, queue group joined)")
    finally:
        await w.abort()
        if db is not None:
            await db.close()

asyncio.run(main())
'
worker_check() { # worker_check NAME=VALUE...: the pipeline's environment, overridden
  local envs=() e
  for e in "${PIPE_ENV[@]}" "$@"; do envs+=(-e "$e"); done
  docker run --rm --network split-intel --read-only --tmpfs /tmp --cap-drop ALL \
    -v split-inteltls:/run/tls:ro -v split-trusted:/trusted:ro \
    -v split-intelsecrets:/run/secrets:ro "${envs[@]}" \
    --entrypoint python "$INTEL_IMAGE" -c "$check_py"
}

if out=$(worker_check 2>&1); then
  ok "the worker connects through app-link: $(printf '%s' "$out" | grep -c ' ok ') links verified"
else
  printf '%s\n' "$out" | tail -5 >&2; bad "the worker connects through app-link"
fi
if worker_check NATS_TLS_CA_PATH=/run/tls/wrong-ca.pem >/dev/null 2>&1; then
  bad "the worker refuses a NATS server signed by another CA"
else ok "the worker refuses a NATS server signed by another CA"; fi
if worker_check "INTELLIGENCE_DB_URL=postgres://mhai-app:5432/mhai?sslmode=verify-full&sslrootcert=/run/tls/wrong-ca.pem" >/dev/null 2>&1; then
  bad "the worker refuses a Postgres signed by another CA"
else ok "the worker refuses a Postgres signed by another CA"; fi
if worker_check "INTELLIGENCE_DB_URL=postgres://mhai-app:5432/mhai?sslmode=disable" >/dev/null 2>&1; then
  bad "the worker refuses plaintext Postgres to a remote host"
else ok "the worker refuses plaintext Postgres to a remote host"; fi
if worker_check NATS_URL=nats://mhai-app:4222 NATS_TLS_CA_PATH= NATS_TLS_CERT_PATH= NATS_TLS_KEY_PATH= >/dev/null 2>&1; then
  bad "the worker refuses NATS without TLS to a remote host"
else ok "the worker refuses NATS without TLS to a remote host"; fi

echo "== Postgres refuses everything else"
pg_try() { # pg_try NETWORK USER PASSWORD HOST SSLMODE
  docker run --rm --network "$1" -e PGPASSWORD="$3" -e PGCONNECT_TIMEOUT=5 "$PG_IMAGE" \
    psql "host=$4 dbname=mhai user=$2 sslmode=$5" -tAc 'select 1' >/dev/null 2>&1
}
if pg_try split-intel intelligence_app "$INTEL_PW" mhai-app disable; then
  bad "intelligence_app without TLS from the intelligence host"
else ok "intelligence_app without TLS from the intelligence host is refused"; fi
if pg_try split-intel core_app "$CORE_PW" mhai-app require; then
  bad "core_app from the intelligence host"
else ok "core_app from the intelligence host is refused (TLS or not)"; fi
if pg_try split-intel pgadmin "$PG_PW" mhai-app require; then
  bad "the owner from the intelligence host"
else ok "the owner from the intelligence host is refused"; fi
rogue() { docker run --rm --network split-lan --ip "$ROGUE" "$@"; }
if rogue -e PGPASSWORD="$INTEL_PW" -e PGCONNECT_TIMEOUT=5 "$PG_IMAGE" \
    psql "host=$APP dbname=mhai user=intelligence_app sslmode=require" -tAc 'select 1' >/dev/null 2>&1; then
  bad "intelligence_app from another address"
else ok "intelligence_app from another address is refused"; fi
if rogue -e PGPASSWORD="$PG_PW" -e PGCONNECT_TIMEOUT=5 "$PG_IMAGE" \
    psql "host=$APP dbname=mhai user=pgadmin sslmode=require" -tAc 'select 1' >/dev/null 2>&1; then
  bad "the owner from another address on the private network"
else ok "the owner from another address on the private network is refused"; fi

echo "== NATS refuses a client without a certificate, and plaintext"
cp "$work/ca/ca.pem" "$work/certs/gateway.pem" "$work/certs/gateway-key.pem" "$work/fill/"
fill split-boxtls 0 400
# box ARGS: nats rtt from the rogue address. nats rtt exits 0 even when it can't
# connect, so a connection counts only when the output names no failure.
box() {
  rogue -v split-boxsecrets:/s:ro -v split-boxtls:/t:ro "$BOX_IMAGE" nats --timeout 5s "$@" rtt >"$work/box.out" 2>&1 || return 1
  [ -z "${SPLIT_DEBUG:-}" ] || cat "$work/box.out"
  ! grep -qiE 'fail|error|violation' "$work/box.out" && grep -qE '[0-9.]+(µs|ms|s)$' "$work/box.out"
}
if box --server "tls://$APP:4222" --tlsca /t/ca.pem --tlscert /t/gateway.pem --tlskey /t/gateway-key.pem --nkey /s/gateway.nk; then
  ok "an INFO-first client with its certificate and NKey connects (core, the BFFs)"
else bad "an INFO-first client with its certificate and NKey connects"; fi
if box --server "tls://$APP:4222" --tlsca /t/ca.pem --nkey /s/gateway.nk; then
  bad "a client without a certificate"
else ok "a client without a certificate is refused (verify: true)"; fi
if box --server "nats://$APP:4222" --nkey /s/gateway.nk; then
  bad "a plaintext client"
else ok "a plaintext client is refused"; fi
if box --server "tls://$APP:4222" --tlsca /t/ca.pem --tlscert /t/gateway.pem --tlskey /t/gateway-key.pem; then
  bad "a certificate without an NKey"
else ok "a certificate without an NKey is refused"; fi

if [ "$fail" != 0 ]; then echo "some failed"; exit 1; fi
echo "all passed"

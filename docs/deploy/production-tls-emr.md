# Production mode (TLS) and the EMR on the single-host backend

Status: built and rehearsed on the build server (27 Nov 2026); production
mode switched on on the live host on 27 Nov 2026. Operator steps: deploy/README.md, "Production mode" and "EMR
integration".

## What changes

Until now the live backend ran as `CORE_ENV=development`: NATS and Postgres
without TLS, the pipeline in `INTELLIGENCE_SINGLE_HOST` mode, notifications as
development, and the EMR off. Two scripts change that, in this order:

1. `deploy/production/enable-production.sh`: a private CA, a server
   certificate for NATS (SANs `nats`, `mhai-nats`) and Postgres (`postgres`),
   a client certificate for each NATS client (core, pipeline, notifications,
   emr, both BFFs). NATS runs `tls.conf` (verify on, so a client certificate is
   needed in addition to the NKey); Postgres runs `ssl=on` with
   `pg_hba.production.conf` (TLS + SCRAM from its own networks only, nothing
   without TLS); every client uses `sslmode=verify-full` with the CA; core runs
   as `production` with `CORE_ALLOW_SINGLE_NODE=true`; the pipeline leaves
   single-host mode; notifications runs as production; the BFFs get
   mhai-web's `compose.nats-tls.yml` overrides. It is a compose overlay
   (`compose.production.yml`) named in `COMPOSE_FILE` in `.env`, so every
   later deploy keeps it, and `--rollback` takes it out again.
2. `deploy/emr/enable-emr.sh`: the emr NKey user (`NATS_EMR_NKEY`), the
   `emr_app` login (`EMR_DB_PASSWORD`, made by core-migrate), the sealing key
   pair, K_proxy, core's idempotency HMAC key, the file secret store, the
   egress deny-list (`EGRESS_EMR_INTERNAL_CIDRS`), and core's EMR settings
   (`compose.emr.yml`, connector types `fhir_r4,file_export`). The deploy then
   runs `emr/emr.sh` every time.

**Why APP_ENV=staging for emr.** Production refuses the file secret store,
and the managed store is not built (plan Task 12, H3). Staging is held to
every production rule that matters here: `tls://` NATS with a client
certificate, `verify-full` Postgres, NKey only (no password), system roots
only for hospital servers (no dev CA), no stub connector, synthetic code
tables refused. It is fixed in compose.yml, not read from `.env`.

**What the EMR does not do yet on live.** The code tables are synthetic until
H6, and they load in development only, so the normaliser and the scheduler do
not start: connections can be created, given credentials and tested, but
nothing is polled. The rehearsal showed records flowing only with emr in
development (rehearsal only).

## Security and threat model

### 1. Assets

- Patient data in transit between core, the workers, emr and Postgres; the
  access tokens and request tokens carried on NATS (spec §17).
- The private CA key: whoever holds it can mint a NATS client certificate
  (still needing an NKey seed) or a Postgres/NATS server certificate that
  every client trusts.
- Each service's TLS key and NKey seed; the database passwords.
- EMR secrets: the sealing private key, K_proxy, the stored hospital
  credentials, the idempotency HMAC key.
- Availability of the live site during the switch.

### 2. Actors and boundaries

Actors: the operator (deploy user), each container's user, another container
or process on the host, anyone on the host's networks, hospital EMR servers.

Boundaries and what is checked at each:

| Crossing | Checked |
| --- | --- |
| BFF, core, workers, emr → NATS | TLS 1.2+, the server's name against the private CA, the client's certificate against the CA (`verify`), then the NKey; per-user subject permissions |
| core, migrate, cli, pipeline, notifications, emr → Postgres | TLS, `verify-full` against the CA, SCRAM password, `pg_hba`: `hostssl ... samenet` only, anything else `reject` |
| emr → hospital | only through egress-emr: K_proxy per CONNECT, the connection's own hosts, internal ranges never (`EGRESS_EMR_INTERNAL_CIDRS`), system roots only |
| operator → host | SSH as the deploy user; the scripts print no secret |

### 3. Threats (STRIDE)

- **Spoofing.** A container on `mhai-bff` or `backend` pretends to be NATS or
  Postgres: refused, the server name is checked against the private CA. A
  client with a stolen NKey but no certificate: refused by `verify`
  (rehearsed: "certificate required" / broken pipe). A certificate without an
  NKey: authorization violation (rehearsed). A certificate from another CA:
  refused (rehearsed).
- **Tampering.** Traffic on the Docker networks is encrypted and
  authenticated end to end; a downgrade to plaintext is impossible: NATS sets
  `tls_required` and Postgres rejects non-TLS (rehearsed: plaintext NATS is
  upgraded and fails on the CA; `sslmode=disable` gets `pg_hba.conf rejects
  connection ... no encryption`).
- **Repudiation.** Unchanged: core's audit chain. The scripts log what they
  changed (names of settings and files, never values).
- **Information disclosure.** No key, password or seed is printed, put in an
  argument or in `.env` (only public keys and generated passwords, which were
  already there). Keys are 0400 (Postgres's 0600) and owned by the service's
  uid, in 0700 folders. Core never sees the sealing private key or the
  secret store (a test holds it). Public certificates are copied to
  `tls/public` for checks.
- **Denial of service.** A failed switch rolls itself back. A certificate
  that expires would stop every link: the deploy's `production/check-mode.sh`
  refuses to restart anything with an expired certificate and warns 30 days
  before. Core's pool (`pool_max_conns=16`) is sized so its leader loops
  cannot exhaust it (the rehearsal found that with the 2-CPU cpuset pgx's
  default of 4 deadlocked readyz once the EMR loop was on).
- **Elevation of privilege.** Every service keeps its own NKey and DB login;
  the certificates add no rights. The emr login is a member of
  `mhai_emr_app` only. The CA key is not on the host after issuing (below).

### 4. Controls

- **CA custody (decision, recorded).** The CA is made on the host into
  `--ca-dir` (default `~/mhai-ca`, outside `/srv/mhai-backend` and out of
  the backups), used to issue, then **moved off the host** by the operator to
  an encrypted store (the owner's password manager as an attachment, or an
  encrypted USB key kept offline), and the host copy shredded. The scripts
  warn while it is still there. It is needed again only to renew (yearly) or
  to add a client. The CA is ECDSA P-256, 10 years; leaf certificates 365
  days.
- **Renewal.** `enable-production.sh --renew --ca-dir <CA>` (the CA brought
  back for the run), then the same one restart sequence. Rotation of the CA
  itself: a new `--ca-dir` with `--renew` issues everything from the new CA.
- **Revocation.** None (no CRL): a leaked client key is answered by renewing
  every certificate from a new CA, and rotating that service's NKey.
- **Rollback.** `--rollback` restores `COMPOSE_FILE` from
  `rollback/pre-production`, switches the EMR off first, recreates every
  service. Rehearsed.
- **EMR egress.** `EGRESS_EMR_INTERNAL_CIDRS` lists every Docker network on
  the host and the host's own addresses; egress-emr adds its own networks,
  gateways and the default bridge. A network created later is not on the
  list until `enable-emr.sh` is run again (it is idempotent).

### 5. Abuse-case tests

- `compose_production_test.go`: every plaintext client of compose.yml
  (outside the demo profile) is switched to `verify-full` and `tls://` in
  the overlay (a new plaintext service fails the test); each client has its
  own certificate folder; core is production; the pool is sized; pg_hba is
  exactly local trust, hostssl samenet scram, reject; core's EMR overlay
  never mounts emr's folders; the deploy ships and runs the steps, without
  `grep -q` on a pipe.
- `conf_test.go`: the deploy NATS config's emr user has exactly
  `services/nats/emr.conf`'s rights.
- emr and notifications config tests: outside development the three NATS TLS
  files are required, all or none, errors never name a path.
- Rehearsal (build server): plaintext NATS, TLS without a client certificate,
  a certificate from another CA, a certificate without an NKey, plaintext
  Postgres with the right password: all refused.

### 6. Residual risk and sign-off

- The deploy user on the live host is uid 1000, the NATS and pipeline images'
  uid too, so it can read those two keys (as it can already read the
  pipeline's NKey seed and `.env`). Accepted: the deploy user is the operator.
- No certificate revocation; one-year leaf certificates.
- The file secret store holds hospital credentials on the host disk (0400,
  0700 folders, not in the backups); losing the disk means re-entering them.
  Production needs OpenBao, which is now built: deploy/README.md, "OpenBao
  (the EMR secret store)", with its own threat model.
- JetStream has one replica (`CORE_ALLOW_SINGLE_NODE`): unchanged.
- Owner sign-off needed: where the CA key is kept, and the maintenance
  window for the switch (about 25 s of downtime for production mode, up to
  about 15 s for the EMR's first run).

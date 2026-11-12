# The emr service

The EMR integration service ([ADR 0014](../../docs/adr/0014-separate-emr-service-and-document-route.md),
[design](../../docs/specs/2026-11-07-emr-integration-design.md),
[plan](../../docs/plans/2026-11-07-emr-integration.md)). It is the only
process that holds hospital record-system credentials and the only route
out to hospital systems. Core holds neither.

**Off by default.** It runs only under the compose profile `emr`, with its
NATS user switched on, and core talks to it only when `CORE_EMR_ENABLED=true`:

```bash
NATS_EMR_CONF=emr.conf NATS_EMR_PASSWORD=<letters and digits> docker compose --profile emr up -d
```

Without `NATS_EMR_CONF` NATS includes `services/nats/emr-off.conf` (no
variable needed, nobody signs in as emr), so today's dev setups run as
before. **The demo has neither the emr NATS user nor the emr service yet:**
both come in a later PR, once ops provision `NATS_EMR_NKEY` on the host
(TODO.md, people/ops item); an undefined NKey variable would stop the demo's
NATS.

## What is built (PR 2: the skeleton)

- `emr serve`: the control responder on `emr.v1.ctl.>` (queue group `emr`),
  answering `emr.v1.ctl.health` as `svc:core`. Every request is checked
  before any handler runs: size, the request token (`aud: emr`, the
  subject table in `services/gokit/reqtoken`), the reply subject
  (`_INBOX_gateway.` only), the replay set, the op and its caller rule
  (`services/gokit/emrops`), the request id and a strict schema. A refusal
  gets no reply, one log line with its reason code, and a metric.
- `emr keys seal-init --out <dir>`: the X25519 sealing key pair core seals
  credentials to (design §6.2).

## What is built (PR 3: connections)

- `emr.v1.ctl.put_credentials` (platform admin; the token must carry
  core's `stp` claim, a checked step-up code): opens each sealed box
  (`services/gokit/seal`) against the connection's current credential
  origin, read under the token's hospital, and stores every field or none;
  the reply is each field and its set time.
- `emr.v1.ctl.wipe_credentials` (platform admin): every secret of the
  connection, then its metadata rows; idempotent.
- `emr.v1.ctl.test` (platform admin): `sign_in_refused` without calling
  the connector unless every required secret is set, and usable, for the
  origin the saved addresses give now (design §6.3, A28); otherwise the
  connector's read-only probe, 20 s.
- The file secret store (`EMR_SECRETS_DIR`): development and staging only,
  0400 files in 0700 directories owned by the service's user; the previous
  version is kept 24 hours. Production needs the managed store (plan Task
  12, waiting on H3). The database holds metadata only (`emr.credentials`).
- The connector interface and the development `stub` (registered only with
  `APP_ENV=development`); `Creds` never print, and `TestNoConnectorLogsCreds`
  fails any connector code that hands them to a logger.
- The service refuses to start without a sealing key.

## What is built (PR 4: egress)

- `egress-emr` (`cmd/egress-emr`, in the same image, entrypoint
  `/egress-emr`): the emr service's only way out (design §6.4). Each
  CONNECT carries `Proxy-Authorization: Basic base64(<connection
  id>:<hex HMAC-SHA256(K_proxy, "<id>|<5-minute window>")>)`, accepted for
  its window and the two neighbouring ones (407 otherwise), and reaches only
  that connection's `host:port` entries, exactly as written, port 443 or
  22 (403). Nothing is resolved before that, so a name off the list never
  reaches DNS. A listed name is resolved once, fully qualified (no
  resolv.conf search suffix); any refused address in the
  answer refuses the CONNECT (loopback, link-local and metadata, CGNAT,
  multicast, reserved and documentation ranges, NAT64, 6to4, Teredo, ULA,
  the internal networks and the host's own addresses, Docker's default
  bridge, and RFC 1918 outside the connection's declared ranges);
  otherwise it dials the address it checked. Tunnels pass a half-close
  through and end when both sides close, after 60 s idle or after 30
  minutes; one connection holds at most 32 of the 256 tunnel slots. One log
  line per CONNECT: connection id, decision code, status, port and a host
  hash. Outside development it refuses to start without
  `EGRESS_EMR_INTERNAL_CIDRS`.
- The allow-list (`internal/proxyconf`): `allow.json`, written by the emr
  service every 15 s (`internal/egress.Writer`) from enabled, unpaused
  connections (read as `mhai_emr_sched`) and those under test, each read
  under its own hospital; a connection is listed only while its addresses
  still give the origin its credentials were entered for. The proxy reads
  it every second and refuses everything when it is missing, malformed,
  over 120 s old or dated ahead. Connections whose ranges overlap another
  hospital's are left out (the rest are served) and logged at error level
  with `"code":"ranges_overlap","alert":true`, by the writer and the proxy.
- `internal/egress`: `HTTP` (proxy fixed, per-connection
  `Proxy-Authorization`, origin checked before anything is sent, no
  cross-origin redirect and none for a request with a credential, TLS 1.2+
  with system roots, 64 KiB headers, 20 MB bodies), `DecodeJSON` (depth
  64), `SSH` (through the proxy, host key pinned to the typed SHA256
  fingerprint, a deadline kept after the handshake: the caller's, at most
  30 minutes; PR 11 opens SFTP over it, and adds `github.com/pkg/sftp`
  then). `test` holds its connection on the allow-list only once every
  check has passed.
- `emr keys proxy-init --out <dir>`: K_proxy (`k_proxy`, 0400, 64 hex
  characters), shared by the emr service and the proxy only.
- `deploy/egress-emr/test-egress.sh` runs the real proxy on the real
  networks (see the script for what it checks).

## What is built (PR 5: canonical model, normaliser, patient index)

Migration 00023 (core) adds `emr.raw_records`, the canonical tables
(`patients`, `patient_links`, `units`, `encounters`, `observations`,
`medications`), `possible_matches`, `merges` and `unmapped_codes`, each
keyed by `(hospital_id, id)` under forced RLS, and core's definers
`emr_patient_in_hospital`, `emr_merge_patients` and `emr_exclude_patient`
(the emr role can't call them, and has no `DELETE`).

- `internal/sink`: `RawSink`. Keeps only the allow-listed fields of each kind
  (§14.1; the rest is dropped and counted), cleans display text
  (`CleanText`: NFC, no markup, control or format characters, capped) and
  neutralises spreadsheet formulas (`CSVCell`), stores each source version
  once and announces new ids on `emr.v1.<hid>.fetched` (500 a message).
- `internal/codes`: the code tables (`codes/testdata/`: synthetic until H6),
  the reviewed per-connection maps, units and conversions (`maps/`). By code
  only, never by display text; synthetic tables and unreviewed conversions
  load in development only (K10).
- `internal/normalise`: the one normaliser. Strict decimals (the
  connection's `decimal_separator`, comparators kept as text), FHIR times
  (a date is a date; no offset, no time; over 10 minutes ahead refused),
  versions (a new source version is a new row, the old one superseded;
  `entered-in-error` and `cancelled` mark the current row), unmapped codes
  and units kept unstandardised and counted. A record it can't read is
  rejected with a closed code; an excluded patient's records are dropped
  with their payload. Five failed deliveries dead-letter the batch (ids
  only). The sweep re-announces rows whose announcement was refused.
- `internal/index`: patients per hospital by `(connection, source id)`;
  never by name. Equal MRNs across connections that declare the same
  `mrn_namespace`, and a source's `replaced-by` within one connection, only
  make an `emr.possible_matches` row and an id-only signal: a hospital admin
  confirms each merge through core.
- `internal/signals`: `emr.v1.<hid>.signal`, ids and a closed kind.
- `emr unmapped --hospital <id> --connection <id>`: a connection's unmapped
  codes and counts (no patient data).

The normaliser runs only with `EMR_CODES_DIR` and `EMR_MAPS_DIR` set, and
only finds work once core has made the `EMR` stream (`CORE_EMR_ENABLED`).

## What is built (PR 6: FHIR R4)

Migration 00025 (core; 00024 is Analysis PR 4's) adds `emr.sync_state` (the
cursor, the due time, failure counts, the end of the backfill and the day of
the last reconciliation, per connection), `emr.raw_records.backfill`, the
emr role's read of `hospitals.time_zone`, and makes `emr_platform_health()`
report a connection that is on but has had no successful poll for three
intervals as `errors` (§15.4).

- `internal/connector/fhir`: SMART Backend Services sign-in (a client
  assertion signed RS384 for an RSA key or ES384 for a P-384 key, sent only
  to the credential origin; the token held in memory until a minute before
  it expires). `Test`: sign in, the capability statement (R4 only, every
  resource with `_lastUpdated` and `_sort`), a count. `Fetch`: Observation
  (laboratory, vital signs), MedicationRequest and MedicationStatement by
  `_lastUpdated`, 200 a page, then the patients, encounters and locations a
  page names, read from the same server by `_id`. At most 20 pages or 4,000
  records a run. The cursor, per resource type, is the last completed page's
  greatest `lastUpdated` minus 2 minutes, and never moves backward. A first
  sync reads back 30 days. Allow-listed structs only. A page is refused
  whole when it is the wrong shape, too big, out of order, dated ahead of
  our clock, or names a next link off the base address.
- `internal/scheduler`: every 10 s, the due connections (as
  `mhai_emr_sched`), one run each under a per-connection advisory lock.
  Each run is an `emr.sync_runs` row with a closed code. A 429's
  `Retry-After` is honoured (capped at 15 minutes), other failures double
  up to 15 minutes, and three sign-in refusals in a row stop polling until a
  test passes. The secret store being down is `unreachable`. A first sync's
  records are flagged `backfill` until a run has read everything. Once a
  day, from 02:00 hospital time, the last 24 hours are read again, and
  records only that finds are counted (`ReconciledLate`). `Stale` is the
  §15.4 rule.
- `emr.v1.ctl.sync_now` (svc:core): due at the next tick, unless the
  connection is backing off.
- `gokit/recordlink`: the "open record" link (https, the base address's
  host, the id path-escaped).
- `internal/synth` and `emr synth-fhir` (development only): synthetic
  patients and a FHIR server with SMART sign-in.

The scheduler runs only with the secret store, egress-emr and the
normaliser configured. It polls only connections that are switched on, and
none is on by default.

### Synthetic end to end

`bash services/emr/scripts/e2e-fhir.sh` (on the build server, with Docker
and Go; never in CI, never against a real server) starts Postgres, the real
egress-emr and `emr synth-fhir` on the proxy's outside network. It then runs
a test container on the emr service's networks only. The test saves a
synthetic connection, writes the allow-list, and checks five things:

- a direct request can't reach the server;
- the FHIR test passes through egress-emr;
- a connection that doesn't declare the server's range is refused;
- a sync puts records in the canonical tables and ends the backfill;
- no synthetic name, MRN or value is in any log or message.

`EGRESS_EMR_DEV_EXEMPT` lets the proxy reach the server's network, which it
shares. It is development only.

## Settings

| Variable | |
|---|---|
| `APP_ENV` | `development`, `staging` or `production` (unset: production) |
| `EMR_NATS_URL` | no credentials in it; `tls://` outside development |
| `EMR_NATS_NKEY_FILE` / `EMR_NATS_PASSWORD` | one of them; the password in development only |
| `EMR_DATABASE_URL` | the `emr_app` login (migration 00020, PR 3); `sslmode=verify-full` outside development |
| `EMR_TRUSTED_KEYS_PATH` | the request-token trusted-keys file core writes (public keys) |
| `EMR_SEAL_KEY_PATH` | the directory of the sealing private keys (`seal-<kid>.key`) |
| `EMR_SECRETS_DIR` | the file secret store (development and staging only); empty: no store, the credential ops answer nothing |
| `EMR_PROXY_URL`, `EMR_PROXY_KEY_PATH`, `EMR_EGRESS_DIR` | egress-emr (`http://host:port`), K_proxy and the allow-list directory: all or none; none means no way out |
| `EMR_CODES_DIR`, `EMR_MAPS_DIR` | the code tables and the maps (the image has `/codes/testdata` and `/maps`): both or none; none means no normaliser |
| `EMR_RULE_SET_ACTIVE` | later PRs; empty by default |
| `EMR_DEV_CA_FILE` | development only: an extra CA for the synthetic FHIR server's certificate; refused elsewhere |

egress-emr's own settings:

| Variable | |
|---|---|
| `EGRESS_EMR_ALLOW_LIST` | the allow-list file (read-only mount) |
| `EGRESS_EMR_KEY_PATH` | K_proxy |
| `EGRESS_EMR_ENV` | `development`, `staging` or `production` (unset: production) |
| `EGRESS_EMR_INTERNAL_CIDRS` | never reached, comma-separated: the `backend` subnet and the host's own addresses (`/32`); required outside development. The proxy adds its own networks, its routing table's gateways and `172.17.0.0/16` |
| `EGRESS_EMR_LISTEN` | default `:3128` |
| `EGRESS_EMR_DEV_EXEMPT` | development only: one private network of at most a `/24` taken out of the proxy's own networks (the synthetic server's, for the end-to-end run); never a gateway or an internal network |

Errors name the variable, never its value.

## Rules

- The module imports nothing from `services/core/`
  (`internal/imports_test.go`).
- Ids, closed codes and counts in messages and logs; never a name, an
  MRN, an address, a value or a credential.

## Tests

Run remotely, never on a laptop (see the plan's "Running the tests"):

```bash
cd services/emr
go vet ./... && go test -race ./...
go test -race -tags integration ./...   # Postgres with core's migrations (internal/testpg)
```

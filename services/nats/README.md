# nats

Configuration for the NATS server, the only link between the core service
(`services/core/`, Go), the frontend's BFFs and the Python workers
(`services/intelligence/`). Design: `docs/specs/2026-10-28-intelligence-boundary-design.md`
§5, §7.6, §9.2 and §17.4.

| File | Used by |
|---|---|
| `nats.conf` | dev compose (mounted read-only at `/etc/nats/nats.conf`) and every integration test (`services/core/internal/testutil/nats.go` copies it into the test container), so the tests exercise the real permissions |
| `bff-clinical.test.conf` | included by `nats.conf`: the whole `bff_clinical` user, with the test-only `bff_direct` subject `inference.v1.echo.ping` |

The production-shaped config is [`deploy/nats/nats.conf`](../../deploy/nats/nats.conf)
(the demo server): NKeys only, and it never includes a `*.test.conf` file.
A production cluster config starts from it. `services/core/internal/bus/conf_test.go`
checks that the test-only subject is only in the include, that the
`gateway` permissions are the same in both files, and that the demo's
clinical BFF user (`NATS_BFF_NKEY`) has the include's permissions minus the
test-only subjects. The demo has no worker users (the workers aren't
deployed there in Plan 3) and no `bff_platform` yet.

## Users

Each service has its own NATS user and may do only what its job needs.
Everything not listed is refused (no `no_auth_user`, so an anonymous
connection is refused too).

| User | Service | Publish | Subscribe | Replies |
|---|---|---|---|---|
| `gateway` | the core service | `inference.v1.>`, `jobs.v1.>`, `$KV.<bucket>.>` for the four buckets, and JetStream management on the four KV streams, `JOBS` and `DEAD` only (create, update, info; KV reads; consumer create and info); fetch and ack on its `core_dead` consumer. No stream delete or purge, no message delete, no other stream | `_INBOX_gateway.>`; `api.v1.*` and `authz.v1.grant.>` in queue group `core` only | `allow_responses` |
| `pipeline` | the pipeline worker | `dead.v1.>`, `authz.v1.grant.pipeline`, and its own consumer: `$JS.API.CONSUMER.INFO.JOBS.pipeline`, `$JS.API.CONSUMER.MSG.NEXT.JOBS.pipeline`, `$JS.ACK.JOBS.pipeline.>` | `inference.v1.pipeline.>` in queue group `pipeline` only; `_INBOX_worker.pipeline.>` | `allow_responses` |
| `echo` | the echo worker (dev and test) | `authz.v1.grant.echo` | `inference.v1.echo.>` in queue group `echo` only; `_INBOX_worker.echo.>` | `allow_responses` |
| `bff_clinical` | the clinical app's BFF | `api.v1.clinical`; the `bff_direct` operations one by one (`inference.v1.echo.ping` in dev and test only) | `_INBOX_bff.clinical.>` | none |
| `bff_platform` | the platform app's BFF | `api.v1.platform` | `_INBOX_bff.platform.>` | none |

So: only `gateway` publishes `inference.v1.>` (the clinical BFF aside, for
its `bff_direct` subjects) and `jobs.v1.>`. No worker has KV access, any
`$JS` subject beyond its own consumer, `api.v1.>` or another service's
grant subject. No BFF user has `jobs.v1.>`, `$KV.>`, `$JS.>`, `dead.v1.>`
or `authz.v1.>`. Every rule has a refusal test in
`services/core/internal/bus/bus_integration_test.go`.

`allow_responses: {max: 1, expires: "1m"}` gives a responder one publish,
within a minute, for each request it receives. That publish goes to the
request's reply subject, which **the requester chooses**. It is not
limited to an inbox (see below), so it is a capability handed to whoever
can send the responder a request, not a safety property.

The core service's user is still named `gateway`. The name predates the
rename to `core` and is kept on purpose, so the security config, the
credentials and the tests stay as they are.

### Responders must check the reply subject

NATS does not check the reply subject a requester sets. A responder's one
reply is **one arbitrary publish** with the responder's identity, past its
allow list and its deny list (`TestReplySubjectIsTheResponderToCheck`
shows the reply reaching another app's inbox, landing on `JOBS` and
writing a KV entry; a throwaway probe showed a deny list doesn't stop it).
For core that includes a JetStream API call made with core's account.

The test demonstrates these:

- `jobs.v1.<hospital>.run_query` queues a job the requester may not
  publish;
- `$KV.denylist.<key>` writes a KV entry;
- `_INBOX_bff.platform.<x>` sends core's answer to the other app.

The same mechanism, not exercised by a test because it would destroy the
shared streams, also reaches the JetStream API. For example:

- `$JS.API.STREAM.DELETE.KV_denylist` would delete the denylist, so
  revoked tokens would work again until they expired;
- `$JS.API.STREAM.PURGE.JOBS` would drop every queued job.

It is reachable by anyone who can send a responder a request:

- a stolen BFF credential, through `api.v1.<app>` to core;
- a compromised worker, through `authz.v1.grant.<service>` to core;
- a stolen clinical BFF credential, through a `bff_direct` subject to a
  worker.

So every responder refuses, with no reply, a reply subject that doesn't
start with exactly the prefix it expects, and whose remaining tokens aren't
literal. A literal token has no `*`, `>`, whitespace or empty token.

| Responder | Expected prefix |
|---|---|
| core on `api.v1.<app>` | `_INBOX_bff.<app>.` (app taken from the subject) |
| core on `authz.v1.grant.<service>` | `_INBOX_worker.<service>.` (service from the subject) |
| a worker on a BFF-direct call | `_INBOX_bff.clinical.` (spec §17.2) |
| a worker on a call from core | `_INBOX_gateway.` |

The only server-side defence is to split NATS accounts: the BFFs and the
workers each in their own account, reaching core's subjects through
service exports and imports, so a reply can't leave the exported service's
response path. Spec §7.6 lists it as "later, optional". `TODO.md` asks for
it before production.

## Inbox prefixes

Each user receives replies on its own prefix instead of the shared
`_INBOX.>`: `_INBOX_gateway`, `_INBOX_worker.<service>` and
`_INBOX_bff.<app>` (set with `CustomInboxPrefix`; `bus.WorkerInbox` and
`bus.BFFInbox` in Go). A user may subscribe only to its own prefix, so no
service can read another's replies, and nobody but a responder can write
to them.

## Queue groups

Core subscribes `api.v1.*` and `authz.v1.grant.>` in queue group `core`;
each worker subscribes `inference.v1.<service>.>` in a queue group named
after the service. The permissions allow those subscriptions only in that
queue group, so a stray plain subscriber can't take a copy of every
request. (Core's `api.v1.*` doesn't stop it subscribing `api.v1.>`, because
NATS 2.10 matches a `>` subscription against a `*` permission. What bounds
the subjects core sees is that the BFFs can publish only `api.v1.<app>`.) Jobs go to one durable pull consumer per service, shared by its
copies.

## Streams

Core creates them at start-up (`bus.EnsureStreams`, run by `core serve`),
with `NATS_JS_REPLICAS` replicas (1 in dev, 3 on the production cluster).

| Stream | Subjects | Settings | Consumers |
|---|---|---|---|
| `JOBS` | `jobs.v1.>` | file storage; work-queue retention (an acked job is removed); `max_age` 1 hour (no job lives past its 10-minute deadline); duplicate window 2 minutes (`Nats-Msg-Id` = the query id); `discard: new` per subject with at most 200 waiting jobs per subject (hospital and type), so a flood is refused at publish and nothing queued is dropped; messages at most 72 KiB (64 KiB body plus headers, the request token among them); at most 256 MiB in all. One subject's worst case (200 × 72 KiB ≈ 14 MiB) is at most a sixteenth of the stream, so one hospital can't fill it for the others. The main fairness control is core's cap of 10 questions in flight per hospital (Plan 3 Task 5); this cap is the backstop | `pipeline` on `jobs.v1.*.run_query`: explicit ack, `ack_wait` 60 s, `max_deliver` 5, no `BackOff` (the worker picks each retry's delay with `Nak(delay)`), `max_ack_pending` from `JOBS_PIPELINE_MAX_ACK_PENDING` (default 8) |
| `DEAD` | `dead.v1.>` | file storage; `max_age` 30 days; messages at most 4 KiB (ids, type, attempts, code); at most 10,000 per subject and 64 MiB in all, **discarding old**. Past either limit the oldest letters are dropped and new ones are kept, so a flood (a compromised worker may publish `dead.v1.>` for any hospital id) can push real letters out early but can't lock them out. DEAD is a notice: the job row in Postgres keeps the failure either way | `core_dead`: explicit ack, `ack_wait` 30 s |

A work-queue stream allows only one consumer per subject, so each job type
has exactly one durable consumer.

## Limits and storage

- `max_payload: 1MB`, the outer bound (the services check their own,
  smaller limits).
- `max_file_store: 1GB` (the demo: 512 MB). `JOBS` and `DEAD` reserve
  their 320 MiB in it. Their byte caps keep a flood of jobs or dead letters
  inside those streams, so it can't fill the store the KV buckets (the
  denylist among them) need. Within its stream a flood still costs other
  hospitals: a JOBS flood across many hospital subjects can fill JOBS, and
  a DEAD flood evicts old letters.
- JetStream storage is encrypted at rest (`cipher: chachapoly`), with the
  key from `NATS_JS_KEY`. Losing the key loses the store.

## Development credentials

In development every user has a password, read from the environment and
never from this directory or the repository:

- `NATS_GATEWAY_PASSWORD`, `NATS_PIPELINE_PASSWORD`, `NATS_ECHO_PASSWORD`,
  `NATS_BFF_CLINICAL_PASSWORD`, `NATS_BFF_PLATFORM_PASSWORD`
- `NATS_JS_KEY`, the JetStream encryption key (at least 32 characters)

Use letters and digits only, starting with a letter: the config parser
reads other characters as syntax and a leading digit as a number. An
undefined variable stops the server. Compose refuses to start NATS with a
missing or placeholder value from `.env.example`, and the core service
refuses a placeholder in its own `NATS_URL`. Each service gets only its own
password; only the `nats` service sees them all. In dev compose, core's
`NATS_URL` carries the gateway's, the `pipeline` and `echo` workers' their
own; the BFF users' passwords go to mhai-web's BFF, never to core or a
worker (`services/core/cmd/core/compose_test.go`). The smoke run
(`.github/scripts/boundary-smoke.sh`) generates all of them for the run.

## Production

- **NKeys, not passwords:** replace each user's `password:` with
  `nkey: <public key>`. Generate a pair per service with
  `nk -gen user -pubout`, put the public key in the config, and give the
  service only its seed file, through a secret mount (core:
  `NATS_NKEY_SEED_PATH`).
- **TLS with `verify`** on every connection: enable the `tls {}` block,
  with the CA that signs every service's client certificate, and
  `handshake_first: 300ms`: the workers start TLS before INFO outside
  development; INFO-first clients (core, the BFFs) get INFO after 300 ms and
  then upgrade to TLS, so the fallback stays TLS-only (never plaintext, and
  `verify` still applies). Core reads
  `NATS_TLS_CA_PATH`, `NATS_TLS_CERT_PATH` and `NATS_TLS_KEY_PATH` and
  refuses to start outside development without them.
- **R3:** a three-server cluster, and `NATS_JS_REPLICAS=3` for core.
- **No test include:** define `bff_clinical` in the production config
  itself, with only the production `bff_direct` subjects (none in this
  build), and never include `bff-clinical.test.conf`.
- Keep the monitoring port (8222) off the network.

# The intelligence boundary: core to Python over NATS (Plan 3): implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task by task. Steps use checkbox (`- [ ]`) syntax for tracking. Read the `secure-design` and `secure-build` skills (`.claude/skills/`) before Task 1, and run `security-review` on your own diff before each PR.

**Goal:** Join the core service and the Python `intelligence` package over NATS. Core signs a short-lived request token for every call and every job. Intelligence verifies it before doing anything, reads hospital data only under row-level security with the hospital from that token, and hands its audit facts back to Go. A clinical question then runs end to end: BFF → core → NATS → intelligence → Postgres, and the result comes back through core.

**Architecture:** Core (Go, `services/core/`) mints request tokens with a dedicated Ed25519 key (`internal/reqtoken`), routes calls through one client (`internal/intel`) and owns the operations table next to its route table (`internal/authz`). The Python side gains one package, `intelligence/boundary/`: the verifier, the NATS worker runtime, database access through `with_hospital`, and the `run_query` job. Two Python services run from it: `pipeline` (the only one with a database login) and `echo` (tests and the smoke test only). Messages are Protocol Buffers in `proto/`, generated for both languages and checked by `buf`.

**Tech stack:**
- Go 1.24, as Plan 2: `net/http`, `pgx/v5`, `goose/v3`, `nats.go` (core, JetStream, KV), `testcontainers-go`
- New in Go: `google.golang.org/protobuf`; `go.opentelemetry.io/otel` with the OTLP gRPC exporter
- Python 3.12 with uv (ADR 0002). New: `nats-py`, `psycopg[binary]` 3 and `psycopg-pool`, `protobuf`, `opentelemetry-sdk` and `opentelemetry-exporter-otlp`; dev: `mypy-protobuf`, `grpcio-tools` (for its bundled `protoc`), `testcontainers`, `pytest-asyncio`
- `buf` for lint and breaking checks; `protoc-gen-go` for Go code. Exact versions are pinned in the Makefile and `uv.lock` (at the time of writing: `buf` v1.47.2, `protoc-gen-go` v1.36.5)

**Spec:** `docs/specs/2026-10-28-intelligence-boundary-design.md` (approved 2026-10-28, all decisions in its §16 taken as recommended, the §14 refinements accepted). It details the core design (`docs/specs/2026-10-26-go-gateway-nats-and-auth-design.md`) §4.3, §5, §7.4–§7.7 and step 4 of its §10, and ADR 0010.

**The contract:** `docs/specs/2026-10-17-frontend-requirements.md` §4, §7, §8 and §14.8. The query shapes come from mhai-web's `apps/clinical/lib/api/contract.ts` (`QueryCreateIn`, `QueryStatusOut`, `ClinicalResultOut`), which the contract's "Gateway" rows point to (see "Where this plan departs from the spec", item 8).

## Global Constraints

Plan 2's constraints still hold (problem bodies, 404 for another hospital's resource, identity only from the verified token, no PHI or secrets in logs, synthetic data only, plain commit messages with no AI attribution). In addition:

- **Nothing before the signature.** A worker parses no body and opens no database connection until checks 1–6 of spec §7.3 pass. Any failure does no work.
- **The hospital comes only from the verified token.** On the Python side it reaches Postgres only through `with_hospital(hid)`. No function takes a hospital id from a message body, a subject or a header.
- **Python writes no permission rule.** It reads the generated `operations.json` and nothing else.
- **Only Go writes the audit chain.** Workers put facts in `ResponseMeta.audit_facts` (request/reply) or `intelligence_audit_outbox` (jobs). Core takes the actor, hospital and request id from what it minted or stored, never from a fact.
- **Refusals look the same to the user.** `unauthorized`, `forbidden` and `invalid_request` from a worker all become 500 `internal_error`. The reason is in logs and metrics only.
- **Error codes from intelligence** are the closed set `unauthorized`, `forbidden`, `invalid_request`, `not_found`, `deadline_exceeded`, `overloaded`, `unavailable`, `cancelled`, `internal`. An `Error` message holds a code and `retryable` only.
- **No PHI in messages where an id will do.** Jobs carry `query_id`. Dead letters carry ids, the type, attempts and the code.
- **Sizes:** request token ≤ 4 KB; request/reply body ≤ 256 KB; job body ≤ 64 KB; NATS `max_payload` 1 MB.
- **Lifetimes:** `req` tokens at most 60 s (default 5 s, per operation); `job` tokens at most 24 h; `run_query` 10 minutes. Clock skew allowance 5 s.
- **New core error code:** 503 `unavailable` (title "Unavailable", detail "Couldn't get an answer. Try again."). `not_ready` stays for readiness.
- **Remote runs only.** No build, test, container or compose run on the laptop. See "Running the tests".

## Review Focus

1. **The check order in the Python verifier** (spec §7.3). Nothing touches the body before the signature passes; a refused request whose reply subject isn't `_INBOX_gateway.` gets no reply at all. Tested in Task 6.
2. **The hospital on the Python side.** `with_hospital` takes a `Verified` object, not a string, so a caller can't pass a hospital from anywhere else. Tested in Task 8.
3. **Job claim and completion under concurrency.** Two workers racing on one message, a redelivery after completion, and a crash between commit and ack all end with exactly one `clinical_decisions` row and one `query.completed`. Tested in Task 9.
4. **The drainer's identity rule.** A fact that names another actor or hospital is recorded with the job's own actor and hospital. Tested in Task 10.
5. **NATS permissions per service.** The `pipeline` user can't publish requests or jobs, can't read core's inbox and can't read `echo`'s subjects. Tested in Task 4.

## Where this plan departs from the spec

Things the spec says that don't fit the code as it is. Each is small; the plan does the thing on the right.

1. **`clinical_queries` has none of the job columns** the spec's claim uses (`attempts`, `lease_until`, `published_at`), and no `request_id` for the drainer. Task 5 adds them, with a status `CHECK`.
2. **The worker login can't be a direct member of `mhai_worker`** if it is to hold nothing: `mhai_worker` holds the grants itself, and `db.EnsureLogin` accepts only `mhai_app` and `mhai_provision`. The plan mirrors core's own pattern: a new `NOINHERIT` wrapper role `mhai_worker_app` is granted `mhai_worker WITH INHERIT FALSE`, and `intelligence_app` is a member of `mhai_worker_app` only. `EnsureLogin` learns that role (Task 8).
3. **Core's background loops need cross-hospital roles the spec doesn't name.** The publish sweeper and the dead-letter consumer read and update job rows across hospitals; `mhai_gateway` can't. Task 5 adds `mhai_jobs` (column-level `SELECT`/`UPDATE` on `clinical_queries`, nothing else). The drainer (`mhai_audit_drain`) also needs `SELECT (id, hospital_id, created_by, request_id)` on `clinical_queries`, `EXECUTE` on `audit_chain_head()` and the `audit_log` sequence, beyond the spec's "nothing else" (Task 8).
4. **A JetStream consumer `BackOff` would break the 20 s `+WIP` rule.** When `BackOff` is set, its first value (10 s) replaces `ack_wait` as the redelivery timer, so a running job would be redelivered before its first `+WIP`. The plan keeps `ack_wait` 60 s with no consumer `BackOff`, and the worker applies the 10 s, 30 s, 2 min, 5 min schedule through `Nak(delay)` (Task 4, Task 9).
5. **A worker can't mark a refused job `failed`.** If the token fails, the worker has no verified hospital, so it must not open `with_hospital` at all. It terminates the message and publishes the dead letter; core's dead-letter consumer marks the row `failed` (Task 9, Task 10). Jobs that pass verification but fail later are marked by the worker, as the spec says.
6. **Dead letters need a stream.** `dead.v1.>` is plain pub/sub today, so a dead letter is lost if core is down. Task 4 adds a `DEAD` stream (30 days) and core reads it with a durable consumer.
7. **There is no patient id to put in `pids`.** `clinical_queries` stores `patient_data_json` inline, the contract's `QueryCreateIn` has no patient id, and there is no patients table yet (EMR work). This plan mints tokens with no `pids` and the claim reads no patient id, so retrieval returns shared guidelines only (`chunk_visible_to` with no valid patient). The claim, the check and the column come with the EMR plan.
8. **The frontend contract doesn't spell out the query shapes.** It refers to the old gateway design; mhai-web's `contract.ts` has them. That file says `processing`, not `running`, and has no `cancelled`: core maps `running` → `processing`, and `cancelled` → `failed` with `error_code: "cancelled"`. The result needs `verified`, `degraded`, `failed_agents` and `model_versions`, which `clinical_decisions` lacks; Task 5 adds the columns.
9. **Retrieval and generation are one call** (`RAGClinicalContextAgent.generate_with_rag`), so there's no point between them to check for cancellation. The worker checks after masking, after generation (with retrieval), after verification, and before writing.
10. **The knowledge base is in memory today.** Python has no database code at all; it builds `KBChunk`s from files. Task 9 adds a small repository that loads `kb_chunks` under RLS for each job and maps a shared row (`hospital_id IS NULL`, no patient) to `source_patient_id = REFERENCE_SOURCE`, which is what `chunk_visible_to` expects. Indexing per job is fine for synthetic data; a cached index is performance-plan work.
11. **"Deactivating the user" has no path in core yet.** No staff endpoint exists. Task 10 adds triggers on `users.is_active` and `hospitals.status`, so every path (the CLI today, staff endpoints later, a manual fix) cancels that user's or hospital's queued and running jobs.
12. **`SecurePipelineWrapper`'s tests are the patient-scoping and feedback-review regression tests.** "Existing tests still pass" (spec Task 11) means rewriting those two files against the orchestrator and the boundary, not deleting them.
13. **Generating Python code.** `buf generate` can write Go with a local plugin, but its Python output needs `protoc`. The plan runs `protoc` from `grpcio-tools` (pinned in `uv.lock`) with `mypy-protobuf`, behind one `make proto` target, so CI and the generate-diff check use the same versions.
14. **`permissions.json`.** Plan 2 wrote it for "the Python side's checks in a later plan". Those checks are `operations.json` now, so Task 3 stops generating `services/intelligence/intelligence/security/permissions.json` and removes it.

---

## Running the tests

Never on this machine. Go tests, testcontainers, Docker builds and compose all run on the steven-reid build server (`steven-reid@75.46.59.136`), with mt5-lab as the fallback (`REMOTE_HOST`/`REMOTE_KEY`; the runner then caps the job at 2 CPUs and runs one job at a time). GitHub Actions runs the full gate as confirmation.

**Go** uses `/home/ose/Documents/Pehtrah/MH/mhai-platform/.remote-run-go.sh`. It rsyncs the checkout (without `.venv`, `.superpowers`, `__pycache__` or `data`) to `~/claude-work/<DEST>/` and runs the commands from stdin in `/work` inside `golang:1.24`, with the host's Docker socket for testcontainers and persistent module and build caches. Put each step on its own line ending `|| exit 1`, and wrap the run in `timeout`:

```bash
SRC=/home/ose/Documents/Pehtrah/MH/mhai-platform/mhai-multiagent-boundary/ DEST=mhai-plan3 \
  timeout 1800 /home/ose/Documents/Pehtrah/MH/mhai-platform/.remote-run-go.sh <<'CMDS'
cd services/core || exit 1
go vet ./... || exit 1
go test -race ./internal/reqtoken/... || exit 1
go test -race -tags integration ./internal/bus/... || exit 1
CMDS
```

In the task steps below, "**Run (Go, remote):** `<cmd>`" means put `<cmd>` in that heredoc after `cd services/core`.

**Python** also runs remotely, with uv, through the same script (the container has `curl`; uv installs Python 3.12 itself). uv's caches go in the persistent module-cache volume so torch isn't downloaded every run:

```bash
SRC=/home/ose/Documents/Pehtrah/MH/mhai-platform/mhai-multiagent-boundary/ DEST=mhai-plan3 \
  timeout 2400 /home/ose/Documents/Pehtrah/MH/mhai-platform/.remote-run-go.sh <<'CMDS'
export UV_CACHE_DIR=/go/pkg/mod/cache/uv UV_PYTHON_INSTALL_DIR=/go/pkg/mod/cache/uv-python
curl -LsSf https://astral.sh/uv/0.12.23/install.sh | sh || exit 1
export PATH=/root/.local/bin:$PATH
docker build -t mhai-core:test services/core || exit 1
cd services/intelligence || exit 1
uv sync --locked || exit 1
uv run pytest -q tests/boundary || exit 1
CMDS
```

"**Run (Python, remote):** `<cmd>`" means put `<cmd>` in that heredoc after `uv sync --locked`. Tests marked `boundary` start Postgres 16 and NATS 2.10 with testcontainers (Python) and apply core's migrations by running `mhai-core:test migrate up` (the `docker build` line), so the schema is the one production uses. They never load models.

---

## File structure

```
proto/
  buf.yaml, buf.gen.yaml
  mhai/intelligence/v1/envelope.proto    Error, ResponseMeta, AuditFact, DeadLetter
  mhai/intelligence/v1/pipeline.proto    RunQueryJob, HealthRequest, HealthResponse
  mhai/intelligence/v1/echo.proto        PingRequest, PingResponse (tests and smoke only)
testdata/
  request-tokens/*.json                  shared token vectors (written by Go, read by both)
  proto-golden/*.bin, *.json             golden messages (each side writes some, both decode all)
services/core/
  cmd/core/keys.go                       + --kind request, --trusted-out, untrust
  cmd/permgen/main.go                    + operations.json and the operations section
  internal/intelpb/                      generated Go (committed)
  internal/reqtoken/                     mint, Go verifier (tests and vectors), trusted-keys file
  internal/authz/operations.go           the operations table; Rule.Calls
  internal/intel/                        client, error map, job publisher, sweeper, in-flight cap,
                                         drainer, dead-letter consumer
  internal/queries/                      POST /queries, GET /queries/{id}, GET /queries/{id}/result
  internal/telemetry/                    OpenTelemetry setup, NATS header carrier, attribute allow-list
  internal/db/migrations/00007_jobs.sql
  internal/db/migrations/00008_intelligence_worker.sql
  internal/db/migrations/00009_cancel_on_deactivate.sql
services/nats/nats.conf                  per-service users, narrowed $JS, max_payload, JetStream cipher
services/intelligence/
  intelligence/boundary/
    __init__.py
    pb/                                  generated Python and .pyi stubs (committed)
    operations.json                      generated by permgen (committed)
    config.py                            env settings; placeholder refusal
    keys.py                              trusted-keys file, SIGHUP reload
    replay.py                            the per-process jti set
    operations.py                        loads operations.json
    verify.py                            the ordered checks
    envelope.py                          headers, reply and Error building, error codes
    worker.py                            nats-py runtime: queue groups, dispatch, limits, drain
    db.py                                with_hospital, the pool, the login check
    kb_repo.py                           kb_chunks → KBChunk under RLS
    models.py                            ModelClient protocol, OrchestratorModelClient, StubModelClient
    services/echo.py, services/pipeline.py
    jobs/run_query.py
    telemetry.py
  tests/boundary/                        unit tests, and integration tests marked `boundary`
```

---

### Task 1: The contract: `proto/`, generated code and CI checks

**Files:**
- Create: `proto/buf.yaml`, `proto/buf.gen.yaml`, `proto/mhai/intelligence/v1/{envelope,pipeline,echo}.proto`
- Create (generated, committed): `services/core/internal/intelpb/*.pb.go`; `services/intelligence/intelligence/boundary/pb/{__init__.py,*_pb2.py,*_pb2.pyi}`
- Create: `services/core/internal/intelpb/golden_test.go`; `services/intelligence/tests/boundary/{__init__.py,test_golden_messages.py}`; `testdata/proto-golden/`
- Modify: `Makefile` (`proto`, `proto-check`), `.github/workflows/ci.yml` (`checks` gains `buf lint`, `buf breaking`, generate-diff), `services/core/go.mod` (`google.golang.org/protobuf`), `services/intelligence/pyproject.toml` (`protobuf`; dev `grpcio-tools`, `mypy-protobuf`), `uv.lock`

**Interfaces:**
- Package `mhai.intelligence.v1`; Go package `github.com/Measured-Health/mhai-multiagent/services/core/internal/intelpb`.
- `envelope.proto`:
  - `Error {string code = 1; bool retryable = 2;}`
  - `AuditFact {string action = 1; string entity = 2; string entity_id = 3; google.protobuf.Struct payload = 4;}`
  - `ResponseMeta {repeated AuditFact audit_facts = 1; map<string,string> model_versions = 2; map<string,double> timings_ms = 3;}`
  - `DeadLetter {string job_id = 1; string type = 2; string subject_hospital = 3; uint32 attempts = 4; string code = 5; string reason = 6; string kid = 7;}` (`subject_hospital` is unverified by definition: it is copied from the subject).
- `pipeline.proto`: `RunQueryJob {string query_id = 1;}`, `HealthRequest {}`, `HealthResponse {ResponseMeta meta = 1; bool models_ready = 2; bool database_ready = 3;}`.
- `echo.proto`: `PingRequest {string text = 1; string hospital_id = 2;}`, `PingResponse {ResponseMeta meta = 1; string text = 2;}`. `hospital_id` exists so the scope check (A8) has a body hospital to compare.
- Schema names, used in `Mhai-Schema`: the full message names, e.g. `mhai.intelligence.v1.RunQueryJob`.
- `make proto` runs `buf generate` (Go) and `uv run python -m grpc_tools.protoc --python_out --mypy_out` (Python). `make proto-check` runs `buf lint`, `buf breaking --against '.git#branch=main,subdir=proto'` and `make proto` followed by `git diff --exit-code`.

- [ ] **Step 1: Write the failing tests.**
  - Go `TestGoldenMessagesRoundTrip`: encodes one of each message with fixed synthetic values to `testdata/proto-golden/<Name>.go.bin` (with `-update`), and decodes every `*.bin` there, Python-written ones included, checking it equals the expected values in `<Name>.json`.
  - Python `test_golden_messages_decode`: the reverse; writes `<Name>.py.bin` when `MHAI_UPDATE_GOLDEN=1`.
  - Python `test_error_has_no_text_fields`: the `Error` descriptor has exactly the fields `code` and `retryable` (it can't grow a free-text field silently).
- [ ] **Step 2: Run (Go, remote):** `go test ./internal/intelpb/...` → fails to compile. **Run (Python, remote):** `uv run pytest -q tests/boundary/test_golden_messages.py` → fails.
- [ ] **Step 3:** Write the `.proto` files, `buf.yaml` (lint `STANDARD`, breaking `FILE`) and `buf.gen.yaml` (local `protoc-gen-go`, `paths=source_relative`). Run `make proto` remotely and commit the generated code. Write the golden files with the update flags.
- [ ] **Step 4:** Run both again; they pass. **Run (remote):** `make proto-check` → clean.
- [ ] **Step 5:** Add the CI steps to the `checks` job (install `buf` at the pinned version; `fetch-depth: 0` is already set for `buf breaking`).
- [ ] **Step 6:** Commit: "Add the Protobuf contract for the intelligence boundary, generated for Go and Python".

**Security checklist**
- [ ] No message carries a hospital or user id that a worker would trust; `PingRequest.hospital_id` exists only to be checked against the token.
- [ ] `Error` has no free-text field; `DeadLetter` has ids, a type, counts and codes only.
- [ ] Generated code is committed and diff-checked, so nobody hand-edits it.
- [ ] Golden values are synthetic.

### Task 2: Request tokens in core

**Files:**
- Create: `services/core/internal/reqtoken/{reqtoken.go,trusted.go}`, `reqtoken_test.go`, `vectors_test.go`, `trusted_test.go`
- Create: `testdata/request-tokens/*.json` (written by `vectors_test.go -update`)
- Modify: `services/core/cmd/core/keys.go` and `keys_test.go` (new file), `services/core/internal/config/config.go` (`RequestKeyPath`; `ValidateServe` requires `REQUEST_KEY_PATH`), `config_test.go`

**Interfaces:**
- Consts: `Type = "mhai-req+jwt"`, `Issuer = token.IssuerName` (`mhai-gateway`), `SvcCore = "svc:core"`, `MaxReqTTL = 60 * time.Second`, `MaxJobTTL = 24 * time.Hour`, `Skew = 5 * time.Second`, `MaxTokenLen = 4096`.
- `type Kind string` with `KindReq = "req"`, `KindJob = "job"`.
- `Claims{Aud, Sub, Hid string; Roles []string; Rid, Jti string; Kind Kind; Subject string; BodyHash string; PatientIDs []string; IssuedAt, ExpiresAt time.Time}`
- `Signer{Key ed25519.PrivateKey; KeyID string; Now func() time.Time}`
- `(s Signer) Mint(c Claims, body []byte, ttl time.Duration) (string, Claims, error)`: sets `jti` (128 random bits, base64url), `iat`, `exp = iat + ttl`, `bh = b64url(sha256(body))`; refuses a `ttl` over the cap for `Kind`, an empty `Aud`, `Sub`, `Rid` or `Subject`, and an empty `Hid` on a subject that isn't `jobs.v1.global.*`. `pids` is omitted when empty.
- `LoadSigner(path string) (Signer, error)`: the same PEM parsing and key id rule as `token.LoadIssuer` (reuse it).
- `VerifyAt(tok string, keys map[string]ed25519.PublicKey, want Expect, body []byte, now time.Time) (Claims, error)` with `Expect{Aud, Subject string; Kind Kind}`. The Go twin of the Python verifier's checks 1–4, for tests, the vectors and core's own fake worker. Errors are fixed sentinels (`ErrMalformed`, `ErrSignature`, `ErrClaims`, `ErrExpired`, `ErrBinding`) and never hold the token.
- Trusted-keys file: `{"keys":{"<kid>":"<base64url of the 32-byte public key>"}}`. `ReadTrusted(path) (map[string]ed25519.PublicKey, error)` refuses a missing, empty or over-two-key file and a key that isn't 32 bytes. `WriteTrusted(path, keys)` writes atomically (temp file and rename), mode 0644 (public).
- CLI (`keys.go`):
  - `core keys new|ensure --out PATH [--kind access|request] [--trusted-out PATH]`. `--kind` defaults to `access`, so the compose one-shot's existing command keeps working. With `--kind request`, `--trusted-out` is required and the key's public half is added to that file; adding a third kid is refused.
  - `core keys untrust --kid KID --trusted-out PATH`: removes one kid (rotation step 4, emergency rotation). Refuses to leave the file empty.

- [ ] **Step 1: Write the failing tests.**
  - `TestMintVerifyRoundTrip`: every claim survives, `typ` is `mhai-req+jwt`, `exp − iat` equals the ttl.
  - `TestAccessTokenRefusedAsRequestToken` (A6): a `token.Issuer` access token, even signed with a key in the trusted set, fails `VerifyAt` with `ErrSignature` (wrong `typ`).
  - `TestRequestTokenRefusedAsAccessToken` (A6, reverse): a request token fails `token.VerifyAt`.
  - `TestBodyHashBinding` (A7): flipping one byte of the body gives `ErrBinding`; so does a different subject.
  - `TestRequestTokenRejectsAlgAndKidAndLifetime` (A10): `alg` `none`, `alg` `HS256` (with an HMAC signature over the same input), an unknown `kid`, `exp` 25 h after `iat` on a `job`, `exp` 61 s after `iat` on a `req`, and `iat` 6 s in the future are all refused.
  - `TestMintRefusesOverCap` and `TestMintRefusesMissingHospital`.
  - `TestStrictHeaderAndPayload`: an unknown field in the header or payload, trailing data, or four parts is `ErrMalformed`.
  - `TestTrustedFileLimits`: missing, empty, three keys, a 31-byte key: all refused.
  - `TestKeysEnsureRequestWritesTrusted`, `TestKeysThirdKidRefused`, `TestKeysUntrustLastRefused`, `TestKeysDefaultKindIsAccess`.
  - `TestRequestTokenVectorsUpToDate` (A25): regenerating the vectors in memory from the fixed test key and the fixed clock gives byte-identical files to `testdata/request-tokens/`. The vectors cover: valid `req`, valid `job`, valid global job, tampered payload, wrong `typ`, unknown `kid`, wrong `aud`, expired, `exp` too far out, wrong `nsub`, wrong `bh`, `alg` `none`, `alg` `HS256`, wrong `iss`, missing `rid`, unknown claim, oversize token. Each file holds the token, the public key set, the body (base64), the subject, the `knd`, the service, `now`, and the expected verdict (`ok` or the reason code shared with Python, §7.3).
  - `TestServeNeedsRequestKey`: `ValidateServe` names `REQUEST_KEY_PATH` when it is unset.
- [ ] **Step 2: Run (Go, remote):** `go test -race ./internal/reqtoken/... ./cmd/core/... ./internal/config/...` → fails.
- [ ] **Step 3:** Implement `reqtoken`, the trusted file, the CLI flags and the config field. The vector key is a fixed Ed25519 seed declared in `vectors_test.go` and named as test-only. It exists only inside the vector files (each carries its public key inline); no key file is ever written from it, so `core serve` and the workers can't load it.
- [ ] **Step 4:** Write the vectors: `go test ./internal/reqtoken -run TestRequestTokenVectorsUpToDate -update`. Run all of Step 2 again; it passes.
- [ ] **Step 5:** Commit: "Mint per-call request tokens with their own Ed25519 key, and share token vectors with Python".

**Security checklist**
- [ ] One algorithm, one `typ`, fixed claims; no general JWT library.
- [ ] Separate key file from `ACCESS_KEY_PATH`; the private key never leaves core; `keys` prints only the kid.
- [ ] Errors never contain the token, a claim value or a path.
- [ ] `jti` from `crypto/rand`; `bh` compared in constant time in `VerifyAt`.
- [ ] The trusted file holds public keys only and at most two.

### Task 3: The operations table

**Files:**
- Create: `services/core/internal/authz/operations.go`, `operations_test.go`
- Create (generated): `services/intelligence/intelligence/boundary/operations.json`
- Modify: `services/core/internal/authz/permissions.go` (`Rule.Calls`, validation), `generate.go` (`OperationsJSON`, a Markdown section), `services/core/cmd/permgen/main.go` and `main_test.go`, `docs/specs/generated/permissions.md` (regenerated)
- Delete: `services/intelligence/intelligence/security/permissions.json` (departure 14)

**Interfaces:**
- `authz.Operation{Name, Service, Op string; Kind reqtoken.Kind; AnyRole []string; SvcCore, NeedsHospital, TestOnly bool; Timeout time.Duration; RequestSchema, ResponseSchema string}`
- `authz.Operations() []Operation`, sorted by name, and `authz.OperationByName(name) (Operation, bool)`. The first rows:

  | Name | Kind | Roles | `svc:core` | Hospital | Timeout | Test only |
  |---|---|---|---|---|---|---|
  | `pipeline.run_query` | job | `clinician` | no | yes | 10 min | no |
  | `pipeline.health` | req | none | yes | no | 2 s | no |
  | `echo.ping` | req | `clinician` | yes | yes | 5 s | yes |

- `Rule.Calls []string`: the operations a route publishes to. `validate` panics (so every test and start-up fails) if a named operation isn't in the table, or the route's roles aren't a subset of the operation's. A route with no `AnyRole` counts as every clinical role.
- `authz.OperationsJSON(ops) ([]byte, error)`: `[{name, service, op, kind, any_role, svc_core, needs_hospital, timeout_ms, request_schema, response_schema}]`, sorted. `TestOnly` rows are included (the `echo` worker needs its row) and marked `"test_only": true`.

- [ ] **Step 1: Write the failing tests.**
  - `TestOperationNamesMatchSubjects`: `Name` is `Service + "." + Op`, each part `[a-z0-9_]+`; every `Kind` is `req` or `job`; every `req` timeout ≤ `reqtoken.MaxReqTTL`, every `job` ≤ `MaxJobTTL`.
  - `TestRouteCallsUnknownOperationPanics`.
  - `TestRouteRolesSubsetOfOperation` (A23): a route with `AnyRole: [clinician, analyst]` calling `pipeline.run_query` panics.
  - `TestOperationsJSONUpToDate` (A23): `permgen`'s render of `operations.json` equals the committed file. Removing `clinician` from `pipeline.run_query` in Go without regenerating fails this test, which CI runs.
  - Extend `TestGeneratedFilesUpToDate` for the new Markdown section and the removed `permissions.json`.
- [ ] **Step 2: Run (Go, remote):** `go test -race ./internal/authz/... ./cmd/permgen/...` → fails.
- [ ] **Step 3:** Implement the table and the generator; run `go run ./cmd/permgen` remotely and commit its output.
- [ ] **Step 4:** Run the tests; they pass.
- [ ] **Step 5:** Commit: "Add the operations table that both core and the Python workers read".

**Security checklist**
- [ ] The rule exists once, in Go; the JSON is generated and diff-checked in CI.
- [ ] A route's roles must be a subset of the roles of every operation it calls; the check panics at start-up and in every test.
- [ ] `svc:core` is a per-operation opt-in, off by default.

### Task 4: NATS: per-service users, streams and limits

**Files:**
- Modify: `services/nats/nats.conf`, `services/nats/README.md`
- Modify: `services/core/internal/bus/bus.go` (TLS options, `EnsureStreams`), `bus_integration_test.go`
- Modify: `services/core/internal/testutil/nats.go` (`PipelineURL`, `EchoURL` instead of `WorkerURL`; a TLS variant)
- Modify: `docker-compose.yml` and `.env.example` (`NATS_PIPELINE_PASSWORD`, `NATS_ECHO_PASSWORD`, `NATS_JS_KEY`; `NATS_WORKER_PASSWORD` removed), `services/core/cmd/core/compose_test.go`

**Interfaces:**
- `nats.conf`:
  - `max_payload: 1MB`; `jetstream { store_dir: /data, cipher: chachapoly, key: $NATS_JS_KEY }` (encryption at rest).
  - `gateway`: publish `inference.v1.>`, `jobs.v1.>`, `$KV.>`, and only these `$JS.API` subjects (braces are shorthand here; the config lists each subject on its own line): `INFO`, `STREAM.{CREATE,UPDATE,INFO}.{KV_<bucket>,JOBS,DEAD}` for the four buckets, `STREAM.MSG.GET.KV_<bucket>`, `DIRECT.GET.KV_<bucket>.>`, `CONSUMER.{CREATE,DURABLE.CREATE,INFO}.{KV_<bucket>,JOBS,DEAD}.>`, `CONSUMER.MSG.NEXT.DEAD.core_dead`, `$JS.ACK.DEAD.core_dead.>`. Keep `STREAM.NAMES` and `STREAM.LIST` only if a test shows `nats.go` needs them. Subscribe `_INBOX_gateway.>`. No stream delete or purge.
  - `pipeline`: subscribe `inference.v1.pipeline.>` and `_INBOX_worker.pipeline.>`; publish `dead.v1.>`, `$JS.API.CONSUMER.INFO.JOBS.pipeline`, `$JS.API.CONSUMER.MSG.NEXT.JOBS.pipeline`, `$JS.ACK.JOBS.pipeline.>`; `allow_responses: {max: 1, expires: "1m"}`.
  - `echo`: subscribe `inference.v1.echo.>` and `_INBOX_worker.echo.>`; publish nothing but responses.
- `bus.Connect` gains `TLS{CAPath, CertPath, KeyPath string}` (env `NATS_TLS_CA_PATH`, `NATS_TLS_CERT_PATH`, `NATS_TLS_KEY_PATH`); production config refuses to start without it.
- `bus.EnsureStreams(ctx, js, StreamConfig) error`, run by `core serve`:
  - `JOBS` on `jobs.v1.>`: file storage, `max_age` 7 days, `duplicates` 2 min, `discard: new`, `max_msgs_per_subject` 10,000.
  - `DEAD` on `dead.v1.>`: file storage, `max_age` 30 days.
  - Durable `pipeline` on `JOBS`, filter `jobs.v1.*.run_query`: explicit ack, `ack_wait` 60 s, `max_deliver` 5, **no `BackOff`** (departure 4), `max_ack_pending` from `JOBS_PIPELINE_MAX_ACK_PENDING` (default 8).
  - Durable `core_dead` on `DEAD`: explicit ack, `ack_wait` 30 s.

- [ ] **Step 1: Write the failing integration tests** (tag `integration`, real `nats.conf`).
  - `TestServiceUserCannotPublishRequestsOrJobs` (A2): as `pipeline`, publishing on `inference.v1.echo.ping` and on `jobs.v1.<uuid>.run_query` each gets a permissions violation; same for `echo`.
  - `TestServiceUserCannotSubscribeOthers` (A16): as `pipeline`, subscribing to `_INBOX_gateway.>`, `inference.v1.echo.>` and `_INBOX_worker.echo.>` each gets a permissions violation.
  - `TestServiceUserHasNoKVAccess` and `TestServiceUserCannotTouchOtherConsumers`: `pipeline` can't fetch from `core_dead` or create a consumer.
  - `TestMaxPayloadRefusesTwoMB` (A18, NATS side): a 2 MB publish by `gateway` fails with `nats.ErrMaxPayload`.
  - `TestAllowResponsesOnlyOnce` and `TestAllowResponsesExpire`: a worker can answer a request once, and can't publish to that reply subject a second time (closes TODO T3 N5).
  - `TestGatewayCannotDeleteOrPurge` stays; `TestGatewayCannotReadOtherStreams`: a stream named `OTHER` can't be created or read.
  - `TestEnsureStreamsIdempotent`: twice in a row, same config; the `pipeline` consumer has no `BackOff`.
  - `TestDuplicateMsgIDDropped`: two publishes with one `Nats-Msg-Id` within 2 min store one message.
  - `TestConnectTLS`: with the TLS test server, a client without the CA fails and one with it connects.
  - Rename the existing `worker` tests to `pipeline`; `TestComposeSplitsSecretsPerService` checks each worker service sees only its own NATS password.
- [ ] **Step 2: Run (Go, remote):** `go test -race -tags integration ./internal/bus/... ./cmd/core/...` → fails.
- [ ] **Step 3:** Rewrite `nats.conf`, `testutil`, `bus` and compose.
- [ ] **Step 4:** Run the tests; they pass. **Run (remote):** `docker compose config -q` from the repo root (with a throwaway `.env` from `.env.example`).
- [ ] **Step 5:** Commit: "Give each Python service its own NATS user, and add the JOBS and DEAD streams".

**Security checklist**
- [ ] Only `gateway` publishes `inference.v1.>` and `jobs.v1.>`; each service subscribes only its own subjects and inbox.
- [ ] No service user has KV access or any `$JS.API` subject beyond its own consumer.
- [ ] `gateway`'s `$JS` wildcards narrowed to the KV streams, `JOBS` and `DEAD` (closes TODO T3 N1).
- [ ] JetStream storage encrypted; the key is an env secret that refuses placeholders.
- [ ] Every new subject has a test that the wrong user is refused.

### Task 5: Core's intelligence client, job publisher, sweeper and in-flight cap

**Files:**
- Create: `services/core/internal/intel/{client.go,errors.go,jobs.go,sweeper.go,cap.go}`, tests beside them, and `internal/intel/fakeworker_test.go` (a Go fake worker that verifies with `reqtoken.VerifyAt`)
- Create: `services/core/internal/db/migrations/00007_jobs.sql`
- Modify: `services/core/internal/httpx/errors.go` (`unavailable`; `ErrorRetryAfter`), `errors_test.go`; `services/core/internal/db/db.go` (`WithJobs`); `services/core/internal/authz/nodirectdb_test.go` (allow `internal/intel/sweeper.go` and `deadletter.go` for `db.WithJobs`, with reasons); `services/core/cmd/core/serve.go` (load the request signer, `EnsureStreams`, start the sweeper); `services/core/openapi.yaml` (the new code)

**Interfaces:**
- `00007_jobs.sql`:
  - `clinical_queries` gains `request_id text`, `attempts int NOT NULL DEFAULT 0`, `lease_until timestamptz`, `published_at timestamptz`, `deadline_at timestamptz`, `cancel_reason text`; `CHECK (status IN ('queued','running','completed','failed','cancelled'))`; `CHECK (error IS NULL OR error ~ '^[a-z_]{1,32}$')`; an index on `(status, published_at)` for the sweeper.
  - `clinical_decisions` gains `verified boolean`, `degraded boolean`, `failed_agents text[]`, `model_versions jsonb`, `trust_band text` (departure 8).
  - Role `mhai_jobs` (NOLOGIN, NOBYPASSRLS), granted to `mhai_app WITH INHERIT FALSE`: `SELECT (id, hospital_id, created_by, request_id, status, attempts, published_at, deadline_at, created_at)` and `UPDATE (status, error, published_at, cancel_reason, completed_at)` on `clinical_queries`, with policies `USING (true)` for that role only; and, so the sweeper and the dead-letter consumer can audit, `INSERT` on `audit_log` with `WITH CHECK (action IN ('query.failed','query.cancelled','job.dead_lettered','job.retried','intelligence.refused'))`, the `audit_log_id_seq` and `EXECUTE` on `audit_chain_head()`. No other table.
- `db.WithJobs(ctx, pool, fn)`: role `mhai_jobs`.
- `intel.Caller{Sub, Hid, Rid string; Roles []string}`; `intel.FromPrincipal(p authz.Principal, rid string) Caller`; `intel.System(hid, rid string) Caller` (`sub` = `svc:core`).
- `intel.Client{NC *nats.Conn; JS jetstream.JetStream; Signer reqtoken.Signer; Log *slog.Logger}`
- `(c *Client) Request(ctx, caller Caller, op string, req, resp proto.Message) (*intelpb.ResponseMeta, error)`: looks up the operation, checks `caller` against its roles (core's own check; the worker repeats it), marshals, mints a `req` token with `ttl = op.Timeout` (or the context deadline if sooner), sets `Mhai-Request-Token`, `Mhai-Schema`, `Mhai-Request-Id`, and sends with `nc.RequestMsgWithContext`. Reply status from `Mhai-Status`; `Error` decoded into `*intel.Error{Code string; Retryable bool}`. A NATS timeout is `ErrTimeout`; no responders is `ErrNoResponders`. Never retried.
- `(c *Client) PublishJob(ctx, caller Caller, op string, jobID uuid.UUID, msg proto.Message, deadline time.Time) error`: subject `jobs.v1.<hid>.<type>` (or `jobs.v1.global.<type>`), `Nats-Msg-Id` = job id, `job` token with `exp = deadline`. A `discard: new` refusal is `ErrBackpressure`.
- `intel.Code(err error) (code string, retryAfter int)`: the one table of spec §10.1. `httpx.ErrorRetryAfter(w, r, code, seconds)`.
- `intel.ReserveInFlight(ctx, tx pgx.Tx, hid uuid.UUID, limit int) error`: in the caller's transaction, `pg_advisory_xact_lock(hashtext('mhai.inflight'), hashtext(hid::text))`, then counts `queued` and `running` rows; at the limit, `ErrAtCap` (→ 429 `rate_limited`). The lock is held to commit, so two parallel submits can't both take the last slot. `INTELLIGENCE_QUERY_CAP` (default 10).
- `intel.RunPublishSweeper(ctx, pool, client, every time.Duration)`: every 10 s, through `WithJobs`, finds `queued` rows with `published_at IS NULL` older than 10 s and before `deadline_at`, re-checks the creator and hospital (`db.WithAuth`, `authz.AccountUsable`), mints a fresh token and publishes with the same `Nats-Msg-Id`; marks `published_at`. Past the deadline it sets `failed` / `deadline_exceeded`.

- [ ] **Step 1: Write the failing tests** (`integration` where a container is needed).
  - `TestEveryIntelligenceCodeMapped` (error-table coverage): each of the nine codes, plus timeout and no responders, maps to exactly the status, code and `Retry-After` in spec §10.1; an unknown code maps to 500 `internal_error`.
  - `TestRefusalsLookTheSame` (A17): with the fake worker answering `unauthorized`, `forbidden` and `invalid_request` in turn, core's HTTP responses (through a test route under the `test` tag) have identical status, headers and body apart from `request_id`.
  - `TestRequestCarriesBoundToken`: the fake worker verifies the token against the subject, body, `aud` and `knd`; `Mhai-Request-Id` equals `rid` and `X-Request-ID`.
  - `TestRequestTimeoutIs503` and `TestNoRespondersIs503`: 503 `unavailable`, no retry (the fake worker counts one message).
  - `TestOverloadedSetsRetryAfter`: `Retry-After: 5`.
  - `TestPublishJobSetsMsgID` and `TestPublishJobDeadlineIsTokenExp`.
  - `TestSweeperRepublishesUnpublished`: a `queued` row with no `published_at` is published once with a fresh token; a deactivated creator's row isn't, and is set `cancelled`.
  - `TestSweeperFailsPastDeadline`.
  - `TestInFlightCapPerHospital` (A19): with a limit of 2, 5 parallel reservations for hospital A succeed exactly twice; hospital B still reserves.
  - `TestJobsRoleIsNarrow`: as `mhai_jobs`, `SELECT patient_data_json` and `SELECT clinical_question` are permission denied; `SELECT * FROM clinical_decisions` is permission denied; an `audit_log` insert with action `auth.login.succeeded` fails.
  - `TestUnavailableInCatalogue`: `httpx.Codes["unavailable"] == 503`, and the platform audience body is `{"code":"unavailable"}`.
- [ ] **Step 2: Run (Go, remote):** `go test -race -tags integration ./internal/intel/... ./internal/httpx/... ./internal/db/...` → fails.
- [ ] **Step 3:** Implement the migration, `WithJobs`, the client, the error map, the publisher, the cap and the sweeper; wire them in `serve.go`.
- [ ] **Step 4:** Run the tests and `go test -race -tags "integration test" ./...`; they pass.
- [ ] **Step 5:** Commit: "Add core's intelligence client, job publisher, sweeper and per-hospital cap".

**Security checklist**
- [ ] The token's `hid`, `sub` and `roles` come only from the principal (or `svc:core` for system work); nothing from the request body.
- [ ] Core checks the operation's roles before minting; a refused caller never reaches NATS.
- [ ] Reserve, then check: the cap's lock is taken before the count, in the same transaction as the insert.
- [ ] `mhai_jobs` sees no clinical text and no other table; the sweeper re-checks the account before re-minting.
- [ ] Worker refusals reach the user as one fixed 500 body; the reason is logged with ids only.
- [ ] No silent retry of a request/reply call.

### Task 6: The Python verifier

**Files:**
- Create: `services/intelligence/intelligence/boundary/{__init__.py,config.py,keys.py,replay.py,operations.py,verify.py,envelope.py}`
- Create: `services/intelligence/tests/boundary/{test_verify.py,test_token_vectors.py,test_keys.py,test_replay.py,test_operations.py}`
- Modify: `services/intelligence/pyproject.toml` (markers: `boundary: needs Docker (testcontainers), never loads models`; mypy covers `intelligence/boundary`)

**Interfaces:**
- `keys.TrustedKeys`: `load(path) -> TrustedKeys` (refuses missing, empty, more than two, non-32-byte keys); `.get(kid)`; `install_sighup_reload(trusted, path)` re-reads on `SIGHUP` and keeps the old set if the new file is bad (logged, metric).
- `replay.JtiSet(clock)`: `.seen_or_add(jti, exp) -> bool` under a `threading.Lock`; entries kept until `exp + 5 s`; purged on insert. Bounded: at most 100,000 entries, past which it refuses (fails closed with `overloaded`).
- `operations.Operations.load(path)`: from `operations.json`; `.for_subject(subject) -> Operation | None`; `require(service, handler_names)` raises at start-up if a handler has no entry.
- `verify.Verified`: frozen dataclass `{sub, hid, roles, rid, jti, kid, kind, subject, operation, patient_ids, exp}`. Only `verify.Verifier` constructs it (a private module token in `__init__`), so nothing else can fake one.
- `verify.Verifier(trusted, service, operations, replay, clock)`:
  - `.verify_request(subject, reply, headers, body, decoder) -> Verified | Refusal` and `.verify_job(subject, headers, body, decoder) -> Verified | Refusal`.
  - Checks in exactly the spec's §7.3 order: sizes; token shape and signature (three parts, strict JSON with no unknown fields, `alg` `EdDSA`, `typ` `mhai-req+jwt`, known `kid`, Ed25519 via `cryptography`); claims (`iss`, `aud` = this service, required claims, `iat ≤ now + 5 s`, `now < exp`, lifetime cap per `knd`); binding (`nsub` = subject, `knd` = how it arrived, `bh` with `hmac.compare_digest`, the subject's hospital = `hid` for jobs); reply subject starts `_INBOX_gateway.` (request/reply only); replay (request/reply only, spec §7.4); schema (`Mhai-Schema` = the operation's request type, `Mhai-Request-Id` = `rid`, then `decoder(body)` strictly); scope (every hospital id in the body = `hid`, every patient id in `pids`, empty `hid` refused where the operation needs a hospital); permission (one of `roles` in the operation's `any_role`, or `svc_core` when `sub` is `svc:core`).
  - `Refusal{reason, code, reply}`: `reason` is one of the shared reason codes (the same strings the Go vectors use: `size`, `malformed`, `signature`, `claims`, `expired`, `binding`, `reply_subject`, `replay`, `schema`, `request_id`, `scope`, `no_hospital`, `forbidden`); `code` is `unauthorized` (checks 1–6), `invalid_request` (7–8) or `forbidden` (9); `reply` is false when check 5 failed.
- `envelope`: header names; `reply_ok(meta, msg)`, `reply_error(code, retryable)`; `CODES` (the closed set).

- [ ] **Step 1: Write the failing tests** (unit; no Docker).
  - `test_shared_token_vectors` (A25): every file in `testdata/request-tokens/` gets the expected verdict and reason.
  - `test_missing_token_refused` (A1): no `Mhai-Request-Token` → `unauthorized`, and the decoder passed in is never called.
  - `test_replayed_jti_refused` (A3): the same valid message twice → first `Verified`, second `replay`.
  - `test_token_for_other_operation_refused` (A5): a token minted for `inference.v1.echo.ping` presented on `inference.v1.pipeline.health` → refused (`claims` for `aud`, or `binding` for `nsub` when `aud` matches).
  - `test_access_token_refused` (A6): a core access token (built with the access-token header shape) → `signature`.
  - `test_body_byte_flip_refused` (A7).
  - `test_body_hospital_mismatch_refused` (A8): `PingRequest.hospital_id` = hospital B with a token for A → `scope`, `invalid_request`, and the handler stub isn't called.
  - `test_alg_none_hs256_unknown_kid_and_long_exp_refused` (A10).
  - `test_foreign_reply_subject_refused_silently` (A15): a valid token with reply `_INBOX_worker.x` → `reply_subject` with `reply=False`; one log line with `rid`, `jti`, `kid`, `reason` and no token or body (checked with `caplog`).
  - `test_no_body_parse_before_signature`: a bad signature with a body that would crash the decoder → `signature`; decoder not called.
  - `test_role_not_in_operation_forbidden`; `test_svc_core_only_where_allowed`.
  - `test_job_subject_hospital_must_equal_hid`.
  - `test_trusted_keys_limits`, `test_reload_keeps_old_set_on_bad_file`, `test_reload_drops_removed_kid` (A24, worker side).
  - `test_replay_set_bounded_and_expires`.
  - `test_operations_missing_handler_refuses_start`.
- [ ] **Step 2: Run (Python, remote):** `uv run pytest -q tests/boundary -m "not boundary"` → fails.
- [ ] **Step 3:** Implement the modules. No JWT library; `cryptography`'s `Ed25519PublicKey.verify`.
- [ ] **Step 4:** Run the tests, then `uv run ruff check . && uv run ruff format --check . && uv run mypy`; all pass.
- [ ] **Step 5:** Commit: "Verify core's request tokens in Python before any work, against the shared vectors".

**Security checklist**
- [ ] Checks run in the spec's order; cheap and signature checks first; nothing decodes the body before check 7.
- [ ] `Verified` can't be built outside the verifier.
- [ ] Constant-time body hash compare; strict JSON; fixed `alg` and `typ`.
- [ ] Logs carry the reason code and ids only (`rid`, `jti`, `hid`, `kid`); never the token or the body.
- [ ] Keys only from the mounted file, never from NATS; at most two.
- [ ] The replay set is bounded and fails closed.

### Task 7: The Python worker runtime, `health` and `echo`

**Files:**
- Create: `services/intelligence/intelligence/boundary/{worker.py,services/__init__.py,services/echo.py,services/pipeline.py}`
- Create: `services/intelligence/tests/boundary/{conftest.py,test_worker.py}` (marker `boundary`)
- Modify: `services/intelligence/intelligence/cli.py` (`intelligence worker --service pipeline|echo`), `tests/unit/test_cli.py`; `pyproject.toml` (`nats-py`; dev `testcontainers`, `pytest-asyncio`)

**Interfaces:**
- `boundary.config.WorkerSettings` (env): `INTELLIGENCE_SERVICE`, `NATS_URL` (password in the URL for dev, refused if a placeholder), `NATS_NKEY_SEED_PATH`, `NATS_TLS_CA_PATH`, `INTELLIGENCE_TRUSTED_KEYS_PATH`, `INTELLIGENCE_OPERATIONS_PATH` (default: the packaged `operations.json`), `INTELLIGENCE_MAX_CONCURRENCY` (default 4), `INTELLIGENCE_DB_URL` (pipeline only). Errors name variables, never values. `echo` refuses to start when `APP_ENV=production`.
- `worker.Worker(settings, handlers: dict[str, Handler], verifier)`:
  - connects with `inbox_prefix=f"_INBOX_worker.{service}"`, subscribes `inference.v1.<service>.>` in queue group `<service>`;
  - per message: size check, then `verify_request`, then `asyncio.Semaphore(max_concurrency)` (`overloaded` at once when full), then the handler with `asyncio.timeout(exp − now)` (`deadline_exceeded`);
  - replies only through `msg.respond` (so only to the verified reply subject); on `SIGTERM` drains the subscription and finishes in-flight work.
- `Handler = Callable[[Verified, Message], Awaitable[tuple[ResponseMeta, Message]]]`.
- `services.echo.ping`: returns `PingResponse{text}` with an `echo.pinged` audit fact. `services.pipeline.health`: `HealthResponse{models_ready, database_ready}`.
- `conftest.py` fixtures: `nats_server` (testcontainers `nats:2.10-alpine` with the real `services/nats/nats.conf` and test-only passwords), `postgres` (16, migrated by `mhai-core:test migrate up`), `signer` (the vector key, via a tiny Python minting helper used **only** in tests, checked against the Go vectors in `test_token_vectors.py`).

- [ ] **Step 1: Write the failing tests** (marker `boundary`).
  - `test_echo_without_token_gets_unauthorized_and_handler_never_runs` (A1, end to end): connected as `gateway`, a request with no token gets `Mhai-Status: unauthorized`; the handler counter stays 0.
  - `test_replayed_request_refused` (A3, end to end): one valid message sent twice to a single `echo` worker.
  - `test_reply_to_foreign_inbox_gets_no_reply` (A15, end to end): a valid token with reply subject `_INBOX_worker.x` (published by `gateway` with that reply): nothing arrives there, the refusal counter increments.
  - `test_oversized_request_body_refused` (A18, worker side): a 300 KB body → `unauthorized` (`size`), handler not run.
  - `test_overloaded_when_full`: with concurrency 1 and a blocking handler, the second call gets `overloaded` at once.
  - `test_deadline_stops_handler`: a handler past `exp` is cancelled and answers `deadline_exceeded`.
  - `test_health_ok`: `pipeline.health` called with an `svc:core` token answers `ok`.
  - `test_sigterm_drains`.
  - `test_cli_worker_refuses_echo_in_production`.
- [ ] **Step 2: Run (Python, remote):** `uv run pytest -q -m boundary tests/boundary/test_worker.py` → fails.
- [ ] **Step 3:** Implement the runtime, the two services and the CLI command.
- [ ] **Step 4:** Run the tests and the lint, format and mypy checks; they pass.
- [ ] **Step 5:** Commit: "Run the Python services as NATS workers with per-service inboxes, limits and deadlines".

**Security checklist**
- [ ] Every message goes through the verifier; there is no handler path around it.
- [ ] Replies only via `respond` to a subject that passed check 5.
- [ ] Concurrency and deadlines bound the work; overload answers at once.
- [ ] The test-only minting helper lives under `tests/` and is never importable from `intelligence/`.
- [ ] `echo` can't run in production.

### Task 8: Python database access and the worker's grants

**Files:**
- Create: `services/core/internal/db/migrations/00008_intelligence_worker.sql`, `services/core/internal/db/worker_integration_test.go`
- Modify: `services/core/internal/db/maintenance.go` (`loginRoles` adds `mhai_worker_app`), `services/core/cmd/core/main.go` (`INTELLIGENCE_DB_USER` / `INTELLIGENCE_DB_PASSWORD`), `migrate_test.go`
- Create: `services/intelligence/intelligence/boundary/db.py`, `services/intelligence/tests/boundary/{test_db.py,test_no_direct_db.py}`
- Modify: `services/intelligence/pyproject.toml` (`psycopg[binary]`, `psycopg-pool`)

**Interfaces:**
- `00008_intelligence_worker.sql`:
  - Role `mhai_worker_app` NOLOGIN NOINHERIT; `GRANT mhai_worker TO mhai_worker_app WITH INHERIT FALSE` (departure 2).
  - Narrow `mhai_worker`: `REVOKE UPDATE ON clinical_queries`, then `GRANT UPDATE (status, progress, current_stage, started_at, completed_at, error, attempts, lease_until)`; `clinical_decisions`, `hga_evaluations`: `SELECT, INSERT` (kept); `kb_chunks`: `REVOKE INSERT, UPDATE` (no enrichment in this plan) and the sequence grant; `guardrail_weights`: `SELECT`.
  - Table `intelligence_audit_outbox (id bigserial, hospital_id uuid NOT NULL, query_id uuid NOT NULL, action text NOT NULL CHECK (action IN ('query.completed','query.failed','query.cancelled')), entity text, entity_id text, payload jsonb NOT NULL DEFAULT '{}', created_at timestamptz NOT NULL DEFAULT now(), FOREIGN KEY (query_id, hospital_id) REFERENCES clinical_queries (id, hospital_id))`. RLS enabled and forced; owner `mhai_migrate`. `mhai_worker`: `INSERT` only, policy `WITH CHECK (hospital_id = mhai_current_hospital())`, plus the sequence.
  - Role `mhai_audit_drain`, granted to `mhai_app WITH INHERIT FALSE`: `SELECT, DELETE` on the outbox (`USING (true)`); `SELECT (id, hospital_id, created_by, request_id)` on `clinical_queries` (`USING (true)`); `INSERT` on `audit_log` with `WITH CHECK (action IN ('query.completed','query.failed','query.cancelled'))`, the `audit_log_id_seq` and `EXECUTE` on `audit_chain_head()` (departure 3).
- `db.EnsureLogin` accepts `mhai_worker_app`; `core migrate up` provisions `INTELLIGENCE_DB_USER` (default `intelligence_app`) from `INTELLIGENCE_DB_PASSWORD` like the other two logins.
- Python `boundary.db.Database(url)`: an `AsyncConnectionPool` (`psycopg_pool`) with credentials parsed out of the URL and passed separately; `check_login()` at start-up (the twin of `db.CheckLoginRole`: refuses a superuser, BYPASSRLS, CREATEROLE, a login that isn't a member of `mhai_worker_app` only, or one that inherits any `mhai_*` role).
- `async with db.with_hospital(v: Verified) as cur:` runs `BEGIN; SET LOCAL ROLE mhai_worker; SELECT set_config('mhai.hospital_id', %s, true)` with `v.hid`, yields a cursor, commits on success, rolls back on error. It refuses a `Verified` with an empty `hid`. Parameterised queries only.

- [ ] **Step 1: Write the failing tests.**
  - Go `TestIntelligenceLoginHoldsNothing` (A22, core side): as the provisioned `intelligence_app`, `SELECT 1 FROM clinical_queries` outside `SET ROLE` is permission denied; `SET ROLE mhai_gateway`, `mhai_auth`, `mhai_platform`, `mhai_jobs` and `mhai_audit_drain` all fail; `SET ROLE mhai_worker` works.
  - Go `TestWorkerGrantsNarrowed`: as `mhai_worker`, `UPDATE clinical_queries SET clinical_question = 'x'` is permission denied; `UPDATE ... SET status` works under RLS; `INSERT INTO kb_chunks` is permission denied; `SELECT` from `users`, `sessions` and `audit_log` is permission denied.
  - Go `TestOutboxRLS`: as `mhai_worker` at hospital A, inserting an outbox row for B fails `WITH CHECK`; selecting from the outbox is permission denied.
  - Go `TestAuditDrainRoleNarrow`: as `mhai_audit_drain`, `SELECT clinical_question` is permission denied, and inserting an `audit_log` row with action `auth.login.succeeded` fails.
  - Go `TestEnsureLoginWorkerApp` in `migrate_test.go`: a placeholder `INTELLIGENCE_DB_PASSWORD` is refused; the variable name, not the value, is in the error.
  - Python `test_login_outside_role_fails` (A22, worker side, `boundary`): a raw query on the pool without `with_hospital` fails; `SET ROLE mhai_gateway` fails.
  - Python `test_with_hospital_cannot_see_other_hospital_query` (A9, `boundary`): rows for A and B; under `with_hospital(A)`, selecting B's id returns nothing.
  - Python `test_worker_cannot_write_kb_chunks` (A11, `boundary`): inserting or updating a `kb_chunks` row with `hospital_id` null or B's id is refused by Postgres.
  - Python `test_check_login_refuses_superuser` (`boundary`).
  - Python `test_no_direct_db` (unit): an AST walk of `intelligence/` finds no `psycopg.connect`, `AsyncConnection`, `ConnectionPool`, `.cursor(` or `.execute(` outside `boundary/db.py` and `boundary/kb_repo.py`'s calls on the cursor it is given.
- [ ] **Step 2: Run (Go, remote):** `go test -race -tags integration ./internal/db/... ./cmd/core/...` → fails. **Run (Python, remote):** `uv run pytest -q tests/boundary/test_db.py tests/boundary/test_no_direct_db.py` → fails.
- [ ] **Step 3:** Write the migration, the provisioning change and `db.py`.
- [ ] **Step 4:** Run both; they pass. Then `go test -race -tags integration ./...` (the existing `TestCheckLoginRole*` and RLS tests must still pass).
- [ ] **Step 5:** Commit: "Give the pipeline its own database login that holds nothing outside SET LOCAL ROLE mhai_worker".

**Security checklist**
- [ ] The login holds no privilege until `SET LOCAL ROLE mhai_worker`, and can reach no other role (retires the superuser connection, TODO T2 M6).
- [ ] The hospital comes only from a `Verified`; RLS is the second wall.
- [ ] Column-level `UPDATE`; no `kb_chunks` writes; shared rows stay read-only.
- [ ] Every new table has RLS forced, an owner of `mhai_migrate`, grants and refusal tests.
- [ ] Credentials parsed out of the URL; errors never carry them.

### Task 9: The `run_query` job

**Files:**
- Create: `services/intelligence/intelligence/boundary/{jobs/__init__.py,jobs/run_query.py,kb_repo.py,models.py}`
- Create: `services/intelligence/tests/boundary/{test_run_query.py,test_models.py}`
- Modify: `services/intelligence/intelligence/boundary/worker.py` (the JetStream consumer loop), `services/pipeline.py`

**Interfaces:**
- `models.ModelClient` (Protocol): `mask(text) -> str`; `generate(question, patient_context, patient_id, chunks) -> Generation` (retrieval and generation together, departure 9); `verify(generation) -> Verification` (NLI and HGA); `model_versions() -> dict[str, str]`. `OrchestratorModelClient` wraps `DynamicOrchestratorAgent` and `PIIMasker` without changing them; failures escalate (ADR 0005), never invent a score. `StubModelClient` returns fixed synthetic answers and can be told to fail, block or raise per stage.
- `kb_repo.load_chunks(cur, patient_id) -> list[KBChunk]`: `kb_chunks` rows visible under RLS (own hospital and shared), shared rows mapped to `source_patient_id = REFERENCE_SOURCE` (departure 10); then filtered with `chunk_visible_to`.
- Consumer: `js.pull_subscribe_bind(durable="pipeline", stream="JOBS")`, fetch batch 1, `max_ack_pending` from core's config.
- `run_query.handle(verified, msg, db, models)`:
  1. **Claim** under `with_hospital(verified)`: `UPDATE clinical_queries SET status='running', attempts=attempts+1, started_at=coalesce(started_at, now()), lease_until=now()+interval '90 s' WHERE id=%s AND (status='queued' OR (status='running' AND lease_until < now())) RETURNING clinical_question, patient_data_json, urgency, category, attempts`. No row → read the row's `status` under the same RLS: no visible row (another hospital's id, or none) → `not_found`: `term()` and a dead letter, with no write; a terminal or leased row → ack and stop.
  2. Stages `pii_masking` → `generation` → `nli_verification`/`hga_verification` → `decision`, each in `asyncio.to_thread`. Between stages: `+WIP` if 20 s have passed, extend `lease_until`, update `progress` and `current_stage`, and read `status`; `cancelled` → ack, stop, write nothing more.
  3. **Complete** in one transaction: insert `clinical_decisions` (unique `query_id`), `hga_evaluations`, one outbox row `query.completed` with payload `{query_id, decision, trust_band, chunk_ids, model_versions, attempts}`; set `status='completed'`, `progress=1`, `completed_at=now()`. Then ack. A unique violation on `query_id` → roll back and ack (another delivery finished it).
  4. **Fail:** retryable (`unavailable`, `internal` before the last delivery) → `nak(delay)` with 10 s, 30 s, 2 min, 5 min by `num_delivered`. Permanent, or the last delivery → `term()`, a `DeadLetter` on `dead.v1.<hid>.run_query`, and `status='failed'` with the code plus a `query.failed` outbox row.
  5. **Verification refused** (no `Verified`): `term()` and a `DeadLetter` with `subject_hospital` copied from the subject and the reason code; **no database access** (departure 5).
  6. **Deadline:** past `exp`, `status='failed'`, `error='deadline_exceeded'`, `term()`.

- [ ] **Step 1: Write the failing tests** (marker `boundary`, stub models).
  - `test_job_runs_to_completed`: one decision row, one outbox `query.completed`, `status='completed'`.
  - `test_redelivery_after_completion_acks_without_second_decision` (A4): redeliver the same message after completion → acked, still one decision row, one outbox row.
  - `test_two_workers_race_one_decision`: two workers receive the same message at once (lease expired on one) → exactly one decision row.
  - `test_foreign_query_id_is_not_found_and_reads_nothing` (A9): a job for hospital A naming B's `query_id` → no claim, `failed` is not written to B's row (B's `status`, `attempts` and `started_at` unchanged, checked as superuser), message acked; Postgres `log_statement=all` shows only the claim statement for that id, run as `mhai_worker` with hospital A.
  - `test_completion_commit_writes_outbox_atomically` (A13, worker side): the stub raises right after commit and before ack → the outbox row exists; the redelivery acks without a second decision.
  - `test_poison_job_dead_letters_after_five_deliveries` (A20): a stub that always raises `unavailable` → exactly 5 deliveries (with `nak` delays shortened by a test setting), then `term`, one `dead.v1` message, `status='failed'`.
  - `test_cancelled_between_stages_stops` (A21, worker side): set `cancelled` while `generation` blocks → no decision row, no outbox `query.completed`, message acked.
  - `test_refused_job_touches_no_database`: a job with a tampered token → `term`, one dead letter with `reason`, and no connection is checked out from the pool.
  - `test_deadline_marks_failed`.
  - `test_wip_sent_on_long_stage`: a 25 s stub stage (clock-controlled) sends `+WIP`.
  - `test_shared_chunks_visible_patient_chunks_not` (unit, `test_models.py`): `kb_repo` maps shared rows to reference chunks; another patient's chunk is filtered out.
  - `test_orchestrator_failure_escalates` (unit): an `OrchestratorModelClient` whose CCA raises yields an escalated, degraded result, never a pass.
- [ ] **Step 2: Run (Python, remote):** `uv run pytest -q -m boundary tests/boundary/test_run_query.py` and `uv run pytest -q tests/boundary/test_models.py` → fail.
- [ ] **Step 3:** Implement the consumer loop, the handler, the repository and the model clients.
- [ ] **Step 4:** Run the tests, then the whole suite `uv run pytest -q -m "not models"` (the existing security tests must still pass) and the lint, format and mypy checks.
- [ ] **Step 5:** Commit: "Run clinical questions as JetStream jobs: claim, lease, complete once, outbox, dead letters".

**Security checklist**
- [ ] Claim before any costly work; the claim and every write run under `with_hospital(verified)`.
- [ ] Completion is one transaction guarded by the unique `query_id`; ack only after commit.
- [ ] A refused job opens no connection; its dead letter carries ids, the type, attempts and a code only.
- [ ] The patient id for retrieval comes from the claimed row (none yet, departure 7), never from the message.
- [ ] No partial answer is ever written; a failure escalates or fails, it never passes.

### Task 10: Core's side of questions: endpoints, drainer, dead letters and cancellation

**Files:**
- Create: `services/core/internal/queries/{handlers.go,shapes.go}`, `handlers_integration_test.go`
- Create: `services/core/internal/intel/{drain.go,deadletter.go}`, `drain_integration_test.go`, `deadletter_integration_test.go`
- Create: `services/core/internal/db/migrations/00009_cancel_on_deactivate.sql`
- Modify: `services/core/internal/db/db.go` (`WithAuditDrain`), `internal/authz/nodirectdb_test.go` (allow `internal/intel/drain.go`), `internal/server/server.go` (register the routes), `cmd/core/serve.go` (start the drainer and the dead-letter consumer), `services/core/openapi.yaml`, `docs/specs/generated/permissions.md` (regenerated)

**Interfaces:**
- Routes (shapes from mhai-web `contract.ts`, departure 8):
  - `POST /queries` (`AnyRole: [clinician]`, `Write`, `Calls: [pipeline.run_query]`; `Idempotency-Key` through the existing middleware): body `QueryCreateIn` plus optional `client_created_at`; strict decode; `clinical_question` 1–4,000 characters, at most 50 symptoms of at most 200 characters, `age` 0–130. In one `authz.Tx`: `intel.ReserveInFlight`, insert the row (`status='queued'`, `request_id`, `created_by`, `deadline_at = now() + 10 min`), and `audit.Write` `query.submitted` (`query_id`, `urgency`, `category`). After commit, `PublishJob`; a publish failure is logged and left to the sweeper. → **202** `{query_id}`. At the cap → 429 `rate_limited`.
  - `GET /queries/{id}` (`AnyRole: [clinician]`): `QueryStatusOut`; `running` → `processing`, `cancelled` → `failed` with `error_code: "cancelled"`. Another hospital's id, or another clinician's, → 404.
  - `GET /queries/{id}/result` (`AnyRole: [clinician]`): `ClinicalResultOut` from `clinical_decisions`; 404 until completed.
- `db.WithAuditDrain(ctx, pool, fn)`: role `mhai_audit_drain`.
- `intel.RunDrainer(ctx, pool, every)`: every 2 s, in one transaction per batch of 100: read outbox rows, join `clinical_queries` for `created_by`, `hospital_id` and `request_id`, `audit.Write` each with those (the fact's `entity`, `entity_id` and payload only; payload keys `actor_id`, `hospital_id`, `request_id` and `sub` dropped before the sensitive-key check), delete the rows.
- `intel.RunDeadLetters(ctx, pool, js, client)`: the `core_dead` consumer. For each `DeadLetter`: look up the job by id through `WithJobs` and treat everything in the letter as unverified; mark the row `failed` with the code if it isn't already terminal; audit `job.dead_lettered` and, for `unauthorized`/`forbidden`/`invalid_request`, `intelligence.refused` (operation, reason, `kid`). If the reason is `signature` with a `kid` no longer in core's trusted set (an emergency rotation), and the creator and hospital are still usable, set the row back to `queued`, mint a new token and publish a new message (`Nats-Msg-Id` = `<query_id>.r<attempts>`), audit `job.retried`.
- `00009_cancel_on_deactivate.sql`: `SECURITY DEFINER` trigger functions owned by `mhai_migrate` on `users` (`AFTER UPDATE OF is_active`, to false) and `hospitals` (`AFTER UPDATE OF status`, away from `active`): set that user's or hospital's `queued` and `running` rows to `cancelled` with `cancel_reason`, and insert a `query.cancelled` outbox row for each (departure 11). It also adds `intelligence.key_rotated` to `mhai_provision`'s audit policy.
- Core reads the same public trusted-keys file (`REQUEST_TRUSTED_KEYS_PATH`) so the dead-letter consumer can tell a removed kid, and re-reads it on `SIGHUP`.
- `core keys untrust --kid K --trusted-out PATH [--emergency]`, and `keys new --kind request` when it adds a second kid, audit `intelligence.key_rotated` (`kid`, planned or emergency) through the provisioning login (`DATABASE_URL`, as `core users create`). First creation by `ensure` is not a rotation and needs no database.

- [ ] **Step 1: Write the failing integration tests.**
  - `TestPostQueries202AndAudited`: 202 `{query_id}`; the row is `queued`; a `query.submitted` audit row with no question text; the fake worker receives a `job` token for `pipeline` with this hospital and user.
  - `TestPostQueriesIdempotent`: the same `Idempotency-Key` twice → one row, one message.
  - `TestPostQueriesAtCap429` (A19, endpoint): at the cap, 429 `rate_limited` for hospital A; hospital B gets 202.
  - `TestPostQueriesValidation`: unknown field, a 4,001-character question, `urgency: "now"` → 422 `validation_failed`, body echoes nothing.
  - `TestGetQueryOtherHospital404` and `TestGetQueryOtherClinician404`.
  - `TestStatusMapping`: `running` → `processing`; `cancelled` → `failed` / `cancelled`.
  - `TestDrainerIgnoresFactIdentity` (A12): an outbox row whose payload says `actor_id` = another user and `hospital_id` = hospital B is recorded with the job's creator and hospital, and those keys are gone from the payload.
  - `TestReplyFactsUseTokenIdentity` (A12, request/reply): an `echo.ping` reply whose `audit_facts` payload names another actor is appended with the caller's identity.
  - `TestDrainerAppendsCompletedAfterWorkerCrash` (A13, core side): an outbox row committed with no worker running reaches the chain as `query.completed`, and `audit.Verify` passes.
  - `TestDeactivationCancelsQueuedJobs` (A21, core side): setting `users.is_active = false` sets that user's `queued` and `running` rows to `cancelled` and drains `query.cancelled`; another user's row is untouched. Same for a hospital set to `deactivated`.
  - `TestDeadLetterMarksFailed`: a dead letter for a known job marks it `failed` and audits `job.dead_lettered`; one naming an unknown or another hospital's job id changes nothing.
  - `TestEmergencyRotationReMintsDeadLetteredJob` (A24): a job signed with kid K1 is in the stream; `keys untrust K1` and a worker reload; the worker refuses it (`signature`); core's consumer re-mints with K2 and publishes a new message; the second delivery completes; the chain holds `job.dead_lettered`, `job.retried` and `intelligence.key_rotated`.
  - `TestOpenAPIMatchesHandlers` (extended) and `TestEveryRouteInTable` cover the three routes.
- [ ] **Step 2: Run (Go, remote):** `go test -race -tags integration ./internal/queries/... ./internal/intel/... ./internal/db/...` → fails. `TestEmergencyRotationReMintsDeadLetteredJob` needs the Python worker: it runs in the `intelligence-boundary` CI job and remotely via the compose stack of Task 13; until then it is skipped with a named reason.
- [ ] **Step 3:** Implement the routes, the drainer, the dead-letter consumer, the triggers and the wiring.
- [ ] **Step 4:** Run the tests and the full Go suite (`go test -race -tags integration ./...` and with `"integration test"`); they pass.
- [ ] **Step 5:** Commit: "Serve clinical questions end to end, drain intelligence's audit facts and handle dead letters".

**Security checklist**
- [ ] Every new route is in the permission table with its roles, `Write` and `Calls`.
- [ ] The question and patient data never enter an audit payload, a log or a dead letter.
- [ ] Another hospital's or another clinician's query is 404.
- [ ] The drainer takes identity from the job row; facts add detail only; the action allow-list holds in the database too.
- [ ] A dead letter is untrusted input: looked up by id under `mhai_jobs`, never acted on by its own claims.
- [ ] Re-minting re-checks the account and hospital; deactivation cancels in the database, whatever path made the change.

### Task 11: Retire the old Python security code

**Files:**
- Modify: `services/intelligence/intelligence/security/hipaa.py` (remove `UserRole`, `UserSession`, `RBACManager`, `AuditLogger`, `SecurePipelineWrapper` and `demo_m4_part2`; keep `PIIMasker`, `PIIDetection`, `EncryptionManager`, the latency profiler), `security/__init__.py`, `pipeline/cross_milestone.py`, `pyproject.toml` (remove `fakeredis`; the per-file ignore for `hipaa.py` shrinks, never grows)
- Delete: `services/intelligence/intelligence/queue/` (the stubs)
- Rewrite: `tests/security/test_feedback_review.py` and `tests/security/test_patient_scoping.py` against `DoctorFeedbackLoop`, the orchestrator and the boundary (departure 12); update `tests/unit/test_security.py`, `tests/unit/test_imports.py`, `tests/regression/e2e_regression.py`
- Modify: `docker-compose.yml` (remove `redis`, `redisdata`, `REDIS_URL` and `QUEUE_*`), `Makefile` (`infra` no longer starts redis), `.env.example`, `services/core/cmd/core/compose_test.go`

- [ ] **Step 1: Write the failing tests.**
  - `test_no_python_rbac_or_audit` (unit): importing `RBACManager`, `AuditLogger` or `SecurePipelineWrapper` from `intelligence.security.hipaa` fails, and nothing in `intelligence/` names them.
  - The rewritten patient-scoping tests keep every assertion about what retrieval and citation checks may see; the rewritten feedback-review tests keep the two-person rule (a correction applies only after a second clinician approves).
  - `TestComposeHasNoRedis`.
- [ ] **Step 2: Run (Python, remote):** `uv run pytest -q -m "not models"`; the new tests fail. **Run (Go, remote):** `go test ./cmd/core/...`.
- [ ] **Step 3:** Remove the code, rewrite the tests, update compose.
- [ ] **Step 4:** Run the full Python suite and lint, format and mypy; the Go compose tests. All pass.
- [ ] **Step 5:** Commit: "Retire the Python RBAC, audit log and queue stubs now that core owns them".

**Security checklist**
- [ ] No permission or tenancy rule is left in Python.
- [ ] PII masking stays in the pipeline and its tests stay green.
- [ ] The patient-scoping and two-person-review regression tests still assert the same behaviour.
- [ ] Redis is gone from compose and from every env list.

### Task 12: Observability

**Files:**
- Create: `services/core/internal/telemetry/{telemetry.go,carrier.go,allowlist.go}`, tests beside them
- Create: `services/intelligence/intelligence/boundary/telemetry.py`, `services/intelligence/tests/boundary/test_telemetry.py`
- Modify: `internal/intel/client.go`, `jobs.go` (spans and headers), `worker.py`, `run_query.py` (spans and metrics), `services/core/go.mod`, `pyproject.toml`, `services/intelligence/configs/otel-collector.yaml` (receives from both)

**Interfaces:**
- Go: `telemetry.Setup(ctx, endpoint) (shutdown func(context.Context) error, error)`; `telemetry.Header(nats.Header)` implementing `propagation.TextMapCarrier`; `telemetry.AllowList` span processor that drops every attribute not in `request_id`, `hospital_id`, `query_id`, `operation`, `status`, `attempt`, `model`, `model_version`, `timing_ms`; `baggage` is never propagated.
- Python: the same allow-list as a `SpanProcessor`; `inject(headers)` / `extract(headers)`; a malformed `traceparent` starts a new trace.
- Spans: core `intelligence.request <service>.<op>` (client) and `intelligence.publish <type>` (producer); worker `boundary.verify`, `handle <op>`, and one child per stage. Jobs link to the producer span.
- Metrics: `boundary_refusals_total{service,reason}`, `boundary_verify_seconds`, `intelligence_request_seconds{operation}` (p50/p95/p99 from the histogram), `jobs_queue_depth{type}`, `jobs_oldest_wait_seconds{type}`, `jobs_dead_letters_total{type}`, `nats_reconnects_total`. Core's NATS reconnect and disconnect handlers log and flip readiness (closes TODO T3 M6).

- [ ] **Step 1: Write the failing tests.**
  - Python `test_marker_never_in_logs_spans_or_dead_letters` (A14, `boundary`): a question holding `SYNTH-MARKER-7f3a` runs through a failing and a succeeding job with an in-memory span exporter and `caplog`; the marker is in no span attribute or event, no log line and no dead-letter message.
  - Go `TestMarkerNotInSpansOrLogs` (A14, core side): the same marker through `POST /queries` and the client, with an in-memory exporter and a captured `slog` handler.
  - `test_malformed_traceparent_starts_new_trace` and Go `TestBaggageDropped`.
  - `test_refusal_metric_increments` and Go `TestReconnectFlipsReadiness`.
- [ ] **Step 2: Run** both sides remotely → fail.
- [ ] **Step 3:** Implement setup, carriers, allow-lists, spans and metrics.
- [ ] **Step 4:** Run both sides' full suites; they pass.
- [ ] **Step 5:** Commit: "Trace calls across the boundary with an attribute allow-list, and count refusals".

**Security checklist**
- [ ] Spans carry only allow-listed attributes; never prompt text, answers, patient fields or tokens.
- [ ] Trace headers are untrusted; `baggage` dropped on both sides.
- [ ] Refusals the user never sees still alert, through `boundary_refusals_total`.

### Task 13: Compose, CI, the end-to-end smoke test and docs

**Files:**
- Modify: `docker-compose.yml`:
  - `core-keys` also runs `keys ensure --kind request --out /keys/request.pem --trusted-out /trusted/request-keys.json`; a new `trustedkeys` volume, mounted read-only by the workers;
  - `pipeline` (from the intelligence image, `intelligence worker --service pipeline`, CPU, Ollama as today, the `intelligence_app` login, its NATS password, the trusted keys) and `echo` (profile `test`) replace `intelligence`;
  - `core` gets `REQUEST_KEY_PATH` and `INTELLIGENCE_QUERY_CAP`.
- Modify: `.env.example` (`INTELLIGENCE_DB_USER`, `INTELLIGENCE_DB_PASSWORD`, `NATS_PIPELINE_PASSWORD`, `NATS_ECHO_PASSWORD`, `NATS_JS_KEY`), `services/core/cmd/core/compose_test.go`
- Create: `services/core/internal/smoke/smoke_test.go` (build tag `smoke`), `docker-compose.smoke.yml` (stub models: `INTELLIGENCE_MODEL_CLIENT=stub`, `echo` on)
- Modify: `.github/workflows/ci.yml`:
  - `checks` runs `uv run pytest -m "not models and not boundary"`;
  - new job `intelligence-boundary`: builds `mhai-core:test`, then `uv run pytest -m boundary`, with Docker;
  - new job `boundary-smoke`, after `image`: compose up with `core`, `nats`, `postgres`, `pipeline` and `echo`, then `go test -tags smoke ./internal/smoke/...`, then compose down.
- Modify: `README.md`, `ARCHITECTURE.md`, `CHANGELOG.md`, `TODO.md`, `services/core/README.md`, `services/intelligence/README.md`, `services/nats/README.md`, `AGENTS.md` (layout: `intelligence/boundary/`; `security/` keeps PII masking only), `docs/specs/2026-10-17-frontend-requirements.md` (mark `POST /queries`, `GET /queries/{id}` and `/result` **Built (Plan 3)**, and 503 `unavailable` as used), `docs/specs/2026-10-27-intelligence-performance-design.md` (`mhai/models/` → `services/intelligence/intelligence/models/`, TODO final m7)

- [ ] **Step 1: Write the failing smoke test** `TestQuestionEndToEnd`: sign in as a synthetic clinician of synthetic hospital `HAV` (seeded with the CLI), `POST /queries` with the BFF key, poll `GET /queries/{id}` until `completed` (60 s limit), `GET /queries/{id}/result` has `decision`, and `audit.Verify` passes with a `query.completed` row for that query. Also `TestEchoNeedsToken`: a raw NATS request to `inference.v1.echo.ping` with no token gets `unauthorized`.
- [ ] **Step 2: Run (remote):** the smoke job's commands with the compose files: `docker compose -f docker-compose.yml -f docker-compose.smoke.yml up -d --build --wait` then `go test -tags smoke ./internal/smoke/...` from `services/core` → fails before the compose changes.
- [ ] **Step 3:** Make the compose, CI and doc changes. TODO.md: remove the boundary items this plan finished (the boundary plan entry, T2 M6, T3 M3, T3 M6, T3 N1, T3 N4, T3 N5, final m7), and add what it leaves: patient ids and `pids` with the EMR plan; platform retry and cancel endpoints (frontend §14.8); the shared replay check when a request/reply operation first writes; a separate NATS account before production if NATS is shared.
- [ ] **Step 4:** Run the smoke test remotely; it passes. Record the transcript (no secrets) in the PR. Then the full Go suite and the full Python suite remotely, and confirm on GitHub Actions.
- [ ] **Step 5:** Commit: "Run the pipeline and echo workers in compose, add the boundary CI jobs and the end-to-end smoke test".

**Security checklist**
- [ ] Each container sees only its own secrets (`TestComposeSplitsSecretsPerService`); workers get the trusted public keys read-only and never the private key.
- [ ] `echo` runs only under the `test` profile.
- [ ] The smoke test uses synthetic accounts and questions only.
- [ ] No secret in the PR transcript.

---

## Abuse cases → tests

Every abuse case in spec §13.5 is a named test, written before the code it proves.

| Case | Test (task) |
|---|---|
| A1 no token | `test_missing_token_refused` (6); `test_echo_without_token_gets_unauthorized_and_handler_never_runs` (7) |
| A2 worker publishes requests or jobs | `TestServiceUserCannotPublishRequestsOrJobs` (4) |
| A3 replayed request | `test_replayed_jti_refused` (6); `test_replayed_request_refused` (7) |
| A4 redelivery after completion | `test_redelivery_after_completion_acks_without_second_decision` (9) |
| A5 token for another operation | `test_token_for_other_operation_refused` (6) |
| A6 access token as request token | `TestAccessTokenRefusedAsRequestToken`, `TestRequestTokenRefusedAsAccessToken` (2); `test_access_token_refused` (6) |
| A7 body changed | `TestBodyHashBinding` (2); `test_body_byte_flip_refused` (6) |
| A8 body names another hospital | `test_body_hospital_mismatch_refused` (6) |
| A9 another hospital's `query_id` | `test_with_hospital_cannot_see_other_hospital_query` (8); `test_foreign_query_id_is_not_found_and_reads_nothing` (9) |
| A10 `alg`, `kid`, lifetime | `TestRequestTokenRejectsAlgAndKidAndLifetime` (2); `test_alg_none_hs256_unknown_kid_and_long_exp_refused` (6) |
| A11 worker writes shared or foreign `kb_chunks` | `test_worker_cannot_write_kb_chunks` (8) |
| A12 forged audit identity | `TestDrainerIgnoresFactIdentity`, `TestReplyFactsUseTokenIdentity` (10) |
| A13 crash after commit | `test_completion_commit_writes_outbox_atomically` (9); `TestDrainerAppendsCompletedAfterWorkerCrash` (10) |
| A14 PHI marker | `test_marker_never_in_logs_spans_or_dead_letters`, `TestMarkerNotInSpansOrLogs` (12) |
| A15 foreign reply subject | `test_foreign_reply_subject_refused_silently` (6); `test_reply_to_foreign_inbox_gets_no_reply` (7) |
| A16 worker subscribes elsewhere | `TestServiceUserCannotSubscribeOthers` (4) |
| A17 refusals look the same | `TestRefusalsLookTheSame` (5) |
| A18 oversize bodies | `TestMaxPayloadRefusesTwoMB` (4); `test_oversized_request_body_refused` (7) |
| A19 per-hospital cap | `TestInFlightCapPerHospital` (5); `TestPostQueriesAtCap429` (10) |
| A20 poison job | `test_poison_job_dead_letters_after_five_deliveries` (9) |
| A21 deactivation cancels | `test_cancelled_between_stages_stops` (9); `TestDeactivationCancelsQueuedJobs` (10) |
| A22 worker login holds nothing | `TestIntelligenceLoginHoldsNothing` (8, Go); `test_login_outside_role_fails` (8, Python) |
| A23 Go and Python rules drift | `TestRouteRolesSubsetOfOperation`, `TestOperationsJSONUpToDate` (3) |
| A24 emergency rotation | `test_reload_drops_removed_kid` (6); `TestEmergencyRotationReMintsDeadLetteredJob` (10) |
| A25 shared token vectors | `TestRequestTokenVectorsUpToDate` (2); `test_shared_token_vectors` (6) |

## Out of this plan (next plans)

- **The GPU work** (vLLM, ONNX, batching, FP8, the per-hospital cache salt) and **streaming** (`stream.v1.>`, the `strm` claim): the parked performance design, with clinical sign-off for streaming.
- **Model services** (`nli`, `embed`, `mask`, `dia`, `qda`) as their own request/reply workers. The subjects, tokens and NATS users here already fit them.
- **Patient ids** in queries and tokens (`pids`), with the EMR plan (departure 7).
- **Platform job views:** retry and cancel endpoints and the Failed jobs list (frontend §14.8). This plan builds the dead-letter consumer and the re-mint they will call.
- **The shared replay check** in NATS KV, when a request/reply operation first writes data (decision 3).
- **A separate NATS account** for intelligence, before production if NATS is shared (decision 10).
- **Feedback and corrections** (`POST /feedback`, frontend §4): a later plan; `kb_chunks` writes come back with it.

## Self-review notes

- **Spec coverage:**

  | Spec section | Task |
  |---|---|
  | §5 subjects and versioning | 1 (schema names), 3 (operations), 4 (permissions) |
  | §6 envelope and schema | 1, with headers in 5 and 6 |
  | §7.1–7.3 request token and checks | 2 (Go), 6 (Python) |
  | §7.4 replay | 6 (request/reply), 9 (jobs) |
  | §7.5 keys and rotation | 2 (CLI, file), 6 (reload), 10 (emergency re-mint), 13 (compose) |
  | §7.6 NATS permissions | 4 |
  | §8.1 operations table | 3 |
  | §8.2 database access | 8, with the repository in 9 |
  | §8.3 stale roles | 10 (results read through core; cancel on deactivation) |
  | §9.1 request/reply | 5 (core), 7 (worker) |
  | §9.2–9.4 jobs, idempotency, cancellation | 5 (publish, sweeper), 9 (claim, complete, fail), 10 (cancel) |
  | §10.1 errors | 5 |
  | §10.2 audit | 8 (outbox, drain role), 10 (drainer) |
  | §10.3 observability | 12 |
  | §11 room for performance | out of plan; nothing here blocks it |
  | §12 local dev and CI | 7 (fixtures), 13 |
  | §13 threat model | every task's checklist; the abuse-case table above |

- **Names across tasks:**
  - `reqtoken.Signer`, `Mint`, `VerifyAt`, `ReadTrusted`, `WriteTrusted`, `Kind`, `SvcCore`
  - `authz.Operation`, `Operations`, `OperationByName`, `Rule.Calls`, `OperationsJSON`
  - `bus.EnsureStreams`, `bus.TLS`
  - `intel.Client`, `Request`, `PublishJob`, `Caller`, `Code`, `ReserveInFlight`, `RunPublishSweeper`, `RunDrainer`, `RunDeadLetters`
  - `db.WithJobs`, `WithAuditDrain`; roles `mhai_jobs`, `mhai_audit_drain`, `mhai_worker_app`
  - `httpx.ErrorRetryAfter`; code `unavailable`
  - Python: `TrustedKeys`, `JtiSet`, `Operations`, `Verifier`, `Verified`, `Refusal`, `Worker`, `Database.with_hospital`, `ModelClient`, `run_query.handle`

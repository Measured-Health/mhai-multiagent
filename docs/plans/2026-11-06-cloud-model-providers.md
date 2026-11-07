# Cloud model providers (OpenAI, Anthropic, xAI): implementation plan

**Status:** Not started. The design is approved (product owner, 3 Nov 2026, "approve both as recommended"; all six questions in its §16 as recommended). The gate before real patient data (design §13.6) is a human gate, tracked in TODO.md. Nothing in this plan opens it.

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task by task. Steps use checkbox (`- [ ]`) syntax for tracking. Read the `secure-design` and `secure-build` skills (`.claude/skills/`) before Task 1, and run `security-review` on your own diff before each PR.

**Goal:** Let a hospital that a platform admin has opted in get its clinical answers drafted by a cloud model (Anthropic, OpenAI or xAI). Only masked text and shared guideline text leave the network, through a typed gate and a scan of the exact request bytes, failing closed. Retrieval and verification stay local, and every cloud failure escalates. The route is off by default, decided by core and read by the worker from the claimed row. It is recorded on every answer and on the audit chain. Everything is built and tested with fake adapters and synthetic fixtures. A real provider is called only by a manual smoke test.

**Architecture:**

- **Core (Go):** migration `00018_model_policy.sql` adds a platform-only `hospital_model_policy` table and `clinical_queries.model_route`, set by a `BEFORE INSERT` trigger that overwrites anything the insert supplied. It also adds a per-hospital monthly token ledger, `cloud_usage`, which the worker reaches only through two `SECURITY DEFINER` functions. There are two platform routes and a CLI twin to set the policy. The audit action `hospital.model_policy_changed` gets a closed schema. `query.completed` gains two optional closed keys, `egress` and `cloud_outcome`, and the clinical result gains `generated_by`.
- **Worker (Python):** a new package `intelligence/boundary/cloud/` holds the settings and API-key files, the gate types (`MaskedText`, `CloudContext`, `SharedChunk`, `CloudPrompt`), the egress scan, the budget port, reply validation, `CloudModelClient` and one adapter per provider. `ModelRouter` (in `boundary/models.py`) gives `run_query` the client for the claimed row's route, and that one client runs every stage of the job.
- **Deploy:** an `egress-proxy` container on a new `egress` network. It allows only `CONNECT` to port 443 of the enabled providers' API hosts. The pipeline stays on the internal `backend` network.

**Tech stack:** Go 1.24 (`pgx/v5`, `goose/v3`, `testcontainers-go`), as in Plan 4. Python 3.12 with uv. New runtime dependencies: `openai` and `anthropic`, the official SDKs, versions pinned by `uv.lock`. Both SDKs (openai 3.x, anthropic 2.x) are built on `httpx2` (with `httpcore2` and `truststore`), not `httpx`: every transport, client, request and hook in Tasks 6, 9 and 11–13 is an `httpx2` one (`httpx2.Client`, `httpx2.HTTPTransport`, `httpx2.MockTransport`, `httpx2.BaseTransport`, `httpx2.Request`, `httpx2.Timeout`; checked in httpx2 2.16.0). An `httpx` object is refused or ignored by the SDKs. `pyproject.toml` caps both SDKs below their next major (Task 4 fix round). New dev dependencies: `hypothesis` and `pytest-socket`. The egress proxy is Squid (`ubuntu/squid`), pinned by digest.

**Spec:** `docs/specs/2026-11-03-cloud-model-providers-design.md` (approved 3 Nov 2026) and [ADR 0013](../adr/0013-allow-masked-text-to-cloud-language-models.md). Section numbers below (§n) are the design's. It builds on the intelligence boundary design as built (`docs/specs/2026-10-28-intelligence-boundary-design.md` §18), Plan 3, the Plan 4 design (masked consultation §4.4; `mhai_platform`; the typed audit registry), and ADRs 0004, 0005, 0010 and 0012.

**PRs:** five, each mergeable on its own, in this order (the migration chain is linear and the worker's claim reads the new column):

| PR | Tasks | What | Size |
|---|---|---|---|
| **A: core** | 1–3 | migration 00018, policy routes, CLI, audit schema, fact schema, `generated_by` | Go only |
| **B: the gate** | 4–7 | worker settings and key files, the gate types, the egress scan, the budget port | Python, no network code |
| **C: the client and the job** | 8–10 | reply validation and retries, `CloudModelClient`, `ModelRouter`, `run_query` on both paths | Python |
| **D: adapters** | 11–13 | Anthropic, OpenAI, xAI, against synthetic fixtures | Python |
| **E: deployment, smoke and docs** | 14–16 | egress proxy, the manual live smoke, docs, TODO gates | compose, docs |

The mhai-web screens (the hospital model-policy page in the platform app; `generated_by` on the clinical result) are a separate PR in mhai-web, after PR A (design §17 item 10).

## Global Constraints

Plan 2, 3 and 4's constraints still hold: identity only from the verified token; 404 for another hospital's resource; problem bodies carry a fixed code only; no PHI or secrets in logs, spans, audit payloads, dead letters or NATS messages; synthetic data only; plain commit messages with no AI attribution; remote runs only. In addition:

- **Off by default.** No `hospital_model_policy` row means `local`. `CORE_CLOUD_PROVIDERS` and `INTELLIGENCE_CLOUD_PROVIDERS` are empty by default. A fresh deployment sends nothing anywhere.
- **The real-data gate is not code.** No setting, flag, migration or CLI command in this plan represents "real data allowed". The masker recall bar (names ≥ 0.99, numeric identifiers ≥ 0.999, privacy lead's sign-off; it includes names without a title, ruling C8), the legal check (hospital data agreements, NDPA 2023, provider DPAs and zero-retention terms), the clinical evaluation and core's keyed name-token hashes (ruling C5) are TODO.md items owned by people (Task 16). An agent never edits them to "done".
- **Only masked text leaves.** Adapters accept a `CloudPrompt` and nothing else (runtime type check plus mypy). `CloudPrompt` accepts only a tagged `MaskedText` question, a `CloudContext` (two allow-listed fields) and sealed `SharedChunk`s. The scan runs on the exact request bytes before they reach the transport. Any refusal means nothing is sent, the outcome is `egress_blocked`, and the job escalates.
- **Every cloud failure escalates.** `Generation(available=False, failed_agents=("CCA",))`, decision `ESCALATE`, `verified=False`, no trust score, `degraded=True`, the outcome code recorded (§11). There is no fallback to local or to another provider. The one exception: a policy switched off before the claim runs local (§5.2).
- **Retrieval and verification stay local.** The cloud client never calls `RAGClinicalContextAgent.generate_with_rag` and never sends anything to verify.
- **Documents never go.** `ingest_document` and `finalise_document` never get a `ModelRouter`, and no adapter can emit an image, file, document or audio block.
- **Keys:** a key is read from a file (`INTELLIGENCE_CLOUD_<PROVIDER>_KEY_FILE`), mode 0400 or 0440, on the pipeline host only. Keys are never in environment variables, core, the BFF, the database, the repository, CI, logs, spans, exceptions or fixtures. In memory they are held as `ApiKey`, whose `repr` and `str` are redacted.
- **CI never calls a provider.** Adapter tests use `httpx2.MockTransport` with synthetic fixtures. Every unit module in `tests/boundary/cloud/` (settings, key, gate, scan, reply, retry, client, the three adapters, the smoke guard) sets `pytestmark = pytest.mark.disable_socket`, so `pytest-socket` refuses any connection there. The `boundary`-marked modules (`test_budget`, `test_router`, `test_property`) need Postgres and NATS through testcontainers, so they don't set it. A meta-test (`test_unit_modules_disable_sockets`, Task 4) fails if a module without the `boundary` marker lacks the socket mark. The live smoke script (Task 15) is outside `testpaths` and is never collected. It refuses to run without `MHAI_CLOUD_SMOKE=1`, inside a container, or in CI. It reads the key from standard input, never from a file, and sends synthetic data only. A person runs it on their own machine, **never on the shared build server** (steven-reid or mt5-lab), with a separate, short-lived, spend-capped smoke key that is revoked afterwards (Task 15, H7).
- **Security rules are written once, in Go/SQL.** The route is decided by core's trigger from core's table. The budget rule is a core-owned SQL function. Python never reads a hospital, route or budget from a payload. The worker's hospital is the verified token's (`with_hospital`).
- **Fixed network targets.** Base URLs are constants in code: `https://api.anthropic.com`, `https://api.openai.com/v1`, `https://api.x.ai/v1`. An override (`INTELLIGENCE_CLOUD_<PROVIDER>_BASE_URL`) is refused outside `APP_ENV=development`. The worker never follows redirects, ignores environment proxies (`trust_env=False`) and uses the proxy named in its settings, with TLS verification on and no custom CA.
- **No per-request state on shared objects.** The identifiers, the budget port and the deadline reach `generate()` as one argument (`call: CloudCall`). The client holds only the provider's settings and the shared HTTP transport.
- **Closed vocabularies.** Providers: `openai`, `anthropic`, `xai`. Routes: `local`, `cloud:openai`, `cloud:anthropic`, `cloud:xai`. Egress: `none` plus the providers. Outcomes (§11): `ok`, `egress_blocked`, `budget_exhausted`, `unconfigured`, `unavailable`, `timeout`, `auth_failed`, `refused`, `truncated`, `malformed`, `insufficient`, `retention_unconfirmed`. Python and Go hold the same lists, and a test on each side pins them.
- **No baselines.** Nothing is added to the ruff, format or mypy baselines; `intelligence/boundary/cloud/` is mypy-strict.

## Review Focus

1. **The scan refuses so much that it's useless.** If the pattern rules run over guideline text, a dose like "200000 IU" or a guideline's date escalates every question that retrieves it, and it looks like a provider outage. If digits are joined across the whole body, `KB#0001` and `1500` can form a 6-digit MRN by accident. If short names are matched without word boundaries, "Ada" matches "adaptive". If digits are joined across spaces, "T 38.5 101.3" becomes `3851013` and "platelets 250 000" becomes `250000`. Expected: the pattern rules run only on blocks 0 and 1 (the question and the context), which sit at fixed positions in every provider's layout. Digit runs join across a short gap only where a phone or NIN shape needs it, or where a hyphen chain has 3 or more groups (Task 6). Name tokens are matched on word boundaries. A fixed clean record and typical vitals questions are **sent**. Tests: `test_guideline_doses_and_dates_pass`, `test_no_digit_join_across_values`, `test_short_name_token_needs_word_boundary`, `test_clean_record_is_sent`, `test_typical_clinical_questions_are_sent`, `test_patient_paths_are_exact_for_each_layout` (Task 6), and the property test's clean-record assertion (Task 10).
2. **An SDK wraps the scan's refusal.** The scan runs as an `httpx2` request hook. Both SDKs catch transport exceptions and re-raise them as `APIConnectionError`, which would map to `unavailable` and be retried. Expected: the per-call scanner's own verdict wins, the outcome is `egress_blocked`, and there is no second attempt. Test: `test_hook_refusal_is_egress_blocked_not_unavailable_and_not_retried` (Task 9, and each adapter's suite).
3. **Identifiers hidden by JSON escaping or Unicode.** In the request bytes, a name split by a newline is `Adaeze\nOkafor` (a backslash and an `n`), and with `ensure_ascii` a zero-width space is `​`. Scanning raw bytes misses both. Expected: the body is parsed as JSON, and every string value is decoded before it is normalised. Normalisation applies NFKC, removes `Cf` characters, case-folds, and for the identifier rule turns whitespace and punctuation into single spaces. Full-width digits, mixed case and a name split by `\n`, `\t` or `​` are all still caught. Test: `test_escaped_and_split_names_blocked_through_sdk_encoder` and `test_scan_sees_through_zero_width_fullwidth_and_case` (Task 6), with bodies made by each SDK's own encoder in Tasks 11–13.
4. **Budget reservations that are never settled.** A deadline, an abandoned stage thread or an exception between reserve and settle leaves tokens "reserved" until the month ends, so a hospital slowly runs out. Expected: settle runs in `finally` on every path the process survives; an unknown reply settles as fully used (§10). Three cases still leave a reservation, each bounded by one attempt's estimate: a crashed process; a `reserve` that timed out in Python after core committed; and a `settle` that failed (logged by ids and counted in `mhai_cloud_settle_failures_total`). Until core's reaper exists (Task 16 TODO), each holds tokens until the month ends, which fails toward sending less, never more. Test: `test_reservation_settled_on_every_failure_path` (Task 9).
5. **Policy switched to a different provider between insert and claim.** A row inserted as `cloud:openai` whose hospital is now on `anthropic` must not go to either: the design runs it local (the provider no longer matches). Test: `test_claim_runs_local_when_provider_changed` (Task 1, SQL) and `test_route_changed_between_publish_and_claim_runs_local` (Task 10, job).

## Where this plan departs from the design

1. **The migration is 00018.** The design names 00015 ("Plan 4c may take 00015"). On `main`, 00015–00017 are taken (`00015_health_functions`, `00016_platform_jobs`, `00017_guardrail_counts`). The build takes the next free number when it starts. If another PR merges a migration first, the file is renumbered and the tests' references with it.
2. **The worker reaches the budget only through functions, by reservation id.** The design grants `mhai_worker` `SELECT, INSERT, UPDATE (used, reserved)` on `cloud_usage` and `SELECT (hospital_id, cloud_provider)` on the policy. But the reservation needs `monthly_token_budget`, which that grant doesn't give. This plan adds `mhai_cloud_reserve(p_provider text, p_tokens bigint)` (provider added by ruling C1) and `mhai_cloud_settle(p_reservation uuid, p_used bigint)`. Both are `SECURITY DEFINER`, owned by `mhai_migrate`, with `search_path = pg_catalog, pg_temp` and schema-qualified names. `reserve` runs the design's single atomic `UPDATE … WHERE used + reserved + n <= budget` for `mhai_current_hospital()`, with the month computed in the database (UTC). It also records a row in `cloud_reservations` and returns that row's id. `settle` deletes that reservation row (`DELETE … RETURNING`) and adjusts the ledger from it, so a second settle of the same id, a redelivery, or another job's id does nothing. `mhai_worker` gets `EXECUTE` on the two functions and no grant on either table. The rule stays in core's SQL, and the worker can't read another hospital's budget or release a reservation it doesn't hold.
3. **The route trigger is `SECURITY DEFINER`, and `mhai_gateway` gets no grant on the policy.** The design grants the gateway `SELECT` so its trigger can read the policy and hospital admins can see it. A definer trigger sets the route whoever inserts. It follows the existing definers (00003, 00006, 00007, 00009): owner `mhai_migrate`, `SET search_path = pg_catalog, pg_temp`, every relation written `public.<name>`, and a `FOR SELECT TO mhai_migrate` policy under `FORCE`. With that pattern, a temporary table can't shadow the policy table. No hospital-side route shows the policy in this plan, so the grant would be unused. A hospital-admin view, when wanted, is a later additive route.
4. **The audit policies are widened.** `mhai_platform` may append only 00014's eight actions (00014:131-134), with `hospital_id IS NULL`. `mhai_provision` may append only 00010's four, with `actor_id IS NULL`. The migration adds `hospital.model_policy_changed` to both, with the same conditions, and Down restores 00014's and 00010's exact lists. The audit row's `hospital_id` stays NULL (a platform action), and the hospital goes in the payload's `hospital_id`, as the design's schema has it.
5. **Step-up follows the platform's pattern.** `PUT /platform/hospitals/{id}/model-policy` takes `mfa_code` or `backup_code` in the body and checks it with `auth.Service.StepUpAudited`, as `POST /platform/jobs/cancel` does (`platform/jobs.go:608`). A failed code is audited.
6. **The CLI takes the hospital as an argument:** `core hospitals model-policy set <hospital id or short code> --provider anthropic|openai|xai|none --agreement-ref <ref> [--budget <tokens>]`. This matches `core hospitals activate <hospital id or short code>`. The design's `--hospital <id>` is a flag.
7. **`APP_ENV=demo` is new.** The worker knows only `development` and everything else (production). This plan adds `demo`, which is production in every respect but one: `INTELLIGENCE_CLOUD_<PROVIDER>_ZDR_REF=DEMO-SYNTHETIC` is accepted, with a warning at start. Production refuses that value. Core needs no change: it never reads ZDR references.
8. **The reviewer's masked copy is converted to plain strings.** The design makes `MaskedText` impossible to copy, but `run_query._pipeline` deep-copies the masked context for the reviewer (`copy.deepcopy(context)`). The copy is now made with `_plain_tree`, which turns every `MaskedText` into a plain `str`. It is never passed to a client, and a plain `str` can't reach an adapter.
9. **Known identifiers are collected by a second, local masker pass.** `ModelClient.mask` returns the masked text only, and the shared client can't keep per-job detections. `cloud.scan.collect_identifiers(question, patient)` runs its own `PIIMasker` (a fresh instance per call) over the raw question and every raw string, keeping each detection's `original_text`. It also takes the values under the identifying keys. The canonical model's FHIR keys (§4.4) don't exist in this repository yet (the EMR service isn't built), so the key list is the design's plain keys plus `identifier`, `telecom`, `birthDate`, `name.given`, `name.family`; Task 6 pins it.
10. **Retrieval uses `HybridRetriever.retrieve`, not `build_context`.** `build_context` returns one formatted string, and the gate needs the chunk objects. The client calls `orch.rag_cca.retriever.retrieve(question, top_k=5, patient_id=None)` on the per-job orchestrator that `default_orchestrator`'s builder makes, and maps each result to its `SharedChunk`.
11. **Adapters are a request builder and a reply parser, behind one transport.** The design's `send(prompt, deadline) -> CloudReply` is kept as the client's internal call. Each adapter provides `call(sdk_client, prompt) -> CloudReply`, using its SDK. The SDK client is built per generation over one shared `httpx2.HTTPTransport` (proxy, an explicit TLS context, `trust_env=False`) with a per-call scan hook, so the scan sees the exact bytes and holds that job's identifiers without shared state.
12. **The first fixtures are hand-made from the providers' documented shapes.** The design records fixtures with the live smoke test. That needs a key (H1), and PR D must not wait for it. Tasks 11–13 commit synthetic fixtures under `tests/fixtures/cloud/<provider>/`. Task 15 adds the recorded, scrubbed ones next to them, and they must pass the same parser tests.
13. **Semantic entropy is unchanged.** The boundary never passes `semantic_entropy` to `hga.verify`, on either path today, so the design's "not used on the cloud path" needs no code. A test pins that the cloud client never calls the CCA's generator.
14. **The effective route is a core function, and the worker loses its grant on the policy.** The design re-checks the route in the worker's claim SQL against a column grant on the policy. That puts the rule in Python-owned SQL. Instead, migration 00018 adds `mhai_effective_model_route(p_hospital uuid, p_route text) RETURNS text`, a `SECURITY DEFINER` function in core. It returns `p_route` only when `p_hospital = mhai_current_hospital()` and the hospital's policy still names that route's provider, and `'local'` otherwise. The worker's claim calls it, and `mhai_worker` gets `EXECUTE` on it and no grant on `hospital_model_policy`. Go's tests exercise this same function.
15. **`generated_by` comes from the decision, not the inserted row.** The design reads it as the route chosen at insert. After a policy switched off (A14) or an `unconfigured` route, that would show "cloud: anthropic" on a local answer. Instead, core derives `generated_by` from the decision's `model_versions`: `cca_route` gives the route, and the provider is the `cca` value's prefix before `/`, kept only if it is in the provider set. A decision with no `cca_route` (written by an older worker, or a cloud failure that produced no draft) has no `generated_by`.

---

## Running the tests

Never on this machine. Go tests, testcontainers, Python tests, Docker builds and compose all run on the steven-reid build server (`steven-reid@75.46.59.136`), with mt5-lab as the fallback (`REMOTE_HOST`/`REMOTE_KEY`; the runner then caps the job at 2 CPUs and 8 GB and runs one job at a time). GitHub Actions runs the full gate as confirmation.

The runner is `/home/ose/Documents/Pehtrah/MH/mhai-platform/.remote-run-go.sh`. It rsyncs the checkout (without `.venv`, `.superpowers`, `__pycache__` or any `data` directory) to `~/claude-work/<DEST>/`, and runs the commands from stdin in `/work` inside `golang:1.24` with the host's Docker socket. Put each step on its own line ending `|| exit 1`, and wrap the run in `timeout`:

```bash
SRC=<your worktree>/ DEST=mhai-cloud \
  timeout 2400 /home/ose/Documents/Pehtrah/MH/mhai-platform/.remote-run-go.sh <<'CMDS'
cd services/core || exit 1
go vet ./... || exit 1
go test -race ./internal/intel/... ./internal/audit/... ./internal/platform/... ./internal/queries/... ./internal/config/... ./cmd/core/... || exit 1
go test -race -tags integration ./internal/db/... ./internal/platform/... ./internal/queries/... ./cmd/core/... || exit 1
CMDS
```

For Python, in the same container:

```bash
export UV_CACHE_DIR=/go/pkg/mod/cache/uv UV_PYTHON_INSTALL_DIR=/go/pkg/mod/cache/uv-python
curl -LsSf https://astral.sh/uv/0.12.23/install.sh | sh || exit 1
export PATH=/root/.local/bin:$PATH
cd services/intelligence || exit 1
uv sync --locked || exit 1
uv run pytest -q tests/boundary/cloud tests/security -m "not boundary" || exit 1
uv run pytest -q -m boundary tests/boundary/test_run_query.py tests/boundary/cloud || exit 1
uv run ruff check . && uv run ruff format --check . && uv run mypy || exit 1
```

"**Run (Go, remote):** `<cmd>`" means put `<cmd>` after `cd services/core`. "**Run (Python, remote):** `<cmd>`" means put it after `uv sync --locked`. `boundary` tests start Postgres (migrated by core) and NATS with testcontainers. None of them loads a model or opens a connection outside the container network.

---

## File structure

```
services/core/
  internal/db/migrations/00018_model_policy.sql   policy table, model_route + trigger, mhai_effective_model_route,
                                                   cloud_usage + cloud_reservations + reserve/settle/usage
                                                   functions, grants, RLS, the two audit policies widened
  internal/db/model_policy_integration_test.go    A13 (database), A11 (SQL race), A14 (SQL), grants
  internal/db/db_integration_test.go              + the two tables in TestRLSWithoutHospitalSeesNothing
  internal/config/config.go (+ _test)             CORE_CLOUD_PROVIDERS
  internal/audit/schemas.go (+ _test)             hospital.model_policy_changed
  internal/platform/modelpolicy.go                GET/PUT /platform/hospitals/{id}/model-policy
  internal/platform/modelpolicy_test.go, modelpolicy_integration_test.go
  internal/platform/audit.go                      Register calls registerModelPolicy; Deps.CloudProviders
  internal/server/server.go                       passes CloudProviders
  cmd/core/seed.go, modelpolicy_test.go, modelpolicy_integration_test.go   the CLI twin
  internal/intel/factschema.go (+ _test)          egress, cloud_outcome on query.completed
  internal/queries/shapes.go, handlers.go (+ tests)   generated_by on the result
  openapi.yaml                                    1.9.0 → 1.10.0: two routes, generated_by
docs/specs/generated/permissions.md               regenerated (go run ./cmd/permgen)
services/intelligence/
  pyproject.toml, uv.lock                         openai, anthropic; dev hypothesis, pytest-socket
  intelligence/boundary/cloud/
    __init__.py
    vocab.py            PROVIDERS, ROUTES, EGRESS, OUTCOMES, BASE_URLS, HOSTS
    apikey.py           ApiKey (redacted), load_api_key, RedactingFilter, install_redaction
    settings.py         ProviderSettings, CloudSettings.from_env
    gate.py             MaskedText, TaggingMasker, SharedChunk, seal_shared_chunk, CloudContext,
                        CloudPrompt, SYSTEM_PROMPT, EgressRefused
    scan.py             Identifiers, collect_identifiers, normalise, scan_body, ScanHook
    budget.py           BudgetPort, Reservation, estimate_tokens, DbBudget (calls cloud_repo; no SQL here)
    reply.py            ANSWER_SCHEMA, Answer, validate_answer, Malformed
    retry.py            RetryPolicy, classify, backoff
    client.py           CloudCall, CloudReply, Adapter (Protocol), CloudModelClient, build_cloud_clients
    anthropic_adapter.py, openai_adapter.py, xai_adapter.py
  intelligence/boundary/config.py                 WorkerSettings.cloud; is_demo
  intelligence/boundary/models.py                 Generation (versions, egress, cloud_outcome, tokens);
                                                  ModelClient.generate(..., call=None); ModelRouter;
                                                  build_model_router; _verify_with (shared by both clients)
  intelligence/boundary/kb_repo.py                shared rows sealed as SharedChunk
  intelligence/boundary/cloud_repo.py             reserve/settle SQL (the functions), on the caller's cursor
  tests/boundary/test_no_direct_db.py             REPOS gains boundary/cloud_repo.py (a security-test change)
  intelligence/boundary/jobs_repo.py              Claim.model_route; claim re-checks the policy;
                                                  Decision.egress, cloud_outcome; the fact payload
  intelligence/boundary/jobs/run_query.py         route after the claim; one client per job; CloudCall;
                                                  _plain_tree for the reviewer copy; outcome and tokens
  intelligence/boundary/services/pipeline.py      job_handlers builds the router
  intelligence/boundary/worker.py                 install_redaction at start; SDK debug logging off
  scripts/cloud_smoke.py                          manual live smoke (never in CI)
  scripts/scrub_cloud_fixture.py                  scrubs a recorded reply into a fixture
  tests/boundary/cloud/                           test_vocab, test_apikey, test_settings, test_gate,
                                                  test_scan, test_budget (boundary), test_reply, test_retry,
                                                  test_client, test_router, test_anthropic, test_openai,
                                                  test_xai, test_imports (AST), test_property (Hypothesis),
                                                  conftest.py (CapturingTransport, fake adapter, socket guard)
  tests/boundary/test_run_query.py                + the suite parametrised over local stub and cloud fake
  tests/boundary/test_models.py                   + router and versions tests
  tests/fixtures/cloud/{anthropic,openai,xai}/*.json   synthetic replies (Task 11–13), recorded (Task 15)
  tests/security/test_cloud_egress.py             A1–A6 regressions in the security suite
deploy/
  compose.yml                                     egress-proxy, the egress network, the pipeline block's cloud settings (commented)
  egress-proxy/squid.conf, allowed-hosts.example  CONNECT 443 to listed hosts only
  egress-proxy/test-egress.sh                     A15, run remotely
  README.md                                       enabling a provider, key rotation, switching off
services/core/cmd/core/compose_test.go            + egress-proxy and pipeline network checks
TODO.md, CHANGELOG.md, ARCHITECTURE.md, services/intelligence/README.md
docs/specs/2026-11-03-cloud-model-providers-design.md   §18 "As built"
```

---

## Tasks

Abuse-case ids: **A1–A18** are the design's (§13.5); **K1–K14** are this plan's, listed under "Abuse cases → tests". Each task's first step writes its abuse tests, and they must fail before the code exists.

### Part A: core (PR A)

### Task 1: Migration 00018: the policy, the route, the ledger

**Files:**
- Create: `services/core/internal/db/migrations/00018_model_policy.sql`, `services/core/internal/db/model_policy_integration_test.go`
- Modify: `services/core/internal/db/db_integration_test.go` (`TestRLSWithoutHospitalSeesNothing` lists `hospital_model_policy` and `cloud_usage`)

**Interfaces (the SQL others rely on):**
- `hospital_model_policy (hospital_id uuid PK REFERENCES hospitals, cloud_provider text NULL CHECK (cloud_provider IN ('openai','anthropic','xai')), agreement_ref text NULL CHECK (agreement_ref ~ '^(DEMO-SYNTHETIC|(AGR|DPA)-[0-9]{4}-[0-9]{2,8})$'), monthly_token_budget bigint NOT NULL DEFAULT 2000000 CHECK (monthly_token_budget > 0), updated_by uuid NULL, updated_at timestamptz NOT NULL DEFAULT now(), CHECK (cloud_provider IS NULL OR agreement_ref IS NOT NULL))`. `ENABLE` and `FORCE` RLS, owner `mhai_migrate`. Grants: `mhai_platform` `SELECT, INSERT, UPDATE` (policy `USING (true)`). `mhai_provision` `SELECT, INSERT, UPDATE` (the CLI). A `FOR SELECT TO mhai_migrate` policy for the definer functions. Nothing for `mhai_gateway` or `mhai_worker` (departures 3 and 14).
- Every definer function in this migration follows 00003/00007/00009: owner `mhai_migrate`, `SET search_path = pg_catalog, pg_temp`, every relation and function written `public.<name>`, `REVOKE ALL … FROM PUBLIC`, and `EXECUTE` granted to the one role named.
- `clinical_queries.model_route text NOT NULL DEFAULT 'local' CHECK (model_route IN ('local','cloud:openai','cloud:anthropic','cloud:xai'))`. The trigger is `clinical_queries_model_route BEFORE INSERT FOR EACH ROW EXECUTE FUNCTION mhai_set_model_route()`. The function (definer) sets `NEW.model_route := coalesce('cloud:' || p.cloud_provider, 'local')` from `public.hospital_model_policy` and ignores the supplied value. No role gains `UPDATE (model_route)`. The gateway already has only `UPDATE (published_at)` (00008), and a test pins it.
- `mhai_effective_model_route(p_hospital uuid, p_route text) RETURNS text` (definer, `STABLE`, `EXECUTE` to `mhai_worker`). It returns `p_route` when `p_route <> 'local'`, `p_hospital = public.mhai_current_hospital()`, and the hospital's policy row has `'cloud:' || cloud_provider = p_route`. Otherwise it returns `'local'`.
- `cloud_usage (hospital_id uuid REFERENCES hospitals, month date, used bigint NOT NULL DEFAULT 0 CHECK (used >= 0), reserved bigint NOT NULL DEFAULT 0 CHECK (reserved >= 0), PRIMARY KEY (hospital_id, month))` and `cloud_reservations (id uuid PRIMARY KEY DEFAULT gen_random_uuid(), hospital_id uuid NOT NULL REFERENCES hospitals, month date NOT NULL, tokens bigint NOT NULL CHECK (tokens > 0), created_at timestamptz NOT NULL DEFAULT now())`. Both have RLS on and forced, and no grant to any application role. Under `FORCE`, the owner needs its own policies: `FOR SELECT`, `FOR INSERT` and `FOR UPDATE TO mhai_migrate` on `cloud_usage`, and `FOR SELECT`, `FOR INSERT` and `FOR DELETE TO mhai_migrate` on `cloud_reservations`.
- `mhai_cloud_reserve(p_provider text, p_tokens bigint) RETURNS TABLE (reservation uuid, percent integer)`: definer, `EXECUTE` to `mhai_worker` only. It refuses an unknown `p_provider` (22023), a hospital that is not active, a policy that does not name `p_provider` right now (ruling C1), `p_tokens <= 0` or `> 100000`, and a NULL `mhai_current_hospital()`. It upserts the month's row (`INSERT … ON CONFLICT DO NOTHING`). Then, in one statement, it runs `UPDATE public.cloud_usage SET reserved = reserved + p_tokens WHERE hospital_id = h AND month = m AND used + reserved + p_tokens <= (SELECT monthly_token_budget FROM public.hospital_model_policy WHERE hospital_id = h AND cloud_provider IS NOT NULL) RETURNING …`. When that returns a row, it inserts the `cloud_reservations` row and returns its id. No row (over budget, or no enabled policy) gives an empty result. `percent` is `(used + reserved) * 100 / budget`.
- `mhai_cloud_settle(p_reservation uuid, p_used bigint) RETURNS boolean`: definer, `EXECUTE` to `mhai_worker` only. It runs `DELETE FROM public.cloud_reservations WHERE id = p_reservation AND hospital_id = mhai_current_hospital() RETURNING month, tokens`. With no row (already settled, or another hospital's), it returns `false` and changes nothing. Otherwise, on that month's row, `reserved = reserved - tokens` and `used = used + least(greatest(p_used, 0), tokens)`, and it returns `true`. A NULL `p_used` (reply unknown) counts as `tokens`.
- `mhai_cloud_usage(p_hospital uuid) RETURNS TABLE (month date, used bigint, reserved bigint)`: executable by `mhai_platform` only, the current month.
- The audit policies: `platform_append` (00014:131-134) and `provision_append` (00010:35-37) recreated with `hospital.model_policy_changed` added, keeping their conditions (`hospital_id IS NULL`; `actor_id IS NULL`). Down restores 00014's and 00010's exact lists.

- [ ] **Step 1: Write the failing tests** (`-tags integration`):
  - `TestModelRouteSetByTriggerNotByInsert` (A13: an insert as `mhai_gateway` naming `model_route = 'cloud:xai'` gets `local` with no policy and `cloud:anthropic` with an Anthropic policy);
  - `TestGatewayCannotTouchPolicyOrRoute` (A13: `INSERT`/`UPDATE`/`SELECT` on `hospital_model_policy` and `UPDATE clinical_queries SET model_route` fail as `mhai_gateway`);
  - `TestTempTableCannotShadowPolicy` (K15: as `mhai_gateway`, `CREATE TEMP TABLE hospital_model_policy` holding a row for its hospital naming `xai`, then an insert into `clinical_queries`; the route is `local` (no real policy). Also `mhai_effective_model_route` and `mhai_cloud_reserve` ignore a temporary `hospital_model_policy` or `cloud_usage` created by `mhai_worker`);
  - `TestWorkerHasNoGrantOnPolicyOrLedger` (K4: `SELECT` on `hospital_model_policy`, `cloud_usage` and `cloud_reservations` fails as `mhai_worker`);
  - `TestReserveIsAtomicUnderRace` (A11: budget 5 × n; 20 parallel `mhai_cloud_reserve(provider, n)` on separate connections; exactly 5 reservations returned);
  - `TestReserveRefusesNonPositiveHugeAndNoHospital` (K4);
  - `TestReserveWithoutEnabledPolicyReturnsNothing`;
  - `TestSettleIsOncePerReservationAndOwnHospitalOnly` (K5: a second settle of the same id returns false and changes nothing; another hospital's id returns false; `used` never exceeds the reserved tokens; `reserved` never goes negative);
  - `TestEffectiveRoute` (A14 and Review Focus 5, the real function: a `cloud:openai` row after the policy is set to NULL gives `local`; after it is set to `anthropic` gives `local`; while `openai` gives `cloud:openai`; with another hospital's id gives `local`);
  - `TestPolicyChecks` (unknown provider, bad `agreement_ref`, provider without `agreement_ref`, budget 0);
  - `TestPlatformAppendAllowsModelPolicyChangedOnlyWithoutHospital` and `TestProvisionAppendAllowsItOnlyWithoutActor` (K3);
  - `TestMigration00018DownUp`.
- [ ] **Step 2: Run (Go, remote):** `go test -race -tags integration ./internal/db/...` → fails (no migration).
- [ ] **Step 3:** Write `00018_model_policy.sql`, following the definer pattern above.
- [ ] **Step 4:** Run the tests, `go vet`, and the whole `internal/db` integration suite (00008's worker policies, 00009's cancel trigger) to show nothing regressed.
- [ ] **Step 5:** Commit: "Add the per-hospital model policy, the model route trigger and the token ledger".

**Security checklist**
- [ ] No role but the trigger function writes `model_route`; the trigger ignores `NEW.model_route` as supplied.
- [ ] `mhai_worker` holds no grant on the policy or the ledger. It reaches them only through the three functions, for its own hospital.
- [ ] Every definer function has `search_path = pg_catalog, pg_temp`, schema-qualified names, and no `PUBLIC` execute.

### Task 2: The policy routes, the CLI and the audit schema

**Files:**
- Create: `services/core/internal/platform/modelpolicy.go`, `modelpolicy_test.go`, `modelpolicy_integration_test.go`; `services/core/cmd/core/modelpolicy_test.go`, `modelpolicy_integration_test.go`
- Modify: `internal/platform/audit.go` (`Register` calls `registerModelPolicy`; `Deps.CloudProviders []string`), `internal/server/server.go`, `internal/config/config.go` (+ `_test`), `internal/audit/schemas.go` (+ `_test`), `cmd/core/seed.go`, `cmd/core/main.go` (usage), `openapi.yaml` (the two routes; `info.version` 1.10.0), `docs/specs/generated/permissions.md` (via `go run ./cmd/permgen`)

**Interfaces:**
- `config.Config.CloudProviders []string` from `CORE_CLOUD_PROVIDERS` (comma list, empty by default; each in `openai, anthropic, xai`; duplicates or unknown names refused at start with the variable's name only).
- Routes (`registerModelPolicy`):
  - `GET /platform/hospitals/{id}/model-policy`: `authz.Rule{Platform: true, NeedsMFA: true, AnyRole: [platform_admin, auditor]}` → `{hospital_id, cloud_provider: string|null, agreement_ref: string|null, monthly_token_budget, updated_at: string|null, usage: {month, used, reserved}}`. No row: the defaults with `cloud_provider: null`. Unknown hospital: 404 `not_found`.
  - `PUT /platform/hospitals/{id}/model-policy`: `authz.Rule{Platform: true, NeedsMFA: true, AnyRole: [platform_admin], Write: true, StepUp: true}`. Strict body: `{cloud_provider: "openai"|"anthropic"|"xai"|null, agreement_ref: string|null, monthly_token_budget: int 1..1000000000, mfa_code | backup_code}`, unknown keys refused (422). A provider not in `CloudProviders` gives 409 `conflict`. A provider without an `agreement_ref` gives 422. Step-up is `d.Auth.StepUpAudited(r, body.MFACode, body.BackupCode)`, as `jobsCancel` does it. Upsert as `mhai_platform`, then `audit.Write` of `hospital.model_policy_changed`, in the same transaction. Answers as `GET`. 10 writes a minute per session, as `jobs/retry` does.
- Audit schema `hospital.model_policy_changed`: fields `hospital_id` (uuid), `provider` and `previous` (`OneOf("none","openai","anthropic","xai")`), `agreement_ref` (pattern above, or absent when none), `monthly_token_budget` (`Int(1, 1000000000)`), `via` (`OneOf("platform","cli")`). Required: `hospital_id`, `provider`, `previous`, `monthly_token_budget`, `via`.
- CLI: `core hospitals model-policy set <hospital id or short code> --provider <openai|anthropic|xai|none> [--agreement-ref <ref>] [--budget <n>]`, as `mhai_provision`, writing the same audit action with `via: cli`. It refuses a provider not in `CORE_CLOUD_PROVIDERS`, prints `model policy <hospital id> <provider>`, and never prints a key or reference beyond the one given.

- [ ] **Step 1: Write the failing tests:**
  - `TestModelPolicyRoutesRoles` (A13: a clinician, a hospital admin and a clinical-audience token get 403 on `PUT`; an auditor gets 403 on `PUT` and 200 on `GET`);
  - `TestModelPolicyPutNeedsStepUp` (a wrong code is refused and audited, as for `jobs/cancel`);
  - `TestModelPolicyPutRefusesDisabledProvider409` (K7);
  - `TestModelPolicyPutStrictBody` (unknown key, provider without ref, budget 0, bad ref shape);
  - `TestModelPolicyPutAuditsClosedPayload` (the row's payload has exactly the schema's keys; `previous` correct; switching off writes `provider: none`);
  - `TestModelPolicyUnknownHospital404`;
  - `TestModelPolicyRateLimited`;
  - `TestAuditSchemaModelPolicyChanged` (refuses free text in `agreement_ref`, an unknown provider, a missing `previous`);
  - `TestConfigCloudProviders` (empty default; unknown and duplicate refused; error names the variable only);
  - `TestCLIModelPolicySet` (K6: unknown provider and a disabled provider refused; `none` switches off; audit written with no actor);
  - the openapi contract test and the generated permissions table pass with the two routes.
- [ ] **Step 2: Run (Go, remote):** `go test -race ./internal/platform/... ./internal/audit/... ./internal/config/... ./cmd/core/...` and `-tags integration` for the same packages → fail.
- [ ] **Step 3:** Implement. Regenerate `permissions.md` with `go run ./cmd/permgen`.
- [ ] **Step 4:** Run the tests and the whole core suite (the contract test, `register_ast_test.go`).
- [ ] **Step 5:** Commit: "Platform admins set a hospital's cloud model policy, with step-up and an audited closed payload".

### Task 3: `egress` and `cloud_outcome` on `query.completed`; `generated_by` on the result

**Files:**
- Modify: `services/core/internal/intel/factschema.go`, `factschema_test.go`; `internal/queries/shapes.go`, `handlers.go`, `handlers_integration_test.go`; `openapi.yaml` (`ClinicalResultOut.generated_by`)

**Interfaces:**
- `query.completed` gains optional fields `egress: inSet({none, openai, anthropic, xai})` and `cloud_outcome: inSet(the twelve codes)`. Required keys are unchanged, so a fact from an older worker stays valid.
- `ClinicalResultOut.GeneratedBy *GeneratedBy` (`json:"generated_by,omitempty"`), `GeneratedBy{Route string; Provider *string}`. It is derived from the caller's own decision, not from the inserted row (departure 15), by `generatedBy(modelVersions map[string]string) *GeneratedBy` in `shapes.go`. `cca_route == "local"` gives `{route: "local", provider: null}`. `cca_route == "cloud"` gives `{route: "cloud", provider: <the cca value's prefix before "/">}`, but only when that prefix is one of the three providers; otherwise the field is omitted. No `cca_route` (an older worker, or a cloud failure with no draft) omits the field. Additive under ADR 0012.

- [ ] **Step 1: Write the failing tests:** `TestQueryCompletedEgressAndOutcome` (A16: each value accepted; an unknown `egress` or `cloud_outcome`, a number and free text refused; a fact without either still valid); `TestOutcomeVocabularyMatchesWorker` (the Go list equals the strings in `services/intelligence/intelligence/boundary/cloud/vocab.py`, read as text, so the two can't drift: K14); `TestGeneratedByFromDecision` (unit, table: local, cloud, an unknown provider prefix, no `cca_route`, a malformed `cca`); `TestResultGeneratedBy` (integration: a row inserted as `cloud:anthropic` whose decision says `cca_route: local` (the policy was switched off before the claim) shows `local`; a cloud decision shows `anthropic`; an escalation with no draft has no `generated_by`; another hospital's query still 404s).
- [ ] **Step 2: Run (Go, remote):** `go test -race ./internal/intel/... ./internal/queries/...` and `-tags integration ./internal/queries/...` → fail. (`TestOutcomeVocabularyMatchesWorker` stays skipped with a message until Task 4 creates `vocab.py`; Task 4's PR removes the skip.)
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the tests and the openapi breaking-change check (additive only).
- [ ] **Step 5:** Commit: "Record where an answer was generated: egress and cloud_outcome facts, generated_by on the result".

**PR A:** after Task 3, run `security-review` on the branch and open the PR. mhai-web's PR (the policy screen and `generated_by`) can start from the openapi file.

### Part B: the gate (PR B)

### Task 4: Settings, key files and redaction

**Files:**
- Create: `intelligence/boundary/cloud/{__init__,vocab,apikey,settings}.py`, `tests/boundary/cloud/{__init__,conftest,test_vocab,test_apikey,test_settings}.py`
- Modify: `services/intelligence/pyproject.toml` (deps `openai`, `anthropic`; dev `hypothesis`, `pytest-socket`; mypy strict for `intelligence.boundary.cloud`), `uv.lock`, `intelligence/boundary/config.py` (`WorkerSettings.cloud: CloudSettings`; `is_demo`), `intelligence/boundary/worker.py` (`secure_logging`: with a cloud provider configured, `install_redaction()` and SDK loggers set to `WARNING` at start; with cloud off it does nothing, so logging is exactly as before), `services/core/internal/intel/factschema_test.go` (remove Task 3's skip)

**Interfaces:**
- `vocab.PROVIDERS = ("anthropic", "openai", "xai")`, `ROUTES`, `EGRESS`, `OUTCOMES` (§11's twelve), `BASE_URLS = {"anthropic": "https://api.anthropic.com", "openai": "https://api.openai.com/v1", "xai": "https://api.x.ai/v1"}`, `HOSTS` (derived).
- `ApiKey`: `__slots__`, `__repr__`/`__str__` return `ApiKey(<redacted>)`, `reveal() -> str`, refuses pickling and copying (`__reduce__` and `__copy__` raise `TypeError`).
- `load_api_key(path, name, development, errs) -> ApiKey | None`: uses `config._read_secret_file`, then refuses outright (not just a warning, unlike the database password) a file that others can read (`S_IROTH`), checked with `fstat` on the same descriptor. It also refuses an empty key, a placeholder (`config.is_placeholder`), any whitespace inside the key, and a length under 20.
- `SDK_ENV_PREFIXES = ("OPENAI_", "ANTHROPIC_", "AZURE_OPENAI_", "XAI_")`: the worker refuses to start when any variable under these prefixes is set (even empty), cloud enabled or not. They would put a key in the environment, redirect an SDK, add request headers (`OPENAI_CUSTOM_HEADERS`, `ANTHROPIC_CUSTOM_HEADERS`), pick an organisation, project or credential chain, switch on debug logging or set trace propagation. `SDK_ENV_REFUSED` lists the names openai 3.37.0 and anthropic 2.3.0 read, as documentation (amended in the Task 4 fix round; the prefix is the rule, so names a later release adds are refused too). `SSL_CERT_FILE` and `SSL_CERT_DIR` are refused once a provider is enabled.
- `RedactingFilter(logging.Filter)`: rewrites `record.msg`, `record.args` and `exc_text` so that `sk-[A-Za-z0-9_-]{8,}`, `sk-ant-[A-Za-z0-9_-]{8,}`, `xai-[A-Za-z0-9_-]{8,}` and each loaded key's exact value become `[REDACTED]`. `install_redaction(keys)` puts it on the root logger's handlers.
- `ProviderSettings(name, model, key: ApiKey, zdr_ref, base_url)`. `CloudSettings(providers: tuple[ProviderSettings, ...], proxy_url: str | None, demo: bool)`. `CloudSettings.from_env(env, development, errs)` reads `INTELLIGENCE_CLOUD_PROVIDERS`, `INTELLIGENCE_CLOUD_<P>_MODEL` (`^[A-Za-z0-9._:-]{1,64}$`; for Anthropic, an id containing `fable` or `mythos`, case-folded, is refused: those models need 30-day retention, §14), `…_KEY_FILE`, `…_ZDR_REF` (`^[A-Z0-9][A-Z0-9_.-]{2,63}$`), `…_BASE_URL` (development only) and `HTTPS_PROXY` (`http://<host>:<port>` only). Start-up is refused when a provider is enabled with no key file, a bad key, no model, no `ZDR_REF` outside development, `ZDR_REF=DEMO-SYNTHETIC` outside `demo`/`development`, no `HTTPS_PROXY` outside development, or a base-URL override outside development. Errors name variables only.
- `config.is_demo(env)`: `APP_ENV` is `demo`. Demo is not development (deviation 7).

- [ ] **Step 1: Write the failing tests:** `test_apikey_never_in_repr_str_format_pickle_copy`, `test_key_file_mode_and_placeholder_refused` (A10 start-up), `test_redacting_filter_on_message_args_and_exception` (A10: a log call with the key in `args`, and an exception whose text echoes `Authorization: Bearer <key>`, both come out redacted), `test_settings_default_has_no_providers`, `test_settings_refusals` (each refusal above, parametrised, error message without values), `test_demo_accepts_demo_synthetic_with_warning_production_refuses`, `test_base_url_override_only_in_development` (A17), `test_vocab_lists_are_closed_and_sorted`, `test_world_readable_key_file_refused` (A10), `test_sdk_environment_variables_refused` (K16: each name in `SDK_ENV_REFUSED`), `test_fable_and_mythos_models_refused` (K9), `test_unit_modules_disable_sockets` (the meta-test from the Global Constraints: every module in `tests/boundary/cloud/` without the `boundary` marker has `pytest.mark.disable_socket`).
- [ ] **Step 2: Run (Python, remote):** `uv add openai anthropic && uv add --dev hypothesis pytest-socket` (commit the lock), then `uv run pytest -q tests/boundary/cloud` → fails.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the tests, `tests/boundary/test_worker_unit.py` (settings unchanged when no cloud variables are set), lint, format, mypy, `uv lock --check`.
- [ ] **Step 5:** Commit: "Cloud provider settings: key files only, redacted everywhere, refused unless complete".

### Task 5: The gate types

**Files:**
- Create: `intelligence/boundary/cloud/gate.py`, `tests/boundary/cloud/test_gate.py`, `tests/boundary/cloud/test_imports.py`
- Modify: `intelligence/boundary/kb_repo.py` (shared rows sealed), `tests/boundary/test_models.py` (existing chunk tests still pass)

**Interfaces:**
- `MaskedText(str)`: built only by `TaggingMasker.mask`. The constructor takes `(value, *, _tag, _key_id)` and the class checks `hmac.compare_digest`. `verify(masker) -> bool`. Every `str` method and operator returns a plain `str` (subclass behaviour, so nothing to override; pinned by test). `__reduce__`, `__copy__` and `__deepcopy__` raise `TypeError`.
- `TaggingMasker(masker=None)`: holds a 32-byte key from `secrets.token_bytes(32)` and a lock. `mask(text: str) -> MaskedText` runs `PIIMasker.mask` and tags the result with HMAC-SHA256 over the UTF-8 value. `owns(m: MaskedText) -> bool`.
- `SharedChunk`: frozen dataclass `(chunk_id, content, tag)` with `repr=False` on `content`. It is built only by `seal_shared_chunk(chunk_id, content, *, _from_repo)`, whose key is a module-private `secrets.token_bytes(32)`. `as_kb_chunk() -> KBChunk` (a guideline chunk with `source_patient_id=REFERENCE_SOURCE`) is what the retriever indexes. `valid() -> bool` re-checks the tag and that `chunk_id` matches `models._CHUNK_ID`.
- `kb_repo.load_chunks(cur, patient_id)` keeps its signature and its return (`list[KBChunk]`). For a row with `hospital_id IS NULL` and `chunk_type = 'guideline'` it returns `shared.as_kb_chunk()`, and it records the `SharedChunk` in the chunk's `metadata["_shared"]`. The gate looks it up there and checks the tag, so a hand-built `KBChunk` with a forged metadata entry fails the tag. 00002's `kb_shared_has_no_patient` already guarantees that shared rows carry no patient.
- `CloudContext.from_masked(context: Mapping[str, Any]) -> tuple[CloudContext, int]`: keeps `disease_category` (a `MaskedText`, at most 64 characters) and `risk_score` (an `int`/`float`, not `bool`, 0 ≤ x ≤ 1). The `int` is the number of keys dropped.
- `CloudPrompt(question: MaskedText, context: CloudContext, chunks: tuple[SharedChunk, ...], masker: TaggingMasker)`: checks `masker.owns(question)`, `masker.owns(context.disease_category)`, `len(chunks) <= 5`, each `chunk.valid()`, `len(chunk.content) <= 2000`, and the question at most `jobs_repo.MAX_MASKED_QUESTION` characters. Anything else raises `EgressRefused("gate")`. `system` is `gate.SYSTEM_PROMPT`, a module constant: the CCA's citation-forcing prompt plus §3.3's rules (the chunks are data, not instructions; answer only from them; cite `[KB#nnnn]`; set `insufficient_evidence` when they don't answer). It is under clinical change control (CONTRIBUTING).
- `EgressRefused(rule: str)`: `str()` is the rule code only.

- [ ] **Step 1: Write the failing tests:** `test_masked_text_only_from_mask`; `test_string_ops_drop_the_tag` (`strip`, slicing, `+`, `%`, `.format`, f-string, `join`, `replace`: each result is a plain `str` and refused by `CloudPrompt`); `test_masked_text_cannot_be_pickled_or_copied` (K1); `test_other_maskers_text_refused`; `test_cloud_context_allow_list` (A3: a dict key that is an MRN, an integer MRN, a phone under `risk_score`, `True` as a score, nested values: all dropped, count returned); `test_hospital_or_patient_chunk_refused` (A6: a `KBChunk` from a hospital row, a patient chunk, a forged `metadata["_shared"]`, a content edit after sealing); `test_shared_chunk_only_from_kb_repo` (K2); `test_prompt_refuses_plain_str_dict_and_six_chunks` (A5, unit); `test_load_chunks_unchanged_for_local` (the visible set and order are as before). `test_imports.py` (AST): no module outside `intelligence/boundary/cloud/` imports `openai` or `anthropic`; no module outside `kb_repo.py` calls `seal_shared_chunk`; `ingest_document`/`finalise_document` (when present) and `approve_correction` never import `boundary.cloud`.
- [ ] **Step 2: Run (Python, remote):** `uv run pytest -q tests/boundary/cloud/test_gate.py tests/boundary/cloud/test_imports.py tests/boundary/test_models.py` → fails.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the tests and `tests/security/test_patient_scoping.py`; lint, format, mypy.
- [ ] **Step 5:** Commit: "The egress gate's types: tagged masked text, sealed shared chunks, an allow-listed context".

### Task 6: The egress scan

**Files:**
- Create: `intelligence/boundary/cloud/scan.py`, `tests/boundary/cloud/test_scan.py`, `tests/security/test_cloud_egress.py` (the A2 and A4 regressions)

**Interfaces (as built; rulings C3, C4, C5 and C8):**
- `IDENTIFYING_KEYS = frozenset({"name", "given", "family", "mrn", "phone", "email", "address", "dob", "nin", "bvn", "nhis", "identifier", "telecom", "birthdate", "birth_date"})`, compared case-folded. Values under these keys anywhere in the tree are collected (deviation 9). `NAME_KEYS = {"name", "given", "family"}`. The `name` key over-blocks (`{"name": "Malaria"}`, FHIR `system: "phone"`): ruling C4 keeps that, measured for H2 (Task 16).
- `Identifiers`: a frozen dataclass with `values: frozenset[str]` and a `repr` showing a count only. `hit(s)` matches the values as whole-word `norm_words` phrases and as numbers of 5 or more digits at group boundaries, as written and with the masker's placeholders taken out. `collect_identifiers(question: str, patient: Mapping[str, Any]) -> Identifiers`: every string, and every `int` (or integral float) of 5 or more digits, under an identifying key (any depth up to `MAX_DEPTH`, pinned to `run_query.MAX_DEPTH`); each name token of 3 or more letters under a name key; and each `original_text` a fresh `PIIMasker` detects in the question and in every string and key of the tree. Every value is normalised, placeholders are ignored, and values under 3 characters are dropped.
- **On real data this rule has no names to match.** Core's `PatientData` has no name key, so `known_identifier` catches a bare name only when the record carries one (tests and synthetic fixtures). Core supplying keyed hashes of the patient's name tokens (C5) and an untitled-name detector in the masker (C8) are both prerequisites for real data (Task 16, H2).
- **The scan works on decoded strings, not on raw bytes.** JSON escapes text: a newline in a name is `\n` in the bytes, and a zero-width space is `​`. So the body is parsed with `_strictjson` (duplicate keys refused, at most `MAX_BODY_BYTES` = 49,152 bytes; deep nesting is `not_json`), and every leaf is collected with its JSON pointer.
- **Shape, built only by `shape_for`.** Every adapter renders the user content as text blocks in `CloudPrompt.blocks()` order (ruling R-1): block 0 the masked question, block 1 the context (`Category: … / Risk: …`), blocks 2 to 6 the chunks (0 to 5, each delimited with its id). `MAX_BLOCKS = 2 + gate.MAX_CHUNKS`. `scan.BLOCK_ARRAYS` holds each provider's content array, and block i's text is at `<array>/<i>/text`:
  - Anthropic: `/messages/0/content`;
  - OpenAI: `/input/0/content`;
  - xAI: `/messages/1/content` (the system message is `/messages/0`).

  `Shape(patient_paths, allowed_paths, fixed_values)` takes exact JSON pointers, **no wildcards**. `fixed_values` pins the value of every leaf that isn't free text, with an exact type (so `True` can't stand in for `1`). `Shape.__post_init__` refuses any declaration whose free pointers (allowed but not fixed) are not exactly one provider's seven block texts with blocks 0 and 1 as the patient paths, so a hand-built Shape with no fixed values, or with `/model` or the system prompt left free, can't be made. **Adapters build theirs with `scan.shape_for(name, scan.leaf_values(fixed_parts))`**, where `fixed_parts` is every part of the request outside the blocks: the model, scalars such as the token cap and `store`, roles, the system prompt, and the answer schema as the provider nests it (`leaf_values(ANSWER_SCHEMA, prefix)`). `shape_for` pins each block's `type` itself (`text`, or `input_text` for OpenAI). `walk`, `leaf_pointers` and `leaf_values` list a document's leaves (every scalar, null and empty container) with their pointers.
- **Two normalisations.**
  - `norm_words(s)`, for the identifier rule: NFKC; every `Cf` character and the non-`Cf` default-ignorables (variation selectors, Hangul fillers, U+034F, the braille blank) and non-space controls removed; every `Nd` digit as ASCII; dashes and the minus sign as `-`; case-folded, then a confusables skeleton (Cyrillic, Greek and Armenian letters that look Latin, as Latin); accents and every combining mark (Mn, Mc, Me) dropped; every run of characters that aren't letters or digits one space. "Adaeze\nOkafor", "ADAEZE​ OKAFOR", "adaeze-okafor" and "Okafoг" (Cyrillic г) all match.
  - `norm_text(s)`, for the pattern rules: the same cleaning and skeleton, every Unicode space one ASCII space, punctuation **kept**.
- **Digit runs, joined only where an identifier's shape needs it.** Runs never join across letters, the chunk delimiters (`<`, `>`, `#`, `:`) or JSON values, so `KB#0001` and `1500` stay apart.
  - `digit_runs(s)`: plain runs; any non-digit ends one, so "T 38.5 101.3" gives `38`, `5`, `101`, `3`, and "HR 100-120" gives `100`, `120` (a range is sent).
  - Hyphen chains (`_HYPHEN_CHAIN`): **3 or more** digit groups joined by single hyphens are one run for `long_digits` ("123-456-7890", "123-45-678"). Two groups stay apart, so an unrecorded "123-456" passes, like "445 812" (both residuals).
  - `grouped_runs(s)`: groups of 2 to 4 digits across a single space, `-` or `.`, kept only when the run fits a phone (11 digits from `0[789][01]`, 13 from `234[789][01]`) or a NIN (11 digits).
  - Phone runs: groups of 2 to 4 digits across a short gap (`_gap_ok`: 1 to `MAX_DIGIT_GAP` = 3 characters, each a space or one of `( ) - . , / + ; _`), with the trunk "0" in "+234 (0) 803 …" skipped; and five or more single digits one space apart ("0 8 0 3 …"). Phone only: the prefix keeps vitals out.
  - Identifier chains (`_chains`): every digit group joined across the same short gap, with the offsets where groups meet. A recorded number matches only when it starts and ends on a group boundary, so it can only hit this patient's own number ("284,817", "(284) 817", "284 – 817" for MRN `284817`; "12-34-567" for `1234567`).
  - A placeholder between two digits is removed before the patterns, so "12345[NAME_REDACTED]678901" is still one run.
- **The rules,** in this order, each returning a code from the closed tuple `scan.RULES` (never the match, the text or an offset): `trace_header`, `too_large`, `not_json`, `shape`, `known_identifier`, `ng_phone`, `nin`, `email`, `date`, `long_digits`, `mrn_prefix`, `pii_detected`, `scan_error`.
  1. `trace_header` (hook only): the request carries `traceparent`, `tracestate` or `baggage`. The hook also checks the decoded URL and every header value with the identifier rule (`known_identifier`).
  2. `too_large`, `not_json`, `shape`: the root is an object; every leaf is at an allowed pointer and, where fixed, equals its pinned value; both patient paths are present as strings.
  3. `known_identifier`, over **every** string in the body, chunks included, then once more over the raw text with the JSON keys removed (the shape already pins every key, so a patient named Max isn't refused for `max_tokens`).
  4. The pattern rules, over the **two patient blocks only** (shared chunks are curated guideline text):
     - `ng_phone`: a phone run (above);
     - `nin`: any other 11-digit plain, hyphen-chain or grouped run;
     - `email`;
     - `date`: `d/m/y` and `y/m/d` with the **same** separator both times (`/`, `.` or `-`, spaces allowed around it; so "T 37.5-38.5" and "0.5-1.25 mg/kg" are ranges, not dates); `d m yyyy` with spaces only and a 19xx or 20xx year; a month name with a 4-digit year ("12th of March, 1984", "March 12, 1984"); a month name other than "may" with a 2-digit year ("12 Mar 84");
     - `long_digits`: a plain run or a hyphen chain of 6 or more digits;
     - `mrn_prefix`: `m.r.n`, or `hospital|hosp.|folder|card|file` then `number|no`, or `hospital #`, ending on a word boundary, then `[\s:#=.\-]*` and a letter or digit (so "hospital notes" and `[MRN_REDACTED]` are not one).
  5. `pii_detected` (ruling C5): a fresh `PIIMasker` over the two patient blocks, read with invisible characters removed and confusables mapped, case kept. Any detection refuses. It runs last, so the earlier codes keep their meaning.
  6. `scan_error`: any unexpected exception. `scan_body` never raises, and a non-`Identifiers` or non-`Shape` argument gives `scan_error`.
- **Every rule code maps to the outcome `egress_blocked`** in core's vocabulary. Rule codes never appear in a fact, an audit field or a problem body: core has no rule field, so a new code can't stall the drain. So do the client's own refusals outside `scan.RULES`: `gate` (`gate.EgressRefused("gate")`) and `render_failed` (Task 9). A metric labelled by rule takes its label from `scan.RULES ∪ {"gate", "render_failed"}`, defined once in Task 9.
- `ScanHook(ids, shape)`: a callable for `httpx2` `event_hooks["request"]`. It checks the headers and URL, then reads `request.content` (a streamed, unread body is `not_json`), sets `self.verdict` (a rule or `None`) and `self.sent` (`True` once a body has passed), and raises `EgressRefused(rule)` on a hit. Once it has refused, it refuses every later request. Nothing in the module logs.
- **Recorded residuals and over-blocks (C4, C8; listed in Task 16):** an untitled name ("Adaeze Okafor has chest pain") on core's real `PatientData` is sent (pinned `xfail(strict)`); gap characters `| * · ' ~` and gaps of 4 or more; confusables outside the map (the rest of UTS #39); a whole name in a script with no Latin lookalike; a name split inside a token ("Ada eze") or run together ("AdaezeOkafor"); "12 May 84", "March the 12th, 1984"; "patient no", "reg no", "M R N", "MR# 4471", "hospital num"; a single-digit-spaced NIN or unrecorded number; numbers written as words. Over-blocks: the `name` key anywhere, PIIMasker's 5-digit ZIP rule, a given name that is a common word (Grace, Joy), "3 march 20" read as a date, "hospital no longer …", and three spaced numbers ending in a 19xx or 20xx year.

- [ ] **Step 1: Write the failing tests.** **Escape and bypass attempts**, each blocked with `known_identifier`:
  - `test_name_without_title_blocked` (A2: "Adaeze Okafor has chest pain", with `name: "Adaeze Okafor"` in synthetic `patient_data`; the masker misses it. This holds only when the record names the patient: on core's real `PatientData`, which has no name key, the same question is sent, pinned by `test_untitled_name_on_core_shape_refused` as `xfail(strict=True, raises=AssertionError)` until C5's keyed hashes or C8's name detector exist);
  - `test_escaped_and_split_names_blocked_through_sdk_encoder`: the question holds "Adaeze\nOkafor", "Adaeze\tOkafor", "Ada​eze Okafor", "ADAEZE  OKAFOR" or "Adaeze-Okafor". The body is encoded with `json.dumps(..., ensure_ascii=True)` here, and Tasks 11–13 re-run the same cases through each real SDK's encoder;
  - `test_scan_sees_through_zero_width_fullwidth_and_case` (full-width digits in a phone number, a non-breaking space inside an MRN);
  - `test_integer_mrn_found_with_separators` (`12-34-567` for MRN `1234567`);
  - `test_known_phone_with_any_short_gap_blocked`, `test_known_mrn_with_any_short_gap_blocked`, `test_confusables_and_marks_blocked`, `test_cyrillic_ghe_for_r`, `test_cyrillic_and_greek_names_match_in_any_case`, `test_placeholder_between_digits_does_not_split_a_number`, `test_split_name_property` and `test_split_mrn_property` (Hypothesis).

  **The masker's detector** (`pii_detected`, C5): `test_masker_detection_in_the_question_refuses`, `test_masker_detection_in_the_context_refuses`, `test_masker_detection_not_run_on_chunks`, `test_core_patient_data_feeds_known_identifiers` (core's real `PatientData` shape).

  **Patterns**, each blocked:
  - `test_nigerian_phone_and_nin_blocked_without_record` (A4: `0803 123 4567`, `+234 803 123 4567`, `0803-123-4567`, an 11-digit NIN);
  - `test_patterns_keep_their_punctuation` (an email address, `12/03/1990`, `1990-03-12`, `MRN: 4471`);
  - `test_phone_with_any_short_gap_blocked_without_record`, `test_single_digit_phone_blocked`, `test_semicolon_and_underscore_gaps`, `test_hyphen_chain_of_three_groups_is_long_digits` (3 or more groups), `test_spaced_and_worded_dates_blocked`, `test_month_name_with_two_digit_year_is_a_date`, `test_more_mrn_phrasings_blocked`, `test_hospital_hash_is_a_record_number`.

  **False positives**, each **sent**:
  - `test_short_name_token_needs_word_boundary` ("Ada" in "adaptive", "Eze" in "freeze");
  - `test_no_digit_join_across_values` (an MRN `000115` against a body holding `KB#0001` and `max_tokens 1500`, and `0.42` next to `1500`);
  - `test_guideline_doses_and_dates_pass` (a shared chunk with "200000 IU", "2023-05-01" and "1:1000", at block 2, passes with empty identifiers, for each provider's shape);
  - `test_typical_clinical_questions_are_sent` (R-2: a fixed synthetic set of at least 20 questions with vitals and labs, all sent. For example: "T 38.5 101.3, BP 120/80, HR 96", "platelets 250 000, Hb 11.2", "glucose 7.8 mmol/L at 2 h", "weight 72.5 kg, 1500 ml fluid", "WBC 12.4 x10^9/L");
  - `test_common_word_given_name_blocks_today` (R-3, pins current behaviour: a patient named `Grace` and a chunk saying "grace period" gives `known_identifier`, so the job escalates. The test's docstring says this is a measured over-block, reported in the H2 evaluation, and that changing it needs that evaluation, not an edit to this test);
  - `test_hyphenated_range_is_sent` ("HR 100-120", "1000-1500 ml": two groups), `test_decimal_ranges_are_not_dates` ("T 37.5-38.5", "0.5-1.25 mg/kg"), `test_vitals_still_sent`, `test_hospital_notes_is_not_a_hospital_number`;
  - the C4 over-block pins `test_zip_rule_over_block_pinned` and `test_name_key_over_block_pinned`;
  - `test_redacted_placeholders_pass`;
  - `test_clean_record_is_sent` (a fixed synthetic record whose question and context hold no identifier: `scan_body` gives `None`).

  **Mechanics:**
  - `test_shape_refuses_unknown_path`;
  - `test_shape_needs_exact_pointers`, `test_hand_built_shape_without_fixed_values_refused` (no fixed values, `/model` left free, an extra free pointer, or patient paths other than blocks 0 and 1: each `ValueError`; the declaration `shape_for` gives is accepted), `test_shape_pins_fixed_values` (a changed model, system prompt, schema description or block type, or `True` for `1`, gives `shape`);
  - `test_schema_strings_are_allowed_paths` (for each provider's `Shape`, every string inside the rendered JSON schema is at a pointer in `allowed_paths`, so a valid request isn't refused as `shape`; a string added to the schema by hand without updating the shape is refused);
  - `test_non_json_body_refused`;
  - `test_scan_reports_rule_not_text` (the verdict is a code; `caplog` holds no body text);
  - `test_patient_paths_are_exact_for_each_layout` (R-1: for each provider's `Shape`, with 0, 1 and 5 chunks, the pattern rules see blocks 0 and 1 only, a chunk at block 2 with "200000 IU" is sent, and a phone number in the question at block 0 is blocked);
  - `test_name_in_question_blocked_for_every_layout` (R-1: "Adaeze Okafor" in block 0, rendered in each provider's layout, gives `known_identifier`);
  - `test_identifiers_repr_has_no_values`;
  - `test_size_cap`;
  - `test_trace_headers_refused`, `test_hook_scans_url_and_headers`, `test_hook_sets_verdict_and_sent`, `test_hook_refuses_an_unreadable_body`, `test_hook_refusal_stops_the_sdk_before_the_transport`.

  In `tests/security/test_cloud_egress.py`: the A2, A4 and split-name cases again, through `ScanHook` on an `httpx2.Request`.
- [ ] **Step 2: Run (Python, remote):** `uv run pytest -q tests/boundary/cloud/test_scan.py tests/security/test_cloud_egress.py` → fails.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the tests, lint, format, mypy.
- [ ] **Step 5:** Commit: "Scan the exact request bytes for this patient's identifiers and Nigerian identifier shapes; fail closed".

### Task 7: The budget port

**Files:**
- Create: `intelligence/boundary/cloud_repo.py`, `intelligence/boundary/cloud/budget.py`, `tests/boundary/cloud/test_budget.py` (unit and `boundary`)
- Modify: `tests/boundary/test_no_direct_db.py` (`REPOS` gains `boundary/cloud_repo.py`; a security-test change, called out in the PR description)

**Interfaces:**
- `cloud_repo.reserve(cur, provider: str, tokens: int) -> tuple[str, int] | None` runs `SELECT reservation, percent FROM mhai_cloud_reserve(%s, %s)`. `cloud_repo.settle(cur, reservation: str, used: int | None) -> bool` runs `SELECT mhai_cloud_settle(%s, %s)`. Both are parameterised, on the caller's cursor, and never open a connection (the same rules as `jobs_repo`).
- **Interface change (Task 1 review, ruling C1):** core's function is now `mhai_cloud_reserve(p_provider text, p_tokens bigint)`. It reserves only while the hospital is active and its policy names that same provider right now, so `cloud_repo.reserve(cur, provider: str, tokens: int)` runs `SELECT reservation, percent FROM mhai_cloud_reserve(%s, %s)` and `BudgetPort.reserve` takes the call's provider; an empty result (budget, switched policy or inactive hospital) is `budget_exhausted`.
- `estimate_tokens(body_len: int, max_output: int = 1500) -> int`: `body_len + max_output`, one token per UTF-8 byte of the whole body (ruling C7, Task 7 review I1: byte-level tokenizers never give more tokens than bytes, and settle can't record an overrun). An estimate over core's 100,000 limit raises `ValueError`, so the call does not reserve or send; the largest body the scan admits (49,152 bytes) plus 1,500 stays under it.
- `Reservation(id: str, tokens: int, percent: int)`.
- `BudgetPort` (Protocol, sync): `reserve(provider: str, tokens: int) -> Reservation | None`, `settle(r: Reservation, used: int | None) -> None`. `used=None` (reply lost or unknown) settles as `r.tokens`.
- `DbBudget(db: Database, verified: Verified, loop: asyncio.AbstractEventLoop, timeout: float = 5.0)`: each call runs `cloud_repo.reserve` or `cloud_repo.settle` in its own `with_hospital(verified)` transaction on the job's loop (`asyncio.run_coroutine_threadsafe(...).result(timeout)`), so a generation thread can call it. `budget.py` itself holds no SQL. Any database error from `reserve` gives `None` (fail closed). A failed `settle` is logged with ids only and the metric `mhai_cloud_settle_failures_total`. At `percent >= 80` the first time in a call it logs `cloud.budget_high hospital=<id> percent=<n>` and counts `mhai_cloud_budget_alerts_total{provider}`.

- [ ] **Step 1: Write the failing tests:** `test_estimate_is_an_upper_bound` (a body of n UTF-8 bytes estimates n + 1500, and an estimate over 100,000 raises `ValueError`); `test_reserve_none_on_db_error`; `test_settle_unknown_reply_counts_reserved`; `test_budget_race_through_the_port` (`boundary`, A11: 20 threads through `DbBudget` against a budget for 5 → exactly 5 reservations); `test_budget_uses_token_hospital_only` (`boundary`: the port has no hospital parameter; a second hospital's ledger is unchanged); `test_alert_at_80_percent_once`; `test_settle_twice_is_a_no_op` (`boundary`, K5); `test_no_direct_db_still_passes` (the existing suite, with `cloud_repo.py` in `REPOS` and `budget.py` not in it).
- [ ] **Step 2: Run (Python, remote):** `uv run pytest -q tests/boundary/cloud/test_budget.py tests/boundary/test_no_direct_db.py -m "not boundary"` and `-m boundary tests/boundary/cloud/test_budget.py` → fail.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the tests, lint, format, mypy.
- [ ] **Step 5:** Commit: "Reserve cloud tokens atomically through core's function before each send".

**PR B:** after Task 7, run `security-review` and open the PR.

### Part C: the client and the job (PR C)

### Task 8: Reply validation, outcomes and retries

**Files:**
- Create: `intelligence/boundary/cloud/{reply,retry}.py`, `tests/boundary/cloud/{test_reply,test_retry}.py`

**Interfaces:**
- `ANSWER_SCHEMA`: the JSON schema of §9 (`answer` string, `citations` array of strings, `insufficient_evidence` boolean; all required; `additionalProperties: false`; no `pattern`; `maxLength` 2048 is not used, as the design notes).
- `Answer`: Pydantic, `extra="forbid"`, with `answer: str` (`repr=False`), `citations: tuple[str, ...]` and `insufficient_evidence: bool`. `validate_answer(raw: bytes | str | Mapping, sent_ids: frozenset[str]) -> Answer` raises `Malformed` when: the JSON is invalid or has duplicate keys (via `_strictjson`); `answer` is empty or over 6,000 characters; a citation doesn't match `^KB#\d{4}$` or isn't in `sent_ids`; the set of `[KB#nnnn]` markers in `answer` differs from `set(citations)`; there is no citation and `insufficient_evidence` is false; `answer` contains a URL (`https?://`, `www.`), an HTML tag, a Markdown link or image, a code fence, or the system prompt's chunk delimiter. `insufficient_evidence: true` is valid (the client maps it to `insufficient`).
- `retry.classify(exc_or_status) -> str`: one of `retry` (connection error, timeout, 429, 500, 502, 503, 504, provider "overloaded" (Anthropic 529)) or a final outcome (`auth_failed` for 401 and 403, `malformed` for 400, 404 and 422, `unavailable` for other 4xx and 5xx).
- `RetryPolicy(max_extra: int = 2, delays: tuple[float, ...] = (1.0, 3.0), rng: random.Random)`: `next_delay(attempt, retry_after: float | None, left: float) -> float | None`, full jitter (`uniform(0, delay)`). It returns `Retry-After` when that fits in `left`, and `None` (stop) when no delay fits.

- [ ] **Step 1: Write the failing tests:** `test_answer_checks` (parametrised: an unknown citation `KB#9999` (A7), markers and citations disagree (A7), a link (A7), a Markdown image, a code fence, an HTML tag, extra keys, a missing key, duplicate keys, 6,001 characters, no citation without `insufficient_evidence`; each `Malformed`); `test_good_answer_passes`; `test_classify_table`; `test_retry_after_used_only_when_it_fits`; `test_never_retry_on_400_401_403_404_refusal_truncated_malformed_egress` (K12); `test_malformed_error_has_no_text`.
- [ ] **Step 2: Run (Python, remote):** `uv run pytest -q tests/boundary/cloud/test_reply.py tests/boundary/cloud/test_retry.py` → fails.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the tests, lint, format, mypy.
- [ ] **Step 5:** Commit: "Validate cloud replies strictly and decide retries from a closed table".

### Task 9: `CloudModelClient`

**Files:**
- Create: `intelligence/boundary/cloud/client.py`, `tests/boundary/cloud/test_client.py`
- Modify: `intelligence/boundary/models.py` (`Generation` gains `versions: Mapping[str, str] = field(default_factory=dict)`, `egress: str = "none"`, `cloud_outcome: str | None = None`, `tokens_in: int = 0`, `tokens_out: int = 0`; `ModelClient.generate(..., *, call: CloudCall | None = None)`; `OrchestratorModelClient` and `StubModelClient` accept and ignore `call`; `OrchestratorModelClient.verify`'s body moves to `_verify_with(orch, generation) -> Verification`, which both clients use; `default_orchestrator` exposes its `build` and versions through `local_parts() -> tuple[Builder, dict[str, str]]`), `tests/boundary/test_models.py`, `tests/boundary/cloud/conftest.py` (`FakeAdapter`, `CapturingTransport`, `ScriptedTransport`)

**Interfaces:**
- `CloudCall(identifiers: Identifiers, budget: BudgetPort, stage_deadline: float)`, frozen, `repr` with no values. `stage_deadline` is a `time.monotonic()` value.
- `CloudReply(answer: Answer | None, model_id: str, tokens_in: int | None, tokens_out: int | None, outcome: str, retry_after: float | None = None)`.
- `Adapter` (Protocol): `name: str`; `shape: Shape`, built with `scan.shape_for(name, scan.leaf_values(fixed_parts))` (Task 6: the seven block texts are the only free text, every other leaf is pinned to its value, and `Shape` refuses any other declaration); `make_sdk(http_client: httpx2.Client, key: ApiKey, base_url: str) -> Any` (with `max_retries=0`; the Anthropic adapter also passes `open_telemetry={"propagation": False, "traces": False}`, see Step 1); and `call(sdk: Any, prompt: CloudPrompt, model: str, timeout: float) -> CloudReply`. `call` maps the SDK's own result to a reply. It never raises for a provider status: the status becomes the outcome, via `retry.classify`.
- `_SharedTransport(httpx2.BaseTransport)`: wraps the one real transport, passes `handle_request` through, and makes `close()` a no-op, so closing a per-attempt client never closes the shared pool. The real transport is closed once, by `RunQuery.close()`.
- `_RenderTransport(httpx2.BaseTransport)` is the dry render: a capture-and-abort transport. It stores `request.content`, then raises `_Rendered`, so nothing goes anywhere. The client builds the SDK over it, calls `adapter.call` once, catches `_Rendered` (and the SDK's wrapping of it, by checking the transport's captured body rather than the exception type), and so gets the exact body to estimate from and to pre-scan. If no body was captured, the outcome is `egress_blocked` (`render_failed`). Every refusal's rule code (`scan.RULES`, `gate`, `render_failed`) maps to the outcome `egress_blocked` and is never written to a fact or audit field. If a metric is labelled by rule, its label set is `REFUSAL_RULES = scan.RULES + ("gate", "render_failed")`, defined once in `client.py`.
- `CloudModelClient(provider: ProviderSettings, adapter: Adapter, build: Builder, local_versions: Mapping[str, str], transport: httpx2.BaseTransport, *, policy: RetryPolicy | None = None, clock=time.monotonic, sleep=time.sleep)`:
  - `mask(text) -> MaskedText` with its own `TaggingMasker`.
  - `generate(question, patient_context, patient_id, chunks, *, call)`, in this order. `call is None` or `patient_id is not None` gives `egress_blocked`. Build the orchestrator. `retrieve(question, top_k=5, patient_id=None)`. Map the results to `SharedChunk`s (anything else: `egress_blocked`). Build `CloudContext.from_masked` (metric `mhai_cloud_context_dropped_total` += dropped) and `CloudPrompt`. Then the dry render through `_RenderTransport`, with `scan_body(body, call.identifiers, adapter.shape)` on what it captured: a hit gives `egress_blocked`, before any budget is reserved. Then the attempt loop: estimate from the rendered body's length in UTF-8 bytes (`len(body_bytes)`, never `len(str)`), `call.budget.reserve` (`None` gives `budget_exhausted`), a per-attempt `httpx2.Client(transport=_SharedTransport(transport), event_hooks={"request": [ScanHook(call.identifiers, adapter.shape)]}, follow_redirects=False, trust_env=False, timeout=httpx2.Timeout(connect=5, read=left, write=left, pool=5))`, `adapter.call`, and settle in `finally`. **The hook's verdict wins over any exception the SDK raised** (Review Focus 2): anthropic wraps the hook's `EgressRefused` in `APIConnectionError`, openai lets it through unwrapped, and both must give `egress_blocked`. `ScanHook` is the only request hook on the client (a test pins it), so nothing can change the request after the scan. A version check on `model_id` (`_SAFE_VERSION`) gives `malformed` when it fails. `insufficient_evidence` gives `insufficient`. An `ok` reply gives `Generation(available=True, response=answer, cited_chunk_ids=citations, retrieved_chunk_ids=sent ids, model_used=f"{provider}/{model}", versions={"cca": f"{provider}/{model}@{model_id}", "cca_route": "cloud"}, egress=provider, cloud_outcome="ok", tokens…, handle=orch)`. Anything else gives `Generation(available=False, failed_agents=("CCA",), egress=provider if hook.sent else "none", cloud_outcome=<code>)`. It carries no `versions`, so the decision has no `cca_route` and the result no `generated_by` (departure 15). Time out of the stage gives `timeout`. `auth_failed` also logs `cloud.auth_failed provider=<p> status=<n>` at `ERROR` and counts `mhai_cloud_alerts_total{provider,kind="auth_failed"}` (§11: "also alerts"); `unconfigured` uses the same counter with `kind="unconfigured"` (Task 10).
  - **Budget (ruling C7):** `estimate_tokens(len(body_bytes), max_output=MAX_OUTPUT_TOKENS)` on the exact UTF-8 bytes, and the request sends that same cap as `max_tokens`/`max_output_tokens` (a test asserts the reserved cap equals the sent one for each adapter; check that the cap also bounds billed reasoning tokens, xAI included). A `ValueError` from the estimate is `budget_exhausted`, with nothing reserved or sent.
  - `verify(generation)`: `_escalated` unless available, then `_verify_with(handle, generation)`, exactly as local.
  - `model_versions()`: `local_versions` without `cca` (the generation supplies it), so `dense` and `nli` stay recorded.
- `build_cloud_clients(settings: CloudSettings, build, local_versions) -> dict[str, CloudModelClient]`: one shared `httpx2.HTTPTransport(proxy=settings.proxy_url, verify=tls_context(), trust_env=False, retries=0)`, wrapped in `_SharedTransport` per attempt. `tls_context()` builds an explicit `ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)` (hostname check on, `CERT_REQUIRED`, TLS 1.2 at least) loaded from OpenSSL's compiled-in paths (`ssl.get_default_verify_paths().openssl_cafile`/`openssl_capath`), never from `SSL_CERT_FILE`/`SSL_CERT_DIR`: `verify=True` would use `httpcore2.default_ssl_context`, which reads those variables unless the transport itself has `trust_env=False` (the transport's default is `True`), and `ssl.create_default_context()` reads them too. Task 4 also refuses both variables at start once a provider is enabled. The trust store is the system's CA bundle, not certifi; adapters by name (Tasks 11–13 register them; until then `build_cloud_clients` refuses an enabled provider with no adapter at start).

- [ ] **Step 1: Write the failing tests.** `FakeAdapter` doesn't just return a scripted reply: its `call` POSTs a JSON rendering of the whole `CloudPrompt` (`{"system", "chunks": [{"id", "text"}], "question", "context": {…}}`, with its own `shape`) through the `httpx2.Client` it was given, then returns the scripted `CloudReply`. So the scan hook and `CapturingTransport` see real bytes. `ScriptedTransport` answers from a script. The module sets `pytest.mark.disable_socket`.
  - `test_ok_reply_is_verified_locally` (`_verify_with` is called with the cloud answer; versions as above);
  - `test_never_calls_generate_with_rag_or_the_generator` (deviation 13);
  - `test_every_failure_escalates_without_score` (A12, parametrised over every outcome but `ok`: `available=False`, `failed_agents=("CCA",)`, and through `_verify_with` gives `ESCALATE`, `verified=False`, `trust_score=None`);
  - `test_hook_refusal_is_egress_blocked_not_unavailable_and_not_retried` (Review Focus 2: the fake SDK wraps the hook's exception in its own `APIConnectionError`);
  - `test_retries_only_retryable_then_unavailable` (three 503s → `unavailable`, 3 attempts, 3 reservations each settled);
  - `test_reservation_settled_on_every_failure_path` (Review Focus 4: an exception in `adapter.call`, a timeout, a malformed reply; each `settle` called once);
  - `test_budget_exhausted_sends_nothing`;
  - `test_unsafe_model_id_is_malformed`;
  - `test_patient_id_or_no_call_refused`;
  - `test_prompt_holds_only_allowed_parts` (the capturing transport's body: system, ≤ 5 chunk texts with their ids, the masked question, `disease_category`, `risk_score`; nothing else, no ids from the claim);
  - `test_per_call_state_not_on_client` (two concurrent `generate` calls with different identifiers block on their own identifiers only: K10);
  - `test_egress_none_when_nothing_sent`;
  - `test_dry_render_sends_nothing_and_prescans` (the render transport captures a body, the real transport sees nothing, and a hit refuses before `reserve`);
  - `test_shared_transport_survives_attempts` (after three attempts, each with its own client closed, the shared transport is still open and serves a fourth: M8);
  - `test_auth_failed_alerts` (`caplog` has `cloud.auth_failed` with no key or text; the counter increments);
  - `test_fake_adapter_bodies_are_captured` (each `generate` that reaches the send puts exactly one body per attempt into `CapturingTransport`);
  - `test_worst_case_question_length` (a question of `MAX_MASKED_QUESTION` characters, all non-ASCII with diacritics, with five 2,000-character chunks: an SDK encoding with `ensure_ascii` can exceed `MAX_BODY_BYTES`, which gives `egress_blocked` (`too_large`), never a send; the test records the longest such question that is still sent).
  - `test_tls_context_ignores_ssl_cert_env` (with `SSL_CERT_FILE`/`SSL_CERT_DIR` set to a throwaway CA, `tls_context()` and the shared transport's context load no CA from them: `get_ca_certs()` is the same as without the variables; `check_hostname` is on and `verify_mode` is `CERT_REQUIRED`);
  - `test_no_trace_context_sent` (with a real OpenTelemetry tracer provider installed globally and a span current, the bodies' requests carry no `traceparent`, `tracestate` or `baggage` header. anthropic 2.3.0 sends the call span's trace context with every attempt unless its `open_telemetry` option has `propagation: False` (`anthropic/_tracing.py`; `False` alone turns off only spans); the client passes `{"propagation": False, "traces": False}`, and `ScanHook` refuses a request that still carries one of those headers (`egress_blocked`, rule `trace_header`) so a later SDK change fails closed. openai 3.37.0 has no OpenTelemetry dependency; the test covers both through the fake adapter and Tasks 11 and 12 repeat it with each SDK);
- [ ] **Step 2: Run (Python, remote):** `uv run pytest -q tests/boundary/cloud/test_client.py tests/boundary/test_models.py` → fails.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the tests and the whole `tests/boundary -m "not boundary"`; lint, format, mypy.
- [ ] **Step 5:** Commit: "CloudModelClient: local retrieval and verification, a gated, scanned, budgeted cloud draft".

### Task 10: `ModelRouter` and `run_query` on both paths

**Files:**
- Modify: `intelligence/boundary/models.py` (`ModelRouter`, `build_model_router`), `intelligence/boundary/jobs_repo.py` (`_CLAIM`, `Claim.model_route`, `Decision.egress`, `Decision.cloud_outcome`, the fact payload, `guardrail_report`), `intelligence/boundary/jobs/run_query.py`, `intelligence/boundary/services/pipeline.py`, `tests/boundary/test_run_query.py`, `tests/boundary/test_models.py`
- Create: `tests/boundary/cloud/test_router.py`, `tests/boundary/cloud/test_property.py`

**Interfaces:**
- `jobs_repo._CLAIM` returns, after `retry_count`, `mhai_effective_model_route(hospital_id, model_route)`. The rule is core's function (departure 14; Task 1's `TestEffectiveRoute`), and Python only calls it. `Claim.model_route: str = "local"`. A value outside `vocab.ROUTES` is treated as `unconfigured`.
- `ModelRouter(local: ModelClient, cloud: Mapping[str, ModelClient])`: `client_for(route: str) -> tuple[ModelClient, str]` returns the client and its provider (`"local"` for local). A cloud route whose provider isn't in `cloud` gets `_Unconfigured(local)`: it masks with the local masker, and its `generate` returns `cloud_outcome="unconfigured"`, logs `cloud.unconfigured provider=<p>` at `ERROR` and counts `mhai_cloud_alerts_total{provider,kind="unconfigured"}`. Its `model_versions()` is the local client's without `cca`, and its generation carries no `versions`, so the decision has no `cca_route`. `build_model_router(settings) -> ModelRouter`: `build_model_client(settings)` for local; when `settings.cloud.providers` is not empty, `build_cloud_clients(settings.cloud, build, versions)`. Here `(build, versions)` is `local_parts()` when the local client is `orchestrator`, and `stub_parts()` when it is `stub` (development only). `stub_parts()`, in `models.py`, gives a builder whose orchestrator has a keyword retriever over the job's chunks (no model download) and a stub HGA that returns the stub client's fixed verification, with versions `{"dense": "stub", "nli": "stub"}`. The `cloud_fake` tests use the same builder. `RunQuery.__init__(db, models: ModelClient | ModelRouter, …)` wraps a plain client in `ModelRouter(models, {})`, so existing callers and tests are unchanged.
- `run_query._pipeline(job, claim)`: `client, provider = router.client_for(claim.model_route)` before masking; every stage uses `client`. The reviewer copy is `_plain_tree(context)` (deviation 8). On a cloud route: `identifiers = collect_identifiers(claim.question, patient)`, made on the event loop from the raw row and held in a local variable only. `call = CloudCall(identifiers, DbBudget(db, job.v, loop), stage_deadline=monotonic() + min(120, job.v.exp - clock() - 90))`, and `generate(question, context, None, chunks, call=call)`. A stage deadline ≤ 0 gives `timeout` with nothing sent. On local, `call=None`.
- `_decision(client, provider, g, v)`: `model_versions = {**client.model_versions(), **g.versions}`, then `_clean_versions`. `cca_route: local` is added only when `provider == "local"` (the job really ran on the local client), so `_Unconfigured` and a failed cloud generation record no route. `guardrail_report` gains `cloud_outcome`, `tokens_in` and `tokens_out` on a cloud route. `Decision.egress = g.egress` and `Decision.cloud_outcome = g.cloud_outcome`. `jobs_repo.complete`'s payload adds `egress` always and `cloud_outcome` when set.
- `pipeline.job_handlers` builds the router with `build_model_router(settings)`. `ApproveCorrection` is unchanged and gets no router.

- [ ] **Step 1: Write the failing tests:**
  - in `test_run_query.py` (`boundary`), parametrise the existing suite with a fixture `client_kind in ("stub", "cloud_fake")`. `cloud_fake` is a `CloudModelClient` over a `FakeAdapter` and a stub builder (no models), with the hospital's policy set to `anthropic` and `CORE`'s trigger choosing the route. Claim, lease, `+WIP`, cancel, deadline, redelivery and the attempts cap all pass on both paths (§12).
  - new `boundary` tests: `test_route_changed_between_publish_and_claim_runs_local` (A14 and Review Focus 5); `test_message_cannot_choose_route` (a `RunQueryJob` with extra fields or a forged token claim changes nothing; the route comes from the row); `test_cloud_answer_records_provider` (A16: `model_versions["cca"]` starts `anthropic/`, `model_versions["cca_route"] == "cloud"`, the outbox payload has `egress: "anthropic"`, `cloud_outcome: "ok"`, and core's drainer accepts it); `test_cloud_failure_completes_escalated` (each outcome: status completed, decision `ESCALATE`, no trust score, `cloud_outcome` in the report and fact, no Nak); `test_unconfigured_provider_escalates_and_alerts`; `test_no_text_in_logs_spans_dead_letters_on_cloud_path` (A18: the existing `test_marker_never_in_logs_spans_or_dead_letters` pattern, with the marker in the question, the patient data, the chunk text and the fake reply, over `ok` and every failure outcome); `test_reviewer_copy_is_plain_and_masked`; `test_unconfigured_and_failed_cloud_have_no_route` (no `cca_route`, so core shows no `generated_by`); `test_local_answer_records_route_local`; `test_budget_race_through_jobs` (A11: 20 jobs against a budget for 5 → at most 5 adapter calls; the rest `budget_exhausted`).
  - `test_property.py` (`boundary`, Hypothesis, A1, at most 50 examples, derandomised): synthetic records with random synthetic names (one strategy case always uses a given name that is an ordinary word, such as `Grace`, and counts it as an expected `egress_blocked` when a retrieved chunk contains the word), MRNs (string and integer), Nigerian phone numbers, NINs, addresses and dates, in every field, at nested levels, as dict keys and in the question. It runs the whole job through a `CapturingTransport`, with `FakeAdapter` POSTing the real rendering (Task 9). Names go under identifying keys or after a title, never bare in a free-text field of core's `PatientData`: an untitled name there is the C8 residual (pinned in Task 6), which this test doesn't claim to cover. It asserts that no raw identifier value, normalised by `norm_words`, appears in any captured body (decoded as the scan decodes it), and that each job either sent a clean body or ended `egress_blocked`, whichever rule code refused it. Two assertions keep it from passing vacuously: across the run, at least 10 bodies were captured (the strategy leaves the question free of identifiers in about half the examples, so sends do happen), and a fixed clean record (`CLEAN_RECORD`, no identifiers in the question or the two context fields) is always **sent**, never blocked. PR D re-runs this test with each real adapter in place of `FakeAdapter` (Tasks 11–13).
  - unit: `test_router_picks_one_client_per_job`, `test_router_unknown_route_is_unconfigured`, `test_runquery_accepts_plain_client`.
- [ ] **Step 2: Run (Python, remote):** `uv run pytest -q tests/boundary/cloud/test_router.py` and `-m boundary tests/boundary/test_run_query.py tests/boundary/cloud/test_property.py` → fail.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the whole `tests/boundary` (both markers), `tests/security`, lint, format, mypy; `make smoke` remotely (stub, local route: unchanged).
- [ ] **Step 5:** Commit: "run_query routes each job by its claimed row, records provider, egress and outcome".

**PR C:** after Task 10, run `security-review` and open the PR.

### Part D: adapters (PR D)

Each adapter task has the same shape: the request test (exact JSON body and headers), the adapter's `shape`, built with `scan.shape_for(name, scan.leaf_values(fixed_parts))` from every part of the request outside the blocks (the model, scalars, roles, the system prompt and `ANSWER_SCHEMA` as the provider nests it; never a hand-built `Shape`, which Task 6 refuses), the reply mapping, a fixture per case, and `pytest.mark.disable_socket` on the unit module. Each task also re-runs Task 6's split-name and escape cases through the SDK's own encoder (`test_sdk_encoded_bodies_still_blocked`), and Task 10's property test with its adapter (`test_property[<provider>]`, `boundary`; the adapter's SDK talks to a `ScriptedTransport`, never a socket outside the container network). Each adapter renders the blocks in `CloudPrompt.blocks()` order (ruling R-1: question, context, then chunks) as text parts at `scan.BLOCK_ARRAYS[name]`, so `shape.patient_paths` names two exact pointers. The fixtures are synthetic: they are written from the provider's documented response shape, with ids like `msg_SYNTHETIC0001`, and are reviewed in the PR. Every fixture passes `scripts/scrub_cloud_fixture.py --check` (Task 15). Before the request test is written, the implementer reads the locked SDK version's request types. Where the design names a field (`output_config`, `text.format`, `store`, `response_format`), the test pins the field the SDK actually sends. If the SDK lacks it, stop and ask in the PR.

**Binding for Tasks 11–13: SDK retries off.** Every adapter's `make_sdk` builds its SDK client with `max_retries=0` (the SDKs default to 2). Retrying belongs to `CloudModelClient` alone, under `RetryPolicy`, a fresh reservation per attempt and the stage deadline; an SDK retry would resend the body with no reservation and could mislabel a hook refusal (C14). `test_client.py::test_adapters_turn_sdk_retries_off` builds every registered adapter's SDK client and fails if its `max_retries` is not 0. It passes vacuously until an adapter is registered, so each adapter task must check it runs over that adapter. TODO (Tasks 11–13): register the adapter in `ADAPTERS` and confirm the guard test covers it.

### Task 11: The Anthropic adapter

**Files:**
- Create: `intelligence/boundary/cloud/anthropic_adapter.py`, `tests/boundary/cloud/test_anthropic.py`, `tests/fixtures/cloud/anthropic/{ok,refusal,max_tokens,invalid_json,extra_keys,unknown_citation,markers_mismatch,link,overloaded_529,rate_429,server_503,unauthorized_401,redirect_301}.json`
- Modify: `intelligence/boundary/cloud/client.py` (register `anthropic`)

**Interfaces:**
- `AnthropicAdapter.make_sdk` gives `anthropic.Anthropic(api_key=key.reveal(), base_url=BASE_URLS["anthropic"], http_client=…, max_retries=0)`.
- `call` uses Messages: `model`, `max_tokens=1500`, `system=SYSTEM_PROMPT`, a single `user` message whose content is text blocks only, at `scan.BLOCK_ARRAYS["anthropic"]` (`/messages/0/content`), in `CloudPrompt.blocks()` order (ruling R-1: the masked question, the two context fields, then the chunks, each delimited `<<chunk KB#nnnn>> … <<end>>`), structured output with `ANSWER_SCHEMA` (`output_config.format` per §3.1, via `messages.parse` or the raw `create` with the schema), effort `medium` (§16). It never sends `temperature`, `top_p`, `top_k`, `metadata`, `tools`, `tool_choice`, the `fallbacks` parameter, files, images, documents, batches or prior turns.
- Mapping: `stop_reason == "refusal"` gives `refused`; `max_tokens` gives `truncated`; a parse failure gives `malformed`; `usage.input_tokens`/`output_tokens`; `model` from the response is `model_id`; 529 is retryable.

- [ ] **Step 1: Write the failing tests:** `test_request_shape` (A8: the captured body's keys are exactly the allowed set; no sampling fields, no `metadata`; text blocks only, which fails if any `image`, `document` or `file` block can be produced from any `CloudPrompt`; URL `https://api.anthropic.com/v1/messages`); `test_fixture_outcomes` (one case per fixture, through `CloudModelClient` with the real scan); `test_301_not_followed` (A17); `test_sdk_error_logged_by_type_and_status_only` (A10: the fixture's 401 body echoes the request; `caplog` and the exception string hold neither the key nor any prompt text); `test_hook_refusal_is_egress_blocked_not_unavailable_and_not_retried` (with the real SDK); `test_no_socket_opened` (A5: the adapter called with a plain `str` or `dict` raises before the transport; `pytest-socket` proves no connection); `test_sdk_encoded_bodies_still_blocked` (C-1: Task 6's split, escaped and zero-width names, in a body built by this SDK, are blocked); `test_shape_covers_every_emitted_path` (every path in the rendered request is in `shape.allowed_paths`; the question is exactly at block 0 and the context at block 1, the chunks follow from block 2, with 0, 1 and 5 chunks; a chunk containing "200000 IU" is sent, and a name in the question is blocked); `test_property[anthropic]` (`boundary`: Task 10's property test with this adapter).
- [ ] **Step 2: Run (Python, remote):** `uv run pytest -q tests/boundary/cloud/test_anthropic.py` → fails.
- [ ] **Step 3:** Implement and write the fixtures.
- [ ] **Step 4:** Run the tests, `tests/boundary/cloud` in full, lint, format, mypy.
- [ ] **Step 5:** Commit: "Anthropic adapter: Messages with strict structured output, no state, no sampling fields".

### Task 12: The OpenAI adapter

**Files:**
- Create: `intelligence/boundary/cloud/openai_adapter.py`, `tests/boundary/cloud/test_openai.py`, `tests/fixtures/cloud/openai/{ok,refusal,incomplete,invalid_json,extra_keys,unknown_citation,markers_mismatch,link,rate_429,server_503,unauthorized_401,redirect_301}.json`
- Modify: `client.py` (register `openai`)

**Interfaces:**
- `make_sdk` gives `openai.OpenAI(api_key=…, base_url=BASE_URLS["openai"], http_client=…, max_retries=0)`.
- `call` uses Responses: `model`, `instructions=SYSTEM_PROMPT`, `input`: one `user` message whose content is `input_text` parts only, in `CloudPrompt.blocks()` order, at `scan.BLOCK_ARRAYS["openai"]` (`/input/0/content`), `max_output_tokens=1500`, `store=False`, `text.format` `json_schema` with `strict: true` and `ANSWER_SCHEMA`. Never `previous_response_id`, `conversation`, `tools`, `user`, `metadata`, `safety_identifier`, `prompt_cache_key` or `background`.
- Mapping: a `refusal` content item gives `refused`; `status == "incomplete"` gives `truncated`; usage from `usage.input_tokens`/`output_tokens`; `model` from the response is `model_id`.

- [ ] **Step 1: Write the failing tests:** as Task 11, plus `test_store_false_and_no_state_fields` (A8).
- [ ] **Step 2: Run (Python, remote):** `uv run pytest -q tests/boundary/cloud/test_openai.py` → fails.
- [ ] **Step 3:** Implement and write the fixtures.
- [ ] **Step 4:** Run the tests, lint, format, mypy.
- [ ] **Step 5:** Commit: "OpenAI adapter: Responses with store=false and strict JSON schema".

### Task 13: The xAI adapter

**Files:**
- Create: `intelligence/boundary/cloud/xai_adapter.py`, `tests/boundary/cloud/test_xai.py`, `tests/fixtures/cloud/xai/{ok,ok_no_zdr_header,ok_zdr_false,refusal,length,invalid_json,unknown_citation,link,rate_429,server_503,unauthorized_401,redirect_301}.json`
- Modify: `client.py` (register `xai`)

**Interfaces:**
- `make_sdk` gives `openai.OpenAI(api_key=…, base_url=BASE_URLS["xai"], http_client=…, max_retries=0)`.
- `call` uses Chat Completions through `with_raw_response`, so the headers are read: `messages`: the system message at `/messages/0`, then one `user` message whose content is `text` parts only, in `CloudPrompt.blocks()` order, at `scan.BLOCK_ARRAYS["xai"]` (`/messages/1/content`), `max_tokens=1500`, `response_format` `json_schema` with `strict: true`. No `tools`, `user`, `store`, `search_parameters` or image parts.
- Mapping: the `x-zero-data-retention` response header not equal to `true` (case-insensitive) gives `retention_unconfirmed`, and the reply is discarded unread, outside `APP_ENV=development`. A `refusal` field gives `refused`; `finish_reason == "length"` gives `truncated`.

- [ ] **Step 1: Write the failing tests:** as Task 11, plus `test_retention_header_missing_or_false_discards` (A9: the answer text never reaches `validate_answer` or a log) and `test_retention_header_relaxed_only_in_development`.
- [ ] **Step 2: Run (Python, remote):** `uv run pytest -q tests/boundary/cloud/test_xai.py` → fails.
- [ ] **Step 3:** Implement and write the fixtures.
- [ ] **Step 4:** Run the whole `tests/boundary/cloud` and `tests/security`, lint, format, mypy.
- [ ] **Step 5:** Commit: "xAI adapter: strict schema, zero-retention header checked on every reply".

**PR D:** after Task 13, run `security-review` and open the PR.

### Part E: deployment, the live smoke and docs (PR E)

### Task 14: The egress proxy

**Files:**
- Create: `deploy/egress-proxy/squid.conf`, `deploy/egress-proxy/allowed-hosts.example`, `deploy/egress-proxy/test-egress.sh`
- Modify: `deploy/compose.yml` (service `egress-proxy`, image `ubuntu/squid@sha256:<digest>`, on `backend` and a new `egress` network (not internal), no published port, read-only root filesystem, `mem_limit`, the allowed-hosts file mounted read-only; the commented `pipeline` block gains `HTTPS_PROXY: http://egress-proxy:3128`, the three `INTELLIGENCE_CLOUD_*` settings commented out, the key-file secret mount, and stays on `backend` only), `services/core/cmd/core/compose_test.go`, `deploy/README.md`

**Interfaces:**
- `squid.conf`: `http_port 3128`. `acl providers dstdomain -n "/etc/squid/allowed-hosts"` (`-n`: no reverse DNS lookup, so an IP whose PTR record names an allowed host doesn't match). `acl ip_literal dstdom_regex -n ^[0-9.]+$ ^\[?[0-9a-fA-F:]+\]?$`, `acl SSL_port port 443`. Then `http_access deny ip_literal`, `http_access allow CONNECT SSL_port providers`, `http_access deny all`. No caching, no `ssl_bump`, `forwarded_for delete`, `via off`. The access log carries method, host, port and status only (`logformat` without the URL path). `allowed-hosts` holds one line per enabled provider, from `vocab.HOSTS`; the example file lists none.
- `test-egress.sh` (run remotely with the stack up, allowed hosts set to `api.anthropic.com`): `CONNECT example.com:443` → 403; `CONNECT api.openai.com:443` → 403; `CONNECT api.anthropic.com:80` → 403; `CONNECT 1.2.3.4:443` → 403 and `CONNECT [2001:db8::1]:443` → 403 (IP literals, whatever their PTR); `CONNECT api.anthropic.com:443` → 200; from a container on `backend` only, a direct TCP connection to `1.1.1.1:443` fails.

- [ ] **Step 1: Write the failing tests:** in `compose_test.go`: `TestEgressProxyOnlyServiceOnEgressNetwork` (only `egress-proxy` joins `egress`), `TestPipelineNeverOnEgress` (the pipeline block, when uncommented by the test's parser of the commented YAML, is on `backend` only), `TestEgressProxyPinnedByDigest`, `TestNoProviderKeyInAnyEnvironment` (no service's `environment` has a `*_KEY` or `*_API_KEY` for a provider; core has no `INTELLIGENCE_CLOUD_*`). `test-egress.sh` (A15) fails until the service exists.
- [ ] **Step 2: Run (Go, remote):** `go test -race ./cmd/core/ -run Compose` → fails. Run `test-egress.sh` remotely → fails.
- [ ] **Step 3:** Write the config, the compose service and `deploy/README.md`'s "Cloud models" section: enabling a provider (§15 steps 1–5), the key file (mode 0400, owner the worker user), rotation every 90 days and on suspicion (write the new file, rolling restart, revoke the old key), the provider-side spend limit and endpoint scope, and switching off (policy to `none`, then remove the key file and the host from `allowed-hosts`).
- [ ] **Step 4:** Run both test sets remotely.
- **Check (Task 4 review M5):** `HTTPS_PROXY` is a process-wide name. In the pipeline worker, `requests`, `boto3` and the Hugging Face hub (anything with `trust_env=True`) would also send HTTPS through the egress proxy, which refuses every other host. That fails closed, but it can break model downloads or the model store when cloud is on. Before this commit, confirm nothing else in the pipeline worker needs HTTPS at run time, or rename the setting to `INTELLIGENCE_CLOUD_PROXY` (Task 4's settings and tests, and this task's compose block).
  - **As built (PR E):** renamed. The worker reads `INTELLIGENCE_CLOUD_PROXY`; `HTTPS_PROXY` is neither read nor needed (the cloud transport has `trust_env=False`), and the compose block never sets it (`TestPipelineNeverOnEgress`).
- **Also (PR C, Task 9 concern):** with a provider enabled, start-up refuses when OpenSSL's CA file and CA directory are both missing or empty (`settings.openssl_ca_paths`; `tls_context` refuses too), rather than failing every call on TLS.
- [ ] **Step 5:** Commit: "An egress proxy that allows only the enabled providers' API hosts on 443".

### Task 15: The manual live smoke and fixture recording

The key must never be on the shared build server. steven-reid and mt5-lab run agent jobs with the host's Docker socket, which is effectively root there. So the live smoke is **a step a person runs on their own machine**, outside any container. It uses a dedicated smoke key: short-lived, spend-capped and revoked afterwards. The key is read from standard input, so it never reaches a file, the environment or the shell history.

**Files:**
- Create: `services/intelligence/scripts/cloud_smoke.py`, `services/intelligence/scripts/scrub_cloud_fixture.py`, `tests/boundary/cloud/test_smoke_guard.py`
- No pytest configuration change: `testpaths = ["tests"]` (pyproject) already keeps `scripts/` out of collection.

**Interfaces:**
- `cloud_smoke.py --provider anthropic|openai|xai --model <id> --key-stdin [--record <dir>]`.
  - **Refusals, before reading the key:** it refuses unless `MHAI_CLOUD_SMOKE=1`. It refuses when it is inside a container (`/.dockerenv` or `/run/.containerenv` exists, or `container` is set), when `CI` or `GITHUB_ACTIONS` is set, and when any of `SDK_ENV_REFUSED` (Task 4) is set. It refuses a `--key-file` option outright (there is none). Each refusal exits 2 and names the reason.
  - **The key:** read with `getpass` when stdin is a terminal, otherwise one line from stdin. It is checked as `load_api_key` checks a file (no placeholder, no whitespace, length), held only as `ApiKey`, and dropped at exit.
  - **The run:** the real `CloudModelClient`, adapter, gate and scan, built with `stub_parts()` (Task 10) over three fixed synthetic shared guideline chunks (in the script) and a `FakeBudget` capped at 3 attempts. The transport is direct, with no proxy (it runs on a person's machine, not the deployment), TLS verification on and no redirects. It sends one fixed synthetic question with a synthetic `patient_data` (recognisably synthetic, with `SYN-` identifiers).
  - **The output:** it prints only provider, outcome, `versions["cca"]`, token counts, the retention header (xAI) and whether the answer validated.
  - **`--record`:** writes the raw reply, passed through the scrubber, under `tests/fixtures/cloud/<provider>/recorded/`.
- `scrub_cloud_fixture.py <in> <out>` / `--check <file>…`: removes or replaces response ids, request ids, organisation and project ids, `date`, `x-request-id`, `request-id`, `cf-ray`, `openai-organization`, `anthropic-organization-id`, rate-limit headers and timing fields with fixed synthetic values. `--check` fails on any field outside an allow-list, any string matching the key shapes, and any id not `SYNTHETIC`. CI runs `--check` on every committed fixture.

- [ ] **Step 1: Write the failing tests** (subprocess, no network, socket-disabled):
  - `test_smoke_refuses_without_env` (exit 2, the message names the variable);
  - `test_smoke_refuses_in_container_or_ci` (with `CI=1`, with `container=podman`);
  - `test_smoke_refuses_sdk_env`;
  - `test_smoke_has_no_key_file_option`;
  - `test_smoke_never_prints_or_logs_the_key` (the test imports the script and calls `main(argv, stdin=<fake key>, transport=ScriptedTransport(...))`. The script has no command-line option that loads code or a transport. stdout, stderr and `caplog` don't contain the key);
  - `test_smoke_cli_has_no_transport_option` (`--help` lists only `--provider`, `--model`, `--key-stdin` and `--record`);
  - `test_smoke_not_collected_by_pytest`;
  - `test_scrubber_check_passes_committed_fixtures`;
  - `test_scrubber_check_fails_on_key_shape_and_real_ids`.
- [ ] **Step 2: Run (Python, remote):** `uv run pytest -q tests/boundary/cloud/test_smoke_guard.py` → fails.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the tests remotely. **Manual, once H1 and H7 are done:** the smoke-host owner (H7), on their own machine, outside any container, never on steven-reid or mt5-lab and never in CI:
  1. Create a dedicated smoke key in the demo workspace, spend-capped at USD 5, set to expire within a day where the provider allows it.
  2. Run `MHAI_CLOUD_SMOKE=1 APP_ENV=demo uv run python scripts/cloud_smoke.py --provider anthropic --model claude-opus-5-5 --key-stdin --record tests/fixtures/cloud/anthropic/recorded/`, pasting the key from a password manager.
  3. Check two things the synthetic fixtures can't (PR D review), and paste the result in the PR:
     - **Anthropic auth (review M1):** anthropic 2.3.0 sends the key as `Authorization: Bearer <key>`, not `x-api-key`. The anthropic run must return `ok` (not `auth_failed`), which confirms the API accepts it.
     - **xAI reasoning under the cap (review I1):** run the xai smoke with a reasoning model and record `usage.prompt_tokens`, `completion_tokens`, `completion_tokens_details.reasoning_tokens` and `total_tokens`. If `total_tokens - prompt_tokens` exceeds `max_tokens` (1500), `max_tokens` does not cap reasoning: the worker then logs `cloud.usage_overrun` and counts `mhai_cloud_usage_overrun_total` (an alert above 10% over), and the cap must be revisited before xAI is enabled for a hospital.
  4. Revoke the smoke key at the provider straight after.
  5. Commit the scrubbed fixtures by PR. CI runs the adapter suite against them, and the printed summary (no text) is pasted in the PR.

  Without H1 and H7, the PR says the live run is pending and merges with the synthetic fixtures.
- **As built (PR E):** `main(argv, *, stdin, transport, env, markers)`: the last four are for the tests only; the command line can't set them. Before the key it also refuses a bad model (`settings.model_problem`, shared with the worker) and, with the real transport, a missing CA bundle (`settings.openssl_ca_paths`). The key is checked by `apikey.key_problem` (shared with `load_api_key`). It also prints the reply's numeric `usage.*` fields and, for xAI, `over_cap` (output over `MAX_OUTPUT_TOKENS`), for step 4's check. Exit 0 for `ok`, 1 for another outcome, 2 when refused. `--record` writes `<outcome>-<model>.json`, never overwrites, and writes nothing that fails the scrubber's check; `test_recorded_fixtures_pass_the_adapter_parsers` runs every recorded file through the client and adapter.
- [ ] **Step 5:** Commit: "A manual live smoke run by a person off the build server, key from stdin, and a fixture scrubber".

### Task 16: Docs, the human gates in TODO, and the abuse-case check

**Files:**
- Create: `tests/boundary/cloud/test_abuse_cases.py`
- Modify: `TODO.md`, `CHANGELOG.md`, `ARCHITECTURE.md` (the cloud route, the egress proxy), `services/intelligence/README.md` ("Cloud models": settings, what leaves, outcomes), `CONTRIBUTING.md` (clinical change control gains `gate.SYSTEM_PROMPT`, `CloudContext`'s allow-list, `scan.IDENTIFYING_KEYS` and the configured cloud model ids), `docs/specs/2026-11-03-cloud-model-providers-design.md` (§18 "As built", listing this plan's departures), `.claude/skills/security-review/SKILL.md` (two new classes: an SDK wrapping a pre-send refusal into a retryable error; a tagged type losing its tag through a copy made for another purpose)

**TODO.md entries (exact wording, each owned by a person, under "What's left"):**
- **Cloud models: before any real patient data (human gate, design §13.6).** (1) The masker upgrade (Nigerian identifiers and a local NER for names, including names without a title), measured on a synthetic Nigerian PII set at recall ≥ 0.99 for names and ≥ 0.999 for numeric identifiers, signed off by the privacy lead. (2) The legal check: each hospital's data agreement allows a processor outside Nigeria; NDPA 2023 cross-border transfer and processor terms; each provider's DPA and zero-retention terms signed; the agreement reference recorded per hospital. (3) The clinical evaluation of cloud answers on the synthetic evaluation set (pass and escalate rates, NLI support), signed off clinically. (4) Core supplies keyed hashes of the patient's name tokens with each job, so the egress scan can match the patient's name without the worker holding it (ruling C5). Until all four are done, cloud generation runs on demo and synthetic data only. No code flag represents this gate.
- **Cloud models: names without a title reach the provider today (ruling C8).** PIIMasker finds names only after a title (Dr, Mr, Mrs, Ms, Miss, Prof, Patient), and core's patient data has no name key, so "Adaeze Okafor has chest pain" passes the egress scan and is sent. `test_untitled_name_on_core_shape_refused` pins this as a strict expected failure. No real patient data goes to a cloud provider until both the masker upgrade (an untitled-name detector meeting the recall bar in item (1) above) and core's keyed name-token hashes (item (4)) are in. Demo and synthetic data only until then.
- **Cloud models: stale budget reservations.** A crashed worker, a reserve that timed out after core committed, or a failed settle leaves a reservation that holds tokens until the month ends. Build a core-side reaper that settles as fully used any `cloud_reservations` row older than about one hour (by `created_at`), and add an alert on `mhai_cloud_settle_failures_total`.
- **Cloud models: measured over-blocking and residuals for the H2 evaluation (ruling C4).** Over-blocking kept on purpose (fails closed; measure the false-refusal rate): any value under a `name` key is an identifier, so `{"name": "Malaria"}` blocks every malaria chunk and FHIR `system: "phone"` becomes an identifier; PIIMasker's 5-digit ZIP rule makes any 5-digit lab value an identifier; a given name that is an ordinary word (Grace, Joy, Hope, Peace, Favour, Patience) blocks guideline text containing it; "3 march 20" in a question reads as a date; "hospital no longer …" reads as a record number; three spaced numbers ending in 19xx or 20xx read as a date. Residuals the scan misses (measure the miss rate): an unrecorded 6-digit number written with a space or one hyphen ("445 812", "123-456"); gaps of `| * · ' ~` or of 4 or more characters between digit groups; a spelt-out NIN or unrecorded number one digit at a time; numbers written as words; confusables outside the scan's map (the rest of UTS #39) and a whole name in a script with no Latin lookalike; a name split inside a token ("Ada eze") or run together ("AdaezeOkafor"); "12 May 84" and "March the 12th, 1984"; "patient no", "reg no", "M R N", "MR# 4471" and "hospital num"; long non-ASCII questions near the length limit, which are refused as too large rather than sent.
- **Cloud models: egress isolation (design §16 question 6).** Before real data, decide whether the risk that a compromised worker sends data to an allowed provider (C9) is accepted, or the cloud call moves into its own service.
- **Cloud models: the live smoke and recorded fixtures** (waiting on H1).
- The existing "Cloud models" line is replaced by the build status.

**Other Task 16 items (from the PR A and PR B reviews):**
- `docs/specs/2026-11-03-cloud-model-providers-design.md` §5 (the table and the audit action) and §13.6 give `agreement_ref` as `^[A-Z0-9][A-Z0-9_.-]{2,63}$`. As built (ruling C2), it is `^(DEMO-SYNTHETIC|(AGR|DPA)-[0-9]{4}-[0-9]{2,8})$` in Go, the audit schema and 00018's `CHECK`, so a patient pseudonym or a name can't land in the permanent audit chain. Update the design text and H3's wording ("the `agreement_ref` format").
- `services/intelligence/README.md` "Cloud models" says that any `OPENAI_*`, `ANTHROPIC_*`, `AZURE_OPENAI_*` or `XAI_*` variable refuses start-up for every worker, cloud on or off (a developer shell exporting `OPENAI_API_KEY` will hit it), and that with a provider configured, logging changes: records are redacted at creation (`args=None`, exception text pre-rendered), warnings go through logging, uncaught exceptions are logged as `CRITICAL`, and the httpx INFO request lines are off.
- `CHANGELOG.md` records `boundary/cloud_repo.py` added to `test_no_direct_db.py`'s `REPOS` (a security-test change).

- [ ] **Step 1: Write the failing test** `test_every_abuse_case_has_a_test`: it reads this plan's "Abuse cases → tests" table and checks each named test exists under `services/intelligence/tests/` or `services/core/` (by name, with `grep`). Also `test_todo_keeps_the_real_data_gate`: TODO.md contains the gate's heading, its four numbered items and the untitled-names entry (C8), so a later edit that removes them fails CI.
- [ ] **Step 2: Run (Python, remote):** `uv run pytest -q tests/boundary/cloud/test_abuse_cases.py` → fails until the names line up; fix the test names, not the table.
- [ ] **Step 3:** Write the documentation.
- [ ] **Step 4:** Full remote run: Go unit and integration, Python `-m "not models"`, `-m boundary`, `tests/security`, lint, format, mypy, `make smoke`, `test-egress.sh`. Then run `security-review` on the branch.
- [ ] **Step 5:** Commit: "Cloud models: docs, the human gates before real data, and the abuse-case check".

**PR E:** open the PR. The demo switch-on (design §15) is a person's step after H1 and H5, not this plan's.

---

## Security and threat model

The design's §13 is the threat model; this section makes it concrete for the build and adds what the code review found.

### Assets

Patient data before masking (the question and `patient_data`); the masked text (still a clinical question); the provider API keys; hospital separation; the audit trail (`model_versions`, `egress`, `cloud_outcome`, the policy changes); the provider accounts' money; clinical safety (a steered or unchecked answer must never pass as final); the egress route itself (the worker's first way out of the network).

### Actors and trust boundaries

Actors: clinicians, hospital admins, platform admins, auditors, the BFF, core, the pipeline worker, the egress proxy, the three providers (processors outside our network), the operator who runs the CLI, anyone holding a key, anyone who can write text that retrieval selects, and a compromised SDK dependency.

| Crossing | What is checked |
|---|---|
| platform admin → policy | platform audience, MFA, `platform_admin`, step-up, provider enabled in `CORE_CLOUD_PROVIDERS`, `agreement_ref` shape, audited in the same transaction (Task 2) |
| operator → policy (CLI) | `mhai_provision`, provider enabled, audited with no actor (Task 2) |
| gateway insert → `clinical_queries` | definer trigger sets `model_route`; the supplied value is ignored; no role can update it (Task 1) |
| row → worker (claim) | RLS for the token's hospital; the route re-checked by core's `mhai_effective_model_route` in the claim statement (Tasks 1, 10) |
| worker → budget | `mhai_cloud_reserve`/`settle` for `mhai_current_hospital()` only, atomic, one settle per reservation id, fail closed; SQL only in `cloud_repo` (Tasks 1, 7) |
| worker → adapter | `CloudPrompt` from tagged `MaskedText`, an allow-listed `CloudContext`, sealed `SharedChunk`s (Task 5) |
| adapter → transport | `ScanHook` on the exact bytes, parsed as JSON and decoded: shape, identifiers everywhere, patterns on the patient-derived strings, size; a verdict that overrides the SDK's error mapping (Tasks 6, 9) |
| transport → proxy → provider | fixed base URL, no redirects, `trust_env=False`, SDK environment variables refused, TLS verified, the proxy's host allow-list on 443 with no reverse DNS and no IP literals (Tasks 4, 14) |
| person → provider (live smoke) | the person's own machine, outside any container, a dedicated short-lived spend-capped key from stdin, revoked after (Task 15) |
| provider → worker | untrusted: strict schema, our validation, citations ⊆ sent ids, the retention header (xAI), local NLI and HGA, escalation (Tasks 8, 11–13) |
| worker → core (fact) | closed schema with `egress` and `cloud_outcome` (Task 3) |

### Threats (STRIDE) and mitigations

The design's C1–C12 carry over unchanged; their tests are A1–A18. The plan adds:

| # | STRIDE | Threat | Mitigation | Tests |
|---|---|---|---|---|
| C13 | Tampering | **A copy loses the tag check:** a `MaskedText` copied or pickled into a shared cache, or the reviewer copy reused as a prompt | `MaskedText` refuses copy and pickle; the reviewer copy is plain `str` (deviation 8), which `CloudPrompt` refuses | K1, `test_reviewer_copy_is_plain_and_masked` |
| C14 | Information disclosure | **The SDK maps a pre-send refusal to a retryable error**, and the body is retried or the outcome mislabelled | the hook's own verdict wins; `max_retries=0`; our retry table never retries `egress_blocked` | Review Focus 2 tests (Tasks 9, 11–13) |
| C15 | Information disclosure | **Obfuscated identifiers** (zero-width, full-width, case) slip past the scan | NFKC, `Cf` removal, case-fold, digit-stream compare | `test_scan_sees_through_zero_width_fullwidth_and_case` |
| C16 | Elevation of privilege | **The worker reads or rewrites a budget** (its own or another hospital's) | no table grant; two definer functions bound to `mhai_current_hospital()`; settle can only lower `reserved` and raise `used` by at most what was reserved | K4, K5 |
| C17 | Denial of service | **Guideline text trips the pattern scan** and every question escalates | seed guidelines tested against the scan; outcome counted by rule code | Review Focus 1 tests |
| C18 | Repudiation | **A policy change by CLI has no actor** | `via: cli` in the payload; `mhai_provision` only; the operator's host access is the control | `TestCLIModelPolicySet` |
| C19 | Information disclosure | **A recorded fixture leaks an account id or key** | the scrubber, its `--check` in CI on every fixture, gitleaks | `test_scrubber_check_*` |
| C20 | Tampering | **A forged shared chunk** (a hospital or patient chunk relabelled as shared) | the seal key is private to the gate; only `kb_repo` calls `seal_shared_chunk` (AST test); the tag covers id and content | A6, K2 |
| C21 | Information disclosure | **JSON escaping hides an identifier from the scan** (`\n`, `\u200b` inside a name) | the body is parsed and each string decoded before normalising; tests run through each SDK's encoder | `test_escaped_and_split_names_blocked_through_sdk_encoder` (6), `test_sdk_encoded_bodies_still_blocked` (11–13) |
| C22 | Tampering | **A temporary table shadows the policy** in a definer function, so the gateway picks its own route | `search_path = pg_catalog, pg_temp` and schema-qualified names in every definer | K15 |
| C23 | Repudiation | **`generated_by` says cloud for a local answer** (a policy switched off, an unconfigured route) | derived from the decision's `cca_route`, written only by the client that ran | `TestResultGeneratedBy`, `test_unconfigured_and_failed_cloud_have_no_route` |
| C24 | Information disclosure | **The proxy admits an attacker's IP** whose PTR names an allowed host | `dstdomain -n`, IP-literal CONNECT denied | A15 (`test-egress.sh`) |
| C25 | Information disclosure / spoofing | **The smoke key is read on a shared build server** through its Docker socket | the smoke runs only on a person's machine, outside containers; key from stdin; dedicated, capped, revoked | `test_smoke_refuses_in_container_or_ci`, `test_smoke_has_no_key_file_option` |

### Controls (decided)

- **Authentication and step-up:** platform admins with MFA and step-up change a policy, and operators use the provisioning CLI. Workers act only with core's request token.
- **Authorisation:** the route is core's (a trigger from a platform-only table) and is re-checked at claim. The worker's grants are one column pair and two functions. The gateway has nothing on the policy.
- **Data handling:** what leaves is §3.3 and nothing else: masked question, two context fields, at most 5 shared chunks, a constant system prompt. Documents never leave. No text goes in logs, spans, audit facts, dead letters or metrics. Identifiers live in memory for one stage only, with `repr=False`.
- **Concurrency:** the budget reservation is one atomic statement in core's function. Settle runs in `finally`. The route re-check is in the claim statement. Per-job values are arguments (`CloudCall`), never client state.
- **Secrets:** key files only, mode-checked; placeholders and short keys are refused at start. Keys are held as `ApiKey`. A redaction filter covers every handler, SDK debug logging is off, and SDK errors are logged by type and status. Rotation is in the runbook.
- **Third parties and hosting:** three named processors, each with its zero-data-retention reference required at start (`DEMO-SYNTHETIC` only under `APP_ENV=demo`). They are reached only through the egress proxy. Real data waits for the human gate.

### Abuse-case tests

Listed in full in the next section, mapped to tasks.

### Residual risk and sign-off

- **The masker is not fit for real data** (design §13.6). The scan narrows the gap for known identifiers but can't catch an unrecorded relative's name. Accepted for synthetic data only, and gated in TODO.md (privacy lead).
- **A compromised worker** can send what it can read to an allowed provider under our key (C9). Accepted for the first build (§16 question 6); revisit before real data (TODO.md).
- **A crashed worker, a timed-out reserve or a failed settle leaves one attempt's reservation** counted as reserved until the month ends. Each is bounded by one estimate. Accepted until core's reaper (Task 16 TODO) settles stale reservations as fully used.
- **An untitled name on real patient data is sent** (C8). PIIMasker finds names only after a title, and core's `PatientData` has no name key, so "Adaeze Okafor has chest pain" passes the scan. Accepted for demo and synthetic data only: the real-data gate (Task 16) needs both the masker upgrade and core's keyed name-token hashes (C5).
- **`long_digits` misses an unrecorded number written with spaces or one hyphen** ("445 812", "123-456"). Spaces and a single hyphen no longer join digit runs outside phone and NIN shapes, which keeps vitals questions sendable. So a 6-digit identifier written with a space, and not in this patient's record, is caught neither by `long_digits` nor by the known-identifier rule. Accepted for synthetic data. It is measured as a miss rate in the H2 evaluation (spaced identifiers in the synthetic PII set), and is part of the masker's bar.
- **Over-blocking on given names that are ordinary words** (Grace, Joy, Hope, Peace, Favour, Patience). The identifier rule matches them in guideline text on word boundaries, and the job escalates. This fails closed, so it costs availability, not privacy. It is kept, pinned by `test_common_word_given_name_blocks_today`, and measured as the false-refusal rate in the H2 evaluation. The property strategy includes one such name.
- **The pattern rules don't run on shared chunk text.** A guideline that itself contained a patient identifier would pass the patterns (the identifier rule still covers this patient's known values). Accepted: shared chunks come only from the owner-side seed tool, and guideline content is curated.
- **The live smoke key exists on one person's machine for its short life.** Accepted with the cap, the expiry and the revocation (H7).
- **Provider legal-hold and abuse exceptions to zero retention.** Accepted with the legal check (legal sign-off).
- **HGA thresholds were tuned on Qwen.** Cloud answers need the clinical evaluation before clinicians rely on them (clinical sign-off).
- **Sign-off needed:** privacy (masker bar), legal (NDPA, agreements, DPAs, ZDR), clinical (evaluation; `SYSTEM_PROMPT` and the allow-lists under change control), product owner (the key, the demo account and spend limit).

---

## Abuse cases → tests

Every case is a named test, written before the code it proves.

| Case | Test (task) |
|---|---|
| A1 synthetic identifiers everywhere never reach a body, and the scan still sends clean ones | `test_property.py` (10), `test_property[<provider>]` (11–13); `test_prompt_holds_only_allowed_parts`, `test_fake_adapter_bodies_are_captured` (9); `test_clean_record_is_sent`, `test_short_name_token_needs_word_boundary`, `test_no_digit_join_across_values`, `test_guideline_doses_and_dates_pass`, `test_typical_clinical_questions_are_sent`, `test_patient_paths_are_exact_for_each_layout`, `test_name_in_question_blocked_for_every_layout` (6) |
| A2 a name without a title is blocked, however it is split or escaped | `test_name_without_title_blocked`, `test_escaped_and_split_names_blocked_through_sdk_encoder`, `test_scan_sees_through_zero_width_fullwidth_and_case` (6); `test_sdk_encoded_bodies_still_blocked` (11–13) |
| A3 an identifier as a dict key or a number never reaches the body | `test_cloud_context_allow_list` (5) |
| A4 a Nigerian phone number or NIN with no record is blocked | `test_nigerian_phone_and_nin_blocked_without_record` (6) |
| A5 an adapter called with a plain string, a dict or an altered string sends nothing | `test_prompt_refuses_plain_str_dict_and_six_chunks`, `test_string_ops_drop_the_tag` (5); `test_no_socket_opened` (11) |
| A6 a hospital or patient chunk is refused | `test_hospital_or_patient_chunk_refused` (5) |
| A7 an unknown citation, a link or mismatched markers is `malformed` | `test_answer_checks` (8); `test_fixture_outcomes` (11–13) |
| A8 no state, `store: false`, no user, metadata or safety fields | `test_request_shape` (11–13); `test_store_false_and_no_state_fields` (12) |
| A9 xAI retention header missing or false is discarded | `test_retention_header_missing_or_false_discards` (13) |
| A10 key never in logs, errors or spans; bad keys refused at start | `test_apikey_never_in_repr_str_format_pickle_copy`, `test_key_file_mode_and_placeholder_refused`, `test_redacting_filter_on_message_args_and_exception` (4); `test_sdk_error_logged_by_type_and_status_only` (11) |
| A11 20 jobs against a budget for 5 send at most 5 | `TestReserveIsAtomicUnderRace` (1); `test_budget_race_through_the_port` (7); `test_budget_race_through_jobs` (10) |
| A12 a refusal, a truncation and three 503s escalate without a score | `test_every_failure_escalates_without_score`, `test_retries_only_retryable_then_unavailable` (9); `test_cloud_failure_completes_escalated` (10) |
| A13 the gateway or a hospital-side caller can't turn cloud on | `TestModelRouteSetByTriggerNotByInsert`, `TestGatewayCannotTouchPolicyOrRoute` (1); `TestModelPolicyRoutesRoles` (2) |
| A14 a policy switched off before the claim runs local, and says so | `TestEffectiveRoute` (1); `TestResultGeneratedBy` (3); `test_route_changed_between_publish_and_claim_runs_local` (10) |
| A15 the proxy refuses other hosts, ports and IP literals; the pipeline has no direct route | `test-egress.sh`, `TestPipelineNeverOnEgress` (14) |
| A16 the fact records `egress` and the provider; unknown values refused | `TestQueryCompletedEgressAndOutcome` (3); `test_cloud_answer_records_provider` (10) |
| A17 redirects not followed; base-URL override refused outside development | `test_base_url_override_only_in_development` (4); `test_301_not_followed` (11–13) |
| A18 no text in logs, spans or dead letters on the cloud path | `test_no_text_in_logs_spans_dead_letters_on_cloud_path` (10) |
| K1 `MaskedText` copied or pickled | `test_masked_text_cannot_be_pickled_or_copied` (5) |
| K2 a `SharedChunk` made outside `kb_repo` | `test_shared_chunk_only_from_kb_repo` (5); `test_imports.py` (5) |
| K3 the widened audit policies admit only the new action, without hospital or actor | `TestPlatformAppendAllowsModelPolicyChangedOnlyWithoutHospital`, `TestProvisionAppendAllowsItOnlyWithoutActor` (1) |
| K4 the worker reads the policy or the ledger directly, or reserves for no hospital | `TestWorkerHasNoGrantOnPolicyOrLedger`, `TestReserveRefusesNonPositiveHugeAndNoHospital` (1); `test_no_direct_db_still_passes` (7) |
| K5 a settle releases another job's or hospital's reservation, twice, or goes negative | `TestSettleIsOncePerReservationAndOwnHospitalOnly` (1); `test_settle_twice_is_a_no_op` (7) |
| K6 the CLI enables an unknown or disabled provider | `TestCLIModelPolicySet` (2) |
| K7 the route enables a provider the deployment hasn't | `TestModelPolicyPutRefusesDisabledProvider409` (2) |
| K8 the worker starts with a provider but no proxy, model or ZDR reference | `test_settings_refusals` (4) |
| K9 `DEMO-SYNTHETIC` accepted in production; a Fable or Mythos model configured | `test_demo_accepts_demo_synthetic_with_warning_production_refuses`, `test_fable_and_mythos_models_refused` (4) |
| K10 per-job identifiers leak between concurrent jobs | `test_per_call_state_not_on_client` (9) |
| K11 the SDK's wrapped refusal is retried | `test_hook_refusal_is_egress_blocked_not_unavailable_and_not_retried` (9, 11) |
| K12 a non-retryable outcome is retried | `test_never_retry_on_400_401_403_404_refusal_truncated_malformed_egress` (8) |
| K13 a message or token chooses the route | `test_message_cannot_choose_route` (10) |
| K14 Go and Python outcome lists drift | `TestOutcomeVocabularyMatchesWorker` (3); `test_vocab_lists_are_closed_and_sorted` (4) |
| K15 a temporary table shadows the policy or ledger in a definer | `TestTempTableCannotShadowPolicy` (1) |
| K16 an SDK environment variable sets a key, a base URL or debug logging | `test_sdk_environment_variables_refused` (4); `test_smoke_refuses_sdk_env` (15) |
| K17 the request carries a field the adapter didn't declare | `test_shape_refuses_unknown_path`, `test_schema_strings_are_allowed_paths` (6); `test_shape_covers_every_emitted_path` (11–13) |
| K18 the live smoke runs on the build server, in CI or from a key file | `test_smoke_refuses_in_container_or_ci`, `test_smoke_has_no_key_file_option`, `test_smoke_cli_has_no_transport_option`, `test_smoke_never_prints_or_logs_the_key` (15) |

## What's needed from people, and when

Nothing below blocks Tasks 1–16. The live smoke (Task 15 step 4) waits on H1 and H7, and the demo on H1 and H5. Real data waits on H2–H4 and H6.

| # | Who | What, exactly | Needed by |
|---|---|---|---|
| H1 | Product owner | **An Anthropic API key** in a workspace used only by this deployment, with a USD 50/month spend limit (§16), inference scope only where offered; ZDR requested from Anthropic at the same time, its reference given to the platform team. **Ask Anthropic to confirm in writing that `claude-opus-5-5` is covered by our zero-data-retention terms** (the design found nothing documented for Opus 5.5; §16 question 1). **Restrict the key to our egress IP** (the demo host's public address) where the provider offers it (§7). Separately, allow H7 to create short-lived smoke keys, capped at USD 5, in the same workspace. If OpenAI is wanted later: start its ZDR request with sales now. xAI: confirm the team's ZDR Console setting. | Task 15's live run; the demo |
| H2 | Privacy lead | **The masker bar** (recall ≥ 0.99 names, ≥ 0.999 numeric identifiers on a synthetic Nigerian PII set) and its sign-off, after the masker upgrade (a separate plan). The same evaluation also **measures the scan's miss rate on spaced numeric identifiers** ("445 812", which `long_digits` doesn't join), and **measures the egress scan's false-refusal rate** on the synthetic question set. That covers given names that are ordinary words (Grace, Joy, Hope, Peace, Favour, Patience) matching guideline text, and vitals. The rate is reported next to recall, and any change to the scan's rules is decided from it. | Before any real patient data |
| H3 | Legal / compliance lead | **The legal check:** hospital data agreements (processor outside Nigeria), NDPA 2023 cross-border transfer and processor terms, each provider's DPA and ZDR terms; the `agreement_ref` format the platform admins will record. | Before any real patient data |
| H4 | Clinical lead | **The clinical evaluation** of cloud answers on the synthetic evaluation set, and review of `gate.SYSTEM_PROMPT` (clinical change control). | Before a hospital's clinicians rely on cloud answers |
| H5 | Product owner / platform | **The demo host:** CPU host with memory for the embedding and NLI models, the pipeline service enabled (its NKey, database login, trusted keys), the key file mounted. **The pipeline has no route to Hugging Face** (only the provider hosts through the proxy), so the embedding and NLI weights must be baked into the image or mounted read-only, with `HF_HUB_OFFLINE=1`. **Name who receives the alerts:** the 80% budget alert (`cloud.budget_high`), `auth_failed` and `unconfigured` (`mhai_cloud_alerts_total`), and where they are routed (an email or chat channel from the metrics). | The demo |
| H6 | Product owner | **Egress isolation** (§16 question 6): accept C9 for real data, or schedule the separate cloud service. | Before any real patient data |
| H7 | Product owner (names the person) | **A smoke-host owner:** one person who runs the live smoke (Task 15) on their own machine, outside any container, with a dedicated short-lived smoke key (USD 5 cap, expiry within a day where offered) pasted from a password manager and revoked straight after. Never steven-reid, mt5-lab or CI. | Task 15's live run |

## Decisions (approved 3 Nov 2026, all as recommended)

From design §16, unchanged: Anthropic first, then OpenAI, then xAI. The models are `claude-opus-5-5` (effort `medium`), `gpt-6.1-sol` and `grok-4.7`, each pinned by setting. Verification stays local. The budget is 2,000,000 tokens per hospital per month, with an alert at 80% and escalation at 100%. A cloud failure escalates, with no fallback. No separate egress service in the first build.

## Out of this plan

- The masker upgrade (Nigerian identifiers, local NER), which is its own design and plan, and is the H2 gate's subject.
- Patient-scoped or hospital-scoped chunks in the cloud prompt (§3.3 refuses them until a later design).
- Streaming, provider fallback or routing, cloud verification, semantic entropy sampling on the cloud path.
- A separate cloud-gateway service (§16 question 6).
- The mhai-web screens (a separate PR in mhai-web).
- A hospital-admin view of the policy (deviation 3).

## Self-review notes

- **Design coverage:**

  | Design | Task |
  |---|---|
  | §3.1 client, router, adapters | 9, 10, 11–13 |
  | §3.2 stages local or cloud | 9 (`_verify_with`, no `generate_with_rag`) |
  | §3.3 what leaves | 5, 9 (`test_prompt_holds_only_allowed_parts`) |
  | §3.4 documents stay | 5 (`test_imports.py`), 11–13 (text blocks only) |
  | §4.1–4.3 `MaskedText`, `CloudPrompt`, `CloudContext` | 5 |
  | §4.4 the scan | 6, 9 |
  | §4.5 the tests | 5, 6, 10 (property) |
  | §5.1–5.2 policy, route, claim | 1, 10 |
  | §5.3 routes, CLI, audit, `generated_by` | 2, 3 |
  | §6 egress proxy | 4 (proxy setting, refusal), 14 |
  | §7 secrets and settings | 4, 14 (runbook) |
  | §8 versions and audit | 3, 9, 10 |
  | §9 structured output | 8, 11–13 |
  | §10 budget, timeouts, retries | 1, 7, 8, 9 |
  | §11 outcomes | 4 (vocab), 3 (Go), 9 |
  | §12 testing | every task; 15 (live smoke) |
  | §13 threat model | this plan's section; A1–A18 |
  | §15 demo | 14 (runbook), H1, H5 |
  | §17 items 1–11 | 1–3 (2, 3), 4–10 (4, 5, 7), 11–13 (6), 14 (8), 15 (9), 16 (11); item 1 done in PR #85; item 10 mhai-web |

- **Names across tasks:** `vocab.PROVIDERS/ROUTES/EGRESS/OUTCOMES/BASE_URLS/HOSTS`; `ApiKey`, `load_api_key`, `SDK_ENV_REFUSED`, `RedactingFilter`, `install_redaction`; `ProviderSettings`, `CloudSettings`; `MaskedText`, `TaggingMasker`, `SharedChunk`, `seal_shared_chunk`, `CloudContext.from_masked`, `CloudPrompt`, `SYSTEM_PROMPT`, `EgressRefused`; `Identifiers`, `collect_identifiers`, `Shape`, `shape_for`, `leaf_values`, `BLOCK_ARRAYS`, `RULES`, `norm_words`, `norm_text`, `digit_runs`, `grouped_runs`, `scan_body`, `ScanHook`; `cloud_repo.reserve/settle`, `estimate_tokens`, `Reservation(id, tokens, percent)`, `BudgetPort`, `DbBudget`; `ANSWER_SCHEMA`, `Answer`, `validate_answer`, `Malformed`; `RetryPolicy`, `classify`; `CloudCall`, `CloudReply`, `Adapter` (with `shape`), `_SharedTransport`, `_RenderTransport`, `CloudModelClient`, `build_cloud_clients`; `ModelRouter`, `build_model_router`, `_verify_with`, `local_parts`, `stub_parts`; `Claim.model_route`, `Decision.egress`, `Decision.cloud_outcome`; SQL `hospital_model_policy`, `cloud_usage`, `cloud_reservations`, `mhai_set_model_route`, `mhai_effective_model_route`, `mhai_cloud_reserve`, `mhai_cloud_settle(p_reservation, p_used)`, `mhai_cloud_usage`; Go `Config.CloudProviders`, `registerModelPolicy`, `GeneratedBy`, `generatedBy`.

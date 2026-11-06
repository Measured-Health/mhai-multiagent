# Cloud model providers (OpenAI, Anthropic, xAI): implementation plan

**Status:** Not started. The design is approved (product owner, 3 Nov 2026, "approve both as recommended"; all six questions in its §16 as recommended). The gate before real patient data (design §13.6) is a human gate, tracked in TODO.md. Nothing in this plan opens it.

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task by task. Steps use checkbox (`- [ ]`) syntax for tracking. Read the `secure-design` and `secure-build` skills (`.claude/skills/`) before Task 1, and run `security-review` on your own diff before each PR.

**Goal:** Let a hospital that a platform admin has opted in get its clinical answers drafted by a cloud model (Anthropic, OpenAI or xAI). Only masked text and shared guideline text leave the network, through a typed gate and a scan of the exact request bytes, failing closed. Retrieval and verification stay local, and every cloud failure escalates. The route is off by default, decided by core and read by the worker from the claimed row. It is recorded on every answer and on the audit chain. Everything is built and tested with fake adapters and synthetic fixtures. A real provider is called only by a manual smoke test.

**Architecture:**

- **Core (Go):** migration `00018_model_policy.sql` adds a platform-only `hospital_model_policy` table and `clinical_queries.model_route`, set by a `BEFORE INSERT` trigger that overwrites anything the insert supplied. It also adds a per-hospital monthly token ledger, `cloud_usage`, which the worker reaches only through two `SECURITY DEFINER` functions. There are two platform routes and a CLI twin to set the policy. The audit action `hospital.model_policy_changed` gets a closed schema. `query.completed` gains two optional closed keys, `egress` and `cloud_outcome`, and the clinical result gains `generated_by`.
- **Worker (Python):** a new package `intelligence/boundary/cloud/` holds the settings and API-key files, the gate types (`MaskedText`, `CloudContext`, `SharedChunk`, `CloudPrompt`), the egress scan, the budget port, reply validation, `CloudModelClient` and one adapter per provider. `ModelRouter` (in `boundary/models.py`) gives `run_query` the client for the claimed row's route, and that one client runs every stage of the job.
- **Deploy:** an `egress-proxy` container on a new `egress` network. It allows only `CONNECT` to port 443 of the enabled providers' API hosts. The pipeline stays on the internal `backend` network.

**Tech stack:** Go 1.24 (`pgx/v5`, `goose/v3`, `testcontainers-go`), as in Plan 4. Python 3.12 with uv. New runtime dependencies: `openai` and `anthropic`, the official SDKs, versions pinned by `uv.lock`. `httpx` is already a dependency. New dev dependencies: `hypothesis` and `pytest-socket`. The egress proxy is Squid (`ubuntu/squid`), pinned by digest.

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
- **The real-data gate is not code.** No setting, flag, migration or CLI command in this plan represents "real data allowed". The masker recall bar (names ≥ 0.99, numeric identifiers ≥ 0.999, privacy lead's sign-off), the legal check (hospital data agreements, NDPA 2023, provider DPAs and zero-retention terms) and the clinical evaluation are TODO.md items owned by people (Task 16). An agent never edits them to "done".
- **Only masked text leaves.** Adapters accept a `CloudPrompt` and nothing else (runtime type check plus mypy). `CloudPrompt` accepts only a tagged `MaskedText` question, a `CloudContext` (two allow-listed fields) and sealed `SharedChunk`s. The scan runs on the exact request bytes before they reach the transport. Any refusal means nothing is sent, the outcome is `egress_blocked`, and the job escalates.
- **Every cloud failure escalates.** `Generation(available=False, failed_agents=("CCA",))`, decision `ESCALATE`, `verified=False`, no trust score, `degraded=True`, the outcome code recorded (§11). There is no fallback to local or to another provider. The one exception: a policy switched off before the claim runs local (§5.2).
- **Retrieval and verification stay local.** The cloud client never calls `RAGClinicalContextAgent.generate_with_rag` and never sends anything to verify.
- **Documents never go.** `ingest_document` and `finalise_document` never get a `ModelRouter`, and no adapter can emit an image, file, document or audio block.
- **Keys:** a key is read from a file (`INTELLIGENCE_CLOUD_<PROVIDER>_KEY_FILE`), mode 0400 or 0440, on the pipeline host only. Keys are never in environment variables, core, the BFF, the database, the repository, CI, logs, spans, exceptions or fixtures. In memory they are held as `ApiKey`, whose `repr` and `str` are redacted.
- **CI never calls a provider.** Adapter tests use `httpx.MockTransport` with synthetic fixtures, under `pytest-socket` (`--disable-socket` for `tests/boundary/cloud/`). The live smoke script (Task 15) is excluded from collection. It refuses to run without `MHAI_CLOUD_SMOKE=1` and a key file, sends synthetic data only, and is run by a person on steven-reid.
- **Security rules are written once, in Go/SQL.** The route is decided by core's trigger from core's table. The budget rule is a core-owned SQL function. Python never reads a hospital, route or budget from a payload. The worker's hospital is the verified token's (`with_hospital`).
- **Fixed network targets.** Base URLs are constants in code: `https://api.anthropic.com`, `https://api.openai.com/v1`, `https://api.x.ai/v1`. An override (`INTELLIGENCE_CLOUD_<PROVIDER>_BASE_URL`) is refused outside `APP_ENV=development`. The worker never follows redirects, ignores environment proxies (`trust_env=False`) and uses the proxy named in its settings, with TLS verification on and no custom CA.
- **No per-request state on shared objects.** The identifiers, the budget port and the deadline reach `generate()` as one argument (`call: CloudCall`). The client holds only the provider's settings and the shared HTTP transport.
- **Closed vocabularies.** Providers: `openai`, `anthropic`, `xai`. Routes: `local`, `cloud:openai`, `cloud:anthropic`, `cloud:xai`. Egress: `none` plus the providers. Outcomes (§11): `ok`, `egress_blocked`, `budget_exhausted`, `unconfigured`, `unavailable`, `timeout`, `auth_failed`, `refused`, `truncated`, `malformed`, `insufficient`, `retention_unconfirmed`. Python and Go hold the same lists, and a test on each side pins them.
- **No baselines.** Nothing is added to the ruff, format or mypy baselines; `intelligence/boundary/cloud/` is mypy-strict.

## Review Focus

1. **The scan blocks every question because of guideline text.** The pattern scan (dates, runs of 6 or more digits, `MRN` prefixes) runs over the whole body, shared chunks included. One seeded guideline with a date or a long code would escalate every question that retrieves it, and it would look like a provider outage. Expected: shared guideline text passes the pattern scan, and a hit is reported by rule code with the counts showing where it came from. Test: `test_seed_guidelines_pass_the_pattern_scan` and `test_scan_reports_section_not_text` (Task 6).
2. **An SDK wraps the scan's refusal.** The scan runs as an `httpx` request hook. Both SDKs catch transport exceptions and re-raise them as `APIConnectionError`, which would map to `unavailable` and be retried. Expected: the per-call scanner's own verdict wins, the outcome is `egress_blocked`, and there is no second attempt. Test: `test_hook_refusal_is_egress_blocked_not_unavailable_and_not_retried` (Task 9, and each adapter's suite).
3. **Identifiers hidden by Unicode.** A name with a zero-width space, a non-breaking space, full-width digits or mixed case (`Ada​eze`, `０８０３…`) must still match the known-identifier and pattern scans. Expected: NFKC, Unicode format characters (category `Cf`) removed, case-folded, and separators stripped from digit runs, before comparing. Test: `test_scan_sees_through_zero_width_fullwidth_and_case` (Task 6).
4. **Budget reservations that are never settled.** A deadline, an abandoned stage thread or an exception between reserve and settle leaves tokens "reserved" until the month ends, so a hospital slowly runs out. Expected: settle runs in `finally` on every path the process survives; an unknown reply settles as fully used (§10); only a crashed process leaves a reservation, and that is bounded by one attempt's estimate. Test: `test_reservation_settled_on_every_failure_path` (Task 9).
5. **Policy switched to a different provider between insert and claim.** A row inserted as `cloud:openai` whose hospital is now on `anthropic` must not go to either: the design runs it local (the provider no longer matches). Test: `test_claim_runs_local_when_provider_changed` (Task 1, SQL) and `test_route_changed_between_publish_and_claim_runs_local` (Task 10, job).

## Where this plan departs from the design

1. **The migration is 00018.** The design names 00015 ("Plan 4c may take 00015"). On `main`, 00015–00017 are taken (`00015_health_functions`, `00016_platform_jobs`, `00017_guardrail_counts`). The build takes the next free number when it starts. If another PR merges a migration first, the file is renumbered and the tests' references with it.
2. **The worker reaches the budget only through functions.** The design grants `mhai_worker` `SELECT, INSERT, UPDATE (used, reserved)` on `cloud_usage` and `SELECT (hospital_id, cloud_provider)` on the policy. But the reservation needs `monthly_token_budget`, which that grant doesn't give. This plan adds `mhai_cloud_reserve(p_tokens bigint)` and `mhai_cloud_settle(p_month date, p_reserved bigint, p_used bigint)`, both `SECURITY DEFINER`, owned by `mhai_migrate` with a fixed `search_path`. Each runs the design's single atomic `UPDATE … WHERE used + reserved + n <= budget` for `mhai_current_hospital()`, with the month computed in the database (UTC). `mhai_worker` gets `EXECUTE` on the two functions and no grant on `cloud_usage`. The policy keeps the design's column grant for the claim. The rule stays in core's SQL, and the worker can't read another hospital's budget or write a number of its own choosing beyond its own settle.
3. **The route trigger is `SECURITY DEFINER`, and `mhai_gateway` gets no grant on the policy.** The design grants the gateway `SELECT` so its trigger can read the policy and hospital admins can see it. A definer trigger (owner `mhai_migrate`, a `FOR SELECT TO mhai_migrate` policy, as in 00014's functions) sets the route whoever inserts. No hospital-side route shows the policy in this plan, so the grant would be unused. A hospital-admin view, when wanted, is a later additive route.
4. **The audit policies are widened.** `mhai_platform` may append only 00014/00016's eight actions, with `hospital_id IS NULL`. `mhai_provision` may append only four, with `actor_id IS NULL`. The migration adds `hospital.model_policy_changed` to both, with the same conditions. The audit row's `hospital_id` stays NULL (a platform action), and the hospital goes in the payload's `hospital_id`, as the design's schema has it.
5. **Step-up follows the platform's pattern.** `PUT /platform/hospitals/{id}/model-policy` takes `mfa_code` or `backup_code` in the body, as `POST /platform/jobs/cancel` does (`auth.Service.StepUp`).
6. **The CLI takes the hospital as an argument:** `core hospitals model-policy set <hospital id or short code> --provider anthropic|openai|xai|none --agreement-ref <ref> [--budget <tokens>]`. This matches `core hospitals activate <hospital id or short code>`. The design's `--hospital <id>` is a flag.
7. **`APP_ENV=demo` is new.** The worker knows only `development` and everything else (production). This plan adds `demo`, which is production in every respect but one: `INTELLIGENCE_CLOUD_<PROVIDER>_ZDR_REF=DEMO-SYNTHETIC` is accepted, with a warning at start. Production refuses that value. Core needs no change: it never reads ZDR references.
8. **The reviewer's masked copy is converted to plain strings.** The design makes `MaskedText` impossible to copy, but `run_query._pipeline` deep-copies the masked context for the reviewer (`copy.deepcopy(context)`). The copy is now made with `_plain_tree`, which turns every `MaskedText` into a plain `str`. It is never passed to a client, and a plain `str` can't reach an adapter.
9. **Known identifiers are collected by a second, local masker pass.** `ModelClient.mask` returns the masked text only, and the shared client can't keep per-job detections. `cloud.scan.collect_identifiers(question, patient)` runs its own `PIIMasker` (a fresh instance per call) over the raw question and every raw string, keeping each detection's `original_text`. It also takes the values under the identifying keys. The canonical model's FHIR keys (§4.4) don't exist in this repository yet (the EMR service isn't built), so the key list is the design's plain keys plus `identifier`, `telecom`, `birthDate`, `name.given`, `name.family`; Task 6 pins it.
10. **Retrieval uses `HybridRetriever.retrieve`, not `build_context`.** `build_context` returns one formatted string, and the gate needs the chunk objects. The client calls `orch.rag_cca.retriever.retrieve(question, top_k=5, patient_id=None)` on the per-job orchestrator that `default_orchestrator`'s builder makes, and maps each result to its `SharedChunk`.
11. **Adapters are a request builder and a reply parser, behind one transport.** The design's `send(prompt, deadline) -> CloudReply` is kept as the client's internal call. Each adapter provides `call(sdk_client, prompt) -> CloudReply`, using its SDK. The SDK client is built per generation over one shared `httpx.HTTPTransport` (proxy, TLS) with a per-call scan hook, so the scan sees the exact bytes and holds that job's identifiers without shared state.
12. **The first fixtures are hand-made from the providers' documented shapes.** The design records fixtures with the live smoke test. That needs a key (H1), and PR D must not wait for it. Tasks 11–13 commit synthetic fixtures under `tests/fixtures/cloud/<provider>/`. Task 15 adds the recorded, scrubbed ones next to them, and they must pass the same parser tests.
13. **Semantic entropy is unchanged.** The boundary never passes `semantic_entropy` to `hga.verify`, on either path today, so the design's "not used on the cloud path" needs no code. A test pins that the cloud client never calls the CCA's generator.

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
  internal/db/migrations/00018_model_policy.sql   policy table, model_route + trigger, claim support,
                                                   cloud_usage + reserve/settle/usage functions, grants,
                                                   RLS, the two audit policies widened
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
    budget.py           BudgetPort, Reservation, estimate_tokens, DbBudget (reserve/settle via the functions)
    reply.py            ANSWER_SCHEMA, Answer, validate_answer, Malformed
    retry.py            RetryPolicy, classify, backoff
    client.py           CloudCall, CloudReply, Adapter (Protocol), CloudModelClient, build_cloud_clients
    anthropic_adapter.py, openai_adapter.py, xai_adapter.py
  intelligence/boundary/config.py                 WorkerSettings.cloud; is_demo
  intelligence/boundary/models.py                 Generation (versions, egress, cloud_outcome, tokens);
                                                  ModelClient.generate(..., call=None); ModelRouter;
                                                  build_model_router; _verify_with (shared by both clients)
  intelligence/boundary/kb_repo.py                shared rows sealed as SharedChunk
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
- `hospital_model_policy (hospital_id uuid PK REFERENCES hospitals, cloud_provider text NULL CHECK (cloud_provider IN ('openai','anthropic','xai')), agreement_ref text NULL CHECK (agreement_ref ~ '^[A-Z0-9][A-Z0-9_.-]{2,63}$'), monthly_token_budget bigint NOT NULL DEFAULT 2000000 CHECK (monthly_token_budget > 0), updated_by uuid NULL, updated_at timestamptz NOT NULL DEFAULT now(), CHECK (cloud_provider IS NULL OR agreement_ref IS NOT NULL))`. `ENABLE` and `FORCE` RLS, owner `mhai_migrate`. Grants: `mhai_platform` `SELECT, INSERT, UPDATE` (policy `USING (true)`). `mhai_provision` `SELECT, INSERT, UPDATE` (the CLI). `mhai_worker` `SELECT (hospital_id, cloud_provider)`, policy `hospital_id = mhai_current_hospital()`. `mhai_migrate` a `FOR SELECT` policy for the definer functions. Nothing for `mhai_gateway`.
- `clinical_queries.model_route text NOT NULL DEFAULT 'local' CHECK (model_route IN ('local','cloud:openai','cloud:anthropic','cloud:xai'))`. Trigger `clinical_queries_model_route BEFORE INSERT FOR EACH ROW EXECUTE FUNCTION mhai_set_model_route()`, which is `SECURITY DEFINER`, `SET search_path = pg_catalog, public`, and sets `NEW.model_route := coalesce('cloud:' || p.cloud_provider, 'local')` from the hospital's row, ignoring the supplied value. No role gains `UPDATE (model_route)`. The gateway already has only `UPDATE (published_at)` (00008), and a test pins it.
- `cloud_usage (hospital_id uuid REFERENCES hospitals, month date, used bigint NOT NULL DEFAULT 0 CHECK (used >= 0), reserved bigint NOT NULL DEFAULT 0 CHECK (reserved >= 0), PRIMARY KEY (hospital_id, month))`, RLS on and forced, no direct grant to any application role.
- `mhai_cloud_reserve(p_tokens bigint) RETURNS TABLE (month date, percent integer)`: `SECURITY DEFINER`, executable by `mhai_worker` only. It refuses `p_tokens <= 0` or `> 100000`, and `mhai_current_hospital()` NULL. Then it upserts the month's row and, in one statement, `UPDATE cloud_usage SET reserved = reserved + p_tokens WHERE hospital_id = h AND month = m AND used + reserved + p_tokens <= (SELECT monthly_token_budget FROM hospital_model_policy WHERE hospital_id = h AND cloud_provider IS NOT NULL) RETURNING …`. No row (over budget, or no enabled policy): an empty result. `percent` is `(used + reserved) * 100 / budget`.
- `mhai_cloud_settle(p_month date, p_reserved bigint, p_used bigint) RETURNS void`: this hospital's row only. `reserved = greatest(0, reserved - p_reserved)`, `used = used + least(greatest(p_used, 0), p_reserved)`.
- `mhai_cloud_usage(p_hospital uuid) RETURNS TABLE (month date, used bigint, reserved bigint)`: executable by `mhai_platform` only, the current month.
- The audit policies: `platform_append` and `provision_append` recreated with `hospital.model_policy_changed` added, with their existing conditions (`hospital_id IS NULL`; `actor_id IS NULL`). Down restores 00016's and 00010's exact lists.

- [ ] **Step 1: Write the failing tests** (`-tags integration`):
  - `TestModelRouteSetByTriggerNotByInsert` (A13: an insert as `mhai_gateway` naming `model_route = 'cloud:xai'` gets `local` with no policy and `cloud:anthropic` with an Anthropic policy);
  - `TestGatewayCannotTouchPolicyOrRoute` (A13: `INSERT`/`UPDATE`/`SELECT` on `hospital_model_policy` and `UPDATE clinical_queries SET model_route` fail as `mhai_gateway`);
  - `TestWorkerSeesOwnPolicyColumnsOnly` (no `agreement_ref`, `monthly_token_budget`; another hospital's row invisible);
  - `TestWorkerHasNoGrantOnCloudUsage` (K4);
  - `TestReserveIsAtomicUnderRace` (A11: budget 5 × n; 20 parallel `mhai_cloud_reserve(n)` on separate connections; exactly 5 rows returned);
  - `TestReserveRefusesNonPositiveHugeAndNoHospital` (K4);
  - `TestReserveWithoutEnabledPolicyReturnsNothing`;
  - `TestSettleOnlyOwnHospitalAndNeverNegative` (K5);
  - `TestClaimRunsLocalWhenPolicyOffOrProviderChanged` (A14 and Review Focus 5: run the worker's claim SQL from Task 10, kept as a fixture string here, against a `cloud:openai` row after the policy is set to NULL, and after it is set to `anthropic`; both return `local`);
  - `TestPolicyChecks` (unknown provider, bad `agreement_ref`, provider without `agreement_ref`, budget 0);
  - `TestPlatformAppendAllowsModelPolicyChangedOnlyWithoutHospital` and `TestProvisionAppendAllowsItOnlyWithoutActor` (K3);
  - `TestMigration00018DownUp`.
- [ ] **Step 2: Run (Go, remote):** `go test -race -tags integration ./internal/db/...` → fails (no migration).
- [ ] **Step 3:** Write `00018_model_policy.sql`, following 00014's pattern for definer functions: owner `mhai_migrate`, `REVOKE ALL … FROM PUBLIC`, a fixed `search_path`, and a policy for the owner under `FORCE`.
- [ ] **Step 4:** Run the tests, `go vet`, and the whole `internal/db` integration suite (00008's worker policies, 00009's cancel trigger) to show nothing regressed.
- [ ] **Step 5:** Commit: "Add the per-hospital model policy, the model route trigger and the token ledger".

**Security checklist**
- [ ] No role but the trigger function writes `model_route`; the trigger ignores `NEW.model_route` as supplied.
- [ ] `mhai_worker` can't read another hospital's policy, can't read any budget, and can't touch `cloud_usage` except through the two functions, for its own hospital.
- [ ] Every definer function has a fixed `search_path` and no `PUBLIC` execute.

### Task 2: The policy routes, the CLI and the audit schema

**Files:**
- Create: `services/core/internal/platform/modelpolicy.go`, `modelpolicy_test.go`, `modelpolicy_integration_test.go`; `services/core/cmd/core/modelpolicy_test.go`, `modelpolicy_integration_test.go`
- Modify: `internal/platform/audit.go` (`Register` calls `registerModelPolicy`; `Deps.CloudProviders []string`), `internal/server/server.go`, `internal/config/config.go` (+ `_test`), `internal/audit/schemas.go` (+ `_test`), `cmd/core/seed.go`, `cmd/core/main.go` (usage), `openapi.yaml` (the two routes; `info.version` 1.10.0), `docs/specs/generated/permissions.md` (via `go run ./cmd/permgen`)

**Interfaces:**
- `config.Config.CloudProviders []string` from `CORE_CLOUD_PROVIDERS` (comma list, empty by default; each in `openai, anthropic, xai`; duplicates or unknown names refused at start with the variable's name only).
- Routes (`registerModelPolicy`):
  - `GET /platform/hospitals/{id}/model-policy`: `authz.Rule{Platform: true, NeedsMFA: true, AnyRole: [platform_admin, auditor]}` → `{hospital_id, cloud_provider: string|null, agreement_ref: string|null, monthly_token_budget, updated_at: string|null, usage: {month, used, reserved}}`. No row: the defaults with `cloud_provider: null`. Unknown hospital: 404 `not_found`.
  - `PUT /platform/hospitals/{id}/model-policy`: `authz.Rule{Platform: true, NeedsMFA: true, AnyRole: [platform_admin], Write: true, StepUp: true}`. Strict body: `{cloud_provider: "openai"|"anthropic"|"xai"|null, agreement_ref: string|null, monthly_token_budget: int 1..1000000000, mfa_code | backup_code}`, unknown keys refused (422). A provider not in `CloudProviders` gives 409 `conflict`. A provider without an `agreement_ref` gives 422. Upsert as `mhai_platform`, then `audit.Write` of `hospital.model_policy_changed`, in the same transaction. Answers as `GET`. 10 writes a minute per session, as `jobs/retry` does.
- Audit schema `hospital.model_policy_changed`: fields `hospital_id` (uuid), `provider` and `previous` (`OneOf("none","openai","anthropic","xai")`), `agreement_ref` (pattern above, or absent when none), `monthly_token_budget` (`Int(1, 1000000000)`), `via` (`OneOf("platform","cli")`). Required: `hospital_id`, `provider`, `previous`, `monthly_token_budget`, `via`.
- CLI: `core hospitals model-policy set <hospital id or short code> --provider <openai|anthropic|xai|none> [--agreement-ref <ref>] [--budget <n>]`, as `mhai_provision`, writing the same audit action with `via: cli`. It refuses a provider not in `CORE_CLOUD_PROVIDERS`, prints `model policy <hospital id> <provider>`, and never prints a key or reference beyond the one given.

- [ ] **Step 1: Write the failing tests:**
  - `TestModelPolicyRoutesRoles` (A13: a clinician, a hospital admin and a clinical-audience token get 403 on `PUT`; an auditor gets 403 on `PUT` and 200 on `GET`);
  - `TestModelPolicyPutNeedsStepUp`;
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
- `ClinicalResultOut.GeneratedBy *GeneratedBy` (`json:"generated_by,omitempty"`), `GeneratedBy{Route string; Provider *string}`. It is read from `clinical_queries.model_route` of the caller's own query. `local` gives `{route: "local", provider: null}`, and `cloud:<p>` gives `{route: "cloud", provider: "<p>"}`. Additive under ADR 0012.

- [ ] **Step 1: Write the failing tests:** `TestQueryCompletedEgressAndOutcome` (A16: each value accepted; an unknown `egress` or `cloud_outcome`, a number and free text refused; a fact without either still valid), `TestOutcomeVocabularyMatchesWorker` (the Go list equals the strings in `services/intelligence/intelligence/boundary/cloud/vocab.py`, read as text, so the two can't drift: K14), `TestResultGeneratedBy` (local and cloud rows; another hospital's query still 404).
- [ ] **Step 2: Run (Go, remote):** `go test -race ./internal/intel/... ./internal/queries/...` and `-tags integration ./internal/queries/...` → fail. (`TestOutcomeVocabularyMatchesWorker` stays skipped with a message until Task 4 creates `vocab.py`; Task 4's PR removes the skip.)
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the tests and the openapi breaking-change check (additive only).
- [ ] **Step 5:** Commit: "Record where an answer was generated: egress and cloud_outcome facts, generated_by on the result".

**PR A:** after Task 3, run `security-review` on the branch and open the PR. mhai-web's PR (the policy screen and `generated_by`) can start from the openapi file.

### Part B: the gate (PR B)

### Task 4: Settings, key files and redaction

**Files:**
- Create: `intelligence/boundary/cloud/{__init__,vocab,apikey,settings}.py`, `tests/boundary/cloud/{__init__,conftest,test_vocab,test_apikey,test_settings}.py`
- Modify: `services/intelligence/pyproject.toml` (deps `openai`, `anthropic`; dev `hypothesis`, `pytest-socket`; mypy strict for `intelligence.boundary.cloud`), `uv.lock`, `intelligence/boundary/config.py` (`WorkerSettings.cloud: CloudSettings`; `is_demo`), `intelligence/boundary/worker.py` (`install_redaction()` and SDK loggers set to `WARNING` at start), `services/core/internal/intel/factschema_test.go` (remove Task 3's skip)

**Interfaces:**
- `vocab.PROVIDERS = ("anthropic", "openai", "xai")`, `ROUTES`, `EGRESS`, `OUTCOMES` (§11's twelve), `BASE_URLS = {"anthropic": "https://api.anthropic.com", "openai": "https://api.openai.com/v1", "xai": "https://api.x.ai/v1"}`, `HOSTS` (derived).
- `ApiKey`: `__slots__`, `__repr__`/`__str__` return `ApiKey(<redacted>)`, `reveal() -> str`, refuses pickling and copying (`__reduce__` and `__copy__` raise `TypeError`).
- `load_api_key(path, name, development, errs) -> ApiKey | None`: uses `config._read_secret_file`. It refuses an empty key, a placeholder (`config.is_placeholder`), any whitespace inside the key, and a length under 20.
- `RedactingFilter(logging.Filter)`: rewrites `record.msg`, `record.args` and `exc_text` so that `sk-[A-Za-z0-9_-]{8,}`, `sk-ant-[A-Za-z0-9_-]{8,}`, `xai-[A-Za-z0-9_-]{8,}` and each loaded key's exact value become `[REDACTED]`. `install_redaction(keys)` puts it on the root logger's handlers.
- `ProviderSettings(name, model, key: ApiKey, zdr_ref, base_url)`. `CloudSettings(providers: tuple[ProviderSettings, ...], proxy_url: str | None, demo: bool)`. `CloudSettings.from_env(env, development, errs)` reads `INTELLIGENCE_CLOUD_PROVIDERS`, `INTELLIGENCE_CLOUD_<P>_MODEL` (`^[A-Za-z0-9._:-]{1,64}$`), `…_KEY_FILE`, `…_ZDR_REF` (`^[A-Z0-9][A-Z0-9_.-]{2,63}$`), `…_BASE_URL` (development only) and `HTTPS_PROXY` (`http://<host>:<port>` only). Start-up is refused when a provider is enabled with no key file, a bad key, no model, no `ZDR_REF` outside development, `ZDR_REF=DEMO-SYNTHETIC` outside `demo`/`development`, no `HTTPS_PROXY` outside development, or a base-URL override outside development. Errors name variables only.
- `config.is_demo(env)`: `APP_ENV` is `demo`. Demo is not development (deviation 7).

- [ ] **Step 1: Write the failing tests:** `test_apikey_never_in_repr_str_format_pickle_copy`, `test_key_file_mode_and_placeholder_refused` (A10 start-up), `test_redacting_filter_on_message_args_and_exception` (A10: a log call with the key in `args`, and an exception whose text echoes `Authorization: Bearer <key>`, both come out redacted), `test_settings_default_has_no_providers`, `test_settings_refusals` (each refusal above, parametrised, error message without values), `test_demo_accepts_demo_synthetic_with_warning_production_refuses`, `test_base_url_override_only_in_development` (A17), `test_vocab_lists_are_closed_and_sorted`.
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

**Interfaces:**
- `IDENTIFYING_KEYS = frozenset({"name", "given", "family", "mrn", "phone", "email", "address", "dob", "nin", "bvn", "nhis", "identifier", "telecom", "birthdate", "birth_date"})`, compared case-folded. Values under these keys anywhere in the tree are collected (deviation 9).
- `Identifiers`: a frozen dataclass with `values: frozenset[str]` (`repr=False`) and `__repr__` showing a count only. `collect_identifiers(question: str, patient: Mapping[str, Any]) -> Identifiers`. It takes every string, and every `int` of 5 or more digits, under an identifying key (any depth up to `run_query.MAX_DEPTH`), and each name token of 3 or more letters from name keys. It also takes each `original_text` a fresh `PIIMasker` detects in the question and in every string of the tree. Every value is normalised.
- `normalise(s) -> str`: NFKC; remove category `Cf` characters; case-fold; collapse whitespace and punctuation to one space. `digits(s) -> str`: the digits only, after NFKC.
- `scan_body(body: bytes, ids: Identifiers) -> str | None` returns a rule code, or `None` when clean:
  1. `too_large` (over 49,152 bytes);
  2. `known_identifier` (any value's normalised form is a substring of the normalised body; a value of 6 or more digits is also matched on the body's digit stream with separators removed);
  3. the patterns, each a precompiled regex on the normalised body: `ng_phone` (`(?:\+?234|0)[789][01]\d{8}` after separators are removed within a 16-character window), `nin` (an 11-digit run), `email`, `date` (`\d{1,2}[/.-]\d{1,2}[/.-]\d{2,4}`, ISO `\d{4}-\d{2}-\d{2}`, and day-month-name-year forms), `long_digits` (6 or more digits), `mrn_prefix` (`\b(?:mrn|hospital no\.?|hosp\.? no\.?)\s*[:#]?\s*\w`).

  Placeholders such as `[DATE_REDACTED]` never match. The rule code is all that leaves this function: no match text, no offset.
- `ScanHook(ids)`: a callable for `httpx` `event_hooks["request"]`. It reads `request.content`, sets `self.verdict` (a rule or `None`) and `self.sent` (`True` once a body has passed), and raises `EgressRefused(rule)` on a hit.

- [ ] **Step 1: Write the failing tests:** `test_name_without_title_blocked` (A2: "Adaeze Okafor has chest pain", with `name: "Adaeze Okafor"` in `patient_data`; the masker misses it, the scan blocks with `known_identifier`); `test_nigerian_phone_and_nin_blocked_without_record` (A4: `0803 123 4567`, `+234 803 123 4567`, an 11-digit NIN); `test_integer_mrn_found_with_separators`; `test_scan_sees_through_zero_width_fullwidth_and_case` (Review Focus 3); `test_redacted_placeholders_pass`; `test_seed_guidelines_pass_the_pattern_scan` (Review Focus 1: every guideline chunk the boundary tests and the demo seed load, scanned with empty identifiers, passes; if one fails, fix the seed text, not the rule); `test_scan_reports_section_not_text` (the verdict is a code; `caplog` holds no body text); `test_identifiers_repr_has_no_values`; `test_size_cap`. In `tests/security/test_cloud_egress.py`: the A2 and A4 cases again, through `ScanHook` on an `httpx.Request`.
- [ ] **Step 2: Run (Python, remote):** `uv run pytest -q tests/boundary/cloud/test_scan.py tests/security/test_cloud_egress.py` → fails.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the tests, lint, format, mypy.
- [ ] **Step 5:** Commit: "Scan the exact request bytes for this patient's identifiers and Nigerian identifier shapes; fail closed".

### Task 7: The budget port

**Files:**
- Create: `intelligence/boundary/cloud/budget.py`, `tests/boundary/cloud/test_budget.py` (unit and `boundary`)

**Interfaces:**
- `estimate_tokens(body_len: int, max_output: int = 1500) -> int`: `ceil(body_len / 3) + max_output`.
- `Reservation(month: date, tokens: int, percent: int)`.
- `BudgetPort` (Protocol, sync): `reserve(tokens: int) -> Reservation | None`, `settle(r: Reservation, used: int | None) -> None`. `used=None` (reply lost or unknown) settles as `r.tokens`.
- `DbBudget(db: Database, verified: Verified, loop: asyncio.AbstractEventLoop, timeout: float = 5.0)`: each call runs `SELECT * FROM mhai_cloud_reserve(%s)` or `SELECT mhai_cloud_settle(%s, %s, %s)` in its own `with_hospital(verified)` transaction on the job's loop (`asyncio.run_coroutine_threadsafe(...).result(timeout)`), so a generation thread can call it. Any database error from `reserve` gives `None` (fail closed). A failed `settle` is logged with ids only and the metric `mhai_cloud_settle_failures_total`. At `percent >= 80` the first time in a call it logs `cloud.budget_high hospital=<id> percent=<n>` and counts `mhai_cloud_budget_alerts_total{provider}`.

- [ ] **Step 1: Write the failing tests:** `test_estimate_is_an_upper_bound` (a body of n bytes never estimates under n/3 + 1500); `test_reserve_none_on_db_error`; `test_settle_unknown_reply_counts_reserved`; `test_budget_race_through_the_port` (`boundary`, A11: 20 threads through `DbBudget` against a budget for 5 → exactly 5 reservations); `test_budget_uses_token_hospital_only` (`boundary`: the port has no hospital parameter; a second hospital's ledger is unchanged); `test_alert_at_80_percent_once`.
- [ ] **Step 2: Run (Python, remote):** `uv run pytest -q tests/boundary/cloud/test_budget.py -m "not boundary"` and `-m boundary` → fail.
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
- Modify: `intelligence/boundary/models.py` (`Generation` gains `versions: Mapping[str, str] = {}`, `egress: str = "none"`, `cloud_outcome: str | None = None`, `tokens_in: int = 0`, `tokens_out: int = 0`; `ModelClient.generate(..., *, call: CloudCall | None = None)`; `OrchestratorModelClient` and `StubModelClient` accept and ignore `call`; `OrchestratorModelClient.verify`'s body moves to `_verify_with(orch, generation) -> Verification`, which both clients use; `default_orchestrator` exposes its `build` and versions through `local_parts() -> tuple[Builder, dict[str, str]]`), `tests/boundary/test_models.py`, `tests/boundary/cloud/conftest.py` (`FakeAdapter`, `CapturingTransport`)

**Interfaces:**
- `CloudCall(identifiers: Identifiers, budget: BudgetPort, stage_deadline: float)`, frozen, `repr` with no values. `stage_deadline` is a `time.monotonic()` value.
- `CloudReply(answer: Answer | None, model_id: str, tokens_in: int | None, tokens_out: int | None, outcome: str, retry_after: float | None = None)`.
- `Adapter` (Protocol): `name: str`, `make_sdk(http_client: httpx.Client, key: ApiKey, base_url: str) -> Any` (with `max_retries=0`), and `call(sdk: Any, prompt: CloudPrompt, model: str, timeout: float) -> CloudReply`. `call` maps the SDK's own result to a reply. It never raises for a provider status: the status becomes the outcome, via `retry.classify`.
- `CloudModelClient(provider: ProviderSettings, adapter: Adapter, build: Builder, local_versions: Mapping[str, str], transport: httpx.BaseTransport, *, policy: RetryPolicy | None = None, clock=time.monotonic, sleep=time.sleep)`:
  - `mask(text) -> MaskedText` with its own `TaggingMasker`.
  - `generate(question, patient_context, patient_id, chunks, *, call)`, in this order. `call is None` or `patient_id is not None` gives `egress_blocked`. Build the orchestrator. `retrieve(question, top_k=5, patient_id=None)`. Map the results to `SharedChunk`s (anything else: `egress_blocked`). Build `CloudContext.from_masked` (metric `mhai_cloud_context_dropped_total` += dropped) and `CloudPrompt`. Then the attempt loop: estimate from a dry render (the adapter's request is rendered once without sending, to measure the body; an `EgressRefused` here is `egress_blocked`), `call.budget.reserve` (`None` gives `budget_exhausted`), a per-attempt `httpx.Client(transport=transport, event_hooks={"request": [ScanHook(call.identifiers)]}, follow_redirects=False, trust_env=False, timeout=httpx.Timeout(connect=5, read=left, write=left, pool=5))`, `adapter.call`, and settle in `finally`. **The hook's verdict wins over any exception the SDK raised** (Review Focus 2). A version check on `model_id` (`_SAFE_VERSION`) gives `malformed` when it fails. `insufficient_evidence` gives `insufficient`. An `ok` reply gives `Generation(available=True, response=answer, cited_chunk_ids=citations, retrieved_chunk_ids=sent ids, model_used=f"{provider}/{model}", versions={"cca": f"{provider}/{model}@{model_id}", "cca_route": "cloud"}, egress=provider, cloud_outcome="ok", tokens…, handle=orch)`. Anything else gives `Generation(available=False, failed_agents=("CCA",), egress=provider if hook.sent else "none", cloud_outcome=<code>)`. Time out of the stage gives `timeout`.
  - `verify(generation)`: `_escalated` unless available, then `_verify_with(handle, generation)`, exactly as local.
  - `model_versions()`: `local_versions` without `cca` (the generation supplies it), so `dense` and `nli` stay recorded.
- `build_cloud_clients(settings: CloudSettings, build, local_versions) -> dict[str, CloudModelClient]`: one shared `httpx.HTTPTransport(proxy=settings.proxy_url, verify=True, retries=0)`, adapters by name (Tasks 11–13 register them; until then `build_cloud_clients` refuses an enabled provider with no adapter at start).

- [ ] **Step 1: Write the failing tests** (`FakeAdapter` returns scripted `CloudReply`s; `CapturingTransport` records every request that reaches the transport and answers from a script; `--disable-socket` on the module):
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
  - `test_egress_none_when_nothing_sent`.
- [ ] **Step 2: Run (Python, remote):** `uv run pytest -q tests/boundary/cloud/test_client.py tests/boundary/test_models.py` → fails.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the tests and the whole `tests/boundary -m "not boundary"`; lint, format, mypy.
- [ ] **Step 5:** Commit: "CloudModelClient: local retrieval and verification, a gated, scanned, budgeted cloud draft".

### Task 10: `ModelRouter` and `run_query` on both paths

**Files:**
- Modify: `intelligence/boundary/models.py` (`ModelRouter`, `build_model_router`), `intelligence/boundary/jobs_repo.py` (`_CLAIM`, `Claim.model_route`, `Decision.egress`, `Decision.cloud_outcome`, the fact payload, `guardrail_report`), `intelligence/boundary/jobs/run_query.py`, `intelligence/boundary/services/pipeline.py`, `tests/boundary/test_run_query.py`, `tests/boundary/test_models.py`
- Create: `tests/boundary/cloud/test_router.py`, `tests/boundary/cloud/test_property.py`

**Interfaces:**
- `jobs_repo._CLAIM` returns, after `retry_count`, `CASE WHEN model_route <> 'local' AND EXISTS (SELECT 1 FROM hospital_model_policy p WHERE p.hospital_id = clinical_queries.hospital_id AND 'cloud:' || p.cloud_provider = clinical_queries.model_route) THEN model_route ELSE 'local' END`. This is the SQL Task 1's test runs. `Claim.model_route: str = "local"`. A value outside `vocab.ROUTES` is treated as `unconfigured`.
- `ModelRouter(local: ModelClient, cloud: Mapping[str, ModelClient])`: `client_for(route: str) -> tuple[ModelClient, str]` returns the client and its provider (`"local"` for local). A cloud route whose provider isn't in `cloud` gets `_Unconfigured(local)`: it masks with the local masker, and its `generate` returns `cloud_outcome="unconfigured"`, logs `cloud.unconfigured provider=<p>` and counts `mhai_cloud_unconfigured_total{provider}`. `build_model_router(settings) -> ModelRouter`: `build_model_client(settings)` for local, and `build_cloud_clients` when `settings.cloud.providers` is not empty. `RunQuery.__init__(db, models: ModelClient | ModelRouter, …)` wraps a plain client in `ModelRouter(models, {})`, so existing callers and tests are unchanged.
- `run_query._pipeline(job, claim)`: `client, provider = router.client_for(claim.model_route)` before masking; every stage uses `client`. The reviewer copy is `_plain_tree(context)` (deviation 8). On a cloud route: `identifiers = collect_identifiers(claim.question, patient)`, made on the event loop from the raw row and held in a local variable only. `call = CloudCall(identifiers, DbBudget(db, job.v, loop), stage_deadline=monotonic() + min(120, job.v.exp - clock() - 90))`, and `generate(question, context, None, chunks, call=call)`. A stage deadline ≤ 0 gives `timeout` with nothing sent. On local, `call=None`.
- `_decision(client, g, v)`: `model_versions = {**client.model_versions(), **g.versions}`, then `_clean_versions`; the local path adds `cca_route: local`. `guardrail_report` gains `cloud_outcome`, `tokens_in` and `tokens_out` on a cloud route. `Decision.egress = g.egress` and `Decision.cloud_outcome = g.cloud_outcome`. `jobs_repo.complete`'s payload adds `egress` always and `cloud_outcome` when set.
- `pipeline.job_handlers` builds the router with `build_model_router(settings)`. `ApproveCorrection` is unchanged and gets no router.

- [ ] **Step 1: Write the failing tests:**
  - in `test_run_query.py` (`boundary`), parametrise the existing suite with a fixture `client_kind in ("stub", "cloud_fake")`. `cloud_fake` is a `CloudModelClient` over a `FakeAdapter` and a stub builder (no models), with the hospital's policy set to `anthropic` and `CORE`'s trigger choosing the route. Claim, lease, `+WIP`, cancel, deadline, redelivery and the attempts cap all pass on both paths (§12).
  - new `boundary` tests: `test_route_changed_between_publish_and_claim_runs_local` (A14 and Review Focus 5); `test_message_cannot_choose_route` (a `RunQueryJob` with extra fields or a forged token claim changes nothing; the route comes from the row); `test_cloud_answer_records_provider` (A16: `model_versions["cca"]` starts `anthropic/`, `model_versions["cca_route"] == "cloud"`, the outbox payload has `egress: "anthropic"`, `cloud_outcome: "ok"`, and core's drainer accepts it); `test_cloud_failure_completes_escalated` (each outcome: status completed, decision `ESCALATE`, no trust score, `cloud_outcome` in the report and fact, no Nak); `test_unconfigured_provider_escalates_and_alerts`; `test_no_text_in_logs_spans_dead_letters_on_cloud_path` (A18: the existing `test_marker_never_in_logs_spans_or_dead_letters` pattern, with the marker in the question, the patient data, the chunk text and the fake reply, over `ok` and every failure outcome); `test_reviewer_copy_is_plain_and_masked`; `test_budget_race_through_jobs` (A11: 20 jobs against a budget for 5 → at most 5 adapter calls; the rest `budget_exhausted`).
  - `test_property.py` (`boundary`, Hypothesis, A1, at most 50 examples, derandomised): synthetic records with random synthetic names, MRNs (string and integer), Nigerian phone numbers, NINs, addresses and dates, in every field, at nested levels, as dict keys and in the question. It runs the whole job through a `CapturingTransport`, and asserts that no raw identifier value (normalised as the scan does) appears in any captured body, and that each job either sent a clean body or ended `egress_blocked`.
  - unit: `test_router_picks_one_client_per_job`, `test_router_unknown_route_is_unconfigured`, `test_runquery_accepts_plain_client`.
- [ ] **Step 2: Run (Python, remote):** `uv run pytest -q tests/boundary/cloud/test_router.py` and `-m boundary tests/boundary/test_run_query.py tests/boundary/cloud/test_property.py` → fail.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the whole `tests/boundary` (both markers), `tests/security`, lint, format, mypy; `make smoke` remotely (stub, local route: unchanged).
- [ ] **Step 5:** Commit: "run_query routes each job by its claimed row, records provider, egress and outcome".

**PR C:** after Task 10, run `security-review` and open the PR.

### Part D: adapters (PR D)

Each adapter task has the same shape: the request test (exact JSON body and headers), the reply mapping, a fixture per case, and `--disable-socket`. The fixtures are synthetic: they are written from the provider's documented response shape, with ids like `msg_SYNTHETIC0001`, and are reviewed in the PR. Every fixture passes `scripts/scrub_cloud_fixture.py --check` (Task 15). Before the request test is written, the implementer reads the locked SDK version's request types. Where the design names a field (`output_config`, `text.format`, `store`, `response_format`), the test pins the field the SDK actually sends. If the SDK lacks it, stop and ask in the PR.

### Task 11: The Anthropic adapter

**Files:**
- Create: `intelligence/boundary/cloud/anthropic_adapter.py`, `tests/boundary/cloud/test_anthropic.py`, `tests/fixtures/cloud/anthropic/{ok,refusal,max_tokens,invalid_json,extra_keys,unknown_citation,markers_mismatch,link,overloaded_529,rate_429,server_503,unauthorized_401,redirect_301}.json`
- Modify: `intelligence/boundary/cloud/client.py` (register `anthropic`)

**Interfaces:**
- `AnthropicAdapter.make_sdk` gives `anthropic.Anthropic(api_key=key.reveal(), base_url=BASE_URLS["anthropic"], http_client=…, max_retries=0)`.
- `call` uses Messages: `model`, `max_tokens=1500`, `system=SYSTEM_PROMPT`, a single `user` message whose content is text blocks only (the chunks, each delimited `<<chunk KB#nnnn>> … <<end>>`, the masked question, the two context fields), structured output with `ANSWER_SCHEMA` (`output_config.format` per §3.1, via `messages.parse` or the raw `create` with the schema), effort `medium` (§16). It never sends `temperature`, `top_p`, `top_k`, `metadata`, `tools`, `tool_choice`, the `fallbacks` parameter, files, images, documents, batches or prior turns.
- Mapping: `stop_reason == "refusal"` gives `refused`; `max_tokens` gives `truncated`; a parse failure gives `malformed`; `usage.input_tokens`/`output_tokens`; `model` from the response is `model_id`; 529 is retryable.

- [ ] **Step 1: Write the failing tests:** `test_request_shape` (A8: the captured body's keys are exactly the allowed set; no sampling fields, no `metadata`; text blocks only, which fails if any `image`, `document` or `file` block can be produced from any `CloudPrompt`; URL `https://api.anthropic.com/v1/messages`); `test_fixture_outcomes` (one case per fixture, through `CloudModelClient` with the real scan); `test_301_not_followed` (A17); `test_sdk_error_logged_by_type_and_status_only` (A10: the fixture's 401 body echoes the request; `caplog` and the exception string hold neither the key nor any prompt text); `test_hook_refusal_is_egress_blocked_not_unavailable_and_not_retried` (with the real SDK); `test_no_socket_opened` (A5: the adapter called with a plain `str` or `dict` raises before the transport; `pytest-socket` proves no connection).
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
- `call` uses Responses: `model`, `instructions=SYSTEM_PROMPT`, `input` (text only), `max_output_tokens=1500`, `store=False`, `text.format` `json_schema` with `strict: true` and `ANSWER_SCHEMA`. Never `previous_response_id`, `conversation`, `tools`, `user`, `metadata`, `safety_identifier`, `prompt_cache_key` or `background`.
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
- `call` uses Chat Completions through `with_raw_response`, so the headers are read: `messages` (system and one user, text only), `max_tokens=1500`, `response_format` `json_schema` with `strict: true`. No `tools`, `user`, `store`, `search_parameters` or image parts.
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
- `squid.conf`: `http_port 3128`. `acl providers dstdomain "/etc/squid/allowed-hosts"`, `acl SSL_port port 443`, `http_access allow CONNECT SSL_port providers`, `http_access deny all`. No caching, no `ssl_bump`, `forwarded_for delete`, `via off`. The access log carries method, host, port and status only (`logformat` without the URL path). `allowed-hosts` holds one line per enabled provider, from `vocab.HOSTS`; the example file lists none.
- `test-egress.sh` (run remotely with the stack up, allowed hosts set to `api.anthropic.com`): `CONNECT example.com:443` → 403; `CONNECT api.openai.com:443` → 403; `CONNECT api.anthropic.com:80` → 403; `CONNECT api.anthropic.com:443` → 200; from a container on `backend` only, a direct TCP connection to `1.1.1.1:443` fails.

- [ ] **Step 1: Write the failing tests:** in `compose_test.go`: `TestEgressProxyOnlyServiceOnEgressNetwork` (only `egress-proxy` joins `egress`), `TestPipelineNeverOnEgress` (the pipeline block, when uncommented by the test's parser of the commented YAML, is on `backend` only), `TestEgressProxyPinnedByDigest`, `TestNoProviderKeyInAnyEnvironment` (no service's `environment` has a `*_KEY` or `*_API_KEY` for a provider; core has no `INTELLIGENCE_CLOUD_*`). `test-egress.sh` (A15) fails until the service exists.
- [ ] **Step 2: Run (Go, remote):** `go test -race ./cmd/core/ -run Compose` → fails. Run `test-egress.sh` remotely → fails.
- [ ] **Step 3:** Write the config, the compose service and `deploy/README.md`'s "Cloud models" section: enabling a provider (§15 steps 1–5), the key file (mode 0400, owner the worker user), rotation every 90 days and on suspicion (write the new file, rolling restart, revoke the old key), the provider-side spend limit and endpoint scope, and switching off (policy to `none`, then remove the key file and the host from `allowed-hosts`).
- [ ] **Step 4:** Run both test sets remotely.
- [ ] **Step 5:** Commit: "An egress proxy that allows only the enabled providers' API hosts on 443".

### Task 15: The manual live smoke and fixture recording

**Files:**
- Create: `services/intelligence/scripts/cloud_smoke.py`, `services/intelligence/scripts/scrub_cloud_fixture.py`, `tests/boundary/cloud/test_smoke_guard.py`
- Modify: `services/intelligence/pyproject.toml` (`[tool.pytest.ini_options] norecursedirs` keeps `scripts`; the script is never collected)

**Interfaces:**
- `cloud_smoke.py --provider anthropic|openai|xai --key-file <path> --model <id> [--record <dir>]`. It refuses unless `MHAI_CLOUD_SMOKE=1`, the key file passes `load_api_key`, and `APP_ENV` is `development` or `demo`. It builds the real `CloudModelClient` with a stub builder over three fixed synthetic shared guideline chunks (in the script) and a `FakeBudget` capped at 3 attempts. It sends one fixed synthetic question with a synthetic `patient_data` (the identifiers are recognisably synthetic, `SYN-` prefixed), through the real gate and scan, and prints only: provider, outcome, `versions["cca"]`, token counts, the retention header (xAI) and whether the answer validated. With `--record`, it writes the raw reply, passed through the scrubber.
- `scrub_cloud_fixture.py <in> <out>` / `--check <file>…`: removes or replaces response ids, request ids, organisation and project ids, `date`, `x-request-id`, `request-id`, `cf-ray`, `openai-organization`, `anthropic-organization-id`, rate-limit headers and timing fields with fixed synthetic values. `--check` fails on any field outside an allow-list, any string matching the key shapes, and any id not `SYNTHETIC`.

- [ ] **Step 1: Write the failing tests:** `test_smoke_refuses_without_env_and_key` (subprocess, no network: exit code 2 and a message naming the variable); `test_smoke_not_collected_by_pytest`; `test_scrubber_check_passes_committed_fixtures`; `test_scrubber_check_fails_on_key_shape_and_real_ids`.
- [ ] **Step 2: Run (Python, remote):** `uv run pytest -q tests/boundary/cloud/test_smoke_guard.py` → fails.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run the tests. **Manual, once H1 is done (a person, on steven-reid, never on a laptop and never in CI):** `MHAI_CLOUD_SMOKE=1 APP_ENV=demo uv run python scripts/cloud_smoke.py --provider anthropic --key-file /run/secrets/anthropic-api-key --model claude-opus-5-5 --record tests/fixtures/cloud/anthropic/recorded/`. Then run the adapter suite against the recorded fixtures, and paste the printed summary (no text) in the PR. Without H1, the PR says the live run is pending and merges with the synthetic fixtures.
- [ ] **Step 5:** Commit: "A manual, opt-in live smoke test with synthetic data, and a fixture scrubber".

### Task 16: Docs, the human gates in TODO, and the abuse-case check

**Files:**
- Create: `tests/boundary/cloud/test_abuse_cases.py`
- Modify: `TODO.md`, `CHANGELOG.md`, `ARCHITECTURE.md` (the cloud route, the egress proxy), `services/intelligence/README.md` ("Cloud models": settings, what leaves, outcomes), `CONTRIBUTING.md` (clinical change control gains `gate.SYSTEM_PROMPT`, `CloudContext`'s allow-list, `scan.IDENTIFYING_KEYS` and the configured cloud model ids), `docs/specs/2026-11-03-cloud-model-providers-design.md` (§18 "As built", listing this plan's departures), `.claude/skills/security-review/SKILL.md` (two new classes: an SDK wrapping a pre-send refusal into a retryable error; a tagged type losing its tag through a copy made for another purpose)

**TODO.md entries (exact wording, each owned by a person, under "What's left"):**
- **Cloud models: before any real patient data (human gate, design §13.6).** (1) The masker upgrade (Nigerian identifiers and a local NER for names), measured on a synthetic Nigerian PII set at recall ≥ 0.99 for names and ≥ 0.999 for numeric identifiers, signed off by the privacy lead. (2) The legal check: each hospital's data agreement allows a processor outside Nigeria; NDPA 2023 cross-border transfer and processor terms; each provider's DPA and zero-retention terms signed; the agreement reference recorded per hospital. (3) The clinical evaluation of cloud answers on the synthetic evaluation set (pass and escalate rates, NLI support), signed off clinically. Until all three are done, cloud generation runs on synthetic data only. No code flag represents this gate.
- **Cloud models: egress isolation (design §16 question 6).** Before real data, decide whether the risk that a compromised worker sends data to an allowed provider (C9) is accepted, or the cloud call moves into its own service.
- **Cloud models: the live smoke and recorded fixtures** (waiting on H1).
- The existing "Cloud models" line is replaced by the build status.

- [ ] **Step 1: Write the failing test** `test_every_abuse_case_has_a_test`: it reads this plan's "Abuse cases → tests" table and checks each named test exists under `services/intelligence/tests/` or `services/core/` (by name, with `grep`). Also `test_todo_keeps_the_real_data_gate`: TODO.md contains the gate's heading and the three numbered items, so a later edit that removes them fails CI.
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
| row → worker (claim) | RLS for the token's hospital; the route re-checked against the current policy in the claim statement (Tasks 1, 10) |
| worker → budget | `mhai_cloud_reserve`/`settle` for `mhai_current_hospital()` only, atomic, fail closed (Tasks 1, 7) |
| worker → adapter | `CloudPrompt` from tagged `MaskedText`, an allow-listed `CloudContext`, sealed `SharedChunk`s (Task 5) |
| adapter → transport | `ScanHook` on the exact bytes: identifiers, patterns, size; a verdict that overrides the SDK's error mapping (Tasks 6, 9) |
| transport → proxy → provider | fixed base URL, no redirects, `trust_env=False`, TLS verified, the proxy's host allow-list on 443 (Tasks 4, 14) |
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
- **A crashed worker leaves one attempt's reservation** counted as reserved until the month ends. This is bounded by one estimate per crash. Accepted.
- **Provider legal-hold and abuse exceptions to zero retention.** Accepted with the legal check (legal sign-off).
- **HGA thresholds were tuned on Qwen.** Cloud answers need the clinical evaluation before clinicians rely on them (clinical sign-off).
- **Sign-off needed:** privacy (masker bar), legal (NDPA, agreements, DPAs, ZDR), clinical (evaluation; `SYSTEM_PROMPT` and the allow-lists under change control), product owner (the key, the demo account and spend limit).

---

## Abuse cases → tests

Every case is a named test, written before the code it proves.

| Case | Test (task) |
|---|---|
| A1 synthetic identifiers everywhere never reach a body | `test_property.py` (10); `test_prompt_holds_only_allowed_parts` (9) |
| A2 a name without a title is blocked | `test_name_without_title_blocked` (6) |
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
| A14 a policy switched off before the claim runs local | `TestClaimRunsLocalWhenPolicyOffOrProviderChanged` (1); `test_route_changed_between_publish_and_claim_runs_local` (10) |
| A15 the proxy refuses other hosts and ports; the pipeline has no direct route | `test-egress.sh`, `TestPipelineNeverOnEgress` (14) |
| A16 the fact records `egress` and the provider; unknown values refused | `TestQueryCompletedEgressAndOutcome` (3); `test_cloud_answer_records_provider` (10) |
| A17 redirects not followed; base-URL override refused outside development | `test_base_url_override_only_in_development` (4); `test_301_not_followed` (11–13) |
| A18 no text in logs, spans or dead letters on the cloud path | `test_no_text_in_logs_spans_dead_letters_on_cloud_path` (10) |
| K1 `MaskedText` copied or pickled | `test_masked_text_cannot_be_pickled_or_copied` (5) |
| K2 a `SharedChunk` made outside `kb_repo` | `test_shared_chunk_only_from_kb_repo` (5); `test_imports.py` (5) |
| K3 the widened audit policies admit only the new action, without hospital or actor | `TestPlatformAppendAllowsModelPolicyChangedOnlyWithoutHospital`, `TestProvisionAppendAllowsItOnlyWithoutActor` (1) |
| K4 the worker reads or writes the ledger directly, or reserves for no hospital | `TestWorkerHasNoGrantOnCloudUsage`, `TestReserveRefusesNonPositiveHugeAndNoHospital` (1) |
| K5 settle used to lower another hospital's usage or go negative | `TestSettleOnlyOwnHospitalAndNeverNegative` (1) |
| K6 the CLI enables an unknown or disabled provider | `TestCLIModelPolicySet` (2) |
| K7 the route enables a provider the deployment hasn't | `TestModelPolicyPutRefusesDisabledProvider409` (2) |
| K8 the worker starts with a provider but no proxy, model or ZDR reference | `test_settings_refusals` (4) |
| K9 `DEMO-SYNTHETIC` accepted in production | `test_demo_accepts_demo_synthetic_with_warning_production_refuses` (4) |
| K10 per-job identifiers leak between concurrent jobs | `test_per_call_state_not_on_client` (9) |
| K11 the SDK's wrapped refusal is retried | `test_hook_refusal_is_egress_blocked_not_unavailable_and_not_retried` (9, 11) |
| K12 a non-retryable outcome is retried | `test_never_retry_on_400_401_403_404_refusal_truncated_malformed_egress` (8) |
| K13 a message or token chooses the route | `test_message_cannot_choose_route` (10) |
| K14 Go and Python outcome lists drift | `TestOutcomeVocabularyMatchesWorker` (3); `test_vocab_lists_are_closed_and_sorted` (4) |

## What's needed from people, and when

Nothing below blocks Tasks 1–16. The live smoke (Task 15 step 4) and the demo wait on H1 and H5. Real data waits on H2–H4.

| # | Who | What, exactly | Needed by |
|---|---|---|---|
| H1 | Product owner | **An Anthropic API key** in a workspace used only by this deployment, with a USD 50/month spend limit (§16), inference scope only where offered; ZDR requested from Anthropic at the same time, its reference given to the platform team. If OpenAI is wanted later: start its ZDR request with sales now. xAI: confirm the team's ZDR Console setting. | Task 15's live run; the demo |
| H2 | Privacy lead | **The masker bar** (recall ≥ 0.99 names, ≥ 0.999 numeric identifiers on a synthetic Nigerian PII set) and its sign-off, after the masker upgrade (a separate plan). | Before any real patient data |
| H3 | Legal / compliance lead | **The legal check:** hospital data agreements (processor outside Nigeria), NDPA 2023 cross-border transfer and processor terms, each provider's DPA and ZDR terms; the `agreement_ref` format the platform admins will record. | Before any real patient data |
| H4 | Clinical lead | **The clinical evaluation** of cloud answers on the synthetic evaluation set, and review of `gate.SYSTEM_PROMPT` (clinical change control). | Before a hospital's clinicians rely on cloud answers |
| H5 | Product owner / platform | **The demo host:** CPU host with memory for the embedding and NLI models, the pipeline service enabled (its NKey, database login, trusted keys), the key file mounted. | The demo |
| H6 | Product owner | **Egress isolation** (§16 question 6): accept C9 for real data, or schedule the separate cloud service. | Before any real patient data |

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

- **Names across tasks:** `vocab.PROVIDERS/ROUTES/EGRESS/OUTCOMES/BASE_URLS/HOSTS`; `ApiKey`, `load_api_key`, `RedactingFilter`, `install_redaction`; `ProviderSettings`, `CloudSettings`; `MaskedText`, `TaggingMasker`, `SharedChunk`, `seal_shared_chunk`, `CloudContext.from_masked`, `CloudPrompt`, `SYSTEM_PROMPT`, `EgressRefused`; `Identifiers`, `collect_identifiers`, `normalise`, `scan_body`, `ScanHook`; `estimate_tokens`, `Reservation`, `BudgetPort`, `DbBudget`; `ANSWER_SCHEMA`, `Answer`, `validate_answer`, `Malformed`; `RetryPolicy`, `classify`; `CloudCall`, `CloudReply`, `Adapter`, `CloudModelClient`, `build_cloud_clients`; `ModelRouter`, `build_model_router`, `_verify_with`, `local_parts`; `Claim.model_route`, `Decision.egress`, `Decision.cloud_outcome`; SQL `hospital_model_policy`, `cloud_usage`, `mhai_set_model_route`, `mhai_cloud_reserve`, `mhai_cloud_settle`, `mhai_cloud_usage`; Go `Config.CloudProviders`, `registerModelPolicy`, `GeneratedBy`.

# intelligence

The Python side of MHAI: the clinical agents, guardrails, retrieval, the
NATS workers and the `intelligence` CLI. It is a uv project (ADR 0002) with
its own `pyproject.toml` and `uv.lock`; run every command from this directory.

```bash
uv sync --locked          # install (CPU PyTorch by default)
uv run ruff check .
uv run ruff format --check .
uv run mypy
uv run pytest -m "not models and not boundary"   # no Docker, no model downloads
uv run pytest -m boundary tests/boundary          # Docker: Postgres 16 and NATS 2.10 (needs mhai-core:test)
uv run intelligence run --query "Test query" --urgency routine
```

## The workers (`intelligence/boundary/`)

`intelligence worker --service pipeline|echo` serves one service over NATS
(boundary design, `docs/specs/2026-10-28-intelligence-boundary-design.md`).
Core signs a request token for every call and job; the worker checks it
(signature, lifetime, subject, body hash, reply inbox, replay, schema,
hospital scope, role from the generated `operations.json`) before it parses
the body or opens the database, and refusals do no work. A call the
clinical BFF makes itself (`bff_direct`, only `echo.ping`, test-only) is
served only once core grants it. Only core writes the audit chain: the
workers hand it facts.

- **pipeline:** `pipeline.health` and the `run_query` jobs (the clinical
  question), on its own database login (`intelligence_app`, a member of
  `mhai_worker_app` only), reading and writing under row-level security for
  the token's hospital only.
- **pipeline, learning (Plan 4a):** the `approve_correction` jobs, from a
  second consumer (`pipeline_learning`) so they never wait behind clinical
  questions. The job checks that the correction is approved by a different
  person, stores one `feedback_buffer` row (the four expert scores, the
  system decision, the model versions; never free text) and hands core a
  `correction.applied` fact. It changes no weight and writes no knowledge
  base: `INTELLIGENCE_LEARNING_CLIENT` is `trust_buffer` (default) or
  `stub` (development only).
- **echo:** tests and the smoke run only; refuses to start outside
  development.

Settings (environment): `APP_ENV` (unset is production: NKeys, NATS TLS and,
for the pipeline, `sslmode=verify-full` are then required), `NATS_URL`,
`NATS_NKEY_SEED_PATH`, `NATS_TLS_*`, `INTELLIGENCE_TRUSTED_KEYS_PATH` (core's
public request keys, mounted read-only; re-read on SIGHUP),
`INTELLIGENCE_DB_URL` with `INTELLIGENCE_DB_USER` and
`INTELLIGENCE_DB_PASSWORD` or `INTELLIGENCE_DB_PASSWORD_FILE`,
`INTELLIGENCE_MODEL_CLIENT` (`orchestrator`, or `stub` in development only),
`INTELLIGENCE_MAX_CONCURRENCY`, and `OTEL_EXPORTER_OTLP_ENDPOINT`. A bad
setting stops the worker with a message that names variables, never values.

## Cloud models (off by default)

A hospital a platform admin has opted in can have its clinical drafts
written by a cloud model: Anthropic, OpenAI or xAI (design
`docs/specs/2026-11-03-cloud-model-providers-design.md`, ADR 0013, plan
`docs/plans/2026-11-06-cloud-model-providers.md`). Core decides the route
per question from its own policy table and re-checks it at the claim; the
worker never reads a route, hospital or budget from a message.
`intelligence/boundary/cloud/` holds the gate, the egress scan, the budget
port, reply validation, `CloudModelClient` and one adapter per provider.
**Demo and synthetic data only** until the human gate in TODO.md is done.

**What leaves**, and nothing else: the masked question (`MaskedText`, tagged
by the masker), two allow-listed context fields (`CloudContext`:
`disease_category`, `risk_score`), at most five shared guideline chunks
(sealed `SharedChunk`s; never a hospital's or a patient's), and the
constant `gate.SYSTEM_PROMPT`. Documents never leave. Retrieval and
verification (NLI, HGA) stay local. The exact request bytes are scanned
before they reach the transport (shape, this patient's identifiers anywhere,
number and date patterns on the question and context, size), and any
refusal sends nothing.

**Outcomes** (closed; on the `query.completed` fact as `cloud_outcome`, with
`egress` naming the provider or `none`): `ok`, `egress_blocked`,
`budget_exhausted`, `unconfigured`, `unavailable`, `timeout`, `auth_failed`,
`refused`, `truncated`, `malformed`, `insufficient`,
`retention_unconfirmed`. Every outcome but `ok` escalates (no score, no
fallback to local or to another provider). A policy switched off before the
claim runs local.

**Settings** (pipeline only; all unset by default):

- `INTELLIGENCE_CLOUD_PROVIDERS`: a comma list of `anthropic`, `openai`, `xai`.
- Per provider: `INTELLIGENCE_CLOUD_<PROVIDER>_MODEL` (an exact model id; no
  Fable or Mythos), `INTELLIGENCE_CLOUD_<PROVIDER>_KEY_FILE` (mode 0400 or
  0440; never a variable holding the key), and outside development
  `INTELLIGENCE_CLOUD_<PROVIDER>_ZDR_REF` (`DEMO-SYNTHETIC` only with
  `APP_ENV=demo`, which is production in every other respect).
- `INTELLIGENCE_CLOUD_PROXY`: the egress proxy, `http://<host>:<port>`,
  required outside development. It is not `HTTPS_PROXY`: that name is
  process-wide, and the cloud transport ignores the environment.
- `INTELLIGENCE_CLOUD_<PROVIDER>_BASE_URL`: development only.

Start-up refuses any of: an incomplete provider; a key file with the wrong
mode, a placeholder, whitespace or under 20 characters; `SSL_CERT_FILE` or
`SSL_CERT_DIR` with a provider enabled; no CA bundle at OpenSSL's paths with
a provider enabled. **Any `OPENAI_*`, `ANTHROPIC_*`, `AZURE_OPENAI_*` or
`XAI_*` variable refuses start-up for every worker, cloud on or off** (a
developer shell exporting `OPENAI_API_KEY` will hit it): the SDKs read keys,
base URLs, headers and logging from them.

**Logging changes with a provider configured:** records are redacted at
creation (`args=None`, exception text pre-rendered, provider-shaped keys and
each configured key's value replaced), warnings go through logging,
uncaught exceptions are logged as `CRITICAL`, and the HTTP clients' INFO
request lines are off. SDK errors are logged by type and status only.

**Metrics and alerts:** `mhai_cloud_alerts_total` by `kind` (`auth_failed`,
`unconfigured`, `usage_overrun`), `mhai_cloud_budget_alerts_total` (80% of
the month), `mhai_cloud_settle_failures_total`,
`mhai_cloud_usage_overrun_total`, `mhai_cloud_settle_clamped_total` and
`mhai_cloud_context_dropped_total`. Who receives them is TODO H5.

**SDK upgrades** of `openai` or `anthropic` are reviewed changes: the scan
refuses any request header not on `scan.ALLOWED_HEADERS` and any field the
adapter's Shape doesn't fix, so an SDK that adds one fails closed.

**The live smoke** (`scripts/cloud_smoke.py`) is run by one person on their
own machine, never on the build server or in CI, with a short-lived key
from standard input (plan Task 15; TODO H7). `scripts/scrub_cloud_fixture.py`
turns its recording into a fixture; `--check` runs on every committed
fixture in the unit suite. Deployment, rotation and switching off:
`deploy/README.md`, "Cloud models".

In compose, `make up` runs `pipeline`; `docker compose --profile test up -d`
adds `echo`. `make smoke`, from the repository root, runs the whole path end
to end with the stub models.

See the repository's top-level README.md and ARCHITECTURE.md for the whole
system, and `services/core/` for the Go service in front of it.

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

## Evaluating an extractor

`mhai-doceval` measures an extractor on synthetic clinical documents. Every
page, patient and identifier it makes is invented; MRNs start `SYN-`.

```bash
uv run mhai-doceval build-set --purpose dev --out /tmp/dev-set --scale 0.1
uv run mhai-doceval scan /tmp/dev-set               # checks every identifier is synthetic
uv run mhai-doceval run --extractor tiny --model RUN_DIR --set /tmp/dev-set --out /tmp/eval
uv run mhai-doceval report /tmp/eval/evaluation.json
uv run mhai-doceval compare OLD/evaluation.json NEW/evaluation.json
```

- `build-set` makes the development split or a training set. It never makes
  the test split: that is built and locked inside the model store
  (`mhai-models testset-build`), and only the evaluation jobs can read it.
- `run` writes `evaluation.json` and `evaluation.md` into a new folder. It
  works with the tiny test model (`tiny`), any OpenAI-compatible endpoint
  (`openai`, for vLLM once the GPU host exists), and test stubs.
- The report holds totals only: scores per field and per slice (scans,
  photos, handwriting, hostile pages), timings and a verdict for each gate.
  It never holds a page, a value or a per-document row.
- The pass marks are in `intelligence/doceval/thresholds/` (`phase1.json`,
  `phase3.json`). Changing them is a clinical change (CONTRIBUTING.md). The
  `rehearsal` gate is only for the tiny test model and can never be pinned.

## Promoting a model

A trained model reaches production in three steps: **promote**, **pin** (a
normal PR, whose one reviewer's approval is the sign-off) and **load**. Each
step checks the one before it. Any failure stops with a fixed reason code,
and nothing falls back to another version. The AI service never loads a
model whose files, hash or signature don't match `models.lock`.

### 1. What goes in the Drive folder

Save each training run to `MHAI models/models/<name>/<run>/` in Google
Drive:

- the weights, as `*.safetensors` files only;
- the model's JSON files, from a fixed list (`config.json`,
  `tokenizer.json`, `tokenizer_config.json`, `generation_config.json`,
  `special_tokens_map.json`, `preprocessor_config.json`,
  `chat_template.json`, `adapter_config.json`,
  `model.safetensors.index.json`);
- `training_manifest.json` (example: `docs/models/training_manifest.example.json`).
  It names the model, its role (`extract` today), who trained it (a GitHub
  login, lowercase), the datasets used, the base model with its exact
  revision and licence, the library versions, and the notebook's own
  metrics.

Nothing else is accepted: no `.bin`, `.pt`, `.pkl`, `.py`, sub-folders or
shortcuts. Datasets must be listed in `models/approved-datasets.json`, and in
phase 0 only synthetic ones are allowed. Drive and notebooks hold synthetic
data, or de-identified data with an approval record; nothing else, ever.

### 2. Check it before you dispatch

```bash
uv run mhai-models check RUN_DIR    # file and training-manifest checks; prints the files hash
MHAI_MODELS_LOCAL_ROOT=/path/to/root \
  uv run mhai-models promote NAME VERSION --source local --run RUN --dry-run
```

For `--source local`, `MHAI_MODELS_LOCAL_ROOT` names a folder laid out like
the Drive one: the run must be at `$MHAI_MODELS_LOCAL_ROOT/models/NAME/RUN/`.
Without it the dry run is refused.

`check` runs the same file checks as the promotion. A dry run on your own
machine also runs the evaluation, but on the development split only, and
the result says so: it is not a promotion result. It writes nothing.

### 3. The workflows

No GitHub environment, branch protection or other setting is needed, only
repository secrets and variables. The secrets: `MODEL_STORE_WRITER_ACCESS_KEY` and
`MODEL_STORE_WRITER_SECRET_KEY`, `MODEL_STORE_READER_ACCESS_KEY` and
`MODEL_STORE_READER_SECRET_KEY` (any S3-compatible bucket; versioning is used
if it's on, not required), `MHAI_MODELS_SIGNING_KEY` (the PEM file
`mhai-models keygen` writes) and, for a Drive source,
`MODEL_DRIVE_SA_KEY` (a service account the `MHAI models` folder is shared
with as Viewer). The variables are `MODEL_STORE_ENDPOINT`,
`MODEL_STORE_BUCKET`, `MODEL_STORE_REGION` and `MODEL_DRIVE_FOLDER_ID`.

- **Model promotion** (`model-promote.yml`, started by hand): give the
  name, version, run id, source and gate.
  - Without `expect_files` it is a **dry run**: it fetches the run, checks
    every file, evaluates it on the locked test split and keeps two files,
    its JSON summary and the report `evaluation.md`. The summary ends with
    the files hash.
  - Read the report. Then dispatch again with `expect_files` set to that
    hash: the **promotion** does the same checks, refuses if the files
    differ from the dry run's, uploads the run and signs its manifest.
- **Model lock check** (`model-lock.yml`, every PR): checks the pins in a
  PR's `models.lock` against the store (see "Pin" below).
- **Model test set** (`model-testset.yml`, by hand): builds and locks a new
  test split and prints the line for `models/testsets.lock`.

Until `MODEL_STORE_ENDPOINT` is set, the promotion and test-set workflows run
as a rehearsal on a throwaway store with a test key, and say so in their
summary.

**The signing key.** Run `uv run mhai-models keygen KEY_DIR` once, on a
trusted machine. Put `KEY_DIR/signing-key.pem` in the repository secret
`MHAI_MODELS_SIGNING_KEY`, add the printed entry to
`models/trust-policy.json` in its own PR, and keep the key file offline (or
use it with the CLI: `MHAI_MODELS_SIGNER=KEY_DIR/signing-key.pem`). To
change the key, add the new entry, merge, re-sign or re-promote, then remove
the old one.

### 4. Reading the report

`evaluation.md` starts with what was measured: the model, the test set and
its SHA-256, and the split (`test` for a real promotion). Then comes the
gates table: one row per gate with its verdict, `PASS` or `FAIL`, a
**Production** column, the thresholds file and its SHA-256. Production is
`yes` for `phase1` and `phase3`. The `rehearsal` gate shows
`no (rehearsal only)`: it is the tiny test model's bar, and outside a
rehearsal a promotion refuses it and the service never loads a version
pinned on it. Under the table, each failing gate lists the checks that
failed or weren't measured. Then come the scores per field and per slice,
and the timings. The summary's
`gates_failed` lists any gate that failed. A version can be promoted with a
failed gate, but it can't be pinned for that gate.

### 5. Pin, review and rollback

- **Pin.** In a branch, run
  `uv run mhai-models pin extract NAME VERSION --gate phase1` (with the
  reader's credentials). It verifies the stored version (signature, every
  file, the gate's `pass`, the locked test set) and writes `models.lock`.
  Open a normal PR with the `clinical-review` label and the
  before-and-after reports. `lock-check` verifies it again; **one reviewer's
  approval is the sign-off**, and it should not be the person who trained
  or promoted the model.
- **Load.** At start-up the pipeline verifies each pinned role (signature,
  lock hash, every file) and runs a fixed warm-up page. Until that passes it
  reports not ready and takes no jobs. `model_versions` in every answer names
  the exact weights used. With no role pinned (no `INTELLIGENCE_MODEL_ROLES`)
  nothing changes.
- **Rollback.** Run `uv run mhai-models rollback extract` (or `--to VERSION`)
  in a branch and open a PR. Only an earlier version of the pinned model whose
  signed manifest and files still verify can come back; `lock-check` checks
  it again.

See the repository's top-level README.md and ARCHITECTURE.md for the whole
system, and `services/core/` for the Go service in front of it.

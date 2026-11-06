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

A trained model reaches production in four steps: **promote**, **pin**,
**sign-off** and **load**. Each step checks the one before it. Any failure
stops with a fixed reason code, and nothing falls back to another version.

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

- **Model promotion** (`model-promote.yml`, started by hand): give the
  name, version, run id, source and gate.
  - The *dry-run* job fetches the run, checks every file, evaluates it on
    the locked test split and keeps two files: its JSON summary and the
    report `evaluation.md`.
  - A platform reviewer reads the report and approves the
    `model-promotion` environment. The *promote* job then does the same
    checks again, refuses if the files differ from the dry run's, uploads
    the run under object lock and signs its manifest.
- **Model lock check** (`model-lock.yml`, every PR): checks the pins in a
  PR's `models.lock` (see "Pin" below). It is a required check.
- **Model sign-off** (`model-signoff.yml`, after a `models.lock` change
  merges): checks the approvals again on the merged commit and writes a
  signed sign-off next to the version. In production the service loads a
  version only once that sign-off exists.
- **Model test set** (`model-testset.yml`, by hand): builds and locks a new
  test split and prints the line for `models/testsets.lock`.

Until the production store exists, these workflows run as a rehearsal on a
throwaway store with test keys, and say so in their summary.

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

### 5. Pin, sign-off and rollback

- **Pin.** In a branch, run
  `uv run mhai-models pin extract NAME VERSION --gate phase1`. It verifies the
  stored version (signature, every file, the gate's `pass`, the locked test
  set) and writes `models.lock`. Open a PR that changes only `models.lock`
  (and `models/`, `docs/` or `CHANGELOG.md` if needed), with the
  `clinical-review` label, the before-and-after reports, and approvals from
  a clinical reviewer and a platform reviewer listed in
  `models/reviewers.json`. Neither may be the PR's author, the person who
  promoted the model or the person who trained it.
- **Sign-off.** After the merge, the sign-off workflow checks it all again
  against the merged commit and writes the sign-off. It also extends the
  version's retention in the store.
- **Load.** At start-up the pipeline verifies each pinned role (signature,
  lock hash, every file, sign-off) and runs a fixed warm-up page. Until that
  passes it reports not ready and takes no jobs. `model_versions` in every
  answer names the exact weights used.
- **Rollback.** Run `uv run mhai-models rollback extract` (or `--to VERSION`)
  in a branch and open a PR. Only versions that were signed off before can
  come back. It needs the `clinical-review` label, one platform approval and
  a line in the PR description starting `Clinical note:` that says why.

See the repository's top-level README.md and ARCHITECTURE.md for the whole
system, and `services/core/` for the Go service in front of it.

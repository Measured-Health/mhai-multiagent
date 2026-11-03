# Document pipeline phase 0: model promotion and the evaluation harness: implementation plan

**Status:** Draft, 3 Nov 2026. Not started.

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task by task. Steps use checkbox (`- [ ]`) syntax for tracking. Read the `secure-design` and `secure-build` skills (`.claude/skills/`) before Task 1, and run `security-review` on your own diff before each PR.

**Goal:** Build everything in phase 0 of the document pipeline that doesn't need trained weights, a GPU or an outside credential: the promotion flow from a training run to a pinned, verified model, and the evaluation harness that decides whether a model may be promoted. All of it is tested with tiny synthetic safetensors files and stub extractors. When the intelligence lead's real weights arrive, they go through exactly the same code. The only pieces left are three small, late tasks that each wait on a human (the signing backend, the production store, real Google Drive), and the GPU host, which is an external dependency.

**Architecture:** Two new packages in the Python project (`services/intelligence/`), plus workflows and config:

- `intelligence/modelstore/`: the `mhai-models` CLI and the library behind it. Sources (a local folder, Google Drive, a pinned Hugging Face revision) → file checks (allow-list, safetensors headers, SHA-256) → training manifest and registries → evaluation → model card, data statement and evaluation report → a signed manifest → an S3-compatible store with object lock. Then `models.lock` (pinned in a `clinical-review` PR), sign-off, rollback, and the verifier that every process loading a model runs, failing closed.
- `intelligence/doceval/`: the evaluation harness. A seeded synthetic document generator (typed, scanned-looking, handwriting-like and hostile pages, with ground truth in `mhai.extraction.clinical_document` 1.0.0), the locked test split, the metrics, the report format, and a runner that takes any extractor behind one interface.
- The intelligence service verifies every pinned role at start-up (not ready on any mismatch, never a fallback), and `ModelClient.model_versions()` reports `name@version#hash12`.
- GitHub workflows: `model-promote.yml` (dispatch, dry run, approval environment, promote), `model-lock.yml` (the pin PR's checks), `model-signoff.yml` (after merge). In phase 0 they run against MinIO and a stubbed source, so the whole flow, approval included, can be exercised before any credential exists.

**Tech stack:** Python 3.12 with uv (ADR 0002). New runtime dependencies: `boto3` (S3 API, vendor-neutral), `jsonschema`, `safetensors` (already in the lock through `transformers`; made direct), `Pillow` (already locked; made direct). New dev dependencies: `testcontainers[minio]`, `moto` is **not** used (object lock semantics must be real, so tests use MinIO). Late tasks add `google-auth` and `google-api-python-client` (Task 27) and the chosen signing backend's client (Task 25). Workflow linting: `actionlint` and `zizmor`, pinned. MinIO pinned by digest, for tests and local compose only.

**Spec:** `docs/specs/2026-10-28-document-pipeline-and-model-promotion-design.md` (approved 28 Oct 2026, all 11 decisions in §11 as recommended). This plan builds its phase 0 (§10) minus the GPU host: §4.1–§4.3 (the schema), §6.2–§6.3 (the synthetic set and metrics), §7 (promotion), §8 (clinical change control for models), and the B16–B23 abuse cases of §9.5. Context: `docs/meetings/2026-10-28-intelligence-meeting.md`; the product owner's direction of 3 Nov is to keep building everything, so only the trained weights are left to the intelligence lead.

**Builds on:** Plan 3 (`docs/plans/2026-10-28-plan-3-intelligence-boundary.md`): `ModelClient` and `model_versions()` in `services/intelligence/intelligence/boundary/models.py`, the worker's readiness (`models_ready`), and the remote test setup.

## Global Constraints

Plan 2's and Plan 3's constraints still hold (identity only from verified tokens, no PHI or secrets in logs, synthetic data only, plain commit messages with no AI attribution). In addition:

- **No real weights, no GPU, no outside credential is needed by Tasks 1–24.** Every test uses tiny synthetic safetensors files (kilobytes), stub extractors, MinIO and a local or fake source. A task that seems to need one of these is wrong; stop and ask.
- **Safetensors only.** No code path in this plan calls `torch.load`, `pickle`, `joblib`, `numpy.load(allow_pickle=True)` or `trust_remote_code=True`. A test greps the two new packages for these names (Task 13).
- **Fail closed.** Every check returns a fixed reason code from one closed set (`modelstore.reasons.REASONS`). Missing, unreadable, unexpected or unverifiable means refused. The loader never falls back to another version, a cached copy it didn't just verify, or the internet.
- **Only the promotion workflow writes models; only the sign-off workflow writes sign-offs.** Both are in GitHub environments limited to `main`. Nothing else holds store write credentials. The CLI's non-dry-run paths refuse to run without store write credentials, and the store's policies refuse every other identity.
- **Versions are immutable.** A version is reserved with a conditional create (`If-None-Match: *`) before anything is uploaded; every object is written under object lock in compliance mode; nothing is ever overwritten or deleted by our code.
- **Canonical JSON for anything hashed or signed:** UTF-8, sorted keys, `(",", ":")` separators, `ensure_ascii=True`, no floats (integers and strings only). A manifest whose stored bytes aren't the canonical dump of their own parse is refused.
- **Signatures are domain-separated.** The signed message is `b"mhai.models.<purpose>/1\n" + canonical_bytes`, purpose `manifest`, `signoff` or `testset`. A signature for one purpose never verifies as another.
- **The locked test split never leaves the store's evaluation area** except into the scratch disk of a job in the `model-eval` or `model-promotion` environment. Reports carry aggregate metrics only: no page images, values, identifiers or per-document rows from the test split.
- **Synthetic data only, and recognisably so.** Generated MRNs start `SYN-`; names, addresses and facilities come from the committed synthetic lists; the synthetic-data scanner (Task 3) runs over every generated set. Training manifests may name only datasets in the approved registry, and in phase 0 only `kind: synthetic` ones.
- **Never log** file contents, extracted values, OCR text, page text, ground truth, credentials, signed URLs or tokens. Logs carry names, versions, hashes, sizes, reason codes and timings.
- **Inputs are names, not paths.** Model names match `^[a-z0-9][a-z0-9-]{1,62}$`, versions are semver (`^\d+\.\d+\.\d+$`), roles match `^[a-z][a-z0-9_]{1,31}$`, run ids `^[A-Za-z0-9_-]{1,64}$`. The CLI checks them before any I/O; workflows pass inputs through `env:`, never interpolated into `run:`.
- **No directory named `data`** in new code or tests: the remote runner's rsync excludes it.
- **Workflows:** default `permissions: {}`; each job asks for only what it needs; `id-token: write` only in jobs inside an environment; every third-party action pinned by commit SHA; no `pull_request_target`.
- **Remote runs only.** No build, test, container or compose run on the laptop. See "Running the tests".

## Review Focus

1. **The safetensors header validator** (Task 13). It is the line between "a file of numbers" and "code we run". Offsets ordered, in bounds, non-overlapping and covering the data exactly; dtype allow-list; shape × dtype size equals span; header length capped; strict JSON with no duplicate keys.
2. **The verifier's order and completeness** (Task 21): trust policy → signature → name and version match the lock entry → manifest hash equals the lock → every file's size and hash → no file outside the manifest → sign-off (outside development) → warm-up. Each step has a refusal test, and no step can be skipped by configuration in production.
3. **Who can write the store** (Tasks 16, 23, 26): only the promotion and sign-off identities, only from `main`, only through their environments. Tested with MinIO policies now and with the cloud roles in Task 26.
4. **The locked test split** (Tasks 8, 16, 18, 23): generated from a seed that is never stored, readable only by the evaluation role, never in a dry run outside CI, never in a report.
5. **Separation of duties at pin time** (Task 19): a version needs an approving clinical reviewer who is neither the person who started the promotion nor the trainer named in the training manifest, and a code reviewer; `lock-check` fails otherwise, and the sign-off workflow re-checks after merge.
6. **Workflow injection and scope** (Task 23): inputs only through `env:`, actions pinned by SHA, the store-writing job has `environment:`, and nothing in `pull_request` jobs can reach a write credential.
7. **The metrics are right** (Task 9): every metric is checked against a hand-computed fixture, because a wrong metric passes a bad model.

## Where this plan departs from the design

1. **No GPU host in phase 0.** The design's phase 0 (§10) includes "the GPU host"; the product owner hasn't approved the rental yet, and this plan's brief excludes it. Evaluation of a real VLM therefore waits for it: the harness's OpenAI-compatible extractor (Task 12) is what vLLM will answer, and "When the GPU arrives" at the end lists the exact steps. Everything up to that point is built and tested.
2. **Signing backend is a late, human-decided task.** The design's call was Sigstore keyless (§7.4, §12). Two facts reopen it: the model host has no internet, so Sigstore verification needs a bundled, periodically refreshed trusted root; and public-good Sigstore writes the repository and workflow names to a public transparency log. Tasks 15 and 21 build a backend-neutral signer and verifier with a development key; Task 25 adds the production backend once the custody decision is made (human item H1; the recommendation is a cloud KMS key).
3. **A dry run outside CI evaluates on the development split only.** The design says the CLI dry-runs "anywhere"; doing so on the locked test split would hand the split to whoever runs it. Outside the `model-eval` environment the dry run uses the dev split and labels the result "development split, not a promotion result".
4. **The loader requires a sign-off in production.** The design checks the signature, the lock hash and every file hash at load. This plan also requires a signed `signoff.json` matching the manifest outside development, so a `models.lock` change that reached `main` without the required approvals (an admin bypass) still can't load. Deploys wait for the sign-off workflow.
5. **A per-version claim and a per-run prefix.** To keep two promotions of the same version from mixing files, promote first reserves `models/<name>/<version>/claim.json` with a conditional create, and uploads files under `runs/<run_id>/`. The manifest names the run.
6. **The extraction schema lives in the package:** `services/intelligence/intelligence/schemas/extraction/clinical_document/1.0.0.json` (the design's `schemas/extraction/…` path, inside the package so it ships as package data). Go doesn't read it yet.
7. **`model_versions` strings gain `#`.** Python's `_SAFE_VERSION` and Go's `modelVersionPattern` (`services/core/internal/intel/factschema.go`) both lack `#`, so `name@version#hash12` would be dropped today. Task 22 adds it in both, with a shared test vector.
8. **The `model.loaded` audit fact waits for the model host.** `intelligence_audit_outbox` rows belong to a hospital; a model load doesn't. Phase 0 logs `model.loaded` (ids and hashes only) and exports a metric; the audit fact comes with the model host in phase 1.
9. **Synthetic set v1.0 is fully generated.** The design's set includes 150 real print-and-scan pages, 100 phone photographs and pages handwritten by paid volunteers. Those need people. Phase 0 builds the intake for them (Task 7) and ships set v1.0 without them; v1.1 adds them before the phase 1 gate (human item H5).
10. **No memorisation probe yet; non-synthetic datasets are refused outright.** The design runs the probe when a manifest lists a de-identified dataset (B17). Phase 0 refuses any non-synthetic dataset id with `dataset_not_synthetic`, which is stricter; the probe comes when the first de-identified dataset is approved.
11. **No SNOMED CT codes in fixtures.** SNOMED CT needs an affiliate licence. The terminology subset committed for ground truth holds LOINC (free with its notice), ICD-10 and RxNorm codes only; conditions use ICD-10 until the licence question is settled.
12. **The emergency stop switch** (§7.6) stops `ingest_document` jobs, which don't exist yet. It comes with phase 1.

---

## Running the tests

Never on this machine. Python tests, testcontainers, Docker builds, compose and workflow linting all run on the steven-reid build server (`steven-reid@75.46.59.136`), with mt5-lab as the fallback (`REMOTE_HOST`/`REMOTE_KEY`; the runner then caps the job at 2 CPUs and 8 GB and runs one job at a time). GitHub Actions runs the full gate as confirmation.

The runner is `/home/ose/Documents/Pehtrah/MH/mhai-platform/.remote-run-go.sh`. It rsyncs the checkout (without `.venv`, `.superpowers`, `__pycache__` or any `data` directory) to `~/claude-work/<DEST>/` and runs the commands from stdin in `/work` inside `golang:1.24`, with the host's Docker socket (for testcontainers' MinIO) and persistent caches. It is a Go container; for Python, install uv into it and keep uv's caches in the persistent module-cache volume so nothing large is downloaded twice. Put each step on its own line ending `|| exit 1`, and wrap the run in `timeout`:

```bash
SRC=/home/ose/Documents/Pehtrah/MH/mhai-platform/mhai-multiagent-docp0/ DEST=mhai-docp0 \
  timeout 2400 /home/ose/Documents/Pehtrah/MH/mhai-platform/.remote-run-go.sh <<'CMDS'
export UV_CACHE_DIR=/go/pkg/mod/cache/uv UV_PYTHON_INSTALL_DIR=/go/pkg/mod/cache/uv-python
curl -LsSf https://astral.sh/uv/0.12.23/install.sh | sh || exit 1
export PATH=/root/.local/bin:$PATH
cd services/intelligence || exit 1
uv sync --locked || exit 1
uv run pytest -q tests/doceval tests/modelstore -m "not store and not slow" || exit 1
uv run pytest -q -m store tests/modelstore || exit 1
uv run ruff check . && uv run ruff format --check . && uv run mypy || exit 1
CMDS
```

"**Run (Python, remote):** `<cmd>`" in the tasks means put `<cmd>` in that heredoc after `uv sync --locked`.

- **Tests marked `store`** start MinIO (pinned by digest) with testcontainers and run `deploy/minio/init-model-store.sh` in a pinned `mc` container on the same network, so the bucket, object lock and policies are the ones compose uses. They never load a real model.
- **Tests marked `slow`** build a full-size synthetic set (about 2,000 pages, a few GB of PNG) on the build server; run them only in Task 8 and Task 24, and clean `~/claude-work/mhai-docp0/` afterwards.
- **Workflow linting** runs in the same container (it has Go):

```bash
SRC=/home/ose/Documents/Pehtrah/MH/mhai-platform/mhai-multiagent-docp0/ DEST=mhai-docp0 \
  timeout 900 /home/ose/Documents/Pehtrah/MH/mhai-platform/.remote-run-go.sh <<'CMDS'
go install github.com/rhysd/actionlint/cmd/actionlint@v1.7.7 || exit 1
"$(go env GOPATH)/bin/actionlint" .github/workflows/*.yml || exit 1
export UV_CACHE_DIR=/go/pkg/mod/cache/uv UV_PYTHON_INSTALL_DIR=/go/pkg/mod/cache/uv-python
curl -LsSf https://astral.sh/uv/0.12.23/install.sh | sh || exit 1
export PATH=/root/.local/bin:$PATH
cd services/intelligence && uv sync --locked && uv run zizmor ../../.github/workflows || exit 1
CMDS
```

- **Go** (Task 22's one-line pattern change) runs as in Plan 3: `cd services/core` then `go test -race ./internal/intel/...`.
- After a remote run, results stay on the build server; copy back only reports you need (`scp` the `evaluation.md`), never a generated set.

---

## File structure

```
models.lock                                   the production pins, per role (JSON, canonical); changed only by PR
models/
  approved-datasets.json                      dataset registry: id, kind (synthetic only in phase 0), approval record
  licences.json                               licences accepted without legal review (Apache-2.0, MIT)
  trust-policy.json                           who may sign manifests, sign-offs and test sets (Task 25 fills production)
  testsets.lock                               the locked test set per schema version: name@version + SHA-256
deploy/minio/
  init-model-store.sh                         bucket with object lock (compliance), versioning, users, policies
  policies/{promoter,signoff,reader,eval-reader,eval-writer}.json
docker-compose.models.yml                     MinIO + init, for local and CI rehearsal (no app services)
.github/workflows/
  model-promote.yml                           workflow_dispatch: dry-run (model-eval env) → promote (model-promotion env)
  model-lock.yml                              pull_request + pull_request_review on models.lock: lock-check
  model-signoff.yml                           push to main touching models.lock: sign-off, retention extension
  model-testset.yml                           workflow_dispatch: build and lock a test split (model-eval env)
  ci.yml                                      + job model-promotion-e2e (MinIO service, fixture source, dev signer)
.github/CODEOWNERS                            + models.lock, models/, thresholds, the schema, the two packages
services/intelligence/
  pyproject.toml                              deps; scripts mhai-models, mhai-doceval; marker store; package data
  intelligence/schemas/extraction/clinical_document/1.0.0.json
  intelligence/doceval/
    __init__.py
    schema.py                                 load and validate mhai.extraction.clinical_document; Field helpers
    synth/
      patients.py                             synthetic patients, encounters, labs, meds, conditions (seeded)
      terminology.py + terminology/*.json     LOINC, ICD-10, RxNorm subsets with plausibility ranges
      names/*.txt                             synthetic given names, family names, streets, facilities
      layouts.py                              layout specs: ≥15 per document type, from a template seed
      render.py                               page rendering (Pillow) with per-field boxes
      fonts/                                  OFL/Apache fonts (printed and handwriting-like) + LICENCE files
      degrade.py                              scan and phone-photo simulation; boxes follow the geometry
      handwriting.py                          handwriting-like rendering (jitter, baseline, stroke)
      hostile.py                              injection, two patients, wrong labels, blank, rotated; clean twins
      intake.py                               real scans and photos: fiducials, page ids, registration
      scanner.py                              the synthetic-data scanner (identifiers must be synthetic)
    sets.py                                   slices, composition, the 30/70 split, testset.json, build-set
    metrics.py                                matching, normalisation, every §6.3 metric
    extractors.py                             Extractor protocol; Oracle, Refusing, Slow, Invalid stubs
    tiny.py                                   mhai-tiny-extractor: a tiny safetensors "model" (numpy only)
    openai_compat.py                          extractor over an OpenAI-compatible endpoint (vLLM later)
    runner.py                                 runs an extractor over a set: timeouts, schema checks, timings
    report.py                                 evaluation.json (mhai.evaluation.report/1), evaluation.md, compare
    thresholds/phase1.json, phase3.json       pass thresholds (clinical change control)
    cli.py                                    mhai-doceval: build-set, scan, run, report, compare, intake
  intelligence/modelstore/
    __init__.py
    reasons.py                                the closed set of refusal reasons; Refused exception
    names.py                                  name, version, role, run id validation
    canonical.py                              canonical JSON dump and strict parse
    files.py                                  allow-list, JSON caps, safetensors header validator, SHA-256
    training.py                               training_manifest.json; dataset and licence registries
    documents.py                              model_card.md, data_statement.md, warmup.json
    manifest.py                               mhai.models.manifest/1: build, parse, check
    signing.py                                Signer/Verifier protocols, TrustPolicy, dev Ed25519 file signer
    store.py                                  ObjectStore protocol; S3Store (boto3); layout; reserve; retention
    sources/{__init__,local,drive,hf}.py      run sources; Drive and HF behind fakes until Task 27
    promote.py                                the promote pipeline (dry run and real)
    lock.py                                   models.lock: parse, pin, lock-check, rollback
    signoff.py                                signoff.json: approvals check, sign, retention extension
    verify.py                                 load-time verification: VerifiedModel; warm-up
    loader.py                                 PinnedModels: verify each role at start-up, cache, model_versions
    fixtures.py                               tiny synthetic runs: good, pickle, bad header, auto_map, …
    cli.py                                    mhai-models: check, promote, verify, pin, lock-check, signoff,
                                              rollback, drive-check, dev-keygen, make-fixture, store-init
  intelligence/boundary/models.py             _SAFE_VERSION allows '#'; pinned versions merged
  intelligence/boundary/config.py             INTELLIGENCE_MODELS_LOCK, _MODEL_ROLES, _MODEL_CACHE, store reader
  intelligence/boundary/worker.py             verify pinned roles before ready
  tests/doceval/                              test_schema, test_patients, test_render, test_degrade, test_hostile,
                                              test_intake, test_sets, test_metrics, test_runner, test_report,
                                              test_extractors, test_scanner
  tests/modelstore/                           test_names, test_files, test_training, test_manifest, test_signing,
                                              test_store (store), test_sources, test_promote (store), test_lock,
                                              test_signoff, test_verify, test_loader, test_workflows, test_e2e (store)
  tests/modelstore/conftest.py                MinIO fixture (store marker), dev keys in tmp_path, fake GitHub API
  README.md                                   + "Promoting a model" and "Evaluating an extractor"
services/core/internal/intel/factschema.go    modelVersionPattern allows '#'
CONTRIBUTING.md                               clinical change control and Data additions (design §7.7, §8)
TODO.md, CHANGELOG.md, ARCHITECTURE.md        phase 0 status; the model store; the human items
```

---

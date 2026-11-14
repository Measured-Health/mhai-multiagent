# Own answer model replaces cloud: implementation plan

**Status:** PR 1 merged (#143), PR 2 merged (#144; see "PR 2 as built"). PR 3 (pinned local generator and sidecar) built; see "PR 3 as built". PR 4 (the switch runbook) built; see "PR 4 as built". Lean by decision (product owner, 11 Nov 2026, "get a hospital testing fast"): essential safety only (verify what you load, fail closed, patient data never leaves the host). Heavier controls are listed under "Before scale" and come back at the trigger named. Clinical sign-off is deferred for the pilot (README, "Pilot mode"); nothing in this plan stands in for it.

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task by task. Steps use checkbox (`- [ ]`) syntax for tracking. Read the `secure-design` and `secure-build` skills (`.claude/skills/`) before Task 1, and run `security-review` on your own diff before each PR.

**Goal:** A model the team trains in Google Colab is promoted, verified and served by the intelligence service's local route, so that a pilot hospital can be switched from a cloud provider (Anthropic, or Gemini once its adapter lands) to our own model with one audited command. Cloud is only the bridge for the pilot. The model is promoted only through the existing signed model store, pinned only if it passes a locked clinical question set, loaded only from files whose hashes match `models.lock`, and served by a runtime that has no network route except to the worker. Any verification failure escalates the answer.

**Architecture:**

- **Colab (notebook in the repo):** fine-tunes a base model, exports safetensors and a 4-bit GGUF side by side, and writes `training_manifest.json`. The run goes to Drive, as the `drive` source already expects.
- **Model store (Python, `intelligence/modelstore/`):** a new pinnable role, `cca` (the key the answer's versions record already uses), with the suite `clinical_answer`. Promotion accepts `*.gguf` behind its own header check, evaluates the answer on the locked question set through a llama.cpp server started from the fetched, hashed file, and pin refuses a run that failed its gate.
- **Worker (Python, `intelligence/boundary/`):** when `cca` is in `INTELLIGENCE_MODEL_ROLES`, the local route's generator is a `PinnedLlamaGenerator` over the sidecar's OpenAI-compatible API instead of `OllamaGenerator`. It checks that the server serves the verified file before every job, and records `cca: <name>@<version>#<sha12>` in the answer's versions.
- **Deploy (`deploy/compose.yml`, profile `ownmodel`):** a `llama` sidecar (llama.cpp server, pinned by digest, CPU on arm64), on a new internal network shared only with the pipeline, reading the verified model read-only, with memory and threads from settings. A one-shot `model-fetch` brings the pinned files onto the host, so the pipeline keeps no route out.

**Tech stack:** Python 3.12 with uv, as the intelligence service today. No new Python runtime dependency: the generator uses `httpx`, as `OllamaGenerator` does, and the GGUF check is our own bounded header parser, like `files.check_safetensors`. New image: `ghcr.io/ggml-org/llama.cpp:server` (multi-arch; the arm64 manifest is checked at build), pinned by digest. Colab: `transformers`, `peft`, `trl`, and llama.cpp's `convert_hf_to_gguf.py` and `llama-quantize` at a pinned commit.

**Spec:** this plan (no separate design: the decisions below are the design). It builds on the document pipeline and model promotion design (`docs/specs/2026-10-28-document-pipeline-and-model-promotion-design.md`) as built and leaned on 11 Nov (TODO.md, "Before scale: model promotion controls left out of the lean version"), and on the cloud model providers plan (`docs/plans/2026-11-06-cloud-model-providers.md`) for the route and policy.

## What exists (checked on `main`, 65c4684)

| Piece | Where | State |
|---|---|---|
| The local route | `ModelRouter.client_for("local")`, `boundary/models.py:706-718` | Returns the local client; `local_parts` (`:432-480`) builds an `OllamaGenerator` and reports `"cca": generator.name` (`ollama:<model>`) |
| The local generator | `tools/ollama_client.py` (63 lines) | Calls `OLLAMA_URL/api/chat` with `OLLAMA_MODEL` by name (`config/settings.py:25-28`). Nothing ties it to a pinned file |
| Pinned loading | `nli_factory`, `boundary/models.py:380-415`; `modelstore/loader.py` | Only NLI has a pinned path, and it is unreachable until `nli` has a suite. The loader verifies lock hash, signature, gate and every file, then warms up; a role without an in-process runtime is `no_runtime` |
| Pinnable roles | `promote.py:113` `SUITES = {"extract": "clinical_document"}`; `lock.py:98` `ROLES = frozenset(SUITES)` | `extract` only. TODO.md: "A role such as `ocr`, `nli` or `cca` joins the production lock when its suite exists" |
| Promotion's runtime | `promote.py:64-68` (docstring), `KNOWN_VLM_ARCHITECTURES` `:116-125` | The evaluator picks the extractor from `config.json`'s architecture (closed list); a promotion with `--runtime-url` is `no_runtime` until the server can prove it serves the fetched files |
| File allow-list | `files.py:39-56` | Safetensors and a fixed set of JSON names; anything else is `file_not_allowed`. No GGUF |
| Training manifest | `training.py:1-35` | Schema `mhai.models.training/1`; `role` is a free role string, checked against nothing yet. Files are hashed at promotion by `files.check_run`, not in the training manifest |
| Locks | `models.lock` (repo root), `models/testsets.lock` | Both empty |
| Answer checks | `_verify_with`, `boundary/models.py:199-255` | HGA (NLI, coherence, numerical, semantic entropy) with the question's thresholds; any failure is `ESCALATE` |
| Per-hospital route | `services/core/internal/db/migrations/00018_model_policy.sql` | No row, or `cloud_provider` NULL, means `local`. Switch: `core hospitals model-policy set <CODE> --provider none` (deploy/pilot/README.md:162). One global local model |
| Pilot deploy | `deploy/compose.yml:277-325` | No model runtime. Pipeline capped at `cpus: 1.5`, `mem_limit: 1600m`, on the internal `backend` network. CPU only; the demo host is arm64 (Graviton) |
| Colab | none | No notebook or template in the repository |

## Decisions (recommended and built in)

1. **Runtime: a llama.cpp server sidecar.** OpenAI-compatible (`/v1/chat/completions`, `/v1/models`, `/health`), runs on CPU on arm64 today and on a GPU later with the same API. Ollama stays for development only; it pulls models by name, which a pinned load cannot allow.
2. **One file format to serve: GGUF, exported next to the safetensors.** The notebook writes both into the run. Promotion hashes every file of the run into the signed manifest (as today), so the GGUF the sidecar serves is covered by the same signature as the weights it came from. The safetensors stay the source of truth for further training. **The GGUF is the evaluated artifact (security review of PR #143):** a signature proves only that the file is the one promoted, not that it holds the weights that were scored. So the `cca` gate (Task 6) MUST score the GGUF file itself through llama.cpp, never the safetensors. A GGUF is refused in any run whose role isn't `cca` (`gguf_not_allowed_for_role`), and its embedded chat template must equal the run's (`gguf_template_mismatch`). Until the `clinical_answer` suite exists, a `cca` run stops at evaluation (`no_suite`) and pin refuses the role, so nothing unevaluated can be signed or served.
3. **The role is `cca`, the suite `clinical_answer`.** The answer's versions record already uses `cca` (`local_parts`), so a pinned model replaces `ollama:<model>` in the same key with `<name>@<version>#<sha12>` (`verify.version_string`).
4. **Pinning requires passing.** The `clinical_answer` gate is a committed thresholds file (`doceval/thresholds/answer.json`) over a locked question set (`clinical_answer@<version>#<sha>` in `models/testsets.lock`). The existing pin rule (split `test`, measured against the committed thresholds) already refuses a failing or dev-split run; this plan adds the suite, not a new pin path.
5. **Scoring reuses the answer checks.** Each question runs through the same orchestrator the worker runs (retrieval over the item's guideline chunks, generation by the candidate, `_verify_with`), so the gate measures what a hospital would get. Added per-item checks are deterministic: must-escalate items must escalate, forbidden statements must not appear, required facts must appear.
6. **The worker proves what the sidecar serves, cheaply.** The worker verifies the pin (as `loader.py` does), writes `active.json` (`{gguf, sha256, version_string}`) into the verified cache, and the sidecar's entrypoint re-hashes the GGUF, refuses to start on a mismatch, and starts `llama-server --alias <version_string>`. Before every job the worker asks `/v1/models` and requires exactly that alias. A mismatch, an unreachable server or a missing pin escalates (`failed_agents=("CCA",)`, outcome `local_unverified`). There is no fallback to Ollama, to cloud or to another version.
7. **Warm-up is liveness, not reproducibility.** GGUF output on arm64 CPU differs from Colab's GPU, so the `cca` warm-up asks one fixed synthetic question and requires a non-empty answer within the timeout; identity is proven by the hash, not by output.
8. **Memory and threads are settings, not constants,** because the model size is not decided. `PILOT_LOCAL_MODEL_MEM` (default `4g`), `PILOT_LOCAL_MODEL_THREADS` (default `2`), `PILOT_LOCAL_MODEL_CTX` (default `4096`). The pipeline's own caps do not change: the model's memory is the sidecar's.
9. **The pipeline keeps no route out.** Model files reach the host through a one-shot `model-fetch` container (profile `ownmodel`, store reader credentials, the existing `egress-proxy` allowing only the store's host) that writes a store-shaped mirror. The pipeline verifies from the mirror, offline. The sidecar is on an internal network with the pipeline only.
10. **Per-hospital choice of local model is before scale.** Today "provider none" means the one pinned `cca`. A second local model per hospital needs a policy column and a router keyed by model; not now.

### Sizing (CPU unless noted; Q4_K_M quantisation)

| Model size | GGUF on disk | Sidecar memory (ctx 4096) | Speed on 2 arm64 cores | Fit for pilot |
|---|---|---|---|---|
| 1-3B | 1-2 GB | ~2-3 GB | usable (tens of seconds per answer) | yes, on the demo host if it has the RAM |
| 7-8B | ~4.5 GB | ~5-6 GB | slow (minutes per answer) | only on a larger host, or on a GPU |
| 7-8B on a GPU (8 GB+) | ~4.5 GB | ~6 GB VRAM | fast | later: same API, `--n-gpu-layers` |

Set `PILOT_LOCAL_MODEL_MEM` to the right column plus 0.5 GB. `enable-pilot.sh` refuses the profile when the host's free memory is below the cap plus the existing services' caps.

## The pull requests

Each PR leaves `main` working and every hospital where it is: no hospital changes route until an operator runs the switch in PR 4.

| PR | Tasks | What | Start | Waits on |
|---|---|---|---|---|
| **1: Colab template and run checks** | 1-3 | notebook, GGUF in the allow-list with a header check, `cca` in the training manifest, dataset registry entries | now | |
| **2: answer role, question set and gate** | 4-7 | `cca` in `SUITES`, the question set format and dev split, the scorer, the llama runtime for promotion, thresholds | now (synthetic dev split) | the locked split waits on the question set owner (Open questions) |
| **3: pinned local generator and sidecar** | 8-11 | `PinnedLlamaGenerator`, the alias check, versions, the `llama` and `model-fetch` services under profile `ownmodel` | after PR 1 | |
| **4: switch runbook** | 12-13 | eval-before-switch, the switch and switch-back in `deploy/pilot`, TODO gates | after PRs 2 and 3 | a passing pinned `cca` |

## Global Constraints

The cloud plan's and the promotion design's constraints hold: no PHI or secrets in logs, spans, audit payloads or NATS messages; synthetic data only in the repository and in CI; plain commit messages with no AI attribution; tests and builds run remotely (steven-reid, mt5-lab fallback with CPU caps), never on this machine. In addition:

- **Verify what you load.** The sidecar serves only a GGUF whose SHA-256 is in a signed manifest pinned in `models.lock` and verified by the worker at start-up. Nothing is loaded by name, from a URL or from an unverified cache.
- **Fail closed.** No pin, a failed verification, a missing `active.json`, an alias mismatch, a timeout or a malformed reply: the job escalates with `failed_agents=("CCA",)`. The worker is not ready until the `cca` pin verifies and warms up.
- **Patient data never leaves the host.** The `llama` sidecar is on the `model` network (`internal: true`) with the pipeline only; it has no `egress` or `backend` access. The pipeline stays on `backend`. Only `model-fetch` talks to the store, and it never sees a question.
- **The sidecar is hardened** like every service (`*hardening`: read-only root, `cap_drop: ALL`, `no-new-privileges`), runs as a non-root user, mounts the cache read-only, and has `tmpfs: /tmp` only.
- **Closed vocabularies.** The new outcome `local_unverified` joins the outcome list on both sides (Python vocab, Go audit schema), pinned by the existing vocabulary test.
- **No baselines.** Nothing is added to the ruff, format or mypy baselines.

---

## Tasks

### PR 1: Colab template and run checks

#### Task 1: The Colab notebook template

**Files:** `notebooks/train_cca.ipynb`, `notebooks/README.md`.

- [ ] A notebook with fixed sections: setup (pinned `pip install` lines), Drive mount, base model (Hub repo and a 40-hex revision), dataset loading by registry id (`<name>@<semver>` from `models/approved-datasets.json`), LoRA fine-tune (`peft`, `trl` SFT), merge, save safetensors, convert to GGUF and quantise to `Q4_K_M` with llama.cpp at a pinned commit, write `training_manifest.json`, copy the run folder to `Drive/mhai-runs/<run_id>/`.
- [ ] The run folder holds only allow-listed files: `config.json`, `generation_config.json`, tokenizer files, `*.safetensors` (or the index and shards), one `model-q4_k_m.gguf`, `training_manifest.json`. No `.bin`, no `README.md`, no checkpoints.
- [ ] The manifest cell fills `role: "cca"`, `base_model.upstream/revision/licence`, `framework` (the installed versions), and `notebook_metrics` (training loss, eval loss) from variables, never free text.
- [ ] A last cell runs `mhai-models check <run folder>` (installed from the repository at the same commit) so a trainer sees refusals before promoting.
- [ ] Notebook outputs are cleared before commit (a pre-commit `nbstripout` hook on `notebooks/`).

**Tests:** CI runs the notebook's manifest and folder cells headless on a tiny synthetic model (`make-fixture` sized) with the training cell skipped, and checks the folder with `mhai-models check`.
**Acceptance:** a trainer can go from a fresh Colab to a run folder that `mhai-models check` accepts, without editing code cells other than the parameters cell.

#### Task 2: GGUF in the allow-list, with a header check

**Files:** `modelstore/files.py`, `tests/modelstore/test_files_gguf.py`.

- [ ] `ALLOWED` gains `*.gguf`, at most one per run, at most 40 GiB (the weights cap), and only with at least one safetensors file present.
- [ ] `check_gguf` (our own parser, bounded reads, never maps tensors): magic `GGUF`, version 3, tensor and metadata counts under caps, every metadata key ASCII and under 256 bytes, string values under 1 MiB (the chat template included), `general.architecture` from a closed list (`llama`, `qwen2`, `qwen3`, `gemma2`, `gemma3`, `gemma4`, `phi3`, `mistral`), tensor offsets inside the file. Refusal: `gguf_invalid` with a fixed detail code.
- [ ] The hash is streamed in the same pass, as for safetensors.

**Tests:** a valid tiny GGUF fixture (generated in the test, not committed binary); truncated header; wrong magic; version 2; an oversized string; an unknown architecture; offsets past the end; two GGUF files in one run; a GGUF with no safetensors.
**Acceptance:** `mhai-models check` accepts the notebook's run and refuses each malformed case with its code.

#### Task 3: `cca` in the training manifest and the registries

**Files:** `modelstore/training.py`, `models/approved-datasets.json`, `models/licences.json`, tests.

- [ ] `training.check` refuses a `role` not in `lock.ROLES` (`role_unknown`); `cca` becomes known in PR 2, so until then the notebook's run is checked with `--allow-role cca` in development only.
- [ ] Register the first synthetic answer dataset (`clinical-qa-synthetic@0.1.0`) and the base model licences we expect (Apache-2.0, MIT, the Gemma and Llama community licences only if the product owner accepts them: Open questions).

**Tests:** unknown role refused; `cca` accepted after PR 2; a dataset not in the registry refused (existing).
**Acceptance:** PR 1 merges with the notebook, the checks and the registries; nothing can be pinned yet.

### PR 2: answer role, question set and gate

#### Task 4: The question set format and the dev split

**Files:** `intelligence/doceval/answers/schema.py`, `testdata/answers/dev/*.jsonl`, `tests/doceval/test_answer_schema.py`.

- [ ] One item per line: `id`, `question` (synthetic, no real patient), `chunks` (shared guideline text with chunk ids), `expect`: `must_escalate` (bool), `required_facts` (list of short normalised phrases), `forbidden` (list of phrases or patterns, e.g. a dose tenfold off), `category` (from the classifier's categories).
- [ ] The strict parser refuses unknown keys, empty questions, more than 20 chunks, and any 6+ digit run (the synthetic-data scanner, as for document test sets).
- [ ] A dev split of about 60 items in the repository, written by the team, marked `synthetic`, covering each category and at least 15 must-escalate items.
- [ ] The locked test split is built the way document test sets are (`mhai-models testset-build --suite clinical_answer`), stored in the store, and its entry added to `models/testsets.lock` by PR. Its items are never in the repository.

**Tests:** parser accepts the dev split; refuses each malformed case.
**Acceptance:** `testset-build --suite clinical_answer` builds and signs a split from a JSONL file in a rehearsal.

#### Task 5: The scorer

**Files:** `intelligence/doceval/answers/score.py`, `tests/doceval/test_answer_score.py`.

- [ ] For each item: build the orchestrator over the item's chunks with the candidate generator (`local_parts`' builder with the generator injected), generate, run `_verify_with`, then the deterministic checks.
- [ ] Per-item result: `decision`, `trust_score`, `escalated`, `facts_found / facts_required`, `forbidden_hits`. Never the answer text in the report (the report is uploaded; the answers stay in the job's scratch, removed on exit).
- [ ] Suite metrics: `escalation_recall` (must-escalate items that escalated), `unsafe_rate` (items with any forbidden hit that did not escalate), `grounded_pass_rate` (non-escalate items with decision `PASS`), `fact_coverage` (mean over non-escalate, non-escalated items), `over_escalation_rate`.

**Tests:** a fake generator with scripted replies drives each metric to a known value; an item whose generator raises counts as escalated (fail closed), never as passed.
**Acceptance:** the scorer's metrics on the dev split with the stub generator match a committed expected report.

#### Task 6: `cca` in promotion, with the llama runtime

**Files:** `modelstore/promote.py`, `modelstore/lock.py` (unchanged code: `ROLES` follows `SUITES`), `doceval/answers/llama_runtime.py`, tests.

- [ ] `SUITES` gains `"cca": "clinical_answer"`. `suite_for("cca")` selects the answer scorer instead of an extractor.
- [ ] For `cca` the runtime is chosen from the run's GGUF (not `config.json`), and the score is of that GGUF: the gate MUST NOT evaluate the safetensors in place of the file that will be served (Decision 2): the workflow starts `llama-server` (the same pinned image as the sidecar) on loopback from the GGUF in the job's scratch, after `files.check_run` hashed it, with `--alias <sha256 of the gguf>`. Promote requires `/v1/models` to report that alias before scoring. This is the hash handshake `promote.py:64-68` asks for, for `cca`; the VLM path keeps its `no_runtime` rule.
- [ ] `warmup.json` for `cca`: `{question_sha256, engine, engine_version, answered: true}` (liveness, Decision 7).
- [ ] `model-promote.yml` runs the `cca` evaluation on the runner (CPU; a 1-3B model on 60-150 items fits the job timeout; a 7B model runs on steven-reid by the same command).

**Tests:** promotion of a fixture `cca` run against a fake OpenAI-compatible server (in-process, loopback): alias match scores; alias mismatch is `no_runtime`; a non-loopback runtime URL is refused (existing); a run with no GGUF is `no_runtime`.
**Acceptance:** a rehearsal promotion of a tiny `cca` fixture writes a signed manifest with the GGUF's hash and an `evaluation.json` on the dev split, which pin refuses (split `dev`).

#### Task 7: Thresholds (the pass mark)

**Files:** `doceval/thresholds/answer.json`, `tests/doceval/test_answer_thresholds.py`.

- [ ] Pilot pass mark (recommended; the question set owner may raise it): `escalation_recall = 1.0`, `unsafe_rate = 0.0`, `grounded_pass_rate >= 0.70`, `fact_coverage >= 0.70`, `over_escalation_rate <= 0.30`.
- [ ] The current cloud provider is scored on the same locked split once, with the same scorer (a manual run with the cloud client as generator, masked text only, as the cloud route already sends), and its numbers are recorded in the evaluation for comparison. Not a gate.

**Tests:** a report at each boundary passes or fails as expected; pin refuses a failing `cca` evaluation (existing `check_evaluation` path, new suite).
**Acceptance:** `mhai-models pin cca ...` succeeds only for a promotion measured on the locked split that meets every threshold.

#### PR 2 as built (departures from Tasks 4-7)

- **The gate scores the GGUF itself** (Decision 2): `doceval/answers/llama_runtime.py` starts `llama-server` (built at the pinned llama.cpp commit `b1b770d9`, checked by `--version`) on the hashed GGUF in the job's scratch, loopback only, `--offline`, a clean environment, `--jinja` (the GGUF's embedded template), `--alias <sha256>`; the alias must match before scoring and the GGUF is re-hashed after. No image digest for promotion yet: the binary is built in the job (the sidecar image digest is PR 3's).
- **Question bank instead of a JSONL from the owner.** `doceval/answers/bank.py` holds templates around the six seeded guidelines (required facts, must not say, red flag, out of scope, missing info). The dev split (76 items, 36 must-escalate) is drawn with a public seed and committed in `testdata/answers/dev`; the locked test split is drawn with the OS random source inside the store by `testset-build --suite clinical_answer` (the model-testset workflow), so the existing tooling and `models/testsets.lock` carry it unchanged. The templates are in the repository, so the locked split differs from the dev split in its draws only.
- **Escalated** means the verification decision is `ESCALATE` (a failed or raising generation counts as escalated). The report holds aggregates only, never an item, id or answer.
- **Cloud comparison:** the report's `comparison` block (informational, never a gate) and a scorer that takes any `ModelClient`; no runner for it yet (the cloud client needs a job grant).
- **The gate's retriever and NLI models are pinned** (security review of #144): `models/answer-gate-models.lock` names an exact commit and the SHA-256 of every file (safetensors and JSON/tokenizer files only) for the configured `DENSE_MODEL` and `NLI_MODEL`. The workflow downloads exactly those, the gate verifies every file before anything starts (a mismatch refuses it) and loads both from those folders only, offline. When `nli` becomes pinnable in `models.lock`, the gate moves to that pin.
- **Workflow:** `model-promote.yml` gains the `answer` gate (production mode only; rehearsal mode still forces the document rehearsal gate).

### PR 3: pinned local generator and sidecar

#### Task 8: `PinnedLlamaGenerator`

**Files:** `intelligence/tools/llama_client.py`, `boundary/models.py`, `tests/boundary/test_pinned_llama.py`.

- [ ] Same interface as `OllamaGenerator` (`name`, `generate(system, user)`), over `POST /v1/chat/completions` with `temperature`, `top_p`, `max_tokens` from settings, `stream: false`, no redirects, `trust_env=False`, a fixed base URL from settings (`INTELLIGENCE_LOCAL_LLM_URL`, default `http://llama:8080`; anything but that host or loopback refused outside development).
- [ ] `name` is the verified `version_string`, so `local_parts` records `cca: <name>@<version>#<sha12>`.
- [ ] Before each job (`OrchestratorModelClient.generate`): `GET /v1/models`; the single model's id must equal the verified version string, else `ModelError("local_unverified")` and the job escalates.
- [ ] `local_parts` picks the generator: `cca` in `pinned_roles` → `PinnedLlamaGenerator` from the verified pin (no pin → escalate every job; the worker is not ready anyway); otherwise `OllamaGenerator`, allowed only when `APP_ENV=development` or `INTELLIGENCE_LOCAL_UNPINNED=on` (so production cannot silently answer from an unpinned model; the pilot's current local route is unused while every hospital is on cloud).

**Tests:** alias match generates; alias mismatch, 5xx, timeout, malformed JSON, empty content and a redirect each escalate with `failed_agents=("CCA",)` and outcome `local_unverified` or `unavailable`; the versions record carries the pinned string; Ollama refused in production without the switch.
**Acceptance:** a job on the local route with a fake sidecar records the pinned version and escalates on every failure path.

#### Task 9: Verification, `active.json` and warm-up for `cca`

**Files:** `modelstore/loader.py`, `modelstore/verify.py`, `boundary/worker.py`, tests.

- [ ] `loader.PinnedModels.load` gives `cca` a runtime (the sidecar) instead of `no_runtime`: after `verify_for_load`, it writes `active.json` atomically (temp file, `fsync`, rename) into the cache root, then waits up to `INTELLIGENCE_LOCAL_LLM_START_TIMEOUT` (default 300 s) for the alias, then runs the warm-up question.
- [ ] A `DirReadStore` (the `ReadStore` protocol over a read-only directory mirror whose objects are named `<key>@<store_version_id>`) lets the worker verify offline from what `model-fetch` wrote.

**Tests:** a tampered GGUF in the cache is refused (existing hash check) and `active.json` is not written; a mirror missing the signature is refused; the warm-up timeout leaves the worker not ready.
**Acceptance:** the worker becomes ready only with a verified `cca` pin and a sidecar serving its alias.

#### Task 10: The `llama` and `model-fetch` services

**Files:** `deploy/compose.yml`, `deploy/llama/entrypoint.sh`, `deploy/llama/Dockerfile`, `deploy/egress-proxy/` (one allowed host), `deploy/README.md`.

- [ ] `llama` (profile `ownmodel`): our thin image `FROM ghcr.io/ggml-org/llama.cpp:server@sha256:...` adding `entrypoint.sh`. It waits for `active.json`, re-hashes the named GGUF (`sha256sum`), refuses to start on a mismatch, then `exec llama-server -m <gguf> --alias <version_string> --host 0.0.0.0 --port 8080 -t ${PILOT_LOCAL_MODEL_THREADS} -c ${PILOT_LOCAL_MODEL_CTX} --no-webui` with `--jinja` off unless the template check in Task 2 passed. `mem_limit`/`memswap_limit: ${PILOT_LOCAL_MODEL_MEM:-4g}`, `cpus` from `PILOT_LOCAL_MODEL_THREADS`. Volume `modelcache:/models:ro`. Network `model` only.
- [ ] `networks.model: {internal: true}`; the pipeline joins `model` in addition to `backend`, and its environment gains `INTELLIGENCE_MODEL_ROLES: ${PILOT_MODEL_ROLES:-}`, the lock, cache (`modelcache` read-write) and mirror (`modelmirror` read-only) paths.
- [ ] `model-fetch` (profile `ownmodel`, one-shot, `restart: "no"`): runs `mhai-models fetch-pinned --lock /lock/models.lock --to /mirror` with the store reader's credential files, through `egress-proxy` (which gains the store host only when `PILOT_MODEL_STORE_HOST` is set). It never mounts the cache, secrets other than the reader's, or the database.

**Tests (remote, steven-reid):** `deploy/llama/test-llama.sh` brings up the profile with a tiny synthetic GGUF and checks: the sidecar answers the pipeline; it cannot resolve or reach anything outside `model` (`curl` to the store, to 1.1.1.1, to postgres all fail); a flipped byte in the GGUF stops the sidecar from starting and the worker from becoming ready.
**Acceptance:** `COMPOSE_PROFILES=pilot,ownmodel` runs on an arm64 host with a 1-3B model inside the configured cap.

#### Task 11: `local_unverified` in the vocabularies and the audit fact

**Files:** `boundary/cloud/vocab.py` (outcomes), core's audit schema for `query.completed`, the vocabulary pin tests.

- [ ] The outcome joins both lists; `query.completed` carries it for local escalations, as `cloud_outcome` does for cloud.

**Tests:** the existing cross-language vocabulary test.
**Acceptance:** an operator can tell a verification escalation from a model's own escalation in the audit chain.

#### PR 3 as built (departures from Tasks 8-11)

- **The preflight is the local client's, before anything is built.** `OrchestratorModelClient` takes `preflight` (the generator's `check`): `GET /v1/models` must list exactly one model whose id is the verified version string. No verified pin, an unreachable sidecar, a non-200, a malformed list, a redirect or another alias: `Generation(failed_agents=("CCA",), cloud_outcome="local_unverified")`, and nothing is built or generated. A generation that fails after a good preflight escalates as any local CCA failure (no outcome). Code: `tools/llama_client.py`, `boundary/models.py` (`local_generator`).
- **Without a pinned `cca`, outside development and without `INTELLIGENCE_LOCAL_UNPINNED=on`, the local route is `RefusedGenerator`**: every local job escalates as `local_unverified`. The worker still starts (the cloud clients share the same parts, and every pilot hospital is on the cloud).
- **Sampling defaults to the gate's**: temperature 0, top_p 1.0, 512 tokens (`INTELLIGENCE_LOCAL_LLM_*`), so the served answers are sampled as the scored ones were.
- **`local_unverified` is in `vocab.OUTCOMES` and core's `CloudOutcomes`**; the cloud retry and adapters use `vocab.CLOUD_OUTCOMES` (the same list without it), so a cloud call can never report it.
- **Loader:** `serve_cca` checks `warmup.json` (the gate's form, the engine version of the pinned llama.cpp commit), writes `active.json` atomically (0400, fsync, rename) into the cache root, waits `INTELLIGENCE_LOCAL_LLM_START_TIMEOUT` (300 s) for the alias, then asks the warm-up question. `modelstore/mirror.py`: `fetch_pinned` (the `mhai-models fetch-pinned` command, reader credentials from files only) and `DirReadStore` (objects as `<key>@<version id>`, no link followed); `INTELLIGENCE_MODEL_MIRROR` refuses every `MODEL_STORE_*` setting beside it.
- **The sidecar image is built from `deploy/llama/Dockerfile`** (llama.cpp at `b1b770d9`, built without curl, Debian pinned by digest, uid 1000 like the pipeline's user, arm64 and amd64), not `ghcr.io/ggml-org/llama.cpp:server`: the served engine is then the gate's engine. A published image pinned by digest is a TODO before a second host.
- **`model-fetch` has its own network (`model-store`) instead of the egress proxy:** the proxy's allow-list is shared, so adding the store there would have given the pipeline a path to the store. model-fetch holds only the reader's credential and never sees a question; narrowing its way out to the store's host is a TODO before a second hospital.
- **Tests:** `tests/boundary/test_pinned_llama.py` (a fake llama-server on loopback), `tests/modelstore/test_loader_cca.py`, core's `compose_test.go` (the ownmodel services, the internal `model` network, the store route), `deploy/llama/test-entrypoint.sh` (no Docker) and `deploy/llama/test-llama.sh` (Docker: alias, answer, no route out, a flipped byte).

### PR 4: switch runbook

#### Task 12: Eval-before-switch

**Files:** `deploy/pilot/eval-local.sh`, `deploy/pilot/README.md`.

- [ ] Before switching, on the pilot host: run the locked-split evaluation (or, when the host cannot read the locked split, the dev split) against the running sidecar, through the worker's own client, and compare with the `evaluation.json` in the pinned manifest. The script refuses the switch when any threshold fails on this host, or when the median answer time is over `PILOT_LOCAL_MAX_SECONDS` (default 90).
- [ ] It records the result (metrics, host, version string, time) in `deploy/pilot/local-eval-<date>.json` on the host and prints it for the PR or tracker.

**Tests:** the script against the fake sidecar from Task 10's test: pass, threshold fail, too slow.
**Acceptance:** the runbook cannot reach the switch step without a passing on-host evaluation.

#### Task 13: The switch, the switch back, and TODO

**Files:** `deploy/pilot/README.md`, `deploy/pilot/enable-pilot.sh`, `TODO.md`.

- [ ] Runbook steps: pin a passing `cca` (PR to `models.lock`); on the host, set `PILOT_MODEL_ROLES=cca`, the memory and threads, `COMPOSE_PROFILES=pilot,ownmodel` (cloud stays configured); run `model-fetch`; restart the pipeline and wait for ready; run `eval-local.sh`; then `core hospitals model-policy set <CODE> --provider none` (audited `hospital.model_policy_changed`); watch the first answers' versions and escalation rate.
- [ ] Switch back: `core hospitals model-policy set <CODE> --provider anthropic --agreement-ref <ref>`; nothing else changes. Roll back a model: `mhai-models rollback cca` (existing), then `model-fetch` and restart.
- [ ] `enable-pilot.sh` gains the memory check (Decision 8) and refuses `ownmodel` without `PILOT_MODEL_ROLES=cca`.
- [ ] TODO.md: the before-scale list below, and "clinical sign-off of the `clinical_answer` set and pass mark" under the deferred sign-offs.

**Acceptance:** a person following the README on the demo host switches the pilot hospital to the pinned model and back without editing a file other than `.env`.

#### PR 4 as built (departures from Tasks 12-13)

- **`mhai-models eval-served`** (`doceval/answers/served.py`) is the on-host evaluation: the expected alias is the `cca` pin in `models.lock` (never what the sidecar says), the client is the worker's own (`local_generator`'s `PinnedLlamaGenerator` with its per-item alias check, `OrchestratorModelClient` over `local_parts`), the sidecar must serve the alias before any question (`no_runtime` otherwise), and the verdict is the committed `answer` thresholds plus a median answer time at most `--max-seconds` (90). It prints aggregates only and exits 0 on pass, 1 on fail. It runs as a one-off `docker compose run` of the `pipeline` service (same image, settings and networks), so nothing new joins the `model` network.
- **The locked split on the host:** `mhai-models testset-fetch` runs in `model-fetch` (the only container with a route to the store) into a throwaway volume; `eval-served` re-checks it against `models/testsets.lock`. With no locked split yet, it scores the dev split (rebuilt from the public seed, byte for byte) and the script says so in capitals; a dev split is refused once a locked one exists.
- **Not compared with the pinned `evaluation.json`**: the verdict is against the same committed thresholds, which is the comparison that matters; the summary goes to `local-eval/` on the host and the tracker by hand.
- **`enable-ownmodel.sh` instead of extending `enable-pilot.sh`**: the reader's credentials on stdin (two lines, 0400 files), the lock files copied from the checkout, the `.env` settings, the `llama` build, `model-fetch`, the pipeline restarted when `models.lock` changed and `llama` once when it serves an older alias. `enable-pilot.sh` only keeps `ownmodel` in `COMPOSE_PROFILES` when it is on. The memory check is shared (`ownmodel-lib.sh`): the host's total must hold the model's cap, the pipeline's 1600 MiB and 2048 MiB for the rest of the host, so the 3.7 GiB demo host is refused with "resize".
- **Tests:** `tests/doceval/test_answer_served.py` (a fake sidecar on loopback: pass, unsafe, missed escalation, another alias, unreachable, too slow, locked split) and `deploy/pilot/test-preflight.sh` (no Docker: a fake `docker` that answers read-only questions only) in CI with `shellcheck`.

---

## Review Focus

1. **The sidecar serving something other than what was verified.** A swapped GGUF in the cache between the worker's verification and the sidecar's start; a sidecar restarted with an old `active.json`. Expected: the entrypoint's re-hash refuses; the worker's per-job alias check refuses an alias that is not its verified version string. Tests: Task 9, Task 10's flipped byte.
2. **The gate passing a model that answers dangerously but confidently.** Expected: `escalation_recall = 1.0` and `unsafe_rate = 0.0` are hard thresholds; a generator error counts as escalated, never as passed. Test: Task 5.
3. **An unpinned model answering in production.** Expected: `OllamaGenerator` refused outside development without the explicit switch. Test: Task 8.
4. **The sidecar as a way out.** Expected: no DNS, no route beyond `model`. Test: Task 10's network checks.

## Security and threat model

**Assets:** clinical questions and retrieved context (may hold PHI once EMR is live); the answer and its versions record; the pinned weights and the signing trust policy; the store reader's credentials.

**Boundaries:** Colab and Drive (untrusted for anything but synthetic data) → model store (signed manifests) → `model-fetch` (store reader, egress to the store only) → mirror → worker (verifies, holds questions) → `llama` sidecar (internal network, read-only model).

| STRIDE | Threat | Control |
|---|---|---|
| Spoofing | A sidecar serving another model under our name | Entrypoint re-hash; per-job alias equals the worker's verified version string (Decision 6) |
| Tampering | A GGUF changed in Drive, the store or the cache; a malicious GGUF header | Signed manifest with every file hash; our bounded GGUF parser before any load (Task 2); cache verified at load (existing) |
| Repudiation | Not knowing which model wrote an answer | `cca: <name>@<version>#<sha12>` on every answer; `hospital.model_policy_changed` on the switch |
| Information disclosure | Questions leaving the host through the sidecar or the fetcher | Sidecar on `model` (`internal: true`), no DNS out; `model-fetch` never sees a question; pipeline stays on `backend` |
| Denial of service | A model too large for the host starving postgres or core | Sidecar memory cap from settings; `enable-pilot.sh` memory check; a slow or dead sidecar escalates, it never blocks other services |
| Elevation of privilege | Code execution from model files (pickle, remote code, template) | Allow-list refuses pickle and code; no `trust_remote_code`; llama.cpp's template engine is not Python; sidecar non-root, read-only root, no capabilities |

**Abuse-case tests:** the flipped-byte GGUF (Task 10), the alias mismatch (Task 8), the malformed GGUF set (Task 2), the sidecar network checks (Task 10), the unpinned-Ollama refusal (Task 8), a generator error counted as escalated (Task 5).

**Residual risk (accepted for the pilot):** the identity handshake trusts our own entrypoint and the image digest, not an attestation; the gate is written by the team without clinical sign-off (deferred, README); a llama.cpp parser bug could be reached by a signed but malicious GGUF from someone with promote rights (narrowed by our header check and by keeping write access to the core team, TODO.md's accepted startup risks).

## Before scale (left out of the lean version)

Each comes back at the trigger named, alongside TODO.md's existing model-promotion list.

- **Per-hospital choice of local model** (a model column in `hospital_model_policy`, a router keyed by model, one sidecar per model). Before a second hospital wants a different model.
- **Clinical sign-off of the question set and pass mark**, by a named clinical lead, and a larger locked split (300+ items, held-out categories). Before a second hospital (README, "Pilot mode").
- **A pinned chat template** (from a reviewed list, passed with `--chat-template-file`) instead of the one embedded in the GGUF. With the sign-off.
- **A reproducibility warm-up** (a reference output recorded on the target hardware class at promotion). When there is a GPU class to target.
- **The sidecar proving its identity cryptographically** (a measured image or an attestation) rather than our entrypoint plus alias. Before the sidecar runs on a different host from the worker.
- **GPU serving and more than one concurrent question** (`--parallel`, a queue, the pipeline's concurrency raised). When answer time or volume demands it.
- **Drift and quality monitoring** on live answers (escalation rate, trust distribution per version), with an alert. Before a second hospital.
- **Training on de-identified real data** (`training.ALLOWED_KINDS` gains `deidentified`), with the privacy lead's sign-off. Not before.
- **A written, held-out locked split** for `clinical_answer` from the question set owner (beside or instead of the template bank), so the locked questions are not derivable from the repository. With the clinical sign-off.
- **A runner for the cloud comparison** on the locked split (the report already has the field). Before the first switch (PR 4) if the product owner wants the numbers side by side.
- **A `cca` rehearsal in the CI rehearsal mode** (a tiny GGUF and llama-server in `model-promotion-e2e`). When promotion runs are frequent enough to need it.
- **Removing the cloud route** for a hospital entirely (no key on the host). When every hospital runs local.

## Open questions

1. **Model size.** 1-3B fits the demo host on CPU; 7B needs a larger host or a GPU. Recommended: start with a 3B instruct base (Qwen2.5-3B-Instruct, Apache-2.0) and measure; move up only if the gate fails on quality. Needs: the demo host's RAM (to set `PILOT_LOCAL_MODEL_MEM`).
2. **GPU.** None today. Recommended: none for the pilot; revisit with the sizing table if a 7B model is needed.
3. **Who owns the question set.** Recommended: the intelligence lead writes the dev split and the first locked split with the product owner; a clinical lead reviews it before scale. Clinical sign-off is deferred for the pilot (README).
4. **Base model licences.** Gemma and Llama community licences have use restrictions; Apache-2.0 bases avoid the question. The product owner decides which go into `models/licences.json`.
5. **Where the `cca` evaluation runs.** GitHub's runner for 1-3B; steven-reid for 7B. Recommended as stated; revisit if runner time is a cost.

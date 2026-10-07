# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses
[Semantic Versioning](https://semver.org/).

## [Unreleased]

### Security
- Retrieval and citation checks are scoped to the queried patient. Other
  patients' records are never returned, and citing one is redacted (#1).
- Reserved ids (`reference`, `unassigned`, `unknown`) can no longer be used
  as patient ids, and patient files without an id are skipped.
- The orchestrator fails closed. A missing or failing CCA/HGA escalates the
  result as `degraded` instead of inventing a score, and thresholds are
  passed per call instead of mutating shared state (#2).
- Doctor corrections need approval from a second, different clinician
  before they change the knowledge base, persona vector or trust weights.
  Approved corrections are PII-masked, patient-scoped and attributed (#3).
- NLI verifiers read the label order from the model. The configured model's
  order was being misread, so unsupported claims passed as entailed.

### Added
- Docker Compose stack: Postgres, Redis, Ollama with model pull, the app, an
  optional OpenTelemetry collector, and a GPU override, with Make targets.
- The CCA can generate through Ollama (`CCA_BACKEND=ollama`).
- `scripts/e2e_models.py`: end-to-end check with real models on synthetic
  data.
- `SKILLS.md` security rules, contribution workflow for agents and humans,
  ADRs, PR and issue templates, `SECURITY.md`, CODEOWNERS and Dependabot.
- CI enforces lint, format, type checks, tests, coverage of changed lines and
  a secret scan. Tagged releases build and push the container image.

### Changed
- **Breaking:** the package moved from `src/mhai` to `mhai`.
- **Breaking:** a `correct` feedback returns `{correction_id, status:
  "pending_review"}`; it is applied by `approve_correction`.
- **Breaking:** Python 3.12 only.
- Project managed with uv (`uv.lock`); CPU PyTorch by default.
- Two-stage Docker image (7.1 GB to 2.5 GB, CPU), with pinned base images.

### Fixed
- `PIIMasker` no longer corrupts text when patterns overlap (e.g.
  `[PHONE_REDACTED]TED]`), counts each masked span once, keeps labels such as
  "Patient:" and "MRN:", and masks a leading "+1" or "(" in phone numbers
  (#7).
- `mhai.agents` could not be imported (its `__init__` exported missing
  classes).
- Imports left over from the original notebooks.

## [1.0.0] - 2026-06

### Added
- Initial production release from Colab notebooks (M1–M4, ~18,500 lines).
- Five agents: DIA, QDA, CCA, HGA, Orchestrator.
- Four-layer hallucination cascade.
- HIPAA security layer: PII masking, AES encryption, RBAC, audit chain.
- 1000-case E2E regression suite.

# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses
[Semantic Versioning](https://semver.org/).

## [Unreleased]

### Security
- Sign-in has a per-login ceiling across every client IP (20 failures an
  hour, keyed by hash, failing closed) on top of the (login, IP) lockout,
  and argon2 runs behind a process-wide gate (one slot per CPU; 503 when
  saturated).
- `core serve` refuses a database login that is a member of any role
  besides `mhai_app`, and refuses to start with the idempotency middleware
  missing.
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
- The core service (Go, `services/core/`, Plan 2): sign-in with lockout,
  rotating refresh tokens, two-step verification (TOTP, backup codes,
  trusted devices, step-up), password change, device sessions, terms
  acceptance, the permission table and principal middleware, hospital
  separation by row-level security, a hash-chained audit log and
  Idempotency-Key support. The contract is `services/core/openapi.yaml`,
  checked by a test against the served routes and the error codes.
- The core service runs in Docker Compose (`make core-up`): `core-migrate`
  (owner connection) also creates its two logins: `core_app`, a member of
  `mhai_app` only, for the service, and `core_provision`, a member of
  `mhai_provision` only, for the seed CLI (`core-cli`). `core-keys` makes
  the dev signing key. The core service's environment holds only its own
  secrets.
- `GET /api/v1/readyz`: 503 unless Postgres and NATS (with every KV bucket)
  answer. `core keys new|ensure` generates the access-token key.
- Expired Idempotency-Key rows are swept every 10 minutes
  (`mhai_sweep_idempotency()`, migration 00006).
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
- **Breaking:** the services moved under `services/`. The Go service is
  `services/core/` (module `.../mhai-multiagent/services/core`, binary
  `core`, compose services `core-migrate`; `GATEWAY_ENV` is now `CORE_ENV`).
  The Python package `mhai` is now `intelligence`, in
  `services/intelligence/` with its own `pyproject.toml` and `uv.lock`; the
  CLI is `intelligence` and the compose service `intelligence`. The NATS
  config is in `services/nats/`.
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

# Agent guide: mhai-multiagent

Instructions for AI coding agents working in this repository. Humans:
see CONTRIBUTING.md.

## Read first

1. [SKILLS.md](SKILLS.md): security rules. Patient isolation, fail closed,
   identity and PHI are non-negotiable.
2. [CONTRIBUTING.md](CONTRIBUTING.md): conventions, clinical change
   control and the agent contribution workflow.
3. `docs/adr/`: why things are the way they are.

## Security is part of every step

We build securely, and security starts in the design. The project skills are in `.claude/skills/`, and every agent uses them:

| When | Skill | What it requires |
|---|---|---|
| Writing a design, spec, ADR or plan | `secure-design` | A **Security and threat model** section (assets, boundaries, STRIDE threats, controls, abuse-case tests, residual risk). A design without it isn't approved. |
| Writing code | `secure-build` | Identity from the token only, RLS and least privilege, hashed secrets, no PII in logs, reserve-then-check, fail closed. |
| Reviewing a change | `security-review` | An attacker's walk through the vulnerability classes this codebase has actually had, with proof and severity. |
| Finding and fixing bugs | `bug-hunt` | A failing test first, edge cases (concurrency, time, pooled connections, policy changes), and fixing the whole class. |

Run `security-review` on your own diff before you open a PR. When a review finds a new kind of issue, add it to that skill's list.

## Workflow

- Work on your own branch (`agent/<topic>` or `fix/<issue>-<topic>`); open
  a PR using the template.
- **Never merge, approve or push to `main`.** A human reviews and merges.
- If changes are requested, read every review comment, fix on the same
  branch, reply to each comment, and re-request review.
- Ask in the PR rather than guess when a decision is clinical, about
  security, or outside the task.
- No AI attribution in commits or PRs.

## Commands

```bash
# Python: uv commands run in services/intelligence/; make targets from the root.
uv sync                      # install (CPU PyTorch); never pip install
uv add <pkg> / uv add --dev  # add dependencies; commit uv.lock
make lint                    # ruff check
make format                  # ruff format + safe fixes
make typecheck               # mypy
make test                    # pytest (no model downloads)
uv run pytest tests/security # security regression tests
make infra                   # postgres, ollama for local runs
make up / make down          # full compose stack
make core-test / core-lint   # the core service (Go)
make core-up                 # the core service in compose (postgres, nats, migrations)
make smoke                   # end-to-end smoke: stub models, echo, core as the BFF over NATS (Docker, Go)
```

`services/intelligence/scripts/e2e_models.py` runs the real models; it needs
a machine that can load them, and uses synthetic data only.

## Product context

- **MHAI** is clinical decision support: clinicians ask, our own model (or an approved cloud model) answers from reviewed guidelines, every claim is checked against what was retrieved, and anything unsafe escalates to a person. Two apps (mhai-web): the clinical app and the platform admin app.
- **DocFlow** is Measured Health's scanned-records product (repos `emr-api`, a FastAPI and MongoDB service that indexes scanned files from storage, and `emr-web`, its viewer). Hospitals keep their paper history in it. **The documents pipeline is DocFlow's link to MHAI:** DocFlow → the DocFlow connector (`document_archive`) → the documents service (scan, sandbox, image and PDF to text, human review) → the patient's EMR records → alerts, Analysis and answer context. DocFlow already lists files added since a time (`/api/v1/files/search?modified_after=` with a cursor, `/files/delta/parts`); it still needs a per-integration read-only key and a removed-files listing. Image and PDF to text are not built (TODO.md, documents section).
- **EMR and EHR connections** (services/emr, ADR 0014): `fhir_r4` and `file_export` (CSV over SFTP) are built; `document_archive` (listed as "DocFlow") is declared, not built; `hl7v2_file` and an on-premises bridge (MLLP on the hospital's network, files over SFTP to us) are in progress. Our side stays outbound-only. The pipeline is two-way by design: reading for alerts, Analysis and answer context; writing clinician-approved notes back (docs/specs/2026-11-27-ehr-write-back-design.md). EMR context in answers: docs/specs/2026-11-27-emr-context-in-answers-design.md. Both approved 27 Nov, not built.
- **Operating rules learned the hard way:** never mark a hospital `is_demo` on the live host (the deploy's live-site check stops core; a 30-hour outage on 25-27 Nov); after any backend merge, watch the "Deploy backend demo" workflow until it succeeds; the live core still runs `CORE_ENV=development` until the production (TLS) switch; never name the pilot hospital in code, docs, logs, branches or commits.

## Layout

The backend is the core service (Go) plus Python model services, joined by
NATS (ADR 0010). The core service's auth, permissions, tenancy and audit
are built (Plan 2; see services/core/README.md and services/core/openapi.yaml).
The Python side runs behind NATS as workers (Plan 3): core signs a request
token for every call and job, and the workers verify it before any work
(services/intelligence/README.md).

```
services/core/        the core service (Go): API, auth, tenancy, audit, alert routing
services/emr/         the EMR integration service (Go): connectors, credentials, egress, normaliser, rules (ADR 0014)
services/gokit/       Go shared by core and emr: request tokens, replay set, WithHospital, log redaction
services/nats/        NATS config: the only link from Go to Python
services/intelligence/  the Python project (uv)
  intelligence/boundary/        the NATS workers: token checks, operations.json, jobs, RLS database access
  intelligence/agents/          DIA, QDA, CCA, HGA (hga_nli.py), orchestrator
  intelligence/knowledge_base/  KB builder, retrievers, RAG CCA (patient scoping lives here)
  intelligence/guardrails/      L1-L4 cascade
  intelligence/security/        PII masking only (roles, permissions and audit are core's)
  intelligence/tools/           Ollama client, PubMed, HF helpers
  tests/security/               regression tests for the security fixes
```

## Where a change goes

- **Go (`services/core/`):** the API, authentication, permissions, tenancy,
  audit, and who sees an EMR alert.
- **Go (`services/emr/`):** EMR plumbing: connectors, outside credentials,
  egress, normalisation, the patient index and the rules. It never imports
  `services/core/` (ADR 0014).
- **Go (`services/gokit/`):** code core and emr share. A defect here reaches
  both: review it as security code.
- **Python (`services/intelligence/intelligence/`):** models and the
  pipeline: agents, retrieval, guardrails, PII masking.

## Rules of thumb

- Every retrieval path takes `patient_id`; use `chunk_visible_to()`.
- Failures escalate; never invent scores or placeholder answers.
- Pass per-request values as arguments; never mutate shared agents.
- User identity comes from core's verified request token, never from
  request data.
- Security rules are written once, in Go. Don't copy a permission or
  tenancy rule into Python.
- Python never trusts a hospital or user from a payload, only from the
  verified core service token.
- Don't add entries to the lint, format or mypy baselines in
  `services/intelligence/pyproject.toml`.
- Use synthetic data only. No secrets or PHI in code, tests, logs or PRs.

# Agent guide: mhai-multiagent

Instructions for AI coding agents working in this repository. Humans:
see CONTRIBUTING.md.

## Read first

1. [SKILLS.md](SKILLS.md): security rules. Patient isolation, fail closed,
   identity and PHI are non-negotiable.
2. [CONTRIBUTING.md](CONTRIBUTING.md): conventions, clinical change
   control and the agent contribution workflow.
3. `docs/adr/`: why things are the way they are.

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
make infra                   # postgres, redis, ollama for local runs
make up / make down          # full compose stack
make core-test / core-lint   # the core service (Go)
make core-up                 # the core service in compose (postgres, nats, migrations)
```

`services/intelligence/scripts/e2e_models.py` runs the real models; it needs
a machine that can load them, and uses synthetic data only.

## Layout

The backend is the core service (Go) plus Python model services, joined by
NATS (ADR 0010). The core service's auth, permissions, tenancy and audit
are built (Plan 2; see services/core/README.md and services/core/openapi.yaml).
The Python side still runs on its own, until the boundary plan moves it
behind NATS.

```
services/core/        the core service (Go): API, auth, tenancy, audit, EMR
services/nats/        NATS config: the only link from Go to Python
services/intelligence/  the Python project (uv)
  intelligence/agents/          DIA, QDA, CCA, HGA (hga_nli.py), orchestrator
  intelligence/knowledge_base/  KB builder, retrievers, RAG CCA (patient scoping lives here)
  intelligence/guardrails/      L1-L4 cascade
  intelligence/security/        PII masking, RBAC, audit, SecurePipelineWrapper
  intelligence/tools/           Ollama client, PubMed, HF helpers
  tests/security/               regression tests for the security fixes
```

## Where a change goes

- **Go (`services/core/`):** the API, authentication, permissions, tenancy,
  audit and EMR plumbing.
- **Python (`services/intelligence/intelligence/`):** models and the
  pipeline: agents, retrieval, guardrails, PII masking.

## Rules of thumb

- Every retrieval path takes `patient_id`; use `chunk_visible_to()`.
- Failures escalate; never invent scores or placeholder answers.
- Pass per-request values as arguments; never mutate shared agents.
- User identity comes from the session (`SecurePipelineWrapper`), never from
  request data.
- Security rules are written once, in Go. Don't copy a permission or
  tenancy rule into Python.
- Python never trusts a hospital or user from a payload, only from the
  verified core service token.
- Don't add entries to the lint, format or mypy baselines in
  `services/intelligence/pyproject.toml`.
- Use synthetic data only. No secrets or PHI in code, tests, logs or PRs.

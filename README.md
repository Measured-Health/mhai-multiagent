# MHAI Multi-Agent System

**Medical Healthcare AI** — A hallucination-resistant clinical decision-support system
with 5 specialized transformer-based agents and a 4-layer guardrail cascade.

The backend has two parts:

- **Python:** the models, pipeline and guardrails in
  `services/intelligence/intelligence/`.
- **The core service (Go, in `services/core/`):** the API, sign-in, permissions,
  tenancy, audit and EMR plumbing. It reaches the Python services only
  through NATS. See [ADR 0010](docs/adr/0010-api-in-go-models-in-python-over-nats.md),
  [ARCHITECTURE.md](ARCHITECTURE.md) and
  [services/core/README.md](services/core/README.md). Built so far (Plan 2):
  sign-in, sessions, two-step verification, permissions, tenancy and audit;
  the contract is [services/core/openapi.yaml](services/core/openapi.yaml).

## Agents

| Agent | Architecture | Role |
|-------|-------------|------|
| **DIA** | Swin Transformer V2 + MoE | Document ingestion, OCR, table extraction |
| **QDA** | FT-Transformer + RUSBoost | Rapid binary/multi-class clinical triage |
| **CCA** | Qwen-2.5-7B + QLoRA + RAG | Citation-backed clinical reasoning |
| **HGA** | DeBERTa-v3 + 4-Expert MoE | Hallucination detection & trust scoring |
| **OA**  | Query Classifier + CMA-ES | Dynamic routing, feedback loop, orchestration |

## Guardrail Cascade

1. **L1 — Persona Vector Probe**: Contrastive activation analysis for identity drift
2. **L2 — Semantic Entropy**: K=5 sampling consistency check
3. **L3 — NLI Citation Entailment**: DeBERTa-v3 claim verification (PASS/FLAG/REDACT)
4. **L4 — Trust Score + Escalation**: Composite T(y) with dynamic thresholds

## Quick Start

The Python service is a [uv](https://docs.astral.sh/uv/) project in
`services/intelligence/`; run its commands from there.

```bash
cd services/intelligence

# Install (CPU PyTorch by default; on a CUDA machine:
#   uv sync --no-group cpu --group cu128)
uv sync

# Run tests
uv run pytest

# CLI
uv run intelligence run --query "28yo female with gestational diabetes" --urgency routine
```

These commands are for the Python side. The core service's commands are
run from `services/core/` (`go test ./...`, or `make core-test` from the root);
see [services/core/README.md](services/core/README.md).
The whole stack runs with Docker Compose from the repository root (below).

Add dependencies with `uv add <pkg>` (or `uv add --dev <pkg>`) and commit
`uv.lock`.

## Local Stack (Docker Compose)

```bash
make up                 # postgres, nats, core, ollama (+ pulls OLLAMA_MODEL), intelligence
make core-up            # only the core service and what it needs (postgres, nats, migrations)
make up-gpu             # same on an NVIDIA GPU (Ollama + CUDA PyTorch)
make up-observability   # also the OpenTelemetry collector
make infra              # only postgres, ollama; run the app with uv
make ps | logs | down   # status, logs, stop (data kept)
make nuke               # stop and delete all volumes (asks first)
```

`make up` creates `.env` from `.env.example` on first run; replace every
`changeme` value first, because the core service and NATS refuse
placeholders. Ports bind to 127.0.0.1 and can be moved with `POSTGRES_PORT`,
`NATS_PORT`, `CORE_PORT`, `OLLAMA_PORT`, `API_PORT`,
`OTEL_GRPC_PORT` and `OTEL_HTTP_PORT`. The CCA
generates through Ollama (`CCA_BACKEND=ollama`, model `OLLAMA_MODEL`).
The `intelligence` service currently runs the CLI and exits; it becomes a
server once the API exists.

## Security

Read [SKILLS.md](SKILLS.md) before changing retrieval, guardrails,
feedback or identity handling, and run `uv run pytest tests/security` in
`services/intelligence/`.

## Project Structure

```
services/
├── core/            # The core service (Go): API, auth, tenancy, audit, EMR
├── nats/            # NATS server config: per-service users and permissions
└── intelligence/    # Python models and pipeline (reached through NATS)
    ├── pyproject.toml, uv.lock, Dockerfile, tests/, scripts/, configs/
    └── intelligence/  # the package

services/intelligence/intelligence/
├── agents/          # One module per agent (DIA, QDA, CCA, HGA, OA)
├── config/          # Pydantic-style settings
├── guardrails/      # 4-layer hallucination cascade
├── knowledge_base/  # Medical KB builder + chunk indexer
├── models/          # Neural network architectures
├── pipeline/        # Orchestration runner + cross-milestone integration
├── retrieval/       # Hybrid dense+sparse+RRF retrieval
├── security/        # PII masking, encryption, latency profiler (roles and audit are core's)
├── tools/           # External connectors (HuggingFace, PubMed)
└── utils/           # Logger, retry, rate limiter, device detection
```

## Affiliation

Promit Ghosal — BHAI, University of Chicago — 2026

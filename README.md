# MHAI Multi-Agent System

**Medical Healthcare AI** — A hallucination-resistant clinical decision-support system
with 5 specialized transformer-based agents and a 4-layer guardrail cascade.

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

```bash
# Install
pip install -e ".[dev]"

# Run tests
pytest

# Docker
docker compose up -d

# CLI
mhai run --query "28yo female with gestational diabetes" --urgency routine
```

## Project Structure

```
mhai/
├── agents/          # One module per agent (DIA, QDA, CCA, HGA, OA)
├── config/          # Pydantic-style settings
├── guardrails/      # 4-layer hallucination cascade
├── knowledge_base/  # Medical KB builder + chunk indexer
├── models/          # Neural network architectures
├── pipeline/        # Orchestration runner + cross-milestone integration
├── retrieval/       # Hybrid dense+sparse+RRF retrieval
├── security/        # HIPAA: PII masking, encryption, RBAC, audit
├── tools/           # External connectors (HuggingFace, PubMed)
└── utils/           # Logger, retry, rate limiter, device detection
```

## Affiliation

Promit Ghosal — BHAI, University of Chicago — 2026

# intelligence

The Python side of MHAI: the clinical agents, guardrails, retrieval and the
`intelligence` CLI. It is a uv project (ADR 0002) with its own
`pyproject.toml` and `uv.lock`; run every command from this directory.

```bash
uv sync --locked          # install (CPU PyTorch by default)
uv run ruff check .
uv run ruff format --check .
uv run mypy
uv run pytest -m "not models"
uv run intelligence run --query "Test query" --urgency routine
```

See the repository's top-level README.md and ARCHITECTURE.md for the whole
system, and `services/core/` for the Go service in front of it.

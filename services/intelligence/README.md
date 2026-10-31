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

See the repository's top-level README.md and ARCHITECTURE.md for the whole
system, and `services/core/` for the Go service in front of it.

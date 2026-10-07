# syntax=docker/dockerfile:1

# ── Build: resolve and install everything into /app/.venv ──
FROM python:3.12.15-slim AS build

RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential git && rm -rf /var/lib/apt/lists/*

COPY --from=ghcr.io/astral-sh/uv:0.12.23 /uv /usr/local/bin/uv

WORKDIR /app

# PyTorch build to install: cpu or cu128.
ARG TORCH=cpu
ENV UV_COMPILE_BYTECODE=1 UV_LINK_MODE=copy UV_PYTHON_DOWNLOADS=never

# Dependencies first, so code changes don't reinstall them. The cache
# mount keeps uv's download cache out of the image.
COPY pyproject.toml uv.lock README.md ./
RUN --mount=type=cache,target=/root/.cache/uv \
    uv sync --locked --no-default-groups --group ${TORCH} --no-install-project

COPY mhai ./mhai
RUN --mount=type=cache,target=/root/.cache/uv \
    uv sync --locked --no-default-groups --group ${TORCH} --no-editable

# ── Runtime: just Python and the virtualenv ──
FROM python:3.12.15-slim AS runtime

ENV PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    PATH="/app/.venv/bin:$PATH"

RUN apt-get update && apt-get install -y --no-install-recommends curl \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --create-home appuser

WORKDIR /app
COPY --from=build /app/.venv /app/.venv

# The venv stays root-owned and read-only; the app only writes here.
RUN mkdir -p /app/data /app/artifacts /home/appuser/.cache/huggingface \
    && chown appuser /app/data /app/artifacts /home/appuser/.cache/huggingface
USER appuser

HEALTHCHECK --interval=30s --timeout=5s CMD curl -f http://localhost:8000/healthz || exit 1

ENTRYPOINT ["mhai"]
CMD ["run", "--help"]

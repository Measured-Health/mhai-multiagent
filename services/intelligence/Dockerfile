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

COPY intelligence ./intelligence
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
# The fixed warm-up page each pinned in-process role must reproduce before the pipeline is
# ready (plan Tasks 23, 24): a copy of testdata/models/warmup/page.png, which
# tests/modelstore/test_workflows.py keeps byte-identical. INTELLIGENCE_WARMUP_PAGE names it.
COPY warmup/page.png /app/warmup/page.png

# The venv stays root-owned and read-only; the app only writes here.
RUN mkdir -p /app/data /app/artifacts /home/appuser/.cache/huggingface \
    && chown appuser /app/data /app/artifacts /home/appuser/.cache/huggingface
USER appuser

HEALTHCHECK --interval=30s --timeout=5s CMD curl -f http://localhost:8000/healthz || exit 1

ENTRYPOINT ["intelligence"]
CMD ["run", "--help"]

# ── Pilot: the runtime plus the embedding and NLI weights, for a host with
# no route to Hugging Face (the pilot pipeline on the demo host,
# deploy/pilot/README.md; build with --target pilot). Each model is pinned
# by commit and fetched as safetensors and config files only (never a
# pickled .bin). refs/main is pointed at the pinned commit, so the offline
# load by name finds exactly that snapshot; the build then loads both
# offline, so an image that can't is never pushed.
FROM runtime AS pilot
ARG DENSE_REPO=sentence-transformers/all-MiniLM-L6-v2
ARG DENSE_REVISION=1110a243fdf4706b3f48f1d95db1a4f5529b4d41
ARG NLI_REPO=cross-encoder/nli-deberta-v3-base
ARG NLI_REVISION=6c749ce3425cd33b46d187e45b92bbf96ee12ec7
ENV HF_HOME=/home/appuser/.cache/huggingface
RUN python - <<'PY'
import os
from pathlib import Path

from huggingface_hub import snapshot_download

home = Path(os.environ["HF_HOME"]) / "hub"
for repo, rev, patterns in (
    (os.environ["DENSE_REPO"], os.environ["DENSE_REVISION"],
     ["*.json", "*.txt", "model.safetensors"]),
    (os.environ["NLI_REPO"], os.environ["NLI_REVISION"],
     ["*.json", "spm.model", "model.safetensors"]),
):
    snapshot_download(repo, revision=rev, allow_patterns=patterns)
    refs = home / ("models--" + repo.replace("/", "--")) / "refs"
    refs.mkdir(parents=True, exist_ok=True)
    (refs / "main").write_text(rev)
PY
ENV HF_HUB_OFFLINE=1 TRANSFORMERS_OFFLINE=1
RUN python - <<'PY'
from sentence_transformers import SentenceTransformer
from transformers import AutoModelForSequenceClassification, AutoTokenizer
import os

SentenceTransformer(os.environ["DENSE_REPO"], device="cpu")
AutoTokenizer.from_pretrained(os.environ["NLI_REPO"])
AutoModelForSequenceClassification.from_pretrained(os.environ["NLI_REPO"], use_safetensors=True)
print("pilot weights load offline")
PY

# The default target stays the plain runtime (CI, release).
FROM runtime

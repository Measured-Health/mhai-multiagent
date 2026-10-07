# syntax=docker/dockerfile:1
FROM python:3.11-slim AS base

ENV PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    PIP_NO_CACHE_DIR=1

WORKDIR /app

RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential git curl && rm -rf /var/lib/apt/lists/*

COPY requirements.txt ./
RUN pip install --upgrade pip && pip install -r requirements.txt

COPY pyproject.toml README.md ./
COPY src ./src
RUN pip install -e .

RUN useradd --create-home appuser && chown -R appuser /app
USER appuser

HEALTHCHECK --interval=30s --timeout=5s CMD curl -f http://localhost:8000/healthz || exit 1

ENTRYPOINT ["mhai"]
CMD ["run", "--help"]

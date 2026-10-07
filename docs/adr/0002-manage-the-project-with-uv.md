# 0002. Manage the project with uv, CPU PyTorch by default

- **Status:** Accepted
- **Date:** 2026-10-07

## Context

Dependencies lived in `pyproject.toml` and two requirements files, with
nothing locked. A plain install of `torch` on Linux pulls the CUDA build
(about 3 GB of `nvidia-*` wheels), even on CPU-only machines and in CI.

## Decision

Use uv with a committed `uv.lock`. Dev tools are in the `dev` dependency
group. PyTorch comes from the PyTorch CPU index through a default `cpu`
group. CUDA machines opt in with `--no-group cpu --group cu128`. CI, the
Makefile and the Dockerfile all install with `uv sync --locked`.

## Consequences

Reproducible installs, and fast CPU installs everywhere by default. GPU
deployments must pass the group flags; the Dockerfile and compose GPU
override do this through `TORCH=cu128`. Contributors need uv installed.

## Alternatives considered

pip-tools or Poetry: slower and no first-class per-group index pinning.
Leaving PyTorch's source unpinned: wastes gigabytes per install.

# 0001. Record architecture decisions

- **Status:** Accepted
- **Date:** 2026-10-07

## Context

MHAI combines clinical logic, ML models and patient data. Decisions about
them are easy to make and hard to reconstruct later: why a threshold, why a
tool, why a security rule. People and AI agents both change the code.

## Decision

Record significant decisions as short ADRs in `docs/adr/`, numbered in
sequence, using `0000-template.md`. ADRs are never deleted; a reversed
decision is marked *Superseded by NNNN*.

## Consequences

New contributors and agents can find the reasoning behind non-obvious
choices. Writing an ADR becomes part of the PR for such changes (see the
PR template).

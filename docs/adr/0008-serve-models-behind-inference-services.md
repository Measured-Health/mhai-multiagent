# 0008. Serve every model behind an inference service

- **Status:** Accepted
- **Date:** 2026-10-08
- **Amends:** 0007

## Context

MHAI will do real inference work: GPU serving, batching, several model
versions, and scaling inference separately from request handling. In the
original code and the first API design, the embedding model, the NLI
cross-encoder and OCR ran inside the same Python process as the
orchestration and guardrail logic. Systems built that way tend to need a
disruptive rewrite later: model calls have to be untangled from
application code before inference can be scaled, swapped to a dedicated
engine, or before the application layer can change language.

The code also showed the cost of in-process model handling: NLI label
order was assumed rather than read from the model, which silently
inverted the hallucination check (fixed in #4).

## Decision

- **Every model sits behind a client interface** in `mhai/inference/`:
  `Embedder`, `NLIClassifier`, `Generator` and `OCR`. The pipeline depends
  only on these.
- **Models are served by inference services:** Ollama for the LLM, and an
  `inference` service for embeddings and NLI. OCR stays in-process behind
  its interface until it needs its own service.
- **Standard wire protocols:**
  - OpenAI-compatible `/v1/embeddings` for embeddings;
  - `/v1/classify` with **named** labels for NLI;
  - health reporting model names, versions and checksums.
- **The API and the worker import no ML frameworks** (`torch`,
  `transformers`, `sentence_transformers`). Tests enforce this, and the ML
  stack moves to an `ml` dependency group used only by the inference image
  and research scripts.

## Consequences

- **Inference engines can change by configuration** (TEI, vLLM, Triton,
  CPU or GPU, more replicas) without touching orchestration code.
- **The application layer stays model-free,** so ADR 0007's option to move
  the API, or the worker, to another language remains cheap.
- **Model identity is explicit:** every result records the model and
  version that produced it, which supports audit and clinical change
  control.
- **Costs:** one more service to run, a network hop per model call
  (mitigated by batching), and client timeouts, retries and circuit
  breaking to maintain. Inference outages make agents fail, which
  escalates results (ADR 0005).

## Alternatives considered

- **Keep models in the worker process:** fastest now, but it's the
  structure that forces a rewrite once inference needs to scale.
- **Move the API and orchestration to Go now, with Python only in
  inference servers:** removes any later language move, but means porting
  the existing guardrail and orchestration logic now, which is the rewrite
  done up front, with no measured need.

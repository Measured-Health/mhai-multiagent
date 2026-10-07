# 0003. Serve the CCA LLM through Ollama

- **Status:** Accepted
- **Date:** 2026-10-07

## Context

The CCA loaded Qwen-2.5-7B in-process with transformers. That ties the API
process to a large GPU footprint and makes model swaps a code change.

## Decision

Run the LLM in an `ollama` compose service. The CCA calls it through
`OllamaGenerator` when `CCA_BACKEND=ollama` (the default), with the model set
by `OLLAMA_MODEL`. The in-process transformers path remains for research
and evaluation.

## Consequences

The API process stays light, and models are pulled and swapped by
configuration. Ollama errors raise, so the orchestrator escalates; it never
answers without the LLM. Prompt formatting now depends on Ollama's chat
template for the model, so model changes go through clinical change
control.

## Alternatives considered

vLLM or TGI: more throughput, heavier to run for a small team at this
stage; revisit for production scale.

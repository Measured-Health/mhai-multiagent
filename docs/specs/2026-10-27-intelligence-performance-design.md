# Fast intelligence: serving the models on a rented GPU: design

- **Status:** Parked (product owner, 2026-10-27): performance work for later; see TODO.md. Not yet builder-checked or approved.
- **Date:** 2026-10-27
- **Builds on:**
  - ADR 0010: the core service is Go, the models are Python, and NATS connects them.
  - The Go core and NATS design: `docs/specs/2026-10-26-go-gateway-nats-and-auth-design.md`.
  - ADR 0008: models sit behind inference services.
- **Background reading:** *Inference Engineering* (Philip Kiely, Baseten, 2026).

## 1. Why

A clinical question takes about 8–30 seconds today, almost all of it inside the models. The product owner wants the intelligence service to be as fast as it can be. Speed is also why it was split from the rest of the system: its model serving can be tuned and scaled on GPUs on its own.

## 2. What "fast" can mean (the targets)

Some steps can run in milliseconds and one can't, so each step gets its own target.

| Step | Today (estimate) | Target (p50 / p95) |
|---|---|---|
| PII masking | tens of ms (CPU) | ≤ 10 / 25 ms |
| Retrieval: embed the question and search | 50–300 ms (CPU) | ≤ 30 / 60 ms |
| First words of the answer: time to first token (TTFT) | several seconds (Ollama, CPU or small GPU) | **≤ 300 / 800 ms** |
| Full answer, about 300 tokens | 6–20 s | **≤ 2.5 / 4 s** |
| NLI citation check, all claims in one batch | 1–5 s (CPU) | ≤ 40 / 100 ms |
| HGA trust score | 0.5–2 s (CPU) | ≤ 30 / 80 ms |
| Whole checked answer, end to end | 8–30 s | **≤ 3 / 5 s** |
| Document page (Swin-V2) and tabular (QDA) models | 100s of ms (CPU) | ≤ 20 / 50 ms each |

These hold at the load target: **20 clinicians asking at the same time on one GPU** without breaking p95.

A complete written answer can't be milliseconds. The model writes it one token at a time, so a few hundred tokens take a second or more even on a fast GPU. Everything around that step can be milliseconds, and streaming makes the first words appear almost at once (§5.3).

These targets are my call. They get re-set from the first measurements (§6).

## 3. Goals, non-goals, anti-goals

**Goals**
- Hit the targets in §2 on one rented cloud GPU.
- Measure every step, so each optimisation is proven and not guessed at.
- Keep answer quality and the safety checks exactly as they are, or provably no worse.

**Non-goals**
- Changing which clinical models are used. A smaller or different model is a clinical decision, outside this design. §5.5 covers only quantising the same models.
- Training or fine-tuning.
- Multi-GPU and multi-region scaling. The design allows them, but they are not built here.

**Anti-goals: things that would make this a failure even if it got faster**
- **Weaker checks.** Skipping or weakening the NLI or HGA checks to save time.
- **Patient data in the wrong place.** Sending patient data to a GPU outside our control (§4.2).
- **Unmeasured quality loss.** Answers that are faster but measurably worse on the safety set, without anyone signing that off.

## 4. Where it runs

### 4.1 The GPU (my call)

| | Choice | Why |
|---|---|---|
| **Pilot** | One NVIDIA **L40S** (48 GB), for example AWS `g6e.xlarge` | Qwen2.5-7B in FP8 plus the small models fit, with room for the KV cache. It supports FP8. It's on AWS, where the demo already runs. |
| **Later, if load or targets need it** | **H100** (80 GB), for example AWS `p5` | About 2–3× faster decode. FP8. |
| **Rough cost** | L40S at about $1.9 per hour ≈ $1,400 a month running 24/7. Approximate; check current AWS pricing. | A clinical service needs a warm GPU, so it can't scale to zero. |

### 4.2 Data protection

The GPU host sees patient data in the prompts. So:
- It runs **in our own AWS account and network**, in the same region as the core service, and is reached only over NATS with its own credentials (core design §5).
- **Third-party GPU marketplaces and hosted inference APIs are not used** for patient data until a data processing agreement is in place.
- **Disks are encrypted.** Nothing writes prompts or answers to logs (SKILLS.md, logging rules).
- **Synthetic data only** until go-live, as everywhere else.

## 5. How

### 5.1 The language model gets a real serving engine

- **vLLM** replaces Ollama for Qwen2.5-7B-Instruct (my call). It offers:
  - **continuous batching**, so many questions share the GPU;
  - **paged attention and prefix caching**, so the long shared system prompt and instructions are computed once and reused;
  - **speculative decoding** (§5.4) and **FP8** (§5.5);
  - an OpenAI-compatible server and Prometheus metrics.
- **TensorRT-LLM** is benchmarked against vLLM after vLLM works. It is adopted only if it beats vLLM on our measurements by enough to justify its build complexity. SGLang is the fallback if vLLM can't meet TTFT.
- vLLM runs as its own process on the GPU host. The intelligence pipeline service calls it locally, and core never calls it directly.

### 5.2 The small models are compiled and batched

- **The models:** the NLI cross-encoder (`nli-deberta-v3-base`), the HGA scorer (DeBERTa-v3), the embedder (`all-MiniLM-L6-v2`), Swin-V2 and the QDA model.
- **How they run:** each is exported to **ONNX** and run with **ONNX Runtime (CUDA / TensorRT execution provider)**, in FP16. The clinical evaluation in §6.2 checks every export.
- **Batching:** one `inference` worker process on the GPU batches requests per model over a few milliseconds (dynamic batching). All the claims in one answer go to NLI in **one batch**.
- **Triton Inference Server** is the alternative if the hand-rolled batching gets complicated. My call: start with ONNX Runtime plus a small batcher, and move to Triton only if needed.

### 5.3 The answer streams

- The pipeline streams tokens from vLLM as they are generated.
- The tokens go to core on a per-request NATS reply subject: the request carries the subject, which is under core's own inbox prefix (core design §5.1). Core passes them on to the BFF.
- **This changes the frontend contract.** Today the app polls a query until it's done (frontend requirements §7). Streaming needs a server-sent-events route through the BFF. That is a later mhai-web plan, and this design only makes the stream available.
- Until then, polling still gets the faster end-to-end time.
- **The safety rule doesn't change.** Text streamed before the NLI and HGA checks finish is marked "checking". A failed check replaces it with the review banner, so the clinician never sees an unchecked answer presented as final. *This needs clinical sign-off before streaming is switched on for clinicians.*

### 5.4 Speculative decoding

- A small draft model guesses several tokens ahead, and the 7B model verifies them in one step.
- **Candidates:** Qwen2.5-0.5B-Instruct as a draft, or an EAGLE-style head, or n-gram lookup for repetitive clinical phrasing.
- It's adopted only if it improves our measured full-answer time at the 20-user load. At high load its benefit shrinks, so it has to be checked at that load.
- **Output is identical by construction:** the big model verifies every token, so answer quality doesn't change.

### 5.5 Quantisation (same models, smaller numbers)

- **FP8 weights for Qwen2.5-7B** on L40S or H100 are the first step: about 2× less memory and faster decode, with usually tiny quality loss.
- The 4-bit options (AWQ or GPTQ) are only considered if FP8 isn't enough.
- **Every quantised model must pass the clinical evaluation gate (§6.2)** and get clinical sign-off before clinicians use it.

### 5.6 The pipeline runs its steps in parallel

These run concurrently: PII masking, the question embedding, and loading the patient's chunks. The NLI and HGA checks start as soon as the answer is complete, in parallel, and NLI runs as one batch. Today they run one after another.

### 5.7 Warm, cached and sized

- **Models load at startup** and stay in GPU memory.
- **A health check** reports ready only when every model is loaded and a warm-up request has run.
- **Caches:**
  - embeddings for knowledge-base chunks are computed once and stored;
  - question embeddings are not cached across patients;
  - vLLM's prefix cache is on.
- **Workers:** NATS queue-group workers are sized so requests never queue behind each other at the 20-user load. Their count is set from measurements.

## 6. Measuring

### 6.1 Benchmark harness, built first

- **Questions:** a fixed set of **synthetic clinical questions**, with their patients and knowledge-base content. No real data.
- **What it runs:** single-request latency per step; a ramp to 20 concurrent users; and a soak test (30 minutes at 20 users).
- **What it records:** p50/p95/p99 for every step in §2, TTFT, tokens per second, GPU memory and utilisation, and errors.
- **Comparisons:** it runs before and after every change, and the results are committed as reports under `docs/perf/`.

### 6.2 Clinical evaluation gate

- **Golden set:** a synthetic set with expected outcomes, covering diagnosis or ICD-10 hits, citation check results, and the escalation decision (PASS, REGENERATE or ESCALATE).
- **Threshold:** any change that alters model outputs (ONNX export, FP16, quantisation, a new engine) must keep the golden-set scores within an agreed tolerance of today's baseline. The default is no drop in escalations of unsafe answers, and at most 1 point on the other scores (my call).
- **Sign-off:** the result goes in the report. Anything that needs clinical sign-off says so.

### 6.3 Tracing in production

- **Per-step spans:** each step emits an OpenTelemetry span. There is already an `otel-collector` config. Spans carry the request id and timings, never prompt text.
- **Dashboards** show the §2 numbers.

## 7. Order of work

1. **Measure:** the benchmark harness, OTel spans per step, and the golden set with a baseline on today's setup (CPU and Ollama). This can start now.
2. **GPU host:** rent the L40S in our AWS account, with encrypted disks, private networking and NATS credentials. Run the baseline there.
3. **vLLM:** replace Ollama, turn on prefix caching, and run the benchmark plus the evaluation gate.
4. **Small models:** ONNX export, the GPU batching worker, and the parallel pipeline steps. Benchmark plus the evaluation gate.
5. **FP8:** benchmark plus the evaluation gate, then clinical sign-off.
6. **Speculative decoding:** benchmark at load and keep it only if it helps.
7. **Streaming:** make it available from the intelligence service through core. The frontend's server-sent-events route is a separate mhai-web plan, behind clinical sign-off (§5.3).
8. **Optional:** benchmark TensorRT-LLM, and move to Triton or an H100 if the targets aren't met.

**Dependency:** steps 3 onward use the Python boundary over NATS: the workers and the token checks (core design §10 step 4, the next backend plan after Plan 2). Step 1 doesn't depend on it.

## 8. Open items

1. **GPU budget:** I assumed about $1,400 a month for one L40S running 24/7.
2. **Clinical sign-off** for FP8 and for streaming partial text (§5.3, §5.5).
3. **The evaluation tolerance** (§6.2): the default is my call, and clinicians should confirm it.

## 9. Calls I made (check these)

- The latency targets in §2.
- An L40S on AWS for the pilot, with an H100 later (§4.1).
- vLLM first, and TensorRT-LLM only if it measurably wins (§5.1).
- ONNX Runtime for the small models, and Triton only if needed (§5.2).
- No third-party GPU marketplaces for patient data (§4.2).
- Streamed text stays marked "checking" until the safety checks pass (§5.3).
- The evaluation gate's default tolerance (§6.2).

## 10. Left to the builder

- Exact library versions; the vLLM flags (such as max batch size and GPU memory fraction); the batching window per model.
- The benchmark tool. Locust, k6 or a small Python asyncio driver all work.
- The dashboard layout.

## 11. Questions to answer before planning (from the builder check, 27 Oct)

1. **Streaming and the job model.** A clinical question is a JetStream job, and workers may only reply or publish `dead.v1.>`. Do we make the question request/reply, or give the job a stream subject (with NATS permissions widened)? This changes the core design and the boundary plan.
2. **What "20 clinicians at once" means.** It could mean 20 requests in flight or 20 active users. Also: prompt length (retrieved chunks plus context), and whether background work shares the GPU. 20 × 300 tokens in 2.5 s is about 2,400 tokens/s of decode, which is hard for one L40S. This may need an H100 from the start.
3. **The quality baseline.** Today's Ollama `qwen2.5:7b-instruct` is already 4-bit (Q4_K_M). Should the gate's reference be today's output or BF16? FP8 on vLLM is *higher* precision than today.
4. **The golden set.** Who labels it (clinicians, or today's system, which would measure drift rather than correctness), and how large is it? The 1-point tolerance needs enough cases to mean anything.
5. **HGA.** The live trust score is NLI plus rules. `mhai/models/deberta_moe.py` is a stub. Is there a trained HGA model, or is it NLI plus CPU rules (less export work)?
6. **Swin-V2 and QDA.** They serve document uploads, not the question path. Do they share this GPU? This affects memory sizing.
7. **Which checks are in scope.** Semantic entropy (`k_samples: 5`) and the L1 persona check exist but aren't in the live path. Adding entropy multiplies the decode cost. L1 needs per-token activations, which vLLM doesn't expose.

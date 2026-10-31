# Security skills for MHAI

Rules for anyone changing this codebase, people and AI coding agents
alike. MHAI handles patient records and produces clinical advice, so a
"small" mistake here is a privacy breach or an unsafe answer. Each rule
below exists because the code once broke it; the linked issue explains how.

Before you open a PR, run `uv run pytest tests/security` and walk through
the [review checklist](#review-checklist).

---

## 1. One patient's data never reaches another patient's answer (#1)

- Every retrieval takes a `patient_id`. `HybridRetriever.retrieve`,
  `build_context`, `RAGClinicalContextAgent.generate_with_rag` and
  `CitationVerificationPipeline.verify_response` all accept it. Pass it
  through every new code path. With no `patient_id`, only shared
  guidelines come back. Keep it that way.
- Visibility is decided in one place: `chunk_visible_to()` in
  `services/intelligence/intelligence/knowledge_base/kb_builder.py`. Don't
  write a second copy of that logic. Call it.
- Filter before anything else sees candidates. A cache, re-ranker, or
  LLM call that sees other patients' chunks "before filtering" is a
  leak.
- `reference`, `unassigned`, `unknown` and `ref` are reserved and can
  never be patient ids (`RESERVED_PATIENT_IDS`). Records with a missing
  or reserved id are skipped at ingestion, not pooled under a
  placeholder.
- Only `chunk_type == "guideline"` chunks tagged `reference` are shared.
- A citation of another patient's chunk is REDACTED, even if NLI says
  it entails the claim.
- Never build anything that spans patients: cross-patient summaries,
  entity graphs, clustering, or "similar cases" lookups.

## 2. Fail closed (#2)

- If CCA or HGA is missing, raises, or returns something unexpected, the
  result is `decision="escalate"`, `badge="RED"`, `degraded=True`, with
  `failed_agents` set. Don't substitute a default trust score, a
  placeholder response, or an amber "review".
- An unknown decision value maps to `escalate`.
- Never show a response that wasn't verified (`verified=False`) as if
  it were.

## 3. No shared mutable state per request (#2)

- Per-query settings, such as guardrail thresholds, go in as arguments
  (`hga.verify(..., pass_threshold=...)`). Never set them on a shared
  object and "restore" them afterwards. That breaks on the first
  exception and on concurrent requests.
- Assume every agent object is shared by concurrent requests.

## 4. Feedback can't change the system without review (#3)

- A correction is queued (`status="pending_review"`) and changes nothing
  until a second, different clinician approves it
  (`approve_correction`). Only then does it touch the KB, the persona
  vector or the trust weights.
- Approved corrections are PII-masked, scoped to the query's patient,
  and record `submitted_by` and `approved_by`.
- Anonymous corrections can't be approved.

## 5. Identity comes from core's token, never from the request

- User ids come from core's verified request token: the worker checks its
  signature, audience, expiry and replay, and reads the user and hospital
  from it. Never accept a `clinician_id` or `user_id` from a request body
  or job payload.
- Compare identities with `_normalize_user()`. Placeholder ids
  (`unknown`, `system`, empty) are "nobody", and a feedback or approval
  with no real user is refused.
- `DynamicOrchestratorAgent` does no access control. Who may do what is
  core's decision (the operations table); the worker serves only the
  operations in `boundary/operations.json`, and reads the database through
  the hospital-scoped helper (`with_hospital`).
- Ids that clients can address (query ids, correction ids) are random,
  never sequential.

## 6. Don't assume model contracts, read them

- NLI label order comes from the model's `id2label` via
  `intelligence.utils.nli.nli_label_map()`. Never hard-code an index-to-label
  map. A wrong map turns "unsupported" into "entailed", which disables
  the main hallucination check without any error.
- Check that a model id exists before using it as a default.
- After changing a model, run `scripts/e2e_models.py` and make sure the
  "NLI labels" checks pass.

## 7. Patient data and secrets

- Use synthetic or anonymised data only, outside production. The end-to-end
  script generates its own patients.
- No PHI in logs, exception messages, metrics or test fixtures. Log
  hashes or ids, not query text.
- Run `PIIMasker` on any free text before it's stored or shared.
- Secrets go in `.env` (never committed) or a secrets manager. Compose
  binds every port to `127.0.0.1`. Keep it that way.

## 8. Every security fix comes with a test that fails first

- Put it in `tests/security/`. Show it fails on the code before your
  fix, and check that it isn't passing by accident. (Two early
  threshold tests did: the old code mutated and restored the value,
  and the test read the restored value.)
- Behaviour that only shows up with real models (NLI labels, the trust
  score, Ollama) belongs in `scripts/e2e_models.py`. Run it with
  synthetic data, on a machine that can load the models, before releasing.

---

## Review checklist

- [ ] Does any new retrieval, cache, prompt or index path take and
      honour `patient_id`?
- [ ] Could any new id value collide with a reserved one?
- [ ] On every failure path, does the result escalate rather than
      invent a value?
- [ ] Is any per-request value written onto a shared object?
- [ ] Does any user id come from somewhere other than the session?
- [ ] Can feedback or an admin action change the KB, weights or
      thresholds without a second person and an audit entry?
- [ ] Is any model output interpreted with an assumed label order or
      format?
- [ ] Could PHI end up in logs, errors, fixtures or a shared store?
- [ ] Is there a `tests/security` test that fails without the change?

## Known open gaps

Check these before relying on the system:

- **The trust score can pass a claim no source supports.** NLI carries
  35% of the score, so one fully unsupported claim can still score
  above 0.55 on coherence, numerical and entropy alone.
- **There is no API gateway yet.** JWT auth, route-level RBAC and rate
  limits described in the integration guide don't exist in code.
- **The KB lives in memory.** Approved corrections reach retrieval only
  after `index_kb()` is run again.

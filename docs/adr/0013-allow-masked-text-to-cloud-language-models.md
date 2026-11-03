# 0013. Allow masked text to go to cloud language models, per hospital, off by default

- **Status:** Accepted 2026-11-03 by the product owner ("approve both as recommended"; PR #85)
- **Date:** 2026-11-03
- **Deciders:** product owner
- **Amends:** the intelligence boundary design (`docs/specs/2026-10-28-intelligence-boundary-design.md`) §8.2 "Data in messages" and §13.4 "Data handling"; the document pipeline design (`docs/specs/2026-10-28-document-pipeline-and-model-promotion-design.md`) §2 and §6.1; the performance design (`docs/specs/2026-10-27-intelligence-performance-design.md`) §4.2, which those two cite.
- **Design:** `docs/specs/2026-11-03-cloud-model-providers-design.md`

## Context

Every approved design so far keeps patient data on our own servers:

- the performance design §4.2: "hosted inference APIs are not used for patient data until a data processing agreement is in place";
- the document pipeline design §2 and §6.1: "Only self-hosted, open-weight models are used. No PHI goes to any outside API, hosted inference or third-party GPU marketplace";
- the intelligence boundary design §8.2 and §13.4: patient data is read only under RLS inside our network, and model services hold no credentials.

On 3 Nov 2026 the product owner asked to "make sure everything is working" and added: "we could even add support for using cloud SOTA models through APIs like GPT, Claude, Grok". The demo has no GPU host for the language model today (`deploy/compose.yml` leaves the pipeline commented out), and the hosted frontier models are stronger than the self-hosted Qwen2.5-7B. The product owner approved going ahead under the conditions in the decision below.

## Decision

The rule "self-hosted only, no PHI to outside APIs" is **replaced** by this narrower rule, for clinical questions (`run_query`) only:

1. **Only masked text goes to a cloud model:** the question and the patient fields the prompt uses, each after PII masking (`ModelClient.mask`), and the text of shared guideline chunks selected by local retrieval. Nothing else: no ids, no unmasked field, no metadata that names a hospital, user or patient.
2. **Document images never go to a cloud model.** The document pipeline stays self-hosted; its rules in §2 and §6.1 of its design stand for documents.
3. **Off by default, opt-in per hospital,** set by a platform admin (MFA and step-up), stored in core and audited. The worker reads it from the claimed row, never from a message.
4. **Every answer records** the provider and model as `provider/model@version` in `model_versions`, and the audit fact says whether the answer left our network.
5. **Only providers whose terms give no retention and no training** for our account (zero data retention approved and recorded), with API keys held as server-side secrets on the pipeline host only.
6. **Nothing else changes for safety:** the same guardrails, local verification (NLI and HGA) and two-person correction review apply. A cloud failure, refusal or malformed answer escalates; it never passes and never gets an invented score.
7. **Providers:** OpenAI, Anthropic and xAI, one adapter each, chosen by setting.
8. **Before any real patient data,** the hospitals' data agreements and Nigeria's Data Protection Act 2023 (cross-border transfer and processor terms) must be checked and signed off, and the PII masker must pass the recall bar in the design (§13.6). Until then the demo uses synthetic data only, as everywhere else.

Verification stays local: a cloud model does not verify its own answer.

## Consequences

- The demo can answer questions without a GPU host: the pipeline needs only the CPU embedding and NLI models.
- Masking becomes a hard security control rather than a defence in depth. Today's `PIIMasker` is regex-based and US-centred (it misses Nigerian phone numbers, NINs and names without a title), so real data waits for the masker upgrade (design §13.6).
- The pipeline worker gains a route out of the network, through an egress proxy that allows only the enabled providers' hosts. A compromised worker could now send data out to those hosts; the proxy, the masking gate and the no-retention terms are what limit that.
- New cost and a new outside dependency: per-hospital token budgets, and escalations when a provider is down.
- More to audit: a per-hospital policy table, a new platform action, and two new closed keys on `query.completed`.

## Alternatives considered

- **Keep self-hosted only.** Safest, but leaves the demo without a working language model until a GPU host exists, and declines the product owner's request.
- **Send unmasked data under a data processing agreement.** The performance design allowed this once a DPA exists. Rejected for now: masking costs little, and NDPA cross-border rules are not yet checked.
- **A separate cloud-gateway model service with no database access** (the pipeline sends it masked text over NATS). Better isolation: the only process with a route out would hold no database credentials. Not chosen for the first build, because the pipeline can't mint the request token a model service needs (only core signs tokens, boundary design §7.2); recorded in the design as the next step if the egress risk needs it (§16, question 6).
- **A gateway or aggregator (for example a hosted router) in front of the three providers.** Adds a fourth processor that sees the text, with its own retention terms. Rejected.

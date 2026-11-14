# Model intake: `gemma4-e4b-cca-v1` (14 Nov 2026)

This is the first trained bundle from the AI researcher, and the record of what has to happen before it can serve a hospital. It was checked by listing the zip and reading `report.md` only. Nothing in the bundle was loaded or run.

- **Source:** the researcher's Drive, `MHAI_outputs/gemma4-e4b-cca-v1/bundle`, downloaded as `bundle-20261114T131446Z-1-001.zip` (1.5 GB, 144 files), with `report.md`.
- **Trained:** on an NVIDIA A100 80 GB, in 8.77 h.

## What the bundle holds

| Part | What it is | Use in MHAI |
|---|---|---|
| `models/cca/adapter/` | A QLoRA adapter (r=16, alpha=32, 140 MB) on **`google/gemma-4-E4B-it`**, with its own tokenizer and chat template. The base model is **not** in the bundle. | The answer model (`cca` role), once merged into the base and converted to GGUF. |
| `kb/` | 246,127 chunks with dense (FAISS) and BM25 indexes: MedlinePlus, drug labels, literature, clinical trials, ontology terms, phenotype profiles, Q&A references, and Synthea synthetic patient records. | A possible guideline source, after a licence review per source. |
| `models/embedder/`, `models/nli/` | The researcher's retriever (BERT) and NLI (DeBERTa-v2) models. | Not used. Our gate and worker use the pinned models in `models/answer-gate-models.lock`. |
| `models/qda/*` | Triage models (`.pt`, `.joblib`). | Not used, and not loaded: pickle files can run code when loaded. |
| `models/persona/` | A persona-vector guardrail. | Not used yet. |
| `backend/`, `mhai_src/`, `config/` | The researcher's own backend and configuration, including `backend/.env.backend`. | Not used; the `.env` file is not opened. |

## Reported results (`report.md`, the researcher's own evaluation)

**Medical multiple choice (held out, 300 items):**
- Overall: 23.0% for the base model, 69.3% fine-tuned.
- symptom_to_diagnosis: 0% → 89.3%.
- MedQA: 9.3% → 65.3%.
- MedMCQA: 14.7% → 53.3%.
- PubMedQA: 68.0% → 69.3%.

**Grounded answering (held out), base → fine-tuned:**
- *Cited answers:* answers cite 96.7% → 100%; valid citations 95.8% → 100%; NLI contradicted 1.6% → 1.1%.
- *Should abstain:* it abstains 72.5% → 97.5%.
- *Patient-record answers:* citations fell from 100% to **0%**. Fine-tuning lost them, and this needs the researcher's attention.
- *NLI pass rate on the researcher's measure:* 34.7% → **32.7%**. Our answer gate's pass mark is 70% grounded, so the model may well fail our gate. It is scored with our own NLI model, so the number will differ.

**Researcher's end-to-end test:** 5 queries; 4 escalated, 1 regenerated.

**The report's own caveats:** Synthea and the malaria data are synthetic, this is a demo system and not for clinical use, and the hallucination calibration used injected examples.

## Path to serving (the own-model plan, `docs/plans/2026-11-13-own-answer-model.md`)

1. Download `google/gemma-4-E4B-it` on the build server, merge the adapter, and export to GGUF with the export step (`docs/models/export-step.md`). Pinned llama.cpp must support Gemma 4; check this first.
2. Register the training datasets and the licence by PR.
3. Promote: a dry run, then the real run in the model-promote workflow. The `clinical_answer` gate scores the GGUF itself, and the model is pinned only if it passes.
4. On the demo host: `enable-ownmodel.sh`, then `eval-local.sh` (PASS needed), then switch a hospital with `core hospitals model-policy set <CODE> --provider none`. Switching back to cloud is one command.

**Sizing:** a 4-bit E4B GGUF needs a host with 8 GB or more. The demo host (3.7 GiB) can't run it next to the pilot.

## Decisions needed (product owner)

1. **A Hugging Face token** with Gemma access, because Gemma downloads are gated behind Google's terms. Place it on the build server as a file, never in chat or a repo.
2. **The Gemma licence:** Google's Gemma Terms of Use, not open source. Adding it to `models/licences.json` needs approval.
3. **Training data policy:** phase 0 admits synthetic datasets only. This model also trained on public, non-patient question sets: MedQA, MedMCQA, PubMedQA and symptom_to_diagnosis. Proposal: a "public, no patient data" category.
4. **If the gate fails:**
   - Recommended: pilot on a cloud model meanwhile, and the researcher retrains, especially to restore patient-record citations.
   - Alternative: a clinician lowers the grounding pass mark for a supervised pilot.
   - Either way, escalation recall 1.0 and unsafe rate 0 never change.

## Checks under way

- Pinned llama.cpp (`b1b770d9`): does it support the Gemma 4 architecture? If not, re-pin it (gate and sidecar together).
- Knowledge base as a guideline source: the licence of each source. Some ontology sources, such as SNOMED CT, are restricted. Synthea patient records must never be loaded into a hospital's knowledge base.

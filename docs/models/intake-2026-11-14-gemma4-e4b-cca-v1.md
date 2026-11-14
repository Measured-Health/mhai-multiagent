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

## Decisions needed (product owner): answered in "Decisions made" below

1. **A Hugging Face token** with Gemma access, because Gemma downloads are gated behind Google's terms. Place it on the build server as a file, never in chat or a repo.
2. **The Gemma licence:** Google's Gemma Terms of Use, not open source. Adding it to `models/licences.json` needs approval.
3. **Training data policy:** phase 0 admits synthetic datasets only. This model also trained on public, non-patient question sets: MedQA, MedMCQA, PubMedQA and symptom_to_diagnosis. Proposal: a "public, no patient data" category.
4. **If the gate fails:**
   - Recommended: pilot on a cloud model meanwhile, and the researcher retrains, especially to restore patient-record citations.
   - Alternative: a clinician lowers the grounding pass mark for a supervised pilot.
   - Either way, escalation recall 1.0 and unsafe rate 0 never change.

## Decisions made (14 Nov 2026, delegated by the product owner)

The product owner delegated these decisions on 14 Nov 2026 ("just do everything"). Each was made the safe way, and is recorded here.

1. **No Hugging Face token is needed.** `google/gemma-4-E4B-it` is not gated (the Hub API reports `gated: false`). The base is pinned at commit `4ecc2ba63bcac23ba4a767fc87025b1450f84c9c`, last changed on 3 Nov 2026, before the 13 Nov training run, so it is the revision the adapter was trained on. The bundle did not record the revision; the researcher should record it in future runs.
2. **The licence is Apache-2.0, so `models/licences.json` is unchanged.** Gemma 4 is released under Apache License 2.0 (the model card's licence, and Google's Gemma 4 licence page), not the older Gemma Terms of Use. Apache-2.0 is already allowed. Adding the Gemma Terms of Use would widen the allow-list for no model we have, so it was not added. A future Gemma 2 or 3 base would need it, by PR.
3. **A new dataset kind, `public`: published data with no patient data.** It is admitted with `synthetic`; `deidentified` (real patients) is still refused (`dataset_not_synthetic`). A `public` entry has no generator.
4. **The training sets, registered in `models/approved-datasets.json`.** `bundle/manifest.json` lists only the bundle's files, so the names come from the run's `report.md` (its training mix and held-out sets). Version `1.0.0` means "as used for `gemma4-e4b-cca-v1`"; the bundle pins no upstream dataset revisions.

   | Dataset | Kind | What it is |
   |---|---|---|
   | `medqa-usmle-4-options@1.0.0` | public | `GBaker/MedQA-USMLE-4-options`: exam questions |
   | `medmcqa@1.0.0` | public | `openlifescienceai/medmcqa`: exam questions |
   | `pubmedqa-pqa-labeled@1.0.0` | public | `qiaojin/PubMedQA`, `pqa_labeled`: literature questions |
   | `symptom-to-diagnosis@1.0.0` | public | `gretelai/symptom_to_diagnosis`: generated symptom descriptions |
   | `cca-v1-rag-cited@1.0.0` | public | The run's cited answers, built over the knowledge base's public sources |
   | `cca-v1-abstain@1.0.0` | public | The run's should-abstain questions |
   | `cca-v1-patient-cited@1.0.0` | synthetic (`synthea`) | The run's patient-record answers, over Synthea synthetic patients |

   All are marked disjoint from the locked test split: the `clinical_answer` questions come from our own bank (`doceval/answers/bank.py`), and the locked split is drawn inside the store. Caveat: the knowledge base holds 6 guideline chunks, which may be our six reference guidelines. The researcher should confirm that no question from our bank was used in training.
5. **Still open before serving a hospital (not before the gate):** the licence of each knowledge-base source behind `cca-v1-rag-cited` (some ontology sources, such as SNOMED CT, are restricted).

## Export and gate (14 Nov 2026, build server, dev split)

- **Merge and export:** the adapter was merged into the base with PEFT (`merge_and_unload`), and exported with `mhai-models export` (role `cca`, `q4_k_m`, llama.cpp `b1b770d9`). The export passed.
  - The GGUF is `model-q4_k_m.gguf`: 5,302,268,608 bytes, SHA-256 `8b2a145151b7509a6bba355909e1a791f0e89f3357bcdf1beec8f00ba49046de`.
  - The run's files hash is `0cd7197b2bea372073a98ebcbb4224c6848dbb1a1fa29b18d313f2e1ee0ce4fa`.
- **Blocks promotion:** the merged safetensors cannot be uploaded to the store. Gemma 4's per-layer embedding is one 5.6 GB tensor, over the store's 5 GiB single-upload cap (`too_large`, `file_too_large_for_store`). Promotion needs the store's conditional multipart upload (TODO Task 27). The export step now refuses this early (#178).
- **The `clinical_answer` gate fails** on the dev split: 76 items, 36 must escalate. It was scored through the promotion dry run, with the cap lifted in that process only.

  | Metric | Value | Pass mark | Verdict |
  |---|---|---|---|
  | Escalation recall | 0.056 (2 of 36) | 1.0 | fail, short by 0.944 |
  | Unsafe rate | 0 | 0 | pass |
  | Grounded pass rate | 0.000 | at least 0.70 | fail, short by 0.70 |
  | Fact coverage | 1.000 | at least 0.70 | pass |
  | Over-escalation | 0.050 | at most 0.30 | pass |

  No locked split exists yet (`models/testsets.lock` is empty).

  The model answers but almost never escalates. No answerable item reached a `PASS` decision either: 36 of 38 got neither `PASS` nor `ESCALATE`.

  Per decision 4 (recommended): pilot on a cloud model meanwhile, and the researcher retrains for escalation and grounding.

## Checks under way

- Pinned llama.cpp (`b1b770d9`): supports the Gemma 4 architecture, and `files.py` allows `gemma4` (#171).
- Knowledge base as a guideline source: the licence of each source. Some ontology sources, such as SNOMED CT, are restricted. Synthea patient records must never be loaded into a hospital's knowledge base.

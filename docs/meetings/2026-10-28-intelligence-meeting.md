# Intelligence meeting notes: 28 Oct 2026

The product owner met the intelligence lead, who works on the Python intelligence service. These notes are context, not decisions to act on yet.

## What was said

- **Role.** The intelligence lead's job is the internals of the intelligence service: making sure the models and the clinical pipeline work well. The engineering around it stays with the platform team: NATS, tokens, the worker runtime, database access, deploys and security.
- **Training.** He will create notebooks that train and fine-tune the models, and save their weights to Google Drive. Production will use those weights.
- **Document translating pipeline.** It takes scanned documents and records from hospitals as input and turns them into a structured format. We need a model that can do this.

## How it fits the current architecture (for reference)

- **The seam** for his work is `ModelClient` in `services/intelligence/intelligence/boundary/models.py`, with `mask`, `generate`, `verify` and `model_versions`. The job runner calls the pipeline only through it.
- **Document pipeline.** The design already reserves the `ingest_document` job. The planned documents service (`services/documents/`) handles uploads and storage. Extracted data would use the same FHIR-based canonical model as the EMR feed (ADR 0009).
- **Model serving** is a planned service, from the parked performance plan.

## Suggestions to discuss (not agreed)

1. **Weights to production.** Keep notebooks and Drive for research, and add a "promote" step: copy a chosen version into our own model store, with its SHA-256, evaluation results, a model card and a version number. Production loads only a pinned version and checks the hash. A new version goes live only after its evaluation passes and it is clinically signed off. Training data is synthetic, or de-identified with approval; no patient data goes on Drive.
2. **Document pipeline steps:**
   1. upload, with a virus scan and per-hospital storage;
   2. OCR and layout;
   3. a vision-language model fills a fixed schema, with a confidence score per field;
   4. normalise to the canonical model (LOINC, SNOMED, units), keeping where on the page each field came from;
   5. low-confidence fields go to a person for review, and nothing is guessed silently.
3. **Model choice.** A self-hosted, open-weight vision-language model, so patient documents never leave our servers. The candidates to test are the Qwen-VL family (the same family as the Qwen-7B already used) and document OCR models such as PaddleOCR-VL, olmOCR or Docling. Compare 2–3 of them on synthetic scanned records with known answers (field accuracy, handwriting, speed), then fine-tune the best one.

Next steps: approved by the product owner on 28 Oct: a short design for the document pipeline and the model promotion flow, and entries in `TODO.md` and the tracker. The design is [docs/specs/2026-10-28-document-pipeline-and-model-promotion-design.md](../specs/2026-10-28-document-pipeline-and-model-promotion-design.md).

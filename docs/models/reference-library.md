# Reference library v1 (`medref`)

A shared, licence-clean reference corpus that every hospital's questions can
cite (migration 00040; `intelligence/knowledge_base/reference_library.py`).
It fills the pilot's guideline gap until hospital and national guidelines
are added.

## What is in it

| Kind | Source | Licence | Rows kept |
|---|---|---|---|
| `drug_label` | openFDA drug labels (links to DailyMed) | CC0-1.0 | the nine label sections below |
| `health_topic` | MedlinePlus health topics | public domain (U.S. government work) | English only, labelled patient-facing |

Drug label sections, in id order (safety-critical first): boxed warning,
contraindications, warnings and precautions, warnings, drug interactions,
use in specific populations, adverse reactions; then dosage and
administration, indications and usage.

Nothing else from a source bundle is used: no synthetic patient rows of any
type, no question-answer references, no PubMed or Europe PMC abstracts, no
phenotype profiles, ontology terms or trials, and not the six placeholder
guidelines. The converter refuses them by type; the tests pin it.

Each row is a shared guideline (`chunk_type` `guideline`, no hospital, no
patient, no source fields) with an id from `KB#1000001` up. Its metadata
holds `kind`, `source`, `title`, `url`, `licence`, `set_id` (labels) or
`topic_id` (topics), `section`, `safety_critical`, `audience`, `retrieved`
(the date the sources were fetched) and `attribution`. Each row carries its
vector from **our** pinned embedder (role `dense` of
`models/answer-gate-models.lock`), never the source bundle's.

## Attribution

Shown with the library and carried on every row:

- Source: MedlinePlus, National Library of Medicine.
- Data provided by the U.S. Food and Drug Administration (open.fda.gov).
- Provided as is, without warranty of any kind. FDA and NLM do not endorse
  MHAI. Not a substitute for the full prescribing information or clinical
  judgement.

## Where the artefact lives

The artefact is about 36 MB, so it is **not committed**. Its SHA-256, version
and row count are pinned in `models/reference-library.lock`, and `load`
refuses any other bytes.

- `medref-1.0.0.jsonl.gz` (SHA-256 `d150e380…06e7`, 23,654 rows): on the build server (steven-reid), in
  `~/mhai-reference-library/`. Copy it from there to the machine that runs the
  load; check it with `verify` first.

The repository keeps only a small synthetic fixture in the source bundle's
shape (`services/intelligence/tests/fixtures/reflib/chunks.jsonl`) for the
tests.

## Build a version (operator, build machine)

Never on a laptop: embedding is heavy. The converter reads one member of the
bundle, `bundle/kb/chunks.jsonl.gz`, as a stream; it extracts nothing and
opens nothing else in the bundle.

```bash
cd services/intelligence
uv run python scripts/reflib_convert.py \
    --input /data/bundle-20261114T131446Z-1-001.zip --retrieved 2026-11-14 \
    --version 1.0.0 --models /data/models --fetch --out /data/out
```

It prints the approved counts, the artefact's path, row count and SHA-256.
Pin those in `models/reference-library.lock` by pull request.

## Load it (operator)

With the migration DSN (the role that runs `core migrate up`; the worker can't
write `kb_chunks`):

```bash
cd services/intelligence
uv run python -m intelligence.knowledge_base.reference_library verify medref-1.0.0.jsonl.gz
MHAI_REFLIB_DATABASE_URL=postgres://... \
  uv run python scripts/reflib_load.py medref-1.0.0.jsonl.gz
```

One transaction: the library row, every row (COPY), the previous version of
`medref` retired and its rows deleted, the new version active. Loading the
active version again changes nothing. A version already loaded with other
bytes is refused. To roll back, load the earlier pinned artefact.

## How a question uses it

The worker never reads the whole library. For each question, Postgres
pre-filters the active library's rows by full-text search on the masked
question and returns the best 100; the hospital's own rows and the seeded
guidelines are read by their own queries, under their own bounds, so the
library can't crowd them out (`intelligence/boundary/kb_repo.py`).

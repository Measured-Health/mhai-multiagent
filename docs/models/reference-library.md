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

**What it is not.** These are reference texts (drug label sections and
patient-facing topic summaries), not clinical practice guidelines. They tell
the answer model what a label says, not how a Nigerian hospital should
manage a condition. Real guidelines for the pilot hospital (WHO, the Nigerian national
guidelines, the hospital's own protocols) still have to be sourced and
licence-checked (TODO.md, "Shared reference library"). The clinical safety
lead signs off the library before it is used with real patients.

### The bundle of 13 Nov, row by row (decided 23 Nov)

The AI researcher's knowledge base (246,127 rows) is untrusted input: it is
read as JSON, never run. `scripts/build_reference_subset.py` applies the
allow-list below and prints these counts:

| Rows in the bundle | Decision | Why |
|---|---|---|
| `drug_label` / openFDA, 20,739 | **kept: 20,295** | US government data, CC0 on openFDA. 444 left out by the row checks: a section outside the nine above, a link that isn't DailyMed's, or text that is too long or holds a citation or delimiter. |
| `health_topic` / MedlinePlus, 6,984 | **kept: 3,359** | Health topic summaries are written by NLM and are public domain. MedlinePlus also hosts copyrighted content (the A.D.A.M. encyclopedia, drug and herb monographs); none of it is in these rows, which are the `medlineplus.gov/<topic>.html` summaries only. 3,625 left out by the row checks: the Spanish pages, and rows with no valid topic id, title or part. |
| `qa_reference` / MedQuAD, 31,543 | left out (could come in a later version) | CC BY 4.0, so attribution is possible, but the rows have no upstream URL ("see dataset card"), so no row could cite its NIH page. The answers were crawled in 2017 and some source sites have since closed (Genetics Home Reference, NIHSeniorHealth). Question-and-answer text is not guidance and mostly repeats MedlinePlus. |
| `qa_reference` / wikidoc and wikidoc_patient, 22,466 | left out | CC BY-SA 3.0: share-alike would bind what we build from it. Wiki text with no editorial review we can point to. |
| `ontology_term` (MONDO, HPO, DOID), 47,126; `phenotype_profile` / HPO, 12,868 | left out | Terms and definitions, not guidance. HPO has its own licence terms. Useful later for coding, not for answering. |
| `clinical_trial` / ClinicalTrials.gov, 25,576 | left out (later decision) | Trial registrations describe studies, not care. |
| `literature` / PubMed 16,667 and Europe PMC 13,345 | left out (later decision) | Abstracts are the publishers' copyright, and a single study is not guidance. |
| Synthea patient rows (`demographics`, `encounter`, `diagnosis`, `lab`, `medication`, `plan`), 48,807 | **refused** | Synthetic patients. A shared row is never about a patient. The converter refuses every patient type by name, and any row whose source fields hold something other than its source's name. The loader refuses any row with a field outside a closed list. |
| `guideline`, 6 | **refused** | The public demo's six synthetic samples. Nothing that names the demo's source ("Sample reference (synthetic demo)") is ever converted or loaded. |

Kept in all: **23,654 rows**, the same as the pinned `medref` 1.0.0.

### Review copy (owner and clinical safety lead)

Before a library is approved, the people signing it off read the rows as
text, without vectors:

```bash
cd services/intelligence
uv run python scripts/build_reference_subset.py \
    --input /data/chunks.jsonl.gz --retrieved 2026-11-14 --out /data/medref-subset.jsonl.gz
```

Each line holds `content`, `kind`, `source`, `title`, `licence`,
`upstream_id` (the label's set id or the topic id), `url`, `attribution`,
and `section` or `part`. The same input always gives the same bytes. The
bundle of 13 Nov gives SHA-256 `b9b4b7f8…f385` (23,654 rows).

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

## Load it on the live site (operator, on the host)

Use `core reference-library load` from `core-cli` (deploy/README.md, "Live
site: the shared reference library"). It runs as the provisioning login
(migration 00056: library rows only), checks the bytes against the pin
compiled into core (kept equal to `models/reference-library.lock` by a
test), refuses any patient-shaped or demo row, and audits
`reference_library.loaded`. The vectors are already in the artefact,
made on the build server with our pinned embedder, so nothing is embedded
on the host and no GPU is needed.

## Load it from a checkout (development)

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

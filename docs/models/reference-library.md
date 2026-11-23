# Reference libraries (`medref`, `local-formulary`, guideline documents)

Shared, licence-checked reference corpora that every hospital's questions can
cite (migration 00040; `intelligence/knowledge_base/reference_library.py`).
Several libraries can be active at once, one version of each name:

| Library | What | Id block | Kinds (source) | Status |
|---|---|---|---|---|
| `medref` | openFDA drug labels, MedlinePlus health topics | `KB#1nnnnnn` | `drug_label` (openFDA), `health_topic` (MedlinePlus) | 1.0.0 pinned, loaded on the live site |
| `local-formulary` | the pilot site's brand and local drug names | `KB#2nnnnnn` | `local_formulary` (Measured Health) | 1.0.0 pinned (34 rows) |
| `who-malaria` | WHO guidelines for malaria | `KB#3nnnnnn` | `guideline` (World Health Organization) | built, **not pinned: licence needs an owner decision** |
| `ng-malaria` | Nigeria national malaria guidelines | `KB#4nnnnnn` | `guideline` (Federal Ministry of Health, Nigeria) | built, **not pinned: licence needs an owner decision** |
| `ng-stg` | Nigeria standard treatment guidelines | `KB#5nnnnnn` | `guideline` (Federal Ministry of Health, Nigeria) | no official copy found; not built |

Each library has its own block of chunk ids, so libraries never collide in
`kb_chunks`, and its own approved kinds and sources. Both are checked row by
row when an artefact is built and again when it is loaded, by the Python
loader and by `core reference-library load`. The table is
`reference_library.LIBRARIES` in Python and `reflibLibraries` in core, kept
equal by a test on each side. A new library is added to both in one pull
request.

## `medref`: what is in it

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
manage a condition. That is what the guideline libraries below are for, once
their licences are decided; the hospital's own protocols are still to come
(TODO.md, "Shared reference library"). The clinical safety
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

## Where `medref`'s artefact lives

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

The formulary and the guideline documents (same machine, same embedder):

```bash
cd services/intelligence
uv run python scripts/reflib_formulary.py --version 1.0.0 --retrieved 2026-11-23 \
    --models /data/models --fetch --out /data/out
uv run python scripts/reflib_guideline.py --document who-malaria \
    --pdf /data/guidelines-src/who-malaria-2026-09-10.pdf --models /data/models --out /data/out
uv run python scripts/reflib_guideline.py --document ng-malaria --dry-run \
    --pdf /data/guidelines-src/ng-malaria-2020-05.pdf   # counts only, no embedding
```

A pinned artefact goes in `models/reference-library.lock` **and** core's
`reflibPins` (`services/core/cmd/core/reflib.go`) in the same pull request;
`TestReflibPinsMatchLock` fails if they differ. A guideline's artefact
SHA-256 and count also go in its `models/guideline-documents.lock` entry.

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

The worker never reads a whole library. For each question
(`intelligence/boundary/kb_repo.py`):

1. **Candidates, in turns.** Postgres pre-filters the active libraries'
   rows by full-text search on the masked question (any of its words,
   `ts_rank`), ranks each library's matches on their own, and returns at
   most 100 rows in turns: each library's best match, then each library's
   second best, and so on. A small library (34 formulary rows, a few hundred
   guideline sections) is never crowded out by `medref`'s 23,654 rows.
2. **Generic expansion.** If a candidate is a `local_formulary` row (the
   question named a brand, "Lonart"), its generic name
   (`artemether-lumefantrine`, from the pinned row's `generic` field, never
   from the question) is searched as well, every word of it required, for at
   most 20 more rows outside the formulary: that drug's openFDA label
   sections and the guideline's dosing section. At most three generics per
   question.
3. The hospital's own rows and the seeded guidelines are read by their own
   queries, under their own bounds.

The pipeline log line "Indexing N KB chunks" counts all of these. The
vectors come with the rows, so nothing is embedded per question.

**Known limit.** The question is masked before the pre-filter
(`security/hipaa.py`). A brand name that the masker's name gazetteer takes
for a person's name would be masked and not found. If clinicians report a
brand that never retrieves, check the masker first.

## Local formulary (`local-formulary`)

**Why.** Clinicians at the pilot site write brand and local names
("Lonart", "Camosunate", "PCM", "MMT"). The other libraries know generic
names, so without this a question that names a brand retrieves nothing about
the drug.

**Input.** `models/reference/local-formulary.csv` (columns
`brand_or_local_name, generic, class, typical_use, confidence, notes`): drug
names only, no patient data. 48 rows on 23 Nov 2026. The converter
(`intelligence/knowledge_base/local_formulary.py`) keeps only rows of
confidence `high` or `medium`, which gives 34 rows. The 14 `low` rows (the
cough syrups, Astyfer, Hostan, Floranorm and others with an unknown or
"possibly" generic) are left out until a clinician confirms them (TODO.md).

**Row text.** One reference sentence per brand, for example:

> Lonart is a local brand or name used in Nigeria for
> artemether-lumefantrine 20/120 mg. Class: artemisinin-based combination
> therapy (ACT). Typical use: uncomplicated falciparum malaria. Local
> formulary entry: confirm the product and the dose against the pack and the
> national guidelines.

The typical use is kept as written in the CSV, so cautions stay: Maldox
"not first-line for uncomplicated malaria", Chloroquine "not recommended for
falciparum malaria in Nigeria (resistance)". The `notes` column is curation
notes and is not used. Metadata: `brand`, `generic` (dose removed, for the
expansion), `drug_class`, `confidence`, `title`, `licence`, `attribution`.

**Licence.** Our own curation, released as CC0-1.0 (source name
"Measured Health").

**Adding the low-confidence rows later.** A clinician confirms the generic,
class and use. Edit the CSV row, set `confidence` to `high` or `medium`,
then bump the version (1.1.0), rebuild on the build server
(`scripts/reflib_formulary.py`), pin the new SHA-256 and count in
`models/reference-library.lock` and core's `reflibPins` in one pull request,
deploy, and load it (deploy/README.md, "Adding reference libraries"). The
load replaces 1.0.0's rows in one transaction.

## Guideline documents (`who-malaria`, `ng-malaria`, `ng-stg`)

Each document is an **opt-in entry** of `models/guideline-documents.lock`:
title, publisher, edition, the exact URL it was fetched from, the PDF's
SHA-256, the page range used, the licence and its text as found, and a
**licence status**:

- `permitted`: the licence allows our use; it may be pinned as loadable;
- `needs_owner_decision`: built and kept ready on the build server, **not
  pinned**;
- `unknown`: no licence read (no official copy found).

A test refuses a guideline library in `models/reference-library.lock` whose
status is not `permitted`. Pinning one is the owner's decision, made in a
pull request that changes its status and adds its pin.

| Document | Edition | URL fetched (23 Nov 2026) | PDF SHA-256 | Licence as found | Status | Artefact |
|---|---|---|---|---|---|---|
| WHO guidelines for malaria | 10 September 2026 (494 pages; pages 5-271 used) | https://iris.who.int/server/api/core/bitstreams/d3c6488b-3ee6-41cb-960f-ddf0202cd74d/content (from https://www.who.int/publications/i/item/guidelines-for-malaria) | `4e2c67b2…de43b` | CC BY-NC-SA 3.0 IGO: non-commercial, share-alike, no suggestion of WHO endorsement; commercial use needs WHO's permission | **needs owner decision** | `who-malaria-1.0.0`, 455 rows, `2c75f177…8701` |
| National Guidelines for Diagnosis and Treatment of Malaria (NMEP, Federal Ministry of Health) | 4th edition, May 2020 (81 pages; pages 13-75 used) | https://drive.google.com/uc?export=download&id=1pxGgYsCjEWYDQ5-2GM5PSeeWHXww7erK (linked from https://www.nmcp.gov.ng/2020/09/21/4th-edition-national-guidelines-for-the-diagnosis-treatment-2020/) | `bda33638…0732` | "© National Malaria Elimination Programme 2020. All rights reserved." No open licence | **needs owner decision** | `ng-malaria-1.0.0`, 97 rows, `313db956…fb92` |
| Nigeria Standard Treatment Guidelines (Federal Ministry of Health) | 2022 (launched 24 Nov 2022) | none: no official PDF found online. The Ministry's 2008 link serves a 2-page fax print; the WHO CPCD entry for the 2nd edition was unavailable | none | not read | **unknown** | not built |

The full licence text as found, page ranges and notes are in the lock. The
WHO PDF says it may be out of date as soon as the online guideline changes.
Each new edition is a new lock entry (new SHA-256) and a new library version.

**The converter** (`intelligence/knowledge_base/guideline_pdf.py`,
`scripts/reflib_guideline.py`):

- text extraction with pypdf 7.1.1 (pinned in `uv.lock`), page by page over
  the entry's range. No OCR: both PDFs have a text layer;
- clean-up: running headers and footers (a line in a page's first or last
  three lines that is there on at least 40% of pages), bare page numbers and
  table-of-contents lines are dropped;
- section-aware chunks: a numbered heading ("4.2.1 Artemether-Lumefantrine
  (AL)") starts a new chunk; text is packed into about 380 words, never more
  than 520; a run of dose-table lines is kept in one chunk where it fits;
- each row: "<document> (<edition>), section <heading>, pp. a-b: <text>",
  with metadata `document`, `publisher`, `edition`, `section`, `page_start`,
  `page_end`, `url`, `licence`, `attribution`;
- deterministic: the same PDF gives the same bytes. Rebuilt on 23 Nov, the
  same SHA-256 came out.

It refuses a PDF whose bytes are not the lock's, a document with no entry,
any row with a field outside the closed list (nothing patient-shaped), and
text holding a prompt delimiter or citation (replaced at clean-up).

**What a guideline row is not.** Extracted PDF text loses layout. A dose
table becomes a run of numbers in reading order, which the answer model has
to read correctly. The clinical safety lead signs off each guideline library
before it is loaded.

## Where the artefacts live (build server, steven-reid)

`~/mhai-reference-library/`:

| File | Rows | SHA-256 | Pinned |
|---|---|---|---|
| `medref-1.0.0.jsonl.gz` | 23,654 | `d150e380…06e7` | yes |
| `local-formulary-1.0.0.jsonl.gz` | 34 | `081e0d2e…4744` | yes |
| `guidelines/who-malaria-1.0.0.jsonl.gz` | 455 | `2c75f177…8701` | no (licence) |
| `guidelines/ng-malaria-1.0.0.jsonl.gz` | 97 | `313db956…fb92` | no (licence) |
| `guidelines-src/*.pdf` | | the PDFs as fetched | |

All built with our pinned embedder,
`sentence-transformers/all-MiniLM-L6-v2@1110a243fdf4706b3f48f1d95db1a4f5529b4d41`.

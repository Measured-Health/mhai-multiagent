# 0016. A separate documents service, with per-hospital envelope encryption and no storage credential in the pipeline worker

- **Status:** Accepted 2026-11-14 under the product owner's delegation ("just do everything"); the design merged as #182. The calls are listed for later review in the design's §16 (D1–D17).
- **Date:** 2026-11-14
- **Deciders:** product owner (delegated)
- **Extends:** [ADR 0010](0010-api-in-go-models-in-python-over-nats.md) (Go for the API and the rules, Python for models, NATS between them; only core publishes jobs), [ADR 0011](0011-frontend-ingress-over-nats.md) (the BFF reaches core over NATS) and [ADR 0014](0014-separate-emr-service-and-document-route.md) (a separate Go service holds what core shouldn't; scanned records reach the canonical model only after review)
- **Keeps:** [ADR 0013](0013-allow-masked-text-to-cloud-language-models.md) §2: document images never go to a cloud model
- **Amends:** the document pipeline design (`docs/specs/2026-10-28-document-pipeline-and-model-promotion-design.md`) §3.3: the pipeline worker no longer reads `pages/` from object storage; the EMR integration design (`docs/specs/2026-11-07-emr-integration-design.md`) §10.3: the patient route is `/review/documents/{id}/patient`
- **Design:** [`docs/designs/2026-11-14-documents-service.md`](../designs/2026-11-14-documents-service.md) (the build plan is its §13; departures in the build are its §17)

## Context

Three pieces of work wait on one missing service (design §1): clearance letters can't be uploaded outside the demo (core keeps a 64 KiB body limit, NATS a 1 MB payload); document pipeline phase 1 needs uploads, a malware scan, a sandboxed renderer and review; and EMR PR 12 (the pilot hospital's scanned archive) needs somewhere to hand each scan.

The pipeline design named "the documents service" but left it unspecified, and let the pipeline worker read page images from the bucket with a prefix check. Four forces decide the shape:

- **Hostile files.** Every uploaded file is attacker-controlled until proven otherwise; PDF and image parsers are a classic way in.
- **Many hospitals, one bucket.** A leaked bucket credential, a copied object or a restored backup must not be readable across hospitals, and a hospital's data must be destroyable on removal or exclusion.
- **Core's secrets.** Core holds sessions, accounts, the audit chain and the request-signing key. ADR 0014 already moved hospital credentials out of it for the same reason.
- **The worker is the likeliest component to be turned.** It parses model output and talks to the GPU host.

## Decision

1. **A separate Go service, `services/documents/` (its own module and binary, importing `services/gokit/`), holds storage and keys** (D1). It is the only holder of the object store credential and of the hospital key-encryption keys (or the KMS identity). It never imports core or the emr service. Core calls it only on `documents.v1.ctl.<op>` with a request token (`aud: documents`) that it verifies with gokit's `reqtoken`, subject table and replay set, as the emr service verifies `emr.v1.ctl.*`. It has its own NATS user and its own narrow database role (`mhai_documents`), and nothing on the clinical tables, `emr.*`, users, sessions or the audit chain.
2. **Per-hospital envelope encryption** (D4). One key-encryption key (KEK) per hospital; a random data key per object, used with Tink's streaming AEAD and associated data binding the object to its hospital, document, kind and page; the wrapped data key lives in Postgres, never in the object's metadata. Deleting a row is a crypto-shred; destroying a KEK removes a hospital, including from bucket versions and storage backups. Production uses a cloud KMS key per hospital; the pilot may use a file keyring only with the product owner's written acceptance (D5).
3. **The pipeline worker holds no storage credential** (D2). It reads a page only through `documents.v1.<hid>.page`, for a document it holds a live claim on, which the documents service checks in the database.
4. **Two calls are authorised by NATS rights plus database state, not a core token** (D3): `documents.v1.<hid>.intake` (only the `emr` user may publish it; the row must be that hospital's, registered by emr and still `uploading`) and `documents.v1.<hid>.page` (only the `pipeline` user; the row must be that hospital's, in `extracting` or `finalising`, with the caller's live `lease_id`). Only core mints request tokens, and neither caller can get one for these. A forged or stale request gets `not_found` and nothing else.
5. **Hostile files are parsed only in a sandbox** (D7): magic bytes and limits first, then a ClamAV scan, then pdfium or Pillow under nsjail in a separate container with no network and no credential; only re-checked pixels leave it. A broken scanner keeps documents out, never lets them through (D8).
6. **Off by default.** The `documents` compose profile isn't started, `CORE_DOCUMENTS_ENABLED=false` makes every document route 404 (the letter upload keeps its 503), no `DOCS` stream exists, the `documents` NATS user is a no-rights placeholder unless switched on, and the `document_archive` connector type isn't enabled. Live use waits on the gates in TODO.md ("Documents service: people items and live-use gates").

## Consequences

- A compromise of core doesn't hand over storage keys or object store access; a compromise of the worker reads only the pages of the document it is working on, for as long as its lease lasts; a sandbox escape reaches only the sandbox's tmpfs.
- A bucket credential leak, a copied object or a restored storage backup is unreadable without the hospital's KEK and the wrapped key in Postgres.
- One more Go service to build, run and watch, plus two sidecars (`clamav`, `docsandbox`). `services/gokit/` gains the documents control-operations table and a subject rule; it stays security code, reviewed as such.
- Postgres backups keep wrapped keys of purged documents for up to 35 days; the restore runbook re-applies purges (design §6.4).
- The pilot's file keyring keeps every hospital's KEK on one host's disk; accepted for the pilot only, with KMS before a second hospital (design §14.6).
- Quarantine objects are protected only by the provider's encryption, for at most 24 hours (design §5.4, §14.6).

## Security and threat model

The full model is the design's §14: assets, actors and boundaries, STRIDE threats S1–S20, the decided controls and abuse-case tests DS1–DS24 (plus the pipeline design's B1–B27), residual risks and sign-off. In short:

- **Assets:** patient data in originals, pages, extractions, review decisions and archive file names; hospital separation; correct values and patient attachment; the KEKs and the bucket credential; the audit chain.
- **Boundaries:** browser → BFF → core (session, permission, hospital from the session); browser or emr → object store (a POST policy for one key in `quarantine/<hid>/`, a size range, 10 minutes); core → documents (request token `aud: documents`, subject, body hash, expiry, replay set); emr and pipeline → documents (NATS user and database state); documents → clamav and docsandbox (an internal network with no route out; the sandbox has none at all); documents → object store (its credential only, decryption bound to hospital, document, kind and page).
- **Threats this decision closes:** parser exploits reaching keys (S1, S2: no key in the sandbox); a turned worker reading other pages (S5: no credential, a live lease); cross-hospital reads and a leaked bucket (S6, S7: per-hospital KEK, associated data, wrapped keys only in Postgres); platform roles reading documents (S19: no permission, 404 everywhere).
- **Sign-off:** clinical (phase 1 thresholds, the clinician-for-criticals rule), privacy (H5: region, retention values, the file-name hint), legal (H3: the letter template and letter retention) and the product owner (D5, the pilot file keyring). They are gates in TODO.md.

## Alternatives considered

- **Documents inside core:** one service fewer, but core would hold every hospital's storage keys and orchestrate hostile files next to the audit chain and the signing key (the reasoning of ADR 0014).
- **Documents in the Python service:** the security rules are written once, in Go (ADR 0010), and gokit already has request-token verification, the replay set and redacting logs.
- **The worker reads `pages/` with a prefix check (pipeline design §3.3 as written):** the likeliest component to be turned would hold a credential for every hospital's pages.
- **A core-minted token for `intake` and `page`:** the emr service and the worker would need to ask core for one per call; core would gain a grant path for two internal callers, and the database state would still have to be checked.
- **Provider-side encryption only (SSE-KMS with one key):** a leaked bucket credential with decrypt rights reads everything, and deleting one document can't be made complete while bucket versions exist.
- **Presigned PUTs instead of POST policies (D6):** a PUT URL can't enforce a size range.
- **MuPDF or Ghostscript as the renderer (D7):** AGPL, and Ghostscript has a long history of sandbox escapes.

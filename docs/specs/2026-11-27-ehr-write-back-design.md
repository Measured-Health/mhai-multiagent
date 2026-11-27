# Write-back to the hospital's EHR: design

- **Status:** Approved by the product owner on 27 Nov 2026 ("go with the recommendations"): every decision in §13 is the recommended option, plus the off-switch rule in §9. Not built. Before any sending: the clinical lead and legal confirm the attribution wording (Q5), legal drafts the agreement clause (Q6), and the sign-offs H-WB1 to H-WB5. Design only, no code yet.
- **Date:** 2026-11-27
- **Owner's ask (27 Nov):** the EMR/EHR pipeline should be "for pulling data and pushing data to the EHR systems".
- **Companion design:** [EMR context in answers](2026-11-27-emr-context-in-answers-design.md) (the "pulling" half).
- **Changes a decision.** The EMR design is read-only (§2 non-goals) and ADR 0009 rule 9 says: "Writing back to an EMR is out of scope. If it is added later, it goes through the same connector interface with its own approval and ADR." This design is that proposal. If approved, a new ADR (next free number) amends rule 9.
- **Builds on:** the EMR design ([`2026-11-07-emr-integration-design.md`](2026-11-07-emr-integration-design.md)): the connector interface (§4), credentials only in the emr service (§6), the `egress-emr` proxy and its per-connection allow-list (§6.4), the patient index (§10), audit through id-only signals (§3.2); the `hl7v2_file` connector and the on-premises bridge (TODO, "HL7 v2", in progress); ADR 0005 (fail closed), ADR 0010 (rules in Go), ADR 0013 (masked text to cloud models); the support gate.
- **Names.** No hospital or vendor is named. Every example is synthetic.

## 1. Why

A checked answer lives in MHAI today. If the clinician wants it in the patient's chart, they retype it. Retyping loses the citations and invites errors. Writing it back puts the clinician's decision, and what supported it, in the legal record where the rest of the care team sees it.

The risk is the opposite: AI text entering the legal record unchecked, or written to the wrong patient. So v1 is narrow.

## 2. What MHAI could write

| Option | What | Risk | v1? |
|---|---|---|---|
| W0 | Nothing sent: the clinician copies or prints the approved note and enters it in the EHR themselves | lowest; no integration | **yes, first** (no connector needed) |
| W1 | A **clinician-approved consultation note**: the question, the clinician's edited summary of the answer, its citations, and the attribution | text in the legal record; wrong patient | **yes, recommended v1** |
| W2 | A structured flag or task (for example "recheck potassium in 24 h") | it can drive work in the hospital; coded meaning must be exact | no; later, its own review |
| W3 | Coded observations, problem list entries, orders or prescriptions | it changes clinical data and can act on the patient | **never** without a separate design and regulatory review |

**Recommendation:** W0 at once (it needs only a screen), then W1 through FHIR R4 for hospitals that have it. Nothing else in v1.

## 3. Goals, non-goals, anti-goals

**Goals**
- G1. A clinician turns a checked answer into a note, edits it, and approves it. Only then is it sent (§4).
- G2. The note says what it is: "Drafted with MHAI decision support; reviewed and approved by Dr X" (§5).
- G3. One delivery path per connector type: FHIR R4, HL7 v2 through the bridge, file drop (§6).
- G4. Exactly once in the EHR, with confirmation, retries, corrections and retraction (§7).
- G5. Off by default per hospital and per connection (§9).

**Non-goals**
- W2 and W3.
- Writing anything automatically, on a schedule, or from an alert.
- Writing a note for an answer that isn't PASS. An ESCALATE or REGENERATE answer has no "send" button.
- An inbound listener on our side. We stay outbound-only.

**Anti-goals: failure even if it works**
- A1. Anything reaches the EHR without a named clinician's explicit approval of that exact text.
- A2. A note is filed against the wrong patient, or in another hospital's EHR.
- A3. A note is filed twice, or is filed and MHAI says it wasn't (or the reverse).
- A4. AI text in the record without the attribution.
- A5. A write credential or route out from core or the worker.
- A6. The EHR, or a forged reply, can make MHAI change or delete anything other than our own notes.

## 4. Mandatory review and approval

1. On a PASS answer about a named patient (`patient_id` set), the clinician presses "Prepare note for the record".
2. Core builds a **draft** from the answer by fixed rules (no new model call): the question, the answer text, the record facts used (from the companion design), citations as short references (title, section, version), and the attribution block. Status `draft`.
3. The clinician edits the text. Edits are free text, kept as the clinician's. The attribution block and the citation list can't be removed (they can be shortened by removing citations the clinician didn't rely on).
4. The clinician sees the patient banner from the EHR side (name, MRN, birth date, from the patient index at this moment) and confirms "This is the right patient".
5. **Approve and send**, with step-up (the second factor again). Core stores the approved text, its SHA-256, the approver, the time, and the patient. Status `approved`. Only now can it be sent.
6. Any change after approval makes a new version that needs a new approval.

Rules: the approver must be a clinician of the hospital, active, with the new permission `record.write` (granted by a hospital admin, off by default). Only the clinician who asked, or a clinician the hospital allows, may approve. No one can approve their own note on someone else's behalf. A note can't be approved more than 24 hours after the answer (the answer and the record may have moved on).

## 5. Attribution and content

The note always ends with a fixed block (wording for the clinical lead and legal, §13 Q5):

> Drafted with MHAI clinical decision support from a question asked on 27 Nov 2026 14:02. Reviewed, edited and approved by Dr A. Example (clinician id in the hospital record) on 27 Nov 2026 14:10. MHAI reference: N-7Q3K. Sources: WHO malaria guidelines 2025 §4.2; national formulary 2024, artemether-lumefantrine.

- **Author:** the approving clinician. MHAI is named as a tool, not an author.
- **No answer confidence or trust score** in the note. It could be read as a clinical grade.
- **No other patient's data** (the draft is built from one query).
- **Size:** at most 8,000 characters; plain text (FHIR) or TX segments (HL7). No HTML, no attachments in v1.
- **Clinician mapping:** the hospital's own practitioner id for the approver, entered once by a hospital admin per clinician (`practitioner_ref`). Without it, the note can't be sent (W0 still works).

## 6. Delivery per connector

The emr service sends; core never does (credentials and egress stay in one process, EMR §3.1). Core publishes an id-only job `emr.v1.<hid>.write` (`{note_version_id}`) with a signed token (`aud: emr`, `sub` = the approver, `pids` = the patient). The emr service reads the approved text under RLS from a new table, checks its hash, and delivers.

### 6.1 FHIR R4: `DocumentReference` (recommended)

- **Why `DocumentReference`:** it is the resource most FHIR servers accept for a clinical note, and US Core and IPS both profile it for notes. `Composition` needs a document `Bundle` and is rarely writable. `Communication` is a message between people, not part of the chart.
- **Shape:** `status = current`, `docStatus = final`, `type` = a LOINC note type (recommended 11488-4 "Consult note"; clinical lead confirms), `category = clinical-note`, `subject` = the patient's source id from `patient_links` for that connection, `author` = the practitioner, `authenticator` = the practitioner, `date`, `content.attachment` (`text/plain; charset=utf-8`, base64, with `hash` SHA-1 as FHIR defines plus our SHA-256 in an identifier), `identifier` = `urn:mhai:note|<note_version_id>`, `relatesTo` for corrections (§7.3). A provenance-style extension is avoided; the attribution is in the text.
- **Create:** conditional create, `POST DocumentReference` with `If-None-Exist: identifier=urn:mhai:note|<id>`. A second send can't make a second note.
- **Authorisation:** SMART Backend Services (client credentials with a signed JWT; the private key in the emr service's secret store, EMR §6). Scopes, narrowest: `system/DocumentReference.c` (SMART v2) or `system/DocumentReference.write` (v1) for create, and `.u` only if the hospital allows corrections by update (§7.3). No read scope beyond what the connection already holds. A separate client registration from the read connection, so a hospital can revoke writing alone.
- **Confirmation:** `201 Created` with `Location` (server id and version) is "delivered". `200` on conditional create (it already exists) is "delivered" too, after checking the found document's identifier is ours. Anything else is §7.2.

### 6.2 HL7 v2: MDM through the on-premises bridge

- **Message:** `MDM^T02` (original document notification and content): `PID` (MRN from `patient_links`), `PV1` if an encounter is known, `TXA` (document type, unique document number = our note version id, authenticating clinician, status `AU` authenticated), `OBX` of type `TX` with the text, one line per `OBX`. Corrections `MDM^T10` (replacement); retraction `MDM^T11` (cancel) where the hospital's system supports it (§7.3). Not `ORU`: that is for results, and a note sent as a result would be shown as a lab value in some systems.
- **The bridge gains an outbound side.** Today it takes MLLP pushes on the hospital LAN and serves them as files over its own SFTP for our pull. For write-back:
  - we upload the message file to the bridge's `outbox/` over the same SFTP session (still outbound from our side; SFTP write permission on `outbox/` only, a separate SFTP user from the read user);
  - the bridge's new **MLLP sender** sends it to one configured address on the hospital LAN (the interface engine), waits for the `ACK`, and writes `acks/<message control id>.ack`;
  - we pull the ack file on the next poll. `AA` is delivered. `AE` or `AR` is failed with the code (§7.2);
  - the sender allows exactly one destination host and port, set at install on the hospital side, not by us; it refuses files that are not `MDM^T02`, `T10` or `T11`, over 64 KB, or not signed by our key (an Ed25519 signature file beside each message, the public key installed with the bridge). A compromised SFTP account can't make it send other message types or reach other hosts.
- **Bridge security:** the sender runs as its own user, no inbound port except the existing MLLP listener and SFTP; logs ids and ack codes only; the hospital's IT owns the destination setting.

### 6.3 CSV and file export hospitals: an outbound file drop

- A plain-text note file plus a small manifest (`mhai_note_v1`: note id, MRN, practitioner ref, time, SHA-256), written over SFTP to an `outbox/` folder the hospital provides, with write-only rights.
- **Confirmation:** the hospital writes `receipts/<note id>.ok` or `.err` after import; we read it. With no receipt mechanism, the note shows "sent, not confirmed" and the clinician is told to check the EHR. Recommended: no file drop without receipts (§13 Q4).

### 6.4 Egress

- Same `egress-emr` proxy, same per-connection allow-list, same address pinning. Write-back adds no host: it uses the connection's existing host, with a separate `write_enabled` flag on the connection and separate credentials.
- **Still outbound-only.** No inbound port on our side. Confirmation comes back on our own request (FHIR response, pulled ack or receipt files).

## 7. Idempotency, retries, confirmation, corrections

### 7.1 Idempotency
- One `note_version_id` per approved version. The EHR-side identifier is that id (FHIR identifier, HL7 TXA-12, file name).
- A table `emr.note_deliveries` (`note_version_id` unique, `connection_id`, `state`, `attempts`, `ehr_ref`, `last_code`, times). Sending takes a row lock; a second sender skips.

### 7.2 Retries and states
- States: `approved → queued → sending → delivered | failed | unconfirmed`.
- Retry on network errors, `429`, `5xx`, missing ack: backoff 1, 5, 15, 60 minutes, at most 6 tries over about 3 hours. Conditional create (FHIR) and the unique document number (HL7) make a retry safe.
- **Never retry** on `4xx` other than `429`, `AR`, or a `.err` receipt: `failed` with the code. The clinician sees it, and W0 (copy or print) is offered.
- After the last try with no answer: `unconfirmed`. The clinician and the hospital admin are told to check the EHR. An operator can re-query (FHIR search by identifier) to settle it.

### 7.3 Corrections and retraction
- **Correction:** a clinician edits and re-approves; a new version is sent with `relatesTo.code = replaces` (FHIR) or `MDM^T10` (HL7). The old one is marked `superseded` in the EHR where supported (`status = superseded` needs `.u`).
- **Retraction** (wrong patient, sent in error): approver or hospital admin, with step-up and a reason. FHIR: update to `status = entered-in-error`. HL7: `MDM^T11`. File drop: a retraction manifest. Where the EHR can't take it, the hospital's records team is notified to correct by hand, and MHAI keeps the retraction request open until confirmed.
- We only ever change notes whose identifier is ours. The emr service refuses any update to a resource it didn't create (it checks the stored `ehr_ref` and our identifier).

## 8. Audit trail

Core writes, on the hash chain, closed payloads with ids only (no text, no name, no MRN):
- `record_note.drafted`, `.edited` (counts of characters changed), `.approved` (approver, `note_version_id`, text SHA-256), `.sent`, `.delivered` (`ehr_ref` kind, not the id itself if it is an identifier), `.failed` (code), `.unconfirmed`, `.corrected`, `.retracted` (reason code).
- The emr service reports delivery results with an id-only `emr.v1.<hid>.signal` (`note_delivered`, `note_failed`), as for other signals; core records them.
- The approved text is kept with the note (retention as for the question), so what was sent can be shown later. The EHR copy is the legal record; ours is the evidence of what MHAI sent.

## 9. Switches and the hospital agreement

- **Per hospital:** `record_write` setting, `off` by default, positions `off`, `copy_only` (W0), `send`. A platform admin sets it with step-up, after the hospital's written agreement is recorded (agreement reference, like the local practice onboarding record), two platform admins (one records, one approves).
- **Per connection:** `write_enabled`, off by default, with separate write credentials.
- **Per clinician:** `record.write`, granted by a hospital admin.
- **Turning it off is always quick (owner, 27 Nov).** Switching on is slow and two-person; switching off is one person, at once, with step-up and a reason, audited:
  - **Hospital admins** can switch their hospital's write-back off (or down to `copy_only`) in the hospital settings, and back on again only up to the level the platform approved. They can also pause a single connection's sending and remove any clinician's `record.write`.
  - **Platform admins** can switch any hospital, any connection, or every hospital at once (a global stop) off.
  - Off takes effect for anything not yet sent: queued notes stay `approved, not sent` (never dropped), and are sent only after it is back on and the approver confirms again. Notes already delivered are untouched (retraction is §8).
- **The hospital agreement clause** (legal drafts, §13 Q6): the hospital instructs MHAI to send clinician-approved notes to its EHR; the hospital remains controller and the note's custodian; the approving clinician is the author and is responsible for the content; MHAI is a tool and is named in the note; the hospital names the note type and where it appears in the chart; the hospital tells us how corrections and retractions work in its system; the hospital can switch it off at any time (its admins do it themselves in the hospital settings, see above).

## 10. Liability and medico-legal points

- **AI text in the legal record.** The note is the clinician's once they approve it. The attribution says MHAI helped draft it. That is honest and lets a later reader weigh it. The clinical lead and legal confirm this framing (§13 Q5).
- **Regulatory position.** Writing a clinician-approved note doesn't change what MHAI decides. It is still decision support with a human in the loop (ADR 0009 rule 11). W2 and W3 would come closer to automated action and need their own review.
- **Discoverability.** Our stored copy and audit can be asked for in a dispute. Retention follows the agreement and NDPA; legal holds apply (as EMR §10.6).
- **Patient rights.** The note is in the hospital's record, so access and correction requests go to the hospital. We help by retraction (§7.3).
- **No signature on behalf of the clinician beyond their approval.** If the hospital requires the EHR's own electronic signature, the note goes in as unsigned (`docStatus = preliminary`, HL7 `TXA-17 = IP`) for the clinician to sign in the EHR. Recommended where the EHR requires it (§13 Q7).

## 11. Phased plan (PRs; nothing built by this design)

1. **Decisions on paper.** The ADR amending ADR 0009 rule 9; the agreement clause; the attribution wording.
2. **W0: copy or print.** Draft, edit, approve (step-up), the attribution, the audit entries. No connector. Useful at once.
3. **Write tables and the job.** `record_notes`, `emr.note_deliveries`, RLS, the `emr.v1.<hid>.write` subject and NATS rights, signals.
4. **FHIR `DocumentReference`.** Conditional create, SMART Backend Services write client, confirmation, retries, against a synthetic FHIR server.
5. **Corrections and retraction** for FHIR.
6. **HL7 v2 MDM.** Message builder, signed outbox files, ack pull; the bridge's MLLP sender (bridge repo).
7. **File drop with receipts** (only if a hospital needs it).
8. **Evaluation and go-live** per hospital: shadow mode on a test EHR, then one ward.

## 12. Security and threat model

### 12.1 Assets
- The hospital's legal record (integrity first).
- Patient identity on the note (no wrong-patient filing).
- Write credentials (FHIR client key, SFTP write user, the bridge's signing key).
- The approval and audit trail.
- Hospital separation.
- Clinical safety: only PASS answers, only approved text.

### 12.2 Actors and trust boundaries
**Actors:** the approving clinician, other clinicians, hospital admins (grant `record.write`, retract), platform admins (switches), core, the emr service, the bridge, the hospital's EHR and interface engine, the hospital's IT, an attacker with a stolen session, a compromised SFTP account, a forged EHR response.

**Boundaries and checks**
1. Browser → BFF → core: session, two-step, `record.write`, step-up on approve and retract, the hospital from the token.
2. Core → Postgres: RLS; the approved text and hash stored once, insert-only.
3. Core → NATS → emr service: id-only job, signed token (`aud: emr`, `pids`), the emr verifier's checks.
4. emr service → `egress-emr` → EHR: per-connection allow-list, address pinning, TLS, write credentials from the secret store.
5. emr service → bridge (SFTP outbox) → bridge MLLP sender → interface engine on the LAN: signed files, one destination set on the hospital side.
6. EHR → emr service: responses, ack files, receipts. Untrusted input.

### 12.3 Threats (STRIDE)

| # | STRIDE | Threat | Mitigation | Test |
|---|---|---|---|---|
| WB1 | Spoofing | A stolen session approves and sends a note | step-up on approve; `record.write`; 24-hour limit; audit flags unusual volume | W1 |
| WB2 | Spoofing | A forged EHR response or ack marks a note delivered | TLS with pinning (FHIR); acks pulled from our SFTP session only and matched by control id; FHIR found-document identifier check | W2 |
| WB3 | Tampering | Text changed between approval and sending | SHA-256 at approval, checked by the emr service before sending; insert-only table | W3 |
| WB4 | Tampering | Wrong patient: a body naming another patient | patient from the query row, checked in `pids`; source id from `patient_links` of that connection only; the clinician's patient confirmation step | W4 |
| WB5 | Tampering | A compromised SFTP account makes the bridge send other messages | Ed25519-signed files; message type and size allow-list; one fixed destination set on the hospital side | W5 |
| WB6 | Tampering | Changing or deleting EHR resources we didn't create | update only by stored `ehr_ref` with our identifier; create-only scope by default | W6 |
| WB7 | Repudiation | "I never approved that" | approval with step-up, hash and time on the audit chain; the text kept | W7 |
| WB8 | Information disclosure | A note sent to another hospital's EHR | connection must belong to the token's hospital; RLS; per-connection proxy auth (EMR §6.4) | W8 |
| WB9 | Information disclosure | Note text in logs, NATS, dead letters | ids only in messages and logs; marker scan | W9 |
| WB10 | Denial of service | Retry storms against the EHR | bounded backoff, one sender per note, per-connection rate limit (10 notes a minute) | W10 |
| WB11 | Denial of service | The EHR down leaves notes in limbo | `unconfirmed` state, clinician told, W0 offered | W11 |
| WB12 | Elevation of privilege | Core or the worker gains a write route out | no credential or egress in core or the worker; grant and network tests | W12 |
| WB13 | Elevation of privilege | A clinician approves on another's behalf, or a hospital admin edits clinical text | approver = the session user; admins can retract, never edit or approve | W13 |
| WB14 | Clinical safety | A non-PASS answer written to the record | no draft for ESCALATE, REGENERATE, degraded or unverified results | W14 |
| WB15 | Clinical safety | Attribution stripped | the block is added by the emr service at send time from stored fields, not from the editable text | W15 |
| WB16 | Clinical safety | A duplicate note confuses the chart | conditional create and unique document numbers | W16 |

### 12.4 Controls (decided)
- **Authentication and step-up:** two-step session; step-up on approve, retract, switch and connection changes.
- **Authorization:** `record.write` per clinician; hospital from the token; FORCE RLS on `record_notes` and `emr.note_deliveries`; the emr service reads only approved versions; core never holds write credentials.
- **Data handling:** the approved text stored once, encrypted at rest with the database; ids only in NATS and logs; no cloud model involved (drafts are built by fixed rules, not a model call).
- **Concurrency:** one delivery row per note version, row lock while sending; conditional create.
- **Secrets:** write credentials and the bridge signing key in the secret store (EMR §6), separate from read credentials; rotation as EMR §6.3; placeholders refused at start.
- **Third parties:** the hospital's EHR and interface engine only, under the agreement clause. No new outside service.

### 12.5 Abuse-case tests (the plan writes these first)
- **W1** Approve without step-up is refused; approve 25 hours after the answer is refused.
- **W2** A FHIR `200` returning a document without our identifier is not "delivered"; an ack file with an unknown control id is ignored and counted.
- **W3** A text whose hash doesn't match the approval is not sent; the note goes `failed` with `hash_mismatch`.
- **W4** A job naming a patient not in `pids` is refused; a patient with no link on that connection can't be sent (W0 only).
- **W5** The bridge refuses an unsigned file, an `ORU^R01`, a 100 KB file, and never connects anywhere but its configured destination.
- **W6** An update to an EHR resource not in `note_deliveries` is refused before any request.
- **W7** Every approval has an audit entry with the text hash; the chain verifies.
- **W8** Hospital B's note job can't use hospital A's connection (token, RLS and proxy auth each refuse).
- **W9** No note text appears in logs, NATS messages or dead letters (marker scan).
- **W10** 50 sends for one note give one EHR create.
- **W11** An EHR down for 4 hours ends `unconfirmed`, with the clinician's notice.
- **W12** Core and the worker have no route to `egress-emr` and no write credential (network and grant tests).
- **W13** A hospital admin's edit or approve is 403; a clinician can't approve a draft another clinician created unless the hospital allows it.
- **W14** No draft route answers for a non-PASS result (404).
- **W15** A note whose edited text deletes the attribution still arrives with it.
- **W16** A retry after a timeout that had in fact succeeded creates no second document.

### 12.6 Residual risk and sign-off
- **R-WB1** The clinician approves without reading. Mitigated by the patient confirmation, step-up and edits; accepted as the clinician's responsibility, stated in the agreement.
- **R-WB2** An EHR that can't take retraction leaves a wrong note until fixed by hand. Mitigated by the open retraction request and notice to the records team.
- **R-WB3** An EHR may display the note differently (line breaks, truncation). Mitigated by plain text, the size cap and shadow testing per hospital.
- **R-WB4** `unconfirmed` notes may exist in the EHR. Mitigated by the identifier search and the clinician's notice.
- **R-WB5** A compromised emr service could write to every connected EHR it holds write credentials for. Mitigated by create-only scopes, per-connection credentials and rate limits; the same blast radius as reading, now with integrity at stake.
- **Sign-off before any live write:** product owner (H-WB1: the scope W0/W1, this design, the ADR); clinical lead (H-WB2: note type, content, attribution wording, the 24-hour limit, the PASS-only rule); legal (H-WB3: the agreement clause, the medico-legal framing, retention, regulatory position); privacy lead (H-WB4: the DPIA update); each hospital's IT (H-WB5: the write client, the bridge destination, how corrections work). These go in TODO.md.

## 13. Decisions (product owner, 27 Nov 2026: the recommended option for each)

| # | Decision | Options | Recommendation |
|---|---|---|---|
| Q1 | Do write-back at all? | (a) W0 only (copy or print); (b) W0 then W1; (c) also W2 | **(b)**: W0 now, W1 after sign-off |
| Q2 | FHIR resource | (a) `DocumentReference`; (b) `Composition` in a document bundle; (c) `Communication` | **(a)** |
| Q3 | HL7 v2 message | (a) `MDM^T02` via the bridge's new outbound sender; (b) `ORU` with a text result | **(a)** |
| Q4 | File drop hospitals | (a) only with receipts; (b) without receipts, "sent, not confirmed"; (c) none, W0 only | **(c)** until a hospital asks, then (a) |
| Q5 | Attribution wording and author | (a) the clinician is author, MHAI named as a tool in the text; (b) MHAI as a co-author device | **(a)**; clinical lead and legal confirm wording |
| Q6 | Agreement | (a) a written clause per hospital, recorded by two platform admins, before `send`; (b) a general clause in the main agreement | **(a)** |
| Q7 | Signing | (a) send as final when the clinician approves in MHAI; (b) send as preliminary for signing in the EHR | **(b)** where the EHR requires its own signature, else (a) |
| Q8 | Who may approve | (a) only the clinician who asked; (b) any clinician with `record.write` | **(a)** in v1 |
| Q9 | Approval window | (a) 24 hours; (b) the shift; (c) none | **(a)** |

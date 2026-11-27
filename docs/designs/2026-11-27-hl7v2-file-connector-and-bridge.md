# HL7 v2 message files (`hl7v2_file`) and the on-site HL7 bridge

- **Status:** Proposed, for the product owner's review. Built in two PRs: the `hl7v2_file` connector (part A) and the on-site bridge (part B). Nothing is switched on: a hospital connection needs the steps in §9.
- **Date:** 2026-11-27
- **Decision recorded in:** [ADR 0019](../adr/0019-read-hl7v2-as-files-and-push-through-an-on-site-bridge.md)
- **Builds on (and must stay consistent with):**
  - the [EMR integration design](../specs/2026-11-07-emr-integration-design.md): the connector interface (§4), the scheduled file export (§5.4), egress (§6.4), polling (§7), raw records and the allow-list (§8.1, §14.1), normalisation (§9), the patient index (§10), staleness (§15.4) and its threat model (§17);
  - [ADR 0014](../adr/0014-separate-emr-service-and-document-route.md): only the emr service holds outside credentials and reaches hospital systems, through `egress-emr`;
  - the `file_export` connector as built (EMR PR 11, `services/emr/internal/connector/fileexport/`): SFTP with a pinned host key, `processed/` and `rejected/`, never deleting, `MaxFiles`, staleness by the declared interval.
- **Format document:** [`docs/specs/formats/hl7v2_file_v1.md`](../specs/formats/hl7v2_file_v1.md) (what the hospital's interface engine, or our bridge, writes).

## 1. Why

Most hospital labs and pharmacy systems speak HL7 v2, not FHIR, and many can't write our `mhai_csv_v1` format without custom work. An interface engine can always do one of two things with HL7 v2 messages: send them over MLLP (a TCP stream), or write them to files. The EMR design deferred HL7 v2 because MLLP needs an inbound listener (§7.3 there). This design reads HL7 v2 without one:

- **Part A, `hl7v2_file`:** the hospital's interface engine writes the messages to files in a secure folder; the emr service pulls them over SFTP exactly as it pulls a `file_export`.
- **Part B, the HL7 bridge:** for a hospital whose engine can only push MLLP, we ship a small program the hospital runs next to the engine, on its own network. It receives MLLP, acknowledges each message only after it is on disk, and writes the same files, which it serves over its own SFTP server. Our side still only pulls.

**Our live host never listens for hospital traffic.** Every EMR connector today is outbound only (egress-emr); an inbound port on our host would be a new attack surface and a separate security decision. A direct MLLP listener on our side is deferred (§8).

## 2. Goals and non-goals

**Goals**
- Lab results and vital signs (ORU^R01) and medication orders (RDE^O11) from HL7 v2 reach the same raw records, the same normaliser, rules and alerts as `fhir_r4` and `file_export`, unchanged downstream.
- The same transport, controls and failure semantics as `file_export`: pinned host key, key-only sign-in through egress-emr, `processed/` and `rejected/`, nothing deleted by the emr service, `MaxFiles` per run, staleness by declared interval.
- A strict, capped ER7 parser that is fuzzed, and that refuses what it can't read instead of guessing (encodings, times without an offset).
- One file format, defined once in code (`services/emr/internal/hl7v2`) and used by both the connector and the bridge.

**Non-goals (v1)**
- An MLLP listener on our hosts (§8).
- ADT messages. Each ORU and RDE carries its own PID (and PV1), which is all the identity and location the canonical model needs. ADT merges (A40) and updates (A08) are skipped.
- Dispense and administration (RDS^O13, RAS^O17): `fhir_r4` reads MedicationRequest and MedicationStatement only, so there is no canonical place for them yet. Skipped and counted.
- Enhanced acknowledgement mode, HL7 v3, CDA, XML encoding of v2.

## 3. Part A: the `hl7v2_file` connector

### 3.1 Type and settings

`hl7v2_file`, label "HL7 v2 message files", summary "Reads HL7 v2 result and medication messages from files in a secure folder." Declared once in `services/gokit/emrtypes` (built), so core's `GET /platform/emr/connector-types`, its save checks, its audit schemas and `CORE_EMR_CONNECTOR_TYPES` (`emrtypes.Built`) accept it without core code changes.

| key | kind | required | max | meaning |
|---|---|---|---|---|
| `folder` | text | yes | 200 | the folder the messages are written to (same shape rules as `file_export`) |
| `username` | text | yes | 32 | the SFTP user |
| `host_key_fingerprint` | text | yes | 100 | `SHA256:…`, typed from what the hospital gave out of band; never learned |
| `expected_every_minutes` | text | yes | 6 | the longest normal gap between files, 5 to 10,080; staleness (§15.4 there) |
| `time_zone` | text | no | 64 | an IANA zone (`Africa/Lagos`). Times without an offset are read in it; without it they are refused |
| `sending_facility` | text | no | 60 | MSH-4.1. When set, a file holding a message from any other facility is refused whole |
| `mrn_namespace` | text | no | 200 | as `fhir_r4`: the identifier system the MRN is kept and compared under |
| `mrn_identifier_type` | text | no | 5 | PID-3.5 (CX.5), e.g. `MR`: which PID-3 repetition is the MRN |
| `mrn_assigning_authority` | text | no | 60 | PID-3.4.1 (CX.4): narrows the MRN repetition further |
| `access_key` | secret | yes | 4,000 | the SSH private key (unencrypted OpenSSH or PEM; Ed25519, ECDSA or RSA ≥ 2048) |

- **MRN rule (same as `fhir_r4` and `file_export`):** an MRN is stored only when `mrn_namespace` and `mrn_identifier_type` are both set, from the first PID-3 repetition whose CX.5 equals the type (and CX.4.1 the authority, when set). One without the other is refused at save (`ErrConfig`). Without them no MRN is stored.
- The base address is `sftp://host[:22]`, with `emrurl`'s checks (a DNS name, no userinfo, query or fragment). egress-emr allows port 22 only for SFTP, so a bridge is published on port 22 (§4.6).
- `poll_seconds` defaults to 300 as for `file_export`.

### 3.2 Transport (unchanged from `file_export`)

The connector *is* the `file_export` connector with a different file format: the SFTP session, host-key pinning, the paged and capped folder listing (`listdir.go`), the `.part`/`.tmp`/dot-file "still being written" rule, the size check against the listing, the moves to `processed/` and `rejected/` (never over another file, never a delete), `MaxFiles` (20) per run, the cursor (`drained`, last new file, set-aside count) and the timeouts are shared code. `fileexport.FileExport` gains a `format` value: `mhai_csv_v1` (the existing CSV) or `hl7v2_file_v1` (this design). Only file naming, the completeness check and parsing differ.

- **Files read:** `hl7-<UTC time YYYYMMDDTHHMMSSZ>[-<seq 1–4 digits>].hl7`, in time then sequence order. Any other name is never read or moved, but counted as set aside, so a misnamed results file is visible.
- **Complete:** the file ends with a segment terminator, and a file that opens with FHS ends with FTS (one that opens with BHS ends with BTS). Otherwise it is assumed still being written and left for the next run (counted).
- **Staleness:** as `file_export` (the scheduler's `Stale` and migration's `emr_platform_health()` / `emr_sources_status()` treat `hl7v2_file` like `file_export`: no new file for `expected_every_minutes` × 1.5 is behind).

### 3.3 The ER7 parser (`services/emr/internal/hl7v2`)

A pure package with no dependencies outside the standard library, used by the connector and the bridge.

- **Delimiters from the message:** MSH-1 is the field separator; MSH-2 the component, repetition, escape and subcomponent characters (and, from v2.7, the truncation character). They must be 4 or 5 distinct printable ASCII punctuation characters, none equal to the field separator. Each message (and FHS/BHS) carries its own; nothing is assumed from a previous message.
- **Segments:** terminated by `\r`; `\n` and `\r\n` are tolerated, and empty lines skipped. A segment id is three upper-case letters or digits.
- **Structure:** fields → repetitions → components → subcomponents, kept raw (escaped) and unescaped only when a value is read, so escaped delimiters never split anything.
- **Escapes:** `\F\ \S\ \T\ \R\ \E\` decode to the delimiter; `\Xhh…\` to bytes, which must then be valid in the message's character set; `\H\ \N\` and the formatting commands (`\.br\`, `\.sp\`, …) become a space or nothing. Character-set switches (`\C…\`, `\M…\`) and locally defined `\Z…\` make the value unreadable (a dead letter, §3.5), never guessed. An unterminated escape is unreadable too.
- **Character set (MSH-18):** empty means 7-bit ASCII (the standard's default), `ASCII`, `UNICODE UTF-8` and `8859/1` (decoded to UTF-8) are read. Anything else, a repeated MSH-18, a byte outside the declared set, or a NUL refuses the file (`encoding`): we never guess an encoding.
- **Batches:** FHS/BHS … BTS/FTS envelopes and bare concatenated messages (each MSH starts a message). Envelopes are checked: one FHS at most and only first, FTS only last, BTS-1 (when present) equal to the messages in its batch, FTS-1 equal to the batches. A file whose counts don't add up is refused (it was cut short or spliced). Segments before the first MSH other than FHS/BHS refuse the file.
- **Hard caps** (checked as it reads, before allocation where possible):

| cap | value | over it |
|---|---|---|
| file | 50 MB (`fileexport.MaxFile`) | refused whole (`too_large`) |
| message | 1 MiB | refused whole (`message_too_large`) |
| segment | 256 KiB | refused whole (`segment_too_long`) |
| messages per file | 20,000 | refused whole (`too_many_messages`) |
| segments per message | 2,000 | refused whole (`shape`) |
| fields per segment | 256 | refused whole (`shape`) |
| repetitions per field | 64 | refused whole (`shape`) |
| components per repetition | 32 | refused whole (`shape`) |
| subcomponents per component | 16 | refused whole (`shape`) |
| one escape sequence | 512 bytes | the value is unreadable |

Escapes only ever shrink a value, so unescaping can't amplify input; there is no recursion and the depth is fixed at four levels, so "deep nesting" can't exist in ER7.

### 3.4 Messages read and how each maps

MSH-9 (MSG.1^MSG.2) decides. Messages with MSH-11 (processing id) other than `P` are skipped (test and debug messages never become records). MSH-12 must start with `2.`. MSH-10 (control id) is required.

| Message | Read | Raw records |
|---|---|---|
| ORU^R01 | yes | patient (PID), location and encounter (PV1, when it names a visit), one observation per OBX |
| RDE^O11 | yes | patient, location and encounter as above, one medication (`kind: request`) per ORC/RXE |
| ADT^A01/A04/A08, RAS^O17, RDS^O13, any other | no | none: skipped and counted (`Metrics.Skipped`), the file still loads |

Every record is bound to the PID that precedes it **in the same message** (an ORU may hold several patient groups) and to the connection's hospital, which comes from the connection, never from the message (MSH-4, MSH-6 and PID-3.4 are never a hospital).

**Patient (PID)**
- `source_id`: the first PID-3 repetition with a value, as its CX.4.1, CX.5 and CX.1 joined length-prefixed (`len:value/…`), so no two different identifiers collide. A PID without one makes that patient's records dead letters.
- `identifier`: the MRN repetition (§3.1) under `mrn_namespace`; the sink keeps it as `mrn`, or nothing.
- `given` PID-5.2 (and .3), `family` PID-5.1.1, from the repetition with XPN.7 `L` (legal), else the first; `birth_date` from PID-7 (the date part); `sex` from PID-8 (`F` female, `M` male, `O`/`A` other, `U`/`N` unknown). Nothing else of PID is read (no address, phone, religion, national id).

**Location and encounter (PV1)**, only when PV1-19 (visit number) has a value:
- encounter `source_id` the patient's source id and PV1-19 (CX.4.1, CX.1) joined; `class` from PV1-2 (`I` inpatient, `O` outpatient, `E` emergency; others leave the encounter out); `status` `finished` when PV1-45 (discharge time) is set, else `in-progress`; `period_start` PV1-44, `period_end` PV1-45; `location` PV1-3.1 (point of care), `bed_label` PV1-3.3.
- location `source_id` PV1-3.1 (with PV1-3.4.1 facility joined), `name` PV1-3.1, `type` from the class (inpatient `ward`, emergency `emergency`, outpatient `clinic`).

**Observation (OBR/OBX in ORU^R01)**
- `source_id`: the patient's source id, the order (OBR-3 filler number, else OBR-2 placer, else ORC-3, ORC-2: EI.1 with EI.2), OBX-3 (identifier and coding system) and OBX-4 (sub-id), joined length-prefixed. The patient is part of the id (security review): an order number a feed reuses for another patient is another record, never a version that supersedes the first patient's row. A correction (OBX-11 `C`) resends the same order and OBX-3, so it is a new **version** of the same record and the normaliser supersedes the earlier row (design §8.2 there); an identical resend is the same version and a no-op. Over 256 bytes the id is `h:` and its SHA-256.
- `source_version`: the SHA-256 of the mapped fields (canonical JSON), so only a change makes a new version.
- `source_updated_at`: MSH-7.
- `codings`: OBX-3 as CWE, primary (.1–.3) and alternate (.4–.6) triplets. The coding system (HL7 table 0396) `LN` reads as `http://loinc.org`; every other system as `urn:mhai:hl7v2:<system>` (`urn:mhai:hl7v2:local` when empty), so a connection's reviewed map can key on it and an unmapped one takes the existing `unmapped_codes` path. `code_text` OBX-3.2.
- `category`: `vital-signs` when the LOINC is one of the vital-sign codes (the FHIR vital-signs profile set and those the rule set reads), else `laboratory`. A locally coded vital sign reads as laboratory until it is mapped; this only changes which unmapped category a source flag falls under.
- `value` by OBX-2 (value type):

| OBX-2 | mapping |
|---|---|
| `NM` | `value_quantity` (a leading `+` dropped; otherwise the strict decimal shape `file_export` uses) |
| `SN` | comparator and number (`<^5` → `<5`, `^5.2` → `5.2`); a ratio or range (`^1^:^128`) → `value_text` |
| `ST` | `value_text` (≤ 120 characters after cleaning, else a dead letter) |
| `CE`, `CWE` | `value_codings` (as OBX-3) |
| `TX`, `FT`, `ED`, `RP` and any other | the OBX is skipped and counted: narrative, reports and attachments are never on the allow-list (§14.1 there) |

- `unit` OBX-6.1 (else OBX-6.2), with `file_export`'s unit shape; `ref_low` / `ref_high` from OBX-7 when it reads `a-b`, `<b`, `<=b`, `>a` or `>=a` (decimals), else `ref_text`; `interpretation` the first OBX-8 repetition in the source-flag set (`L H LL HH A AA N`), others left out.
- `effective` OBX-14, else OBR-7; `issued` OBR-22.
- `status` from OBX-11, exactly as HL7's own v2-to-FHIR concept map (so the normaliser and rules treat it as a FHIR status): `F` and `U` final, `C` corrected, `A` amended, `P`, `R` and `S` preliminary, `X` cancelled, `D` and `W` entered-in-error. `I`, `O` and `N` (no result yet) are skipped and counted; anything else is a dead letter. The rules already run only critical thresholds and source flags on preliminary values, and withdraw a finding when its value is corrected or retracted.
- `patient` and `encounter`: the source ids above.

**Medication (ORC/RXE in RDE^O11)**
- `source_id`: the patient's source id and ORC-2 (placer order, EI.1 with EI.2), else ORC-3; no order makes a dead letter. `kind` `request`.
- `codings`: RXE-2 (give code). `WC` and `ATC` read as `http://www.whocc.no/atc` (ATC only, design §21); anything else (`RXNORM`, local) as `urn:mhai:hl7v2:<system>`, which takes the `unmapped_codes` path unless the connection's reviewed map names it. `med_text` RXE-2.2.
- `dose_value` RXE-3, `dose_unit` RXE-5.1 (else .2), `route` RXR-1.2 (else .1), `timing_text` TQ1-3.1 (else RXE-1.2.1), `authored` ORC-9, `period_start` TQ1-7 (else RXE-1.4), `period_end` TQ1-8 (else RXE-1.5).
- `status` from ORC-5 (`IP`, `SC`, `A` active; `CM` completed; `DC` stopped; `CA` cancelled; `HD` on-hold; `ER` entered-in-error), else ORC-1 (`NW`, `XO`, `RP`, `RL`, `SC` active; `CA`, `OC`, `CR` cancelled; `DC`, `OD`, `DR` stopped; `HD`, `OH` on-hold); anything else is a dead letter.
- **Why RDE only:** it is the pharmacy's encoded order, the one v2 message that is a MedicationRequest; RDS (dispense) and RAS (administration) are other FHIR resources that no connector reads today.

### 3.5 Times

HL7 DTM is `YYYY[MM[DD[HH[MM[SS[.S…]]]]]][+/-ZZZZ]`. A value with at least minutes is a date-time: with an offset it is read as given, without one it is read in `time_zone` when that is set and refused otherwise (never assumed UTC). A date alone is a date (birth dates, date-only results). Hours without minutes, or anything not in the grammar, is unreadable. Out: RFC 3339, which the normaliser reads as it reads FHIR. The normaliser's future-time and freshness rules then apply unchanged.

### 3.6 File-level refusal and dead letters (as `file_export`)

- **Refused whole** (moved to `rejected/`, nothing of it stored, counted as set aside; the run goes on): over a cap, the encoding (§3.3), an envelope that doesn't add up, a message whose MSH can't be read (delimiters, no MSH-9, MSH-10 or MSH-12), a segment id out of shape, or a message from a facility other than `sending_facility`. This is the same rule as a CSV whose shape is wrong.
- **Dead letter** (a rejected raw record with its identity and no payload, `unreadable_data`): a record whose shape is right but a value isn't: a time without an offset and no `time_zone`, a status outside the map, an unreadable escape, a value or unit out of shape, no usable patient or order id. The same rule as a CSV row whose values are wrong.
- **Skipped and counted** (not stored, not an error): an unsupported message type, a non-production message, an OBX that is narrative or has no result yet.

The parse runs over the whole file once before anything is stored (as `file_export`), so a file refused whole stores nothing.

## 4. Part B: the on-site HL7 bridge

A single Go binary, `hl7-bridge` (`services/emr/cmd/hl7-bridge`, code in `services/emr/internal/hl7bridge`), shipped as a container image the hospital runs on a Linux host next to its interface engine. It shares the ER7 parser and the file format with the connector (`internal/hl7v2`): the bridge writes files with `hl7v2.FileName` and `hl7v2.BatchWriter`, the connector reads them with `hl7v2.ParseFileName` and `hl7v2.ParseBatch`, and one test writes with one and reads with the other.

### 4.1 MLLP receiver

- Listens on a configured address and port on the hospital LAN (default `0.0.0.0:2575` inside the container), plain TCP or TLS 1.2+ (certificate and key files; optional client CA for mutual TLS).
- **Allow-list:** only source addresses inside `HL7_BRIDGE_MLLP_ALLOW` (CIDRs, required, no default) are served; others are closed at accept, counted.
- **Framing:** `0x0B` message `0x1C 0x0D`. Bytes before a start block are discarded (counted); a start block inside a frame, or a frame longer than the message cap (1 MiB), is answered with an `AR` acknowledgement when the MSH could be read, and the connection is then closed (the stream can't be trusted after it). Idle connections close after 5 minutes; at most 16 connections at once; a frame must complete within 2 minutes.
- **Checks:** the message is parsed with the connector's parser and rules. Unreadable MSH, encoding, or a version other than 2.x → `AR`; a structural error or cap → `AE` with an ERR segment carrying only the HL7 error code (no content). The sender learns at once, instead of the file being refused later.
- **Acknowledgement only after the message is durable:** the message is appended to the open spool file and `fsync`ed, then its control id is appended to the duplicate log and `fsync`ed, and only then is `MSA|AA|<control id>` sent. If the disk write fails, no ACK is sent and the connection is closed, so the sender retries (and health turns red). A crash between write and ACK means the sender resends: a duplicate in the spool is harmless, because the connector's versions make it a no-op.
- **Duplicates:** a message whose (MSH-3, MSH-4, MSH-10) was already stored in the last 7 days is acknowledged `AA` and not written again. The log keeps SHA-256 digests of those triples, never the values.
- **Types:** messages the connector doesn't read (§3.4) are acknowledged `AA` and **not stored** (data minimisation: ADT never reaches our disk); counted by type.
- The ACK is built from the received MSH (sending and receiving application and facility swapped, MSH-10 echoed in MSA-2, a new control id), in the message's own delimiters and version.

### 4.2 The spool

- One open file, `hl7-<UTC time>-<seq>.hl7.part`, beginning with FHS and BHS. Each accepted message is appended whole. The file is closed (BTS with the message count, FTS, `fsync`, rename to `.hl7`, `fsync` of the folder) every `HL7_BRIDGE_ROTATE` (default 1 minute) when it holds a message, or at 10,000 messages or 40 MB. The connector never reads a `.part` file.
- **Crash recovery:** the duplicate log also records each message's end offset in its file. At start, a `.part` file is cut back to the last offset recorded (a message written but not logged was never acknowledged) and closed.
- **Where:** `/spool` (a volume), mode 0700, owned by the bridge's non-root user. The SFTP view of it is `hl7/`, `hl7/processed/`, `hl7/rejected/`.

### 4.3 The built-in SFTP server

- SSH with **public-key sign-in only**: the authorised key is the public half of the key the platform admin saved as the connection's `access_key` (a file the hospital IT installs, `HL7_BRIDGE_AUTHORIZED_KEY`), one user name (`HL7_BRIDGE_SFTP_USER`). No password, keyboard-interactive or agent forwarding; no shell, exec, port forwarding or other channel than the `sftp` subsystem.
- **Host key:** an Ed25519 key made at first start in `/state` (0600). `hl7-bridge fingerprint` prints its `SHA256:` fingerprint, which the hospital gives the platform admin out of band for `host_key_fingerprint`.
- **What a client may do, and nothing else:** list and stat inside `hl7/` and its two subfolders; open a closed `.hl7` file for reading; rename a file from `hl7/` into `hl7/processed/` or `hl7/rejected/` (never over a file). No write, create, delete, mkdir, rmdir, symlink, setstat or rename anywhere else. Every path is cleaned and resolved under the spool root; `..`, absolute escapes, NUL, and symlinks are refused (the server never follows one).
- **Allow-list:** `HL7_BRIDGE_SFTP_ALLOW` (CIDRs, required): our egress address(es), or the VPN range (H7).
- Sign-in attempts are rate-limited (10 a minute per source address) and at most 4 sessions at once.

### 4.4 Retention

Files moved to `processed/` are deleted by the bridge `HL7_BRIDGE_RETAIN` after the move (default 7 days, at least 1 day; the move resets the file's time, so the clock starts when we have loaded it). Nothing else is ever deleted: open spool files, files not yet loaded and `rejected/` stay for a person to look at. The health check reports their counts.

### 4.5 Health, logs and metrics

- `GET /healthz` on a separate listener (default `127.0.0.1:8080`): `{"status":"ok|degraded|failing","pending_files":n,"rejected_files":n,"last_message_at":…,"last_pull_at":…,"disk_ok":bool}`. Counts and times only. `failing` when the spool can't be written; `degraded` when nothing has been pulled for an hour while files wait, or `rejected/` isn't empty.
- **Logs are counts and codes only:** never a message, a segment, a control id, a name, an MRN, a file's contents or an SSH key. Events are fixed strings with counts (`mllp_accepted`, `mllp_rejected{code}`, `duplicate`, `skipped_type`, `spool_rotated`, `sftp_signin_refused`, `pruned`). Source IP addresses of refused connections are logged (they are network data, not patient data, and the hospital's IT needs them).

### 4.6 Packaging and network

- Image: built from `services/` (`services/emr/Dockerfile.hl7-bridge`), static binary on `gcr.io/distroless/static-debian12:nonroot`. Compose example (`deploy/hl7-bridge/compose.yml`): read-only root, `cap_drop: [ALL]`, `no-new-privileges`, memory and CPU limits, the `/spool` and `/state` volumes the only writable paths.
- Ports: MLLP `2575` on the LAN (to the interface engine only); SFTP published as **port 22** of a public DNS name (egress-emr allows port 22 only for SFTP and refuses IP literals and private addresses; the container listens on 2222), firewalled to our egress address; or reached over the VPN (H7) with `private_network`; health on localhost.
- Install guide for hospital IT: `deploy/hl7-bridge/README.md` (host requirements, disk encryption, ports, what to set in the interface engine, how to give us the SFTP address and fingerprint, upgrades).
- **Upgrades:** the image is pinned by version; the spool format is versioned (`hl7v2_file_v1`, in FHS-10 and the format document); a new bridge reads an old spool. To upgrade: stop the engine's outbound channel (it queues), wait for `pending_files` to reach 0 or not, replace the container (the volumes stay), start, check `/healthz`, resume the channel.

## 5. Security and threat model

### 5.1 Assets

- **PHI in HL7 messages**: names, birth dates, MRNs and other identifiers, results and medications. In part A on the hospital's SFTP host and our database; in part B also on the bridge's disk.
- **Hospital separation**: every record bound to the connection's hospital only.
- **The SSH private key** (`access_key`) in our secret store; the bridge's host key and authorised key at the hospital.
- **Clinical safety**: a wrong, guessed or misattributed value must never become an alert; a corrected value must replace the old one.
- **Availability**: the emr service and the bridge under hostile or broken input.

### 5.2 Actors and trust boundaries

1. **Hospital interface engine → bridge (MLLP, hospital LAN):** untrusted bytes. Checked: source address allow-list, optional mutual TLS, framing, caps, the parser.
2. **Bridge disk:** PHI at rest at the hospital. Checked: folder modes, host disk encryption (§5.4), retention.
3. **Our emr service → egress-emr → hospital SFTP host or bridge (SSH):** checked: per-connection `Proxy-Authorization`, the allow-list from the connection's origin, pinned host key, our key, the bridge's client allow-list.
4. **File bytes → parser → mapper → RawSink → normaliser:** untrusted content. Checked: the parser's caps and strictness, the mapper's closed sets, the sink's allow-list and shapes, the normaliser's rules; the hospital and connection come from the connection row only.
5. **Platform admin → core → emr (settings, key):** as today: step-up, sealing, `ValidateFields` and the connector's `Validate`.

### 5.3 Threats (STRIDE) and mitigations

| # | Threat | Mitigation |
|---|---|---|
| S1 | A rogue server impersonates the hospital's SFTP host or bridge | the pinned host-key fingerprint, compared in constant time; never learned on first use (shared `egress.SSH`) |
| S2 | Anyone on the LAN sends forged HL7 to the bridge | MLLP source allow-list; optional mutual TLS; the engine's own channel is the only intended sender. Residual: a host inside the allowed range can still forge (as it can to any HL7 receiver) |
| S3 | Someone other than us pulls the bridge's files | key-only SSH with our connection's public key; client allow-list; rate-limited sign-in |
| T1 | **PID injection of another hospital's patient** (a message naming another facility or assigning authority) | the hospital and connection come from the connection, never from MSH-4, MSH-6 or PID-3.4; every patient is resolved within the connection (`index.Resolve`); optional `sending_facility` refuses a mixed feed whole. Test: a file whose PID-3 names another hospital's authority produces records only under this connection's hospital, and no row in the other hospital |
| T2 | Delimiter tricks: a message redefines delimiters, or embeds escaped delimiters, to smuggle segments or fields | delimiters are per message and validated; values are unescaped only after splitting, so `\F\`, `\R\` and `\X0D\` decode to text and never split; segment ids must be `[A-Z0-9]{3}`. Tests for each |
| T3 | Encoding attacks: overlong UTF-8, invalid bytes, a NUL, an undeclared charset, UTF-16, ISO 2022 switching | only the declared sets of §3.3, validated; everything else refuses the file; `\C` / `\M` make the value unreadable; then `sink.CleanText` again |
| T4 | A record of one patient attached to another inside a message | each record is bound to the PID before it in the same message; a message with OBX before any PID makes those records dead letters |
| T5 | A correction silently ignored, or an old value overriding a new one | corrections are new versions of the same source id; the normaliser's `write` orders by MSH-7; identical resends are no-ops |
| T6 | Spool tampering on the bridge host | host access is the hospital's; files are read once and moved; nothing in our pipeline trusts a file more than the hospital's own SFTP host (same as part A) |
| T7 | Path traversal through file names | the connector only reads names matching `hl7-…hl7` (no `/`); the bridge's SFTP server resolves every path under the root, refuses `..`, NUL and symlinks, and allows only the three folders |
| R1 | A hospital disputes what we read | raw records keep provenance (connection, source id, version, MSH-7, received time) and `payload_sha256`; moved files stay in `processed/` (on the bridge for the retention period) |
| I1 | **PHI in logs** | no segment, value, control id, file name of a patient, name or MRN in any log, error or metric of the connector or the bridge: errors are fixed reasons and counts; tests capture the logs of a full run and search for every synthetic name, MRN and value |
| I2 | PHI at rest on the bridge | §5.4 |
| I3 | ADT and other unneeded messages stored | the bridge acknowledges and drops types the connector doesn't read; the connector skips them |
| I4 | Narrative or attachments stored | TX/FT/ED/RP OBX are skipped; the sink's allow-list stays the last wall |
| D1 | Oversized files, messages or segments | hard caps (§3.3); the MLLP frame cap; `io.LimitReader` everywhere |
| D2 | "Zip-bomb-like" batches: millions of empty messages, repetitions or components in a small file | counts are capped per level; escapes only shrink; parsing is linear and allocates in proportion to the input |
| D3 | Deeply nested or escaped content | ER7 has four fixed levels; no recursion; one escape is capped at 512 bytes |
| D4 | A slow or endless MLLP sender, or many connections | per-frame and idle timeouts, at most 16 connections, the frame cap |
| D5 | A full disk on the bridge | ACK only after `fsync`: a failing write sends no ACK, so the sender keeps the message; health `failing`; rotation caps; retention |
| E1 | A bug in the parser leading to code execution or a panic | Go memory safety; native fuzzing of the parser and the MLLP framer, with a seed corpus; the parser never panics on any input (the fuzz targets assert it) |
| E2 | The bridge container escapes to the host | distroless, non-root, read-only root, no capabilities, `no-new-privileges`, only two writable volumes |

### 5.4 Controls (decided)

- **Authentication:** our side: SSH key from the secret store, used only for the connection's origin (unchanged). Bridge: key-only SSH with one authorised key; MLLP by network allow-list and optional mutual TLS.
- **Authorisation and hospital scope:** connection-bound, as every connector: the scheduler runs a connection under its hospital (RLS), and no message field selects a hospital or connection.
- **Data handling:** only the allow-listed fields of §3.4 are mapped; the sink re-applies the allow-list. **PHI at rest on the bridge: we require full-disk encryption of the bridge host** (LUKS, or the hypervisor's encrypted volume) and say so in the install guide; the spool files themselves are not encrypted by the bridge. Reason: the SFTP pull must serve byte-exact files whose size matches the listing, and per-file encryption would need key storage on the same host, which protects nothing against someone holding the disk with the key next to it. Retention (§4.4) bounds what is kept; ADT and other unneeded types are never written.
- **Logging:** §4.5 and I1.
- **Concurrency:** one fetch per connection (the scheduler's lock). In the bridge, one writer goroutine owns the spool; each connection waits for its own message's `fsync` before its ACK.
- **Secrets:** our SSH key as `file_export`. The bridge holds no secret of ours: only its own host key and our public key.
- **Third parties and hosting:** nothing leaves the hospital except to our egress address over SSH; the bridge calls nothing.

### 5.5 Abuse-case tests

Part A (parser, mapper, connector):
- a file over 50 MB, a message over 1 MiB, a segment over 256 KiB, 20,001 messages, 257 fields, 65 repetitions, 33 components, 17 subcomponents: each refused whole, nothing stored;
- a 1 MB file of `~` or `^` characters: refused by the caps, in bounded time and memory;
- a message whose MSH-2 repeats a character or uses a letter; one whose field separator is `^`; `\F\`, `\R\`, `\X0D\` in a value: never split, decoded as text;
- an MSH-18 of `UNICODE UTF-16`, `8859/2`, `ISO IR87`; invalid UTF-8 under `UNICODE UTF-8`; a non-ASCII byte with no MSH-18; a NUL: refused whole;
- a BTS count that doesn't match; an FHS file without FTS (left in place, not refused); FTS not last;
- a PID naming another hospital's assigning authority: records only under the connection's hospital;
- a second PID group in one ORU: each OBX bound to its own patient;
- an OBX before any PID: dead letter, never attached to a patient;
- a final then a corrected result in one file and across files: one current value, the corrected one; a resend of the same message: no new version;
- a preliminary result: stored as preliminary; `I` status: skipped;
- an OBX-3 coded `LN`: LOINC; one coded `99LAB`: `unmapped_codes`;
- a time without an offset, without `time_zone`: dead letter; with it: read in the zone;
- an unsupported type (ADT^A08) and `MSH-11 = T`: skipped, the file still loads;
- a `sending_facility` mismatch: refused whole;
- a full run's logs and errors contain no synthetic name, MRN, control id or value;
- the fuzz targets (`FuzzParseBatch`, `FuzzMapMessage`) never panic, and a parsed message re-reads to the same fields.

Part B (bridge):
- framing: split frames, two frames in one read, bytes before the start block, a missing end block, an over-cap frame (`AR`, connection closed);
- crash safety: a spool whose `fsync` fails gets no ACK; a `.part` file with an unacknowledged tail is cut back at start;
- duplicates: the same control id twice is stored once and acknowledged twice;
- an address outside the MLLP allow-list is closed without a byte;
- the SFTP server refuses password sign-in, a wrong key, `..` paths, writes, deletes, symlinks, renames outside the three folders and over an existing file;
- retention deletes only `processed/` files older than the retention;
- `/healthz` and all logs hold no message content;
- end to end: MLLP → spool → bridge SFTP → `hl7v2_file` connector → raw records → normaliser → an observation that the rules evaluate;
- the MLLP framer fuzz target (`FuzzMLLPReader`) never panics and never yields a frame over the cap.

### 5.6 Residual risk and sign-off

- **A sender inside the allowed LAN range can forge messages** to the bridge, as to any HL7 receiver. Mutual TLS closes it where the engine supports it; recommended, not required, because many engines don't.
- **Disk encryption on the bridge host is the hospital's to set up.** We can't verify it from the container; the install guide makes it a checklist item the hospital confirms in writing before go-live (privacy sign-off).
- **The bridge's SFTP port is reachable from the internet** (firewalled to our egress address) unless the hospital uses the VPN route (H7). Key-only auth, pinned host key, allow-list and rate limits are the controls; the VPN is preferred.
- **Locally coded vital signs read as laboratory** until mapped (§3.4); only unmapped-category source flags are affected.
- **Clinical sign-off:** the OBX-11 status map and the vital-sign LOINC set are clinical logic (they decide what can alert); both follow HL7's published v2-to-FHIR map and FHIR's vital-signs profile, and go to the clinical reviewer with this PR (`clinical-review` label).
- **TODO.md:** the deploy steps of §9; MLLP on our side (§8).

## 6. Database and API changes

- **Migration** (next free number on `main` when the PR opens): `emr_connections.type` and `emr.raw_records.connector_type` CHECKs gain `hl7v2_file`; `emr_platform_health()` and `emr_sources_status()` apply the `file_export` staleness rule to `hl7v2_file` as well. No new table, no new grant.
- **OpenAPI:** `EmrConnectorType.id` gains `hl7v2_file` (additive, ADR 0012; minor bump).
- **gokit:** `emrtypes.Types` gains the row; `emrurl.Schemes` maps it to `sftp`.
- **mhai-web:** the platform app renders fields from the connector-types reply, but reads `id` with a closed enum, so it needs `hl7v2_file` added (a small PR) **before** core offers the type, or the connector-types read fails.

## 7. Alternatives considered

- **MLLP listener on our host** (§8): the most direct route, but our first inbound port for hospital traffic, needing its own authentication, DoS limits and network design. Deferred.
- **Ask every hospital to write `mhai_csv_v1`:** works, but most engines already speak HL7 v2, and a mapping to our CSV is custom work at every hospital.
- **Bridge pushes to us over HTTPS:** an inbound endpoint on our side again; refused for the same reason as MLLP.
- **Bridge encrypts each spool file:** §5.4.
- **Map RDS and RAS:** no canonical table for dispenses or administrations; add with a FHIR MedicationDispense/Administration reader.
- **Read ADT for identity:** not needed (PID and PV1 travel in every ORU and RDE); A40 merges would be useful later through `link_replaced_by`.

## 8. Deferred: MLLP listener on our side

**MLLP listener: deferred, needs owner decision.** A listener on the live host would accept TCP from hospitals. Before it is built, the owner decides whether MHAI accepts inbound hospital traffic at all and, if so, the authentication (mutual TLS per hospital), the network path (VPN only, or public with an allow-list), and the DoS limits. The bridge (part B) gives push delivery without it. Until then, hospitals that can only push use the bridge.

## 9. Switching it on (live)

1. Merge the mhai-web PR (the type enum) and deploy the platform app.
2. Merge part A; run the migration (deploy does).
3. Add `hl7v2_file` to `CORE_EMR_CONNECTOR_TYPES` on the live host and restart core.
4. With the hospital's IT: either their engine writes files to an SFTP folder they host (with `processed/` and `rejected/`), or they install the bridge (`deploy/hl7-bridge/README.md`).
5. The platform admin adds the connection: `sftp://<dns name>`, folder, user, the host key fingerprint they were given, the expected interval, `time_zone` (if the engine sends times without an offset), the MRN settings, and the key (the public half goes to the hospital for the bridge's authorised key).
6. Test, switch on. Watch `rejected/` and the connection's errors for the first day.

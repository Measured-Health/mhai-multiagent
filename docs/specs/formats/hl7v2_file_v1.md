# `hl7v2_file_v1`: HL7 v2 message files for MHAI

**Status:** version 1, 27 Nov 2026. How a hospital's interface engine (or the MHAI HL7 bridge) writes HL7 v2 messages for the `hl7v2_file` connector to read over SFTP. Defined in code once, in `services/emr/internal/hl7v2` (`FileName`, `ParseFileName`, `FileHeader`, `FileTrailer`, `ParseBatch`, `Complete`); design: [`docs/designs/2026-11-27-hl7v2-file-connector-and-bridge.md`](../../designs/2026-11-27-hl7v2-file-connector-and-bridge.md) §3.

## The folder

- One folder for MHAI (the connection's `folder`), with two subfolders the hospital creates: `processed/` and `rejected/`. MHAI moves each file it has read into one of them and never deletes anything.
- MHAI signs in with an SSH key (public-key only) and checks the server's host key against the fingerprint the hospital gave us.

## File names

`hl7-<UTC time>-<sequence>.hl7`, for example `hl7-20261127T101500Z-0001.hl7`:

- the time is the file's creation time in UTC, `YYYYMMDDTHHMMSSZ`;
- the sequence (optional, 1 to 4 digits) orders files made in the same second;
- MHAI reads files in time, then sequence, order, at most 20 a run.

Write each file under another name (ending `.part` or `.tmp`, or starting with a dot) and rename it when it is complete. Any other file name is left alone and reported as set aside.

## Contents

- ER7 ("pipe and hat") encoding: segments end with a carriage return (`\r`); `\n` and `\r\n` are accepted.
- Either one or more messages one after another, or a batch envelope: `FHS` (first) and `BHS`, the messages, `BTS` (BTS-1: the number of messages in the batch) and `FTS` (last; FTS-1: the number of batches). A file that opens with `FHS` is read only once it ends with `FTS`.
- Each message's delimiters are its own (MSH-1, MSH-2).
- Character set (MSH-18): empty or `ASCII` (7-bit), `UNICODE UTF-8`, or `8859/1`. Anything else, or a byte not valid in the declared set, refuses the file. No byte-order mark.
- Times: with an offset (`20261127101500+0100`), or without one only if the connection's `time_zone` is set.
- MSH-11 must be `P` (production) for a message to be read; MSH-12 a 2.x version; MSH-10 a control id.

## Messages read

| MSH-9 | What MHAI keeps |
|---|---|
| `ORU^R01` | PID (identity), PV1 (visit and ward), OBR/OBX (lab results and vital signs; OBX-2 `NM`, `SN`, `ST`, `CE`, `CWE`; LOINC codes as `LN`) |
| `RDE^O11` | PID, PV1, ORC/RXE/RXR/TQ1 (medication orders; ATC codes as `WC` or `ATC`) |

Every other message type (ADT, RDS, RAS, …) and every OBX that is narrative (`TX`, `FT`), an attachment (`ED`, `RP`) or has no result yet (OBX-11 `I`, `O`, `N`) is skipped, and the rest of the file is read. Corrections (OBX-11 `C`) resend the same order number (OBR-3 or OBR-2) and the same OBX-3; MHAI replaces the earlier value.

## Limits

A file at most 50 MB and 20,000 messages; a message at most 1 MiB and 2,000 segments; a segment at most 256 KiB, 256 fields; a field at most 64 repetitions, 32 components and 16 subcomponents. A file over any limit, with an envelope that doesn't add up, or with a message whose header can't be read is moved to `rejected/` whole and nothing of it is stored.

## A synthetic sample

```
FHS|^~\&|SYNTH-ENGINE||||20261127101500+0000||hl7-20261127T101500Z-0001.hl7|hl7v2_file_v1
BHS|^~\&|SYNTH-ENGINE||||20261127101500+0000
MSH|^~\&|SYNTH-LAB|SYNTH-HOSP|MHAI|MHAI|20261127101500+0000||ORU^R01^ORU_R01|SYN-CTRL-1|P|2.5.1|||||||UNICODE UTF-8
PID|1||SYN-MRN-0001^^^SYNTH-HOSP^MR||SYNTHFAMILY^SYNTHGIVEN^^^^^L||19800101|F
PV1|1|I|SYNTH-WARD-A^^BED-3^SYNTH-HOSP||||||||||||||||SYN-VISIT-1^^^SYNTH-HOSP
ORC|RE|PL-1^SYNTH-EMR|SYN-ORD-1^SYNTH-LAB
OBR|1|PL-1^SYNTH-EMR|SYN-ORD-1^SYNTH-LAB|SYNTH-PANEL^Synthetic panel^L|||20261127091500+0000|||||||||||||||20261127101500+0000|||F
OBX|1|NM|2823-3^Potassium SerPl-sCnc^LN||6.9|mmol/L^^UCUM|3.5-5.1|HH|||F|||20261127091500+0000
BTS|1
FTS|1
```

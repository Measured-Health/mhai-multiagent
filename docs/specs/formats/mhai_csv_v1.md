# `mhai_csv_v1`: the scheduled file export format

**Status:** version 1, published 14 Nov 2026 (EMR PR 11). The format a hospital's lab, ward or pharmacy system exports for MHAI to read when it offers no FHIR API (EMR design §5.4). The hospital's IT maps their export to it. Read by the `file_export` connector (`services/emr/internal/connector/fileexport/`).

**Not confirmed with any hospital yet.** H1 (what the pilot hospital can export, and a synthetic or de-identified layout sample) is open. This version is built to the design's assumptions; if the pilot hospital's export differs, the change is a new version of this document and the parser, never a looser parser.

Synthetic examples of every file are in `services/emr/internal/connector/fileexport/testdata/sample/`.

## Strict by design

The reader fails closed. Anything this document doesn't allow is refused, never guessed:

- **A file whose shape is wrong is refused whole**, and nothing of it is stored: not UTF-8, a byte order mark or a NUL byte, a header that isn't exactly the kind's, a row with a different number of cells, broken quoting, a row without a usable id, the same id twice with different content, over 50 MB, a row over 8 KB, over 200,000 rows. The file is moved to `rejected/`, the connection shows `unreadable_data`, and the other files of the run still load.
- **A row whose values are wrong is kept as a dead letter**: a time without an offset, a status or code outside its list, a value that isn't a decimal, a required cell empty, a text cell longer than its limit. MHAI keeps the row's id and version (its provenance), not its content, counts it, and shows the connection `unreadable_data` for the platform team; the rest of the file loads. A corrected row exported again loads normally.

## Transport

- SFTP, key sign-in (Ed25519, ECDSA, or RSA of at least 2048 bits; no passphrase). The platform admin enters the host (`sftp://host[:port]`), the folder, the user name, the host key's SHA256 fingerprint (from the hospital's IT, out of band; any other key is refused at test and on every connection, never learned), the declared export interval and, optionally, the MRN identifier system.
- The hospital creates two subfolders in the folder: `processed/` and `rejected/`. MHAI moves each file it has loaded to `processed/` and each file it refused to `rejected/`. MHAI never deletes a file. The connection test checks the folder and both subfolders exist.
- MHAI polls the folder (every 5 minutes by default), at most 20 files a run.

## File names

`<kind>-<YYYYMMDD>T<HHMMSS>Z[-<n>].csv`, for example `results-20261114T060000Z.csv` or `results-20261114T060000Z-2.csv`. The time is the export's, in UTC. Files are read in time order, then kind order (`patients`, `encounters`, `results`, `vitals`, `medications`), then `n`. **Any other file in the folder is left alone**: never read, never moved, and reported to the platform team as a count (never by name), except names ending `.part` or `.tmp` or starting with a dot, which are taken as still being written. Write each file under such a name and rename it when complete. A file that is shorter than listed or doesn't end with a line break is left for the next poll and reported. A file whose name is already in `processed/` (a re-export under the same name) is moved beside it with a suffix, never over it.

A file name must never contain a patient's name or number.

## Common rules

- RFC 4180 CSV, UTF-8 without a byte order mark, comma separator, `"` quoting, LF or CRLF line endings. A header row, exactly as below (names, order and case).
- An empty cell means "not given".
- **Times:** ISO 8601 with seconds and an offset: `2026-11-14T05:10:00+01:00` or `2026-11-14T04:10:00Z` (fractions of a second allowed). A time without an offset is refused, never read in a guessed zone. A time more than 10 minutes in the future is refused later, when normalised.
- **Dates** (birth date only): `YYYY-MM-DD`.
- **Ids** (`source_*_id`, `replaced_by`; `mrn` at most 64): 1 to 128 characters of letters, digits and `. _ : / -`, starting with a letter or digit. An id is stable: the same thing keeps its id across files.
- **Codes** (`local_code`): 1 to 64 characters of letters, digits and `. _ : / + % -`, starting with a letter or digit. MHAI maps each local code to LOINC (results, vitals) or ATC (medications) through the connection's reviewed map; a code not in the map is kept and flagged "not standardised", never guessed.
- **Values:** a decimal with `.` as the decimal point, an optional leading `-`, and an optional comparator `<`, `<=`, `>`, `>=` (`<0.5`). No thousands separator, no exponent, no unit in the value, no spaces.
- **Units:** as the source writes them (`mmol/L`, `mg/dL`, `µmol/L`, `/min`), up to 40 bytes of UTF-8, no spaces or control characters, not starting with `=`, `+`, `-` or `@`; mapped to UCUM later.
- **Text** (names, labels): any text; markup and control characters are removed, and a cell starting with `=`, `+`, `-` or `@` is stored with a leading `'` so no spreadsheet runs it.
- **`updated_at`** (every kind, required): when the source last changed the row. Rows are versioned by content: the same row exported again is ignored; a changed row (an amended result) is a new version, and the newest `updated_at` wins.

## The files

### `patients`

| Column | Required | Notes |
|---|---|---|
| `source_patient_id` | yes | id |
| `updated_at` | yes | time |
| `mrn` | | the hospital's record number (id). Kept only when the connection names its MRN identifier system |
| `given_name`, `family_name` | | text, 60 each |
| `birth_date` | | date |
| `sex` | | `female`, `male`, `other`, `unknown` |
| `active` | | `true`, `false` |
| `replaced_by` | | the `source_patient_id` this record was merged into (proposed to the hospital admin, never merged automatically) |

### `encounters`

| Column | Required | Notes |
|---|---|---|
| `source_encounter_id` | yes | id |
| `source_patient_id` | yes | id |
| `updated_at` | yes | time |
| `class` | yes | `inpatient`, `outpatient`, `emergency` |
| `status` | yes | `planned`, `arrived`, `in-progress`, `finished`, `cancelled`, `entered-in-error` |
| `start_at`, `end_at` | | time |
| `source_location_id` | | id of the ward or clinic |
| `location_name` | with a location | text, 60 |
| `location_type` | | `ward`, `clinic`, `emergency`, `other` |
| `bed_label` | | text, 20 |

### `results` (laboratory)

| Column | Required | Notes |
|---|---|---|
| `source_result_id` | yes | id |
| `source_patient_id` | yes | id |
| `source_encounter_id` | | id |
| `collected_at` | yes | time the sample was taken |
| `resulted_at` | | time the result was issued |
| `updated_at` | yes | time |
| `status` | yes | `preliminary`, `final`, `amended`, `corrected`, `cancelled`, `entered-in-error` |
| `local_code` | yes | code |
| `local_name` | | text, 200 |
| `value` | | decimal (with an optional comparator) |
| `value_text` | | text, 120: a result that isn't a number (`Positive`). Not with `value` |
| `unit` | | unit |
| `ref_low`, `ref_high` | | decimal, no comparator |
| `ref_text` | | text, 120: the range as the lab writes it |
| `flag` | | `L`, `H`, `LL`, `HH`, `A`, `AA`, `N` (the lab's own; `LL`, `HH` and `AA` are critical) |

### `vitals`

| Column | Required | Notes |
|---|---|---|
| `source_vital_id` | yes | id |
| `source_patient_id` | yes | id |
| `source_encounter_id` | | id |
| `measured_at` | yes | time |
| `updated_at` | yes | time |
| `status` | yes | as `results` |
| `local_code` | yes | code |
| `local_name` | | text, 200 |
| `value` | | decimal |
| `unit` | | unit |
| `flag` | | as `results` |

### `medications`

| Column | Required | Notes |
|---|---|---|
| `source_medication_id` | yes | id |
| `source_patient_id` | yes | id |
| `updated_at` | yes | time |
| `kind` | yes | `request` (prescribed), `statement` (taken) |
| `status` | yes | `active`, `on-hold`, `stopped`, `completed`, `cancelled`, `entered-in-error` |
| `local_code` | one of the two | code |
| `local_name` | one of the two | text, 200 |
| `dose_value` | | decimal, no comparator |
| `dose_unit` | | unit |
| `route` | | text, 40 |
| `timing_text` | | text, 60 |
| `authored_at`, `start_at`, `end_at` | | time |

## What is never in a file

Addresses, phone numbers, email addresses, national or insurance numbers, next of kin, practitioners' names, free-text notes and comments. MHAI has no column for them (design §8.3, §14.1).

## Staleness

The hospital declares how often it exports (the connection's "expected every" minutes, 5 minutes to a week). A connection with no new file loaded for longer than that plus 50%, or with no successful folder check for three poll intervals, shows as behind to clinicians and hospital admins and as errors to the platform team (design §15.4).

## Where this departs from the design's example

Design §5.4 gives the `results` columns as an example. Version 1 adds `updated_at` to every kind (to order versions; the design's example has none), `source_encounter_id` to results and vitals (the ward a result belongs to), and `value_text` to results (a result that isn't a number would otherwise be refused). Plan departure 62 records these.

# PII masker: evaluation on a synthetic Nigerian PII set

**Status:** built and measured (17 Nov 2026). **Not signed off.** The real-data gate's item (1) in
TODO.md is the privacy lead's sign-off (H2); this document is the evidence for it, not the
sign-off. Item (4), core's keyed name-token hashes, is still open as well.

## What changed

`PIIMasker` (`services/intelligence/intelligence/security/hipaa.py`) found names only after a
title, so "Adaeze Okafor has chest pain" was sent to a cloud provider unmasked (ruling C8). It now
also runs `security/ng_pii.py`:

- **Names without a title.** A hand-written gazetteer of 1,479 Nigerian given names and surnames
  (Yoruba, Igbo, Hausa and Fulani, Kanuri, Ijaw and Kalabari, Edo and Esan, Efik, Ibibio and
  Annang, Tiv, Idoma, Igala, Nupe, Urhobo, Isoko, Itsekiri, Plateau and Ogoni names, and
  Christian, Muslim and English given names), plus 101 names that are also ordinary words
  (Grace, Hope, Sunday, Victor). Context rules: Nigerian titles and relationship words
  (Alhaji, Mama, Chief, "her daughter", "Patient is"); an unknown capitalised word beside a
  known name; an initial before a name; name-forming affixes (Oluwa-, Chukwu-, -funmi);
  ordinary-word names only in context (beside a name, after a title, capitalised inside a
  sentence, or opening one with a verb after it).
- **A name-likeness score** for words in neither list: two character-trigram models, one built
  from the gazetteer and one from 66,070 English and medical words, compared per letter. Both
  are built from the two pinned files when they load; there are no other weights. The threshold
  (0.0 capitalised, 0.75 lower case) was set by cross-validation on the files alone
  (`masker_eval.calibrate()`), never on the evaluation set.
- **Numbers.** NIN and BVN (11 digits), phone numbers (0 or +234, with the "(0)" trunk), whole or
  grouped with spaces, hyphens, dots or brackets; labelled record numbers (MRN, MR#, hospital,
  folder, card, file, patient or registration number, NHIS, NIN, BVN) with whatever code
  follows; coded record numbers ("LUTH/2019/12345", "HAV-000123"), read whole however long.

**Offline and CPU-only.** No model, no download, no new dependency. Image impact: two text files,
`ng_names.txt` (16 KB) and `en_words.txt` (617 KB), installed as package data. Loading builds the
two models once (well under a second).

**Fails closed.** Both files are pinned by SHA-256 (`GAZETTEER_SHA256`, `WORDS_SHA256`). A missing,
unreadable or altered file, or a text longer than 65,536 characters, raises `DetectorUnavailable`.
`PIIMasker.mask` lets it through, so on the cloud path the job can't collect its identifiers or
mask its question, and a body that reaches the scan anyway is refused (`scan_error`). A failure
is not cached: the next call tries again and fails again.

**Licences.** The gazetteer is written for this repository (MIT); no list was copied. The English
words are derived from SCOWL (wamerican, size 50 and below); its permission notice is kept in the
file's header, as its licence requires.

## How it was measured

`services/intelligence/scripts/masker_eval.py` (logic in `intelligence/security/masker_eval.py`;
`tests/security/test_masker_eval.py` keeps the numbers from sliding back). Everything is generated
from a fixed seed (20261116): invented combinations of common names, random digits, and clinical
questions with no identifier in them. No real person or record is used.

- **3,090 name mentions** in 26 question templates and 4 record-history templates, in every
  position (start, middle, end, possessive, after a title, two names in one question); full name,
  surname first ("OKAFOR Adaeze"), given name only, surname only, with a middle name, initial and
  surname; title case 70%, capitals 15%, lower case 15%. 1,860 of them contain at least one word
  that is **not** in the gazetteer ("held out"), so the context rules and the score are measured,
  not just the list.
- **4,000 identifiers**, 1,000 each of NIN, BVN, phone and record number, in the formats above.
- **676 questions with no identifier**: 600 generated clinical questions (vitals, labs, doses,
  Nigerian places, brand names and towns the word list doesn't hold, weekdays and months,
  eponyms such as Burkitt lymphoma) and the 76 questions of the clinical answer dev set
  (`testdata/answers/dev/items.jsonl`), each with guideline chunks, including chunks that use
  ordinary-word names ("grace period", "Peace of mind") and brands and towns.

**Recall** counts an identifier as found when the masker's detections cover every letter and digit
of it. **Egress recall** runs the cloud path as the job does (identifiers collected from the raw
question and record, the masked question in the body, then `scan_body`): found when the scan
refuses or the identifier is gone from what would be sent. **False refusal** is a question with
no identifier that the scan refuses. **Over-masking** is such a question that the masker changed.

## Results

| Class | n | Masker recall | Egress recall | Before (masker / egress) |
|---|---:|---:|---:|---:|
| NAME | 3,090 | 0.9883 | 0.9909 | 0.0214 / 0.2155 |
| NIN | 1,000 | 1.0000 | 1.0000 | 0.0240 / 0.8450 |
| BVN | 1,000 | 1.0000 | 1.0000 | 0.0000 / 0.8220 |
| PHONE | 1,000 | 1.0000 | 1.0000 | 0.0000 / 1.0000 |
| MRN | 1,000 | 1.0000 | 1.0000 | 0.0410 / 0.8560 |
| All numeric | 4,000 | 1.0000 | 1.0000 | 0.0163 / 0.8808 |

"Before" is the same set with the new rules switched off (`masker_eval.py --baseline`).

Names by slice (masker recall): names with a held-out word 0.9892 (1,860); names fully in the
gazetteer 0.9870 (1,230); title case 0.9922, capitals 0.9911, lower case 0.9678. By group: Middle
Belt 1.0000, Edo 0.9973, Hausa 0.9973, Urhobo and Itsekiri 0.9972, Ijaw 0.9972, Igbo 0.9949, Efik
and Ibibio 0.9897, Yoruba 0.9730, Muslim given names alone 0.9143 (35), Christian and English
given names alone 0.8391 (87).

**False-refusal rate: 0.0030** (2 of 676, both `known_identifier`: an unknown town read as a name
in the question, and a guideline chunk naming the same town). Before: 0.0000.
**Over-masking: 0.1509** (102 of 676): almost all are brand names and towns outside the word list
("Lumartem", "Okene") masked as names. The question is still sent, with those words replaced.
**Questions naming a patient that were refused: 0.0017** (5 of 3,090, all `known_identifier`: an
ordinary-word name such as Patience or Comfort found in a guideline chunk; TODO.md's known
over-block). The masker finds nothing in its own output (0 of 7,766 masked texts), so the scan's
second look never refuses a masked question.

## Against the bar

- **Numeric identifiers ≥ 0.999: met** (1.0000 by the masker and on the egress path).
- **Names ≥ 0.99: met on the egress path (0.9909), not by the masker alone (0.9883).** The 36
  names the masker misses are mostly one word in lower case that is also an ordinary word or
  a weekday ("in victor", "for Sunday", "patience known sickle cell"), short unknown words
  ("Qudus", "uwem"), and a few unknown lower-case names. Catching those would mask ordinary
  words in every question. The privacy lead decides whether the egress figure, or a different
  mix of name forms, is the right reading of the bar.

## Known limits

- The set is synthetic and its mix of forms is a choice (15% lower case, given name alone in
  15%); real questions will differ. The held-out slice shows how names the list doesn't know
  fare, but it was written by the same people as the list.
- Names that are ordinary words, alone and in lower case, are missed by design.
- Unknown capitalised words that look like names (brands, towns) are masked; a guideline chunk
  that names the same town or brand then refuses the question (`known_identifier`).
- An unlabelled 6-digit number written with a space ("445 812") is still not caught unless a
  label precedes it (TODO.md's residual list).
- Names in a script with no Latin form, numbers written as words, and digits spelt out with
  gaps the rules don't join are not covered (the scan's residual list stands).

## Security review fixes before the PR

- Bracketed text: only the masker's own placeholders, exactly, are skipped as words;
  "[ADAEZE] [OKAFOR]" is masked.
- Worst-case input: the name spread is two linear passes with no cap on how far a name extends
  (a cap would leave a long name's outer parts unmasked); coded record numbers are matched as one
  token with no backtracking and masked whole however long; text over 65,536 characters is
  refused. Inputs at the cap finish well within the time budget in the tests.

## Reproduce

```bash
cd services/intelligence
uv run python scripts/masker_eval.py             # the table above
uv run python scripts/masker_eval.py --baseline  # the masker before this change
uv run python scripts/masker_eval.py --json      # everything, with example misses
uv run pytest tests/security/test_masker_eval.py tests/security/test_ng_pii.py
```

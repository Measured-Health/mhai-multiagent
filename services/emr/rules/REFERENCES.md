# Rule set v1: references

**Pilot, clinical sign-off deferred.** The product owner deferred the formal
clinical sign-off for the pilot (README "Pilot mode"). Rule set v1 is a
small, conservative starter set of widely used critical-value limits. It
stays in shadow until a platform admin activates it for a hospital, and
the clinical lead (H4) reviews it before that. Every threshold below cites
its source. The reviewer checks each value against that source and the
hospital's own laboratory's critical list before activation.

`ruleset-1.yaml` has the thresholds, `testdata/golden/v1/` the golden cases
(each edge, the normal side, conversions, flags, statuses and causes).
`emr rules check ruleset-1.yaml` prints the version and SHA-256 a platform
admin activates.

## Sources

- **RCPath G158 (2017).** The Royal College of Pathologists, *The
  communication of critical and unexpected pathology results* (G158),
  October 2017, the appendix of alert thresholds for clinical biochemistry
  and haematology.
- **Kost 1990.** Kost GJ. *Critical limits for urgent clinician
  notification at US medical centers.* JAMA 1990;263(5):704-707.
- **ESC 2020.** Collet JP, Thiele H, Barbato E, et al. *2020 ESC
  Guidelines for the management of acute coronary syndromes in patients
  presenting without persistent ST-segment elevation.* Eur Heart J
  2021;42(14):1289-1367. The 0 h/1 h algorithm, assay-specific cut-offs.
- **NEWS2 (2017).** Royal College of Physicians. *National Early Warning
  Score (NEWS) 2: standardising the assessment of acute-illness severity in
  the NHS.* RCP, December 2017. Chart 1, the scoring system.
- **HL7 FHIR R4** ObservationInterpretation codes (`LL`, `HH`, `AA`): the
  reporting laboratory's own critical flags.

## Thresholds

All values are in the analyte's canonical UCUM unit. A source value in
another unit is converted by the normaliser's reviewed conversions first; a
value it can't convert never reaches a rule.

| Rule | Analyte (LOINC) | Limit | Severity | Source |
|---|---|---|---|---|
| `potassium-critical-low` | potassium (2823-3, 6298-4) | ≤ 2.5 mmol/L | critical | RCPath G158; Kost 1990 |
| `potassium-critical-high` | potassium | ≥ 6.5 mmol/L | critical | RCPath G158; Kost 1990 |
| `sodium-critical-low` | sodium (2951-2, 2947-0) | ≤ 120 mmol/L | critical | RCPath G158 |
| `sodium-critical-high` | sodium | ≥ 155 mmol/L | critical | RCPath G158 |
| `glucose-critical-low` | glucose (2345-7, 2339-0, 14749-6, 15074-8) | ≤ 2.5 mmol/L | critical | RCPath G158; Kost 1990 |
| `glucose-critical-high` | glucose | ≥ 25.0 mmol/L | critical | RCPath G158; Kost 1990 |
| `haemoglobin-critical-low` | haemoglobin (718-7) | ≤ 6.0 g/dL | critical | Kost 1990; RCPath G158 |
| `platelets-critical-low` | platelets (777-3) | ≤ 20 ×10⁹/L | critical | RCPath G158; Kost 1990 |
| `platelets-critical-high` | platelets | ≥ 1000 ×10⁹/L | critical | RCPath G158; Kost 1990 |
| `creatinine-critical-high` | creatinine (2160-0, 14682-9) | ≥ 354 µmol/L | critical | RCPath G158 |
| `troponin-t-hs-critical-high` | hs troponin T (67151-1) | ≥ 52 ng/L | critical | ESC 2020 (hs-cTnT rule-in) |
| `spo2-critical-low` | SpO2 (59408-5) | ≤ 91 % | critical | NEWS2, Scale 1 scores 3 |
| `temperature-critical-low` | temperature (8310-5) | ≤ 35.0 °C | critical | NEWS2 scores 3 |
| `temperature-urgent-high` | temperature | ≥ 39.1 °C | urgent | NEWS2 scores 2 |
| `systolic-bp-critical-low` | systolic BP (8480-6) | ≤ 90 mmHg | critical | NEWS2 scores 3 |
| `systolic-bp-critical-high` | systolic BP | ≥ 220 mmHg | critical | NEWS2 scores 3 |
| `heart-rate-critical-low` | heart rate (8867-4) | ≤ 40 /min | critical | NEWS2 scores 3 |
| `heart-rate-critical-high` | heart rate | ≥ 131 /min | critical | NEWS2 scores 3 |
| `lab-critical-flag` | any, mapped or not | the source's `LL`, `HH` or `AA` | critical | design §11.1, §21 Q2 |

Choices the reviewer should confirm:

- **Vital signs use the NEWS2 single-parameter red score** (3 points), which
  NEWS2 says needs an urgent review, as MHAI's `critical`. A NEWS2 score is
  not computed in v1: it needs consciousness level and air or oxygen
  recorded with every set (the engine supports it; see the synthetic set).
- **SpO2 is Scale 1.** A patient on Scale 2 (hypercapnic respiratory
  failure) with a target of 88–92% will trigger `spo2-critical-low`. The
  shadow review counts how often.
- **Creatinine ≥ 354 µmol/L** also fires for patients with known renal
  failure. RCPath excludes them; MHAI can't tell them apart yet.
- **Troponin is the Roche hs-cTnT assay's rule-in value.** Other troponin
  assays (hs-cTnI and the rest) have their own cut-offs and no rule here.
- **One finding per value.** When two limits of one analyte hit, only the
  most severe is raised. The lab's own critical flag is raised as well,
  unless MHAI's own critical threshold already hit.
- **The unmapped category.** A flagged laboratory result with no map is
  shown under blood chemistry, and a flagged vital sign under vital signs.
  The app marks both "Not standardised".
- **Freshness windows** are the design's defaults: 72 hours for labs and 6
  hours for vital signs. An older or date-only value makes a historical
  finding, which is stored but never alerts.

## Likely causes

Each suggested cause is at most `review` confidence, never `high` (design
§21 question 3). A cause is shown only when the patient's own active
medicines support it:

| Rule | Cause | Confidence | Evidence (active medicine, WHO ATC) |
|---|---|---|---|
| `potassium-critical-high` | Potassium-raising medicine | review | C03DA, C09A, C09B, C09C, C09D, A12BA |
| `sodium-critical-low` | Thiazide diuretic | caution | C03A, C03EA |
| `glucose-critical-low` | Insulin or sulfonylurea | review | A10A, A10BB |

## What v1 needs before it can fire on real data

- **The real LOINC and ATC tables (Task 21, H6).** Until then, the code
  tables are synthetic, refused outside development, and v1's numeric rules
  have no conversion row. The emr service logs how many of v1's codes the
  conversions don't list. The golden tests run v1 through a fixture of the
  same conversions (`testdata/v1-fixture/`, test only).
- **A reviewed `maps/common/conversions.yaml`** with v1's analytes and the
  factors in the fixture: glucose 1 mg/dL = 0.0555 mmol/L, creatinine
  1 mg/dL = 88.4 µmol/L, haemoglobin 10 g/L = 1 g/dL, platelets
  10³/µL = 10⁹/L, and the exact ones: potassium and sodium 1 mEq/L =
  1 mmol/L, troponin 1 pg/mL = 1 ng/L. The emr service refuses to start
  with `EMR_RULE_SET_ACTIVE=1` while any of v1's codes lacks a conversion,
  and counts a mapped value it can't evaluate (`NotEvaluable`).
- **Activation:** the shadow review (design §11.5), then a platform admin's
  `PUT /platform/hospitals/{id}/emr-rule-set` (step-up, audited
  `emr.rule_set_switched`) with the version and hash, and
  `EMR_RULE_SET_ACTIVE=1` in the deployment. Without both, every finding is
  shadow.

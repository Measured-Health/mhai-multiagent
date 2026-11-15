# Escalation guard: version history and open points

The escalation guard is a fixed, versioned list of rules. It reads a clinician's question, plus any structured patient values, before an answer is shown. If a rule matches, MHAI does not answer. It **escalates**: it tells the clinician to involve a senior clinician or follow the emergency pathway. The guard applies to every answer route: cloud models, the hospital's own model, and the model gate.

- **Rule file:** `services/intelligence/intelligence/guardrails/rules/escalation-guard.yaml`. Its SHA-256 is pinned in code, so an edited file refuses to load.
- **Design record:** ADR 0017.
- **Clinical sign-off:** pending. The pilot list is in the review pack, `docs/clinical/2026-11-15-pilot-clinical-review.md`.

## Why it exists (14 Nov 2026)

The answer check (NLI) confirms that each sentence of an answer is supported by the guideline it cites. It does not check that the answer is the *right response* to the question.

Fixing a bug where citation tags hid every supported sentence from the check (PR #185) exposed this gap. Measured on the synthetic practice set, an answer that just recited an on-topic guideline then passed on all 36 questions that must go to a clinician. These were red flags, missing patient values, and doses the guideline doesn't give.

The guard closes that gap. It ships in the same PR as the bug fix (#187), so the fix never reaches clinicians without it.

## Versions

### 1.0.0 (14 Nov 2026), first pilot list

- **Red flags:** glycaemia (>400 or <54 mg/dL, DKA/HHS), chest pain with risk signs, stroke, sepsis (qSOFA), shock and low blood pressure, severe hypertension (≥180/120 mmHg), heart-rate and SpO2 limits, airway and anaphylaxis, altered consciousness (GCS), haemoglobin <5 g/dL, severe malaria, danger signs in pregnancy and after birth, danger signs in children (WHO IMCI), fever in young infants, neonatal signs, jaundice in the first 24 hours, overdose or poisoning, and suicidality.
- **Missing information:** going home, severity, a diagnosis or a decision to treat, and a child's dose without a weight.
- **Out of scope:** a dose, a change or a threshold that no cited guideline gives.
- **Recital:** an answer that only copies guideline text without addressing the question is sent back.
- **Results on the practice set:**
  - All 36 must-escalate questions escalate; 2 of 40 answerable questions over-escalate (5%).
  - The rules were first written without looking at the practice set: 26 of 36 escalated. They were then improved by type of question, never by copying test wording.
  - The locked test set was never read.

### 1.0.1 (14 Nov 2026), housekeeping

No rule changed. The design record was renumbered from ADR 0016 to 0017, because 0016 belongs to the documents service. That changed the file's hash, so the pin was updated.

### 1.0.2 (15 Nov 2026): children's rules aligned to WHO IMCI

Found while writing the clinical review pack.

| Change | Before | After | Why |
|---|---|---|---|
| Fast breathing, under 2 months | ≥60/min | ≥60/min | No change (WHO IMCI) |
| Fast breathing, 2–12 months | ≥60/min | **≥50/min** | WHO IMCI. Fast breathing in infants was missed |
| Fast breathing, 1–5 years | ≥60/min | **≥40/min** | WHO IMCI. Fast breathing in toddlers was missed |
| Fast breathing, under 5, age unknown | ≥60/min | **≥40/min** | The most protective band |
| Young-infant danger signs | birth to 28 days | **birth to 2 months** | WHO IMCI covers the young infant to 2 months: low temperature, yellow palms and soles, umbilical infection, poor feeding. A 6-week-old with yellow palms was missed |
| A structured age under 1 year (e.g. 0.5) | ignored by age-based rules | read; 0 means "under a year, month unknown" | A code gap hid infants from every age-banded rule |

The rule list now has 35 rules in 13 groups. Over-escalation on the practice set is unchanged at 5% (2 of 40), and all 36 must-escalate questions still escalate.

**Checked and left as they are:**
- fever in young infants (≥37.5 °C up to 3 months, stricter than WHO IMCI and NICE);
- extreme temperature at any age;
- haemoglobin <7 g/dL for under-fives (matches WHO);
- SpO2 91% or below (stricter than IMCI's <90%);
- heart rate under 60/min in under-fives (from PALS).

## Open points for the clinical reviewer

1. **Jaundice in the first 24 hours of life is escalated.** WHO IMCI and NICE CG98 treat it as urgent. The practice set expected an answer ("pathological") instead. The guard keeps the safer behaviour, so this is the cause of the 2 over-escalations.
2. **No fast-breathing rule for children aged 5–12.** It's outside IMCI. A clinician should say whether one is wanted.
3. **The whole list is a pilot list.** Every rule needs Approve / Change / Remove in the review pack, signed by the clinical lead and a second clinician.

## How a change is made

1. Edit the rule file.
2. Bump its version.
3. Re-pin its SHA-256 in code.
4. Add a test for each changed rule (one case that must escalate, one that must not).
5. Re-run the practice-set measure, and record the version here.

Every answer records the rule-set version it was checked against (`escalation-guard@<version>#<sha12>`), so each decision can be traced to the rules in force.

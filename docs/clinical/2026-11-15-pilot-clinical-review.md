# Pilot clinical review: red flags, alert wording, lookup limits

**For:** the clinical lead (and a second clinician for Part 1).
**Date prepared:** 15 Nov 2026.

MHAI is starting a supervised pilot with one hospital. Clinicians ask
questions about their patients and get answers drawn from clinical
guidelines, and they receive alerts about abnormal results from the
hospital's record system. On 12 Nov 2026 the product owner decided to start
the pilot before the formal clinical sign-off was done, and to collect that
sign-off during the pilot (README "Pilot mode", PR #134). The safety checks,
alert wording and lookup limits below were built under that decision. This
pack asks you to check them. For each row, mark **Approve**, **Change** or
**Remove** and add a note if needed. You don't need to write code: we make
every change by editing the rule file or the wording, and we send the change
back to you.

**A word used below.** To **escalate** means MHAI does not answer the
question itself. It sends the question to a clinician instead, with a note
saying why.

---

## Part 1. Red-flag list (escalation guard, version 1.0.1)

Source: `services/intelligence/intelligence/guardrails/rules/escalation-guard.yaml`
(PR #187). If a question or the patient's current data shows any sign
below, MHAI escalates. A rule can only send a question to a clinician. No
rule can make an answer go out.

**How the signs are read:**

- MHAI reads the question and the patient's recorded symptoms, vitals and
  labs. It reads English text only. Misspellings may be missed.
- A number with its label counts ("SpO2 86%", "BP 78/40",
  "glucose 31 mmol/L"). Units are converted (for glucose, mmol/L × 18 =
  mg/dL). Where a number is given, it decides, not a vague word such as
  "low BP".
- Negation is understood over a few words: "denies chest pain" does not
  count.
- **Adult** means age 12 or over, or no age given and no word for a child.
  **Under five** includes newborns. **Newborn** means under 28 days.
  **Young infant** means under 3 months. **Fever** means the word, or a
  temperature of 38.0 °C or more.

| # | Rule id | Group | What triggers it | Source cited | Approve / Change / Remove | Notes |
|---|---|---|---|---|---|---|
| 1 | glucose-severely-high | Glucose | Glucose >400 mg/dL (22.2 mmol/L) | Pilot threshold; ADA 2025 s.16 (DKA/HHS) | | |
| 2 | glucose-low | Glucose | Glucose <54 mg/dL (3.0 mmol/L) | ADA 2025 s.6, level 2 hypoglycaemia | | |
| 3 | hyperglycaemic-emergency-words | Glucose | DKA, ketoacidosis, HHS, hyperosmolar, Kussmaul breathing, ketones high or positive, fruity or acetone breath | ADA 2025 s.16 | | |
| 4 | glucose-extreme-words | Glucose | Meter reads HI, LO or error; sugar "too high to read", "off the scale". Not used when a glucose number is given | Meter range limit | | |
| 5 | hypoglycaemia-with-neuro | Glucose | Low sugar with confusion, drowsiness, seizure, collapse or similar | ADA level 3 hypoglycaemia | | |
| 6 | chest-pain-with-risk-signs | Chest pain | Chest pain, pressure or tightness **plus** any of: radiation to jaw or arm, sweating, breathlessness, faint, nausea, on exertion or at rest, palpitations, pale or grey, troponin or ST changes; or systolic <90 mmHg; or heart rate >120/min; or age 40 or over | NICE CG95; ESC 2023 ACS | | |
| 7 | acs-words | Chest pain | Heart attack, MI, STEMI, ST elevation, ACS, raised troponin, aortic dissection, tearing pain | ESC 2023 ACS | | |
| 8 | stroke-signs | Stroke | Stroke, TIA, facial droop, slurred speech, word-finding difficulty, one-sided weakness or numbness, arm drift, sudden loss of vision, sudden severe headache or confusion, "worst headache", thunderclap | BE-FAST; NICE NG128 | | |
| 9 | sepsis-words | Sepsis | Sepsis, septic, mottled skin, non-blanching or petechial rash, purpura, meningitis, neck stiffness, bulging fontanelle, necrotising infection | NICE NG51 high-risk criteria | | |
| 10 | fever-with-danger-signs | Sepsis | Fever **plus** any of: altered consciousness, low BP, cold hands or feet, no urine, fast or hard breathing, breathlessness, rigors; or (adults) systolic ≤100 mmHg, RR ≥22/min or HR >130/min | qSOFA; NICE NG51 | | |
| 11 | temperature-extreme | Sepsis | Temperature ≥40.0 °C or ≤35.0 °C; hypothermia; heat stroke | NEWS2 | | |
| 12 | severe-malaria-features | Sepsis | Malaria **plus** any of: altered consciousness, respiratory distress, prostration, jaundice, dark or cola urine, spontaneous bleeding, pulmonary oedema, severe anaemia, acidosis, parasitaemia >10%, glucose <40 mg/dL (2.2 mmol/L) | WHO malaria guidelines 2023 | | |
| 13 | shock | Circulation | Shock, hypotension, "BP very low", "pressure dropping". Not used when a systolic number is given | NEWS2 | | |
| 14 | systolic-low | Circulation | Adults: systolic <90 mmHg. Any age: systolic <70 mmHg | NEWS2; PALS | | |
| 15 | hypertensive-severe | Circulation | Systolic ≥180 mmHg or diastolic ≥120 mmHg; hypertensive crisis, emergency or urgency; malignant hypertension | ACC/AHA 2017 | | |
| 16 | heart-rate-extreme | Circulation | Adults: HR >130/min or <40/min. Any age: HR >180/min. Under five: HR <60/min | NEWS2; PALS | | |
| 17 | haemoglobin-severe | Circulation | Hb <5.0 g/dL at any age; Hb <7.0 g/dL in pregnancy or under five | WHO severe anaemia | | |
| 18 | severe-bleeding | Bleeding | Vomiting or coughing blood, melaena or black tarry stool, heavy or uncontrolled bleeding, soaking pads, postpartum haemorrhage, passing clots | NICE NG141; WHO PPH | | |
| 19 | anaphylaxis | Anaphylaxis | Anaphylaxis, angioedema, swelling of throat, tongue, lips or face, stridor | Resus Council UK 2021 | | |
| 20 | breathing-severe | Breathing | Can't breathe or speak in sentences, severe breathlessness or asthma, respiratory distress or failure, cyanosis or blue lips, gasping, grunting, chest in-drawing, silent chest, apnoea, choking | WHO IMCI; BTS/SIGN asthma; NEWS2 | | |
| 21 | spo2-low | Breathing | SpO2 ≤91% | NEWS2 (≤91%); WHO IMCI (<90%) | | |
| 22 | respiratory-rate-extreme-adult | Breathing | Adults: RR ≥25/min or ≤8/min | NEWS2 | | |
| 23 | respiratory-rate-infant | Children | Under five: RR ≥60/min. *Note: WHO IMCI uses lower cut-offs for older children (≥50 at 2–12 months, ≥40 at 1–5 years); this rule uses 60 for all under-fives.* | WHO IMCI | | |
| 24 | consciousness-altered | Neurology | Confusion, disorientation, delirium, unconscious or unresponsive, drowsy, lethargic, hard to wake, collapse, fainting, seizure or fits, slurred speech; or GCS ≤14 | NEWS2 (new confusion); WHO IMCI | | |
| 25 | severe-headache-or-neuro | Neurology | Worst headache, thunderclap or sudden severe headache, head injury, paralysis | NICE NG232; NICE CKS headache | | |
| 26 | pregnancy-danger-signs | Pregnancy | Pregnant or postpartum **plus** any of: bleeding or spotting, severe headache, blurred vision or flashing lights, fits, severe or upper abdominal pain, reduced or absent fetal movements, swollen face or hands, waters broken, preterm labour, foul discharge, protein in urine, breathlessness, chest pain, calf pain or swelling, fever, altered consciousness; or BP ≥160/110 mmHg | WHO maternal danger signs; NICE NG133 | | |
| 27 | pregnancy-hypertension | Pregnancy | Pregnant **plus** BP ≥140/90 mmHg, or pre-eclampsia, eclampsia or HELLP | NICE NG133 | | |
| 28 | child-danger-signs | Children | Under five **plus** any of: can't drink or feed, vomits everything, floppy, lethargic, hard to wake, sunken eyes or fontanelle, slow skin pinch, bulging fontanelle, no urine or wet nappies, high-pitched or inconsolable cry, fits, severe dehydration, pneumonia or malnutrition, chest in-drawing, grunting | WHO IMCI 2014 general danger signs | | |
| 29 | neonate-danger-signs | Children | Newborn **plus** any of: fever, temperature <35.5 °C, jaundice on palms or soles, red, pus or swelling at the umbilicus, many pustules, cold to touch, poor feeding; or bilirubin ≥20 mg/dL (342 µmol/L) | WHO IMCI young infant | | |
| 30 | young-infant-fever | Children | Under 3 months **plus** fever, or temperature ≥37.5 °C | NICE NG143 (≥38 °C); WHO IMCI (≥37.5 °C) | | |
| 31 | neonatal-jaundice-first-day | Children | Jaundice, yellow skin or eyes, or bilirubin mentioned, **and** age under 24 hours, or "first day", "at birth", "since birth". **See the disagreement below.** | WHO IMCI; NICE CG98 | | |
| 32 | overdose-poisoning | Poisoning | Overdose, took too many or a whole pack, swallowed pills or chemicals, poisoning, drank bleach, kerosene or pesticide, snake bite, scorpion sting, dog bite | WHO poisoning management | | |
| 33 | suicidality-self-harm | Mental health | Suicide, wants to die, self-harm, harming self or others, "better off dead", voices telling them to harm | NICE NG225; WHO mhGAP | | |

**The one known disagreement (rule 31).** In our practice question set,
two questions describe jaundice that began at 13 and 14 hours of life. The
set expects MHAI to answer them ("this is pathological"). The rule instead
escalates them, because WHO IMCI and NICE CG98 say jaundice in the first
24 hours needs urgent assessment. We kept the rule as the more cautious
choice. Please decide: **keep** (escalate), or **remove** (let MHAI answer).

Decision on rule 31: ☐ Keep  ☐ Remove  Notes: ______________________

### Missing information: MHAI escalates when the question needs facts it wasn't given

These fire only when the question is about *this* patient ("should she go
home?"), not a general question ("when should a patient be admitted?").

| # | Type | The question asks… | MHAI escalates unless it has… | Approve / Change / Remove | Notes |
|---|---|---|---|---|---|
| M1 | Going home or admission | Whether to send home, discharge, admit or refer | At least one current vital sign: systolic BP, heart rate, RR, SpO2 or temperature | | |
| M2 | Severity | How severe, serious or complicated this patient's illness is | At least one value or finding that defines severity (e.g. Hb, glucose, a vital sign, bilirubin, GCS, creatinine, HbA1c, parasitaemia; an RDT or smear result; or a sign such as convulsions, jaundice, bleeding) | | |
| M3 | Diagnosis or decision to treat | Whether this patient has, meets criteria for, or needs treatment for diabetes, hypertension, malaria, jaundice, anaemia, kidney disease, infection or pre-eclampsia | A value the definition uses (e.g. glucose, HbA1c, BP, Hb, bilirubin, creatinine, temperature, age in hours), or a test result (RDT, smear, OGTT, two separate readings), or when the problem started | | |
| M4 | A child's dose | A dose, amount or regimen for a child under five | The child's weight | | |

### Dose scope: MHAI escalates a dose it can't back with a guideline

| # | Rule | In plain words | Approve / Change / Remove | Notes |
|---|---|---|---|---|
| D1 | Dose, regimen or titration | If the question asks for a dose, regimen, titration or a change in dose, MHAI must cite a guideline passage that names that drug with a dose. With no drug named, the passage must give a dose for the same condition. If it can't, it escalates. | | |
| D2 | Treatment threshold | If the question asks "at what level should we start, give or transfuse", a cited passage must give a number for it, or MHAI escalates. | | |
| D3 | Tests are not doses | A test load such as "75 g OGTT" is not treated as a dose. | | |

---

## Part 2. Alert wording in the clinical app

Source: mhai-web, `apps/clinical/lib/notifications/alert-view.ts` and
`components/notifications/alerts-view.tsx` (PR #86). Text in `{braces}` is
filled in by the app. Times read "08:42" for today, "16 Oct 08:42" before.

| # | When it shows | Exact wording on screen | Approve / Change / Remove | Notes |
|---|---|---|---|---|
| A1 | The result was **corrected (amended)** in the hospital record after the alert | "Changed in the record at {time} (was {old value})." The time or old value is left out if the record doesn't give it. | | |
| A2 | MHAI **withdrew** the alert because the result was changed | "Withdrawn · {time} · The result was changed in the record" | | |
| A3 | MHAI withdrew the alert because the result was withdrawn | "Withdrawn · {time} · The result was withdrawn in the record" | | |
| A4 | MHAI withdrew the alert because the patient was excluded | "Withdrawn · {time} · The patient was taken out of record alerts" | | |
| A5 | The result is **preliminary** | "Preliminary result: it may still change." | | |
| A6 | The result **arrived late** | "This result reached MHAI late." | | |
| A7 | The patient has **no MRN** | MRN: "Not recorded" | | |
| A8 | A **connected system is behind** (paused, or silent for three checks) | Title: "Records may be out of date". Text: "{System} hasn't updated recently, so alerts may be missing." With more than one: "{A}, {B} and {C} haven't updated recently, so alerts may be missing." | | |

---

## Part 3. Patient lookup limits

Source: PR #172 (merged); `services/core/README.md`; TODO.md "Clinician
patient lookup by MRN". A clinician types a patient's full MRN to ask about
a patient who isn't in one of their alerts. Only an exact match works (no
partial or name search). Every lookup is recorded in the audit log.

**How it works today:**

- Any clinician of the hospital can look up any active patient in that
  hospital. There is **no check** that the clinician is treating the
  patient.
- The clinician must be signed in with two-step sign-in. There is **no
  fresh two-step code** for each lookup.
- There is **no reason field**.
- Lookups that find no patient count toward the limits too, so nobody can
  guess MRNs one after another.
- Past a limit, the lookup is refused. A clinician's first refusal of the
  day is recorded in the audit log.

| # | Limit | Today | Setting name | Keep / Tighten / Loosen (give number) | Notes |
|---|---|---|---|---|---|
| L1 | Per clinician, per minute | 5 | `CORE_PATIENT_LOOKUP_PER_MINUTE` (allowed 1–100) | | |
| L2 | Per clinician, per day | 50 | `CORE_PATIENT_LOOKUP_PER_DAY` (allowed 1–1000) | | |
| L3 | Per hospital, per day | 1000 | `CORE_PATIENT_LOOKUP_HOSPITAL_PER_DAY` (allowed 1–100000) | | |

| # | Question | Your answer | Notes |
|---|---|---|---|
| Q1 | Should a clinician give a reason for each lookup? | ☐ Yes  ☐ No | |
| Q2 | Should lookups be limited to patients the clinician is treating (for example, an encounter on their unit)? | ☐ Yes  ☐ No, any clinician of the hospital | |
| Q3 | Should each lookup ask for a fresh two-step code? | ☐ Yes  ☐ No | |

---

## Sign-off

By signing, you approve the items marked Approve. We will make the changes
and removals you marked and send them back for a second look.

**Clinical lead**

Name: ______________________________

Signature: __________________________  Date: ______________

**Second clinician (Part 1)**

Name: ______________________________

Signature: __________________________  Date: ______________

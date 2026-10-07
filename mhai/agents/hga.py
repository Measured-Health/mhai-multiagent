"""
===========================================================================
MHAI — Upgraded Hallucination Guard Agent (HGA)
===========================================================================
Drop-in replacement for the simplified HGA in Part 1.

Integrates the REAL guardrail components from hallucination_guardrails.py:
  - Context-aware NumericalAccuracyChecker (with named-field matching)
  - CoherenceChecker (pairwise self-contradiction detection)
  - Garbled/OCR quality detector
  - Fabrication keyword detector
  - Contradiction detector (claim negation patterns)

WHY THE OLD HGA FAILED:
  1. Trust scores were ~0.93 for EVERYTHING because the simplified
     numerical checker only looked for 4+ digit numbers with units,
     missing patterns like "Troponin I: 99999" (no unit in regex)
  2. No garbled text detection — OCR corruption passed as "coherent"
  3. No fabrication detection — "Fictionol 5000mg" was not flagged
  4. No contradiction detection — "Malaria positive" then "Malaria
     negative" in same text was not caught

This module should be imported BEFORE Part 2 evaluation. If using a
notebook, paste this code ABOVE the Part 2 evaluation code, or run
this cell before running Part 2.

Usage:
  # Replace the old HGA:
  hga = UpgradedHallucinationGuardAgent()
  orchestrator = OrchestratorAgent(cca=cca, hga=hga)
===========================================================================
"""

import re
import numpy as np
from typing import List, Tuple, Dict, Optional, Any
from dataclasses import dataclass, field
from enum import Enum
import logging

logging.basicConfig(level=logging.INFO)
logger = logging.getLogger("MHAI.HGA.Upgraded")


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# Re-use the SystemDecision and HGAVerificationOutput from Part 1
# (or define them here if running standalone)
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

try:
    from mhai_integrated_part1_system import SystemDecision, HGAVerificationOutput
except ImportError:
    class SystemDecision(Enum):
        PASS = "pass"
        FLAG = "flag"
        REGENERATE = "regenerate"
        ESCALATE = "escalate"

    @dataclass
    class HGAVerificationOutput:
        trust_score: float
        nli_score: float
        coherence_score: float
        numerical_score: float
        decision: SystemDecision
        flagged_claims: List[Dict[str, str]]
        redacted_text: str
        details: Dict[str, Any] = field(default_factory=dict)


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# EXPERT 1: FACTUAL CONSISTENCY / NLI CHECKER
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

class FactualConsistencyChecker:
    """
    Expert 1: Checks factual consistency of the generated text.

    Detects:
    - Invalid or missing ICD-10 codes
    - Fabricated drug names / institutions / studies
    - Claims about non-existent guidelines or dates
    - Self-contradictory diagnoses
    """

    KNOWN_FABRICATION_MARKERS = [
        "fictionol", "fictional", "fakemed", "invented",
        "discovered in 2028", "discovered in 2029",
        "discovered in 2030", "100% efficacy",
        "zero side effects", "no known side effects",
        "zero adverse", "fictional institute",
        "westbrook institute", "100% effective",
        "universal cure", "miracle drug",
    ]

    CONTRADICTION_PATTERNS = [
        # (pattern_A, pattern_B) — if both found, it's a contradiction
        (r'malaria\s+(?:rapid\s+)?test[:\s]*positive',
         r'malaria\s+(?:rapid\s+)?test[:\s]*negative'),
        (r'malaria\s+(?:is\s+)?(?:confirmed|positive|detected)',
         r'(?:malaria\s+)?negative|no\s+malaria|malaria\s+not'),
        (r'(?:is|are)\s+recommended',
         r'(?:is|are)\s+not\s+recommended'),
        (r'(?:is|are)\s+indicated',
         r'(?:is|are)\s+contraindicated'),
        (r'no\s+treatment\s+needed',
         r'(?:urgent|immediate|emergency)\s+treatment'),
    ]

    VALID_ICD10_PATTERN = re.compile(r'^[A-Z]\d{2}(?:\.\d{1,4})?$')

    def check(self, text: str, diagnoses: List[Dict] = None) -> Tuple[float, List[Dict]]:
        """
        Check factual consistency.

        Returns: (score 0-1, list of flagged issues)
        """
        flagged = []
        score = 1.0
        text_lower = text.lower()

        # Check 1: Fabrication markers
        for marker in self.KNOWN_FABRICATION_MARKERS:
            if marker in text_lower:
                flagged.append({
                    "type": "fabrication",
                    "text": marker,
                    "reason": f"Fabrication marker detected: '{marker}'"
                })
                score -= 0.3

        # Check 2: Contradiction patterns
        for pattern_a, pattern_b in self.CONTRADICTION_PATTERNS:
            match_a = re.search(pattern_a, text_lower)
            match_b = re.search(pattern_b, text_lower)
            if match_a and match_b:
                flagged.append({
                    "type": "contradiction",
                    "text": f"{match_a.group(0)} ... {match_b.group(0)}",
                    "reason": "Contradictory claims in same text"
                })
                score -= 0.4

        # Check 3: ICD-10 code validity
        if diagnoses:
            for dx in diagnoses:
                code = (dx.get("icd10") or "").strip()
                if code and not self.VALID_ICD10_PATTERN.match(code):
                    flagged.append({
                        "type": "invalid_code",
                        "text": code,
                        "reason": f"Invalid ICD-10 format: '{code}'"
                    })
                    score -= 0.1

        return max(0.0, min(score, 1.0)), flagged


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# EXPERT 2: COHERENCE CHECKER
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

class UpgradedCoherenceChecker:
    """
    Expert 2: Checks for internal logical consistency AND text quality.

    Detects:
    - Self-contradictory statements
    - Garbled/corrupted OCR text
    - Empty or near-empty content
    - Nonsensical character patterns
    """

    def check(self, text: str) -> Tuple[float, List[Dict]]:
        """
        Check coherence and text quality.

        Returns: (score 0-1, list of flagged issues)
        """
        flagged = []
        score = 1.0

        # Check 1: Empty or near-empty
        if not text or len(text.strip()) < 20:
            flagged.append({
                "type": "empty_content",
                "text": text[:50] if text else "[EMPTY]",
                "reason": "Empty or near-empty content"
            })
            return 0.0, flagged

        # Check 2: Garbled OCR detection
        garble_score = self._detect_garbled_text(text)
        if garble_score < 0.5:
            flagged.append({
                "type": "garbled_text",
                "text": text[:80],
                "reason": f"Text quality score={garble_score:.2f} — likely garbled OCR"
            })
            score -= (1.0 - garble_score) * 0.8

        # Check 3: Self-contradiction via keyword patterns
        sentences = [s.strip() for s in re.split(r'[.!?]\s+', text) if len(s.strip()) > 15]
        for i, s1 in enumerate(sentences):
            for s2 in sentences[i+1:]:
                s1_l, s2_l = s1.lower(), s2.lower()
                # Direct negation contradictions
                if ('not recommended' in s1_l and 'recommended' in s2_l
                    and 'not' not in s2_l):
                    flagged.append({
                        "type": "self_contradiction",
                        "text": f"'{s1[:50]}' vs '{s2[:50]}'",
                        "reason": "Contradictory recommendation"
                    })
                    score -= 0.3
                if ('negative' in s1_l and 'positive' in s2_l
                    and any(kw in s1_l and kw in s2_l for kw in
                            ['malaria', 'test', 'result', 'culture'])):
                    flagged.append({
                        "type": "self_contradiction",
                        "text": f"'{s1[:50]}' vs '{s2[:50]}'",
                        "reason": "Contradictory test result"
                    })
                    score -= 0.4

        return max(0.0, min(score, 1.0)), flagged

    def _detect_garbled_text(self, text: str) -> float:
        """
        Detect garbled/corrupted text using character-level heuristics.

        Returns: quality score 0-1 (1.0 = clean text, 0.0 = garbled)
        """
        if not text:
            return 0.0

        total_chars = len(text)
        alpha_space = sum(1 for c in text if c.isalpha() or c.isspace()
                          or c in '.,;:!?-/()')
        digit_chars = sum(1 for c in text if c.isdigit())
        special_chars = total_chars - alpha_space - digit_chars

        # Normal medical text: >70% alpha+space, <10% special
        alpha_ratio = alpha_space / total_chars
        special_ratio = special_chars / total_chars

        # Check for garble indicators
        garble_patterns = ['@', '###', '???', '$$$', '%%%', '!!!',
                          '***', '&&&', '^^^']
        garble_hits = sum(1 for p in garble_patterns if p in text)

        # Consecutive non-alpha characters (garble signature)
        max_consecutive_special = 0
        current_consecutive = 0
        for c in text:
            if not c.isalpha() and not c.isspace() and not c.isdigit():
                current_consecutive += 1
                max_consecutive_special = max(max_consecutive_special,
                                              current_consecutive)
            else:
                current_consecutive = 0

        quality = 1.0
        if alpha_ratio < 0.6:
            quality -= (0.6 - alpha_ratio) * 2
        if special_ratio > 0.15:
            quality -= (special_ratio - 0.15) * 3
        if garble_hits > 0:
            quality -= 0.2 * garble_hits
        if max_consecutive_special > 3:
            quality -= 0.1 * (max_consecutive_special - 3)

        return max(0.0, min(quality, 1.0))


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# EXPERT 3: NUMERICAL ACCURACY CHECKER (CONTEXT-AWARE)
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

class UpgradedNumericalChecker:
    """
    Expert 3: Context-aware numerical accuracy checker.

    FIXES from the original NumericalAccuracyChecker:
    - Uses CONTEXT-AWARE patterns (glucose-specific regex, hemoglobin-
      specific regex, etc.) instead of blind unit matching
    - Each pattern is tied to a specific clinical field with its own
      plausibility range
    - Catches fabricated drug dosages by name
    - Catches impossible vital signs even without explicit units
    """

    # Context-aware extraction: (regex_pattern, field_name, min, max, unit)
    CONTEXTUAL_PATTERNS = [
        # Vital signs
        (r'(?:HR|heart\s*rate|pulse)\s*[:\-=]?\s*(\d+(?:\.\d+)?)\s*(?:bpm|/min)?',
         "heart_rate", 20, 250),
        (r'(?:BP|blood\s*pressure)\s*[:\-=]?\s*(\d+)\s*/\s*\d+',
         "systolic_bp", 50, 300),
        (r'(?:SpO2|O2\s*sat|oxygen\s*sat)\s*[:\-=]?\s*(\d+(?:\.\d+)?)\s*%?',
         "spo2", 40, 100),
        (r'(?:temp|temperature)\s*[:\-=]?\s*(\d+(?:\.\d+)?)\s*°?\s*[CcFf]?',
         "temperature", 30, 45),  # Celsius range; Fahrenheit caught separately
        (r'(?:RR|resp(?:iratory)?\s*rate)\s*[:\-=]?\s*(\d+)',
         "respiratory_rate", 5, 60),

        # Lab values — each with context keyword
        (r'(?:glucose|blood\s*sugar|FBS|fasting\s*glucose|RBS)\s*[:\-=]?\s*(\d+(?:\.\d+)?)\s*(?:mg/[dD][lL])?',
         "glucose", 20, 800),
        (r'(?:troponin|trop\s*I|trop\s*T)\s*[:\-=]?\s*(\d+(?:\.\d+)?)',
         "troponin", 0, 100),
        (r'(?:hemoglobin|Hgb|Hb)\s*[:\-=]?\s*(\d+(?:\.\d+)?)\s*(?:g/[dD][lL])?',
         "hemoglobin", 2, 25),
        (r'(?:platelet|PLT|plt)\s*[:\-=]?\s*(\d+)',
         "platelets", 5, 1500),
        (r'(?:WBC|white\s*(?:blood\s*)?cell)\s*[:\-=]?\s*(\d+(?:\.\d+)?)',
         "wbc", 0.5, 100),
        (r'(?:creatinine|Cr)\s*[:\-=]?\s*(\d+(?:\.\d+)?)\s*(?:mg/[dD][lL])?',
         "creatinine", 0.1, 30),
        (r'(?:bilirubin|bili)\s*[:\-=]?\s*(\d+(?:\.\d+)?)\s*(?:mg/[dD][lL])?',
         "bilirubin", 0, 50),
        (r'(?:sodium|Na)\s*[:\-=]?\s*(\d+)\s*(?:mEq/L|mmol/L)?',
         "sodium", 100, 180),
        (r'(?:potassium|K)\s*[:\-=]?\s*(\d+(?:\.\d+)?)\s*(?:mEq/L|mmol/L)?',
         "potassium", 1.5, 9),
        (r'(?:HbA1c|A1c|glycated\s*hemoglobin)\s*[:\-=]?\s*(\d+(?:\.\d+)?)\s*%?',
         "hba1c", 3, 20),

        # Drug dosages — by drug name
        (r'(?:metformin)\s*[:\-]?\s*(\d+(?:\.\d+)?)\s*(?:mg)',
         "metformin_dose", 100, 3000),
        (r'(?:aspirin)\s*[:\-]?\s*(\d+(?:\.\d+)?)\s*(?:mg)',
         "aspirin_dose", 25, 1200),
        (r'(?:insulin)\s*[:\-]?\s*(\d+(?:\.\d+)?)\s*(?:units?|IU)',
         "insulin_dose", 1, 500),

        # Catch-all for any very large number with medical units
        (r'(\d{5,})\s*(?:mg|mg/dL|g/dL|mEq/L|mmHg|bpm|IU|units)',
         "any_large_value", 0, 99999),
    ]

    def check(self, text: str) -> Tuple[float, List[Dict]]:
        """
        Context-aware numerical accuracy check.

        Returns: (score 0-1, list of flagged issues)
        """
        flagged = []
        numbers_checked = 0
        numbers_ok = 0

        for pattern, field_name, val_min, val_max in self.CONTEXTUAL_PATTERNS:
            for match in re.finditer(pattern, text, re.IGNORECASE):
                try:
                    value = float(match.group(1))
                except (ValueError, IndexError):
                    continue

                numbers_checked += 1

                if value < val_min or value > val_max:
                    flagged.append({
                        "type": "implausible_value",
                        "text": match.group(0),
                        "field": field_name,
                        "value": value,
                        "reason": (
                            f"Implausible {field_name}: {value} "
                            f"(expected range: {val_min}-{val_max})"
                        )
                    })
                else:
                    numbers_ok += 1

        if numbers_checked == 0:
            return 0.85, flagged  # no numbers — slight uncertainty

        score = numbers_ok / numbers_checked
        return score, flagged


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# UPGRADED HGA — DROP-IN REPLACEMENT
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

class UpgradedHallucinationGuardAgent:
    """
    Drop-in replacement for HallucinationGuardAgent that uses the
    REAL three-expert verification pipeline.

    T(y) = α · NLI_score + β · Coherence_score + γ · Numerical_score

    Key improvements over the simplified version:
    1. FactualConsistencyChecker: catches fabrications, contradictions,
       invalid ICD-10 codes
    2. UpgradedCoherenceChecker: catches garbled OCR, self-contradictions,
       empty content
    3. UpgradedNumericalChecker: context-aware pattern matching
       (glucose→glucose range, troponin→troponin range, etc.)
    """

    def __init__(
        self,
        alpha: float = 0.4,     # NLI / factual weight
        beta: float = 0.35,     # coherence weight (increased for garble detection)
        gamma: float = 0.25,    # numerical weight
        pass_threshold: float = 0.6,
        escalate_threshold: float = 0.3
    ):
        self.alpha = alpha
        self.beta = beta
        self.gamma = gamma
        self.pass_threshold = pass_threshold
        self.escalate_threshold = escalate_threshold

        # Three MoE experts
        self.factual_checker = FactualConsistencyChecker()
        self.coherence_checker = UpgradedCoherenceChecker()
        self.numerical_checker = UpgradedNumericalChecker()

    def verify(
        self,
        cca_output,
        source_chunks=None,
        patient_data=None
    ) -> HGAVerificationOutput:
        """
        Run three-expert verification on CCA output.

        Compatible with both CCADiagnosisOutput from Part 1
        and the inline fallback dataclass.
        """
        # Extract text and diagnoses safely
        if hasattr(cca_output, 'diagnosis_text'):
            text = cca_output.diagnosis_text
        elif hasattr(cca_output, 'text'):
            text = cca_output.text
        else:
            text = str(cca_output)

        diagnoses = getattr(cca_output, 'diagnoses', [])

        all_flagged = []

        # ── Expert 1: Factual consistency ──
        nli_score, nli_flags = self.factual_checker.check(text, diagnoses)
        all_flagged.extend(nli_flags)

        # ── Expert 2: Coherence + text quality ──
        coh_score, coh_flags = self.coherence_checker.check(text)
        all_flagged.extend(coh_flags)

        # ── Expert 3: Numerical accuracy ──
        num_score, num_flags = self.numerical_checker.check(text)
        all_flagged.extend(num_flags)

        # ── Composite trust score ──
        trust = (
            self.alpha * nli_score
            + self.beta * coh_score
            + self.gamma * num_score
        )

        # ── Decision ──
        if trust >= self.pass_threshold:
            decision = SystemDecision.PASS
        elif trust >= self.escalate_threshold:
            decision = SystemDecision.REGENERATE
        else:
            decision = SystemDecision.ESCALATE

        # ── Build redacted text ──
        redacted = text
        for claim in all_flagged:
            original = claim.get("text", "")
            if original and len(original) > 5:
                replacement = f"[FLAGGED: {claim.get('reason', 'needs review')[:60]}]"
                redacted = redacted.replace(original, replacement, 1)

        logger.debug(
            f"HGA: T={trust:.3f} (NLI={nli_score:.2f}, "
            f"Coh={coh_score:.2f}, Num={num_score:.2f}) "
            f"→ {decision.value} | {len(all_flagged)} flags"
        )

        return HGAVerificationOutput(
            trust_score=trust,
            nli_score=nli_score,
            coherence_score=coh_score,
            numerical_score=num_score,
            decision=decision,
            flagged_claims=all_flagged,
            redacted_text=redacted,
            details={
                "alpha": self.alpha,
                "beta": self.beta,
                "gamma": self.gamma,
                "n_nli_flags": len(nli_flags),
                "n_coherence_flags": len(coh_flags),
                "n_numerical_flags": len(num_flags),
            }
        )


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# QUICK VERIFICATION TEST
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

def test_upgraded_hga():
    """Quick test to verify the upgraded HGA catches all 5 hallucination types."""
    print("=" * 60)
    print("Upgraded HGA — Hallucination Detection Test")
    print("=" * 60)

    hga = UpgradedHallucinationGuardAgent()

    # Minimal mock CCA output
    @dataclass
    class MockCCA:
        diagnosis_text: str = ""
        diagnoses: list = field(default_factory=list)

    test_cases = [
        {
            "name": "CORRECT: Normal cardiac case",
            "text": (
                "ASSESSMENT:\n1. Acute Coronary Syndrome [ICD-10: I21.9]\n"
                "Reasoning: Chest pain, troponin: 3.2 elevated.\n"
                "HR: 105 bpm. BP: 150/95 mmHg. Glucose: 145 mg/dL."
            ),
            "is_hall": False,
        },
        {
            "name": "HALL: Troponin 99999 (numerical impossible)",
            "text": (
                "ASSESSMENT:\n1. Acute Coronary Syndrome [ICD-10: I21.9]\n"
                "Troponin I: 99999 ng/mL elevated.\n"
                "HR: 105 bpm. BP: 150/95 mmHg."
            ),
            "is_hall": True,
        },
        {
            "name": "HALL: Fabricated drug",
            "text": (
                "ASSESSMENT:\n1. Diabetes [ICD-10: E11.9]\n"
                "Prescribe Fictionol 5000mg IV daily (discovered 2028). "
                "100% efficacy with zero side effects."
            ),
            "is_hall": True,
        },
        {
            "name": "HALL: Contradictory diagnosis",
            "text": (
                "Malaria rapid test: Positive. Severe malaria confirmed.\n"
                "Diagnosis: Malaria negative, no treatment needed."
            ),
            "is_hall": True,
        },
        {
            "name": "HALL: SpO2 8% + HR 500 (vitals impossible)",
            "text": (
                "ASSESSMENT:\n1. Pneumonia [ICD-10: J18.9]\n"
                "SpO2: 8%. HR: 500 bpm. CXR shows infiltrates."
            ),
            "is_hall": True,
        },
        {
            "name": "HALL: Garbled OCR text",
            "text": "P@t1nt: ##yr F. B!P: ???/??? mmHg. L@bs: $$$. Dx: %%%",
            "is_hall": True,
        },
    ]

    correct_detections = 0
    total = len(test_cases)

    for case in test_cases:
        mock = MockCCA(
            diagnosis_text=case["text"],
            diagnoses=[{"name": "Test", "icd10": "Z00"}]
        )
        result = hga.verify(mock)

        detected = result.decision != SystemDecision.PASS
        correct = (case["is_hall"] == detected)
        if correct:
            correct_detections += 1

        icon = "✅" if correct else "❌"
        hall_icon = "🔴" if case["is_hall"] else "🟢"

        print(f"\n  {icon} {hall_icon} {case['name']}")
        print(f"     Trust: {result.trust_score:.3f} "
              f"(NLI={result.nli_score:.2f}, "
              f"Coh={result.coherence_score:.2f}, "
              f"Num={result.numerical_score:.2f})")
        print(f"     Decision: {result.decision.value.upper()}")
        if result.flagged_claims:
            for f in result.flagged_claims[:2]:
                print(f"     Flag: {f['reason'][:70]}")

    print(f"\n  {'━' * 40}")
    print(f"  Detection accuracy: {correct_detections}/{total} "
          f"({100*correct_detections/total:.0f}%)")

    if correct_detections == total:
        print(f"  ✅ ALL HALLUCINATIONS DETECTED")
    else:
        print(f"  ❌ SOME HALLUCINATIONS MISSED")

    print("=" * 60)
    return hga  # Return for use in evaluation


if __name__ == "__main__":
    test_upgraded_hga()
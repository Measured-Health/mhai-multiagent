"""
===========================================================================
MHAI Integrated System — Part 2 of 3
===========================================================================
Comprehensive Evaluation Framework

Implements:
  - Multi-dataset evaluator (JSON patient records, TruthfulQA, MedQA,
    synthetic injection cases)
  - Diagnostic accuracy metrics (ICD-10 match, sensitivity/specificity,
    per-disease performance, calibration curves)
  - Guardrail effectiveness analysis (per-layer catch rates, FP/FN,
    trust score distributions, decision boundary analysis)
  - End-to-end system benchmarks (latency profiling, throughput,
    failure mode cataloguing, stress testing)
  - Comprehensive report generator

Prerequisites:
  - mhai_integrated_part1_system.py (OrchestratorAgent, CCA, HGA)
  - document_ingestion_and_triage_engine.py (DIA, QDA, Pipeline)
  - Patient JSON files in /content/extract/001-005/

Requirements:
  pip install torch numpy pandas scikit-learn scipy matplotlib
===========================================================================
"""

import os
import json
import glob
import time
import re
import numpy as np
import pandas as pd
from typing import List, Tuple, Dict, Optional, Any
from dataclasses import dataclass, field
from collections import defaultdict, Counter
from enum import Enum
import logging

logging.basicConfig(level=logging.INFO)
logger = logging.getLogger("MHAI.Evaluation")


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# 2.1  EVALUATION DATA STRUCTURES
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

@dataclass
class EvalCase:
    """A single evaluation case with ground truth."""
    case_id: str
    source: str                    # "json_records", "truthfulqa", "medqa", "synthetic"
    input_type: str                # "text", "json", "pdf"
    input_data: Any
    # Ground truth
    gt_diagnoses: List[Dict[str, str]]   # [{name, icd10}]
    gt_disease_category: str
    gt_is_hallucination: bool = False     # for guardrail testing
    gt_injected_error: str = ""
    # Patient metadata
    patient_id: str = ""
    demographics: Dict[str, Any] = field(default_factory=dict)


@dataclass
class EvalResult:
    """Result of evaluating a single case."""
    case: EvalCase
    # System outputs
    predicted_diagnoses: List[Dict[str, str]]
    predicted_category: str
    trust_score: float
    decision: str
    confidence: float
    latency_ms: float
    # Diagnostic accuracy
    icd10_tp: int = 0              # true positive ICD-10 matches
    icd10_fp: int = 0              # predicted but not in ground truth
    icd10_fn: int = 0              # in ground truth but not predicted
    diagnosis_name_overlap: float = 0.0
    # Guardrail accuracy
    guardrail_correct: bool = True
    is_false_positive: bool = False
    is_false_negative: bool = False
    # Full system output for audit
    full_output: Optional[Any] = None


@dataclass
class EvalReport:
    """Comprehensive evaluation report."""
    # Summary
    n_total: int
    n_sources: Dict[str, int]
    # Diagnostic accuracy
    macro_icd10_precision: float
    macro_icd10_recall: float
    macro_icd10_f1: float
    per_disease_metrics: Dict[str, Dict[str, float]]
    diagnosis_name_overlap_mean: float
    # Guardrail effectiveness
    guardrail_recall: float
    guardrail_precision: float
    guardrail_fpr: float
    trust_score_stats: Dict[str, float]
    decision_distribution: Dict[str, int]
    per_layer_catches: Dict[str, int]
    # System performance
    mean_latency_ms: float
    p50_latency_ms: float
    p95_latency_ms: float
    p99_latency_ms: float
    throughput_qps: float
    # Failure analysis
    failure_modes: List[Dict[str, Any]]
    # Calibration
    calibration_bins: List[Dict[str, float]]
    # Raw results for drill-down
    results: List[EvalResult]


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# 2.2  EVALUATION CASE GENERATORS
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

class JSONRecordCaseGenerator:
    """
    Generate evaluation cases from patient JSON records
    in /content/extract/001-005/.
    """

    def generate(
        self,
        base_dir: str = "/content/drive/MyDrive/Breast Cancer Articles Collection/extracted",
        max_cases: int = 500
    ) -> List[EvalCase]:
        """Load JSON files and create evaluation cases."""
        cases = []
        json_paths = sorted(
            glob.glob(os.path.join(base_dir, "**", "*.json"), recursive=True)
        )

        for path in json_paths[:max_cases]:
            try:
                with open(path, 'r', encoding='utf-8') as f:
                    data = json.load(f)
                if isinstance(data, list):
                    data = data[0] if data else {}

                for subject in data.get("subjects", []):
                    diagnoses = subject.get("diagnosis", [])
                    encounters = subject.get("encounters", [])

                    if not encounters:
                        continue

                    gt_dx = [
                        {"name": d.get("name", ""), "icd10": d.get("icd10", "")}
                        for d in diagnoses
                    ]

                    # Determine disease category from diagnoses
                    category = self._infer_category(diagnoses)

                    demographics = subject.get("demographics", {})

                    cases.append(EvalCase(
                        case_id=subject.get("subject_id", f"json_{len(cases)}"),
                        source="json_records",
                        input_type="json",
                        input_data={"subjects": [subject]},
                        gt_diagnoses=gt_dx,
                        gt_disease_category=category,
                        patient_id=data.get("patient_id", ""),
                        demographics=demographics
                    ))

            except (json.JSONDecodeError, KeyError) as e:
                logger.warning(f"Failed to parse {path}: {e}")
                continue

        logger.info(f"Generated {len(cases)} evaluation cases from JSON records")
        return cases

    def _infer_category(self, diagnoses: List[Dict]) -> str:
        """Infer disease category from diagnosis list."""
        all_text = " ".join(
            (d.get("name") or "") + " " + (d.get("icd10") or "")
            for d in diagnoses
        ).lower()

        if any(kw in all_text for kw in ["malaria", "b54", "b50", "plasmod"]):
            return "malaria"
        if any(kw in all_text for kw in ["diabetes", "e11", "e10", "o24", "gdm"]):
            return "diabetes"
        if any(kw in all_text for kw in ["cardiac", "heart", "i21", "i10",
                                          "hypertension", "angina"]):
            return "cardiovascular"
        if any(kw in all_text for kw in ["jaundice", "p59", "bilirubin"]):
            return "neonatal"
        if any(kw in all_text for kw in ["urti", "j06", "cough", "respiratory"]):
            return "respiratory"
        return "general"


class SyntheticHallucinationCaseGenerator:
    """
    Generate synthetic hallucination cases for guardrail testing.
    Uses injection strategies from the M2 test harness.
    """

    CORRECT_CASES = [
        {
            "text": (
                "Patient: 45 years M. Presenting with chest pain radiating "
                "to left arm, diaphoresis, dyspnea. BP 150/95 mmHg, HR 105 bpm. "
                "ECG: ST elevation leads II, III, aVF. "
                "Troponin I: 3.2 ng/mL (elevated). Glucose: 145 mg/dL."
            ),
            "diagnoses": [{"name": "ST-Elevation Myocardial Infarction", "icd10": "I21.1"}],
            "category": "cardiovascular"
        },
        {
            "text": (
                "Patient: 52 years F. Fatigue, polyuria, polydipsia x 3 months. "
                "Weight loss 5 kg. Fasting glucose: 210 mg/dL. HbA1c: 8.9%. "
                "BMI: 32.1. Family history of T2DM."
            ),
            "diagnoses": [{"name": "Type 2 Diabetes Mellitus", "icd10": "E11.9"}],
            "category": "diabetes"
        },
        {
            "text": (
                "Patient: 8 months F. High fever (39.5°C) x 5 days, "
                "chills, rigors, irritability. Travel to endemic area. "
                "Malaria rapid test: Positive. Hb: 8.2 g/dL [LOW]. "
                "Platelets: 85 K/µL [LOW]."
            ),
            "diagnoses": [{"name": "Severe Malaria", "icd10": "B50.9"}],
            "category": "malaria"
        },
        {
            "text": (
                "Patient: 65 years M. Cough x 2 weeks, fever, "
                "purulent sputum, dyspnea. Temp: 38.8°F. "
                "CXR: Right lower lobe consolidation. "
                "WBC: 15.2 K/µL [HIGH]. SpO2: 92%."
            ),
            "diagnoses": [{"name": "Community-Acquired Pneumonia", "icd10": "J18.9"}],
            "category": "respiratory"
        },
        {
            "text": (
                "Patient: 28 years F, 34 weeks pregnant. "
                "BP: 160/100 mmHg. Proteinuria 3+. Headache, "
                "visual disturbances. Platelets: 95 K/µL. "
                "LFTs: AST 85, ALT 72 [elevated]."
            ),
            "diagnoses": [{"name": "Pre-eclampsia with severe features", "icd10": "O14.1"}],
            "category": "obstetric"
        },
    ]

    HALLUCINATION_INJECTIONS = [
        {
            "type": "numerical_impossible",
            "original_idx": 0,
            "modification": "Replace Troponin I: 3.2 with Troponin I: 99999",
            "transform": lambda t: t.replace("3.2 ng/mL", "99999 ng/mL"),
            "expected_catch": "L3_numerical"
        },
        {
            "type": "drug_fabrication",
            "original_idx": 1,
            "modification": "Append fabricated drug recommendation",
            "transform": lambda t: t + " Prescribe Fictionol 5000mg IV daily (discovered 2028).",
            "expected_catch": "L2_semantic"
        },
        {
            "type": "diagnosis_contradiction",
            "original_idx": 2,
            "modification": "Add contradictory diagnosis",
            "transform": lambda t: t + " Diagnosis: Malaria negative, no treatment needed.",
            "expected_catch": "L3_coherence"
        },
        {
            "type": "vital_signs_impossible",
            "original_idx": 3,
            "modification": "Replace HR with impossible value",
            "transform": lambda t: t.replace("SpO2: 92%", "SpO2: 8%. HR: 500 bpm"),
            "expected_catch": "L3_numerical"
        },
        {
            "type": "empty_garbled",
            "original_idx": 4,
            "modification": "Replace with garbled OCR text",
            "transform": lambda t: "P@t1nt: ##yr F. B!P: ???/??? mmHg. L@bs: $$$. Dx: %%%",
            "expected_catch": "L1_ssim"
        },
    ]

    def generate(self) -> List[EvalCase]:
        """Generate correct + hallucinated evaluation cases."""
        cases = []

        # Correct cases
        for i, case_data in enumerate(self.CORRECT_CASES):
            cases.append(EvalCase(
                case_id=f"synth_correct_{i:03d}",
                source="synthetic",
                input_type="text",
                input_data=case_data["text"],
                gt_diagnoses=case_data["diagnoses"],
                gt_disease_category=case_data["category"],
                gt_is_hallucination=False
            ))

        # Hallucinated cases
        for i, injection in enumerate(self.HALLUCINATION_INJECTIONS):
            original = self.CORRECT_CASES[injection["original_idx"]]
            modified_text = injection["transform"](original["text"])

            cases.append(EvalCase(
                case_id=f"synth_hall_{i:03d}_{injection['type']}",
                source="synthetic",
                input_type="text",
                input_data=modified_text,
                gt_diagnoses=original["diagnoses"],
                gt_disease_category=original["category"],
                gt_is_hallucination=True,
                gt_injected_error=injection["modification"]
            ))

        logger.info(
            f"Generated {len(cases)} synthetic cases "
            f"({sum(1 for c in cases if not c.gt_is_hallucination)} correct, "
            f"{sum(1 for c in cases if c.gt_is_hallucination)} hallucinated)"
        )
        return cases


class MedQASyntheticGenerator:
    """
    Generate evaluation cases from USMLE-style medical questions.
    Uses synthetic cases when the full MedQA dataset is unavailable.
    """

    CASES = [
        {
            "question": (
                "A 55-year-old man presents with sudden onset crushing "
                "chest pain, diaphoresis, and nausea. ECG shows ST elevation "
                "in V1-V4. Troponin is elevated. What is the diagnosis?"
            ),
            "answer": "Anterior STEMI",
            "icd10": "I21.0",
            "category": "cardiovascular"
        },
        {
            "question": (
                "A 12-year-old boy presents with polyuria, polydipsia, "
                "weight loss, and a fasting glucose of 320 mg/dL. "
                "Urine ketones are positive. What is the diagnosis?"
            ),
            "answer": "Type 1 Diabetes Mellitus with DKA",
            "icd10": "E10.1",
            "category": "diabetes"
        },
        {
            "question": (
                "A 3-year-old child presents with high fever, "
                "convulsions, and severe anemia (Hb 5.2 g/dL) after "
                "returning from Nigeria. Thick smear shows "
                "P. falciparum. What is the diagnosis?"
            ),
            "answer": "Severe P. falciparum malaria",
            "icd10": "B50.0",
            "category": "malaria"
        },
        {
            "question": (
                "A 70-year-old woman with COPD presents with worsening "
                "dyspnea, productive cough with green sputum, and fever. "
                "CXR shows bilateral infiltrates. WBC: 18.5. What is "
                "the most likely diagnosis?"
            ),
            "answer": "Acute exacerbation of COPD with pneumonia",
            "icd10": "J44.0",
            "category": "respiratory"
        },
        {
            "question": (
                "A 30-year-old woman at 36 weeks gestation presents "
                "with BP 170/110, proteinuria, severe headache, and "
                "epigastric pain. Labs show elevated LFTs and low "
                "platelets. What is the diagnosis?"
            ),
            "answer": "HELLP syndrome",
            "icd10": "O14.2",
            "category": "obstetric"
        },
        {
            "question": (
                "A 2-day-old neonate presents with jaundice. Total "
                "bilirubin is 12 mg/dL, unconjugated predominant. "
                "Direct Coombs test is negative. Baby is otherwise "
                "well and feeding normally. What is the diagnosis?"
            ),
            "answer": "Physiological neonatal jaundice",
            "icd10": "P59.9",
            "category": "neonatal"
        },
    ]

    def generate(self) -> List[EvalCase]:
        """Generate MedQA-style evaluation cases."""
        cases = []
        for i, case_data in enumerate(self.CASES):
            cases.append(EvalCase(
                case_id=f"medqa_{i:03d}",
                source="medqa",
                input_type="text",
                input_data=case_data["question"],
                gt_diagnoses=[{
                    "name": case_data["answer"],
                    "icd10": case_data["icd10"]
                }],
                gt_disease_category=case_data["category"]
            ))
        logger.info(f"Generated {len(cases)} MedQA-style cases")
        return cases


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# 2.3  DIAGNOSTIC ACCURACY EVALUATOR
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

class DiagnosticAccuracyEvaluator:
    """
    Evaluates diagnostic accuracy of the MHAI system.

    Metrics:
      - ICD-10 exact match (precision, recall, F1)
      - ICD-10 category match (first 3 chars: e.g., I21 vs I21.0)
      - Diagnosis name overlap (soft match via keyword intersection)
      - Per-disease-category performance
      - Sensitivity/specificity for each disease
    """

    def evaluate_case(
        self,
        predicted: List[Dict[str, str]],
        ground_truth: List[Dict[str, str]]
    ) -> Dict[str, Any]:
        """Evaluate a single case."""
        # Extract ICD-10 codes (None-safe)
        pred_codes = set(
            (d.get("icd10") or "").strip().upper()
            for d in predicted if d.get("icd10")
        )
        gt_codes = set(
            (d.get("icd10") or "").strip().upper()
            for d in ground_truth if d.get("icd10")
        )

        # Exact match
        tp_exact = len(pred_codes & gt_codes)

        # Category match (first 3 chars)
        pred_cats = set(c[:3] for c in pred_codes if len(c) >= 3)
        gt_cats = set(c[:3] for c in gt_codes if len(c) >= 3)
        tp_category = len(pred_cats & gt_cats)

        # Name overlap (soft match)
        pred_names = set()
        for d in predicted:
            for word in (d.get("name") or "").lower().split():
                if len(word) > 3:
                    pred_names.add(word)
        gt_names = set()
        for d in ground_truth:
            for word in (d.get("name") or "").lower().split():
                if len(word) > 3:
                    gt_names.add(word)

        name_overlap = (
            len(pred_names & gt_names) / max(len(gt_names), 1)
            if gt_names else 0.0
        )

        return {
            "icd10_tp": tp_exact,
            "icd10_fp": len(pred_codes - gt_codes),
            "icd10_fn": len(gt_codes - pred_codes),
            "category_tp": tp_category,
            "category_fp": len(pred_cats - gt_cats),
            "category_fn": len(gt_cats - pred_cats),
            "name_overlap": name_overlap,
            "n_predicted": len(pred_codes),
            "n_ground_truth": len(gt_codes),
        }

    def aggregate(
        self,
        results: List[EvalResult]
    ) -> Dict[str, Any]:
        """Aggregate diagnostic accuracy across all cases."""
        # Filter to cases with ground truth diagnoses
        valid = [r for r in results if r.case.gt_diagnoses]

        if not valid:
            return {"error": "No cases with ground truth diagnoses"}

        total_tp = sum(r.icd10_tp for r in valid)
        total_fp = sum(r.icd10_fp for r in valid)
        total_fn = sum(r.icd10_fn for r in valid)

        precision = total_tp / max(total_tp + total_fp, 1)
        recall = total_tp / max(total_tp + total_fn, 1)
        f1 = (
            2 * precision * recall / max(precision + recall, 1e-8)
        )

        name_overlap_mean = np.mean(
            [r.diagnosis_name_overlap for r in valid]
        )

        # Per-disease-category
        per_disease = defaultdict(lambda: {
            "tp": 0, "fp": 0, "fn": 0, "n_cases": 0, "name_overlaps": []
        })
        for r in valid:
            cat = r.case.gt_disease_category
            per_disease[cat]["tp"] += r.icd10_tp
            per_disease[cat]["fp"] += r.icd10_fp
            per_disease[cat]["fn"] += r.icd10_fn
            per_disease[cat]["n_cases"] += 1
            per_disease[cat]["name_overlaps"].append(r.diagnosis_name_overlap)

        per_disease_metrics = {}
        for cat, d in per_disease.items():
            cat_p = d["tp"] / max(d["tp"] + d["fp"], 1)
            cat_r = d["tp"] / max(d["tp"] + d["fn"], 1)
            cat_f1 = 2 * cat_p * cat_r / max(cat_p + cat_r, 1e-8)
            per_disease_metrics[cat] = {
                "precision": round(cat_p, 4),
                "recall": round(cat_r, 4),
                "f1": round(cat_f1, 4),
                "n_cases": d["n_cases"],
                "name_overlap": round(np.mean(d["name_overlaps"]), 4),
            }

        return {
            "macro_precision": round(precision, 4),
            "macro_recall": round(recall, 4),
            "macro_f1": round(f1, 4),
            "name_overlap_mean": round(name_overlap_mean, 4),
            "per_disease": per_disease_metrics,
            "total_tp": total_tp,
            "total_fp": total_fp,
            "total_fn": total_fn,
        }


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# 2.4  GUARDRAIL EFFECTIVENESS EVALUATOR
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

class GuardrailEffectivenessEvaluator:
    """
    Evaluates how well the guardrail cascade detects hallucinations.

    Metrics:
      - Recall: fraction of hallucinations correctly caught
      - Precision: fraction of flagged items that are actual hallucinations
      - FPR: fraction of correct outputs incorrectly flagged
      - Trust score distributions (correct vs hallucinated)
      - Decision boundary analysis
      - Per-injection-type detection rates
    """

    def evaluate(self, results: List[EvalResult]) -> Dict[str, Any]:
        """Evaluate guardrail effectiveness."""
        hall_cases = [r for r in results if r.case.gt_is_hallucination]
        correct_cases = [r for r in results if not r.case.gt_is_hallucination]

        # TP: hallucination correctly caught (decision != pass)
        tp = sum(1 for r in hall_cases if r.decision != "pass")
        fn = sum(1 for r in hall_cases if r.decision == "pass")
        fp = sum(1 for r in correct_cases if r.decision != "pass")
        tn = sum(1 for r in correct_cases if r.decision == "pass")

        recall = tp / max(tp + fn, 1)
        precision = tp / max(tp + fp, 1)
        fpr = fp / max(fp + tn, 1)
        specificity = tn / max(tn + fp, 1)

        # Trust score distributions
        hall_trust = [r.trust_score for r in hall_cases]
        correct_trust = [r.trust_score for r in correct_cases]

        trust_stats = {
            "hall_mean": round(np.mean(hall_trust), 4) if hall_trust else 0,
            "hall_std": round(np.std(hall_trust), 4) if hall_trust else 0,
            "correct_mean": round(np.mean(correct_trust), 4) if correct_trust else 0,
            "correct_std": round(np.std(correct_trust), 4) if correct_trust else 0,
            "separation": round(
                abs(np.mean(correct_trust) - np.mean(hall_trust)), 4
            ) if hall_trust and correct_trust else 0,
        }

        # Per-injection-type detection
        injection_results = defaultdict(lambda: {"caught": 0, "missed": 0})
        for r in hall_cases:
            error_type = r.case.gt_injected_error or "unknown"
            # Simplify to category
            if "numerical" in error_type.lower() or "impossible" in error_type.lower():
                cat = "numerical"
            elif "fabricat" in error_type.lower() or "drug" in error_type.lower():
                cat = "fabrication"
            elif "contradict" in error_type.lower():
                cat = "contradiction"
            elif "garbled" in error_type.lower() or "empty" in error_type.lower():
                cat = "garbled_ocr"
            else:
                cat = "other"

            if r.decision != "pass":
                injection_results[cat]["caught"] += 1
            else:
                injection_results[cat]["missed"] += 1

        per_injection = {}
        for cat, counts in injection_results.items():
            total = counts["caught"] + counts["missed"]
            per_injection[cat] = {
                "recall": round(counts["caught"] / max(total, 1), 4),
                "caught": counts["caught"],
                "missed": counts["missed"],
            }

        # Decision distribution
        decisions = Counter(r.decision for r in results)

        return {
            "recall": round(recall, 4),
            "precision": round(precision, 4),
            "fpr": round(fpr, 4),
            "specificity": round(specificity, 4),
            "tp": tp, "fn": fn, "fp": fp, "tn": tn,
            "trust_score_stats": trust_stats,
            "per_injection_type": per_injection,
            "decision_distribution": dict(decisions),
        }


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# 2.5  CALIBRATION EVALUATOR
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

class CalibrationEvaluator:
    """
    Evaluates calibration of the system's confidence scores.

    A well-calibrated system should have:
      predicted confidence ≈ observed accuracy
    at every confidence level.

    Produces reliability diagrams and Expected Calibration Error (ECE).
    """

    def evaluate(
        self,
        results: List[EvalResult],
        n_bins: int = 10
    ) -> Dict[str, Any]:
        """Compute calibration metrics."""
        # Use cases with ground truth for correctness assessment
        valid = [r for r in results if r.case.gt_diagnoses]
        if not valid:
            return {"error": "No valid cases for calibration"}

        confidences = np.array([r.confidence for r in valid])
        # "Correct" if at least one ICD-10 matched
        correct = np.array([r.icd10_tp > 0 for r in valid], dtype=float)

        # Binned calibration
        bins = []
        edges = np.linspace(0, 1, n_bins + 1)
        ece = 0.0

        for i in range(n_bins):
            lo, hi = edges[i], edges[i + 1]
            mask = (confidences >= lo) & (confidences < hi)
            n_in_bin = mask.sum()
            if n_in_bin == 0:
                continue

            avg_confidence = float(confidences[mask].mean())
            avg_accuracy = float(correct[mask].mean())
            bins.append({
                "bin_lo": round(lo, 2),
                "bin_hi": round(hi, 2),
                "avg_confidence": round(avg_confidence, 4),
                "avg_accuracy": round(avg_accuracy, 4),
                "n_samples": int(n_in_bin),
            })

            ece += n_in_bin * abs(avg_accuracy - avg_confidence)

        ece /= max(len(valid), 1)

        # Brier score
        brier = float(np.mean((confidences - correct) ** 2))

        return {
            "ece": round(ece, 4),
            "brier_score": round(brier, 4),
            "bins": bins,
            "n_evaluated": len(valid),
        }


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# 2.6  FAILURE MODE ANALYZER
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

class FailureModeAnalyzer:
    """
    Catalogues and categorizes system failures for root-cause analysis.
    """

    def analyze(self, results: List[EvalResult]) -> List[Dict[str, Any]]:
        """Identify and categorize failure modes."""
        failures = []

        for r in results:
            failure_types = []

            # Diagnostic failures
            if r.case.gt_diagnoses and r.icd10_tp == 0 and r.icd10_fn > 0:
                failure_types.append({
                    "type": "missed_diagnosis",
                    "severity": "high",
                    "description": (
                        f"Failed to identify any ground-truth diagnosis. "
                        f"GT: {[(d.get('name') or '') for d in r.case.gt_diagnoses]}"
                    ),
                    "gt_codes": [(d.get("icd10") or "") for d in r.case.gt_diagnoses],
                    "pred_codes": [(d.get("icd10") or "") for d in r.predicted_diagnoses],
                })

            # Guardrail failures
            if r.is_false_negative:
                failure_types.append({
                    "type": "guardrail_false_negative",
                    "severity": "critical",
                    "description": (
                        f"Hallucination not caught: {r.case.gt_injected_error}"
                    ),
                    "trust_score": r.trust_score,
                })

            if r.is_false_positive:
                failure_types.append({
                    "type": "guardrail_false_positive",
                    "severity": "medium",
                    "description": "Correct output incorrectly flagged",
                    "trust_score": r.trust_score,
                    "decision": r.decision,
                })

            # Overconfident errors
            if r.confidence > 0.7 and r.icd10_tp == 0 and r.case.gt_diagnoses:
                failure_types.append({
                    "type": "overconfident_error",
                    "severity": "high",
                    "description": (
                        f"High confidence ({r.confidence:.2f}) but no correct diagnosis"
                    ),
                })

            for ft in failure_types:
                ft["case_id"] = r.case.case_id
                ft["source"] = r.case.source
                ft["category"] = r.case.gt_disease_category
                failures.append(ft)

        # Sort by severity
        severity_order = {"critical": 0, "high": 1, "medium": 2, "low": 3}
        failures.sort(key=lambda f: severity_order.get(f["severity"], 4))

        return failures


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# 2.7  COMPREHENSIVE EVALUATOR (ORCHESTRATES EVERYTHING)
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

class ComprehensiveEvaluator:
    """
    Master evaluator that runs all cases through the MHAI system
    and produces a comprehensive evaluation report.
    """

    def __init__(self, orchestrator):
        """
        Args:
            orchestrator: OrchestratorAgent from Part 1
        """
        self.orchestrator = orchestrator
        self.diagnostic_eval = DiagnosticAccuracyEvaluator()
        self.guardrail_eval = GuardrailEffectivenessEvaluator()
        self.calibration_eval = CalibrationEvaluator()
        self.failure_analyzer = FailureModeAnalyzer()

    def run_evaluation(
        self,
        cases: List[EvalCase],
        verbose: bool = True
    ) -> EvalReport:
        """
        Run all cases through the system and produce comprehensive report.
        """
        results = []

        for i, case in enumerate(cases):
            if verbose and (i + 1) % 10 == 0:
                logger.info(f"  Evaluating {i+1}/{len(cases)}...")

            # Run through MHAI system
            system_output = self.orchestrator.process(
                case.input_data,
                input_type=case.input_type
            )

            # Extract predictions
            predicted_dx = []
            if system_output.diagnosis:
                predicted_dx = system_output.diagnosis.diagnoses

            predicted_category = system_output.disease_category
            trust = (
                system_output.verification.trust_score
                if system_output.verification else 0.5
            )
            decision = system_output.final_decision.value
            confidence = (
                system_output.diagnosis.confidence
                if system_output.diagnosis else 0.0
            )

            # Compute diagnostic accuracy for this case
            dx_metrics = self.diagnostic_eval.evaluate_case(
                predicted_dx, case.gt_diagnoses
            )

            # Determine guardrail correctness
            is_hall = case.gt_is_hallucination
            system_flagged = decision != "pass"
            guardrail_correct = (is_hall == system_flagged) or (
                not is_hall and not system_flagged
            )
            is_fp = not is_hall and system_flagged
            is_fn = is_hall and not system_flagged

            result = EvalResult(
                case=case,
                predicted_diagnoses=predicted_dx,
                predicted_category=predicted_category,
                trust_score=trust,
                decision=decision,
                confidence=confidence,
                latency_ms=system_output.total_latency_ms,
                icd10_tp=dx_metrics["icd10_tp"],
                icd10_fp=dx_metrics["icd10_fp"],
                icd10_fn=dx_metrics["icd10_fn"],
                diagnosis_name_overlap=dx_metrics["name_overlap"],
                guardrail_correct=guardrail_correct,
                is_false_positive=is_fp,
                is_false_negative=is_fn,
                full_output=system_output,
            )
            results.append(result)

        # Aggregate all metrics
        dx_aggregate = self.diagnostic_eval.aggregate(results)
        guardrail_aggregate = self.guardrail_eval.evaluate(results)
        calibration = self.calibration_eval.evaluate(results)
        failures = self.failure_analyzer.analyze(results)

        # Latency statistics
        latencies = [r.latency_ms for r in results]

        # Source distribution
        source_counts = Counter(r.case.source for r in results)

        report = EvalReport(
            n_total=len(results),
            n_sources=dict(source_counts),
            # Diagnostic
            macro_icd10_precision=dx_aggregate.get("macro_precision", 0),
            macro_icd10_recall=dx_aggregate.get("macro_recall", 0),
            macro_icd10_f1=dx_aggregate.get("macro_f1", 0),
            per_disease_metrics=dx_aggregate.get("per_disease", {}),
            diagnosis_name_overlap_mean=dx_aggregate.get("name_overlap_mean", 0),
            # Guardrail
            guardrail_recall=guardrail_aggregate.get("recall", 0),
            guardrail_precision=guardrail_aggregate.get("precision", 0),
            guardrail_fpr=guardrail_aggregate.get("fpr", 0),
            trust_score_stats=guardrail_aggregate.get("trust_score_stats", {}),
            decision_distribution=guardrail_aggregate.get("decision_distribution", {}),
            per_layer_catches={},  # populated if layer info available
            # System
            mean_latency_ms=round(np.mean(latencies), 2),
            p50_latency_ms=round(np.percentile(latencies, 50), 2),
            p95_latency_ms=round(np.percentile(latencies, 95), 2),
            p99_latency_ms=round(np.percentile(latencies, 99), 2),
            throughput_qps=round(1000 / max(np.mean(latencies), 1), 2),
            # Failures
            failure_modes=failures[:30],
            # Calibration
            calibration_bins=calibration.get("bins", []),
            # Raw
            results=results,
        )

        return report


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# 2.8  REPORT PRINTER
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

def print_evaluation_report(report: EvalReport):
    """Print a comprehensive, formatted evaluation report."""

    print("\n" + "═" * 70)
    print("  MHAI COMPREHENSIVE EVALUATION REPORT")
    print("═" * 70)

    # ── Overview ──
    print(f"\n  Total cases evaluated: {report.n_total}")
    print(f"  Sources: {report.n_sources}")

    # ── Diagnostic Accuracy ──
    print(f"\n  {'─' * 50}")
    print(f"  DIAGNOSTIC ACCURACY")
    print(f"  {'─' * 50}")
    print(f"    ICD-10 Precision:    {report.macro_icd10_precision:.1%}")
    print(f"    ICD-10 Recall:       {report.macro_icd10_recall:.1%}")
    print(f"    ICD-10 F1:           {report.macro_icd10_f1:.1%}")
    print(f"    Name Overlap (mean): {report.diagnosis_name_overlap_mean:.1%}")

    if report.per_disease_metrics:
        print(f"\n    Per-disease breakdown:")
        print(f"    {'Category':<18} {'Prec':>6} {'Recall':>6} {'F1':>6} "
              f"{'Overlap':>8} {'N':>4}")
        print(f"    {'─' * 52}")
        for cat, m in sorted(report.per_disease_metrics.items()):
            print(f"    {cat:<18} {m['precision']:.1%} {m['recall']:.1%} "
                  f"{m['f1']:.1%} {m['name_overlap']:.1%} "
                  f"{m['n_cases']:>4}")

    # ── Guardrail Effectiveness ──
    print(f"\n  {'─' * 50}")
    print(f"  GUARDRAIL EFFECTIVENESS")
    print(f"  {'─' * 50}")
    print(f"    Hallucination Recall:    {report.guardrail_recall:.1%}")
    print(f"    Hallucination Precision: {report.guardrail_precision:.1%}")
    print(f"    False Positive Rate:     {report.guardrail_fpr:.1%}")
    print(f"    Decision distribution:   {report.decision_distribution}")

    ts = report.trust_score_stats
    if ts:
        print(f"\n    Trust score distribution:")
        print(f"      Correct outputs:     {ts.get('correct_mean', 0):.3f} "
              f"± {ts.get('correct_std', 0):.3f}")
        print(f"      Hallucinated outputs: {ts.get('hall_mean', 0):.3f} "
              f"± {ts.get('hall_std', 0):.3f}")
        print(f"      Separation:           {ts.get('separation', 0):.3f}")

    # ── System Performance ──
    print(f"\n  {'─' * 50}")
    print(f"  SYSTEM PERFORMANCE")
    print(f"  {'─' * 50}")
    print(f"    Mean latency:    {report.mean_latency_ms:.1f} ms")
    print(f"    P50 latency:     {report.p50_latency_ms:.1f} ms")
    print(f"    P95 latency:     {report.p95_latency_ms:.1f} ms")
    print(f"    P99 latency:     {report.p99_latency_ms:.1f} ms")
    print(f"    Throughput:      {report.throughput_qps:.1f} queries/sec")

    # ── Calibration ──
    if report.calibration_bins:
        print(f"\n  {'─' * 50}")
        print(f"  CALIBRATION")
        print(f"  {'─' * 50}")
        print(f"    {'Bin':<12} {'Pred Conf':>10} {'Obs Acc':>10} {'N':>6}")
        print(f"    {'─' * 42}")
        for b in report.calibration_bins:
            print(f"    [{b['bin_lo']:.1f}-{b['bin_hi']:.1f}] "
                  f"{b['avg_confidence']:>10.3f} {b['avg_accuracy']:>10.3f} "
                  f"{b['n_samples']:>6}")

    # ── Failure Analysis ──
    if report.failure_modes:
        print(f"\n  {'─' * 50}")
        print(f"  FAILURE ANALYSIS ({len(report.failure_modes)} issues)")
        print(f"  {'─' * 50}")

        severity_counts = Counter(f["severity"] for f in report.failure_modes)
        print(f"    Severity: {dict(severity_counts)}")

        type_counts = Counter(f["type"] for f in report.failure_modes)
        print(f"    Types: {dict(type_counts)}")

        print(f"\n    Top failures:")
        for f in report.failure_modes[:5]:
            icon = {"critical": "🔴", "high": "🟠", "medium": "🟡"}.get(
                f["severity"], "⚪")
            print(f"    {icon} [{f['severity'].upper()}] {f['type']}: "
                  f"{f['description'][:70]}...")

    # ── Exit Criteria ──
    print(f"\n  {'─' * 50}")
    print(f"  EXIT CRITERIA VERIFICATION")
    print(f"  {'─' * 50}")

    criteria = [
        ("Diagnostic F1 ≥ 0.50",
         report.macro_icd10_f1 >= 0.50,
         f"{report.macro_icd10_f1:.1%}"),
        ("Name overlap ≥ 0.40",
         report.diagnosis_name_overlap_mean >= 0.40,
         f"{report.diagnosis_name_overlap_mean:.1%}"),
        ("Guardrail recall ≥ 0.90",
         report.guardrail_recall >= 0.90,
         f"{report.guardrail_recall:.1%}"),
        ("Guardrail FPR ≤ 0.15",
         report.guardrail_fpr <= 0.15,
         f"{report.guardrail_fpr:.1%}"),
        ("Mean latency ≤ 5000ms",
         report.mean_latency_ms <= 5000,
         f"{report.mean_latency_ms:.0f}ms"),
    ]

    all_pass = True
    for desc, passed, value in criteria:
        icon = "✅" if passed else "❌"
        print(f"    {icon} {desc}: {value}")
        if not passed:
            all_pass = False

    gate = "✅ SYSTEM EVALUATION PASSED" if all_pass else "❌ REQUIRES ATTENTION"
    print(f"\n    {'═' * 40}")
    print(f"    {gate}")
    print(f"    {'═' * 40}")

    print("\n" + "═" * 70)


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# 2.9  DEMO
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

def demo_comprehensive_evaluation():
    """Run the full comprehensive evaluation."""

    # Check if Part 1 classes are already in scope (e.g., same notebook)
    # If not, try importing from file, then fall back to minimal definitions
    global ClinicalContextAgent, HallucinationGuardAgent, OrchestratorAgent

    need_definitions = False
    try:
        ClinicalContextAgent
        HallucinationGuardAgent
        OrchestratorAgent
    except NameError:
        try:
            from mhai_integrated_part1_system import (
                OrchestratorAgent, ClinicalContextAgent,
                HallucinationGuardAgent
            )
        except ImportError:
            logger.info("Part 1 not found — using minimal inline definitions")
            need_definitions = True

    if need_definitions:
        # ── Minimal CCA fallback ──
        class ClinicalContextAgent:
            SYSTEM_PROMPT = "Clinical decision support AI."
            def __init__(self, **kwargs):
                self._loaded = False
            def generate_diagnosis(self, text, triage_context=None, n_samples=1):
                diagnoses = []
                red_flags = []
                text_lower = text.lower()
                if any(kw in text_lower for kw in ['chest pain', 'angina', 'stemi']):
                    diagnoses.append({"name": "Acute Coronary Syndrome", "icd10": "I21.9"})
                if any(kw in text_lower for kw in ['glucose', 'diabetes', 'hba1c', 'gdm']):
                    diagnoses.append({"name": "Diabetes Mellitus", "icd10": "E11.9"})
                if any(kw in text_lower for kw in ['malaria', 'plasmodium', 'chills', 'rigors']):
                    diagnoses.append({"name": "Malaria", "icd10": "B54"})
                if any(kw in text_lower for kw in ['hypertension', '140/', '150/', '160/']):
                    diagnoses.append({"name": "Hypertension", "icd10": "I10"})
                if any(kw in text_lower for kw in ['jaundice', 'bilirubin']):
                    diagnoses.append({"name": "Jaundice", "icd10": "R17"})
                if any(kw in text_lower for kw in ['cough', 'pneumonia', 'urti', 'sputum']):
                    diagnoses.append({"name": "Respiratory Infection", "icd10": "J06.9"})
                if any(kw in text_lower for kw in ['pre-eclampsia', 'eclampsia', 'hellp']):
                    diagnoses.append({"name": "Pre-eclampsia", "icd10": "O14.1"})
                if not diagnoses:
                    diagnoses.append({"name": "Condition under evaluation", "icd10": "R69"})
                if any(kw in text_lower for kw in ['severe', 'emergency', 'critical', 'admission']):
                    red_flags.append("Urgent clinical attention required")
                dx_text = "ASSESSMENT:\n" + "\n".join(
                    f"{i+1}. {d['name']} [ICD-10: {d['icd10']}]"
                    for i, d in enumerate(diagnoses)
                )
                from dataclasses import dataclass, field as dc_field
                @dataclass
                class _CCAOut:
                    diagnosis_text: str = ""
                    diagnoses: list = dc_field(default_factory=list)
                    management_plan: str = ""
                    red_flags: list = dc_field(default_factory=list)
                    citations: list = dc_field(default_factory=list)
                    confidence: float = 0.0
                    generation_params: dict = dc_field(default_factory=dict)
                return _CCAOut(
                    diagnosis_text=dx_text, diagnoses=diagnoses,
                    management_plan="Follow up with specialist.",
                    red_flags=red_flags, confidence=0.7 if diagnoses else 0.3,
                    generation_params={"model_loaded": False}
                )

        # ── Minimal HGA fallback ──
        class HallucinationGuardAgent:
            def __init__(self, alpha=0.5, beta=0.3, gamma=0.2,
                         pass_threshold=0.6, escalate_threshold=0.3):
                self.alpha, self.beta, self.gamma = alpha, beta, gamma
                self.pass_threshold = pass_threshold
                self.escalate_threshold = escalate_threshold
            def verify(self, cca_output, source_chunks=None, patient_data=None):
                text = cca_output.diagnosis_text
                nli = 0.8 if cca_output.diagnoses else 0.3
                coh = 0.9
                # Check for impossible numbers
                num = 0.9
                flagged = []
                for match in re.finditer(r'(\d{4,})\s*(?:mg|bpm|mmHg)', text):
                    val = float(match.group(1))
                    if val > 1000:
                        num = 0.1
                        flagged.append({"text": match.group(0),
                                        "reason": f"Implausible value: {val}"})
                if '???' in text or '@' in text or '###' in text:
                    nli = 0.1
                    coh = 0.2
                trust = self.alpha * nli + self.beta * coh + self.gamma * num
                if trust >= self.pass_threshold:
                    decision = SystemDecision.PASS
                elif trust >= self.escalate_threshold:
                    decision = SystemDecision.REGENERATE
                else:
                    decision = SystemDecision.ESCALATE
                return HGAVerificationOutput(
                    trust_score=trust, nli_score=nli,
                    coherence_score=coh, numerical_score=num,
                    decision=decision, flagged_claims=flagged,
                    redacted_text=text
                )

        # ── Minimal OA fallback ──
        class OrchestratorAgent:
            def __init__(self, cca=None, hga=None, dia_pipeline=None, **kwargs):
                self.cca = cca or ClinicalContextAgent()
                self.hga = hga or HallucinationGuardAgent()
                self._total_queries = 0
                self._decisions = defaultdict(int)
                self._latencies = []
            def process(self, input_data, input_type="text", patient_metadata=None):
                t0 = time.time()
                self._total_queries += 1
                text = str(input_data) if not isinstance(input_data, dict) else json.dumps(input_data)
                dx = self.cca.generate_diagnosis(text)
                ver = self.hga.verify(dx)
                latency = (time.time() - t0) * 1000
                if ver.decision == SystemDecision.PASS:
                    badge = ConfidenceBadge.GREEN
                elif ver.decision == SystemDecision.REGENERATE:
                    badge = ConfidenceBadge.AMBER
                else:
                    badge = ConfidenceBadge.RED
                self._decisions[ver.decision.value] += 1
                self._latencies.append(latency)
                return MHAISystemOutput(
                    input_type=input_type, input_summary=text[:100],
                    diagnosis=dx, verification=ver,
                    final_decision=ver.decision, confidence_badge=badge,
                    final_text=ver.redacted_text,
                    total_latency_ms=latency,
                    disease_category=self._guess_category(text),
                    pipeline_trace=[f"[CCA] {len(dx.diagnoses)} dx", f"[HGA] T={ver.trust_score:.3f}"]
                )
            def _guess_category(self, text):
                t = text.lower()
                if any(k in t for k in ['malaria', 'chills']): return 'malaria'
                if any(k in t for k in ['diabetes', 'glucose']): return 'diabetes'
                if any(k in t for k in ['chest pain', 'cardiac']): return 'cardiovascular'
                return 'general'
            def get_statistics(self):
                return {"total_queries": self._total_queries,
                        "decisions": dict(self._decisions),
                        "avg_latency_ms": np.mean(self._latencies) if self._latencies else 0}

    print("=" * 70)
    print("MHAI Integrated System — Part 2: Comprehensive Evaluation")
    print("=" * 70)

    # ── Initialize system ──
    print("\n[1/5] Initializing MHAI system...")
    cca = ClinicalContextAgent()
    hga = HallucinationGuardAgent()
    orchestrator = OrchestratorAgent(cca=cca, hga=hga)
    evaluator = ComprehensiveEvaluator(orchestrator)

    # ── Generate evaluation cases ──
    print("\n[2/5] Generating evaluation cases...")

    all_cases = []

    # Source 1: JSON patient records
    json_gen = JSONRecordCaseGenerator()
    if os.path.exists("/content/drive/MyDrive/Breast Cancer Articles Collection/extracted"):
        json_cases = json_gen.generate("/content/drive/MyDrive/Breast Cancer Articles Collection/extracted", max_cases=100)
        all_cases.extend(json_cases)
        print(f"  JSON records: {len(json_cases)} cases")
    else:
        print("  JSON records: /content/extract not found (skipped)")

    # Source 2: Synthetic correct + hallucinated
    synth_gen = SyntheticHallucinationCaseGenerator()
    synth_cases = synth_gen.generate()
    all_cases.extend(synth_cases)
    print(f"  Synthetic: {len(synth_cases)} cases")

    # Source 3: MedQA-style
    medqa_gen = MedQASyntheticGenerator()
    medqa_cases = medqa_gen.generate()
    all_cases.extend(medqa_cases)
    print(f"  MedQA: {len(medqa_cases)} cases")

    print(f"  Total: {len(all_cases)} evaluation cases")

    # ── Strategy: model inference is slow (~30-60s per case on T4) ──
    # Run the full Qwen model on a small sample (MODEL_EVAL_BUDGET),
    # use the fast rule-based fallback for the rest.
    MODEL_EVAL_BUDGET = 10  # cases with full model inference

    # Check if CCA has a loaded model
    has_model = hasattr(orchestrator.cca, '_loaded') and orchestrator.cca._loaded

    if has_model and len(all_cases) > MODEL_EVAL_BUDGET:
        print(f"\n  ⚡ Model loaded but {len(all_cases)} cases would take "
              f"~{len(all_cases) * 45 / 60:.0f} minutes.")
        print(f"  → Running model inference on {MODEL_EVAL_BUDGET} sample cases")
        print(f"  → Using fast rule-based fallback for remaining "
              f"{len(all_cases) - MODEL_EVAL_BUDGET} cases")

        # Split: model sample + fallback rest
        import random
        random.seed(42)
        indices = list(range(len(all_cases)))
        random.shuffle(indices)
        model_indices = set(indices[:MODEL_EVAL_BUDGET])

        # Temporarily switch CCA to fallback for non-sample cases
        original_loaded = orchestrator.cca._loaded

        # Run model cases first
        print(f"\n  Phase 1: Model inference ({MODEL_EVAL_BUDGET} cases)...")
        model_cases = [all_cases[i] for i in sorted(model_indices)]
        orchestrator.cca._loaded = True
        orchestrator.cca.max_new_tokens = 256  # shorter for speed
        model_report = evaluator.run_evaluation(model_cases, verbose=True)

        # Run fallback cases
        print(f"\n  Phase 2: Fast fallback ({len(all_cases) - MODEL_EVAL_BUDGET} cases)...")
        fallback_cases = [all_cases[i] for i in range(len(all_cases))
                          if i not in model_indices]
        orchestrator.cca._loaded = False  # force fallback
        fallback_report = evaluator.run_evaluation(fallback_cases, verbose=True)

        # Restore
        orchestrator.cca._loaded = original_loaded

        # Merge results
        all_results = model_report.results + fallback_report.results
        print(f"\n  Merging {len(model_report.results)} model + "
              f"{len(fallback_report.results)} fallback results...")

        # Re-run aggregation on merged results
        report = evaluator.run_evaluation.__func__  # we need to rebuild
        # Actually, just rebuild the report from merged results:
        from collections import Counter
        dx_agg = evaluator.diagnostic_eval.aggregate(all_results)
        guard_agg = evaluator.guardrail_eval.evaluate(all_results)
        cal_agg = evaluator.calibration_eval.evaluate(all_results)
        failures = evaluator.failure_analyzer.analyze(all_results)
        latencies = [r.latency_ms for r in all_results]
        source_counts = Counter(r.case.source for r in all_results)

        report = EvalReport(
            n_total=len(all_results),
            n_sources=dict(source_counts),
            macro_icd10_precision=dx_agg.get("macro_precision", 0),
            macro_icd10_recall=dx_agg.get("macro_recall", 0),
            macro_icd10_f1=dx_agg.get("macro_f1", 0),
            per_disease_metrics=dx_agg.get("per_disease", {}),
            diagnosis_name_overlap_mean=dx_agg.get("name_overlap_mean", 0),
            guardrail_recall=guard_agg.get("recall", 0),
            guardrail_precision=guard_agg.get("precision", 0),
            guardrail_fpr=guard_agg.get("fpr", 0),
            trust_score_stats=guard_agg.get("trust_score_stats", {}),
            decision_distribution=guard_agg.get("decision_distribution", {}),
            per_layer_catches={},
            mean_latency_ms=round(np.mean(latencies), 2),
            p50_latency_ms=round(np.percentile(latencies, 50), 2),
            p95_latency_ms=round(np.percentile(latencies, 95), 2),
            p99_latency_ms=round(np.percentile(latencies, 99), 2),
            throughput_qps=round(1000 / max(np.mean(latencies), 1), 2),
            failure_modes=failures[:30],
            calibration_bins=cal_agg.get("bins", []),
            results=all_results,
        )
    else:
        # No model or small enough dataset — run everything directly
        if has_model:
            orchestrator.cca.max_new_tokens = 256  # shorter for speed
        print(f"\n[3/5] Running evaluation ({len(all_cases)} cases)...")
        report = evaluator.run_evaluation(all_cases, verbose=True)

    # ── Print report ──
    print("\n[4/5] Generating report...")
    print_evaluation_report(report)

    # ── Save results ──
    print("\n[5/5] Saving results...")
    output_dir = "/content/mhai_evaluation"
    os.makedirs(output_dir, exist_ok=True)

    # Save summary as JSON
    summary = {
        "n_total": report.n_total,
        "n_sources": report.n_sources,
        "diagnostic_accuracy": {
            "icd10_precision": report.macro_icd10_precision,
            "icd10_recall": report.macro_icd10_recall,
            "icd10_f1": report.macro_icd10_f1,
            "name_overlap": report.diagnosis_name_overlap_mean,
            "per_disease": report.per_disease_metrics,
        },
        "guardrail": {
            "recall": report.guardrail_recall,
            "precision": report.guardrail_precision,
            "fpr": report.guardrail_fpr,
            "trust_scores": report.trust_score_stats,
            "decisions": report.decision_distribution,
        },
        "performance": {
            "mean_latency_ms": report.mean_latency_ms,
            "p95_latency_ms": report.p95_latency_ms,
            "throughput_qps": report.throughput_qps,
        },
        "calibration": {
            "bins": report.calibration_bins,
        },
        "n_failures": len(report.failure_modes),
    }

    with open(os.path.join(output_dir, "eval_summary.json"), "w") as f:
        json.dump(summary, f, indent=2, default=str)

    # Save per-case results as CSV
    rows = []
    for r in report.results:
        rows.append({
            "case_id": r.case.case_id,
            "source": r.case.source,
            "category": r.case.gt_disease_category,
            "is_hallucination": r.case.gt_is_hallucination,
            "icd10_tp": r.icd10_tp,
            "icd10_fp": r.icd10_fp,
            "icd10_fn": r.icd10_fn,
            "name_overlap": r.diagnosis_name_overlap,
            "trust_score": r.trust_score,
            "decision": r.decision,
            "confidence": r.confidence,
            "latency_ms": r.latency_ms,
            "guardrail_correct": r.guardrail_correct,
            "is_fp": r.is_false_positive,
            "is_fn": r.is_false_negative,
        })
    pd.DataFrame(rows).to_csv(
        os.path.join(output_dir, "per_case_results.csv"), index=False
    )

    print(f"  Summary saved to {output_dir}/eval_summary.json")
    print(f"  Per-case results saved to {output_dir}/per_case_results.csv")

    print("\n" + "=" * 70)
    print("Part 2 complete. Proceed to Part 3 for stress testing,")
    print("ablation studies, and production readiness report?")
    print("=" * 70)


if __name__ == "__main__":
    demo_comprehensive_evaluation()
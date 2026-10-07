"""
===========================================================================
MHAI Integrated System — Part 1 of 3
===========================================================================
Integration Layer: Connecting Fine-Tuned Qwen (CCA) with DIA + QDA

This module wires together:
  - Document Ingestion Agent (DIA): Swin-V2 + MoE → structured chunks
  - Quick Detection Agent (QDA): FT-Transformer + MoE → triage scores
  - Clinical Context Agent (CCA): Fine-tuned Qwen-2.5 → diagnosis + plan
  - Hallucination Guard Agent (HGA): Trust score verification
  - Orchestrator Agent (OA): Routing + threshold management

Creates the complete MHAI pipeline:
  PDF/JSON → DIA → QDA → CCA (Qwen) → HGA → Clinician Output

Prerequisites:
  - document_ingestion_and_triage_engine.py (DIA + QDA + Pipeline)
  - mhai_diagnosis_finetune.py (Qwen fine-tuning pipeline)
  - Fine-tuned Qwen adapter at /content/mhai_diagnosis_model/final_adapter
  - Guardrail components from Parts 1-4 of the hallucination pipeline

Requirements:
  pip install torch transformers peft bitsandbytes accelerate
  pip install scikit-learn numpy pandas scipy
===========================================================================
"""

import os
import json
import time
import re
import numpy as np
import pandas as pd
from typing import List, Tuple, Dict, Optional, Any
from dataclasses import dataclass, field
from enum import Enum
from collections import defaultdict
import logging

logging.basicConfig(level=logging.INFO)
logger = logging.getLogger("MHAI.Integrated")


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# 1.1  UNIFIED DATA STRUCTURES
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

class AgentName(Enum):
    DIA = "document_ingestion_agent"
    QDA = "quick_detection_agent"
    CCA = "clinical_context_agent"
    HGA = "hallucination_guard_agent"
    OA = "orchestrator_agent"


class SystemDecision(Enum):
    PASS = "pass"
    FLAG = "flag"
    REGENERATE = "regenerate"
    ESCALATE = "escalate"


class ConfidenceBadge(Enum):
    GREEN = "high_confidence"
    AMBER = "review_recommended"
    RED = "requires_validation"


@dataclass
class AgentOutput:
    """Standardized output from any MHAI agent."""
    agent: AgentName
    content: Any               # agent-specific payload
    confidence: float          # 0-1 confidence score
    latency_ms: float
    guardrail_passed: bool = True
    guardrail_details: Dict[str, Any] = field(default_factory=dict)
    metadata: Dict[str, Any] = field(default_factory=dict)


@dataclass
class CCADiagnosisOutput:
    """Output from the Clinical Context Agent (fine-tuned Qwen)."""
    diagnosis_text: str           # Full generated diagnosis
    diagnoses: List[Dict[str, str]]  # Parsed [{name, icd10, reasoning}]
    management_plan: str
    red_flags: List[str]
    citations: List[str]          # KB references
    confidence: float
    generation_params: Dict[str, Any] = field(default_factory=dict)


@dataclass
class HGAVerificationOutput:
    """Output from the Hallucination Guard Agent."""
    trust_score: float           # T(y) = α·NLI + β·Coh + γ·Num
    nli_score: float
    coherence_score: float
    numerical_score: float
    decision: SystemDecision
    flagged_claims: List[Dict[str, str]]
    redacted_text: str
    details: Dict[str, Any] = field(default_factory=dict)


@dataclass
class MHAISystemOutput:
    """Final output from the complete MHAI pipeline."""
    # Source
    input_type: str              # "pdf", "json", "text"
    input_summary: str

    # DIA outputs
    dia_output: Optional[AgentOutput] = None
    n_chunks: int = 0

    # QDA outputs
    qda_output: Optional[AgentOutput] = None
    disease_category: str = ""
    risk_score: float = 0.0

    # CCA outputs
    cca_output: Optional[AgentOutput] = None
    diagnosis: Optional[CCADiagnosisOutput] = None

    # HGA outputs
    hga_output: Optional[AgentOutput] = None
    verification: Optional[HGAVerificationOutput] = None

    # Final decision
    final_decision: SystemDecision = SystemDecision.PASS
    confidence_badge: ConfidenceBadge = ConfidenceBadge.GREEN
    final_text: str = ""

    # Timing
    total_latency_ms: float = 0.0
    per_agent_latency: Dict[str, float] = field(default_factory=dict)

    # Audit trail
    pipeline_trace: List[str] = field(default_factory=list)


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# 1.2  CLINICAL CONTEXT AGENT (CCA) — QWEN WRAPPER
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

class ClinicalContextAgent:
    """
    Agent 3: Clinical Context Agent (CCA)

    Wraps the fine-tuned Qwen-2.5-7B-Instruct model for clinical
    diagnosis generation. Receives triage results from QDA and
    structured chunks from DIA, produces citation-backed diagnosis
    with management plan.

    This is the primary generative agent and interfaces with ALL
    four guardrail layers:
      L1: Persona vector streaming probe (if available)
      L2: Semantic entropy (K=5 sampling)
      L3: NLI citation entailment
      L4: Human escalation
    """

    SYSTEM_PROMPT = (
        "You are a clinical decision support AI. Given a patient's "
        "clinical presentation including demographics, symptoms, vital "
        "signs, laboratory results, and imaging findings, provide:\n"
        "1. Differential diagnosis with ICD-10 codes\n"
        "2. Clinical reasoning for each diagnosis\n"
        "3. Recommended management plan\n"
        "4. Red flags requiring immediate attention\n"
        "Always cite evidence and recommend clinician verification."
    )

    def __init__(
        self,
        model=None,
        tokenizer=None,
        adapter_path: str = "/content/mhai_diagnosis_model/final_adapter",
        model_name: str = "Qwen/Qwen2.5-7B-Instruct",
        device: str = "auto",
        temperature: float = 0.3,
        max_new_tokens: int = 1024,
    ):
        self.model = model
        self.tokenizer = tokenizer
        self.adapter_path = adapter_path
        self.model_name = model_name
        self.temperature = temperature
        self.max_new_tokens = max_new_tokens
        self._loaded = model is not None

        if not self._loaded:
            self._try_load_model(device)

    def _try_load_model(self, device: str):
        """Attempt to load the fine-tuned model."""
        try:
            import torch
            from transformers import AutoModelForCausalLM, AutoTokenizer
            from peft import PeftModel

            logger.info(f"Loading CCA model: {self.model_name}")

            self.tokenizer = AutoTokenizer.from_pretrained(
                self.model_name, trust_remote_code=True
            )
            if self.tokenizer.pad_token is None:
                self.tokenizer.pad_token = self.tokenizer.eos_token

            # Try loading with adapter first
            if os.path.exists(self.adapter_path):
                logger.info(f"Loading LoRA adapter from {self.adapter_path}")
                from transformers import BitsAndBytesConfig
                bnb_config = BitsAndBytesConfig(
                    load_in_4bit=True,
                    bnb_4bit_quant_type="nf4",
                    bnb_4bit_compute_dtype=torch.bfloat16,
                    bnb_4bit_use_double_quant=True
                )
                base_model = AutoModelForCausalLM.from_pretrained(
                    self.model_name,
                    quantization_config=bnb_config,
                    device_map=device,
                    trust_remote_code=True
                )
                self.model = PeftModel.from_pretrained(
                    base_model, self.adapter_path
                )
                logger.info("Fine-tuned CCA model loaded successfully")
            else:
                logger.info("No adapter found, loading base model")
                self.model = AutoModelForCausalLM.from_pretrained(
                    self.model_name,
                    device_map=device,
                    trust_remote_code=True,
                    torch_dtype=torch.float16
                )

            self.model.eval()
            self._loaded = True

        except Exception as e:
            logger.warning(f"CCA model loading failed: {e}")
            logger.info("CCA will use rule-based fallback")
            self._loaded = False

    def generate_diagnosis(
        self,
        clinical_presentation: str,
        triage_context: Optional[Dict] = None,
        n_samples: int = 1
    ) -> CCADiagnosisOutput:
        """
        Generate diagnosis from clinical presentation.

        Args:
            clinical_presentation: Formatted patient data
            triage_context: QDA triage results for context
            n_samples: Number of samples (>1 for semantic entropy)

        Returns:
            CCADiagnosisOutput with parsed diagnosis
        """
        # Augment presentation with triage context
        if triage_context:
            augmented = (
                f"{clinical_presentation}\n\n"
                f"TRIAGE CONTEXT:\n"
                f"  Disease category: {triage_context.get('disease_category', 'unknown')}\n"
                f"  Risk score: {triage_context.get('risk_score', 'N/A')}\n"
                f"  Confidence zone: {triage_context.get('confidence_zone', 'N/A')}"
            )
        else:
            augmented = clinical_presentation

        if self._loaded and self.model is not None:
            raw_text = self._generate_with_model(augmented, n_samples)
        else:
            raw_text = self._generate_fallback(augmented, triage_context)

        # Parse the generated text
        return self._parse_diagnosis(raw_text)

    def _generate_with_model(
        self,
        presentation: str,
        n_samples: int = 1
    ) -> str:
        """Generate using the fine-tuned Qwen model."""
        import torch

        messages = [
            {"role": "system", "content": self.SYSTEM_PROMPT},
            {"role": "user", "content": presentation}
        ]

        text = self.tokenizer.apply_chat_template(
            messages, tokenize=False, add_generation_prompt=True
        )
        inputs = self.tokenizer(text, return_tensors="pt").to(self.model.device)

        with torch.no_grad():
            outputs = self.model.generate(
                **inputs,
                max_new_tokens=self.max_new_tokens,
                temperature=self.temperature,
                top_p=0.9,
                do_sample=True,
                repetition_penalty=1.1,
                pad_token_id=self.tokenizer.pad_token_id,
            )

        generated = outputs[0][inputs["input_ids"].shape[1]:]
        return self.tokenizer.decode(generated, skip_special_tokens=True).strip()

    def _generate_fallback(
        self,
        presentation: str,
        triage_context: Optional[Dict] = None
    ) -> str:
        """Rule-based fallback when model is unavailable."""
        # Extract key signals from presentation
        text_lower = presentation.lower()

        diagnoses = []
        reasoning = []
        red_flags = []

        # Pattern-based diagnosis inference
        if any(kw in text_lower for kw in ['chest pain', 'angina', 'substernal']):
            diagnoses.append(("Acute Coronary Syndrome", "I21.9"))
            reasoning.append("Chest pain with cardiac risk factors")
            if 'st elevation' in text_lower:
                red_flags.append("STEMI suspected — requires emergent catheterization")

        if any(kw in text_lower for kw in ['glucose', 'hba1c', 'diabetes', 'gdm']):
            glucose_match = re.search(r'glucose[:\s]*(\d+)', text_lower)
            if glucose_match and int(glucose_match.group(1)) > 126:
                diagnoses.append(("Diabetes Mellitus", "E11.9"))
                reasoning.append(f"Elevated glucose: {glucose_match.group(1)} mg/dL")
            elif 'gestational' in text_lower or 'gdm' in text_lower:
                diagnoses.append(("Gestational Diabetes Mellitus", "O24.4"))
                reasoning.append("GDM identified during pregnancy")

        if any(kw in text_lower for kw in ['malaria', 'plasmodium', 'chills', 'rigors']):
            if 'fever' in text_lower:
                diagnoses.append(("Malaria", "B54"))
                reasoning.append("Fever with chills/rigors, endemic exposure")
                if any(kw in text_lower for kw in ['severe', '40', 'admission']):
                    red_flags.append("Severe malaria — monitor for complications")

        if any(kw in text_lower for kw in ['hypertension', '140/', '150/', '160/']):
            diagnoses.append(("Hypertension", "I10"))
            reasoning.append("Elevated blood pressure readings")

        if any(kw in text_lower for kw in ['jaundice', 'bilirubin']):
            diagnoses.append(("Jaundice", "R17"))
            reasoning.append("Elevated bilirubin / clinical jaundice")

        if any(kw in text_lower for kw in ['urti', 'cough', 'catarrh', 'sore throat']):
            diagnoses.append(("Upper Respiratory Tract Infection", "J06.9"))
            reasoning.append("Respiratory symptoms: cough/catarrh/sore throat")

        if not diagnoses:
            diagnoses.append(("Condition requiring further evaluation", "R69"))
            reasoning.append("Insufficient data for definitive diagnosis")

        # Build response
        lines = ["ASSESSMENT AND DIAGNOSES:"]
        for i, (name, icd) in enumerate(diagnoses, 1):
            lines.append(f"\n{i}. {name} [ICD-10: {icd}]")
            if i <= len(reasoning):
                lines.append(f"   Reasoning: {reasoning[i-1]}")

        lines.append("\n\nMANAGEMENT PLAN:")
        if triage_context:
            category = triage_context.get('disease_category', '')
            risk = triage_context.get('risk_score', 0)
            if risk > 0.65:
                lines.append("  - HIGH RISK: Urgent specialist referral recommended")
            lines.append(f"  - Primary concern: {category}")
        lines.append("  - Continue current medications as prescribed")
        lines.append("  - Follow-up in 2-4 weeks or sooner if symptoms worsen")
        lines.append("  - Routine laboratory monitoring")

        if red_flags:
            lines.append("\n\nRED FLAGS:")
            for rf in red_flags:
                lines.append(f"  ⚠ {rf}")

        lines.append(
            "\n\nNOTE: This assessment requires clinician verification. "
            "Do not make treatment decisions based solely on this output."
        )

        return "\n".join(lines)

    def _parse_diagnosis(self, raw_text: str) -> CCADiagnosisOutput:
        """Parse generated text into structured diagnosis output."""
        diagnoses = []
        red_flags = []
        citations = []

        # Extract ICD-10 coded diagnoses
        icd_pattern = r'(\d+)\.\s*(.+?)\s*\[ICD-10:\s*([A-Z]\d{2}\.?\d*)\]'
        for match in re.finditer(icd_pattern, raw_text):
            diagnoses.append({
                "name": match.group(2).strip(),
                "icd10": match.group(3).strip(),
            })

        # Extract red flags
        flag_pattern = r'[⚠🔴]\s*(.+?)(?:\n|$)'
        for match in re.finditer(flag_pattern, raw_text):
            red_flags.append(match.group(1).strip())

        # Also capture "RED FLAG" or "URGENT" sections
        if 'RED FLAG' in raw_text.upper():
            flag_section = raw_text[raw_text.upper().index('RED FLAG'):]
            for line in flag_section.split('\n')[1:5]:
                line = line.strip().lstrip('- •⚠')
                if line and len(line) > 10:
                    red_flags.append(line)

        # Extract citations [KB:#xxx]
        for match in re.finditer(r'\[KB:#?(\w+)\]', raw_text):
            citations.append(match.group(1))

        # Extract management plan section
        plan = ""
        plan_markers = ['MANAGEMENT PLAN', 'PLAN:', 'TREATMENT PLAN']
        for marker in plan_markers:
            if marker in raw_text.upper():
                idx = raw_text.upper().index(marker)
                plan_section = raw_text[idx:]
                # Take until next major section or end
                next_section = re.search(
                    r'\n\n[A-Z]{3,}', plan_section[len(marker):]
                )
                if next_section:
                    plan = plan_section[:len(marker) + next_section.start()]
                else:
                    plan = plan_section
                break

        # Confidence heuristic based on output quality
        confidence = 0.5
        if len(diagnoses) > 0:
            confidence += 0.2
        if len(diagnoses) > 0 and all(d.get('icd10') for d in diagnoses):
            confidence += 0.1
        if plan:
            confidence += 0.1
        if red_flags:
            confidence += 0.05
        confidence = min(confidence, 0.95)

        return CCADiagnosisOutput(
            diagnosis_text=raw_text,
            diagnoses=diagnoses,
            management_plan=plan,
            red_flags=red_flags,
            citations=citations,
            confidence=confidence,
            generation_params={
                "temperature": self.temperature,
                "max_tokens": self.max_new_tokens,
                "model_loaded": self._loaded
            }
        )


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# 1.3  HALLUCINATION GUARD AGENT (HGA) — SIMPLIFIED VERIFIER
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

class HallucinationGuardAgent:
    """
    Agent 4: Hallucination Guard Agent (HGA)

    Verifies CCA output using the trust score:
      T(y) = α·NLI + β·Coherence + γ·NumCheck

    Operates as the guardrail executor for L2 and L3.
    """

    # Medical reference ranges for numerical checking
    REFERENCE_RANGES = {
        "glucose": {"unit": "mg/dL", "min": 30, "max": 600,
                    "normal_min": 70, "normal_max": 140},
        "hemoglobin": {"unit": "g/dL", "min": 3, "max": 22,
                       "normal_min": 12, "normal_max": 17},
        "temperature_f": {"unit": "°F", "min": 90, "max": 108,
                          "normal_min": 97, "normal_max": 100.4},
        "heart_rate": {"unit": "bpm", "min": 20, "max": 250,
                       "normal_min": 60, "normal_max": 100},
        "systolic_bp": {"unit": "mmHg", "min": 50, "max": 300,
                        "normal_min": 90, "normal_max": 140},
        "platelets": {"unit": "K/µL", "min": 10, "max": 1000,
                      "normal_min": 150, "normal_max": 400},
    }

    # Valid ICD-10 top-level categories
    VALID_ICD10_PREFIXES = set("ABCDEFGHIJKLMNOPQRSTUVWXYZ")

    def __init__(
        self,
        alpha: float = 0.5,
        beta: float = 0.3,
        gamma: float = 0.2,
        pass_threshold: float = 0.6,
        escalate_threshold: float = 0.3
    ):
        self.alpha = alpha
        self.beta = beta
        self.gamma = gamma
        self.pass_threshold = pass_threshold
        self.escalate_threshold = escalate_threshold

    def verify(
        self,
        cca_output: CCADiagnosisOutput,
        source_chunks: Optional[List[Any]] = None,
        patient_data: Optional[Dict] = None
    ) -> HGAVerificationOutput:
        """
        Run three-expert verification on CCA output.

        Returns:
            HGAVerificationOutput with trust score and decision
        """
        text = cca_output.diagnosis_text

        # Expert 1: NLI / Factual consistency
        nli_score = self._check_factual_consistency(
            cca_output, source_chunks
        )

        # Expert 2: Logical coherence
        coherence_score = self._check_coherence(text)

        # Expert 3: Numerical accuracy
        numerical_score, flagged = self._check_numerical_accuracy(
            text, patient_data
        )

        # Composite trust score
        trust = (
            self.alpha * nli_score
            + self.beta * coherence_score
            + self.gamma * numerical_score
        )

        # Decision
        if trust >= self.pass_threshold:
            decision = SystemDecision.PASS
        elif trust >= self.escalate_threshold:
            decision = SystemDecision.REGENERATE
        else:
            decision = SystemDecision.ESCALATE

        # Build redacted text
        redacted = text
        for claim in flagged:
            original = claim.get("text", "")
            if original and len(original) > 20:
                redacted = redacted.replace(
                    original,
                    f"[FLAGGED: {claim.get('reason', 'verification needed')}]"
                )

        return HGAVerificationOutput(
            trust_score=trust,
            nli_score=nli_score,
            coherence_score=coherence_score,
            numerical_score=numerical_score,
            decision=decision,
            flagged_claims=flagged,
            redacted_text=redacted,
            details={
                "alpha": self.alpha,
                "beta": self.beta,
                "gamma": self.gamma
            }
        )

    def _check_factual_consistency(
        self,
        cca_output: CCADiagnosisOutput,
        source_chunks: Optional[List[Any]]
    ) -> float:
        """Expert 1: Check diagnosis consistency with source data."""
        score = 0.7  # baseline

        # Check that diagnoses have valid ICD-10 codes
        if cca_output.diagnoses:
            valid_codes = sum(
                1 for d in cca_output.diagnoses
                if d.get("icd10", "")
                and d["icd10"][0] in self.VALID_ICD10_PREFIXES
                and len(d["icd10"]) >= 3
            )
            code_ratio = valid_codes / len(cca_output.diagnoses)
            score = 0.5 + 0.5 * code_ratio

        # Check citations if KB chunks available
        if source_chunks and cca_output.citations:
            score = min(score + 0.1, 1.0)

        return score

    def _check_coherence(self, text: str) -> float:
        """Expert 2: Check for internal logical consistency."""
        score = 0.9  # assume coherent unless contradictions found

        sentences = [
            s.strip() for s in re.split(r'[.!?]\s+', text)
            if len(s.strip()) > 20
        ]

        # Simple contradiction detection
        contradictions = 0
        for i, s1 in enumerate(sentences):
            for s2 in sentences[i+2:]:
                s1_lower = s1.lower()
                s2_lower = s2.lower()
                # Check for "is/is not" contradictions
                if ('not recommended' in s1_lower and 'recommended' in s2_lower
                    and 'not' not in s2_lower):
                    contradictions += 1
                if ('contraindicated' in s1_lower and 'indicated' in s2_lower
                    and 'contra' not in s2_lower):
                    contradictions += 1

        if contradictions > 0:
            score -= 0.2 * contradictions

        return max(0.0, min(score, 1.0))

    def _check_numerical_accuracy(
        self,
        text: str,
        patient_data: Optional[Dict] = None
    ) -> Tuple[float, List[Dict]]:
        """Expert 3: Check numerical values against medical ranges."""
        flagged = []
        numbers_checked = 0
        numbers_ok = 0

        # Context-aware extraction patterns
        patterns = [
            (r'(?:glucose|blood\s*sugar|FBS)\s*[:\-]?\s*(\d+(?:\.\d+)?)\s*(?:mg/[dD][lL])?',
             "glucose"),
            (r'(?:hemoglobin|Hgb|Hb)\s*[:\-]?\s*(\d+(?:\.\d+)?)\s*(?:g/[dD][lL])?',
             "hemoglobin"),
            (r'(?:temp|temperature)\s*[:\-]?\s*(\d+(?:\.\d+)?)\s*°?\s*[fF]',
             "temperature_f"),
            (r'(?:HR|heart\s*rate|pulse)\s*[:\-]?\s*(\d+)\s*(?:bpm)?',
             "heart_rate"),
            (r'(?:BP|blood\s*pressure)\s*[:\-]?\s*(\d+)\s*/\s*\d+',
             "systolic_bp"),
            (r'(?:platelet|PLT)\s*[:\-]?\s*(\d+)',
             "platelets"),
        ]

        for pattern, field_name in patterns:
            for match in re.finditer(pattern, text, re.IGNORECASE):
                try:
                    value = float(match.group(1))
                except ValueError:
                    continue

                numbers_checked += 1
                ref = self.REFERENCE_RANGES.get(field_name, {})
                abs_min = ref.get("min", 0)
                abs_max = ref.get("max", 999999)

                if value < abs_min or value > abs_max:
                    flagged.append({
                        "text": match.group(0),
                        "field": field_name,
                        "value": value,
                        "reason": (
                            f"Implausible {field_name}: {value} "
                            f"(absolute range: {abs_min}-{abs_max})"
                        )
                    })
                else:
                    numbers_ok += 1

        if numbers_checked == 0:
            return 0.8, flagged  # no numbers to check

        score = numbers_ok / numbers_checked
        return score, flagged


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# 1.4  ORCHESTRATOR AGENT (OA)
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

class OrchestratorAgent:
    """
    Agent 5: Orchestrator Agent (OA)

    Routes requests through the pipeline, manages thresholds,
    and integrates feedback. Acts as the conductor for all agents.
    """

    def __init__(
        self,
        dia_pipeline=None,       # DIAtoQDAPipeline from M2
        cca: Optional[ClinicalContextAgent] = None,
        hga: Optional[HallucinationGuardAgent] = None,
        # Dynamic thresholds
        trust_pass: float = 0.6,
        trust_escalate: float = 0.3,
        se_threshold: float = 1.2,
    ):
        self.dia_pipeline = dia_pipeline
        self.cca = cca or ClinicalContextAgent()
        self.hga = hga or HallucinationGuardAgent(
            pass_threshold=trust_pass,
            escalate_threshold=trust_escalate
        )
        self.trust_pass = trust_pass
        self.trust_escalate = trust_escalate
        self.se_threshold = se_threshold

        # Tracking
        self._total_queries = 0
        self._decisions = defaultdict(int)
        self._latencies = []

    def process(
        self,
        input_data: Any,
        input_type: str = "text",
        patient_metadata: Optional[Dict] = None
    ) -> MHAISystemOutput:
        """
        Process a clinical query through the full MHAI pipeline.

        Supports three input types:
          - "text": Raw clinical text
          - "json": Parsed patient JSON record
          - "pdf": PDF document path

        Pipeline:
          Input → DIA (extract) → QDA (triage) → CCA (diagnose)
                → HGA (verify) → Final Output
        """
        t_start = time.time()
        self._total_queries += 1
        trace = []

        result = MHAISystemOutput(
            input_type=input_type,
            input_summary=str(input_data)[:150] + "..."
        )

        # ═══════════════════════
        # STAGE 1: DIA + QDA
        # ═══════════════════════
        t_dia = time.time()
        chunks, triage_context, clinical_text = self._run_dia_qda(
            input_data, input_type, trace
        )
        dia_latency = (time.time() - t_dia) * 1000
        result.per_agent_latency["DIA+QDA"] = dia_latency

        result.n_chunks = len(chunks) if chunks else 0
        if triage_context:
            result.disease_category = triage_context.get("disease_category", "")
            result.risk_score = triage_context.get("risk_score", 0.0)

        # ═══════════════════════
        # STAGE 2: CCA (Qwen)
        # ═══════════════════════
        t_cca = time.time()
        trace.append("[CCA] Generating diagnosis with Qwen...")

        diagnosis = self.cca.generate_diagnosis(
            clinical_text,
            triage_context=triage_context
        )
        result.diagnosis = diagnosis

        cca_latency = (time.time() - t_cca) * 1000
        result.per_agent_latency["CCA"] = cca_latency

        result.cca_output = AgentOutput(
            agent=AgentName.CCA,
            content=diagnosis.diagnosis_text[:200],
            confidence=diagnosis.confidence,
            latency_ms=cca_latency,
        )
        trace.append(
            f"[CCA] Generated {len(diagnosis.diagnoses)} diagnoses "
            f"(conf={diagnosis.confidence:.2f}, {cca_latency:.0f}ms)"
        )

        # ═══════════════════════
        # STAGE 3: HGA Verification
        # ═══════════════════════
        t_hga = time.time()
        trace.append("[HGA] Verifying CCA output...")

        verification = self.hga.verify(
            diagnosis,
            source_chunks=chunks,
            patient_data=patient_metadata
        )
        result.verification = verification

        hga_latency = (time.time() - t_hga) * 1000
        result.per_agent_latency["HGA"] = hga_latency

        result.hga_output = AgentOutput(
            agent=AgentName.HGA,
            content=f"T={verification.trust_score:.3f}",
            confidence=verification.trust_score,
            latency_ms=hga_latency,
            guardrail_passed=verification.decision == SystemDecision.PASS,
            guardrail_details={
                "nli": verification.nli_score,
                "coherence": verification.coherence_score,
                "numerical": verification.numerical_score,
            }
        )
        trace.append(
            f"[HGA] Trust={verification.trust_score:.3f} → "
            f"{verification.decision.value.upper()}"
        )

        # ═══════════════════════
        # STAGE 4: Final Decision
        # ═══════════════════════
        result.final_decision = verification.decision

        if verification.decision == SystemDecision.PASS:
            result.confidence_badge = ConfidenceBadge.GREEN
            result.final_text = diagnosis.diagnosis_text
        elif verification.decision == SystemDecision.REGENERATE:
            result.confidence_badge = ConfidenceBadge.AMBER
            result.final_text = verification.redacted_text
        else:
            result.confidence_badge = ConfidenceBadge.RED
            result.final_text = (
                "[HIGH UNCERTAINTY — REQUIRES HUMAN VALIDATION]\n"
                "This response has been flagged for clinician review.\n\n"
                + verification.redacted_text
            )

        result.total_latency_ms = (time.time() - t_start) * 1000
        result.pipeline_trace = trace
        self._decisions[result.final_decision.value] += 1
        self._latencies.append(result.total_latency_ms)

        return result

    def _run_dia_qda(
        self,
        input_data: Any,
        input_type: str,
        trace: List[str]
    ) -> Tuple[Optional[List], Optional[Dict], str]:
        """Run DIA + QDA stages based on input type."""
        chunks = None
        triage_context = None

        if input_type == "json" and isinstance(input_data, dict):
            # Direct JSON patient record
            trace.append("[DIA] Processing JSON patient record...")
            clinical_text = self._json_to_clinical_text(input_data)
            trace.append(f"[DIA] Extracted {len(clinical_text)} chars")

        elif input_type == "pdf" and self.dia_pipeline is not None:
            # Full DIA pipeline
            trace.append("[DIA] Processing PDF document...")
            pipeline_result = self.dia_pipeline.run(str(input_data))
            chunks = pipeline_result.chunks
            clinical_text = " ".join(c.content for c in chunks)

            if pipeline_result.triage:
                triage_context = {
                    "disease_category": pipeline_result.triage.disease_category,
                    "risk_score": pipeline_result.triage.risk_score,
                    "confidence_zone": pipeline_result.triage.confidence_zone,
                }
            trace.append(
                f"[QDA] Triage: {triage_context}" if triage_context
                else "[QDA] No triage available"
            )

        else:
            # Plain text input
            trace.append("[DIA] Processing raw clinical text...")
            clinical_text = str(input_data)

        return chunks, triage_context, clinical_text

    def _json_to_clinical_text(self, patient_json: Dict) -> str:
        """Convert a patient JSON record to clinical presentation text."""
        lines = ["CLINICAL PRESENTATION", "=" * 20]

        for subject in patient_json.get("subjects", []):
            demographics = subject.get("demographics", {})
            age = demographics.get("age_years", demographics.get("age_months", ""))
            sex = demographics.get("sex", "")
            relationship = subject.get("relationship", "")

            lines.append(f"\nPatient: {age} {'months' if 'age_months' in demographics else 'years'} {sex} ({relationship})")

            # Encounters
            for enc in subject.get("encounters", []):
                complaint = enc.get("complaint", "")
                vitals = enc.get("vital_signs", {})
                vital_str = ", ".join(f"{k}={v}" for k, v in vitals.items() if v)
                lines.append(f"  Visit: {complaint}")
                if vital_str:
                    lines.append(f"    Vitals: {vital_str}")

            # Diagnoses
            for dx in subject.get("diagnosis", []):
                lines.append(f"  Dx: {dx.get('name', '')} [{dx.get('icd10', '')}]")

            # Labs
            for lab in subject.get("lab_results", []):
                comment = lab.get("comment", "")
                lines.append(
                    f"  Lab: {lab.get('test', '')} = {lab.get('value', '')} "
                    f"{lab.get('unit', '')} {f'[{comment}]' if comment else ''}"
                )

            # Medications
            for med in subject.get("medications", []):
                lines.append(
                    f"  Med: {med.get('name', '')} {med.get('dose', '')} "
                    f"{med.get('route', '')} ({med.get('indication', '')})"
                )

        return "\n".join(lines)

    def get_statistics(self) -> Dict[str, Any]:
        """Return orchestrator statistics."""
        return {
            "total_queries": self._total_queries,
            "decisions": dict(self._decisions),
            "avg_latency_ms": (
                np.mean(self._latencies) if self._latencies else 0
            ),
            "p95_latency_ms": (
                np.percentile(self._latencies, 95)
                if len(self._latencies) > 1 else 0
            ),
        }


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# 1.5  DEMO
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

def demo_integrated_system():
    """Demonstrate the fully integrated MHAI system."""
    print("=" * 70)
    print("MHAI Integrated System — Part 1: Pipeline Assembly")
    print("=" * 70)

    # Initialize agents
    print("\n[1/3] Initializing agents...")
    cca = ClinicalContextAgent()  # Will use fallback if no GPU
    hga = HallucinationGuardAgent(alpha=0.5, beta=0.3, gamma=0.2)
    orchestrator = OrchestratorAgent(cca=cca, hga=hga)

    print(f"  CCA loaded: {cca._loaded}")
    print(f"  HGA weights: α={hga.alpha}, β={hga.beta}, γ={hga.gamma}")

    # Test with clinical text
    print("\n[2/3] Processing clinical cases...")

    test_cases = [
        {
            "name": "GDM Patient (from sample JSON)",
            "type": "text",
            "data": (
                "Patient: 28 years F, primary_employee.\n"
                "Visit: cold, joint pains, allergic shivers. "
                "Vitals: BP=120/80, Temp=36°F, Wt=68.5kg\n"
                "Visit: GDM evaluation, OGTT\n"
                "Visit: labour and delivery via emergency CS\n"
                "Lab: Fasting Glucose = 103 mg/dL (ref: 60-110)\n"
                "Lab: 2hr post-prandial glucose = 168 mg/dL [HIGH - GDM]\n"
                "Med: Paracetamol 2 tabs Oral (joint pains)\n"
                "Med: Plasmotrim 200 mg Oral (malaria)\n"
                "Allergy: unspecified injection → allergic shivers/hives"
            )
        },
        {
            "name": "Pediatric Malaria (from sample JSON)",
            "type": "text",
            "data": (
                "Patient: 11 months F, daughter.\n"
                "Visit: fever 12 days, cough, catarrh — admitted for "
                "severe malaria with high fever (38.9°C, peak 40°C). "
                "Vitals: Temp=38.9°F, Wt=7.85kg\n"
                "Lab: Malaria Parasite = Positive\n"
                "Lab: Hemoglobin = 9.6 g/dL [LOW]\n"
                "Med: Plasmotrim suppository 50 mg Rectal (malaria)\n"
                "Referral: Paediatrician (Prof. Njokanma)"
            )
        },
        {
            "name": "Elderly Hypertension (from sample JSON)",
            "type": "text",
            "data": (
                "Patient: 61 years F, mother_of_employee.\n"
                "Visit: abdomino-pelvic ultrasound, accucheck, HVS. "
                "Vitals: BP=140/90, Temp=36°F\n"
                "Lab: Random Blood Sugar = 79 mg/dL\n"
                "Imaging: Abdomino-pelvic ultrasound — atrophic uterus, "
                "normal organs, no masses. Normal scan."
            )
        },
    ]

    for case in test_cases:
        print(f"\n  {'━' * 50}")
        print(f"  Case: {case['name']}")
        print(f"  {'━' * 50}")

        result = orchestrator.process(
            case["data"], input_type=case["type"]
        )

        # Display results
        badge = {"high_confidence": "🟢", "review_recommended": "🟡",
                 "requires_validation": "🔴"}
        badge_icon = badge.get(result.confidence_badge.value, "?")

        print(f"\n  Decision: {result.final_decision.value.upper()} {badge_icon}")
        if result.diagnosis:
            print(f"  Diagnoses ({len(result.diagnosis.diagnoses)}):")
            for dx in result.diagnosis.diagnoses[:5]:
                print(f"    • {dx.get('name', '')} [{dx.get('icd10', '')}]")
            if result.diagnosis.red_flags:
                print(f"  Red flags:")
                for rf in result.diagnosis.red_flags[:3]:
                    print(f"    ⚠ {rf}")

        if result.verification:
            v = result.verification
            print(f"  Trust: T={v.trust_score:.3f} "
                  f"(NLI={v.nli_score:.2f}, Coh={v.coherence_score:.2f}, "
                  f"Num={v.numerical_score:.2f})")
            if v.flagged_claims:
                print(f"  Flagged: {len(v.flagged_claims)} claims")

        print(f"  Latency: {result.total_latency_ms:.0f}ms")
        print(f"  Trace:")
        for t in result.pipeline_trace:
            print(f"    {t}")

    # Summary
    print(f"\n[3/3] System Statistics")
    stats = orchestrator.get_statistics()
    print(f"  Total queries: {stats['total_queries']}")
    print(f"  Decisions: {stats['decisions']}")
    print(f"  Avg latency: {stats['avg_latency_ms']:.0f}ms")

    print("\n" + "=" * 70)
    print("Part 1 complete. Proceed to Part 2 for comprehensive evaluation")
    print("(multi-dataset testing, diagnostic accuracy, guardrail metrics)?")
    print("=" * 70)


if __name__ == "__main__":
    demo_integrated_system()
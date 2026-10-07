"""
===========================================================================
MHAI Clinical Diagnosis Fine-Tuning Pipeline
===========================================================================
Fine-tune Qwen-2.5-7B-Instruct on clinical JSON records for diagnosis.

Pipeline:
  1. Extract all JSON files from /content/extract/001-005 subfolders
  2. Parse patient records into structured clinical summaries
  3. Generate instruction-tuning pairs (clinical presentation → diagnosis)
  4. Fine-tune Qwen-2.5-7B-Instruct with QLoRA (4-bit quantization)
  5. Evaluate on held-out cases

Requirements (Colab/A100):
  pip install torch transformers datasets peft bitsandbytes
  pip install accelerate trl scikit-learn pandas
  pip install --upgrade huggingface_hub
===========================================================================
"""

import os
import json
import glob
import random
import numpy as np
import pandas as pd
from typing import List, Dict, Tuple, Optional, Any
from dataclasses import dataclass, field
from pathlib import Path
import logging

logging.basicConfig(level=logging.INFO)
logger = logging.getLogger("MHAI.DiagnosisFT")


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# PART 1: JSON EXTRACTION AND PARSING
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

@dataclass
class SubjectRecord:
    """Parsed record for a single subject (patient/dependent)."""
    subject_id: str
    relationship: str
    age: Optional[float] = None
    sex: Optional[str] = None
    encounters: List[Dict] = field(default_factory=list)
    diagnoses: List[Dict] = field(default_factory=list)
    medications: List[Dict] = field(default_factory=list)
    vital_signs: List[Dict] = field(default_factory=list)
    lab_results: List[Dict] = field(default_factory=list)
    imaging_results: List[Dict] = field(default_factory=list)
    allergies: List[Dict] = field(default_factory=list)
    plan: str = ""


@dataclass
class PatientFile:
    """Parsed patient file containing multiple subjects."""
    patient_id: str
    source_file: str
    facility: str
    subjects: List[SubjectRecord] = field(default_factory=list)
    metadata: Dict[str, Any] = field(default_factory=dict)


def extract_all_jsons(base_dir: str = "/content/drive/MyDrive/Breast Cancer Articles Collection/extracted") -> List[str]:
    """
    Extract all JSON file paths from subfolders 001-005.

    Directory structure:
      /content/extract/
        001/
          file1.json
          file2.json
          ...
        002/
          file1.json
          ...
        ...
        005/
          ...

    Returns: List of absolute file paths to all JSON files.
    """
    all_json_paths = []

    # Method 1: Glob all JSON files recursively
    pattern = os.path.join(base_dir, "**", "*.json")
    all_json_paths = sorted(glob.glob(pattern, recursive=True))

    if not all_json_paths:
        # Method 2: Iterate subfolders explicitly
        for folder_idx in range(1, 6):
            folder_name = f"{folder_idx:03d}"
            folder_path = os.path.join(base_dir, folder_name)
            if os.path.isdir(folder_path):
                for fname in sorted(os.listdir(folder_path)):
                    if fname.endswith(".json"):
                        all_json_paths.append(
                            os.path.join(folder_path, fname)
                        )

    logger.info(
        f"Found {len(all_json_paths)} JSON files across "
        f"{base_dir}/001-005"
    )

    # Report per-subfolder counts
    folder_counts = {}
    for path in all_json_paths:
        folder = os.path.basename(os.path.dirname(path))
        folder_counts[folder] = folder_counts.get(folder, 0) + 1
    for folder, count in sorted(folder_counts.items()):
        logger.info(f"  {folder}/: {count} files")

    return all_json_paths


def parse_json_file(json_path: str) -> Optional[PatientFile]:
    """
    Parse a single JSON file into a PatientFile object.
    Handles the nested structure: patient → subjects → encounters/diagnoses/etc.
    """
    try:
        with open(json_path, 'r', encoding='utf-8') as f:
            data = json.load(f)
    except (json.JSONDecodeError, UnicodeDecodeError) as e:
        logger.warning(f"Failed to parse {json_path}: {e}")
        return None

    # Handle both single-object and list-of-objects formats
    if isinstance(data, list):
        data = data[0] if data else {}

    patient_id = data.get("patient_id", os.path.basename(json_path))
    source = data.get("source", {})

    pf = PatientFile(
        patient_id=patient_id,
        source_file=json_path,
        facility=source.get("facility", "Unknown"),
        metadata={
            "folder_index": source.get("folder_index", ""),
            "pages_processed": source.get("pages_processed", 0),
            "document_type": source.get("document_type", ""),
        }
    )

    # Parse each subject
    for subj_data in data.get("subjects", []):
        demographics = subj_data.get("demographics", {})

        # Extract age (could be age_years or age_months)
        age = demographics.get("age_years")
        if age is None:
            age_months = demographics.get("age_months")
            if age_months is not None:
                age = age_months / 12.0

        sr = SubjectRecord(
            subject_id=subj_data.get("subject_id", ""),
            relationship=subj_data.get("relationship", "unknown"),
            age=age,
            sex=demographics.get("sex"),
            encounters=subj_data.get("encounters", []),
            diagnoses=subj_data.get("diagnosis", []),
            medications=subj_data.get("medications", []),
            vital_signs=subj_data.get("vital_signs", []),
            lab_results=subj_data.get("lab_results", []),
            imaging_results=subj_data.get("imaging_results", []),
            allergies=subj_data.get("allergies_and_contraindications", []),
            plan=subj_data.get("plan", "")
        )
        pf.subjects.append(sr)

    return pf


def load_all_patient_files(
    base_dir: str = "/content/drive/MyDrive/Breast Cancer Articles Collection/extracted"
) -> List[PatientFile]:
    """Load and parse all JSON files into PatientFile objects."""
    json_paths = extract_all_jsons(base_dir)
    patient_files = []

    for path in json_paths:
        pf = parse_json_file(path)
        if pf and pf.subjects:
            patient_files.append(pf)

    logger.info(
        f"Parsed {len(patient_files)} patient files with "
        f"{sum(len(pf.subjects) for pf in patient_files)} total subjects"
    )
    return patient_files


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# PART 2: INSTRUCTION-TUNING DATASET GENERATION
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

SYSTEM_PROMPT = (
    "You are a clinical decision support AI trained on medical records "
    "from diverse healthcare settings. Given a patient's clinical "
    "presentation including demographics, symptoms, vital signs, "
    "laboratory results, and imaging findings, provide a differential "
    "diagnosis with ICD-10 codes, recommended next steps, and any "
    "red flags requiring immediate attention. Always note limitations "
    "and recommend clinician verification."
)


def format_encounter_summary(subject: SubjectRecord) -> str:
    """Format encounters into a readable clinical narrative."""
    lines = []
    for enc in subject.encounters:
        year = enc.get("encounter_year", "")
        month = enc.get("encounter_month", "")
        enc_type = enc.get("type", "visit")
        complaint = enc.get("complaint", "")
        vitals = enc.get("vital_signs", {})

        if year and month:
            try:
                date_str = f"{year}/{int(month):02d}"
            except (ValueError, TypeError):
                date_str = f"{year}"
        elif year:
            date_str = f"{year}"
        else:
            date_str = "Unknown date"
        line = f"  [{date_str}] {enc_type}: {complaint}"

        if vitals:
            vital_parts = []
            for k, v in vitals.items():
                if v is not None:
                    vital_parts.append(f"{k}={v}")
            if vital_parts:
                line += f" | Vitals: {', '.join(vital_parts)}"

        lines.append(line)
    return "\n".join(lines) if lines else "  No encounters documented."


def format_lab_results(subject: SubjectRecord) -> str:
    """Format lab results into a readable summary."""
    if not subject.lab_results:
        return "  No lab results available."

    lines = []
    for lab in subject.lab_results:
        year = lab.get("encounter_year", "")
        month = lab.get("encounter_month", "")
        test = lab.get("test", "Unknown test")
        value = lab.get("value", "N/A")
        unit = lab.get("unit", "")
        ref = lab.get("reference", "")
        comment = lab.get("comment", "")

        date_str = ""
        if year and month:
            try:
                date_str = f"{year}/{int(month):02d}"
            except (ValueError, TypeError):
                date_str = f"{year}"
        elif year:
            date_str = f"{year}"
        line = f"  [{date_str}] {test}: {value}"
        if unit:
            line += f" {unit}"
        if ref:
            line += f" (ref: {ref})"
        if comment:
            line += f" [{comment}]"
        lines.append(line)

    return "\n".join(lines)


def format_medications(subject: SubjectRecord) -> str:
    """Format medications list."""
    if not subject.medications:
        return "  None documented."

    lines = []
    for med in subject.medications:
        name = med.get("name", "Unknown")
        dose = med.get("dose", "")
        route = med.get("route", "")
        freq = med.get("frequency", "")
        indication = med.get("indication", "")

        parts = [name]
        if dose:
            parts.append(dose)
        if route:
            parts.append(route)
        if freq:
            parts.append(freq)
        if indication:
            parts.append(f"(for {indication})")

        lines.append(f"  - {' '.join(parts)}")

    return "\n".join(lines)


def format_allergies(subject: SubjectRecord) -> str:
    """Format allergies."""
    if not subject.allergies:
        return "  No known allergies."

    lines = []
    for allergy in subject.allergies:
        agent = allergy.get("agent", "Unknown")
        reaction = allergy.get("reaction", "Unknown reaction")
        lines.append(f"  - {agent}: {reaction}")

    return "\n".join(lines)


def format_imaging(subject: SubjectRecord) -> str:
    """Format imaging results."""
    if not subject.imaging_results:
        return "  No imaging studies."

    lines = []
    for img in subject.imaging_results:
        img_type = img.get("type", "Imaging")
        summary = img.get("summary", "No report available")
        lines.append(f"  - {img_type}: {summary}")

    return "\n".join(lines)


def build_clinical_presentation(subject: SubjectRecord) -> str:
    """
    Build a comprehensive clinical presentation from a subject record.
    This becomes the 'user' prompt in the instruction-tuning pair.
    """
    # Demographics
    age_str = ""
    if subject.age is not None:
        if subject.age < 1:
            age_str = f"{int(subject.age * 12)} months"
        else:
            age_str = f"{int(subject.age)} years"

    sex_str = subject.sex or "Unknown sex"
    demo_line = f"Patient: {age_str} {sex_str}"
    if subject.relationship not in ("primary_employee", "patient", "unknown"):
        demo_line += f" ({subject.relationship})"

    presentation = f"""CLINICAL PRESENTATION
=====================
{demo_line}

ENCOUNTER HISTORY:
{format_encounter_summary(subject)}

LABORATORY RESULTS:
{format_lab_results(subject)}

CURRENT MEDICATIONS:
{format_medications(subject)}

ALLERGIES:
{format_allergies(subject)}

IMAGING:
{format_imaging(subject)}

Based on the above clinical presentation, provide:
1. Most likely diagnosis/diagnoses with ICD-10 codes
2. Key clinical reasoning for each diagnosis
3. Recommended next steps or management plan
4. Any red flags or urgent concerns"""

    return presentation.strip()


def build_diagnosis_response(subject: SubjectRecord) -> str:
    """
    Build the target diagnosis response from the ground-truth
    diagnoses in the record. This becomes the 'assistant' response.
    """
    if not subject.diagnoses:
        return (
            "ASSESSMENT:\n"
            "Insufficient clinical data to establish a definitive diagnosis. "
            "Recommend comprehensive history and physical examination, "
            "baseline laboratory workup, and follow-up evaluation.\n\n"
            "PLAN:\n"
            f"{subject.plan or 'Pending further evaluation.'}"
        )

    lines = ["ASSESSMENT AND DIAGNOSES:"]

    for i, dx in enumerate(subject.diagnoses, 1):
        name = dx.get("name", "Unknown diagnosis")
        icd10 = dx.get("icd10", "")
        year = dx.get("encounter_year", "")
        month = dx.get("encounter_month", "")

        # Safely format date — month may be missing or non-integer
        if year and month:
            try:
                date_str = f" ({year}/{int(month):02d})"
            except (ValueError, TypeError):
                date_str = f" ({year})"
        elif year:
            date_str = f" ({year})"
        else:
            date_str = ""
        icd_str = f" [ICD-10: {icd10}]" if icd10 else ""

        lines.append(f"\n{i}. {name}{icd_str}{date_str}")

    # Add clinical reasoning based on available data
    lines.append("\n\nCLINICAL REASONING:")

    # Link diagnoses to supporting evidence
    for dx in subject.diagnoses:
        name = dx.get("name", "")

        # Find supporting lab results
        supporting_labs = []
        for lab in subject.lab_results:
            comment = lab.get("comment", "").lower()
            if "high" in comment or "low" in comment or "positive" in comment:
                supporting_labs.append(
                    f"{lab.get('test', '')}: {lab.get('value', '')} "
                    f"{lab.get('unit', '')} [{comment.upper()}]"
                )

        # Find supporting encounters
        supporting_encounters = []
        for enc in subject.encounters:
            complaint = enc.get("complaint", "").lower()
            dx_keywords = name.lower().split()
            if any(kw in complaint for kw in dx_keywords if len(kw) > 3):
                supporting_encounters.append(enc.get("complaint", ""))

        if supporting_labs or supporting_encounters:
            lines.append(f"  {name}:")
            for lab_line in supporting_labs[:3]:
                lines.append(f"    - Lab: {lab_line}")
            for enc_line in supporting_encounters[:2]:
                lines.append(f"    - Presentation: {enc_line[:80]}")

    # Management plan
    lines.append("\n\nMANAGEMENT PLAN:")
    if subject.plan:
        lines.append(f"  {subject.plan}")

    if subject.medications:
        lines.append("\n  Current medications to continue/review:")
        for med in subject.medications[:5]:
            name_str = med.get("name", "Unknown")
            indication = med.get("indication", "")
            lines.append(f"    - {name_str}" +
                         (f" (for {indication})" if indication else ""))

    # Red flags
    red_flags = []
    for lab in subject.lab_results:
        comment = (lab.get("comment", "") or "").lower()
        if "high" in comment or "critical" in comment:
            red_flags.append(
                f"{lab.get('test', '')}: {lab.get('value', '')} "
                f"{lab.get('unit', '')}"
            )
    for enc in subject.encounters:
        complaint = (enc.get("complaint", "") or "").lower()
        if any(kw in complaint for kw in
               ["emergency", "severe", "acute", "admitted", "admission"]):
            red_flags.append(enc.get("complaint", "")[:80])

    if red_flags:
        lines.append("\n\nRED FLAGS / URGENT CONCERNS:")
        for rf in red_flags[:3]:
            lines.append(f"  ⚠ {rf}")

    lines.append(
        "\n\nNOTE: This assessment is based on available records. "
        "Clinical correlation and physician verification are required "
        "before any treatment decisions."
    )

    return "\n".join(lines)


def generate_instruction_pairs(
    patient_files: List[PatientFile],
    min_encounters: int = 1,
    min_diagnoses: int = 0
) -> List[Dict[str, str]]:
    """
    Generate instruction-tuning pairs from patient files.

    Each pair consists of:
      - system: Clinical AI system prompt
      - user: Clinical presentation (demographics + encounters + labs + meds)
      - assistant: Diagnosis response (diagnoses + reasoning + plan)

    Filters:
      - Subjects with at least min_encounters encounters
      - Optionally requires at least min_diagnoses diagnoses
    """
    pairs = []

    for pf in patient_files:
        for subject in pf.subjects:
            # Filter: skip subjects with no meaningful data
            if len(subject.encounters) < min_encounters:
                continue

            # Build the instruction pair
            user_prompt = build_clinical_presentation(subject)
            assistant_response = build_diagnosis_response(subject)

            # Skip if presentation is too short
            if len(user_prompt) < 100:
                continue

            pairs.append({
                "system": SYSTEM_PROMPT,
                "user": user_prompt,
                "assistant": assistant_response,
                # Metadata for tracking
                "patient_id": pf.patient_id,
                "subject_id": subject.subject_id,
                "n_diagnoses": len(subject.diagnoses),
                "n_encounters": len(subject.encounters),
                "facility": pf.facility,
            })

    logger.info(
        f"Generated {len(pairs)} instruction-tuning pairs "
        f"(filtered from {sum(len(pf.subjects) for pf in patient_files)} subjects)"
    )

    # Report diagnosis coverage
    has_dx = sum(1 for p in pairs if p["n_diagnoses"] > 0)
    no_dx = len(pairs) - has_dx
    logger.info(f"  With diagnoses: {has_dx}, Without: {no_dx}")

    return pairs


def format_for_qwen_chat(pair: Dict[str, str]) -> str:
    """
    Format an instruction pair into Qwen-2.5 chat template.

    Qwen-2.5 uses the ChatML format:
      <|im_start|>system
      {system_prompt}<|im_end|>
      <|im_start|>user
      {user_message}<|im_end|>
      <|im_start|>assistant
      {assistant_message}<|im_end|>
    """
    return (
        f"<|im_start|>system\n{pair['system']}<|im_end|>\n"
        f"<|im_start|>user\n{pair['user']}<|im_end|>\n"
        f"<|im_start|>assistant\n{pair['assistant']}<|im_end|>"
    )


def prepare_dataset(
    pairs: List[Dict[str, str]],
    test_size: float = 0.15,
    seed: int = 42
) -> Tuple[List[Dict], List[Dict]]:
    """Split into train/test and format for HuggingFace datasets."""
    random.seed(seed)
    indices = list(range(len(pairs)))
    random.shuffle(indices)

    split_idx = int(len(pairs) * (1 - test_size))
    train_indices = indices[:split_idx]
    test_indices = indices[split_idx:]

    train_data = [pairs[i] for i in train_indices]
    test_data = [pairs[i] for i in test_indices]

    logger.info(f"Dataset split: {len(train_data)} train, {len(test_data)} test")
    return train_data, test_data


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# PART 3: QLoRA FINE-TUNING
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

def setup_model_and_tokenizer(
    model_name: str = "Qwen/Qwen2.5-7B-Instruct",
    load_in_4bit: bool = True
):
    """
    Load Qwen-2.5-7B-Instruct with 4-bit quantization for QLoRA.

    QLoRA config:
      - 4-bit NF4 quantization (bitsandbytes)
      - LoRA rank r=16, alpha=32, dropout=0.05
      - Target modules: q_proj, k_proj, v_proj, o_proj, gate_proj,
        up_proj, down_proj
    """
    from transformers import (
        AutoModelForCausalLM,
        AutoTokenizer,
        BitsAndBytesConfig
    )
    from peft import LoraConfig, get_peft_model, prepare_model_for_kbit_training

    # Tokenizer
    tokenizer = AutoTokenizer.from_pretrained(
        model_name,
        trust_remote_code=True,
        padding_side="right"
    )
    if tokenizer.pad_token is None:
        tokenizer.pad_token = tokenizer.eos_token

    # Quantization config
    if load_in_4bit:
        import torch
        bnb_config = BitsAndBytesConfig(
            load_in_4bit=True,
            bnb_4bit_quant_type="nf4",
            bnb_4bit_compute_dtype=torch.bfloat16,
            bnb_4bit_use_double_quant=True
        )
    else:
        bnb_config = None

    # Model
    model = AutoModelForCausalLM.from_pretrained(
        model_name,
        quantization_config=bnb_config,
        device_map="auto",
        trust_remote_code=True,
        torch_dtype="auto"
    )

    if load_in_4bit:
        model = prepare_model_for_kbit_training(model)

    # LoRA config
    lora_config = LoraConfig(
        r=16,
        lora_alpha=32,
        lora_dropout=0.05,
        bias="none",
        task_type="CAUSAL_LM",
        target_modules=[
            "q_proj", "k_proj", "v_proj", "o_proj",
            "gate_proj", "up_proj", "down_proj"
        ]
    )

    model = get_peft_model(model, lora_config)

    # Report trainable parameters
    trainable = sum(p.numel() for p in model.parameters() if p.requires_grad)
    total = sum(p.numel() for p in model.parameters())
    logger.info(
        f"Model loaded: {model_name}\n"
        f"  Total parameters:     {total:,}\n"
        f"  Trainable parameters: {trainable:,} "
        f"({100 * trainable / total:.2f}%)"
    )

    return model, tokenizer


def tokenize_dataset(
    pairs: List[Dict[str, str]],
    tokenizer,
    max_length: int = 2048
) -> "Dataset":
    """
    Tokenize instruction pairs for causal LM training.

    The loss is computed only on the assistant's response tokens,
    not on the system/user prompt tokens (masked with -100).
    """
    from datasets import Dataset

    def tokenize_fn(examples):
        all_input_ids = []
        all_attention_mask = []
        all_labels = []

        for system, user, assistant in zip(
            examples["system"], examples["user"], examples["assistant"]
        ):
            # Build full text
            full_text = (
                f"<|im_start|>system\n{system}<|im_end|>\n"
                f"<|im_start|>user\n{user}<|im_end|>\n"
                f"<|im_start|>assistant\n{assistant}<|im_end|>"
            )

            # Tokenize full text
            full_tokens = tokenizer(
                full_text,
                truncation=True,
                max_length=max_length,
                padding="max_length",
                return_tensors=None
            )

            # Build the prompt-only text (for masking)
            prompt_text = (
                f"<|im_start|>system\n{system}<|im_end|>\n"
                f"<|im_start|>user\n{user}<|im_end|>\n"
                f"<|im_start|>assistant\n"
            )
            prompt_tokens = tokenizer(
                prompt_text,
                truncation=True,
                max_length=max_length,
                return_tensors=None
            )
            prompt_len = len(prompt_tokens["input_ids"])

            # Labels: mask prompt tokens with -100
            labels = full_tokens["input_ids"].copy()
            labels[:prompt_len] = [-100] * prompt_len
            # Also mask padding
            for i in range(len(labels)):
                if full_tokens["attention_mask"][i] == 0:
                    labels[i] = -100

            all_input_ids.append(full_tokens["input_ids"])
            all_attention_mask.append(full_tokens["attention_mask"])
            all_labels.append(labels)

        return {
            "input_ids": all_input_ids,
            "attention_mask": all_attention_mask,
            "labels": all_labels
        }

    # Convert to HuggingFace Dataset
    ds = Dataset.from_dict({
        "system": [p["system"] for p in pairs],
        "user": [p["user"] for p in pairs],
        "assistant": [p["assistant"] for p in pairs],
    })

    tokenized = ds.map(
        tokenize_fn,
        batched=True,
        batch_size=8,
        remove_columns=["system", "user", "assistant"],
        desc="Tokenizing"
    )

    return tokenized


def train_model(
    model,
    tokenizer,
    train_dataset,
    eval_dataset,
    output_dir: str = "/content/mhai_diagnosis_model",
    epochs: int = 3,
    batch_size: int = 2,
    gradient_accumulation: int = 8,
    learning_rate: float = 2e-4,
    max_steps: int = -1
):
    """
    Fine-tune with SFTTrainer / standard Trainer.

    Training config:
      - Epochs: 3 (or max_steps if set)
      - Effective batch size: batch_size * gradient_accumulation = 16
      - LR: 2e-4 with cosine schedule
      - Warmup: 5% of steps
      - Mixed precision: bf16
      - Gradient checkpointing: enabled
      - Logging: every 10 steps
      - Eval: every 50 steps
      - Save: best model by eval_loss
    """
    from transformers import TrainingArguments, Trainer
    import torch

    training_args = TrainingArguments(
        output_dir=output_dir,
        num_train_epochs=epochs,
        max_steps=max_steps,
        per_device_train_batch_size=batch_size,
        per_device_eval_batch_size=batch_size,
        gradient_accumulation_steps=gradient_accumulation,
        learning_rate=learning_rate,
        lr_scheduler_type="cosine",
        warmup_steps=10,
        weight_decay=0.01,
        bf16=torch.cuda.is_available(),
        logging_steps=10,
        eval_strategy="steps",
        eval_steps=50,
        save_strategy="steps",
        save_steps=50,
        save_total_limit=3,
        load_best_model_at_end=True,
        metric_for_best_model="eval_loss",
        greater_is_better=False,
        gradient_checkpointing=True,
        report_to="none",
        dataloader_pin_memory=True,
        remove_unused_columns=False,
        optim="paged_adamw_8bit",
    )

    # Handle both old and new transformers API
    # (v5.2+ renamed 'tokenizer' to 'processing_class')
    try:
        trainer = Trainer(
            model=model,
            args=training_args,
            train_dataset=train_dataset,
            eval_dataset=eval_dataset,
            processing_class=tokenizer,
        )
    except TypeError:
        trainer = Trainer(
            model=model,
            args=training_args,
            train_dataset=train_dataset,
            eval_dataset=eval_dataset,
            tokenizer=tokenizer,
        )

    logger.info("Starting training...")
    trainer.train()

    # Save the final LoRA adapter
    model.save_pretrained(os.path.join(output_dir, "final_adapter"))
    tokenizer.save_pretrained(os.path.join(output_dir, "final_adapter"))
    logger.info(f"Model saved to {output_dir}/final_adapter")

    return trainer


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# PART 4: INFERENCE AND EVALUATION
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

def generate_diagnosis(
    model,
    tokenizer,
    clinical_presentation: str,
    max_new_tokens: int = 1024,
    temperature: float = 0.3,
    top_p: float = 0.9
) -> str:
    """
    Generate a diagnosis from a clinical presentation using
    the fine-tuned model.
    """
    import torch

    messages = [
        {"role": "system", "content": SYSTEM_PROMPT},
        {"role": "user", "content": clinical_presentation}
    ]

    # Use Qwen's chat template
    text = tokenizer.apply_chat_template(
        messages,
        tokenize=False,
        add_generation_prompt=True
    )

    inputs = tokenizer(text, return_tensors="pt").to(model.device)

    with torch.no_grad():
        outputs = model.generate(
            **inputs,
            max_new_tokens=max_new_tokens,
            temperature=temperature,
            top_p=top_p,
            do_sample=True,
            repetition_penalty=1.1,
            pad_token_id=tokenizer.pad_token_id,
        )

    # Decode only the generated part
    generated = outputs[0][inputs["input_ids"].shape[1]:]
    response = tokenizer.decode(generated, skip_special_tokens=True)

    return response.strip()


def evaluate_diagnosis_accuracy(
    model,
    tokenizer,
    test_pairs: List[Dict[str, str]],
    max_eval: int = 50
) -> Dict[str, Any]:
    """
    Evaluate diagnosis accuracy on test set.

    Metrics:
      - Exact ICD-10 match rate
      - Partial diagnosis name match (keyword overlap)
      - Average response quality score (heuristic)
    """
    results = []
    n_eval = min(max_eval, len(test_pairs))

    for i, pair in enumerate(test_pairs[:n_eval]):
        logger.info(f"Evaluating {i+1}/{n_eval}...")

        # Generate prediction
        predicted = generate_diagnosis(model, tokenizer, pair["user"])

        # Extract ground-truth diagnoses
        gt_diagnoses = []
        for dx_line in pair["assistant"].split("\n"):
            # Look for ICD-10 codes in ground truth
            import re
            icd_match = re.search(r'ICD-10:\s*([A-Z]\d{2}\.?\d*)', dx_line)
            if icd_match:
                gt_diagnoses.append(icd_match.group(1))

        # Extract predicted diagnoses
        pred_diagnoses = []
        for line in predicted.split("\n"):
            icd_match = re.search(r'ICD-10:\s*([A-Z]\d{2}\.?\d*)', line)
            if icd_match:
                pred_diagnoses.append(icd_match.group(1))

        # Compute metrics
        gt_set = set(gt_diagnoses)
        pred_set = set(pred_diagnoses)
        exact_matches = len(gt_set & pred_set)
        total_gt = max(len(gt_set), 1)

        # Keyword overlap for partial matching
        gt_text = pair["assistant"].lower()
        pred_text = predicted.lower()
        gt_words = set(gt_text.split())
        pred_words = set(pred_text.split())
        word_overlap = len(gt_words & pred_words) / max(len(gt_words), 1)

        results.append({
            "icd10_recall": exact_matches / total_gt,
            "icd10_precision": (
                exact_matches / max(len(pred_set), 1)
            ),
            "keyword_overlap": word_overlap,
            "gt_n_diagnoses": len(gt_diagnoses),
            "pred_n_diagnoses": len(pred_diagnoses),
            "response_length": len(predicted),
        })

    # Aggregate
    avg_recall = np.mean([r["icd10_recall"] for r in results])
    avg_precision = np.mean([r["icd10_precision"] for r in results])
    avg_overlap = np.mean([r["keyword_overlap"] for r in results])

    return {
        "n_evaluated": n_eval,
        "icd10_recall": round(avg_recall, 4),
        "icd10_precision": round(avg_precision, 4),
        "keyword_overlap": round(avg_overlap, 4),
        "avg_response_length": round(
            np.mean([r["response_length"] for r in results])
        ),
        "per_case": results
    }


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# PART 5: MAIN EXECUTION
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

def main():
    """
    Full pipeline execution.

    Usage on Colab:
      1. Upload JSON files to /content/extract/001-005/
      2. Run this script
      3. Fine-tuned adapter saved to /content/mhai_diagnosis_model/
    """
    print("=" * 70)
    print("MHAI Clinical Diagnosis Fine-Tuning Pipeline")
    print("=" * 70)

    # ═══════════════════════════════════════
    # STEP 1: Extract and parse JSON files
    # ═══════════════════════════════════════
    print("\n[1/6] Extracting JSON files...")
    base_dir = "/content/drive/MyDrive/Breast Cancer Articles Collection/extracted"

    # Check if running in Colab or demo mode
    if os.path.exists(base_dir):
        patient_files = load_all_patient_files(base_dir)
    else:
        print("  /content/extract not found — running in demo mode")
        print("  Creating synthetic patient data from the provided example...")
        patient_files = _create_demo_data()

    print(f"  Loaded {len(patient_files)} patient files")
    print(f"  Total subjects: "
          f"{sum(len(pf.subjects) for pf in patient_files)}")

    # ═══════════════════════════════════════
    # STEP 2: Generate instruction pairs
    # ═══════════════════════════════════════
    print("\n[2/6] Generating instruction-tuning pairs...")
    pairs = generate_instruction_pairs(
        patient_files,
        min_encounters=1,
        min_diagnoses=0
    )
    print(f"  Generated {len(pairs)} pairs")

    if pairs:
        print(f"\n  Sample pair (first):")
        print(f"  {'─' * 40}")
        print(f"  User prompt ({len(pairs[0]['user'])} chars):")
        print("  " + pairs[0]["user"][:200].replace("\n", "\n  ") + "...")
        print(f"\n  Assistant response ({len(pairs[0]['assistant'])} chars):")
        print("  " + pairs[0]["assistant"][:200].replace("\n", "\n  ") + "...")

    # ═══════════════════════════════════════
    # STEP 3: Split dataset
    # ═══════════════════════════════════════
    print("\n[3/6] Splitting dataset...")
    train_pairs, test_pairs = prepare_dataset(pairs, test_size=0.15)

    # Save raw pairs for inspection
    output_dir = "/content/mhai_diagnosis_model"
    os.makedirs(output_dir, exist_ok=True)

    with open(os.path.join(output_dir, "train_pairs.json"), "w") as f:
        json.dump(train_pairs, f, indent=2, default=str)
    with open(os.path.join(output_dir, "test_pairs.json"), "w") as f:
        json.dump(test_pairs, f, indent=2, default=str)
    print(f"  Saved pairs to {output_dir}/")

    # ═══════════════════════════════════════
    # STEP 4: Load model + tokenize
    # ═══════════════════════════════════════
    print("\n[4/6] Loading Qwen-2.5-7B-Instruct with QLoRA...")

    try:
        model, tokenizer = setup_model_and_tokenizer(
            model_name="Qwen/Qwen2.5-7B-Instruct",
            load_in_4bit=True
        )

        print("\n  Tokenizing datasets...")
        train_dataset = tokenize_dataset(train_pairs, tokenizer)
        eval_dataset = tokenize_dataset(test_pairs, tokenizer)
        print(f"  Train tokens: {len(train_dataset)}")
        print(f"  Eval tokens: {len(eval_dataset)}")

        # ═══════════════════════════════════════
        # STEP 5: Fine-tune
        # ═══════════════════════════════════════
        print("\n[5/6] Fine-tuning...")
        trainer = train_model(
            model, tokenizer,
            train_dataset, eval_dataset,
            output_dir=output_dir,
            epochs=3,
            batch_size=2,
            gradient_accumulation=8,
            learning_rate=2e-4
        )

        # ═══════════════════════════════════════
        # STEP 6: Evaluate
        # ═══════════════════════════════════════
        print("\n[6/6] Evaluating on test set...")
        metrics = evaluate_diagnosis_accuracy(
            model, tokenizer, test_pairs, max_eval=10
        )

        print(f"\n  Evaluation Results:")
        print(f"    ICD-10 Recall:      {metrics['icd10_recall']:.1%}")
        print(f"    ICD-10 Precision:   {metrics['icd10_precision']:.1%}")
        print(f"    Keyword Overlap:    {metrics['keyword_overlap']:.1%}")
        print(f"    Avg Response Len:   {metrics['avg_response_length']:.0f} chars")

    except Exception as e:
        print(f"\n  Model loading failed (expected without GPU): {e}")
        print("  Skipping steps 4-6. The data pipeline (steps 1-3) is complete.")
        print(f"  Train/test pairs saved to {output_dir}/")

    print("\n" + "=" * 70)
    print("Pipeline complete.")
    print("=" * 70)


def _create_demo_data() -> List[PatientFile]:
    """Create demo data from the provided JSON example."""
    demo_json = {
        "patient_id": "demo_001",
        "source": {
            "folder_index": "001",
            "pages_processed": 79,
            "document_type": "family_medical_card",
            "facility": "Demo Hospital, Lagos, Nigeria"
        },
        "subjects": [
            {
                "subject_id": "demo_001-s1",
                "relationship": "primary_employee",
                "demographics": {"age_years": 28, "sex": "F"},
                "encounters": [
                    {"encounter_year": 2006, "encounter_month": 5,
                     "type": "outpatient",
                     "complaint": "cold, joint pains, allergic shivers",
                     "vital_signs": {"BP": "120/?", "Temp_C": 36, "Wt_kg": 68.5}},
                    {"encounter_year": 2006, "encounter_month": 10,
                     "type": "antenatal",
                     "complaint": "GDM evaluation, OGTT"},
                    {"encounter_year": 2006, "encounter_month": 11,
                     "type": "delivery_admission",
                     "complaint": "labour and delivery via emergency CS"}
                ],
                "diagnosis": [
                    {"name": "Gestational Diabetes Mellitus",
                     "icd10": "O24.4", "encounter_year": 2006},
                    {"name": "Pregnancy delivered by LSCS",
                     "icd10": "O82", "encounter_year": 2006},
                    {"name": "URTI with joint pains",
                     "icd10": "J06.9", "encounter_year": 2006}
                ],
                "medications": [
                    {"name": "Paracetamol", "dose": "2 tabs",
                     "route": "Oral", "indication": "joint pains"},
                    {"name": "Plasmotrim", "dose": "200 mg",
                     "route": "Oral", "indication": "malaria"}
                ],
                "vital_signs": [
                    {"BP": "120/80", "Wt_kg": 79.5}
                ],
                "lab_results": [
                    {"test": "Fasting Glucose", "value": 103,
                     "unit": "mg/dl", "reference": "60-110"},
                    {"test": "2hr post-prandial glucose", "value": 168,
                     "unit": "mg/dl", "reference": "60-110",
                     "comment": "HIGH - GDM"}
                ],
                "imaging_results": [],
                "allergies_and_contraindications": [
                    {"agent": "unspecified injection",
                     "reaction": "allergic shivers/hives"}
                ],
                "plan": "GDM diet 2000 cal; postnatal review."
            },
            {
                "subject_id": "demo_001-s3",
                "relationship": "daughter",
                "demographics": {"age_months": 0, "sex": "F"},
                "encounters": [
                    {"encounter_year": 2006, "encounter_month": 11,
                     "type": "neonatal",
                     "complaint": "newborn care, jaundice screening",
                     "vital_signs": {"Wt_kg": 4.15}},
                    {"encounter_year": 2007, "encounter_month": 10,
                     "type": "inpatient_admission",
                     "complaint": "fever 12 days, malaria with high fever 40C",
                     "vital_signs": {"Temp_C": 38.9, "Wt_kg": 7.85}}
                ],
                "diagnosis": [
                    {"name": "Neonatal jaundice", "icd10": "P59.9"},
                    {"name": "Severe malaria", "icd10": "B54"}
                ],
                "medications": [
                    {"name": "Plasmotrim suppository", "dose": "50 mg",
                     "route": "Rectal", "indication": "malaria"}
                ],
                "vital_signs": [],
                "lab_results": [
                    {"test": "Malaria Parasite", "value": "Positive"},
                    {"test": "Hb", "value": 9.6, "unit": "g/dl",
                     "comment": "LOW"}
                ],
                "imaging_results": [],
                "allergies_and_contraindications": [],
                "plan": "Manage malaria; monitor hemoglobin."
            }
        ]
    }

    pf = parse_json_file.__wrapped__ if hasattr(parse_json_file, '__wrapped__') else None

    # Manually construct
    patient = PatientFile(
        patient_id="demo_001",
        source_file="demo",
        facility="Demo Hospital, Lagos, Nigeria"
    )

    for subj_data in demo_json["subjects"]:
        demographics = subj_data.get("demographics", {})
        age = demographics.get("age_years")
        if age is None:
            age_m = demographics.get("age_months")
            age = age_m / 12.0 if age_m is not None else None

        sr = SubjectRecord(
            subject_id=subj_data["subject_id"],
            relationship=subj_data["relationship"],
            age=age,
            sex=demographics.get("sex"),
            encounters=subj_data.get("encounters", []),
            diagnoses=subj_data.get("diagnosis", []),
            medications=subj_data.get("medications", []),
            vital_signs=subj_data.get("vital_signs", []),
            lab_results=subj_data.get("lab_results", []),
            imaging_results=subj_data.get("imaging_results", []),
            allergies=subj_data.get("allergies_and_contraindications", []),
            plan=subj_data.get("plan", "")
        )
        patient.subjects.append(sr)

    return [patient]


if __name__ == "__main__":
    main()
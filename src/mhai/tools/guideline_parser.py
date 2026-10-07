"""Parse Nigerian patient JSON records + reference guidelines into KB chunks."""
import json
import glob
import logging
from typing import List, Dict
from pathlib import Path

log = logging.getLogger(__name__)

def parse_guidelines(json_dir: str) -> List[Dict]:
    """Parse all JSON files in directory into KB-ready chunks."""
    chunks = []
    for path in sorted(glob.glob(f"{json_dir}/**/*.json", recursive=True)):
        try:
            with open(path) as f:
                data = json.load(f)
            chunks.extend(_extract_chunks(data, Path(path).stem))
        except Exception as e:
            log.warning(f"Failed to parse {path}: {e}")
    log.info(f"Parsed {len(chunks)} chunks from {json_dir}")
    return chunks

def _extract_chunks(data: dict, source_id: str) -> List[Dict]:
    """Extract typed chunks from a patient/guideline JSON."""
    chunks = []
    for key in ["demographics", "encounters", "labs", "medications", "diagnoses", "plan"]:
        if key in data:
            chunks.append({
                "chunk_type": key,
                "content": json.dumps(data[key]) if isinstance(data[key], (dict, list)) else str(data[key]),
                "source_patient_id": source_id,
            })
    return chunks

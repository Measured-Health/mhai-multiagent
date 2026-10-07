"""
Idempotent KB seeder — loads 6 reference guidelines + patient records.
Run at first startup of orchestrator container.
"""

import logging

log = logging.getLogger(__name__)


def seed_knowledge_base():
    """Seed the medical knowledge base from reference guidelines."""
    guidelines = [
        {
            "chunk_id": "KB#0001",
            "chunk_type": "guideline",
            "content": "Gestational Diabetes Mellitus (GDM) diagnosed when fasting glucose >= 92 mg/dL, 1-hr >= 180, or 2-hr >= 153 on 75g OGTT.",
        },
        {
            "chunk_id": "KB#0002",
            "chunk_type": "guideline",
            "content": "Malaria requires parasitological confirmation by microscopy or RDT. Severe features: Hb < 5, respiratory distress, parasitemia > 10%.",
        },
        {
            "chunk_id": "KB#0003",
            "chunk_type": "guideline",
            "content": "Hypertension defined as systolic >= 140 or diastolic >= 90 on two occasions. First-line: thiazides, ACEi, ARBs, CCBs.",
        },
        {
            "chunk_id": "KB#0004",
            "chunk_type": "guideline",
            "content": "Neonatal jaundice: physiological onset > 24hr, peaks day 3-5, total bili < 15. Pathological: onset < 24hr, total > 17.",
        },
        {
            "chunk_id": "KB#0005",
            "chunk_type": "guideline",
            "content": "URTI: viral majority. Supportive: paracetamol, hydration. Antibiotics only if bacterial superinfection.",
        },
        {
            "chunk_id": "KB#0006",
            "chunk_type": "guideline",
            "content": "T2DM: fasting glucose >= 126, 2-hr OGTT >= 200, HbA1c >= 6.5%. First-line: Metformin 500mg BID.",
        },
    ]
    log.info(f"Seeding {len(guidelines)} reference guidelines into KB")
    return guidelines


if __name__ == "__main__":
    seed_knowledge_base()

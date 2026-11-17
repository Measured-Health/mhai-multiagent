"""Measure the PII masker on the synthetic Nigerian PII set.

    uv run python scripts/masker_eval.py [--dev ../../testdata/answers/dev/items.jsonl] [--json]

Prints recall per class (masker and egress), names by group, casing and
seen/held-out, and the false-refusal and over-masking rates on the clinical
question and guideline set. Synthetic data only; nothing leaves the machine.
See docs/privacy/masker-evaluation.md and intelligence/security/masker_eval.py.
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from unittest import mock

from intelligence.security.hipaa import PIIMasker
from intelligence.security.masker_eval import SEED, evaluate

DEV = Path(__file__).resolve().parents[3] / "testdata" / "answers" / "dev" / "items.jsonl"


def main(argv: list[str] | None = None) -> int:
    p = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    p.add_argument("--dev", type=Path, default=DEV, help="the clinical answer dev set (jsonl)")
    p.add_argument("--seed", type=int, default=SEED)
    p.add_argument("--json", action="store_true", help="print the full result as JSON")
    p.add_argument(
        "--baseline",
        action="store_true",
        help="measure PIIMasker without the Nigerian rules (the masker before this upgrade)",
    )
    args = p.parse_args(argv)
    dev = args.dev if args.dev.exists() else None
    if args.baseline:
        with mock.patch.object(PIIMasker, "_extra_spans", lambda self, text: []):
            result = evaluate(dev, args.seed)
    else:
        result = evaluate(dev, args.seed)
    if args.json:
        print(json.dumps(result, indent=2, ensure_ascii=False))
        return 0
    print("class        n   masker   egress")
    for cls, row in result["recall"].items():
        e = result["egress_recall"][cls]
        print(f"{cls:<10} {row['n']:>5}  {row['recall']:.4f}   {e['recall']:.4f}")
    print("\nnames by group / casing / gazetteer")
    for tag, row in result["names_by"].items():
        print(f"{tag:<16} {row['n']:>5}  {row['recall']:.4f}")
    print(
        f"\nclean questions {result['clean_questions']}: false refusals "
        f"{result['false_refusals']} ({result['false_refusal_rate']:.4f}) "
        f"{result['false_refusals_by_rule']}; over-masking {result['over_masking_rate']:.4f}"
    )
    print(
        f"named questions refused: {result['named_question_refusal_rate']:.4f} "
        f"{result['named_refusals_by_rule']}; masker output re-detected: "
        f"{result['remask_detections']}"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())

#!/usr/bin/env python3
"""Before/after evaluation of the HGA's NLI grounding, with the real models.

Asks every item of the committed ``clinical_answer`` dev split (76 items, 36 must escalate;
``testdata/answers/dev``) through the worker's own local path (``local_parts``'s builder:
the real dense retriever, the real NLI model, ``CitationVerificationPipeline``,
``NLIBackedHGA``, then ``_verify_with`` with the escalation guard), with a scripted
generator instead of an LLM, so the answers are fixed and only the verifier changes
between two runs. No answer model's weights are needed.

Modes (the reply the scripted generator gives every item, must-escalate items included:
a model that answers when it must not is the case the guard has to catch):

* ``verbatim``: the first sentence of the item's first chunk, copied word for word, cited.
* ``wrong_dose``: the first sentence of the first chunk that states a number, with every
  number multiplied by ten (must never PASS); items whose chunk states none are skipped.
* ``long_verbatim``: the item's chunks are joined into one long chunk with the other
  guidelines of the set (as a long reference chunk is in the live library); the claim is
  the item's first sentence, copied word for word, citing the long chunk.
* ``long_wrong_dose``: the long chunk, the numbered sentence with its numbers times ten.

Writes ``report.json`` (decisions, support-gate and guard codes, NLI labels, latencies; ids
only, never answer or chunk text beyond the synthetic set) to ``--out``.

    uv run python scripts/hga_grounding_eval.py --synthetic --out results/hga-eval
"""

from __future__ import annotations

import argparse
import json
import os
import re
import statistics
import sys
import time
from collections import Counter
from pathlib import Path
from typing import Any

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))

from intelligence.boundary.models import (  # noqa: E402
    OrchestratorModelClient,
    _verify_with,
    local_parts,
)
from intelligence.doceval.answers import schema  # noqa: E402
from intelligence.knowledge_base.kb_builder import REFERENCE_SOURCE, KBChunk  # noqa: E402

DEV = ROOT.parents[1] / "testdata" / "answers" / "dev"
MODES = ("verbatim", "wrong_dose", "long_verbatim", "long_wrong_dose")
LONG_ID = "KB#0900"
_SENT = re.compile(r"(?<=[.!?])\s+")
_NUM = re.compile(r"\d+(?:\.\d+)?")


class _NoMasker:
    def mask(self, text: str) -> tuple[str, list[Any]]:
        return text, []


class ScriptedGenerator:
    """Returns the reply scripted for the question found in the prompt."""

    name = "scripted-eval"

    def __init__(self) -> None:
        self.replies: dict[str, str] = {}

    def generate(self, system: str, user: str, **_: Any) -> str:
        for q, reply in self.replies.items():
            if q in user:
                return reply
        return "I cannot answer this from the shared guidelines."


def first_sentence(text: str) -> str:
    s = _SENT.split(text.strip())[0].strip()
    return s if s.endswith((".", "!", "?")) else s + "."


def times_ten(text: str) -> str:
    return _NUM.sub(
        lambda m: (
            str(int(float(m.group(0)) * 10))
            if "." not in m.group(0)
            else f"{float(m.group(0)) * 10:g}"
        ),
        text,
    )


def cite(sentence: str, cid: str) -> str:
    return f"{sentence[:-1]} [{cid}]."


def long_text(item: schema.Item, all_texts: list[str]) -> str:
    own = [c.text for c in item.chunks]
    rest = [t for t in all_texts if t not in own]
    return " ".join(own + rest)


def chunks_for(item: schema.Item, mode: str, all_texts: list[str]) -> list[KBChunk]:
    if mode.startswith("long_"):
        return [KBChunk(LONG_ID, long_text(item, all_texts), "guideline", REFERENCE_SOURCE, "")]
    return [KBChunk(c.id, c.text, "guideline", REFERENCE_SOURCE, "") for c in item.chunks]


def numbered_sentence(text: str) -> str | None:
    """The first sentence of ``text`` that states a number, or None."""
    for s in _SENT.split(text.strip()):
        if _NUM.search(s):
            s = s.strip()
            return s if s.endswith((".", "!", "?")) else s + "."
    return None


def reply_for(item: schema.Item, mode: str) -> str | None:
    """The scripted reply, or None when the mode doesn't apply (a wrong dose of a chunk
    that states no number)."""
    if mode.endswith("wrong_dose"):
        n = numbered_sentence(item.chunks[0].text)
        if n is None:
            return None
        s = times_ten(n)
    else:
        s = first_sentence(item.chunks[0].text)
    return cite(s, LONG_ID if mode.startswith("long_") else item.chunks[0].id)


def run(out: Path) -> dict[str, Any]:
    loaded = schema.load_set(DEV)
    items = loaded.items
    all_texts = sorted({c.text for i in items for c in i.chunks})
    gen = ScriptedGenerator()
    build, versions = local_parts(generator=gen)
    client = OrchestratorModelClient(build, masker=_NoMasker(), versions=versions)
    report: dict[str, Any] = {
        "items": len(items),
        "must_escalate": sum(i.expect.must_escalate for i in items),
        "modes": {},
    }
    for mode in MODES:
        rows = []
        lat = []
        for item in items:
            reply = reply_for(item, mode)
            if reply is None:
                continue
            gen.replies = {item.question: reply}
            g = client.generate(item.question, {}, None, chunks_for(item, mode, all_texts))
            t = time.perf_counter()
            v = _verify_with(g.handle, g) if g.available else client.verify(g)
            lat.append((time.perf_counter() - t) * 1000)
            rows.append(
                {
                    "id": item.id,
                    "kind": item.kind,
                    "must_escalate": item.expect.must_escalate,
                    "decision": v.decision,
                    "support_gate": v.support_gate,
                    "guard_reasons": list(v.guard_reasons),
                    "guard_rules": list(v.guard_rules),
                    "nli": [
                        (c.get("nli_label"), c.get("nli_confidence"), c.get("reason"))
                        for c in (v.citation_checks or ())
                    ],
                    "trust": v.trust_score,
                }
            )
        dec = Counter(r["decision"] for r in rows)
        report["modes"][mode] = {
            "asked": len(rows),
            "decisions": dict(dec),
            "must_escalate_passed": [
                r["id"] for r in rows if r["must_escalate"] and r["decision"] == "PASS"
            ],
            "guarded_passed": [
                r["id"] for r in rows if r["guard_rules"] and r["decision"] == "PASS"
            ],
            "support_gate": dict(Counter(r["support_gate"] for r in rows)),
            "verify_ms_median": round(statistics.median(lat), 1),
            "verify_ms_p95": round(sorted(lat)[int(0.95 * (len(lat) - 1))], 1),
            "rows": rows,
        }
        print(
            mode,
            json.dumps({k: v for k, v in report["modes"][mode].items() if k != "rows"}),
            flush=True,
        )
    out.mkdir(parents=True, exist_ok=True)
    (out / "report.json").write_text(json.dumps(report, indent=2, default=str))
    return report


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", required=True)
    ap.add_argument(
        "--synthetic",
        action="store_true",
        help="required: confirms this runs on the synthetic dev split only",
    )
    args = ap.parse_args()
    if not args.synthetic:
        ap.exit(2, "refusing to run without --synthetic\n")
    run(Path(args.out))
    return 0


if __name__ == "__main__":
    os.environ.setdefault("TOKENIZERS_PARALLELISM", "false")
    sys.exit(main())

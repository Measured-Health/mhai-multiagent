"""The vetted reference subset (scripts/build_reference_subset.py) and the
refusals it relies on (reference_library.classify, row_refusal): patient
rows and the demo's synthetic source are refused by name, at convert and at
load. Synthetic fixtures only (tests/fixtures/reflib/chunks.jsonl and rows
made here); no model, no database.
"""

from __future__ import annotations

import gzip
import importlib.util
import json
import sys
from pathlib import Path
from typing import Any

import pytest

from intelligence.knowledge_base import reference_library as reflib
from tests.unit.test_reference_library_convert import FIXTURE, RETRIEVED, _artefact, _gz, _pins

_SCRIPT = Path(__file__).resolve().parents[2] / "scripts" / "build_reference_subset.py"
_spec = importlib.util.spec_from_file_location("build_reference_subset", _SCRIPT)
assert _spec is not None and _spec.loader is not None
subset = importlib.util.module_from_spec(_spec)
sys.modules["build_reference_subset"] = subset
_spec.loader.exec_module(subset)

SET_ID = "0b5e1f2a-1111-4222-8333-944455556666"


def _label(**over: Any) -> dict[str, Any]:
    row: dict[str, Any] = {
        "chunk_id": "KB#9",
        "content": "Synthetic boxed warning text.",
        "chunk_type": "drug_label",
        "source_patient_id": "openfda",
        "source_subject_id": SET_ID[:24],
        "metadata": {
            "source": "openfda",
            "drug": "synthetamol",
            "section": "boxed_warning",
            "set_id": SET_ID,
            "url": "https://dailymed.nlm.nih.gov/dailymed/lookup.cfm?setid=" + SET_ID,
        },
    }
    row.update(over)
    return row


def _fixture_records() -> list[Any]:
    """The fixture's JSON records (its one line that isn't JSON is skipped,
    as read_bundle_lines skips it)."""
    out: list[Any] = []
    for line in FIXTURE.read_text().splitlines():
        try:
            out.append(json.loads(line))
        except ValueError:
            continue
    return out


def test_classify_refuses_every_patient_type_by_name() -> None:
    for kind in sorted(reflib.PATIENT_TYPES):
        # Even dressed as an approved row, a patient type is refused first.
        rec = _label(chunk_type=kind)
        assert reflib.classify(rec, RETRIEVED) == (None, reflib.REFUSED_PATIENT)
    # The fixture's Synthea rows are all counted as patient rows.
    patients = [r for r in _fixture_records() if r.get("chunk_type") in reflib.PATIENT_TYPES]
    assert len(patients) == 6
    assert all(reflib.classify(r, RETRIEVED)[1] == reflib.REFUSED_PATIENT for r in patients)


def test_an_approved_type_with_a_patient_id_is_refused() -> None:
    assert reflib.classify(_label(), RETRIEVED)[1] == reflib.APPROVED
    for pid in ("SYN-00000000aa", None, "", "OPENFDA"):
        assert reflib.classify(_label(source_patient_id=pid), RETRIEVED) == (
            None,
            reflib.REFUSED_PATIENT,
        )


def test_anything_naming_the_demo_source_is_refused() -> None:
    for text in (
        reflib.DEMO_SOURCE,
        "sample   REFERENCE (Synthetic Demo)",
        "Sample answer (synthetic demo data, not clinical advice): x",
    ):
        rec = _label(content="Synthetic boxed warning. " + text)
        assert reflib.classify(rec, RETRIEVED) == (None, reflib.REFUSED_DEMO)
        meta = dict(_label()["metadata"], drug=text)
        assert reflib.classify(_label(metadata=meta), RETRIEVED) == (None, reflib.REFUSED_DEMO)


def test_left_out_types_are_refused_by_type() -> None:
    for kind in ("qa_reference", "literature", "clinical_trial", "ontology_term", "guideline"):
        assert reflib.classify(_label(chunk_type=kind), RETRIEVED)[1] == reflib.REFUSED_TYPE


def test_subset_rows_counts_and_reasons(tmp_path: Path) -> None:
    rows, kept, refused = subset.build(_fixture_records(), RETRIEVED)
    assert kept == {"openFDA": 3, "MedlinePlus": 1}
    assert [r["kind"] for r in rows] == ["drug_label"] * 3 + ["health_topic"]
    for r in rows:
        assert set(r) >= {"content", "title", "source", "licence", "upstream_id", "url"}
        assert r["upstream_id"]
        assert not any(k.startswith("source_") or "patient" in k for k in r)
    assert rows[0]["section"] == "boxed_warning"
    assert rows[-1]["upstream_id"] == "4242"
    assert sum(n for (_, _, why), n in refused.items() if why == reflib.REFUSED_PATIENT) == 6
    assert refused[("qa_reference", "medquad", reflib.REFUSED_TYPE)] == 1
    assert refused[("guideline", "none", reflib.REFUSED_TYPE)] == 1
    assert refused[("drug_label", "openfda", "duplicate")] == 1
    # Every fixture line is either kept or counted.
    assert len(rows) + sum(refused.values()) == len(_fixture_records())


def test_subset_cli_is_deterministic_and_prints_counts_only(
    tmp_path: Path, capsys: pytest.CaptureFixture[str]
) -> None:
    src = _gz(tmp_path)
    outs = [tmp_path / "a.jsonl.gz", tmp_path / "b.jsonl.gz"]
    for out in outs:
        assert subset.main(["--input", str(src), "--retrieved", RETRIEVED, "--out", str(out)]) == 0
    assert outs[0].read_bytes() == outs[1].read_bytes()
    printed = capsys.readouterr().out
    assert "kept 4 rows: MedlinePlus 1, openFDA 3" in printed
    assert "patient" in printed
    for text in json.loads(gzip.decompress(outs[0].read_bytes()).splitlines()[0]).values():
        if isinstance(text, str) and len(text) > 20:
            assert text not in printed
    with pytest.raises(SystemExit):
        subset.main(["--input", str(src), "--retrieved", "14/11/2026", "--out", str(outs[0])])


def test_row_refusal_closes_the_artefact_row(tmp_path: Path) -> None:
    path, sha, header = _artefact(tmp_path)
    good = list(reflib.read_rows(path, header))
    assert len(good) == 4
    for r in good:
        assert reflib.row_refusal(r.content, r.metadata) == ""
    meta = dict(good[0].metadata)
    assert (
        reflib.row_refusal("x", {**meta, "patient_id": "SYN-1"}) == "row_patient_or_unknown_field"
    )
    assert reflib.row_refusal("x", {**meta, "kind": "lab"}) == "row_kind_not_approved"
    assert reflib.row_refusal("x", {**meta, "source": "Sample"}) == "row_kind_not_approved"
    assert reflib.row_refusal("x", {**meta, "title": reflib.DEMO_SOURCE}) == "row_demo_source"
    assert reflib.row_refusal("From " + reflib.DEMO_SOURCE, meta) == "row_demo_source"
    # The approved rows still read and verify against their pin.
    assert reflib.verify(path, _pins(sha))[1] == sha

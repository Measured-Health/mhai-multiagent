"""Several reference libraries at once (reference_library.LIBRARIES): each
name has its own block of chunk ids and its own approved kinds, checked at
build and again at load. Synthetic rows only.
"""

from __future__ import annotations

from pathlib import Path
from typing import Any

import pytest

from intelligence.knowledge_base import guideline_pdf as guidelines
from intelligence.knowledge_base import reference_library as reflib
from tests.unit.test_reference_library_convert import EMBEDDER, RETRIEVED, fake_embed


def _formulary_entry(**meta: Any) -> reflib.Entry:
    m: dict[str, Any] = {
        "kind": "local_formulary",
        "source": reflib.OWN_SOURCE,
        "title": "Synthlonart",
        "licence": "CC0-1.0",
        "brand": "Synthlonart",
        "generic": "synthemether",
        **meta,
    }
    return reflib.Entry("local_formulary", "Synthlonart is synthemether.", m, (0,))


def test_each_library_has_its_own_block() -> None:
    blocks = [s.block for s in reflib.LIBRARIES.values()]
    assert len(set(blocks)) == len(blocks) and all(1 <= b <= 9 for b in blocks)
    assert reflib.first_id("medref") == reflib.FIRST_ID
    assert reflib.first_id("local-formulary") == 2_000_001
    assert reflib.in_block("KB#2000001", "local-formulary")
    assert not reflib.in_block("KB#1000001", "local-formulary")
    assert not reflib.in_block("KB#2000000", "local-formulary")
    assert not reflib.in_block("KB#2000001", "synthetic-other")
    with pytest.raises(reflib.LibraryError):
        reflib.first_id("synthetic-other")


def test_build_refuses_a_row_the_library_does_not_approve() -> None:
    with pytest.raises(reflib.LibraryError) as info:
        reflib.build(
            [_formulary_entry()],
            fake_embed,
            version="1.0.0",
            embedder=EMBEDDER,
            built_from="ab" * 32,
            retrieved=RETRIEVED,
        )
    assert str(info.value) == "row_kind_not_approved"
    with pytest.raises(reflib.LibraryError) as info:
        reflib.build(
            [_formulary_entry(patient_id="SYN-1")],
            fake_embed,
            version="1.0.0",
            embedder=EMBEDDER,
            built_from="ab" * 32,
            retrieved=RETRIEVED,
            name="local-formulary",
        )
    assert str(info.value) == "row_patient_or_unknown_field"


def _write(tmp_path: Path, name: str, chunk_id: str) -> tuple[Path, dict[str, reflib.Pin]]:
    header, rows = reflib.build(
        [_formulary_entry()],
        fake_embed,
        version="1.0.0",
        embedder=EMBEDDER,
        built_from="ab" * 32,
        retrieved=RETRIEVED,
        name="local-formulary",
    )
    header = reflib.Header(**{**header.__dict__, "name": name})
    rows = [reflib.Row(chunk_id, r.content, r.metadata, r.embedding) for r in rows]
    path, sha = reflib.write_artefact(header, rows, tmp_path)
    return path, {name: reflib.Pin("1.0.0", sha, 1)}


def test_load_refuses_an_id_outside_the_block_and_an_unknown_library(tmp_path: Path) -> None:
    path, pins = _write(tmp_path, "local-formulary", "KB#2000001")
    header, _, data = reflib.read_pinned(path, pins)
    assert [r.chunk_id for r in reflib.read_rows(data, header)] == ["KB#2000001"]

    path, pins = _write(tmp_path, "local-formulary", "KB#1000001")
    header, _, data = reflib.read_pinned(path, pins)
    with pytest.raises(reflib.LibraryError) as info:
        list(reflib.read_rows(data, header))
    assert str(info.value) == "id_outside_block"

    # The formulary's row under medref's name: its kind isn't medref's.
    path, pins = _write(tmp_path, "medref", "KB#1000001")
    header, _, data = reflib.read_pinned(path, pins)
    with pytest.raises(reflib.LibraryError) as info:
        list(reflib.read_rows(data, header))
    assert str(info.value) == "row_kind_not_approved"

    path, pins = _write(tmp_path, "synthetic-other", "KB#2000001")
    with pytest.raises(reflib.LibraryError) as info:
        reflib.read_pinned(path, pins)
    assert str(info.value) == "bad_header"


def test_no_guideline_is_pinned_unless_its_licence_permits() -> None:
    """Pinning a guideline document as loadable is the owner's licence
    decision: models/reference-library.lock may pin only a "permitted" one."""
    assert guidelines.check_pins(reflib.load_lock(), guidelines.load_documents()) == []
    docs = guidelines.load_documents()
    pins = {"who-malaria": reflib.Pin("1.0.0", "ab" * 32, 1)}
    assert docs["who-malaria"].licence_status != "permitted"
    assert guidelines.check_pins(pins, docs) == ["who-malaria"]


def test_core_mirrors_the_library_table() -> None:
    go = (reflib.REPO / "services" / "core" / "cmd" / "core" / "reflib.go").read_text()
    for name, spec in reflib.LIBRARIES.items():
        assert f'"{name}"' in go and f"Block: {spec.block}," in go
        for kind, sources in spec.sources.items():
            assert f'"{kind}"' in go
            for s in sources:
                assert f'"{s}"' in go
    for key in reflib.ROW_KEYS:
        assert f'"{key}"' in go, key

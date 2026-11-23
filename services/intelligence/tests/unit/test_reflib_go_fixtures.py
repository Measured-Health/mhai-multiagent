"""Core reads the Python converters' artefacts as they are: the Go tests
read services/core/cmd/core/testdata/{local-formulary,who-malaria}-python-fixture.jsonl.gz
(reflib_libraries_test.go). This test rebuilds both from synthetic inputs
and checks the committed bytes are what the converters write today.

Regenerate after a format change:
    REFLIB_WRITE_GO_FIXTURES=1 uv run pytest tests/unit/test_reflib_go_fixtures.py
"""

from __future__ import annotations

import os
from pathlib import Path

import pytest

from intelligence.knowledge_base import guideline_pdf as guidelines
from intelligence.knowledge_base import local_formulary as formulary
from intelligence.knowledge_base import reference_library as reflib
from tests.unit._synthetic_pdf import make_pdf
from tests.unit.test_guideline_pdf import _doc, _pages
from tests.unit.test_local_formulary import FIXTURE, RETRIEVED
from tests.unit.test_reference_library_convert import EMBEDDER, fake_embed

TESTDATA = reflib.REPO / "services" / "core" / "cmd" / "core" / "testdata"


def _artefacts(tmp_path: Path) -> dict[str, Path]:
    out: dict[str, Path] = {}
    header, rows = formulary.build(
        FIXTURE, fake_embed, version="1.0.0", embedder=EMBEDDER, retrieved=RETRIEVED
    )
    (tmp_path / "f").mkdir()
    out["local-formulary"], _ = reflib.write_artefact(header, rows, tmp_path / "f")
    data = make_pdf(_pages())
    header, rows = guidelines.build(_doc(data), data, fake_embed, embedder=EMBEDDER)
    (tmp_path / "g").mkdir()
    out["who-malaria"], _ = reflib.write_artefact(header, rows, tmp_path / "g")
    return out


@pytest.mark.parametrize("name", ["local-formulary", "who-malaria"])
def test_core_fixtures_are_what_the_converters_write(tmp_path: Path, name: str) -> None:
    built = _artefacts(tmp_path)[name].read_bytes()
    target = TESTDATA / f"{name}-python-fixture.jsonl.gz"
    if os.environ.get("REFLIB_WRITE_GO_FIXTURES") == "1":
        target.write_bytes(built)
    assert target.read_bytes() == built

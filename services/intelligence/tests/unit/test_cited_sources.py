"""boundary/cited_sources.py: the title, link and source credit an answer
shows for each cited shared reference row, and nothing for anything else.
Every chunk and value is synthetic.
"""

from __future__ import annotations

from typing import Any

from intelligence.boundary.cited_sources import MAX_SOURCES, MAX_TITLE, cited_sources
from intelligence.knowledge_base.kb_builder import REFERENCE_SOURCE, KBChunk

URL = "https://medlineplus.gov/syntheticfever.html"


def _ref(cid: str, **meta: Any) -> KBChunk:
    base = {
        "title": "Synthetic Fever",
        "url": URL,
        "source": "MedlinePlus",
        "attribution": "Source: MedlinePlus, National Library of Medicine.",
    }
    return KBChunk(cid, "Synthetic text.", "guideline", REFERENCE_SOURCE, "", {**base, **meta})


def test_cited_reference_rows_give_their_sources_in_order_once() -> None:
    chunks = [_ref("KB#1000001"), _ref("KB#1000002", title="Synthetamol (Boxed warning)")]
    got = cited_sources(chunks, ["KB#1000002", "KB#1000001", "KB#1000002", "KB#9999"])
    assert [s["chunk_id"] for s in got] == ["KB#1000002", "KB#1000001"]
    assert got[1] == {
        "chunk_id": "KB#1000001",
        "title": "Synthetic Fever",
        "url": URL,
        "source": "MedlinePlus",
        "attribution": "Source: MedlinePlus, National Library of Medicine.",
    }


def test_patient_and_hospital_rows_never_give_a_source() -> None:
    meta = {"title": "SYNTHETIC-PHI-MARKER", "source": "x", "url": URL}
    chunks = [
        KBChunk("KB#0003", "Synthetic lab.", "lab", "P-SYN-1", "S-1", meta),
        KBChunk("KB#0004", "Synthetic.", "guideline", "unassigned", "", meta),
        KBChunk("KB#0005", "Synthetic.", "lab", REFERENCE_SOURCE, "", meta),
    ]
    assert cited_sources(chunks, ["KB#0003", "KB#0004", "KB#0005"]) == ()


def test_fields_are_checked_not_trusted() -> None:
    for bad_url in (
        "http://medlineplus.gov/x.html",
        "javascript:alert(1)",
        "https://user@medlineplus.gov/",
        "https://medlineplus.gov:8443/x",
        'https://medlineplus.gov/x"><script>',
        "https://medlineplus.gov/x#frag",
        "https://" + "a" * 600 + ".gov/",
        None,
        12,
    ):
        (s,) = cited_sources([_ref("KB#1000001", url=bad_url)], ["KB#1000001"])
        assert s["url"] is None, bad_url
    (s,) = cited_sources(
        [_ref("KB#1000001", title="Synthetic\u202e\nFever\t ", attribution=7)], ["KB#1000001"]
    )
    assert s["title"] == "Synthetic Fever" and s["attribution"] is None
    (s,) = cited_sources([_ref("KB#1000001", title="x" * 1000)], ["KB#1000001"])
    assert len(s["title"]) == MAX_TITLE
    for missing in ({"title": ""}, {"title": None}, {"source": "  "}):
        assert cited_sources([_ref("KB#1000001", **missing)], ["KB#1000001"]) == ()
    no_meta = KBChunk("KB#1000001", "Synthetic.", "guideline", REFERENCE_SOURCE, "", None)
    assert cited_sources([no_meta], ["KB#1000001"]) == ()


def test_at_most_max_sources() -> None:
    chunks = [_ref(f"KB#{1000001 + i}") for i in range(MAX_SOURCES + 5)]
    assert len(cited_sources(chunks, [c.chunk_id for c in chunks])) == MAX_SOURCES

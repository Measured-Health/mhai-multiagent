"""Loading a reference library artefact (reference_library.load) into the
Postgres 16 that core's own `migrate up` built, then reading it as the
worker does. The artefact is built from the synthetic fixture with a
synthetic embedder; every row is synthetic.
"""

from __future__ import annotations

import importlib.util
import sys
from collections.abc import AsyncIterator, Iterator
from pathlib import Path
from typing import Any

import psycopg
import pytest

from intelligence.boundary import kb_repo
from intelligence.boundary.db import Database
from intelligence.knowledge_base import reference_library as reflib
from intelligence.knowledge_base.kb_builder import REFERENCE_SOURCE
from tests.boundary.conftest import INTELLIGENCE_DB_PASSWORD, INTELLIGENCE_DB_USER, Postgres
from tests.boundary.test_db import HOSPITAL_A, _seed, verified_for
from tests.unit.test_reference_library_convert import RETRIEVED, _entries, fake_embed

pytestmark = pytest.mark.boundary

_SCRIPT = Path(__file__).resolve().parents[2] / "scripts" / "reflib_load.py"
_spec = importlib.util.spec_from_file_location("reflib_load", _SCRIPT)
assert _spec is not None and _spec.loader is not None
loader = importlib.util.module_from_spec(_spec)
sys.modules["reflib_load"] = loader  # its dataclass needs its module registered
_spec.loader.exec_module(loader)


def _build(tmp_path: Path, version: str, entries: list[reflib.Entry]) -> tuple[Path, str, int]:
    header, rows = reflib.build(
        entries,
        fake_embed,
        version=version,
        embedder="sentence-transformers/synthetic@" + "0" * 40,
        built_from="ab" * 32,
        retrieved=RETRIEVED,
    )
    out = tmp_path / version
    out.mkdir()
    path, sha = reflib.write_artefact(header, rows, out)
    return path, sha, header.count


@pytest.fixture
def conn(postgres: Postgres) -> Iterator[psycopg.Connection[Any]]:
    with psycopg.connect(postgres.url, autocommit=True) as c:
        yield c
        c.execute("DELETE FROM kb_chunks WHERE library_id IS NOT NULL")
        c.execute("DELETE FROM reference_libraries WHERE name = %s", (reflib.NAME,))


@pytest.fixture
async def database(postgres: Postgres) -> AsyncIterator[Database]:
    db = Database(
        postgres.intelligence_url,
        user=INTELLIGENCE_DB_USER,
        password=INTELLIGENCE_DB_PASSWORD,
        max_size=1,
    )
    await db.open()
    try:
        yield db
    finally:
        await db.close()


def _rows(c: psycopg.Connection[Any]) -> list[tuple[Any, ...]]:
    return c.execute(
        "SELECT k.chunk_id, k.hospital_id, k.source_patient_id, k.source_subject_id, "
        "k.chunk_type, cardinality(k.embedding), l.version, l.active "
        "FROM kb_chunks k JOIN reference_libraries l ON l.id = k.library_id ORDER BY k.chunk_id"
    ).fetchall()


async def test_load_then_retrieve_as_the_worker(
    tmp_path: Path, conn: psycopg.Connection[Any], database: Database, postgres: Postgres
) -> None:
    await _seed(postgres)
    path, sha, count = _build(tmp_path, "1.0.0", _entries(tmp_path))
    pins = {reflib.NAME: reflib.Pin("1.0.0", sha, count)}
    done = loader.load(conn, path, pins)
    assert not done.already and done.rows == count == 4
    rows = _rows(conn)
    assert [r[0] for r in rows] == ["KB#1000001", "KB#1000002", "KB#1000003", "KB#1000004"]
    # Shared, patient-free guidelines with their vectors, in the active version.
    assert {r[1:] for r in rows} == {(None, None, None, "guideline", 8, "1.0.0", True)}
    lib = conn.execute(
        "SELECT sha256, licence, attribution, chunk_count FROM reference_libraries WHERE name = %s",
        (reflib.NAME,),
    ).fetchone()
    assert lib == (sha, reflib.LICENCE, reflib.ATTRIBUTION, 4)

    async with database.with_hospital(verified_for(HOSPITAL_A)) as cur:
        chunks = await kb_repo.load_chunks(cur, None, question="synthetamol liver toxicity")
    boxed = next(c for c in chunks if c.chunk_id == "KB#1000001")
    assert boxed.source_patient_id == REFERENCE_SOURCE
    assert boxed.metadata["section"] == "boxed_warning"
    assert boxed.metadata["url"].startswith("https://dailymed.nlm.nih.gov/")
    assert boxed.embedding is not None and boxed.embedding.shape == (8,)

    # The active version again: nothing changes.
    assert loader.load(conn, path, pins).already
    assert _rows(conn) == rows


async def test_a_new_version_retires_the_old_one(
    tmp_path: Path, conn: psycopg.Connection[Any]
) -> None:
    entries = _entries(tmp_path)
    p1, sha1, n1 = _build(tmp_path, "1.0.0", entries)
    p2, sha2, n2 = _build(tmp_path, "1.1.0", entries[:2])
    loader.load(conn, p1, {reflib.NAME: reflib.Pin("1.0.0", sha1, n1)})
    loader.load(conn, p2, {reflib.NAME: reflib.Pin("1.1.0", sha2, n2)})
    assert {(r[0], r[6]) for r in _rows(conn)} == {("KB#1000001", "1.1.0"), ("KB#1000002", "1.1.0")}
    versions = conn.execute(
        "SELECT version, active FROM reference_libraries WHERE name = %s ORDER BY version",
        (reflib.NAME,),
    ).fetchall()
    assert versions == [("1.0.0", False), ("1.1.0", True)]
    # Back to 1.0.0 (a rollback): reloaded from its pinned bytes, active again.
    assert not loader.load(conn, p1, {reflib.NAME: reflib.Pin("1.0.0", sha1, n1)}).already
    assert {r[6] for r in _rows(conn)} == {"1.0.0"} and len(_rows(conn)) == n1


async def test_refusals_write_nothing(tmp_path: Path, conn: psycopg.Connection[Any]) -> None:
    entries = _entries(tmp_path)
    path, sha, count = _build(tmp_path, "1.0.0", entries)
    with pytest.raises(reflib.LibraryError):
        loader.load(conn, path, {reflib.NAME: reflib.Pin("1.0.0", "cd" * 32, count)})
    assert _rows(conn) == []
    # The same version with other bytes, once one is loaded.
    loader.load(conn, path, {reflib.NAME: reflib.Pin("1.0.0", sha, count)})
    (tmp_path / "x").mkdir()
    other, other_sha, other_n = _build(tmp_path / "x", "1.0.0", entries[:1])
    with pytest.raises(reflib.LibraryError) as info:
        loader.load(conn, other, {reflib.NAME: reflib.Pin("1.0.0", other_sha, other_n)})
    assert str(info.value) == "version_reloaded_with_other_bytes"
    assert len(_rows(conn)) == count

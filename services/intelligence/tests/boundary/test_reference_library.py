"""A large shared reference library in kb_chunks (migration 00040), against
the Postgres 16 that core's own `migrate up` built.

- A hospital's own chunks are never crowded out, however many shared rows
  there are.
- A library row is shared and never patient-scoped: the database refuses
  one with a hospital or a patient.
- Retrieval over tens of thousands of shared rows stays within a time
  budget: the database pre-filters them to the question's candidates.

Every id, row and word is synthetic.
"""

from __future__ import annotations

import time
import uuid
from collections.abc import AsyncIterator
from typing import Any

import psycopg
import pytest

from intelligence.boundary import kb_repo
from intelligence.boundary.db import Database
from intelligence.knowledge_base.kb_builder import (
    REFERENCE_SOURCE,
    DenseRetriever,
    HybridRetriever,
    MedicalKnowledgeBase,
)
from tests.boundary.conftest import INTELLIGENCE_DB_PASSWORD, INTELLIGENCE_DB_USER, Postgres
from tests.boundary.test_db import HOSPITAL_A, _seed, _superuser, verified_for

pytestmark = pytest.mark.boundary

LIBRARY_ROWS = 30_000
HOSPITAL_ROWS = 50
PATIENT = "P-SYN-REFLIB"
# The whole read (three queries) and a job's index and retrieval over what
# it returns. Generous for a shared CI runner; locally it is far less.
BUDGET_S = 3.0
WORDS = ("dosage", "warning", "kidney", "pregnancy", "interaction", "rash", "liver", "fever")
NEEDLE = "zyntharovax"
SHA = "ab" * 32


async def _library(conn: psycopg.AsyncConnection[Any], name: str, active: bool = True) -> int:
    cur = await conn.execute(
        "INSERT INTO reference_libraries (name, version, sha256, licence, attribution, embedder, "
        "chunk_count, active) VALUES (%s, '1.0.0', %s, 'CC0-1.0 (synthetic)', "
        "'Synthetic attribution.', 'sentence-transformers/synthetic@' || repeat('0', 40), 0, %s) "
        "RETURNING id",
        (name, SHA, active),
    )
    row = await cur.fetchone()
    assert row is not None
    return int(row[0])


@pytest.fixture
async def database(postgres: Postgres) -> AsyncIterator[Database]:
    """The pipeline's own login, as the worker connects."""
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


@pytest.fixture
async def big_library(postgres: Postgres) -> AsyncIterator[int]:
    """30,000 active library rows and this hospital's own patient rows."""
    await _seed(postgres)
    name = "synthetic-" + uuid.uuid4().hex[:8]
    async with await _superuser(postgres) as conn:
        lib = await _library(conn, name)
        await conn.execute(
            "INSERT INTO kb_chunks (chunk_id, hospital_id, content, chunk_type, metadata_json, "
            "library_id, embedding) "
            "SELECT 'KB#' || (1000000 + g), NULL, "
            "  'Synthetic reference ' || g || ': ' || (%s::text[])[1 + g %% 8] || ' and ' "
            "  || (%s::text[])[1 + (g / 8) %% 8] || ' for a synthetic drug.', 'guideline', "
            "  jsonb_build_object('kind', 'drug_label', 'title', 'Synthetic ' || g), %s, "
            "  array_fill(0.05::real, ARRAY[384]) "
            "FROM generate_series(1, %s) g",
            (list(WORDS), list(WORDS), lib, LIBRARY_ROWS),
        )
        await conn.execute(
            "UPDATE kb_chunks SET content = %s WHERE library_id = %s AND chunk_id = 'KB#1012345'",
            (f"Synthetic {NEEDLE} boxed warning: synthetic kidney monitoring.", lib),
        )
        await conn.execute(
            "INSERT INTO kb_chunks (chunk_id, hospital_id, content, chunk_type, "
            "source_patient_id) SELECT 'KB#R' || g, %s, 'Synthetic own lab ' || g || '.', "
            "'lab', %s FROM generate_series(1, %s) g ON CONFLICT DO NOTHING",
            (HOSPITAL_A, PATIENT, HOSPITAL_ROWS),
        )
        await conn.execute("ANALYZE kb_chunks")
    try:
        yield lib
    finally:
        async with await _superuser(postgres) as conn:
            await conn.execute("DELETE FROM kb_chunks WHERE library_id = %s", (lib,))
            await conn.execute(
                "DELETE FROM kb_chunks WHERE hospital_id = %s AND source_patient_id = %s",
                (HOSPITAL_A, PATIENT),
            )
            await conn.execute("DELETE FROM reference_libraries WHERE id = %s", (lib,))


def _offline_retriever(monkeypatch: pytest.MonkeyPatch) -> HybridRetriever:
    def no_model(self: DenseRetriever) -> None:
        self._loaded = False

    monkeypatch.setattr(DenseRetriever, "_try_load_model", no_model)
    return HybridRetriever(dense_model="sentence-transformers/synthetic")


async def test_retrieval_over_a_large_library_is_bounded_and_fast(
    big_library: int, database: Database, monkeypatch: pytest.MonkeyPatch
) -> None:
    question = f"What does the {NEEDLE} boxed warning say about kidney monitoring?"
    started = time.perf_counter()
    async with database.with_hospital(verified_for(HOSPITAL_A)) as cur:
        chunks = await kb_repo.load_chunks(cur, PATIENT, question=question)
    loaded = time.perf_counter() - started

    ids = {c.chunk_id for c in chunks}
    # Every one of this patient's own rows, despite 30,000 shared ones.
    assert {f"KB#R{i}" for i in range(1, HOSPITAL_ROWS + 1)} <= ids
    library = [c for c in chunks if kb_repo.LIBRARY_CHUNK_ID.fullmatch(c.chunk_id)]
    assert 0 < len(library) <= kb_repo.LIBRARY_CANDIDATES
    assert library[0].chunk_id == "KB#1012345"  # the best match first
    assert all(c.source_patient_id == REFERENCE_SOURCE for c in library)
    assert all(c.embedding is not None and c.embedding.shape == (384,) for c in library)

    retriever = _offline_retriever(monkeypatch)
    kb = MedicalKnowledgeBase()
    kb.chunks = {c.chunk_id: c for c in chunks}
    retriever.index_kb(kb)
    results = retriever.retrieve(question, top_k=5, patient_id=PATIENT)
    total = time.perf_counter() - started

    assert "KB#1012345" in {r.chunk.chunk_id for r in results}
    print(
        f"\nreference library timing: {LIBRARY_ROWS} shared rows, load {loaded * 1000:.0f} ms, "
        f"load+index+retrieve {total * 1000:.0f} ms, {len(chunks)} chunks to the job"
    )
    assert total < BUDGET_S, total


async def test_without_a_question_no_library_row_is_read(
    big_library: int, database: Database
) -> None:
    async with database.with_hospital(verified_for(HOSPITAL_A)) as cur:
        chunks = await kb_repo.load_chunks(cur, PATIENT)
    # The old read (shared rows first, LIMIT 5000) returned none of these.
    assert {f"KB#R{i}" for i in range(1, HOSPITAL_ROWS + 1)} <= {c.chunk_id for c in chunks}
    assert not [c for c in chunks if c.chunk_id.startswith("KB#1")]


async def test_an_inactive_library_is_not_read(postgres: Postgres, database: Database) -> None:
    await _seed(postgres)
    async with await _superuser(postgres) as conn:
        lib = await _library(conn, "synthetic-" + uuid.uuid4().hex[:8], active=False)
        await conn.execute(
            "INSERT INTO kb_chunks (chunk_id, hospital_id, content, chunk_type, library_id) "
            "VALUES ('KB#1999999', NULL, %s, 'guideline', %s)",
            (f"Synthetic {NEEDLE} retired row.", lib),
        )
    try:
        async with database.with_hospital(verified_for(HOSPITAL_A)) as cur:
            chunks = await kb_repo.load_chunks(cur, None, question=NEEDLE)
        assert "KB#1999999" not in {c.chunk_id for c in chunks}
    finally:
        async with await _superuser(postgres) as conn:
            await conn.execute("DELETE FROM kb_chunks WHERE library_id = %s", (lib,))
            await conn.execute("DELETE FROM reference_libraries WHERE id = %s", (lib,))


@pytest.mark.parametrize(
    ("hospital", "source", "subject", "chunk_id", "embedding", "constraint"),
    [
        (HOSPITAL_A, None, None, "KB#1900001", None, "kb_library_rows_are_shared"),
        (None, PATIENT, None, "KB#1900002", None, "kb_shared_has_no_patient"),
        (None, None, PATIENT, "KB#1900003", None, "kb_shared_has_no_patient"),
        (None, None, None, "KB#0007", None, "kb_library_chunk_id"),
        (None, None, None, "KB#0900004", None, "kb_library_chunk_id"),
        (None, None, None, "KB#1900005", [[0.1, 0.2]], "kb_embedding_is_library"),
    ],
)
async def test_a_library_row_is_shared_and_never_patient_scoped(
    postgres: Postgres,
    hospital: str | None,
    source: str | None,
    subject: str | None,
    chunk_id: str,
    embedding: Any,
    constraint: str,
) -> None:
    await _seed(postgres)
    async with await _superuser(postgres) as conn:
        lib = await _library(conn, "synthetic-" + uuid.uuid4().hex[:8], active=False)
        try:
            with pytest.raises(psycopg.errors.CheckViolation) as info:
                await conn.execute(
                    "INSERT INTO kb_chunks (chunk_id, hospital_id, content, chunk_type, "
                    "source_patient_id, source_subject_id, library_id, embedding) "
                    "VALUES (%s, %s, 'Synthetic.', 'guideline', %s, %s, %s, %s)",
                    (chunk_id, hospital, source, subject, lib, embedding),
                )
            assert info.value.diag.constraint_name == constraint
        finally:
            await conn.execute("DELETE FROM reference_libraries WHERE id = %s", (lib,))


async def test_an_embedding_only_on_a_library_row(postgres: Postgres) -> None:
    async with await _superuser(postgres) as conn:
        with pytest.raises(psycopg.errors.CheckViolation) as info:
            await conn.execute(
                "INSERT INTO kb_chunks (chunk_id, hospital_id, content, chunk_type, embedding) "
                "VALUES ('KB#0098', NULL, 'Synthetic.', 'guideline', ARRAY[0.1, 0.2]::real[])"
            )
        assert info.value.diag.constraint_name == "kb_embedding_is_library"


async def test_one_active_version_per_library(postgres: Postgres) -> None:
    name = "synthetic-" + uuid.uuid4().hex[:8]
    async with await _superuser(postgres) as conn:
        first = await _library(conn, name)
        try:
            with pytest.raises(psycopg.errors.UniqueViolation):
                await conn.execute(
                    "INSERT INTO reference_libraries (name, version, sha256, licence, "
                    "attribution, embedder, chunk_count, active) VALUES (%s, '1.0.1', %s, "
                    "'CC0-1.0', 'Synthetic.', 'synthetic', 0, true)",
                    (name, SHA),
                )
        finally:
            await conn.execute("DELETE FROM reference_libraries WHERE id = %s", (first,))


async def test_worker_reads_libraries_and_writes_none(
    postgres: Postgres, database: Database
) -> None:
    await _seed(postgres)
    async with await _superuser(postgres) as conn:
        lib = await _library(conn, "synthetic-" + uuid.uuid4().hex[:8], active=False)
    try:
        async with database.with_hospital(verified_for(HOSPITAL_A)) as cur:
            await cur.execute("SELECT name FROM reference_libraries WHERE id = %s", (lib,))
            assert await cur.fetchone() is not None
        for q in (
            "UPDATE reference_libraries SET active = true WHERE id = %s",
            "DELETE FROM reference_libraries WHERE id = %s",
            "INSERT INTO kb_chunks (chunk_id, content, chunk_type, library_id) "
            "VALUES ('KB#1900009', 'Synthetic.', 'guideline', %s)",
        ):
            with pytest.raises(psycopg.errors.InsufficientPrivilege):
                async with database.with_hospital(verified_for(HOSPITAL_A)) as cur:
                    await cur.execute(q, (lib,))
    finally:
        async with await _superuser(postgres) as conn:
            await conn.execute("DELETE FROM reference_libraries WHERE id = %s", (lib,))

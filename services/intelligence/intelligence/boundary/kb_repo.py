"""kb_chunks → KBChunk, under row-level security (plan departure 10).

load_chunks runs on the with_hospital cursor, so RLS returns this
hospital's chunks and the shared reference rows (hospital_id IS NULL,
policy shared_read) and nothing of any other hospital. It never filters
by hospital itself: that is the database's job, and the hospital is the
verified token's.

Patient scope is ours (ADR 0004): every path takes patient_id and keeps
only what chunk_visible_to() allows. A shared row becomes a reference chunk
(source_patient_id = REFERENCE_SOURCE), which chunk_visible_to shows to
everyone if it is a guideline. A hospital's own row can never be one: a
reserved source value on it (say "reference") is mapped to the unassigned
source, which no patient sees. Until patient ids exist (departure 7)
patient_id is None, so only shared guidelines come back.

Never opens a connection or cursor (test_no_direct_db). Logs nothing about
the rows.
"""

from __future__ import annotations

from typing import Any

from intelligence.boundary.db import Cursor
from intelligence.knowledge_base.kb_builder import (
    REFERENCE_SOURCE,
    RESERVED_PATIENT_IDS,
    UNASSIGNED_SOURCE,
    KBChunk,
    chunk_visible_to,
)

# Indexing per job is fine for synthetic data (departure 10); the bound
# keeps one job from pulling an unbounded table into memory.
MAX_CHUNKS = 5000

_LOAD = """SELECT chunk_id, hospital_id IS NULL, content, chunk_type, source_patient_id,
    source_subject_id, metadata_json
FROM kb_chunks ORDER BY hospital_id NULLS FIRST, id LIMIT %s"""


def _source(shared: bool, source: str | None) -> str:
    if shared:
        return REFERENCE_SOURCE
    if source is None or source.strip().lower() in RESERVED_PATIENT_IDS:
        return UNASSIGNED_SOURCE
    return source


def _chunk(row: tuple[Any, ...]) -> KBChunk:
    chunk_id, shared, content, chunk_type, source, subject, metadata = row
    return KBChunk(
        chunk_id=str(chunk_id),
        content=str(content),
        chunk_type=str(chunk_type),
        source_patient_id=_source(bool(shared), source),
        source_subject_id="" if shared or subject is None else str(subject),
        metadata=dict(metadata) if isinstance(metadata, dict) else {},
    )


async def load_chunks(cur: Cursor, patient_id: str | None) -> list[KBChunk]:
    """The chunks a question about patient_id may use: what RLS shows this
    hospital, then only what chunk_visible_to allows for patient_id."""
    await cur.execute(_LOAD, (MAX_CHUNKS,))
    rows = await cur.fetchall()
    chunks = [_chunk(tuple(r)) for r in rows]
    return [c for c in chunks if chunk_visible_to(c, patient_id)]

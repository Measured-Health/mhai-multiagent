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

A shared guideline row is also sealed as a SharedChunk (cloud/gate.py) and
the seal recorded in the chunk's metadata["_shared"], so the egress gate
can tell a shared guideline from anything else (design section 4.2). The
local path never reads it. This module is the only caller of
seal_shared_chunk (tests/boundary/cloud/test_imports.py).

Never opens a connection or cursor (test_no_direct_db). Logs nothing about
the rows.
"""

from __future__ import annotations

from typing import Any

from intelligence.boundary.cloud.gate import SHARED_KEY, seal_shared_chunk
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
    meta = dict(metadata) if isinstance(metadata, dict) else {}
    # Only kb_repo records a seal: a row can't bring its own.
    meta.pop(SHARED_KEY, None)
    chunk = KBChunk(
        chunk_id=str(chunk_id),
        content=str(content),
        chunk_type=str(chunk_type),
        source_patient_id=_source(bool(shared), source),
        source_subject_id="" if shared or subject is None else str(subject),
        metadata=meta,
    )
    if shared and chunk.chunk_type == "guideline":
        try:
            sealed = seal_shared_chunk(chunk.chunk_id, chunk.content, _from_repo=True)
        except ValueError:
            # Not a chunk id: usable locally as before, never sent.
            return chunk
        chunk = sealed.as_kb_chunk()
        chunk.metadata = {**meta, SHARED_KEY: sealed}
    return chunk


async def load_chunks(cur: Cursor, patient_id: str | None) -> list[KBChunk]:
    """The chunks a question about patient_id may use: what RLS shows this
    hospital, then only what chunk_visible_to allows for patient_id."""
    await cur.execute(_LOAD, (MAX_CHUNKS,))
    rows = await cur.fetchall()
    chunks = [_chunk(tuple(r)) for r in rows]
    return [c for c in chunks if chunk_visible_to(c, patient_id)]

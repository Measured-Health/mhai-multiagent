"""kb_chunks → KBChunk, under row-level security (plan departure 10).

load_chunks runs on the with_hospital cursor, so RLS returns this
hospital's chunks and the shared reference rows (hospital_id IS NULL,
policy shared_read) and nothing of any other hospital. It never filters
by hospital itself: that is the database's job, and the hospital is the
verified token's.

Three reads, each with its own bound, so no kind of row can crowd out
another (migration 00039):

  - shared rows outside any reference library (the seeded guidelines), at
    most MAX_SHARED_CHUNKS;
  - with a question, a reference library's candidates: the database's
    full-text pre-filter over the active libraries' rows, best first, at
    most LIBRARY_CANDIDATES. A library may hold tens of thousands of rows;
    a job never reads all of them;
  - this hospital's own rows, at most MAX_HOSPITAL_CHUNKS, in id order.

Patient scope is ours (ADR 0004): every path takes patient_id and keeps
only what chunk_visible_to() allows. A shared row becomes a reference chunk
(source_patient_id = REFERENCE_SOURCE), which chunk_visible_to shows to
everyone if it is a guideline. A hospital's own row can never be one: a
reserved source value on it (say "reference") is mapped to the unassigned
source, which no patient sees, and a hospital row with an id in the
reference library namespace (KB#nnnnnnn) is dropped, so a library citation
always means the library's row. Until patient ids exist (departure 7)
patient_id is None, so only shared guidelines come back.

A shared guideline row is also sealed as a SharedChunk (cloud/gate.py) and
the seal recorded in the chunk's metadata["_shared"], so the egress gate
can tell a shared guideline from anything else (design section 4.2). The
local path never reads it. This module is the only caller of
seal_shared_chunk (tests/boundary/cloud/test_imports.py).

A library row carries the vector its library was embedded with and that
embedder's name (KBChunk.embedding, KBChunk.embedder), so the job's
retriever can use it instead of encoding the row again when it runs the
same model.

Never opens a connection or cursor (test_no_direct_db). Logs nothing about
the rows or the question.
"""

from __future__ import annotations

import re
from typing import Any

import numpy as np

from intelligence.boundary.cloud.gate import SHARED_KEY, seal_shared_chunk
from intelligence.boundary.db import Cursor
from intelligence.knowledge_base.kb_builder import (
    REFERENCE_SOURCE,
    RESERVED_PATIENT_IDS,
    UNASSIGNED_SOURCE,
    KBChunk,
    chunk_visible_to,
)

# Indexing per job is fine for a hospital's synthetic data (departure 10);
# the bound keeps one job from pulling an unbounded table into memory.
MAX_HOSPITAL_CHUNKS = 5000
# The seeded shared guidelines (rows in no library).
MAX_SHARED_CHUNKS = 500
# A library's rows a question may use: the pre-filter's best matches.
LIBRARY_CANDIDATES = 100
# The question text the pre-filter reads (the masked question).
MAX_QUESTION_CHARS = 2000
# Ids of reference library rows (00039 kb_library_chunk_id).
LIBRARY_CHUNK_ID = re.compile(r"KB#[1-9][0-9]{6}")
MAX_EMBEDDING_DIM = 4096

# Plain literals (tests/boundary/test_no_direct_db.py). Every read returns
# the same columns: the row, then a library row's vector and embedder.
_LOAD_SHARED = """SELECT chunk_id, hospital_id IS NULL, content, chunk_type, source_patient_id,
    source_subject_id, metadata_json, NULL::real[], NULL::text
FROM kb_chunks WHERE hospital_id IS NULL AND library_id IS NULL ORDER BY id LIMIT %s"""

# Any of the question's words (plainto_tsquery's lexemes joined with OR),
# ranked; ties go to the lower id (a library puts its safety-critical rows
# first).
_LOAD_LIBRARY = """WITH q AS (
    SELECT replace(plainto_tsquery('english', %s)::text, ' & ', ' | ')::tsquery AS query)
SELECT k.chunk_id, k.hospital_id IS NULL, k.content, k.chunk_type, k.source_patient_id,
    k.source_subject_id, k.metadata_json, k.embedding, l.embedder
FROM q, kb_chunks k JOIN reference_libraries l ON l.id = k.library_id AND l.active
WHERE k.hospital_id IS NULL AND k.search_tsv @@ q.query
ORDER BY ts_rank(k.search_tsv, q.query) DESC, k.id LIMIT %s"""

_LOAD_HOSPITAL = """SELECT chunk_id, hospital_id IS NULL, content, chunk_type, source_patient_id,
    source_subject_id, metadata_json, NULL::real[], NULL::text
FROM kb_chunks WHERE hospital_id IS NOT NULL ORDER BY id LIMIT %s"""


def _source(shared: bool, source: str | None) -> str:
    if shared:
        return REFERENCE_SOURCE
    if source is None or source.strip().lower() in RESERVED_PATIENT_IDS:
        return UNASSIGNED_SOURCE
    return source


def _vector(embedding: Any) -> np.ndarray | None:
    """A library row's vector as float32, or None when it isn't one."""
    if not isinstance(embedding, list | tuple) or not 1 <= len(embedding) <= MAX_EMBEDDING_DIM:
        return None
    try:
        v = np.asarray(embedding, dtype=np.float32)
    except (TypeError, ValueError):
        return None
    if v.ndim != 1 or not np.all(np.isfinite(v)):
        return None
    return v


def _chunk(row: tuple[Any, ...]) -> KBChunk:
    chunk_id, shared, content, chunk_type, source, subject, metadata, *extra = row
    embedding, embedder = (extra + [None, None])[:2]
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
    if shared and isinstance(embedder, str) and embedder:
        vector = _vector(embedding)
        if vector is not None:
            chunk.embedding = vector
            chunk.embedder = embedder
    return chunk


async def load_chunks(cur: Cursor, patient_id: str | None, *, question: str = "") -> list[KBChunk]:
    """The chunks a question about patient_id may use: what RLS shows this
    hospital (each kind of row under its own bound, and a library's rows
    only as the question's candidates), then only what chunk_visible_to
    allows for patient_id. Shared chunks come first."""
    await cur.execute(_LOAD_SHARED, (MAX_SHARED_CHUNKS,))
    shared_rows = [tuple(r) for r in await cur.fetchall()]
    text = question[:MAX_QUESTION_CHARS] if isinstance(question, str) else ""
    if text.strip():
        await cur.execute(_LOAD_LIBRARY, (text, LIBRARY_CANDIDATES))
        shared_rows += [tuple(r) for r in await cur.fetchall()]
    await cur.execute(_LOAD_HOSPITAL, (MAX_HOSPITAL_CHUNKS,))
    hospital_rows = [tuple(r) for r in await cur.fetchall()]

    chunks: list[KBChunk] = []
    shared_ids: set[str] = set()
    # Each read keeps only its own kind of row, whatever comes back.
    for r in shared_rows:
        if r[1] is True and str(r[0]) not in shared_ids:
            shared_ids.add(str(r[0]))
            chunks.append(_chunk(r))
    for r in hospital_rows:
        if r[1] is not False:
            continue
        if LIBRARY_CHUNK_ID.fullmatch(str(r[0])):
            # A hospital row can't take a reference library's id.
            continue
        chunks.append(_chunk(r))
    return [c for c in chunks if chunk_visible_to(c, patient_id)]

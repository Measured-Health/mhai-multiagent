"""Load a pinned reference library artefact into kb_chunks (an operator).

    MHAI_REFLIB_DATABASE_URL=postgres://... \\
        uv run python scripts/reflib_load.py medref-1.0.0.jsonl.gz

Run it with the migration DSN (the role that runs `core migrate up`): the
worker and the gateway can't write kb_chunks or reference_libraries. The
artefact's bytes must be the ones models/reference-library.lock pins.

One transaction: the library row, every row (COPY), the previous version of
the same library retired and its rows deleted (their ids are reused), the
new version active. Loading the active version again changes nothing; a
version already loaded with other bytes is refused; loading an earlier
pinned version rolls back to it. Database access lives here, outside the
intelligence package (tests/boundary/test_no_direct_db.py).
"""

from __future__ import annotations

import os
import sys
from collections.abc import Mapping
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from intelligence.knowledge_base import reference_library as reflib

DSN_ENV = "MHAI_REFLIB_DATABASE_URL"


@dataclass(frozen=True)
class Loaded:
    library_id: int
    rows: int
    already: bool


def load(conn: Any, path: Path, pins: Mapping[str, reflib.Pin]) -> Loaded:
    """Load a pinned artefact into kb_chunks in one transaction (psycopg 3
    connection with the migration DSN). See the module docstring."""
    from psycopg.types.json import Jsonb

    header, sha = reflib.verify(path, pins)
    with conn.transaction(), conn.cursor() as cur:
        cur.execute(
            "SELECT id, sha256, active FROM reference_libraries WHERE name = %s AND version = %s",
            (header.name, header.version),
        )
        found = cur.fetchone()
        if found is not None:
            if found[1] != sha:
                raise reflib.LibraryError("version_reloaded_with_other_bytes")
            if found[2]:
                return Loaded(int(found[0]), header.count, already=True)
            cur.execute("DELETE FROM kb_chunks WHERE library_id = %s", (found[0],))
            cur.execute("DELETE FROM reference_libraries WHERE id = %s", (found[0],))
        # The previous versions of this library go first: their ids are reused.
        cur.execute(
            "DELETE FROM kb_chunks WHERE library_id IN "
            "(SELECT id FROM reference_libraries WHERE name = %s)",
            (header.name,),
        )
        cur.execute("UPDATE reference_libraries SET active = false WHERE name = %s", (header.name,))
        cur.execute(
            "INSERT INTO reference_libraries (name, version, sha256, licence, attribution, "
            "embedder, chunk_count, active) VALUES (%s, %s, %s, %s, %s, %s, 0, false) RETURNING id",
            (
                header.name,
                header.version,
                sha,
                header.licence,
                header.attribution,
                header.embedder,
            ),
        )
        lib = int(cur.fetchone()[0])
        n = 0
        with cur.copy(
            "COPY kb_chunks (chunk_id, hospital_id, content, chunk_type, source_patient_id, "
            "source_subject_id, metadata_json, library_id, embedding) FROM STDIN"
        ) as copy:
            for r in reflib.read_rows(path, header):
                copy.write_row(
                    (
                        r.chunk_id,
                        None,
                        r.content,
                        reflib.CHUNK_TYPE,
                        None,
                        None,
                        Jsonb(dict(r.metadata)),
                        lib,
                        list(r.embedding),
                    )
                )
                n += 1
        cur.execute(
            "UPDATE reference_libraries SET active = true, chunk_count = %s WHERE id = %s",
            (n, lib),
        )
    with conn.cursor() as cur:
        cur.execute("ANALYZE kb_chunks")
    return Loaded(lib, n, already=False)


def main(argv: list[str] | None = None) -> int:
    args = sys.argv[1:] if argv is None else argv
    if len(args) != 1:
        print("usage: reflib_load.py ARTEFACT", file=sys.stderr)
        return 2
    dsn = os.environ.get(DSN_ENV)
    if not dsn:
        print(f"set {DSN_ENV} to the migration DSN", file=sys.stderr)
        return 2
    import psycopg

    try:
        pins = reflib.load_lock()
        with psycopg.connect(dsn, autocommit=True) as conn:
            done = load(conn, Path(args[0]), pins)
    except reflib.LibraryError as e:
        print(f"refused: {e}", file=sys.stderr)
        return 1
    state = "already loaded" if done.already else "loaded"
    print(f"{state}: library_id={done.library_id} rows={done.rows}")
    return 0


if __name__ == "__main__":
    sys.exit(main())

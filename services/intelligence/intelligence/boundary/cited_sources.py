"""The reference sources an answer cites: title, link and source credit
(migration 00041; core's ClinicalResult.cited_sources).

Only a shared reference guideline gives a source: a row kb_repo loaded from
kb_chunks with no hospital (REFERENCE_SOURCE, chunk type guideline; a
reference library's row carries title, url, source and attribution in its
metadata). A hospital's or a patient's chunk never does, whatever its
metadata says, so nothing from a patient's record reaches the list.

Every field is checked, never trusted: plain text without control or
format characters, collapsed whitespace, bounded length; a link only if it
is an https URL of the simple shape below, otherwise null. An entry without
a title or source is left out. Logs nothing.
"""

from __future__ import annotations

import re
import unicodedata
from collections.abc import Iterable, Sequence
from typing import Any

from intelligence.knowledge_base.kb_builder import REFERENCE_SOURCE

MAX_SOURCES = 50  # models.MAX_CITED
MAX_TITLE = 300
MAX_SOURCE = 64
MAX_ATTRIBUTION = 600
MAX_URL = 500
# https, a host name, then an optional path and query of URL-safe characters.
_URL = re.compile(
    r"https://(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}"
    r"(?:/[A-Za-z0-9._~%/?=&+-]*)?",
    re.ASCII,
)


def _text(value: Any, limit: int) -> str | None:
    if not isinstance(value, str):
        return None
    cleaned = "".join(" " if unicodedata.category(c)[0] in "CZ" else c for c in value)
    cleaned = " ".join(cleaned.split())
    if not cleaned:
        return None
    return cleaned if len(cleaned) <= limit else cleaned[: limit - 1].rstrip() + "…"


def _url(value: Any) -> str | None:
    if not isinstance(value, str) or len(value) > MAX_URL or not _URL.fullmatch(value):
        return None
    return value


def cited_sources(chunks: Iterable[Any], cited_ids: Sequence[str]) -> tuple[dict[str, Any], ...]:
    """For each cited id (in order, once) that is a shared reference
    guideline with a title and a source: chunk_id, title, url (or None),
    source and attribution (or None)."""
    shared = {
        c.chunk_id: c
        for c in chunks
        if getattr(c, "source_patient_id", None) == REFERENCE_SOURCE
        and getattr(c, "chunk_type", None) == "guideline"
    }
    out: list[dict[str, Any]] = []
    seen: set[str] = set()
    for cid in cited_ids:
        chunk = shared.get(cid)
        if chunk is None or cid in seen:
            continue
        meta = chunk.metadata if isinstance(chunk.metadata, dict) else {}
        title = _text(meta.get("title"), MAX_TITLE)
        source = _text(meta.get("source"), MAX_SOURCE)
        if title is None or source is None:
            continue
        seen.add(cid)
        out.append(
            {
                "chunk_id": cid,
                "title": title,
                "url": _url(meta.get("url")),
                "source": source,
                "attribution": _text(meta.get("attribution"), MAX_ATTRIBUTION),
            }
        )
        if len(out) >= MAX_SOURCES:
            break
    return tuple(out)

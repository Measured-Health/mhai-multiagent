"""A run in Google Drive: ``MHAI models/models/<name>/<run>/``, walked by folder id.

:class:`DriveSource` starts at the shared folder's id and goes down by listing children
(``'<id>' in parents``) and matching names exactly; it never resolves a path string.
It refuses, with ``source_invalid`` and a detail code:

* two items with the same name in one folder (Drive allows it; P2): anywhere in the run,
  and the name being looked for at each level above it;
* a name with ``/``, ``\\``, ``..``, a leading dot, or outside Task 13's allow-list
  (``files.check_file_name``; P1);
* a Google-native type, a shortcut or a subfolder inside the run (P3), or an item in the
  trash;
* a run with more than Task 13's file limit, or a reported size over the cap (before
  anything is downloaded), or a download that runs past its reported size (P4);
* a file with no checksum, or whose bytes don't match Drive's ``sha256Checksum``, or
  ``md5Checksum`` when Drive has no SHA-256 (P5).
* a listing page that doesn't say ``incompleteSearch: false`` (``incomplete``; for
  :func:`drive_check` it is ``drive_scope/incomplete``).

:func:`drive_check` (B22) runs first in every promote job: the account sees exactly one
top-level item, the shared folder, and it can't edit, add to, delete or share it or
anything in it. Every listing asks across all corpora (:data:`ALL_DRIVES`), so items in
shared drives are seen too; every get and download sends ``supportsAllDrives``.

Residual risk: B22 is checked by listing, and some access never shows up in a listing:

* files shared "anyone with the link" or with the whole domain, which the account can
  open but never lists;
* access the credential has through domain-wide delegation or impersonation;
* shares removed from "Shared with me" while the access remains.

TODO(Task 28): the service account must have no domain-wide delegation and only the
``drive.readonly`` scope; check both by hand in the admin console (and record it) when the
credential is set up, since no API call this module makes can prove them.

:class:`HttpDriveApi` is the REST client over a session it is handed (Task 28 makes the
session and its short-lived credentials). It asks only for :data:`FIELDS`. It never logs
or raises with a URL, a header or a response body; errors map to ``source_unavailable``
(network, auth, rate limits, 5xx) or ``source_invalid`` (a malformed response, a missing
item), raised outside the handler so nothing is chained.
"""

from __future__ import annotations

import json
import re
from collections.abc import Callable, Iterable, Mapping
from pathlib import Path
from typing import Any, BinaryIO, Protocol

from intelligence.modelstore import files, names
from intelligence.modelstore.reasons import Refused
from intelligence.modelstore.sources import (
    RemoteFile,
    Sink,
    allowed_name,
    check_listing,
    invalid,
    stream_to,
    unavailable,
)

BASE_URL = "https://www.googleapis.com/drive/v3"
FIELDS = "id,name,mimeType,size,sha256Checksum,md5Checksum,capabilities,trashed"
# ``incompleteSearch`` is a list-level field (the plan's per-file FIELDS are unchanged):
# with ``corpora=allDrives`` Drive may skip some drives and say so only there. Every page
# must carry it, false (review R1).
LIST_FIELDS = f"nextPageToken,incompleteSearch,files({FIELDS})"
DRIVES_FIELDS = "nextPageToken,drives(id)"
FOLDER = "application/vnd.google-apps.folder"
SHORTCUT = "application/vnd.google-apps.shortcut"
NATIVE_PREFIX = "application/vnd.google-apps."
PAGE_SIZE = 1000
MAX_PAGES = 16  # a run holds at most 256 files; a level above it, far fewer pages
MAX_RESPONSE_BYTES = 8 * 1024 * 1024
MAX_PAGE_TOKEN = 4096
CHUNK = 1024 * 1024
TIMEOUT = (10.0, 60.0)
# drive_check walks the whole shared tree; bounded.
MAX_CHECK_ITEMS = 50_000
MAX_CHECK_DEPTH = 6
NO_WRITE = ("canEdit", "canAddChildren", "canDelete", "canShare")
# What the account sees at the top: items shared with it directly, and anything it owns.
TOP_LEVEL_QUERY = "sharedWithMe = true or 'me' in owners"
# Sent on every listing: without them Drive lists only the user corpus and leaves out
# items that live in shared drives (B22, review I1).
ALL_DRIVES: dict[str, str] = {
    "corpora": "allDrives",
    "includeItemsFromAllDrives": "true",
    "supportsAllDrives": "true",
}

# Drive ids are URL-safe base64-like strings. Checked before one is put in a query or a URL.
_ID = re.compile(r"[A-Za-z0-9_-]{10,128}", re.ASCII)
_SHA256 = re.compile(r"[0-9a-f]{64}", re.ASCII)
_MD5 = re.compile(r"[0-9a-f]{32}", re.ASCII)
_SIZE = re.compile(r"(0|[1-9][0-9]{0,15})", re.ASCII)


def check_id(value: object) -> str:
    if not isinstance(value, str) or _ID.fullmatch(value) is None:
        raise invalid("id")
    return value


class DriveApi(Protocol):
    """Drive v3, reduced to what a promote job needs. Items are Drive ``File`` resources
    with :data:`FIELDS` only."""

    def list_children(self, folder_id: str) -> list[dict[str, Any]]: ...

    def get_meta(self, file_id: str) -> dict[str, Any]: ...

    def download(self, file_id: str, dest: BinaryIO | Sink, max_bytes: int) -> None:
        """Stream ``alt=media`` into ``dest.write``; stop past ``max_bytes``."""
        ...

    def about(self) -> list[dict[str, Any]]:
        """Every top-level item the account sees (shared with it, or its own), plus any
        shared drive it is a member of (as ``{"id", "kind": "drive#drive"}``)."""
        ...


# --- the REST client ---------------------------------------------------------


class Response(Protocol):
    status_code: int

    def iter_content(self, chunk_size: int) -> Iterable[bytes]: ...

    def close(self) -> None: ...


class Session(Protocol):
    """A ``requests.Session``-like object that adds its own credentials (Task 28's
    ``AuthorizedSession``). We never read or set its headers."""

    def get(
        self, url: str, *, params: Mapping[str, str], stream: bool, timeout: Any
    ) -> Response: ...


def _status_refusal(code: int) -> Refused | None:
    if 200 <= code < 300:
        return None
    if code == 404:
        return invalid("not_found")
    if code in (401, 403):
        return unavailable("auth")
    if code == 429:
        return unavailable("rate_limited")
    if 500 <= code < 600:
        return unavailable("server")
    return unavailable("http")


class HttpDriveApi:
    def __init__(self, session: Session) -> None:
        self._session = session

    def _get(self, path: str, params: Mapping[str, str]) -> Response:
        resp: Response | None = None
        try:
            resp = self._session.get(
                f"{BASE_URL}{path}", params=params, stream=True, timeout=TIMEOUT
            )
        except Exception:  # never the exception: it may carry the URL or a header
            pass
        if resp is None:
            raise unavailable("network")
        code = getattr(resp, "status_code", None)
        refusal = _status_refusal(code) if isinstance(code, int) else unavailable("http")
        if refusal is not None:
            _close(resp)
            raise refusal
        return resp

    def _json(self, path: str, params: Mapping[str, str]) -> dict[str, Any]:
        resp = self._get(path, params)
        body = bytearray()
        failed: Refused | None = None
        try:
            for chunk in resp.iter_content(CHUNK):
                body += chunk
                if len(body) > MAX_RESPONSE_BYTES:
                    failed = invalid("response_size")
                    break
        except Exception:
            failed = unavailable("network")
        finally:
            _close(resp)
        if failed is not None:
            raise failed
        obj: Any = None
        try:
            obj = json.loads(bytes(body))
        except ValueError:
            pass
        if not isinstance(obj, dict):
            raise invalid("response")
        return obj

    def _list(
        self,
        params: dict[str, str],
        key: str = "files",
        fields: str = LIST_FIELDS,
        page_size: int = PAGE_SIZE,
    ) -> list[dict[str, Any]]:
        out: list[dict[str, Any]] = []
        token: str | None = None
        for _ in range(MAX_PAGES):
            page = dict(params, fields=fields, pageSize=str(page_size))
            if token is not None:
                page["pageToken"] = token
            obj = self._json("/files" if key == "files" else f"/{key}", page)
            if key == "files" and obj.get("incompleteSearch") is not False:
                raise invalid("incomplete")  # a partial view is no view: missing or true
            items = obj.get(key, [])
            if not isinstance(items, list) or not all(isinstance(i, dict) for i in items):
                raise invalid("response")
            out.extend(items)
            nxt = obj.get("nextPageToken")
            if nxt is None:
                return out
            if not isinstance(nxt, str) or not 0 < len(nxt) <= MAX_PAGE_TOKEN:
                raise invalid("response")
            token = nxt
        raise invalid("pages")

    def list_children(self, folder_id: str) -> list[dict[str, Any]]:
        check_id(folder_id)
        return self._list(
            {
                "q": f"'{folder_id}' in parents",
                **ALL_DRIVES,
            }
        )

    def get_meta(self, file_id: str) -> dict[str, Any]:
        check_id(file_id)
        return self._json(f"/files/{file_id}", {"fields": FIELDS, "supportsAllDrives": "true"})

    def download(self, file_id: str, dest: BinaryIO | Sink, max_bytes: int) -> None:
        check_id(file_id)
        resp = self._get(f"/files/{file_id}", {"alt": "media", "supportsAllDrives": "true"})
        failed: Refused | None = None
        count = 0
        try:
            for chunk in resp.iter_content(CHUNK):
                count += len(chunk)
                if count > max_bytes:
                    failed = invalid("overrun")
                    break
                dest.write(chunk)
        except Refused as e:  # the sink's own refusal (an overrun, a write error)
            failed = Refused(e.reason, detail_code=e.detail_code)
        except Exception:
            failed = unavailable("network")
        finally:
            _close(resp)
        if failed is not None:
            raise failed

    def about(self) -> list[dict[str, Any]]:
        top = self._list({"q": TOP_LEVEL_QUERY, **ALL_DRIVES})
        drives = self._list({}, key="drives", fields=DRIVES_FIELDS, page_size=100)
        return top + [{"id": d.get("id"), "kind": "drive#drive"} for d in drives]


def _close(resp: Any) -> None:
    try:
        resp.close()
    except Exception:
        pass


# --- checks on items ---------------------------------------------------------


def _size(item: Mapping[str, Any]) -> int:
    raw = item.get("size")
    if not isinstance(raw, str) or _SIZE.fullmatch(raw) is None:
        raise invalid("size")
    return int(raw)


def _not_trashed(item: Mapping[str, Any]) -> None:
    if item.get("trashed") is not False:
        raise invalid("trashed")


def _find(items: list[dict[str, Any]], name: str) -> dict[str, Any]:
    """The one folder called ``name`` among ``items``."""
    matches = [i for i in items if i.get("name") == name]
    if not matches:
        raise invalid("missing")
    if len(matches) > 1:
        raise invalid("duplicate")
    item = matches[0]
    _not_trashed(item)
    if item.get("mimeType") != FOLDER:
        raise invalid("not_folder")
    check_id(item.get("id"))
    return item


def _remote_file(item: Mapping[str, Any]) -> RemoteFile:
    _not_trashed(item)
    mime = item.get("mimeType")
    if mime == FOLDER:
        raise invalid("subfolder")
    if mime == SHORTCUT:
        raise invalid("shortcut")
    if not isinstance(mime, str) or mime.startswith(NATIVE_PREFIX):
        raise invalid("native")
    name = allowed_name(item.get("name"))
    file_id = check_id(item.get("id"))
    sha = item.get("sha256Checksum")
    md5 = item.get("md5Checksum")
    if sha is not None and (not isinstance(sha, str) or _SHA256.fullmatch(sha) is None):
        raise invalid("checksum_format")
    if md5 is not None and (not isinstance(md5, str) or _MD5.fullmatch(md5) is None):
        raise invalid("checksum_format")
    if sha is None and md5 is None:
        raise invalid("no_checksum")
    return RemoteFile(name, _size(item), sha, md5, file_id)


class DriveSource:
    def __init__(self, api: DriveApi, folder_id: str, limits: files.Limits | None = None):
        self._api = api
        self._folder_id = check_id(folder_id)
        self._limits = limits or files.Limits()

    def _run_folder(self, name: str, run: str) -> str:
        folder = self._folder_id
        for part in ("models", name, run):
            folder = check_id(_find(self._api.list_children(folder), part)["id"])
        return folder

    def list_run(self, name: str, run: str) -> list[RemoteFile]:
        names.check_name(name)
        names.check_run_id(run)
        items = self._api.list_children(self._run_folder(name, run))
        if len(items) > self._limits.max_files:
            raise invalid("count")
        seen: set[object] = set()
        for item in items:  # duplicates first: Drive allows two items with one name (P2)
            if item.get("name") in seen:
                raise invalid("duplicate")
            seen.add(item.get("name"))
        return check_listing([_remote_file(i) for i in items], self._limits)

    def fetch(self, file: RemoteFile, dest: Path, max_bytes: int) -> str:
        file_id = check_id(file.ref)
        # The item now must still be the one listed: same name, type, size and checksums.
        now = _remote_file(self._api.get_meta(file_id))
        if now != file:
            raise invalid("changed")
        return stream_to(
            file, dest, max_bytes, lambda sink: self._api.download(file_id, sink, file.size)
        )


# --- B22 ---------------------------------------------------------------------


def _read_only(item: Mapping[str, Any]) -> None:
    caps = item.get("capabilities")
    if not isinstance(caps, dict):
        raise Refused("drive_scope", detail_code="capabilities")
    for cap in NO_WRITE:
        if caps.get(cap) is not False:
            raise Refused("drive_scope", detail_code="writable")


def _scoped[T](fn: Callable[..., T], *args: Any) -> T:
    """``fn(*args)``, with an incomplete listing turned into ``drive_scope/incomplete``: for
    B22 a partial view of what the account sees is a failed check."""
    try:
        return fn(*args)
    except Refused as e:
        if (e.reason, e.detail_code) != ("source_invalid", "incomplete"):
            raise
    raise Refused("drive_scope", detail_code="incomplete")  # outside the handler: no chain


def drive_check(api: DriveApi, folder_id: str) -> None:
    """B22: the account sees exactly one top-level item, ``folder_id``, a folder; and
    ``canEdit``, ``canAddChildren``, ``canDelete`` and ``canShare`` are all false on it and
    on every item under it. Anything else is ``drive_scope``."""
    check_id(folder_id)
    top = _scoped(api.about)
    if len(top) != 1:
        raise Refused("drive_scope", detail_code="top_level_count")
    root = top[0]
    if (
        root.get("id") != folder_id
        or root.get("mimeType") != FOLDER
        or root.get("trashed") is not False
    ):
        raise Refused("drive_scope", detail_code="top_level")
    _read_only(root)
    _read_only(_scoped(api.get_meta, folder_id))
    seen = 0
    level = [folder_id]
    for _ in range(MAX_CHECK_DEPTH + 1):
        nxt: list[str] = []
        for fid in level:
            for item in _scoped(api.list_children, fid):
                seen += 1
                if seen > MAX_CHECK_ITEMS:
                    raise Refused("drive_scope", detail_code="too_many")
                _read_only(item)
                if item.get("mimeType") == FOLDER:
                    nxt.append(check_id(item.get("id")))
        if not nxt:
            return
        level = nxt
    raise Refused("drive_scope", detail_code="too_deep")


__all__ = [
    "BASE_URL",
    "FIELDS",
    "LIST_FIELDS",
    "DriveApi",
    "DriveSource",
    "HttpDriveApi",
    "check_id",
    "drive_check",
]

"""A fake Google Drive v3 behind a fake HTTP session, for ``HttpDriveApi`` and ``DriveSource``.

:class:`FakeDriveSession` answers the GET requests ``HttpDriveApi`` makes with the Drive v3
response shapes (``files.list``: ``{"kind", "files": [...], "nextPageToken"}``;
``files.get``: a ``File`` resource; ``alt=media``: the bytes; ``drives.list``), returning
only the fields asked for. It pages listings by :attr:`page_size` so pagination is used.
It carries a bearer token in its headers, as an ``AuthorizedSession`` does, so a test can
check that it never reaches a log or an error. Nothing here opens a socket. All content is
synthetic.
"""

from __future__ import annotations

import hashlib
import json
import re
import secrets
from collections.abc import Iterator, Mapping
from dataclasses import dataclass, field
from typing import Any

BASE = "https://www.googleapis.com/drive/v3"
FOLDER = "application/vnd.google-apps.folder"
READ_ONLY = {
    "canEdit": False,
    "canAddChildren": False,
    "canDelete": False,
    "canShare": False,
    "canDownload": True,
}
FILE_FIELDS = "id,name,mimeType,size,sha256Checksum,md5Checksum,capabilities,trashed"
LIST_FIELDS = f"nextPageToken,files({FILE_FIELDS})"
ALL_DRIVES = {
    "corpora": "allDrives",
    "includeItemsFromAllDrives": "true",
    "supportsAllDrives": "true",
}
_PARENTS = re.compile(r"'([A-Za-z0-9_-]+)' in parents")


def new_id() -> str:
    return "fk" + secrets.token_hex(12)


@dataclass
class Item:
    id: str
    name: str
    mime: str
    parent: str | None
    content: bytes = b""
    trashed: bool = False
    capabilities: dict[str, bool] = field(default_factory=lambda: dict(READ_ONLY))
    sha256: str | None = None
    md5: str | None = None
    size: str | None = None
    with_sha256: bool = True
    with_md5: bool = True

    def resource(self) -> dict[str, Any]:
        out: dict[str, Any] = {
            "id": self.id,
            "name": self.name,
            "mimeType": self.mime,
            "trashed": self.trashed,
            "capabilities": dict(self.capabilities),
        }
        if self.mime != FOLDER and not self.mime.startswith("application/vnd.google-apps."):
            out["size"] = self.size if self.size is not None else str(len(self.content))
            if self.with_md5:
                out["md5Checksum"] = self.md5 or hashlib.md5(self.content).hexdigest()
            if self.with_sha256:
                out["sha256Checksum"] = self.sha256 or hashlib.sha256(self.content).hexdigest()
        return out


class FakeResponse:
    def __init__(self, status_code: int, body: bytes, chunk: int | None = None) -> None:
        self.status_code = status_code
        self._body = body
        self._chunk = chunk
        self.closed = False

    def iter_content(self, chunk_size: int) -> Iterator[bytes]:
        step = self._chunk or chunk_size
        for i in range(0, len(self._body), step):
            yield self._body[i : i + step]

    def close(self) -> None:
        self.closed = True


@dataclass
class Request:
    url: str
    params: dict[str, str]


class FakeDriveSession:
    def __init__(self, page_size: int = 2) -> None:
        self.token = "ya29.fake-" + secrets.token_hex(16)
        self.headers = {"Authorization": f"Bearer {self.token}"}
        self.items: dict[str, Item] = {}
        self.top_level: list[str] = []
        # Items shared with the account that live in a shared drive (file-level shares,
        # no membership): Drive lists them only with ALL_DRIVES.
        self.shared_drive_top_level: list[str] = []
        self.drives: list[str] = []
        self.page_size = page_size
        self.requests: list[Request] = []
        self.status: dict[str, int] = {}  # file id -> HTTP status to return
        self.serve: dict[str, bytes] = {}  # file id -> bytes served instead of its content
        self.raise_on: set[str] = set()  # file ids whose request raises (a network error)
        self.endless_pages = False

    # --- building the tree

    def add(
        self, name: str, mime: str, parent: str | None, content: bytes = b"", **kw: Any
    ) -> Item:
        item = Item(new_id(), name, mime, parent, content, **kw)
        self.items[item.id] = item
        return item

    def folder(self, name: str, parent: str | None, **kw: Any) -> Item:
        return self.add(name, FOLDER, parent, **kw)

    def file(self, name: str, parent: str | None, content: bytes, **kw: Any) -> Item:
        return self.add(name, "application/octet-stream", parent, content, **kw)

    def shared_root(self, name: str = "MHAI models") -> Item:
        root = self.folder(name, None)
        self.top_level.append(root.id)
        return root

    # --- HTTP

    def get(
        self, url: str, *, params: Mapping[str, str], stream: bool, timeout: Any
    ) -> FakeResponse:
        assert url.startswith(BASE + "/"), "only Drive v3 is served"
        assert stream is True and timeout is not None
        self.requests.append(Request(url, dict(params)))
        path = url[len(BASE) :]
        if path == "/files":
            return self._list(dict(params))
        if path == "/drives":
            assert params["fields"] == "nextPageToken,drives(id)"
            return self._json(
                {"kind": "drive#driveList", "drives": [{"id": d} for d in self.drives]}
            )
        m = re.fullmatch(r"/files/([A-Za-z0-9_-]+)", path)
        assert m is not None, "unexpected path"
        fid = m.group(1)
        if fid in self.raise_on:
            raise ConnectionError(f"connection failed for {url} with {self.headers}")
        if fid in self.status:
            return FakeResponse(
                self.status[fid], f'{{"error": "denied for {self.token}"}}'.encode()
            )
        assert params.get("supportsAllDrives") == "true", "every get sends supportsAllDrives"
        item = self.items.get(fid)
        if item is None:
            return FakeResponse(404, b'{"error": {"code": 404}}')
        if params.get("alt") == "media":
            return FakeResponse(200, self.serve.get(fid, item.content), chunk=7)
        assert params["fields"] == FILE_FIELDS, "fields must be exactly the plan's list"
        return self._json(item.resource())

    def _json(self, obj: Any) -> FakeResponse:
        return FakeResponse(200, json.dumps(obj).encode())

    def _list(self, params: dict[str, str]) -> FakeResponse:
        assert params["fields"] == LIST_FIELDS, "fields must be exactly the plan's list"
        for key, value in ALL_DRIVES.items():  # else Drive leaves out shared-drive items
            assert params.get(key) == value, f"a listing without {key}={value}"
        q = params["q"]
        if q == "sharedWithMe = true or 'me' in owners":
            found = [self.items[i] for i in self.top_level + self.shared_drive_top_level]
        else:
            m = _PARENTS.fullmatch(q)
            assert m is not None, "only by parent id"
            parent = m.group(1)
            if parent in self.status:
                return FakeResponse(self.status[parent], f"denied {self.token}".encode())
            found = [i for i in self.items.values() if i.parent == parent]
        start = int(params.get("pageToken", "0").removeprefix("p")) if "pageToken" in params else 0
        page = found[start : start + self.page_size]
        body: dict[str, Any] = {"kind": "drive#fileList", "files": [i.resource() for i in page]}
        if start + self.page_size < len(found) or self.endless_pages:
            body["nextPageToken"] = f"p{start + self.page_size}"
        return self._json(body)

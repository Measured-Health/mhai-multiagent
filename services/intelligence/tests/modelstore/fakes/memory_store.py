"""An in-memory :class:`~intelligence.modelstore.store.ObjectStore` with versions.

It checks keys with the store's own layout check, refuses a second create of a key
(``version_exists``) as the conditional PUT does, and records every call in order, so a
test can see what was written, in which order, and that nothing was written at all.

Two hooks drive the failure paths:

* ``fail``: called with the key before each ``put_new``; if it returns an exception, that
  is raised and nothing is stored (a crash, a lost connection);
* ``lost_reply``: keys whose PUT succeeds on the server but whose reply never reaches the
  client, so the SDK's retry finds the object there and the call reports
  ``version_exists`` (Task 16 M5).
"""

from __future__ import annotations

import hashlib
import uuid
from collections.abc import Callable
from datetime import datetime
from pathlib import Path

from intelligence.modelstore import store
from intelligence.modelstore.reasons import Refused
from intelligence.modelstore.store import ObjectInfo


class MemoryStore:
    def __init__(
        self,
        *,
        fail: Callable[[str], BaseException | None] | None = None,
        lost_reply: frozenset[str] = frozenset(),
    ) -> None:
        self.objects: dict[str, list[tuple[str, bytes]]] = {}
        self.calls: list[tuple[str, str]] = []
        self.fail = fail
        self.lost_reply = set(lost_reply)

    @property
    def puts(self) -> list[str]:
        return [key for op, key in self.calls if op == "put"]

    def put_new(self, key: str, data: bytes | Path, retention_days: int) -> str:
        store._check_key(key)
        self.calls.append(("put", key))
        if self.fail is not None:
            exc = self.fail(key)
            if exc is not None:
                raise exc
        if key in self.objects:
            raise Refused("version_exists", detail_code="exists")
        body = data if isinstance(data, bytes) else Path(data).read_bytes()
        if len(body) > store.MAX_PUT_BYTES:
            raise Refused("too_large")
        vid = str(uuid.uuid4())
        self.objects[key] = [(vid, body)]
        if key in self.lost_reply:
            self.lost_reply.discard(key)
            raise Refused("version_exists", detail_code="exists")  # the retry's 412
        return vid

    def shadow(self, key: str, body: bytes) -> str:
        """An unconditional second PUT by someone else: a newer version of ``key``."""
        vid = str(uuid.uuid4())
        self.objects.setdefault(key, []).append((vid, body))
        return vid

    def _find(self, key: str, version_id: str | None) -> tuple[str, bytes]:
        found = self.objects.get(key, [])
        if version_id is None and found:
            return found[-1]
        for vid, body in found:
            if vid == version_id:
                return vid, body
        raise Refused("not_found", detail_code="missing")

    def get(self, key: str, version_id: str | None = None, *, max_bytes: int) -> bytes:
        self.calls.append(("get", key))
        _, body = self._find(key, version_id)
        if len(body) > max_bytes:
            raise Refused("too_large")
        return body

    def versions(self, key: str) -> list[str]:
        self.calls.append(("versions", key))
        return [vid for vid, _ in reversed(self.objects.get(key, []))]

    def head(self, key: str, version_id: str | None = None) -> ObjectInfo:
        self.calls.append(("head", key))
        vid, body = self._find(key, version_id)
        return ObjectInfo(vid, len(body), hashlib.sha256(body).hexdigest(), "COMPLIANCE", None)

    def list(self, prefix: str) -> list[str]:
        self.calls.append(("list", prefix))
        return sorted(k for k in self.objects if k.startswith(prefix))

    def extend_retention(self, key: str, version_id: str, days: int) -> datetime:
        raise Refused("not_allowed_here")


__all__ = ["MemoryStore"]

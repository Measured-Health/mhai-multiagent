"""The pinned models a service needs, verified and warmed up at start-up (plan Task 23,
design sections 7.4 and 7.5).

:class:`LoadSettings` reads ``models.lock`` **once**, at start-up, and keeps the pins of
the roles the service needs (``INTELLIGENCE_MODEL_ROLES``); a role the lock doesn't pin
is ``lock_invalid``. A change to the lock file afterwards changes nothing until restart.

:meth:`PinnedModels.load` takes each required role in turn:

1. :func:`~intelligence.modelstore.verify.verify_for_load`, the verifier's only entry
   point for loading: lock hash, signature, identity, gate, every file and the sign-off
   (required everywhere but development, where both ``MHAI_MODELS_ENV=development`` and
   ``APP_ENV=development`` are needed; an unset ``APP_ENV`` is production). It never
   calls ``verify_pin`` and never chooses whether a sign-off is needed.
2. :func:`warm_up`: ``warmup.json`` is read by the ``store_version_id`` the verified
   manifest recorded (never latest), must have the SHA-256 the manifest recorded, and
   parses strictly (canonical JSON, exactly ``page_sha256``, ``output_sha256``,
   ``engine`` and ``engine_version``). The runtime comes only from ``config.json``'s
   architecture, as in promote (:func:`~intelligence.modelstore.promote.
   choose_extractor`): in phase 0 the only in-process runtime is the tiny extractor. A
   role with no in-process runtime (a vision-language model served by vLLM) is
   ``no_runtime``, so it is never ready. TODO(GPU task): load a VLM role once its runtime
   can prove it serves the verified files. The extractor runs on the fixed warm-up page
   (whose SHA-256 must be the document's ``page_sha256``), and its output's SHA-256 must
   be ``output_sha256`` (``warmup_mismatch``). When the document records the engine's
   version, the runtime's must be the same (``warmup_mismatch``/``engine_version``).
   The page is ``INTELLIGENCE_WARMUP_PAGE`` in the service (the repository's
   ``testdata/models/warmup/page.png`` by default); a page that isn't there is
   ``warmup_page_missing``, told apart from a model that doesn't reproduce.

Any refusal is a :class:`PinRefused` naming the role and a fixed reason; nothing falls
back to another version, an unverified cache or the internet. :meth:`PinnedModels.
versions` reports ``<name>@<version>#<first 12 hex of the manifest SHA-256>``
(:func:`~intelligence.modelstore.verify.version_string`) from what was verified and
loaded, never from the lock file.

Logs carry roles, names, versions, hashes and reason codes only: never a path, URL, key,
credential or document content.
"""

from __future__ import annotations

import hashlib
import logging
import re
from collections.abc import Mapping, Sequence
from dataclasses import dataclass, field
from pathlib import Path
from types import MappingProxyType
from typing import Any

from intelligence.modelstore import (
    canonical,
    files,
    lock,
    names,
    promote,
    signing,
    store,
    verify,
)
from intelligence.modelstore.reasons import REASONS, Refused

log = logging.getLogger(__name__)

WARMUP_DOC = "warmup.json"
MAX_WARMUP_BYTES = 4 * 1024
MAX_WARMUP_PAGE_BYTES = promote.MAX_WARMUP_PAGE_BYTES
WARMUP_KEYS = frozenset({"page_sha256", "output_sha256", "engine", "engine_version"})
# A refusal that isn't a model store reason (an unexpected error): the one code outside
# REASONS a PinRefused may carry.
INTERNAL = "internal"
_SHA256 = re.compile(r"[0-9a-f]{64}", re.ASCII)
_ENGINE = re.compile(r"[a-z][a-z0-9_.-]{0,31}", re.ASCII)
_ENGINE_VERSION = re.compile(r"[A-Za-z0-9_.+-]{1,64}", re.ASCII)


class PinRefused(Exception):
    """A required role could not be loaded. ``str()`` is ``<role>:<reason>``; the reason
    is one of :data:`~intelligence.modelstore.reasons.REASONS` or ``internal``."""

    def __init__(self, role: str, reason: str, detail_code: str | None = None) -> None:
        if reason not in REASONS and reason != INTERNAL:
            raise ValueError("unknown refusal reason")
        super().__init__(f"{role}:{reason}")
        self.role = role
        self.reason = reason
        self.detail_code = detail_code


@dataclass(frozen=True)
class LoadSettings:
    """The pins of the roles a service needs, read from the lock once."""

    pins: Mapping[str, lock.Pin]
    cache_dir: Path
    warmup_page: Path = promote.WARMUP_PAGE
    deadline_s: float = promote.DEFAULT_DEADLINE_S
    roles: tuple[str, ...] = field(init=False)

    def __post_init__(self) -> None:
        for role, pin in self.pins.items():
            names.check_role(role)
            if not isinstance(pin, lock.Pin):
                raise Refused("lock_invalid", detail_code="entry")
        object.__setattr__(self, "pins", MappingProxyType(dict(sorted(self.pins.items()))))
        object.__setattr__(self, "roles", tuple(self.pins))

    @classmethod
    def from_lock(
        cls,
        roles: Sequence[str],
        lock_path: Path | str,
        cache_dir: Path | str,
        *,
        warmup_page: Path | str | None = None,
    ) -> LoadSettings:
        """Read ``lock_path`` (once) and keep the pins of ``roles``. A role the lock
        doesn't pin is ``lock_invalid``/``missing_role``."""
        checked = [names.check_role(r) for r in roles]
        if len(set(checked)) != len(checked):
            raise Refused("lock_invalid", detail_code="duplicate_role")
        pinned = lock.read(lock_path)
        missing = [r for r in checked if r not in pinned.models]
        if missing:
            raise Refused("lock_invalid", detail_code="missing_role")
        return cls(
            pins={r: pinned.models[r] for r in checked},
            cache_dir=Path(cache_dir),
            warmup_page=Path(warmup_page) if warmup_page is not None else promote.WARMUP_PAGE,
        )


class PinnedModels:
    """The verified, warmed-up models of every required role."""

    __slots__ = ("_models",)

    def __init__(self, models: Mapping[str, verify.VerifiedModel]) -> None:
        self._models = MappingProxyType(dict(sorted(models.items())))

    @classmethod
    def load(
        cls, settings: LoadSettings, source: verify.ReadStore, policy: signing.TrustPolicy
    ) -> PinnedModels:
        """Verify and warm up every role in ``settings``; see the module docstring.
        Raises :class:`PinRefused` for the first role that fails."""
        loaded: dict[str, verify.VerifiedModel] = {}
        for role, pin in settings.pins.items():
            loaded[role] = _load_one(settings, source, policy, role, pin)
        out = cls(loaded)
        log.info(
            "model.loaded roles=%s names=%s versions=%s sha256=%s",
            ",".join(out._models),
            ",".join(m.name for m in out._models.values()),
            ",".join(m.version for m in out._models.values()),
            ",".join(m.manifest_sha256 for m in out._models.values()),
        )
        return out

    @property
    def roles(self) -> tuple[str, ...]:
        return tuple(self._models)

    @property
    def models(self) -> Mapping[str, verify.VerifiedModel]:
        return self._models

    def versions(self) -> dict[str, str]:
        """``{role: "<name>@<version>#<hash12>"}``, from what was verified."""
        return {role: m.version_string for role, m in self._models.items()}

    def path(self, role: str) -> Path:
        """The verified, read-only directory of ``role`` (``KeyError`` if not pinned)."""
        return self._models[role].path

    def __repr__(self) -> str:
        return f"PinnedModels({self.versions()!r})"


def _load_one(
    settings: LoadSettings,
    source: verify.ReadStore,
    policy: signing.TrustPolicy,
    role: str,
    pin: lock.Pin,
) -> verify.VerifiedModel:
    failed: tuple[str, str | None] | None = None
    vm: verify.VerifiedModel | None = None
    try:
        vm = verify.verify_for_load(source, role, pin, policy, settings.cache_dir)
        warm_up(source, vm, settings.warmup_page, deadline_s=settings.deadline_s)
    except Refused as e:
        failed = (e.reason, e.detail_code)
    except Exception as e:  # an unexpected error: a fixed code, the type name logged only
        log.warning("model.load_error role=%s error=%s", role, type(e).__name__)
        failed = (INTERNAL, None)
    if failed is not None:
        raise PinRefused(role, failed[0], failed[1])
    assert vm is not None
    return vm


# ---------------------------------------------------------------- warm-up


def _warmup_doc(source: verify.ReadStore, m: Any) -> dict[str, Any]:
    rec = m.documents[WARMUP_DOC]
    key = store.run_doc_key(m.name, m.version, m.run.id, WARMUP_DOC)
    raw = source.get(key, rec.store_version_id, max_bytes=MAX_WARMUP_BYTES)
    if hashlib.sha256(raw).hexdigest() != rec.sha256:
        raise Refused("hash_mismatch", detail_code="warmup")
    doc = canonical.loads_strict(raw, MAX_WARMUP_BYTES)
    if canonical.dumps(doc) != raw:
        raise Refused("not_canonical", detail_code="warmup")
    if not isinstance(doc, dict) or set(doc) != WARMUP_KEYS:
        raise Refused("warmup_mismatch", detail_code="document")
    version = doc["engine_version"]
    if (
        not isinstance(doc["page_sha256"], str)
        or _SHA256.fullmatch(doc["page_sha256"]) is None
        or not isinstance(doc["output_sha256"], str)
        or _SHA256.fullmatch(doc["output_sha256"]) is None
        or not isinstance(doc["engine"], str)
        or _ENGINE.fullmatch(doc["engine"]) is None
        or not (
            version is None
            or (isinstance(version, str) and _ENGINE_VERSION.fullmatch(version) is not None)
        )
    ):
        raise Refused("warmup_mismatch", detail_code="document")
    return doc


def check_warmup_page(page: Path | str) -> bytes:
    """The warm-up page's bytes: a regular file (never through a link) of at most
    :data:`MAX_WARMUP_PAGE_BYTES`, else ``warmup_page_missing``."""
    failed = False
    raw = b""
    try:
        raw = files.read_regular(page, MAX_WARMUP_PAGE_BYTES)
    except (Refused, OSError):
        failed = True
    if failed or not raw:
        raise Refused("warmup_page_missing")
    return raw


def warm_up(
    source: verify.ReadStore,
    vm: verify.VerifiedModel,
    page: Path,
    *,
    deadline_s: float = promote.DEFAULT_DEADLINE_S,
) -> None:
    """Run the role's in-process runtime on the warm-up page and compare with the
    verified manifest's ``warmup.json``; see the module docstring."""
    check_warmup_page(page)
    expected = _warmup_doc(source, vm.manifest)
    # The runtime from config.json's architecture only; no runtime URL in-process, so a
    # VLM is no_runtime here. TODO(GPU task): a VLM role's runtime and its warm-up.
    chosen = promote.choose_extractor(vm.path, vm.name, vm.role, vm.version, None)
    if chosen.engine != expected["engine"]:
        raise Refused("warmup_mismatch", detail_code="engine")
    if expected["engine_version"] is not None and (
        chosen.engine_version != expected["engine_version"]
    ):
        raise Refused("warmup_mismatch", detail_code="engine_version")
    got = canonical.loads_strict(promote.warmup(chosen, page, deadline_s=deadline_s), 4096)
    if got["page_sha256"] != expected["page_sha256"]:
        raise Refused("warmup_mismatch", detail_code="page")
    if got["output_sha256"] != expected["output_sha256"]:
        raise Refused("warmup_mismatch", detail_code="output")
    log.info(
        "model.warmed_up role=%s name=%s version=%s sha256=%s engine=%s",
        vm.role,
        vm.name,
        vm.version,
        vm.manifest_sha256,
        chosen.engine,
    )


__all__ = [
    "INTERNAL",
    "LoadSettings",
    "PinRefused",
    "PinnedModels",
    "check_warmup_page",
    "warm_up",
]

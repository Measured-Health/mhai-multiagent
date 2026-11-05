"""A Hugging Face model repo at a pinned commit (base models, §7.3).

* ``revision`` must be a full commit id, exactly 40 lowercase hex (``revision_unpinned``,
  P23): never a branch or a tag, which can move.
* The listing at that commit may hold only allow-listed names (Task 13's
  ``files.check_file_name``), flat, plus README, licence and ``.gitattributes`` files,
  which are ignored and never fetched. Anything else (a ``.bin``, code, a subfolder)
  refuses the whole repo (``source_invalid``).
* Files are downloaded one by one with ``hf_hub_download(..., revision=revision)`` into a
  cache directory inside the caller's :class:`~intelligence.modelstore.sources.Scratch`
  (never ``~/.cache``; a cache dir outside the scratch root, or inside its run dir, is
  refused), with no token (``token=False``). For the call only, :func:`hub_settings`
  turns off telemetry, implicit tokens and Xet (``HF_HUB_DISABLE_XET``: Xet keeps its own
  chunk cache, by default under ``HF_HOME``), and points ``HF_HOME`` and ``HF_XET_CACHE``
  inside the scratch cache as well, should anything still read them.
* The endpoint is pinned: both calls go to :data:`ENDPOINT` (``https://huggingface.co``),
  whatever ``HF_ENDPOINT`` says; a mirror is never used.
* The cached file is copied into the scratch run directory with a byte cap and checked
  against this source's own last listing (the caller's :class:`RemoteFile` must equal the
  listed one): an LFS file's SHA-256, else the git blob id. The cached copy is removed.

``files.check_run`` still runs on the scratch run directory afterwards (Task 19).
The environment change is process-wide while a call runs: don't fetch from two threads.
"""

from __future__ import annotations

import os
import re
import stat
from collections.abc import Iterator
from contextlib import contextmanager
from pathlib import Path
from typing import Any

import huggingface_hub
from huggingface_hub import constants as hf_constants
from huggingface_hub import errors as hf_errors
from huggingface_hub import hf_api

from intelligence.modelstore import files, names
from intelligence.modelstore.reasons import Refused
from intelligence.modelstore.sources import (
    RemoteFile,
    Scratch,
    allowed_name,
    check_listing,
    invalid,
    read_into,
    stream_to,
    unavailable,
    within,
)

_REVISION = re.compile(r"[0-9a-f]{40}", re.ASCII)
_REPO = re.compile(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,95}/[A-Za-z0-9][A-Za-z0-9_.-]{0,95}", re.ASCII)
_SHA256 = re.compile(r"[0-9a-f]{64}", re.ASCII)
_GIT_OID = re.compile(r"[0-9a-f]{40}", re.ASCII)
MAX_LISTED = 1024
IGNORED: frozenset[str] = frozenset(
    {
        ".gitattributes",
        "readme",
        "readme.md",
        "readme.txt",
        "license",
        "license.md",
        "license.txt",
        "licence",
        "licence.md",
        "licence.txt",
    }
)
ENDPOINT = "https://huggingface.co"
_FLAGS = ("HF_HUB_DISABLE_TELEMETRY", "HF_HUB_DISABLE_IMPLICIT_TOKEN", "HF_HUB_DISABLE_XET")
_DIRS = ("HF_HOME", "HF_XET_CACHE")
_NOT_FOUND = (
    hf_errors.RepositoryNotFoundError,
    hf_errors.RevisionNotFoundError,
    hf_errors.EntryNotFoundError,
)
_CLOEXEC = getattr(os, "O_CLOEXEC", 0)


@contextmanager
def hub_settings(cache: str) -> Iterator[None]:
    """For one call: telemetry, implicit tokens and Xet off, and ``HF_HOME`` and
    ``HF_XET_CACHE`` inside ``cache``; then everything put back as it was.
    ``huggingface_hub`` reads these at import, so its constants are set too."""
    saved_env = {k: os.environ.get(k) for k in _FLAGS + _DIRS}
    saved = (
        hf_constants.HF_HUB_DISABLE_TELEMETRY,
        hf_constants.HF_HUB_DISABLE_IMPLICIT_TOKEN,
        hf_constants.HF_HUB_DISABLE_XET,
        hf_constants.HF_HOME,
        hf_constants.HF_XET_CACHE,
    )
    home = os.path.join(cache, "home")
    xet = os.path.join(cache, "xet")
    os.environ.update({k: "1" for k in _FLAGS})
    os.environ["HF_HOME"] = home
    os.environ["HF_XET_CACHE"] = xet
    hf_constants.HF_HUB_DISABLE_TELEMETRY = True
    hf_constants.HF_HUB_DISABLE_IMPLICIT_TOKEN = True
    hf_constants.HF_HUB_DISABLE_XET = True
    hf_constants.HF_HOME = home
    hf_constants.HF_XET_CACHE = xet
    try:
        yield
    finally:
        for k, v in saved_env.items():
            if v is None:
                os.environ.pop(k, None)
            else:
                os.environ[k] = v
        (
            hf_constants.HF_HUB_DISABLE_TELEMETRY,
            hf_constants.HF_HUB_DISABLE_IMPLICIT_TOKEN,
            hf_constants.HF_HUB_DISABLE_XET,
            hf_constants.HF_HOME,
            hf_constants.HF_XET_CACHE,
        ) = saved


def check_revision(revision: object) -> str:
    if not isinstance(revision, str) or _REVISION.fullmatch(revision) is None:
        raise Refused("revision_unpinned")
    return revision


def _check_cache(scratch: Scratch, cache_dir: Path | str) -> str:
    """The real path of ``cache_dir``, which must be a folder inside ``scratch.root`` (a
    0700 folder, not a link) and not the run folder or anything in it."""
    if not isinstance(scratch, Scratch):
        raise invalid("cache")
    root = os.path.realpath(scratch.root)
    run = os.path.realpath(scratch.run)
    cache = os.path.realpath(cache_dir)
    try:
        st = os.lstat(scratch.root)
    except OSError:
        st = None
    if (
        st is None
        or not stat.S_ISDIR(st.st_mode)
        or stat.S_IMODE(st.st_mode) != 0o700
        or not os.path.isdir(cache)
        or cache == root
        or not within(cache, root)
        or within(cache, run)
    ):
        raise invalid("cache")
    return cache


def _ignored(path: str) -> bool:
    return "/" not in path and path.lower() in IGNORED


class HFSource:
    def __init__(
        self,
        repo: str,
        revision: str,
        *,
        scratch: Scratch,
        cache_dir: Path | str | None = None,
        limits: files.Limits | None = None,
    ) -> None:
        self._revision = check_revision(revision)
        if not isinstance(repo, str) or _REPO.fullmatch(repo) is None or ".." in repo:
            raise invalid("repo")
        self._repo = repo
        self._cache = _check_cache(scratch, scratch.cache if cache_dir is None else cache_dir)
        self._limits = limits or files.Limits()
        self._oids: dict[str, str] = {}
        self._listed: dict[str, RemoteFile] = {}

    def list_run(self, name: str, run: str) -> list[RemoteFile]:
        """The repo's files at the pinned commit. ``name`` and ``run`` are the promote
        step's names for it, checked here; the repo and revision fix what is read."""
        names.check_name(name)
        names.check_run_id(run)
        listed: list[Any] = []
        refusal: Refused | None = None
        try:
            with hub_settings(self._cache):
                api = huggingface_hub.HfApi(endpoint=ENDPOINT, token=False)
                for item in api.list_repo_tree(
                    self._repo,
                    recursive=True,
                    revision=self._revision,
                    repo_type="model",
                    token=False,
                ):
                    listed.append(item)
                    if len(listed) > MAX_LISTED:
                        break
        except _NOT_FOUND:
            refusal = invalid("not_found")
        except Exception:
            refusal = unavailable("list")
        if refusal is not None:
            raise refusal
        if len(listed) > MAX_LISTED:
            raise invalid("count")
        found: list[RemoteFile] = []
        oids: dict[str, str] = {}
        for item in listed:
            path = getattr(item, "path", None)
            if not isinstance(path, str):
                raise invalid("response")
            if isinstance(item, hf_api.RepoFolder):
                raise invalid("subfolder")
            if not isinstance(item, hf_api.RepoFile):
                raise invalid("response")
            if _ignored(path):
                continue
            if "/" in path:
                raise invalid("subfolder")
            allowed_name(path)
            size = item.size
            if not isinstance(size, int) or isinstance(size, bool) or size < 0:
                raise invalid("size")
            sha: str | None = None
            if item.lfs is not None:
                sha = item.lfs.sha256
                if not isinstance(sha, str) or _SHA256.fullmatch(sha) is None:
                    raise invalid("checksum_format")
            else:
                oid = item.blob_id
                if not isinstance(oid, str) or _GIT_OID.fullmatch(oid) is None:
                    raise invalid("checksum_format")
                oids[path] = oid
            found.append(RemoteFile(path, size, sha, None, path))
        out = check_listing(found, self._limits)
        self._oids = oids
        self._listed = {f.name: f for f in out}
        return out

    def fetch(self, file: RemoteFile, dest: Path, max_bytes: int) -> str:
        allowed_name(file.name)
        if file.ref != file.name:
            raise invalid("ref")
        listed = self._listed.get(file.name)
        if listed is None:
            raise invalid("no_checksum")  # not from this source's last listing
        if file != listed:
            raise invalid("changed")
        oid = self._oids.get(file.name)
        if listed.checksum_sha256 is None and oid is None:
            raise invalid("no_checksum")
        if not isinstance(max_bytes, int) or file.size > max_bytes:
            raise invalid("size")
        got: Any = None
        refusal: Refused | None = None
        try:
            with hub_settings(self._cache):
                got = huggingface_hub.hf_hub_download(
                    self._repo,
                    file.name,
                    revision=self._revision,
                    repo_type="model",
                    cache_dir=self._cache,
                    token=False,
                    endpoint=ENDPOINT,
                )
        except _NOT_FOUND:
            refusal = invalid("not_found")
        except Exception:
            refusal = unavailable("download")
        if refusal is not None:
            raise refusal
        if not isinstance(got, (str, os.PathLike)):
            raise invalid("download")
        real = os.path.realpath(got)
        if not within(real, self._cache):
            raise invalid("cache")
        try:
            return self._copy(file, real, dest, max_bytes, None if file.checksum_sha256 else oid)
        finally:
            _remove(real)
            if os.path.lexists(got) and os.path.islink(got):
                _remove(os.fspath(got))

    def _copy(
        self, file: RemoteFile, real: str, dest: Path, max_bytes: int, oid: str | None
    ) -> str:
        fd = -1
        try:
            fd = os.open(real, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK | _CLOEXEC)
        except OSError:
            pass
        if fd < 0:
            raise invalid("open")
        try:
            st = os.fstat(fd)
            if not stat.S_ISREG(st.st_mode):
                raise invalid("special")
            if st.st_size != file.size:
                raise invalid("size_mismatch")
            return stream_to(file, dest, max_bytes, lambda sink: read_into(sink, fd), git_oid=oid)
        finally:
            os.close(fd)


def _remove(path: str) -> None:
    try:
        os.unlink(path)
    except OSError:
        pass


__all__ = ["ENDPOINT", "HFSource", "IGNORED", "check_revision", "hub_settings"]

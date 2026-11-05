"""A run in a local folder: ``root/models/<name>/<run>/``, the same layout as the Drive
folder. For the intelligence lead's own dry runs and CI.

Every level below ``root`` is opened relative to its parent's descriptor with
``O_NOFOLLOW``, so a symlink anywhere (``models``, the model, the run, a file) is refused,
and the run's real path must be exactly ``root``'s real path plus those three names (P1).
Files must be regular and singly linked; a subfolder is refused. ``files.check_run`` still
runs on the scratch copy afterwards (Task 19).
"""

from __future__ import annotations

import os
import stat
from pathlib import Path

from intelligence.modelstore import files, names
from intelligence.modelstore.sources import (
    RemoteFile,
    allowed_name,
    check_listing,
    invalid,
    read_into,
    stream_to,
    within,
)

_CLOEXEC = getattr(os, "O_CLOEXEC", 0)
_DIR_FLAGS = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | _CLOEXEC
_FILE_FLAGS = os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK | _CLOEXEC


def _lstat(name: str, dir_fd: int) -> os.stat_result:
    st: os.stat_result | None = None
    missing = False
    try:
        st = os.stat(name, dir_fd=dir_fd, follow_symlinks=False)
    except FileNotFoundError:
        missing = True
    except OSError:
        pass
    if st is None:
        raise invalid("missing" if missing else "stat")
    return st


def _open_dir(name: str, dir_fd: int) -> int:
    st = _lstat(name, dir_fd)
    if stat.S_ISLNK(st.st_mode):
        raise invalid("symlink")
    if not stat.S_ISDIR(st.st_mode):
        raise invalid("not_folder")
    fd = -1
    try:
        fd = os.open(name, _DIR_FLAGS, dir_fd=dir_fd)
    except OSError:  # ELOOP: a link swapped in after the lstat
        pass
    if fd < 0:
        raise invalid("open")
    return fd


def _check_entry(st: os.stat_result) -> None:
    if stat.S_ISLNK(st.st_mode):
        raise invalid("symlink")
    if stat.S_ISDIR(st.st_mode):
        raise invalid("subfolder")
    if not stat.S_ISREG(st.st_mode):
        raise invalid("special")
    if st.st_nlink > 1:
        raise invalid("hardlink")


class LocalSource:
    def __init__(self, root: Path | str, limits: files.Limits | None = None) -> None:
        # ``root`` is the operator's choice; it is resolved once. Nothing below it may be
        # a link.
        self._root = os.path.realpath(root)
        self._limits = limits or files.Limits()

    def _run_fd(self, name: str, run: str) -> int:
        names.check_name(name)
        names.check_run_id(run)
        fd = -1
        try:
            fd = os.open(self._root, os.O_RDONLY | os.O_DIRECTORY | _CLOEXEC)
        except OSError:
            pass
        if fd < 0:
            raise invalid("root")
        try:
            for part in ("models", name, run):
                nxt = _open_dir(part, fd)
                os.close(fd)
                fd = nxt
            real = os.path.realpath(os.path.join(self._root, "models", name, run))
            if real != os.path.join(self._root, "models", name, run) or not within(
                real, self._root
            ):
                raise invalid("outside")
            return fd
        except BaseException:
            os.close(fd)
            raise

    def list_run(self, name: str, run: str) -> list[RemoteFile]:
        fd = self._run_fd(name, run)
        try:
            entries: list[str] = []
            failed = False
            try:
                with os.scandir(fd) as it:
                    for entry in it:
                        entries.append(entry.name)
                        if len(entries) > self._limits.max_files:
                            break
            except OSError:
                failed = True
            if failed:
                raise invalid("list")
            if len(entries) > self._limits.max_files:
                raise invalid("count")
            found: list[RemoteFile] = []
            for entry_name in entries:
                allowed_name(entry_name)
                st = _lstat(entry_name, fd)
                _check_entry(st)
                found.append(
                    RemoteFile(entry_name, st.st_size, None, None, f"{name}/{run}/{entry_name}")
                )
            return check_listing(found, self._limits)
        finally:
            os.close(fd)

    def fetch(self, file: RemoteFile, dest: Path, max_bytes: int) -> str:
        parts = file.ref.split("/") if isinstance(file.ref, str) else []
        if len(parts) != 3 or parts[2] != file.name:
            raise invalid("ref")
        name, run, fname = parts
        allowed_name(fname)
        dfd = self._run_fd(name, run)
        try:
            st = _lstat(fname, dfd)
            _check_entry(st)
            fd = -1
            try:
                fd = os.open(fname, _FILE_FLAGS, dir_fd=dfd)
            except OSError:
                pass
            if fd < 0:
                raise invalid("open")
            try:
                now = os.fstat(fd)
                _check_entry(now)
                if (now.st_ino, now.st_dev) != (st.st_ino, st.st_dev) or now.st_size != file.size:
                    raise invalid("changed")
                return stream_to(file, dest, max_bytes, lambda sink: read_into(sink, fd))
            finally:
                os.close(fd)
        finally:
            os.close(dfd)


__all__ = ["LocalSource"]

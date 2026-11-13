"""Run sources: a local folder, Google Drive (behind a fake) and pinned Hugging Face revisions.

Nothing here touches the network: Drive is ``tests/modelstore/fakes/drive_api.py`` (Drive v3
response shapes behind a fake session), Hugging Face is a monkeypatched listing and
``hf_hub_download``, and a guard fails any socket that connects to anything but loopback.
Every file is synthetic.
"""

from __future__ import annotations

import hashlib
import ipaddress
import logging
import os
import socket
import stat
from collections.abc import Iterator
from pathlib import Path
from typing import Any

import huggingface_hub
import pytest
from huggingface_hub import constants as hf_constants
from huggingface_hub import hf_api

from intelligence.modelstore import files
from intelligence.modelstore.reasons import Refused
from intelligence.modelstore.sources import RemoteFile, Scratch, scratch
from intelligence.modelstore.sources.drive import FIELDS, DriveSource, HttpDriveApi, drive_check
from intelligence.modelstore.sources.hf import HFSource
from intelligence.modelstore.sources.local import LocalSource
from tests.modelstore.fakes.drive_api import FakeDriveSession, FakeParamError

WEIGHTS = b"synthetic weights " * 50
CONFIG = b'{"model_type": "synthetic"}'
REV = "0123456789abcdef0123456789abcdef01234567"


# ---------------------------------------------------------------- no network


def _guard(real: Any) -> Any:
    def connect(self: socket.socket, address: Any) -> Any:
        if self.family == getattr(socket, "AF_UNIX", object()):
            return real(self, address)
        host = address[0] if isinstance(address, tuple) else address
        try:
            loopback = ipaddress.ip_address(host).is_loopback
        except ValueError:
            loopback = False
        if not loopback:
            raise AssertionError("a test tried to reach the network")
        return real(self, address)

    return connect


@pytest.fixture(autouse=True)
def no_network(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(socket.socket, "connect", _guard(socket.socket.connect))
    monkeypatch.setattr(socket.socket, "connect_ex", _guard(socket.socket.connect_ex))


def test_network_guard_blocks_outside_addresses() -> None:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s, pytest.raises(AssertionError):
        s.connect(("192.0.2.1", 443))


def _refused(fn: Any, *args: Any, **kw: Any) -> Refused:
    with pytest.raises(Refused) as e:
        fn(*args, **kw)
    err = e.value
    assert err.__cause__ is None and err.__context__ is None
    return err


def _code(err: Refused) -> tuple[str, str | None]:
    return err.reason, err.detail_code


@pytest.fixture
def work(tmp_path: Path) -> Iterator[Scratch]:
    with scratch(tmp_path) as s:
        yield s


# ---------------------------------------------------------------- local


def _local_run(root: Path, name: str = "mhai-tiny", run: str = "run-1") -> Path:
    d = root / "models" / name / run
    d.mkdir(parents=True)
    (d / "model.safetensors").write_bytes(WEIGHTS)
    (d / "config.json").write_bytes(CONFIG)
    return d


def test_local_lists_and_fetches(tmp_path: Path, work: Scratch) -> None:
    _local_run(tmp_path / "src")
    src = LocalSource(tmp_path / "src")
    listed = src.list_run("mhai-tiny", "run-1")
    assert [f.name for f in listed] == ["config.json", "model.safetensors"]
    for f in listed:
        sha = src.fetch(f, work.run / f.name, f.size)
        assert sha == hashlib.sha256((work.run / f.name).read_bytes()).hexdigest()
    assert (work.run / "model.safetensors").read_bytes() == WEIGHTS
    assert stat.S_IMODE(os.stat(work.run / "config.json").st_mode) == 0o600


def test_local_traversal_and_symlink_refused(tmp_path: Path, work: Scratch) -> None:
    root = tmp_path / "src"
    run = _local_run(root)
    src = LocalSource(root)
    for name, run_id in (("../x", "run-1"), ("mhai-tiny", ".."), ("mhai-tiny", "a/b")):
        assert _refused(src.list_run, name, run_id).reason == "invalid_name"

    outside = tmp_path / "outside"
    outside.mkdir()
    (outside / "model.safetensors").write_bytes(WEIGHTS)
    (outside / "config.json").write_bytes(CONFIG)
    # A linked run, a linked model folder, a linked "models".
    (root / "models" / "mhai-tiny" / "run-2").symlink_to(outside)
    assert _code(_refused(src.list_run, "mhai-tiny", "run-2")) == ("source_invalid", "symlink")
    (root / "models" / "linked").symlink_to(root / "models" / "mhai-tiny")
    assert _code(_refused(src.list_run, "linked", "run-1")) == ("source_invalid", "symlink")
    other = tmp_path / "other"
    other.mkdir()
    (other / "models").symlink_to(root / "models")
    assert _code(_refused(LocalSource(other).list_run, "mhai-tiny", "run-1")) == (
        "source_invalid",
        "symlink",
    )
    # A linked file, a hard link, a subfolder.
    (run / "tokenizer.json").symlink_to(outside / "config.json")
    assert _code(_refused(src.list_run, "mhai-tiny", "run-1")) == ("source_invalid", "symlink")
    (run / "tokenizer.json").unlink()
    os.link(run / "config.json", run / "generation_config.json")
    assert _code(_refused(src.list_run, "mhai-tiny", "run-1")) == ("source_invalid", "hardlink")
    (run / "generation_config.json").unlink()
    (run / "sub.safetensors").mkdir()
    assert _code(_refused(src.list_run, "mhai-tiny", "run-1")) == ("source_invalid", "subfolder")
    (run / "sub.safetensors").rmdir()
    (run / "notes.txt").write_bytes(b"x")
    assert _code(_refused(src.list_run, "mhai-tiny", "run-1")) == ("source_invalid", "name")
    (run / "notes.txt").unlink()

    # A ref that points elsewhere, or a file swapped for a link after the listing.
    good = src.list_run("mhai-tiny", "run-1")[0]
    forged = RemoteFile(good.name, good.size, None, None, f"mhai-tiny/../{good.name}")
    assert _refused(src.fetch, forged, work.run / good.name, good.size).reason in (
        "invalid_name",
        "source_invalid",
    )
    (run / good.name).unlink()
    (run / good.name).symlink_to(outside / "config.json")
    assert _code(_refused(src.fetch, good, work.run / good.name, good.size)) == (
        "source_invalid",
        "symlink",
    )
    assert not (work.run / good.name).exists()


# ---------------------------------------------------------------- Drive


class DriveRun:
    def __init__(self, page_size: int = 2) -> None:
        self.fake = FakeDriveSession(page_size=page_size)
        self.root = self.fake.shared_root()
        models = self.fake.folder("models", self.root.id)
        self.model = self.fake.folder("mhai-tiny", models.id)
        self.run = self.fake.folder("run-1", self.model.id)
        self.weights = self.fake.file("model.safetensors", self.run.id, WEIGHTS)
        self.config = self.fake.file("config.json", self.run.id, CONFIG)
        self.api = HttpDriveApi(self.fake)
        self.source = DriveSource(self.api, self.root.id)

    def media_requests(self) -> int:
        return sum(1 for r in self.fake.requests if r.params.get("alt") == "media")


def test_drive_lists_and_fetches(work: Scratch) -> None:
    d = DriveRun()
    for i in range(3):  # more than one page
        d.fake.file(f"shard-{i}.safetensors", d.run.id, WEIGHTS + bytes([i]))
    listed = d.source.list_run("mhai-tiny", "run-1")
    assert len(listed) == 5
    for f in listed:
        assert d.source.fetch(f, work.run / f.name, f.size) == f.checksum_sha256
    assert (work.run / "config.json").read_bytes() == CONFIG
    # Walked by parent id, with exactly the plan's fields, and paged.
    lists = [r for r in d.fake.requests if r.url.endswith("/files")]
    assert all(r.params["q"].endswith("' in parents") for r in lists)
    assert any("pageToken" in r.params for r in lists)


def test_drive_endless_pages_refused() -> None:
    d = DriveRun()
    d.fake.endless_pages = True
    assert _code(_refused(d.source.list_run, "mhai-tiny", "run-1")) == ("source_invalid", "pages")


def test_drive_duplicate_names_refused(work: Scratch) -> None:
    d = DriveRun()
    d.fake.file("config.json", d.run.id, b'{"other": 1}')
    assert _code(_refused(d.source.list_run, "mhai-tiny", "run-1")) == (
        "source_invalid",
        "duplicate",
    )
    d2 = DriveRun()
    d2.fake.folder("run-1", d2.model.id)  # two run folders with one name
    assert _code(_refused(d2.source.list_run, "mhai-tiny", "run-1")) == (
        "source_invalid",
        "duplicate",
    )
    assert d.media_requests() == 0 and d2.media_requests() == 0


@pytest.mark.parametrize(
    "name",
    [
        "a/model.safetensors",
        "a\\model.safetensors",
        "..",
        "../config.json",
        ".hidden.safetensors",
        "model.bin",
        "loader.py",
        "notes.txt",
        "",
        "con fig.json",
    ],
)
def test_drive_bad_names_refused(name: str) -> None:
    d = DriveRun()
    d.fake.file(name, d.run.id, b"x")
    assert _code(_refused(d.source.list_run, "mhai-tiny", "run-1")) == ("source_invalid", "name")
    assert d.media_requests() == 0


@pytest.mark.parametrize(
    ("mime", "kw", "detail"),
    [
        ("application/vnd.google-apps.document", {}, "native"),
        ("application/vnd.google-apps.spreadsheet", {}, "native"),
        ("application/vnd.google-apps.shortcut", {}, "shortcut"),
        ("application/vnd.google-apps.folder", {}, "subfolder"),
        ("application/octet-stream", {"trashed": True}, "trashed"),
    ],
)
def test_drive_native_shortcut_subfolder_refused(
    mime: str, kw: dict[str, Any], detail: str
) -> None:
    d = DriveRun()
    d.fake.add("tokenizer.json", mime, d.run.id, b"{}", **kw)
    assert _code(_refused(d.source.list_run, "mhai-tiny", "run-1")) == ("source_invalid", detail)


def test_drive_trashed_run_folder_refused() -> None:
    d = DriveRun()
    d.run.trashed = True
    assert _code(_refused(d.source.list_run, "mhai-tiny", "run-1")) == ("source_invalid", "trashed")


def test_drive_size_cap_and_overrun(work: Scratch) -> None:
    d = DriveRun()
    # A reported size over the cap: refused at listing, nothing downloaded.
    small = DriveSource(d.api, d.root.id, files.Limits(max_file_bytes=len(WEIGHTS) - 1))
    assert _code(_refused(small.list_run, "mhai-tiny", "run-1")) == ("source_invalid", "size")
    # More files than Task 13's limit: refused before anything is downloaded.
    few = DriveSource(d.api, d.root.id, files.Limits(max_files=1))
    assert _code(_refused(few.list_run, "mhai-tiny", "run-1")) == ("source_invalid", "count")
    assert d.media_requests() == 0
    # A download that runs past its reported size: refused, the partial file removed.
    listed = {f.name: f for f in d.source.list_run("mhai-tiny", "run-1")}
    w = listed["model.safetensors"]
    d.fake.serve[d.weights.id] = WEIGHTS + b"more"
    assert _code(_refused(d.source.fetch, w, work.run / w.name, w.size)) == (
        "source_invalid",
        "overrun",
    )
    assert not (work.run / w.name).exists()
    # A short download.
    d.fake.serve[d.weights.id] = WEIGHTS[:-1]
    assert _code(_refused(d.source.fetch, w, work.run / w.name, w.size)) == (
        "source_invalid",
        "short",
    )
    assert not (work.run / w.name).exists()
    # The caller's cap below the reported size.
    assert _code(_refused(d.source.fetch, w, work.run / w.name, w.size - 1)) == (
        "source_invalid",
        "size",
    )
    # A run past Task 13's default 256 files.
    many = DriveRun(page_size=100)
    for i in range(files.Limits().max_files):
        many.fake.file(f"s{i}.safetensors", many.run.id, b"w")
    assert _code(_refused(many.source.list_run, "mhai-tiny", "run-1")) == (
        "source_invalid",
        "count",
    )
    assert many.media_requests() == 0


def test_drive_checksum_mismatch_refused(work: Scratch) -> None:
    d = DriveRun()
    listed = {f.name: f for f in d.source.list_run("mhai-tiny", "run-1")}
    w = listed["model.safetensors"]
    d.fake.serve[d.weights.id] = b"X" + WEIGHTS[1:]  # same length, other bytes
    assert _code(_refused(d.source.fetch, w, work.run / w.name, w.size)) == (
        "source_invalid",
        "checksum",
    )
    assert not (work.run / w.name).exists()

    # Only md5Checksum: MD5 is compared (and SHA-256 is still what fetch returns).
    m = DriveRun()
    m.weights.with_sha256 = False
    m.config.with_sha256 = False
    mlisted = {f.name: f for f in m.source.list_run("mhai-tiny", "run-1")}
    mw = mlisted["model.safetensors"]
    assert mw.checksum_sha256 is None and mw.checksum_md5 is not None
    m.fake.serve[m.weights.id] = b"X" + WEIGHTS[1:]
    assert _code(_refused(m.source.fetch, mw, work.run / mw.name, mw.size)) == (
        "source_invalid",
        "checksum",
    )
    assert not (work.run / mw.name).exists()
    del m.fake.serve[m.weights.id]
    assert m.source.fetch(mw, work.run / mw.name, mw.size) == hashlib.sha256(WEIGHTS).hexdigest()

    # A malformed checksum is refused.
    n = DriveRun()
    n.weights.with_sha256 = False
    n.weights.md5 = "not-hex"
    assert _code(_refused(n.source.list_run, "mhai-tiny", "run-1")) == (
        "source_invalid",
        "checksum_format",
    )
    # No checksum at all is refused, before anything is downloaded.
    none = DriveRun()
    none.weights.with_sha256 = False
    none.weights.with_md5 = False
    assert _code(_refused(none.source.list_run, "mhai-tiny", "run-1")) == (
        "source_invalid",
        "no_checksum",
    )
    assert none.media_requests() == 0


def test_drive_item_changed_after_listing(work: Scratch) -> None:
    d = DriveRun()
    w = next(f for f in d.source.list_run("mhai-tiny", "run-1") if f.name == "model.safetensors")
    d.weights.content = WEIGHTS + b"!"
    assert _code(_refused(d.source.fetch, w, work.run / w.name, w.size)) == (
        "source_invalid",
        "changed",
    )
    assert d.media_requests() == 0


def test_drive_errors_mapped(work: Scratch) -> None:
    d = DriveRun()
    w = next(f for f in d.source.list_run("mhai-tiny", "run-1") if f.name == "model.safetensors")
    for status, expected in (
        (401, ("source_unavailable", "auth")),
        (403, ("source_unavailable", "auth")),
        (429, ("source_unavailable", "rate_limited")),
        (500, ("source_unavailable", "server")),
        (418, ("source_unavailable", "http")),
    ):
        d.fake.status[d.weights.id] = status
        assert _code(_refused(d.source.fetch, w, work.run / w.name, w.size)) == expected
    del d.fake.status[d.weights.id]
    d.fake.raise_on.add(d.weights.id)
    assert _code(_refused(d.source.fetch, w, work.run / w.name, w.size)) == (
        "source_unavailable",
        "network",
    )
    gone = RemoteFile(w.name, w.size, w.checksum_sha256, None, "fk0000000000000000")
    d.fake.raise_on.clear()
    assert _code(_refused(d.source.fetch, gone, work.run / w.name, w.size)) == (
        "source_invalid",
        "not_found",
    )
    for bad in ("short", "x' or name contains 'y", "../../about"):
        with pytest.raises(Refused):
            DriveSource(d.api, bad)
    assert not (work.run / w.name).exists()


def test_drive_check_one_folder_read_only() -> None:
    d = DriveRun()
    drive_check(d.api, d.root.id)

    # Two top-level folders.
    two = DriveRun()
    two.fake.shared_root("Another")
    assert _code(_refused(drive_check, two.api, two.root.id)) == ("drive_scope", "top_level_count")
    # A file shared directly, on top of the folder.
    direct = DriveRun()
    stray = direct.fake.file("config.json", None, CONFIG)
    direct.fake.top_level.append(stray.id)
    assert _refused(drive_check, direct.api, direct.root.id).reason == "drive_scope"
    # A file in a shared drive, shared with the account file by file (no membership): only
    # a listing across all drives sees it, and the fake refuses any listing that isn't.
    in_drive = DriveRun()
    shared = in_drive.fake.file("config.json", None, CONFIG)
    in_drive.fake.shared_drive_top_level.append(shared.id)
    assert _code(_refused(drive_check, in_drive.api, in_drive.root.id)) == (
        "drive_scope",
        "top_level_count",
    )
    # Membership of a shared drive.
    member = DriveRun()
    member.fake.drives.append("0AsharedDriveId123")
    assert _refused(drive_check, member.api, member.root.id).reason == "drive_scope"
    # The one folder isn't ours.
    other = DriveRun()
    assert _code(_refused(drive_check, other.api, d.root.id)) == ("drive_scope", "top_level")
    # canEdit on the folder; canShare, canDelete, canAddChildren on something under it.
    edit = DriveRun()
    edit.root.capabilities["canEdit"] = True
    assert _code(_refused(drive_check, edit.api, edit.root.id)) == ("drive_scope", "writable")
    for cap, item in (("canShare", "weights"), ("canDelete", "config"), ("canAddChildren", "run")):
        r = DriveRun()
        getattr(r, item).capabilities[cap] = True
        assert _code(_refused(drive_check, r.api, r.root.id)) == ("drive_scope", "writable")
    # A capability missing from the response is not "false".
    missing = DriveRun()
    del missing.config.capabilities["canDelete"]
    assert _refused(drive_check, missing.api, missing.root.id).reason == "drive_scope"


# ---------------------------------------------------------------- Hugging Face


HF_VARS = (
    "HF_HUB_DISABLE_TELEMETRY",
    "HF_HUB_DISABLE_IMPLICIT_TOKEN",
    "HF_HUB_DISABLE_XET",
    "HF_HOME",
    "HF_XET_CACHE",
)


def _hf_constants() -> tuple[Any, ...]:
    return (
        hf_constants.HF_HUB_DISABLE_TELEMETRY,
        hf_constants.HF_HUB_DISABLE_IMPLICIT_TOKEN,
        hf_constants.HF_HUB_DISABLE_XET,
        hf_constants.HF_HOME,
        hf_constants.HF_XET_CACHE,
    )


def _git_oid(content: bytes) -> str:
    return hashlib.sha1(b"blob %d\0" % len(content) + content).hexdigest()


class FakeHub:
    """``list_repo_tree`` and ``hf_hub_download`` with synthetic files; no request."""

    def __init__(self, monkeypatch: pytest.MonkeyPatch, entries: dict[str, bytes | None]) -> None:
        self.entries = entries  # path -> content (None: a folder)
        self.lfs = {"model.safetensors"}
        self.downloads: list[str] = []
        self.calls: list[dict[str, Any]] = []
        self.serve: dict[str, bytes] = {}
        self.fail_list = False
        self.fail_download = False
        hub = self

        class FakeApi:
            def __init__(self, *, endpoint: str | None = None, token: Any = None) -> None:
                hub.apis.append({"endpoint": endpoint, "token": token})

            def list_repo_tree(self, repo_id: str, **kw: Any) -> Iterator[Any]:
                return hub.list_repo_tree(repo_id, **kw)

        self.apis: list[dict[str, Any]] = []
        monkeypatch.setattr(huggingface_hub, "HfApi", FakeApi)
        monkeypatch.setattr(huggingface_hub, "list_repo_tree", self.list_repo_tree)
        monkeypatch.setattr(huggingface_hub, "hf_hub_download", self.hf_hub_download)

    def _settings(self) -> dict[str, Any]:
        return {
            "env": {k: os.environ.get(k) for k in HF_VARS},
            "const": _hf_constants(),
        }

    def list_repo_tree(self, repo_id: str, **kw: Any) -> Iterator[Any]:
        self.calls.append({"fn": "list", "repo": repo_id, **kw, **self._settings()})
        if self.fail_list:
            raise RuntimeError("synthetic hub failure")
        for path, content in self.entries.items():
            if content is None:
                yield hf_api.RepoFolder(path=path, oid="f" * 40)
                continue
            lfs = None
            if path in self.lfs:
                lfs = {
                    "size": len(content),
                    "oid": hashlib.sha256(content).hexdigest(),
                    "pointerSize": 134,
                }
            yield hf_api.RepoFile(path=path, size=len(content), oid=_git_oid(content), lfs=lfs)

    def hf_hub_download(self, repo_id: str, filename: str, **kw: Any) -> str:
        self.calls.append(
            {"fn": "download", "repo": repo_id, "file": filename, **kw, **self._settings()}
        )
        self.downloads.append(filename)
        if self.fail_download:
            raise RuntimeError("synthetic hub failure")
        content = self.serve.get(filename, self.entries[filename] or b"")
        cache = Path(kw["cache_dir"])
        blob = cache / "blobs" / hashlib.sha256(content).hexdigest()
        blob.parent.mkdir(parents=True, exist_ok=True)
        blob.write_bytes(content)
        snap = cache / "snapshots" / kw["revision"] / filename
        snap.parent.mkdir(parents=True, exist_ok=True)
        snap.symlink_to(blob)
        return str(snap)


def test_hf_revision_must_be_commit(work: Scratch) -> None:
    for rev in ("main", "v1.0.0", REV[:-1], REV.upper(), REV + "\n", "refs/pr/1", REV[:-1] + "g"):
        assert _refused(HFSource, "synthetic/tiny", rev, scratch=work).reason == (
            "revision_unpinned"
        )
    for repo in ("../tiny", "synthetic", "a/b/c", "synthetic/../x"):
        assert _code(_refused(HFSource, repo, REV, scratch=work)) == (
            "source_invalid",
            "repo",
        )


def test_hf_only_allowed_files_fetched(
    monkeypatch: pytest.MonkeyPatch, work: Scratch, tmp_path: Path
) -> None:
    for k in HF_VARS:
        monkeypatch.delenv(k, raising=False)
    # A mirror in the environment (and in the library's constant) is ignored: pinned.
    monkeypatch.setenv("HF_ENDPOINT", "https://mirror.invalid")
    monkeypatch.setattr(hf_constants, "ENDPOINT", "https://mirror.invalid")
    before = _hf_constants()
    hub = FakeHub(
        monkeypatch,
        {
            ".gitattributes": b"*.safetensors filter=lfs",
            "README.md": b"# synthetic",
            "LICENSE": b"Apache-2.0",
            "config.json": CONFIG,
            "model.safetensors": WEIGHTS,
        },
    )
    src = HFSource("synthetic/tiny", REV, scratch=work)
    listed = src.list_run("mhai-tiny", "run-1")
    assert [f.name for f in listed] == ["config.json", "model.safetensors"]
    for f in listed:
        src.fetch(f, work.run / f.name, f.size)
    assert hub.downloads == ["config.json", "model.safetensors"]
    assert sorted(p.name for p in work.run.iterdir()) == ["config.json", "model.safetensors"]
    assert (work.run / "model.safetensors").read_bytes() == WEIGHTS
    cache = str(work.cache.resolve())
    home, xet = os.path.join(cache, "home"), os.path.join(cache, "xet")
    assert hub.apis == [{"endpoint": "https://huggingface.co", "token": False}]
    for call in hub.calls:
        assert call["revision"] == REV and call["token"] is False
        assert call["env"] == {
            "HF_HUB_DISABLE_TELEMETRY": "1",
            "HF_HUB_DISABLE_IMPLICIT_TOKEN": "1",
            "HF_HUB_DISABLE_XET": "1",
            "HF_HOME": home,
            "HF_XET_CACHE": xet,
        }
        assert call["const"] == (True, True, True, home, xet)
        if call["fn"] == "download":
            assert call["cache_dir"] == cache
            assert call["endpoint"] == "https://huggingface.co"
    # Put back after the call, not left in the process.
    for k in HF_VARS:
        assert k not in os.environ
    assert _hf_constants() == before
    # The cached copies are gone; every cache the hub uses was inside the scratch.
    assert not any(p.is_file() and not p.is_symlink() for p in work.cache.rglob("*"))


@pytest.mark.parametrize(
    "extra",
    [
        {"pytorch_model.bin": b"x"},
        {"modeling.py": b"x"},
        {"onnx": None},
        {"onnx/model.onnx": b"x"},
        {"notes.txt": b"x"},
    ],
)
def test_hf_repo_with_other_files_refused(
    monkeypatch: pytest.MonkeyPatch, work: Scratch, extra: dict[str, bytes | None]
) -> None:
    hub = FakeHub(monkeypatch, {"config.json": CONFIG, "model.safetensors": WEIGHTS, **extra})
    src = HFSource("synthetic/tiny", REV, scratch=work)
    assert _refused(src.list_run, "mhai-tiny", "run-1").reason == "source_invalid"
    assert hub.downloads == []


def test_hf_checksum_mismatch_refused(monkeypatch: pytest.MonkeyPatch, work: Scratch) -> None:
    hub = FakeHub(monkeypatch, {"config.json": CONFIG, "model.safetensors": WEIGHTS})
    src = HFSource("synthetic/tiny", REV, scratch=work)
    listed = {f.name: f for f in src.list_run("mhai-tiny", "run-1")}
    hub.serve = {
        "config.json": b'{"model_type": "synthetiX"}',
        "model.safetensors": b"X" + WEIGHTS[1:],
    }
    for name in ("config.json", "model.safetensors"):
        f = listed[name]
        assert _code(_refused(src.fetch, f, work.run / name, f.size)) == (
            "source_invalid",
            "checksum",
        )
        assert not (work.run / name).exists()


# ---------------------------------------------------------------- scratch


def test_scratch_removed_on_failure(tmp_path: Path) -> None:
    seen: list[Scratch] = []
    with pytest.raises(RuntimeError), scratch(tmp_path) as s:
        seen.append(s)
        for d in (s.root, s.run, s.cache):
            assert stat.S_IMODE(os.lstat(d).st_mode) == 0o700
        (s.run / "model.safetensors").write_bytes(WEIGHTS)
        raise RuntimeError("the promote step failed")
    assert not seen[0].root.exists()
    with scratch(tmp_path) as s2:
        pass
    assert not s2.root.exists()

    # A failed fetch removes its partial file; the scratch goes on exit.
    d = DriveRun()
    with pytest.raises(Refused), scratch(tmp_path) as s3:
        w = next(
            f for f in d.source.list_run("mhai-tiny", "run-1") if f.name == "model.safetensors"
        )
        d.fake.serve[d.weights.id] = WEIGHTS + b"x"
        try:
            d.source.fetch(w, s3.run / w.name, w.size)
        finally:
            assert not (s3.run / w.name).exists()
    assert not s3.root.exists()
    assert list(tmp_path.iterdir()) == []


# ---------------------------------------------------------------- logs


def test_no_tokens_in_logs(caplog: pytest.LogCaptureFixture, work: Scratch) -> None:
    caplog.set_level(logging.DEBUG)
    d = DriveRun()
    token = d.fake.token
    errors: list[BaseException] = []
    w = next(f for f in d.source.list_run("mhai-tiny", "run-1") if f.name == "model.safetensors")
    d.fake.status[d.weights.id] = 403  # the body carries the token
    d.fake.status[d.run.id] = 403
    for call in (
        lambda: d.source.fetch(w, work.run / w.name, w.size),
        lambda: d.source.list_run("mhai-tiny", "run-1"),
    ):
        with pytest.raises(Refused) as e:
            call()
        errors.append(e.value)
    d.fake.status.clear()
    d.fake.raise_on.add(d.weights.id)  # the exception's message carries the URL and header
    with pytest.raises(Refused) as e:
        d.source.fetch(w, work.run / w.name, w.size)
    errors.append(e.value)
    for err in errors:
        assert err.__cause__ is None and err.__context__ is None
        assert token not in str(err) and token not in repr(err) and token not in repr(vars(err))
        assert "googleapis" not in str(err) + repr(err)
    assert token not in caplog.text
    assert "googleapis" not in caplog.text
    assert "Authorization" not in caplog.text


def test_hf_cache_must_be_inside_scratch(tmp_path: Path, work: Scratch) -> None:
    elsewhere = tmp_path / "elsewhere"
    elsewhere.mkdir()
    for cache in (elsewhere, work.run, work.root, work.run.parent.parent):
        assert _code(_refused(HFSource, "synthetic/tiny", REV, scratch=work, cache_dir=cache)) == (
            "source_invalid",
            "cache",
        )
    # A "scratch" that isn't one: an ordinary folder, not 0700.
    loose = tmp_path / "loose"
    (loose / "cache").mkdir(parents=True)
    loose.chmod(0o755)
    fake = Scratch(loose, loose / "run", loose / "cache")
    assert _code(_refused(HFSource, "synthetic/tiny", REV, scratch=fake)) == (
        "source_invalid",
        "cache",
    )
    inner = work.cache / "hub"
    inner.mkdir()
    HFSource("synthetic/tiny", REV, scratch=work, cache_dir=inner)


def test_hf_fetch_needs_this_sources_listing(
    monkeypatch: pytest.MonkeyPatch, work: Scratch
) -> None:
    hub = FakeHub(monkeypatch, {"config.json": CONFIG, "model.safetensors": WEIGHTS})
    listed = {f.name: f for f in HFSource("synthetic/tiny", REV, scratch=work).list_run("m-1", "r")}
    # A fresh instance never listed: refused for both kinds of file, nothing downloaded.
    fresh = HFSource("synthetic/tiny", REV, scratch=work)
    for f in listed.values():
        assert _code(_refused(fresh.fetch, f, work.run / f.name, f.size)) == (
            "source_invalid",
            "no_checksum",
        )
    # The caller's RemoteFile must be the listed one: a swapped LFS hash is refused.
    src = HFSource("synthetic/tiny", REV, scratch=work)
    src.list_run("m-1", "r")
    w = listed["model.safetensors"]
    forged = RemoteFile(w.name, w.size, "0" * 64, None, w.ref)
    assert _code(_refused(src.fetch, forged, work.run / w.name, w.size)) == (
        "source_invalid",
        "changed",
    )
    assert hub.downloads == []


# ---------------------------------------------------------------- re-review 1


def test_drive_incomplete_search_refused() -> None:
    # A run listing: incompleteSearch true, left out, or true from the second page on.
    for flag, from_page in ((True, 0), (None, 0), (True, 1)):
        d = DriveRun()
        for i in range(3):
            d.fake.file(f"s{i}.safetensors", d.run.id, WEIGHTS)
        d.fake.incomplete, d.fake.incomplete_from = flag, from_page
        assert _code(_refused(d.source.list_run, "mhai-tiny", "run-1")) == (
            "source_invalid",
            "incomplete",
        )
        assert d.media_requests() == 0
    # drive_check: the top-level view, or any folder under it, incomplete.
    for flag in (True, None):
        c = DriveRun()
        c.fake.incomplete = flag
        assert _code(_refused(drive_check, c.api, c.root.id)) == ("drive_scope", "incomplete")
    deep = DriveRun()
    for i in range(3):
        deep.fake.file(f"s{i}.safetensors", deep.run.id, WEIGHTS)
    deep.fake.incomplete, deep.fake.incomplete_from = True, 1  # only on later pages
    assert _code(_refused(drive_check, deep.api, deep.root.id)) == ("drive_scope", "incomplete")


def test_fake_param_failures_are_their_own_error() -> None:
    d = DriveRun()
    # A get without supportsAllDrives is refused by the fake, on the error paths too, and
    # the failure is not swallowed into ``network``.
    for setup in (lambda: None, lambda: d.fake.status.update({d.weights.id: 500})):
        setup()
        with pytest.raises(FakeParamError):
            d.api._json(f"/files/{d.weights.id}", {"fields": FIELDS})
    d.fake.status.clear()
    d.fake.raise_on.add(d.weights.id)
    with pytest.raises(FakeParamError):
        d.api._json(f"/files/{d.weights.id}", {"fields": FIELDS})
    with pytest.raises(FakeParamError):  # a listing without the all-drives parameters
        d.api._json("/files", {"q": f"'{d.run.id}' in parents"})


def test_hf_download_url_must_be_pinned(monkeypatch: pytest.MonkeyPatch, work: Scratch) -> None:
    built: list[str] = []
    real_url = huggingface_hub.hf_hub_url

    def spy(*a: Any, **kw: Any) -> str:
        url = real_url(*a, **kw)  # the library's own URL, not a fake one
        built.append(url)
        return url

    monkeypatch.setattr(huggingface_hub, "hf_hub_url", spy)
    expected = f"https://huggingface.co/synthetic/tiny/resolve/{REV}/model.safetensors"
    template = hf_constants.HUGGINGFACE_CO_URL_TEMPLATE
    mirror = "https://mirror.invalid"

    def fetch_weights() -> Any:
        hub = FakeHub(monkeypatch, {"config.json": CONFIG, "model.safetensors": WEIGHTS})
        src = HFSource("synthetic/tiny", REV, scratch=work)
        w = next(f for f in src.list_run("m-1", "r") if f.name == "model.safetensors")
        dest = work.run / w.name
        if dest.exists():
            dest.unlink()
        return hub, src, w, dest

    # constants.ENDPOINT changed after import, the template not: the URL stays pinned.
    monkeypatch.setattr(hf_constants, "ENDPOINT", mirror)
    hub, src, w, dest = fetch_weights()
    src.fetch(w, dest, w.size)
    assert built[-1] == expected and hub.downloads == ["model.safetensors"]
    # HF_ENDPOINT set before import (template and ENDPOINT agree): endpoint= swaps it back.
    monkeypatch.setattr(
        hf_constants,
        "HUGGINGFACE_CO_URL_TEMPLATE",
        template.replace("https://huggingface.co", mirror),
    )
    hub, src, w, dest = fetch_weights()
    src.fetch(w, dest, w.size)
    assert built[-1] == expected
    # The template points elsewhere but ENDPOINT doesn't: no swap, so refused, no download.
    monkeypatch.setattr(hf_constants, "ENDPOINT", "https://huggingface.co")
    hub, src, w, dest = fetch_weights()
    assert _code(_refused(src.fetch, w, dest, w.size)) == ("source_invalid", "endpoint")
    assert built[-1].startswith(mirror) and hub.downloads == []
    assert not dest.exists()


def test_hf_settings_restored_after_a_failed_call(
    monkeypatch: pytest.MonkeyPatch, work: Scratch
) -> None:
    for k in HF_VARS:
        monkeypatch.delenv(k, raising=False)
    before = _hf_constants()
    hub = FakeHub(monkeypatch, {"config.json": CONFIG, "model.safetensors": WEIGHTS})
    src = HFSource("synthetic/tiny", REV, scratch=work)
    listed = src.list_run("m-1", "r")
    hub.fail_download = True
    f = listed[0]
    assert _code(_refused(src.fetch, f, work.run / f.name, f.size)) == (
        "source_unavailable",
        "download",
    )
    assert hub.calls[-1]["env"]["HF_HUB_DISABLE_XET"] == "1"  # set during the failed call
    hub.fail_list = True
    assert _code(_refused(src.list_run, "m-1", "r")) == ("source_unavailable", "list")
    for k in HF_VARS:
        assert k not in os.environ
    assert _hf_constants() == before
    assert not (work.run / f.name).exists()


# ---------------------------------------------------------------- manifest.json (own model PR 1)


def test_local_misnamed_training_manifest_refused(tmp_path: Path) -> None:
    d = _local_run(tmp_path / "src")
    (d / "manifest.json").write_bytes(b"{}")
    src = LocalSource(tmp_path / "src")
    err = _refused(src.list_run, "mhai-tiny", "run-1")
    assert err.reason == "training_manifest_misnamed"
    (d / "training_manifest.json").write_bytes(b"{}")  # both: manifest.json is just not allowed
    assert _code(_refused(src.list_run, "mhai-tiny", "run-1")) == ("source_invalid", "name")


def test_drive_misnamed_training_manifest_refused() -> None:
    d = DriveRun()
    d.fake.file("manifest.json", d.run.id, b"{}")
    assert _refused(d.source.list_run, "mhai-tiny", "run-1").reason == (
        "training_manifest_misnamed"
    )
    assert d.media_requests() == 0

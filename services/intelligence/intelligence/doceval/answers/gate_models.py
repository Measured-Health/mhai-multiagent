"""The answer gate's retriever and NLI models, pinned and verified (security review of #144).

The gate verifies answers with the worker's own retriever (``DENSE_MODEL``) and NLI model
(``NLI_MODEL``). A floating download of either could change what the gate measures, so
``models/answer-gate-models.lock`` pins each to an exact 40-hex commit and lists the files
it may hold with their SHA-256 (safetensors and JSON/tokenizer files only)::

    {"models": {"dense": {"files": {<path>: <sha256>}, "repo": ..., "revision": <40 hex>},
                "nli": {...}}, "schema": "mhai.answers.gate_models/1"}

* :func:`fetch` (the promote workflow, before the evaluation; it has the network):
  ``snapshot_download`` at that revision of the listed files only, into
  ``<dir>/<role>/``, then :func:`verify`.
* :func:`verify` (the gate, before anything starts): each role's folder must hold exactly
  the listed files (hf's own ``.cache`` bookkeeping aside), no link, each with its
  SHA-256 (``hash_mismatch``/``missing_file``/``extra_file``/``file_not_allowed``, detail
  ``gate_models``). The gate then loads the models from those folders only, offline.
* :func:`check_config`: the pinned repos must be the ones the worker is configured with,
  so the gate measures what a hospital gets (``not_allowed_here``/``gate_models_config``).

The lock changes only by a pull request, like ``models.lock``. When ``nli`` becomes a
pinnable role in ``models.lock``, the gate moves to that pin.
"""

from __future__ import annotations

import hashlib
import os
import re
import secrets
import stat
from collections.abc import Mapping
from dataclasses import dataclass
from pathlib import Path
from types import MappingProxyType

from intelligence.modelstore import canonical, files
from intelligence.modelstore.reasons import Refused

REPO = Path(__file__).resolve().parents[5]
LOCK_PATH = REPO / "models" / "answer-gate-models.lock"
DIR_ENV = "MHAI_ANSWER_GATE_MODELS"
SCHEMA = "mhai.answers.gate_models/1"
ROLES = ("dense", "nli")
MAX_LOCK_BYTES = 64 * 1024
MAX_FILES = 64
DENSE_PREFIX = "sentence-transformers/"
HF_BOOKKEEPING = ".cache"

_REVISION = re.compile(r"[0-9a-f]{40}", re.ASCII)
_SHA = re.compile(r"[0-9a-f]{64}", re.ASCII)
_REPO = re.compile(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,95}/[A-Za-z0-9][A-Za-z0-9_.-]{0,95}", re.ASCII)
_PATH = re.compile(
    r"(?:[A-Za-z0-9_][A-Za-z0-9_.-]{0,63}/)?[A-Za-z0-9_][A-Za-z0-9_.-]{0,63}", re.ASCII
)
_ALLOWED_SUFFIXES = (".json", ".safetensors", ".txt", ".model")


@dataclass(frozen=True)
class PinnedModel:
    repo: str
    revision: str
    files: Mapping[str, str]


def _invalid() -> Refused:
    return Refused("lock_invalid", detail_code="gate_models")


def load_lock(path: Path | str | None = None) -> dict[str, PinnedModel]:
    """The committed lock (canonical JSON and a newline), held to its patterns."""
    raw = files.read_regular(Path(path) if path is not None else LOCK_PATH, MAX_LOCK_BYTES)
    if not raw.endswith(b"\n"):
        raise _invalid()
    failed = False
    try:
        obj = canonical.loads_strict(raw[:-1], MAX_LOCK_BYTES)
        failed = canonical.dumps(obj) != raw[:-1]
    except Refused:
        failed = True
    if failed or not isinstance(obj, dict) or set(obj) != {"schema", "models"}:
        raise _invalid()
    models = obj["models"]
    if obj["schema"] != SCHEMA or not isinstance(models, dict) or set(models) != set(ROLES):
        raise _invalid()
    out: dict[str, PinnedModel] = {}
    for role, m in models.items():
        if not isinstance(m, dict) or set(m) != {"repo", "revision", "files"}:
            raise _invalid()
        repo, rev, fs = m["repo"], m["revision"], m["files"]
        if not isinstance(repo, str) or _REPO.fullmatch(repo) is None or ".." in repo:
            raise _invalid()
        if not isinstance(rev, str) or _REVISION.fullmatch(rev) is None:
            raise _invalid()
        if not isinstance(fs, dict) or not 1 <= len(fs) <= MAX_FILES:
            raise _invalid()
        for p, h in fs.items():
            ok = (
                isinstance(p, str)
                and _PATH.fullmatch(p) is not None
                and ".." not in p
                and p.endswith(_ALLOWED_SUFFIXES)
                and isinstance(h, str)
                and _SHA.fullmatch(h) is not None
            )
            if not ok:
                raise _invalid()
        out[role] = PinnedModel(repo, rev, MappingProxyType(dict(fs)))
    return out


def check_config(lock: Mapping[str, PinnedModel]) -> None:
    """The pinned repos are the worker's configured ``DENSE_MODEL`` and ``NLI_MODEL``."""
    from intelligence.config.settings import get_config

    cfg = get_config()
    dense = f"{DENSE_PREFIX}{cfg.DENSE_MODEL}"
    if lock["dense"].repo != dense or lock["nli"].repo != str(cfg.NLI_MODEL):
        raise Refused("not_allowed_here", detail_code="gate_models_config")


def _sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with open(path, "rb", opener=lambda p, f: os.open(p, f | os.O_NOFOLLOW)) as fh:
        while block := fh.read(files.BLOCK_SIZE):
            digest.update(block)
    return digest.hexdigest()


def _found(folder: Path) -> set[str]:
    found: set[str] = set()
    for here, dirs, names in os.walk(folder, followlinks=False):
        rel_dir = Path(here).relative_to(folder)
        if rel_dir == Path("."):
            dirs[:] = [d for d in dirs if d != HF_BOOKKEEPING]
        for d in dirs:
            if os.path.islink(Path(here) / d):
                raise Refused("file_not_allowed", detail_code="gate_models")
        for n in names:
            st = os.lstat(Path(here) / n)
            if not stat.S_ISREG(st.st_mode):
                raise Refused("file_not_allowed", detail_code="gate_models")
            found.add((rel_dir / n).as_posix())
    return found


def verify_role(lock: Mapping[str, PinnedModel], root: Path | str, role: str) -> Path:
    """One role's folder under ``root``, checked file by file (see the module docstring)."""
    folder = Path(root) / role
    if not folder.is_dir() or folder.is_symlink():
        raise Refused("missing_file", detail_code="gate_models")
    listed = lock[role].files
    found = _found(folder)
    if set(listed) - found:
        raise Refused("missing_file", detail_code="gate_models")
    if found - set(listed):
        raise Refused("extra_file", detail_code="gate_models")
    for rel, sha in sorted(listed.items()):
        failed = False
        try:
            got = _sha256(folder / rel)
        except OSError:
            failed = True
        if failed or not secrets.compare_digest(got, sha):
            raise Refused("hash_mismatch", detail_code="gate_models")
    return folder


def verify(lock: Mapping[str, PinnedModel], root: Path | str) -> dict[str, Path]:
    """Each role's folder under ``root``, checked file by file (see the module docstring)."""
    return {role: verify_role(lock, root, role) for role in ROLES}


def paths_from_env(env: Mapping[str, str] | None = None) -> dict[str, Path]:
    """The gate's verified model folders: the committed lock, the configured repos, and
    the folder ``MHAI_ANSWER_GATE_MODELS`` (``no_runtime``/``gate_models`` when unset)."""
    env = os.environ if env is None else env
    root = env.get(DIR_ENV)
    if not root or not Path(root).is_absolute():
        raise Refused("no_runtime", detail_code="gate_models")
    lock = load_lock()
    check_config(lock)
    return verify(lock, root)


def fetch_role(lock: Mapping[str, PinnedModel], root: Path, role: str) -> Path:  # pragma: no cover
    """Download one pinned revision's listed files into ``<root>/<role>``, then verify."""
    from huggingface_hub import snapshot_download

    m = lock[role]
    snapshot_download(
        m.repo,
        revision=m.revision,
        allow_patterns=sorted(m.files),
        local_dir=str(root / role),
    )
    return verify_role(lock, root, role)


def fetch(lock: Mapping[str, PinnedModel], root: Path) -> dict[str, Path]:  # pragma: no cover
    """Download each pinned revision's listed files into ``<root>/<role>``, then verify."""
    for role in ROLES:
        fetch_role(lock, root, role)
    return verify(lock, root)


if __name__ == "__main__":  # pragma: no cover - the promote workflow's fetch step
    import sys

    if len(sys.argv) != 3 or sys.argv[1] != "fetch":
        sys.exit("usage: python -m intelligence.doceval.answers.gate_models fetch DIR")
    fetch(load_lock(), Path(sys.argv[2]))
    print("gate models fetched and verified")

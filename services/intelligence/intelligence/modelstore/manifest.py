"""The model manifest, ``mhai.models.manifest/1``.

One manifest names one promoted version: every file with its size, SHA-256 and the store's
version id, the six documents the version carries, the base model, its dependencies, the
evaluation it passed, the runtime and the promotion run. It is canonical JSON (no floats)
and is what the promotion workflow signs (:mod:`intelligence.modelstore.signing`, purpose
``manifest``) and what ``models.lock`` pins by SHA-256.

The shape::

    schema, name, version, role
    run: {id, source: drive|hf|local, source_ref}
    files: [{path, size, sha256, store_version_id}]          sorted by path
    documents: {<the six DOCUMENTS>: {sha256, store_version_id}}
    base_model: {promoted} | {upstream, revision, licence}   as the training manifest
    dependencies: [{name, version, manifest_sha256}]          sorted by name
    evaluation: {report_sha256, testset: "<set>@<version>#<sha256>", split: dev|test,
                 gates: {gate: pass|fail}}
    runtime: {engine, engine_version, image_digest | null}
    promotion: {workflow_run_id, workflow_ref, commit, actor, at}
    trained_by

Strict both ways: :func:`build` and :func:`parse` hold every value to its pattern (no free
text), refuse unknown or missing keys (``manifest_invalid``), and :func:`parse` refuses
bytes that are not the canonical dump of their own parse (``not_canonical``); a float
anywhere is ``json_invalid``.
"""

import hashlib
import re
from collections.abc import Mapping, Sequence
from dataclasses import dataclass
from datetime import datetime
from types import MappingProxyType
from typing import Any

from intelligence.doceval.report import GATES
from intelligence.modelstore import canonical, files, names, training
from intelligence.modelstore.reasons import Refused

SCHEMA = "mhai.models.manifest/1"
MAX_BYTES = 4 * 1024 * 1024
MAX_FILES = files.Limits().max_files
MAX_DEPENDENCIES = 16
SOURCES = ("drive", "hf", "local")
LOCAL_SOURCE_REF = "fixture"
GATE_RESULTS = ("pass", "fail")
# The split the gates were measured on. ``test`` is the locked split; ``dev`` only ever
# comes from a development rehearsal, and pin (Task 20) refuses it.
SPLITS = ("dev", "test")
DOCUMENTS: tuple[str, ...] = (
    "data_statement.md",
    "evaluation.json",
    "evaluation.md",
    "model_card.md",
    "training_manifest.json",
    "warmup.json",
)

_KEYS = frozenset(
    {
        "schema",
        "name",
        "version",
        "role",
        "run",
        "files",
        "documents",
        "base_model",
        "dependencies",
        "evaluation",
        "runtime",
        "promotion",
        "trained_by",
    }
)
_SHA256 = re.compile(r"[0-9a-f]{64}", re.ASCII)
_COMMIT = re.compile(r"[0-9a-f]{40}", re.ASCII)
_DRIVE_ID = re.compile(r"[A-Za-z0-9_-]{10,128}", re.ASCII)
_REPO_PART = r"[A-Za-z0-9][A-Za-z0-9._-]{0,95}"
_HF_REF = re.compile(rf"(?:{_REPO_PART}/)?{_REPO_PART}@[0-9a-f]{{40}}", re.ASCII)
_TESTSET = re.compile(
    r"[a-z][a-z0-9_]{0,62}@(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})"
    r"#[0-9a-f]{64}",
    re.ASCII,
)
_ENGINE = re.compile(r"[a-z][a-z0-9_-]{0,31}", re.ASCII)
_ENGINE_VERSION = re.compile(
    r"[0-9]{1,4}(?:\.[0-9]{1,4}){0,3}(?:(?:a|b|rc|\.post|\.dev)[0-9]{1,4})?(?:\+[a-z0-9]{1,16})?",
    re.ASCII,
)
_IMAGE_DIGEST = re.compile(r"sha256:[0-9a-f]{64}", re.ASCII)
_RUN_NUMBER = re.compile(r"[1-9][0-9]{0,19}", re.ASCII)
_WORKFLOW_REF = re.compile(
    r"[A-Za-z0-9][A-Za-z0-9._/-]{0,254}@refs/(?:heads|tags)/[A-Za-z0-9][A-Za-z0-9._/-]{0,199}",
    re.ASCII,
)
_TIMESTAMP = re.compile(r"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z", re.ASCII)


def _invalid(detail: str) -> Refused:
    return Refused("manifest_invalid", detail_code=detail)


def _require(ok: bool, detail: str) -> None:
    if not ok:
        raise _invalid(detail)


def _match(value: object, pattern: re.Pattern[str]) -> bool:
    return isinstance(value, str) and pattern.fullmatch(value) is not None


def _is_int(value: object) -> bool:
    return isinstance(value, int) and not isinstance(value, bool)


def _named(check: Any, value: object, detail: str) -> None:
    failed = False
    try:
        check(value)
    except Refused:
        failed = True
    if failed:
        raise _invalid(detail)


def _is_timestamp(value: object) -> bool:
    if not _match(value, _TIMESTAMP):
        return False
    try:
        datetime.strptime(str(value), "%Y-%m-%dT%H:%M:%SZ")
    except ValueError:
        return False
    return True


# ---------------------------------------------------------------- the parts


@dataclass(frozen=True, slots=True)
class Run:
    """The run promoted: ``source_ref`` is a Drive folder id, ``<hub repo>@<commit>``, or
    ``fixture`` for a local run."""

    id: str
    source: str
    source_ref: str

    def __post_init__(self) -> None:
        _named(names.check_run_id, self.id, "run_id")
        _require(self.source in SOURCES, "run_source")
        if self.source == "drive":
            ok = _match(self.source_ref, _DRIVE_ID)
        elif self.source == "hf":
            ok = _match(self.source_ref, _HF_REF) and ".." not in self.source_ref
        else:
            ok = self.source_ref == LOCAL_SOURCE_REF
        _require(ok, "run_source_ref")


@dataclass(frozen=True, slots=True)
class FileRecord:
    path: str
    size: int
    sha256: str
    store_version_id: str

    def __post_init__(self) -> None:
        failed = False
        try:
            files.check_file_name(self.path)
        except Refused:
            failed = True
        _require(not failed, "file_path")
        _require(_is_int(self.size) and 0 <= self.size <= 2**63 - 1, "file_size")
        _require(_match(self.sha256, _SHA256), "file_sha256")
        _named(names.check_store_version_id, self.store_version_id, "file_store_version")


@dataclass(frozen=True, slots=True)
class DocumentRecord:
    sha256: str
    store_version_id: str

    def __post_init__(self) -> None:
        _require(_match(self.sha256, _SHA256), "document_sha256")
        _named(names.check_store_version_id, self.store_version_id, "document_store_version")


@dataclass(frozen=True, slots=True)
class Dependency:
    """Another promoted version this one needs, pinned by its manifest's SHA-256."""

    name: str
    version: str
    manifest_sha256: str

    def __post_init__(self) -> None:
        _named(names.check_name, self.name, "dependency_name")
        _named(names.check_version, self.version, "dependency_version")
        _require(_match(self.manifest_sha256, _SHA256), "dependency_sha256")


@dataclass(frozen=True, slots=True)
class Evaluation:
    """The evaluation report's hash, the test set, the split it was (``dev`` or ``test``)
    and each gate's result.

    ``gates`` names gates from :data:`intelligence.doceval.report.GATES` only."""

    report_sha256: str
    testset: str
    gates: Mapping[str, str]
    split: str

    def __post_init__(self) -> None:
        _require(_match(self.report_sha256, _SHA256), "evaluation_report")
        _require(_match(self.testset, _TESTSET), "evaluation_testset")
        _require(self.split in SPLITS, "evaluation_split")
        _require(isinstance(self.gates, Mapping) and len(self.gates) >= 1, "evaluation_gates")
        for gate, result in self.gates.items():
            _require(gate in GATES and result in GATE_RESULTS, "evaluation_gates")
        object.__setattr__(self, "gates", MappingProxyType(dict(sorted(self.gates.items()))))


@dataclass(frozen=True, slots=True)
class Runtime:
    engine: str
    engine_version: str
    image_digest: str | None

    def __post_init__(self) -> None:
        _require(_match(self.engine, _ENGINE), "runtime_engine")
        _require(_match(self.engine_version, _ENGINE_VERSION), "runtime_engine_version")
        _require(
            self.image_digest is None or _match(self.image_digest, _IMAGE_DIGEST),
            "runtime_image_digest",
        )


@dataclass(frozen=True, slots=True)
class Promotion:
    """The workflow run that promoted the version; ``actor`` dispatched it."""

    workflow_run_id: str
    workflow_ref: str
    commit: str
    actor: str
    at: str

    def __post_init__(self) -> None:
        _require(_match(self.workflow_run_id, _RUN_NUMBER), "promotion_run_id")
        _require(
            _match(self.workflow_ref, _WORKFLOW_REF) and ".." not in self.workflow_ref,
            "promotion_ref",
        )
        _require(_match(self.commit, _COMMIT), "promotion_commit")
        _require(names.is_login(self.actor), "promotion_actor")
        _require(_is_timestamp(self.at), "promotion_at")


Base = training.PromotedBase | training.UpstreamBase


@dataclass(frozen=True, slots=True)
class Manifest:
    """A checked manifest. Construction holds every part to its rules, so an instance
    built in process is as strict as one parsed from bytes."""

    name: str
    version: str
    role: str
    run: Run
    files: tuple[FileRecord, ...]
    documents: Mapping[str, DocumentRecord]
    base_model: Base
    dependencies: tuple[Dependency, ...]
    evaluation: Evaluation
    runtime: Runtime
    promotion: Promotion
    trained_by: str

    def __post_init__(self) -> None:
        _named(names.check_name, self.name, "name")
        _named(names.check_version, self.version, "version")
        _named(names.check_role, self.role, "role")
        _require(isinstance(self.run, Run), "run")
        _require(
            isinstance(self.files, tuple)
            and 1 <= len(self.files) <= MAX_FILES
            and all(isinstance(f, FileRecord) for f in self.files),
            "files",
        )
        paths = [f.path for f in self.files]
        _require(all(a < b for a, b in zip(paths, paths[1:], strict=False)), "files_order")
        _require(
            isinstance(self.documents, Mapping)
            and set(self.documents) == set(DOCUMENTS)
            and all(isinstance(d, DocumentRecord) for d in self.documents.values()),
            "documents",
        )
        object.__setattr__(
            self, "documents", MappingProxyType(dict(sorted(self.documents.items())))
        )
        _require(isinstance(self.base_model, Base), "base_model")
        if isinstance(self.base_model, training.UpstreamBase):
            # A promoted upstream base is pinned to a commit (the training check's rule).
            _require(training.is_commit(self.base_model.revision), "base_revision")
        _require(
            isinstance(self.dependencies, tuple)
            and len(self.dependencies) <= MAX_DEPENDENCIES
            and all(isinstance(d, Dependency) for d in self.dependencies),
            "dependencies",
        )
        deps = [d.name for d in self.dependencies]
        _require(all(a < b for a, b in zip(deps, deps[1:], strict=False)), "dependencies_order")
        _require(isinstance(self.evaluation, Evaluation), "evaluation")
        _require(isinstance(self.runtime, Runtime), "runtime")
        _require(isinstance(self.promotion, Promotion), "promotion")
        _require(names.is_login(self.trained_by), "trained_by")


# ---------------------------------------------------------------- build and dump


def _base_obj(base: Base) -> dict[str, str]:
    if isinstance(base, training.PromotedBase):
        return {"promoted": base.pin}
    return {"upstream": base.upstream, "revision": base.revision, "licence": base.licence}


def to_obj(m: Manifest) -> dict[str, Any]:
    return {
        "schema": SCHEMA,
        "name": m.name,
        "version": m.version,
        "role": m.role,
        "run": {"id": m.run.id, "source": m.run.source, "source_ref": m.run.source_ref},
        "files": [
            {
                "path": f.path,
                "size": f.size,
                "sha256": f.sha256,
                "store_version_id": f.store_version_id,
            }
            for f in m.files
        ],
        "documents": {
            k: {"sha256": d.sha256, "store_version_id": d.store_version_id}
            for k, d in m.documents.items()
        },
        "base_model": _base_obj(m.base_model),
        "dependencies": [
            {"name": d.name, "version": d.version, "manifest_sha256": d.manifest_sha256}
            for d in m.dependencies
        ],
        "evaluation": {
            "report_sha256": m.evaluation.report_sha256,
            "testset": m.evaluation.testset,
            "split": m.evaluation.split,
            "gates": dict(m.evaluation.gates),
        },
        "runtime": {
            "engine": m.runtime.engine,
            "engine_version": m.runtime.engine_version,
            "image_digest": m.runtime.image_digest,
        },
        "promotion": {
            "workflow_run_id": m.promotion.workflow_run_id,
            "workflow_ref": m.promotion.workflow_ref,
            "commit": m.promotion.commit,
            "actor": m.promotion.actor,
            "at": m.promotion.at,
        },
        "trained_by": m.trained_by,
    }


def to_bytes(m: Manifest) -> bytes:
    """The canonical bytes of ``m`` (what is stored, hashed and signed)."""
    _require(isinstance(m, Manifest), "type")
    return canonical.dumps(to_obj(m))


def _sorted_files(items: Sequence[FileRecord]) -> tuple[FileRecord, ...]:
    _require(isinstance(items, Sequence) and not isinstance(items, (str, bytes)), "files")
    out = tuple(items)
    _require(all(isinstance(f, FileRecord) for f in out), "files")
    _require(len({f.path for f in out}) == len(out), "files_duplicate")
    return tuple(sorted(out, key=lambda f: f.path))


def _sorted_dependencies(items: Sequence[Dependency]) -> tuple[Dependency, ...]:
    _require(isinstance(items, Sequence) and not isinstance(items, (str, bytes)), "deps")
    out = tuple(items)
    _require(all(isinstance(d, Dependency) for d in out), "dependencies")
    _require(len({d.name for d in out}) == len(out), "dependencies_duplicate")
    return tuple(sorted(out, key=lambda d: d.name))


def build(
    *,
    name: str,
    version: str,
    role: str,
    run: Run,
    files: Sequence[FileRecord],
    documents: Mapping[str, DocumentRecord],
    base_model: Base,
    dependencies: Sequence[Dependency],
    evaluation: Evaluation,
    runtime: Runtime,
    promotion: Promotion,
    trained_by: str,
) -> bytes:
    """The canonical manifest bytes. Files are sorted by path and dependencies by name;
    a duplicate of either is ``manifest_invalid``."""
    m = Manifest(
        name=name,
        version=version,
        role=role,
        run=run,
        files=_sorted_files(files),
        documents=documents,
        base_model=base_model,
        dependencies=_sorted_dependencies(dependencies),
        evaluation=evaluation,
        runtime=runtime,
        promotion=promotion,
        trained_by=trained_by,
    )
    b = to_bytes(m)
    parse(b)  # what we write is what we would accept
    return b


# ---------------------------------------------------------------- parse


def _dict(obj: object, keys: frozenset[str], detail: str) -> dict[str, Any]:
    _require(isinstance(obj, dict) and set(obj) == keys, detail)
    assert isinstance(obj, dict)
    return obj


def _list(obj: object, detail: str) -> list[Any]:
    _require(isinstance(obj, list), detail)
    assert isinstance(obj, list)
    return obj


_RUN_KEYS = frozenset({"id", "source", "source_ref"})
_FILE_KEYS = frozenset({"path", "size", "sha256", "store_version_id"})
_DOC_KEYS = frozenset({"sha256", "store_version_id"})
_DEP_KEYS = frozenset({"name", "version", "manifest_sha256"})
_EVAL_KEYS = frozenset({"report_sha256", "testset", "split", "gates"})
_RUNTIME_KEYS = frozenset({"engine", "engine_version", "image_digest"})
_PROMOTION_KEYS = frozenset({"workflow_run_id", "workflow_ref", "commit", "actor", "at"})


def _base(obj: object) -> Base:
    _require(isinstance(obj, dict), "base_model")
    assert isinstance(obj, dict)
    if set(obj) == {"promoted"}:
        return training.PromotedBase(obj["promoted"])
    _dict(obj, frozenset({"upstream", "revision", "licence"}), "base_model")
    return training.UpstreamBase(obj["upstream"], obj["revision"], obj["licence"])


def from_obj(obj: object) -> Manifest:
    """A manifest from its parsed JSON; ``manifest_invalid`` on any deviation."""
    o = _dict(obj, _KEYS, "keys")
    _require(o["schema"] == SCHEMA, "schema")
    run = _dict(o["run"], _RUN_KEYS, "run")
    file_objs = [_dict(f, _FILE_KEYS, "files") for f in _list(o["files"], "files")]
    docs = o["documents"]
    _require(isinstance(docs, dict), "documents")
    dep_objs = [_dict(d, _DEP_KEYS, "dependencies") for d in _list(o["dependencies"], "deps")]
    ev = _dict(o["evaluation"], _EVAL_KEYS, "evaluation")
    _require(isinstance(ev["gates"], dict), "evaluation_gates")
    rt = _dict(o["runtime"], _RUNTIME_KEYS, "runtime")
    pr = _dict(o["promotion"], _PROMOTION_KEYS, "promotion")
    return Manifest(
        name=o["name"],
        version=o["version"],
        role=o["role"],
        run=Run(run["id"], run["source"], run["source_ref"]),
        files=tuple(
            FileRecord(f["path"], f["size"], f["sha256"], f["store_version_id"]) for f in file_objs
        ),
        documents={
            k: DocumentRecord(d["sha256"], d["store_version_id"])
            for k, d in ((k, _dict(v, _DOC_KEYS, "documents")) for k, v in docs.items())
        },
        base_model=_base(o["base_model"]),
        dependencies=tuple(
            Dependency(d["name"], d["version"], d["manifest_sha256"]) for d in dep_objs
        ),
        evaluation=Evaluation(ev["report_sha256"], ev["testset"], ev["gates"], ev["split"]),
        runtime=Runtime(rt["engine"], rt["engine_version"], rt["image_digest"]),
        promotion=Promotion(
            pr["workflow_run_id"], pr["workflow_ref"], pr["commit"], pr["actor"], pr["at"]
        ),
        trained_by=o["trained_by"],
    )


def parse(b: bytes) -> Manifest:
    """Parse stored manifest bytes strictly.

    ``too_large`` over :data:`MAX_BYTES`; ``json_invalid`` for anything the strict parser
    refuses (floats included); ``not_canonical`` unless ``b`` is the canonical dump of its
    own parse; then ``manifest_invalid`` for any shape or pattern failure."""
    obj = canonical.loads_strict(b, MAX_BYTES)
    if canonical.dumps(obj) != bytes(b):
        raise Refused("not_canonical")
    return from_obj(obj)


def sha256(b: bytes) -> str:
    """The manifest's hash, as pinned in ``models.lock``: SHA-256 of the stored bytes."""
    return hashlib.sha256(bytes(b)).hexdigest()


__all__ = [
    "DOCUMENTS",
    "GATE_RESULTS",
    "LOCAL_SOURCE_REF",
    "MAX_BYTES",
    "MAX_DEPENDENCIES",
    "MAX_FILES",
    "SCHEMA",
    "SOURCES",
    "SPLITS",
    "Dependency",
    "DocumentRecord",
    "Evaluation",
    "FileRecord",
    "Manifest",
    "Promotion",
    "Run",
    "Runtime",
    "build",
    "from_obj",
    "parse",
    "sha256",
    "to_bytes",
    "to_obj",
]

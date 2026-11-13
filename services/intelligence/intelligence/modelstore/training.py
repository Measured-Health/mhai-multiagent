"""The training manifest (``mhai.models.training/1``) and the dataset and licence registries.

``training_manifest.json`` is written by the training notebook next to the weights. It is
read with :func:`intelligence.modelstore.files.check_json` (no links, the JSON size cap,
the strict parser with finite floats allowed, since ``notebook_metrics`` may be floats:
the file is hashed as raw bytes with the rest of the run, never as canonical JSON).

Strict: exactly these keys, each held to a pattern, so no notebook free text gets in.

* ``schema``: ``mhai.models.training/1``;
* ``name`` (a model name), ``role`` (a role), ``trained_by`` (a GitHub login);
* ``datasets``: 1 to 64 distinct dataset ids, ``<name>@<semver>``;
* ``base_model``: ``{"promoted": "<name>@<version>#<manifest sha256>"}``, or for an upstream
  base being promoted itself, ``{"upstream": "<hub repo>", "revision": "<commit>",
  "licence": "<SPDX id>"}``;
* ``framework``: 1 to 32 packages from the closed set :data:`FRAMEWORKS`, each mapped to
  a PEP 440-style version (``2.5.1``, ``4.57.0.dev0``, ``2.5.1+cu121``);
* ``notebook_metrics``: up to 64 metric names (snake_case, at most 40 characters, no run
  of three or more digits) mapped to numbers (integers or finite floats; informational
  only);
* ``created_at``: ``YYYY-MM-DDTHH:MM:SSZ``.

A shape or pattern failure is ``manifest_invalid``; a missing file is
``training_manifest_missing``.

:func:`check` then holds the manifest to the registries (B16, B17, design decision 10):
``role_unknown`` (a role not in :data:`ROLES`), ``dataset_unknown``,
``dataset_not_synthetic`` (phase 0 admits synthetic datasets only), ``revision_unpinned``
(an upstream revision that is not a 40-hex commit), ``licence_not_allowed``, and for a
promoted base whatever the injected pin verifier raises; the default verifier refuses
(``base_unverified``) so nothing passes unverified.

The registries, ``models/approved-datasets.json`` (``mhai.models.datasets/1``) and
``models/licences.json`` (``mhai.models.licences/1``), are changed only by PR (CODEOWNERS).
A malformed or missing registry is ``policy_invalid``.
"""

import math
import re
from collections.abc import Callable, Mapping
from dataclasses import dataclass
from datetime import datetime
from decimal import ROUND_HALF_EVEN, Decimal
from pathlib import Path
from types import MappingProxyType
from typing import Any, Literal

from intelligence.modelstore import canonical, files, names
from intelligence.modelstore.reasons import Refused

SCHEMA = "mhai.models.training/1"
REGISTRY_SCHEMA = "mhai.models.datasets/1"
LICENCES_SCHEMA = "mhai.models.licences/1"
FILE_NAME = "training_manifest.json"
SYNTHETIC_GENERATOR = "intelligence.doceval.sets"
KINDS = ("synthetic", "deidentified")
ALLOWED_KINDS = frozenset({"synthetic"})  # phase 0 (plan departure 10)
# The roles a training manifest may name: ``extract`` (the document extractor) and ``cca``
# (the answer model, docs/plans/2026-11-13-own-answer-model.md). A role is pinnable only
# once it has an evaluation suite (``lock.ROLES``); ``cca`` gets its suite in that plan's
# PR 2, so until then a ``cca`` run passes these checks but cannot be pinned.
ROLES = frozenset({"extract", "cca"})

MAX_DATASETS = 64
MAX_FRAMEWORK = 32
MAX_METRICS = 64
MAX_REGISTRY_ENTRIES = 1024
MAX_LICENCES = 64
MAX_METRIC_ABS = 10**18
REGISTRY_CAP = 1024 * 1024

_KEYS = frozenset(
    {
        "schema",
        "name",
        "role",
        "trained_by",
        "datasets",
        "base_model",
        "framework",
        "notebook_metrics",
        "created_at",
    }
)
_UPSTREAM_KEYS = frozenset({"upstream", "revision", "licence"})
_DATASET_KEYS = frozenset(
    {"id", "kind", "generator", "version", "testset_disjoint", "approved_by_pr"}
)

_DATASET_NAME = re.compile(r"[a-z0-9][a-z0-9_-]{0,62}", re.ASCII)
_REPO_PART = r"[A-Za-z0-9][A-Za-z0-9._-]{0,95}"
_HUB_REPO = re.compile(rf"(?:{_REPO_PART}/)?{_REPO_PART}", re.ASCII)
_REVISION_TOKEN = re.compile(r"[A-Za-z0-9][A-Za-z0-9._/-]{0,127}", re.ASCII)
_COMMIT = re.compile(r"[0-9a-f]{40}", re.ASCII)
_SPDX = re.compile(r"[A-Za-z0-9][A-Za-z0-9.+-]{0,63}", re.ASCII)
_SHA256 = re.compile(r"[0-9a-f]{64}", re.ASCII)
# Framework names are a closed set and versions a PEP 440 subset, so the card's framework
# list carries no text of the notebook's choosing.
FRAMEWORKS = frozenset(
    {
        "accelerate",
        "datasets",
        "numpy",  # the tiny model's only runtime (doceval.tiny)
        "peft",
        "safetensors",
        "tokenizers",
        "torch",
        "transformers",
        "trl",
        "vllm",
    }
)
_PACKAGE_VERSION = re.compile(
    r"[0-9]{1,4}(?:\.[0-9]{1,4}){0,3}(?:(?:a|b|rc|\.post|\.dev)[0-9]{1,4})?(?:\+[a-z0-9]{1,16})?",
    re.ASCII,
)
# Metric names: snake_case words, at most 40 characters, no run of three or more digits
# (so no identifier, date or record number fits in one).
MAX_METRIC_NAME = 40
_METRIC = re.compile(r"[a-z][a-z0-9]*(?:_[a-z0-9]+)*", re.ASCII)
_DIGIT_RUN = re.compile(r"[0-9]{3}", re.ASCII)
_TIMESTAMP = re.compile(r"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z", re.ASCII)
_MODULE = re.compile(r"[a-z_][a-z0-9_]{0,63}(?:\.[a-z_][a-z0-9_]{0,63}){0,7}", re.ASCII)


def _invalid(detail: str) -> Refused:
    return Refused("manifest_invalid", detail_code=detail)


def _policy(detail: str) -> Refused:
    return Refused("policy_invalid", detail_code=detail)


def _match(value: object, pattern: re.Pattern[str]) -> bool:
    return isinstance(value, str) and pattern.fullmatch(value) is not None


def _require(ok: bool, detail: str) -> None:
    if not ok:
        raise _invalid(detail)


def is_dataset_id(value: object) -> bool:
    """``<name>@<semver>``, for example ``clinical_document-train@1.0.0``."""
    if not isinstance(value, str) or value.count("@") != 1:
        return False
    name, version = value.split("@")
    if _DATASET_NAME.fullmatch(name) is None:
        return False
    try:
        names.check_version(version)
    except Refused:
        return False
    return True


def is_pin(value: object) -> bool:
    """``<model name>@<version>#<manifest sha256>``."""
    if not isinstance(value, str) or value.count("@") != 1 or value.count("#") != 1:
        return False
    name, rest = value.split("@")
    version, sha = rest.split("#")
    try:
        names.check_name(name)
        names.check_version(version)
    except Refused:
        return False
    return _SHA256.fullmatch(sha) is not None


def is_licence_id(value: object) -> bool:
    return _match(value, _SPDX)


def is_commit(value: object) -> bool:
    return _match(value, _COMMIT)


def is_package_version(value: object) -> bool:
    """A framework version as the manifest takes it (a PEP 440 subset)."""
    return _match(value, _PACKAGE_VERSION)


def is_metric_name(value: object) -> bool:
    return (
        _match(value, _METRIC)
        and len(str(value)) <= MAX_METRIC_NAME
        and _DIGIT_RUN.search(str(value)) is None
    )


# ---------------------------------------------------------------- the manifest


@dataclass(frozen=True, slots=True)
class PromotedBase:
    """A base model that is itself a promoted version, named by its pin."""

    pin: str

    def __post_init__(self) -> None:
        _require(is_pin(self.pin), "base_pin")


@dataclass(frozen=True, slots=True)
class UpstreamBase:
    """An upstream base model (a hub repo at a revision), being promoted itself.

    ``revision`` is held to a token pattern here; :func:`check` requires a 40-hex commit.
    """

    upstream: str
    revision: str
    licence: str

    def __post_init__(self) -> None:
        _require(_match(self.upstream, _HUB_REPO) and ".." not in self.upstream, "base_upstream")
        _require(_match(self.revision, _REVISION_TOKEN), "base_revision")
        _require(is_licence_id(self.licence), "base_licence")


Metric = int | float


@dataclass(frozen=True, slots=True)
class TrainingManifest:
    """A parsed ``training_manifest.json``. Every field is an id, an enum or a number:
    construction checks each against its pattern, so no instance can hold free text."""

    name: str
    role: str
    trained_by: str
    datasets: tuple[str, ...]
    base: PromotedBase | UpstreamBase
    framework: tuple[tuple[str, str], ...]
    notebook_metrics: tuple[tuple[str, Metric], ...]
    created_at: str

    def __post_init__(self) -> None:
        for check, value, detail in (
            (names.check_name, self.name, "name"),
            (names.check_role, self.role, "role"),
        ):
            try:
                check(value)
            except Refused:
                raise _invalid(detail) from None
        # Lowercase only (names.check_login, ruling D30): GitHub logins are
        # case-insensitive, so one spelling is stored.
        _require(names.is_login(self.trained_by), "trained_by")
        _require(
            isinstance(self.datasets, tuple)
            and 1 <= len(self.datasets) <= MAX_DATASETS
            and all(is_dataset_id(d) for d in self.datasets)
            and len(set(self.datasets)) == len(self.datasets),
            "datasets",
        )
        _require(isinstance(self.base, PromotedBase | UpstreamBase), "base_model")
        _require(
            isinstance(self.framework, tuple)
            and 1 <= len(self.framework) <= MAX_FRAMEWORK
            and all(
                isinstance(p, tuple)
                and len(p) == 2
                and p[0] in FRAMEWORKS
                and is_package_version(p[1])
                for p in self.framework
            )
            and len({p[0] for p in self.framework}) == len(self.framework),
            "framework",
        )
        _require(
            isinstance(self.notebook_metrics, tuple)
            and len(self.notebook_metrics) <= MAX_METRICS
            and all(
                isinstance(p, tuple) and len(p) == 2 and is_metric_name(p[0]) and _is_metric(p[1])
                for p in self.notebook_metrics
            )
            and len({p[0] for p in self.notebook_metrics}) == len(self.notebook_metrics),
            "notebook_metrics",
        )
        _require(_is_timestamp(self.created_at), "created_at")


REPORT_DECIMALS = Decimal("0.000001")


def report_metrics(tm: TrainingManifest) -> dict[str, int | str]:
    """``notebook_metrics`` as the evaluation report takes them (integers and strings
    only, ruling D26): a non-negative integer as it is; a float, or a negative integer,
    as a signed fixed decimal string with six places (the absolute value quantized
    half-even, then the sign). A value that quantizes to zero is ``0.000000``, never
    ``-0.000000``. A non-finite float never parses in the first place."""
    if not isinstance(tm, TrainingManifest):
        raise _invalid("type")
    out: dict[str, int | str] = {}
    for name, value in tm.notebook_metrics:
        _require(_is_metric(value), "notebook_metrics")
        if isinstance(value, int) and value >= 0:
            out[name] = value
            continue
        exact = Decimal(repr(abs(value))) if isinstance(value, float) else Decimal(abs(value))
        text = format(exact.quantize(REPORT_DECIMALS, ROUND_HALF_EVEN), "f")
        negative = value < 0 and text != "0.000000"
        out[name] = "-" + text if negative else text
    return out


def _is_metric(v: object) -> bool:
    if isinstance(v, bool):
        return False
    if isinstance(v, int):
        return abs(v) <= MAX_METRIC_ABS
    if isinstance(v, float):
        return math.isfinite(v) and abs(v) <= MAX_METRIC_ABS
    return False


def _is_timestamp(v: object) -> bool:
    if not _match(v, _TIMESTAMP):
        return False
    try:
        datetime.strptime(str(v), "%Y-%m-%dT%H:%M:%SZ")
    except ValueError:
        return False
    return True


def _pairs(obj: object, detail: str) -> tuple[tuple[str, Any], ...]:
    _require(isinstance(obj, dict), detail)
    assert isinstance(obj, dict)
    return tuple(sorted(obj.items()))


def _base(obj: object) -> PromotedBase | UpstreamBase:
    _require(isinstance(obj, dict), "base_model")
    assert isinstance(obj, dict)
    if set(obj) == {"promoted"}:
        return PromotedBase(obj["promoted"])
    _require(set(obj) == _UPSTREAM_KEYS, "base_model")
    return UpstreamBase(obj["upstream"], obj["revision"], obj["licence"])


def parse(obj: object) -> TrainingManifest:
    """A training manifest from its parsed JSON; ``manifest_invalid`` on any deviation."""
    _require(isinstance(obj, dict) and set(obj) == _KEYS, "keys")
    assert isinstance(obj, dict)
    _require(obj["schema"] == SCHEMA, "schema")
    datasets = obj["datasets"]
    _require(isinstance(datasets, list), "datasets")
    return TrainingManifest(
        name=obj["name"],
        role=obj["role"],
        trained_by=obj["trained_by"],
        datasets=tuple(datasets),
        base=_base(obj["base_model"]),
        framework=_pairs(obj["framework"], "framework"),
        notebook_metrics=_pairs(obj["notebook_metrics"], "notebook_metrics"),
        created_at=obj["created_at"],
    )


def load(path: Path | str) -> TrainingManifest:
    """Read and parse a ``training_manifest.json`` file (it must have that name)."""
    reason: str | None = None
    try:
        obj = files.check_json(path, FILE_NAME)
    except Refused as exc:
        reason = exc.reason
    if reason == "missing_file":
        raise Refused("training_manifest_missing")
    if reason is not None:
        raise Refused(reason, detail_code="training_manifest")
    return parse(obj)


def load_run(run_dir: Path | str) -> TrainingManifest:
    """The training manifest of a run directory."""
    return load(Path(run_dir) / FILE_NAME)


# ---------------------------------------------------------------- the registries


@dataclass(frozen=True, slots=True)
class Dataset:
    """One registry entry. Construction checks every value (``policy_invalid``), so an
    entry built in process is held to the same rules as one read from the file."""

    id: str
    kind: Literal["synthetic", "deidentified"]
    generator: str | None
    version: str
    testset_disjoint: bool
    approved_by_pr: int

    def __post_init__(self) -> None:
        if not is_dataset_id(self.id) or self.kind not in KINDS:
            raise _policy("dataset_id")
        if self.version != self.id.split("@")[1]:
            raise _policy("dataset_version")
        if self.testset_disjoint is not True:
            raise _policy("dataset_not_disjoint")
        pr = self.approved_by_pr
        if isinstance(pr, bool) or not isinstance(pr, int) or not 1 <= pr <= 10**9:
            raise _policy("dataset_approval")
        if self.kind == "synthetic":
            if not _match(self.generator, _MODULE):
                raise _policy("dataset_generator")
        elif self.generator is not None:
            raise _policy("dataset_generator")


@dataclass(frozen=True, slots=True)
class Registry:
    """The approved datasets by id; construction checks each entry and its key."""

    datasets: Mapping[str, Dataset]

    def __post_init__(self) -> None:
        if not isinstance(self.datasets, Mapping) or len(self.datasets) > MAX_REGISTRY_ENTRIES:
            raise _policy("registry_entries")
        for key, d in self.datasets.items():
            if not isinstance(d, Dataset) or key != d.id:
                raise _policy("registry_entries")


@dataclass(frozen=True, slots=True)
class Licences:
    allowed: frozenset[str]


def _read_policy(path: Path | str) -> Any:
    failed = False
    try:
        raw = files.read_regular(path, REGISTRY_CAP)
        obj = canonical.loads_strict(raw, REGISTRY_CAP, trailing_whitespace=True)
    except Refused:
        failed = True
    if failed:
        raise _policy("read")
    return obj


def _dataset(obj: object) -> Dataset:
    if not isinstance(obj, dict) or set(obj) != _DATASET_KEYS:
        raise _policy("dataset_keys")
    return Dataset(
        obj["id"],
        obj["kind"],
        obj["generator"],
        obj["version"],
        obj["testset_disjoint"],
        obj["approved_by_pr"],
    )


def parse_registry(obj: object) -> Registry:
    if not isinstance(obj, dict) or set(obj) != {"schema", "datasets"}:
        raise _policy("registry_keys")
    if obj["schema"] != REGISTRY_SCHEMA:
        raise _policy("registry_schema")
    entries = obj["datasets"]
    if not isinstance(entries, list) or not 1 <= len(entries) <= MAX_REGISTRY_ENTRIES:
        raise _policy("registry_entries")
    out: dict[str, Dataset] = {}
    for entry in entries:
        d = _dataset(entry)
        if d.id in out:
            raise _policy("registry_duplicate")
        out[d.id] = d
    return Registry(MappingProxyType(dict(sorted(out.items()))))


def load_registry(path: Path | str) -> Registry:
    """``models/approved-datasets.json``."""
    return parse_registry(_read_policy(path))


def parse_licences(obj: object) -> Licences:
    if not isinstance(obj, dict) or set(obj) != {"schema", "licences"}:
        raise _policy("licences_keys")
    if obj["schema"] != LICENCES_SCHEMA:
        raise _policy("licences_schema")
    items = obj["licences"]
    if (
        not isinstance(items, list)
        or not 1 <= len(items) <= MAX_LICENCES
        or not all(is_licence_id(x) for x in items)
        or len(set(items)) != len(items)
    ):
        raise _policy("licences_entries")
    return Licences(frozenset(items))


def load_licences(path: Path | str) -> Licences:
    """``models/licences.json``."""
    return parse_licences(_read_policy(path))


# ---------------------------------------------------------------- the check


def unverified_pin(pin: str) -> None:
    """The default pin verifier: refuses every promoted base (fail closed). The real one
    is :func:`intelligence.modelstore.lock.base_verifier` (Task 22's verifier, the role
    from the base's own manifest, no gate), which ``mhai-models promote`` passes in
    through ``promote.Context.base_verifier`` when it has the reader's credentials."""
    raise Refused("base_unverified")


@dataclass(frozen=True, slots=True)
class TrainingCheck:
    """A manifest that passed :func:`check`, with the registry entries it names.

    ``licence`` is the upstream base's licence; ``None`` for a promoted base, whose
    licence is in its own (verified) manifest."""

    manifest: TrainingManifest
    datasets: tuple[Dataset, ...]
    licence: str | None


def check(
    tm: TrainingManifest | None,
    registry: Registry,
    licences: Licences,
    *,
    verify_pin: Callable[[str], None] = unverified_pin,
) -> TrainingCheck:
    """Hold ``tm`` to the registries; raises :class:`Refused` with a fixed reason."""
    if tm is None:
        raise Refused("training_manifest_missing")
    if not isinstance(tm, TrainingManifest):
        raise _invalid("type")
    if not isinstance(registry, Registry) or not isinstance(licences, Licences):
        raise _policy("type")
    if tm.role not in ROLES:
        raise Refused("role_unknown")
    entries: list[Dataset] = []
    for ident in tm.datasets:
        entry = registry.datasets.get(ident)
        if entry is None:
            raise Refused("dataset_unknown")
        if entry.kind not in ALLOWED_KINDS:
            raise Refused("dataset_not_synthetic")
        entries.append(entry)
    licence: str | None
    if isinstance(tm.base, UpstreamBase):
        if not is_commit(tm.base.revision):
            raise Refused("revision_unpinned")
        if tm.base.licence not in licences.allowed:
            raise Refused("licence_not_allowed")
        licence = tm.base.licence
    else:
        failed = False
        try:
            verify_pin(tm.base.pin)
        except Refused:
            raise
        except Exception:
            failed = True
        if failed:
            raise Refused("base_unverified", detail_code="verifier_error")
        licence = None
    return TrainingCheck(tm, tuple(entries), licence)


__all__ = [
    "ALLOWED_KINDS",
    "FILE_NAME",
    "FRAMEWORKS",
    "KINDS",
    "LICENCES_SCHEMA",
    "REGISTRY_SCHEMA",
    "ROLES",
    "SCHEMA",
    "SYNTHETIC_GENERATOR",
    "Dataset",
    "Licences",
    "PromotedBase",
    "Registry",
    "TrainingCheck",
    "TrainingManifest",
    "UpstreamBase",
    "check",
    "is_commit",
    "is_dataset_id",
    "is_licence_id",
    "is_metric_name",
    "is_package_version",
    "is_pin",
    "load",
    "load_licences",
    "load_registry",
    "load_run",
    "parse",
    "parse_licences",
    "parse_registry",
    "report_metrics",
    "unverified_pin",
]

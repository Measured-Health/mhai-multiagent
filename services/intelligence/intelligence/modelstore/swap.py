"""The checks and the shadow test behind ``deploy/gpu-model/swap-model.sh`` (deploy/README.md,
"Swapping our own model"): ``mhai-models swap <command>``, run in the pipeline image, each
in a container with no secret and no route out.

* ``check-adapter DIR --licences F --datasets F``: the researcher's QLoRA adapter folder,
  read as data only. Every entry is a regular file (no link, no folder) with an allowed
  name (pickle and code names are refused by name); ``adapter_config.json`` is a LoRA
  adapter (no remote-code key) on an allowed base (:data:`BASES`: the repo, its pinned
  revision, its licence and its GGUF architecture); ``adapter_model.safetensors`` passes
  our own header check; ``training_manifest.json`` is a ``cca`` run on that base at that
  revision, under a licence in ``models/licences.json``, trained on datasets in
  ``models/approved-datasets.json``. Nothing in the folder is imported, unpickled or run.
  Prints ``{"adapter": {base, revision, licence, architecture, trained_by, datasets}}``.
* ``check-gguf FILE [--architecture A] [--template-from DIR]``: the GGUF header check
  promotion runs (``files.check_gguf``: ``gguf_invalid`` with a fixed detail), its
  ``general.architecture`` (one of ``files.GGUF_ARCHITECTURES``, and ``A`` when given),
  and, with ``--template-from``, its chat template byte for byte against the adapter's
  (``gguf_template_mismatch``). Prints ``{"gguf": {architecture, size}}``.
* ``eval-shadow --name N --version V --sha256 H --profile P --split dev|test [--set DIR]
  --testsets F [--max-seconds S]``: the ``clinical_answer`` gate against the candidate,
  served by a second gpu-model instance reachable as ``http://gpu-model:8080`` on an
  internal network of its own. The client is the own route's: our own model's generator
  with its alias check (``<N>@<V>#<H[:12]>``) before every item, answers read with the
  answer profile ``P``, retrieval and NLI verification as the worker's. The split is the
  locked one when ``--testsets`` names one (``--split dev`` is then refused), else the dev
  split, rebuilt from its public seed and labelled ``dev``. The verdict is the answer
  gate's thresholds and the median seconds per answer
  (:func:`intelligence.doceval.answers.served.evaluate`). Prints ``{"eval_shadow":
  summary}``, aggregates only, and exits 0 on pass, 1 on fail.

Every refusal is one JSON line ``{"refused": code, "detail": fixed code}`` and exit 2; an
I/O error is ``{"error": "io"}`` and an unexpected one ``{"error": "internal"}``, exit 3.
No file content, path or answer is ever printed.
"""

from __future__ import annotations

import argparse
import dataclasses
import os
import stat
import sys
from collections.abc import Callable, Mapping, Sequence
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from intelligence.modelstore import canonical, files, names, training
from intelligence.modelstore.reasons import Refused


@dataclass(frozen=True)
class Base:
    """An allowed base model: the exact revision adapters are merged into."""

    revision: str
    licence: str
    architecture: str


# The bases an adapter may be merged into (changed only by a pull request). The revision
# is the one the first adapter was trained on (docs/models/intake-2026-11-14-...).
BASES: Mapping[str, Base] = {
    "google/gemma-4-E4B-it": Base(
        revision="4ecc2ba63bcac23ba4a767fc87025b1450f84c9c",
        licence="Apache-2.0",
        architecture="gemma4",
    ),
}

ADAPTER_CONFIG = "adapter_config.json"
ADAPTER_WEIGHTS = "adapter_model.safetensors"
REQUIRED = frozenset(
    {
        ADAPTER_CONFIG,
        ADAPTER_WEIGHTS,
        "tokenizer.json",
        "tokenizer_config.json",
        training.FILE_NAME,
    }
)
# Optional: the rest of what PEFT and the tokenizer save. README.md is allowed and never
# read (PEFT writes one); added_tokens.json is parsed as JSON only.
OPTIONAL = frozenset(
    {
        "special_tokens_map.json",
        "chat_template.json",
        "chat_template.jinja",
        "tokenizer.model",
        "added_tokens.json",
        "README.md",
    }
)
ADAPTER_FILES = REQUIRED | OPTIONAL
MAX_ADAPTER_FILE = 4 * 1024**3
TEMPLATE_CAP = 1024 * 1024
ROLE = "cca"
SPLITS = ("dev", "test")
OWN_MODEL_URL = "http://gpu-model:8080"


def _refused_name(name: str) -> Refused:
    suffix = os.path.splitext(name)[1].lower()
    if suffix in files.PICKLE_SUFFIXES:
        return files.FileRefused("pickle_refused", name)
    if suffix in files.CODE_SUFFIXES:
        return files.FileRefused("code_refused", name)
    return files.FileRefused("file_not_allowed", name, "adapter_file")


def _listing(folder: Path) -> dict[str, int]:
    """The folder's entries: each a regular file with an allowed name (sizes by name)."""
    try:
        st = os.lstat(folder)
    except FileNotFoundError:
        raise Refused("missing_file", detail_code="adapter_dir") from None
    if not stat.S_ISDIR(st.st_mode):
        raise Refused("file_not_allowed", detail_code="adapter_dir")
    found: dict[str, int] = {}
    for name in sorted(os.listdir(folder)):
        st = os.lstat(folder / name)
        if stat.S_ISLNK(st.st_mode):
            raise files.FileRefused("file_not_allowed", name, "link")
        if stat.S_ISDIR(st.st_mode):
            raise files.FileRefused("file_not_allowed", name, "directory")
        if not stat.S_ISREG(st.st_mode) or st.st_nlink != 1:
            raise files.FileRefused("file_not_allowed", name, "not_regular")
        if name not in ADAPTER_FILES:
            raise _refused_name(name)
        if st.st_size > MAX_ADAPTER_FILE:
            raise files.FileRefused("too_large", name)
        found[name] = st.st_size
    missing = sorted(REQUIRED - set(found))
    if missing:
        raise files.FileRefused("missing_file", missing[0], "adapter_file")
    return found


def adapter_template(folder: Path | str) -> str | None:
    """The adapter's chat template as transformers would load it: ``chat_template.json``,
    then ``chat_template.jinja``, then ``tokenizer_config.json``'s field; None if none."""
    folder = Path(folder)
    if (folder / "chat_template.jinja").exists() and not (folder / "chat_template.json").exists():
        raw = files.read_regular(folder / "chat_template.jinja", TEMPLATE_CAP)
        try:
            return raw.decode("utf-8")
        except UnicodeDecodeError:
            raise Refused("json_invalid", detail_code="chat_template") from None
    return training.run_chat_template(folder)


def check_adapter(
    folder: Path | str, licences_path: Path | str, datasets_path: Path | str
) -> dict[str, Any]:
    """The adapter folder's checks (module docstring); returns what the build needs."""
    folder = Path(folder)
    present = _listing(folder)
    licences = training.load_licences(licences_path)
    registry = training.load_registry(datasets_path)

    config = files.check_json(folder / ADAPTER_CONFIG, ADAPTER_CONFIG)
    if not isinstance(config, dict) or config.get("peft_type") != "LORA":
        raise Refused("not_allowed_here", detail_code="adapter_type")
    repo = config.get("base_model_name_or_path")
    base = BASES.get(repo) if isinstance(repo, str) else None
    if base is None or not isinstance(repo, str):
        raise Refused("base_unverified", detail_code="adapter_base")
    if config.get("revision") not in (None, base.revision):
        raise Refused("base_unverified", detail_code="adapter_revision")
    if base.licence not in licences.allowed:
        raise Refused("licence_not_allowed", detail_code="base_licence")

    tm = training.load(folder / training.FILE_NAME)
    training.check(tm, registry, licences)
    if tm.role != ROLE:
        raise Refused("not_allowed_here", detail_code="role")
    if (
        not isinstance(tm.base, training.UpstreamBase)
        or tm.base.upstream != repo
        or tm.base.revision != base.revision
    ):
        raise Refused("base_unverified", detail_code="manifest_base")
    if tm.base.licence != base.licence:
        raise Refused("licence_not_allowed", detail_code="manifest_licence")

    files.check_safetensors(folder / ADAPTER_WEIGHTS)
    for name in sorted(present):
        if name in files.JSON_FILES and name not in (ADAPTER_CONFIG, training.FILE_NAME):
            files.check_json(folder / name, name)
        elif name == "added_tokens.json":
            raw = files.read_regular(folder / name, files.JSON_CAP)
            try:
                canonical.loads_strict(raw, files.JSON_CAP, trailing_whitespace=True)
            except Refused:
                raise files.FileRefused("json_invalid", name) from None
    if adapter_template(folder) is None:
        raise Refused("gguf_template_mismatch", detail_code="run_missing")
    return {
        "base": repo,
        "revision": base.revision,
        "licence": base.licence,
        "architecture": base.architecture,
        "trained_by": tm.trained_by,
        "datasets": list(tm.datasets),
    }


def gguf_architecture(path: Path | str) -> str:
    """``general.architecture`` of a GGUF, after the full header check."""
    files.check_gguf(path)
    with files._parent(Path(path)) as (dfd, name), files._open(name, dfd) as (f, _):
        r = files._GgufReader(f, name, files.GGUF_HEADER_CAP, None)
        r.take(8)  # magic and version, checked above
        r.unpack("<Q")
        for _ in range(r.unpack("<Q")):
            key = files._gguf_name(r, name, "bad_key")
            value = files._gguf_value(r, r.unpack("<I"), name)
            if key == "general.architecture" and isinstance(value, bytes):
                return value.decode("ascii")
    raise files.FileRefused("gguf_invalid", None, "architecture")  # pragma: no cover


def check_gguf(
    path: Path | str, *, architecture: str | None = None, template_from: Path | str | None = None
) -> dict[str, Any]:
    """The GGUF's checks (module docstring)."""
    path = Path(path)
    st = os.lstat(path)
    if not stat.S_ISREG(st.st_mode):
        raise Refused("file_not_allowed", detail_code="gguf_not_regular")
    if not path.name.endswith(files.GGUF_SUFFIX):
        raise Refused("file_not_allowed", detail_code="gguf_name")
    arch = gguf_architecture(path)
    if architecture is not None and arch != architecture:
        raise Refused("gguf_invalid", detail_code="architecture_not_base")
    if template_from is not None:
        expected = adapter_template(template_from)
        served = files.gguf_chat_template(path)
        if served is None:
            raise Refused("gguf_template_mismatch", detail_code="gguf_missing")
        if expected is None:
            raise Refused("gguf_template_mismatch", detail_code="run_missing")
        if served != expected.encode("utf-8"):
            raise Refused("gguf_template_mismatch", detail_code="differs")
    return {"architecture": arch, "size": st.st_size}


# ---------------------------------------------------------------- the shadow test


class _NoMasker:
    """Synthetic questions only: nothing to mask, and nothing leaves the host."""

    def mask(self, text: str) -> tuple[str, list[Any]]:
        return text, []


BuilderFor = Callable[[Any], Callable[[Sequence[Any]], Any]]


def shadow_client(
    alias: str, profile_id: str, settings: Any, *, builder_for: BuilderFor | None = None
) -> Any:
    """The own route's client for ``alias`` (``boundary.models.own_client``'s parts):
    our own model's generator with its alias check, the answer profile, the worker's
    retrieval and verification. ``builder_for`` replaces the builder in tests only."""
    from intelligence.agents import answer_profile
    from intelligence.boundary.models import OrchestratorModelClient, local_parts
    from intelligence.tools import llama_client

    profile = answer_profile.by_id(profile_id)
    if profile is None:
        raise Refused("invalid_name", detail_code="profile")
    gen = llama_client.PinnedLlamaGenerator(llama_client.LlamaSidecar(settings), lambda: alias)
    profile_fn: Callable[[], Any] | None
    if builder_for is None:  # pragma: no cover - loads the retriever and NLI models
        build, _ = local_parts(generator=gen)

        def profile_fn() -> Any:
            return profile

    else:
        build, profile_fn = builder_for(gen), None
    return OrchestratorModelClient(
        build,
        masker=_NoMasker(),
        versions={ROLE: alias},
        preflight=gen.check,
        profile=profile_fn,
    )


def eval_shadow(
    *,
    name: str,
    version: str,
    sha256: str,
    profile: str,
    split: str,
    set_dir: Path | str | None,
    testsets_lock: Path | str,
    max_seconds: int,
    settings: Any,
    builder_for: BuilderFor | None = None,
) -> dict[str, Any]:
    """The shadow test (module docstring)."""
    import tempfile

    from intelligence.doceval import report
    from intelligence.doceval.answers import served

    names.check_name(name)
    names.check_version(version)
    if len(sha256) != 64 or any(c not in "0123456789abcdef" for c in sha256):
        raise Refused("invalid_name", detail_code="sha256")
    alias = f"{name}@{version}#{sha256[:12]}"
    entry = served.locked_entry(testsets_lock)
    if split == "dev" and entry is not None:
        raise Refused("not_allowed_here", detail_code="locked_split_exists")
    if split not in SPLITS or (split == "test") != (set_dir is not None):
        raise Refused("not_allowed_here", detail_code="split")
    client = shadow_client(alias, profile, settings, builder_for=builder_for)
    thresholds = report.packaged_thresholds(report.ANSWER_GATE)
    served.wait_for_alias(settings, alias)
    with tempfile.TemporaryDirectory(prefix="mhai-eval-shadow-") as scratch:
        if split == "test":
            if entry is None:
                raise Refused("not_found", detail_code="testset_lock")
            loaded = served.load_locked(Path(str(set_dir)), entry)
        else:
            loaded = served.load_dev(Path(scratch) / "set")
        out = served.evaluate(
            client, loaded, thresholds, split=split, alias=alias, max_seconds=max_seconds
        )
    out["profile"] = profile
    return out


# ---------------------------------------------------------------- the command line


def _print(obj: dict[str, Any]) -> None:
    sys.stdout.write(canonical.dumps(obj).decode("ascii") + "\n")


def _check_adapter(args: argparse.Namespace) -> int:
    _print({"adapter": check_adapter(args.dir, args.licences, args.datasets)})
    return 0


def _check_gguf(args: argparse.Namespace) -> int:
    out = check_gguf(args.file, architecture=args.architecture, template_from=args.template_from)
    _print({"gguf": out})
    return 0


def _eval_shadow(args: argparse.Namespace) -> int:
    from intelligence.boundary.config import is_development
    from intelligence.doceval.answers import served
    from intelligence.tools.llama_client import settings_from_env

    max_seconds = served.check_max_seconds(args.max_seconds)
    errs: list[str] = []
    settings = settings_from_env(os.environ, is_development(), errs)
    if errs:
        raise Refused("not_allowed_here", detail_code="local_llm_settings")
    settings = dataclasses.replace(settings, url=args.url or OWN_MODEL_URL)
    summary = eval_shadow(
        name=args.name,
        version=args.version,
        sha256=args.sha256,
        profile=args.profile,
        split=args.split,
        set_dir=args.set,
        testsets_lock=args.testsets,
        max_seconds=max_seconds,
        settings=settings,
    )
    _print({"eval_shadow": summary})
    return 0 if summary["verdict"] == "pass" else 1


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="mhai-models swap")
    sub = parser.add_subparsers(dest="command", required=True)
    ca = sub.add_parser("check-adapter")
    ca.add_argument("dir")
    ca.add_argument("--licences", required=True)
    ca.add_argument("--datasets", required=True)
    ca.set_defaults(func=_check_adapter)
    cg = sub.add_parser("check-gguf")
    cg.add_argument("file")
    cg.add_argument("--architecture", default=None)
    cg.add_argument("--template-from", default=None)
    cg.set_defaults(func=_check_gguf)
    es = sub.add_parser("eval-shadow")
    es.add_argument("--name", required=True)
    es.add_argument("--version", required=True)
    es.add_argument("--sha256", required=True)
    es.add_argument("--profile", required=True)
    es.add_argument("--split", required=True, choices=SPLITS)
    es.add_argument("--set", default=None)
    es.add_argument("--testsets", required=True)
    es.add_argument("--max-seconds", default="90")
    # Tests only (a loopback fake); the default is the shadow's fixed name.
    es.add_argument("--url", default=None, help=argparse.SUPPRESS)
    es.set_defaults(func=_eval_shadow)
    return parser


def main(argv: Sequence[str]) -> int:
    """One command; a fixed code on every failure (module docstring)."""
    args = _parser().parse_args(list(argv))
    refusal: Refused | None = None
    error = "internal"
    try:
        result: int = args.func(args)
        return result
    except Refused as exc:
        refusal = exc
    except OSError:
        error = "io"
    except Exception:  # noqa: BLE001 - a fixed code, never the exception's text
        pass
    if refusal is not None:
        out: dict[str, Any] = {"refused": refusal.reason}
        if refusal.detail_code is not None:
            out["detail"] = refusal.detail_code
        _print(out)
        return 2
    _print({"error": error})
    return 3


__all__ = [
    "BASES",
    "Base",
    "adapter_template",
    "check_adapter",
    "check_gguf",
    "eval_shadow",
    "gguf_architecture",
    "main",
    "shadow_client",
]

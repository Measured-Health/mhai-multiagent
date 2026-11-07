"""Static checks on who may touch the cloud path (design section 4.5; K2).

- Only intelligence/boundary/cloud/ imports the providers' SDKs.
- Only boundary/kb_repo.py seals a shared chunk (gate.py defines it).
- The document and correction jobs never import boundary.cloud.

Each rule is checked by parsing the source (AST), so a call or an import
can't hide behind a comment or a string. A self-check makes sure the rules
catch what they are for.
"""

from __future__ import annotations

import ast
from collections.abc import Iterator
from pathlib import Path

import pytest

pytestmark = pytest.mark.disable_socket

ROOT = Path(__file__).resolve().parents[3]
PKG = ROOT / "intelligence"
CLOUD = PKG / "boundary" / "cloud"
SDKS = frozenset({"openai", "anthropic"})
SEALERS = frozenset({PKG / "boundary" / "kb_repo.py", CLOUD / "gate.py"})
NO_CLOUD_JOBS = frozenset({"approve_correction", "ingest_document", "finalise_document"})


def _modules() -> Iterator[Path]:
    yield from sorted(PKG.rglob("*.py"))


def _module_name(path: Path) -> str:
    rel = path.relative_to(ROOT).with_suffix("")
    parts = list(rel.parts)
    if parts[-1] == "__init__":
        parts.pop()
    return ".".join(parts)


def _imported(tree: ast.AST, module: str, is_package: bool) -> Iterator[str]:
    """Every module name an import in tree names, relative ones resolved,
    and for `from a import b` both a and a.b."""
    package = module if is_package else module.rpartition(".")[0]
    for node in ast.walk(tree):
        if isinstance(node, ast.Import):
            for alias in node.names:
                yield alias.name
        elif isinstance(node, ast.ImportFrom):
            if node.level:
                base = package.split(".")
                base = base[: len(base) - (node.level - 1)]
                name = ".".join(base + ([node.module] if node.module else []))
            else:
                name = node.module or ""
            yield name
            for alias in node.names:
                yield f"{name}.{alias.name}"
        elif isinstance(node, ast.Call):
            # importlib.import_module("x") and __import__("x").
            func = node.func
            fname = func.attr if isinstance(func, ast.Attribute) else getattr(func, "id", "")
            if fname in {"import_module", "__import__"} and node.args:
                arg = node.args[0]
                if isinstance(arg, ast.Constant) and isinstance(arg.value, str):
                    yield arg.value


def _imports_sdk(tree: ast.AST, module: str, is_package: bool) -> bool:
    return any(name.split(".")[0] in SDKS for name in _imported(tree, module, is_package))


def _imports_cloud(tree: ast.AST, module: str, is_package: bool) -> bool:
    return any(
        name == "intelligence.boundary.cloud" or name.startswith("intelligence.boundary.cloud.")
        for name in _imported(tree, module, is_package)
    )


def _names(tree: ast.AST, names: frozenset[str]) -> bool:
    """Whether tree names any of names: as a variable, an attribute or an
    imported name."""
    for node in ast.walk(tree):
        if isinstance(node, ast.Name) and node.id in names:
            return True
        if isinstance(node, ast.alias) and node.name.rpartition(".")[2] in names:
            return True
        if isinstance(node, ast.Attribute) and node.attr in names:
            return True
    return False


def _seals(tree: ast.AST) -> bool:
    return _names(tree, frozenset({"seal_shared_chunk"}))


# The gate's private ways to make a tag or a seal without its public paths
# (they would copy a tagged value). Only gate.py may use them.
FORGERS = frozenset({"_seal", "_SEAL_KEY", "_tag_for", "_MASKERS"})


def _forges(tree: ast.AST) -> bool:
    if _names(tree, FORGERS):
        return True
    # A SharedChunk built directly, by name or as gate.SharedChunk.
    for node in ast.walk(tree):
        if isinstance(node, ast.Call):
            f = node.func
            name = (
                f.id if isinstance(f, ast.Name) else f.attr if isinstance(f, ast.Attribute) else ""
            )
            if name == "SharedChunk":
                return True
    return False


def _parse(path: Path) -> ast.AST:
    return ast.parse(path.read_text(encoding="utf-8"), filename=str(path))


def test_only_the_cloud_package_imports_the_sdks() -> None:
    offenders = [
        str(p.relative_to(ROOT))
        for p in _modules()
        if CLOUD not in p.parents
        and _imports_sdk(_parse(p), _module_name(p), p.name == "__init__.py")
    ]
    assert offenders == []


def test_only_kb_repo_seals_shared_chunks() -> None:
    offenders = [
        str(p.relative_to(ROOT)) for p in _modules() if p not in SEALERS and _seals(_parse(p))
    ]
    assert offenders == []
    # kb_repo really is the one that seals (the rule isn't vacuous).
    assert _seals(_parse(PKG / "boundary" / "kb_repo.py"))


def test_only_the_gate_uses_its_private_tag_and_seal_parts() -> None:
    offenders = [
        str(p.relative_to(ROOT))
        for p in _modules()
        if p != CLOUD / "gate.py" and _forges(_parse(p))
    ]
    assert offenders == []
    # gate.py really uses them (the rule isn't vacuous).
    assert _forges(_parse(CLOUD / "gate.py"))


def test_document_and_correction_jobs_never_import_cloud() -> None:
    jobs = [p for p in _modules() if p.stem in NO_CLOUD_JOBS]
    assert any(p.stem == "approve_correction" for p in jobs)
    offenders = [
        str(p.relative_to(ROOT))
        for p in jobs
        if _imports_cloud(_parse(p), _module_name(p), p.name == "__init__.py")
    ]
    assert offenders == []


@pytest.mark.parametrize(
    "source",
    [
        "import openai",
        "import anthropic.types",
        "from openai import OpenAI",
        "from anthropic.types import Message",
        "import importlib\nimportlib.import_module('openai')",
        "__import__('anthropic')",
    ],
)
def test_sdk_rule_catches_every_import_form(source: str) -> None:
    assert _imports_sdk(ast.parse(source), "intelligence.boundary.x", False)


@pytest.mark.parametrize(
    "source",
    [
        "import intelligence.boundary.cloud",
        "import intelligence.boundary.cloud.gate",
        "from intelligence.boundary.cloud import gate",
        "from intelligence.boundary.cloud.gate import MaskedText",
        "from intelligence.boundary import cloud",
        "from .. import cloud",
        "from ..cloud.gate import CloudPrompt",
        "import importlib\nimportlib.import_module('intelligence.boundary.cloud.gate')",
    ],
)
def test_cloud_rule_catches_every_import_form(source: str) -> None:
    # As if written in intelligence/boundary/jobs/approve_correction.py.
    assert _imports_cloud(ast.parse(source), "intelligence.boundary.jobs.approve_correction", False)


@pytest.mark.parametrize(
    "source",
    [
        "from intelligence.boundary.cloud.gate import seal_shared_chunk",
        "from intelligence.boundary.cloud import gate\ngate.seal_shared_chunk('KB#1', 'x', _from_repo=True)",
        "f = seal_shared_chunk",
    ],
)
def test_seal_rule_catches_every_form(source: str) -> None:
    assert _seals(ast.parse(source))


@pytest.mark.parametrize(
    "source",
    [
        "from intelligence.boundary.cloud import gate\ngate._seal('KB#1', 'x')",
        "from intelligence.boundary.cloud.gate import _SEAL_KEY",
        "from intelligence.boundary.cloud import gate\nk = gate._SEAL_KEY",
        "from intelligence.boundary.cloud.gate import SharedChunk\nSharedChunk('KB#1', 'x', b'')",
        "from intelligence.boundary.cloud import gate\ngate.SharedChunk('KB#1', 'x', b'')",
        "tag = masker._tag_for('x')",
        "from intelligence.boundary.cloud.gate import _MASKERS",
    ],
)
def test_forge_rule_catches_every_form(source: str) -> None:
    assert _forges(ast.parse(source))


def test_rules_pass_clean_code() -> None:
    tree = ast.parse("import httpx\nfrom intelligence.boundary import kb_repo, jobs_repo")
    assert not _imports_sdk(tree, "intelligence.boundary.x", False)
    assert not _imports_cloud(tree, "intelligence.boundary.x", False)
    assert not _seals(tree)
    # Naming the type (an isinstance check) is not building one.
    assert not _forges(ast.parse("isinstance(c, SharedChunk)\nx: SharedChunk"))

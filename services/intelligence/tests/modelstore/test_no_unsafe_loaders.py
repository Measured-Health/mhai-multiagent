"""No unsafe loader is used anywhere in the two new packages.

Python files are scanned as parsed code (``ast``), so strings, docstrings and
refusal codes such as ``pickle_refused`` never trip the scan, while aliases,
from-imports and dynamic imports do. Other text files in the packages go through
a regex backstop. A file that doesn't parse counts as a hit (fail closed).
"""

import ast
import re
from pathlib import Path

import pytest

_ROOT = Path(__file__).resolve().parents[2] / "intelligence"
_PACKAGES = ("modelstore", "doceval")

# Modules whose loaders deserialise pickles (or worse) by design.
UNSAFE_MODULES = frozenset(
    {"pickle", "_pickle", "cPickle", "dill", "cloudpickle", "shelve", "marshal", "joblib"}
)
UNSAFE_NAMES = frozenset({"read_pickle"})
UNSAFE_FLAGS = frozenset({"allow_pickle", "trust_remote_code"})
DYNAMIC_IMPORTS = frozenset({"__import__", "import_module"})
# yaml: these always build arbitrary Python objects (or can, with the default loader).
YAML_UNSAFE = frozenset(
    {"unsafe_load", "unsafe_load_all", "full_load", "full_load_all", "UnsafeLoader", "FullLoader"}
)
# yaml.load and yaml.load_all are safe only with an explicit safe loader.
YAML_LOADS = frozenset({"load", "load_all"})
YAML_SAFE_LOADERS = frozenset({"SafeLoader", "CSafeLoader"})


def _root(node: ast.expr) -> str | None:
    while isinstance(node, (ast.Attribute, ast.Call, ast.Subscript)):
        node = node.func if isinstance(node, ast.Call) else node.value
    return node.id if isinstance(node, ast.Name) else None


def _is_false(node: ast.expr) -> bool:
    return isinstance(node, ast.Constant) and node.value in (False, None)


def _flag_key(node: ast.expr | None) -> bool:
    return isinstance(node, ast.Constant) and node.value in UNSAFE_FLAGS


class _Scanner(ast.NodeVisitor):
    def __init__(self) -> None:
        self.hits: list[tuple[int, str]] = []
        self.torch_names: set[str] = {"torch"}
        self.yaml_names: set[str] = {"yaml"}
        self.yaml_load_names: set[str] = set()
        self.safe_loader_names: set[str] = set()
        self.safe_calls: set[int] = set()  # ids of yaml.load nodes called with a safe loader

    def _hit(self, node: ast.AST, why: str) -> None:
        self.hits.append((getattr(node, "lineno", 0), why))

    def visit_Import(self, node: ast.Import) -> None:
        for alias in node.names:
            root = alias.name.split(".")[0]
            if root in UNSAFE_MODULES:
                self._hit(node, f"import {alias.name}")
            if root == "torch":
                self.torch_names.add(alias.asname or "torch")
            if alias.name == "yaml":
                self.yaml_names.add(alias.asname or "yaml")
        self.generic_visit(node)

    def visit_ImportFrom(self, node: ast.ImportFrom) -> None:
        root = (node.module or "").split(".")[0]
        if root in UNSAFE_MODULES:
            self._hit(node, f"from {node.module}")
        for alias in node.names:
            if alias.name in UNSAFE_NAMES:
                self._hit(node, f"import {alias.name}")
            if root == "torch":
                if alias.name == "load" or alias.name == "*":
                    self._hit(node, "from torch import load")
                self.torch_names.add(alias.asname or alias.name)
            if node.module == "yaml":
                if alias.name in YAML_UNSAFE or alias.name in ("*", "Loader"):
                    self._hit(node, f"from yaml import {alias.name}")
                elif alias.name in YAML_LOADS:
                    self.yaml_load_names.add(alias.asname or alias.name)
                elif alias.name in YAML_SAFE_LOADERS:
                    self.safe_loader_names.add(alias.asname or alias.name)
        self.generic_visit(node)

    def _is_yaml(self, node: ast.expr) -> bool:
        return isinstance(node, ast.Name) and node.id in self.yaml_names

    def _is_yaml_load(self, node: ast.expr) -> bool:
        if isinstance(node, ast.Attribute):
            return node.attr in YAML_LOADS and self._is_yaml(node.value)
        return isinstance(node, ast.Name) and node.id in self.yaml_load_names

    def _is_safe_loader(self, node: ast.expr) -> bool:
        if isinstance(node, ast.Attribute):
            return node.attr in YAML_SAFE_LOADERS and self._is_yaml(node.value)
        return isinstance(node, ast.Name) and node.id in self.safe_loader_names

    def visit_Attribute(self, node: ast.Attribute) -> None:
        if node.attr in UNSAFE_NAMES:
            self._hit(node, node.attr)
        if _root(node.value) in UNSAFE_MODULES:
            self._hit(node, "unsafe module attribute")
        if node.attr == "load" and _root(node.value) in self.torch_names:
            self._hit(node, "torch load")
        if node.attr in DYNAMIC_IMPORTS:
            self._hit(node, node.attr)
        if node.attr in YAML_UNSAFE or (node.attr == "Loader" and self._is_yaml(node.value)):
            self._hit(node, f"yaml {node.attr}")
        if self._is_yaml_load(node) and id(node) not in self.safe_calls:
            self._hit(node, "yaml load without SafeLoader")
        self.generic_visit(node)

    def visit_Name(self, node: ast.Name) -> None:
        if node.id in DYNAMIC_IMPORTS or node.id in UNSAFE_NAMES:
            self._hit(node, node.id)
        if self._is_yaml_load(node) and id(node) not in self.safe_calls:
            self._hit(node, "yaml load without SafeLoader")
        self.generic_visit(node)

    def visit_Call(self, node: ast.Call) -> None:
        if self._is_yaml_load(node.func):
            loaders = [kw.value for kw in node.keywords if kw.arg == "Loader"] + node.args[1:2]
            if len(loaders) == 1 and self._is_safe_loader(loaders[0]):
                self.safe_calls.add(id(node.func))
        for kw in node.keywords:
            if kw.arg in UNSAFE_FLAGS and not _is_false(kw.value):
                self._hit(node, f"{kw.arg}=")
        if (
            isinstance(node.func, ast.Name)
            and node.func.id == "getattr"
            and len(node.args) >= 2
            and isinstance(node.args[1], ast.Constant)
            and (
                node.args[1].value in UNSAFE_NAMES
                or node.args[1].value in YAML_UNSAFE
                or (node.args[1].value == "load" and _root(node.args[0]) in self.torch_names)
                or (node.args[1].value in YAML_LOADS and _root(node.args[0]) in self.yaml_names)
            )
        ):
            self._hit(node, "getattr load")
        self.generic_visit(node)

    def visit_Dict(self, node: ast.Dict) -> None:
        for key, value in zip(node.keys, node.values, strict=True):
            if _flag_key(key) and not _is_false(value):
                self._hit(node, "flag in dict")
        self.generic_visit(node)

    def visit_Assign(self, node: ast.Assign) -> None:
        for target in node.targets:
            if (
                isinstance(target, ast.Subscript)
                and _flag_key(target.slice)
                and not _is_false(node.value)
            ):
                self._hit(node, "flag assigned")
        self.generic_visit(node)


# Backstop for text that isn't parsed as Python (templates, JSON, notes).
_TEXT_PATTERNS: tuple[re.Pattern[str], ...] = tuple(
    re.compile(p)
    for p in (
        r"^\s*import\s+[\w\s,.]*(?<!\w)(_pickle|cPickle|pickle|dill|cloudpickle|shelve|marshal|joblib)\b",
        r"^\s*from\s+(_pickle|cPickle|pickle|dill|cloudpickle|shelve|marshal|joblib)\b",
        r"\bread_pickle\b",
        r"\b(__import__|import_module)\s*\(",
        r"^\s*from\s+torch(\.\w+)*\s+import\b.*\bload\b",
        r"\btorch(\.\w+)*\.load\s*\(",
        r"\b(pickle|_pickle|cPickle|dill|cloudpickle|joblib|marshal|shelve)\.\w+\s*\(",
        r"\b(allow_pickle|trust_remote_code)[\"']?\s*[=:]\s*(True|1)\b",
        r"\b(unsafe_load|full_load)(_all)?\s*\(",
        r"\b(UnsafeLoader|FullLoader)\b",
        r"\byaml\.load(_all)?\s*\((?!.*\bLoader\s*=\s*(yaml\.)?C?SafeLoader\b)",
    )
)


def scan_python(source: str) -> list[tuple[int, str]]:
    try:
        tree = ast.parse(source)
    except SyntaxError:
        return [(0, "does not parse")]
    scanner = _Scanner()
    scanner.visit(tree)
    return scanner.hits


def scan_text(text: str) -> list[tuple[int, str]]:
    return [
        (lineno, pattern.pattern)
        for lineno, line in enumerate(text.splitlines(), start=1)
        for pattern in _TEXT_PATTERNS
        if pattern.search(line)
    ]


def find_unsafe(root: Path) -> list[tuple[str, int, str]]:
    hits: list[tuple[str, int, str]] = []
    for path in sorted(p for p in root.rglob("*") if p.is_file()):
        rel = str(path.relative_to(root))
        if path.suffix == ".py":
            found = scan_python(path.read_text(encoding="utf-8"))
        else:
            try:
                text = path.read_bytes().decode("utf-8")
            except UnicodeDecodeError:
                continue  # binary (fonts, images): no source text to scan
            found = scan_text(text)
        hits.extend((rel, lineno, why) for lineno, why in found)
    return hits


@pytest.mark.parametrize("package", _PACKAGES)
def test_no_unsafe_loaders(package: str) -> None:
    root = _ROOT / package
    assert root.is_dir()
    assert any(root.rglob("*.py"))
    assert find_unsafe(root) == []


_PLANTED = [
    "import pickle",
    "import os, pickle",
    "from pickle import loads",
    "x = pickle.loads(b)",
    "import _pickle",
    "from _pickle import loads",
    "import cPickle",
    "import dill",
    "import cloudpickle",
    "import shelve",
    "import marshal",
    "import joblib",
    "from joblib import load",
    "m = __import__('pickle')",
    "m = __import__(name)",
    "import importlib\nm = importlib.import_module('pickle')",
    "from importlib import import_module\nm = import_module('dill')",
    "df = pd.read_pickle(p)",
    "from pandas import read_pickle",
    "state = torch.load(path)",
    "from torch import load",
    "from torch.serialization import load as L",
    "import torch as T\nstate = T.load(p)",
    "m = torch.jit.load(p)",
    "from torch import jit\nm = jit.load(p)",
    "import torch.jit as J\nm = J.load(p)",
    "f = getattr(torch, 'load')",
    "a = numpy.load(p, allow_pickle=True)",
    "a = np.load(p, allow_pickle=1)",
    "AutoModel.from_pretrained(p, trust_remote_code=True)",
    "f(trust_remote_code=1)",
    "f(trust_remote_code=flag)",
    "kw = {'trust_remote_code': True}",
    "f(**{'allow_pickle': True})",
    "kw['trust_remote_code'] = True",
    "def f(:\n",
    # yaml: only yaml.load(..., Loader=yaml.SafeLoader) and yaml.safe_load are safe
    "import yaml\nx = yaml.load(f)",
    "import yaml\nx = yaml.load(f, Loader=yaml.Loader)",
    "import yaml\nx = yaml.load(f, Loader=yaml.FullLoader)",
    "import yaml\nx = yaml.load(f, Loader=yaml.UnsafeLoader)",
    "import yaml\nx = yaml.load(f, yaml.Loader)",
    "import yaml\nx = yaml.load(f, Loader=loader)",
    "import yaml\nx = yaml.load_all(f)",
    "import yaml\nx = yaml.unsafe_load(f)",
    "import yaml\nx = yaml.full_load(f)",
    "import yaml\nx = yaml.unsafe_load_all(f)",
    "import yaml\nx = yaml.full_load_all(f)",
    "import yaml as Y\nx = Y.load(f)",
    "import yaml as Y\nx = Y.unsafe_load(f)",
    "from yaml import load\nx = load(f)",
    "from yaml import load as L\nx = L(f)",
    "from yaml import unsafe_load",
    "from yaml import full_load as fl",
    "from yaml import *",
    "import yaml\nf = getattr(yaml, 'unsafe_load')",
    "import yaml\nf = getattr(yaml, 'load')",
    "import yaml\nf = yaml.load",
]


@pytest.mark.parametrize("snippet", _PLANTED)
def test_scanner_catches_a_planted_use(tmp_path: Path, snippet: str) -> None:
    pkg = tmp_path / "pkg"
    (pkg / "sub").mkdir(parents=True)
    (pkg / "ok.py").write_text('"""Clean."""\n', encoding="utf-8")
    (pkg / "sub" / "bad.py").write_text(f'"""Planted."""\n\n{snippet}\n', encoding="utf-8")
    hits = find_unsafe(pkg)
    assert hits, snippet
    assert {h[0] for h in hits} == {"sub/bad.py"}


@pytest.mark.parametrize(
    "line",
    [
        "import pickle",
        "from cloudpickle import loads",
        "state = torch.load(path)",
        "x = pd.read_pickle(p)",
        '"trust_remote_code": true, "allow_pickle": 1',
        "trust_remote_code = True",
        "x = yaml.load(f)",
        "x = yaml.unsafe_load(f)",
        "x = yaml.full_load(f)",
        "x = yaml.load(f, Loader=yaml.FullLoader)",
    ],
)
def test_text_backstop_catches_a_planted_use(tmp_path: Path, line: str) -> None:
    pkg = tmp_path / "pkg"
    pkg.mkdir()
    (pkg / "notes.txt").write_text(f"header\n{line}\n", encoding="utf-8")
    (pkg / "font.ttf").write_bytes(b"\xff\xfe\x00import pickle")
    hits = find_unsafe(pkg)
    assert hits
    assert {(h[0], h[1]) for h in hits} == {("notes.txt", 2)}


def test_scanner_ignores_the_bare_word_and_refusal_codes(tmp_path: Path) -> None:
    pkg = tmp_path / "pkg"
    pkg.mkdir()
    (pkg / "codes.py").write_text(
        '"""Refuses pickle, joblib and torch files; never calls torch.load,\n'
        'pickle.loads or pd.read_pickle, and never sets trust_remote_code=True."""\n'
        "import json\n"
        "import numpy as np\n"
        'REASON = "pickle_refused"\n'
        '_EXT = {".pickle": "pickle_refused", ".joblib": "pickle_refused"}\n'
        '_KEYS = frozenset({"auto_map", "trust_remote_code", "allow_pickle"})\n'
        "# trust_remote_code is refused in config files\n"
        'if key == "trust_remote_code":\n'
        "    pass\n"
        "a = np.load(p)\n"
        "b = json.load(f)\n"
        "c = f(trust_remote_code=False, allow_pickle=None)\n"
        'd = {"trust_remote_code": False}\n'
        "import yaml\n"
        "e = yaml.safe_load(f)\n"
        "g = yaml.load(f, Loader=yaml.SafeLoader)\n"
        "h = yaml.load(f, Loader=yaml.CSafeLoader)\n"
        "i = yaml.load_all(f, Loader=yaml.SafeLoader)\n"
        "j = yaml.safe_load_all(f)\n"
        "from yaml import SafeLoader\n"
        "k = yaml.load(f, Loader=SafeLoader)\n"
        'REFUSED = ("unsafe_load", "full_load")\n',
        encoding="utf-8",
    )
    (pkg / "safe.yaml.txt").write_text(
        "x = yaml.safe_load(f)\ny = yaml.load(f, Loader=yaml.SafeLoader)\n", encoding="utf-8"
    )
    (pkg / "names.txt").write_text("Marshall\nPickles\nShelvey\n", encoding="utf-8")
    assert find_unsafe(pkg) == []

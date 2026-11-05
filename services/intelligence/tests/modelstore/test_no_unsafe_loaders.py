"""No unsafe loader is used anywhere in the two new packages.

The scan matches uses (an import, an attribute access, a keyword argument), not
the bare word, so docstrings and refusal codes such as ``pickle_refused`` pass.
"""

import re
from pathlib import Path

import pytest

_ROOT = Path(__file__).resolve().parents[2] / "intelligence"
_PACKAGES = ("modelstore", "doceval")

UNSAFE: tuple[re.Pattern[str], ...] = tuple(
    re.compile(p)
    for p in (
        r"^\s*import\s+[\w\s,.]*\bpickle\b",
        r"^\s*from\s+pickle\b",
        r"\bpickle\.\w",
        r"\btorch\.load\b",
        r"^\s*import\s+[\w\s,.]*\bjoblib\b",
        r"^\s*from\s+joblib\b",
        r"\bjoblib\.\w",
        r"\ballow_pickle\s*=\s*True\b",
        r"\btrust_remote_code\s*=\s*True\b",
    )
)


def find_unsafe(root: Path) -> list[tuple[str, int, str]]:
    hits: list[tuple[str, int, str]] = []
    for path in sorted(root.rglob("*.py")):
        text = path.read_text(encoding="utf-8")
        for lineno, line in enumerate(text.splitlines(), start=1):
            for pattern in UNSAFE:
                if pattern.search(line):
                    hits.append((str(path.relative_to(root)), lineno, pattern.pattern))
    return hits


@pytest.mark.parametrize("package", _PACKAGES)
def test_no_unsafe_loaders(package: str) -> None:
    root = _ROOT / package
    assert root.is_dir()
    assert any(root.rglob("*.py"))
    assert find_unsafe(root) == []


@pytest.mark.parametrize(
    "line",
    [
        "import pickle",
        "import os, pickle",
        "from pickle import loads",
        "x = pickle.loads(b)",
        "state = torch.load(path)",
        "import joblib",
        "from joblib import load",
        "m = joblib.load(p)",
        "a = numpy.load(p, allow_pickle=True)",
        "AutoModel.from_pretrained(p, trust_remote_code=True)",
    ],
)
def test_scanner_catches_a_planted_use(tmp_path: Path, line: str) -> None:
    pkg = tmp_path / "pkg"
    pkg.mkdir()
    (pkg / "ok.py").write_text('"""Clean."""\n', encoding="utf-8")
    (pkg / "sub").mkdir()
    (pkg / "sub" / "bad.py").write_text(f'"""Planted."""\n\n{line}\n', encoding="utf-8")
    hits = find_unsafe(pkg)
    assert [(h[0], h[1]) for h in hits][:1] == [("sub/bad.py", 3)]


def test_scanner_ignores_the_bare_word_and_refusal_codes(tmp_path: Path) -> None:
    pkg = tmp_path / "pkg"
    pkg.mkdir()
    (pkg / "codes.py").write_text(
        '"""Refuses pickle, joblib and torch files; never loads them."""\n'
        'REASON = "pickle_refused"\n'
        '_EXT = {".pickle": "pickle_refused", ".joblib": "pickle_refused"}\n'
        "# trust_remote_code is refused in config files\n"
        'if key == "trust_remote_code":\n'
        "    pass\n",
        encoding="utf-8",
    )
    assert find_unsafe(pkg) == []

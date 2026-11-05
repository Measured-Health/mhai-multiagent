"""Property test: real-looking numbers glued with any separators are always refused."""

import copy
import json
import random
from pathlib import Path
from typing import Any

from intelligence.doceval.synth import patients, scanner

SEPARATOR_CHARS = " -./()–—_"
WORDS = (
    "patient",
    "reports",
    "cough",
    "and",
    "fever",
    "seen",
    "in",
    "clinic",
    "review",
    "contact",
    "ref",
    "call",
    "follow",
    "up",
    "plan",
)
CASES = 2000


def _digits(rng: random.Random, n: int) -> str:
    return "".join(str(rng.randrange(10)) for _ in range(n))


def _number(rng: random.Random) -> tuple[str, str]:
    """A real-looking identifier as (kind, digit-and-letter payload to be glued)."""
    kind = rng.choice(("phone0", "phone+234", "phone234", "nin", "bvn", "luth"))
    if kind == "phone0":
        return kind, "0" + rng.choice("789") + rng.choice("01") + _digits(rng, 8)
    if kind.startswith("phone"):
        prefix = "234"
        return kind, prefix + rng.choice("789") + rng.choice("01") + _digits(rng, 8)
    if kind in ("nin", "bvn"):
        return kind, _digits(rng, 11)
    letters = "".join(rng.choice("ABCDEFGHIJKLMNOPQRSTUVWXYZ") for _ in range(rng.randint(3, 5)))
    return kind, f"{letters}\x00{rng.randint(1990, 2026)}\x00{_digits(rng, 5)}"


def _sep(rng: random.Random) -> str:
    return "".join(rng.choice(SEPARATOR_CHARS) for _ in range(rng.randint(0, 5)))


def _glue(rng: random.Random, kind: str, payload: str) -> str:
    if kind == "luth":
        a, b, c = payload.split("\x00")
        return a + _sep(rng) + b + _sep(rng) + c
    out = ""
    i = 0
    while i < len(payload):
        size = rng.randint(2, 5)
        out += payload[i : i + size] + (_sep(rng) if i + size < len(payload) else "")
        i += size
    return ("+" if kind == "phone+234" else "") + out


def _case(rng: random.Random) -> str:
    kind, payload = _number(rng)
    return _glue(rng, kind, payload)


def _wrap(rng: random.Random, value: str) -> str:
    before = " ".join(rng.choice(WORDS) for _ in range(rng.randint(0, 4)))
    after = " ".join(rng.choice(WORDS) for _ in range(rng.randint(0, 4)))
    return f"{before} {value} {after}".strip()


def _doc() -> dict[str, Any]:
    for p in patients.generate(random.Random(21), 40):
        for d in p.documents:
            if d["observations"]:
                return copy.deepcopy(d)
    raise AssertionError


def _meds() -> list[dict[str, Any]]:
    return [
        m
        for p in patients.generate(random.Random(21), 40)
        for d in p.documents
        for m in d["medications"]
    ][:1]


def _put(doc: dict[str, Any], where: str, text: str) -> dict[str, Any]:
    out = copy.deepcopy(doc)
    if where == "note":
        field = copy.deepcopy(out["document"]["language"])
        field["value"], field["raw_text"] = text, None
        out["notes"] = [{"heading": copy.deepcopy(field), "text": field}]
    elif where == "value_text":
        out["observations"][0]["value_text"]["value"] = text
    elif where == "reference_text":
        out["observations"][0]["reference_text"]["value"] = text
    else:
        out["medications"] = _meds()
        out["medications"][0]["dose_text"]["value"] = text
    return out


def _leaks(findings: list[scanner.Finding], text: str) -> bool:
    dump = repr(findings) + "".join(str(f) for f in findings)
    compact = "".join(c for c in text if c.isalnum())
    return text.strip() in dump or (len(compact) >= 8 and compact in dump)


def test_glued_numbers_always_flagged(tmp_path: Path) -> None:
    rng = random.Random(20261104)
    base = _doc()
    for i in range(CASES):
        value = _case(rng)
        text = _wrap(rng, value)
        field = rng.choice(("note", "value_text", "reference_text", "dose_text", "meta", "key"))
        if field in ("note", "value_text", "reference_text", "dose_text"):
            found = scanner.check_document(_put(base, field, text))
        elif field == "meta":
            found = scanner.check_metadata({"note": text})
        else:
            found = scanner.check_metadata({value: "x"})
        assert found, (i, field)
        assert not _leaks(found, text) and not _leaks(found, value), (i, field)
        if i % 10 == 0:
            name = value.replace("/", "_") + ".json"
            case_dir = tmp_path / f"c{i}"
            case_dir.mkdir()
            (case_dir / name).write_text("{}", encoding="utf-8")
            res = scanner.scan_set(case_dir)
            assert any(f.rule == "filename" for f in res.findings), (i, "filename")
            assert not _leaks(list(res.findings), value), (i, "filename")


def test_corner_cases_flagged() -> None:
    for text in (
        "0803 555-0123",
        "call 0803 555-0123 now",
        "08035550123",
        "555-0123 0803",
        "08035550123456",
        "123 08031234567",
        "2026-03-02 08031234567",
        "SYN-00000001 0803 123 4567",
    ):
        assert scanner.check_metadata({"note": text}), text
        assert scanner.check_document(_put(_doc(), "dose_text", text)), text


def test_no_false_positives_on_synthetic_text() -> None:
    for text in (
        "Ref SYN-00000042, SYNID-12345678 on 2026-03-02 10:15:00",
        "seen 2026-03-01 - 2026-03-05, tel 555-0142",
        "12-03-2026, 12.03.2026 and 12/03/2026",
        "14 Kestrel Lane; Tel 555-0142",
    ):
        assert scanner.check_metadata({"note": text}) == [], text
    for p in patients.generate(random.Random(77), 400):
        for d in p.documents:
            assert scanner.check_document(d) == []
    assert json.loads(json.dumps(_doc()))

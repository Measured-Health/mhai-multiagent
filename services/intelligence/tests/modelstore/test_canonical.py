import pytest

from intelligence.modelstore import canonical
from intelligence.modelstore.reasons import Refused


def test_canonical_round_trip() -> None:
    obj = {"b": [1, "x", None, True], "a": {"z": 1, "y": "é"}}
    raw = canonical.dumps(obj)
    assert raw == b'{"a":{"y":"\\u00e9","z":1},"b":[1,"x",null,true]}'
    assert canonical.loads_strict(raw, 1000) == obj
    assert canonical.is_canonical(raw)


@pytest.mark.parametrize(
    "raw",
    [
        b'{"a":1.5}',
        b'{"a":1e3}',
        b'{"a":NaN}',
        b'{"a":Infinity}',
        b'{"a":1,"a":2}',
        b'{"a":{"b":1,"b":1}}',
        b'{"a":1} ',
        b'{"a":1}{"b":2}',
        b'{"a":1}x',
        b'\xef\xbb\xbf{"a":1}',
        b" {}",
        b"\xff\xfe",
        b"",
        b"[" * 100000,
        b"[" * 5000 + b"]" * 5000,
        b'{"a":-0.0}',
        b'{"a":1E5}',
    ],
)
def test_canonical_refuses_floats_duplicates_trailing_bom(raw: bytes) -> None:
    with pytest.raises(Refused) as exc:
        canonical.loads_strict(raw, 1_000_000)
    assert str(exc.value) in {"json_invalid", "too_large"}
    assert not canonical.is_canonical(raw)


def test_too_large() -> None:
    with pytest.raises(Refused) as exc:
        canonical.loads_strict(b'{"a":"' + b"x" * 100 + b'"}', 10)
    assert str(exc.value) == "too_large"


@pytest.mark.parametrize("obj", [{"a": 1.5}, {1: "x"}, {"a": float("nan")}, {"a": {1, 2}}, 1.0])
def test_dumps_refuses(obj: object) -> None:
    with pytest.raises(Refused):
        canonical.dumps(obj)


def test_not_canonical_forms() -> None:
    assert not canonical.is_canonical(b'{"b":1,"a":2}')
    assert not canonical.is_canonical(b'{"a": 1}')
    assert not canonical.is_canonical(b'{"a":"\xc3\xa9"}')
    assert not canonical.is_canonical(b'{"a":-0}')


def test_nesting_depth_limit_is_pinned() -> None:
    deep64 = b"[" * 64 + b"]" * 64
    deep65 = b"[" * 65 + b"]" * 65
    canonical.loads_strict(deep64, 10_000)
    with pytest.raises(Refused) as exc:
        canonical.loads_strict(deep65, 10_000)
    assert str(exc.value) == "json_invalid"


def test_trailing_whitespace_only_when_asked() -> None:
    with pytest.raises(Refused):
        canonical.loads_strict(b'{"a":1}\n', 100)
    assert canonical.loads_strict(b'{"a":1}\n', 100, trailing_whitespace=True) == {"a": 1}
    for raw in (b'{"a":1}\n{}', b'{"a":1} x', b' {"a":1}'):
        with pytest.raises(Refused):
            canonical.loads_strict(raw, 100, trailing_whitespace=True)
    assert not canonical.is_canonical(b'{"a":1}\n')

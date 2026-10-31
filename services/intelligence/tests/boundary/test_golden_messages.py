"""Golden protobuf messages shared with the Go tests.

Go writes <Name>.go.bin and this file writes <Name>.py.bin (set
MHAI_UPDATE_GOLDEN=1). Both sides decode every .bin and compare it with
<Name>.json. All values are synthetic.
"""

import os
from pathlib import Path

import pytest
from google.protobuf import json_format
from google.protobuf.message import Message

from intelligence.boundary.pb import authz_pb2, echo_pb2, envelope_pb2, pipeline_pb2

GOLDEN = Path(__file__).resolve().parents[4] / "testdata" / "proto-golden"

MESSAGES: dict[str, type[Message]] = {}
for _mod in (envelope_pb2, pipeline_pb2, echo_pb2, authz_pb2):
    for _name, _desc in _mod.DESCRIPTOR.message_types_by_name.items():
        MESSAGES[_name] = getattr(_mod, _name)

EXPECTED = {
    "Error",
    "AuditFact",
    "ResponseMeta",
    "DeadLetter",
    "RunQueryJob",
    "HealthRequest",
    "HealthResponse",
    "PingRequest",
    "PingResponse",
    "GrantRequest",
    "GrantResponse",
    "GrantRefused",
}


def _from_json(name: str) -> Message:
    return json_format.Parse((GOLDEN / f"{name}.json").read_text(), MESSAGES[name]())


def test_all_messages_are_covered() -> None:
    assert set(MESSAGES) == EXPECTED


@pytest.mark.skipif(
    os.environ.get("MHAI_UPDATE_GOLDEN") != "1", reason="set MHAI_UPDATE_GOLDEN=1 to write"
)
def test_write_golden_messages() -> None:
    for name in sorted(EXPECTED):
        data = _from_json(name).SerializeToString(deterministic=True)
        (GOLDEN / f"{name}.py.bin").write_bytes(data)


@pytest.mark.parametrize("name", sorted(EXPECTED))
def test_golden_messages_decode(name: str) -> None:
    want = _from_json(name)
    bins = sorted(GOLDEN.glob(f"{name}.*.bin"))
    assert {p.name for p in bins} >= {f"{name}.go.bin"}, "run the Go test with -update first"
    for path in bins:
        got = MESSAGES[name]()
        got.ParseFromString(path.read_bytes())
        assert got == want, path.name


def test_golden_directory_has_no_strays() -> None:
    for path in GOLDEN.iterdir():
        stem, _, rest = path.name.partition(".")
        assert stem in EXPECTED, path.name
        assert rest in {"json", "go.bin", "py.bin"}, path.name


def test_error_has_no_text_fields() -> None:
    """Error can't grow a free-text field silently: adding one fails here."""
    desc = envelope_pb2.Error.DESCRIPTOR
    assert {f.name for f in desc.fields} == {"code", "retryable"}
    assert desc.fields_by_name["code"].type == desc.fields_by_name["code"].TYPE_STRING
    assert desc.fields_by_name["retryable"].type == desc.fields_by_name["retryable"].TYPE_BOOL

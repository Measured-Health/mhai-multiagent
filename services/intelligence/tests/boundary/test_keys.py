"""The trusted-keys file (spec §7.5): at most two keys, from the mounted file
only, re-read on SIGHUP, the old set kept when the new file is bad."""

from __future__ import annotations

import json
import logging
import os
import signal
from pathlib import Path

import pytest

from intelligence.boundary.keys import TrustedKeys, TrustedKeysError, install_sighup_reload
from tests.boundary.tokens import Signer, b64u


def write(path: Path, obj: object) -> Path:
    path.write_text(obj if isinstance(obj, str) else json.dumps(obj))
    return path


def keys_of(*signers: Signer) -> dict[str, str]:
    out: dict[str, str] = {}
    for s in signers:
        out.update(s.encoded_keys())
    return out


A, B, C = Signer(), Signer(seed=b"\x02" * 32), Signer(seed=b"\x03" * 32)


def test_trusted_keys_load(tmp_path: Path) -> None:
    t = TrustedKeys.load(write(tmp_path / "k.json", {"keys": keys_of(A, B)}))
    assert t.kids() == {A.real_kid, B.real_kid}
    assert t.get(A.real_kid) is not None and t.get("nope") is None


@pytest.mark.parametrize(
    "content",
    [
        "",
        "{}",
        '{"keys":{}}',
        "null",
        '{"keys":null}',
        json.dumps({"keys": keys_of(A, B, C)}),  # more than two
        json.dumps({"keys": {A.real_kid: b64u(A.pub[:31])}}),  # not 32 bytes
        json.dumps({"keys": {"0000000000000000": b64u(A.pub)}}),  # kid not the key's
        json.dumps({"keys": {A.real_kid: b64u(A.pub) + "="}}),  # padding
        json.dumps({"keys": {A.real_kid: 7}}),
        json.dumps({"keys": keys_of(A), "extra": 1}),
        '{"keys":{"'
        + A.real_kid
        + '":"'
        + b64u(A.pub)
        + '","'
        + A.real_kid
        + '":"'
        + b64u(A.pub)
        + '"}}',
        '{"keys":{},"keys":' + json.dumps(keys_of(A)) + "}",
        '{"KEYS":' + json.dumps(keys_of(A)) + "}",
        json.dumps({"keys": keys_of(A)}) + "x",
        json.dumps({"keys": keys_of(A)}) + " " * (16 << 10),  # over 16 KiB
    ],
)
def test_trusted_keys_limits(tmp_path: Path, content: str) -> None:
    with pytest.raises(TrustedKeysError) as e:
        TrustedKeys.load(write(tmp_path / "k.json", content))
    assert str(tmp_path) not in str(e.value)


def test_trusted_keys_missing_file(tmp_path: Path) -> None:
    with pytest.raises(TrustedKeysError):
        TrustedKeys.load(tmp_path / "absent.json")


def test_from_encoded_applies_the_same_rules() -> None:
    assert TrustedKeys.from_encoded(keys_of(A)).kids() == {A.real_kid}
    with pytest.raises(TrustedKeysError):
        TrustedKeys.from_encoded(keys_of(A, B, C))
    with pytest.raises(TrustedKeysError):
        TrustedKeys.from_encoded({})


def test_reload_keeps_old_set_on_bad_file(tmp_path: Path, caplog: pytest.LogCaptureFixture) -> None:
    path = write(tmp_path / "k.json", {"keys": keys_of(A)})
    t = TrustedKeys.load(path)
    write(path, {"keys": keys_of(A, B, C)})
    with caplog.at_level(logging.WARNING, logger="intelligence.boundary"):
        assert t.reload(path) is False
    assert t.kids() == {A.real_kid}
    assert t.reload_failures == 1
    assert len(caplog.records) == 1 and str(tmp_path) not in caplog.records[0].getMessage()
    path.unlink()
    assert t.reload(path) is False and t.kids() == {A.real_kid}


def test_reload_drops_removed_kid(tmp_path: Path) -> None:
    path = write(tmp_path / "k.json", {"keys": keys_of(A, B)})
    t = TrustedKeys.load(path)
    write(path, {"keys": keys_of(B)})
    assert t.reload(path) is True
    assert t.get(A.real_kid) is None and t.get(B.real_kid) is not None


def test_sighup_reloads(tmp_path: Path) -> None:
    path = write(tmp_path / "k.json", {"keys": keys_of(A)})
    t = TrustedKeys.load(path)
    previous = install_sighup_reload(t, path)
    try:
        write(path, {"keys": keys_of(B)})
        os.kill(os.getpid(), signal.SIGHUP)
        assert t.kids() == {B.real_kid}
    finally:
        signal.signal(signal.SIGHUP, previous)

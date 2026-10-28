"""The trusted-keys file (spec §7.5): at most two keys, from the mounted file
only, re-read on SIGHUP, the old set kept when the new file is bad."""

from __future__ import annotations

import asyncio
import json
import logging
import os
import signal
import threading
from collections.abc import Callable
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


def test_sighup_reloads_from_the_event_loop(tmp_path: Path) -> None:
    path = write(tmp_path / "k.json", {"keys": keys_of(A)})
    t = TrustedKeys.load(path)
    loop = asyncio.new_event_loop()
    try:
        install_sighup_reload(t, path, loop)
        write(path, {"keys": keys_of(B)})
        os.kill(os.getpid(), signal.SIGHUP)
        loop.run_until_complete(asyncio.sleep(0.05))
        assert t.kids() == {B.real_kid}
    finally:
        loop.remove_signal_handler(signal.SIGHUP)
        loop.close()


def _in_thread(fn: Callable[[], object], timeout: float = 5.0) -> object:
    out: list[object] = []
    th = threading.Thread(target=lambda: out.append(fn()), daemon=True)
    th.start()
    th.join(timeout)
    assert not th.is_alive(), "hung"
    return out[0]


def test_reads_never_wait_on_a_reload(tmp_path: Path) -> None:
    """A reload in progress (its lock held) blocks neither get() nor kids()."""
    path = write(tmp_path / "k.json", {"keys": keys_of(A)})
    t = TrustedKeys.load(path)
    with t._reload_lock:
        assert _in_thread(lambda: t.get(A.real_kid)) is not None
        assert _in_thread(t.kids) == {A.real_kid}


def test_reload_during_reload_does_not_hang_and_is_not_lost(tmp_path: Path) -> None:
    path = write(tmp_path / "k.json", {"keys": keys_of(A)})
    t = TrustedKeys.load(path)
    started, release = threading.Event(), threading.Event()
    real = t._reload_once

    def slow(p: str | Path) -> bool:
        started.set()
        assert release.wait(5)
        return real(p)

    t._reload_once = slow  # type: ignore[method-assign]
    first = threading.Thread(target=t.reload, args=(path,), daemon=True)
    first.start()
    assert started.wait(5)
    write(path, {"keys": keys_of(B)})
    # A second SIGHUP while the first reload runs returns at once (same thread
    # or not): it is queued, never waited for.
    assert _in_thread(lambda: t.reload(path), timeout=1.0) is False
    started.clear()
    release.set()
    first.join(5)
    assert not first.is_alive()
    assert t.kids() == {B.real_kid}  # the queued reload ran


def test_reload_reentrant_from_the_same_thread(tmp_path: Path) -> None:
    """Even a reload asked for inside a reload (a signal on the same thread) returns."""
    path = write(tmp_path / "k.json", {"keys": keys_of(A)})
    t = TrustedKeys.load(path)
    real = t._reload_once
    calls: list[bool] = []

    def nested(p: str | Path) -> bool:
        if not calls:
            calls.append(t.reload(p))  # would deadlock on a held, blocking lock
        return real(p)

    t._reload_once = nested  # type: ignore[method-assign]
    assert _in_thread(lambda: t.reload(path)) is True
    assert calls == [False]

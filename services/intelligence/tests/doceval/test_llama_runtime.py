"""The llama.cpp runtime the answer gate scores a GGUF with (own answer model plan,
Task 6), against a fake ``llama-server`` executable (tests.doceval.answer_fakes) that
behaves as the GGUF it is given says. Loopback only; synthetic."""

from __future__ import annotations

import hashlib
import socket
from pathlib import Path
from typing import Any

import pytest

from intelligence.doceval.answers import llama_runtime as lr
from intelligence.modelstore.reasons import Refused
from tests.doceval.answer_fakes import marker, server_log, write_fake_llama
from tests.modelstore.fakes import gguf as g


def _gguf(path: Path, mode: str = "good") -> str:
    kvs = [*g.tiny_kvs(), g.kv_string(b"general.name", marker(mode))]
    data = g.gguf(kvs, g.TINY_INFOS, g.TINY_DATA)
    path.write_bytes(data)
    return hashlib.sha256(data).hexdigest()


def _settings(server: Path, **kw: Any) -> lr.Settings:
    base = {
        "server": server,
        "mem_bytes": 1 << 30,
        "threads": 2,
        "ctx": 2048,
        "start_timeout_s": 20.0,
        "answer_timeout_s": 10.0,
        "max_tokens": 64,
    }
    base.update(kw)
    return lr.Settings(**base)


def test_settings_from_the_environment(tmp_path: Path) -> None:
    server, _ = write_fake_llama(tmp_path / "bin")
    s = lr.Settings.from_env(
        {
            lr.SERVER_ENV: str(server),
            lr.MEM_ENV: "6g",
            lr.THREADS_ENV: "4",
            lr.CTX_ENV: "8192",
        }
    )
    assert (s.mem_bytes, s.threads, s.ctx) == (6 << 30, 4, 8192)
    d = lr.Settings.from_env({lr.SERVER_ENV: str(server)})
    assert (d.mem_bytes, d.threads, d.ctx) == (4 << 30, 2, 4096)  # plan Decision 8
    for env in (
        {},
        {lr.SERVER_ENV: "relative/llama-server"},
        {lr.SERVER_ENV: str(tmp_path / "missing")},
        {lr.SERVER_ENV: str(server), lr.MEM_ENV: "lots"},
        {lr.SERVER_ENV: str(server), lr.THREADS_ENV: "0"},
        {lr.SERVER_ENV: str(server), lr.CTX_ENV: "99999999"},
    ):
        with pytest.raises(Refused) as e:
            lr.Settings.from_env(env)
        assert (e.value.reason, e.value.detail_code) == ("no_runtime", "settings")


def test_engine_must_be_the_pinned_llama_cpp_commit(tmp_path: Path) -> None:
    server, _ = write_fake_llama(tmp_path / "ok")
    assert lr.check_engine(_settings(server)) == "1.0.0+b1b770d90d94" == lr.ENGINE_VERSION
    assert lr.LLAMA_CPP_COMMIT == "b1b770d90d94cad9e9ea5b993bdbc383a522fb28"  # PR 1's pin
    dev, _ = write_fake_llama(
        tmp_path / "dev", version_line="version: 1.0.0-dev (build 1, commit b1b770d90d)"
    )
    assert lr.check_engine(_settings(dev)) == lr.ENGINE_VERSION
    for n, line in enumerate(
        (
            "version: 1.0.0 (build 12359, commit 0123abc)",  # another commit
            "version: 0.9.0 (build 12000, commit b1b770d)",  # another version
            "version: 12359 (b1b770d9)",  # not llama.cpp's line
        )
    ):
        other, _ = write_fake_llama(tmp_path / f"other-{n}", version_line=line)
        with pytest.raises(Refused) as e:
            lr.check_engine(_settings(other))
        assert (e.value.reason, e.value.detail_code) == ("no_runtime", "engine_version")


def test_serves_the_gguf_offline_on_loopback_and_answers(tmp_path: Path) -> None:
    server, log = write_fake_llama(tmp_path / "bin")
    gguf = tmp_path / "run" / "model-q4_k_m.gguf"
    gguf.parent.mkdir()
    sha = _gguf(gguf)
    with lr.LlamaServer(_settings(server), gguf, sha, tmp_path / "home") as srv:
        gen = srv.generator
        assert gen.name == f"llama-cpp:{sha[:12]}"
        assert "metformin" in gen.generate("system", "CLINICAL QUERY:\nwarm-up?")
        port = srv.port
    (entry,) = server_log(log)
    argv = entry["argv"]
    assert argv[argv.index("-m") + 1] == str(gguf)
    assert argv[argv.index("--alias") + 1] == sha
    assert argv[argv.index("--host") + 1] == "127.0.0.1"
    assert argv[argv.index("-t") + 1] == "2" and argv[argv.index("-c") + 1] == "2048"
    assert "--jinja" in argv and "--offline" in argv  # the GGUF's own template; no fetch
    assert not any(a.startswith("--chat-template") or a in ("-hf", "-mu") for a in argv)
    # a clean environment: no proxy, token or LLAMA_ARG_* setting reaches the server
    assert set(entry["env"]) <= {"PATH", "HOME", "LANG", "LC_ALL", "LC_CTYPE"}
    with socket.socket() as s:  # stopped on exit
        assert s.connect_ex(("127.0.0.1", port)) != 0


@pytest.mark.parametrize(
    ("mode", "detail"),
    [("wrong-alias", "alias_mismatch"), ("exit", "server_exited")],
)
def test_a_server_that_does_not_serve_the_gguf_is_no_runtime(
    tmp_path: Path, mode: str, detail: str
) -> None:
    server, _ = write_fake_llama(tmp_path / "bin")
    gguf = tmp_path / "model-q4_k_m.gguf"
    sha = _gguf(gguf, mode)
    with pytest.raises(Refused) as e:
        with lr.LlamaServer(_settings(server, start_timeout_s=5.0), gguf, sha, tmp_path / "h"):
            pass
    assert (e.value.reason, e.value.detail_code) == ("no_runtime", detail)


def test_a_changed_gguf_is_refused_before_start(tmp_path: Path) -> None:
    server, log = write_fake_llama(tmp_path / "bin")
    gguf = tmp_path / "model-q4_k_m.gguf"
    _gguf(gguf)
    with pytest.raises(Refused) as e:
        with lr.LlamaServer(_settings(server), gguf, "0" * 64, tmp_path / "h"):
            pass
    assert (e.value.reason, e.value.detail_code) == ("hash_mismatch", "scratch_changed")
    assert server_log(log) == []  # never started


def test_a_gguf_larger_than_the_memory_setting_is_refused(tmp_path: Path) -> None:
    server, log = write_fake_llama(tmp_path / "bin")
    gguf = tmp_path / "model-q4_k_m.gguf"
    sha = _gguf(gguf)
    with pytest.raises(Refused) as e:
        with lr.LlamaServer(_settings(server, mem_bytes=64), gguf, sha, tmp_path / "h"):
            pass
    assert (e.value.reason, e.value.detail_code) == ("no_runtime", "memory")
    assert server_log(log) == []


def test_a_malformed_reply_raises(tmp_path: Path) -> None:
    server, _ = write_fake_llama(tmp_path / "bin")
    gguf = tmp_path / "model-q4_k_m.gguf"
    sha = _gguf(gguf, "garbage")
    with lr.LlamaServer(_settings(server), gguf, sha, tmp_path / "h") as srv:
        with pytest.raises(lr.RuntimeFailed):
            srv.generator.generate("system", "question")

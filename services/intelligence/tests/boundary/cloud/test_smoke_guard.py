"""The manual live smoke and the fixture scrubber (plan Task 15; K16, K18,
C19, C25). Nothing here reaches a network: the smoke runs in-process over
fake transports, or as a subprocess that refuses before it reads a key.
Every key is the synthetic FAKE_KEY."""

from __future__ import annotations

import importlib.util
import io
import json
import logging
import subprocess
import sys
import tomllib
from fnmatch import fnmatch
from pathlib import Path
from types import ModuleType
from typing import Any

import pytest

from intelligence.boundary.cloud import vocab
from intelligence.boundary.cloud.settings import SDK_ENV_REFUSED
from tests.boundary.cloud import adapter_kit as kit
from tests.boundary.cloud.conftest import FAKE_KEY, FAKE_SK_KEY

pytestmark = pytest.mark.disable_socket

ROOT = Path(__file__).resolve().parents[3]
SCRIPTS = ROOT / "scripts"
SMOKE = SCRIPTS / "cloud_smoke.py"
SCRUB = SCRIPTS / "scrub_cloud_fixture.py"
FIXTURES = ROOT / "tests" / "fixtures" / "cloud"
MODELS = {
    "anthropic": "claude-opus-5-5",
    "gemini": "gemini-synthetic-1",
    "openai": "gpt-6.1-sol",
    "xai": "grok-4.7",
}
# A person's machine: the smoke's own variable, nothing else of note.
OK_ENV = {"MHAI_CLOUD_SMOKE": "1", "APP_ENV": "demo", "PATH": "/usr/bin"}


def _load(path: Path) -> ModuleType:
    spec = importlib.util.spec_from_file_location(f"_script_{path.stem}", path)
    assert spec is not None and spec.loader is not None
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


@pytest.fixture(scope="module")
def smoke() -> ModuleType:
    return _load(SMOKE)


@pytest.fixture(scope="module")
def scrub() -> ModuleType:
    return _load(SCRUB)


class NoRead(io.StringIO):
    """A stdin that fails the test if anything reads the key."""

    def read(self, *a: Any) -> str:
        raise AssertionError("the key was read before a refusal")

    def readline(self, *a: Any) -> str:
        raise AssertionError("the key was read before a refusal")

    def isatty(self) -> bool:
        return False


def argv(provider: str = "anthropic", *extra: str) -> list[str]:
    return ["--provider", provider, "--model", MODELS[provider], "--key-stdin", *extra]


def run_main(
    smoke: ModuleType,
    args: list[str],
    *,
    env: dict[str, str] | None = None,
    stdin: io.StringIO | None = None,
    transport: Any = None,
    markers: tuple[Path, ...] = (),
) -> int:
    rc: int = smoke.main(
        args,
        stdin=stdin if stdin is not None else NoRead(),
        transport=transport,
        env=OK_ENV if env is None else env,
        markers=markers,
    )
    return rc


def cli(
    args: list[str], env: dict[str, str], key: str = FAKE_KEY
) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        [sys.executable, str(SMOKE), *args],
        input=key + "\n",
        capture_output=True,
        text=True,
        env=env,
        cwd=ROOT,
        timeout=120,
        check=False,
    )


# --- refusals, before the key is read ------------------------------------------------------


def test_smoke_refuses_without_env(smoke: ModuleType, capsys: pytest.CaptureFixture[str]) -> None:
    for value in (None, "", "0", "true", "yes", " 1"):
        env = {k: v for k, v in OK_ENV.items() if k != "MHAI_CLOUD_SMOKE"}
        if value is not None:
            env["MHAI_CLOUD_SMOKE"] = value
        assert run_main(smoke, argv(), env=env) == 2
        err = capsys.readouterr().err
        assert "MHAI_CLOUD_SMOKE" in err and "refused" in err
    # The real command line, as a person would run it.
    p = cli(argv(), {"PATH": "/usr/bin:/bin"})
    assert p.returncode == 2, p.stderr
    assert "MHAI_CLOUD_SMOKE" in p.stderr
    assert FAKE_KEY not in p.stdout + p.stderr


def test_smoke_refuses_in_container_or_ci(
    smoke: ModuleType, tmp_path: Path, capsys: pytest.CaptureFixture[str]
) -> None:
    for extra, named in (
        ({"CI": "1"}, "CI"),
        ({"CI": ""}, "CI"),
        ({"GITHUB_ACTIONS": "true"}, "GITHUB_ACTIONS"),
        ({"container": "podman"}, "container"),
        ({"container": "docker"}, "container"),
    ):
        assert run_main(smoke, argv(), env={**OK_ENV, **extra}) == 2, extra
        err = capsys.readouterr().err
        assert named in err and "refused" in err, (extra, err)
    for marker in (".dockerenv", ".containerenv"):
        m = tmp_path / marker
        m.write_text("")
        assert run_main(smoke, argv(), markers=(tmp_path / "absent", m)) == 2
        assert str(m) in capsys.readouterr().err
    # The real markers are /.dockerenv and /run/.containerenv.
    assert Path("/.dockerenv") in smoke.CONTAINER_MARKERS
    assert Path("/run/.containerenv") in smoke.CONTAINER_MARKERS
    # As a subprocess in CI (or in this build container), it refuses too.
    p = cli(argv(), {**OK_ENV, "PATH": "/usr/bin:/bin", "CI": "1"})
    assert p.returncode == 2 and "refused" in p.stderr


def test_smoke_refuses_sdk_env(smoke: ModuleType, capsys: pytest.CaptureFixture[str]) -> None:
    names = (
        *SDK_ENV_REFUSED,
        "OPENAI_NEW_SETTING",
        "XAI_BASE_URL",
        "SSL_CERT_FILE",
        "SSL_CERT_DIR",
    )
    for name in names:
        value = "sdk-value-" + "v" * 12
        assert run_main(smoke, argv(), env={**OK_ENV, name: value}) == 2, name
        err = capsys.readouterr().err
        assert name in err and value not in err, name


def test_smoke_refuses_without_ca_bundle(
    smoke: ModuleType,
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    capsys: pytest.CaptureFixture[str],
) -> None:
    import ssl

    from intelligence.boundary.cloud import settings as settings_mod

    real = ssl.get_default_verify_paths()
    none, nodir = str(tmp_path / "none.pem"), str(tmp_path / "none")
    fake = ssl.DefaultVerifyPaths(
        none, nodir, real.openssl_cafile_env, none, real.openssl_capath_env, nodir
    )
    monkeypatch.setattr(settings_mod.ssl, "get_default_verify_paths", lambda: fake)
    # The real transport (none given): refused before the key is read.
    assert run_main(smoke, argv()) == 2
    assert "CA bundle" in capsys.readouterr().err


def test_smoke_has_no_key_file_option(
    smoke: ModuleType, capsys: pytest.CaptureFixture[str]
) -> None:
    parser = smoke.build_parser()
    options = {o for a in parser._actions for o in a.option_strings}
    assert not any("file" in o or "env" in o for o in options), options
    for bad in (
        ["--provider", "anthropic", "--model", "m", "--key-file", "/run/secrets/k"],
        ["--provider", "anthropic", "--model", "m", "--key", "x"],
        ["--provider", "anthropic", "--model", "m", "--key-stdin", "--key-f", "x"],
        ["--provider", "azure", "--model", "m", "--key-stdin"],
    ):
        with pytest.raises(SystemExit) as e:
            run_main(smoke, bad)
        assert e.value.code == 2, bad
    capsys.readouterr()
    # Without --key-stdin, refused: stdin is the only way in.
    assert run_main(smoke, ["--provider", "anthropic", "--model", MODELS["anthropic"]]) == 2
    assert "--key-stdin" in capsys.readouterr().err


def test_smoke_cli_has_no_transport_option(smoke: ModuleType) -> None:
    parser = smoke.build_parser()
    options = {o for a in parser._actions for o in a.option_strings}
    assert options == {"-h", "--help", "--provider", "--model", "--key-stdin", "--record"}
    assert parser.allow_abbrev is False
    p = subprocess.run(
        [sys.executable, str(SMOKE), "--help"],
        capture_output=True,
        text=True,
        cwd=ROOT,
        check=False,
    )
    assert p.returncode == 0
    listed = {w.strip(",[]") for w in p.stdout.split() if w.startswith(("-", "[-"))}
    assert {w for w in listed if w.startswith("--")} == {
        "--help",
        "--provider",
        "--model",
        "--key-stdin",
        "--record",
    }


def test_smoke_refuses_bad_model_and_key(
    smoke: ModuleType, capsys: pytest.CaptureFixture[str]
) -> None:
    t = kit.ReplyingTransport("anthropic")
    for model in ("claude-fable-5", "claude mythos", "m" * 65):
        args = ["--provider", "anthropic", "--model", model, "--key-stdin"]
        assert run_main(smoke, args, transport=t) == 2, model
        capsys.readouterr()
    for key in (
        "",
        "changeme",
        "short-fake",
        "fake key with spaces " + "0" * 20,
        "fakeé" + "0" * 24,
    ):
        assert run_main(smoke, argv(), stdin=io.StringIO(key + "\n"), transport=t) == 2, key
        err = capsys.readouterr().err
        assert "key" in err and (not key or key not in err)
    assert t.bodies == []


# --- the run, with fakes ---------------------------------------------------------------------


@pytest.mark.parametrize("provider", vocab.PROVIDERS)
def test_smoke_never_prints_or_logs_the_key(
    smoke: ModuleType,
    provider: str,
    tmp_path: Path,
    capsys: pytest.CaptureFixture[str],
    caplog: pytest.LogCaptureFixture,
) -> None:
    caplog.set_level(logging.DEBUG)
    record = tmp_path / provider / "recorded"
    t = kit.ReplyingTransport(provider)
    rc = run_main(
        smoke,
        argv(provider, "--record", str(record)),
        stdin=io.StringIO(FAKE_KEY + "\n"),
        transport=t,
    )
    out, err = capsys.readouterr()
    assert rc == 0, out + err
    assert len(t.bodies) == 1
    # The key was used (the SDK put it in a header) and never shown.
    assert any(FAKE_KEY in v for v in t.headers[0].values())
    for text in (out, err, caplog.text):
        assert FAKE_KEY not in text
    lines = dict(line.split("=", 1) for line in out.splitlines() if "=" in line)
    assert lines["provider"] == provider and lines["outcome"] == "ok"
    assert lines["cca"].startswith(f"{provider}/{MODELS[provider]}@")
    assert lines["answer_validated"] == "yes"
    assert int(lines["tokens_in"]) > 0 and int(lines["tokens_out"]) > 0
    if provider == "xai":
        assert lines["retention_header"] == "true"
        assert lines["usage.completion_tokens_details.reasoning_tokens"] == "20"
        assert lines["over_cap"] == "no"
    else:
        assert "retention_header" not in lines
    if provider == "gemini":
        # The reply's shape as closed codes: what the adapter assumes of
        # Google's OpenAI-compatible endpoint, checked on the live run.
        assert lines["reply_object"] == "chat.completion"
        assert lines["finish_reason"] == "stop"
        assert lines["choices"] == "1" and lines["model_present"] == "yes"
        assert lines["over_cap"] == "no"
    else:
        assert "reply_object" not in lines
    # Only provider, outcome, versions, counts, the header and the verdict:
    # no text of the question, the chunks or the answer.
    assert "Synthetic" not in out and "KB#" not in out
    # The recording is scrubbed, passes --check, and parses to the same
    # outcome through the adapter (plan Task 15).
    files = sorted(record.glob("*.json"))
    assert [f.name for f in files] == [f"ok-{MODELS[provider]}.json"]
    raw = files[0].read_text("utf-8")
    assert FAKE_KEY not in raw and "SYNTHETIC" in raw
    assert (
        subprocess.run(
            [sys.executable, str(SCRUB), "--check", str(files[0])], check=False
        ).returncode
        == 0
    )
    kit.check_recorded(provider, files[0])
    # A second run never overwrites a recording.
    rc = run_main(
        smoke,
        argv(provider, "--record", str(record)),
        stdin=io.StringIO(FAKE_KEY + "\n"),
        transport=kit.ReplyingTransport(provider),
    )
    assert rc == 2 and "exists" in capsys.readouterr().err


@pytest.mark.parametrize("provider", vocab.PROVIDERS)
def test_smoke_echoed_key_in_an_error_is_not_shown(
    smoke: ModuleType,
    provider: str,
    capsys: pytest.CaptureFixture[str],
    caplog: pytest.LogCaptureFixture,
) -> None:
    """A 401 that echoes the request (key header included) is auth_failed,
    exits 1, and shows nothing of it."""
    caplog.set_level(logging.DEBUG)
    for key in (FAKE_KEY, FAKE_SK_KEY):
        rc = run_main(
            smoke,
            argv(provider),
            stdin=io.StringIO(key + "\n"),
            transport=kit.EchoTransport(provider),
        )
        out, err = capsys.readouterr()
        assert rc == 1
        assert "outcome=auth_failed" in out and "answer_validated=no" in out
        for text in (out, err, caplog.text):
            assert key not in text


def test_smoke_reads_a_terminal_with_getpass(
    smoke: ModuleType, monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str]
) -> None:
    class Tty(NoRead):
        def isatty(self) -> bool:
            return True

    asked: list[str] = []

    def fake_getpass(prompt: str = "", stream: Any = None) -> str:
        asked.append(prompt)
        return FAKE_KEY

    monkeypatch.setattr(smoke.getpass, "getpass", fake_getpass)
    assert run_main(smoke, argv(), stdin=Tty(), transport=kit.ReplyingTransport("anthropic")) == 0
    assert len(asked) == 1 and FAKE_KEY not in asked[0]
    assert FAKE_KEY not in capsys.readouterr().out


def test_smoke_not_collected_by_pytest() -> None:
    cfg = tomllib.loads((ROOT / "pyproject.toml").read_text("utf-8"))["tool"]["pytest"][
        "ini_options"
    ]
    assert cfg["testpaths"] == ["tests"]
    patterns = cfg.get("python_files", ["test_*.py", "*_test.py"])
    for script in (SMOKE, SCRUB):
        assert ROOT / "tests" not in script.parents
        assert not any(fnmatch(script.name, p) for p in patterns)


# --- the scrubber -------------------------------------------------------------------------------


def _committed_fixtures() -> list[Path]:
    files = sorted(FIXTURES.rglob("*.json"))
    assert len(files) >= 37  # 13 + 12 + 12 synthetic, plus any recorded
    return files


def test_scrubber_check_passes_committed_fixtures() -> None:
    files = _committed_fixtures()
    p = subprocess.run(
        [sys.executable, str(SCRUB), "--check", *map(str, files)],
        capture_output=True,
        text=True,
        check=False,
    )
    assert p.returncode == 0, p.stdout + p.stderr


def test_recorded_fixtures_pass_the_adapter_parsers() -> None:
    """Every recorded fixture (Task 15's live run) parses to the outcome its
    name gives, through the same client and adapter as the synthetic ones."""
    for provider in vocab.PROVIDERS:
        for f in sorted((FIXTURES / provider / "recorded").glob("*.json")):
            kit.check_recorded(provider, f)


def _raw_reply() -> dict[str, Any]:
    """A reply as a provider sends it: real-looking ids, account headers,
    timings (all synthetic values in a real shape)."""
    return {
        "status": 200,
        "headers": {
            "Content-Type": "application/json",
            "Date": "Sat, 07 Nov 2026 10:00:00 GMT",
            "request-id": "req_011CXq7aB2cD3eF4gH5iJ6kL",
            "x-request-id": "req_9f8e7d6c5b4a",
            "cf-ray": "8d1f2e3a4b5c6d7e-LOS",
            "anthropic-organization-id": "0a1b2c3d-4e5f-6789-abcd-ef0123456789",
            "openai-organization": "org-AbCdEfGhIjKlMnOp",
            "openai-project": "proj_AbCdEfGhIjKlMnOp",
            "openai-processing-ms": "812",
            "x-ratelimit-remaining-requests": "49",
            "anthropic-ratelimit-tokens-reset": "2026-11-07T10:01:00Z",
            "set-cookie": "__cf_bm=abcdef; path=/",
            "x-zero-data-retention": "true",
        },
        "body": {
            "id": "msg_011CXq7aB2cD3eF4gH5iJ6kL",
            "type": "message",
            "model": "claude-opus-5-5-20261001",
            "created": 1762509600,
            "created_at": 1762509600,
            "system_fingerprint": "fp_4a5b6c7d",
            "output": [{"id": "rs_0123456789abcdef", "type": "reasoning", "summary": []}],
            "usage": {"input_tokens": 812, "output_tokens": 64},
        },
    }


def test_scrubber_scrubs_ids_headers_and_timings(scrub: ModuleType, tmp_path: Path) -> None:
    raw = _raw_reply()
    src, dst = tmp_path / "raw.json", tmp_path / "out.json"
    src.write_text(json.dumps(raw), "utf-8")
    p = subprocess.run(
        [sys.executable, str(SCRUB), str(src), str(dst)],
        capture_output=True,
        text=True,
        check=False,
    )
    assert p.returncode == 0, p.stderr
    out = json.loads(dst.read_text("utf-8"))
    assert scrub.check(out) == []
    text = json.dumps(out)
    for v in (
        "011CXq7a",
        "9f8e7d6c",
        "8d1f2e3a",
        "0a1b2c3d",
        "org-AbCd",
        "proj_AbCd",
        "fp_4a5b",
        "rs_0123",
        "1762509600",
        "__cf_bm",
        "2026-11-07",
    ):
        assert v not in text, v
    assert set(out["headers"]) == {
        "content-type",
        "request-id",
        "x-request-id",
        "x-zero-data-retention",
    }
    assert out["headers"]["x-zero-data-retention"] == "true"
    assert (
        out["body"]["id"] == "msg_SYNTHETIC0001"
        and out["headers"]["request-id"] == "req_SYNTHETIC0001"
    )
    assert out["body"]["output"][0]["id"] == "rs_SYNTHETIC0001"
    assert (
        out["body"]["usage"] == raw["body"]["usage"]
        and out["body"]["model"] == raw["body"]["model"]
    )


def test_scrubber_check_fails_on_key_shape_and_real_ids(scrub: ModuleType, tmp_path: Path) -> None:
    good = json.loads((FIXTURES / "anthropic" / "ok.json").read_text("utf-8"))
    bad_cases: dict[str, Any] = {
        "key_in_body": {**good, "body": {**good["body"], "note": "key " + FAKE_SK_KEY}},
        "ant_key_in_header": {
            **good,
            "headers": {**good["headers"], "request-id": "sk-ant-" + "a" * 24},
        },
        "xai_key_in_text": {
            **good,
            "body": {**good["body"], "content": [{"type": "text", "text": "xai-" + "b" * 24}]},
        },
        "bearer": {
            **good,
            "body": {**good["body"], "note": "Authorization: Bearer abcdefghijklmnop"},
        },
        "real_id": {**good, "body": {**good["body"], "id": "msg_011CXq7aB2cD3eF4gH5iJ6kL"}},
        "real_nested_id": {
            **good,
            "body": {**good["body"], "usage": {"input_tokens": 1, "request_id": "req_42"}},
        },
        "real_request_header": {
            **good,
            "headers": {**good["headers"], "request-id": "req_011CXq7a"},
        },
        "header_outside_list": {**good, "headers": {**good["headers"], "cf-ray": "SYNTHETIC"}},
        "org_header": {**good, "headers": {**good["headers"], "openai-organization": "SYNTHETIC"}},
        "org_in_body": {**good, "body": {**good["body"], "organization": "org-AbCdEfGhIjKlMnOp"}},
        "timing": {**good, "body": {**good["body"], "created_at": 1762509600}},
        "extra_top_level": {**good, "request": {"x": 1}},
        "no_status": {k: v for k, v in good.items() if k != "status"},
    }
    for name, fx in bad_cases.items():
        assert scrub.check(fx), name
        f = tmp_path / f"{name}.json"
        f.write_text(json.dumps(fx), "utf-8")
        p = subprocess.run(
            [sys.executable, str(SCRUB), "--check", str(f)],
            capture_output=True,
            text=True,
            check=False,
        )
        assert p.returncode == 1, name
        # It names the rule and the file, never the value.
        assert name in p.stdout + p.stderr and FAKE_SK_KEY not in p.stdout + p.stderr
    # Not JSON at all.
    f = tmp_path / "not_json.json"
    f.write_text("{", "utf-8")
    assert (
        subprocess.run(
            [sys.executable, str(SCRUB), "--check", str(f)], capture_output=True, check=False
        ).returncode
        == 1
    )
    # The scrubber itself refuses to write what it can't clean.
    src, dst = tmp_path / "keyed.json", tmp_path / "keyed-out.json"
    src.write_text(json.dumps(bad_cases["key_in_body"]), "utf-8")
    assert (
        subprocess.run(
            [sys.executable, str(SCRUB), str(src), str(dst)], capture_output=True, check=False
        ).returncode
        == 1
    )
    assert not dst.exists()

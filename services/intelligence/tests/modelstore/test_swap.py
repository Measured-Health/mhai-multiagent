"""The checks and the shadow test behind deploy/gpu-model/swap-model.sh
(``mhai-models swap``). Every adapter, GGUF, question and answer here is synthetic."""

from __future__ import annotations

import json
import os
import struct
import threading
from collections.abc import Iterator
from dataclasses import dataclass, field
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any

import pytest

from intelligence.doceval.answers import schema
from intelligence.modelstore import cli, swap, testsets
from intelligence.modelstore.reasons import Refused
from intelligence.tools.llama_client import LocalLLMSettings
from tests.doceval.answer_fakes import oracle, test_builder
from tests.modelstore.fakes import gguf as g

REPO = Path(__file__).resolve().parents[4]
LICENCES = REPO / "models" / "licences.json"
DATASETS = REPO / "models" / "approved-datasets.json"
DEV = REPO / "testdata" / "answers" / "dev"
BASE = "google/gemma-4-E4B-it"
REVISION = swap.BASES[BASE].revision
TEMPLATE = g.TINY_TEMPLATE.decode("ascii")
SHA = "0123456789ab" + "d" * 52
ALIAS = f"cca-shadow@2.0.0#{SHA[:12]}"
LOCKED_LINE = testsets.LockEntry("clinical_answer", "1.0.0", "a" * 64).text + "\n"


def _safetensors() -> bytes:
    header = json.dumps(
        {"lora_a.weight": {"dtype": "F32", "shape": [2, 2], "data_offsets": [0, 16]}}
    ).encode()
    return struct.pack("<Q", len(header)) + header + bytes(16)


def _manifest(**over: Any) -> dict[str, Any]:
    m: dict[str, Any] = {
        "schema": "mhai.models.training/1",
        "name": "cca-shadow",
        "role": "cca",
        "trained_by": "synthetic-trainer",
        "datasets": ["medqa-usmle-4-options@1.0.0"],
        "base_model": {"upstream": BASE, "revision": REVISION, "licence": "Apache-2.0"},
        "framework": {"peft": "0.22.0", "transformers": "5.24.0"},
        "notebook_metrics": {"train_loss": 0.5},
        "created_at": "2026-11-20T10:00:00Z",
    }
    m.update(over)
    return m


def _adapter(tmp_path: Path, *, config: dict[str, Any] | None = None, **manifest: Any) -> Path:
    d = tmp_path / "adapter"
    d.mkdir()
    cfg = {"peft_type": "LORA", "base_model_name_or_path": BASE, "r": 16, "lora_alpha": 32}
    cfg.update(config or {})
    (d / "adapter_config.json").write_text(json.dumps(cfg))
    (d / "adapter_model.safetensors").write_bytes(_safetensors())
    (d / "tokenizer.json").write_text(json.dumps({"version": "1.0", "model": {}}))
    (d / "tokenizer_config.json").write_text(json.dumps({"chat_template": TEMPLATE}))
    (d / "training_manifest.json").write_text(json.dumps(_manifest(**manifest)))
    (d / "README.md").write_text("synthetic adapter card, never read\n")
    return d


def _code(fn: Any, *args: Any, **kw: Any) -> tuple[str, str | None]:
    with pytest.raises(Refused) as err:
        fn(*args, **kw)
    return err.value.reason, err.value.detail_code


def _check(d: Path, licences: Path = LICENCES) -> dict[str, Any]:
    return swap.check_adapter(d, licences, DATASETS)


# ---------------------------------------------------------------- the adapter


def test_a_good_adapter_passes(tmp_path: Path) -> None:
    out = _check(_adapter(tmp_path))
    assert out == {
        "base": BASE,
        "revision": REVISION,
        "licence": "Apache-2.0",
        "architecture": "gemma4",
        "trained_by": "synthetic-trainer",
        "datasets": ["medqa-usmle-4-options@1.0.0"],
    }


def test_optional_files_are_checked_and_pass(tmp_path: Path) -> None:
    d = _adapter(tmp_path, config={"revision": REVISION})
    (d / "special_tokens_map.json").write_text("{}")
    (d / "added_tokens.json").write_text('{"<x>": 5}\n')
    (d / "tokenizer.model").write_bytes(b"\x0a\x03syn")
    assert _check(d)["base"] == BASE


@pytest.mark.parametrize(
    ("name", "reason"),
    [
        ("adapter_model.bin", "pickle_refused"),
        ("optimizer.pt", "pickle_refused"),
        ("modeling_x.py", "code_refused"),
        ("notes.txt", "file_not_allowed"),
        ("manifest.json", "file_not_allowed"),
    ],
)
def test_files_outside_the_allow_list_are_refused(tmp_path: Path, name: str, reason: str) -> None:
    d = _adapter(tmp_path)
    (d / name).write_bytes(b"synthetic, never loaded")
    assert _code(_check, d)[0] == reason


def test_a_link_a_folder_and_a_hard_link_are_refused(tmp_path: Path) -> None:
    d = _adapter(tmp_path)
    os.symlink(d / "tokenizer.json", d / "special_tokens_map.json")
    assert _code(_check, d) == ("file_not_allowed", "link")
    os.unlink(d / "special_tokens_map.json")
    (d / "checkpoint-1").mkdir()
    assert _code(_check, d) == ("file_not_allowed", "directory")
    (d / "checkpoint-1").rmdir()
    os.link(d / "tokenizer.json", tmp_path / "second-name")
    assert _code(_check, d) == ("file_not_allowed", "not_regular")


def test_a_missing_folder_file_or_a_non_folder_is_refused(tmp_path: Path) -> None:
    assert _code(_check, tmp_path / "nope") == ("missing_file", "adapter_dir")
    (tmp_path / "file").write_text("x")
    assert _code(_check, tmp_path / "file") == ("file_not_allowed", "adapter_dir")
    d = _adapter(tmp_path)
    (d / "training_manifest.json").unlink()
    assert _code(_check, d) == ("missing_file", "adapter_file")


@pytest.mark.parametrize(
    ("config", "code"),
    [
        ({"peft_type": "PROMPT_TUNING"}, ("not_allowed_here", "adapter_type")),
        ({"base_model_name_or_path": "someone/other-model"}, ("base_unverified", "adapter_base")),
        ({"base_model_name_or_path": None}, ("base_unverified", "adapter_base")),
        ({"revision": "main"}, ("base_unverified", "adapter_revision")),
    ],
)
def test_the_adapter_config_is_held_to_the_allowed_bases(
    tmp_path: Path, config: dict[str, Any], code: tuple[str, str]
) -> None:
    assert _code(_check, _adapter(tmp_path, config=config)) == code


def test_remote_code_in_the_adapter_config_is_refused(tmp_path: Path) -> None:
    d = _adapter(tmp_path, config={"auto_mapping": {"base_model_class": "X"}})
    assert _code(_check, d)[0] == "code_refused"


def test_a_licence_outside_the_registry_is_refused(tmp_path: Path) -> None:
    lic = tmp_path / "licences.json"
    lic.write_text(json.dumps({"schema": "mhai.models.licences/1", "licences": ["MIT"]}))
    assert _code(_check, _adapter(tmp_path), lic) == ("licence_not_allowed", "base_licence")


@pytest.mark.parametrize(
    ("manifest", "code"),
    [
        ({"role": "extract"}, ("not_allowed_here", "role")),
        ({"datasets": ["unknown-set@1.0.0"]}, ("dataset_unknown", None)),
        (
            {"base_model": {"upstream": BASE, "revision": "a" * 40, "licence": "Apache-2.0"}},
            ("base_unverified", "manifest_base"),
        ),
        (
            {"base_model": {"upstream": "x/y", "revision": REVISION, "licence": "Apache-2.0"}},
            ("base_unverified", "manifest_base"),
        ),
        (
            {"base_model": {"upstream": BASE, "revision": REVISION, "licence": "MIT"}},
            ("licence_not_allowed", "manifest_licence"),
        ),
    ],
)
def test_the_training_manifest_must_match_the_base_and_the_registries(
    tmp_path: Path, manifest: dict[str, Any], code: tuple[str, str | None]
) -> None:
    assert _code(_check, _adapter(tmp_path, **manifest)) == code


def test_a_bad_safetensors_header_is_refused(tmp_path: Path) -> None:
    d = _adapter(tmp_path)
    (d / "adapter_model.safetensors").write_bytes(b"\x00" * 7)
    assert _code(_check, d)[0] == "safetensors_invalid"


def test_bad_added_tokens_are_refused(tmp_path: Path) -> None:
    d = _adapter(tmp_path)
    (d / "added_tokens.json").write_text("{not json")
    assert _code(_check, d)[0] == "json_invalid"


def test_an_adapter_without_a_chat_template_is_refused(tmp_path: Path) -> None:
    d = _adapter(tmp_path)
    (d / "tokenizer_config.json").write_text("{}")
    assert _code(_check, d) == ("gguf_template_mismatch", "run_missing")


def test_the_jinja_template_is_read_when_there_is_no_json_one(tmp_path: Path) -> None:
    d = _adapter(tmp_path)
    (d / "chat_template.jinja").write_text("{{ jinja }}")
    assert swap.adapter_template(d) == "{{ jinja }}"
    (d / "chat_template.jinja").write_bytes(b"\xff\xfe")
    assert _code(swap.adapter_template, d) == ("json_invalid", "chat_template")
    (d / "chat_template.json").write_text(json.dumps({"chat_template": "{{ json }}"}))
    assert swap.adapter_template(d) == "{{ json }}"


# ---------------------------------------------------------------- the GGUF


def _gguf(tmp_path: Path, raw: bytes, name: str = "model-q4_k_m.gguf") -> Path:
    p = tmp_path / name
    p.write_bytes(raw)
    return p


def test_a_gemma4_gguf_passes_with_its_architecture(tmp_path: Path) -> None:
    p = _gguf(tmp_path, g.tiny(b"gemma4"))
    assert swap.check_gguf(p, architecture="gemma4") == {
        "architecture": "gemma4",
        "size": p.stat().st_size,
    }
    assert swap.check_gguf(p)["architecture"] == "gemma4"


def test_another_architecture_than_the_base_is_refused(tmp_path: Path) -> None:
    p = _gguf(tmp_path, g.tiny(b"llama"))
    assert _code(swap.check_gguf, p, architecture="gemma4") == (
        "gguf_invalid",
        "architecture_not_base",
    )


def test_an_unlisted_architecture_or_a_broken_header_is_refused(tmp_path: Path) -> None:
    assert _code(swap.check_gguf, _gguf(tmp_path, g.tiny(b"bert"))) == (
        "gguf_invalid",
        "architecture",
    )
    assert _code(swap.check_gguf, _gguf(tmp_path, b"GGUF" + bytes(3), "b.gguf"))[0] == (
        "gguf_invalid"
    )


def test_a_link_or_another_name_is_refused(tmp_path: Path) -> None:
    p = _gguf(tmp_path, g.tiny(b"gemma4"))
    os.symlink(p, tmp_path / "link.gguf")
    assert _code(swap.check_gguf, tmp_path / "link.gguf") == (
        "file_not_allowed",
        "gguf_not_regular",
    )
    q = _gguf(tmp_path, g.tiny(b"gemma4"), "model.bin")
    assert _code(swap.check_gguf, q) == ("file_not_allowed", "gguf_name")


def test_the_template_must_be_the_adapters(tmp_path: Path) -> None:
    d = _adapter(tmp_path)
    p = _gguf(tmp_path, g.tiny(b"gemma4"))
    assert swap.check_gguf(p, architecture="gemma4", template_from=d)["architecture"] == "gemma4"
    (d / "tokenizer_config.json").write_text(json.dumps({"chat_template": "other"}))
    assert _code(swap.check_gguf, p, template_from=d) == ("gguf_template_mismatch", "differs")
    (d / "tokenizer_config.json").write_text("{}")
    assert _code(swap.check_gguf, p, template_from=d) == (
        "gguf_template_mismatch",
        "run_missing",
    )
    kvs = [g.kv_string(b"general.architecture", b"gemma4")]
    bare = _gguf(tmp_path, g.gguf(kvs, g.TINY_INFOS, g.TINY_DATA), "bare.gguf")
    assert _code(swap.check_gguf, bare, template_from=d) == (
        "gguf_template_mismatch",
        "gguf_missing",
    )


# ---------------------------------------------------------------- the command line


def test_the_command_line_prints_one_line(
    tmp_path: Path, capsys: pytest.CaptureFixture[str]
) -> None:
    d = _adapter(tmp_path)
    argv = ["swap", "check-adapter", str(d), "--licences", str(LICENCES)]
    assert cli.main([*argv, "--datasets", str(DATASETS)]) == 0
    assert json.loads(capsys.readouterr().out)["adapter"]["revision"] == REVISION
    p = _gguf(tmp_path, g.tiny(b"gemma4"))
    argv = ["swap", "check-gguf", str(p), "--architecture", "gemma4", "--template-from", str(d)]
    assert cli.main(argv) == 0
    assert json.loads(capsys.readouterr().out) == {
        "gguf": {"architecture": "gemma4", "size": p.stat().st_size}
    }
    assert swap.main(["check-gguf", str(p), "--architecture", "llama"]) == 2
    assert json.loads(capsys.readouterr().out) == {
        "refused": "gguf_invalid",
        "detail": "architecture_not_base",
    }
    assert swap.main(["check-gguf", str(tmp_path / "missing.gguf")]) == 3
    assert json.loads(capsys.readouterr().out) == {"error": "io"}
    (d / "adapter_model.bin").write_bytes(b"x")
    assert swap.main(["check-adapter", str(d), "--licences", "x", "--datasets", "y"]) == 2
    out = capsys.readouterr().out
    assert json.loads(out) == {"refused": "pickle_refused"}
    assert str(tmp_path) not in out


def test_an_unexpected_error_is_a_fixed_code(
    monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str]
) -> None:
    def boom(*a: Any, **k: Any) -> Any:
        raise RuntimeError("synthetic detail that must not be printed")

    monkeypatch.setattr(swap, "check_gguf", boom)
    assert swap.main(["check-gguf", "x.gguf"]) == 3
    out = capsys.readouterr().out
    assert json.loads(out) == {"error": "internal"} and "synthetic" not in out


# ---------------------------------------------------------------- the shadow test


@dataclass
class Fake:
    alias: str = ALIAS
    chats: int = 0
    replies: dict[str, dict[str, str]] = field(default_factory=dict)
    mode: str = "good"


def _handler(f: Fake) -> type[BaseHTTPRequestHandler]:
    class H(BaseHTTPRequestHandler):
        def _send(self, obj: Any) -> None:
            body = json.dumps(obj).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def do_GET(self) -> None:  # noqa: N802 - http.server's naming
            self._send({"object": "list", "data": [{"id": f.alias, "object": "model"}]})

        def do_POST(self) -> None:  # noqa: N802
            n = int(self.headers["Content-Length"])
            user = json.loads(self.rfile.read(n))["messages"][-1]["content"]
            q = user.split("CLINICAL QUERY:\n", 1)[-1].split("\n\nTRIAGE:", 1)[0].strip()
            f.chats += 1
            text = f.replies.get(q, {}).get(f.mode, "Synthetic answer [KB#0006].")
            self._send({"choices": [{"message": {"role": "assistant", "content": text}}]})

        def log_message(self, format: str, *args: Any) -> None:
            pass

    return H


@pytest.fixture
def fake() -> Iterator[tuple[Fake, LocalLLMSettings]]:
    f = Fake(replies=oracle(schema.load_set(DEV).items))
    httpd = ThreadingHTTPServer(("127.0.0.1", 0), _handler(f))
    httpd.daemon_threads = True
    threading.Thread(target=httpd.serve_forever, daemon=True).start()
    url = f"http://127.0.0.1:{httpd.server_address[1]}"
    try:
        yield f, LocalLLMSettings(url=url, timeout_s=120.0, start_timeout_s=1.0)
    finally:
        httpd.shutdown()
        httpd.server_close()


def _testsets(tmp_path: Path, text: str = "# no entries\n") -> Path:
    p = tmp_path / "testsets.lock"
    p.write_text(text, encoding="ascii")
    return p


def _shadow(tmp_path: Path, settings: LocalLLMSettings, **kw: Any) -> dict[str, Any]:
    args: dict[str, Any] = {
        "name": "cca-shadow",
        "version": "2.0.0",
        "sha256": SHA,
        "profile": "gemma4-e4b-cca-v1",
        "split": "dev",
        "set_dir": None,
        "max_seconds": 90,
        "settings": settings,
        "builder_for": test_builder,
        **kw,
    }
    if "testsets_lock" not in args:
        args["testsets_lock"] = _testsets(tmp_path)
    return swap.eval_shadow(**args)


def test_a_good_candidate_passes_on_the_dev_split(
    tmp_path: Path, fake: tuple[Fake, LocalLLMSettings]
) -> None:
    f, settings = fake
    out = _shadow(tmp_path, settings)
    assert out["verdict"] == "pass" and out["split"] == "dev" and out["alias"] == ALIAS
    assert out["profile"] == "gemma4-e4b-cca-v1"
    assert out["counts"]["items"] == f.chats == schema.load_set(DEV).meta["items"]
    text = json.dumps(out)
    assert not any(i.question in text for i in schema.load_set(DEV).items)


def test_a_candidate_with_an_unsafe_answer_fails(
    tmp_path: Path, fake: tuple[Fake, LocalLLMSettings]
) -> None:
    f, settings = fake
    f.mode = "unsafe"
    out = _shadow(tmp_path, settings)
    assert out["verdict"] == "fail" and "overall.unsafe_rate" in out["failed"]


def test_another_alias_is_never_scored(tmp_path: Path, fake: tuple[Fake, LocalLLMSettings]) -> None:
    f, settings = fake
    f.alias = "cca-shadow@2.0.0#ffffffffffff"
    assert _code(_shadow, tmp_path, settings) == ("no_runtime", "alias_mismatch")
    assert f.chats == 0


def test_the_split_rules(tmp_path: Path, fake: tuple[Fake, LocalLLMSettings]) -> None:
    _, settings = fake
    locked = _testsets(tmp_path, LOCKED_LINE)
    assert _code(_shadow, tmp_path, settings, testsets_lock=locked) == (
        "not_allowed_here",
        "locked_split_exists",
    )
    assert _code(_shadow, tmp_path, settings, split="test") == ("not_allowed_here", "split")
    assert _code(_shadow, tmp_path, settings, split="test", set_dir=str(tmp_path)) == (
        "not_found",
        "testset_lock",
    )
    assert _code(_shadow, tmp_path, settings, split="x") == ("not_allowed_here", "split")


def test_a_locked_split_that_does_not_match_is_refused(
    tmp_path: Path, fake: tuple[Fake, LocalLLMSettings]
) -> None:
    _, settings = fake
    locked = _testsets(tmp_path, LOCKED_LINE)
    assert _code(
        _shadow, tmp_path, settings, split="test", set_dir=str(DEV), testsets_lock=locked
    ) == ("testset_mismatch", "locked_split")


@pytest.mark.parametrize(
    ("kw", "code"),
    [
        ({"sha256": "ABC"}, ("invalid_name", "sha256")),
        ({"sha256": "g" * 64}, ("invalid_name", "sha256")),
        ({"profile": "unknown"}, ("invalid_name", "profile")),
        ({"name": "Bad"}, ("invalid_name", None)),
        ({"version": "1"}, ("invalid_name", None)),
    ],
)
def test_bad_settings_are_refused_before_anything_is_asked(
    tmp_path: Path, fake: tuple[Fake, LocalLLMSettings], kw: dict[str, str], code: Any
) -> None:
    f, settings = fake
    assert _code(_shadow, tmp_path, settings, **kw) == code
    assert f.chats == 0


def test_the_eval_shadow_command(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str]
) -> None:
    seen: dict[str, Any] = {}

    def stub(**kw: Any) -> dict[str, Any]:
        seen.update(kw)
        return {"verdict": "fail" if kw["version"] == "9.9.9" else "pass"}

    monkeypatch.setattr(swap, "eval_shadow", stub)
    monkeypatch.delenv("INTELLIGENCE_LOCAL_LLM_URL", raising=False)
    argv = [
        "eval-shadow",
        "--name",
        "cca-shadow",
        "--version",
        "2.0.0",
        "--sha256",
        SHA,
        "--profile",
        "default",
        "--split",
        "dev",
        "--testsets",
        str(_testsets(tmp_path)),
    ]
    assert swap.main(argv) == 0
    assert json.loads(capsys.readouterr().out) == {"eval_shadow": {"verdict": "pass"}}
    assert seen["settings"].url == swap.OWN_MODEL_URL and seen["max_seconds"] == 90
    argv[4] = "9.9.9"
    assert swap.main(argv) == 1
    capsys.readouterr()
    assert swap.main([*argv, "--max-seconds", "0"]) == 2
    assert json.loads(capsys.readouterr().out)["detail"] == "max_seconds"
    monkeypatch.setenv("INTELLIGENCE_LOCAL_LLM_TIMEOUT", "nope")
    assert swap.main(argv) == 2
    assert json.loads(capsys.readouterr().out)["detail"] == "local_llm_settings"

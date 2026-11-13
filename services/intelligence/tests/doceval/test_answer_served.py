"""The on-host evaluation before a switch (own answer model plan, PR 4, Task 12):
``mhai-models eval-served`` against a fake ``llama-server`` on loopback.

The alias comes from ``models.lock``, never from the sidecar; the client is the worker's
own (``PinnedLlamaGenerator`` with its alias check before every item, the real
``OrchestratorModelClient`` and CCA prompt; a stand-in HGA instead of NLI). Every
question, alias and answer is synthetic.
"""

from __future__ import annotations

import hashlib
import json
import threading
from collections.abc import Iterator
from dataclasses import dataclass, field
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any

import pytest

from intelligence.doceval import report
from intelligence.doceval.answers import schema, served
from intelligence.modelstore import canonical, cli, lock, testsets
from intelligence.modelstore.reasons import Refused
from intelligence.tools.llama_client import LocalLLMSettings
from tests.doceval.answer_fakes import oracle, test_builder

REPO = Path(__file__).resolve().parents[4]
DEV = REPO / "testdata" / "answers" / "dev"
SHA = "0123456789ab" + "c" * 52
ALIAS = f"mhai-cca-synthetic@1.0.0#{SHA[:12]}"
EMPTY_TESTSETS = "# no entries\n"


@dataclass
class Fake:
    alias: str = ALIAS
    mode: str = "good"  # good, unsafe, miss
    chats: int = 0
    replies: dict[str, dict[str, str]] = field(default_factory=dict)


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
        yield f, LocalLLMSettings(url=url, timeout_s=10.0, start_timeout_s=1.0)
    finally:
        httpd.shutdown()
        httpd.server_close()


def _lock(tmp_path: Path, *, cca: bool = True) -> Path:
    pins = {"cca": lock.Pin("mhai-cca-synthetic", "1.0.0", SHA, "answer")} if cca else {}
    path = tmp_path / "models.lock"
    path.write_bytes(lock.to_bytes(lock.Lock(pins)))
    return path


def _testsets(tmp_path: Path, text: str = EMPTY_TESTSETS) -> Path:
    path = tmp_path / "testsets.lock"
    path.write_text(text, encoding="ascii")
    return path


def _run(tmp_path: Path, settings: LocalLLMSettings, **kw: Any) -> dict[str, Any]:
    args: dict[str, Any] = {
        "split": "dev",
        "set_dir": None,
        "max_seconds": 90,
        "settings": settings,
        "builder_for": test_builder,
        **kw,
    }
    if "lock_path" not in args:
        args["lock_path"] = _lock(tmp_path)
    if "testsets_lock" not in args:
        args["testsets_lock"] = _testsets(tmp_path)
    return served.run(**args)


def test_the_expected_alias_is_the_pinned_version_string(tmp_path: Path) -> None:
    assert served.expected_alias(_lock(tmp_path)) == ALIAS


def test_no_cca_pin_is_refused(tmp_path: Path) -> None:
    with pytest.raises(Refused) as e:
        served.expected_alias(_lock(tmp_path, cca=False))
    assert (e.value.reason, e.value.detail_code) == ("not_found", "cca_pin")


def test_a_good_model_passes_on_the_dev_split(
    tmp_path: Path, fake: tuple[Fake, LocalLLMSettings]
) -> None:
    f, settings = fake
    out = _run(tmp_path, settings)
    assert out["verdict"] == "pass" and out["failed"] == []
    assert out["split"] == "dev" and out["alias"] == ALIAS
    dev_meta = schema.load_set(DEV).meta
    # the dev split is rebuilt byte for byte from its public seed
    assert out["testset"]["sha256"] == dev_meta["sha256"]
    assert out["counts"]["items"] == dev_meta["items"] == f.chats
    assert out["metrics"]["escalation_recall"] == "1.000000"
    assert out["thresholds_sha256"] == report.packaged_thresholds("answer").sha256
    # aggregates only: no question or answer text in the summary
    text = json.dumps(out)
    assert not any(i.question in text for i in schema.load_set(DEV).items)


@pytest.mark.parametrize(
    ("mode", "metric"), [("unsafe", "unsafe_rate"), ("miss", "escalation_recall")]
)
def test_an_unsafe_or_missed_answer_fails(
    tmp_path: Path, fake: tuple[Fake, LocalLLMSettings], mode: str, metric: str
) -> None:
    f, settings = fake
    f.mode = mode
    out = _run(tmp_path, settings)
    assert out["verdict"] == "fail"
    assert f"overall.{metric}" in out["failed"]


def test_another_alias_is_refused_before_any_question(
    tmp_path: Path, fake: tuple[Fake, LocalLLMSettings]
) -> None:
    f, settings = fake
    f.alias = "mhai-cca-synthetic@0.9.0#ba9876543210"
    with pytest.raises(Refused) as e:
        _run(tmp_path, settings)
    assert (e.value.reason, e.value.detail_code) == ("no_runtime", "alias_mismatch")
    assert f.chats == 0


def test_an_unreachable_sidecar_is_refused(tmp_path: Path) -> None:
    settings = LocalLLMSettings(url="http://127.0.0.1:9", start_timeout_s=1.0)
    with pytest.raises(Refused) as e:
        _run(tmp_path, settings)
    assert (e.value.reason, e.value.detail_code) == ("no_runtime", "unreachable")


def test_too_slow_fails_even_when_every_answer_is_right(
    tmp_path: Path, fake: tuple[Fake, LocalLLMSettings]
) -> None:
    _, settings = fake
    ticks = iter(range(0, 10**6, 100))  # every clock read is 100 s later
    out = _run(tmp_path, settings, max_seconds=90, clock=lambda: float(next(ticks)))
    assert out["verdict"] == "fail" and out["failed"] == ["median_seconds"]
    assert out["median_seconds"] == 100.0


def _locked_copy(tmp_path: Path) -> tuple[Path, testsets.LockEntry]:
    """The dev split re-labelled as a locked test split (synthetic), and its lock entry."""
    folder = tmp_path / "locked"
    folder.mkdir()
    items = (DEV / "items.jsonl").read_bytes()
    (folder / "items.jsonl").write_bytes(items)
    meta = json.loads((DEV / "testset.json").read_bytes())
    meta.update(purpose="test", locked=True, store_version_ids={"items.jsonl": "v-synthetic-0001"})
    raw = canonical.dumps(meta)
    (folder / "testset.json").write_bytes(raw)
    entry = testsets.LockEntry("clinical_answer", "1.0.0", hashlib.sha256(raw).hexdigest())
    return folder, entry


def test_a_locked_split_must_be_the_one_scored(
    tmp_path: Path, fake: tuple[Fake, LocalLLMSettings]
) -> None:
    _, settings = fake
    folder, entry = _locked_copy(tmp_path)
    tl = _testsets(tmp_path, entry.text + "\n")
    with pytest.raises(Refused) as e:
        _run(tmp_path, settings, testsets_lock=tl)  # dev asked for
    assert e.value.detail_code == "locked_split_exists"
    out = _run(tmp_path, settings, testsets_lock=tl, split="test", set_dir=folder)
    assert out["split"] == "test" and out["verdict"] == "pass"
    assert out["testset"]["sha256"] == entry.sha256


def test_a_locked_split_that_is_not_the_locked_one_is_refused(
    tmp_path: Path, fake: tuple[Fake, LocalLLMSettings]
) -> None:
    _, settings = fake
    folder, entry = _locked_copy(tmp_path)
    other = testsets.LockEntry("clinical_answer", "1.0.0", "f" * 64)
    with pytest.raises(Refused) as e:
        served.load_locked(folder, other)
    assert e.value.reason == "testset_mismatch"
    with pytest.raises(Refused):
        served.load_locked(DEV, entry)  # a dev split is never the locked one


@pytest.mark.parametrize("bad", ["0", "-1", "abc", "9999", " 90"])
def test_max_seconds_is_checked(bad: str) -> None:
    with pytest.raises(Refused):
        served.check_max_seconds(bad)


def test_cli_refuses_without_a_cca_pin(tmp_path: Path, capsys: pytest.CaptureFixture[str]) -> None:
    argv = [
        "eval-served",
        "--lock",
        str(_lock(tmp_path, cca=False)),
        "--testsets",
        str(_testsets(tmp_path)),
        "--split",
        "dev",
    ]
    assert cli.main(argv) == 2
    assert json.loads(capsys.readouterr().out) == {"refused": "not_found", "detail": "cca_pin"}


def test_cli_testset_fetch_needs_a_locked_split_and_an_empty_folder(
    tmp_path: Path, capsys: pytest.CaptureFixture[str]
) -> None:
    out = tmp_path / "split"
    out.mkdir()
    argv = ["testset-fetch", "--testsets", str(_testsets(tmp_path)), "--trust-policy", "x"]
    assert cli.main([*argv, "--to", str(out)]) == 2
    assert json.loads(capsys.readouterr().out)["detail"] == "testset_lock"
    (out / "left-over").write_text("synthetic", encoding="ascii")
    assert cli.main([*argv, "--to", str(out)]) == 2
    assert json.loads(capsys.readouterr().out)["detail"] == "testset_dir"

"""mhai-doceval: the commands, and what each refuses."""

import json
from pathlib import Path

import pytest

from intelligence.doceval import cli, report
from intelligence.modelstore import canonical
from tests.doceval import minisets


def _out(capsys: pytest.CaptureFixture[str]) -> str:
    return capsys.readouterr().out


def _set(root: Path, purpose: str = "dev", n: int = 6) -> minisets.MiniSet:
    docs = [minisets.MiniDoc("lab_typed_clean", minisets.visible_truth(k)) for k in range(n)]
    s = minisets.build(root, docs)
    if purpose != "dev":
        path = root / "testset.json"
        manifest = json.loads(path.read_bytes())
        manifest["purpose"] = purpose
        path.write_bytes(canonical.dumps(manifest))
    return s


@pytest.fixture(scope="module")
def dev_set(tmp_path_factory: pytest.TempPathFactory) -> minisets.MiniSet:
    return _set(tmp_path_factory.mktemp("cli") / "set")


def test_cli_refuses_test_build(tmp_path: Path, capsys: pytest.CaptureFixture[str]) -> None:
    code = cli.main(["build-set", "--purpose", "test", "--out", str(tmp_path / "out")])
    assert code == 2
    assert json.loads(_out(capsys)) == {"refused": "not_allowed_here"}
    assert not (tmp_path / "out").exists()


def test_cli_refuses_unknown_purpose_and_bad_scale(
    tmp_path: Path, capsys: pytest.CaptureFixture[str]
) -> None:
    assert cli.main(["build-set", "--purpose", "prod", "--out", str(tmp_path / "o")]) == 2
    assert json.loads(_out(capsys))["refused"] == "not_allowed_here"
    args = ["build-set", "--purpose", "dev", "--out", str(tmp_path / "o"), "--scale", "99"]
    assert cli.main(args) == 2
    assert json.loads(_out(capsys)) == {"refused": "bad_scale"}


def test_cli_oracle_needs_the_test_only_flag(
    tmp_path: Path, dev_set: minisets.MiniSet, capsys: pytest.CaptureFixture[str]
) -> None:
    args = [
        "run",
        "--extractor",
        "oracle",
        "--set",
        str(dev_set.root),
        "--out",
        str(tmp_path / "r"),
    ]
    assert cli.main(args) == 2
    assert json.loads(_out(capsys)) == {"refused": "not_allowed_here"}
    assert not (tmp_path / "r").exists()


def test_cli_oracle_never_on_the_locked_split(
    tmp_path: Path, capsys: pytest.CaptureFixture[str], monkeypatch: pytest.MonkeyPatch
) -> None:
    locked = _set(tmp_path / "locked", purpose="test")
    args = [
        "run",
        "--extractor",
        "oracle",
        "--test-only-oracle",
        "--set",
        str(locked.root),
        "--out",
        str(tmp_path / "r"),
    ]
    assert cli.main(args) == 2
    assert json.loads(_out(capsys)) == {"refused": "not_allowed_here"}
    dev = _set(tmp_path / "dev")
    args[5] = str(dev.root)
    monkeypatch.setenv("MHAI_EVAL_ROLE", "locked")
    assert cli.main(args) == 2
    assert json.loads(_out(capsys)) == {"refused": "not_allowed_here"}
    assert not (tmp_path / "r").exists()


def test_cli_oracle_run_rehearsal_only(
    tmp_path: Path, dev_set: minisets.MiniSet, capsys: pytest.CaptureFixture[str]
) -> None:
    base = ["run", "--extractor", "oracle", "--test-only-oracle", "--set", str(dev_set.root)]
    assert cli.main([*base, "--out", str(tmp_path / "p1"), "--gate", "phase1"]) == 2
    assert json.loads(_out(capsys)) == {"refused": "not_allowed_here"}
    assert not (tmp_path / "p1").exists()

    assert cli.main([*base, "--out", str(tmp_path / "r")]) == 0
    summary = json.loads(_out(capsys))
    assert summary["verdicts"] == {"rehearsal": "pass"}
    raw = (tmp_path / "r" / "evaluation.json").read_bytes()
    rep = report.parse_report(raw)
    assert rep["split"]["label"] == report.DEV_LABEL
    md = (tmp_path / "r" / "evaluation.md").read_text()
    assert md == report.render_markdown(rep)
    # never overwrites
    assert cli.main([*base, "--out", str(tmp_path / "r")]) == 2
    assert json.loads(_out(capsys)) == {"refused": "not_allowed_here"}
    assert (tmp_path / "r" / "evaluation.json").read_bytes() == raw


def test_cli_report_and_compare(
    tmp_path: Path, dev_set: minisets.MiniSet, capsys: pytest.CaptureFixture[str]
) -> None:
    base = ["run", "--extractor", "oracle", "--test-only-oracle", "--set", str(dev_set.root)]
    assert cli.main([*base, "--out", str(tmp_path / "a")]) == 0
    assert cli.main([*base, "--out", str(tmp_path / "b"), "--oracle-drop", "0.5"]) == 0
    _out(capsys)
    a = tmp_path / "a" / "evaluation.json"
    b = tmp_path / "b" / "evaluation.json"
    assert cli.main(["report", str(a)]) == 0
    assert _out(capsys) == (tmp_path / "a" / "evaluation.md").read_text()
    assert cli.main(["compare", str(a), str(b)]) == 0
    md = _out(capsys)
    assert "Regressions:" in md and "Regressions: 0" not in md
    # a tampered report is refused
    (tmp_path / "bad.json").write_text(json.dumps(json.loads(a.read_bytes()), indent=1))
    assert cli.main(["report", str(tmp_path / "bad.json")]) == 2
    assert json.loads(_out(capsys)) == {"refused": "not_canonical"}


def test_cli_unknown_extractor_and_missing_runtime(
    tmp_path: Path, dev_set: minisets.MiniSet, capsys: pytest.CaptureFixture[str]
) -> None:
    args = ["run", "--extractor", "magic", "--set", str(dev_set.root), "--out", str(tmp_path / "r")]
    assert cli.main(args) == 2
    assert json.loads(_out(capsys)) == {"refused": "no_runtime"}
    args = ["run", "--extractor", "tiny", "--set", str(dev_set.root), "--out", str(tmp_path / "r")]
    assert cli.main(args) == 2  # no --model
    assert json.loads(_out(capsys))["refused"] in {"no_runtime", "not_found"}
    assert not (tmp_path / "r").exists()


def test_cli_scan(tmp_path: Path, capsys: pytest.CaptureFixture[str]) -> None:
    folder = tmp_path / "s"
    folder.mkdir()
    (folder / "notes.txt").write_text("x")
    assert cli.main(["scan", str(folder)]) == 2
    out = json.loads(_out(capsys))
    assert out["ok"] is False
    assert out["findings"] == [{"path": "notes.txt", "rule": "unexpected_file"}]


def test_cli_intake_refuses_a_bad_manifest(
    tmp_path: Path, capsys: pytest.CaptureFixture[str]
) -> None:
    images = tmp_path / "images"
    images.mkdir()
    for bad in (b"not json", b'{"schema":"other","pages":[]}', b"[]"):
        (tmp_path / "m.json").write_bytes(bad)
        args = ["intake", "--images", str(images), "--manifest", str(tmp_path / "m.json")]
        assert cli.main([*args, "--out", str(tmp_path / "o")]) == 2
        assert json.loads(_out(capsys)) == {"refused": "manifest_invalid"}
    assert not (tmp_path / "o").exists()


def test_cli_logs_and_prints_no_values(
    tmp_path: Path, capsys: pytest.CaptureFixture[str], caplog: pytest.LogCaptureFixture
) -> None:
    marker = "Zqxmarkerwv"
    docs = [
        minisets.MiniDoc("lab_typed_clean", minisets.visible_truth(k, text=marker))
        for k in range(6)
    ]
    s = minisets.build(tmp_path / "set", docs)
    caplog.set_level("INFO")  # the runner's DEBUG lines carry ids by design
    args = ["run", "--extractor", "oracle", "--test-only-oracle", "--set", str(s.root)]
    assert cli.main([*args, "--out", str(tmp_path / "r"), "--oracle-corrupt", "0.5"]) == 0
    out = capsys.readouterr()
    for text in (out.out, out.err, caplog.text):
        assert marker not in text
        for doc_id in s.ids:
            assert doc_id not in text

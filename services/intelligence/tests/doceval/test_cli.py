"""mhai-doceval: the commands, and what each refuses."""

import json
from pathlib import Path
from typing import Any

import pytest

from intelligence.doceval import cli, report
from intelligence.modelstore import canonical, fixtures
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


@pytest.mark.parametrize(
    "extra",
    [
        ["--extractor", "tiny", "--model-version", "Extract=0.1.0"],
        ["--extractor", "tiny", "--model-version", "extract=0.1"],
        ["--extractor", "tiny", "--model-version", "extract"],
        ["--extractor", "openai", "--engine", "<details>"],
        ["--extractor", "openai", "--hardware", "nvidia l40s"],
        ["--extractor", "tiny", "--engine-version", "1|2"],
        ["--extractor", "tiny", "--deadline", "nan"],
        ["--extractor", "tiny", "--deadline", "0"],
        ["--extractor", "openai", "--timeout", "-1"],
        ["--extractor", "oracle", "--oracle-drop", "2"],
        ["--extractor", "tiny", "--gate", "phase9"],
    ],
)
def test_cli_run_checks_inputs_before_any_io(
    tmp_path: Path, capsys: pytest.CaptureFixture[str], extra: list[str]
) -> None:
    # the set does not exist: a check that ran after I/O would say testset_mismatch
    args = ["run", "--set", str(tmp_path / "nowhere"), "--out", str(tmp_path / "r"), *extra]
    assert cli.main(args) == 2
    assert json.loads(_out(capsys)) == {"refused": "invalid_name"}
    assert not (tmp_path / "r").exists()


def test_cli_oracle_refused_when_eval_role_set_even_empty(
    tmp_path: Path,
    dev_set: minisets.MiniSet,
    capsys: pytest.CaptureFixture[str],
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setenv("MHAI_EVAL_ROLE", "")
    args = ["run", "--extractor", "oracle", "--test-only-oracle", "--set", str(dev_set.root)]
    assert cli.main([*args, "--out", str(tmp_path / "r")]) == 2
    assert json.loads(_out(capsys)) == {"refused": "not_allowed_here"}


def test_cli_oracle_refused_on_a_signed_or_locked_set(
    tmp_path: Path, capsys: pytest.CaptureFixture[str]
) -> None:
    signed = _set(tmp_path / "signed")
    (signed.root / "testset.sig").write_bytes(b"sig")
    locked = _set(tmp_path / "locked")
    path = locked.root / "testset.json"
    manifest = json.loads(path.read_bytes())
    manifest["locked"] = True
    path.write_bytes(canonical.dumps(manifest))
    # The other field of a locked split (modelstore.testsets), even without ``locked``.
    pinned = _set(tmp_path / "pinned")
    path = pinned.root / "testset.json"
    manifest = json.loads(path.read_bytes())
    manifest["store_version_ids"] = {}
    path.write_bytes(canonical.dumps(manifest))
    for s in (signed, locked, pinned):
        args = ["run", "--extractor", "oracle", "--test-only-oracle", "--set", str(s.root)]
        assert cli.main([*args, "--out", str(tmp_path / "r")]) == 2
        assert json.loads(_out(capsys)) == {"refused": "not_allowed_here"}
    assert not (tmp_path / "r").exists()


def test_cli_timeout_stop_written_other_failures_refused(
    tmp_path: Path,
    dev_set: minisets.MiniSet,
    capsys: pytest.CaptureFixture[str],
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    from intelligence.doceval import runner
    from intelligence.modelstore.reasons import Refused

    def stop(detail: str) -> Any:
        def run(*a: Any, **k: Any) -> Any:
            raise Refused("evaluation_failed", detail_code=detail)

        return run

    base = ["run", "--extractor", "oracle", "--test-only-oracle", "--set", str(dev_set.root)]
    monkeypatch.setattr(runner, "run", stop("timeouts"))
    assert cli.main([*base, "--out", str(tmp_path / "t")]) == 0
    assert json.loads(_out(capsys))["stopped"] == "timeouts"
    rep = report.read_report(tmp_path / "t" / "evaluation.json")
    assert rep["verdicts"]["rehearsal"]["verdict"] == "fail"
    monkeypatch.setattr(runner, "run", stop("internal"))
    assert cli.main([*base, "--out", str(tmp_path / "i")]) == 2
    assert json.loads(_out(capsys)) == {"refused": "evaluation_failed"}
    assert not (tmp_path / "i").exists()


def test_cli_unexpected_error_is_a_fixed_code(
    tmp_path: Path,
    dev_set: minisets.MiniSet,
    capsys: pytest.CaptureFixture[str],
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    def boom(*a: Any, **k: Any) -> Any:
        raise RuntimeError("secret Zqxmarkerwv")

    monkeypatch.setattr(report, "build_report", boom)
    args = ["run", "--extractor", "oracle", "--test-only-oracle", "--set", str(dev_set.root)]
    assert cli.main([*args, "--out", str(tmp_path / "r")]) == 2
    out = capsys.readouterr()
    assert json.loads(out.out) == {"refused": "internal_error"}
    assert "Zqxmarkerwv" not in out.out + out.err and "Traceback" not in out.err
    assert not (tmp_path / "r").exists()


def test_cli_training_manifest_read_safely(
    tmp_path: Path, dev_set: minisets.MiniSet, capsys: pytest.CaptureFixture[str]
) -> None:
    # Through training.load (Task 14): the whole manifest is checked, floats and a final
    # newline are accepted, and floats reach the report as fixed decimals.
    run = fixtures.make_run(tmp_path / "run", "good", 3)
    target = run / "training_manifest.json"
    obj = json.loads(target.read_bytes())
    obj["notebook_metrics"] = {"val_f1": 0.8, "steps": 900}
    target.write_text(json.dumps(obj) + "\n", encoding="utf-8")
    (tmp_path / "linked").mkdir()
    link = tmp_path / "linked" / "training_manifest.json"
    link.symlink_to(target)
    (tmp_path / "big").mkdir()
    big = tmp_path / "big" / "training_manifest.json"
    big.write_bytes(b" " * (17 * 1024 * 1024))
    (tmp_path / "bad").mkdir()
    bad = tmp_path / "bad" / "training_manifest.json"
    bad.write_bytes(canonical.dumps({"notebook_metrics": {"val_f1": "0.8"}}))
    misnamed = tmp_path / "tm.json"
    misnamed.write_bytes(target.read_bytes())
    base = ["run", "--extractor", "oracle", "--test-only-oracle", "--set", str(dev_set.root)]
    for path, code in (
        (link, "file_not_allowed"),
        (big, "too_large"),
        (bad, "manifest_invalid"),
        (misnamed, "file_not_allowed"),
        (tmp_path / "missing" / "training_manifest.json", "training_manifest_missing"),
    ):
        args = [*base, "--out", str(tmp_path / "r"), "--training-manifest", str(path)]
        assert cli.main(args) == 2, path
        assert json.loads(_out(capsys)) == {"refused": code}, path
    args = [*base, "--out", str(tmp_path / "ok"), "--training-manifest", str(target)]
    assert cli.main(args) == 0
    _out(capsys)
    rep = report.read_report(tmp_path / "ok" / "evaluation.json")
    assert rep["notebook_metrics"] == {
        "informational": True,
        "metrics": {"steps": 900, "val_f1": "0.800000"},
    }


def test_cli_split_checked_before_the_run(
    tmp_path: Path, capsys: pytest.CaptureFixture[str], monkeypatch: pytest.MonkeyPatch
) -> None:
    from intelligence.doceval import runner

    train = _set(tmp_path / "train", purpose="train")
    called: list[bool] = []
    monkeypatch.setattr(runner, "run", lambda *a, **k: called.append(True))
    args = ["run", "--extractor", "tiny", "--model", str(tmp_path), "--set", str(train.root)]
    assert cli.main([*args, "--out", str(tmp_path / "r")]) == 2
    assert json.loads(_out(capsys)) == {"refused": "not_allowed_here"}
    assert not called and not (tmp_path / "r").exists()


def test_cli_result_must_be_the_set_that_was_checked(
    tmp_path: Path,
    dev_set: minisets.MiniSet,
    capsys: pytest.CaptureFixture[str],
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    import dataclasses

    from intelligence.doceval import runner

    real = runner.run
    base = ["run", "--extractor", "oracle", "--test-only-oracle", "--set", str(dev_set.root)]
    for change in ({"testset_sha256": "0" * 64}, {"purpose": "test"}):
        monkeypatch.setattr(
            runner,
            "run",
            lambda *a, change=change, **k: dataclasses.replace(real(*a, **k), **change),
        )
        out = tmp_path / f"r{len(change)}{sorted(change)[0]}"
        assert cli.main([*base, "--out", str(out)]) == 2
        assert json.loads(_out(capsys)) == {"refused": "testset_mismatch"}
        assert not out.exists()

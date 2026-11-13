"""The export step a trainer calls at the end of a Colab notebook (own model PR 1).

llama.cpp is faked: a ``convert_hf_to_gguf.py`` that copies a tiny synthetic GGUF and a
``llama-quantize`` that copies its input, each logging its arguments.
"""

import json
import os
import stat
import subprocess
import sys
from pathlib import Path
from typing import Any

import pytest

from intelligence.modelstore import cli, export, files, fixtures, promote, training
from intelligence.modelstore.reasons import FIXES, Refused
from tests.modelstore.fakes import gguf as g

REVISION = "0123456789abcdef0123456789abcdef01234567"
PIN = "mhai-tiny-extractor@1.0.0#" + "ab" * 32
MISNAMED_FIX = (
    "found manifest.json; rename it to training_manifest.json"
    " (manifest.json is reserved for the signed store manifest)"
)

_CONVERT = """
import json, pathlib, sys
here = pathlib.Path(__file__).parent
with open(here / "calls.jsonl", "a") as log:
    log.write(json.dumps(["convert", *sys.argv[1:]]) + "\\n")
if (here / "FAIL").exists():
    sys.exit(1)
out = sys.argv[sys.argv.index("--outfile") + 1]
pathlib.Path(out).write_bytes((here / "tiny.gguf").read_bytes())
"""

_QUANTIZE = """#!{python}
import json, pathlib, shutil, sys
here = pathlib.Path(__file__).resolve().parents[2]
with open(here / "calls.jsonl", "a") as log:
    log.write(json.dumps(["quantize", *sys.argv[1:]]) + "\\n")
shutil.copyfile(sys.argv[1], sys.argv[2])
"""


@pytest.fixture
def llama(tmp_path: Path) -> Path:
    d = tmp_path / "llama.cpp"
    (d / "build" / "bin").mkdir(parents=True)
    (d / "tiny.gguf").write_bytes(g.tiny(b"qwen2"))
    (d / "convert_hf_to_gguf.py").write_text(_CONVERT, encoding="utf-8")
    q = d / "build" / "bin" / "llama-quantize"
    q.write_text(_QUANTIZE.format(python=sys.executable), encoding="utf-8")
    q.chmod(q.stat().st_mode | stat.S_IXUSR)
    return d


def _calls(llama: Path) -> list[list[str]]:
    log = llama / "calls.jsonl"
    if not log.exists():
        return []
    return [json.loads(line) for line in log.read_text(encoding="utf-8").splitlines()]


@pytest.fixture
def run(tmp_path: Path) -> Path:
    """A trained run as the notebook leaves it: weights, config, tokenizer, no manifest."""
    r = fixtures.make_run(tmp_path / "run", "good", 3)
    (r / training.FILE_NAME).unlink()
    return r


def _kw(**over: Any) -> dict[str, Any]:
    kw: dict[str, Any] = {
        "name": "mhai-answer",
        "version": "0.1.0",
        "datasets": [fixtures.TRAIN_DATASET],
        "trained_by": fixtures.TRAINED_BY,
        "base_upstream": fixtures.BASE_REPO,
        "base_revision": REVISION,
        "base_licence": fixtures.LICENCE,
        "notebook_metrics": {"train_loss": 0.42, "steps": 100},
    }
    kw.update(over)
    return kw


def _argv(run: Path, *extra: str) -> list[str]:
    return [
        str(run),
        "--name",
        "mhai-answer",
        "--version",
        "0.1.0",
        "--dataset",
        fixtures.TRAIN_DATASET,
        "--trained-by",
        fixtures.TRAINED_BY,
        "--base-upstream",
        fixtures.BASE_REPO,
        "--base-licence",
        fixtures.LICENCE,
        *extra,
    ]


def test_export_writes_manifest_and_gguf_and_passes(run: Path, llama: Path) -> None:
    out = export.export_run(run, llama_cpp=llama, **_kw())
    tm = training.load_run(run)
    assert (tm.name, tm.role, tm.datasets) == ("mhai-answer", "cca", (fixtures.TRAIN_DATASET,))
    assert isinstance(tm.base, training.UpstreamBase) and tm.base.revision == REVISION
    assert dict(tm.notebook_metrics) == {"train_loss": 0.42, "steps": 100}
    assert tm.framework and all(p in training.FRAMEWORKS for p, _ in tm.framework)
    assert out.gguf == "model-q4_k_m.gguf"
    assert (run / out.gguf).read_bytes() == g.tiny(b"qwen2")
    assert out.files_sha256 == promote.files_hash(files.check_run(run))
    convert, quantize = _calls(llama)
    assert convert[0] == "convert" and convert[1] == str(run)
    assert convert[convert.index("--outtype") + 1] == "f16"
    assert not convert[convert.index("--outfile") + 1].startswith(str(run))  # f16 kept out
    assert quantize[2] == str(run / "model-q4_k_m.gguf") and quantize[3] == "Q4_K_M"
    assert sorted(p.name for p in run.iterdir() if p.suffix == ".gguf") == ["model-q4_k_m.gguf"]


def test_q8_0_is_written_by_the_converter_directly(run: Path, llama: Path) -> None:
    out = export.export_run(run, llama_cpp=llama, quant="q8_0", **_kw())
    assert out.gguf == "model-q8_0.gguf"
    (convert,) = _calls(llama)
    assert convert[convert.index("--outtype") + 1] == "q8_0"
    assert convert[convert.index("--outfile") + 1] == str(run / "model-q8_0.gguf")


def test_llama_cpp_found_through_the_environment(
    run: Path, llama: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setenv(export.LLAMA_CPP_ENV, str(llama))
    assert export.export_run(run, **_kw()).gguf == "model-q4_k_m.gguf"


def test_unknown_quant_refused_before_anything_is_written(run: Path, llama: Path) -> None:
    with pytest.raises(export.ExportError, match="q4_k_m"):
        export.export_run(run, llama_cpp=llama, quant="q3_xs", **_kw())
    assert not (run / training.FILE_NAME).exists() and _calls(llama) == []


def test_missing_llama_cpp_is_a_clear_message(
    run: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.delenv(export.LLAMA_CPP_ENV, raising=False)
    monkeypatch.setenv("PATH", str(tmp_path / "empty"))
    with pytest.raises(export.ExportError) as err:
        export.export_run(run, **_kw())
    assert "llama.cpp" in str(err.value) and "--no-gguf" in str(err.value)
    assert not (run / training.FILE_NAME).exists()
    with pytest.raises(export.ExportError, match="llama-quantize"):
        (tmp_path / "half").mkdir()
        (tmp_path / "half" / "convert_hf_to_gguf.py").write_text("", encoding="utf-8")
        export.export_run(run, llama_cpp=tmp_path / "half", **_kw())


def test_failed_conversion_is_a_clear_message(run: Path, llama: Path) -> None:
    (llama / "FAIL").write_text("", encoding="utf-8")
    with pytest.raises(export.ExportError, match="conversion failed"):
        export.export_run(run, llama_cpp=llama, **_kw())
    assert not any(p.suffix == ".gguf" for p in run.iterdir())


def test_no_gguf_still_writes_and_checks(run: Path) -> None:
    out = export.export_run(run, gguf=False, **_kw())
    assert out.gguf is None
    assert out.files_sha256 == promote.files_hash(files.check_run(run))


def test_promoted_base_is_left_to_promotion(run: Path) -> None:
    kw = _kw(base_upstream=None, base_revision=None, base_licence=None, base_promoted=PIN)
    out = export.export_run(run, gguf=False, **kw)
    assert isinstance(training.load_run(run).base, training.PromotedBase)
    assert out.base_verified_at_promotion is True


def test_misnamed_manifest_refused_before_writing(run: Path, llama: Path) -> None:
    (run / "manifest.json").write_text("{}", encoding="utf-8")
    with pytest.raises(Refused) as err:
        export.export_run(run, llama_cpp=llama, **_kw())
    assert err.value.reason == "training_manifest_misnamed"
    assert FIXES["training_manifest_misnamed"] == MISNAMED_FIX
    assert not (run / training.FILE_NAME).exists() and _calls(llama) == []


def test_check_run_refuses_a_misnamed_manifest(run: Path) -> None:
    (run / "manifest.json").write_text("{}", encoding="utf-8")
    with pytest.raises(Refused) as err:
        files.check_run(run)
    assert err.value.reason == "training_manifest_misnamed"


@pytest.mark.parametrize(
    ("over", "reason"),
    [
        ({"name": "Bad Name"}, "manifest_invalid"),
        ({"version": "1.0"}, "invalid_name"),
        ({"role": "ocr"}, "role_unknown"),
        ({"datasets": ["clinical_document-unknown@9.9.9"]}, "dataset_unknown"),
        ({"base_revision": "main"}, "revision_unpinned"),
        ({"base_licence": "LicenseRef-x"}, "licence_not_allowed"),
        ({"trained_by": "Alice"}, "manifest_invalid"),
        ({"notebook_metrics": {"patient_123": 1}}, "manifest_invalid"),
        ({"base_promoted": PIN}, "manifest_invalid"),  # both kinds of base
    ],
)
def test_bad_values_refused(run: Path, over: dict[str, Any], reason: str) -> None:
    with pytest.raises(Refused) as err:
        export.export_run(run, gguf=False, **_kw(**over))
    assert err.value.reason == reason


def test_every_refusal_a_trainer_can_hit_has_a_fix() -> None:
    for reason in (
        "training_manifest_misnamed",
        "file_not_allowed",
        "pickle_refused",
        "code_refused",
        "missing_file",
        "gguf_invalid",
        "dataset_unknown",
        "dataset_not_synthetic",
        "revision_unpinned",
        "licence_not_allowed",
        "role_unknown",
        "manifest_invalid",
        "invalid_name",
        "too_large",
    ):
        assert export.fix_for(Refused(reason)), reason


def test_main_prints_the_fix(run: Path, capsys: pytest.CaptureFixture[str]) -> None:
    code = export.main(_argv(run, "--base-revision", "main", "--no-gguf"))
    out = capsys.readouterr().out
    assert code == 2
    assert "refused: revision_unpinned" in out and "fix: " in out
    (run / training.FILE_NAME).unlink()
    (run / "manifest.json").write_text("{}", encoding="utf-8")
    assert export.main(_argv(run, "--base-revision", REVISION, "--no-gguf")) == 2
    assert f"fix: {MISNAMED_FIX}" in capsys.readouterr().out


def test_main_success_and_cli_delegation(
    run: Path, llama: Path, capsys: pytest.CaptureFixture[str]
) -> None:
    argv = _argv(
        run,
        "--base-revision",
        REVISION,
        "--metric",
        "train_loss=0.42",
        "--metric",
        "steps=100",
        "--llama-cpp",
        str(llama),
    )
    assert export.main(argv) == 0
    out = capsys.readouterr().out
    sha = promote.files_hash(files.check_run(run))
    assert f"--expect-files {sha}" in out and "model-q4_k_m.gguf" in out
    assert dict(training.load_run(run).notebook_metrics) == {"train_loss": 0.42, "steps": 100}
    assert cli.main(["export", *argv]) == 0  # the same step, through mhai-models
    assert "--expect-files " in capsys.readouterr().out


@pytest.mark.parametrize("metric", ["train_loss", "train_loss=", "train_loss=nan", "x=1e999"])
def test_main_bad_metric(run: Path, metric: str, capsys: pytest.CaptureFixture[str]) -> None:
    argv = _argv(run, "--base-revision", REVISION, "--no-gguf", "--metric", metric)
    assert export.main(argv) == 2
    assert "--metric" in capsys.readouterr().out
    assert not (run / training.FILE_NAME).exists()


def test_imports_stay_light() -> None:
    """Colab installs the package without its dependencies: the export step must import
    with the standard library and the light model store modules only."""
    code = (
        "import json, sys, intelligence.modelstore.export; print(json.dumps(sorted(sys.modules)))"
    )
    env = {k: v for k, v in os.environ.items() if k != "PYTHONSTARTUP"}
    done = subprocess.run(
        [sys.executable, "-c", code],
        capture_output=True,
        text=True,
        check=True,
        env=env,
        cwd=Path(__file__).resolve().parents[2],
    )
    loaded = set(json.loads(done.stdout))
    heavy = {"numpy", "torch", "transformers", "boto3", "botocore", "PIL", "httpx", "pydantic"}
    assert not heavy & loaded
    ours = {m for m in loaded if m.startswith("intelligence")}
    assert ours <= {
        "intelligence",
        "intelligence.modelstore",
        "intelligence.modelstore.canonical",
        "intelligence.modelstore.export",
        "intelligence.modelstore.files",
        "intelligence.modelstore.names",
        "intelligence.modelstore.reasons",
        "intelligence.modelstore.training",
    }

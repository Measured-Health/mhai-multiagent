"""The model card and the data statement: ids, enums and numbers only."""

import copy
import dataclasses
import json
import random
from pathlib import Path
from typing import Any

import pytest

from intelligence.doceval import extractors, report, runner
from intelligence.modelstore import canonical, documents, fixtures, training
from intelligence.modelstore.reasons import Refused
from tests.doceval import minisets

REPO = Path(__file__).resolve().parents[4]
MARKER = "Zqxmarkerwv"
TINY = extractors.ExtractorInfo("mhai-tiny-extractor", {"extract": "0.1.0"}, "numpy", "cpu")


@pytest.fixture(scope="module")
def result(tmp_path_factory: pytest.TempPathFactory) -> runner.RunResult:
    docs = [
        minisets.MiniDoc("lab_typed_clean", minisets.visible_truth(k, text=MARKER))
        for k in range(5)
    ] + [
        minisets.MiniDoc("lab_typed_poor", minisets.visible_truth(20 + k, text=MARKER))
        for k in range(5)
    ]
    mini = minisets.build(tmp_path_factory.mktemp("mini") / "set", docs)
    oracle = extractors.OracleExtractor(mini.root / "truth", random.Random(7))
    return dataclasses.replace(runner.run(oracle, mini.root), extractor=TINY)


def _with_bad_slice(result: runner.RunResult, slice_name: str) -> runner.RunResult:
    m = copy.deepcopy(result.metrics)
    m["slices"][slice_name]["field_pr"]["text"] = {
        "tp": 1,
        "fp": 50,
        "fn": 0,
        "precision": 1 / 51,
        "recall": 1.0,
    }
    return dataclasses.replace(result, metrics=m)


@pytest.fixture(scope="module")
def evaluation(result: runner.RunResult) -> dict[str, Any]:
    gates = [report.packaged_thresholds(g) for g in ("phase1", "rehearsal")]
    rep = report.build_report(_with_bad_slice(result, "lab_typed_poor"), gates)
    return report.parse_report(report.dumps(rep))  # as Task 19 will read it back


@pytest.fixture
def tm(tmp_path: Path) -> training.TrainingManifest:
    return training.load_run(fixtures.make_run(tmp_path / "run", "good", 3))


@pytest.fixture
def registry() -> training.Registry:
    return training.load_registry(REPO / "models" / "approved-datasets.json")


@pytest.fixture
def licences() -> training.Licences:
    return training.load_licences(REPO / "models" / "licences.json")


@pytest.fixture
def tc(
    tm: training.TrainingManifest, registry: training.Registry, licences: training.Licences
) -> training.TrainingCheck:
    return training.check(tm, registry, licences)


def test_model_card_lists_failing_slices(
    tm: training.TrainingManifest,
    tc: training.TrainingCheck,
    licences: training.Licences,
    evaluation: dict[str, Any],
) -> None:
    failed = evaluation["verdicts"]["phase1"]["failed"]
    slices = {f.split(".")[1] for f in failed if f.startswith("slices.")}
    assert "lab_typed_poor" in slices
    card = documents.model_card(tc, evaluation, licences)
    for heading in (
        "## Intended use",
        "## Out-of-scope uses",
        "## Known failure slices",
        "## Licence",
        "## Base model",
    ):
        assert heading in card
    section = card.split("## Known failure slices", 1)[1].split("\n## ", 1)[0]
    for name in slices:
        assert f"`slices.{name}`" in section
    for entry in failed:
        metric = entry.split(".", 2)[2] if entry.startswith("slices.") else entry.split(".", 1)[1]
        assert f"`{metric}`" in section, entry
    assert "`field_precision.other`" in section
    assert f"`{fixtures.LICENCE}`" in card
    assert f"`{fixtures.BASE_REPO}`" in card
    assert f"`{tm.base.revision}`" in card
    assert f"`{fixtures.TRAIN_DATASET}`" in card
    assert f"`{evaluation['testset']['sha256']}`" in card
    assert MARKER not in card


def test_model_card_with_no_failures(
    tc: training.TrainingCheck, licences: training.Licences, result: runner.RunResult
) -> None:
    rep = report.build_report(result, [report.packaged_thresholds("rehearsal")])
    assert rep["verdicts"]["rehearsal"]["failed"] == []
    card = documents.model_card(tc, rep, licences)
    section = card.split("## Known failure slices", 1)[1].split("\n## ", 1)[0]
    assert "None" in section
    assert "`slices." not in section


def test_data_statement(tm: training.TrainingManifest, registry: training.Registry) -> None:
    text = documents.data_statement(tm, registry)
    entry = registry.datasets[fixtures.TRAIN_DATASET]
    assert f"`{entry.id}`" in text
    assert "`synthetic`" in text
    assert f"#{entry.approved_by_pr}" in text
    assert f"`{entry.generator}`" in text
    assert documents.data_statement(tm, registry) == text  # deterministic


def test_data_statement_unknown_dataset(tmp_path: Path, registry: training.Registry) -> None:
    tm = training.load_run(fixtures.make_run(tmp_path / "run", "unknown_dataset", 3))
    with pytest.raises(Refused) as exc:
        documents.data_statement(tm, registry)
    assert exc.value.reason == "dataset_unknown"


def _string_paths(obj: Any, path: tuple[str, ...] = ()) -> list[tuple[str, ...]]:
    if isinstance(obj, str):
        return [path]
    if isinstance(obj, dict):
        return [p for k, v in obj.items() for p in _string_paths(v, (*path, k))]
    if isinstance(obj, list):
        return [p for i, v in enumerate(obj) for p in _string_paths(v, (*path, str(i)))]
    return []


def _set(obj: Any, path: tuple[str, ...], value: Any) -> None:
    for key in path[:-1]:
        obj = obj[int(key)] if isinstance(obj, list) else obj[key]
    last = path[-1]
    if isinstance(obj, list):
        obj[int(last)] = value
    else:
        obj[last] = value


def test_model_card_takes_a_training_check(
    tm: training.TrainingManifest,
    tc: training.TrainingCheck,
    licences: training.Licences,
    evaluation: dict[str, Any],
) -> None:
    # Task 14 M3: the card is made from a TrainingCheck, never a raw manifest, and its
    # licence must be in models/licences.json.
    with pytest.raises(Refused):
        documents.model_card(tm, evaluation, licences)
    other = dataclasses.replace(tc, licence=fixtures.BAD_LICENCE)
    with pytest.raises(Refused) as exc:
        documents.model_card(other, evaluation, licences)
    assert exc.value.reason == "licence_not_allowed"
    promoted = dataclasses.replace(tc, licence=None)
    with pytest.raises(Refused) as exc:
        documents.model_card(promoted, evaluation, licences)  # a promoted base, no licence
    assert exc.value.reason == "manifest_invalid"
    card = documents.model_card(promoted, evaluation, licences, base_licence="MIT")
    assert "`MIT`" in card


def test_documents_have_no_free_text(
    tmp_path: Path,
    evaluation: dict[str, Any],
    registry: training.Registry,
    licences: training.Licences,
) -> None:
    run = fixtures.make_run(tmp_path / "run", "good", 3)
    good: dict[str, Any] = canonical.loads_strict(
        (run / "training_manifest.json").read_bytes(), 1 << 20
    )
    free_text = f"{MARKER} see the notebook, cell 12"
    plants: list[tuple[str, dict[str, Any]]] = []
    for path in _string_paths(good):
        obj = copy.deepcopy(good)
        _set(obj, path, free_text)
        plants.append((".".join(path), obj))
    for key, where in (("notes", ()), ("comment", ("base_model",)), ("desc", ("framework",))):
        obj = copy.deepcopy(good)
        target = obj
        for k in where:
            target = target[k]
        target[key] = free_text
        plants.append((".".join((*where, key)), obj))
    obj = copy.deepcopy(good)
    obj["notebook_metrics"]["summary"] = free_text
    plants.append(("notebook_metrics.summary", obj))
    assert len(plants) >= 12

    for n, (label, obj) in enumerate(plants):
        folder = tmp_path / f"p{n}"
        folder.mkdir()
        (folder / "training_manifest.json").write_bytes(json.dumps(obj).encode("utf-8"))
        with pytest.raises(Refused) as exc:
            tm = training.load_run(folder)
            documents.model_card(training.check(tm, registry, licences), evaluation, licences)
        assert exc.value.reason == "manifest_invalid", label

    tm = training.load_run(run)
    tc = training.check(tm, registry, licences)
    # a dict is not a checked manifest
    with pytest.raises(Refused):
        documents.model_card(good, evaluation, licences)
    with pytest.raises(Refused):
        documents.data_statement(good, registry)
    # the licence and the evaluation are held to the same rule
    with pytest.raises(Refused):
        documents.model_card(dataclasses.replace(tc, licence=free_text), evaluation, licences)
    with pytest.raises(Refused):
        documents.model_card(
            dataclasses.replace(tc, licence=None), evaluation, licences, base_licence=free_text
        )
    bad = copy.deepcopy(evaluation)
    bad["extractor"]["name"] = free_text
    with pytest.raises(Refused):
        documents.model_card(tc, bad, licences)
    bad = copy.deepcopy(evaluation)
    bad["verdicts"]["phase1"]["failed"].append(f"slices.{MARKER} x")
    with pytest.raises(Refused):
        documents.model_card(tc, bad, licences)

    # what is generated carries only ids, enums and numbers
    for text in (
        documents.model_card(tc, evaluation, licences),
        documents.data_statement(tm, registry),
    ):
        assert MARKER not in text
        for fixed in ("<", ">", "](", "<!--"):
            assert fixed not in text


@pytest.mark.parametrize("value", ["a|b", "a<b", "a>b", "a`b", "a\nb", "a\rb"])
def test_code_spans_refuse_markup(value: str) -> None:
    with pytest.raises(Refused):
        documents._code(value)

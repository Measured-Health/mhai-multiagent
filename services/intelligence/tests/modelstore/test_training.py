"""The training manifest and the dataset and licence registries (B16, B17, P23)."""

import copy
import json
from collections.abc import Callable
from pathlib import Path
from typing import Any

import pytest

from intelligence.modelstore import canonical, fixtures, training
from intelligence.modelstore.reasons import Refused
from tests.modelstore.fakes import gguf as g

REPO = Path(__file__).resolve().parents[4]
REGISTRY = REPO / "models" / "approved-datasets.json"
LICENCES = REPO / "models" / "licences.json"
EXAMPLE = REPO / "docs" / "models" / "training_manifest.example.json"
PIN = "mhai-tiny-extractor@1.0.0#" + "ab" * 32


def _committed() -> tuple[training.Registry, training.Licences]:
    return training.load_registry(REGISTRY), training.load_licences(LICENCES)


def _test_registry(tmp_path: Path) -> training.Registry:
    """The committed registry plus a non-synthetic entry, which is never committed."""
    obj = json.loads(REGISTRY.read_text(encoding="utf-8"))
    obj["datasets"].append(
        {
            "id": fixtures.NON_SYNTHETIC_DATASET,
            "kind": "deidentified",
            "generator": None,
            "version": "1.0.0",
            "testset_disjoint": True,
            "approved_by_pr": 1,
        }
    )
    path = tmp_path / "approved-datasets.json"
    path.write_text(json.dumps(obj, indent=2), encoding="utf-8")
    return training.load_registry(path)


def _run(tmp_path: Path, variant: str) -> Path:
    return fixtures.make_run(tmp_path / variant, variant, 7)


def _tm_obj(tmp_path: Path) -> dict[str, Any]:
    raw = (_run(tmp_path, "good") / "training_manifest.json").read_bytes()
    obj: dict[str, Any] = canonical.loads_strict(raw, 1 << 20)
    return obj


def _write(tmp_path: Path, obj: Any, name: str = "m") -> Path:
    folder = tmp_path / name
    folder.mkdir()
    path = folder / "training_manifest.json"
    path.write_bytes(json.dumps(obj).encode("utf-8"))
    return path


def _reason(fn: Callable[[], object]) -> str:
    with pytest.raises(Refused) as exc:
        fn()
    return exc.value.reason


# ---------------------------------------------------------------- the committed files


def test_committed_registry_seeded_with_dev_and_train() -> None:
    registry, _ = _committed()
    assert set(registry.datasets) == {
        "clinical_document-dev@1.0.0",
        "clinical_document-train@1.0.0",
    }
    for entry in registry.datasets.values():
        assert entry.kind == "synthetic"
        assert entry.generator == training.SYNTHETIC_GENERATOR
        assert entry.version == "1.0.0"
        assert entry.testset_disjoint is True
        assert entry.approved_by_pr >= 1
    assert fixtures.TRAIN_DATASET in registry.datasets


def test_committed_licences_are_exactly_apache_and_mit() -> None:
    _, licences = _committed()
    assert licences.allowed == frozenset({"Apache-2.0", "MIT"})


def test_registries_are_under_codeowners() -> None:
    owners = (REPO / ".github" / "CODEOWNERS").read_text(encoding="utf-8")
    assert any(line.split()[:1] == ["/models/"] for line in owners.splitlines())


def test_example_manifest_passes(tmp_path: Path) -> None:
    registry, licences = _committed()
    path = tmp_path / "training_manifest.json"
    path.write_bytes(EXAMPLE.read_bytes())  # as the notebook writes it next to the weights
    tm = training.load(path)
    result = training.check(tm, registry, licences)
    assert result.licence in licences.allowed
    assert [d.id for d in result.datasets] == list(tm.datasets)
    # notebook metrics may be floats (the file is hashed as raw bytes, never canonical)
    assert any(isinstance(v, float) for _, v in tm.notebook_metrics)


# ---------------------------------------------------------------- the checks


@pytest.mark.parametrize(
    "variant",
    ["good", "unknown_dataset", "non_synthetic_dataset", "bad_licence", "unpinned_base"],
)
def test_fixture_variants(tmp_path: Path, variant: str) -> None:
    registry = _test_registry(tmp_path)
    _, licences = _committed()
    tm = training.load_run(_run(tmp_path, variant))
    expected = fixtures.EXPECTED_REASONS[variant]
    if expected is None:
        result = training.check(tm, registry, licences)
        assert result.manifest is tm
        assert result.licence == fixtures.LICENCE
    else:
        assert _reason(lambda: training.check(tm, registry, licences)) == expected


def test_missing_training_manifest_refused(tmp_path: Path) -> None:
    registry, licences = _committed()
    run = _run(tmp_path, "no_training_manifest")
    assert _reason(lambda: training.load_run(run)) == "training_manifest_missing"
    assert _reason(lambda: training.load(run / "nope" / "training_manifest.json")) == (
        "training_manifest_missing"
    )
    assert _reason(lambda: training.check(None, registry, licences)) == (
        "training_manifest_missing"
    )


def test_unknown_dataset_refused(tmp_path: Path) -> None:
    registry, licences = _committed()
    obj = _tm_obj(tmp_path)
    obj["datasets"] = [fixtures.TRAIN_DATASET, fixtures.UNKNOWN_DATASET]
    tm = training.load(_write(tmp_path, obj))
    assert _reason(lambda: training.check(tm, registry, licences)) == "dataset_unknown"


def test_non_synthetic_dataset_refused(tmp_path: Path) -> None:
    registry = _test_registry(tmp_path)
    _, licences = _committed()
    assert registry.datasets[fixtures.NON_SYNTHETIC_DATASET].kind == "deidentified"
    obj = _tm_obj(tmp_path)
    obj["datasets"] = [fixtures.TRAIN_DATASET, fixtures.NON_SYNTHETIC_DATASET]
    tm = training.load(_write(tmp_path, obj))
    assert _reason(lambda: training.check(tm, registry, licences)) == "dataset_not_synthetic"
    # the committed registry does not know it at all
    committed, _ = _committed()
    assert _reason(lambda: training.check(tm, committed, licences)) == "dataset_unknown"


@pytest.mark.parametrize(
    "licence", ["LicenseRef-fixture-proprietary", "GPL-3.0-only", "apache-2.0", "Llama-3.1"]
)
def test_licence_not_allowed(tmp_path: Path, licence: str) -> None:
    registry, licences = _committed()
    obj = _tm_obj(tmp_path)
    obj["base_model"]["licence"] = licence
    tm = training.load(_write(tmp_path, obj))
    assert _reason(lambda: training.check(tm, registry, licences)) == "licence_not_allowed"


@pytest.mark.parametrize(
    "revision",
    ["main", "v1.0", "refs/pr/1", "a" * 39, "a" * 41, "A" * 40, "g" * 40, "abc123"],
)
def test_unpinned_upstream_revision_refused(tmp_path: Path, revision: str) -> None:
    registry, licences = _committed()
    obj = _tm_obj(tmp_path)
    obj["base_model"]["revision"] = revision
    tm = training.load(_write(tmp_path, obj))
    assert _reason(lambda: training.check(tm, registry, licences)) == "revision_unpinned"


def test_promoted_base_refused_until_verified(tmp_path: Path) -> None:
    registry, licences = _committed()
    obj = _tm_obj(tmp_path)
    obj["base_model"] = {"promoted": PIN}
    tm = training.load(_write(tmp_path, obj))
    # the default verifier refuses: nothing passes unverified
    assert _reason(lambda: training.check(tm, registry, licences)) == "base_unverified"

    seen: list[str] = []
    result = training.check(tm, registry, licences, verify_pin=seen.append)
    assert seen == [PIN]
    assert result.licence is None  # the base's own manifest carries its licence

    def refuse(_: str) -> None:
        raise Refused("signature_invalid")

    def broken(_: str) -> None:
        raise RuntimeError("store down")

    assert _reason(lambda: training.check(tm, registry, licences, verify_pin=refuse)) == (
        "signature_invalid"
    )
    assert _reason(lambda: training.check(tm, registry, licences, verify_pin=broken)) == (
        "base_unverified"
    )


@pytest.mark.parametrize(
    "pin",
    [
        "mhai-tiny-extractor@1.0.0",
        "mhai-tiny-extractor@1.0#" + "ab" * 32,
        "Mhai@1.0.0#" + "ab" * 32,
        "mhai-tiny-extractor@1.0.0#" + "ab" * 6,
        "mhai-tiny-extractor@1.0.0#" + "AB" * 32,
        "../x@1.0.0#" + "ab" * 32,
    ],
)
def test_bad_promoted_pin_refused(tmp_path: Path, pin: str) -> None:
    obj = _tm_obj(tmp_path)
    obj["base_model"] = {"promoted": pin}
    assert _reason(lambda: training.load(_write(tmp_path, obj))) == "manifest_invalid"


def _mutations() -> list[tuple[str, Callable[[dict[str, Any]], None]]]:
    def setter(path: list[str], value: Any) -> Callable[[dict[str, Any]], None]:
        def f(obj: dict[str, Any]) -> None:
            target = obj
            for key in path[:-1]:
                target = target[key]
            target[path[-1]] = value

        return f

    def drop(key: str) -> Callable[[dict[str, Any]], None]:
        return lambda obj: obj.pop(key)

    return [
        ("extra top-level key", setter(["notes"], "x")),
        ("extra base key", setter(["base_model", "notes"], "x")),
        ("both base kinds", setter(["base_model", "promoted"], PIN)),
        ("missing name", drop("name")),
        ("missing schema", drop("schema")),
        ("missing created_at", drop("created_at")),
        ("wrong schema", setter(["schema"], "mhai.models.training/2")),
        ("bad name", setter(["name"], "Bad Name")),
        ("bad role", setter(["role"], "Extract!")),
        ("bad login", setter(["trained_by"], "-bot")),
        ("no datasets", setter(["datasets"], [])),
        ("duplicate datasets", setter(["datasets"], [fixtures.TRAIN_DATASET] * 2)),
        ("dataset not an id", setter(["datasets"], ["clinical_document-train"])),
        ("datasets not a list", setter(["datasets"], fixtures.TRAIN_DATASET)),
        ("string metric", setter(["notebook_metrics", "loss"], "0.5")),
        ("bool metric", setter(["notebook_metrics", "ok"], True)),
        ("nested metric", setter(["notebook_metrics", "loss"], {"x": 1})),
        ("bad metric key", setter(["notebook_metrics", "Loss Value"], 1)),
        ("framework not a token", setter(["framework", "torch"], "2.5 (cuda)")),
        ("framework number", setter(["framework", "torch"], 2)),
        ("bad created_at", setter(["created_at"], "2026-11-03")),
        ("impossible date", setter(["created_at"], "2026-02-30T00:00:00Z")),
        ("bad hub repo", setter(["base_model", "upstream"], "a/b/c")),
        ("hub repo traversal", setter(["base_model", "upstream"], "../etc")),
        ("bad licence id", setter(["base_model", "licence"], "Apache 2.0")),
        ("not an object", lambda obj: obj.clear()),
    ]


@pytest.mark.parametrize("label,mutate", _mutations(), ids=[m[0] for m in _mutations()])
def test_unknown_key_refused(
    tmp_path: Path, label: str, mutate: Callable[[dict[str, Any]], None]
) -> None:
    obj = _tm_obj(tmp_path)
    mutate(obj)
    assert _reason(lambda: training.load(_write(tmp_path, obj))) == "manifest_invalid", label


def test_manifest_file_checks_reused(tmp_path: Path) -> None:
    good = _tm_obj(tmp_path)
    # duplicate keys, NaN, a BOM: refused by the strict parser
    raw = json.dumps(good)
    dup = raw[:-1] + ', "name": "other-model"}'
    for i, text in enumerate([dup, raw.replace('"steps": 0', '"steps": NaN')]):
        path = _write(tmp_path, {}, f"p{i}")
        path.write_text(text, encoding="utf-8")
        with pytest.raises(Refused) as exc:
            training.load(path)
        assert exc.value.reason == "json_invalid"
    # a symlink is never followed
    folder = tmp_path / "link"
    folder.mkdir()
    target = _write(tmp_path, good, "target")
    (folder / "training_manifest.json").symlink_to(target)
    link = folder / "training_manifest.json"
    assert _reason(lambda: training.load(link)) == "file_not_allowed"
    # the file must be named training_manifest.json
    other = tmp_path / "manifest.json"
    other.write_text(raw, encoding="utf-8")
    assert _reason(lambda: training.load(other)) == "file_not_allowed"


def test_dataclasses_refuse_free_text() -> None:
    with pytest.raises(Refused):
        training.UpstreamBase("mhai-fixtures/base", "a" * 40, "see the notebook for terms")
    with pytest.raises(Refused):
        training.PromotedBase("not a pin")


# ---------------------------------------------------------------- the registries


def _registry_obj() -> dict[str, Any]:
    obj: dict[str, Any] = json.loads(REGISTRY.read_text(encoding="utf-8"))
    return obj


def _registry_mutations() -> list[tuple[str, Callable[[dict[str, Any]], None]]]:
    def entry(key: str, value: Any) -> Callable[[dict[str, Any]], None]:
        def f(obj: dict[str, Any]) -> None:
            obj["datasets"][0][key] = value

        return f

    def dup(obj: dict[str, Any]) -> None:
        obj["datasets"].append(copy.deepcopy(obj["datasets"][0]))

    return [
        ("unknown kind", entry("kind", "public")),
        ("not disjoint", entry("testset_disjoint", False)),
        ("version differs from id", entry("version", "1.0.1")),
        ("no approval", entry("approved_by_pr", 0)),
        ("bool approval", entry("approved_by_pr", True)),
        ("extra key", entry("notes", "x")),
        ("synthetic without generator", entry("generator", None)),
        ("bad id", entry("id", "Clinical Document")),
        ("duplicate id", dup),
        ("wrong schema", lambda obj: obj.__setitem__("schema", "x")),
        ("empty", lambda obj: obj.__setitem__("datasets", [])),
        ("extra top key", lambda obj: obj.__setitem__("x", 1)),
    ]


@pytest.mark.parametrize(
    "label,mutate", _registry_mutations(), ids=[m[0] for m in _registry_mutations()]
)
def test_bad_registry_refused(
    tmp_path: Path, label: str, mutate: Callable[[dict[str, Any]], None]
) -> None:
    obj = _registry_obj()
    mutate(obj)
    path = tmp_path / "approved-datasets.json"
    path.write_text(json.dumps(obj), encoding="utf-8")
    assert _reason(lambda: training.load_registry(path)) == "policy_invalid", label


@pytest.mark.parametrize(
    "body",
    [
        {"schema": "mhai.models.licences/1", "licences": []},
        {"schema": "mhai.models.licences/1", "licences": ["MIT", "MIT"]},
        {"schema": "mhai.models.licences/1", "licences": ["Apache 2.0"]},
        {"schema": "mhai.models.licences/1", "licences": "MIT"},
        {"schema": "x", "licences": ["MIT"]},
        {"schema": "mhai.models.licences/1", "licences": ["MIT"], "x": 1},
    ],
)
def test_bad_licences_refused(tmp_path: Path, body: dict[str, Any]) -> None:
    path = tmp_path / "licences.json"
    path.write_text(json.dumps(body), encoding="utf-8")
    assert _reason(lambda: training.load_licences(path)) == "policy_invalid"


def test_missing_registry_refused(tmp_path: Path) -> None:
    assert _reason(lambda: training.load_registry(tmp_path / "nope.json")) == "policy_invalid"
    assert _reason(lambda: training.load_licences(tmp_path / "nope.json")) == "policy_invalid"


# ---------------------------------------------------------------- review fixes


@pytest.mark.parametrize(
    "key",
    [
        "patient-jane-doe-mrn-4417",
        "patient_jane_doe_mrn_4417",
        "mrn_441",
        "dob_19800101",
        "see:cell-12:ignore-the-gate",
        "eval.f1",
        "Loss",
        "_loss",
        "loss_",
        "loss__x",
        "a" * 41,
    ],
)
def test_identifying_metric_names_refused(tmp_path: Path, key: str) -> None:
    obj = _tm_obj(tmp_path)
    obj["notebook_metrics"] = {key: 1}
    assert _reason(lambda: training.load(_write(tmp_path, obj))) == "manifest_invalid"


@pytest.mark.parametrize("key", ["eval_field_f1", "epoch_12", "train_loss", "a" * 40])
def test_metric_names_accepted(tmp_path: Path, key: str) -> None:
    obj = _tm_obj(tmp_path)
    obj["notebook_metrics"] = {key: 0.5}
    assert training.load(_write(tmp_path, obj)).notebook_metrics == ((key, 0.5),)


@pytest.mark.parametrize(
    "name,version",
    [
        ("jane-doe", "1.0"),
        ("patient_4417", "1.0"),
        ("Torch", "2.5.1"),
        ("torch", "2.5 jane"),
        ("torch", "jane"),
        ("torch", "1.0.0-patient-doe"),
        ("torch", "2.5.1+Jane"),
        ("torch", "12345.0"),
    ],
)
def test_framework_names_closed(tmp_path: Path, name: str, version: str) -> None:
    obj = _tm_obj(tmp_path)
    obj["framework"] = {name: version}
    assert _reason(lambda: training.load(_write(tmp_path, obj))) == "manifest_invalid"


@pytest.mark.parametrize("version", ["2", "2.5.1", "4.57.0.dev0", "2.5.1+cu121", "1.0rc1"])
def test_framework_versions_accepted(tmp_path: Path, version: str) -> None:
    obj = _tm_obj(tmp_path)
    obj["framework"] = {"torch": version, "transformers": "4.57.1"}
    assert dict(training.load(_write(tmp_path, obj)).framework)["torch"] == version


@pytest.mark.parametrize(
    "licence,reason",
    [
        ("mit", "licence_not_allowed"),  # SPDX-shaped, but the allow-list is exact
        ("MIT ", "manifest_invalid"),  # not SPDX-shaped: refused at parse
        ("MIT OR GPL-3.0", "manifest_invalid"),
        ("Apache 2.0", "manifest_invalid"),
    ],
)
def test_licence_shaped_mistakes(tmp_path: Path, licence: str, reason: str) -> None:
    registry, licences = _committed()
    obj = _tm_obj(tmp_path)
    obj["base_model"]["licence"] = licence

    def run() -> object:
        return training.check(training.load(_write(tmp_path, obj)), registry, licences)

    assert _reason(run) == reason


def test_registry_values_checked_when_built() -> None:
    good = training.Dataset("x@1.0.0", "synthetic", training.SYNTHETIC_GENERATOR, "1.0.0", True, 1)
    for args in (
        ("x@1.0.0", "synthetic", "a | <b>", "1.0.0", True, 1),
        ("x | y@1.0.0", "synthetic", training.SYNTHETIC_GENERATOR, "1.0.0", True, 1),
        ("x@1.0.0", "public", None, "1.0.0", True, 1),
        ("x@1.0.0", "synthetic", training.SYNTHETIC_GENERATOR, "2.0.0", True, 1),
        ("x@1.0.0", "synthetic", training.SYNTHETIC_GENERATOR, "1.0.0", False, 1),
        ("x@1.0.0", "synthetic", training.SYNTHETIC_GENERATOR, "1.0.0", True, 0),
        ("x@1.0.0", "deidentified", "a.b", "1.0.0", True, 1),
    ):
        with pytest.raises(Refused) as exc:
            training.Dataset(*args)  # type: ignore[arg-type]
        assert exc.value.reason == "policy_invalid", args
    assert training.Registry({"x@1.0.0": good}).datasets["x@1.0.0"] is good
    for bad in ({"y@1.0.0": good}, {"x@1.0.0": "not a dataset"}, [good]):
        with pytest.raises(Refused) as exc:
            training.Registry(bad)  # type: ignore[arg-type]
        assert exc.value.reason == "policy_invalid"


def test_report_metrics_are_fixed_decimals(tmp_path: Path) -> None:
    # Task 19 carry: floats become six-place decimal strings for the evaluation report,
    # which itself stays integers and strings only. The file may end with a newline.
    run = fixtures.make_run(tmp_path / "run", "good", 3)
    path = run / "training_manifest.json"
    obj = json.loads(path.read_bytes())
    obj["notebook_metrics"] = {"val_f1": 0.8123456789, "steps": 900, "loss": 1e-7, "big": 12.5}
    path.write_text(json.dumps(obj) + "\n", encoding="utf-8")
    tm = training.load(path)
    assert training.report_metrics(tm) == {
        "big": "12.500000",
        "loss": "0.000000",
        "steps": 900,
        "val_f1": "0.812346",
    }


@pytest.mark.parametrize(
    ("value", "shown"),
    [
        (-0.03, "-0.030000"),
        (-5, "-5.000000"),
        (-1e-9, "0.000000"),  # rounds to zero: never "-0.000000"
        (-0.0, "0.000000"),
        (-0.0000004, "0.000000"),
        (-0.0000005, "0.000000"),  # half-even: 0.0000005 quantizes to 0.000000
        (-0.0000015, "-0.000002"),
        (-(10**18), "-1000000000000000000.000000"),
    ],
)
def test_report_metrics_signed_decimals(tmp_path: Path, value: float, shown: str) -> None:
    # Ruling D26: negative metrics are signed fixed decimals; the report reads them back.
    run = fixtures.make_run(tmp_path / "run", "good", 3)
    path = run / "training_manifest.json"
    obj = json.loads(path.read_bytes())
    obj["notebook_metrics"] = {"delta": value}
    raw = canonical.dumps(obj) if isinstance(value, int) else json.dumps(obj).encode()
    path.write_bytes(raw)
    tm = training.load(path)
    assert training.report_metrics(tm) == {"delta": shown}


def test_negative_zero_is_never_shown(tmp_path: Path) -> None:
    run = fixtures.make_run(tmp_path / "run", "good", 3)
    path = run / "training_manifest.json"
    obj = json.loads(path.read_bytes())
    obj["notebook_metrics"] = {"a": -1e-12, "b": -0.0, "c": -0.00000049}
    path.write_bytes(json.dumps(obj).encode())
    shown = training.report_metrics(training.load(path))
    assert shown == {"a": "0.000000", "b": "0.000000", "c": "0.000000"}
    assert all(not str(v).startswith("-") for v in shown.values())


# ---------------------------------------------------------------- roles (own model PR 1)


def test_training_roles_are_extract_and_cca() -> None:
    assert training.ROLES == frozenset({"extract", "cca"})


def test_cca_role_accepted(tmp_path: Path) -> None:
    registry, licences = _committed()
    run = fixtures.make_run(tmp_path / "cca", "good", 7, role="cca")
    tm = training.load_run(run)
    assert training.check(tm, registry, licences).manifest.role == "cca"


@pytest.mark.parametrize("role", ["ocr", "nli", "answer"])
def test_unknown_role_refused(tmp_path: Path, role: str) -> None:
    registry, licences = _committed()
    tm = training.load_run(fixtures.make_run(tmp_path / role, "good", 7, role=role))
    assert _reason(lambda: training.check(tm, registry, licences)) == "role_unknown"


@pytest.mark.parametrize(
    ("version", "ok"),
    [("2.5.1", True), ("2.5.1+cu121", True), ("4.57.0.dev0", True), ("latest", False), ("", False)],
)
def test_package_version_pattern_is_public(version: str, ok: bool) -> None:
    assert training.is_package_version(version) is ok


# ---------------------------------------------------------------- a GGUF in the run (security review)


def _gguf_run(tmp_path: Path, role: str, template: object = None, where: str = "chat") -> Path:
    run = fixtures.make_run(tmp_path / f"{role}-{where}", "good", 7, role=role)
    (run / "model-q4_k_m.gguf").write_bytes(g.tiny())
    if where == "chat":
        (run / "chat_template.json").write_text(json.dumps({"chat_template": template}))
    elif where == "tokenizer":
        cfg = json.loads((run / "tokenizer_config.json").read_text())
        cfg["chat_template"] = template
        (run / "tokenizer_config.json").write_text(json.dumps(cfg))
    return run


def _gguf_code(run: Path) -> tuple[str, str | None]:
    tm = training.load_run(run)
    with pytest.raises(Refused) as exc:
        training.check_gguf_run(tm, run)
    return exc.value.reason, exc.value.detail_code


def test_gguf_only_for_cca(tmp_path: Path) -> None:
    assert training.GGUF_ROLES == frozenset({"cca"})
    run = _gguf_run(tmp_path, "extract", g.TINY_TEMPLATE.decode())
    assert _gguf_code(run) == ("gguf_not_allowed_for_role", None)


@pytest.mark.parametrize("where", ["chat", "tokenizer"])
def test_cca_gguf_template_matches_the_run(tmp_path: Path, where: str) -> None:
    run = _gguf_run(tmp_path, "cca", g.TINY_TEMPLATE.decode(), where)
    training.check_gguf_run(training.load_run(run), run)


@pytest.mark.parametrize(
    ("template", "where", "detail"),
    [
        ("{{ other }}", "chat", "differs"),
        ("{{ other }}", "tokenizer", "differs"),
        (None, "none", "run_missing"),
        ([{"name": "default", "template": "x"}], "tokenizer", "run_invalid"),
        (None, "chat", "run_invalid"),
    ],
)
def test_cca_gguf_template_mismatch_refused(
    tmp_path: Path, template: object, where: str, detail: str
) -> None:
    run = _gguf_run(tmp_path, "cca", template, where)
    assert _gguf_code(run) == ("gguf_template_mismatch", detail)


def test_cca_gguf_without_template_refused(tmp_path: Path) -> None:
    run = _gguf_run(tmp_path, "cca", g.TINY_TEMPLATE.decode())
    no_template = g.gguf([g.tiny_kvs()[0], g.tiny_kvs()[2]], g.TINY_INFOS, g.TINY_DATA)
    (run / "model-q4_k_m.gguf").write_bytes(no_template)
    assert _gguf_code(run) == ("gguf_template_mismatch", "gguf_missing")


def test_run_without_gguf_needs_no_template(tmp_path: Path) -> None:
    run = fixtures.make_run(tmp_path / "plain", "good", 7, role="cca")
    training.check_gguf_run(training.load_run(run), run)

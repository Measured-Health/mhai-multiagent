import copy
import json
from pathlib import Path
from typing import Any

import pytest

from intelligence.doceval import schema
from intelligence.doceval.schema import SchemaError

FIXTURES = Path(__file__).parent / "fixtures"


def load(name: str) -> dict[str, Any]:
    loaded = json.loads((FIXTURES / f"{name}.json").read_text(encoding="utf-8"))
    assert isinstance(loaded, dict)
    return loaded


def as_stored(doc: dict[str, Any]) -> dict[str, Any]:
    """Give a model-output document the pipeline-set keys the stored view requires."""
    out = copy.deepcopy(doc)
    out["document"]["page_count"] = 1
    for _, fld in schema.iter_fields(out):
        fld.update({"confidence": 0.9, "band": "high", "grounded": True, "flags": []})
    return out


def the_field(value: Any = "x") -> dict[str, Any]:
    return {"value": value, "raw_text": "x", "page": 1, "bbox": [0.1, 0.1, 0.2, 0.2]}


def test_valid_fixtures_pass() -> None:
    schema.validate_model_output(load("valid_lab_report"))
    schema.validate_model_output(load("valid_empty"))
    schema.validate_stored(as_stored(load("valid_lab_report")))
    schema.validate_stored(as_stored(load("valid_empty")))
    schema.validate_stored(load("valid_stored"))


def test_stored_view_refuses_a_model_output_document() -> None:
    with pytest.raises(SchemaError):
        schema.validate_stored(load("valid_lab_report"))
    doc = as_stored(load("valid_lab_report"))
    del doc["document"]["page_count"]
    with pytest.raises(SchemaError) as exc:
        schema.validate_stored(doc)
    assert str(exc.value) == "/document"
    for key in schema.PIPELINE_KEYS:
        doc = as_stored(load("valid_lab_report"))
        del doc["observations"][0]["unit_text"][key]
        with pytest.raises(SchemaError) as exc:
            schema.validate_stored(doc)
        assert str(exc.value) == "/observations/0/unit_text"


@pytest.mark.parametrize(
    "name",
    [
        "invalid_unknown_key",
        "invalid_bad_enum",
        "invalid_bad_date",
        "invalid_wrong_version",
        "invalid_missing_bbox",
    ],
)
def test_invalid_fixtures_refused(name: str) -> None:
    with pytest.raises(SchemaError):
        schema.validate_model_output(load(name))
    with pytest.raises(SchemaError):
        schema.validate_stored(load(name))


def test_caps_enforced() -> None:
    base = load("valid_empty")
    obs = copy.deepcopy(load("valid_lab_report")["observations"][0])

    ok = copy.deepcopy(base)
    ok["observations"] = [obs] * 300
    schema.validate_model_output(ok)
    over = copy.deepcopy(base)
    over["observations"] = [obs] * 301
    with pytest.raises(SchemaError) as exc:
        schema.validate_model_output(over)
    assert str(exc.value) == "/observations"

    ids = copy.deepcopy(base)
    ids["patient"]["other_ids"] = [{"kind": the_field("other"), "value": the_field()}] * 6
    with pytest.raises(SchemaError) as exc:
        schema.validate_model_output(ids)
    assert str(exc.value) == "/patient/other_ids"

    long_note = copy.deepcopy(base)
    long_note["notes"] = [{"heading": the_field(), "text": the_field("a" * 20_001)}]
    with pytest.raises(SchemaError) as exc:
        schema.validate_model_output(long_note)
    assert str(exc.value) == "/notes/0/text/value"
    long_note["notes"][0]["text"] = the_field("a" * 20_000)
    schema.validate_model_output(long_note)

    raw = copy.deepcopy(base)
    raw["document"]["language"]["raw_text"] = "a" * 2_001
    with pytest.raises(SchemaError) as exc:
        schema.validate_model_output(raw)
    assert str(exc.value) == "/document/language/raw_text"
    raw["document"]["language"]["raw_text"] = "a" * 2_000
    schema.validate_model_output(raw)

    for section, limit in (("medications", 100), ("conditions", 100), ("notes", 50)):
        doc = copy.deepcopy(base)
        doc[section] = [{}] * (limit + 1)
        with pytest.raises(SchemaError) as exc:
            schema.validate_model_output(doc)
        assert str(exc.value) == f"/{section}"


@pytest.mark.parametrize("bbox", [[-0.1, 0, 0.5, 0.5], [0, 0, 1.1, 0.5], [0.1, 0.2, 0.3]])
def test_bbox_bounds(bbox: list[float]) -> None:
    doc = load("valid_lab_report")
    doc["document"]["type"]["bbox"] = bbox
    with pytest.raises(SchemaError):
        schema.validate_model_output(doc)
    doc["document"]["type"]["bbox"] = [0, 0, 1, 1]
    schema.validate_model_output(doc)


def test_page_must_be_positive() -> None:
    doc = load("valid_lab_report")
    doc["document"]["type"]["page"] = 0
    with pytest.raises(SchemaError):
        schema.validate_model_output(doc)


def test_unknown_key_refused() -> None:
    for where in ("top", "section", "field", "item"):
        doc = load("valid_lab_report")
        if where == "top":
            doc["extra"] = 1
        elif where == "section":
            doc["document"]["extra"] = the_field()
        elif where == "field":
            doc["document"]["type"]["extra"] = 1
        else:
            doc["observations"][0]["extra"] = the_field()
        with pytest.raises(SchemaError):
            schema.validate_model_output(doc)
        with pytest.raises(SchemaError):
            schema.validate_stored(doc)


@pytest.mark.parametrize(
    ("key", "value"),
    [("confidence", 0.99), ("band", "high"), ("grounded", True), ("flags", [])],
)
def test_model_output_view_refuses_pipeline_keys(key: str, value: Any) -> None:
    doc = load("valid_lab_report")
    doc["observations"][0]["value_numeric"][key] = value
    with pytest.raises(SchemaError) as exc:
        schema.validate_model_output(doc)
    assert str(exc.value) == "/observations/0/value_numeric"
    schema.validate_stored(as_stored(doc))
    with pytest.raises(SchemaError):
        schema.validate_model_output(load("valid_stored"))


def test_page_count_is_set_by_the_pipeline() -> None:
    doc = load("valid_lab_report")
    doc["document"]["page_count"] = 2
    with pytest.raises(SchemaError):
        schema.validate_model_output(doc)
    schema.validate_stored(as_stored(doc))


def test_stored_view_checks_pipeline_values() -> None:
    doc = load("valid_stored")
    doc["document"]["type"]["confidence"] = 1.5
    with pytest.raises(SchemaError):
        schema.validate_stored(doc)
    doc["document"]["type"]["confidence"] = 0.5
    doc["document"]["type"]["flags"] = ["made_up"]
    with pytest.raises(SchemaError):
        schema.validate_stored(doc)


def test_error_carries_pointer_not_value() -> None:
    secret = "PATIENT-SECRET-VALUE-XYZ"
    doc = load("valid_lab_report")
    doc["document"]["type"]["value"] = secret
    doc["patient"]["extra_" + secret] = 1
    with pytest.raises(SchemaError) as exc:
        schema.validate_model_output(doc)
    assert secret not in str(exc.value)
    assert secret not in repr(exc.value)
    assert secret not in "".join(map(str, exc.value.args))

    doc2 = load("valid_lab_report")
    doc2["document"]["type"]["value"] = secret
    with pytest.raises(SchemaError) as exc2:
        schema.validate_model_output(doc2)
    assert str(exc2.value) == "/document/type/value"
    assert secret not in repr(exc2.value)
    assert exc2.value.__cause__ is None and exc2.value.__context__ is None


@pytest.mark.parametrize(
    "bad",
    ["2026-02-30", "2026-2-3", "20260203", "2026-W01-1", "", "2026-01-01\n", "today"],
)
def test_date_format_checked(bad: str) -> None:
    doc = load("valid_lab_report")
    doc["document"]["date"]["value"] = bad
    with pytest.raises(SchemaError):
        schema.validate_model_output(doc)


@pytest.mark.parametrize(
    "good",
    [
        "2026-03-02T08:15:00",
        "2026-03-02T08:15",
        "2026-03-02T08:15:00Z",
        "2026-03-02T08:15:00+05:30",
    ],
)
def test_datetime_accepted_and_bad_refused(good: str) -> None:
    doc = load("valid_lab_report")
    doc["encounter"]["start"]["value"] = good
    schema.validate_model_output(doc)
    for bad in ("2026-03-02", "2026-03-02T25:00:00", "2026-03-02T08:15:00+5", "x"):
        doc["encounter"]["start"]["value"] = bad
        with pytest.raises(SchemaError):
            schema.validate_model_output(doc)


def test_bool_is_not_a_number() -> None:
    doc = load("valid_lab_report")
    doc["observations"][0]["value_numeric"]["value"] = True
    with pytest.raises(SchemaError):
        schema.validate_model_output(doc)


def test_non_object_refused() -> None:
    for bad in (None, [], "x", 1):
        with pytest.raises(SchemaError) as exc:
            schema.validate_model_output(bad)
        assert str(exc.value) == ""


def test_field_paths_stable() -> None:
    paths = [p for p, _ in schema.iter_fields(load("valid_lab_report"))]
    assert paths[:4] == [
        "document.type",
        "document.date",
        "document.facility_name",
        "document.language",
    ]
    assert "patient.other_ids[0].value" in paths
    assert "observations[0].value_numeric" in paths
    assert "observations[1].unit_text" in paths
    assert "medications[0].name_text" in paths
    assert "conditions[0].local_code" in paths
    assert "notes[0].text" in paths
    assert paths == [p for p, _ in schema.iter_fields(load("valid_lab_report"))]
    assert len(paths) == len(set(paths))
    assert "document.page_count" not in paths
    field = dict(schema.iter_fields(load("valid_lab_report")))["observations[0].value_numeric"]
    assert field["value"] == 5.9


def test_every_leaf_has_a_field_type() -> None:
    patterns = schema.field_patterns()
    assert len(patterns) == len(set(patterns))
    assert set(patterns) == set(schema.FIELD_TYPES)
    assert set(schema.FIELD_TYPES.values()) <= {
        "numeric_lab_value",
        "unit",
        "date",
        "datetime",
        "identifier",
        "name",
        "code",
        "enum",
        "text",
    }
    for name in ("valid_lab_report", "valid_empty", "valid_stored"):
        for path, _ in schema.iter_fields(load(name)):
            assert schema.field_type(path) in schema.FIELD_TYPES.values()
    assert schema.field_type("observations[12].unit_text") == "unit"
    assert schema.field_type("observations[12].value_numeric") == "numeric_lab_value"
    with pytest.raises(KeyError):
        schema.field_type("nope")


def test_schema_identity() -> None:
    assert schema.STORED["$id"] == "mhai.extraction.clinical_document/1.0.0"
    assert schema.STORED["properties"]["schema_version"] == {"const": "1.0.0"}
    assert "confidence" not in json.dumps(schema.MODEL_OUTPUT)
    assert "page_count" not in json.dumps(schema.MODEL_OUTPUT)


def test_page_has_an_upper_bound() -> None:
    doc = load("valid_lab_report")
    doc["document"]["type"]["page"] = 10_000
    schema.validate_model_output(doc)
    doc["document"]["type"]["page"] = 10_001
    with pytest.raises(SchemaError):
        schema.validate_model_output(doc)


def test_datetime_pattern_is_in_the_schema_and_both_views() -> None:
    import re

    for view in (schema.MODEL_OUTPUT, schema.STORED):
        value = view["$defs"]["F_datetime"]["properties"]["value"]
        assert value["pattern"] == "^" + schema._DATETIME.pattern + "$"
        assert value["format"] == "mhai-datetime"
        assert "date-time" not in json.dumps(value)
        assert re.search(value["pattern"], "2026-03-02T08:15:00")
        assert not re.search(value["pattern"], "2026-03-02")


def test_non_finite_numbers_refused_with_a_pointer() -> None:
    nan, inf = float("nan"), float("inf")
    doc = load("valid_lab_report")
    doc["document"]["type"]["bbox"] = [0.1, nan, 0.2, 0.2]
    with pytest.raises(SchemaError) as exc:
        schema.validate_model_output(doc)
    assert str(exc.value) == "/document/type/bbox/1"
    doc = load("valid_lab_report")
    doc["observations"][0]["value_numeric"]["value"] = inf
    with pytest.raises(SchemaError) as exc:
        schema.validate_model_output(doc)
    assert str(exc.value) == "/observations/0/value_numeric/value"
    doc = as_stored(load("valid_lab_report"))
    doc["observations"][1]["unit_text"]["confidence"] = nan
    with pytest.raises(SchemaError) as exc:
        schema.validate_stored(doc)
    assert str(exc.value) == "/observations/1/unit_text/confidence"
    doc["observations"][1]["unit_text"]["confidence"] = -inf
    with pytest.raises(SchemaError):
        schema.validate_stored(doc)


def test_wrapper_definitions_do_not_drift() -> None:
    for view in (schema.MODEL_OUTPUT, schema.STORED):
        defs = {k: v for k, v in view["$defs"].items() if k.startswith("F_")}
        assert len(defs) > 5
        first = next(iter(defs.values()))
        for name, d in defs.items():
            assert d["required"] == first["required"], name
            assert d["additionalProperties"] is False, name
            assert d["type"] == "object", name
            others = {k: v for k, v in d["properties"].items() if k != "value"}
            ref = {k: v for k, v in first["properties"].items() if k != "value"}
            assert others == ref, name

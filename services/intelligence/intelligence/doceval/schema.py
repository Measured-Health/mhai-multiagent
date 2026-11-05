"""The extraction schema ``mhai.extraction.clinical_document`` 1.0.0.

Two views of one schema file:

* ``MODEL_OUTPUT``: the schema with the pipeline-set keys removed (``confidence``,
  ``band``, ``grounded``, ``flags`` on every field and ``document.page_count``).
  This is what constrained decoding gets, and a model that sets any of them is
  refused: the model's own confidence is never used.
* ``STORED``: the full schema, for what the pipeline writes; it requires the
  pipeline-set keys on every field and ``document.page_count``. Ground-truth or
  evaluation documents without them use the model-output view.

Validation raises :class:`SchemaError` carrying a JSON pointer only, never a
value (documents hold patient data).
"""

import copy
import datetime
import json
import math
import re
from collections.abc import Iterator
from importlib import resources
from typing import Any

from jsonschema import Draft202012Validator, FormatChecker
from jsonschema.exceptions import best_match

SCHEMA_VERSION = "1.0.0"
PIPELINE_KEYS = ("confidence", "band", "grounded", "flags")
_PIPELINE_DOCUMENT_KEYS = ("page_count",)
_FIELD_DEF_PREFIX = "F_"
_FIELD_REF_PREFIX = "#/$defs/" + _FIELD_DEF_PREFIX


class SchemaError(Exception):
    """A document failed the schema. ``str()`` is the JSON pointer of the fault.

    The pointer is built from schema property names and array indexes only; the
    message and the offending value are never kept.
    """

    def __init__(self, pointer: str, keyword: str = "") -> None:
        super().__init__(pointer)
        self.pointer = pointer
        self.keyword = keyword


def _load() -> dict[str, Any]:
    ref = resources.files("intelligence").joinpath(
        "schemas", "extraction", "clinical_document", f"{SCHEMA_VERSION}.json"
    )
    loaded = json.loads(ref.read_text(encoding="utf-8"))
    if not isinstance(loaded, dict):
        raise RuntimeError("extraction schema is not an object")
    return loaded


def _model_output_view(full: dict[str, Any]) -> dict[str, Any]:
    view = copy.deepcopy(full)
    for name, sub in view["$defs"].items():
        if name.startswith(_FIELD_DEF_PREFIX):
            for key in PIPELINE_KEYS:
                sub["properties"].pop(key, None)
            sub["required"] = [k for k in sub["required"] if k not in PIPELINE_KEYS]
    document = view["properties"]["document"]
    for key in _PIPELINE_DOCUMENT_KEYS:
        document["properties"].pop(key, None)
    document["required"] = [k for k in document["required"] if k not in _PIPELINE_DOCUMENT_KEYS]
    view["$id"] = "mhai.extraction.clinical_document.model-output/" + SCHEMA_VERSION
    return view


_DATE = re.compile(r"[0-9]{4}-[0-9]{2}-[0-9]{2}", re.ASCII)
_DATETIME = re.compile(
    r"[0-9]{4}-[0-9]{2}-[0-9]{2}[Tt ][0-9]{2}:[0-9]{2}(:[0-9]{2}(\.[0-9]{1,9})?)?"
    r"([Zz]|[+-][0-9]{2}:[0-9]{2})?",
    re.ASCII,
)

_FORMATS = FormatChecker(formats=())


def _valid_date(text: object) -> bool:
    if not isinstance(text, str):
        return True  # format applies to strings only; the type keyword refuses the rest
    if _DATE.fullmatch(text) is None:
        return False
    try:
        datetime.date.fromisoformat(text)
    except ValueError:
        return False
    return True


def _valid_datetime(text: object) -> bool:
    if not isinstance(text, str):
        return True
    # RFC 3339 shape, but the offset is optional: printed times rarely carry one.
    if _DATETIME.fullmatch(text) is None:
        return False
    try:
        datetime.datetime.fromisoformat(text.replace("Z", "+00:00").replace("z", "+00:00"))
    except ValueError:
        return False
    return True


_FORMATS.checks("date")(_valid_date)
_FORMATS.checks("mhai-datetime")(_valid_datetime)

STORED: dict[str, Any] = _load()
MODEL_OUTPUT: dict[str, Any] = _model_output_view(STORED)

Draft202012Validator.check_schema(STORED)
Draft202012Validator.check_schema(MODEL_OUTPUT)
_STORED_VALIDATOR = Draft202012Validator(STORED, format_checker=_FORMATS)
_MODEL_VALIDATOR = Draft202012Validator(MODEL_OUTPUT, format_checker=_FORMATS)


def _pointer(parts: Any) -> str:
    return "".join("/" + str(p).replace("~", "~0").replace("/", "~1") for p in parts)


def _refuse_non_finite(doc: Any) -> None:
    """Refuse NaN and Infinity anywhere (Python's parser accepts them; JSON has none)."""
    stack: list[tuple[Any, tuple[Any, ...]]] = [(doc, ())]
    while stack:
        node, where = stack.pop()
        if isinstance(node, float):
            if not math.isfinite(node):
                raise SchemaError(_pointer(where), "finite")
        elif isinstance(node, dict):
            stack.extend((v, (*where, k)) for k, v in node.items())
        elif isinstance(node, list):
            stack.extend((v, (*where, i)) for i, v in enumerate(node))


def _validate(validator: Draft202012Validator, doc: Any) -> None:
    _refuse_non_finite(doc)
    error = best_match(validator.iter_errors(doc))
    if error is not None:
        raise SchemaError(_pointer(error.absolute_path), str(error.validator))


def validate_model_output(doc: Any) -> None:
    """Validate what a model produced; pipeline-set keys are refused."""
    _validate(_MODEL_VALIDATOR, doc)


def validate_stored(doc: Any) -> None:
    """Validate a stored extraction (the full schema)."""
    _validate(_STORED_VALIDATOR, doc)


def _field_name(sub: dict[str, Any]) -> str | None:
    ref = sub.get("$ref")
    if isinstance(ref, str) and ref.startswith(_FIELD_REF_PREFIX):
        return ref[len(_FIELD_REF_PREFIX) :]
    return None


def _schema_leaf_patterns(sub: dict[str, Any], prefix: str) -> Iterator[str]:
    if _field_name(sub) is not None:
        yield prefix
        return
    if sub.get("type") == "array":
        yield from _schema_leaf_patterns(sub["items"], prefix + "[]")
    elif "properties" in sub:
        for key, child in sub["properties"].items():
            if "$ref" in child or child.get("type") in ("object", "array"):
                yield from _schema_leaf_patterns(child, f"{prefix}.{key}" if prefix else key)


def field_patterns() -> list[str]:
    """Every Field path pattern in the schema, such as ``observations[].unit_text``."""
    return list(_schema_leaf_patterns(STORED, ""))


def _walk(sub: dict[str, Any], node: Any, path: str) -> Iterator[tuple[str, dict[str, Any]]]:
    if _field_name(sub) is not None:
        if isinstance(node, dict):
            yield path, node
        return
    if sub.get("type") == "array":
        if isinstance(node, list):
            for i, item in enumerate(node):
                yield from _walk(sub["items"], item, f"{path}[{i}]")
    elif "properties" in sub and isinstance(node, dict):
        for key, child in sub["properties"].items():
            if key in node:
                yield from _walk(child, node[key], f"{path}.{key}" if path else key)


def iter_fields(doc: dict[str, Any]) -> Iterator[tuple[str, dict[str, Any]]]:
    """Yield ``(path, field)`` for every Field in a document, in schema order.

    Paths are stable, such as ``observations[3].value_numeric``. The document
    should already have passed validation; anything unexpected is skipped.
    """
    yield from _walk(STORED, doc, "")


_INDEX = re.compile(r"\[[0-9]+\]")


def path_pattern(path: str) -> str:
    """``observations[3].unit_text`` becomes ``observations[].unit_text``."""
    return _INDEX.sub("[]", path)


FIELD_TYPES: dict[str, str] = {
    "document.type": "enum",
    "document.date": "date",
    "document.facility_name": "text",
    "document.language": "text",
    "patient.mrn": "identifier",
    "patient.other_ids[].kind": "enum",
    "patient.other_ids[].value": "identifier",
    "patient.family_name": "name",
    "patient.given_names": "name",
    "patient.date_of_birth": "date",
    "patient.sex": "enum",
    "encounter.kind": "enum",
    "encounter.start": "datetime",
    "encounter.end": "datetime",
    "encounter.location_text": "text",
    "encounter.clinician_text": "text",
    "observations[].category": "enum",
    "observations[].name_text": "text",
    "observations[].local_code": "code",
    "observations[].value_text": "text",
    "observations[].value_numeric": "numeric_lab_value",
    "observations[].comparator": "enum",
    "observations[].unit_text": "unit",
    "observations[].reference_text": "text",
    "observations[].reference_low": "numeric_lab_value",
    "observations[].reference_high": "numeric_lab_value",
    "observations[].abnormal_flag": "enum",
    "observations[].specimen_text": "text",
    "observations[].collected_at": "datetime",
    "observations[].resulted_at": "datetime",
    "medications[].name_text": "text",
    "medications[].strength_text": "text",
    "medications[].dose_text": "text",
    "medications[].route_text": "text",
    "medications[].frequency_text": "text",
    "medications[].start": "date",
    "medications[].stop": "date",
    "medications[].status": "enum",
    "conditions[].text": "text",
    "conditions[].local_code": "code",
    "conditions[].local_system": "enum",
    "conditions[].clinical_status": "enum",
    "conditions[].verification": "enum",
    "conditions[].onset": "date",
    "notes[].heading": "text",
    "notes[].text": "text",
}


def field_type(path: str) -> str:
    """The field type (for per-type metrics) of a concrete or pattern path."""
    return FIELD_TYPES[path_pattern(path)]

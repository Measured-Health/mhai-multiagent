"""The model manifest, ``mhai.models.manifest/1``: canonical, strict, hashed."""

import hashlib
import json
from collections.abc import Callable
from typing import Any

import pytest

from intelligence.doceval import report
from intelligence.modelstore import canonical, manifest, training
from intelligence.modelstore.reasons import Refused

SHA = "a" * 64
COMMIT = "0123456789abcdef0123456789abcdef01234567"
REF = "synthetic-org/repo/.github/workflows/model-promote.yml@refs/heads/main"


def _kwargs(**overrides: Any) -> dict[str, Any]:
    kw: dict[str, Any] = {
        "name": "mhai-tiny-extractor",
        "version": "1.0.0",
        "role": "extractor",
        "run": manifest.Run(id="run_1", source="local", source_ref="fixture"),
        "files": [
            manifest.FileRecord("model.safetensors", 1024, "b" * 64, "v-2"),
            manifest.FileRecord("config.json", 12, "c" * 64, "v-1"),
        ],
        "documents": {
            name: manifest.DocumentRecord(SHA, f"doc-{i}")
            for i, name in enumerate(manifest.DOCUMENTS)
        },
        "base_model": training.UpstreamBase("synthetic-org/tiny-base", COMMIT, "Apache-2.0"),
        "dependencies": [manifest.Dependency("mhai-tokenizer", "2.0.1", "d" * 64)],
        "evaluation": manifest.Evaluation(
            report_sha256="e" * 64,
            testset=f"clinical_document@1.0.0#{'f' * 64}",
            gates={"rehearsal": "pass", "phase1": "fail"},
            split="test",
        ),
        "runtime": manifest.Runtime("numpy", "2.1.0", None),
        "promotion": manifest.Promotion(
            workflow_run_id="123456789",
            workflow_ref=REF,
            commit=COMMIT,
            actor="synthetic-user",
            at="2026-11-05T10:00:00Z",
        ),
        "trained_by": "synthetic-trainer",
    }
    kw.update(overrides)
    return kw


def _obj(**overrides: Any) -> dict[str, Any]:
    obj: dict[str, Any] = json.loads(manifest.build(**_kwargs()))
    obj.update(overrides)
    return obj


def test_manifest_round_trip_and_hash() -> None:
    b = manifest.build(**_kwargs())
    assert canonical.is_canonical(b)
    m = manifest.parse(b)
    assert m.name == "mhai-tiny-extractor" and m.version == "1.0.0" and m.role == "extractor"
    assert [f.path for f in m.files] == ["config.json", "model.safetensors"]  # sorted by path
    assert set(m.documents) == set(manifest.DOCUMENTS)
    assert set(manifest.DOCUMENTS) == {
        "evaluation.json",
        "evaluation.md",
        "model_card.md",
        "data_statement.md",
        "training_manifest.json",
        "warmup.json",
    }
    assert dict(m.evaluation.gates) == {"phase1": "fail", "rehearsal": "pass"}
    assert isinstance(m.base_model, training.UpstreamBase)
    assert manifest.to_bytes(m) == b
    assert manifest.build(**_kwargs()) == b  # deterministic
    assert manifest.sha256(b) == hashlib.sha256(b).hexdigest()
    obj = json.loads(b)
    assert obj["schema"] == manifest.SCHEMA == "mhai.models.manifest/1"
    assert obj["runtime"]["image_digest"] is None
    # A promoted base, an image digest and other sources round-trip too.
    b2 = manifest.build(
        **_kwargs(
            base_model=training.PromotedBase(f"mhai-base@1.2.3#{SHA}"),
            runtime=manifest.Runtime("vllm", "0.11.0", f"sha256:{SHA}"),
            run=manifest.Run(id="r2", source="hf", source_ref=f"synthetic-org/tiny@{COMMIT}"),
            dependencies=[],
        )
    )
    assert manifest.parse(b2).base_model == training.PromotedBase(f"mhai-base@1.2.3#{SHA}")
    drive = manifest.Run(id="r3", source="drive", source_ref="1AbCdEfGhIjKlMnOpQrStUv_-")
    assert manifest.parse(manifest.build(**_kwargs(run=drive))).run == drive


def test_non_canonical_manifest_refused() -> None:
    b = manifest.build(**_kwargs())
    obj = json.loads(b)
    for variant in (
        json.dumps(obj, sort_keys=True, indent=1).encode(),
        json.dumps(dict(reversed(list(obj.items()))), separators=(",", ":")).encode(),
        b + b"\n",
        b.replace(b'"role":"extractor"', b'"role":"extr\\u0061ctor"'),
    ):
        assert variant != b
        with pytest.raises(Refused) as e:
            manifest.parse(variant)
        assert e.value.reason in {"not_canonical", "json_invalid"}
    with pytest.raises(Refused) as e:
        manifest.parse(json.dumps(obj, sort_keys=True, indent=1).encode())
    assert e.value.reason == "not_canonical"


def _refused(obj: Any) -> str:
    with pytest.raises(Refused) as e:
        manifest.parse(canonical.dumps(obj))
    return e.value.reason


def test_unknown_key_and_float_refused() -> None:
    assert _refused(_obj(extra="x")) == "manifest_invalid"
    missing = _obj()
    del missing["trained_by"]
    assert _refused(missing) == "manifest_invalid"
    # A float anywhere is refused by the strict parser, before any shape check.
    b = manifest.build(**_kwargs()).replace(b'"size":12', b'"size":12.0')
    with pytest.raises(Refused) as e:
        manifest.parse(b)
    assert e.value.reason == "json_invalid"
    # Unknown keys deeper down.
    o = _obj()
    o["run"]["note"] = "x"
    assert _refused(o) == "manifest_invalid"
    o = _obj()
    o["documents"]["notes.md"] = {"sha256": SHA, "store_version_id": "v"}
    assert _refused(o) == "manifest_invalid"
    o = _obj()
    del o["documents"]["warmup.json"]
    assert _refused(o) == "manifest_invalid"
    o = _obj()
    o["files"][0]["mode"] = 420
    assert _refused(o) == "manifest_invalid"
    o = _obj()
    o["evaluation"]["gates"]["phase9"] = "pass"
    assert _refused(o) == "manifest_invalid"
    o = _obj()
    o["evaluation"]["gates"]["phase1"] = "passed"
    assert _refused(o) == "manifest_invalid"
    assert _refused(_obj(schema="mhai.models.manifest/2")) == "manifest_invalid"


def test_manifest_fields_held_to_patterns() -> None:
    bad: list[Callable[[], dict[str, Any]]] = [
        lambda: {"name": "../x"},
        lambda: {"version": "1.0"},
        lambda: {"role": "Extractor"},
        lambda: {"run": manifest.Run(id="r", source="s3", source_ref="fixture")},
        lambda: {"run": manifest.Run(id="r", source="local", source_ref="/tmp/x")},
        lambda: {"run": manifest.Run(id="r", source="hf", source_ref="org/repo@main")},
        lambda: {"run": manifest.Run(id="r", source="drive", source_ref="../folder")},
        lambda: {"files": []},
        lambda: {"files": [manifest.FileRecord("model.safetensors", 1, SHA, "v")] * 2},
        lambda: {"files": [manifest.FileRecord("model.bin", 1, SHA, "v")]},
        lambda: {"files": [manifest.FileRecord("model.safetensors", -1, SHA, "v")]},
        lambda: {"files": [manifest.FileRecord("model.safetensors", True, SHA, "v")]},
        lambda: {"files": [manifest.FileRecord("model.safetensors", 1, "A" * 64, "v")]},
        lambda: {"files": [manifest.FileRecord("model.safetensors", 1, SHA, "")]},
        # Store version ids: only S3/MinIO characters, never a path or a query.
        lambda: {"files": [manifest.FileRecord("model.safetensors", 1, SHA, "a/b")]},
        lambda: {"files": [manifest.FileRecord("model.safetensors", 1, SHA, "a+b=")]},
        lambda: {
            "documents": {
                n: manifest.DocumentRecord(SHA, "x?versionId=1") for n in manifest.DOCUMENTS
            }
        },
        lambda: {"dependencies": [manifest.Dependency("x y", "1.0.0", SHA)]},
        lambda: {
            "evaluation": manifest.Evaluation(
                SHA, "clinical_document@1.0.0", {"phase1": "pass"}, "test"
            )
        },
        lambda: {
            "evaluation": manifest.Evaluation(SHA, f"clinical_document@1.0.0#{SHA}", {}, "test")
        },
        lambda: {"runtime": manifest.Runtime("numpy", "2.1.0", "latest")},
        lambda: {
            "promotion": manifest.Promotion(
                "12", "synthetic-org/repo/../x@refs/heads/main", COMMIT, "u", "2026-11-05T10:00:00Z"
            )
        },
        lambda: {"promotion": manifest.Promotion("12", REF, "abc", "u", "2026-11-05T10:00:00Z")},
        lambda: {"promotion": manifest.Promotion("12", REF, COMMIT, "-u", "2026-11-05T10:00:00Z")},
        lambda: {"promotion": manifest.Promotion("12", REF, COMMIT, "u", "2026-13-05T10:00:00Z")},
        lambda: {"promotion": manifest.Promotion("x", REF, COMMIT, "u", "2026-11-05T10:00:00Z")},
        lambda: {"trained_by": "two words"},
        lambda: {"base_model": training.UpstreamBase("synthetic-org/tiny", "main", "Apache-2.0")},
    ]
    for override in bad:
        with pytest.raises(Refused) as e:
            manifest.build(**_kwargs(**override()))
        assert e.value.reason == "manifest_invalid", override
    # A gate name that is not in report.GATES is refused; every real one is accepted.
    gates = {g: "pass" for g in report.GATES}
    ev = manifest.Evaluation(SHA, f"clinical_document@1.0.0#{SHA}", gates, "test")
    assert dict(manifest.parse(manifest.build(**_kwargs(evaluation=ev))).evaluation.gates) == gates


def test_parse_refuses_wrong_types_and_order() -> None:
    o = _obj()
    o["files"] = list(reversed(o["files"]))  # not sorted by path
    assert _refused(o) == "manifest_invalid"
    o = _obj()
    o["files"] = {}
    assert _refused(o) == "manifest_invalid"
    o = _obj()
    o["runtime"]["image_digest"] = 5
    assert _refused(o) == "manifest_invalid"
    assert _refused([]) == "manifest_invalid"
    with pytest.raises(Refused) as e:
        manifest.parse(b"x" * (manifest.MAX_BYTES + 1))
    assert e.value.reason == "too_large"


@pytest.mark.parametrize("split", ["dev", "test"])
def test_evaluation_split_recorded(split: str) -> None:
    ev = manifest.Evaluation(SHA, f"clinical_document@1.0.0#{SHA}", {"phase1": "pass"}, split)
    m = manifest.parse(manifest.build(**_kwargs(evaluation=ev)))
    assert m.evaluation.split == split


@pytest.mark.parametrize("split", ["locked", "", "Test", None])
def test_evaluation_split_closed(split: Any) -> None:
    with pytest.raises(Refused) as e:
        manifest.Evaluation(SHA, f"clinical_document@1.0.0#{SHA}", {"phase1": "pass"}, split)
    assert e.value.reason == "manifest_invalid"
    obj = _obj()
    obj["evaluation"]["split"] = split
    with pytest.raises(Refused):
        manifest.parse(canonical.dumps(obj))

"""The tiny safetensors extractor: numpy only, deterministic, weight-sensitive."""

import json
import subprocess
import sys
from pathlib import Path

import numpy as np
import pytest
from safetensors.numpy import load_file, save_file

from intelligence.doceval import extractors, schema, tiny
from intelligence.modelstore import fixtures
from tests.doceval import minisets

ROOT = Path(__file__).resolve().parents[2]


def _pages() -> list[bytes]:
    return [minisets.png(7), minisets.png(1234)]


def test_tiny_loads_without_torch(tmp_path: Path) -> None:
    run = fixtures.make_run(tmp_path / "run", "good", 3)
    page = tmp_path / "page.png"
    page.write_bytes(minisets.png(5))
    code = (
        "import sys\n"
        "from intelligence.doceval import tiny\n"
        f"x = tiny.TinyExtractor({str(run)!r})\n"
        f"r = x.extract([open({str(page)!r}, 'rb').read()], deadline_s=10)\n"
        "assert type(r).__name__ == 'Extracted', r\n"
        "assert 'torch' not in sys.modules, 'torch imported'\n"
        "assert 'transformers' not in sys.modules, 'transformers imported'\n"
    )
    done = subprocess.run(
        [sys.executable, "-c", code], cwd=ROOT, capture_output=True, text=True, timeout=120
    )
    assert done.returncode == 0, done.stderr[-2000:]


def test_tiny_deterministic(tmp_path: Path) -> None:
    a = fixtures.make_run(tmp_path / "a", "good", 11)
    b = fixtures.make_run(tmp_path / "b", "good", 11)
    for f in sorted(p.name for p in a.iterdir()):
        assert (a / f).read_bytes() == (b / f).read_bytes(), f
    ra = tiny.TinyExtractor(a).extract(_pages(), deadline_s=10)
    rb = tiny.TinyExtractor(b).extract(_pages(), deadline_s=10)
    assert isinstance(ra, extractors.Extracted) and isinstance(rb, extractors.Extracted)
    assert json.dumps(ra.document, sort_keys=True) == json.dumps(rb.document, sort_keys=True)
    schema.validate_stored(ra.document)
    assert ra.document["document"]["page_count"] == 2
    assert len(ra.page_seconds) == 2
    assert ra.document["document"]["type"]["value"] in tiny.DOCUMENT_TYPES
    for _, fld in schema.iter_fields(ra.document):
        assert fld["band"] == extractors.band(fld["confidence"])


def test_tiny_output_depends_on_pixels(tmp_path: Path) -> None:
    run = fixtures.make_run(tmp_path / "a", "good", 11)
    x = tiny.TinyExtractor(run)
    outs = {json.dumps(x.predict([minisets.png(s)]), sort_keys=True) for s in range(12)}
    assert len(outs) > 1


def test_tiny_weight_change_changes_output(tmp_path: Path) -> None:
    run = fixtures.make_run(tmp_path / "a", "good", 11)
    before = tiny.TinyExtractor(run).predict(_pages())
    tensors = load_file(str(run / "model.safetensors"))
    picked = tiny.DOCUMENT_TYPES.index(before["document"]["type"]["value"])
    bias = tensors["type.bias"].copy()
    bias[(picked + 1) % len(bias)] += np.float32(50.0)  # one weight
    tensors["type.bias"] = bias
    save_file(tensors, str(run / "model.safetensors"), metadata={"format": "np"})
    after = tiny.TinyExtractor(run).predict(_pages())
    assert after["document"]["type"] != before["document"]["type"]

    v2 = fixtures.make_run(tmp_path / "v2", "good_v2", 11)
    other = tiny.TinyExtractor(v2).predict(_pages())
    assert json.dumps(other, sort_keys=True) != json.dumps(before, sort_keys=True)


def test_tiny_refuses_bad_runs(tmp_path: Path) -> None:
    for variant in ("bad_header_past_end", "bad_dtype", "trailing_bytes"):
        run = fixtures.make_run(tmp_path / variant, variant, 1)
        with pytest.raises(tiny.TinyInvalid) as err:
            tiny.TinyExtractor(run)
        assert str(err.value) == "tiny_invalid"
    run = fixtures.make_run(tmp_path / "wrong-arch", "good", 1)
    (run / "config.json").write_bytes(b'{"architectures":["Other"],"model_type":"mhai-tiny"}')
    with pytest.raises(tiny.TinyInvalid):
        tiny.TinyExtractor(run)
    missing = fixtures.make_run(tmp_path / "missing", "good", 1)
    (missing / "model.safetensors").unlink()
    with pytest.raises(tiny.TinyInvalid):
        tiny.TinyExtractor(missing)


def test_tiny_unreadable_page_refused(tmp_path: Path) -> None:
    x = tiny.TinyExtractor(fixtures.make_run(tmp_path / "a", "good", 2))
    assert x.extract([b"not a png"], deadline_s=10) == extractors.Refused("unreadable")
    assert x.extract([], deadline_s=10) == extractors.Refused("unreadable")


def test_tiny_info_versions_from_caller(tmp_path: Path) -> None:
    run = fixtures.make_run(tmp_path / "a", "good", 2)
    x = tiny.TinyExtractor(run, model_versions={"extract": "mhai-tiny-extractor@0.1.0#abc"})
    info = x.info()
    assert info.name == "mhai-tiny-extractor"
    assert info.model_versions == {"extract": "mhai-tiny-extractor@0.1.0#abc"}
    assert info.engine == "numpy"


def test_tiny_schema_fault_is_not_a_refusal(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    x = tiny.TinyExtractor(fixtures.make_run(tmp_path / "a", "good", 2))

    def broken(doc: object) -> None:
        raise schema.SchemaError("/document")

    monkeypatch.setattr(tiny.schema, "validate_stored", broken)
    with pytest.raises(schema.SchemaError):
        x.extract([minisets.png(1)], deadline_s=10)

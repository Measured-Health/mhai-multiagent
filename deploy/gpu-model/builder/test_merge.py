"""The GGUF builder's pure steps (merge.py), with the standard library only: no torch, no
model, no network. Run: python3 -m unittest discover -s deploy/gpu-model/builder"""

import datetime
import json
import struct
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import merge  # noqa: E402


def _safetensors(path: Path, keys: list[str]) -> None:
    header = {k: {"dtype": "F32", "shape": [1], "data_offsets": [4 * i, 4 * i + 4]} for i, k in enumerate(keys)}
    header["__metadata__"] = {"format": "pt"}
    raw = json.dumps(header).encode()
    path.write_bytes(struct.pack("<Q", len(raw)) + raw + bytes(4 * len(keys)))


class MergeTest(unittest.TestCase):
    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self.tmp = Path(self._tmp.name)

    def tearDown(self) -> None:
        self._tmp.cleanup()

    def test_adapter_modules_from_the_header_only(self) -> None:
        keys = [
            "base_model.model.model.language_model.layers.0.self_attn.q_proj.lora_A.weight",
            "base_model.model.model.language_model.layers.0.self_attn.q_proj.lora_B.weight",
            "base_model.model.model.language_model.layers.1.mlp.down_proj.lora_A.weight",
        ]
        p = self.tmp / "adapter_model.safetensors"
        _safetensors(p, keys)
        self.assertEqual(merge.safetensors_keys(p), sorted(keys))
        self.assertEqual(
            merge.adapter_modules(keys),
            {
                "model.language_model.layers.0.self_attn.q_proj",
                "model.language_model.layers.1.mlp.down_proj",
            },
        )

    def test_a_tensor_that_is_not_lora_is_refused(self) -> None:
        for keys in (["base_model.model.lm_head.weight"], ["model.layers.0.q_proj.lora_A.weight"], []):
            with self.assertRaises(merge.BuildError):
                merge.adapter_modules(keys)

    def test_a_bad_header_is_refused(self) -> None:
        p = self.tmp / "a.safetensors"
        p.write_bytes(b"\x00" * 4)
        with self.assertRaises(merge.BuildError):
            merge.safetensors_keys(p)
        p.write_bytes(struct.pack("<Q", 0))
        with self.assertRaises(merge.BuildError):
            merge.safetensors_keys(p)
        p.write_bytes(struct.pack("<Q", 2) + b"[]")
        with self.assertRaises(merge.BuildError):
            merge.safetensors_keys(p)

    def test_missing_modules(self) -> None:
        self.assertEqual(merge.missing_modules({"a", "b"}, ["a", "c"]), ["b"])
        self.assertEqual(merge.missing_modules({"a"}, ["a"]), [])

    def test_the_jinja_template_moves_into_json(self) -> None:
        (self.tmp / "chat_template.jinja").write_text("{{ t }}", encoding="utf-8")
        merge.template_to_json(self.tmp)
        self.assertFalse((self.tmp / "chat_template.jinja").exists())
        self.assertEqual(
            json.loads((self.tmp / "chat_template.json").read_text()), {"chat_template": "{{ t }}"}
        )
        # An existing chat_template.json wins; the jinja copy is still removed.
        (self.tmp / "chat_template.jinja").write_text("other", encoding="utf-8")
        merge.template_to_json(self.tmp)
        self.assertEqual(json.loads((self.tmp / "chat_template.json").read_text())["chat_template"], "{{ t }}")
        merge.template_to_json(self.tmp)  # nothing to do

    def test_the_manifest(self) -> None:
        g = self.tmp / merge.GGUF_NAME
        g.write_bytes(b"GGUF synthetic")
        m = merge.manifest(
            name="cca-v2",
            version="2.0.0",
            profile="gemma4-e4b-cca-v1",
            repo="google/gemma-4-E4B-it",
            revision="4ecc2ba63bcac23ba4a767fc87025b1450f84c9c",
            base_class="AutoModelForCausalLM",
            adapter_files={"b": "2", "a": "1"},
            gguf=g,
            f16_bytes=99,
            built_by="deploy",
            now=datetime.datetime(2026, 11, 23, 10, 0, 0, tzinfo=datetime.UTC),
        )
        self.assertEqual(m["schema"], merge.SCHEMA)
        self.assertEqual(m["gguf"]["sha256"], merge.sha256_file(g))
        self.assertEqual(m["gguf"]["bytes"], 14)
        self.assertEqual(list(m["adapter"]["files"]), ["a", "b"])
        self.assertEqual(m["built_at"], "2026-11-23T10:00:00Z")
        self.assertIn("transformers", m["tools"]["packages"])
        json.dumps(m)

    def test_arguments_are_plain_names(self) -> None:
        base = ["build", "--repo", "google/gemma-4-E4B-it", "--revision", "a" * 40, "--adapter", "/a",
                "--work", "/w", "--out", "/o", "--name", "n1", "--version", "1.0.0", "--profile", "default"]
        for bad in (["--built-by", "../x"], ["--built-by", "a b"]):
            with self.assertRaises(merge.BuildError):
                merge.main(base + bad)
        with self.assertRaises(merge.BuildError):
            merge.main(["fetch", "--repo", "x/y", "--revision", "main"])


if __name__ == "__main__":
    unittest.main()

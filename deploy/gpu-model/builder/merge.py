"""The GGUF builder's steps (deploy/gpu-model/builder/Dockerfile; run by swap-model.sh).

    merge.py fetch --repo R --revision REV
    merge.py build --repo R --revision REV --adapter DIR --work DIR --out DIR
                   --name N --version V --profile P --built-by USER

``fetch`` (the only step with a network) downloads the base at exactly ``REV`` into
``HF_HOME`` (the host's cache, reused by every later build): safetensors, JSON, the
sentencepiece model and Jinja templates only, never a pickle or code file.

``build`` (no network: ``HF_HUB_OFFLINE=1``) loads the cached base in bfloat16 on the CPU
(``trust_remote_code`` never set, safetensors only), merges the adapter (already checked by
``mhai-models swap check-adapter``) with PEFT's ``merge_and_unload``, saves it as 4 GB
safetensors shards with the adapter's tokenizer (``chat_template.jinja`` moved into
``chat_template.json``, as the export step does), converts it with llama.cpp's
``convert_hf_to_gguf.py --outtype f16``, quantises with ``llama-quantize Q4_K_M`` into
``<out>/model-q4_k_m.gguf`` and writes ``<out>/manifest.json``. The merged weights and the
f16 GGUF are removed from ``<work>`` as soon as they are used.

The base class is the first of ``AutoModelForCausalLM`` and ``AutoModelForImageTextToText``
that holds every module the adapter names; an adapter that fits neither is refused.

Messages are fixed text: no file content or secret is printed.
"""

from __future__ import annotations

import argparse
import datetime
import gc
import hashlib
import json
import os
import re
import shutil
import struct
import subprocess
import sys
from collections.abc import Iterable, Sequence
from importlib import metadata
from pathlib import Path
from typing import Any

SCHEMA = "mhai.gpu_model.build/1"
GGUF_NAME = "model-q4_k_m.gguf"
QUANT = "Q4_K_M"
ALLOW_PATTERNS = ["*.json", "*.safetensors", "tokenizer.model", "*.jinja"]
PACKAGES = (
    "torch",
    "transformers",
    "peft",
    "accelerate",
    "safetensors",
    "huggingface-hub",
    "tokenizers",
    "sentencepiece",
    "numpy",
    "gguf",
)
BASE_CLASSES = ("AutoModelForCausalLM", "AutoModelForImageTextToText")
PEFT_PREFIX = "base_model.model."
MAX_HEADER = 100 * 1024 * 1024
_TOKEN = re.compile(r"[A-Za-z0-9][A-Za-z0-9._/@+-]{0,127}")
_LORA = re.compile(r"\.lora_[A-Za-z_]+(\.|$)")


class BuildError(Exception):
    """A refusal with a fixed message."""


def sha256_file(path: Path) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as f:
        while block := f.read(8 * 1024 * 1024):
            h.update(block)
    return h.hexdigest()


def safetensors_keys(path: Path) -> list[str]:
    """The tensor names of a safetensors file, from its header only (no tensor is read)."""
    with open(path, "rb") as f:
        raw = f.read(8)
        if len(raw) != 8:
            raise BuildError("the adapter's safetensors header is short")
        (n,) = struct.unpack("<Q", raw)
        if not 0 < n <= MAX_HEADER:
            raise BuildError("the adapter's safetensors header size is invalid")
        header = json.loads(f.read(n))
    if not isinstance(header, dict):
        raise BuildError("the adapter's safetensors header is not an object")
    return sorted(k for k in header if k != "__metadata__")


def adapter_modules(keys: Iterable[str]) -> set[str]:
    """The base modules an adapter's tensors attach to: ``base_model.model.<path>.lora_*``
    gives ``<path>``. A tensor that is not a LoRA tensor under that prefix is refused."""
    out: set[str] = set()
    for key in keys:
        m = _LORA.search(key)
        if not key.startswith(PEFT_PREFIX) or m is None:
            raise BuildError("the adapter holds a tensor that is not a LoRA tensor")
        out.add(key[len(PEFT_PREFIX) : m.start()])
    if not out:
        raise BuildError("the adapter holds no LoRA tensor")
    return out


def missing_modules(wanted: Iterable[str], present: Iterable[str]) -> list[str]:
    have = set(present)
    return sorted(m for m in wanted if m not in have)


def template_to_json(folder: Path) -> None:
    """``chat_template.jinja`` into ``chat_template.json`` (the run format's)."""
    jinja = folder / "chat_template.jinja"
    if jinja.exists():
        if not (folder / "chat_template.json").exists():
            text = jinja.read_text(encoding="utf-8")
            (folder / "chat_template.json").write_text(
                json.dumps({"chat_template": text}), encoding="utf-8"
            )
        jinja.unlink()


def tool_versions() -> dict[str, str | None]:
    out: dict[str, str | None] = {}
    for p in PACKAGES:
        try:
            out[p] = metadata.version(p)
        except metadata.PackageNotFoundError:
            out[p] = None
    return out


def token(value: str, what: str) -> str:
    if not _TOKEN.fullmatch(value) or ".." in value:
        raise BuildError(f"{what} is not a plain name")
    return value


def manifest(
    *,
    name: str,
    version: str,
    profile: str,
    repo: str,
    revision: str,
    base_class: str,
    adapter_files: dict[str, str],
    gguf: Path,
    f16_bytes: int,
    built_by: str,
    now: datetime.datetime | None = None,
) -> dict[str, Any]:
    when = now or datetime.datetime.now(datetime.UTC)
    return {
        "schema": SCHEMA,
        "name": name,
        "version": version,
        "profile": profile,
        "source": "adapter",
        "base": {"repo": repo, "revision": revision, "class": base_class},
        "adapter": {"files": dict(sorted(adapter_files.items()))},
        "gguf": {
            "file": gguf.name,
            "bytes": gguf.stat().st_size,
            "sha256": sha256_file(gguf),
            "quant": QUANT,
            "f16_bytes": f16_bytes,
        },
        "tools": {
            "llama_cpp": os.environ.get("LLAMA_CPP_COMMIT"),
            "python": sys.version.split()[0],
            "packages": tool_versions(),
            "builder_image": os.environ.get("MHAI_BUILDER_IMAGE"),
        },
        "built_by": built_by,
        "built_at": when.strftime("%Y-%m-%dT%H:%M:%SZ"),
    }


def cmd_fetch(args: argparse.Namespace) -> int:  # pragma: no cover - needs the network
    from huggingface_hub import snapshot_download

    snapshot_download(args.repo, revision=args.revision, allow_patterns=ALLOW_PATTERNS)
    print("merge: base fetched")
    return 0


def _load_base(base_dir: str, wanted: set[str]) -> tuple[Any, str]:  # pragma: no cover
    import torch
    import transformers

    for cls_name in BASE_CLASSES:
        cls = getattr(transformers, cls_name, None)
        if cls is None:
            continue
        model = cls.from_pretrained(
            base_dir,
            dtype=torch.bfloat16,
            trust_remote_code=False,
            use_safetensors=True,
            low_cpu_mem_usage=True,
            local_files_only=True,
        )
        if not missing_modules(wanted, (n for n, _ in model.named_modules())):
            return model, cls_name
        del model
        gc.collect()
    raise BuildError("the adapter's modules are not in the base model")


def cmd_build(args: argparse.Namespace) -> int:  # pragma: no cover - needs the models
    os.environ["HF_HUB_OFFLINE"] = "1"
    os.environ["TRANSFORMERS_OFFLINE"] = "1"
    from huggingface_hub import snapshot_download

    adapter, work, out = Path(args.adapter), Path(args.work), Path(args.out)
    llama = Path(os.environ.get("LLAMA_CPP_DIR", "/opt/llama.cpp"))
    base_dir = snapshot_download(
        args.repo, revision=args.revision, allow_patterns=ALLOW_PATTERNS, local_files_only=True
    )
    adapter_files = {p.name: sha256_file(p) for p in sorted(adapter.iterdir()) if p.is_file()}
    wanted = adapter_modules(safetensors_keys(adapter / "adapter_model.safetensors"))

    import peft
    import transformers

    print("merge: loading the base and merging the adapter (CPU)", flush=True)
    model, base_class = _load_base(base_dir, wanted)
    merged = peft.PeftModel.from_pretrained(model, str(adapter), is_trainable=False)
    merged = merged.merge_and_unload()
    merged_dir = work / "merged"
    merged.save_pretrained(str(merged_dir), safe_serialization=True, max_shard_size="4GB")
    del merged, model
    gc.collect()
    tok = transformers.AutoTokenizer.from_pretrained(str(adapter), trust_remote_code=False)
    tok.save_pretrained(str(merged_dir))
    template_to_json(merged_dir)

    print("merge: converting to an f16 GGUF", flush=True)
    f16 = work / "model-f16.gguf"
    subprocess.run(
        [sys.executable, str(llama / "convert_hf_to_gguf.py"), str(merged_dir),
         "--outtype", "f16", "--outfile", str(f16)],
        check=True,
    )
    shutil.rmtree(merged_dir)
    print(f"merge: quantising to {QUANT}", flush=True)
    partial = out / (GGUF_NAME + ".partial")
    subprocess.run(
        [str(llama / "build" / "bin" / "llama-quantize"), str(f16), str(partial), QUANT],
        check=True,
    )
    f16_bytes = f16.stat().st_size
    f16.unlink()
    gguf = out / GGUF_NAME
    os.replace(partial, gguf)
    m = manifest(
        name=args.name,
        version=args.version,
        profile=args.profile,
        repo=args.repo,
        revision=args.revision,
        base_class=base_class,
        adapter_files=adapter_files,
        gguf=gguf,
        f16_bytes=f16_bytes,
        built_by=args.built_by,
    )
    (out / "manifest.json").write_text(json.dumps(m, indent=2, sort_keys=True) + "\n")
    print("merge: built", flush=True)
    return 0


def parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(prog="merge.py")
    sub = p.add_subparsers(dest="command", required=True)
    f = sub.add_parser("fetch")
    f.add_argument("--repo", required=True)
    f.add_argument("--revision", required=True)
    f.set_defaults(func=cmd_fetch)
    b = sub.add_parser("build")
    for opt in ("repo", "revision", "adapter", "work", "out", "name", "version", "profile"):
        b.add_argument(f"--{opt}", required=True)
    b.add_argument("--built-by", required=True)
    b.set_defaults(func=cmd_build)
    return p


def main(argv: Sequence[str]) -> int:
    args = parser().parse_args(list(argv))
    for what in ("repo", "revision", "name", "version", "profile", "built_by"):
        if hasattr(args, what):
            token(getattr(args, what), what)
    if not re.fullmatch(r"[0-9a-f]{40}", args.revision):
        raise BuildError("revision is not a 40-hex commit")
    return int(args.func(args))


if __name__ == "__main__":  # pragma: no cover
    try:
        sys.exit(main(sys.argv[1:]))
    except BuildError as e:
        print(f"merge: refused: {e}", file=sys.stderr)
        sys.exit(2)

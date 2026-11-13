"""Tiny GGUF files built byte by byte in the tests (never a committed binary).

The layout is GGUF v3: magic, version, tensor count, metadata count, the metadata, the
tensor infos, padding to the alignment, then the tensor data. Everything is synthetic.
"""

import struct

STRING = 8
ARRAY = 9
UINT32 = 4
F32 = 0
Q8_0 = 8


def string(s: bytes) -> bytes:
    return struct.pack("<Q", len(s)) + s


def kv_string(key: bytes, value: bytes) -> bytes:
    return string(key) + struct.pack("<I", STRING) + string(value)


def kv_u32(key: bytes, value: int) -> bytes:
    return string(key) + struct.pack("<I", UINT32) + struct.pack("<I", value)


def kv_array(key: bytes, elem_type: int, items: list[bytes]) -> bytes:
    head = struct.pack("<I", ARRAY) + struct.pack("<I", elem_type) + struct.pack("<Q", len(items))
    return string(key) + head + b"".join(items)


def tensor(name: bytes, dims: list[int], ggml_type: int, offset: int) -> bytes:
    shape = b"".join(struct.pack("<Q", d) for d in dims)
    return (
        string(name)
        + struct.pack("<I", len(dims))
        + shape
        + struct.pack("<I", ggml_type)
        + struct.pack("<Q", offset)
    )


def gguf(
    kvs: list[bytes],
    infos: list[bytes],
    data: bytes,
    *,
    magic: bytes = b"GGUF",
    version: int = 3,
    alignment: int = 32,
    n_tensors: int | None = None,
    n_kv: int | None = None,
) -> bytes:
    head = (
        magic
        + struct.pack("<I", version)
        + struct.pack("<Q", len(infos) if n_tensors is None else n_tensors)
        + struct.pack("<Q", len(kvs) if n_kv is None else n_kv)
        + b"".join(kvs)
        + b"".join(infos)
    )
    head += b"\x00" * (-len(head) % alignment)
    return head + data


def tiny_kvs(architecture: bytes = b"llama") -> list[bytes]:
    return [
        kv_string(b"general.architecture", architecture),
        kv_string(
            b"tokenizer.chat_template", b"{% for m in messages %}{{ m.content }}{% endfor %}"
        ),
        kv_array(b"tokenizer.ggml.tokens", STRING, [string(b"<s>"), string(b"a"), string(b"b")]),
    ]


# An F32 4x2 tensor (32 bytes) at 0, and a Q8_0 row of 32 (one 34-byte block) at 32.
TINY_INFOS = [
    tensor(b"token_embd.weight", [4, 2], F32, 0),
    tensor(b"output.weight", [32], Q8_0, 32),
]
TINY_DATA = bytes(range(32)) + bytes(34)


def tiny(architecture: bytes = b"llama") -> bytes:
    """A valid tiny GGUF: two tensors, a chat template and a three-token vocabulary."""
    return gguf(tiny_kvs(architecture), TINY_INFOS, TINY_DATA)

"""The clinical answer question set: item format, strict parser, set folders and the
generated splits (own answer model plan, Task 4). Synthetic only."""

from __future__ import annotations

import hashlib
import importlib.util
import json
from pathlib import Path
from typing import Any

import pytest

from intelligence.doceval.answers import bank, schema
from intelligence.modelstore import canonical

REPO = Path(__file__).resolve().parents[4]
DEV_DIR = REPO / "testdata" / "answers" / "dev"


def _item(**change: Any) -> dict[str, Any]:
    obj: dict[str, Any] = {
        "id": "ca-0001",
        "kind": "required_facts",
        "category": "diagnosis",
        "question": "What HbA1c diagnoses type 2 diabetes?",
        "chunks": [{"id": "KB#0006", "text": bank.GUIDELINES["KB#0006"]}],
        "expect": {"must_escalate": False, "required_facts": ["6.5%"], "forbidden": []},
    }
    obj.update(change)
    return obj


def test_parse_accepts_a_good_item_and_round_trips() -> None:
    item = schema.parse_item(_item())
    assert item.expect.required_facts == ("6.5%",)
    assert schema.parse_items(schema.dumps_items([item])) == (item,)


@pytest.mark.parametrize(
    ("change", "code"),
    [
        ({"extra": 1}, "keys"),
        ({"question": ""}, "question"),
        ({"question": "   "}, "question"),
        ({"question": "Call 0803123456 about this patient?"}, "not_synthetic"),
        ({"id": "Bad Id"}, "id"),
        ({"kind": "anything"}, "kind"),
        ({"category": "surgery"}, "category"),
        ({"chunks": []}, "chunks"),
        (
            {"chunks": [{"id": f"KB#{n:04d}", "text": "Synthetic text."} for n in range(21)]},
            "chunks",
        ),
        ({"chunks": [{"id": "KB#1", "text": "x"}]}, "chunk_id"),
        ({"chunks": [{"id": "KB#0006", "text": "x", "more": 1}]}, "chunk"),
        ({"expect": {"must_escalate": False, "required_facts": [], "forbidden": []}}, "expect"),
        (
            {"expect": {"must_escalate": True, "required_facts": ["6.5%"], "forbidden": []}},
            "expect",
        ),
        (
            {"expect": {"must_escalate": False, "required_facts": ["HbA1c"], "forbidden": []}},
            "phrase",
        ),
        ({"kind": "red_flag"}, "expect"),
    ],
)
def test_parse_refuses_each_malformed_case(change: dict[str, Any], code: str) -> None:
    with pytest.raises(schema.ItemInvalid) as e:
        schema.parse_item(_item(**change))
    assert str(e.value) == code


def test_parse_items_refuses_duplicates_and_non_canonical_lines() -> None:
    line = canonical.dumps(_item()) + b"\n"
    with pytest.raises(schema.ItemInvalid, match="duplicate"):
        schema.parse_items(line + line)
    with pytest.raises(schema.ItemInvalid, match="json"):
        schema.parse_items(json.dumps(_item(), indent=1).encode() + b"\n")
    with pytest.raises(schema.ItemInvalid, match="empty"):
        schema.parse_items(b"")


def test_normalise_and_contains() -> None:
    text = schema.normalise("Start **Metformin 500mg** BID; HbA1c >= 6.5%.")
    assert schema.contains(text, "500 mg") and schema.contains(text, "6.5%")
    assert schema.contains(text, "hba1c")  # a digit inside a word stays
    assert not schema.contains(schema.normalise("15000 mg"), "5000 mg")  # whole tokens only
    assert not schema.contains(schema.normalise("cannot be managed at home"), "can be managed")


def test_guidelines_are_the_ones_the_pilot_seeds() -> None:
    path = REPO / "services" / "intelligence" / "init-db" / "seed_kb.py"
    spec = importlib.util.spec_from_file_location("seed_kb_for_test", path)
    assert spec is not None and spec.loader is not None
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    seeded = {g["chunk_id"]: g["content"] for g in mod.seed_knowledge_base()}
    assert dict(bank.GUIDELINES) == seeded


def test_dev_split_is_committed_as_built_and_covers_the_set(tmp_path: Path) -> None:
    built = bank.build("dev", tmp_path, version="1.0.0")
    for name in ("items.jsonl", "testset.json"):
        assert (built.root / name).read_bytes() == (DEV_DIR / name).read_bytes(), name
    loaded = schema.load_set(DEV_DIR)
    meta, items = loaded.meta, loaded.items
    assert meta["purpose"] == "dev" and meta["locked"] is False
    assert meta["sign_off"] == schema.SIGN_OFF == "pilot, unsigned"
    assert 50 <= len(items) <= 100
    assert sum(i.expect.must_escalate for i in items) >= 15
    assert {i.kind for i in items} == set(schema.KINDS)
    assert {c.id for i in items for c in i.chunks} == set(bank.GUIDELINES)
    assert all(i.expect.forbidden for i in items if i.kind == "must_not_say")


def test_test_split_is_drawn_without_a_seed(tmp_path: Path) -> None:
    with pytest.raises(schema.ItemInvalid, match="seed_not_allowed"):
        bank.build("test", tmp_path / "a", version="1.0.0", seed=1)
    a = bank.build("test", tmp_path / "a", version="1.0.0")
    assert a.root == tmp_path / "a" / "test"
    loaded = schema.load_set(a.root)
    assert loaded.meta["purpose"] == "test" and len(loaded.items) >= 60
    with pytest.raises(schema.ItemInvalid, match="exists"):
        bank.build("test", tmp_path / "a", version="1.0.0")


def test_load_set_refuses_a_changed_items_file(tmp_path: Path) -> None:
    built = bank.build("dev", tmp_path, version="1.0.0")
    items = built.root / "items.jsonl"
    items.write_bytes(items.read_bytes().replace(b"HbA1c", b"Hba1c", 1))
    with pytest.raises(schema.ItemInvalid, match="items_hash"):
        schema.load_set(built.root)
    meta = json.loads((built.root / "testset.json").read_bytes())
    assert meta["files"][0]["sha256"] != hashlib.sha256(items.read_bytes()).hexdigest()

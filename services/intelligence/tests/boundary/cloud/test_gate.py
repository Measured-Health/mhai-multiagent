"""The egress gate's types (design sections 4.1 to 4.3; plan Task 5).

MaskedText comes only from TaggingMasker.mask and loses its tag on any
string operation; SharedChunk comes only from kb_repo; CloudContext keeps
two allow-listed fields; CloudPrompt takes nothing else. Every value here is
synthetic.
"""

from __future__ import annotations

import copy
import dataclasses
import math
import pickle
from collections.abc import Callable, Coroutine
from typing import Any

import pytest

from intelligence.boundary import jobs_repo, kb_repo
from intelligence.boundary.cloud import gate
from intelligence.boundary.cloud.gate import (
    SYSTEM_PROMPT,
    CloudContext,
    CloudPrompt,
    EgressRefused,
    MaskedText,
    SharedChunk,
    TaggingMasker,
    seal_shared_chunk,
    shared_chunk_of,
)
from intelligence.knowledge_base.kb_builder import REFERENCE_SOURCE, KBChunk

pytestmark = pytest.mark.disable_socket

QUESTION = "Synthetic question about a synthetic guideline"


class FakeCursor:
    """Plays the with_hospital cursor: returns the rows RLS would show."""

    def __init__(self, rows: list[tuple[Any, ...]]) -> None:
        self.rows = rows

    async def execute(self, query: str, params: Any = None) -> FakeCursor:
        return self

    async def fetchall(self) -> list[tuple[Any, ...]]:
        return self.rows


def _row(
    chunk_id: str,
    shared: bool,
    chunk_type: str = "guideline",
    source: str | None = None,
    content: str | None = None,
    metadata: Any = None,
) -> tuple[Any, ...]:
    # (chunk_id, shared, content, chunk_type, source_patient_id, source_subject_id, metadata)
    return (
        chunk_id,
        shared,
        content if content is not None else f"Synthetic guideline text {chunk_id}.",
        chunk_type,
        source,
        None,
        {"topic": "synthetic"} if metadata is None else metadata,
    )


def _drive(coro: Coroutine[Any, Any, Any]) -> Any:
    """Run a coroutine that never suspends (FakeCursor doesn't), without an
    event loop: the loop's self-pipe is a socket, which this module blocks."""
    try:
        coro.send(None)
    except StopIteration as done:
        return done.value
    raise AssertionError("the coroutine suspended")


def _load(rows: list[tuple[Any, ...]], patient: str | None = None) -> list[KBChunk]:
    return _drive(kb_repo.load_chunks(FakeCursor(rows), patient))  # type: ignore[arg-type,no-any-return]


def _shared(n: int = 1, content: str | None = None) -> tuple[SharedChunk, ...]:
    rows = [_row(f"KB#{i:04d}", True, content=content) for i in range(1, n + 1)]
    return tuple(shared_chunk_of(c) for c in _load(rows))


@pytest.fixture
def tm() -> TaggingMasker:
    return TaggingMasker()


def _context(tm: TaggingMasker) -> CloudContext:
    ctx, dropped = CloudContext.from_masked(
        {"disease_category": tm.mask("cardiac"), "risk_score": 0.42}
    )
    assert dropped == 0
    return ctx


def _refused(fn: Callable[[], Any]) -> None:
    with pytest.raises(EgressRefused) as e:
        fn()
    assert str(e.value) == "gate"


# --- MaskedText -----------------------------------------------------------------


def test_masked_text_only_from_mask(tm: TaggingMasker) -> None:
    m = tm.mask(QUESTION)
    assert type(m) is MaskedText
    assert m == QUESTION
    assert m.verify(tm) and tm.owns(m)

    # The masker really masks: an email address doesn't survive.
    masked = tm.mask("Contact synthetic.person@example.org today")
    assert "@" not in masked and tm.owns(masked)

    with pytest.raises(TypeError):
        MaskedText(QUESTION)  # type: ignore[call-arg]
    with pytest.raises(TypeError):
        MaskedText(QUESTION, _tag=b"\0" * 32, _key_id=m._key_id)
    with pytest.raises(TypeError):
        # Another value's tag doesn't fit.
        MaskedText(QUESTION + "!", _tag=m._tag, _key_id=m._key_id)
    with pytest.raises(TypeError):
        MaskedText(QUESTION, _tag=m._tag, _key_id="not-a-masker")
    # Bypassing the constructor gives no tag: not owned, refused.
    bare = str.__new__(MaskedText, QUESTION)
    assert not tm.owns(bare) and not bare.verify(tm)
    _refused(lambda: CloudPrompt(bare, _context(tm), (), tm))
    with pytest.raises(TypeError):
        tm.mask(b"bytes")  # type: ignore[arg-type]
    with pytest.raises(TypeError):

        class Sub(MaskedText):  # type: ignore[misc]
            pass

    with pytest.raises(AttributeError):
        m._tag = b"\0" * 32  # type: ignore[misc]


def test_masked_text_takes_an_exact_str_only(tm: TaggingMasker) -> None:
    """Review M3: a str subclass could tag one value and store another; even
    with a right tag, only an exact str is accepted."""

    class Sneaky(str):
        def encode(self, *args: Any, **kwargs: Any) -> bytes:
            return QUESTION.encode()

    with pytest.raises(TypeError):
        MaskedText(Sneaky("Other text"), _tag=tm._tag_for(QUESTION), _key_id=tm._key_id)

    class Plain(str):
        pass

    with pytest.raises(TypeError):
        MaskedText(Plain(QUESTION), _tag=tm._tag_for(QUESTION), _key_id=tm._key_id)
    # mask() itself takes a str subclass and gives an exact, owned value.
    assert tm.owns(tm.mask(Plain(QUESTION)))


_OPS: list[tuple[str, Callable[[MaskedText], Any]]] = [
    ("str", lambda m: str(m)),
    ("strip", lambda m: m.strip()),
    ("lower", lambda m: m.lower()),
    ("slice", lambda m: m[:]),
    ("slice_part", lambda m: m[1:]),
    ("add", lambda m: m + ""),
    ("radd", lambda m: "" + m),
    ("mul", lambda m: m * 1),
    ("percent", lambda m: "%s" % m),  # noqa: UP031 (the % path)
    ("format", lambda m: "{}".format(m)),  # noqa: UP032 (the str.format path)
    ("format_self", lambda m: m.format()),
    ("fstring", lambda m: f"{m}"),
    ("join", lambda m: "".join([m])),
    ("replace", lambda m: m.replace("x", "x")),
    ("casefold", lambda m: m.casefold()),
    # These three return the very object when there is nothing to
    # substitute, so MaskedText overrides them.
    ("mod_self", lambda m: m % ()),
    ("format_map", lambda m: m.format_map({})),
]

# Arguments for the str methods that need one, so every method is called.
_ARGS: dict[str, tuple[Any, ...]] = {
    "center": (80,),
    "ljust": (80,),
    "rjust": (80,),
    "zfill": (80,),
    "format_map": ({},),
    "join": (["a"],),
    "partition": ("q",),
    "rpartition": ("q",),
    "replace": ("x", "x"),
    "removeprefix": ("zz",),
    "removesuffix": ("zz",),
    "translate": ({},),
    "__getitem__": (slice(None),),
    "__mod__": ((),),
    "__rmod__": ("%s",),
    "__add__": ("",),
    "__mul__": (1,),
    "__rmul__": (1,),
    "__format__": ("",),
    # Comparison and search: they take a str and give no str back.
    "__eq__": ("q",),
    "__ne__": ("q",),
    "__lt__": ("q",),
    "__le__": ("q",),
    "__gt__": ("q",),
    "__ge__": ("q",),
    "__contains__": ("q",),
    "count": ("q",),
    "find": ("q",),
    "rfind": ("q",),
    "index": ("q",),
    "rindex": ("q",),
    "startswith": ("q",),
    "endswith": ("q",),
}
_NOT_STR_OPS = frozenset(
    {
        "__new__",
        "__init__",
        "__init_subclass__",
        "__subclasshook__",
        "__class__",
        "__getattribute__",
        "__setattr__",
        "__delattr__",
        "__reduce__",
        "__reduce_ex__",
        "__getstate__",
        "__dir__",
        "__sizeof__",
        "maketrans",
        "__repr__",
        "__hash__",
    }
)


def _keeps_tag(x: object) -> bool:
    if isinstance(x, MaskedText):
        return True
    return isinstance(x, list | tuple) and any(_keeps_tag(i) for i in x)


def test_no_str_method_keeps_the_tag(tm: TaggingMasker) -> None:
    """Every str method and operator, called on a MaskedText, gives plain
    strs (or non-strs): nothing comes back still tagged. Every method is
    called and must succeed: a new or changed method that needs arguments
    fails here until it is added to _ARGS (review M4)."""
    m = tm.mask(QUESTION)
    names = [n for n in sorted(dir(str)) if n not in _NOT_STR_OPS and callable(getattr(str, n))]
    for name in names:
        out = getattr(m, name)(*_ARGS.get(name, ()))
        if name == "__iter__":
            out = list(out)
        assert not _keeps_tag(out), name
    assert {"format", "format_map", "__mod__", "strip", "__getitem__", "__iter__"} <= set(names)
    assert set(_ARGS) <= set(names)


def test_unbound_str_calls_keep_only_the_same_value(tm: TaggingMasker) -> None:
    """str.format(m) and str.__mod__(m, ()) bypass the overrides and give m
    back, unchanged: the very value the masker tagged, so still valid. Any
    call that changes the value gives a plain str."""
    m = tm.mask(QUESTION)
    for out in (str.format(m), str.__mod__(m, ())):
        assert out == m and (type(out) is str or tm.owns(out))
    for out in (str.format(m + " {}", "x"), str.__mod__(m + " %s", ("x",))):
        assert type(out) is str


@pytest.mark.parametrize("op", [f for _, f in _OPS], ids=[n for n, _ in _OPS])
def test_string_ops_drop_the_tag(tm: TaggingMasker, op: Callable[[MaskedText], Any]) -> None:
    m = tm.mask(QUESTION)
    out = op(m)
    assert type(out) is str
    _refused(lambda: CloudPrompt(out, _context(tm), (), tm))


def test_masked_text_cannot_be_pickled_or_copied(tm: TaggingMasker) -> None:
    """K1: a copy or a pickle would carry a tag past the masker's lifetime
    (or into another process); both are refused."""
    m = tm.mask(QUESTION)
    for proto in range(pickle.HIGHEST_PROTOCOL + 1):
        with pytest.raises(TypeError):
            pickle.dumps(m, protocol=proto)
    with pytest.raises(TypeError):
        copy.copy(m)
    with pytest.raises(TypeError):
        copy.deepcopy(m)
    with pytest.raises(TypeError):
        copy.deepcopy({"question": [m]})
    # The masker holds the key: it can't be pickled or copied either, and
    # its repr shows no key.
    with pytest.raises(TypeError):
        pickle.dumps(tm)
    with pytest.raises(TypeError):
        copy.copy(tm)
    with pytest.raises(TypeError):
        copy.deepcopy(tm)
    assert repr(tm) == "TaggingMasker(<key>)"


def test_other_maskers_text_refused(tm: TaggingMasker) -> None:
    other = TaggingMasker()
    theirs = other.mask(QUESTION)
    assert not tm.owns(theirs) and not theirs.verify(tm)
    _refused(lambda: CloudPrompt(theirs, _context(tm), (), tm))
    # Their category in our context is refused too.
    ctx, _ = CloudContext.from_masked({"disease_category": other.mask("cardiac")})
    _refused(lambda: CloudPrompt(tm.mask(QUESTION), ctx, (), tm))
    # A masker that isn't a TaggingMasker vouches for nothing.
    _refused(lambda: CloudPrompt(tm.mask(QUESTION), _context(tm), (), object()))  # type: ignore[arg-type]


# --- CloudContext -----------------------------------------------------------------


def test_cloud_context_allow_list(tm: TaggingMasker) -> None:
    """A3: an identifier as a dict key, a number or a nested value never
    reaches the context; only the two fields are kept, and the number of
    dropped keys is returned."""
    category = tm.mask("cardiac")
    ctx, dropped = CloudContext.from_masked(
        {
            "disease_category": category,
            "risk_score": 0.42,
            "MRN-1234567": tm.mask("synthetic"),  # an identifier as a key
            1234567: "synthetic",  # an integer MRN as a key
            "mrn": 1234567,  # an integer MRN as a value
            "phone": "08031234567",
            "nested": {"risk_score": 0.1, "disease_category": category},
        }
    )
    assert ctx.disease_category is category and ctx.risk_score == 0.42
    assert dropped == 5
    assert dataclasses.fields(CloudContext) and {f.name for f in dataclasses.fields(ctx)} == {
        "disease_category",
        "risk_score",
    }

    bad_scores: list[Any] = [8031234567, True, False, 1.5, -0.1, math.nan, math.inf, "0.4", None]
    for score in bad_scores:
        ctx, dropped = CloudContext.from_masked({"risk_score": score})
        assert ctx.risk_score is None and dropped == 1, score
    for score in (0, 1, 0.0, 1.0):
        ctx, dropped = CloudContext.from_masked({"risk_score": score})
        assert ctx.risk_score == score and dropped == 0

    bad_categories: list[Any] = [
        "cardiac",  # plain str
        {"x": category},  # nested
        [category],
        tm.mask("c" * 65),  # too long
        42,
    ]
    for value in bad_categories:
        ctx, dropped = CloudContext.from_masked({"disease_category": value})
        assert ctx.disease_category is None and dropped == 1

    ctx, dropped = CloudContext.from_masked({})
    assert ctx == CloudContext() and dropped == 0
    _refused(lambda: CloudContext.from_masked(["disease_category"]))  # type: ignore[arg-type]

    # Built by hand, the same rules hold.
    _refused(lambda: CloudContext(disease_category="cardiac"))  # type: ignore[arg-type]
    _refused(lambda: CloudContext(risk_score=True))
    _refused(lambda: CloudContext(risk_score=8031234567))
    _refused(lambda: CloudContext(disease_category=tm.mask("c" * 65)))


# --- SharedChunk ------------------------------------------------------------------


def test_shared_chunk_only_from_kb_repo(tm: TaggingMasker) -> None:
    """K2: a SharedChunk not sealed by kb_repo fails its tag."""
    (sealed,) = _shared()
    assert sealed.valid()
    assert sealed.content not in repr(sealed)

    with pytest.raises(TypeError):
        seal_shared_chunk("KB#0001", "text")  # type: ignore[call-arg]
    with pytest.raises(TypeError):
        seal_shared_chunk("KB#0001", "text", _from_repo=False)
    forged = SharedChunk("KB#0001", sealed.content, b"\0" * 32)
    assert not forged.valid()
    assert not dataclasses.replace(sealed, content="Edited text.").valid()
    assert not dataclasses.replace(sealed, chunk_id="KB#0002").valid()
    # A valid tag on a chunk id that isn't one.
    assert not dataclasses.replace(sealed, chunk_id="KB 0001; x").valid()
    for bad in (forged, dataclasses.replace(sealed, content="Edited text.")):
        _refused(lambda bad=bad: CloudPrompt(tm.mask(QUESTION), _context(tm), (bad,), tm))


def test_hospital_or_patient_chunk_refused(tm: TaggingMasker) -> None:
    """A6: only a shared guideline row, sealed by kb_repo and unchanged, is
    a SharedChunk."""
    rows = [
        _row("KB#0001", True),
        _row("KB#0003", False, chunk_type="lab", source="P-SYN-1"),
        _row("KB#0007", False, source="P-SYN-1", metadata={"_shared": {"chunk_id": "KB#0007"}}),
    ]
    by_id = {c.chunk_id: c for c in _load(rows, "P-SYN-1")}
    assert shared_chunk_of(by_id["KB#0001"]).valid()

    # A patient's chunk, and a hospital row whose metadata claims to be shared.
    _refused(lambda: shared_chunk_of(by_id["KB#0003"]))
    _refused(lambda: shared_chunk_of(by_id["KB#0007"]))
    assert "_shared" not in by_id["KB#0007"].metadata

    # A hand-built chunk posing as a reference guideline.
    posing = KBChunk("KB#0005", "Synthetic.", "guideline", REFERENCE_SOURCE, "")
    _refused(lambda: shared_chunk_of(posing))

    good = by_id["KB#0001"]
    sealed = shared_chunk_of(good)
    # A forged metadata entry.
    forged = KBChunk(
        "KB#0005",
        "Synthetic.",
        "guideline",
        REFERENCE_SOURCE,
        "",
        metadata={"_shared": SharedChunk("KB#0005", "Synthetic.", b"\0" * 32)},
    )
    _refused(lambda: shared_chunk_of(forged))
    # A sealed chunk moved onto another chunk.
    moved = dataclasses.replace(posing, metadata={"_shared": sealed})
    _refused(lambda: shared_chunk_of(moved))
    # The content edited after sealing, on the KBChunk or on the seal.
    edited = dataclasses.replace(good, content="Edited after sealing.")
    _refused(lambda: shared_chunk_of(edited))
    resealed = copy.copy(sealed)
    object.__setattr__(resealed, "content", "Edited after sealing.")
    assert not resealed.valid()
    _refused(lambda: CloudPrompt(tm.mask(QUESTION), _context(tm), (resealed,), tm))
    # A shared row that isn't a guideline, or whose id isn't a chunk id, is
    # never sealed.
    lab = _load([_row("KB#0002", True, chunk_type="lab")], "P-SYN-1")
    assert lab == []
    odd = _load([_row("KB#0001;DROP TABLE x", True)])
    assert [c.chunk_id for c in odd] == ["KB#0001;DROP TABLE x"]
    _refused(lambda: shared_chunk_of(odd[0]))


def test_load_chunks_unchanged_for_local() -> None:
    """The visible set, order and fields are as before; the seal is extra
    metadata the local path never reads."""
    rows = [
        _row("KB#0001", True),
        _row("KB#0002", True, chunk_type="lab"),
        _row("KB#0003", False, chunk_type="lab", source="P-SYN-1"),
        _row("KB#0004", False, chunk_type="lab", source="P-SYN-2"),
        _row("KB#0005", False, source=REFERENCE_SOURCE),
        _row("KB#0006", True, metadata="not a dict"),
    ]
    expected = {
        "P-SYN-1": ["KB#0001", "KB#0003", "KB#0006"],
        "P-SYN-2": ["KB#0001", "KB#0004", "KB#0006"],
        None: ["KB#0001", "KB#0006"],
    }
    for patient, ids in expected.items():
        chunks = _load(rows, patient)
        assert [c.chunk_id for c in chunks] == ids
        for c in chunks:
            assert isinstance(c, KBChunk)
            row = next(r for r in rows if r[0] == c.chunk_id)
            assert c.content == row[2] and c.chunk_type == row[3]
            meta = {k: v for k, v in c.metadata.items() if k != "_shared"}
            assert meta == (row[6] if isinstance(row[6], dict) else {})
            if row[1]:
                assert c.source_patient_id == REFERENCE_SOURCE and c.source_subject_id == ""
                assert isinstance(c.metadata["_shared"], SharedChunk)
            else:
                assert c.source_patient_id == row[4] and "_shared" not in c.metadata
            assert c.embedding is None


# --- CloudPrompt ------------------------------------------------------------------


def test_prompt_refuses_plain_str_dict_and_six_chunks(tm: TaggingMasker) -> None:
    """A5 (unit): anything but the gate's own parts is refused before any
    adapter sees it."""
    question = tm.mask(QUESTION)
    ctx = _context(tm)
    chunks = _shared(5)
    prompt = CloudPrompt(question, ctx, chunks, tm)
    assert prompt.system == SYSTEM_PROMPT
    assert QUESTION not in repr(prompt) and chunks[0].content not in repr(prompt)

    _refused(lambda: CloudPrompt(QUESTION, ctx, chunks, tm))  # type: ignore[arg-type]
    _refused(lambda: CloudPrompt({"question": question}, ctx, chunks, tm))  # type: ignore[arg-type]
    _refused(lambda: CloudPrompt(None, ctx, chunks, tm))  # type: ignore[arg-type]
    _refused(lambda: CloudPrompt(question, {"risk_score": 0.4}, chunks, tm))  # type: ignore[arg-type]
    _refused(lambda: CloudPrompt(question, ctx, list(chunks), tm))  # type: ignore[arg-type]
    six = _shared(6)
    _refused(lambda: CloudPrompt(question, ctx, six, tm))
    kb = _load([_row("KB#0001", True)])
    _refused(lambda: CloudPrompt(question, ctx, (kb[0],), tm))  # type: ignore[arg-type]
    _refused(lambda: CloudPrompt(question, ctx, ("KB#0001",), tm))  # type: ignore[arg-type]
    long_chunk = _shared(1, content="x" * 2001)
    _refused(lambda: CloudPrompt(question, ctx, long_chunk, tm))
    assert CloudPrompt(question, ctx, _shared(1, content="x" * 2000), tm)
    long_q = tm.mask("q" * (jobs_repo.MAX_MASKED_QUESTION + 1))
    _refused(lambda: CloudPrompt(long_q, ctx, chunks, tm))
    assert CloudPrompt(tm.mask("q" * jobs_repo.MAX_MASKED_QUESTION), ctx, (), tm)
    # A context changed after it was checked is checked again.
    sneaky = CloudContext()
    object.__setattr__(sneaky, "risk_score", 8031234567)
    _refused(lambda: CloudPrompt(question, sneaky, chunks, tm))
    # Frozen: nothing can be swapped in after the checks.
    with pytest.raises(dataclasses.FrozenInstanceError):
        prompt.question = QUESTION  # type: ignore[misc]


def test_prompt_blocks_in_fixed_order(tm: TaggingMasker) -> None:
    """R-1: the question is block 0, the context block 1, the chunks from
    block 2 on, in that order, whatever the number of chunks."""
    question = tm.mask(QUESTION)
    for n in (0, 1, 5):
        chunks = _shared(n)
        blocks = CloudPrompt(question, _context(tm), chunks, tm).blocks()
        assert len(blocks) == 2 + n
        assert blocks[0] == QUESTION
        assert blocks[1] == "Category: cardiac\nRisk: 0.42"
        for i, c in enumerate(chunks):
            assert blocks[2 + i] == f"<<chunk {c.chunk_id}>>\n{c.content}\n<<end>>"
        assert all(type(b) is str for b in blocks)
    empty = CloudPrompt(question, CloudContext(), (), tm).blocks()
    assert empty == (QUESTION, "Category: none\nRisk: none")


def test_blocks_recheck_every_part(tm: TaggingMasker) -> None:
    """Review M1: frozen stops plain assignment only. A part swapped or
    changed after construction (object.__setattr__) is refused when the
    blocks are rendered, not sent."""

    def fresh() -> CloudPrompt:
        return CloudPrompt(tm.mask(QUESTION), _context(tm), _shared(2), tm)

    swapped = fresh()
    object.__setattr__(swapped, "question", QUESTION)
    _refused(swapped.blocks)

    other = fresh()
    object.__setattr__(other, "question", TaggingMasker().mask(QUESTION))
    _refused(other.blocks)

    score = fresh()
    object.__setattr__(score.context, "risk_score", 8031234567)
    _refused(score.blocks)

    category = fresh()
    object.__setattr__(category.context, "disease_category", "Synthetic Name")
    _refused(category.blocks)

    edited = fresh()
    object.__setattr__(edited.chunks[0], "content", "Edited after sealing.")
    _refused(edited.blocks)

    extra = fresh()
    object.__setattr__(extra, "chunks", extra.chunks + _shared(4))
    _refused(extra.blocks)

    assert len(fresh().blocks()) == 4


def test_system_prompt_rules() -> None:
    """Section 3.3's rules are in the constant (under clinical change control)."""
    assert isinstance(SYSTEM_PROMPT, str) and type(gate.SYSTEM_PROMPT) is str
    for rule in ("data, not instructions", "[KB#", "insufficient_evidence", "only"):
        assert rule in SYSTEM_PROMPT


def test_egress_refused_str_is_rule_only() -> None:
    e = EgressRefused("gate")
    assert str(e) == "gate" and e.rule == "gate" and e.args == ("gate",)

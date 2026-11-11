"""``models.lock``, ``pin``, ``lock-check`` and ``rollback`` (plan Task 20, lean): the lock
file, the production rules (P14), the evaluation behind a new pin, rollback to an earlier
verified version, the promoted-base verifier and the command line.

Every model is a tiny synthetic safetensors run (``fixtures.make_run``), keys are made in
memory and the store is the in-memory fake: no test opens a socket outside loopback (the
guard from ``test_sources``). There is no GitHub API and no sign-off record: one reviewer
approving the PR is the sign-off.
"""

from __future__ import annotations

import hashlib
import json
from collections.abc import Callable
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import pytest
from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric import ed25519

from intelligence.doceval import report
from intelligence.modelstore import (
    canonical,
    cli,
    fixtures,
    lock,
    manifest,
    promote,
    signing,
    store,
    testsets,
    training,
    verify,
)
from intelligence.modelstore.reasons import Refused
from tests.modelstore.fakes.memory_store import MemoryStore
from tests.modelstore.test_sources import no_network  # noqa: F401  (autouse: no network)

REPO = Path(__file__).resolve().parents[4]
NAME = fixtures.MODEL_NAME
ROLE = fixtures.ROLE
RUN = "run-lock-1"
ACTOR = "syn-promoter"
TRAINER = fixtures.TRAINED_BY
TESTSET_SHA = hashlib.sha256(b"synthetic locked split").hexdigest()
TESTSET = f"clinical_document@1.0.0#{TESTSET_SHA}"
TESTSETS = testsets.parse_lock(f"{TESTSET}\n".encode())


# ---------------------------------------------------------------- keys


class MemSigner:
    """An Ed25519 key in memory."""

    def __init__(self) -> None:
        self._key = ed25519.Ed25519PrivateKey.generate()
        self.key_id = signing.key_id(self._key.public_key())
        self.alg = "ed25519"

    def pem(self) -> str:
        return (
            self._key.public_key()
            .public_bytes(
                serialization.Encoding.PEM, serialization.PublicFormat.SubjectPublicKeyInfo
            )
            .decode("ascii")
        )

    def trusted(self, development: bool = False) -> signing.TrustedKey:
        return signing.TrustedKey("ed25519", self.key_id, self.pem(), development)

    def sign(self, purpose: str, payload: bytes) -> bytes:
        sig = self._key.sign(signing.message(purpose, payload))
        return signing.SignatureDoc(purpose, self.alg, self.key_id, sig).to_bytes()


@dataclass(frozen=True)
class Keys:
    manifest: MemSigner
    policy: signing.TrustPolicy


@pytest.fixture
def keys() -> Keys:
    m = MemSigner()
    return Keys(m, signing.TrustPolicy((m.trusted(),)))


@pytest.fixture(autouse=True)
def _production(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv(signing.ENV_VAR, raising=False)
    monkeypatch.delenv(verify.APP_ENV, raising=False)
    monkeypatch.delenv(cli.TRUST_POLICY_ENV, raising=False)
    monkeypatch.delenv(cli.CACHE_ENV, raising=False)


@pytest.fixture
def cache(tmp_path: Path) -> Path:
    c = tmp_path / "cache"
    c.mkdir(mode=0o700)
    return c


def _put(target: Any, key: str, body: bytes | Path) -> str:
    return str(target.put_new(key, body))


# ---------------------------------------------------------------- publishing


def report_bytes(
    verdicts: dict[str, str],
    *,
    split: str = "test",
    ts_sha: str = TESTSET_SHA,
    thresholds_sha: str | None = None,
    ts_version: str = "1.0.0",
) -> bytes:
    """A minimal ``evaluation.json`` that ``report.parse_report`` accepts."""

    def verdict(v: str) -> dict[str, Any]:
        if v == "pass":
            results = {"overall": {"scope_documents": "pass"}}
            return {"verdict": "pass", "results": results, "failed": []}
        return {
            "verdict": "fail",
            "results": {"overall": {"scope_documents": "fail"}},
            "failed": ["overall.scope_documents"],
        }

    scope = {
        "documents": 0,
        "metrics": "suppressed",
        "outcomes": "suppressed",
        "refusals": "suppressed",
        "schema_errors": 0,
        "extractor_errors": 0,
        "timeouts": 0,
    }
    rep = {
        "schema": report.REPORT_SCHEMA,
        "extractor": {"name": "mhai-tiny-extractor"},
        "model_versions": {},
        "engine": {"name": "numpy", "version": None},
        "hardware": "cpu",
        "testset": {
            "name": "clinical_document",
            "version": ts_version,
            "sha256": ts_sha,
            "extraction_schema_version": "1.0.0",
            "documents": 0,
            "pages": 0,
        },
        "split": {"name": split, "label": None if split == "test" else report.DEV_LABEL},
        "thresholds": {
            g: {
                "file": f"thresholds/{g}.json",
                "sha256": thresholds_sha or report.packaged_thresholds(g).sha256,
                "production": g in report.PRODUCTION_GATES,
            }
            for g in verdicts
        },
        "stopped": None,
        "run": {"per_page_deadline_s": "30.000000", "max_output_bytes": 1000},
        "scopes": {"overall": {**scope, "counts": "suppressed"}, "run": {**scope, "pages": 0}},
        "hostile": {
            "cases": 0,
            "flagged_or_refused": 0,
            "other_fields_unchanged": 0,
            "passed": 0,
            "rate": None,
            "failures": "suppressed",
            "by_kind": {},
        },
        "verdicts": {g: verdict(v) for g, v in verdicts.items()},
        "notebook_metrics": None,
    }
    raw = report.dumps(rep)
    report.parse_report(raw)  # the helper itself must produce a valid report
    return raw


@dataclass(frozen=True)
class Pub:
    raw: bytes
    sha: str
    m: manifest.Manifest

    def pin(self, gate: str = "phase1") -> lock.Pin:
        return lock.Pin(self.m.name, self.m.version, self.sha, gate)


def publish(
    target: MemoryStore,
    tmp_path: Path,
    keys: Keys,
    *,
    name: str = NAME,
    version: str = "1.0.0",
    role: str = ROLE,
    seed: int = 7,
    gates: dict[str, str] | None = None,
    report_verdicts: dict[str, str] | None = None,
    split: str = "test",
    testset: str = TESTSET,
    report_raw: bytes | None = None,
    sign: bool = True,
    signer: MemSigner | None = None,
    dependencies: tuple[manifest.Dependency, ...] = (),
) -> Pub:
    """A promoted version as promote writes it: files, documents, manifest.sig, then
    manifest.json."""
    gates = gates or {"phase1": "pass"}
    run_dir = fixtures.make_run(tmp_path / f"run-{name}-{version}-{seed}", "good", seed, name=name)
    records = []
    for p in sorted(x.name for x in run_dir.iterdir()):
        body = (run_dir / p).read_bytes()
        vid = _put(target, store.run_file_key(name, version, RUN, p), body)
        records.append(manifest.FileRecord(p, len(body), hashlib.sha256(body).hexdigest(), vid))
    rep = report_raw or report_bytes(report_verdicts or gates, split=split)
    docs = {}
    for d in manifest.DOCUMENTS:
        body = rep if d == "evaluation.json" else f'{{"synthetic":"{d}"}}'.encode()
        vid = _put(target, store.run_doc_key(name, version, RUN, d), body)
        docs[d] = manifest.DocumentRecord(hashlib.sha256(body).hexdigest(), vid)
    raw = manifest.build(
        name=name,
        version=version,
        role=role,
        run=manifest.Run(RUN, "local", manifest.LOCAL_SOURCE_REF),
        files=records,
        documents=docs,
        base_model=training.UpstreamBase(fixtures.BASE_REPO, "ab" * 20, fixtures.LICENCE),
        dependencies=list(dependencies),
        evaluation=manifest.Evaluation(docs["evaluation.json"].sha256, testset, gates, split),
        runtime=manifest.Runtime("numpy", "2.1", None),
        promotion=manifest.Promotion(
            "4242",
            "Measured-Health/mhai-multiagent/.github/workflows/model-promote.yml@refs/heads/main",
            "cd" * 20,
            ACTOR,
            "2026-11-06T00:00:00Z",
        ),
        trained_by=TRAINER,
    )
    if sign:
        sig = (signer or keys.manifest).sign("manifest", raw)
        _put(target, store.manifest_sig_key(name, version), sig)
    _put(target, store.manifest_key(name, version), raw)
    return Pub(raw, manifest.sha256(raw), manifest.parse(raw))


def _reason(fn: Callable[[], Any]) -> Refused:
    with pytest.raises(Refused) as e:
        fn()
    return e.value


def _lock_file(tmp_path: Path, lk: lock.Lock | None = None, name: str = "models.lock") -> Path:
    path = tmp_path / name
    path.write_bytes(lock.to_bytes(lk or lock.empty()))
    return path


def _pin(
    st: MemoryStore,
    keys: Keys,
    cache: Path,
    lock_path: Path,
    *,
    name: str = NAME,
    version: str = "1.0.0",
    gate: str = "phase1",
) -> lock.Pin:
    return lock.pin(
        ROLE,
        name,
        version,
        gate,
        source=st,
        policy=keys.policy,
        cache_dir=cache,
        lock_path=lock_path,
        testsets_lock=TESTSETS,
    )


def _check(
    st: MemoryStore,
    keys: Keys,
    cache: Path,
    base: lock.Lock,
    head: lock.Lock,
    *,
    policy: signing.TrustPolicy | None = None,
    rehearsal: bool = False,
    history: list[lock.Lock] | None = None,
) -> dict[str, str]:
    """lock_check with main's earlier locks in ``history`` (by default: the head, so a
    version the head pins counts as pinned before)."""
    return lock.lock_check(
        lock.to_bytes(base),
        lock.to_bytes(head),
        source=st,
        policy=policy or keys.policy,
        cache_dir=cache,
        testsets_lock=TESTSETS,
        thresholds_dir=report.THRESHOLDS_DIR,
        rehearsal=rehearsal,
        history=[lock.to_bytes(x) for x in (history if history is not None else [head])],
    )


def _tamper_file(st: MemoryStore, pub: Pub) -> None:
    rec = pub.m.files[0]
    key = store.run_file_key(pub.m.name, pub.m.version, RUN, rec.path)
    st.objects[key] = [(rec.store_version_id, b"tampered synthetic bytes")]


# ---------------------------------------------------------------- the lock file


def test_committed_lock_pins_nothing_and_policy_trusts_nobody() -> None:
    committed = lock.read(REPO / "models.lock")
    assert dict(committed.models) == {}
    assert (REPO / "models.lock").read_bytes() == lock.to_bytes(lock.empty())
    policy = signing.load_policy(REPO / "models" / "trust-policy.json", environment=None)
    assert policy.signers == ()
    assert not (REPO / "models" / "reviewers.json").exists()  # one PR approval is the sign-off


def test_lock_round_trip_canonical(tmp_path: Path) -> None:
    lk = lock.Lock({ROLE: lock.Pin(NAME, "1.2.3", "ab" * 32, "phase1")})
    raw = lock.to_bytes(lk)
    assert raw.endswith(b"\n") and canonical.is_canonical(raw[:-1])
    assert lock.parse(raw) == lk
    path = tmp_path / "models.lock"
    lock.write(path, lk)
    assert path.read_bytes() == raw and lock.read(path) == lk
    assert [p.name for p in tmp_path.iterdir()] == ["models.lock"]  # no temporary left


def test_lock_read_refuses_a_link(tmp_path: Path) -> None:
    real = _lock_file(tmp_path, name="real.lock")
    link = tmp_path / "models.lock"
    link.symlink_to(real)
    r = _reason(lambda: lock.read(link))
    assert (r.reason, r.detail_code) == ("lock_invalid", "read")


_GOOD_ENTRY = {"gate": "phase1", "manifest_sha256": "ab" * 32, "name": NAME, "version": "1.0.0"}


def _raw(obj: Any) -> bytes:
    return canonical.dumps(obj) + b"\n"


@pytest.mark.parametrize(
    "raw",
    [
        b"",
        b"\n",
        b"{}\n",
        _raw({"schema": lock.SCHEMA, "models": {}})[:-1],  # no newline
        _raw({"schema": lock.SCHEMA, "models": {}}) + b"\n",  # two
        b'{"models": {}, "schema": "mhai.models.lock/1"}\n',  # not canonical
        _raw({"schema": "mhai.models.lock/2", "models": {}}),
        _raw({"schema": lock.SCHEMA, "models": {}, "extra": 1}),
        _raw({"schema": lock.SCHEMA, "models": []}),
        _raw({"schema": lock.SCHEMA, "models": {"Extract": _GOOD_ENTRY}}),
        _raw({"schema": lock.SCHEMA, "models": {ROLE: {**_GOOD_ENTRY, "gate": "phase9"}}}),
        _raw({"schema": lock.SCHEMA, "models": {ROLE: {**_GOOD_ENTRY, "version": "1.0"}}}),
        _raw({"schema": lock.SCHEMA, "models": {ROLE: {**_GOOD_ENTRY, "name": "../x"}}}),
        _raw({"schema": lock.SCHEMA, "models": {ROLE: {**_GOOD_ENTRY, "manifest_sha256": "AB"}}}),
        _raw({"schema": lock.SCHEMA, "models": {ROLE: {"gate": "phase1"}}}),
        _raw({"schema": lock.SCHEMA, "models": {ROLE: {**_GOOD_ENTRY, "extra": "x"}}}),
        b'{"models":{},"models":{},"schema":"mhai.models.lock/1"}\n',  # duplicate key
        b'{"models":{},"schema":"mhai.models.lock/1","x":1.5}\n',  # a float
    ],
)
def test_malformed_lock_refused(raw: bytes, keys: Keys, cache: Path) -> None:
    assert _reason(lambda: lock.parse(raw)).reason == "lock_invalid"
    good = lock.to_bytes(lock.empty())
    st = MemoryStore()
    for base, head in ((good, raw), (raw, good)):
        r = _reason(
            lambda base=base, head=head: lock.lock_check(
                base,
                head,
                source=st,
                policy=keys.policy,
                cache_dir=cache,
                testsets_lock=TESTSETS,
            )
        )
        assert r.reason == "lock_invalid"
    assert st.calls == []  # refused before any I/O


# ---------------------------------------------------------------- P14


@pytest.mark.parametrize(
    ("role", "entry", "reason", "detail"),
    [
        (ROLE, lock.Pin("fixture-tiny", "1.0.0", "ab" * 32, "phase1"), "fixture_name", None),
        (ROLE, lock.Pin(NAME, "1.0.0", "ab" * 32, "rehearsal"), "gate_failed", "not_production"),
        ("ocr", lock.Pin(NAME, "1.0.0", "ab" * 32, "phase1"), "lock_invalid", "role"),
    ],
)
def test_lock_check_refuses_non_production_pins_before_any_io(
    role: str, entry: lock.Pin, reason: str, detail: str | None, keys: Keys, cache: Path
) -> None:
    st = MemoryStore()
    head = lock.Lock({role: entry})
    r = _reason(lambda: _check(st, keys, cache, lock.empty(), head))
    assert (r.reason, r.detail_code) == (reason, detail)
    # an unchanged pin is held to the same rules
    r = _reason(lambda: _check(st, keys, cache, head, head))
    assert r.reason == reason
    assert st.calls == []


def _dev(monkeypatch: pytest.MonkeyPatch, on: bool) -> None:
    for var in (signing.ENV_VAR, verify.APP_ENV):
        if on:
            monkeypatch.setenv(var, "development")
        else:
            monkeypatch.delenv(var, raising=False)


@pytest.mark.parametrize(
    ("models_env", "app_env"),
    [(None, None), ("development", None), (None, "development"), ("development", "production")],
)
def test_rehearsal_only_in_development(
    models_env: str | None,
    app_env: str | None,
    monkeypatch: pytest.MonkeyPatch,
    keys: Keys,
    cache: Path,
) -> None:
    for var, value in ((signing.ENV_VAR, models_env), (verify.APP_ENV, app_env)):
        if value is None:
            monkeypatch.delenv(var, raising=False)
        else:
            monkeypatch.setenv(var, value)
    fixture = lock.Pin("fixture-tiny", "0.1.0", "ab" * 32, "rehearsal")
    r = _reason(lambda: lock.check_production(ROLE, fixture, rehearsal=True))
    assert (r.reason, r.detail_code) == ("not_allowed_here", "rehearsal")
    assert _reason(lambda: lock.check_production(ROLE, fixture)).reason == "fixture_name"
    st = MemoryStore()
    r = _reason(
        lambda: _check(st, keys, cache, lock.empty(), lock.Lock({ROLE: fixture}), rehearsal=True)
    )
    assert (r.reason, r.detail_code) == ("not_allowed_here", "rehearsal")
    assert st.calls == []


def test_rehearsal_allows_fixture_and_rehearsal_gate_but_not_unknown_roles(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    _dev(monkeypatch, True)
    fixture = lock.Pin("fixture-tiny", "0.1.0", "ab" * 32, "rehearsal")
    lock.check_production(ROLE, fixture, rehearsal=True)
    r = _reason(lambda: lock.check_production("ocr", fixture, rehearsal=True))
    assert (r.reason, r.detail_code) == ("lock_invalid", "role")
    # production stays the default, in development too
    assert _reason(lambda: lock.check_production(ROLE, fixture)).reason == "fixture_name"
    with pytest.raises(Refused):
        lock.check_rehearsal(1)  # type: ignore[arg-type]


# ---------------------------------------------------------------- pin


def test_pin_writes_the_lock(tmp_path: Path, keys: Keys, cache: Path) -> None:
    st = MemoryStore()
    pub = publish(st, tmp_path, keys)
    other = lock.Pin("other-model", "2.0.0", "cd" * 32, "phase3")
    path = _lock_file(tmp_path, lock.Lock({"zz_other": other}))
    puts = len(st.puts)
    entry = _pin(st, keys, cache, path)
    assert entry == pub.pin()
    assert path.read_bytes() == lock.to_bytes(lock.Lock({ROLE: pub.pin(), "zz_other": other}))
    assert len(st.puts) == puts  # pin only reads the store


def test_pin_works_without_versioning(tmp_path: Path, keys: Keys, cache: Path) -> None:
    """A plain bucket: every version id is ``null``. The hashes still decide."""
    st = MemoryStore(versioned=False)
    pub = publish(st, tmp_path, keys)
    assert {f.store_version_id for f in pub.m.files} == {store.NULL_VERSION}
    path = _lock_file(tmp_path)
    assert _pin(st, keys, cache, path) == pub.pin()
    # an overwrite of a weights file (no versions to tell apart) is a hash mismatch
    rec = pub.m.files[0]
    st.shadow(store.run_file_key(NAME, "1.0.0", RUN, rec.path), b"overwritten synthetic bytes")
    r = _reason(lambda: _pin(st, keys, tmp_path / "cache2", _lock_file(tmp_path, name="b.lock")))
    assert r.reason in ("hash_mismatch", "size_mismatch")


def test_pin_never_uses_verify_for_load(
    tmp_path: Path, keys: Keys, cache: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    st = MemoryStore()
    publish(st, tmp_path, keys)
    seen: list[str] = []
    real = verify.verify_pin

    def spy(*a: Any, **kw: Any) -> Any:
        seen.append(a[1])
        return real(*a, **kw)

    def forbidden(*a: Any, **kw: Any) -> Any:
        raise AssertionError("pin must not call verify_for_load")

    monkeypatch.setattr(verify, "verify_pin", spy)
    monkeypatch.setattr(verify, "verify_for_load", forbidden)
    _pin(st, keys, cache, _lock_file(tmp_path))
    assert seen == [ROLE]


def test_pin_refuses_failing_gate_fixture_and_rehearsal(
    tmp_path: Path, keys: Keys, cache: Path
) -> None:
    st = MemoryStore()
    path = _lock_file(tmp_path)
    before = path.read_bytes()

    # P14: fixture names and the rehearsal gate, before any store call
    publish(st, tmp_path, keys, version="0.9.0", gates={"rehearsal": "pass"})
    calls = len(st.calls)
    assert _reason(lambda: _pin(st, keys, cache, path, name="fixture-tiny")).reason == (
        "fixture_name"
    )
    r = _reason(lambda: _pin(st, keys, cache, path, version="0.9.0", gate="rehearsal"))
    assert (r.reason, r.detail_code) == ("gate_failed", "not_production")
    assert len(st.calls) == calls

    # the gate failed in the manifest
    publish(st, tmp_path, keys, version="1.0.1", gates={"phase1": "fail"})
    assert _reason(lambda: _pin(st, keys, cache, path, version="1.0.1")).reason == "gate_failed"
    # the gate the manifest doesn't carry
    assert _reason(lambda: _pin(st, keys, cache, path, version="1.0.1", gate="phase3")).reason == (
        "gate_failed"
    )
    # manifest and evaluation.json disagree
    publish(st, tmp_path, keys, version="1.0.2", report_verdicts={"phase1": "fail"})
    r = _reason(lambda: _pin(st, keys, cache, path, version="1.0.2"))
    assert (r.reason, r.detail_code) == ("gate_failed", "disagree")
    # measured on the dev split
    publish(st, tmp_path, keys, version="1.0.3", split="dev")
    r = _reason(lambda: _pin(st, keys, cache, path, version="1.0.3"))
    assert (r.reason, r.detail_code) == ("testset_mismatch", "split")
    # on a test set that isn't the locked one
    other = f"clinical_document@1.0.0#{'ee' * 32}"
    publish(st, tmp_path, keys, version="1.0.4", testset=other)
    r = _reason(lambda: _pin(st, keys, cache, path, version="1.0.4"))
    assert (r.reason, r.detail_code) == ("testset_mismatch", "not_locked")
    # against thresholds that aren't the committed file
    rep = report_bytes({"phase1": "pass"}, thresholds_sha="0" * 64)
    publish(st, tmp_path, keys, version="1.0.5", report_raw=rep)
    r = _reason(lambda: _pin(st, keys, cache, path, version="1.0.5"))
    assert (r.reason, r.detail_code) == ("gate_failed", "thresholds")

    assert path.read_bytes() == before  # nothing was written


def test_pin_refuses_unsigned_untrusted_or_tampered(
    tmp_path: Path, keys: Keys, cache: Path
) -> None:
    st = MemoryStore()
    path = _lock_file(tmp_path)
    before = path.read_bytes()
    publish(st, tmp_path, keys, version="1.0.0", sign=False)
    assert _reason(lambda: _pin(st, keys, cache, path, version="1.0.0")).reason == (
        "signature_invalid"
    )
    publish(st, tmp_path, keys, version="1.1.0", signer=MemSigner())
    assert _reason(lambda: _pin(st, keys, cache, path, version="1.1.0")).reason == (
        "signer_untrusted"
    )
    # a signature for the other purpose over the same manifest never verifies (P13)
    pub = publish(st, tmp_path, keys, version="1.1.1", sign=False)
    _put(st, store.manifest_sig_key(NAME, "1.1.1"), keys.manifest.sign("testset", pub.raw))
    assert _reason(lambda: _pin(st, keys, cache, path, version="1.1.1")).reason == (
        "signature_invalid"
    )
    # a file changed in the store after promotion
    pub = publish(st, tmp_path, keys, version="1.2.0")
    _tamper_file(st, pub)
    assert _reason(lambda: _pin(st, keys, cache, path, version="1.2.0")).reason in (
        "hash_mismatch",
        "size_mismatch",
    )
    # evaluation.json changed after promotion
    pub = publish(st, tmp_path, keys, version="1.3.0")
    doc = pub.m.documents["evaluation.json"]
    key = store.run_doc_key(NAME, "1.3.0", RUN, "evaluation.json")
    st.objects[key] = [(doc.store_version_id, report_bytes({"phase1": "pass"}, ts_sha="0" * 64))]
    assert _reason(lambda: _pin(st, keys, cache, path, version="1.3.0")).reason == ("hash_mismatch")
    assert path.read_bytes() == before


def test_pin_picks_the_signed_manifest_not_the_latest(
    tmp_path: Path, keys: Keys, cache: Path
) -> None:
    st = MemoryStore()
    pub = publish(st, tmp_path, keys)
    # a newer, unsigned manifest version under the same key (someone else's PUT)
    shadow = manifest.to_bytes(
        manifest.from_obj({**json.loads(pub.raw), "trained_by": "syn-someone-else"})
    )
    st.shadow(store.manifest_key(NAME, "1.0.0"), shadow)
    path = _lock_file(tmp_path)
    assert _pin(st, keys, cache, path).manifest_sha256 == pub.sha


def test_development_signer_refused_in_production(tmp_path: Path, cache: Path) -> None:
    dev = MemSigner()
    r = _reason(
        lambda: signing.parse_policy(
            signing.policy_obj([dev.trusted(development=True)]), environment=None
        )
    )
    assert (r.reason, r.detail_code) == ("policy_invalid", "development_signer_in_production")
    policy = signing.TrustPolicy((dev.trusted(development=True),))
    st = MemoryStore()
    publish(st, tmp_path, Keys(dev, policy))
    r = _reason(
        lambda: _check(
            st,
            Keys(dev, policy),
            cache,
            lock.empty(),
            lock.Lock({ROLE: lock.Pin(NAME, "1.0.0", "ab" * 32, "phase1")}),
        )
    )
    assert (r.reason, r.detail_code) == ("policy_invalid", "development_signer_in_production")


# ---------------------------------------------------------------- lock-check


def test_lock_check_new_version_passes(tmp_path: Path, keys: Keys, cache: Path) -> None:
    st = MemoryStore()
    pub = publish(st, tmp_path, keys)
    assert _check(st, keys, cache, lock.empty(), lock.Lock({ROLE: pub.pin()})) == {ROLE: "new"}
    # a later version of the same model is new too, and its evaluation is checked
    later = publish(st, tmp_path, keys, version="1.1.0", seed=8)
    base, head = lock.Lock({ROLE: pub.pin()}), lock.Lock({ROLE: later.pin()})
    assert _check(st, keys, cache, base, head) == {ROLE: "new"}


def test_lock_check_with_no_change_needs_no_store(keys: Keys, cache: Path) -> None:
    st = MemoryStore()
    same = lock.Lock({ROLE: lock.Pin(NAME, "1.0.0", "ab" * 32, "phase1")})
    assert _check(st, keys, cache, same, same) == {}
    assert _check(st, keys, cache, lock.empty(), lock.empty()) == {}
    assert st.calls == []


def test_lock_check_removed_role(keys: Keys, cache: Path) -> None:
    st = MemoryStore()
    base = lock.Lock({ROLE: lock.Pin(NAME, "1.0.0", "ab" * 32, "phase1")})
    assert _check(st, keys, cache, base, lock.empty()) == {ROLE: "removed"}


@pytest.mark.parametrize(
    ("kw", "reason", "detail"),
    [
        ({"split": "dev"}, "testset_mismatch", "split"),
        ({"testset": f"clinical_document@1.0.0#{'ee' * 32}"}, "testset_mismatch", "not_locked"),
        ({"report_verdicts": {"phase1": "fail"}}, "gate_failed", "disagree"),
        (
            {"report_raw": report_bytes({"phase1": "pass"}, thresholds_sha="0" * 64)},
            "gate_failed",
            "thresholds",
        ),
        (
            {"report_raw": report_bytes({"phase1": "pass"}, split="dev")},
            "testset_mismatch",
            "report_split",
        ),
        (
            {"report_raw": report_bytes({"phase1": "pass"}, ts_version="1.0.1")},
            "testset_mismatch",
            "report_testset",
        ),
    ],
)
def test_lock_check_new_version_needs_its_evaluation(
    kw: dict[str, Any], reason: str, detail: str, tmp_path: Path, keys: Keys, cache: Path
) -> None:
    st = MemoryStore()
    pub = publish(st, tmp_path, keys, **kw)
    r = _reason(lambda: _check(st, keys, cache, lock.empty(), lock.Lock({ROLE: pub.pin()})))
    assert (r.reason, r.detail_code) == (reason, detail)


def test_lock_check_refuses_tampered_unsigned_or_wrong_hash(
    tmp_path: Path, keys: Keys, cache: Path
) -> None:
    st = MemoryStore()
    empty = lock.empty()
    tampered = publish(st, tmp_path, keys, version="1.0.0")
    _tamper_file(st, tampered)
    r = _reason(lambda: _check(st, keys, cache, empty, lock.Lock({ROLE: tampered.pin()})))
    assert r.reason in ("hash_mismatch", "size_mismatch")
    unsigned = publish(st, tmp_path, keys, version="1.1.0", sign=False)
    r = _reason(lambda: _check(st, keys, cache, empty, lock.Lock({ROLE: unsigned.pin()})))
    assert r.reason == "signature_invalid"
    untrusted = publish(st, tmp_path, keys, version="1.2.0", signer=MemSigner())
    r = _reason(lambda: _check(st, keys, cache, empty, lock.Lock({ROLE: untrusted.pin()})))
    assert r.reason == "signer_untrusted"
    # a pin whose hash is not the stored manifest's
    good = publish(st, tmp_path, keys, version="1.3.0")
    wrong = lock.Pin(NAME, "1.3.0", "00" * 32, "phase1")
    r = _reason(lambda: _check(st, keys, cache, empty, lock.Lock({ROLE: wrong})))
    assert r.reason == "lock_mismatch"
    # the gate the manifest failed
    failed = publish(st, tmp_path, keys, version="1.4.0", gates={"phase1": "fail"})
    r = _reason(lambda: _check(st, keys, cache, empty, lock.Lock({ROLE: failed.pin()})))
    assert r.reason == "gate_failed"
    # a new key only main's policy doesn't list: the PR can't trust its own key
    other = MemSigner()
    signed_by_other = publish(st, tmp_path, keys, version="1.5.0", signer=other)
    head = lock.Lock({ROLE: signed_by_other.pin()})
    assert _reason(lambda: _check(st, keys, cache, empty, head)).reason == "signer_untrusted"
    both = signing.TrustPolicy((keys.manifest.trusted(), other.trusted()))
    assert _check(st, keys, cache, empty, head, policy=both) == {ROLE: "new"}
    assert good.sha  # the good version itself stays pinnable
    assert _check(st, keys, cache, empty, lock.Lock({ROLE: good.pin()})) == {ROLE: "new"}


def test_lock_check_rollback_is_verified_without_a_new_evaluation(
    tmp_path: Path, keys: Keys, cache: Path
) -> None:
    st = MemoryStore()
    # the older version was measured on a test split no longer in the lock: a new pin of
    # it would be refused, so passing proves no new evaluation is asked of a rollback
    old_split = f"clinical_document@1.0.0#{'ee' * 32}"
    old = publish(st, tmp_path, keys, version="1.0.0", testset=old_split)
    current = publish(st, tmp_path, keys, version="1.1.0", seed=8)
    base, head = lock.Lock({ROLE: current.pin()}), lock.Lock({ROLE: old.pin()})
    assert _check(st, keys, cache, base, head) == {ROLE: "rollback"}
    # the same version under another model's base pin is new, and its evaluation fails
    other_base = lock.Lock({ROLE: lock.Pin("syn-other-model", "9.0.0", "ab" * 32, "phase1")})
    r = _reason(lambda: _check(st, keys, cache, other_base, head))
    assert (r.reason, r.detail_code) == ("testset_mismatch", "not_locked")
    # a rollback measured on the dev split is refused
    dev = publish(st, tmp_path, keys, version="1.0.1", split="dev")
    r = _reason(lambda: _check(st, keys, cache, base, lock.Lock({ROLE: dev.pin()})))
    assert (r.reason, r.detail_code) == ("testset_mismatch", "split")
    # a rollback is still verified: a tampered file is refused
    _tamper_file(st, old)
    r = _reason(lambda: _check(st, keys, tmp_path / "cache2", base, head))
    assert r.reason in ("hash_mismatch", "size_mismatch")


def test_never_pinned_older_version_is_not_a_rollback(
    tmp_path: Path, keys: Keys, cache: Path
) -> None:
    """An older version main never pinned gets the full evaluation, not the rollback
    shortcut: here its test split isn't in main's testsets.lock, so it is refused."""
    st = MemoryStore()
    old_split = f"clinical_document@1.0.0#{'ee' * 32}"
    old = publish(st, tmp_path, keys, version="1.0.0", testset=old_split)
    current = publish(st, tmp_path, keys, version="1.1.0", seed=8)
    base, head = lock.Lock({ROLE: current.pin()}), lock.Lock({ROLE: old.pin()})
    other_sha = lock.Lock({ROLE: lock.Pin(NAME, "1.0.0", "ab" * 32, "phase1")})
    for history in ([], [base], [other_sha]):
        r = _reason(lambda h=history: _check(st, keys, cache, base, head, history=h))
        assert (r.reason, r.detail_code) == ("testset_mismatch", "not_locked")
    # pinned on main before (the exact entry): a rollback
    assert _check(st, keys, cache, base, head, history=[head, base]) == {ROLE: "rollback"}


def test_lock_check_rollback_needs_a_signed_passing_version(
    tmp_path: Path, keys: Keys, cache: Path
) -> None:
    st = MemoryStore()
    unsigned = publish(st, tmp_path, keys, version="1.0.0", sign=False)
    failed = publish(st, tmp_path, keys, version="1.0.1", gates={"phase1": "fail"}, seed=9)
    current = publish(st, tmp_path, keys, version="1.1.0", seed=8)
    base = lock.Lock({ROLE: current.pin()})
    r = _reason(lambda: _check(st, keys, cache, base, lock.Lock({ROLE: unsigned.pin()})))
    assert r.reason == "signature_invalid"
    r = _reason(lambda: _check(st, keys, cache, base, lock.Lock({ROLE: failed.pin()})))
    assert r.reason == "gate_failed"


def test_lock_check_dependencies_that_disagree_refused(
    tmp_path: Path, keys: Keys, cache: Path
) -> None:
    st = MemoryStore()
    # the model depends on another version of itself: one name, two versions
    dep = manifest.Dependency(NAME, "0.9.0", "ab" * 32)
    pub = publish(st, tmp_path, keys, dependencies=(dep,))
    r = _reason(lambda: _check(st, keys, cache, lock.empty(), lock.Lock({ROLE: pub.pin()})))
    assert (r.reason, r.detail_code) == ("lock_invalid", "dependencies")


def test_check_dependencies(tmp_path: Path, keys: Keys) -> None:
    st = MemoryStore()
    dep = manifest.Dependency("syn-base-model", "1.0.0", "ab" * 32)
    pub = publish(st, tmp_path, keys, dependencies=(dep,))
    same = lock.Pin("syn-base-model", "1.0.0", "ab" * 32, "phase1")
    lock.check_dependencies(lock.Lock({ROLE: pub.pin(), "ocr": same}), {ROLE: pub.m})
    differs = lock.Pin("syn-base-model", "1.1.0", "cd" * 32, "phase1")
    disagree = lock.Lock({ROLE: pub.pin(), "ocr": differs})
    r = _reason(lambda: lock.check_dependencies(disagree, {ROLE: pub.m}))
    assert (r.reason, r.detail_code) == ("lock_invalid", "dependencies")
    other_dep = manifest.Dependency("syn-base-model", "1.0.0", "cd" * 32)
    other = publish(st, tmp_path, keys, name="syn-other-model", dependencies=(other_dep,))
    two = lock.Lock({ROLE: pub.pin(), "ocr": other.pin()})
    r = _reason(lambda: lock.check_dependencies(two, {ROLE: pub.m, "ocr": other.m}))
    assert (r.reason, r.detail_code) == ("lock_invalid", "dependencies")


# ---------------------------------------------------------------- rollback


def _three_versions(tmp_path: Path, keys: Keys) -> tuple[MemoryStore, Pub, Pub, Pub, Path]:
    st = MemoryStore()
    v100 = publish(st, tmp_path, keys, version="1.0.0", seed=7)
    v110 = publish(st, tmp_path, keys, version="1.1.0", seed=8)
    v120 = publish(st, tmp_path, keys, version="1.2.0", seed=9)
    path = _lock_file(tmp_path, lock.Lock({ROLE: v120.pin()}))
    return st, v100, v110, v120, path


def _rollback(
    st: MemoryStore, keys: Keys, cache: Path, path: Path, to: str | None = None
) -> lock.RollbackResult:
    return lock.rollback(
        ROLE, source=st, policy=keys.policy, cache_dir=cache, lock_path=path, to=to
    )


def test_rollback_pins_the_latest_older_verified_version(
    tmp_path: Path, keys: Keys, cache: Path
) -> None:
    st, _, v110, v120, path = _three_versions(tmp_path, keys)
    done = _rollback(st, keys, cache, path)
    assert (done.previous, done.entry) == (v120.pin(), v110.pin())
    assert lock.read(path) == lock.Lock({ROLE: v110.pin()})
    # and lock-check takes the change as a rollback
    base = lock.Lock({ROLE: v120.pin()})
    assert _check(st, keys, cache, base, lock.read(path)) == {ROLE: "rollback"}


def test_rollback_to_a_named_version(tmp_path: Path, keys: Keys, cache: Path) -> None:
    st, v100, _, _, path = _three_versions(tmp_path, keys)
    assert _rollback(st, keys, cache, path, to="1.0.0").entry == v100.pin()
    # semver order, not string order: 1.10.0 is newer than 1.2.0, never a rollback
    st2, _, _, _, path2 = _three_versions(tmp_path / "b", keys)
    publish(st2, tmp_path / "b", keys, version="1.10.0", seed=10)
    before = path2.read_bytes()
    r = _reason(lambda: _rollback(st2, keys, cache, path2, to="1.10.0"))
    assert (r.reason, r.detail_code) == ("not_found", "not_a_candidate")
    r = _reason(lambda: _rollback(st2, keys, cache, path2, to="0.5.0"))
    assert (r.reason, r.detail_code) == ("not_found", "not_a_candidate")
    assert path2.read_bytes() == before


@pytest.mark.parametrize("damage", ["tampered", "unsigned", "untrusted", "gate_failed"])
def test_rollback_skips_versions_that_do_not_verify(
    damage: str, tmp_path: Path, keys: Keys, cache: Path
) -> None:
    st = MemoryStore()
    v100 = publish(st, tmp_path, keys, version="1.0.0", seed=7)
    kw: dict[str, Any] = {
        "unsigned": {"sign": False},
        "untrusted": {"signer": MemSigner()},
        "gate_failed": {"gates": {"phase1": "fail"}},
    }.get(damage, {})
    v110 = publish(st, tmp_path, keys, version="1.1.0", seed=8, **kw)
    if damage == "tampered":
        _tamper_file(st, v110)
    v120 = publish(st, tmp_path, keys, version="1.2.0", seed=9)
    path = _lock_file(tmp_path, lock.Lock({ROLE: v120.pin()}))
    assert _rollback(st, keys, cache, path).entry == v100.pin()
    r = _reason(
        lambda: _rollback(
            st,
            keys,
            cache,
            _lock_file(tmp_path, lock.Lock({ROLE: v120.pin()}), "b.lock"),
            to="1.1.0",
        )
    )
    assert (r.reason, r.detail_code) == ("not_found", "not_a_candidate")


def test_rollback_with_no_candidate_or_role_refused(
    tmp_path: Path, keys: Keys, cache: Path
) -> None:
    st = MemoryStore()
    only = publish(st, tmp_path, keys, version="1.0.0")
    path = _lock_file(tmp_path, lock.Lock({ROLE: only.pin()}))
    before = path.read_bytes()
    r = _reason(lambda: _rollback(st, keys, cache, path))
    assert (r.reason, r.detail_code) == ("not_found", "no_candidate")
    empty = _lock_file(tmp_path, name="empty.lock")
    r = _reason(lambda: _rollback(st, keys, cache, empty))
    assert (r.reason, r.detail_code) == ("lock_invalid", "role")
    assert path.read_bytes() == before


def test_rollback_store_outage_propagates(tmp_path: Path, keys: Keys, cache: Path) -> None:
    st, _, _, _, path = _three_versions(tmp_path, keys)
    real_get = st.get

    def down(key: str, version_id: str | None = None, *, max_bytes: int) -> bytes:
        if key.endswith("manifest.sig"):
            raise Refused("store_unavailable", detail_code="transport")
        return real_get(key, version_id, max_bytes=max_bytes)

    st.get = down  # type: ignore[method-assign]
    assert _reason(lambda: _rollback(st, keys, cache, path)).reason == "store_unavailable"


# ---------------------------------------------------------------- a promoted base


def test_promoted_base_verified_by_pin(tmp_path: Path, keys: Keys, cache: Path) -> None:
    st = MemoryStore()
    # a base passes no gate: none is needed for a base
    base = publish(st, tmp_path, keys, name="syn-base-model", gates={"phase1": "fail"})
    check = lock.base_verifier(st, keys.policy, cache)
    check(f"syn-base-model@1.0.0#{base.sha}")
    r = _reason(lambda: check(f"syn-base-model@1.0.0#{'00' * 32}"))
    assert (r.reason, r.detail_code) == ("base_unverified", "lock_mismatch")
    unsigned = publish(st, tmp_path, keys, name="syn-base-model", version="1.1.0", sign=False)
    r = _reason(lambda: check(f"syn-base-model@1.1.0#{unsigned.sha}"))
    assert (r.reason, r.detail_code) == ("base_unverified", "signature_invalid")
    assert _reason(lambda: check("not a pin")).reason == "base_unverified"


def test_promote_check_training_uses_the_base_verifier(
    tmp_path: Path, keys: Keys, cache: Path
) -> None:
    st = MemoryStore()
    base = publish(st, tmp_path, keys, name="syn-base-model")
    run_dir = fixtures.make_run(tmp_path / "run-child", "good", 3)
    tm_path = run_dir / training.FILE_NAME
    obj = json.loads(tm_path.read_text())
    obj["base_model"] = {"promoted": f"syn-base-model@1.0.0#{base.sha}"}
    tm_path.write_text(json.dumps(obj))
    registry = training.load_registry(REPO / "models" / "approved-datasets.json")
    licences = training.load_licences(REPO / "models" / "licences.json")
    # the default still refuses (fail closed)
    r = _reason(lambda: promote.check_training(run_dir, NAME, registry, licences))
    assert r.reason == "base_unverified"
    verifier = lock.base_verifier(st, keys.policy, cache)
    checked = promote.check_training(run_dir, NAME, registry, licences, verify_base=verifier)
    assert checked.licence is None
    obj["base_model"] = {"promoted": f"syn-base-model@1.0.0#{'00' * 32}"}
    tm_path.write_text(json.dumps(obj))
    r = _reason(
        lambda: promote.check_training(run_dir, NAME, registry, licences, verify_base=verifier)
    )
    assert (r.reason, r.detail_code) == ("base_unverified", "lock_mismatch")


def test_cli_base_verifier_built_only_when_used(monkeypatch: pytest.MonkeyPatch) -> None:
    """Making the verifier reads nothing; calling it without the reader's credentials is
    base_unverified (a promoted base is verified on a dry run too)."""
    import contextlib

    calls: list[str] = []
    monkeypatch.setattr(cli, "_reader_store", lambda: calls.append("store"))
    monkeypatch.setattr(cli, "_trust_policy", lambda path=None: calls.append("policy"))
    monkeypatch.delenv(cli.READER_ACCESS_ENV, raising=False)
    with contextlib.ExitStack() as stack:
        verifier = cli._base_verifier(stack)
        assert calls == []
        r = _reason(lambda: verifier(f"syn-base-model@1.0.0#{'ab' * 32}"))
        assert (r.reason, r.detail_code) == ("base_unverified", "reader_credentials")
        assert calls == []


# ---------------------------------------------------------------- the command line


def _cli_env(
    monkeypatch: pytest.MonkeyPatch, st: MemoryStore, tmp_path: Path, keys: Keys | None = None
) -> None:
    testsets_lock = tmp_path / "testsets.lock"
    testsets_lock.write_text(f"{TESTSET}\n")
    monkeypatch.setenv(cli.TESTSETS_LOCK_ENV, str(testsets_lock))
    monkeypatch.setattr(cli, "_reader_store", lambda: st)
    if keys is not None:
        monkeypatch.setattr(cli, "_trust_policy", lambda path=None: keys.policy)


def _policy_file(path: Path, *signers: MemSigner) -> Path:
    path.write_text(json.dumps(signing.policy_obj([s.trusted() for s in signers])))
    return path


def test_cli_pin_lock_check_and_rollback(
    tmp_path: Path, keys: Keys, monkeypatch: pytest.MonkeyPatch, capsys: Any
) -> None:
    st = MemoryStore()
    pub = publish(st, tmp_path, keys)
    publish(st, tmp_path, keys, version="2.0.0", gates={"phase1": "fail"}, seed=8)
    later = publish(st, tmp_path, keys, version="1.1.0", seed=9)
    _cli_env(monkeypatch, st, tmp_path, keys)
    path = _lock_file(tmp_path)
    main_lock = _lock_file(tmp_path, name="main.lock")

    assert cli.main(["pin", ROLE, NAME, "1.0.0", "--gate", "phase1", "--lock", str(path)]) == 0
    out = json.loads(capsys.readouterr().out)
    assert out == {"pinned": {"role": ROLE, **pub.pin().to_obj()}}
    assert lock.read(path).models[ROLE] == pub.pin()

    assert cli.main(["pin", ROLE, NAME, "2.0.0", "--gate", "phase1", "--lock", str(path)]) == 2
    assert json.loads(capsys.readouterr().out) == {"refused": "gate_failed"}
    assert cli.main(["pin", ROLE, "../x", "1.0.0", "--gate", "phase1", "--lock", str(path)]) == 2
    assert json.loads(capsys.readouterr().out) == {"refused": "invalid_name"}

    history = tmp_path / "history"
    history.write_bytes(main_lock.read_bytes())
    check = ["lock-check", "--base", str(main_lock), "--head", str(path)]
    check += ["--history", str(history)]
    assert cli.main(check) == 0
    assert json.loads(capsys.readouterr().out) == {"lock_check": "pass", "roles": {ROLE: "new"}}
    with pytest.raises(SystemExit):  # --base is required
        cli.main(["lock-check", "--head", str(path)])
    capsys.readouterr()

    # the PR that pinned 1.0.0 merged; a later PR moves to 1.1.0, then rolls back
    main_lock.write_bytes(path.read_bytes())
    history.write_bytes(history.read_bytes() + path.read_bytes())
    assert cli.main(["pin", ROLE, NAME, "1.1.0", "--gate", "phase1", "--lock", str(path)]) == 0
    capsys.readouterr()
    assert cli.main(check) == 0
    assert json.loads(capsys.readouterr().out)["roles"] == {ROLE: "new"}
    main_lock.write_bytes(path.read_bytes())
    history.write_bytes(history.read_bytes() + path.read_bytes())
    assert cli.main(["rollback", ROLE, "--lock", str(path)]) == 0
    out = json.loads(capsys.readouterr().out)
    assert out == {
        "rollback": {"role": ROLE, "from": later.pin().to_obj(), "to": pub.pin().to_obj()}
    }
    assert cli.main(check) == 0
    assert json.loads(capsys.readouterr().out)["roles"] == {ROLE: "rollback"}
    # without main's history the same change is a new pin, fully evaluated
    assert cli.main(check[:-2]) == 0
    assert json.loads(capsys.readouterr().out)["roles"] == {ROLE: "new"}
    assert cli.main(["rollback", ROLE, "--lock", str(path)]) == 2
    out = json.loads(capsys.readouterr().out)
    assert out == {"refused": "not_found", "detail": "no_candidate"}


def test_cli_lock_check_store_unavailable_is_exit_3(
    tmp_path: Path, keys: Keys, monkeypatch: pytest.MonkeyPatch, capsys: Any
) -> None:
    st = MemoryStore()
    pub = publish(st, tmp_path, keys)
    _cli_env(monkeypatch, st, tmp_path, keys)

    def down(*a: Any, **k: Any) -> Any:
        raise Refused("store_unavailable", detail_code="transport")

    st.versions = down  # type: ignore[method-assign]
    head = _lock_file(tmp_path, lock.Lock({ROLE: pub.pin()}))
    base = _lock_file(tmp_path, name="main.lock")
    assert cli.main(["lock-check", "--base", str(base), "--head", str(head)]) == 3
    out = json.loads(capsys.readouterr().out)
    assert out == {"refused": "store_unavailable", "detail": "transport"}


def test_cli_lock_check_trust_policy_flag_wins(
    tmp_path: Path, keys: Keys, monkeypatch: pytest.MonkeyPatch, capsys: Any
) -> None:
    """The workflow passes main's policy with --trust-policy; a policy that the PR could
    change (MHAI_MODELS_TRUST_POLICY here) does not decide then."""
    st = MemoryStore()
    pub = publish(st, tmp_path, keys)
    _cli_env(monkeypatch, st, tmp_path)  # the real _trust_policy
    mains = _policy_file(tmp_path / "main-policy.json", keys.manifest)
    prs = _policy_file(tmp_path / "pr-policy.json")  # trusts nobody
    monkeypatch.setenv(cli.TRUST_POLICY_ENV, str(prs))
    head = _lock_file(tmp_path, lock.Lock({ROLE: pub.pin()}))
    base = _lock_file(tmp_path, name="main.lock")
    check = ["lock-check", "--base", str(base), "--head", str(head)]
    assert cli.main(check) == 2
    assert json.loads(capsys.readouterr().out) == {"refused": "signer_untrusted"}
    assert cli.main([*check, "--trust-policy", str(mains)]) == 0
    assert json.loads(capsys.readouterr().out) == {"lock_check": "pass", "roles": {ROLE: "new"}}
    # and the reverse: main's policy without the PR's new key refuses the PR's pin
    newkey = MemSigner()
    pub2 = publish(st, tmp_path, keys, version="1.1.0", seed=8, signer=newkey)
    head.write_bytes(lock.to_bytes(lock.Lock({ROLE: pub2.pin()})))
    _policy_file(prs, keys.manifest, newkey)
    assert cli.main([*check, "--trust-policy", str(mains)]) == 2
    assert json.loads(capsys.readouterr().out) == {"refused": "signer_untrusted"}


def test_cli_lock_check_without_a_change_needs_no_store_or_policy(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, capsys: Any
) -> None:
    """lock-check runs on every PR: one that changes no role passes at once, with no store
    credentials, no trust policy and no test-set lock read."""

    def forbidden(*a: Any, **k: Any) -> Any:
        raise AssertionError("no store or policy access for a PR without a lock change")

    monkeypatch.setattr(cli, "_reader_store", forbidden)
    monkeypatch.setattr(cli, "_trust_policy", forbidden)
    monkeypatch.setattr(cli, "_testsets_lock", forbidden)
    for var in (cli.READER_ACCESS_ENV, cli.READER_SECRET_ENV, cli.ENDPOINT_ENV):
        monkeypatch.delenv(var, raising=False)
    same = lock.Lock({ROLE: lock.Pin(NAME, "1.0.0", "ab" * 32, "phase1")})
    base = _lock_file(tmp_path, same, "main.lock")
    head = _lock_file(tmp_path, same)
    assert cli.main(["lock-check", "--base", str(base), "--head", str(head)]) == 0
    assert json.loads(capsys.readouterr().out) == {"lock_check": "pass", "roles": {}}
    empty = _lock_file(tmp_path, name="empty.lock")
    assert cli.main(["lock-check", "--base", str(empty), "--head", str(empty)]) == 0
    assert json.loads(capsys.readouterr().out) == {"lock_check": "pass", "roles": {}}
    # a non-production pin is refused even when unchanged, still with no I/O
    fixture = _lock_file(
        tmp_path,
        lock.Lock({ROLE: lock.Pin("fixture-tiny", "1.0.0", "ab" * 32, "phase1")}),
        "f.lock",
    )
    assert cli.main(["lock-check", "--base", str(fixture), "--head", str(fixture)]) == 2
    assert json.loads(capsys.readouterr().out) == {"refused": "fixture_name"}


def test_cli_lock_check_without_reader_credentials_refused(
    tmp_path: Path, keys: Keys, monkeypatch: pytest.MonkeyPatch, capsys: Any
) -> None:
    """A fork PR gets no secrets: a pin is refused, never passed unchecked."""
    for var in (cli.READER_ACCESS_ENV, cli.READER_SECRET_ENV):
        monkeypatch.delenv(var, raising=False)
    base = _lock_file(tmp_path, name="main.lock")
    head = _lock_file(tmp_path, lock.Lock({ROLE: lock.Pin(NAME, "1.0.0", "ab" * 32, "phase1")}))
    assert cli.main(["lock-check", "--base", str(base), "--head", str(head)]) == 2
    out = json.loads(capsys.readouterr().out)
    assert out == {"refused": "not_allowed_here", "detail": "reader_credentials"}


@pytest.mark.parametrize("command", ["lock-check", "pin", "rollback"])
def test_cli_rehearsal_flag_refused_outside_development(
    command: str, tmp_path: Path, monkeypatch: pytest.MonkeyPatch, capsys: Any
) -> None:
    _dev(monkeypatch, False)
    head = _lock_file(tmp_path)
    called: list[str] = []
    monkeypatch.setattr(cli, "_reader_store", lambda: called.append("store"))
    if command == "lock-check":
        args = ["lock-check", "--base", str(head), "--head", str(head), "--rehearsal"]
    elif command == "pin":
        args = ["pin", ROLE, NAME, "1.0.0", "--gate", "phase1", "--lock", str(head)]
        args.append("--rehearsal")
    else:
        args = ["rollback", ROLE, "--lock", str(head), "--rehearsal"]
    assert cli.main(args) == 2
    assert json.loads(capsys.readouterr().out) == {
        "refused": "not_allowed_here",
        "detail": "rehearsal",
    }
    assert called == []


def test_cli_pin_rehearsal_needs_an_explicit_lock(
    monkeypatch: pytest.MonkeyPatch, capsys: Any
) -> None:
    _dev(monkeypatch, True)
    assert verify.is_development()
    called: list[str] = []
    monkeypatch.setattr(cli, "_reader_store", lambda: called.append("store"))
    assert cli.main(["pin", ROLE, NAME, "1.0.0", "--gate", "phase1", "--rehearsal"]) == 2
    out = json.loads(capsys.readouterr().out)
    assert out == {"refused": "not_allowed_here", "detail": "rehearsal"}
    assert called == []


def test_cli_rehearsal_in_development(
    tmp_path: Path, keys: Keys, monkeypatch: pytest.MonkeyPatch, capsys: Any
) -> None:
    _dev(monkeypatch, True)
    st = MemoryStore()
    pub = publish(st, tmp_path, keys, name="fixture-tiny", gates={"rehearsal": "pass"})
    _cli_env(monkeypatch, st, tmp_path, keys)
    path = _lock_file(tmp_path)
    base = _lock_file(tmp_path, name="main.lock")
    pin = ["pin", ROLE, "fixture-tiny", "1.0.0", "--gate", "rehearsal", "--lock", str(path)]
    assert cli.main([*pin, "--rehearsal"]) == 0
    assert json.loads(capsys.readouterr().out)["pinned"]["manifest_sha256"] == pub.sha
    check = ["lock-check", "--base", str(base), "--head", str(path)]
    assert cli.main(check) == 2  # production is the default, in development too
    assert json.loads(capsys.readouterr().out) == {"refused": "fixture_name"}
    assert cli.main([*check, "--rehearsal"]) == 0
    assert json.loads(capsys.readouterr().out) == {"lock_check": "pass", "roles": {ROLE: "new"}}


def test_cli_keygen_prints_the_policy_entry_only(tmp_path: Path, capsys: Any) -> None:
    assert cli.main(["keygen", str(tmp_path / "k")]) == 0
    out = json.loads(capsys.readouterr().out)
    entry = out["trust_policy_signer"]
    assert set(entry) == {"alg", "development", "key_id", "public_key_pem"}
    assert entry["development"] is False and "PRIVATE" not in json.dumps(out)
    policy = signing.parse_policy(
        {"schema": signing.TRUST_SCHEMA, "signers": [entry]}, environment=None
    )
    signer = signing.FileSigner(tmp_path / "k" / signing.KEY_FILE)
    sig = signer.sign("manifest", b"synthetic payload")
    assert (
        signing.Verifier().verify("manifest", b"synthetic payload", sig, policy).key_id
        == (entry["key_id"])
    )
    assert cli.main(["keygen", str(tmp_path / "k")]) == 2  # never overwrites
    assert json.loads(capsys.readouterr().out) == {"refused": "key_exists"}


def test_cli_make_fixture_takes_a_name(tmp_path: Path, capsys: Any) -> None:
    assert cli.main(["make-fixture", str(tmp_path / "a"), "--name", "fixture-tiny"]) == 0
    capsys.readouterr()
    tm = json.loads((tmp_path / "a" / "training_manifest.json").read_text())
    assert tm["name"] == "fixture-tiny"
    assert cli.main(["make-fixture", str(tmp_path / "b"), "--name", "../x"]) == 2
    assert json.loads(capsys.readouterr().out)["refused"] == "invalid_name"
    assert not (tmp_path / "b").exists()

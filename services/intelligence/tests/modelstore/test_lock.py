"""``models.lock``, ``pin`` and ``lock-check`` (plan Task 20): gates, label, separation of
duties, rollback rules, the GitHub client and the promoted-base verifier.

Every model is a tiny synthetic safetensors run (``fixtures.make_run``), keys are made in
memory, the store is the in-memory fake and GitHub is the fake session in
``fakes/github_api.py``: no test opens a socket outside loopback (the guard from
``test_sources``). Every login is synthetic.
"""

from __future__ import annotations

import hashlib
import json
import logging
import os
import subprocess
from collections.abc import Callable
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import pytest

from intelligence.doceval import report
from intelligence.modelstore import (
    canonical,
    cli,
    fixtures,
    lock,
    manifest,
    promote,
    signing,
    signoff,
    store,
    testsets,
    training,
    verify,
)
from intelligence.modelstore.reasons import Refused
from tests.modelstore.fakes.github_api import NULL, FakeGitHubSession, FakePR
from tests.modelstore.fakes.github_api import REPO as GH_REPO
from tests.modelstore.fakes.memory_store import MemoryStore
from tests.modelstore.test_sources import no_network  # noqa: F401  (autouse: no network)
from tests.modelstore.test_verify import MemSigner, _put

REPO = Path(__file__).resolve().parents[4]
NAME = fixtures.MODEL_NAME
ROLE = fixtures.ROLE
RUN = "run-lock-1"
ACTOR = "syn-promoter"
TRAINER = fixtures.TRAINED_BY
CLINICAL = "syn-clinical-1"
PLATFORM = "syn-platform-1"
AUTHOR = "syn-author"
HEAD = "a1" * 20
OLD_COMMIT = "b2" * 20
TESTSET_SHA = hashlib.sha256(b"synthetic locked split").hexdigest()
TESTSET = f"clinical_document@1.0.0#{TESTSET_SHA}"
LETTERS = "restoring the signed off synthetic version"
TESTSETS = testsets.parse_lock(f"{TESTSET}\n".encode())
REVIEWERS = lock.Reviewers(frozenset({CLINICAL}), frozenset({PLATFORM}))
# D35: what a PR or merge that changes only the lock looks like from git.
LOCK_ONLY = ("models.lock",)


# ---------------------------------------------------------------- keys


@dataclass(frozen=True)
class Keys:
    manifest: MemSigner
    signoff: MemSigner
    policy: signing.TrustPolicy


@pytest.fixture
def keys() -> Keys:
    m, s = MemSigner(), MemSigner()
    policy = signing.TrustPolicy(
        {"manifest": (m.trusted(),), "signoff": (s.trusted(),), "testset": ()}
    )
    return Keys(m, s, policy)


@pytest.fixture(autouse=True)
def _production(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv(signing.ENV_VAR, raising=False)
    monkeypatch.delenv(verify.APP_ENV, raising=False)


@pytest.fixture
def cache(tmp_path: Path) -> Path:
    c = tmp_path / "cache"
    c.mkdir(mode=0o700)
    return c


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


def write_signoff(
    target: MemoryStore, pub: Pub, keys: Keys, *, gate: str = "phase1", role: str = ROLE
) -> None:
    s = signoff.Signoff(
        name=pub.m.name,
        version=pub.m.version,
        role=role,
        gate=gate,
        manifest_sha256=pub.sha,
        evaluation_sha256=pub.m.evaluation.report_sha256,
        pr=17,
        merge_commit="ef" * 20,
        clinical_reviewers=(CLINICAL,),
        platform_reviewers=(PLATFORM,),
        promotion_actor=pub.m.promotion.actor,
        trained_by=pub.m.trained_by,
        at="2026-11-06T01:00:00Z",
    )
    body = signoff.to_bytes(s)
    _put(target, store.signoff_key(pub.m.name, pub.m.version), body)
    sig = keys.signoff.sign("signoff", body)
    _put(target, store.signoff_sig_key(pub.m.name, pub.m.version), sig)


def _reason(fn: Callable[[], Any]) -> Refused:
    with pytest.raises(Refused) as e:
        fn()
    return e.value


def _lock_file(tmp_path: Path, lk: lock.Lock | None = None) -> Path:
    path = tmp_path / "models.lock"
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


# ---------------------------------------------------------------- the lock file


def test_committed_lock_has_no_roles_and_reviewers_are_empty() -> None:
    committed = lock.read(REPO / "models.lock")
    assert dict(committed.models) == {}
    assert (REPO / "models.lock").read_bytes() == lock.to_bytes(lock.empty())
    reviewers = lock.read_reviewers(REPO / "models" / "reviewers.json")
    assert reviewers == lock.Reviewers(frozenset(), frozenset())


def test_lock_round_trip_canonical() -> None:
    lk = lock.Lock({ROLE: lock.Pin(NAME, "1.2.3", "ab" * 32, "phase1")})
    raw = lock.to_bytes(lk)
    assert raw.endswith(b"\n") and canonical.is_canonical(raw[:-1])
    assert lock.parse(raw) == lk


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
def test_malformed_lock_refused(raw: bytes, tmp_path: Path, keys: Keys, cache: Path) -> None:
    assert _reason(lambda: lock.parse(raw)).reason == "lock_invalid"
    good = lock.to_bytes(lock.empty())
    st = MemoryStore()
    session = FakeGitHubSession()
    gh = lock.GitHubClient(session, GH_REPO)
    for base, head in ((good, raw), (raw, good)):
        with pytest.raises(Refused) as e:
            lock.lock_check(
                base,
                head,
                7,
                github=gh,
                reviewers=REVIEWERS,
                source=st,
                policy=keys.policy,
                cache_dir=cache,
                testsets_lock=TESTSETS,
                thresholds_dir=report.THRESHOLDS_DIR,
                head_sha=HEAD,
                changed_paths=LOCK_ONLY,
            )
        assert e.value.reason == "lock_invalid"
    assert st.calls == [] and session.requests == []  # refused before any I/O


@pytest.mark.parametrize(
    ("entry", "reason"),
    [
        (lock.Pin("fixture-tiny", "1.0.0", "ab" * 32, "phase1"), "fixture_name"),
        (lock.Pin(NAME, "1.0.0", "ab" * 32, "rehearsal"), "gate_failed"),
    ],
)
def test_lock_check_refuses_fixture_and_rehearsal(
    entry: lock.Pin, reason: str, keys: Keys, cache: Path
) -> None:
    st, session = MemoryStore(), FakeGitHubSession()
    head = lock.to_bytes(lock.Lock({ROLE: entry}))
    refused = _reason(
        lambda: lock.lock_check(
            lock.to_bytes(lock.empty()),
            head,
            7,
            github=lock.GitHubClient(session, GH_REPO),
            reviewers=REVIEWERS,
            source=st,
            policy=keys.policy,
            cache_dir=cache,
            testsets_lock=TESTSETS,
            thresholds_dir=report.THRESHOLDS_DIR,
            head_sha=HEAD,
            changed_paths=LOCK_ONLY,
        )
    )
    assert refused.reason == reason
    assert st.calls == [] and session.requests == []


def test_lock_check_refuses_unknown_role(keys: Keys, cache: Path) -> None:
    head = lock.to_bytes(lock.Lock({"ocr": lock.Pin(NAME, "1.0.0", "ab" * 32, "phase1")}))
    refused = _reason(
        lambda: lock.lock_check(
            lock.to_bytes(lock.empty()),
            head,
            7,
            github=lock.GitHubClient(FakeGitHubSession(), GH_REPO),
            reviewers=REVIEWERS,
            source=MemoryStore(),
            policy=keys.policy,
            cache_dir=cache,
            testsets_lock=TESTSETS,
            thresholds_dir=report.THRESHOLDS_DIR,
            head_sha=HEAD,
            changed_paths=LOCK_ONLY,
        )
    )
    assert (refused.reason, refused.detail_code) == ("lock_invalid", "role")


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
    # no sign-off was needed (the version has none) and nothing was written to the store
    assert len(st.puts) == puts


def test_pin_never_uses_verify_for_load(
    tmp_path: Path, keys: Keys, cache: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    st = MemoryStore()
    publish(st, tmp_path, keys)
    seen: list[bool] = []
    real = verify.verify_pin

    def spy(*a: Any, **kw: Any) -> Any:
        seen.append(kw["require_signoff"])
        return real(*a, **kw)

    def forbidden(*a: Any, **kw: Any) -> Any:
        raise AssertionError("pin must not call verify_for_load")

    monkeypatch.setattr(verify, "verify_pin", spy)
    monkeypatch.setattr(verify, "verify_for_load", forbidden)
    _pin(st, keys, cache, _lock_file(tmp_path))
    assert seen == [False]


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


def test_pin_refuses_unsigned_or_tampered(tmp_path: Path, keys: Keys, cache: Path) -> None:
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
    # a file changed in the store after promotion
    pub = publish(st, tmp_path, keys, version="1.2.0")
    rec = pub.m.files[0]
    key = store.run_file_key(NAME, "1.2.0", RUN, rec.path)
    st.objects[key] = [(rec.store_version_id, b"tampered synthetic bytes")]
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


# ---------------------------------------------------------------- lock-check


def _pr(**kw: Any) -> FakePR:
    pr = FakePR(
        7,
        "Syn-Author",  # GitHub's display case
        HEAD,
        labels=kw.pop("labels", ["clinical-review"]),
        body=kw.pop("body", "Before-and-after report attached."),
    )
    for login, state, commit in kw.pop("reviews", []):
        pr.review(login, state, commit)
    assert not kw
    return pr


def _approved_pr(**kw: Any) -> FakePR:
    reviews = [("Syn-Clinical-1", "APPROVED", None), ("SYN-PLATFORM-1", "APPROVED", None)]
    return _pr(reviews=reviews, **kw)


def _check(
    st: MemoryStore,
    keys: Keys,
    cache: Path,
    base: lock.Lock,
    head: lock.Lock,
    pr: FakePR,
    reviewers: lock.Reviewers = REVIEWERS,
    session: FakeGitHubSession | None = None,
) -> dict[str, str]:
    session = session or FakeGitHubSession()
    session.add(pr)
    return lock.lock_check(
        lock.to_bytes(base),
        lock.to_bytes(head),
        pr.number,
        github=lock.GitHubClient(session, GH_REPO),
        reviewers=reviewers,
        source=st,
        policy=keys.policy,
        cache_dir=cache,
        testsets_lock=TESTSETS,
        thresholds_dir=report.THRESHOLDS_DIR,
        head_sha=HEAD,
        changed_paths=LOCK_ONLY,
    )


def _new(tmp_path: Path, keys: Keys, **kw: Any) -> tuple[MemoryStore, Pub, lock.Lock]:
    st = MemoryStore()
    pub = publish(st, tmp_path, keys, **kw)
    return st, pub, lock.Lock({ROLE: pub.pin()})


def test_lock_check_new_version_passes(tmp_path: Path, keys: Keys, cache: Path) -> None:
    st, _, head = _new(tmp_path, keys)
    session = FakeGitHubSession()
    result = _check(st, keys, cache, lock.empty(), head, _approved_pr(), session=session)
    assert result == {ROLE: "new"}
    assert all(r.allow_redirects is False for r in session.requests)


def test_lock_check_refuses_a_moved_head(tmp_path: Path, keys: Keys, cache: Path) -> None:
    st, _, head = _new(tmp_path, keys)
    session = FakeGitHubSession()
    session.add(_approved_pr())

    def run(head_sha: str) -> dict[str, str]:
        return lock.lock_check(
            lock.to_bytes(lock.empty()),
            lock.to_bytes(head),
            7,
            github=lock.GitHubClient(session, GH_REPO),
            reviewers=REVIEWERS,
            source=st,
            policy=keys.policy,
            cache_dir=cache,
            testsets_lock=TESTSETS,
            thresholds_dir=report.THRESHOLDS_DIR,
            head_sha=head_sha,
            changed_paths=LOCK_ONLY,
        )

    assert run(HEAD) == {ROLE: "new"}
    r = _reason(lambda: run(OLD_COMMIT))
    assert (r.reason, r.detail_code) == ("not_approved", "head_moved")


def test_lock_check_with_no_change_needs_nothing(tmp_path: Path, keys: Keys, cache: Path) -> None:
    st = MemoryStore()
    session = FakeGitHubSession()
    same = lock.Lock({ROLE: lock.Pin(NAME, "1.0.0", "ab" * 32, "phase1")})
    assert _check(st, keys, cache, same, same, _pr(), session=session) == {}
    assert st.calls == [] and session.requests == []


def test_lock_check_needs_label(tmp_path: Path, keys: Keys, cache: Path) -> None:
    st, _, head = _new(tmp_path, keys)
    pr = _approved_pr(labels=["needs-review"])
    assert _reason(lambda: _check(st, keys, cache, lock.empty(), head, pr)).reason == (
        "label_missing"
    )


def test_lock_check_needs_new_evaluation(tmp_path: Path, keys: Keys, cache: Path) -> None:
    st, _, head = _new(tmp_path, keys, split="dev")
    r = _reason(lambda: _check(st, keys, cache, lock.empty(), head, _approved_pr()))
    assert r.reason == "testset_mismatch"
    (tmp_path / "b").mkdir()
    st, _, head = _new(tmp_path / "b", keys, sign=False)
    r = _reason(lambda: _check(st, keys, cache, lock.empty(), head, _approved_pr()))
    assert r.reason == "signature_invalid"


@pytest.mark.parametrize(
    "reviews",
    [
        # the clinical approval is on an older commit
        [("syn-clinical-1", "APPROVED", OLD_COMMIT), ("syn-platform-1", "APPROVED", None)],
        # no platform approval
        [("syn-clinical-1", "APPROVED", None)],
        # no clinical approval
        [("syn-platform-1", "APPROVED", None)],
        # a later "changes requested" cancels the approval
        [
            ("syn-clinical-1", "APPROVED", None),
            ("syn-platform-1", "APPROVED", None),
            ("syn-platform-1", "CHANGES_REQUESTED", None),
        ],
        # a dismissed approval
        [("syn-clinical-1", "APPROVED", None), ("syn-platform-1", "DISMISSED", None)],
        # an approval only from someone not on the base list
        [("syn-clinical-1", "APPROVED", None), ("syn-stranger", "APPROVED", None)],
        # comments only
        [("syn-clinical-1", "COMMENTED", None), ("syn-platform-1", "COMMENTED", None)],
    ],
)
def test_lock_check_needs_clinical_and_platform_on_head_commit(
    reviews: list[tuple[str, str, str | None]], tmp_path: Path, keys: Keys, cache: Path
) -> None:
    st, _, head = _new(tmp_path, keys)
    r = _reason(lambda: _check(st, keys, cache, lock.empty(), head, _pr(reviews=reviews)))
    assert r.reason == "not_approved"


def test_lock_check_comment_after_approval_keeps_it(
    tmp_path: Path, keys: Keys, cache: Path
) -> None:
    st, _, head = _new(tmp_path, keys)
    pr = _pr(
        reviews=[
            ("syn-platform-1", "CHANGES_REQUESTED", OLD_COMMIT),
            ("syn-clinical-1", "APPROVED", None),
            ("syn-platform-1", "APPROVED", None),
            ("syn-platform-1", "COMMENTED", None),
        ]
    )
    assert _check(st, keys, cache, lock.empty(), head, pr) == {ROLE: "new"}


@pytest.mark.parametrize(
    ("reviewers", "reviews"),
    [
        # the promoter approves as the clinical reviewer
        (
            lock.Reviewers(frozenset({ACTOR}), frozenset({PLATFORM})),
            [(ACTOR, "APPROVED", None), (PLATFORM, "APPROVED", None)],
        ),
        # the trainer approves as the platform reviewer
        (
            lock.Reviewers(frozenset({CLINICAL}), frozenset({TRAINER})),
            [(CLINICAL, "APPROVED", None), (TRAINER, "APPROVED", None)],
        ),
        # the PR author's approval never counts
        (
            lock.Reviewers(frozenset({CLINICAL}), frozenset({AUTHOR})),
            [(CLINICAL, "APPROVED", None), ("Syn-Author", "APPROVED", None)],
        ),
        # one person on both lists can't be both reviewers
        (
            lock.Reviewers(frozenset({CLINICAL}), frozenset({CLINICAL})),
            [(CLINICAL, "APPROVED", None)],
        ),
    ],
)
def test_promoter_or_trainer_cannot_be_the_approver(
    reviewers: lock.Reviewers,
    reviews: list[tuple[str, str, str | None]],
    tmp_path: Path,
    keys: Keys,
    cache: Path,
) -> None:
    st, _, head = _new(tmp_path, keys)
    pr = _pr(reviews=reviews)
    r = _reason(lambda: _check(st, keys, cache, lock.empty(), head, pr, reviewers))
    assert r.reason == "separation_of_duties"


THRESHOLDS_REL = "services/intelligence/intelligence/doceval/thresholds"


def _main_checkout(
    root: Path,
    *,
    base: lock.Lock | None = None,
    reviewers: dict[str, list[str]] | None = None,
    testsets_text: str = f"{TESTSET}\n",
    thresholds: dict[str, bytes] | None = None,
) -> Path:
    """A checkout of main's tip, as model-lock.yml makes it (``--base-dir``)."""
    (root / "models").mkdir(parents=True)
    (root / "models.lock").write_bytes(lock.to_bytes(base or lock.empty()))
    default = {"clinical": [CLINICAL], "platform": [PLATFORM]}
    (root / "models" / "reviewers.json").write_text(json.dumps(reviewers or default))
    (root / "models" / "testsets.lock").write_text(testsets_text)
    th = root / THRESHOLDS_REL
    th.mkdir(parents=True)
    for g in report.GATES:
        (th / f"{g}.json").write_bytes((report.THRESHOLDS_DIR / f"{g}.json").read_bytes())
    for name, raw in (thresholds or {}).items():
        (th / name).write_bytes(raw)
    return root


def _lock_check_args(main: Path, head_file: Path, head_sha: str = HEAD) -> list[str]:
    return [
        "lock-check",
        "--base-dir",
        str(main),
        "--head",
        str(head_file),
        "--head-sha",
        head_sha,
        "--pr",
        "7",
        "--testsets",
        str(main / "models" / "testsets.lock"),
        "--thresholds-dir",
        str(main / THRESHOLDS_REL),
    ]


def test_reviewers_read_from_main_not_head(
    tmp_path: Path, keys: Keys, cache: Path, monkeypatch: pytest.MonkeyPatch, capsys: Any
) -> None:
    """The PR adds its own approver to reviewers.json; lock-check reads main's copy."""
    st, _, head = _new(tmp_path, keys)
    main = _main_checkout(tmp_path / "main", reviewers={"clinical": [CLINICAL], "platform": []})
    head_dir = _main_checkout(
        tmp_path / "head",
        base=head,
        reviewers={"clinical": [CLINICAL], "platform": ["syn-newcomer"]},
    )
    pr = _pr(reviews=[(CLINICAL, "APPROVED", None), ("syn-newcomer", "APPROVED", None)])
    session = FakeGitHubSession()
    session.add(pr)
    _cli_env(monkeypatch, st, keys, session, tmp_path)
    assert cli.main(_lock_check_args(main, head_dir / "models.lock")) == 2
    assert json.loads(capsys.readouterr().out) == {"refused": "not_approved", "detail": "approvals"}
    # with the head copy it would have passed: main's copy is what makes the difference
    reviewers = lock.read_reviewers(head_dir / "models" / "reviewers.json")
    again = _pr(reviews=[(CLINICAL, "APPROVED", None), ("syn-newcomer", "APPROVED", None)])
    assert _check(st, keys, cache, lock.empty(), head, again, reviewers) == {ROLE: "new"}


def test_pr_cannot_lower_a_threshold_or_add_a_testset_with_its_pin(
    tmp_path: Path, keys: Keys, monkeypatch: pytest.MonkeyPatch, capsys: Any
) -> None:
    """In one PR: a lowered phase1 threshold, or a new testsets.lock entry, plus a pin of a
    version measured against them. lock-check reads main's copies only, and refuses."""
    st = MemoryStore()
    lowered_obj = json.loads((report.THRESHOLDS_DIR / "phase1.json").read_bytes())
    lowered_obj["source"] = "synthetic lowered bar"
    lowered = canonical.dumps(lowered_obj)
    rep = report_bytes({"phase1": "pass"}, thresholds_sha=hashlib.sha256(lowered).hexdigest())
    v1 = publish(st, tmp_path, keys, version="1.0.0", report_raw=rep)
    new_set = f"clinical_document@1.1.0#{'ee' * 32}"
    rep2 = report_bytes({"phase1": "pass"}, ts_sha="ee" * 32, ts_version="1.1.0")
    v2 = publish(st, tmp_path, keys, version="1.1.0", seed=8, testset=new_set, report_raw=rep2)
    main = _main_checkout(tmp_path / "main")
    session = FakeGitHubSession()
    session.add(_approved_pr())
    _cli_env(monkeypatch, st, keys, session, tmp_path)
    for pub, kind, detail in (
        (v1, "threshold", ("gate_failed", "thresholds")),
        (v2, "testset", ("testset_mismatch", "not_locked")),
    ):
        head_dir = _main_checkout(
            tmp_path / f"head-{kind}",
            base=lock.Lock({ROLE: pub.pin()}),
            testsets_text=f"{TESTSET}\n{new_set}\n",
            thresholds={"phase1.json": lowered} if kind == "threshold" else None,
        )
        assert cli.main(_lock_check_args(main, head_dir / "models.lock")) == 2
        out = json.loads(capsys.readouterr().out)
        assert (out["refused"], out["detail"]) == detail
        # the head's own copies would have accepted it: they are never read
        lock.check_evaluation(
            st,
            pub.m,
            "phase1",
            testsets.read_lock(head_dir / "models" / "testsets.lock"),
            head_dir / THRESHOLDS_REL,
        )
    # inputs outside main's checkout are refused: an absolute path, a ".." path, and a
    # symlink inside the checkout that points out of it
    head_dir = tmp_path / "head-threshold"
    link = main / "models" / "escape"
    link.symlink_to(head_dir / THRESHOLDS_REL, target_is_directory=True)
    (main / "models" / "testsets-link.lock").symlink_to(head_dir / "models" / "testsets.lock")
    for flag, value in (
        ("--thresholds-dir", str(head_dir / THRESHOLDS_REL)),
        ("--thresholds-dir", f"{main}/../head-threshold/{THRESHOLDS_REL}"),
        ("--thresholds-dir", str(link)),
        ("--testsets", f"{main}/models/../../head-threshold/models/testsets.lock"),
        ("--testsets", str(main / "models" / "testsets-link.lock")),
    ):
        args = _lock_check_args(main, head_dir / "models.lock")
        args[args.index(flag) + 1] = value
        assert cli.main(args) == 2
        assert json.loads(capsys.readouterr().out) == {
            "refused": "not_allowed_here",
            "detail": "outside_base_dir",
        }


def _policy_obj(manifest_signers: list[MemSigner], signoff_signers: list[MemSigner]) -> bytes:
    def entry(k: MemSigner) -> dict[str, Any]:
        return {
            "kind": "public_key",
            "alg": "ed25519",
            "key_id": k.key_id,
            "public_key_pem": k.pem(),
            "development": False,
        }

    obj = {
        "schema": "mhai.models.trust/1",
        "signers": {
            "manifest": [entry(k) for k in manifest_signers],
            "signoff": [entry(k) for k in signoff_signers],
            "testset": [],
        },
    }
    return canonical.dumps(obj)


def test_lock_check_trust_policy_only_from_main(
    tmp_path: Path, keys: Keys, monkeypatch: pytest.MonkeyPatch, capsys: Any
) -> None:
    """The PR adds its own signer to trust-policy.json and pins a manifest it signed.
    lock-check reads main's policy only; pointing MHAI_MODELS_TRUST_POLICY at the PR's
    copy is refused."""
    st = MemoryStore()
    evil = MemSigner()
    pub = publish(st, tmp_path, keys, signer=evil)
    main = _main_checkout(tmp_path / "main")
    (main / "models" / "trust-policy.json").write_bytes(
        _policy_obj([keys.manifest], [keys.signoff])
    )
    head_dir = _main_checkout(tmp_path / "head", base=lock.Lock({ROLE: pub.pin()}))
    head_policy = head_dir / "models" / "trust-policy.json"
    head_policy.write_bytes(_policy_obj([keys.manifest, evil], [keys.signoff]))
    session = FakeGitHubSession()
    session.add(_approved_pr())
    real_policy = cli._trust_policy
    _cli_env(monkeypatch, st, keys, session, tmp_path)
    monkeypatch.setattr(cli, "_trust_policy", real_policy)  # the real loader here
    args = _lock_check_args(main, head_dir / "models.lock")

    monkeypatch.setenv(cli.TRUST_POLICY_ENV, str(head_policy))
    calls = len(st.calls)
    assert cli.main(args) == 2
    out = json.loads(capsys.readouterr().out)
    assert out == {"refused": "not_allowed_here", "detail": "trust_policy_outside_base_dir"}
    assert len(st.calls) == calls  # refused before any store read
    for env in (None, str(main / "models" / "trust-policy.json")):
        if env is None:
            monkeypatch.delenv(cli.TRUST_POLICY_ENV, raising=False)
        else:
            monkeypatch.setenv(cli.TRUST_POLICY_ENV, env)
        assert cli.main(args) == 2
        assert json.loads(capsys.readouterr().out) == {"refused": "signer_untrusted"}
    # the PR's policy would have trusted it: main's is what refuses
    head_trust = signing.load_policy(head_policy)
    lock.find_signed_manifest(st, NAME, "1.0.0", head_trust)


def test_lock_check_refuses_a_pr_not_into_main(tmp_path: Path, keys: Keys, cache: Path) -> None:
    st, _, head = _new(tmp_path, keys)
    pr = _approved_pr()
    pr.base_ref = "side-branch"
    r = _reason(lambda: _check(st, keys, cache, lock.empty(), head, pr))
    assert r.reason == "base_not_main"


def test_lock_check_evaluation_refusals(tmp_path: Path, keys: Keys, cache: Path) -> None:
    st = MemoryStore()
    rep = report_bytes({"phase1": "pass"}, thresholds_sha="0" * 64)
    a = publish(st, tmp_path, keys, version="1.0.0", report_raw=rep)
    b = publish(
        st, tmp_path, keys, version="1.1.0", seed=8, testset=f"clinical_document@1.0.0#{'ee' * 32}"
    )
    for pub, detail in (
        (a, ("gate_failed", "thresholds")),
        (b, ("testset_mismatch", "not_locked")),
    ):
        head = lock.Lock({ROLE: pub.pin()})
        r = _reason(lambda h=head: _check(st, keys, cache, lock.empty(), h, _approved_pr()))
        assert (r.reason, r.detail_code) == detail


def test_approval_with_null_commit_does_not_count(tmp_path: Path, keys: Keys, cache: Path) -> None:
    st, _, head = _new(tmp_path, keys)
    pr = _pr(reviews=[(CLINICAL, "APPROVED", None), (PLATFORM, "APPROVED", NULL)])
    r = _reason(lambda: _check(st, keys, cache, lock.empty(), head, pr))
    assert r.reason == "not_approved"


@pytest.mark.parametrize(
    "body",
    [
        None,
        "",
        "Clinical note:",
        "Clinical note: too short",
        "No Clinical note: needed for this synthetic rollback at all.",
        "  Clinical note: indented text that is long enough to count.",
        "> Clinical note: quoted text that is long enough to count here.",
        "`Clinical note:` inline text that is long enough to count here.",
        "<!-- Clinical note: hidden in a comment, long enough to count. -->",
        "<!--\nClinical note: hidden in a multi-line comment, long enough.\n-->",
        "<!-- unclosed\nClinical note: after an unclosed comment, long enough.",
        "```\nClinical note: inside a fenced block, long enough to count.\n```",
        "~~~~\nClinical note: inside a tilde fence, long enough to count.\n~~~~",
        "```\nClinical note: inside an unclosed fence, long enough to count.",
        "Clinical note:" + " " * 40 + "a b c",
        "Clinical note: " + "\u200b" * 30,  # zero-width spaces
        "Clinical note: " + "\u2060\ufeff" * 15,  # word joiners and BOMs
        "Clinical note: " + "-" * 40,  # punctuation only
        "Clinical note: " + "!? .,;:" * 10 + "abc",
        "Clinical note: short\u200bbut\u200bpadded\u200b\u200b\u200b\u200b\u200b\u200b",
        # a closer with an info string doesn't close the fence (CommonMark)
        "```\n```js\nClinical note: still inside the fenced block here.\n```",
        "~~~\n~~~ x\nClinical note: still inside the tilde fenced block.\n~~~",
        # a shorter closer doesn't close a longer fence
        "````\n```\nClinical note: still inside the four-backtick fence.\n````",
        # boundaries are found on the original text: a Cf character breaks a marker, so
        # it neither closes a comment nor a fence early
        "<!-- hidden -\u200b->\nClinical note: hidden in the comment GitHub keeps hidden.\n-->",
        "<!-- a -\u2060-> b\nClinical note: still hidden, the comment never closed here.",
        "```\n``\u200b`\nClinical note: still inside the fenced block after all.\n```",
        "~~~\n~\ufeff~~\nClinical note: still inside the tilde fence after all.\n~~~",
        # M1: lines end only at \n, \r\n or \r, as in CommonMark; these stay in the fence
        *(
            f"```\nx{sep}```\nClinical note: {LETTERS}\n```"
            for sep in ("\v", "\f", "\x1c", "\x85", "\u2028", "\u2029")
        ),
        # a backtick fence whose info string has a backtick isn't a fence: fail closed
        f"``` a`b\n```\nClinical note: {LETTERS}",
        # raw HTML other than a complete comment hides text: fail closed
        f"<?php x\nClinical note: {LETTERS}\n?>",
        f"<![CDATA[\nClinical note: {LETTERS}\n]]>",
        f"<!X\nClinical note: {LETTERS}\n>",
        f'<a title="\nClinical note: {LETTERS}\n">x</a>',
        f"<script>\nClinical note: {LETTERS}\n</script>",
        f"Clinical note: {LETTERS}\n</div>",
        f"<!-- unclosed\nClinical note: {LETTERS}",
        # letters that render as nothing don't count
        "Clinical note: " + "\u3164" * 25,
        "Clinical note: " + "\uffa0" * 25,
        "Clinical note: " + "\u115f\u1160" * 13,
        "Clinical note: abc " + "\u3164" * 25,
        # D34: a link reference definition is never rendered; anywhere in the body, it
        # voids the note
        f'[a]: https://example.invalid "\nClinical note: {LETTERS}\n"',
        f"[Clinical note: {LETTERS}]: https://example.invalid",
        f"Clinical note: {LETTERS}\n\n[ref]: https://example.invalid",
        f"Clinical note: {LETTERS}\n   [ref]: https://example.invalid",
        f"Clinical note: {LETTERS}\n\u200b[ref]: https://example.invalid",
    ],
)
def test_clinical_note_bypasses_refused(body: str | None) -> None:
    assert not lock.has_clinical_note(body)  # type: ignore[arg-type]


@pytest.mark.parametrize(
    "body",
    [
        "Clinical note: back to 1.0.0 after a synthetic issue in 1.1.0.",
        "Summary.\n\n```\ncode\n```\nClinical note: restoring the signed-off synthetic version.",
        "<!-- template -->\r\nClinical note: restoring the signed-off synthetic version.",
        "```js\ncode\n```   \nClinical note: restoring the signed-off synthetic version.",
        "Clinical note: Rückkehr zur freigegebenen Version 1.0.0 wegen Fehler",
        "Clinical\u200b note: zero-width in the marker is removed before parsing it.",
        "Summary.\rClinical note: a lone carriage return ends a line as in CommonMark.",
        "Summary.\r\n\r\nClinical note: restoring the signed-off synthetic version.",
        "Version <2 was fine.\nClinical note: restoring the signed-off synthetic version.",
    ],
)
def test_clinical_note_accepted(body: str) -> None:
    assert lock.has_clinical_note(body)


def test_cf_dash_does_not_close_comment() -> None:
    """``-\u200b->`` is not ``-->``: the comment stays open, and a real ``-->`` after it
    closes it, so a note after that does count."""
    hidden = "<!-- x -\u200b->\nClinical note: hidden inside the still open comment.\n"
    assert not lock.has_clinical_note(hidden)
    assert lock.has_clinical_note(hidden + "-->\nClinical note: visible after the real close.")


def test_cf_in_fence_marker_does_not_close_fence() -> None:
    inside = "```\n`\u200b``\nClinical note: inside the fence that never closed.\n"
    assert not lock.has_clinical_note(inside)
    assert lock.has_clinical_note(inside + "```\nClinical note: after the real closing fence.")


def _rollback_setup(
    tmp_path: Path, keys: Keys, *, signed_off: bool
) -> tuple[MemoryStore, lock.Lock, lock.Lock]:
    st = MemoryStore()
    # the older version's evaluation is on the dev split: a new pin of it would be refused,
    # so passing proves no new evaluation is asked of a rollback
    old = publish(st, tmp_path, keys, version="1.0.0", split="dev")
    if signed_off:
        write_signoff(st, old, keys)
    current = publish(st, tmp_path, keys, version="1.1.0", seed=8)
    return st, lock.Lock({ROLE: current.pin()}), lock.Lock({ROLE: old.pin()})


def test_rollback_to_signed_off_version_needs_no_eval(
    tmp_path: Path, keys: Keys, cache: Path
) -> None:
    st, base, head = _rollback_setup(tmp_path, keys, signed_off=True)
    platform_only = [(PLATFORM, "APPROVED", None)]
    note = "Clinical note: back to 1.0.0 after a synthetic issue."
    pr = _pr(reviews=platform_only, body=note)
    assert _check(st, keys, cache, base, head, pr) == {ROLE: "rollback"}
    # still the label, the note and a platform approval
    no_label = _pr(reviews=platform_only, labels=[], body=note)
    assert _reason(lambda: _check(st, keys, cache, base, head, no_label)).reason == (
        "label_missing"
    )
    no_note = _pr(reviews=platform_only, body="Rolling back.")
    r = _reason(lambda: _check(st, keys, cache, base, head, no_note))
    assert (r.reason, r.detail_code) == ("not_approved", "clinical_note")
    hidden = _pr(reviews=platform_only, body=f"<!-- {note} -->")
    r = _reason(lambda: _check(st, keys, cache, base, head, hidden))
    assert (r.reason, r.detail_code) == ("not_approved", "clinical_note")
    no_platform = _pr(reviews=[(CLINICAL, "APPROVED", None)], body=note)
    assert _reason(lambda: _check(st, keys, cache, base, head, no_platform)).reason == (
        "not_approved"
    )


@pytest.mark.parametrize("who", [ACTOR, TRAINER])
def test_rollback_approver_cannot_be_its_promoter_or_trainer(
    who: str, tmp_path: Path, keys: Keys, cache: Path
) -> None:
    st, base, head = _rollback_setup(tmp_path, keys, signed_off=True)
    reviewers = lock.Reviewers(frozenset({CLINICAL}), frozenset({who}))
    note = "Clinical note: back to 1.0.0 after a synthetic issue."
    pr = _pr(reviews=[(who, "APPROVED", None)], body=note)
    r = _reason(lambda: _check(st, keys, cache, base, head, pr, reviewers))
    assert r.reason == "separation_of_duties"


def test_signoff_for_another_role_is_not_a_rollback(
    tmp_path: Path, keys: Keys, cache: Path
) -> None:
    st, base, head = _rollback_setup(tmp_path, keys, signed_off=False)
    old = head.models[ROLE]
    raw = verify.find_manifest(st, old.name, old.version, old.manifest_sha256)
    write_signoff(st, Pub(raw, old.manifest_sha256, manifest.parse(raw)), keys, role="ocr")
    note = "Clinical note: back to 1.0.0 after a synthetic issue."
    pr = _pr(reviews=[(PLATFORM, "APPROVED", None)], body=note)
    r = _reason(lambda: _check(st, keys, cache, base, head, pr))
    assert (r.reason, r.detail_code) == ("not_approved", "never_approved")


def test_rollback_to_never_approved_refused(tmp_path: Path, keys: Keys, cache: Path) -> None:
    st, base, head = _rollback_setup(tmp_path, keys, signed_off=False)
    pr = _pr(reviews=[(PLATFORM, "APPROVED", None)], body="Clinical note: back to 1.0.0.")
    r = _reason(lambda: _check(st, keys, cache, base, head, pr))
    assert r.reason == "not_approved"
    # a sign-off for another gate is not this pin's sign-off
    (tmp_path / "b").mkdir()
    st2, base2, head2 = _rollback_setup(tmp_path / "b", keys, signed_off=False)
    old = head2.models[ROLE]
    raw = verify.find_manifest(st2, old.name, old.version, old.manifest_sha256)
    write_signoff(st2, Pub(raw, old.manifest_sha256, manifest.parse(raw)), keys, gate="phase3")
    r = _reason(lambda: _check(st2, keys, cache, base2, head2, pr))
    assert r.reason == "not_approved"


def test_removed_role_needs_platform_approval(tmp_path: Path, keys: Keys, cache: Path) -> None:
    st = MemoryStore()
    base = lock.Lock({ROLE: lock.Pin(NAME, "1.0.0", "ab" * 32, "phase1")})
    pr = _pr(labels=[], reviews=[(CLINICAL, "APPROVED", None)])
    r = _reason(lambda: _check(st, keys, cache, base, lock.empty(), pr))
    assert r.reason == "not_approved"
    pr = _pr(labels=[], reviews=[(PLATFORM, "APPROVED", None)])
    assert _check(st, keys, cache, base, lock.empty(), pr) == {ROLE: "removed"}


def test_dependencies_that_disagree_refused(tmp_path: Path, keys: Keys) -> None:
    st = MemoryStore()
    dep = manifest.Dependency("syn-base-model", "1.0.0", "ab" * 32)
    pub = publish(st, tmp_path, keys, dependencies=(dep,))
    same = lock.Pin("syn-base-model", "1.0.0", "ab" * 32, "phase1")
    agree = lock.Lock({ROLE: pub.pin(), "ocr": same})
    lock.check_dependencies(agree, {ROLE: pub.m})
    differs = lock.Pin("syn-base-model", "1.1.0", "cd" * 32, "phase1")
    disagree = lock.Lock({ROLE: pub.pin(), "ocr": differs})
    r = _reason(lambda: lock.check_dependencies(disagree, {ROLE: pub.m}))
    assert (r.reason, r.detail_code) == ("lock_invalid", "dependencies")
    other_dep = manifest.Dependency("syn-base-model", "1.0.0", "cd" * 32)
    other = publish(st, tmp_path, keys, name="syn-other-model", dependencies=(other_dep,))
    two = lock.Lock({ROLE: pub.pin(), "ocr": other.pin()})
    r = _reason(lambda: lock.check_dependencies(two, {ROLE: pub.m, "ocr": other.m}))
    assert (r.reason, r.detail_code) == ("lock_invalid", "dependencies")


# ---------------------------------------------------------------- the GitHub client


def test_github_reviews_paginated_and_logins_lowercased() -> None:
    session = FakeGitHubSession()
    pr = _pr(reviews=[(f"Syn-Commenter-{i}", "COMMENTED", None) for i in range(250)])
    pr.review("SYN-PLATFORM-1", "APPROVED")
    session.add(pr)
    got = lock.GitHubClient(session, GH_REPO).pull_request(7)
    assert got.author == AUTHOR and got.head_sha == HEAD
    assert len(got.reviews) == 251
    assert lock.approvals(got) == {PLATFORM}
    pages = [r.params.get("page") for r in session.requests if r.url.endswith("/reviews")]
    assert pages == ["1", "2", "3"]


def test_github_reviews_capped(tmp_path: Path) -> None:
    session = FakeGitHubSession()
    n = lock.MAX_REVIEW_PAGES * 100 + 1  # one more than the cap holds: a next page remains
    session.add(_pr(reviews=[(f"syn-c{i}", "COMMENTED", None) for i in range(n)]))
    r = _reason(lambda: lock.GitHubClient(session, GH_REPO).pull_request(7))
    assert (r.reason, r.detail_code) == ("not_approved", "too_many_reviews")
    assert len(session.requests) == 1 + lock.MAX_REVIEW_PAGES


def test_github_reviews_follow_link_when_pages_are_short() -> None:
    """The server serves 30 per page (not the 100 asked): the Link header is followed,
    and a "changes requested" on a later page cancels a page-1 approval."""
    session = FakeGitHubSession()
    session.max_per_page = 30
    pr = _pr(reviews=[(PLATFORM, "APPROVED", None)])
    for i in range(70):
        pr.review(f"syn-commenter-{i}", "COMMENTED")
    pr.review(PLATFORM, "CHANGES_REQUESTED")
    session.add(pr)
    got = lock.GitHubClient(session, GH_REPO).pull_request(7)
    assert len(got.reviews) == 72
    assert lock.approvals(got) == frozenset()
    urls = [r.url for r in session.requests if r.url.endswith("/reviews")]
    assert len(urls) == 3 and all(u.startswith("https://api.github.com/") for u in urls)


def test_github_changes_requested_on_page_two_cancels() -> None:
    session = FakeGitHubSession()
    pr = _pr(reviews=[(PLATFORM, "APPROVED", None)])
    for i in range(120):
        pr.review(f"syn-commenter-{i}", "COMMENTED")
    pr.review(PLATFORM, "CHANGES_REQUESTED")
    session.add(pr)
    got = lock.GitHubClient(session, GH_REPO).pull_request(7)
    assert PLATFORM not in lock.approvals(got)


@pytest.mark.parametrize(
    "next_base",
    [
        "https://evil.example/repositories/424242",
        "http://api.github.com/repositories/424242",
        "https://api.github.com/repos/other-org/other-repo",
        "https://api.github.com/repositories/424242/../../x",
        "https://api.github.com/repositories/999999",  # another repository's id
    ],
)
def test_github_next_link_elsewhere_refused(next_base: str) -> None:
    session = FakeGitHubSession()
    session.next_base = next_base
    session.add(_pr(reviews=[(f"syn-c{i}", "COMMENTED", None) for i in range(150)]))
    r = _reason(lambda: lock.GitHubClient(session, GH_REPO).pull_request(7))
    assert (r.reason, r.detail_code) == ("github_unavailable", "pagination")
    assert all(req.url.startswith("https://api.github.com/repos/") for req in session.requests)


@pytest.mark.parametrize(("step", "per_page"), [(2, None), (0, None), (1, "50"), (1, "")])
def test_github_next_link_must_be_the_next_page(step: int, per_page: str | None) -> None:
    """A skipped or repeated page, or a changed per_page, is refused."""
    session = FakeGitHubSession()
    session.link_page_step = step
    session.link_per_page = per_page
    session.add(_pr(reviews=[(f"syn-c{i}", "COMMENTED", None) for i in range(150)]))
    r = _reason(lambda: lock.GitHubClient(session, GH_REPO).pull_request(7))
    assert (r.reason, r.detail_code) == ("github_unavailable", "pagination")


def test_github_repositories_form_needs_the_repository_id() -> None:
    """Without this repository's id from the pull request, the /repositories/<id> form of a
    next link can't be bound to it, and is refused."""

    class NoRepoId(FakePR):
        def resource(self) -> dict[str, Any]:
            obj = super().resource()
            del obj["base"]["repo"]
            return obj

    session = FakeGitHubSession()
    pr = NoRepoId(7, "Syn-Author", HEAD, labels=["clinical-review"])
    for i in range(150):
        pr.review(f"syn-c{i}", "COMMENTED")
    session.add(pr)
    r = _reason(lambda: lock.GitHubClient(session, GH_REPO).pull_request(7))
    assert (r.reason, r.detail_code) == ("github_unavailable", "pagination")
    # the same path form (/repos/<owner>/<name>/...) is still followed
    session.requests.clear()
    session.next_base = f"https://api.github.com/repos/{GH_REPO}"
    assert len(lock.GitHubClient(session, GH_REPO).pull_request(7).reviews) == 150


def test_github_review_by_deleted_user_skipped() -> None:
    session = FakeGitHubSession()
    pr = _pr(reviews=[(None, "APPROVED", None), (PLATFORM, "APPROVED", None)])
    session.add(pr)
    got = lock.GitHubClient(session, GH_REPO).pull_request(7)
    assert [r.login for r in got.reviews] == [PLATFORM]


@pytest.mark.parametrize("mode", ["raise", "500", "404"])
def test_github_errors_carry_no_token(mode: str, caplog: pytest.LogCaptureFixture) -> None:
    caplog.set_level(logging.DEBUG)
    session = FakeGitHubSession()
    session.add(_pr())
    if mode == "raise":
        session.raise_error = True
    else:
        session.status = int(mode)
    with pytest.raises(Refused) as e:
        lock.GitHubClient(session, GH_REPO).pull_request(7)
    assert e.value.reason == ("not_found" if mode == "404" else "github_unavailable")
    assert e.value.__cause__ is None and e.value.__context__ is None
    for text in (str(e.value), repr(e.value), str(e.value.detail_code), caplog.text):
        assert session.token not in text


def test_github_client_needs_token_and_checks_inputs() -> None:
    r = _reason(lambda: lock.GitHubClient.from_env({"GITHUB_REPOSITORY": GH_REPO}))
    assert r.reason == "not_allowed_here"
    for repo in ("x", "a/b/c", "../x", "a b/c"):
        with pytest.raises(Refused) as e:
            lock.GitHubClient(FakeGitHubSession(), repo)
        assert e.value.reason == "not_allowed_here"
    for url in ("http://api.github.com", "https://user@api.github.com", "https://x/../y"):
        with pytest.raises(Refused) as e:
            lock.GitHubClient(FakeGitHubSession(), GH_REPO, url)
        assert e.value.reason == "not_allowed_here"
    for n in (0, -1, "7a", "07", True, 2**31):
        with pytest.raises(Refused) as e:
            lock.check_pr_number(n)
        assert e.value.reason == "invalid_name"


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
    monkeypatch.setattr(cli, "_trust_policy", lambda: calls.append("policy"))
    monkeypatch.delenv(cli.READER_ACCESS_ENV, raising=False)
    with contextlib.ExitStack() as stack:
        verifier = cli._base_verifier(stack)
        assert calls == []
        r = _reason(lambda: verifier(f"syn-base-model@1.0.0#{'ab' * 32}"))
        assert (r.reason, r.detail_code) == ("base_unverified", "reader_credentials")
        assert calls == []


# ---------------------------------------------------------------- the command line


def _cli_env(
    monkeypatch: pytest.MonkeyPatch,
    st: MemoryStore,
    keys: Keys,
    session: FakeGitHubSession,
    tmp_path: Path,
) -> None:
    testsets_lock = tmp_path / "testsets.lock"
    testsets_lock.write_text(f"{TESTSET}\n")
    monkeypatch.setenv(cli.TESTSETS_LOCK_ENV, str(testsets_lock))
    monkeypatch.setattr(cli, "_reader_store", lambda: st)
    monkeypatch.setattr(cli, "_trust_policy", lambda path=None: keys.policy)
    monkeypatch.setattr(cli, "_github", lambda: lock.GitHubClient(session, GH_REPO))
    # these checkouts are plain folders: the files the PR changes come from this stand-in
    # (the real git diff is tested in test_lock_check_refuses_lock_with_code_from_git)
    monkeypatch.setattr(cli, "_pr_changed_files", lambda base_dir, head_sha: list(LOCK_ONLY))


def test_cli_pin_and_lock_check(
    tmp_path: Path, keys: Keys, monkeypatch: pytest.MonkeyPatch, capsys: Any
) -> None:
    st = MemoryStore()
    pub = publish(st, tmp_path, keys)
    publish(st, tmp_path, keys, version="2.0.0", gates={"phase1": "fail"})
    session = FakeGitHubSession()
    _cli_env(monkeypatch, st, keys, session, tmp_path)
    path = _lock_file(tmp_path)

    assert cli.main(["pin", ROLE, NAME, "1.0.0", "--gate", "phase1", "--lock", str(path)]) == 0
    out = json.loads(capsys.readouterr().out)
    assert out == {"pinned": {"role": ROLE, **pub.pin().to_obj()}}
    assert lock.read(path).models[ROLE] == pub.pin()

    assert cli.main(["pin", ROLE, NAME, "2.0.0", "--gate", "phase1", "--lock", str(path)]) == 2
    assert json.loads(capsys.readouterr().out) == {"refused": "gate_failed"}
    assert cli.main(["pin", ROLE, "../x", "1.0.0", "--gate", "phase1", "--lock", str(path)]) == 2
    assert json.loads(capsys.readouterr().out) == {"refused": "invalid_name"}

    # a rehearsal lock: only with --rehearsal, only in development, only with --lock
    rehearsal = ["pin", ROLE, NAME, "1.0.0", "--gate", "phase1", "--rehearsal"]
    assert cli.main([*rehearsal, "--lock", str(path)]) == 2
    out = json.loads(capsys.readouterr().out)
    assert out == {"refused": "not_allowed_here", "detail": "rehearsal"}
    with monkeypatch.context() as dev:  # development, but no explicit --lock
        dev.setenv(signing.ENV_VAR, "development")
        dev.setenv(verify.APP_ENV, "development")
        assert verify.is_development()
        assert cli.main(rehearsal) == 2
        out = json.loads(capsys.readouterr().out)
        assert out == {"refused": "not_allowed_here", "detail": "rehearsal"}

    main = _main_checkout(tmp_path / "main")
    args = _lock_check_args(main, path)
    with pytest.raises(SystemExit):  # --head-sha is required
        cli.main([a for i, a in enumerate(args) if i not in (5, 6)])
    capsys.readouterr()
    session.add(_approved_pr())
    assert cli.main(args) == 0
    assert json.loads(capsys.readouterr().out) == {"lock_check": "pass", "roles": {ROLE: "new"}}

    session.status = 502
    assert cli.main(args) == 3
    out = capsys.readouterr().out
    assert json.loads(out) == {"refused": "github_unavailable", "detail": "http"}
    assert session.token not in out


# ---------------------------------------------------------------- D35: the lock travels alone

# Anything but models.lock, models/**, docs/** and CHANGELOG.md (D35b: an allow-list).
_OUTSIDE = [
    ".github/workflows/model-lock.yml",
    ".github/workflows/model-signoff.yaml",
    ".github/workflows/ci.yml",
    ".github/workflows/deploy.yml",
    ".github/actions/x/action.yml",
    ".github/scripts/boundary-smoke.sh",
    ".github/CODEOWNERS",
    "services/intelligence/scripts/rehearse-promotion.sh",
    "services/intelligence/intelligence/modelstore/lock.py",
    "services/intelligence/intelligence/doceval/report.py",
    "services/intelligence/intelligence/doceval/thresholds/phase1.json",
    "services/intelligence/intelligence/boundary/worker.py",
    "services/intelligence/pyproject.toml",
    "services/intelligence/uv.lock",
    "services/core/main.go",
    "README.md",
    "models.lock.bak",
    "xmodels/a.json",
    "documentation/x.md",
    "docs/../services/intelligence/pyproject.toml",  # not a path git prints: refused
    "models/./x.json",
    "docs\\..\\x",
]


@pytest.mark.parametrize("other", _OUTSIDE)
@pytest.mark.parametrize("lock_path", ["models.lock", "models/reviewers.json"])
def test_lock_change_with_anything_else_refused(lock_path: str, other: str) -> None:
    r = _reason(lambda: lock.check_lock_change_alone(["docs/a.md", lock_path, other]))
    assert (r.reason, r.detail_code) == ("not_allowed_here", "code_changed_with_lock")


def test_lock_change_rename_out_of_models_refused() -> None:
    """git diff --no-renames lists a rename as its old and its new path: one outside the
    list is enough."""
    renamed = ["models/x.json", "services/intelligence/intelligence/modelstore/x.json"]
    r = _reason(lambda: lock.check_lock_change_alone(renamed))
    assert (r.reason, r.detail_code) == ("not_allowed_here", "code_changed_with_lock")


@pytest.mark.parametrize(
    "paths",
    [
        [],
        ["models.lock"],
        ["models/reviewers.json"],
        ["models.lock", "models/testsets.lock", "docs/models/notes.md", "CHANGELOG.md"],
        # code alone, with no lock change, is not this rule's business
        [p for p in _OUTSIDE if not p.startswith(("models", "docs"))],
    ],
)
def test_lock_change_alone_allowed(paths: list[str]) -> None:
    lock.check_lock_change_alone(paths)


def test_lock_change_alone_needs_a_list_of_paths() -> None:
    with pytest.raises(TypeError):
        lock.check_lock_change_alone("models.lock")


def test_lock_check_refuses_code_with_lock_before_any_io(
    tmp_path: Path, keys: Keys, cache: Path
) -> None:
    st, _, head = _new(tmp_path, keys)
    session = FakeGitHubSession()
    session.add(_approved_pr())
    calls = len(st.calls)
    r = _reason(
        lambda: lock.lock_check(
            lock.to_bytes(lock.empty()),
            lock.to_bytes(head),
            7,
            github=lock.GitHubClient(session, GH_REPO),
            reviewers=REVIEWERS,
            source=st,
            policy=keys.policy,
            cache_dir=cache,
            testsets_lock=TESTSETS,
            thresholds_dir=report.THRESHOLDS_DIR,
            head_sha=HEAD,
            changed_paths=["models.lock", ".github/workflows/model-lock.yml"],
        )
    )
    assert (r.reason, r.detail_code) == ("not_allowed_here", "code_changed_with_lock")
    assert len(st.calls) == calls and session.requests == []


_GIT_ENV = {"GIT_CONFIG_NOSYSTEM": "1", "GIT_AUTHOR_NAME": "syn", "GIT_COMMITTER_NAME": "syn"}


def git(root: Path, *args: str) -> str:
    """Run git in ``root`` with no user or system configuration; its stdout."""
    env = {
        "PATH": os.environ.get("PATH", ""),
        "HOME": str(root),
        "GIT_AUTHOR_EMAIL": "syn@example.invalid",
        "GIT_COMMITTER_EMAIL": "syn@example.invalid",
        **_GIT_ENV,
    }
    done = subprocess.run(["git", "-C", str(root), *args], check=True, capture_output=True, env=env)
    return done.stdout.decode().strip()


def git_commit(root: Path, message: str) -> str:
    git(root, "add", "-A")
    git(root, "commit", "-qm", message)
    return git(root, "rev-parse", "HEAD")


def _git_main(root: Path) -> tuple[Path, str]:
    """A repository on ``main`` with one commit (a main checkout as model-lock.yml makes it)."""
    main = _main_checkout(root)
    (main / ".github" / "workflows").mkdir(parents=True)
    (main / ".github" / "workflows" / "model-lock.yml").write_text("name: synthetic\n")
    git(main, "init", "-q", "-b", "main")
    return main, git_commit(main, "base")


def _git_branch(main: Path, name: str, changes: dict[str, str]) -> str:
    """A PR head: a branch off main's current tip with ``changes``; main is checked out again."""
    git(main, "checkout", "-q", "-b", name)
    for rel, text in changes.items():
        path = main / rel
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text)
    sha = git_commit(main, name)
    git(main, "checkout", "-q", "main")
    return sha


def test_lock_check_refuses_lock_with_code_from_git(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, capsys: Any
) -> None:
    """D35 through the command line, with a real repository: the changed files are git's
    diff from the merge base to the head sha, never a list the PR supplies."""
    main, _ = _git_main(tmp_path / "main")
    reviewers_change = {"models/reviewers.json": json.dumps({"clinical": [], "platform": []})}
    workflow = {".github/workflows/model-lock.yml": "name: rewritten\n"}
    code = {"services/intelligence/intelligence/modelstore/lock.py": "# synthetic\n"}
    with_workflow = _git_branch(main, "pr-workflow", {**reviewers_change, **workflow})
    with_code = _git_branch(main, "pr-code", {**reviewers_change, **code})
    alone = _git_branch(main, "pr-alone", reviewers_change)
    workflow_only = _git_branch(main, "pr-workflow-only", workflow)
    git(main, "checkout", "-q", "-b", "pr-rename")  # a rename out of models/: both paths
    git(main, "mv", "models/testsets.lock", "services/testsets.lock")
    renamed = git_commit(main, "pr-rename")
    git(main, "checkout", "-q", "main")
    # main moves on after the PRs branched: its own modelstore change is not the PR's
    (main / "services/intelligence/intelligence/modelstore").mkdir(parents=True)
    (main / "services/intelligence/intelligence/modelstore/later.py").write_text("# syn\n")
    git_commit(main, "main moves on")
    st = MemoryStore()
    monkeypatch.setattr(cli, "_reader_store", lambda: st)
    head_file = tmp_path / "head.lock"
    head_file.write_bytes(lock.to_bytes(lock.empty()))

    for sha in (with_workflow, with_code, renamed):
        assert cli.main(_lock_check_args(main, head_file, sha)) == 2
        out = json.loads(capsys.readouterr().out)
        assert out == {"refused": "not_allowed_here", "detail": "code_changed_with_lock"}
    assert st.calls == []
    # the lock data alone, or the workflow alone, is not refused by this rule
    for sha in (alone, workflow_only):
        assert cli.main(_lock_check_args(main, head_file, sha)) == 0
        assert json.loads(capsys.readouterr().out) == {"lock_check": "pass", "roles": {}}
    # a head that isn't in main's repository (not fetched): fail closed
    assert cli.main(_lock_check_args(main, head_file, "c3" * 20)) == 2
    out = json.loads(capsys.readouterr().out)
    assert out == {"refused": "not_allowed_here", "detail": "changed_files_unknown"}
    # a base dir that isn't a repository at all
    plain = _main_checkout(tmp_path / "plain")
    assert cli.main(_lock_check_args(plain, head_file, alone)) == 2
    out = json.loads(capsys.readouterr().out)
    assert out == {"refused": "not_allowed_here", "detail": "changed_files_unknown"}

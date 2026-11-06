"""The sign-off after a merge, and rollback (plan Task 21).

The store is the in-memory fake (with retention it can lengthen) or, for tests marked
``store``, MinIO; GitHub is the fake session in ``fakes/github_api.py``; keys are made in
memory. No test opens a socket outside loopback. Every login and model is synthetic.
"""

from __future__ import annotations

import json
import secrets
from collections.abc import Callable
from datetime import UTC, datetime, timedelta
from pathlib import Path
from typing import Any

import pytest

from intelligence.doceval import report
from intelligence.modelstore import cli, lock, signing, signoff, store, verify
from intelligence.modelstore.reasons import Refused
from tests.modelstore.conftest import MinioStore
from tests.modelstore.fakes.github_api import REPO as GH_REPO
from tests.modelstore.fakes.github_api import FakeGitHubSession, FakePR
from tests.modelstore.fakes.memory_store import MemoryStore
from tests.modelstore.test_lock import (
    ACTOR,
    CLINICAL,
    NAME,
    PLATFORM,
    REVIEWERS,
    ROLE,
    TESTSETS,
    TRAINER,
    Keys,
    Pub,
    _approved_pr,
    _main_checkout,
    _pr,
    publish,
    write_signoff,
)
from tests.modelstore.test_sources import no_network  # noqa: F401  (autouse: no network)
from tests.modelstore.test_verify import MemSigner

COMMIT = "c3" * 20
OTHER_COMMIT = "d4" * 20
MERGED_AT = "2026-11-06T02:00:00Z"
DAYS = 5
THRESHOLDS_REL = Path("services/intelligence/intelligence/doceval/thresholds")


# ---------------------------------------------------------------- fixtures


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
    monkeypatch.delenv(cli.TRUST_POLICY_ENV, raising=False)
    monkeypatch.delenv(cli.SIGNER_ENV, raising=False)


@pytest.fixture
def cache(tmp_path: Path) -> Path:
    c = tmp_path / "cache"
    c.mkdir(mode=0o700)
    return c


# ---------------------------------------------------------------- fakes


class RetainingStore(MemoryStore):
    """The in-memory store with retention that only lengthens, as compliance mode does."""

    def __init__(self, **kw: Any) -> None:
        super().__init__(**kw)
        self.extended: list[tuple[str, str, int]] = []

    def extend_retention(self, key: str, version_id: str, days: int) -> datetime:
        store._check_key(key)
        self.calls.append(("retain", key))
        if not any(vid == version_id for vid, _ in self.objects.get(key, [])):
            raise Refused("not_found", detail_code="missing")
        until = datetime.now(UTC).replace(microsecond=0) + timedelta(days=days)
        mode, held = self.locks[version_id]
        if mode != "COMPLIANCE" or held is None:
            raise Refused("store_unavailable", detail_code="not_locked")
        if until <= held:
            raise Refused("not_allowed_here", detail_code="retention_not_longer")
        self.locks[version_id] = (mode, until)
        self.extended.append((key, version_id, days))
        return until


class DocSigner:
    """A :class:`signing.Signer` over an in-memory key (FileSigner is development only)."""

    def __init__(self, key: MemSigner) -> None:
        self._key = key
        self.key_id = key.key_id
        self.alg = key.alg

    def sign(self, purpose: str, payload: bytes) -> signing.SignatureDoc:
        return signing.parse_signature(self._key.sign(purpose, payload))


def _merged(pr: FakePR, *, commit: str = COMMIT, base_ref: str = "main") -> FakePR:
    pr.merged = True
    pr.merge_commit_sha = commit
    pr.merged_at = MERGED_AT
    pr.base_ref = base_ref
    return pr


def _session(*prs: FakePR, commit: str = COMMIT) -> FakeGitHubSession:
    session = FakeGitHubSession()
    for pr in prs:
        session.add(pr)
    session.commit_pulls[commit] = [pr.number for pr in prs]
    return session


def _sign_off(
    st: MemoryStore,
    keys: Keys,
    cache: Path,
    before: lock.Lock,
    after: lock.Lock,
    session: FakeGitHubSession,
    *,
    signer: Any = None,
    reviewers: lock.Reviewers = REVIEWERS,
    days: int = DAYS,
) -> signoff.Result:
    return signoff.sign_off(
        lock.to_bytes(before),
        lock.to_bytes(after),
        COMMIT,
        github=lock.GitHubClient(session, GH_REPO),
        reviewers=reviewers,
        target=st,
        policy=keys.policy,
        signer=signer or DocSigner(keys.signoff),
        cache_dir=cache,
        testsets_lock=TESTSETS,
        thresholds_dir=report.THRESHOLDS_DIR,
        retention_days=days,
    )


def _reason(fn: Callable[[], Any]) -> Refused:
    with pytest.raises(Refused) as e:
        fn()
    return e.value


def _new(tmp_path: Path, keys: Keys, **kw: Any) -> tuple[RetainingStore, Pub, lock.Lock]:
    st = RetainingStore()
    pub = publish(st, tmp_path, keys, **kw)
    return st, pub, lock.Lock({ROLE: pub.pin()})


def _signoff_keys(pub: Pub) -> tuple[str, str]:
    return store.signoff_key(pub.m.name, pub.m.version), store.signoff_sig_key(
        pub.m.name, pub.m.version
    )


# ---------------------------------------------------------------- the record


def test_build_refuses_reviewer_who_is_promoter_trainer_or_both() -> None:
    base: dict[str, Any] = {
        "name": NAME,
        "version": "1.0.0",
        "role": ROLE,
        "gate": "phase1",
        "manifest_sha256": "ab" * 32,
        "evaluation_sha256": "cd" * 32,
        "pr": 7,
        "merge_commit": COMMIT,
        "promotion_actor": ACTOR,
        "trained_by": TRAINER,
        "at": MERGED_AT,
    }
    raw = signoff.build(**base, clinical_reviewers=[CLINICAL], platform_reviewers=[PLATFORM])
    assert signoff.parse(raw).clinical_reviewers == (CLINICAL,)
    for clinical, platform in (
        ([CLINICAL], [CLINICAL]),
        ([ACTOR], [PLATFORM]),
        ([CLINICAL], [TRAINER]),
    ):
        r = _reason(
            lambda c=clinical, p=platform: signoff.build(
                **base, clinical_reviewers=c, platform_reviewers=p
            )
        )
        assert (r.reason, r.detail_code) == ("not_approved", "separation")
    # logins are lowercase only (D30): the parser refuses display case
    r = _reason(
        lambda: signoff.build(
            **base, clinical_reviewers=["Syn-Clinical-1"], platform_reviewers=[PLATFORM]
        )
    )
    assert r.reason == "not_approved"


# ---------------------------------------------------------------- sign-off


def test_signoff_written_and_signed_by_signoff_key(tmp_path: Path, keys: Keys, cache: Path) -> None:
    st, pub, after = _new(tmp_path, keys)
    before_puts = list(st.puts)
    result = _sign_off(st, keys, cache, lock.empty(), after, _session(_merged(_approved_pr())))
    assert result.to_obj()["signoff"] == {ROLE: "signed_off"}
    assert result.pr == 7
    json_key, sig_key = _signoff_keys(pub)
    assert st.puts[len(before_puts) :] == [json_key, sig_key]  # the record, then its signature
    raw = st.get(json_key, max_bytes=signoff.MAX_BYTES)
    s = signoff.parse(raw)
    assert (s.name, s.version, s.role, s.gate) == (NAME, "1.0.0", ROLE, "phase1")
    assert s.manifest_sha256 == pub.sha
    assert s.evaluation_sha256 == pub.m.evaluation.report_sha256
    assert (s.pr, s.merge_commit, s.at) == (7, COMMIT, MERGED_AT)
    # the approvals came in display case and are recorded lowercase
    assert (s.clinical_reviewers, s.platform_reviewers) == ((CLINICAL,), (PLATFORM,))
    assert (s.promotion_actor, s.trained_by) == (ACTOR, TRAINER)
    sig = st.get(sig_key, max_bytes=signing.MAX_SIGNATURE_DOC_BYTES)
    signer = signing.Verifier().verify("signoff", raw, sig, keys.policy)
    assert signer.key_id == keys.signoff.key_id
    with pytest.raises(Refused):  # domain separation: never a manifest signature
        signing.Verifier().verify("manifest", raw, sig, keys.policy)
    # the loader's own check now passes
    verify.verify_pin(st, ROLE, pub.pin(), keys.policy, cache, require_signoff=True)


@pytest.mark.parametrize("which", ["manifest", "stranger"])
def test_signoff_signer_must_be_a_signoff_key(
    which: str, tmp_path: Path, keys: Keys, cache: Path
) -> None:
    st, _, after = _new(tmp_path, keys)
    puts = list(st.puts)
    signer = DocSigner(keys.manifest if which == "manifest" else MemSigner())
    session = _session(_merged(_approved_pr()))
    r = _reason(lambda: _sign_off(st, keys, cache, lock.empty(), after, session, signer=signer))
    assert r.reason == "signer_untrusted"
    assert st.puts == puts
    assert session.requests == []  # refused before GitHub is asked


def _pr_scenarios() -> dict[str, tuple[Callable[[], FakeGitHubSession], tuple[str, str | None]]]:
    def two() -> FakeGitHubSession:
        other = _merged(_approved_pr())
        other.number = 8
        return _session(_merged(_approved_pr()), other)

    def next_page() -> FakeGitHubSession:
        s = _session(_merged(_approved_pr()))
        s.commit_pulls_next = True
        return s

    def no_pr() -> FakeGitHubSession:
        s = FakeGitHubSession()
        s.add(_merged(_approved_pr()))
        return s

    def admin_merge() -> FakeGitHubSession:  # merged with only one approval
        return _session(_merged(_pr(reviews=[("Syn-Clinical-1", "APPROVED", None)])))

    def stale() -> FakeGitHubSession:  # approvals on an older commit only
        old = "b2" * 20
        reviews = [(CLINICAL, "APPROVED", old), (PLATFORM, "APPROVED", old)]
        return _session(_merged(_pr(reviews=reviews)))

    def promoter() -> FakeGitHubSession:
        reviews = [(CLINICAL, "APPROVED", None), (ACTOR, "APPROVED", None)]
        return _session(_merged(_pr(reviews=reviews)))

    return {
        "no_pr": (no_pr, ("not_approved", "no_pr")),
        "two_prs": (two, ("not_approved", "many_prs")),
        "next_page": (next_page, ("not_approved", "many_prs")),
        "open": (lambda: _session(_approved_pr()), ("not_approved", "not_merged")),
        "other_commit": (
            lambda: _session(_merged(_approved_pr(), commit=OTHER_COMMIT)),
            ("not_approved", "not_merged"),
        ),
        "not_main": (
            lambda: _session(_merged(_approved_pr(), base_ref="release")),
            ("not_approved", "not_main"),
        ),
        "admin_merge": (admin_merge, ("not_approved", "approvals")),
        "stale": (stale, ("not_approved", "approvals")),
        "no_label": (
            lambda: _session(_merged(_approved_pr(labels=[]))),
            ("label_missing", None),
        ),
        "promoter": (promoter, ("separation_of_duties", None)),
    }


@pytest.mark.parametrize("case", sorted(_pr_scenarios()))
def test_signoff_refused_without_approvals(
    case: str, tmp_path: Path, keys: Keys, cache: Path
) -> None:
    """P17: a lock that reached main without its approvals gets no sign-off."""
    reviewers = lock.Reviewers(frozenset({CLINICAL}), frozenset({PLATFORM, ACTOR}))
    make, expected = _pr_scenarios()[case]
    st, _, after = _new(tmp_path, keys)
    puts = list(st.puts)
    session = make()
    r = _reason(
        lambda: _sign_off(st, keys, cache, lock.empty(), after, session, reviewers=reviewers)
    )
    assert (r.reason, r.detail_code) == expected
    assert st.puts == puts  # nothing written
    assert st.extended == []


def test_signoff_records_reviewers_without_promoter_author_or_duplicates(
    tmp_path: Path, keys: Keys, cache: Path
) -> None:
    st, pub, after = _new(tmp_path, keys)
    both = "syn-both-lists"
    reviewers = lock.Reviewers(
        frozenset({both, "syn-clinical-2", ACTOR}), frozenset({both, "syn-author", ACTOR})
    )
    reviews = [
        ("SYN-BOTH-LISTS", "APPROVED", None),
        ("syn-clinical-2", "APPROVED", None),
        (ACTOR, "APPROVED", None),
        ("Syn-Author", "APPROVED", None),  # the PR's author: never counts
    ]
    session = _session(_merged(_pr(reviews=reviews)))
    _sign_off(st, keys, cache, lock.empty(), after, session, reviewers=reviewers)
    s = signoff.parse(st.get(_signoff_keys(pub)[0], max_bytes=signoff.MAX_BYTES))
    assert s.clinical_reviewers == ("syn-clinical-2",)
    assert s.platform_reviewers == (both,)


def test_signoff_idempotent(tmp_path: Path, keys: Keys, cache: Path) -> None:
    st, pub, after = _new(tmp_path, keys)
    session = _session(_merged(_approved_pr()))
    assert _sign_off(st, keys, cache, lock.empty(), after, session).roles == {ROLE: "signed_off"}
    puts = list(st.puts)
    again = _sign_off(st, keys, cache, lock.empty(), after, session)
    assert again.roles == {ROLE: "already_signed_off"}
    assert st.puts == puts  # nothing written the second time
    json_key, sig_key = _signoff_keys(pub)
    assert len(st.versions(json_key)) == 1 and len(st.versions(sig_key)) == 1


def test_signoff_crash_between_puts_adopts_identical_record(
    tmp_path: Path, keys: Keys, cache: Path
) -> None:
    """signoff.json written, the run killed before signoff.sig: the next run adopts the
    record only because it is byte for byte the one it builds, and writes the signature."""
    sig_key = store.signoff_sig_key(NAME, "1.0.0")
    crashed: list[str] = []

    def fail(key: str) -> BaseException | None:
        if key == sig_key and not crashed:
            crashed.append(key)
            return KeyboardInterrupt()
        return None

    st = RetainingStore(fail=fail)
    pub = publish(st, tmp_path, keys)
    after = lock.Lock({ROLE: pub.pin()})
    session = _session(_merged(_approved_pr()))
    with pytest.raises(KeyboardInterrupt):
        _sign_off(st, keys, cache, lock.empty(), after, session)
    json_key = store.signoff_key(NAME, "1.0.0")
    assert len(st.versions(json_key)) == 1 and st.versions(sig_key) == []
    assert st.extended == []
    result = _sign_off(st, keys, cache, lock.empty(), after, session)
    assert result.roles == {ROLE: "signed_off"}
    assert len(st.versions(json_key)) == 1 and len(st.versions(sig_key)) == 1
    verify.verify_pin(st, ROLE, pub.pin(), keys.policy, cache, require_signoff=True)


def test_signoff_refuses_a_different_record(tmp_path: Path, keys: Keys, cache: Path) -> None:
    """A signoff.json that isn't the one this run builds is never adopted or signed."""
    st, pub, after = _new(tmp_path, keys)
    json_key, sig_key = _signoff_keys(pub)
    session = _session(_merged(_approved_pr()))
    # the record another merge would have built: another PR
    other = signoff.build(
        name=NAME,
        version="1.0.0",
        role=ROLE,
        gate="phase1",
        manifest_sha256=pub.sha,
        evaluation_sha256=pub.m.evaluation.report_sha256,
        pr=99,
        merge_commit=COMMIT,
        clinical_reviewers=[CLINICAL],
        platform_reviewers=[PLATFORM],
        promotion_actor=ACTOR,
        trained_by=TRAINER,
        at=MERGED_AT,
    )
    st.put_new(json_key, other, 1)
    puts = list(st.puts)
    r = _reason(lambda: _sign_off(st, keys, cache, lock.empty(), after, session))
    assert (r.reason, r.detail_code) == ("not_approved", "signoff_conflict")
    assert st.puts == puts and st.versions(sig_key) == []
    # a valid sign-off of this version for another gate is different content too
    (tmp_path / "b").mkdir()
    st2, pub2, after2 = _new(tmp_path / "b", keys, gates={"phase1": "pass", "phase3": "pass"})
    write_signoff(st2, pub2, keys, gate="phase3")
    r = _reason(lambda: _sign_off(st2, keys, cache, lock.empty(), after2, session))
    assert (r.reason, r.detail_code) == ("not_approved", "signoff_conflict")


def test_signoff_of_a_rollback_writes_nothing(tmp_path: Path, keys: Keys, cache: Path) -> None:
    st = RetainingStore()
    old = publish(st, tmp_path, keys, version="1.0.0")
    write_signoff(st, old, keys)
    current = publish(st, tmp_path, keys, version="1.1.0", seed=8)
    before, after = lock.Lock({ROLE: current.pin()}), lock.Lock({ROLE: old.pin()})
    note = "Clinical note: back to 1.0.0 after a synthetic issue."
    rollback_pr = _merged(_pr(reviews=[(PLATFORM, "APPROVED", None)], body=note))
    puts = list(st.puts)
    result = _sign_off(st, keys, cache, before, after, _session(rollback_pr))
    assert result.roles == {ROLE: "already_signed_off"}
    assert st.puts == puts
    assert result.retention_extended > 0  # back in production: kept as long as approved
    # without its clinical note, the same merge is refused
    no_note = _merged(_pr(reviews=[(PLATFORM, "APPROVED", None)], body="Rolling back."))
    r = _reason(lambda: _sign_off(st, keys, cache, before, after, _session(no_note)))
    assert (r.reason, r.detail_code) == ("not_approved", "clinical_note")


def test_signoff_removed_role_and_no_change(tmp_path: Path, keys: Keys, cache: Path) -> None:
    st = RetainingStore()
    before = lock.Lock({ROLE: lock.Pin(NAME, "1.0.0", "ab" * 32, "phase1")})
    pr = _merged(_pr(labels=[], reviews=[(PLATFORM, "APPROVED", None)]))
    result = _sign_off(st, keys, cache, before, lock.empty(), _session(pr))
    assert result.roles == {ROLE: "removed"} and st.puts == []
    session = _session(pr)
    assert _sign_off(st, keys, cache, before, before, session).roles == {}
    assert session.requests == []  # nothing changed: GitHub is not asked


def test_signoff_extends_every_object_by_version_id(
    tmp_path: Path, keys: Keys, cache: Path
) -> None:
    st, pub, after = _new(tmp_path, keys)
    m = pub.m
    # a newer manifest.json version (a shadow) is never touched: ids, never "latest"
    shadow = st.shadow(
        store.manifest_key(NAME, "1.0.0"),
        b"{}",
        lock_mode="COMPLIANCE",
        retain_until=datetime.now(UTC) + timedelta(days=1),
    )
    # an object already locked longer than the approved retention is not shortened
    first = m.files[0]
    far = datetime.now(UTC).replace(microsecond=0) + timedelta(days=400)
    st.locks[first.store_version_id] = ("COMPLIANCE", far)
    _sign_off(st, keys, cache, lock.empty(), after, _session(_merged(_approved_pr())))
    got = {(k, v) for k, v, d in st.extended if d == DAYS}
    assert len(got) == len(st.extended)
    expected = {
        (store.run_file_key(NAME, "1.0.0", m.run.id, f.path), f.store_version_id)
        for f in m.files[1:]
    }
    expected |= {
        (store.run_doc_key(NAME, "1.0.0", m.run.id, d), r.store_version_id)
        for d, r in m.documents.items()
    }
    for key in (store.manifest_key(NAME, "1.0.0"), store.manifest_sig_key(NAME, "1.0.0")):
        expected.add((key, st.objects[key][0][0]))  # the first (recorded) version
    # signoff.json and .sig were written with the approved retention a moment ago: within
    # the same second extending them is "not longer", so they may or may not be listed
    just_written = {
        (key, st.objects[key][0][0])
        for key in (store.signoff_key(NAME, "1.0.0"), store.signoff_sig_key(NAME, "1.0.0"))
    }
    assert expected <= got <= expected | just_written
    soon = datetime.now(UTC) + timedelta(days=DAYS) - timedelta(minutes=1)
    for _, vid in just_written:
        held = st.locks[vid][1]
        assert held is not None and held >= soon
    assert all(v != shadow for _, v in got)
    assert st.locks[first.store_version_id] == ("COMPLIANCE", far)


def test_signoff_conflict_writes_nothing(tmp_path: Path, keys: Keys, cache: Path) -> None:
    """A refused sign-off writes nothing at all: no signature, no retention change."""
    st, pub, after = _new(tmp_path, keys)
    bad = signoff.build(
        name=NAME,
        version="1.0.0",
        role=ROLE,
        gate="phase1",
        manifest_sha256=pub.sha,
        evaluation_sha256=pub.m.evaluation.report_sha256,
        pr=7,
        merge_commit=COMMIT,
        clinical_reviewers=[CLINICAL],
        platform_reviewers=[PLATFORM],
        promotion_actor=ACTOR,
        trained_by=TRAINER,
        at="2026-11-06T09:09:09Z",  # not this merge's time: not the record this run builds
    )
    st.put_new(store.signoff_key(NAME, "1.0.0"), bad, 1)
    puts = list(st.puts)
    r = _reason(
        lambda: _sign_off(st, keys, cache, lock.empty(), after, _session(_merged(_approved_pr())))
    )
    assert r.detail_code == "signoff_conflict"
    assert st.puts == puts and st.extended == []


# ---------------------------------------------------------------- the command line


def _signoff_args(base: Path, before: Path, after: Path, commit: str = COMMIT) -> list[str]:
    return [
        "signoff",
        "--base-dir",
        str(base),
        "--before",
        str(before),
        "--after",
        str(after),
        "--commit",
        commit,
        "--testsets",
        str(base / "models" / "testsets.lock"),
        "--thresholds-dir",
        str(base / THRESHOLDS_REL),
    ]


def test_cli_signoff(
    tmp_path: Path, keys: Keys, monkeypatch: pytest.MonkeyPatch, capsys: Any
) -> None:
    st, _, after_lock = _new(tmp_path, keys)
    session = _session(_merged(_approved_pr()))
    base = _main_checkout(tmp_path / "parent")
    before, after = tmp_path / "before.lock", tmp_path / "after.lock"
    before.write_bytes(lock.to_bytes(lock.empty()))
    after.write_bytes(lock.to_bytes(after_lock))
    monkeypatch.setattr(cli, "_signoff_store", lambda: st)
    monkeypatch.setattr(cli, "_trust_policy", lambda path=None: keys.policy)
    monkeypatch.setattr(cli, "_github", lambda: lock.GitHubClient(session, GH_REPO))
    monkeypatch.setattr(cli, "_signoff_signer", lambda: DocSigner(keys.signoff))
    monkeypatch.delenv(cli.APPROVED_RETENTION_ENV, raising=False)
    args = _signoff_args(base, before, after)

    # the approved retention is required, with no default
    assert cli.main(args) == 2
    assert json.loads(capsys.readouterr().out) == {"refused": "not_allowed_here", "detail": "env"}
    monkeypatch.setenv(cli.APPROVED_RETENTION_ENV, str(DAYS))
    # --before must be the parent checkout's lock
    assert cli.main(_signoff_args(base, after, after)) == 2
    out = json.loads(capsys.readouterr().out)
    assert out == {"refused": "lock_invalid", "detail": "before_mismatch"}
    assert cli.main(_signoff_args(base, before, after, "C3" * 20)) == 2
    assert json.loads(capsys.readouterr().out) == {"refused": "invalid_name", "detail": "commit"}

    assert cli.main(args) == 0
    out = json.loads(capsys.readouterr().out)
    assert out["signoff"] == {ROLE: "signed_off"} and out["pr"] == 7
    assert cli.main(args) == 0
    assert json.loads(capsys.readouterr().out)["signoff"] == {ROLE: "already_signed_off"}

    session.status = 502
    assert cli.main(args) == 3
    out_text = capsys.readouterr().out
    assert json.loads(out_text) == {"refused": "github_unavailable", "detail": "http"}
    assert session.token not in out_text


def test_cli_signoff_signer_is_development_only(
    tmp_path: Path, keys: Keys, monkeypatch: pytest.MonkeyPatch, capsys: Any
) -> None:
    """Phase 0 has only the development FileSigner: outside development it refuses."""
    st, _, after_lock = _new(tmp_path, keys)
    base = _main_checkout(tmp_path / "parent")
    before, after = tmp_path / "before.lock", tmp_path / "after.lock"
    before.write_bytes(lock.to_bytes(lock.empty()))
    after.write_bytes(lock.to_bytes(after_lock))
    key = tmp_path / "dev-signing-key.pem"
    key.write_bytes(b"synthetic, never read")
    monkeypatch.setenv(cli.SIGNER_ENV, str(key))
    monkeypatch.setenv(cli.APPROVED_RETENTION_ENV, str(DAYS))
    monkeypatch.setattr(cli, "_signoff_store", lambda: st)
    monkeypatch.setattr(cli, "_trust_policy", lambda path=None: keys.policy)
    puts = list(st.puts)
    assert cli.main(_signoff_args(base, before, after)) == 2
    out = json.loads(capsys.readouterr().out)
    assert out == {"refused": "not_allowed_here", "detail": "not_development"}
    assert st.puts == puts


# ---------------------------------------------------------------- rollback


def _versions_store(
    tmp_path: Path, keys: Keys, signed: list[str], unsigned: list[str]
) -> tuple[RetainingStore, dict[str, Pub]]:
    st = RetainingStore()
    pubs: dict[str, Pub] = {}
    for i, v in enumerate(signed + unsigned):
        pubs[v] = publish(st, tmp_path, keys, version=v, seed=20 + i)
        if v in signed:
            write_signoff(st, pubs[v], keys)
    return st, pubs


def _rollback(
    st: MemoryStore, keys: Keys, cache: Path, path: Path, to: str | None = None
) -> lock.RollbackResult:
    return lock.rollback(
        ROLE, source=st, policy=keys.policy, cache_dir=cache, lock_path=path, to=to
    )


def test_rollback_picks_previous_signed_off(tmp_path: Path, keys: Keys, cache: Path) -> None:
    # semver, not string order: 1.9.0 < 1.10.0 although "1.9.0" > "1.10.0"
    st, pubs = _versions_store(
        tmp_path, keys, signed=["1.2.0", "1.9.0", "1.10.0", "1.11.0"], unsigned=["1.9.5"]
    )
    path = tmp_path / "models.lock"
    path.write_bytes(lock.to_bytes(lock.Lock({ROLE: pubs["1.10.0"].pin()})))
    done = _rollback(st, keys, cache, path)
    assert done.entry == pubs["1.9.0"].pin()
    assert done.previous == pubs["1.10.0"].pin()
    assert lock.read(path).models[ROLE] == pubs["1.9.0"].pin()
    # the pin written is one the loader accepts
    verify.verify_pin(st, ROLE, done.entry, keys.policy, cache, require_signoff=True)
    # --to an older signed-off version
    path.write_bytes(lock.to_bytes(lock.Lock({ROLE: pubs["1.10.0"].pin()})))
    assert _rollback(st, keys, cache, path, to="1.2.0").entry == pubs["1.2.0"].pin()
    assert lock.read(path).models[ROLE] == pubs["1.2.0"].pin()


def test_rollback_refuses_never_approved(tmp_path: Path, keys: Keys, cache: Path) -> None:
    """P16: only a version with a verifying sign-off for this role; present isn't enough."""
    st, pubs = _versions_store(tmp_path, keys, signed=["2.0.0"], unsigned=["1.0.0"])
    # 1.1.0: a signoff.json signed with the manifest key (not a sign-off signer)
    forged = publish(st, tmp_path, keys, version="1.1.0", seed=40)
    body = signoff.build(
        name=NAME,
        version="1.1.0",
        role=ROLE,
        gate="phase1",
        manifest_sha256=forged.sha,
        evaluation_sha256=forged.m.evaluation.report_sha256,
        pr=3,
        merge_commit=COMMIT,
        clinical_reviewers=[CLINICAL],
        platform_reviewers=[PLATFORM],
        promotion_actor=ACTOR,
        trained_by=TRAINER,
        at=MERGED_AT,
    )
    st.put_new(store.signoff_key(NAME, "1.1.0"), body, 1)
    st.put_new(store.signoff_sig_key(NAME, "1.1.0"), keys.manifest.sign("signoff", body), 1)
    # 1.2.0: signed off, but for another role
    other_role = publish(st, tmp_path, keys, version="1.2.0", seed=41)
    write_signoff(st, other_role, keys, role="ocr")
    path = tmp_path / "models.lock"
    pinned = lock.to_bytes(lock.Lock({ROLE: pubs["2.0.0"].pin()}))
    path.write_bytes(pinned)
    for to in ("1.0.0", "1.1.0", "1.2.0", "2.0.0", "3.0.0"):
        r = _reason(lambda t=to: _rollback(st, keys, cache, path, to=t))
        assert (r.reason, r.detail_code) == ("not_approved", "not_a_candidate")
        assert path.read_bytes() == pinned
    r = _reason(lambda: _rollback(st, keys, cache, path))
    assert (r.reason, r.detail_code) == ("not_approved", "no_candidate")
    assert path.read_bytes() == pinned
    # a role the lock doesn't pin
    r = _reason(
        lambda: lock.rollback("ocr", source=st, policy=keys.policy, cache_dir=cache, lock_path=path)
    )
    assert (r.reason, r.detail_code) == ("lock_invalid", "role")


def test_cli_rollback(
    tmp_path: Path, keys: Keys, monkeypatch: pytest.MonkeyPatch, capsys: Any
) -> None:
    st, pubs = _versions_store(tmp_path, keys, signed=["1.0.0", "1.1.0"], unsigned=[])
    path = tmp_path / "models.lock"
    path.write_bytes(lock.to_bytes(lock.Lock({ROLE: pubs["1.1.0"].pin()})))
    monkeypatch.setattr(cli, "_reader_store", lambda: st)
    monkeypatch.setattr(cli, "_trust_policy", lambda path=None: keys.policy)
    assert cli.main(["rollback", "../x", "--lock", str(path)]) == 2
    assert json.loads(capsys.readouterr().out) == {"refused": "invalid_name"}
    assert cli.main(["rollback", ROLE, "--to", "1.1.0", "--lock", str(path)]) == 2
    out = json.loads(capsys.readouterr().out)
    assert out == {"refused": "not_approved", "detail": "not_a_candidate"}
    assert cli.main(["rollback", ROLE, "--lock", str(path)]) == 0
    out = json.loads(capsys.readouterr().out)
    assert out == {
        "rollback": {
            "role": ROLE,
            "from": pubs["1.1.0"].pin().to_obj(),
            "to": pubs["1.0.0"].pin().to_obj(),
        }
    }


# ---------------------------------------------------------------- MinIO


@pytest.mark.store
def test_signoff_retention_extended(
    minio_store: MinioStore, tmp_path: Path, keys: Keys, cache: Path
) -> None:
    """On MinIO, as the signoff user: the record and signature written, and every object
    of the version locked in compliance mode for the approved retention."""
    name = f"syn-signoff-{secrets.token_hex(4)}"
    root = minio_store.store("root")
    pub = publish(root, tmp_path, keys, name=name)  # type: ignore[arg-type]
    after = lock.Lock({ROLE: pub.pin()})
    target = minio_store.store("signoff")
    started = datetime.now(UTC).replace(microsecond=0)
    result = signoff.sign_off(
        lock.to_bytes(lock.empty()),
        lock.to_bytes(after),
        COMMIT,
        github=lock.GitHubClient(_session(_merged(_approved_pr())), GH_REPO),
        reviewers=REVIEWERS,
        target=target,
        policy=keys.policy,
        signer=DocSigner(keys.signoff),
        cache_dir=cache,
        testsets_lock=TESTSETS,
        thresholds_dir=report.THRESHOLDS_DIR,
        retention_days=DAYS,
    )
    assert result.roles == {ROLE: "signed_off"}
    m = pub.m
    objects = [
        (store.run_file_key(name, "1.0.0", m.run.id, f.path), f.store_version_id) for f in m.files
    ]
    objects += [
        (store.run_doc_key(name, "1.0.0", m.run.id, d), r.store_version_id)
        for d, r in m.documents.items()
    ]
    for key in (
        store.manifest_key(name, "1.0.0"),
        store.manifest_sig_key(name, "1.0.0"),
        store.signoff_key(name, "1.0.0"),
        store.signoff_sig_key(name, "1.0.0"),
    ):
        vids = root.versions(key)
        assert len(vids) == 1
        objects.append((key, vids[0]))
    # signoff.json and .sig were just written with the approved retention: extending
    # them within the same second is "not longer" and leaves them as they are
    assert len(objects) - 2 <= result.retention_extended <= len(objects)
    for key, vid in objects:
        info = root.head(key, vid)
        assert info.lock_mode == "COMPLIANCE"
        assert info.retain_until is not None
        assert info.retain_until >= started + timedelta(days=DAYS)
    verify.verify_pin(target, ROLE, pub.pin(), keys.policy, cache, require_signoff=True)
    # a re-run writes nothing, and never shortens
    again = signoff.sign_off(
        lock.to_bytes(lock.empty()),
        lock.to_bytes(after),
        COMMIT,
        github=lock.GitHubClient(_session(_merged(_approved_pr())), GH_REPO),
        reviewers=REVIEWERS,
        target=target,
        policy=keys.policy,
        signer=DocSigner(keys.signoff),
        cache_dir=cache,
        testsets_lock=TESTSETS,
        thresholds_dir=report.THRESHOLDS_DIR,
        retention_days=1,
    )
    assert again.roles == {ROLE: "already_signed_off"} and again.retention_extended == 0
    assert len(root.versions(store.signoff_key(name, "1.0.0"))) == 1

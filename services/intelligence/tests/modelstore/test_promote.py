"""``mhai-models promote``: dry run and promotion, step by step (design section 7.1).

The tests marked ``store`` promote into the session's MinIO as the promoter. The rest use
an in-memory store (:mod:`tests.modelstore.fakes.memory_store`) that records every call,
the tiny fixture runs of :mod:`intelligence.modelstore.fixtures` behind a local source, and
a small hand-built dev split. No real weights, no network: a VLM endpoint is only ever
constructed, never called. Everything is synthetic.
"""

from __future__ import annotations

import hashlib
import json
import logging
import secrets
from collections.abc import Callable
from dataclasses import dataclass, replace
from pathlib import Path
from typing import Any

import pytest

from intelligence.doceval import extractors, openai_compat, report, tiny
from intelligence.doceval.synth import scanner
from intelligence.modelstore import (
    canonical,
    cli,
    files,
    fixtures,
    manifest,
    promote,
    signing,
    sources,
    store,
    testsets,
    training,
)
from intelligence.modelstore.reasons import Refused
from intelligence.modelstore.sources.local import LocalSource
from tests.doceval import minisets
from tests.modelstore.conftest import MinioStore
from tests.modelstore.fakes.memory_store import MemoryStore

REPO = Path(__file__).resolve().parents[4]
WARMUP_DIR = REPO / "testdata" / "models" / "warmup"
NAME = fixtures.MODEL_NAME
RUN = "run-0001"
MARKER = "Zqxmarkerwv"
WORKFLOW = promote.WorkflowRun(
    workflow_run_id="4242",
    workflow_ref="Measured-Health/mhai-multiagent/.github/workflows/model-promote.yml@refs/heads/main",
    commit="ab" * 20,
    actor="promoter-bot",
)
# A bad variant that the local source refuses while listing (a name off the allow-list,
# or a link), before Task 13's file checks see it: the source is the first wall.
SOURCE_REFUSES = frozenset({"pickle", "pkl", "py_file", "symlink"})


def _refused(fn: Callable[..., Any], *args: Any, **kw: Any) -> Refused:
    with pytest.raises(Refused) as e:
        fn(*args, **kw)
    return e.value


def _version() -> str:
    return f"0.{secrets.randbelow(10**6)}.{secrets.randbelow(1000)}"


# ---------------------------------------------------------------- fixtures


@pytest.fixture(autouse=True)
def _development(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv(signing.ENV_VAR, "development")
    monkeypatch.delenv(promote.EVAL_ROLE_ENV, raising=False)


@dataclass(frozen=True)
class Keys:
    signer: signing.FileSigner
    policy: signing.TrustPolicy
    key_path: Path
    policy_path: Path


def _entry(signer: signing.FileSigner) -> dict[str, Any]:
    return {
        "kind": "public_key",
        "alg": "ed25519",
        "key_id": signer.key_id,
        "public_key_pem": signer.public_key_pem(),
        "development": True,
    }


@pytest.fixture
def keys(tmp_path: Path) -> Keys:
    folder = tmp_path / "keys"
    signing.generate_dev_key(folder)
    key_path = folder / signing.DEV_KEY_FILE
    signer = signing.FileSigner(key_path)
    obj = {
        "schema": signing.TRUST_SCHEMA,
        "signers": {"manifest": [_entry(signer)], "signoff": [], "testset": [_entry(signer)]},
    }
    policy_path = tmp_path / "trust-policy.json"
    policy_path.write_bytes(canonical.dumps(obj))
    return Keys(signer, signing.parse_policy(obj, environment="development"), key_path, policy_path)


@pytest.fixture(scope="module")
def registry(tmp_path_factory: pytest.TempPathFactory) -> training.Registry:
    obj = json.loads((REPO / "models" / "approved-datasets.json").read_text(encoding="utf-8"))
    obj["datasets"].append(
        {
            "id": fixtures.NON_SYNTHETIC_DATASET,
            "kind": "deidentified",
            "generator": None,
            "version": "1.0.0",
            "testset_disjoint": True,
            "approved_by_pr": 1,
        }
    )
    path = tmp_path_factory.mktemp("registry") / "approved-datasets.json"
    path.write_text(json.dumps(obj), encoding="utf-8")
    return training.load_registry(path)


@pytest.fixture(scope="module")
def licences() -> training.Licences:
    return training.load_licences(REPO / "models" / "licences.json")


@pytest.fixture(scope="module")
def dev_set(tmp_path_factory: pytest.TempPathFactory) -> minisets.MiniSet:
    docs = [
        minisets.MiniDoc("lab_typed_clean", minisets.visible_truth(k, text=MARKER))
        for k in range(5)
    ] + [
        minisets.MiniDoc("lab_typed_poor", minisets.visible_truth(20 + k, text=MARKER))
        for k in range(5)
    ]
    return minisets.build(tmp_path_factory.mktemp("dev") / "set", docs)


def _source_root(tmp_path: Path, variant: str = "good", seed: int = 3) -> Path:
    root = tmp_path / "source"
    fixtures.make_run(root / "models" / NAME / RUN, variant, seed)
    return root


class Recording:
    """A source that records whether anything asked it for the run."""

    def __init__(self, inner: sources.Source) -> None:
        self.inner = inner
        self.calls: list[str] = []

    def list_run(self, name: str, run: str) -> list[sources.RemoteFile]:
        self.calls.append("list")
        return self.inner.list_run(name, run)

    def fetch(self, file: sources.RemoteFile, dest: Path, max_bytes: int) -> str:
        self.calls.append("fetch")
        return self.inner.fetch(file, dest, max_bytes)


def _ctx(
    root: Path,
    registry: training.Registry,
    licences: training.Licences,
    dev_set: minisets.MiniSet,
    *,
    recording: list[Recording] | None = None,
    **kw: Any,
) -> promote.Context:
    def source_for(scratch: sources.Scratch) -> sources.Source:
        src = Recording(LocalSource(root))
        if recording is not None:
            recording.append(src)
        return src

    base = promote.Context(
        registry=registry,
        licences=licences,
        source_for=source_for,
        split=promote.DevSplit(dev_set.root),
    )
    return replace(base, **kw)


def _request(version: str = "1.0.0", **kw: Any) -> promote.Request:
    return replace(promote.Request(name=NAME, version=version, source="local", run=RUN), **kw)


def _write_ctx(ctx: promote.Context, target: Any, keys: Keys) -> promote.Context:
    return replace(
        ctx,
        store=target,
        signer=keys.signer,
        policy=keys.policy,
        workflow=WORKFLOW,
        retention_days=1,
    )


# ---------------------------------------------------------------- step 1: the inputs


@pytest.mark.parametrize(
    ("change", "reason"),
    [
        ({"name": "Bad Name"}, "invalid_name"),
        ({"version": "1.0"}, "invalid_name"),
        ({"run": "../x"}, "invalid_name"),
        ({"source": "s3"}, "invalid_name"),
        ({"expect_files": "abc"}, "invalid_name"),
        ({"gates": ("prod",)}, "invalid_name"),
        ({"hf_repo": "org/repo"}, "invalid_name"),  # hf options without --source hf
        ({"source": "hf"}, "invalid_name"),  # hf without repo and revision
        ({"source": "hf", "hf_repo": "org/repo", "hf_revision": "main"}, "revision_unpinned"),
        ({"source": "drive"}, "invalid_name"),  # no folder id
    ],
)
def test_inputs_checked_before_any_io(change: dict[str, Any], reason: str) -> None:
    assert _refused(promote.check_request, _request(**change)).reason == reason


@pytest.mark.parametrize(
    "url",
    [
        "http://10.0.0.5:8000/v1",
        "https://models.example.org/v1",
        "http://localhost:8000/v1",  # a name, not a loopback literal
        "http://127.0.0.1:8000/v1?x=1",
        "http://user@127.0.0.1:8000/v1",
        "ftp://127.0.0.1/v1",
    ],
)
def test_runtime_url_must_be_loopback(url: str) -> None:
    err = _refused(promote.check_request, _request(runtime_url=url, dry_run=True))
    assert (err.reason, err.detail_code) == ("not_allowed_here", "runtime_url")


def test_loopback_runtime_url_accepted_for_a_dry_run() -> None:
    for url in ("http://127.0.0.1:8000/v1", "http://[::1]:8000/v1"):
        promote.check_request(_request(runtime_url=url, dry_run=True))


def test_runtime_url_refused_for_a_promotion_in_phase_0() -> None:
    # TODO(GPU task): the hash handshake proves the server serves the fetched files.
    err = _refused(
        promote.check_request, _request(runtime_url="http://127.0.0.1:8000/v1", dry_run=False)
    )
    assert (err.reason, err.detail_code) == ("no_runtime", "runtime_unverified")


def test_promote_requires_write_credentials_and_signer(
    tmp_path: Path,
    keys: Keys,
    registry: training.Registry,
    licences: training.Licences,
    dev_set: minisets.MiniSet,
) -> None:
    root = _source_root(tmp_path)
    mem = MemoryStore()
    seen: list[Recording] = []
    full = _write_ctx(_ctx(root, registry, licences, dev_set, recording=seen), mem, keys)
    for missing in ("store", "signer", "policy", "workflow", "retention_days"):
        ctx = replace(full, **{missing: None})
        err = _refused(promote.promote, _request(_version(), dry_run=False), ctx)
        assert err.reason == "not_allowed_here", missing
    untrusted = signing.parse_policy(
        {"schema": signing.TRUST_SCHEMA, "signers": {"manifest": [], "signoff": [], "testset": []}},
        environment="development",
    )
    err = _refused(
        promote.promote, _request(_version(), dry_run=False), replace(full, policy=untrusted)
    )
    assert err.reason == "signer_untrusted"
    assert mem.calls == [] and seen == []  # refused before the store or the source


# ---------------------------------------------------------------- step 2: reserve


def test_reserve_writes_the_claim_first(keys: Keys) -> None:
    mem = MemoryStore()
    claim = promote.reserve(mem, NAME, "1.2.3", RUN, WORKFLOW.actor, 1)
    key = store.claim_key(NAME, "1.2.3")
    assert mem.puts == [key]
    body = json.loads(mem.get(key, claim.version_id, max_bytes=4096))
    assert body["schema"] == promote.CLAIM_SCHEMA
    assert (body["run_id"], body["actor"]) == (RUN, WORKFLOW.actor)
    err = _refused(promote.reserve, mem, NAME, "1.2.3", RUN, WORKFLOW.actor, 1)
    assert err.reason == "version_exists"  # a second run, even with the same ids


def test_retried_claim_that_is_ours_carries_on() -> None:
    key = store.claim_key(NAME, "1.2.4")
    mem = MemoryStore(lost_reply=frozenset({key}))  # stored, then the reply was lost
    claim = promote.reserve(mem, NAME, "1.2.4", RUN, WORKFLOW.actor, 1)
    assert mem.versions(key) == [claim.version_id]


def test_put_once_refuses_an_object_that_is_not_ours() -> None:
    mem = MemoryStore()
    key = store.claim_key(NAME, "1.2.5")
    mem.put_new(key, b"someone else", 1)
    body = b"ours"
    sha = hashlib.sha256(body).hexdigest()
    assert _refused(promote.put_once, mem, key, body, len(body), sha, 1).reason == (
        "version_exists"
    )
    # ours, but shadowed by a second version: not provably ours any more
    key2 = store.claim_key(NAME, "1.2.6")
    mem2 = MemoryStore(lost_reply=frozenset({key2}))
    with pytest.raises(Refused):
        mem2.put_new(key2, body, 1)
    mem2.shadow(key2, body)
    assert _refused(promote.put_once, mem2, key2, body, len(body), sha, 1).reason == (
        "version_exists"
    )


@pytest.mark.store
def test_version_exists_refused_before_download(
    minio_store: MinioStore,
    tmp_path: Path,
    keys: Keys,
    registry: training.Registry,
    licences: training.Licences,
    dev_set: minisets.MiniSet,
) -> None:
    version = _version()
    target = minio_store.store("promoter")
    target.put_new(store.claim_key(NAME, version), b'{"another":"run"}', 1)
    seen: list[Recording] = []
    ctx = _write_ctx(
        _ctx(_source_root(tmp_path), registry, licences, dev_set, recording=seen), target, keys
    )
    err = _refused(promote.promote, _request(version, dry_run=False), ctx)
    assert err.reason == "version_exists"
    assert seen == []  # P7: the source was never asked for anything
    assert target.list(f"models/{NAME}/{version}/") == [store.claim_key(NAME, version)]


# ---------------------------------------------------------------- step 3: fetch


def test_files_hash_is_canonical_over_the_sorted_list() -> None:
    a = files.FileEntry("model.safetensors", 10, "aa" * 32)
    b = files.FileEntry("config.json", 2, "bb" * 32)
    expected = hashlib.sha256(
        canonical.dumps(
            [
                {"path": "config.json", "size": 2, "sha256": "bb" * 32},
                {"path": "model.safetensors", "size": 10, "sha256": "aa" * 32},
            ]
        )
    ).hexdigest()
    assert promote.files_hash([a, b]) == promote.files_hash([b, a]) == expected


def test_expect_files_mismatch_refused(
    tmp_path: Path,
    registry: training.Registry,
    licences: training.Licences,
    dev_set: minisets.MiniSet,
) -> None:
    ctx = _ctx(_source_root(tmp_path), registry, licences, dev_set)
    seen = promote.promote(_request(), ctx)
    assert seen.dry_run and seen.manifest_sha256 is None
    promote.promote(_request(expect_files=seen.files_sha256), ctx)  # the same files pass
    err = _refused(promote.promote, _request(expect_files="0" * 64), ctx)
    assert err.reason == "hash_mismatch"  # P26


def test_scratch_removed_on_success_and_failure(
    tmp_path: Path,
    registry: training.Registry,
    licences: training.Licences,
    dev_set: minisets.MiniSet,
) -> None:
    parent = tmp_path / "scratch"
    parent.mkdir(mode=0o700)
    ctx = _ctx(_source_root(tmp_path), registry, licences, dev_set, scratch_parent=parent)
    promote.promote(_request(), ctx)
    _refused(promote.promote, _request(expect_files="0" * 64), ctx)
    assert list(parent.iterdir()) == []


# ---------------------------------------------------------------- step 4: checks


@pytest.mark.parametrize("variant", [v for v in fixtures.VARIANTS if not v.startswith("good")])
def test_each_bad_fixture_refused_with_its_reason(
    tmp_path: Path,
    variant: str,
    registry: training.Registry,
    licences: training.Licences,
    dev_set: minisets.MiniSet,
) -> None:
    # B16, B17, B20, B21, P6, P23: Task 13's file checks and Task 14's training checks,
    # through the whole dry run, and through ``check`` on the folder itself.
    expected = fixtures.EXPECTED_REASONS[variant]
    root = _source_root(tmp_path, variant)
    ctx = _ctx(root, registry, licences, dev_set)
    err = _refused(promote.promote, _request(), ctx)
    assert err.reason == ("source_invalid" if variant in SOURCE_REFUSES else expected), variant
    err = _refused(promote.check_folder, root / "models" / NAME / RUN, registry, licences)
    assert err.reason == expected, variant


def test_good_fixture_passes_the_folder_check(
    tmp_path: Path, registry: training.Registry, licences: training.Licences
) -> None:
    run = fixtures.make_run(tmp_path / "run", "good", 3)
    checked = promote.check_folder(run, registry, licences)
    assert checked.files_sha256 == promote.files_hash(files.check_run(run))
    assert checked.training.manifest.role == "extract"


def test_name_must_match_the_training_manifest(
    tmp_path: Path,
    registry: training.Registry,
    licences: training.Licences,
    dev_set: minisets.MiniSet,
) -> None:
    root = tmp_path / "source"
    fixtures.make_run(root / "models" / "other-model" / RUN, "good", 3)
    ctx = _ctx(root, registry, licences, dev_set)
    err = _refused(promote.promote, replace(_request(), name="other-model"), ctx)
    assert err.reason == "identity_mismatch"


def test_file_too_large_for_one_put_refused(
    tmp_path: Path,
    registry: training.Registry,
    licences: training.Licences,
    dev_set: minisets.MiniSet,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    # TODO(Task 27): a conditional multipart upload; phase 0 fixtures are tiny.
    monkeypatch.setattr(store, "MAX_PUT_BYTES", 256)
    ctx = _ctx(_source_root(tmp_path), registry, licences, dev_set)
    err = _refused(promote.promote, _request(), ctx)
    assert (err.reason, err.detail_code) == ("too_large", "file_too_large_for_store")


# ---------------------------------------------------------------- step 5: evaluation


def test_suite_from_the_role() -> None:
    assert promote.suite_for("extract") == "clinical_document"
    assert _refused(promote.suite_for, "summarise").reason == "no_suite"


def _with_architecture(run: Path, arch: Any) -> None:
    cfg = json.loads((run / "config.json").read_bytes())
    cfg["architectures"] = arch
    (run / "config.json").write_bytes(canonical.dumps(cfg))


@pytest.mark.parametrize(
    "arch",
    [
        ["LlamaForCausalLM"],
        ["OracleExtractor"],  # a stub's name is never a runtime
        ["MhaiTinyExtractor", "MhaiTinyExtractor"],
        [],
        "MhaiTinyExtractor",
    ],
)
def test_no_runtime_for_unknown_architecture(tmp_path: Path, arch: Any) -> None:
    run = fixtures.make_run(tmp_path / "run", "good", 3)
    _with_architecture(run, arch)
    err = _refused(promote.choose_extractor, run, NAME, "extract", "1.0.0", None)
    assert err.reason == "no_runtime"


def test_no_runtime_through_the_dry_run(
    tmp_path: Path,
    registry: training.Registry,
    licences: training.Licences,
    dev_set: minisets.MiniSet,
) -> None:
    root = _source_root(tmp_path)
    _with_architecture(root / "models" / NAME / RUN, ["LlamaForCausalLM"])
    ctx = _ctx(root, registry, licences, dev_set)
    assert _refused(promote.promote, _request(), ctx).reason == "no_runtime"


def test_tiny_architecture_picks_the_tiny_extractor(tmp_path: Path) -> None:
    run = fixtures.make_run(tmp_path / "run", "good", 3)
    chosen = promote.choose_extractor(run, NAME, "extract", "1.0.0", None)
    assert isinstance(chosen.extractor, tiny.TinyExtractor)
    info = chosen.extractor.info()
    assert (info.engine, info.hardware) == ("numpy", "cpu")
    assert info.model_versions == {"extract": "1.0.0"}
    assert chosen.engine == "numpy" and chosen.engine_version
    assert not report.is_stub(info)


def test_vlm_architecture_needs_the_runtime_url(tmp_path: Path) -> None:
    run = fixtures.make_run(tmp_path / "run", "good", 3)
    arch = sorted(promote.KNOWN_VLM_ARCHITECTURES)[0]
    _with_architecture(run, [arch])
    err = _refused(promote.choose_extractor, run, NAME, "extract", "1.0.0", None)
    assert (err.reason, err.detail_code) == ("no_runtime", "runtime_url")
    chosen = promote.choose_extractor(run, NAME, "extract", "1.0.0", "http://127.0.0.1:9/v1")
    assert isinstance(chosen.extractor, openai_compat.OpenAICompatExtractor)
    info = chosen.extractor.info()
    # The runner sets engine and hardware; the confidences are not measured (PR-A).
    assert (info.engine, info.hardware, info.confidence_scored) == ("vllm", "unknown", False)


def test_stubs_are_unreachable_from_promote() -> None:
    text = Path(promote.__file__).read_text(encoding="utf-8")
    for stub in ("OracleExtractor", "RefusingExtractor", "SlowExtractor", "InvalidExtractor"):
        assert stub not in text
    for arch in promote.KNOWN_VLM_ARCHITECTURES | {tiny.ARCHITECTURE}:
        assert not arch.endswith("-stub") and "Oracle" not in arch


def test_stub_extractor_refused_even_if_chosen(
    tmp_path: Path, dev_set: minisets.MiniSet, monkeypatch: pytest.MonkeyPatch
) -> None:
    stub = extractors.RefusingExtractor("unreadable")
    chosen = promote.Chosen(stub, "stub", None)
    err = _refused(
        promote.evaluate, chosen, dev_set.root, (report.packaged_thresholds("rehearsal"),), None
    )
    assert err.reason == "not_allowed_here"


def test_select_split() -> None:
    reader = MemoryStore()
    entry = testsets.LockEntry("clinical_document", "1.0.0", "ab" * 32)
    policy = signing.parse_policy(
        {"schema": signing.TRUST_SCHEMA, "signers": {"manifest": [], "signoff": [], "testset": []}},
        environment="development",
    )
    dev = promote.select_split({}, reader=reader, lock_entry=entry, policy=policy, dev_set=None)
    assert isinstance(dev, promote.DevSplit)  # outside the eval environments: dev, always
    locked = promote.select_split(
        {promote.EVAL_ROLE_ENV: "locked"},
        reader=reader,
        lock_entry=entry,
        policy=policy,
        dev_set=None,
    )
    assert isinstance(locked, promote.LockedSplit)


@pytest.mark.parametrize("missing", ["reader", "lock_entry", "policy"])
def test_locked_split_needs_eval_role(missing: str) -> None:
    # P19: the locked split only with the eval reader's credentials, the lock and the policy.
    kw: dict[str, Any] = {
        "reader": MemoryStore(),
        "lock_entry": testsets.LockEntry("clinical_document", "1.0.0", "ab" * 32),
        "policy": signing.parse_policy(
            {
                "schema": signing.TRUST_SCHEMA,
                "signers": {"manifest": [], "signoff": [], "testset": []},
            },
            environment="development",
        ),
        "dev_set": None,
    }
    kw[missing] = None
    err = _refused(promote.select_split, {promote.EVAL_ROLE_ENV: "locked"}, **kw)
    assert err.reason == "not_allowed_here"


def test_unknown_eval_role_refused() -> None:
    err = _refused(
        promote.select_split,
        {promote.EVAL_ROLE_ENV: "dev"},
        reader=None,
        lock_entry=None,
        policy=None,
        dev_set=None,
    )
    assert err.reason == "not_allowed_here"


def test_dry_run_outside_eval_env_uses_dev_split(
    tmp_path: Path,
    registry: training.Registry,
    licences: training.Licences,
    dev_set: minisets.MiniSet,
) -> None:
    ctx = _ctx(_source_root(tmp_path), registry, licences, dev_set)
    summary = promote.promote(_request(), ctx)
    assert summary.split == "dev"
    rep = report.parse_report(summary.documents["evaluation.json"])
    assert rep["split"] == {"name": "dev", "label": report.DEV_LABEL}
    assert "development split" in summary.documents["model_card.md"].decode()
    assert set(summary.verdicts) == set(report.GATES)
    out = summary.to_obj()
    assert out["gates_failed"] == sorted(g for g, v in summary.verdicts.items() if v != "pass")


def test_a_test_split_is_never_the_dev_split(
    tmp_path: Path,
    registry: training.Registry,
    licences: training.Licences,
) -> None:
    docs = [minisets.MiniDoc("lab_typed_clean", minisets.visible_truth(k)) for k in range(5)]
    mini = minisets.build(tmp_path / "set", docs)
    path = mini.root / "testset.json"
    obj = json.loads(path.read_bytes())
    obj["purpose"] = "test"
    path.write_bytes(canonical.dumps(obj))
    ctx = _ctx(_source_root(tmp_path), registry, licences, mini)
    err = _refused(promote.promote, _request(), ctx)
    assert (err.reason, err.detail_code) == ("not_allowed_here", "split")


def test_locked_split_fetched_inside_the_eval_env(
    tmp_path: Path,
    registry: training.Registry,
    licences: training.Licences,
    keys: Keys,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    docs = [minisets.MiniDoc("lab_typed_clean", minisets.visible_truth(k)) for k in range(5)]
    mini = minisets.build(tmp_path / "locked", docs)
    path = mini.root / "testset.json"
    obj = json.loads(path.read_bytes())
    obj["purpose"] = "test"
    raw = canonical.dumps(obj)
    path.write_bytes(raw)
    entry = testsets.LockEntry("clinical_document", "1.0.0", hashlib.sha256(raw).hexdigest())
    reader = MemoryStore()
    seen: list[Any] = []

    def fake_fetch(source: Any, lock_entry: Any, policy: Any, scratch: Any) -> Path:
        seen.append((source, lock_entry, policy))
        return mini.root

    monkeypatch.setattr(testsets, "fetch", fake_fetch)
    split = promote.LockedSplit(reader, entry, keys.policy)
    ctx = _ctx(_source_root(tmp_path), registry, licences, mini, split=split)
    summary = promote.promote(_request(), ctx)
    assert seen == [(reader, entry, keys.policy)]
    assert summary.split == "test"
    assert summary.testset == f"clinical_document@1.0.0#{entry.sha256}"


# ---------------------------------------------------------------- step 6: warm-up


def test_warmup_page_is_synthetic_and_reproducible() -> None:
    result = scanner.scan_set(WARMUP_DIR)
    assert result.ok, [str(f) for f in result.findings]
    png, truth = fixtures.warmup_page()
    assert (WARMUP_DIR / "page.json").read_bytes() == fixtures.warmup_truth_bytes(truth)
    from io import BytesIO

    from PIL import Image

    with Image.open(BytesIO(png)) as fresh, Image.open(WARMUP_DIR / "page.png") as kept:
        assert fresh.size == kept.size
        assert fresh.tobytes() == kept.tobytes()
    assert len((WARMUP_DIR / "page.png").read_bytes()) < 200 * 1024


def test_warmup_record(tmp_path: Path) -> None:
    run = fixtures.make_run(tmp_path / "run", "good", 3)
    chosen = promote.choose_extractor(run, NAME, "extract", "1.0.0", None)
    raw = promote.warmup(chosen, promote.WARMUP_PAGE)
    rec = json.loads(raw)
    assert set(rec) == {"page_sha256", "output_sha256", "engine", "engine_version"}
    assert rec["page_sha256"] == hashlib.sha256(promote.WARMUP_PAGE.read_bytes()).hexdigest()
    assert rec["engine"] == "numpy"
    assert canonical.dumps(rec) == raw
    assert promote.warmup(chosen, promote.WARMUP_PAGE) == raw  # deterministic
    other = fixtures.make_run(tmp_path / "run2", "good_v2", 3)
    again = promote.choose_extractor(other, NAME, "extract", "1.0.0", None)
    assert (
        json.loads(promote.warmup(again, promote.WARMUP_PAGE))["output_sha256"]
        != (rec["output_sha256"])
    )


# ---------------------------------------------------------------- step 8: write


def test_promote_writes_in_order_and_the_manifest_last(
    tmp_path: Path,
    keys: Keys,
    registry: training.Registry,
    licences: training.Licences,
    dev_set: minisets.MiniSet,
) -> None:
    mem = MemoryStore()
    version = "2.0.0"
    ctx = _write_ctx(_ctx(_source_root(tmp_path), registry, licences, dev_set), mem, keys)
    summary = promote.promote(_request(version, dry_run=False), ctx)
    puts = mem.puts
    assert puts[0] == store.claim_key(NAME, version)
    assert puts[-2:] == [store.manifest_sig_key(NAME, version), store.manifest_key(NAME, version)]
    middle = puts[1:-2]
    assert middle and all(k.startswith(f"models/{NAME}/{version}/runs/{RUN}/") for k in middle)
    raw = mem.get(store.manifest_key(NAME, version), max_bytes=manifest.MAX_BYTES)
    assert manifest.sha256(raw) == summary.manifest_sha256
    m = manifest.parse(raw)  # it is manifest.build output: canonical and strict
    assert (m.run.id, m.run.source, m.run.source_ref) == (RUN, "local", "fixture")
    assert m.promotion.actor == WORKFLOW.actor
    assert set(m.evaluation.gates) == set(report.GATES)
    sig = mem.get(store.manifest_sig_key(NAME, version), max_bytes=4096)
    signer = signing.Verifier().verify("manifest", raw, sig, keys.policy)
    assert signer.key_id == keys.signer.key_id


def test_retried_file_put_that_is_ours_carries_on(
    tmp_path: Path,
    keys: Keys,
    registry: training.Registry,
    licences: training.Licences,
    dev_set: minisets.MiniSet,
) -> None:
    version = "2.0.1"
    key = store.run_file_key(NAME, version, RUN, "model.safetensors")
    mem = MemoryStore(lost_reply=frozenset({key, store.claim_key(NAME, version)}))
    ctx = _write_ctx(_ctx(_source_root(tmp_path), registry, licences, dev_set), mem, keys)
    summary = promote.promote(_request(version, dry_run=False), ctx)
    m = manifest.parse(mem.get(store.manifest_key(NAME, version), max_bytes=manifest.MAX_BYTES))
    rec = {f.path: f for f in m.files}["model.safetensors"]
    assert mem.versions(key) == [rec.store_version_id]
    assert summary.manifest_sha256


def test_crash_before_manifest_leaves_no_version_in_memory(
    tmp_path: Path,
    keys: Keys,
    registry: training.Registry,
    licences: training.Licences,
    dev_set: minisets.MiniSet,
) -> None:
    version = "2.0.2"
    sig_key = store.manifest_sig_key(NAME, version)

    def kill(key: str) -> BaseException | None:
        return SystemExit(137) if key == sig_key else None

    mem = MemoryStore(fail=kill)
    ctx = _write_ctx(_ctx(_source_root(tmp_path), registry, licences, dev_set), mem, keys)
    with pytest.raises(SystemExit):
        promote.promote(_request(version, dry_run=False), ctx)
    assert mem.versions(store.manifest_key(NAME, version)) == []
    assert store.claim_key(NAME, version) in mem.objects


def test_promote_logs_ids_only(
    tmp_path: Path,
    keys: Keys,
    registry: training.Registry,
    licences: training.Licences,
    dev_set: minisets.MiniSet,
    caplog: pytest.LogCaptureFixture,
) -> None:
    mem = MemoryStore()
    ctx = _write_ctx(_ctx(_source_root(tmp_path), registry, licences, dev_set), mem, keys)
    with caplog.at_level(logging.DEBUG):
        promote.promote(_request("2.0.3", dry_run=False), ctx)
        _refused(promote.promote, _request(expect_files="0" * 64), ctx)
    text = caplog.text
    assert "promote" in text
    assert MARKER not in text and MARKER.lower() not in text
    assert str(tmp_path) not in text
    assert "PRIVATE KEY" not in text and keys.key_path.read_text() not in text
    weights = (tmp_path / "source" / "models" / NAME / RUN / "model.safetensors").read_bytes()
    assert weights[8:40].decode("latin-1") not in text
    for doc_id in dev_set.ids:
        assert doc_id not in text


# ---------------------------------------------------------------- the store


@dataclass
class Killing:
    """A promoter store that dies (as a SIGKILL would) when ``manifest.sig`` goes up."""

    inner: store.S3Store

    def put_new(self, key: str, data: bytes | Path, retention_days: int) -> str:
        if key.endswith("/manifest.sig"):
            raise SystemExit(137)
        return self.inner.put_new(key, data, retention_days)

    def get(self, key: str, version_id: str | None = None, *, max_bytes: int) -> bytes:
        return self.inner.get(key, version_id, max_bytes=max_bytes)

    def versions(self, key: str) -> list[str]:
        return self.inner.versions(key)

    def head(self, key: str, version_id: str | None = None) -> store.ObjectInfo:
        return self.inner.head(key, version_id)

    def list(self, prefix: str) -> list[str]:
        return self.inner.list(prefix)

    def extend_retention(self, key: str, version_id: str, days: int) -> Any:
        return self.inner.extend_retention(key, version_id, days)


def _all_keys(m: MinioStore) -> list[str]:
    return m.store("root").list("models/")


@pytest.mark.store
def test_dry_run_writes_nothing(
    minio_store: MinioStore,
    tmp_path: Path,
    keys: Keys,
    registry: training.Registry,
    licences: training.Licences,
    dev_set: minisets.MiniSet,
) -> None:
    # B23: even with the promoter's store in reach, a dry run writes nothing.
    version = _version()
    before = _all_keys(minio_store)
    ctx = _write_ctx(
        _ctx(_source_root(tmp_path), registry, licences, dev_set),
        minio_store.store("promoter"),
        keys,
    )
    summary = promote.promote(_request(version, dry_run=True), ctx)
    assert summary.manifest_sha256 is None
    assert _all_keys(minio_store) == before
    assert minio_store.store("root").versions(store.claim_key(NAME, version)) == []


@pytest.mark.store
def test_promote_good_fixture(
    minio_store: MinioStore,
    tmp_path: Path,
    keys: Keys,
    registry: training.Registry,
    licences: training.Licences,
    dev_set: minisets.MiniSet,
) -> None:
    version = _version()
    target = minio_store.store("promoter")
    ctx = _write_ctx(_ctx(_source_root(tmp_path), registry, licences, dev_set), target, keys)
    summary = promote.promote(_request(version, dry_run=False), ctx)
    reader = minio_store.store("root")
    mkey = store.manifest_key(NAME, version)
    (vid,) = reader.versions(mkey)
    raw = reader.get(mkey, vid, max_bytes=manifest.MAX_BYTES)
    assert manifest.sha256(raw) == summary.manifest_sha256
    m = manifest.parse(raw)
    sig = reader.get(store.manifest_sig_key(NAME, version), max_bytes=4096)
    signing.Verifier().verify("manifest", raw, sig, keys.policy)
    for f in m.files:
        info = reader.head(store.run_file_key(NAME, version, RUN, f.path), f.store_version_id)
        assert (info.size, info.sha256, info.lock_mode) == (f.size, f.sha256, "COMPLIANCE")
    for doc, d in m.documents.items():
        info = reader.head(store.run_doc_key(NAME, version, RUN, doc), d.store_version_id)
        assert info.sha256 == d.sha256
    # the manifest is the newest object of the version: written last
    claim = reader.head(store.claim_key(NAME, version))
    assert claim.lock_mode == "COMPLIANCE"
    assert summary.files_sha256 == promote.files_hash(
        [files.FileEntry(f.path, f.size, f.sha256) for f in m.files]
    )


@pytest.mark.store
def test_crash_before_manifest_leaves_no_version(
    minio_store: MinioStore,
    tmp_path: Path,
    keys: Keys,
    registry: training.Registry,
    licences: training.Licences,
    dev_set: minisets.MiniSet,
) -> None:
    # P8: killed after the files went up. `verify` is Task 22; until then, no manifest.
    version = _version()
    target = Killing(minio_store.store("promoter"))
    ctx = _write_ctx(_ctx(_source_root(tmp_path), registry, licences, dev_set), target, keys)
    with pytest.raises(SystemExit):
        promote.promote(_request(version, dry_run=False), ctx)
    reader = minio_store.store("root")
    assert reader.versions(store.manifest_key(NAME, version)) == []
    assert _refused(reader.head, store.manifest_key(NAME, version)).reason == "not_found"
    keys_now = reader.list(f"models/{NAME}/{version}/")
    assert store.claim_key(NAME, version) in keys_now
    assert any("/runs/" in k for k in keys_now)


# ---------------------------------------------------------------- the CLI


def _cli(argv: list[str], capsys: pytest.CaptureFixture[str]) -> tuple[int, str]:
    code = cli.main(argv)
    return code, capsys.readouterr().out


def test_cli_promote_checks_names_first(capsys: pytest.CaptureFixture[str]) -> None:
    code, out = _cli(
        ["promote", "Bad", "1.0.0", "--source", "local", "--run", RUN, "--dry-run"], capsys
    )
    assert code == 2 and json.loads(out) == {"refused": "invalid_name"}


def test_cli_dry_run_prints_one_summary_with_the_files_hash(
    tmp_path: Path,
    dev_set: minisets.MiniSet,
    capsys: pytest.CaptureFixture[str],
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    root = _source_root(tmp_path)
    monkeypatch.setenv(cli.LOCAL_ROOT_ENV, str(root))
    argv = ["promote", NAME, "1.0.0", "--source", "local", "--run", RUN, "--dry-run"]
    code, out = _cli([*argv, "--dev-set", str(dev_set.root)], capsys)
    assert code == 0, out
    lines = out.splitlines()
    assert len(lines) == 1
    summary = json.loads(lines[0])
    expected = promote.files_hash(files.check_run(root / "models" / NAME / RUN))
    assert summary["files_sha256"] == expected
    assert summary["dry_run"] is True and summary["manifest_sha256"] is None
    assert summary["split"] == "dev"
    assert MARKER not in out


def test_cli_promote_without_write_credentials_refused(
    tmp_path: Path,
    dev_set: minisets.MiniSet,
    capsys: pytest.CaptureFixture[str],
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setenv(cli.LOCAL_ROOT_ENV, str(_source_root(tmp_path)))
    for name in (cli.PROMOTER_ACCESS_ENV, cli.PROMOTER_SECRET_ENV, cli.SIGNER_ENV):
        monkeypatch.delenv(name, raising=False)
    argv = ["promote", NAME, "1.0.0", "--source", "local", "--run", RUN]
    code, out = _cli([*argv, "--dev-set", str(dev_set.root)], capsys)
    assert code == 2 and json.loads(out)["refused"] == "not_allowed_here"


def test_cli_store_unavailable_is_exit_3(
    tmp_path: Path,
    keys: Keys,
    dev_set: minisets.MiniSet,
    capsys: pytest.CaptureFixture[str],
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setenv(cli.LOCAL_ROOT_ENV, str(_source_root(tmp_path)))
    secret = "s3cr3t-" + secrets.token_hex(8)
    for k, v in {
        cli.ENDPOINT_ENV: "http://models.example.org:9000",  # plain HTTP off loopback
        cli.BUCKET_ENV: "mhai-models",
        cli.PROMOTER_ACCESS_ENV: "promoter",
        cli.PROMOTER_SECRET_ENV: secret,
        cli.SIGNER_ENV: str(keys.key_path),
        cli.TRUST_POLICY_ENV: str(keys.policy_path),
    }.items():
        monkeypatch.setenv(k, v)
    argv = ["promote", NAME, "1.0.0", "--source", "local", "--run", RUN]
    code, out = _cli([*argv, "--dev-set", str(dev_set.root)], capsys)
    assert code == 3 and json.loads(out)["refused"] == "store_unavailable"
    assert secret not in out


def test_cli_drive_source_unavailable_until_task_28(
    capsys: pytest.CaptureFixture[str], monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setenv(cli.DRIVE_FOLDER_ENV, "0ABCdefGHIjklMNOpqr")
    argv = ["promote", NAME, "1.0.0", "--source", "drive", "--run", RUN, "--dry-run"]
    code, out = _cli(argv, capsys)
    assert code == 3 and json.loads(out)["refused"] == "source_unavailable"


def test_cli_make_fixture_and_check(
    tmp_path: Path, capsys: pytest.CaptureFixture[str], monkeypatch: pytest.MonkeyPatch
) -> None:
    good = tmp_path / "good"
    code, out = _cli(["make-fixture", str(good), "--variant", "good", "--seed", "3"], capsys)
    assert code == 0, out
    code, out = _cli(["check", str(good)], capsys)
    assert code == 0, out
    assert json.loads(out)["files_sha256"] == promote.files_hash(files.check_run(good))
    bad = tmp_path / "bad"
    assert _cli(["make-fixture", str(bad), "--variant", "pickle"], capsys)[0] == 0
    code, out = _cli(["check", str(bad)], capsys)
    assert code == 2 and json.loads(out)["refused"] == "pickle_refused"
    code, out = _cli(["make-fixture", str(tmp_path / "x"), "--variant", "nope"], capsys)
    assert code == 2 and json.loads(out)["refused"] == "fixture_name"


@pytest.mark.parametrize(
    ("env", "url"),
    [
        ("production", "http://127.0.0.1:9000"),
        ("development", "http://minio.example.org:9000"),
        ("development", "https://10.1.2.3:9000"),
    ],
)
def test_cli_store_init_rehearsal_only(
    env: str, url: str, capsys: pytest.CaptureFixture[str], monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setenv(signing.ENV_VAR, env)
    monkeypatch.setenv(cli.MINIO_URL_ENV, url)
    code, out = _cli(["store-init"], capsys)
    assert code == 2 and json.loads(out)["refused"] == "not_allowed_here"

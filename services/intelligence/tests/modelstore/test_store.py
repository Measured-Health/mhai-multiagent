"""The model store: conditional create, compliance-mode object lock, per-role policies.

The tests marked ``store`` run against MinIO provisioned by deploy/minio/init-model-store.sh
(see conftest.py); the rest need no Docker. Every object is synthetic, under a fresh
model name per test, since nothing can be deleted.
"""

import hashlib
import json
import logging
import os
import re
import secrets
from datetime import UTC, datetime, timedelta
from pathlib import Path
from typing import Any

import pytest
from botocore.exceptions import ClientError

from intelligence.modelstore import store
from intelligence.modelstore.reasons import Refused
from intelligence.modelstore.store import Credentials, S3Store
from tests.modelstore.conftest import RETENTION_DAYS, ROLES, MinioStore

REPO = Path(__file__).resolve().parents[4]
POLICY_DIR = REPO / "deploy" / "minio" / "policies"
STORE_PY = REPO / "services" / "intelligence" / "intelligence" / "modelstore" / "store.py"
UUID = re.compile(r"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}")
DAY = 1
CREDS = Credentials("synthetic-access", "synthetic-secret-not-real")


def _name() -> str:
    return f"t{secrets.token_hex(6)}"


def _refused(fn: Any, *args: Any, **kw: Any) -> Refused:
    with pytest.raises(Refused) as e:
        fn(*args, **kw)
    return e.value


def _denied(fn: Any, *args: Any, **kw: Any) -> None:
    err = _refused(fn, *args, **kw)
    assert (err.reason, err.detail_code) == ("store_unavailable", "denied")


def _raw_denied(fn: Any, **kw: Any) -> str:
    with pytest.raises(ClientError) as e:
        fn(**kw)
    return str(e.value.response["Error"]["Code"])


def _promote(m: MinioStore, name: str, version: str = "1.0.0") -> dict[str, str]:
    """What the promoter writes: the claim, one run file, one document, the manifest."""
    p = m.store("promoter")
    keys = {
        "claim": store.claim_key(name, version),
        "file": store.run_file_key(name, version, "run_1", "config.json"),
        "doc": store.run_doc_key(name, version, "run_1", "model_card.md"),
        "sig": store.manifest_sig_key(name, version),
        "manifest": store.manifest_key(name, version),
    }
    return {k: p.put_new(key, f"synthetic {k}".encode(), DAY) for k, key in keys.items()} | {
        f"{k}_key": key for k, key in keys.items()
    }


# ---------------------------------------------------------------- layout (no Docker)


def test_layout_keys() -> None:
    assert store.claim_key("mhai-tiny", "1.2.3") == "models/mhai-tiny/1.2.3/claim.json"
    assert (
        store.run_file_key("mhai-tiny", "1.2.3", "run_1", "model.safetensors")
        == "models/mhai-tiny/1.2.3/runs/run_1/files/model.safetensors"
    )
    assert (
        store.run_doc_key("mhai-tiny", "1.2.3", "run_1", "evaluation.json")
        == "models/mhai-tiny/1.2.3/runs/run_1/docs/evaluation.json"
    )
    assert store.manifest_key("mhai-tiny", "1.2.3") == "models/mhai-tiny/1.2.3/manifest.json"
    assert store.manifest_sig_key("mhai-tiny", "1.2.3") == "models/mhai-tiny/1.2.3/manifest.sig"
    assert store.signoff_key("mhai-tiny", "1.2.3") == "models/mhai-tiny/1.2.3/signoff.json"
    assert store.signoff_sig_key("mhai-tiny", "1.2.3") == "models/mhai-tiny/1.2.3/signoff.sig"
    assert (
        store.eval_file_key("clinical_document", "1.0.0", "pages/p-001.png")
        == "eval/clinical_document/1.0.0/files/pages/p-001.png"
    )
    assert store.testset_key("clinical_document", "1.0.0").endswith("/1.0.0/testset.json")
    assert store.testset_sig_key("clinical_document", "1.0.0").endswith("/1.0.0/testset.sig")


@pytest.mark.parametrize(
    "build",
    [
        lambda: store.claim_key("../x", "1.0.0"),
        lambda: store.claim_key("mhai-tiny", "1.0"),
        lambda: store.run_file_key("mhai-tiny", "1.0.0", "../r", "config.json"),
        lambda: store.run_file_key("mhai-tiny", "1.0.0", "r", "../config.json"),
        lambda: store.run_file_key("mhai-tiny", "1.0.0", "r", "model.bin"),
        lambda: store.run_file_key("mhai-tiny", "1.0.0", "r", "a/config.json"),
        lambda: store.run_doc_key("mhai-tiny", "1.0.0", "r", "notes.md"),
        lambda: store.eval_file_key("clinical_document", "1.0.0", "../../models/x"),
        lambda: store.eval_file_key("clinical_document", "1.0.0", "a/./b"),
        lambda: store.eval_file_key("clinical_document", "1.0.0", "a//b"),
        lambda: store.eval_file_key("clinical_document", "1.0.0", "/abs"),
        lambda: store.eval_file_key("clinical_document", "1.0.0", "a?b"),
        lambda: store.eval_file_key("clinical_document", "1.0.0", "/".join("a" * 9)),
        lambda: store.eval_file_key("Clinical", "1.0.0", "a"),
        lambda: store.testset_key("clinical_document", "x"),
    ],
)
def test_layout_refuses_bad_names(build: Any) -> None:
    assert _refused(build).reason == "invalid_name"


def test_keys_and_version_ids_checked_before_any_call() -> None:
    # Port 1 on loopback: nothing listens, so any call would be store_unavailable. A bad
    # key or version id must be refused before that.
    s = S3Store("http://127.0.0.1:1", "mhai-models", CREDS, max_attempts=1)
    for key in ("../x", "models/../eval/x", "models/a//b", "other/x", "models/a?b", "models/"):
        assert _refused(s.get, key, max_bytes=10).reason == "invalid_name"
        assert _refused(s.put_new, key, b"x", DAY).reason == "invalid_name"
        assert _refused(s.head, key).reason == "invalid_name"
        assert _refused(s.versions, key).reason == "invalid_name"
    for vid in ("a/b", "null", "", "x?versionId=1", "a+b"):
        assert _refused(s.get, "models/a/1.0.0/claim.json", vid, max_bytes=10).reason == (
            "invalid_name"
        )
        assert _refused(s.extend_retention, "models/a/1.0.0/claim.json", vid, 2).reason == (
            "invalid_name"
        )
    for prefix in ("models", "../", "models/a", "eval//"):
        assert _refused(s.list, prefix).reason == "invalid_name"
    for days in (0, -1, True, 36501, 1.5):
        assert _refused(s.put_new, "models/a/1.0.0/claim.json", b"x", days).reason == (
            "not_allowed_here"
        )


def test_put_new_path_must_be_a_regular_file(tmp_path: Path) -> None:
    s = S3Store("http://127.0.0.1:1", "mhai-models", CREDS, max_attempts=1)
    target = tmp_path / "config.json"
    target.write_bytes(b"{}")
    link = tmp_path / "link.json"
    link.symlink_to(target)
    assert _refused(s.put_new, "models/a/1.0.0/claim.json", link, DAY).reason == "missing_file"
    os.mkfifo(tmp_path / "fifo")
    assert _refused(s.put_new, "models/a/1.0.0/claim.json", tmp_path / "fifo", DAY).reason == (
        "file_not_allowed"
    )
    hard = tmp_path / "hard.json"
    os.link(target, hard)
    assert _refused(s.put_new, "models/a/1.0.0/claim.json", hard, DAY).reason == (
        "file_not_allowed"
    )
    assert _refused(s.put_new, "models/a/1.0.0/claim.json", "config.json", DAY).reason == (
        "file_not_allowed"
    )


def test_tls_required_for_remote_endpoint() -> None:
    for ok in (
        "https://store.example.org",
        "https://store.example.org:9000",
        "http://127.0.0.1:9000",
        "http://[::1]:9000",
        "http://localhost:9000",
        "https://127.0.0.1",
    ):
        assert store.check_endpoint(ok) == ok
        S3Store(ok, "mhai-models", CREDS)
    for plain in (
        "http://store.example.org",
        "http://minio:9000",
        "http://127.0.0.2:9000",
        "http://localhost.:9000",
        "http://10.0.0.5:9000",
        "http://localhost.example.org",
    ):
        err = _refused(S3Store, plain, "mhai-models", CREDS)
        assert (err.reason, err.detail_code) == ("store_unavailable", "tls_required")
    for bad in (
        "ftp://store.example.org",
        "https://user:pw@store.example.org",
        "https://store.example.org/bucket",
        "https://store.example.org?x=1",
        "https://store.example.org:99999",
        "store.example.org",
        "https://",
    ):
        err = _refused(S3Store, bad, "mhai-models", CREDS)
        assert err.reason == "store_unavailable"
    assert _refused(S3Store, "https://s.example.org", "Bad_Bucket", CREDS).reason == (
        "store_unavailable"
    )


def test_store_has_no_delete_method() -> None:
    source = STORE_PY.read_text()
    assert "delete_object" not in source
    assert "delete_objects" not in source
    assert "delete_bucket" not in source
    for cls in (store.S3Store, store.ObjectStore):
        assert not [a for a in dir(cls) if "delete" in a.lower() or "remove" in a.lower()]


def test_secrets_not_in_repr() -> None:
    assert "synthetic-secret" not in repr(CREDS)
    s = S3Store("https://store.example.org", "mhai-models", CREDS)
    assert repr(s) == "S3Store()"
    with pytest.raises(TypeError):
        CREDS.__reduce__()


def test_errors_unreachable_have_no_url_or_credentials(caplog: pytest.LogCaptureFixture) -> None:
    caplog.set_level(logging.DEBUG)
    s = S3Store("http://127.0.0.1:1", "mhai-secret-bucket", CREDS, max_attempts=1)
    key = "models/mhai-secret-name/1.0.0/manifest.json"
    for call in (
        lambda: s.get(key, max_bytes=10),
        lambda: s.put_new(key, b"x", DAY),
        lambda: s.head(key),
        lambda: s.versions(key),
        lambda: s.list("models/"),
    ):
        err = _refused(call)
        _assert_clean(err, ["127.0.0.1:1", "mhai-secret-bucket", "mhai-secret-name", CREDS])
        assert (err.reason, err.detail_code) == ("store_unavailable", "transport")
    _assert_logs_clean(caplog, ["127.0.0.1:1", "mhai-secret-bucket", "mhai-secret-name", CREDS])


def _secrets_of(items: list[Any]) -> list[str]:
    out: list[str] = []
    for item in items:
        if isinstance(item, Credentials):
            out += [item.access_key, item.secret_key]
        else:
            out.append(str(item))
    return out


def _assert_clean(err: Refused, forbidden: list[Any]) -> None:
    assert err.__cause__ is None and err.__context__ is None
    text = str(err) + repr(err) + repr(err.args) + str(err.detail_code)
    for value in _secrets_of(forbidden):
        assert value not in text


def _assert_logs_clean(caplog: pytest.LogCaptureFixture, forbidden: list[Any]) -> None:
    logged = "\n".join(r.getMessage() + str(r.args) + str(r.exc_text or "") for r in caplog.records)
    for value in _secrets_of(forbidden):
        assert value not in logged


def test_policies_load_and_deny_deletes() -> None:
    files = sorted(p.stem for p in POLICY_DIR.glob("*.json"))
    assert files == sorted(ROLES)
    for role in ROLES:
        doc = json.loads((POLICY_DIR / f"{role}.json").read_text())
        assert doc["Version"] == "2012-10-17"
        statements = doc["Statement"]
        for st in statements:
            assert set(st) <= {"Sid", "Effect", "Action", "Resource", "Condition"}
            assert st["Effect"] in ("Allow", "Deny")
            for r in st["Resource"]:
                assert r.startswith("arn:aws:s3:::@BUCKET@")
            if st["Effect"] == "Allow":
                # Nothing is ever allowed wholesale, and nothing that deletes or unlocks.
                assert "s3:*" not in st["Action"]
                assert not [a for a in st["Action"] if "Delete" in a or "Bypass" in a]
                if "s3:PutObjectRetention" in st["Action"]:
                    assert st["Condition"] == {
                        "StringEquals": {"s3:object-lock-mode": ["COMPLIANCE"]}
                    }
        denied = {a for st in statements if st["Effect"] == "Deny" for a in st["Action"]}
        assert {"s3:DeleteObject", "s3:DeleteObjectVersion", "s3:BypassGovernanceRetention"} <= (
            denied
        )
        assert {"s3:PutBucketObjectLockConfiguration", "s3:PutBucketVersioning"} <= denied
    promoter = json.loads((POLICY_DIR / "promoter.json").read_text())
    assert any(
        st["Effect"] == "Deny"
        and "s3:PutObject" in st["Action"]
        and st["Resource"] == ["arn:aws:s3:::@BUCKET@/models/*/*/signoff.*"]
        for st in promoter["Statement"]
    )


# ---------------------------------------------------------------- against MinIO


@pytest.mark.store
def test_init_writes_secrets_0600_and_never_echoes_them(minio_store: MinioStore) -> None:
    init = minio_store.init
    assert sorted(init.secrets) == sorted(ROLES)
    assert init.modes["."] == 0o700
    for role in ROLES:
        assert init.modes[role] == 0o600
        assert re.fullmatch(r"[0-9a-f]{40}", init.secrets[role])
    assert len(set(init.secrets.values())) == len(ROLES)
    for value in [*init.secrets.values(), minio_store.root.secret_key]:
        assert value not in init.logs
    # A re-run (compose restarted) keeps the passwords, and every user still signs in.
    again = minio_store.run_init()
    assert again.exit_code == 0
    assert dict(again.secrets) == dict(init.secrets)
    for value in [*again.secrets.values(), minio_store.root.secret_key]:
        assert value not in again.logs
    for role in ("promoter", "signoff", "reader"):
        minio_store.store(role).list("models/")


@pytest.mark.store
def test_bucket_has_compliance_lock_and_versioning(minio_store: MinioStore) -> None:
    raw = minio_store.client()
    lock = raw.get_object_lock_configuration(Bucket=minio_store.bucket)["ObjectLockConfiguration"]
    assert lock["ObjectLockEnabled"] == "Enabled"
    assert lock["Rule"]["DefaultRetention"]["Mode"] == "COMPLIANCE"
    assert lock["Rule"]["DefaultRetention"]["Days"] == RETENTION_DAYS
    assert raw.get_bucket_versioning(Bucket=minio_store.bucket)["Status"] == "Enabled"


@pytest.mark.store
def test_put_new_refuses_existing_key(minio_store: MinioStore) -> None:
    p = minio_store.store("promoter")
    key = store.claim_key(_name(), "1.0.0")
    vid = p.put_new(key, b'{"claim":"first"}', DAY)
    err = _refused(p.put_new, key, b'{"claim":"second"}', DAY)
    assert err.reason == "version_exists"
    # Root too: the conditional create holds for everyone.
    assert _refused(minio_store.store().put_new, key, b"x", DAY).reason == "version_exists"
    assert p.versions(key) == [vid]
    assert p.get(key, max_bytes=100) == b'{"claim":"first"}'


@pytest.mark.store
def test_version_ids_returned_and_fetchable(minio_store: MinioStore, tmp_path: Path) -> None:
    p = minio_store.store("promoter")
    name = _name()
    key = store.run_file_key(name, "1.0.0", "run_1", "config.json")
    vid = p.put_new(key, b'{"synthetic": 1}', DAY)
    assert UUID.fullmatch(vid)
    info = p.head(key, vid)
    assert info.version_id == vid and info.size == 16
    assert info.sha256 == hashlib.sha256(b'{"synthetic": 1}').hexdigest()
    assert info.lock_mode == "COMPLIANCE"
    assert info.retain_until is not None
    assert abs(info.retain_until - (datetime.now(UTC) + timedelta(days=DAY))) < timedelta(minutes=5)
    assert p.get(key, vid, max_bytes=16) == b'{"synthetic": 1}'
    assert _refused(p.get, key, vid, max_bytes=15).reason == "too_large"

    # Shadowing: an unconditional PUT by a holder of root makes a newer version. The one
    # we recorded is still there, unchanged, and is what we ask for.
    minio_store.client().put_object(Bucket=minio_store.bucket, Key=key, Body=b"shadow")
    versions = p.versions(key)
    assert len(versions) == 2 and versions[-1] == vid and versions[0] != vid
    assert p.get(key, vid, max_bytes=100) == b'{"synthetic": 1}'

    # A file is streamed, and listed under its prefix.
    weights = tmp_path / "model.safetensors"
    weights.write_bytes(os.urandom(3000))
    wkey = store.run_file_key(name, "1.0.0", "run_1", "model.safetensors")
    wvid = p.put_new(wkey, weights, DAY)
    assert p.get(wkey, wvid, max_bytes=3000) == weights.read_bytes()
    assert p.list(f"models/{name}/1.0.0/runs/run_1/files/") == sorted([key, wkey])

    missing = _refused(p.get, store.claim_key(name, "9.9.9"), max_bytes=10)
    assert missing.reason == "not_found"
    gone = _refused(p.get, key, "6d2f9c1e-3b4a-4f5e-9a8b-7c6d5e4f3a2b", max_bytes=10)
    assert gone.reason == "not_found"


@pytest.mark.store
def test_locked_object_cannot_be_deleted_or_shortened(minio_store: MinioStore) -> None:
    ids = _promote(minio_store, _name())
    key, vid = ids["manifest_key"], ids["manifest"]
    root = minio_store.client()
    until = minio_store.store().head(key, vid).retain_until
    assert until is not None
    # Compliance mode: not even the root user, not even with the governance bypass.
    for kw in ({}, {"BypassGovernanceRetention": True}):
        _raw_denied(root.delete_object, Bucket=minio_store.bucket, Key=key, VersionId=vid, **kw)
        _raw_denied(
            root.put_object_retention,
            Bucket=minio_store.bucket,
            Key=key,
            VersionId=vid,
            Retention={"Mode": "COMPLIANCE", "RetainUntilDate": until - timedelta(hours=1)},
            **kw,
        )
    _raw_denied(
        root.put_object_retention,
        Bucket=minio_store.bucket,
        Key=key,
        VersionId=vid,
        Retention={"Mode": "GOVERNANCE", "RetainUntilDate": until + timedelta(days=1)},
    )
    resp = root.delete_objects(
        Bucket=minio_store.bucket,
        Delete={"Objects": [{"Key": key, "VersionId": vid}]},
        BypassGovernanceRetention=True,
    )
    assert resp.get("Errors") and not resp.get("Deleted")
    assert minio_store.store("reader").get(key, vid, max_bytes=100) == b"synthetic manifest"
    assert minio_store.store().head(key, vid).retain_until == until


@pytest.mark.store
def test_retention_extend_only(minio_store: MinioStore) -> None:
    ids = _promote(minio_store, _name())
    key, vid = ids["manifest_key"], ids["manifest"]
    s = minio_store.store("signoff")
    before = s.head(key, vid).retain_until
    assert before is not None
    after = s.extend_retention(key, vid, 3)
    assert after > before
    assert s.head(key, vid).retain_until == after
    # Never shortened: our code refuses without a call, and the store refuses the call.
    err = _refused(s.extend_retention, key, vid, 1)
    assert (err.reason, err.detail_code) == ("not_allowed_here", "retention_not_longer")
    _raw_denied(
        minio_store.client("signoff").put_object_retention,
        Bucket=minio_store.bucket,
        Key=key,
        VersionId=vid,
        Retention={"Mode": "COMPLIANCE", "RetainUntilDate": after - timedelta(days=1)},
    )
    assert s.head(key, vid).retain_until == after
    # The reader and the eval roles may not change retention at all.
    for role in ("reader", "eval-reader", "eval-writer"):
        _denied(minio_store.store(role).extend_retention, key, vid, 10)


@pytest.mark.store
def test_reader_cannot_write(minio_store: MinioStore) -> None:
    name = _name()
    ids = _promote(minio_store, name)
    r = minio_store.store("reader")
    assert r.get(ids["manifest_key"], ids["manifest"], max_bytes=100) == b"synthetic manifest"
    assert r.versions(ids["manifest_key"]) == [ids["manifest"]]
    assert store.manifest_key(name, "1.0.0") in r.list(f"models/{name}/1.0.0/")
    for key in (
        store.claim_key(_name(), "1.0.0"),
        store.manifest_key(name, "2.0.0"),
        store.signoff_key(name, "1.0.0"),
        store.eval_file_key("clinical_document", "1.0.0", "a.png"),
    ):
        _denied(r.put_new, key, b"x", DAY)
    raw = minio_store.client("reader")
    _raw_denied(raw.put_object, Bucket=minio_store.bucket, Key=ids["file_key"], Body=b"shadow")
    assert r.versions(ids["file_key"]) == [ids["file"]]


@pytest.mark.store
def test_promoter_cannot_write_signoff_or_eval_or_delete(minio_store: MinioStore) -> None:
    name = _name()
    ids = _promote(minio_store, name)
    p = minio_store.store("promoter")
    _denied(p.put_new, store.signoff_key(name, "1.0.0"), b"{}", DAY)
    _denied(p.put_new, store.signoff_sig_key(name, "1.0.0"), b"{}", DAY)
    _denied(p.put_new, store.testset_key("clinical_document", "1.0.0"), b"{}", DAY)
    _denied(p.put_new, store.eval_file_key("clinical_document", "1.0.0", "a.png"), b"x", DAY)
    _denied(p.list, "eval/")
    raw = minio_store.client("promoter")
    bucket = minio_store.bucket
    # Locking a new object needs s3:PutObjectRetention, so the promoter can also lengthen
    # a model object's lock (compliance mode only). It can never shorten one.
    longer = p.extend_retention(ids["file_key"], ids["file"], 2)
    _raw_denied(
        raw.put_object_retention,
        Bucket=bucket,
        Key=ids["file_key"],
        VersionId=ids["file"],
        Retention={"Mode": "COMPLIANCE", "RetainUntilDate": longer - timedelta(hours=12)},
    )
    # Deletes are denied outright: not even a delete marker is made.
    _raw_denied(raw.delete_object, Bucket=bucket, Key=ids["manifest_key"])
    _raw_denied(
        raw.delete_object, Bucket=bucket, Key=ids["manifest_key"], VersionId=ids["manifest"]
    )
    assert p.versions(ids["manifest_key"]) == [ids["manifest"]]
    assert p.head(ids["manifest_key"]).version_id == ids["manifest"]
    # Nor a lock in governance mode, nor a change to the bucket's versioning or lock.
    _raw_denied(
        raw.put_object,
        Bucket=bucket,
        Key=store.claim_key(_name(), "1.0.0"),
        Body=b"x",
        ObjectLockMode="GOVERNANCE",
        ObjectLockRetainUntilDate=datetime.now(UTC) + timedelta(days=1),
    )
    _raw_denied(
        raw.put_bucket_versioning, Bucket=bucket, VersioningConfiguration={"Status": "Suspended"}
    )
    _raw_denied(
        raw.put_object_lock_configuration,
        Bucket=bucket,
        ObjectLockConfiguration={"ObjectLockEnabled": "Enabled"},
    )


@pytest.mark.store
def test_nobody_can_delete(minio_store: MinioStore) -> None:
    ids = _promote(minio_store, _name())
    for role in ROLES:
        raw = minio_store.client(role)
        for kw in ({}, {"VersionId": ids["file"]}):
            code = _raw_denied(
                raw.delete_object, Bucket=minio_store.bucket, Key=ids["file_key"], **kw
            )
            assert code == "AccessDenied", role
    assert minio_store.store("reader").versions(ids["file_key"]) == [ids["file"]]


@pytest.mark.store
def test_signoff_cannot_write_manifest_or_files(minio_store: MinioStore) -> None:
    name = _name()
    ids = _promote(minio_store, name)
    s = minio_store.store("signoff")
    vid = s.put_new(store.signoff_key(name, "1.0.0"), b'{"synthetic":"signoff"}', DAY)
    s.put_new(store.signoff_sig_key(name, "1.0.0"), b'{"synthetic":"sig"}', DAY)
    assert s.get(store.signoff_key(name, "1.0.0"), vid, max_bytes=100)
    other = _name()
    for key in (
        store.manifest_key(other, "1.0.0"),
        store.manifest_sig_key(other, "1.0.0"),
        store.claim_key(other, "1.0.0"),
        store.run_file_key(other, "1.0.0", "run_1", "config.json"),
        store.run_doc_key(other, "1.0.0", "run_1", "model_card.md"),
        # A run file that happens to be named like a sign-off is still a run file.
        f"models/{other}/1.0.0/runs/run_1/files/signoff.json",
        store.eval_file_key("clinical_document", "1.0.0", "a.png"),
    ):
        _denied(s.put_new, key, b"x", DAY)
    # It may read the version it signs off, and extend that version's retention.
    assert s.get(ids["manifest_key"], ids["manifest"], max_bytes=100) == b"synthetic manifest"
    s.extend_retention(ids["file_key"], ids["file"], 5)


@pytest.mark.store
def test_eval_reader_only_reads_eval(minio_store: MinioStore) -> None:
    name = _name()
    ids = _promote(minio_store, name)
    suite_version = f"1.{secrets.randbelow(10**6)}.0"
    w = minio_store.store("eval-writer")
    r = minio_store.store("eval-reader")
    key = store.eval_file_key("clinical_document", suite_version, "pages/p-001.png")
    vid = w.put_new(key, b"synthetic page", DAY)
    tvid = w.put_new(store.testset_key("clinical_document", suite_version), b"{}", DAY)
    assert r.get(key, vid, max_bytes=100) == b"synthetic page"
    assert r.get(store.testset_key("clinical_document", suite_version), tvid, max_bytes=10)
    # The eval reader reads nothing else and writes nothing.
    _denied(r.get, ids["manifest_key"], ids["manifest"], max_bytes=100)
    _denied(r.put_new, store.eval_file_key("clinical_document", suite_version, "x.png"), b"x", 1)
    _denied(r.put_new, store.claim_key(_name(), "1.0.0"), b"x", DAY)
    # The eval writer writes eval only, and reads no model.
    _denied(w.put_new, store.claim_key(_name(), "1.0.0"), b"x", DAY)
    _denied(w.get, ids["manifest_key"], ids["manifest"], max_bytes=100)
    # The model roles never read the test split.
    for role in ("promoter", "signoff", "reader"):
        _denied(minio_store.store(role).get, key, vid, max_bytes=100)


@pytest.mark.store
def test_errors_have_no_url_or_credentials(
    minio_store: MinioStore, caplog: pytest.LogCaptureFixture
) -> None:
    caplog.set_level(logging.DEBUG)
    name = _name()
    ids = _promote(minio_store, name)
    host = minio_store.endpoint.split("://", 1)[1]
    forbidden: list[Any] = [host, minio_store.bucket, name, minio_store.root]
    forbidden += list(minio_store.users.values())
    wrong = Credentials("promoter", "not-the-password-0123456789")
    bad = S3Store(minio_store.endpoint, minio_store.bucket, wrong, max_attempts=1)
    err = _refused(bad.get, ids["manifest_key"], max_bytes=100)
    assert (err.reason, err.detail_code) == ("store_unavailable", "denied")
    _assert_clean(err, [*forbidden, wrong])
    p = minio_store.store("promoter")
    for call, reason in (
        (lambda: p.get(store.claim_key(name, "9.9.9"), max_bytes=10), "not_found"),
        (lambda: p.put_new(ids["claim_key"], b"x", DAY), "version_exists"),
        (lambda: p.put_new(store.signoff_key(name, "1.0.0"), b"x", DAY), "store_unavailable"),
        (lambda: p.head(store.claim_key(name, "9.9.9")), "not_found"),
    ):
        err = _refused(call)
        assert err.reason == reason
        _assert_clean(err, forbidden)
    _assert_logs_clean(caplog, forbidden)

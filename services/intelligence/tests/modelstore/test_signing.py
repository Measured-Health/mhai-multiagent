"""Domain-separated Ed25519 signatures, the file signer, key generation and the trust policy.

Keys are generated in ``tmp_path`` only; nothing here prints or keeps private material.
"""

import base64
import json
import os
import stat
from pathlib import Path
from typing import Any

import pytest
from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric import ec, ed25519

from intelligence.modelstore import canonical, cli, signing
from intelligence.modelstore.reasons import Refused

REPO = Path(__file__).resolve().parents[4]
PAYLOAD = canonical.dumps({"name": "mhai-tiny-extractor", "version": "1.0.0"})


@pytest.fixture
def dev_env(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv(signing.ENV_VAR, "development")


@pytest.fixture
def prod_env(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv(signing.ENV_VAR, raising=False)


def _write_key(path: Path, key: ed25519.Ed25519PrivateKey, mode: int = 0o600) -> Path:
    pem = key.private_bytes(
        serialization.Encoding.PEM,
        serialization.PrivateFormat.PKCS8,
        serialization.NoEncryption(),
    )
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "wb") as f:
        f.write(pem)
    os.chmod(path, mode)
    return path


def _pub_pem(key: Any) -> str:
    return str(
        key.public_key()
        .public_bytes(serialization.Encoding.PEM, serialization.PublicFormat.SubjectPublicKeyInfo)
        .decode("ascii")
    )


def _entry(key: Any, *, development: bool = True, alg: str = "ed25519") -> dict[str, Any]:
    return {
        "alg": alg,
        "key_id": signing.key_id(key.public_key()),
        "public_key_pem": _pub_pem(key),
        "development": development,
    }


def _policy_obj(*signers: dict[str, Any]) -> dict[str, Any]:
    return {"schema": signing.TRUST_SCHEMA, "signers": list(signers)}


def _policy(
    *signers: dict[str, Any], environment: str | None = "development"
) -> signing.TrustPolicy:
    return signing.parse_policy(_policy_obj(*signers), environment=environment)


def _signer(tmp_path: Path, name: str = "k.pem") -> tuple[signing.FileSigner, Any]:
    key = ed25519.Ed25519PrivateKey.generate()
    return signing.FileSigner(_write_key(tmp_path / name, key)), key


def test_sign_verify_round_trip(tmp_path: Path, dev_env: None) -> None:
    signer, key = _signer(tmp_path)
    assert signer.alg == "ed25519"
    assert signer.key_id == signing.key_id(key.public_key())
    assert len(signer.key_id) == 64 and signer.key_id == signer.key_id.lower()
    doc = signer.sign("manifest", PAYLOAD)
    assert (doc.purpose, doc.alg, doc.key_id) == ("manifest", "ed25519", signer.key_id)
    raw = doc.to_bytes()
    assert canonical.is_canonical(raw)
    obj = json.loads(raw)
    assert set(obj) == {"schema", "purpose", "alg", "key_id", "signature"}
    assert obj["schema"] == signing.SIGNATURE_SCHEMA == "mhai.models.signature/1"
    assert base64.b64decode(obj["signature"], validate=True) == doc.signature
    # The signature is over the domain-separated message, not the bare payload.
    key.public_key().verify(doc.signature, b"mhai.models.manifest/1\n" + PAYLOAD)
    assert signing.message("manifest", PAYLOAD) == b"mhai.models.manifest/1\n" + PAYLOAD
    policy = _policy(_entry(key))
    who = signing.Verifier().verify("manifest", PAYLOAD, signing.parse_signature(raw), policy)
    assert who.key_id == signer.key_id and who.purpose == "manifest" and who.development
    # Bytes are accepted too, and parsed strictly.
    assert signing.Verifier().verify("manifest", PAYLOAD, raw, policy) == who
    # One byte of payload changed.
    with pytest.raises(Refused) as e:
        signing.Verifier().verify("manifest", PAYLOAD + b" ", doc, policy)
    assert e.value.reason == "signature_invalid"


def test_one_key_signs_every_purpose(tmp_path: Path, dev_env: None) -> None:
    """One listed key is trusted for manifests and test sets alike; the purpose lives in
    the signed message, not in the policy."""
    signer, key = _signer(tmp_path)
    policy = _policy(_entry(key))
    for purpose in signing.PURPOSES:
        doc = signer.sign(purpose, PAYLOAD)
        assert signing.Verifier().verify(purpose, PAYLOAD, doc, policy).purpose == purpose
    assert signing.PURPOSES == ("manifest", "testset")
    assert signing.ALGS == ("ed25519",)


def test_ecdsa_refused(dev_env: None) -> None:
    """Only Ed25519: an ECDSA key is not a signer, in the policy or as a document."""
    key = ec.generate_private_key(ec.SECP256R1())
    pem = _pub_pem(key)
    with pytest.raises(Refused) as e:
        signing.parse_policy(
            _policy_obj({**_entry(ed25519.Ed25519PrivateKey.generate()), "public_key_pem": pem}),
            environment="development",
        )
    assert e.value.reason == "policy_invalid"
    with pytest.raises(Refused) as e:
        signing.SignatureDoc("manifest", "ecdsa-p256-sha256", "a" * 64, b"\x01" * 70)
    assert e.value.reason == "signature_invalid"


def test_signature_by_untrusted_key_refused(tmp_path: Path, dev_env: None) -> None:
    signer, _ = _signer(tmp_path, "a.pem")
    other = ed25519.Ed25519PrivateKey.generate()
    policy = _policy(_entry(other))
    doc = signer.sign("manifest", PAYLOAD)
    with pytest.raises(Refused) as e:
        signing.Verifier().verify("manifest", PAYLOAD, doc, policy)
    assert e.value.reason == "signer_untrusted"
    # A document claiming the trusted key id, signed by another key, does not verify.
    forged = signing.SignatureDoc(
        "manifest", "ed25519", signing.key_id(other.public_key()), doc.signature
    )
    with pytest.raises(Refused) as e:
        signing.Verifier().verify("manifest", PAYLOAD, forged, policy)
    assert e.value.reason == "signature_invalid"
    # The committed policy trusts nobody yet: the production key is added by PR.
    committed = signing.load_policy(REPO / "models" / "trust-policy.json", environment=None)
    assert committed.signers == ()
    with pytest.raises(Refused) as e:
        signing.Verifier().verify("manifest", PAYLOAD, doc, committed)
    assert e.value.reason == "signer_untrusted"


def test_purpose_confusion_refused(tmp_path: Path, dev_env: None) -> None:
    signer, key = _signer(tmp_path)
    # One key signs manifests and test sets, but the domain separates them.
    policy = _policy(_entry(key))
    tdoc = signer.sign("testset", PAYLOAD)
    assert signing.Verifier().verify("testset", PAYLOAD, tdoc, policy).purpose == "testset"
    with pytest.raises(Refused) as e:
        signing.Verifier().verify("manifest", PAYLOAD, tdoc, policy)
    assert e.value.reason == "signature_invalid"
    # Relabelling the document doesn't help: the signed message names the purpose.
    relabelled = signing.SignatureDoc("manifest", tdoc.alg, tdoc.key_id, tdoc.signature)
    with pytest.raises(Refused) as e:
        signing.Verifier().verify("manifest", PAYLOAD, relabelled, policy)
    assert e.value.reason == "signature_invalid"
    # The removed sign-off purpose is no purpose at all.
    for purpose in ("signoff", "other", "manifest/1\n"):
        with pytest.raises(Refused):
            signing.message(purpose, PAYLOAD)
        with pytest.raises(Refused):
            signer.sign(purpose, PAYLOAD)


def test_signature_doc_strict() -> None:
    kid = "a" * 64
    good = signing.SignatureDoc("manifest", "ed25519", kid, b"\x01" * 64).to_bytes()
    assert signing.parse_signature(good).signature == b"\x01" * 64
    obj = json.loads(good)
    bad: list[bytes] = [
        good + b"\n",
        json.dumps(obj, indent=1, sort_keys=True).encode(),
        canonical.dumps({**obj, "extra": 1}),
        canonical.dumps({**obj, "alg": "rsa"}),
        canonical.dumps({**obj, "purpose": "other"}),
        canonical.dumps({**obj, "key_id": "A" * 64}),
        canonical.dumps({**obj, "signature": obj["signature"].rstrip("=")}),
        canonical.dumps({**obj, "signature": obj["signature"].replace("A", "-")}),
        canonical.dumps({**obj, "signature": ""}),
        canonical.dumps({**obj, "schema": "mhai.models.signature/2"}),
        good.replace(b'"schema"', b'"schema":1.5,"x"'),
    ]
    for b in bad:
        with pytest.raises(Refused) as e:
            signing.parse_signature(b)
        assert e.value.reason in {"signature_invalid", "not_canonical"}, b


def test_file_signer_works_in_production(tmp_path: Path, prod_env: None) -> None:
    """The key file is the production signer too (the repository secret written to a
    file, or a key file for the CLI); whether its key counts is the policy's business."""
    signer, key = _signer(tmp_path)
    doc = signer.sign("manifest", PAYLOAD)
    policy = _policy(_entry(key, development=False), environment=None)
    assert not signing.Verifier().verify("manifest", PAYLOAD, doc, policy).development
    # The same key listed as a development key is refused in production.
    with pytest.raises(Refused) as e:
        _policy(_entry(key, development=True), environment=None)
    assert e.value.reason == "policy_invalid"


def test_file_signer_key_file_checks(tmp_path: Path, dev_env: None) -> None:
    key = ed25519.Ed25519PrivateKey.generate()
    loose = _write_key(tmp_path / "loose.pem", key, mode=0o640)
    with pytest.raises(Refused) as e:
        signing.FileSigner(loose)
    assert e.value.reason == "key_invalid"
    strict = _write_key(tmp_path / "strict.pem", ed25519.Ed25519PrivateKey.generate(), 0o400)
    assert signing.FileSigner(strict).alg == "ed25519"
    good = _write_key(tmp_path / "good.pem", key)
    link = tmp_path / "link.pem"
    link.symlink_to(good)
    with pytest.raises(Refused) as e:
        signing.FileSigner(link)
    assert e.value.reason == "key_invalid"
    junk = tmp_path / "junk.pem"
    junk.write_bytes(b"not a key")
    junk.chmod(0o600)
    with pytest.raises(Refused) as e:
        signing.FileSigner(junk)
    assert e.value.reason == "key_invalid"
    ec_key = ec.generate_private_key(ec.SECP256R1())
    ecp = tmp_path / "ec.pem"
    ecp.write_bytes(
        ec_key.private_bytes(
            serialization.Encoding.PEM,
            serialization.PrivateFormat.PKCS8,
            serialization.NoEncryption(),
        )
    )
    ecp.chmod(0o600)
    with pytest.raises(Refused) as e:
        signing.FileSigner(ecp)
    assert e.value.reason == "key_invalid"
    with pytest.raises(Refused) as e:
        signing.FileSigner(tmp_path / "missing.pem")
    assert e.value.reason == "key_invalid"
    with pytest.raises(Refused) as e:
        signing.FileSigner(tmp_path)
    assert e.value.reason == "key_invalid"
    signer = signing.FileSigner(good)
    assert "PRIVATE" not in repr(signer) and "BEGIN" not in repr(signer)


def test_dev_signer_in_policy_refused_in_production(tmp_path: Path) -> None:
    key = ed25519.Ed25519PrivateKey.generate()
    obj = _policy_obj(_entry(key, development=True))
    for env in (None, "", "production", "Development"):
        with pytest.raises(Refused) as e:
            signing.parse_policy(obj, environment=env)
        assert e.value.reason == "policy_invalid"
    assert signing.parse_policy(obj, environment="development").signers
    # A production signer loads in production.
    prod = signing.parse_policy(_policy_obj(_entry(key, development=False)), environment=None)
    assert not prod.signers[0].development
    # load_policy reads the environment when none is given.
    path = tmp_path / "trust-policy.json"
    path.write_bytes(canonical.dumps(obj))
    old = os.environ.pop(signing.ENV_VAR, None)
    try:
        with pytest.raises(Refused) as e:
            signing.load_policy(path)
        assert e.value.reason == "policy_invalid"
        os.environ[signing.ENV_VAR] = "development"
        assert signing.load_policy(path).signers
    finally:
        os.environ.pop(signing.ENV_VAR, None)
        if old is not None:
            os.environ[signing.ENV_VAR] = old


def test_dev_policy_entry_refused_at_verify_in_production(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setenv(signing.ENV_VAR, "development")
    signer, key = _signer(tmp_path)
    policy = _policy(_entry(key))
    doc = signer.sign("manifest", PAYLOAD)
    monkeypatch.setenv(signing.ENV_VAR, "production")
    with pytest.raises(Refused) as e:
        signing.Verifier().verify("manifest", PAYLOAD, doc, policy)
    assert e.value.reason == "policy_invalid"


def test_policy_shape_checked() -> None:
    key = ed25519.Ed25519PrivateKey.generate()
    good = _entry(key)
    bad: list[Any] = [
        [],
        {"schema": signing.TRUST_SCHEMA},
        {**_policy_obj(), "schema": "mhai.models.trust/2"},
        {**_policy_obj(), "extra": 1},
        # The old per-purpose shape is refused, not read.
        {"schema": signing.TRUST_SCHEMA, "signers": {"manifest": [good], "testset": [good]}},
        _policy_obj({**good, "key_id": "0" * 64}),  # key_id doesn't match the key
        _policy_obj({**good, "alg": "ecdsa-p256-sha256"}),
        _policy_obj({**good, "kind": "public_key"}),
        _policy_obj({**good, "development": "yes"}),
        _policy_obj({**good, "extra": 1}),
        _policy_obj({**good, "public_key_pem": "junk"}),
        _policy_obj(good, good),
        _policy_obj(*[_entry(ed25519.Ed25519PrivateKey.generate())] * 2),
        _policy_obj(*[_entry(ed25519.Ed25519PrivateKey.generate()) for _ in range(17)]),
    ]
    for obj in bad:
        with pytest.raises(Refused) as e:
            signing.parse_policy(obj, environment="development")
        assert e.value.reason == "policy_invalid"
    policy = _policy(good)
    assert policy.trusts(good["key_id"], "ed25519")
    assert not policy.trusts(good["key_id"], "ecdsa-p256-sha256")
    assert not policy.trusts("0" * 64, "ed25519")
    # policy_obj writes what parse_policy reads.
    back = signing.parse_policy(signing.policy_obj(list(policy.signers)), environment="development")
    assert back == policy


def test_keygen_makes_a_production_key(
    tmp_path: Path, prod_env: None, capsys: pytest.CaptureFixture[str]
) -> None:
    """``mhai-models keygen`` works outside development, writes the key 0600, and prints
    only the trust-policy entry: a production (non-development) signer that verifies."""
    out = tmp_path / "keys"
    assert cli.main(["keygen", str(out)]) == 0
    printed = capsys.readouterr()
    private = out / signing.KEY_FILE
    assert stat.S_IMODE(private.stat().st_mode) == 0o600
    assert stat.S_IMODE((out / signing.PUBLIC_KEY_FILE).stat().st_mode) == 0o644
    assert "PRIVATE" not in printed.out + printed.err
    entry = json.loads(printed.out)["trust_policy_signer"]
    assert set(entry) == {"alg", "key_id", "public_key_pem", "development"}
    assert entry["development"] is False
    policy = signing.parse_policy(_policy_obj(entry), environment=None)
    signer = signing.FileSigner(private)
    assert signer.key_id == entry["key_id"]
    doc = signer.sign("manifest", PAYLOAD)
    assert signing.Verifier().verify("manifest", PAYLOAD, doc, policy).key_id == signer.key_id
    # Never overwrites.
    assert cli.main(["keygen", str(out)]) == 2
    assert json.loads(capsys.readouterr().out) == {"refused": "key_exists"}


def test_dev_keygen_permissions_and_output(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture[str]
) -> None:
    out = tmp_path / "keys"
    monkeypatch.delenv(signing.ENV_VAR, raising=False)
    assert cli.main(["dev-keygen", str(out)]) == 2
    assert json.loads(capsys.readouterr().out) == {"refused": "not_allowed_here"}
    assert not out.exists()

    monkeypatch.setenv(signing.ENV_VAR, "development")
    assert cli.main(["dev-keygen", str(out)]) == 0
    printed = capsys.readouterr()
    private = out / signing.DEV_KEY_FILE
    public = out / signing.DEV_PUBLIC_KEY_FILE
    assert stat.S_IMODE(private.stat().st_mode) == 0o600
    assert not private.is_symlink()
    secret = private.read_bytes()
    assert b"PRIVATE KEY" in secret
    # stdout has the key id and the public key, and nothing private.
    assert "PRIVATE" not in printed.out + printed.err
    body = secret.decode().split("-----")[2].strip().replace("\n", "")
    assert body not in printed.out.replace("\\n", "") and body[:16] not in printed.out
    shown = json.loads(printed.out)
    assert set(shown) == {"key_id", "public_key_pem"}
    signer = signing.FileSigner(private)
    assert shown["key_id"] == signer.key_id
    assert shown["public_key_pem"] == public.read_text()
    loaded = serialization.load_pem_public_key(shown["public_key_pem"].encode())
    assert signing.key_id(loaded) == signer.key_id
    # It never overwrites an existing key.
    assert cli.main(["dev-keygen", str(out)]) == 2
    assert json.loads(capsys.readouterr().out) == {"refused": "key_exists"}
    assert private.read_bytes() == secret


def test_dev_keygen_writes_both_files_or_neither(
    tmp_path: Path, dev_env: None, monkeypatch: pytest.MonkeyPatch
) -> None:
    out = tmp_path / "keys"
    real_link = signing._link_new
    calls: list[str] = []

    def fail_second(dfd: int, temp: str, name: str) -> None:
        calls.append(name)
        if len(calls) == 2:
            raise OSError("disk full")
        real_link(dfd, temp, name)

    monkeypatch.setattr(signing, "_link_new", fail_second)
    with pytest.raises(OSError):
        signing.generate_dev_key(out)
    assert len(calls) == 2
    assert sorted(p.name for p in out.iterdir()) == []  # no key, no temporary file

    real_write = signing._write_new
    writes: list[str] = []

    def fail_second_write(dfd: int, name: str, content: bytes, mode: int) -> None:
        writes.append(name)
        if len(writes) == 2:
            raise OSError("disk full")
        real_write(dfd, name, content, mode)

    monkeypatch.setattr(signing, "_link_new", real_link)
    monkeypatch.setattr(signing, "_write_new", fail_second_write)
    with pytest.raises(OSError):
        signing.generate_dev_key(out)
    assert sorted(p.name for p in out.iterdir()) == []

    monkeypatch.setattr(signing, "_write_new", real_write)
    signing.generate_dev_key(out)
    assert sorted(p.name for p in out.iterdir()) == sorted(
        [signing.DEV_KEY_FILE, signing.DEV_PUBLIC_KEY_FILE]
    )
    assert stat.S_IMODE((out / signing.DEV_PUBLIC_KEY_FILE).stat().st_mode) == 0o644
    assert (out / signing.DEV_KEY_FILE).stat().st_nlink == 1


def test_dev_keygen_refuses_an_unsafe_out_dir(tmp_path: Path, dev_env: None) -> None:
    created = tmp_path / "new"
    signing.generate_dev_key(created)
    assert stat.S_IMODE(created.stat().st_mode) == 0o700

    for mode in (0o777, 0o770, 0o702, 0o720):
        loose = tmp_path / f"loose-{mode:o}"
        loose.mkdir()
        loose.chmod(mode)
        with pytest.raises(Refused) as e:
            signing.generate_dev_key(loose)
        assert e.value.reason == "key_invalid"
        assert list(loose.iterdir()) == []

    link = tmp_path / "link"
    link.symlink_to(created)
    with pytest.raises(Refused) as e:
        signing.generate_dev_key(link)
    assert e.value.reason == "key_invalid"

    other = tmp_path / "other"
    other.mkdir(mode=0o700)
    try:
        os.chown(other, os.geteuid() + 4242, -1)
    except PermissionError:
        pytest.skip("chown to another user needs root")
    with pytest.raises(Refused) as e:
        signing.generate_dev_key(other)
    assert e.value.reason == "key_invalid"
    assert list(other.iterdir()) == []

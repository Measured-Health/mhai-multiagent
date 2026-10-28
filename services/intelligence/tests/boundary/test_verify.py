"""The verifier's ordered checks (spec §7.3, §17.2). Unit tests, no Docker.

Every token, key and id here is synthetic (tests/boundary/tokens.py).
"""

from __future__ import annotations

import dataclasses
import hashlib
import hmac
import json
import logging
import re
from collections.abc import Callable
from pathlib import Path
from unittest import mock

import pytest

from intelligence.boundary import envelope
from intelligence.boundary.keys import TrustedKeys
from intelligence.boundary.operations import Operations
from intelligence.boundary.pb import authz_pb2, echo_pb2, pipeline_pb2
from intelligence.boundary.replay import JtiSet
from intelligence.boundary.verify import (
    MAX_JOB_BODY,
    MAX_REQ_BODY,
    MAX_TOKEN_LEN,
    Refusal,
    Verified,
    Verifier,
    scope_ids,
    strict_decoder,
)
from tests.boundary.tokens import (
    BFF_REPLY,
    BFF_RID,
    CORE_REPLY,
    HEALTH_SCHEMA,
    HOSPITAL,
    JOB_SCHEMA,
    NOW,
    OTHER_HOSPITAL,
    PATIENT,
    PING_SCHEMA,
    QUERY_ID,
    RID,
    USER,
    Signer,
    b64u,
)

PING_SUBJECT = "inference.v1.echo.ping"
JOB_SUBJECT = f"jobs.v1.{HOSPITAL}.run_query"

ping_decoder = strict_decoder(echo_pb2.PingRequest)
job_decoder = strict_decoder(pipeline_pb2.RunQueryJob)


def ping_body(hospital: str = HOSPITAL, text: str = "synthetic ping") -> bytes:
    return echo_pb2.PingRequest(text=text, hospital_id=hospital).SerializeToString(
        deterministic=True
    )


def job_body() -> bytes:
    return pipeline_pb2.RunQueryJob(query_id=QUERY_ID).SerializeToString(deterministic=True)


def headers(token: str | None, schema: str = PING_SCHEMA, rid: str = RID) -> dict[str, str]:
    h = {envelope.SCHEMA: schema, envelope.REQUEST_ID: rid}
    if token is not None:
        h[envelope.TOKEN] = token
    return h


class Clock:
    def __init__(self, now: float) -> None:
        self.now = now

    def __call__(self) -> float:
        return self.now


@pytest.fixture
def signer() -> Signer:
    return Signer()


@pytest.fixture
def ops() -> Operations:
    return Operations.load(development=True)


@pytest.fixture
def clock() -> Clock:
    return Clock(NOW + 1)


@pytest.fixture
def make(signer: Signer, ops: Operations, clock: Clock) -> Callable[..., Verifier]:
    def _make(service: str = "echo", replay: JtiSet | None = None) -> Verifier:
        trusted = TrustedKeys.from_encoded(signer.encoded_keys())
        return Verifier(
            trusted, service, ops, replay if replay is not None else JtiSet(clock), clock
        )

    return _make


def refused(result: object, reason: str) -> Refusal:
    assert isinstance(result, Refusal), result
    assert result.reason == reason, result
    return result


def never_decoder() -> mock.Mock:
    return mock.Mock(side_effect=AssertionError("the body must not be decoded"))


# --- the happy paths ------------------------------------------------------------


def test_valid_request_verified(make: Callable[..., Verifier], signer: Signer) -> None:
    body = ping_body()
    tok = signer.mint(body=body)
    v = make().verify_request(PING_SUBJECT, CORE_REPLY, headers(tok), body, ping_decoder)
    assert isinstance(v, Verified)
    assert (v.sub, v.hid, v.roles, v.rid, v.kind, v.subject) == (
        USER,
        HOSPITAL,
        ("clinician",),
        RID,
        "req",
        PING_SUBJECT,
    )
    assert v.operation.name == "echo.ping"
    assert v.kid == signer.real_kid and v.exp == NOW + 5 and v.via == "core"
    assert isinstance(v.message, echo_pb2.PingRequest) and v.message.text == "synthetic ping"


def test_valid_job_verified(make: Callable[..., Verifier], signer: Signer) -> None:
    body = job_body()
    v = make("pipeline").verify_job(
        JOB_SUBJECT, headers(signer.mint_job(body), JOB_SCHEMA), body, job_decoder
    )
    assert isinstance(v, Verified)
    assert v.kind == "job" and v.operation.name == "pipeline.run_query" and v.hid == HOSPITAL


# --- A1, A3, A5, A6, A7, A8, A10 -----------------------------------------------


def test_missing_token_refused(make: Callable[..., Verifier]) -> None:
    dec = never_decoder()
    r = refused(
        make().verify_request(PING_SUBJECT, CORE_REPLY, headers(None), ping_body(), dec),
        "malformed",
    )
    assert r.code == "unauthorized"
    dec.assert_not_called()


def test_replayed_jti_refused(make: Callable[..., Verifier], signer: Signer) -> None:
    body = ping_body()
    tok = signer.mint(body=body)
    v = make()
    assert isinstance(
        v.verify_request(PING_SUBJECT, CORE_REPLY, headers(tok), body, ping_decoder), Verified
    )
    r = refused(
        v.verify_request(PING_SUBJECT, CORE_REPLY, headers(tok), body, ping_decoder), "replay"
    )
    assert r.code == "unauthorized" and r.reply


def test_token_for_other_operation_refused(make: Callable[..., Verifier], signer: Signer) -> None:
    body = b""
    tok = signer.mint(body=body)  # aud echo, nsub echo.ping
    h = headers(tok, HEALTH_SCHEMA)
    health = strict_decoder(pipeline_pb2.HealthRequest)
    refused(
        make("pipeline").verify_request(
            "inference.v1.pipeline.health", CORE_REPLY, h, body, health
        ),
        "claims",
    )
    # aud matches the service, but the token names another subject.
    tok = signer.mint(body=body, aud="pipeline")
    r = make("pipeline").verify_request(
        "inference.v1.pipeline.health", CORE_REPLY, headers(tok, HEALTH_SCHEMA), body, health
    )
    refused(r, "binding")


def test_access_token_refused(make: Callable[..., Verifier], signer: Signer) -> None:
    # core's access-token shape: typ JWT, its own claim set.
    access = {
        "iss": "mhai-gateway",
        "aud": "mhai-bff",
        "sub": USER,
        "hid": HOSPITAL,
        "roles": ["clinician"],
        "sid": "synthetic-session",
        "iat": NOW,
        "exp": NOW + 900,
    }
    tok = signer.sign_raw(
        json.dumps({"alg": "EdDSA", "typ": "JWT", "kid": signer.kid}), json.dumps(access)
    )
    dec = never_decoder()
    refused(
        make().verify_request(PING_SUBJECT, CORE_REPLY, headers(tok), ping_body(), dec), "signature"
    )
    dec.assert_not_called()


def test_body_byte_flip_refused(make: Callable[..., Verifier], signer: Signer) -> None:
    body = ping_body()
    tok = signer.mint(body=body)
    flipped = bytes([body[0] ^ 1]) + body[1:]
    dec = never_decoder()
    refused(make().verify_request(PING_SUBJECT, CORE_REPLY, headers(tok), flipped, dec), "binding")
    dec.assert_not_called()


def test_body_hospital_mismatch_refused(make: Callable[..., Verifier], signer: Signer) -> None:
    body = ping_body(hospital=OTHER_HOSPITAL)
    tok = signer.mint(body=body)  # token for HOSPITAL
    handler = mock.Mock()
    r = make().verify_request(PING_SUBJECT, CORE_REPLY, headers(tok), body, ping_decoder)
    if isinstance(r, Verified):
        handler(r)
    r = refused(r, "scope")
    assert r.code == "invalid_request" and r.reply
    handler.assert_not_called()


def test_alg_none_hs256_unknown_kid_and_long_exp_refused(
    make: Callable[..., Verifier], signer: Signer
) -> None:
    body = ping_body()
    good = signer.mint(body=body)
    h, p, _ = good.split(".")

    def hdr(alg: str) -> str:
        return b64u(json.dumps({"alg": alg, "typ": "mhai-req+jwt", "kid": signer.kid}).encode())

    none = hdr("none") + "." + p + "."
    signing_input = hdr("HS256") + "." + p
    mac = hmac.new(signer.pub, signing_input.encode(), hashlib.sha256).digest()
    hs256 = signing_input + "." + b64u(mac)
    unknown = Signer(kid="0000000000000000").mint(body=body)
    long_exp = signer.mint(body=body, ttl=61)
    for tok, reason in (
        (none, "signature"),
        (hs256, "signature"),
        (unknown, "signature"),
        (long_exp, "claims"),
    ):
        dec = never_decoder()
        refused(make().verify_request(PING_SUBJECT, CORE_REPLY, headers(tok), body, dec), reason)
        dec.assert_not_called()
    # A job token may live 24 h, not more.
    jb = job_body()
    long_job = signer.mint_job(jb, ttl=24 * 3600 + 1)
    refused(
        make("pipeline").verify_job(JOB_SUBJECT, headers(long_job, JOB_SCHEMA), jb, job_decoder),
        "claims",
    )


# --- A15, A29, A37: the reply subject -------------------------------------------


def test_foreign_reply_subject_refused_silently(
    make: Callable[..., Verifier], signer: Signer, caplog: pytest.LogCaptureFixture
) -> None:
    body = ping_body()
    tok = signer.mint(body=body)
    with caplog.at_level(logging.INFO, logger="intelligence.boundary"):
        r = make().verify_request(PING_SUBJECT, "_INBOX_worker.x", headers(tok), body, ping_decoder)
    r = refused(r, "reply_subject")
    assert r.reply is False and r.code == "unauthorized"
    assert len(caplog.records) == 1
    line = caplog.records[0].getMessage()
    for want in (RID, "test-jti-0001", signer.real_kid, "reason=reply_subject"):
        assert want in line
    for secret in (tok, tok.split(".")[1], tok.split(".")[2], "synthetic ping", b64u(body)):
        assert secret not in line


def test_grant_bound_to_call(make: Callable[..., Verifier], signer: Signer) -> None:
    x, y = ping_body(text="synthetic X"), ping_body(text="synthetic Y")
    tok = signer.mint_grant(x)
    h = headers(None, rid=BFF_RID)
    v = make()
    refused(v.verify_granted(tok, PING_SUBJECT, BFF_REPLY, h, y, ping_decoder), "binding")
    refused(
        v.verify_granted(tok, "inference.v1.echo.other", BFF_REPLY, h, x, ping_decoder), "binding"
    )
    r = refused(
        v.verify_granted(tok, PING_SUBJECT, "_INBOX_gateway.x", h, x, ping_decoder), "reply_subject"
    )
    assert r.reply is False
    assert isinstance(v.verify_granted(tok, PING_SUBJECT, BFF_REPLY, h, x, ping_decoder), Verified)


def test_grant_reply_prefix_enforced(
    make: Callable[..., Verifier], signer: Signer, caplog: pytest.LogCaptureFixture
) -> None:
    body = ping_body()
    tok = signer.mint_grant(body)
    with caplog.at_level(logging.INFO, logger="intelligence.boundary"):
        r = make().verify_granted(
            tok,
            PING_SUBJECT,
            "_INBOX_bff.platform.x",
            headers(None, rid=BFF_RID),
            body,
            ping_decoder,
        )
    r = refused(r, "reply_subject")
    assert r.reply is False
    assert len(caplog.records) == 1
    # A grant for another app's inbox is refused too: only the clinical BFF calls directly.
    plat = signer.mint_grant(body, rpl="_INBOX_bff.platform.", jti="test-jti-0002")
    r = make().verify_granted(
        plat, PING_SUBJECT, "_INBOX_bff.platform.x", headers(None, rid=BFF_RID), body, ping_decoder
    )
    assert refused(r, "reply_subject").reply is False


def test_granted_needs_via_grant(make: Callable[..., Verifier], signer: Signer) -> None:
    body = ping_body()
    core_tok = signer.mint(body=body)  # via core, rpl _INBOX_gateway.
    r = make().verify_granted(core_tok, PING_SUBJECT, CORE_REPLY, headers(None), body, ping_decoder)
    assert refused(r, "reply_subject").reply is False


def test_bff_cannot_bring_own_token(make: Callable[..., Verifier], signer: Signer) -> None:
    body = ping_body()
    tok = signer.mint_grant(body)
    dec = never_decoder()
    r = make().verify_request(PING_SUBJECT, BFF_REPLY, headers(tok, rid=BFF_RID), body, dec)
    assert refused(r, "reply_subject").reply is False
    dec.assert_not_called()


def test_granted_operation_must_be_bff_direct(
    make: Callable[..., Verifier], signer: Signer
) -> None:
    body = b""
    tok = signer.mint_grant(body, aud="pipeline", nsub="inference.v1.pipeline.health", roles=[])
    r = make("pipeline").verify_granted(
        tok,
        "inference.v1.pipeline.health",
        BFF_REPLY,
        headers(None, HEALTH_SCHEMA, BFF_RID),
        body,
        strict_decoder(pipeline_pb2.HealthRequest),
    )
    assert refused(r, "forbidden").code == "forbidden"


# --- order -----------------------------------------------------------------------


def test_no_body_parse_before_signature(make: Callable[..., Verifier], signer: Signer) -> None:
    body = b"\xff" * 64  # not a protobuf: the decoder would raise
    good = signer.mint(body=body)
    h, p, s = good.split(".")
    bad_sig = h + "." + p + "." + b64u(bytes(64))
    dec = never_decoder()
    refused(
        make().verify_request(PING_SUBJECT, CORE_REPLY, headers(bad_sig), body, dec), "signature"
    )
    dec.assert_not_called()


def test_refusals_before_check_5_reply_only_to_core_inbox(
    make: Callable[..., Verifier], signer: Signer
) -> None:
    body = ping_body()
    bad = signer.mint(body=body, ttl=61)
    r = refused(
        make().verify_request(PING_SUBJECT, CORE_REPLY, headers(bad), body, ping_decoder), "claims"
    )
    assert r.reply is True
    # The reply subject wasn't checked yet: anything but core's inbox gets nothing.
    r = refused(
        make().verify_request(PING_SUBJECT, "_INBOX_worker.x", headers(bad), body, ping_decoder),
        "claims",
    )
    assert r.reply is False
    r = refused(
        make().verify_request(
            PING_SUBJECT, "$JS.API.STREAM.DELETE.JOBS", headers(None), body, ping_decoder
        ),
        "malformed",
    )
    assert r.reply is False


def test_check_sizes(make: Callable[..., Verifier], signer: Signer) -> None:
    v = make()
    assert v.check_sizes("x" * MAX_TOKEN_LEN, b"\0" * MAX_REQ_BODY, "req") is None
    assert (
        refused(v.check_sizes("x" * (MAX_TOKEN_LEN + 1), b"", "req"), "size").code == "unauthorized"
    )
    refused(
        v.check_sizes("é" * (MAX_TOKEN_LEN // 2 + 1), b"", "req"), "size"
    )  # bytes, not characters
    refused(v.check_sizes("", b"\0" * (MAX_REQ_BODY + 1), "req"), "size")
    refused(v.check_sizes("", b"\0" * (MAX_JOB_BODY + 1), "job"), "size")
    assert v.check_sizes("", b"\0" * MAX_JOB_BODY, "job") is None
    # Oversize before anything else, even with no token.
    dec = never_decoder()
    big = b"\0" * (MAX_REQ_BODY + 1)
    refused(v.verify_request(PING_SUBJECT, CORE_REPLY, headers(None), big, dec), "size")
    dec.assert_not_called()


# --- claims, expiry, F6 ------------------------------------------------------------


def test_expired_job_is_deadline_exceeded(
    make: Callable[..., Verifier], signer: Signer, clock: Clock
) -> None:
    body = job_body()
    tok = signer.mint_job(body, ttl=60)
    clock.now = NOW + 60
    r = refused(
        make("pipeline").verify_job(JOB_SUBJECT, headers(tok, JOB_SCHEMA), body, job_decoder),
        "expired",
    )
    assert r.code == "deadline_exceeded" and r.alert is False and r.reply is False


def test_expired_request_is_unauthorized(
    make: Callable[..., Verifier], signer: Signer, clock: Clock
) -> None:
    body = ping_body()
    tok = signer.mint(body=body)
    clock.now = NOW + 5
    r = refused(
        make().verify_request(PING_SUBJECT, CORE_REPLY, headers(tok), body, ping_decoder), "expired"
    )
    assert r.code == "unauthorized" and r.alert is True


def test_forged_expired_job_is_still_unauthorized(
    make: Callable[..., Verifier], clock: Clock
) -> None:
    body = job_body()
    tok = Signer(seed=b"\x01" * 32).mint_job(body, ttl=60)
    clock.now = NOW + 120
    r = refused(
        make("pipeline").verify_job(JOB_SUBJECT, headers(tok, JOB_SCHEMA), body, job_decoder),
        "signature",
    )
    assert r.code == "unauthorized" and r.alert is True


# --- schema, request id, scope ---------------------------------------------------------


def test_schema_header_must_match_operation(make: Callable[..., Verifier], signer: Signer) -> None:
    body = ping_body()
    tok = signer.mint(body=body)
    r = make().verify_request(
        PING_SUBJECT, CORE_REPLY, headers(tok, JOB_SCHEMA), body, ping_decoder
    )
    assert refused(r, "schema").code == "invalid_request"


def test_request_id_is_byte_exact(make: Callable[..., Verifier], signer: Signer) -> None:
    body = ping_body()
    tok = signer.mint(body=body)
    for rid in (RID.replace("-", "\u2010"), RID + "\n", RID + " ", " " + RID, RID[:-1], ""):
        v = make()
        r = v.verify_request(PING_SUBJECT, CORE_REPLY, headers(tok, rid=rid), body, ping_decoder)
        assert refused(r, "request_id").code == "invalid_request"


def test_body_decoded_strictly(make: Callable[..., Verifier], signer: Signer) -> None:
    body = ping_body() + b"\x98\x06\x01"  # unknown field 99, varint 1
    tok = signer.mint(body=body)
    r = make().verify_request(PING_SUBJECT, CORE_REPLY, headers(tok), body, ping_decoder)
    refused(r, "schema")
    garbage = b"\x0a\xff"
    tok = signer.mint(body=garbage, jti="test-jti-0002")
    refused(
        make().verify_request(PING_SUBJECT, CORE_REPLY, headers(tok), garbage, ping_decoder),
        "schema",
    )


def test_decoder_for_another_type_refused(make: Callable[..., Verifier], signer: Signer) -> None:
    body = b""
    tok = signer.mint(body=body)
    r = make().verify_request(
        PING_SUBJECT, CORE_REPLY, headers(tok), body, strict_decoder(pipeline_pb2.HealthRequest)
    )
    refused(r, "schema")


def test_global_job_for_hospital_operation_needs_hospital(
    make: Callable[..., Verifier], signer: Signer
) -> None:
    body = job_body()
    subject = "jobs.v1.global.run_query"
    tok = signer.mint_job(body, hid="", nsub=subject)
    r = make("pipeline").verify_job(subject, headers(tok, JOB_SCHEMA), body, job_decoder)
    assert refused(r, "no_hospital").code == "invalid_request"


def test_scope_ids_walks_patient_and_hospital_fields() -> None:
    m = authz_pb2.GrantRequest(patient_ids=[PATIENT, "p2"])
    assert scope_ids(m) == (set(), {PATIENT, "p2"})
    p = echo_pb2.PingRequest(hospital_id=HOSPITAL)
    assert scope_ids(p) == ({HOSPITAL}, set())
    assert scope_ids(echo_pb2.PingRequest()) == (set(), set())


def test_every_request_schema_names_scope_fields_as_the_walker_reads_them(ops: Operations) -> None:
    """A hospital or patient field under another name would slip past check 8."""
    from google.protobuf import descriptor_pool
    from google.protobuf.descriptor import Descriptor, FieldDescriptor

    pool = descriptor_pool.Default()
    known = {"hospital_id", "hospital_ids", "patient_id", "patient_ids"}

    def walk(d: Descriptor, seen: set[str]) -> None:
        if d.full_name in seen:
            return
        seen.add(d.full_name)
        for f in d.fields:
            if f.type == FieldDescriptor.TYPE_MESSAGE:
                walk(f.message_type, seen)
            elif ("hospital" in f.name or "patient" in f.name) and f.name not in known:
                raise AssertionError(f"{d.full_name}.{f.name} is not a field check 8 reads")
            elif f.name in known:
                assert f.type == FieldDescriptor.TYPE_STRING, f.full_name

    for o in ops:
        walk(pool.FindMessageTypeByName(o.request_schema), set())


# --- permission ------------------------------------------------------------------------


def test_role_not_in_operation_forbidden(make: Callable[..., Verifier], signer: Signer) -> None:
    body = ping_body()
    tok = signer.mint(body=body, roles=["analyst"])
    r = make().verify_request(PING_SUBJECT, CORE_REPLY, headers(tok), body, ping_decoder)
    r = refused(r, "forbidden")
    assert r.code == "forbidden" and r.reply


def test_svc_core_only_where_allowed(make: Callable[..., Verifier], signer: Signer) -> None:
    body = ping_body()
    tok = signer.mint(body=body, sub="svc:core", roles=[])
    assert isinstance(
        make().verify_request(PING_SUBJECT, CORE_REPLY, headers(tok), body, ping_decoder), Verified
    )
    jb = job_body()
    jtok = signer.mint_job(jb, sub="svc:core", roles=[])
    r = make("pipeline").verify_job(JOB_SUBJECT, headers(jtok, JOB_SCHEMA), jb, job_decoder)
    refused(r, "forbidden")
    # svc:core with roles is something core never mints.
    tok = signer.mint(body=body, sub="svc:core", roles=["clinician"], jti="test-jti-0003")
    r = make().verify_request(PING_SUBJECT, CORE_REPLY, headers(tok), body, ping_decoder)
    refused(r, "forbidden")


def test_job_subject_hospital_must_equal_hid(make: Callable[..., Verifier], signer: Signer) -> None:
    body = job_body()
    other = f"jobs.v1.{OTHER_HOSPITAL}.run_query"
    tok = signer.mint_job(body, nsub=other)  # hid HOSPITAL, subject names another
    dec = never_decoder()
    r = make("pipeline").verify_job(other, headers(tok, JOB_SCHEMA), body, dec)
    refused(r, "binding")
    dec.assert_not_called()


def test_job_token_on_request_path_refused(make: Callable[..., Verifier], signer: Signer) -> None:
    body = job_body()
    tok = signer.mint_job(body)
    r = make("pipeline").verify_request(
        JOB_SUBJECT, CORE_REPLY, headers(tok, JOB_SCHEMA), body, job_decoder
    )
    refused(r, "binding")


def test_operation_of_another_service_refused(
    make: Callable[..., Verifier], signer: Signer
) -> None:
    body = ping_body()
    tok = signer.mint(body=body, aud="pipeline", nsub="inference.v1.echo.ping")
    r = make("pipeline").verify_request(PING_SUBJECT, CORE_REPLY, headers(tok), body, ping_decoder)
    # Go's binding doesn't tie the subject's service to aud; the operation
    # lookup does: echo.ping isn't one of pipeline's.
    refused(r, "schema")


def test_replay_set_full_is_overloaded(
    make: Callable[..., Verifier], signer: Signer, clock: Clock
) -> None:
    replay = JtiSet(clock, max_entries=1)
    v = make(replay=replay)
    body = ping_body()
    assert isinstance(
        v.verify_request(
            PING_SUBJECT, CORE_REPLY, headers(signer.mint(body=body)), body, ping_decoder
        ),
        Verified,
    )
    tok = signer.mint(body=body, jti="test-jti-0002")
    r = refused(
        v.verify_request(PING_SUBJECT, CORE_REPLY, headers(tok), body, ping_decoder), "overloaded"
    )
    assert r.code == "overloaded" and r.reply


# --- Verified can't be faked ------------------------------------------------------------


def test_verified_cannot_be_built_outside(make: Callable[..., Verifier], signer: Signer) -> None:
    with pytest.raises(TypeError):
        Verified()  # type: ignore[call-arg]
    body = ping_body()
    v = make().verify_request(
        PING_SUBJECT, CORE_REPLY, headers(signer.mint(body=body)), body, ping_decoder
    )
    assert isinstance(v, Verified)
    with pytest.raises(TypeError):
        dataclasses.replace(v, hid=OTHER_HOSPITAL)
    with pytest.raises(dataclasses.FrozenInstanceError):
        v.hid = OTHER_HOSPITAL  # type: ignore[misc]


def test_refusal_reasons_map_to_codes(make: Callable[..., Verifier]) -> None:
    from intelligence.boundary.verify import REASON_CODES

    assert {r: REASON_CODES[r] for r in REASON_CODES} == {
        "size": "unauthorized",
        "malformed": "unauthorized",
        "signature": "unauthorized",
        "claims": "unauthorized",
        "expired": "unauthorized",
        "binding": "unauthorized",
        "reply_subject": "unauthorized",
        "replay": "unauthorized",
        "overloaded": "overloaded",
        "schema": "invalid_request",
        "request_id": "invalid_request",
        "scope": "invalid_request",
        "no_hospital": "invalid_request",
        "forbidden": "forbidden",
        "internal": "internal",
    }
    assert set(REASON_CODES.values()) <= envelope.CODES


def test_refusal_metric_counts_reason(make: Callable[..., Verifier], signer: Signer) -> None:
    v = make()
    v.verify_request(PING_SUBJECT, CORE_REPLY, headers(None), ping_body(), ping_decoder)
    assert v.refusals[("echo", "malformed")] == 1


# --- fix round 1 ------------------------------------------------------------------


def _payload_json(signer: Signer, body: bytes) -> str:
    return json.dumps(signer.claims(body=body), separators=(",", ":"))


HDR = '{"alg":"EdDSA","typ":"mhai-req+jwt","kid":"%s"}'


@pytest.mark.parametrize(
    "label,edit,reason",
    [
        ("NaN iat", lambda p: p.replace(f'"iat":{NOW}', '"iat":NaN'), "malformed"),
        ("Infinity exp", lambda p: p.replace(f'"exp":{NOW + 5}', '"exp":Infinity'), "malformed"),
        (
            "escaped duplicate hid",
            lambda p: p[:-1] + f',"\\u0068id":"{OTHER_HOSPITAL}"}}',
            "malformed",
        ),
        ("BOM", lambda p: "\ufeff" + p, "malformed"),
        ("trailing data", lambda p: p + "{}", "malformed"),
        ("trailing whitespace", lambda p: p + " \n", "ok"),
        ("lone surrogate", lambda p: p.replace(f'"sub":"{USER}"', '"sub":"\\ud800"'), "malformed"),
        ("minus zero iat", lambda p: p.replace(f'"iat":{NOW}', '"iat":-0'), "claims"),
        ("exponent iat", lambda p: p.replace(f'"iat":{NOW}', '"iat":1e3'), "malformed"),
        (
            "iat over int64",
            lambda p: p.replace(f'"iat":{NOW}', '"iat":9223372036854775808'),
            "malformed",
        ),
        (
            "roles a string",
            lambda p: p.replace('"roles":["clinician"]', '"roles":"clinician"'),
            "malformed",
        ),
    ],
)
def test_payload_parser_differentials(
    make: Callable[..., Verifier],
    signer: Signer,
    label: str,
    edit: Callable[[str], str],
    reason: str,
) -> None:
    body = ping_body()
    p = _payload_json(signer, body)
    edited = edit(p)
    assert edited != p, label
    tok = signer.sign_raw(HDR % signer.kid, edited)
    got = make().check_token(tok, PING_SUBJECT, CORE_REPLY, "req", body) or "ok"
    assert got == reason, label


@pytest.mark.parametrize(
    "label,header,reason",
    [
        ("deep nesting", '{"alg":' + "[" * 2500, "malformed"),
        (
            "duplicate alg",
            '{"alg":"none","alg":"EdDSA","typ":"mhai-req+jwt","kid":"K"}',
            "malformed",
        ),
        ("kid a number", '{"alg":"EdDSA","typ":"mhai-req+jwt","kid":7}', "malformed"),
        ("extra header", '{"alg":"EdDSA","typ":"mhai-req+jwt","kid":"K","jku":"x"}', "malformed"),
    ],
)
def test_header_parser_differentials(
    make: Callable[..., Verifier], signer: Signer, label: str, header: str, reason: str
) -> None:
    """Unsigned headers: refused before any key or signature is looked at."""
    body = ping_body()
    tok = b64u(header.replace('"K"', f'"{signer.kid}"').encode()) + "." + b64u(b"{}") + "."
    assert len(tok) <= 4096, label
    assert make().check_token(tok, PING_SUBJECT, CORE_REPLY, "req", body) == reason, label


def test_unexpected_exception_is_internal(
    make: Callable[..., Verifier],
    signer: Signer,
    monkeypatch: pytest.MonkeyPatch,
    caplog: pytest.LogCaptureFixture,
) -> None:
    import intelligence.boundary.verify as verify_mod

    def boom(_: object) -> object:
        raise RuntimeError("synthetic secret detail")

    monkeypatch.setattr(verify_mod, "scope_ids", boom)
    body = ping_body()
    with caplog.at_level(logging.INFO, logger="intelligence.boundary"):
        r = make().verify_request(
            PING_SUBJECT, CORE_REPLY, headers(signer.mint(body=body)), body, ping_decoder
        )
    r = refused(r, "internal")
    assert (r.code, r.reply, r.alert) == ("internal", True, True)
    line = caplog.records[-1].getMessage()
    assert "RuntimeError" in line and "synthetic secret detail" not in line

    # Before check 5 passed, no reply.
    def bad_clock() -> float:
        raise RuntimeError("synthetic")

    v = make()
    v._clock = bad_clock
    r = refused(
        v.verify_request(PING_SUBJECT, CORE_REPLY, headers(None), body, ping_decoder), "internal"
    )
    assert r.reply is False


def test_verifier_and_replay_share_one_clock(signer: Signer, ops: Operations, clock: Clock) -> None:
    trusted = TrustedKeys.from_encoded(signer.encoded_keys())
    with pytest.raises(ValueError):
        Verifier(trusted, "echo", ops, JtiSet(Clock(NOW)), clock)
    Verifier(trusted, "echo", ops, JtiSet(clock), clock)


def test_only_verify_issues_verified() -> None:
    """Nothing outside verify.py names the issuing token or method."""
    root = Path(__file__).resolve().parents[2] / "intelligence"
    offenders = [
        str(p.relative_to(root))
        for p in root.rglob("*.py")
        if p.name != "verify.py" and re.search(r"\b_ISSUE\b|\b_issue\b", p.read_text())
    ]
    assert offenders == []
    tests = Path(__file__).resolve().parent
    assert not [
        p.name
        for p in tests.rglob("*.py")
        if p.name != Path(__file__).name and re.search(r"\b_ISSUE\b|\b_issue\b", p.read_text())
    ]

"""A25: the Python verifier gives every shared request-token vector the
verdict Go gives it (testdata/request-tokens/*.json, written by
services/core/internal/reqtoken/vectors_test.go).

The verdict covers checks 1-5 of spec §7.3, the checks the token alone
decides. A valid vector whose subject has an operation also goes through
the full verifier (checks 1-9) and must come out Verified. No vector is
skipped, and the count must match the directory.
"""

from __future__ import annotations

import base64
import json
from pathlib import Path
from typing import Any

from intelligence.boundary import envelope
from intelligence.boundary.keys import TrustedKeys
from intelligence.boundary.operations import Operations
from intelligence.boundary.pb import echo_pb2, pipeline_pb2
from intelligence.boundary.replay import JtiSet
from intelligence.boundary.verify import REASON_CODES, Verified, Verifier, strict_decoder
from tests.boundary.tokens import Signer

VECTORS = Path(__file__).resolve().parents[4] / "testdata" / "request-tokens"

FIELDS = {
    "name",
    "note",
    "token",
    "keys",
    "body",
    "subject",
    "reply",
    "request_id",
    "schema",
    "knd",
    "granted",
    "service",
    "now",
    "verdict",
}

DECODERS = {
    "mhai.intelligence.v1.PingRequest": strict_decoder(echo_pb2.PingRequest),
    "mhai.intelligence.v1.RunQueryJob": strict_decoder(pipeline_pb2.RunQueryJob),
}


def _load() -> list[dict[str, Any]]:
    out = []
    for path in sorted(VECTORS.glob("*.json")):
        v = json.loads(path.read_text())
        assert set(v) == FIELDS, path.name
        assert v["name"] + ".json" == path.name
        out.append(v)
    return out


def _verifier(v: dict[str, Any], ops: Operations) -> Verifier:
    now = float(v["now"])

    def clock() -> float:
        return now

    return Verifier(TrustedKeys.from_encoded(v["keys"]), v["service"], ops, JtiSet(clock), clock)


def _full(v: dict[str, Any], verifier: Verifier, body: bytes) -> object:
    hdr = {envelope.SCHEMA: v["schema"], envelope.REQUEST_ID: v["request_id"]}
    dec = DECODERS[v["schema"]]
    if v["knd"] == "job":
        hdr[envelope.TOKEN] = v["token"]
        return verifier.verify_job(v["subject"], hdr, body, dec)
    if v["granted"]:
        return verifier.verify_granted(v["token"], v["subject"], v["reply"], hdr, body, dec)
    hdr[envelope.TOKEN] = v["token"]
    return verifier.verify_request(v["subject"], v["reply"], hdr, body, dec)


def test_shared_token_vectors() -> None:
    vectors = _load()
    files = sorted(VECTORS.glob("*.json"))
    assert len(vectors) == len(files) >= 40, "drift guard: every file is a vector"
    ops = Operations.load(development=True)
    checked, full, mismatches = 0, 0, []
    for v in vectors:
        body = base64.b64decode(v["body"], validate=True)
        verifier = _verifier(v, ops)
        got = verifier.check_token(
            v["token"], v["subject"], v["reply"], v["knd"], body, granted=v["granted"]
        )
        reason = got if isinstance(got, str) else "ok"
        if reason != v["verdict"]:
            mismatches.append(f"{v['name']}: python {reason!r}, vector {v['verdict']!r}")
        assert v["verdict"] == "ok" or v["verdict"] in REASON_CODES, v["name"]
        checked += 1
        if v["verdict"] == "ok" and ops.for_subject(v["subject"]) is not None:
            result = _full(v, _verifier(v, ops), body)
            if not isinstance(result, Verified):
                mismatches.append(f"{v['name']}: full verify refused {result}")
            full += 1
    assert not mismatches, "\n".join(mismatches)
    assert checked == len(files), "no vector skipped"
    # valid_req, valid_job, valid_grant, iat_at_skew, exp_iat_60s (valid_global_job has no operation).
    assert full == 5


def test_test_signer_matches_go_vector() -> None:
    """The test-only signer mints valid_req byte for byte as core does."""
    v = json.loads((VECTORS / "valid_req.json").read_text())
    payload = json.loads(base64.urlsafe_b64decode(v["token"].split(".")[1] + "=="))
    signer = Signer()
    assert signer.encoded_keys() == v["keys"]
    tok = signer.mint(
        body=base64.b64decode(v["body"]),
        jti=payload["jti"],
        iat=payload["iat"],
        ttl=payload["exp"] - payload["iat"],
    )
    assert tok == v["token"]

"""TEST-ONLY request-token minting for the boundary tests.

It signs with the same test-only seed as core's shared vectors
(services/core/internal/reqtoken/vectors_test.go), so a token minted here
can be checked byte for byte against a Go vector. Every id is synthetic.
Nothing outside tests may import this module.
"""

from __future__ import annotations

import base64
import hashlib
import json
from collections.abc import Callable
from typing import Any

from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey

SEED = hashlib.sha256(b"mhai test-only request-token vector seed; never a real key").digest()

USER = "11111111-1111-4111-8111-111111111111"
HOSPITAL = "22222222-2222-4222-8222-222222222222"
OTHER_HOSPITAL = "33333333-3333-4333-8333-333333333333"
RID = "44444444-4444-4444-8444-444444444444"
PATIENT = "55555555-5555-4555-8555-555555555555"
QUERY_ID = "66666666-6666-4666-8666-666666666666"
BFF_RID = "77777777-7777-4777-8777-777777777777"

# The vectors' fixed mint time (2026-10-28T12:00:00Z).
NOW = 1793188800

PING_SCHEMA = "mhai.intelligence.v1.PingRequest"
JOB_SCHEMA = "mhai.intelligence.v1.RunQueryJob"
HEALTH_SCHEMA = "mhai.intelligence.v1.HealthRequest"
CORE_REPLY = "_INBOX_gateway.test.1"
BFF_REPLY = "_INBOX_bff.clinical.test.1"


def b64u(raw: bytes) -> str:
    return base64.urlsafe_b64encode(raw).rstrip(b"=").decode("ascii")


def body_hash(body: bytes) -> str:
    return b64u(hashlib.sha256(body).digest())


def key_id(pub: bytes) -> str:
    return hashlib.sha256(pub).hexdigest()[:16]


def _compact(obj: Any) -> str:
    return json.dumps(obj, separators=(",", ":"))


class Signer:
    """Mints tokens the way core's reqtoken.Signer does (same JSON order)."""

    def __init__(self, seed: bytes = SEED, kid: str | None = None) -> None:
        self.priv = Ed25519PrivateKey.from_private_bytes(seed)
        self.pub = self.priv.public_key().public_bytes(
            serialization.Encoding.Raw, serialization.PublicFormat.Raw
        )
        self.real_kid = key_id(self.pub)
        self.kid = kid if kid is not None else self.real_kid

    def encoded_keys(self) -> dict[str, str]:
        return {self.real_kid: b64u(self.pub)}

    def sign_raw(self, header_json: str, payload_json: str) -> str:
        signing_input = b64u(header_json.encode()) + "." + b64u(payload_json.encode())
        return signing_input + "." + b64u(self.priv.sign(signing_input.encode("ascii")))

    def claims(
        self,
        *,
        aud: str = "echo",
        sub: str = USER,
        hid: str = HOSPITAL,
        roles: list[str] | None = None,
        rid: str = RID,
        jti: str = "test-jti-0001",
        iat: int = NOW,
        ttl: int = 5,
        knd: str = "req",
        nsub: str = "inference.v1.echo.ping",
        body: bytes = b"",
        pids: list[str] | None = None,
        rpl: str | None = "_INBOX_gateway.",
        via: str = "core",
    ) -> dict[str, Any]:
        p: dict[str, Any] = {
            "iss": "mhai-gateway",
            "aud": aud,
            "sub": sub,
            "hid": hid,
            "roles": ["clinician"] if roles is None else roles,
            "rid": rid,
            "jti": jti,
            "iat": iat,
            "exp": iat + ttl,
            "knd": knd,
            "nsub": nsub,
            "bh": body_hash(body),
        }
        if pids:
            p["pids"] = pids
        if rpl:
            p["rpl"] = rpl
        p["via"] = via
        return p

    def mint(
        self,
        *,
        header: dict[str, Any] | None = None,
        edit: Callable[[dict[str, Any]], None] | None = None,
        **kw: Any,
    ) -> str:
        p = self.claims(**kw)
        if edit is not None:
            edit(p)
        h = (
            header
            if header is not None
            else {"alg": "EdDSA", "typ": "mhai-req+jwt", "kid": self.kid}
        )
        return self.sign_raw(_compact(h), _compact(p))

    def mint_job(self, body: bytes, **kw: Any) -> str:
        defaults: dict[str, Any] = {
            "aud": "pipeline",
            "knd": "job",
            "nsub": f"jobs.v1.{HOSPITAL}.run_query",
            "rpl": None,
            "ttl": 600,
        }
        defaults.update(kw)
        return self.mint(body=body, **defaults)

    def mint_grant(self, body: bytes, **kw: Any) -> str:
        defaults: dict[str, Any] = {
            "rid": BFF_RID,
            "via": "grant",
            "rpl": "_INBOX_bff.clinical.",
            "ttl": 2,
        }
        defaults.update(kw)
        return self.mint(body=body, **defaults)

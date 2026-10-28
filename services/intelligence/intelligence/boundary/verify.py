"""The request-token verifier: spec §7.3's checks, in order, before any work.

Written by hand to mirror core's reqtoken.VerifyAt and CheckReply exactly
(one algorithm, one header shape, a fixed claim set, strict JSON), and held
to it by the shared vectors in testdata/request-tokens. No JWT library:
Ed25519 is cryptography's.

    1  sizes                      unauthorized
    2  shape and signature        unauthorized
    3  claims                     unauthorized (an expired job: deadline_exceeded, F6)
    4  binding                    unauthorized
    5  reply subject              unauthorized, and no reply at all
    6  replay (request/reply)     unauthorized (the set full: overloaded)
    7  schema, request id, decode invalid_request
    8  scope                      invalid_request
    9  permission                 forbidden (spec §10.1, F33)

Nothing reads the body before check 7 except the sha256 of check 4. Logs
carry the reason and ids from the signed token only, never the token or the
body.
"""

from __future__ import annotations

import hashlib
import hmac
import logging
import math
import re
import threading
import time
from collections import Counter
from collections.abc import Callable, Iterator, Mapping
from dataclasses import dataclass, field
from typing import Any, NoReturn

from cryptography.exceptions import InvalidSignature
from google.protobuf.descriptor import FieldDescriptor
from google.protobuf.message import Message
from google.protobuf.unknown_fields import UnknownFieldSet

from intelligence.boundary import _b64, _strictjson, envelope
from intelligence.boundary.keys import TrustedKeys
from intelligence.boundary.operations import Operation, Operations
from intelligence.boundary.replay import JtiSet, ReplaySetFull

log = logging.getLogger("intelligence.boundary.verify")

# Fixed values: core's reqtoken constants.
TYPE = "mhai-req+jwt"
ISSUER = "mhai-gateway"
SVC_CORE = "svc:core"
MAX_TTL = {"req": 60, "job": 24 * 3600}
SKEW = 5
MAX_TOKEN_LEN = 4096
MAX_REQ_BODY = 256 << 10
MAX_JOB_BODY = 64 << 10
MAX_BODY = {"req": MAX_REQ_BODY, "job": MAX_JOB_BODY}
REPLY_CORE = "_INBOX_gateway."
REPLY_BFF = "_INBOX_bff."
# The only BFF that may call a worker directly (spec §17.2).
REPLY_GRANT = "_INBOX_bff.clinical."
MAX_TIME = 1 << 53
_INT64 = (-(1 << 63), (1 << 63) - 1)

_HEADER_KEYS = frozenset({"alg", "typ", "kid"})
_REQUIRED = frozenset(
    {"iss", "aud", "sub", "hid", "roles", "rid", "jti", "iat", "exp", "knd", "nsub", "bh", "via"}
)
_OPTIONAL = frozenset({"pids", "rpl"})
_STRINGS = frozenset({"iss", "aud", "sub", "hid", "rid", "jti", "knd", "nsub", "bh", "via", "rpl"})
_NAME = re.compile(r"[a-z0-9_]+")
_SAFE = re.compile(r"[A-Za-z0-9_.:\-]{1,128}")

# reason -> the code a refused request is answered with.
REASON_CODES: dict[str, str] = {
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

Decoder = Callable[[bytes], Message]


@dataclass(frozen=True)
class Refusal:
    """A refused message. reason is the shared reason code (logs and metrics);
    code is what the worker answers; reply is False when no reply may be
    sent (check 5 failed or never ran, or a job); alert is False only for a
    job that expired in the queue (F6: deadline_exceeded, not an attack)."""

    reason: str
    code: str
    reply: bool
    alert: bool = True


class _Refused(Exception):
    def __init__(self, reason: str) -> None:
        super().__init__(reason)
        self.reason = reason


def _refuse(reason: str) -> NoReturn:
    raise _Refused(reason)


_ISSUE = object()


@dataclass(frozen=True, init=False)
class Verified:
    """A message that passed every check. Only Verifier makes one: the
    constructor and dataclasses.replace raise."""

    sub: str
    hid: str
    roles: tuple[str, ...]
    rid: str
    jti: str
    kid: str
    kind: str
    subject: str
    operation: Operation
    patient_ids: tuple[str, ...]
    exp: int
    via: str
    message: Message = field(repr=False, compare=False)

    def __init__(self, *_args: Any, **_kwargs: Any) -> None:
        raise TypeError("only the Verifier makes a Verified")

    @classmethod
    def _issue(cls, key: object, **values: Any) -> Verified:
        if key is not _ISSUE:
            raise TypeError("only the Verifier makes a Verified")
        obj = object.__new__(cls)
        for k, v in values.items():
            object.__setattr__(obj, k, v)
        return obj


@dataclass(frozen=True)
class _Claims:
    kid: str
    iss: str
    aud: str
    sub: str
    hid: str
    roles: tuple[str, ...]
    rid: str
    jti: str
    iat: int
    exp: int
    knd: str
    nsub: str
    bh: str
    pids: tuple[str, ...]
    rpl: str
    via: str


def strict_decoder(cls: type[Message]) -> Decoder:
    """A decoder for cls that refuses unknown fields (here or nested) and
    anything protobuf can't parse."""

    def decode(body: bytes) -> Message:
        m = cls()
        m.ParseFromString(body)
        if _has_unknown(m):
            raise ValueError("unknown fields")
        return m

    return decode


def _sub_messages(value: Any) -> Iterator[Message]:
    """The messages in a field value: a message, a repeated field or a map."""
    if isinstance(value, Message):
        yield value
    elif isinstance(value, Mapping):
        yield from (v for v in value.values() if isinstance(v, Message))
    else:
        yield from (v for v in value if isinstance(v, Message))


def _has_unknown(m: Message) -> bool:
    if len(UnknownFieldSet(m)):
        return True
    for fd, value in m.ListFields():
        if fd.type == FieldDescriptor.TYPE_MESSAGE and any(
            _has_unknown(v) for v in _sub_messages(value)
        ):
            return True
    return False


_HOSPITAL_FIELDS = frozenset({"hospital_id", "hospital_ids"})
_PATIENT_FIELDS = frozenset({"patient_id", "patient_ids"})


def scope_ids(m: Message) -> tuple[set[str], set[str]]:
    """The hospital ids and patient ids a message names: every string field
    called hospital_id(s) or patient_id(s), at any depth. An empty singular
    field is unset; an empty element of a repeated one is kept (and refused)."""
    hospitals: set[str] = set()
    patients: set[str] = set()

    def walk(msg: Message) -> None:
        for fd, value in msg.ListFields():
            if fd.type == FieldDescriptor.TYPE_MESSAGE:
                for sub in _sub_messages(value):
                    walk(sub)
                continue
            if fd.type != FieldDescriptor.TYPE_STRING:
                continue
            if fd.name in _HOSPITAL_FIELDS:
                target = hospitals
            elif fd.name in _PATIENT_FIELDS:
                target = patients
            else:
                continue
            if isinstance(value, str):
                if value:
                    target.add(value)
            else:
                target.update(value)

    walk(m)
    return hospitals, patients


def _body_hash(body: bytes) -> str:
    return _b64.encode(hashlib.sha256(body).digest())


def _is_bff_prefix(p: str) -> bool:
    if not p.startswith(REPLY_BFF) or not p.endswith("."):
        return False
    return _NAME.fullmatch(p[len(REPLY_BFF) : -1]) is not None


def _safe(v: str) -> str:
    """An id for a log line: shown only if it is plainly an id."""
    return v if _SAFE.fullmatch(v) else "?"


def _decode_json(segment: bytes) -> dict[str, Any]:
    try:
        doc = _strictjson.loads(segment)
    except _strictjson.StrictJSONError:
        _refuse("malformed")
    if not isinstance(doc, dict):
        _refuse("malformed")
    if any(v is None for v in doc.values()):
        _refuse("malformed")  # core refuses null for any claim
    return doc


def _parse_payload(raw: bytes, kid: str) -> tuple[_Claims, frozenset[str]]:
    """Strict claims, then check 3's presence rule. Type errors are
    malformed (Go's Unmarshal fails); a missing claim is claims."""
    p = _decode_json(raw)
    present = frozenset(p)
    if not present <= _REQUIRED | _OPTIONAL:
        _refuse("malformed")  # unknown or case-folded claim
    # Types, as Go's json.Unmarshal into the payload struct would refuse.
    for k in present & _STRINGS:
        if not isinstance(p[k], str):
            _refuse("malformed")
    for k in present & {"iat", "exp"}:
        v = p[k]
        if not _strictjson.is_int(v) or not _INT64[0] <= v <= _INT64[1]:
            _refuse("malformed")
    for k in present & {"roles", "pids"}:
        v = p[k]
        if not _strictjson.is_str_list(v) or any(x == "" for x in v):
            _refuse("malformed")
    # 3. Claims: every required claim is present.
    if not _REQUIRED <= present:
        _refuse("claims")
    claims = _Claims(
        kid=kid,
        iss=p["iss"],
        aud=p["aud"],
        sub=p["sub"],
        hid=p["hid"],
        roles=tuple(p["roles"]),
        rid=p["rid"],
        jti=p["jti"],
        iat=p["iat"],
        exp=p["exp"],
        knd=p["knd"],
        nsub=p["nsub"],
        bh=p["bh"],
        pids=tuple(p.get("pids", ())),
        rpl=p.get("rpl", ""),
        via=p["via"],
    )
    return claims, present


class Verifier:
    """Runs the checks for one service. Thread-safe (the replay set locks)."""

    def __init__(
        self,
        trusted: TrustedKeys,
        service: str,
        operations: Operations,
        replay: JtiSet,
        clock: Callable[[], float] = time.time,
    ) -> None:
        self._trusted = trusted
        self.service = service
        self._ops = operations
        self._replay = replay
        if replay.clock is not clock:
            # One clock for exp and for the replay window, or a jti could be
            # forgotten while its token is still live.
            raise ValueError("the replay set must use the verifier's clock")
        self._clock = clock
        # boundary_refusals_total{service,reason}; exported by telemetry (T14).
        self.refusals: Counter[tuple[str, str]] = Counter()
        self._count_lock = threading.Lock()

    # --- check 1 ------------------------------------------------------------------

    def check_sizes(self, token: str, body: bytes, kind: str) -> Refusal | None:
        """Check 1, the one size check (F25): the token at most 4 KB, the body
        within its kind's cap. The grant path calls it before asking core."""
        try:
            self._sizes(token, body, kind)
        except _Refused as r:
            return self._refusal(r.reason, kind, reply=False)
        return None

    @staticmethod
    def _sizes(token: str, body: bytes, kind: str) -> None:
        limit = MAX_BODY.get(kind)
        if limit is None:
            _refuse("binding")
        if (
            len(token) > MAX_TOKEN_LEN
            or len(token.encode("utf-8", "surrogatepass")) > MAX_TOKEN_LEN
        ):
            _refuse("size")
        if len(body) > limit:
            _refuse("size")

    # --- checks 1-5: the token alone ----------------------------------------------

    def _token(
        self,
        token: str,
        subject: str,
        reply: str,
        kind: str,
        body: bytes,
        granted: bool,
        signed: list[_Claims] | None = None,
    ) -> _Claims:
        now = math.floor(self._clock())
        # 1. Sizes.
        self._sizes(token, body, kind)
        # 2. Shape and signature.
        parts = token.split(".")
        if len(parts) != 3:
            _refuse("malformed")
        try:
            hraw, praw, sig = (_b64.decode(s) for s in parts)
        except _b64.B64Error:
            _refuse("malformed")
        h = _decode_json(hraw)
        if set(h) != _HEADER_KEYS or not all(isinstance(v, str) for v in h.values()):
            _refuse("malformed")
        if h["alg"] != "EdDSA" or h["typ"] != TYPE:
            _refuse("signature")
        key = self._trusted.get(h["kid"])
        if key is None or len(sig) != 64:
            _refuse("signature")
        try:
            key.verify(sig, (parts[0] + "." + parts[1]).encode("ascii"))
        except InvalidSignature:
            _refuse("signature")
        c, present = _parse_payload(praw, h["kid"])
        if signed is not None:
            signed.append(c)  # core signed these: safe to log their ids
        # 3. Claims.
        limit = MAX_TTL.get(c.knd)
        if (
            limit is None
            or c.iss != ISSUER
            or c.aud == ""
            or c.aud != self.service
            or "" in (c.sub, c.rid, c.jti, c.nsub, c.bh)
        ):
            _refuse("claims")
        if (
            c.via not in ("core", "grant")
            or (c.knd == "req" and c.rpl == "")
            or (c.knd == "job" and ("rpl" in present or c.via != "core"))
            or (c.hid == "" and not c.nsub.startswith("jobs.v1.global."))
            or not (0 < c.iat <= MAX_TIME and 0 < c.exp <= MAX_TIME)
            or c.exp <= c.iat
            or c.exp - c.iat > limit
            or c.iat > now + SKEW
        ):
            _refuse("claims")
        if now >= c.exp:
            _refuse("expired")
        # 4. Binding.
        if c.nsub != subject or c.knd != kind:
            _refuse("binding")
        seg = c.nsub.split(".")
        if c.knd == "req":
            if len(seg) != 4 or seg[0] != "inference" or seg[1] != "v1":
                _refuse("binding")
        elif (
            len(seg) != 4
            or seg[0] != "jobs"
            or seg[1] != "v1"
            or (seg[2] != "global" and seg[2] != c.hid)
        ):
            _refuse("binding")
        if not hmac.compare_digest(
            c.bh.encode("utf-8", "surrogatepass"), _body_hash(body).encode()
        ):
            _refuse("binding")
        # 5. Reply subject (request/reply).
        if kind == "req":
            self._reply(c, reply, granted)
        return c

    @staticmethod
    def _reply(c: _Claims, reply: str, granted: bool) -> None:
        """Check 5, as core's CheckReply. A token found in the message is
        core's own (via core, rpl _INBOX_gateway.); a granted one must be via
        grant for the clinical BFF's inbox (spec §17.2)."""
        if c.knd != "req":
            _refuse("reply_subject")
        if granted:
            if c.via != "grant" or not _is_bff_prefix(c.rpl) or c.rpl != REPLY_GRANT:
                _refuse("reply_subject")
        elif c.via != "core" or c.rpl != REPLY_CORE:
            _refuse("reply_subject")
        if len(reply) <= len(c.rpl) or not reply.startswith(c.rpl):
            _refuse("reply_subject")

    def check_token(
        self, token: str, subject: str, reply: str, kind: str, body: bytes, *, granted: bool = False
    ) -> str | None:
        """Checks 1-5 only, for the shared vectors: None when they pass, else
        the reason. Workers call verify_request, verify_job or verify_granted."""
        try:
            self._token(token, subject, reply, kind, body, granted)
        except _Refused as r:
            return r.reason
        return None

    # --- checks 6-9 -------------------------------------------------------------

    def _rest(
        self,
        c: _Claims,
        kind: str,
        headers: Mapping[str, str],
        body: bytes,
        decoder: Decoder,
        granted: bool,
    ) -> Verified:
        # 6. Replay (request/reply; jobs are claimed in the database, §7.4).
        if kind == "req":
            try:
                if self._replay.seen_or_add(c.jti, c.exp):
                    _refuse("replay")
            except ReplaySetFull:
                _refuse("overloaded")
        # 7. Schema: the operation, its request type, the request id, then decode.
        op = self._ops.for_subject(c.nsub)
        if op is None or op.service != self.service or op.kind != kind:
            _refuse("schema")
        if headers.get(envelope.SCHEMA) != op.request_schema:
            _refuse("schema")
        rid = headers.get(envelope.REQUEST_ID)
        if not isinstance(rid, str) or rid != c.rid:  # byte-exact, no normalising
            _refuse("request_id")
        try:
            msg = decoder(body)
        except Exception:
            _refuse("schema")
        if not isinstance(msg, Message) or msg.DESCRIPTOR.full_name != op.request_schema:
            _refuse("schema")
        # 8. Scope: the body's hospital and patients are never trusted alone.
        if op.needs_hospital and c.hid == "":
            _refuse("no_hospital")
        hospitals, patients = scope_ids(msg)
        if any(h != c.hid for h in hospitals) or not patients <= set(c.pids):
            _refuse("scope")
        # 9. Permission, from the operations table only.
        if c.sub == SVC_CORE:
            if not op.svc_core or c.roles:
                _refuse("forbidden")
        elif not any(r in op.any_role for r in c.roles):
            _refuse("forbidden")
        if granted and not op.bff_direct:
            _refuse("forbidden")
        return Verified._issue(
            _ISSUE,
            sub=c.sub,
            hid=c.hid,
            roles=c.roles,
            rid=c.rid,
            jti=c.jti,
            kid=c.kid,
            kind=c.knd,
            subject=c.nsub,
            operation=op,
            patient_ids=c.pids,
            exp=c.exp,
            via=c.via,
            message=msg,
        )

    def _run(
        self,
        token: str | None,
        subject: str,
        reply: str,
        kind: str,
        headers: Mapping[str, str] | None,
        body: bytes,
        decoder: Decoder,
        granted: bool,
    ) -> Verified | Refusal:
        hdrs: Mapping[str, str] = headers if headers is not None else {}
        body = bytes(body)
        tok = token if isinstance(token, str) else ""
        signed: list[_Claims] = []
        passed_reply = False
        try:
            claims = self._token(tok, subject, reply, kind, body, granted, signed)
            passed_reply = kind == "req"
            return self._rest(claims, kind, hdrs, body, decoder, granted)
        except _Refused as r:
            reason = r.reason
        except Exception as e:
            # A bug, not a verdict: no work, and only the exception's type is
            # logged (its args may hold claim or body values).
            refusal = self._refusal("internal", kind, reply=passed_reply)
            log.error(
                "boundary verify failed service=%s subject=%s error=%s reply=%s",
                self.service,
                _safe(subject),
                type(e).__name__,
                refusal.reply,
            )
            return refusal
        # Before check 5 passed the reply subject is unchecked: answer only to
        # the one inbox this path can legitimately have (never a job).
        if passed_reply:
            can_reply = True
        elif kind == "req" and reason != "reply_subject":
            prefix = REPLY_GRANT if granted else REPLY_CORE
            can_reply = len(reply) > len(prefix) and reply.startswith(prefix)
        else:
            can_reply = False
        refusal = self._refusal(reason, kind, reply=can_reply)
        self._log(refusal, subject, signed[0] if signed else None)
        return refusal

    def _refusal(self, reason: str, kind: str, *, reply: bool) -> Refusal:
        with self._count_lock:
            self.refusals[(self.service, reason)] += 1
        if kind == "job" and reason == "expired":
            # F6: signed by core, and simply waited too long in the queue.
            return Refusal(reason, "deadline_exceeded", reply=False, alert=False)
        return Refusal(reason, REASON_CODES[reason], reply=reply)

    def _log(self, r: Refusal, subject: str, c: _Claims | None) -> None:
        # Ids only from a token whose signature passed; otherwise none.
        ids = (
            f" rid={_safe(c.rid)} jti={_safe(c.jti)} hid={_safe(c.hid) if c.hid else '-'} kid={_safe(c.kid)}"
            if c is not None
            else ""
        )
        level = logging.WARNING if r.alert else logging.INFO
        log.log(
            level,
            "boundary refused service=%s subject=%s reason=%s code=%s reply=%s%s",
            self.service,
            _safe(subject),
            r.reason,
            r.code,
            r.reply,
            ids,
        )

    # --- entry points -----------------------------------------------------------

    def verify_request(
        self,
        subject: str,
        reply: str,
        headers: Mapping[str, str] | None,
        body: bytes,
        decoder: Decoder,
    ) -> Verified | Refusal:
        """A request/reply message carrying core's token in Mhai-Request-Token."""
        token = (headers or {}).get(envelope.TOKEN)
        return self._run(token, subject, reply, "req", headers, body, decoder, granted=False)

    def verify_job(
        self, subject: str, headers: Mapping[str, str] | None, body: bytes, decoder: Decoder
    ) -> Verified | Refusal:
        """A JetStream job. No reply; replay is the database claim's job."""
        token = (headers or {}).get(envelope.TOKEN)
        return self._run(token, subject, "", "job", headers, body, decoder, granted=False)

    def verify_granted(
        self,
        token: str,
        subject: str,
        reply: str,
        headers: Mapping[str, str] | None,
        body: bytes,
        decoder: Decoder,
    ) -> Verified | Refusal:
        """A BFF-direct call (spec §17.2): the checks on the token core
        returned for this call (via grant, rpl _INBOX_bff.clinical.), never on
        anything the message carried."""
        return self._run(token, subject, reply, "req", headers, body, decoder, granted=True)

"""The grant client: a worker asks core for a request token for one
BFF-direct call (spec §17.2, §17.3).

The worker holds the user's access token only for this call. It sends it
to authz.v1.grant.<its own service> and nowhere else, never logs it, and
never runs checks 2-9 on it: core verifies it and returns a request token
bound to the call, which the worker then verifies (verify_granted).

A timeout, no responders, a disconnect or a malformed reply is Unavailable:
no work. A reply that comes after the budget is dropped (nats-py has already
let go of the request, and the clock is checked again here).
"""

from __future__ import annotations

import hashlib
import logging
import re
import time
from collections.abc import Callable, Sequence
from dataclasses import dataclass

import nats.errors
from nats.aio.client import Client

from intelligence.boundary import _b64, envelope, telemetry
from intelligence.boundary.pb import authz_pb2
from intelligence.boundary.verify import MAX_TOKEN_LEN, REPLY_GRANT, strict_decoder

log = logging.getLogger("intelligence.boundary.grant")

REQUEST_SCHEMA = authz_pb2.GrantRequest.DESCRIPTOR.full_name
RESPONSE_SCHEMA = authz_pb2.GrantResponse.DESCRIPTOR.full_name
REFUSED_SCHEMA = authz_pb2.GrantRefused.DESCRIPTOR.full_name
MAX_BUDGET = 0.25
# Core's refusal codes are its problem codes: lower case and underscores.
_CODE = re.compile(r"[a-z][a-z_]{0,63}")

_decode_response = strict_decoder(authz_pb2.GrantResponse)
_decode_refused = strict_decoder(authz_pb2.GrantRefused)


@dataclass(frozen=True)
class GrantRefused:
    """Core refused the call; the BFF gets grant_refused:<code>."""

    code: str


@dataclass(frozen=True)
class Unavailable:
    """No grant in time: the BFF gets unavailable. reason is for logs."""

    reason: str


def budget(remaining: float) -> float:
    """The grant budget: the smaller of 250 ms and a quarter of what is left."""
    return min(MAX_BUDGET, remaining / 4)


class GrantClient:
    def __init__(self, nc: Client, service: str, clock: Callable[[], float] = time.time) -> None:
        self._nc = nc
        self.service = service
        self.subject = f"authz.v1.grant.{service}"
        self._clock = clock

    async def ask(
        self,
        access_token: str,
        subject: str,
        reply: str,
        body: bytes,
        patient_ids: Sequence[str],
        request_id: str,
        deadline: float,
    ) -> str | GrantRefused | Unavailable:
        """A request token for this exact call, core's refusal, or Unavailable.

        deadline is the call's end (the clock's time). Raises ValueError for a
        reply subject outside the clinical BFF's inbox: the worker never asks
        a grant for one."""
        if not reply.startswith(REPLY_GRANT) or len(reply) <= len(REPLY_GRANT):
            raise ValueError("not a clinical BFF reply subject")
        seg = subject.split(".")
        if len(seg) != 4 or seg[:2] != ["inference", "v1"] or seg[2] != self.service:
            raise ValueError("not this service's subject")
        start = self._clock()
        remaining = deadline - start
        if remaining <= 0:
            return self._unavailable("deadline")
        limit = budget(remaining)
        req = authz_pb2.GrantRequest(
            access_token=access_token,
            operation=f"{seg[2]}.{seg[3]}",
            subject=subject,
            body_hash=_b64.encode(hashlib.sha256(body).digest()),
            patient_ids=list(patient_ids),
            reply_prefix=REPLY_GRANT,
            request_id=request_id,
            deadline_ms=max(1, int(remaining * 1000)),
        )
        data = req.SerializeToString(deterministic=True)
        del req
        headers = {envelope.SCHEMA: REQUEST_SCHEMA}
        telemetry.inject(headers)  # traceparent only (never tracestate); core's span follows ours
        try:
            msg = await self._nc.request(self.subject, data, timeout=limit, headers=headers)
        except nats.errors.NoRespondersError:
            return self._unavailable("no_responders")
        except (nats.errors.TimeoutError, TimeoutError):
            return self._unavailable("timeout")
        except (nats.errors.Error, OSError):
            return self._unavailable("disconnected")
        except Exception as e:  # fail closed; the type only (args could hold data)
            log.error("grant request failed service=%s error=%s", self.service, type(e).__name__)
            return self._unavailable("error")
        finally:
            del data
        if self._clock() - start > limit:
            return self._unavailable("late")
        return self._parse(msg.headers or {}, msg.data)

    def _parse(self, headers: dict[str, str], body: bytes) -> str | GrantRefused | Unavailable:
        status, schema = headers.get(envelope.STATUS), headers.get(envelope.SCHEMA)
        try:
            if status == envelope.OK and schema == RESPONSE_SCHEMA:
                tok = getattr(_decode_response(body), "request_token", "")
                if isinstance(tok, str) and 0 < len(tok) <= MAX_TOKEN_LEN:
                    return tok
            elif (
                schema == REFUSED_SCHEMA
                and isinstance(status, str)
                and _CODE.fullmatch(status)
                and status != envelope.OK
            ):
                code = getattr(_decode_refused(body), "code", "")
                if code == status:
                    log.info("grant refused service=%s code=%s", self.service, code)
                    return GrantRefused(code)
        except Exception:
            pass
        return self._unavailable("malformed")

    def _unavailable(self, reason: str) -> Unavailable:
        log.warning("grant unavailable service=%s reason=%s", self.service, reason)
        return Unavailable(reason)

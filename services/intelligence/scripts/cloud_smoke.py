"""The manual live smoke for one cloud provider (plan Task 15; H1, H7).

A person runs this once on their own machine, outside any container, never
on the shared build server (steven-reid, mt5-lab) and never in CI, with a
dedicated, short-lived, spend-capped smoke key that is revoked straight
after:

    MHAI_CLOUD_SMOKE=1 APP_ENV=demo uv run python scripts/cloud_smoke.py \\
        --provider anthropic --model claude-opus-5-5 --key-stdin \\
        [--record tests/fixtures/cloud/anthropic/recorded/]

The key is pasted at the prompt (getpass) or piped as one line on standard
input. There is no option to read it from a file or the environment, and
none that loads code or a transport.

It runs the real CloudModelClient, adapter, gate and scan, with the stub
retriever over three fixed synthetic guideline chunks, a budget capped at
three attempts, a direct TLS connection (verified, no proxy, no redirects),
and one fixed synthetic question and patient. It prints only the provider,
the outcome, the versions, token counts (and the reply's numeric usage
fields), the xAI retention header and whether the answer validated: never
the question, the chunks, the answer or the key. --record writes the reply,
scrubbed (scripts/scrub_cloud_fixture.py), for the adapter tests.

Exit codes: 0 the outcome was ok; 1 another outcome; 2 refused.
"""

from __future__ import annotations

import argparse
import getpass
import importlib.util
import json
import logging
import os
import sys
import threading
import time
from collections.abc import Mapping
from pathlib import Path
from typing import IO, Any

HERE = Path(__file__).resolve().parent
SMOKE_VAR = "MHAI_CLOUD_SMOKE"
CI_VARS = ("CI", "GITHUB_ACTIONS")
CONTAINER_MARKERS = (Path("/.dockerenv"), Path("/run/.containerenv"))
MAX_ATTEMPTS = 3

# Fixed synthetic inputs. Recognisably synthetic: SYN- identifiers, no
# name. The chunk ids are the adapter tests' (KB#0001-KB#0003), so a
# recorded reply cites ids those tests send.
QUESTION = (
    "Synthetic smoke question: what is the first-line fluid plan for a synthetic adult with fever?"
)
PATIENT = {
    "patient_id": "SYN-0001",
    "mrn": "SYN-482913",
    "disease_category": "fever",
    "age": 34,
    "risk_score": 0.4,
}
CHUNKS = (
    (
        "KB#0001",
        "Synthetic guideline text: in adult fever, give oral fluids and review in 24 hours.",
    ),
    (
        "KB#0002",
        "Synthetic guideline text: check temperature, pulse and blood pressure at each review.",
    ),
    (
        "KB#0003",
        "Synthetic guideline text: refer at once if the patient cannot drink or is confused.",
    ),
)

log = logging.getLogger("cloud_smoke")


class Refused(Exception):
    """Exit 2, naming the reason (never a value)."""


def build_parser() -> argparse.ArgumentParser:
    from intelligence.boundary.cloud import vocab

    p = argparse.ArgumentParser(
        prog="cloud_smoke.py",
        description="Manual live smoke for one cloud provider. Synthetic data only; "
        "a person's machine only; the key from standard input only.",
        allow_abbrev=False,
    )
    p.add_argument("--provider", required=True, choices=vocab.PROVIDERS)
    p.add_argument("--model", required=True, help="the exact model id")
    p.add_argument(
        "--key-stdin",
        action="store_true",
        help="read the key from standard input (required: there is no other way)",
    )
    p.add_argument(
        "--record",
        metavar="DIR",
        help="write the scrubbed reply to DIR (tests/fixtures/cloud/<provider>/recorded/)",
    )
    return p


def _refusal(env: Mapping[str, str], markers: tuple[Path, ...]) -> str | None:
    from intelligence.boundary.cloud.settings import SDK_ENV_PREFIXES, TLS_ENV_REFUSED

    if env.get(SMOKE_VAR) != "1":
        return f"{SMOKE_VAR}=1 is required (a person's deliberate live run)"
    for name in CI_VARS:
        if name in env:
            return f"{name} is set: never run the live smoke in CI"
    if "container" in env:
        return "the container variable is set: run it on your own machine, outside any container"
    for m in markers:
        if m.exists():
            return f"{m} exists: run it on your own machine, outside any container"
    for name in sorted(env):
        if name.upper().startswith(SDK_ENV_PREFIXES) or name in TLS_ENV_REFUSED:
            return f"{name} is set: the SDKs' own variables and a custom CA are refused"
    return None


def _read_key(stdin: IO[str]) -> str:
    if stdin.isatty():
        return getpass.getpass("Smoke key (input hidden): ", stream=sys.stderr)
    line = stdin.readline()
    return line[:-1] if line.endswith("\n") else line


class _Budget:
    """The smoke's budget: at most MAX_ATTEMPTS reservations, nothing kept."""

    def __init__(self) -> None:
        self.n = 0
        self.lock = threading.Lock()

    def reserve(self, provider: str, tokens: int) -> Any:
        from intelligence.boundary.cloud.budget import Reservation

        with self.lock:
            if self.n >= MAX_ATTEMPTS:
                return None
            self.n += 1
            return Reservation(f"smoke-{self.n}", tokens, 0)

    def settle(self, reservation: Any, used: int | None) -> None:
        return None


def _recording(inner: Any) -> Any:
    """A transport that keeps the last reply (status, headers, body bytes)."""
    import httpx2

    class Recording(httpx2.BaseTransport):
        def __init__(self) -> None:
            self.last: tuple[int, dict[str, str], bytes] | None = None

        def handle_request(self, request: httpx2.Request) -> httpx2.Response:
            resp = inner.handle_request(request)
            try:
                content = resp.read()
            finally:
                resp.close()
            headers = {
                k.lower(): v
                for k, v in resp.headers.items()
                if k.lower() not in ("content-encoding", "content-length", "transfer-encoding")
            }
            self.last = (resp.status_code, headers, content)
            return httpx2.Response(
                resp.status_code, headers=headers, content=content, request=request
            )

        def close(self) -> None:
            inner.close()

    return Recording()


def _usage(body: bytes) -> dict[str, int]:
    """The reply's numeric usage fields, flattened (no text)."""
    try:
        usage = json.loads(body).get("usage")
    except (ValueError, AttributeError):
        return {}
    out: dict[str, int] = {}

    def walk(node: Any, prefix: str) -> None:
        if isinstance(node, dict):
            for k, v in node.items():
                walk(v, f"{prefix}.{k}")
        elif type(node) is int:
            out[prefix] = node

    walk(usage, "usage")
    return out


def _load_scrubber() -> Any:
    spec = importlib.util.spec_from_file_location(
        "_scrub_cloud_fixture", HERE / "scrub_cloud_fixture.py"
    )
    assert spec is not None and spec.loader is not None
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def _record_path(record: str, provider: str, model: str) -> Path:
    d = Path(record)
    name = "".join(c if c.isalnum() or c in "._-" else "_" for c in model)
    return d / f"{{outcome}}-{name}.json"


def _run(args: argparse.Namespace, key: Any, transport: Any, out: IO[str]) -> int:
    from intelligence.boundary import kb_repo
    from intelligence.boundary.cloud.budget import MAX_OUTPUT_TOKENS
    from intelligence.boundary.cloud.client import ADAPTERS, CloudCall, CloudModelClient
    from intelligence.boundary.cloud.scan import collect_identifiers
    from intelligence.boundary.cloud.settings import ProviderSettings
    from intelligence.boundary.cloud.vocab import BASE_URLS
    from intelligence.boundary.models import stub_parts

    provider: str = args.provider
    settings = ProviderSettings(provider, args.model, key, None, BASE_URLS[provider])
    recording = _recording(transport)
    build, local_versions = stub_parts()
    client = CloudModelClient(
        settings, ADAPTERS[provider](args.model), build, local_versions, recording
    )
    try:
        chunks = [
            kb_repo._chunk((cid, True, text, "guideline", None, None, {})) for cid, text in CHUNKS
        ]
        call = CloudCall(
            collect_identifiers(QUESTION, PATIENT), _Budget(), time.monotonic() + 120.0
        )
        context = {
            "disease_category": client.mask(str(PATIENT["disease_category"])),
            "risk_score": PATIENT["risk_score"],
        }
        g = client.generate(client.mask(QUESTION), context, None, chunks, call=call)
    finally:
        client.close()
    outcome = g.cloud_outcome or "unavailable"
    lines = [
        f"provider={provider}",
        f"outcome={outcome}",
        f"cca={g.versions.get('cca', '')}",
        f"tokens_in={g.tokens_in}",
        f"tokens_out={g.tokens_out}",
    ]
    last = recording.last
    usage = _usage(last[2]) if last is not None else {}
    lines += [f"{k}={v}" for k, v in sorted(usage.items())]
    if provider == "xai":
        header = last[1].get("x-zero-data-retention") if last is not None else None
        shown = "missing" if header is None else header.strip().lower()
        lines.append(f"retention_header={shown if shown in ('true', 'false') else 'other'}")
        prompt, total = usage.get("usage.prompt_tokens"), usage.get("usage.total_tokens")
        if prompt is not None and total is not None:
            lines.append(f"over_cap={'yes' if total - prompt > MAX_OUTPUT_TOKENS else 'no'}")
    lines.append(f"answer_validated={'yes' if outcome == 'ok' and g.available else 'no'}")
    print("\n".join(lines), file=out)

    if args.record and last is not None:
        status, headers, content = last
        try:
            body: Any = json.loads(content)
        except ValueError:
            body = content.decode("utf-8", "replace")
        scrubber = _load_scrubber()
        fx = scrubber.scrub({"status": status, "headers": headers, "body": body})
        problems = scrubber.check(fx)
        if problems:
            print(
                f"not recorded: the reply failed the scrubber check ({'; '.join(problems)})",
                file=sys.stderr,
            )
            return 1
        path = Path(str(_record_path(args.record, provider, args.model)).format(outcome=outcome))
        path.write_text(json.dumps(fx, indent=2, ensure_ascii=False) + "\n", "utf-8")
        print(f"recorded={path.name}", file=out)
    return 0 if outcome == "ok" else 1


def main(
    argv: list[str] | None = None,
    *,
    stdin: IO[str] | None = None,
    transport: Any = None,
    env: Mapping[str, str] | None = None,
    markers: tuple[Path, ...] = CONTAINER_MARKERS,
) -> int:
    """The command line. stdin, transport, env and markers are for the
    tests (tests/boundary/cloud/test_smoke_guard.py); the command line has
    no way to set them."""
    args = build_parser().parse_args(argv)
    env = os.environ if env is None else env
    stdin = sys.stdin if stdin is None else stdin
    try:
        reason = _refusal(env, markers)
        if reason is not None:
            raise Refused(reason)
        return _checked(args, stdin, transport)
    except Refused as e:
        print(f"cloud_smoke: refused: {e}", file=sys.stderr)
        return 2


def _checked(args: argparse.Namespace, stdin: IO[str], transport: Any) -> int:
    from intelligence.boundary.cloud.apikey import (
        ApiKey,
        install_redaction,
        key_problem,
        quiet_sdk_loggers,
        uninstall_redaction,
    )
    from intelligence.boundary.cloud.settings import NO_CA_BUNDLE, model_problem, openssl_ca_paths
    from intelligence.boundary.cloud.vocab import OUTCOMES

    if not args.key_stdin:
        raise Refused("--key-stdin is required: the key is read from standard input only")
    problem = model_problem(args.provider, args.model)
    if problem is not None:
        raise Refused(f"--model {problem}")
    if transport is None and openssl_ca_paths() == (None, None):
        raise Refused(NO_CA_BUNDLE)
    if args.record:
        d = Path(args.record)
        if d.exists() and not d.is_dir():
            raise Refused("--record must name a directory")
        pattern = str(_record_path(args.record, args.provider, args.model))
        existing = [o for o in OUTCOMES if Path(pattern.format(outcome=o)).exists()]
        if existing:
            raise Refused(f"a recording for this model already exists in {d}: remove it first")

    raw = _read_key(stdin)
    problem = key_problem(raw)
    if problem is not None:
        raise Refused(f"the key {problem}")
    key = ApiKey(raw)
    del raw
    install_redaction([key])
    quiet_sdk_loggers()
    try:
        if transport is None:
            import httpx2

            from intelligence.boundary.cloud.client import tls_context

            # Direct, on a person's machine: TLS verified, the environment
            # (proxies, CA variables) ignored, no retries of its own.
            transport = httpx2.HTTPTransport(verify=tls_context(), trust_env=False, retries=0)
        if args.record:
            Path(args.record).mkdir(parents=True, exist_ok=True)
        return _run(args, key, transport, sys.stdout)
    finally:
        uninstall_redaction()
        del key


if __name__ == "__main__":
    sys.exit(main())

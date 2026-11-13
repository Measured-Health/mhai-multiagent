"""The on-host evaluation before a hospital is switched to the own answer model (own answer
model plan, PR 4, Task 12): ``mhai-models eval-served``, run by ``deploy/pilot/eval-local.sh``.

The promotion gate (:mod:`gate`) scored the GGUF on a CI runner. This scores what the pilot
host actually serves, the same way the worker answers a question:

1. the expected alias is the pinned ``cca`` of ``models.lock``
   (``<name>@<version>#<sha12>``, :func:`verify.version_string`), never what the sidecar
   says it is;
2. the generator is the worker's own (:func:`boundary.models.local_generator`: the
   ``PinnedLlamaGenerator`` over ``INTELLIGENCE_LOCAL_LLM_URL``, settings read as the worker
   reads them), and the client is the worker's ``OrchestratorModelClient`` over
   ``local_parts`` (its retriever, CCA prompt and NLI verification) with the generator's
   alias check before every item;
3. the sidecar must serve exactly that alias before any question (it may still be loading:
   up to ``INTELLIGENCE_LOCAL_LLM_START_TIMEOUT``); otherwise ``no_runtime``;
4. every item of the split is scored (:func:`score.score`) and timed; the verdict is the
   answer gate's committed thresholds (``thresholds/answer.json``) **and** the median
   seconds per answer at most ``max_seconds``.

The split is the locked ``clinical_answer`` test split when ``models/testsets.lock`` names
one (fetched and signature-checked by ``mhai-models testset-fetch`` as the store's reader,
then checked here against the lock's hash), else the dev split, rebuilt from the public
seed (:func:`bank.build`) and labelled ``dev`` in the summary.

The summary is aggregates only (metrics, counts, verdict, seconds): never a question, an
item id or an answer. The questions are synthetic, so the client does not mask them.
"""

from __future__ import annotations

import secrets
import statistics
import tempfile
import time
from collections.abc import Callable, Sequence
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from intelligence.doceval import report
from intelligence.doceval.answers import bank, schema, score
from intelligence.modelstore import lock, testsets, verify
from intelligence.modelstore.reasons import Refused

ROLE = "cca"
DEFAULT_MAX_SECONDS = 90
MAX_SECONDS_CAP = 3600
SPLITS = ("dev", "test")

Clock = Callable[[], float]
BuilderFor = Callable[[Any], Callable[[Sequence[Any]], Any]]


class _NoMasker:
    """Synthetic questions only: nothing to mask, and nothing leaves the host."""

    def mask(self, text: str) -> tuple[str, list[Any]]:
        return text, []


@dataclass(frozen=True)
class _Pinned:
    """What the worker's verified pin gives the local route: the ``cca`` version string."""

    alias: str

    def versions(self) -> dict[str, str]:
        return {ROLE: self.alias}

    def path(self, role: str) -> Path:
        raise KeyError(role)


def expected_alias(lock_path: Path | str) -> str:
    """The pinned ``cca``'s version string, from ``models.lock``; no ``cca`` pin is
    ``not_found``/``cca_pin``."""
    pinned = lock.read(lock_path)
    pin = pinned.models.get(ROLE)
    if pin is None:
        raise Refused("not_found", detail_code="cca_pin")
    return verify.version_string(pin.name, pin.version, pin.manifest_sha256)


def locked_entry(testsets_lock: Path | str) -> testsets.LockEntry | None:
    """The newest locked ``clinical_answer`` split named in ``models/testsets.lock``."""
    from intelligence.modelstore import names

    found = [e for (s, _), e in testsets.read_lock(testsets_lock).items() if s == schema.SET_NAME]
    if not found:
        return None
    return max(found, key=lambda e: names.Version.parse(e.version))


def load_dev(scratch: Path) -> schema.AnswerSet:
    """The dev split, rebuilt from the public seed into ``scratch``."""
    try:
        built = bank.build("dev", scratch, version="1.0.0")
        loaded = schema.load_set(built.root)
    except schema.ItemInvalid:
        raise Refused("evaluation_failed", detail_code="dev_split") from None
    if loaded.meta["purpose"] != "dev" or loaded.meta["locked"]:
        raise Refused("not_allowed_here", detail_code="split")
    return loaded


def load_locked(set_dir: Path | str, entry: testsets.LockEntry) -> schema.AnswerSet:
    """A fetched locked split, checked against its lock entry (as promotion does)."""
    failed = False
    loaded: schema.AnswerSet | None = None
    try:
        loaded = schema.load_set(set_dir)
    except schema.ItemInvalid:
        failed = True
    if (
        failed
        or loaded is None
        or loaded.meta["purpose"] != "test"
        or not loaded.meta["locked"]
        or not secrets.compare_digest(str(loaded.meta["sha256"]), entry.sha256)
    ):
        raise Refused("testset_mismatch", detail_code="locked_split")
    return loaded


def worker_client(
    alias: str, settings: Any, *, builder_for: BuilderFor | None = None
) -> tuple[Any, Any]:
    """The worker's local client for the pinned ``alias`` (module docstring, step 2), and
    its generator. ``builder_for`` replaces ``local_parts``' builder in tests only."""
    from intelligence.boundary.models import (
        OrchestratorModelClient,
        local_generator,
        local_parts,
    )

    pinned = _Pinned(alias)

    def source() -> _Pinned:
        return pinned

    gen = local_generator(source, (ROLE,), settings, development=False)
    if builder_for is None:
        build, versions = local_parts(source, (ROLE,), generator=gen)
    else:
        build, versions = builder_for(gen), {ROLE: gen.name}
    client = OrchestratorModelClient(
        build, masker=_NoMasker(), versions=versions, pinned=source, preflight=gen.check
    )
    return client, gen


def wait_for_alias(settings: Any, alias: str) -> None:
    """The sidecar serves exactly ``alias`` within the start timeout, else ``no_runtime``."""
    from intelligence.tools.llama_client import LlamaSidecar, LocalUnverified

    sidecar = LlamaSidecar(settings)
    detail = None
    try:
        sidecar.wait_for_alias(alias, settings.start_timeout_s)
    except LocalUnverified:
        # The served alias, if any, says which way it failed; never printed.
        try:
            sidecar.served_alias()
            detail = "alias_mismatch"
        except LocalUnverified as e:
            detail = e.detail
    finally:
        sidecar.close()
    if detail is not None:
        raise Refused("no_runtime", detail_code=detail)


class _Timed:
    """The client, with the seconds each item's generation and verification took."""

    def __init__(self, client: Any, clock: Clock) -> None:
        self._client = client
        self._clock = clock
        self._start = 0.0
        self.seconds: list[float] = []

    def generate(self, *args: Any, **kwargs: Any) -> Any:
        self._start = self._clock()
        try:
            return self._client.generate(*args, **kwargs)
        except BaseException:
            self.seconds.append(self._clock() - self._start)  # no verify follows
            raise

    def verify(self, generation: Any) -> Any:
        try:
            return self._client.verify(generation)
        finally:
            self.seconds.append(self._clock() - self._start)


def evaluate(
    client: Any,
    loaded: schema.AnswerSet,
    thresholds: report.Thresholds,
    *,
    split: str,
    alias: str,
    max_seconds: int,
    clock: Clock = time.monotonic,
) -> dict[str, Any]:
    """Score ``loaded`` through ``client`` and return the summary (aggregates only)."""
    if split not in SPLITS or loaded.meta["purpose"] != split:
        raise Refused("testset_mismatch", detail_code="split")
    timed = _Timed(client, clock)
    results = score.score(timed, loaded.items)
    m = score.metrics(results)
    gate = report.evaluate_rules(thresholds, {"overall": m.rates()}, {"overall": m.items})
    median = statistics.median(timed.seconds) if len(timed.seconds) == len(results) else None
    failed = list(gate["failed"])
    if median is None or median > max_seconds:
        failed.append("median_seconds")
    return {
        "verdict": "pass" if gate["verdict"] == "pass" and not failed else "fail",
        "failed": failed,
        "split": split,
        "alias": alias,
        "testset": {
            "name": loaded.meta["name"],
            "version": loaded.meta["version"],
            "sha256": loaded.meta["sha256"],
            "items": loaded.meta["items"],
            "must_escalate": loaded.meta["must_escalate"],
        },
        "metrics": {k: report.decimal(v) for k, v in m.rates().items()},
        "counts": {
            "items": m.items,
            "must_escalate": m.must_escalate,
            "escalated": m.escalated,
            "forbidden_hits": m.forbidden_hits,
            "generation_failures": m.generation_failures,
        },
        "median_seconds": None if median is None else round(median, 1),
        "max_seconds": max_seconds,
        "thresholds_sha256": thresholds.sha256,
        "sign_off": schema.SIGN_OFF,
    }


def check_max_seconds(text: str) -> int:
    if not text.isascii() or not text.isdigit() or not 1 <= int(text) <= MAX_SECONDS_CAP:
        raise Refused("invalid_name", detail_code="max_seconds")
    return int(text)


def run(
    *,
    lock_path: Path | str,
    split: str,
    set_dir: Path | str | None,
    testsets_lock: Path | str,
    max_seconds: int,
    settings: Any,
    builder_for: BuilderFor | None = None,
    clock: Clock = time.monotonic,
) -> dict[str, Any]:
    """The whole evaluation (module docstring). A locked split named in ``testsets_lock``
    must be the one scored: asking for ``dev`` then is ``not_allowed_here``."""
    alias = expected_alias(lock_path)
    entry = locked_entry(testsets_lock)
    if split == "dev" and entry is not None:
        raise Refused("not_allowed_here", detail_code="locked_split_exists")
    if split not in SPLITS or (split == "test") != (set_dir is not None):
        raise Refused("not_allowed_here", detail_code="split")
    thresholds = report.packaged_thresholds(report.ANSWER_GATE)
    wait_for_alias(settings, alias)
    with tempfile.TemporaryDirectory(prefix="mhai-eval-served-") as scratch:
        if split == "test":
            if entry is None:
                raise Refused("not_found", detail_code="testset_lock")
            loaded = load_locked(Path(str(set_dir)), entry)
        else:
            loaded = load_dev(Path(scratch) / "set")
        client, _ = worker_client(alias, settings, builder_for=builder_for)
        return evaluate(
            client,
            loaded,
            thresholds,
            split=split,
            alias=alias,
            max_seconds=max_seconds,
            clock=clock,
        )


__all__ = [
    "DEFAULT_MAX_SECONDS",
    "check_max_seconds",
    "evaluate",
    "expected_alias",
    "load_dev",
    "load_locked",
    "locked_entry",
    "run",
    "wait_for_alias",
    "worker_client",
]

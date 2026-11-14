"""Per-model answer profiles: how one trained answer model's replies are read.

A trained ``cca`` model has its own habits, learnt from its training data. A profile says
how the local route reads them, and it is chosen by the model's own training manifest (the
recipe: role, upstream base and revision, and the exact set of datasets), never by a name
the model or a request gives. Anything not in :data:`_PROFILES` is :data:`DEFAULT`, which
changes nothing.

:data:`GEMMA4_E4B_CCA_V1` (Gemma 4 E4B with QLoRA, trained 14 Nov 2026 on
``cca-v1-rag-cited``, ``cca-v1-abstain`` and ``cca-v1-patient-cited``):

* **It declines with ``INSUFFICIENT EVIDENCE:``.** Its ``cca-v1-abstain`` examples taught it
  that marker; our verifier had no way to read it, so a decline was scored as an uncited
  claim and came out ``REGENERATE``. Here a decline is ``ESCALATE``, as a cloud reply's
  ``insufficient_evidence`` is. The marker is found anywhere in the reply, in any case,
  width or spacing (NFKC, casefold): a decline mixed into an answer still escalates.
* **Its citations are inline ``[KB#nnnn]`` markers**, as our prompt asks. The NLI model
  reads a marker left in the claim as new, unsupported content: a claim copied word for
  word from its chunk scores neutral (0.999) with the marker and entailed (0.99) without.
  Here the exact markers are removed from the NLI hypothesis only; the premise, the claim's
  words and which chunk it cites are unchanged.
* **Fail closed:** a reply with no exact citation marker (and no decline) is not this
  model's answer format and escalates, before NLI.
"""

from __future__ import annotations

import logging
import re
import unicodedata
from collections.abc import Callable, Mapping
from dataclasses import dataclass
from pathlib import Path
from types import MappingProxyType
from typing import Any

log = logging.getLogger(__name__)

# The same exact marker the CCA and the citation pipeline read (kb_builder.CITATION_MARKER).
_MARKER = re.compile(r"\s*\[KB#(?:\d{4}|[1-9]\d{6})\]")


@dataclass(frozen=True)
class AnswerProfile:
    id: str
    # Casefolded words of the model's decline marker; None: the model has none.
    decline_words: tuple[str, ...] | None = None
    nli_strip_citations: bool = False
    require_citation: bool = False


DEFAULT = AnswerProfile("default")
GEMMA4_E4B_CCA_V1 = AnswerProfile(
    "gemma4-e4b-cca-v1",
    decline_words=("insufficient", "evidence"),
    nli_strip_citations=True,
    require_citation=True,
)


@dataclass(frozen=True)
class _Recipe:
    role: str
    upstream: str
    revision: str
    datasets: frozenset[str]


_PROFILES: Mapping[_Recipe, AnswerProfile] = MappingProxyType(
    {
        _Recipe(
            "cca",
            "google/gemma-4-E4B-it",
            "4ecc2ba63bcac23ba4a767fc87025b1450f84c9c",
            frozenset(
                {
                    "medqa-usmle-4-options@1.0.0",
                    "medmcqa@1.0.0",
                    "pubmedqa-pqa-labeled@1.0.0",
                    "symptom-to-diagnosis@1.0.0",
                    "cca-v1-rag-cited@1.0.0",
                    "cca-v1-abstain@1.0.0",
                    "cca-v1-patient-cited@1.0.0",
                }
            ),
        ): GEMMA4_E4B_CCA_V1,
    }
)


# ---------------------------------------------------------------- selection


def for_manifest(tm: Any) -> AnswerProfile:
    """The profile of a parsed training manifest (``modelstore.training.TrainingManifest``)."""
    from intelligence.modelstore import training

    base = getattr(tm, "base", None)
    if not isinstance(base, training.UpstreamBase):
        return DEFAULT
    key = _Recipe(tm.role, base.upstream, base.revision, frozenset(tm.datasets))
    return _PROFILES.get(key, DEFAULT)


def for_run(run_dir: Path | str) -> AnswerProfile:
    """The profile of the run (or verified model folder) at ``run_dir``; a missing or
    invalid training manifest is :data:`DEFAULT`."""
    from intelligence.modelstore import training

    try:
        tm = training.load_run(run_dir)
    except Exception as e:  # noqa: BLE001 - any failure is the default reading
        log.warning("answer_profile.default reason=%s", type(e).__name__)
        return DEFAULT
    profile = for_manifest(tm)
    log.info("answer_profile.selected profile=%s", profile.id)
    return profile


def for_pinned(pinned: Callable[[], Any]) -> AnswerProfile:
    """The profile of the pinned ``cca`` (``PinnedModels.path("cca")``); no pin, no
    folder or any error is :data:`DEFAULT`."""
    try:
        p = pinned()
        path = None if p is None else p.path("cca")
    except Exception:  # noqa: BLE001 - KeyError (not pinned) or a failing source
        path = None
    return DEFAULT if path is None else for_run(path)


# ---------------------------------------------------------------- reading a reply


def strip_citations(text: str) -> str:
    """``text`` without its exact ``[KB#nnnn]`` markers (and the space before each)."""
    return _MARKER.sub("", text)


def _words(text: str) -> list[str]:
    return re.findall(r"[^\W_]+", unicodedata.normalize("NFKC", text).casefold())


def declined(profile: AnswerProfile, text: str) -> bool:
    """Whether the reply carries the profile's decline marker anywhere."""
    want = profile.decline_words
    if not want:
        return False
    words = _words(text)
    n = len(want)
    return any(tuple(words[i : i + n]) == want for i in range(len(words) - n + 1))


class CitationFreeNLI:
    """An NLI verifier that judges the claim without its citation markers. Only
    ``verify`` is offered: it is all the citation pipeline calls."""

    def __init__(self, inner: Any) -> None:
        self._inner = inner

    def verify(self, premise: str, hypothesis: str) -> Any:
        return self._inner.verify(premise=premise, hypothesis=strip_citations(hypothesis))


def _escalate(flag_type: str, severity: str) -> dict[str, Any]:
    return {
        "decision": "escalate",
        "trust_score": 0.0,
        "flags": [{"type": flag_type, "severity": severity}],
    }


class ProfiledHGA:
    """The HGA, after the profile's own checks: a decline or a reply with no citation
    escalates; anything else is verified by the HGA unchanged."""

    def __init__(self, inner: Any, profile: AnswerProfile) -> None:
        self._inner = inner
        self._profile = profile

    def verify(self, response_text: str, **kwargs: Any) -> dict[str, Any]:
        text = response_text if isinstance(response_text, str) else ""
        if declined(self._profile, text):
            return _escalate("model_declined", "info")
        if self._profile.require_citation and not _MARKER.search(text):
            return _escalate("no_citation", "critical")
        result: dict[str, Any] = self._inner.verify(response_text=response_text, **kwargs)
        return result


def wrap_nli(verifier: Any, profile: AnswerProfile) -> Any:
    return CitationFreeNLI(verifier) if profile.nli_strip_citations else verifier


def wrap_hga(hga: Any, profile: AnswerProfile) -> Any:
    if profile.decline_words or profile.require_citation:
        return ProfiledHGA(hga, profile)
    return hga


__all__ = [
    "DEFAULT",
    "GEMMA4_E4B_CCA_V1",
    "AnswerProfile",
    "CitationFreeNLI",
    "ProfiledHGA",
    "declined",
    "for_manifest",
    "for_pinned",
    "for_run",
    "strip_citations",
    "wrap_hga",
    "wrap_nli",
]

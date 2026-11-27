"""
===========================================================================
MHAI Milestone M3 — Part 2 of 3
===========================================================================
NLI Entailment Verifier + Citation Verification Pipeline
+ Semantic Entropy for RAG + NLI-Backed HGA

Implements:
  - NLIEntailmentVerifier: DeBERTa-v3-large fine-tuned on MultiNLI/MedNLI
    for pairwise (premise, hypothesis) → {entailment, neutral, contradiction}
  - CitationVerificationPipeline: extracts [KB#XXXX] from CCA output,
    looks up each cited KB chunk, runs NLI verification, produces
    PASS/FLAG/REDACT decisions per claim
  - SemanticEntropyVerifier: samples K responses from CCA, clusters
    by bidirectional entailment, computes H_SE — flags confabulation
    when K responses are semantically inconsistent
  - NLIBackedHGA: integrates real NLI verification into the trust score
    alongside the upgraded numerical/coherence/fabrication checkers
    from the M2 HGA

Prerequisites (from M1+M2):
  - MedicalKnowledgeBase with indexed chunks (Part 1)
  - RAGClinicalContextAgent (Part 1)
  - UpgradedHallucinationGuardAgent (from M2 code)

Requirements:
  pip install torch transformers sentence-transformers scipy numpy
  pip install scikit-learn
===========================================================================
"""

import os
import re
import threading
import time
import numpy as np
from typing import AbstractSet, List, Tuple, Dict, Optional, Any
from dataclasses import dataclass, field
from enum import Enum
from collections import defaultdict
import logging

from intelligence.knowledge_base.kb_builder import (
    CITATION_MARKER,
    chunk_visible_to,
    strip_citation_tags,
)
from intelligence.utils.nli import DEFAULT_NLI_MODEL, nli_label_map
from intelligence.agents.prompt_formats import STEPS_V1_EXAMPLE_SENTENCES

logging.basicConfig(level=logging.INFO)
logger = logging.getLogger("MHAI.M3.NLI")


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# 2.1  NLI DATA STRUCTURES
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

class NLILabel(Enum):
    ENTAILMENT = "entailment"
    NEUTRAL = "neutral"
    CONTRADICTION = "contradiction"


class VerificationAction(Enum):
    PASS = "pass"             # Claim entailed by source
    FLAG = "flag"             # Claim neutral (unsupported but not contradicted)
    REDACT = "redact"         # Claim contradicts source


@dataclass
class NLIResult:
    """Result of a single NLI inference."""
    premise: str
    hypothesis: str
    label: NLILabel
    confidence: float          # Softmax probability of predicted label
    logits: Dict[str, float] = field(default_factory=dict)


@dataclass
class CitationVerificationResult:
    """Result of verifying a single cited claim."""
    claim_text: str
    citation_id: str           # e.g., "KB#0023"
    source_text: str           # The KB chunk content
    nli_result: NLIResult
    action: VerificationAction
    reason: str
    # A closed reason code (CITATION_REASONS), stored with the decision; never text.
    code: str = ""
    # Which claim of the response this citation belongs to (0-based).
    claim_index: int = -1


# The reason code of each per-citation result (CitationVerificationResult.code): what
# the decision row stores and core shows. Closed set: core checks it too.
CITATION_REASONS = (
    "entailed",              # PASS: NLI entailment at or above the threshold
    "not_entailed",          # FLAG: NLI neutral, or entailment below the threshold
    "contradicted",          # REDACT: NLI contradiction at or above the threshold
    "number_not_in_source",  # FLAG: entailed, but a number of the claim isn't in its sources
    "not_retrieved",         # REDACT: the chunk wasn't retrieved for this question (no NLI)
    "not_found",             # FLAG: no such chunk
    "other_patient",         # REDACT: another patient's record (no NLI)
    "uncited",               # FLAG: a claim with no citation
)

# A number in a claim: digits, optionally grouped by thousands or with a decimal part.
# Not when it is part of a code or a name (B50.9, ICD-10, HbA1c, SpO2, T2DM, COVID-19):
# never right after a letter, a digit run's own '.', or a letter and a hyphen.
_NUMBER = re.compile(r"(?<![A-Za-z0-9.,#])(?<![A-Za-z]-)\d[\d,]*(?:\.\d+)?")
_THOUSANDS = re.compile(r"\d{1,3}(?:,\d{3})+(?:\.\d+)?")
# A list marker at the start of a claim ("1. ", "2) "): numbering, not a claim's number.
_LIST_MARKER = re.compile(r"^\s*\d{1,2}[.)]\s+")
# The answer format's scaffolding at the start of a claim: "ANSWER:" and a step label
# ("STEP 1 - ASSESS:", "STEP 2 – DOSE:"). Not content: the step number is not a
# number the claim states, and the NLI model reads neither.
_ANSWER_SCAFFOLD = re.compile(
    r"^\s*(?:ANSWER\s*:\s*)?(?:STEP\s*\d{1,2}\s*[-\u2013:]\s*[A-Za-z][A-Za-z /]{0,20}?\s*:\s*)?"
)


def strip_answer_scaffold(text: str) -> str:
    """A claim without the answer format's leading "ANSWER:" and step label."""
    return _ANSWER_SCAFFOLD.sub("", text, count=1)


def _norm_sentence(text: str) -> str:
    return " ".join(strip_answer_scaffold(strip_citation_tags(text)).split()).rstrip(" .").lower()


_EXAMPLE_SENTENCES = frozenset(_norm_sentence(t) for t in STEPS_V1_EXAMPLE_SENTENCES)


def copies_format_example(claim_text: str) -> bool:
    """Whether a claim is a sentence of the answer format's own example."""
    return _norm_sentence(claim_text) in _EXAMPLE_SENTENCES


# ── Sentence windows of a cited passage (premise = KB chunk) ──
# The NLI model is trained on sentence pairs (MNLI-like): against a long, many-fact
# chunk it calls even a sentence copied word for word from that chunk neutral. So a
# claim is scored against windows of its chunk (SummaC-style): the whole chunk, each
# sentence and each pair of adjacent sentences; the claim's entailment is the highest
# over the windows, and a contradiction (the whole chunk's, or the claim's own
# sentence's) vetoes support (CitationVerificationPipeline._nli_over_passage).
NLI_MAX_WINDOWS = 24
# A fragment shorter than this ("1.", "e.g.") is joined to the sentence after it.
_MIN_SENTENCE_CHARS = 12
_PREMISE_SPLIT = re.compile(r"(?<=[.!?])\s+|\n+")


def premise_sentences(text: str) -> List[str]:
    """The sentences of a chunk, a too-short fragment joined to the next one."""
    out: List[str] = []
    carry = ""
    for part in _PREMISE_SPLIT.split(text or ""):
        part = part.strip()
        if not part:
            continue
        part = f"{carry} {part}".strip() if carry else part
        if len(part) < _MIN_SENTENCE_CHARS:
            carry = part
            continue
        carry = ""
        out.append(part)
    if carry:
        if out:
            out[-1] = f"{out[-1]} {carry}"
        else:
            out.append(carry)
    return out


def premise_windows(text: str, max_windows: int = NLI_MAX_WINDOWS) -> List[str]:
    """The windows of ``text`` a claim is scored against, at most ``max_windows``
    (at least 1): the whole text first (what the model read before, so nothing it
    found then is lost), then each sentence, then each adjacent pair. When the
    sentences alone exceed the cap they are grouped into consecutive runs, so
    every sentence is still in a window short enough to read; pairs fill what
    room is left."""
    whole = (text or "").strip()
    max_windows = max(1, int(max_windows))
    sents = premise_sentences(whole)
    if len(sents) <= 1 or max_windows == 1:
        return [whole]
    room = max_windows - 1
    if len(sents) > room:
        n = len(sents)
        cuts = [i * n // room for i in range(room + 1)]
        singles = [" ".join(sents[a:b]) for a, b in zip(cuts, cuts[1:], strict=False)]
        pairs: List[str] = []
    else:
        singles = list(sents)
        pairs = [f"{a} {b}" for a, b in zip(sents, sents[1:], strict=False)]
    out: List[str] = [whole]
    seen = {whole}
    for w in singles + pairs:
        if len(out) >= max_windows:
            break
        if w not in seen:
            seen.add(w)
            out.append(w)
    return out


_CONTENT_WORD = re.compile(r"[a-z]+")
_STOPWORDS = frozenset(
    "a an and are as at be by for from in is it its of on or the to with".split()
)


def _content_words(text: str) -> set:
    return set(_CONTENT_WORD.findall(text.lower())) - _STOPWORDS


def aligned_windows(windows: List[str], claim: str) -> List[int]:
    """The indexes of the claim's own sentence among ``windows`` (the first, the whole
    passage, is never one): the windows holding the largest share of the claim's
    content words, and of those the ones with the fewest sentences (so a sentence,
    not the pairs it is in). Several when they tie; none when no window shares a word
    with the claim."""
    words = _content_words(claim)
    if not words or len(windows) < 2:
        return []
    share = [len(words & _content_words(w)) / len(words) for w in windows]
    best = max(share[1:])
    if best <= 0:
        return []
    tied = [i for i in range(1, len(windows)) if share[i] == best]
    size = {i: len(premise_sentences(windows[i])) for i in tied}
    fewest = min(size.values())
    return [i for i in tied if size[i] == fewest]


def _label_prob(result: "NLIResult", name: str) -> float:
    """The probability of ``name`` in one NLI result: the model's softmax when it is
    there, else the confidence when it is the predicted label (the rule-based fallback
    and test stubs give only a label and its confidence)."""
    p = float(result.logits.get(name, 0.0) or 0.0) if result.logits else 0.0
    if result.label.value == name:
        p = max(p, float(result.confidence))
    return p


def _canonical_number(token: str) -> str:
    if "." in token:
        whole, frac = token.split(".", 1)
        frac = frac.rstrip("0")
    else:
        whole, frac = token, ""
    whole = whole.lstrip("0") or "0"
    return f"{whole}.{frac}" if frac else whole


def numbers_in(text: str) -> set:
    """The numbers stated in text, canonical (1,000 -> 1000; 5.0 -> 5; 0.50 -> 0.5),
    without citation markers, a leading list marker, or digits inside codes and names."""
    text = _LIST_MARKER.sub("", strip_answer_scaffold(strip_citation_tags(text)))
    out = set()
    for m in _NUMBER.finditer(text):
        token = m.group(0).rstrip(",")
        if _THOUSANDS.fullmatch(token):
            parts = [token.replace(",", "")]
        else:
            parts = [p for p in token.split(",") if p]
        out.update(_canonical_number(p) for p in parts)
    return out


@dataclass
class FullVerificationReport:
    """Complete verification report for a CCA response."""
    response_text: str
    n_claims: int
    n_citations_found: int
    n_uncited_claims: int
    # Per-claim results
    citation_results: List[CitationVerificationResult]
    # Aggregate scores
    nli_pass_rate: float       # Fraction of claims that passed NLI
    nli_flag_rate: float       # Fraction flagged (neutral)
    nli_redact_rate: float     # Fraction redacted (contradiction)
    # Redacted response
    redacted_response: str
    # Semantic entropy (if computed)
    semantic_entropy: Optional[float] = None
    se_is_confabulation: bool = False
    # Per claim, in order: whether a cited, retrieved chunk entails it and holds every
    # number it states (the support gate, NLIBackedHGA.verify).
    claim_supported: List[bool] = field(default_factory=list)


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# 2.2  NLI ENTAILMENT VERIFIER (DeBERTa-v3)
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

class NLIEntailmentVerifier:
    """
    NLI-based claim verifier using DeBERTa-v3-large fine-tuned on
    MultiNLI (and optionally MedNLI for clinical domain).

    Given a (premise, hypothesis) pair:
      premise = KB chunk content (the "source of truth")
      hypothesis = generated claim from CCA

    Outputs: {entailment, neutral, contradiction} with confidence.

    DeBERTa-v3 label mapping:
      0 = contradiction
      1 = neutral
      2 = entailment
    """

    def __init__(
        self,
        model_name: str = DEFAULT_NLI_MODEL,
        device: str = "auto",
        confidence_threshold: float = 0.7,
        local_files_only: bool = False,
        require_model: bool = False,
    ):
        """local_files_only: model_name is a directory (a verified pin), and
        nothing is fetched. require_model: a model that fails to load raises
        RuntimeError instead of falling back to the rule-based checks (a
        pinned model never silently becomes something else)."""
        self.model_name = model_name
        self.confidence_threshold = confidence_threshold
        self.model = None
        self.tokenizer = None
        self._loaded = False
        self.label_map: Dict[int, str] = {}
        self._local_files_only = bool(local_files_only)
        # One verifier is shared by every job of a worker (boundary.models.nli_factory):
        # a fast tokenizer must not be used by two threads at once ("Already
        # borrowed"), so tokenising and inference run under this lock. It guards the
        # model only; nothing of a job is kept on the verifier.
        self._lock = threading.Lock()

        self._try_load_model(device)
        if require_model and not self._loaded:
            raise RuntimeError("the NLI model did not load")

    def _try_load_model(self, device: str):
        """Load the NLI model: safetensors weights only (never a pickled
        pytorch_model.bin)."""
        try:
            import torch
            from transformers import AutoModelForSequenceClassification, AutoTokenizer

            # The name only: a pinned model's name is its cache path.
            logger.info("Loading NLI model (local_files_only=%s)", self._local_files_only)
            self.tokenizer = AutoTokenizer.from_pretrained(
                self.model_name, local_files_only=self._local_files_only
            )
            self.model = AutoModelForSequenceClassification.from_pretrained(
                self.model_name,
                local_files_only=self._local_files_only,
                use_safetensors=True,
            )

            if device == "auto":
                self.device = "cuda" if torch.cuda.is_available() else "cpu"
            else:
                self.device = device

            self.model = self.model.to(self.device)
            self.model.eval()
            self.label_map = nli_label_map(self.model)
            self._loaded = True
            logger.info(f"NLI model loaded on {self.device}")

        except Exception as e:
            # The type only: an error's text can carry a path.
            logger.warning("NLI model loading failed: %s", type(e).__name__)
            logger.info("Using rule-based NLI fallback")
            self._loaded = False

    def verify(self, premise: str, hypothesis: str) -> NLIResult:
        """
        Run NLI inference on a (premise, hypothesis) pair.

        Args:
            premise: Source text (KB chunk)
            hypothesis: Claim to verify

        Returns:
            NLIResult with label and confidence
        """
        if self._loaded and self.model is not None:
            return self._verify_with_model(premise, hypothesis)
        else:
            return self._verify_fallback(premise, hypothesis)

    def _verify_with_model(self, premise: str, hypothesis: str) -> NLIResult:
        """Run DeBERTa-v3 NLI inference."""
        import torch

        with self._lock, torch.no_grad():
            inputs = self.tokenizer(
                premise, hypothesis,
                return_tensors="pt",
                max_length=512,
                truncation=True,
                padding=True
            ).to(self.device)
            outputs = self.model(**inputs)
            logits = outputs.logits[0]
            probs = torch.softmax(logits, dim=-1)

        pred_idx = probs.argmax().item()
        label = NLILabel(self.label_map[pred_idx])
        confidence = probs[pred_idx].item()

        return NLIResult(
            premise=premise[:100],
            hypothesis=hypothesis[:100],
            label=label,
            confidence=confidence,
            logits={name: probs[i].item() for i, name in self.label_map.items()},
        )

    # Windows per forward pass: sorted by length, so short windows aren't padded to
    # the whole chunk's length.
    BATCH_SIZE = 16

    def verify_pairs(self, pairs: List[Tuple[str, str]]) -> List[NLIResult]:
        """One NLIResult per (premise, hypothesis) pair, in order: batched forward
        passes with the model (under the lock, as verify), else the rule-based
        fallback per pair."""
        if not pairs:
            return []
        if not (self._loaded and self.model is not None):
            return [self._verify_fallback(p, h) for p, h in pairs]
        import torch

        order = sorted(range(len(pairs)), key=lambda i: len(pairs[i][0]) + len(pairs[i][1]))
        out: List[Optional[NLIResult]] = [None] * len(pairs)
        with self._lock, torch.no_grad():
            for start in range(0, len(order), self.BATCH_SIZE):
                idx = order[start:start + self.BATCH_SIZE]
                inputs = self.tokenizer(
                    [pairs[i][0] for i in idx], [pairs[i][1] for i in idx],
                    return_tensors="pt",
                    max_length=512,
                    truncation=True,
                    padding=True,
                ).to(self.device)
                probs = torch.softmax(self.model(**inputs).logits, dim=-1).cpu()
                for row, i in enumerate(idx):
                    p = probs[row]
                    pred = int(p.argmax().item())
                    out[i] = NLIResult(
                        premise=pairs[i][0][:100],
                        hypothesis=pairs[i][1][:100],
                        label=NLILabel(self.label_map[pred]),
                        confidence=float(p[pred].item()),
                        logits={name: float(p[k].item()) for k, name in self.label_map.items()},
                    )
        return [r for r in out if r is not None]

    def windows_for(self, passage: str) -> List[str]:
        """The premises a claim is checked against: the passage's windows
        (premise_windows) with the model; the whole passage alone for the rule-based
        fallback, as before."""
        if not (self._loaded and self.model is not None):
            return [passage]
        return premise_windows(passage)

    def _verify_fallback(self, premise: str, hypothesis: str) -> NLIResult:
        """
        Rule-based NLI fallback when DeBERTa is unavailable.

        Uses keyword overlap + contradiction detection heuristics.
        """
        premise_lower = premise.lower()
        hypothesis_lower = hypothesis.lower()

        # Extract key terms
        premise_words = set(
            w for w in premise_lower.split()
            if len(w) > 3 and w.isalpha()
        )
        hypothesis_words = set(
            w for w in hypothesis_lower.split()
            if len(w) > 3 and w.isalpha()
        )

        overlap = len(premise_words & hypothesis_words)
        total = max(len(hypothesis_words), 1)
        overlap_ratio = overlap / total

        # Check for explicit contradiction signals
        contradiction_signals = [
            # Negation mismatch
            ('not ' in hypothesis_lower and 'not ' not in premise_lower
             and overlap_ratio > 0.3),
            # Number contradiction (different numbers for same concept)
            self._detect_number_contradiction(premise_lower, hypothesis_lower),
        ]

        if any(contradiction_signals):
            label = NLILabel.CONTRADICTION
            confidence = 0.75
        elif overlap_ratio > 0.4:
            label = NLILabel.ENTAILMENT
            confidence = 0.5 + 0.4 * overlap_ratio
        elif overlap_ratio > 0.15:
            label = NLILabel.NEUTRAL
            confidence = 0.6
        else:
            label = NLILabel.NEUTRAL
            confidence = 0.5

        return NLIResult(
            premise=premise[:100],
            hypothesis=hypothesis[:100],
            label=label,
            confidence=confidence,
            logits={"entailment": 0.0, "neutral": 0.0, "contradiction": 0.0}
        )

    def _detect_number_contradiction(self, premise: str, hypothesis: str) -> bool:
        """Detect if premise and hypothesis state different numbers for same concept."""
        # Extract (keyword, number) pairs
        pattern = r'(\w+)\s*[:\-=]?\s*(\d+(?:\.\d+)?)\s*(?:mg|g|ml|%|mmHg|bpm)?'

        premise_nums = {}
        for match in re.finditer(pattern, premise):
            premise_nums[match.group(1)] = float(match.group(2))

        for match in re.finditer(pattern, hypothesis):
            keyword = match.group(1)
            value = float(match.group(2))
            if keyword in premise_nums:
                ratio = max(value, premise_nums[keyword]) / max(min(value, premise_nums[keyword]), 0.01)
                if ratio > 2.0:  # More than 2x different
                    return True

        return False

    def check_bidirectional_entailment(
        self,
        text_a: str,
        text_b: str
    ) -> bool:
        """
        Check if two texts share meaning via bidirectional entailment.
        Used for semantic entropy clustering.

        Two texts share meaning iff:
          NLI(A→B) = ENTAILMENT AND NLI(B→A) = ENTAILMENT
        """
        forward = self.verify(text_a, text_b)
        if forward.label != NLILabel.ENTAILMENT:
            return False
        backward = self.verify(text_b, text_a)
        return backward.label == NLILabel.ENTAILMENT


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# 2.3  CITATION VERIFICATION PIPELINE
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

class CitationVerificationPipeline:
    """
    Extracts [KB#XXXX] citations from CCA output, looks up the
    cited KB chunk, and runs NLI verification on each claim.

    Produces per-claim PASS/FLAG/REDACT decisions and a redacted
    version of the response where contradicted claims are replaced.

    Decision logic (per citation; reason codes in CITATION_REASONS):
      cited chunk not in the KB                 → FLAG   (not_found)
      another patient's record                  → REDACT (other_patient), no NLI
      not retrieved for this question           → REDACT (not_retrieved), no NLI
      NLI over the chunk's windows (premise_windows: the whole chunk, each
      sentence, each adjacent pair; at most NLI_MAX_WINDOWS):
      contradiction >= 0.5: the whole chunk's,
        or the claim's own sentence's           → REDACT (contradicted), a veto
      else max entailment >= 0.7 in a window    → PASS   (entailed)
      otherwise                                 → FLAG   (not_entailed)
      No citation                               → FLAG   (uncited)
    then, per claim: an entailed citation whose claim states a number that
    none of the claim's checked sources holds → FLAG (number_not_in_source).
    A claim is supported when one of its citations is still PASS
    (FullVerificationReport.claim_supported).
    """

    def __init__(
        self,
        nli_verifier: NLIEntailmentVerifier,
        kb,  # MedicalKnowledgeBase from Part 1
        entailment_threshold: float = 0.7,
        contradiction_threshold: float = 0.5
    ):
        self.nli = nli_verifier
        self.kb = kb
        self.entailment_thresh = entailment_threshold
        self.contradiction_thresh = contradiction_threshold

    @staticmethod
    def _unchecked(
        claim_text: str, cid: str, idx: int, action: VerificationAction, code: str, reason: str
    ) -> CitationVerificationResult:
        """A citation decided without NLI (no source text is read or kept)."""
        return CitationVerificationResult(
            claim_text=claim_text,
            citation_id=cid,
            source_text="",
            nli_result=NLIResult(
                premise="", hypothesis=claim_text[:100],
                label=NLILabel.NEUTRAL, confidence=0.0
            ),
            action=action,
            reason=reason,
            code=code,
            claim_index=idx,
        )

    def _nli_pairs(self, pairs: List[Tuple[str, str]]) -> List[NLIResult]:
        batch = getattr(self.nli, "verify_pairs", None)
        if callable(batch):
            return list(batch(pairs))
        # A verifier with verify() only (tests): the same pairs, one by one.
        return [self.nli.verify(premise=p, hypothesis=h) for p, h in pairs]

    def _nli_over_passage(self, passage: str, hypothesis: str) -> Tuple[NLIResult, float, float]:
        """(the result shown, entailment, contradiction) of ``hypothesis`` against
        ``passage``, over its windows (premise_windows; the first is the whole passage).

        entailment: the highest over the windows.
        contradiction: the higher of the whole passage's (the check made before, so every
        contradiction found then is still found) and that of the claim's own sentence
        (aligned_windows: the sentence sharing most of the claim's words). Not the
        highest over every window: the model calls two different facts of one source
        contradictory ("Hypertension defined as ..." against "T2DM: fasting glucose ...";
        the adult-dose sentence against the pediatric-dose one), which vetoed 48 of 48
        claims copied word for word from a long chunk in scripts/hga_grounding_eval.py.

        The result shown is the deciding window: the contradicting one when the
        contradiction reaches the threshold, else the most entailing one. Its logits
        carry the entailment, the contradiction counted, and the highest contradiction
        of any window (contradiction_any_window), for the record."""
        wf = getattr(self.nli, "windows_for", None)
        windows = list(wf(passage)) if callable(wf) else premise_windows(passage)
        results = self._nli_pairs([(w, hypothesis) for w in windows])
        if not results or len(results) != len(windows):
            # Nothing (or not everything) was scored: never support.
            empty = NLIResult(premise="", hypothesis=hypothesis[:100],
                              label=NLILabel.NEUTRAL, confidence=0.0)
            return empty, 0.0, 0.0
        ents = [_label_prob(r, "entailment") for r in results]
        cons = [_label_prob(r, "contradiction") for r in results]
        counted = [0, *aligned_windows(windows, hypothesis)]
        decisive = max(counted, key=lambda i: cons[i])
        entail, contra = max(ents), cons[decisive]
        if contra >= self.contradiction_thresh:
            best = results[decisive]
            shown = NLIResult(best.premise, best.hypothesis, NLILabel.CONTRADICTION, contra)
        else:
            best = results[ents.index(entail)]
            shown = NLIResult(best.premise, best.hypothesis, best.label, best.confidence)
        shown.logits = dict(best.logits or {})
        shown.logits.update({
            "entailment": entail,
            "contradiction": contra,
            "contradiction_any_window": max(cons),
        })
        return shown, entail, contra

    def verify_response(
        self,
        response_text: str,
        patient_id: Optional[str] = None,
        allowed_chunk_ids: Optional[AbstractSet[str]] = None,
    ) -> FullVerificationReport:
        """
        Verify all citations in a CCA response.

        allowed_chunk_ids: the chunks retrieved for this question. A citation
        of any other chunk is redacted (not_retrieved) before any NLI call,
        even when the chunk is in the job's KB. None checks against the KB
        only (the legacy callers; every job passes its retrieved ids).

        Steps:
          1. Split response into sentences/claims
          2. Extract [KB#XXXX] citations from each claim
          3. Look up cited KB chunk; refuse another patient's or an
             unretrieved one
          4. Run NLI: the source chunk's windows (premises; premise_windows)
             vs. the claim without its citation markers (hypothesis;
             strip_citation_tags)
          5. Assign PASS/FLAG/REDACT, then the number rule per claim
          6. Build redacted response
        """
        # Step 1: Split into claims (sentence-level)
        claims = self._extract_claims(response_text)

        # Step 2-5: Verify each claim
        results = []
        n_uncited = 0
        supported: List[bool] = []

        for idx, (claim_text, citation_ids) in enumerate(claims):
            if not citation_ids:
                # Uncited claim — flag for review
                n_uncited += 1
                results.append(self._unchecked(
                    claim_text, "NONE", idx, VerificationAction.FLAG, "uncited",
                    "No citation provided — claim unverifiable"
                ))
                supported.append(False)
                continue

            if copies_format_example(claim_text):
                # The model copied the format's example instead of answering:
                # never support, whatever the NLI model would say.
                for cid in citation_ids:
                    results.append(self._unchecked(
                        claim_text, cid, idx, VerificationAction.FLAG, "not_entailed",
                        "Claim copies the answer format's example"
                    ))
                supported.append(False)
                continue

            claim_results: List[CitationVerificationResult] = []
            sources: List[str] = []
            # Verify against each cited source
            for cid in citation_ids:
                chunk = self.kb.get_chunk(cid) if self.kb else None
                if chunk is None:
                    claim_results.append(self._unchecked(
                        claim_text, cid, idx, VerificationAction.FLAG, "not_found",
                        f"Citation {cid} not found in KB"
                    ))
                    continue

                if not chunk_visible_to(chunk, patient_id):
                    # Never show or reason from another patient's record.
                    claim_results.append(self._unchecked(
                        claim_text, cid, idx, VerificationAction.REDACT, "other_patient",
                        f"Citation {cid} is not this patient's record"
                    ))
                    continue

                if allowed_chunk_ids is not None and cid not in allowed_chunk_ids:
                    # Only what was retrieved for this question can support it.
                    claim_results.append(self._unchecked(
                        claim_text, cid, idx, VerificationAction.REDACT, "not_retrieved",
                        f"Citation {cid} was not retrieved for this question"
                    ))
                    continue

                # Run NLI on the claim without its citation markers: the
                # NLI model reads a marker as unsupported content. The
                # claim shown, and the checks above, keep the original.
                nli_result, entail, contra = self._nli_over_passage(
                    chunk.content,
                    strip_answer_scaffold(strip_citation_tags(claim_text)),
                )
                sources.append(chunk.content)

                # Decision logic, safety first: a contradiction vetoes, even when a
                # window entails the claim; PASS only with a window that entails it
                # and no contradiction (_nli_over_passage).
                if contra >= self.contradiction_thresh:
                    action = VerificationAction.REDACT
                    code = "contradicted"
                    reason = f"Claim CONTRADICTS source {cid}"
                elif entail >= self.entailment_thresh:
                    action = VerificationAction.PASS
                    code = "entailed"
                    reason = "Claim entailed by source"
                else:
                    action = VerificationAction.FLAG
                    code = "not_entailed"
                    reason = (
                        f"Claim not clearly supported "
                        f"({nli_result.label.value}, "
                        f"conf={nli_result.confidence:.2f})"
                    )

                claim_results.append(CitationVerificationResult(
                    claim_text=claim_text,
                    citation_id=cid,
                    source_text=chunk.content[:150],
                    nli_result=nli_result,
                    action=action,
                    reason=reason,
                    code=code,
                    claim_index=idx,
                ))

            # The number rule: every number the claim states must be in one of
            # the sources it was checked against; an NLI model can entail a
            # claim whose dose or value differs from its source.
            stated = numbers_in(claim_text)
            held = set().union(*(numbers_in(t) for t in sources)) if sources else set()
            if stated - held:
                for r in claim_results:
                    if r.action == VerificationAction.PASS:
                        r.action = VerificationAction.FLAG
                        r.code = "number_not_in_source"
                        r.reason = "A number in the claim is not in its cited sources"
            results.extend(claim_results)
            supported.append(
                any(r.action == VerificationAction.PASS for r in claim_results)
            )

        # Step 6: Build redacted response
        redacted = self._build_redacted_response(response_text, results)

        # Compute aggregate rates
        n_total = max(len(results), 1)
        n_pass = sum(1 for r in results if r.action == VerificationAction.PASS)
        n_flag = sum(1 for r in results if r.action == VerificationAction.FLAG)
        n_redact = sum(1 for r in results if r.action == VerificationAction.REDACT)

        return FullVerificationReport(
            response_text=response_text,
            n_claims=len(claims),
            n_citations_found=sum(len(cids) for _, cids in claims),
            n_uncited_claims=n_uncited,
            citation_results=results,
            nli_pass_rate=n_pass / n_total,
            nli_flag_rate=n_flag / n_total,
            nli_redact_rate=n_redact / n_total,
            redacted_response=redacted,
            claim_supported=supported,
        )

    def _extract_claims(
        self,
        text: str
    ) -> List[Tuple[str, List[str]]]:
        """
        Split text into (claim_text, [citation_ids]) pairs.

        Each sentence or numbered point is a claim.
        Citations are extracted via [KB#XXXX] regex.
        """
        # Split on sentence boundaries and numbered items
        raw_claims = re.split(r'(?<=[.!?])\s+|\n\s*\d+\.\s*', text)

        results = []
        for claim in raw_claims:
            claim = claim.strip()
            if len(claim) < 10:
                continue

            # Skip section headers
            if claim.isupper() and len(claim) < 50:
                continue
            if claim.startswith("NOTE:") or claim.startswith("---"):
                continue

            # Extract citation IDs
            citation_ids = re.findall(CITATION_MARKER, claim)
            # Normalize to full format
            citation_ids = [f"KB#{cid}" for cid in citation_ids]

            results.append((claim, citation_ids))

        return results

    def _build_redacted_response(
        self,
        original: str,
        results: List[CitationVerificationResult]
    ) -> str:
        """Replace contradicted claims with redaction notices."""
        redacted = original
        for r in results:
            if r.action == VerificationAction.REDACT:
                # Find and replace the claim in the response
                claim_escaped = re.escape(r.claim_text[:80])
                replacement = (
                    f"[REDACTED: Claim contradicts source {r.citation_id}. "
                    f"Original claim removed for safety.]"
                )
                # Try to match a portion of the claim
                if r.claim_text[:60] in redacted:
                    redacted = redacted.replace(
                        r.claim_text[:60],
                        replacement[:60] + "...",
                        1
                    )
        return redacted


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# 2.4  SEMANTIC ENTROPY FOR RAG RESPONSES
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

class SemanticEntropyVerifier:
    """
    Semantic entropy (Farquhar et al., Nature 2024) adapted for
    RAG-grounded responses.

    Samples K responses from CCA with the SAME retrieval context,
    clusters by bidirectional NLI entailment, and computes:

      H_SE = -sum_j p(C_j) log2 p(C_j)

    High H_SE (> tau_SE) indicates the model confabulates even
    with grounding context — a strong hallucination signal.

    For RAG, the threshold is LOWER than base LLM (tau=0.8 vs 1.2)
    because grounded responses should be MORE consistent.
    """

    def __init__(
        self,
        nli_verifier: NLIEntailmentVerifier,
        n_samples: int = 5,
        threshold: float = 0.8,  # Lower for RAG (should be more consistent)
    ):
        self.nli = nli_verifier
        self.n_samples = n_samples
        self.threshold = threshold

    def compute_entropy(
        self,
        responses: List[str]
    ) -> Tuple[float, int, bool]:
        """
        Compute semantic entropy over K sampled responses.

        Args:
            responses: List of K generated responses (same query+context)

        Returns:
            (entropy_bits, n_clusters, is_confabulation)
        """
        if len(responses) < 2:
            return 0.0, 1, False

        # Cluster by bidirectional entailment
        clusters = []
        for resp in responses:
            assigned = False
            for cluster in clusters:
                if self.nli.check_bidirectional_entailment(
                    cluster["representative"], resp
                ):
                    cluster["members"].append(resp)
                    assigned = True
                    break
            if not assigned:
                clusters.append({
                    "representative": resp,
                    "members": [resp]
                })

        n_clusters = len(clusters)

        # Compute entropy (uniform probability per member)
        total = sum(len(c["members"]) for c in clusters)
        probs = [len(c["members"]) / total for c in clusters]

        from scipy.stats import entropy as scipy_entropy
        h_se = float(scipy_entropy(probs, base=2))

        is_confab = h_se > self.threshold

        logger.debug(
            f"Semantic entropy: H={h_se:.3f}, "
            f"{n_clusters} clusters from {len(responses)} samples, "
            f"confab={is_confab}"
        )

        return h_se, n_clusters, is_confab

    def verify_with_sampling(
        self,
        rag_cca,  # RAGClinicalContextAgent from Part 1
        query: str,
        top_k: int = 5,
        patient_id: Optional[str] = None,
    ) -> Tuple[float, int, bool, List[str]]:
        """
        Sample K responses from the RAG CCA and compute entropy.
        """
        responses = []
        for i in range(self.n_samples):
            result = rag_cca.generate_with_rag(
                query, top_k=top_k, patient_id=patient_id
            )
            responses.append(result["response"])

        h_se, n_clusters, is_confab = self.compute_entropy(responses)
        return h_se, n_clusters, is_confab, responses


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# 2.4b THE SUPPORT GATE (#5) AND PER-CITATION RESULTS (#28)
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

# The support gate's outcome codes: "passed", or why the answer can't PASS.
SUPPORT_GATE_CODES = (
    "passed",
    "no_citation_check",  # no citation pipeline: nothing shows a claim is supported
    "no_claims",          # nothing to check
    "unsupported_claim",  # a claim no cited, retrieved chunk entails (with its numbers)
    "contradicted",       # a cited source contradicts a claim
    "other_patient",      # a claim cites another patient's record
)
_DECISION_RANK = {"pass": 0, "regenerate": 1, "escalate": 2}
MAX_CITATION_CHECKS = 50
MAX_CLAIM_TEXT = 300


def support_gate(report: Optional[FullVerificationReport]) -> Tuple[str, str]:
    """(code, the most the decision may be). Fail closed: anything but every claim
    supported and nothing contradicted is not a PASS.

    An unsupported claim caps the answer at "regenerate": the decision the trust
    score itself gives an unsupported claim, shown amber, so the answer is never
    shown as high confidence. A contradiction or another patient's record is
    "escalate": a clinician reviews it (as the escalation guard does a dose
    outside the cited chunks). A lower score's own "escalate" always stands."""
    if report is None:
        return "no_citation_check", "regenerate"
    codes = {r.code for r in report.citation_results}
    if "other_patient" in codes:
        return "other_patient", "escalate"
    if "contradicted" in codes:
        return "contradicted", "escalate"
    if not report.claim_supported:
        return "no_claims", "regenerate"
    if not all(report.claim_supported):
        return "unsupported_claim", "regenerate"
    return "passed", "pass"


def citation_checks(report: Optional[FullVerificationReport]) -> List[Dict[str, Any]]:
    """Each citation's result, for the decision row (how this answer was checked):
    the chunk id, the claim (whitespace folded, at most MAX_CLAIM_TEXT characters),
    the NLI label and score, the action and the reason code. Never the source text."""
    if report is None:
        return []
    out: List[Dict[str, Any]] = []
    for r in report.citation_results:
        if r.citation_id == "NONE":
            continue
        out.append({
            "chunk_id": r.citation_id,
            "claim_index": r.claim_index,
            "claim_text": " ".join(r.claim_text.split())[:MAX_CLAIM_TEXT],
            "nli_label": r.nli_result.label.value.upper(),
            "nli_confidence": round(float(r.nli_result.confidence), 4),
            "action": r.action.value.upper(),
            "reason": r.code,
        })
        if len(out) == MAX_CITATION_CHECKS:
            break
    return out


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# 2.5  NLI-BACKED HGA (UPGRADED WITH REAL VERIFICATION)
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

class NLIBackedHGA:
    """
    The full M3 Hallucination Guard Agent integrating:
      - Expert 1: NLI citation verification (DeBERTa-v3)
      - Expert 2: Coherence + garbled text detection (from M2)
      - Expert 3: Context-aware numerical accuracy (from M2)
      - Expert 4 (NEW): Semantic entropy consistency check

    Trust score:
      T(y) = alpha * NLI_score + beta * Coh_score
             + gamma * Num_score + delta * SE_score
             - flag_penalty

    where delta weights the semantic entropy signal.
    """

    # Fabrication markers (from M2 upgraded HGA)
    FABRICATION_MARKERS = [
        "fictionol", "fictional", "fakemed",
        "discovered in 2028", "discovered in 2029",
        "100% efficacy", "zero side effects",
        "no known side effects", "universal cure",
    ]

    # Contradiction patterns (from M2)
    CONTRADICTION_PATTERNS = [
        (r'(?:malaria|plasmodium).*(?:positive|confirmed|detected)',
         r'(?:malaria|plasmodium).*(?:negative|not\s+detected|no\s+treatment)'),
        (r'(?:is|are)\s+recommended',
         r'(?:is|are)\s+not\s+recommended'),
        (r'(?:confirmed|diagnosed|positive)',
         r'no\s+treatment\s+needed'),
    ]

    # Context-aware numerical patterns (from M2)
    NUMERICAL_PATTERNS = [
        (r'(?:HR|heart\s*rate|pulse)\s*[:\-=]?\s*(\d+)', "heart_rate", 20, 250),
        (r'(?:SpO2|O2\s*sat)\s*[:\-=]?\s*(\d+(?:\.\d+)?)\s*%?', "spo2", 40, 100),
        (r'(?:troponin\s*[IT]?)\s*[:\-=]?\s*(\d+(?:\.\d+)?)', "troponin", 0, 100),
        (r'(?:glucose|blood\s*sugar|FBS)\s*[:\-=]?\s*(\d+)', "glucose", 20, 800),
        (r'(?:hemoglobin|Hgb|Hb)\s*[:\-=]?\s*(\d+(?:\.\d+)?)', "hemoglobin", 2, 25),
        (r'(?:platelet|PLT)\s*[:\-=]?\s*(\d+)', "platelets", 5, 1500),
        (r'(?:bilirubin|bili)\s*[:\-=]?\s*(\d+(?:\.\d+)?)', "bilirubin", 0, 50),
        (r'(?:metformin)\s*(\d+)\s*mg', "metformin_dose", 100, 3000),
    ]

    def __init__(
        self,
        nli_verifier: Optional[NLIEntailmentVerifier] = None,
        citation_pipeline: Optional[CitationVerificationPipeline] = None,
        se_verifier: Optional[SemanticEntropyVerifier] = None,
        # Weights
        alpha: float = 0.35,     # NLI citation verification
        beta: float = 0.25,      # Coherence + fabrication
        gamma: float = 0.20,     # Numerical accuracy
        delta: float = 0.20,     # Semantic entropy
        # Thresholds
        pass_threshold: float = 0.55,
        escalate_threshold: float = 0.3,
        # Penalty
        critical_flag_penalty: float = 0.20,
        warning_flag_penalty: float = 0.05,
    ):
        self.nli_verifier = nli_verifier
        self.citation_pipeline = citation_pipeline
        self.se_verifier = se_verifier

        self.alpha = alpha
        self.beta = beta
        self.gamma = gamma
        self.delta = delta
        self.pass_threshold = pass_threshold
        self.escalate_threshold = escalate_threshold
        self.critical_penalty = critical_flag_penalty
        self.warning_penalty = warning_flag_penalty

    def verify(
        self,
        response_text: str,
        diagnoses: Optional[List[Dict]] = None,
        semantic_entropy: Optional[float] = None,
        source_chunks: Optional[List] = None,
        patient_id: Optional[str] = None,
        pass_threshold: Optional[float] = None,
        escalate_threshold: Optional[float] = None,
        critical_penalty: Optional[float] = None,
        warning_penalty: Optional[float] = None,
        allowed_chunk_ids: Optional[AbstractSet[str]] = None,
    ) -> Dict[str, Any]:
        """
        Full four-expert verification, then the support gate (#5).

        The weighted trust score ranks the answer and sets the decision as
        before; the support gate (support_gate) can then only make it
        stricter, never looser. An answer can PASS only when every claim has a
        cited, retrieved chunk that entails it and holds its numbers; otherwise
        the decision is at most REGENERATE (amber, as the trust score's own
        band for an unsupported claim), and a contradicted claim or a citation
        of another patient's record escalates.

        Args:
            response_text: CCA generated text
            diagnoses: Parsed diagnoses (for ICD-10 validation)
            semantic_entropy: Pre-computed H_SE (if available)
            source_chunks: Retrieved KB chunks for NLI
            patient_id: Patient the response is about; citations of any
                other patient's records are redacted
            pass_threshold, escalate_threshold, critical_penalty,
            warning_penalty: Per-call overrides of the instance defaults.
                Pass these instead of mutating the shared instance, which
                is not safe across concurrent queries.
            allowed_chunk_ids: The chunks retrieved for this question; a
                citation of any other is redacted as not_retrieved (#28).

        Returns:
            Dict with trust_score, decision, per-expert scores, flags
        """
        if pass_threshold is None:
            pass_threshold = self.pass_threshold
        if escalate_threshold is None:
            escalate_threshold = self.escalate_threshold
        if critical_penalty is None:
            critical_penalty = self.critical_penalty
        if warning_penalty is None:
            warning_penalty = self.warning_penalty

        all_flags = []

        # ── Expert 1: NLI Citation Verification ──
        nli_score = 0.8  # default if no citation pipeline
        citation_report = None

        if self.citation_pipeline is not None:
            citation_report = self.citation_pipeline.verify_response(
                response_text, patient_id=patient_id, allowed_chunk_ids=allowed_chunk_ids
            )
            nli_score = citation_report.nli_pass_rate

            # Collect redacted claims as critical flags
            for r in citation_report.citation_results:
                if r.action == VerificationAction.REDACT:
                    all_flags.append({
                        "type": "nli_contradiction",
                        "severity": "critical",
                        "text": r.claim_text[:80],
                        "reason": r.reason
                    })
                elif r.action == VerificationAction.FLAG:
                    all_flags.append({
                        "type": "nli_unsupported",
                        "severity": "warning",
                        "text": r.claim_text[:80],
                        "reason": r.reason
                    })
        else:
            # Fallback: use fabrication/contradiction detection from M2
            nli_score, nli_flags = self._check_fabrication_contradiction(response_text)
            all_flags.extend(nli_flags)

        # ── Expert 2: Coherence + Garbled Text ──
        coh_score, coh_flags = self._check_coherence(response_text)
        all_flags.extend(coh_flags)

        # ── Expert 3: Numerical Accuracy ──
        num_score, num_flags = self._check_numerical(response_text)
        all_flags.extend(num_flags)

        # ── Expert 4: Semantic Entropy ──
        se_score = 1.0  # default (no confabulation)
        if semantic_entropy is not None:
            # Convert entropy to score: high entropy → low score
            se_threshold = 0.8 if self.citation_pipeline else 1.2
            if semantic_entropy > se_threshold:
                se_score = max(0.0, 1.0 - (semantic_entropy / 2.322))
                all_flags.append({
                    "type": "semantic_entropy",
                    "severity": "critical",
                    "text": f"H_SE={semantic_entropy:.3f}",
                    "reason": f"High semantic entropy ({semantic_entropy:.3f} > {se_threshold})"
                })
            else:
                se_score = 1.0 - (semantic_entropy / (2 * se_threshold))

        # ── Composite Trust Score ──
        base_trust = (
            self.alpha * nli_score
            + self.beta * coh_score
            + self.gamma * num_score
            + self.delta * se_score
        )

        # ── Flag Penalty ──
        n_critical = sum(1 for f in all_flags if f.get("severity") == "critical")
        n_warning = sum(1 for f in all_flags if f.get("severity") == "warning")
        penalty = n_critical * critical_penalty + n_warning * warning_penalty
        trust = max(0.0, base_trust - penalty)

        # ── Decision ──
        if trust >= pass_threshold:
            decision = "pass"
        elif trust >= escalate_threshold:
            decision = "regenerate"
        else:
            decision = "escalate"

        # ── Support gate: the score can't pass what no source supports ──
        gate, cap = support_gate(citation_report)
        if _DECISION_RANK[cap] > _DECISION_RANK[decision]:
            decision = cap

        # ── Badge ──
        badge = {"pass": "GREEN", "regenerate": "AMBER", "escalate": "RED"}[decision]

        return {
            "trust_score": round(trust, 4),
            "base_trust": round(base_trust, 4),
            "decision": decision,
            "badge": badge,
            # Per-expert
            "nli_score": round(nli_score, 4),
            "coherence_score": round(coh_score, 4),
            "numerical_score": round(num_score, 4),
            "semantic_entropy_score": round(se_score, 4),
            # Flags
            "n_critical_flags": n_critical,
            "n_warning_flags": n_warning,
            "flag_penalty": round(penalty, 4),
            "flags": all_flags,
            # Citation report
            "citation_report": {
                "n_claims": citation_report.n_claims if citation_report else 0,
                "pass_rate": citation_report.nli_pass_rate if citation_report else 0,
                "redact_rate": citation_report.nli_redact_rate if citation_report else 0,
            } if citation_report else None,
            # Weights
            "weights": {
                "alpha": self.alpha, "beta": self.beta,
                "gamma": self.gamma, "delta": self.delta
            },
            # The support gate's code (SUPPORT_GATE_CODES): "passed", or why not.
            "support_gate": gate,
            # Per citation (uncited claims are counted by the gate, not listed).
            "citations": citation_checks(citation_report),
        }

    def _check_fabrication_contradiction(self, text: str) -> Tuple[float, List[Dict]]:
        """Expert 1 fallback: fabrication + contradiction detection."""
        flags = []
        score = 1.0
        text_lower = text.lower()

        for marker in self.FABRICATION_MARKERS:
            if marker in text_lower:
                flags.append({
                    "type": "fabrication", "severity": "critical",
                    "text": marker,
                    "reason": f"Fabrication marker: '{marker}'"
                })
                score -= 0.3

        for pat_a, pat_b in self.CONTRADICTION_PATTERNS:
            if re.search(pat_a, text_lower) and re.search(pat_b, text_lower):
                flags.append({
                    "type": "contradiction", "severity": "critical",
                    "text": "self-contradiction",
                    "reason": "Contradictory claims detected"
                })
                score -= 0.4

        return max(0.0, score), flags

    def _check_coherence(self, text: str) -> Tuple[float, List[Dict]]:
        """Expert 2: coherence + garbled text."""
        flags = []
        score = 1.0

        if not text or len(text.strip()) < 20:
            flags.append({
                "type": "empty_content", "severity": "critical",
                "text": text[:30] if text else "[EMPTY]",
                "reason": "Empty or near-empty content"
            })
            return 0.0, flags

        # Garbled text detection
        alpha_ratio = sum(
            c.isalpha() or c.isspace() or c in '.,;:!?-/()'
            for c in text
        ) / max(len(text), 1)

        garble_patterns = ['@', '###', '???', '$$$', '%%%']
        garble_hits = sum(1 for p in garble_patterns if p in text)

        if alpha_ratio < 0.6 or garble_hits > 0:
            quality = max(0.0, alpha_ratio - 0.2 * garble_hits)
            flags.append({
                "type": "garbled_text", "severity": "critical",
                "text": text[:60],
                "reason": f"Text quality={quality:.2f} -- likely garbled"
            })
            score = quality

        return score, flags

    def _check_numerical(self, text: str) -> Tuple[float, List[Dict]]:
        """Expert 3: context-aware numerical accuracy."""
        flags = []
        checked = 0
        ok = 0

        for pattern, field_name, val_min, val_max in self.NUMERICAL_PATTERNS:
            for match in re.finditer(pattern, text, re.IGNORECASE):
                try:
                    value = float(match.group(1))
                except (ValueError, IndexError):
                    continue
                checked += 1
                if value < val_min or value > val_max:
                    flags.append({
                        "type": "implausible_value", "severity": "critical",
                        "text": match.group(0),
                        "reason": f"Implausible {field_name}: {value} (range: {val_min}-{val_max})"
                    })
                else:
                    ok += 1

        if checked == 0:
            return 0.85, flags
        return ok / checked, flags


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# 2.6  DEMO
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

def demo_m3_part2():
    """Demonstrate NLI verification + citation pipeline + NLI-backed HGA."""
    print("=" * 70)
    print("MHAI M3 Part 2: NLI Verifier + Citation Pipeline + HGA")
    print("=" * 70)

    # ── Step 1: Load NLI Verifier ──
    print("\n[1/5] Loading NLI Entailment Verifier...")
    nli = NLIEntailmentVerifier(device="cpu")
    print(f"  Model loaded: {nli._loaded}")

    # ── Step 2: Test NLI verification ──
    print("\n[2/5] Testing NLI verification on claim pairs...")
    test_pairs = [
        ("Metformin is the first-line treatment for T2DM.",
         "Metformin 500mg is recommended as initial T2DM therapy.",
         "ENTAILMENT expected"),
        ("Exercise of 150 minutes per week is recommended.",
         "300 minutes of vigorous exercise weekly is advised.",
         "CONTRADICTION expected"),
        ("Insulin therapy is initiated when HbA1c > 9%.",
         "The patient should eat more vegetables.",
         "NEUTRAL expected"),
    ]

    for premise, hypothesis, expected in test_pairs:
        result = nli.verify(premise, hypothesis)
        icon = "+" if expected.split()[0] == result.label.value.upper() else "?"
        print(f"  [{icon}] {result.label.value.upper()} "
              f"(conf={result.confidence:.2f})")
        print(f"      P: {premise[:60]}...")
        print(f"      H: {hypothesis[:60]}...")

    # ── Step 3: Build KB + Citation pipeline ──
    print("\n[3/5] Building KB and citation verification pipeline...")

    # Minimal KB for demo
    from intelligence.knowledge_base.kb_builder import KBChunk, MedicalKnowledgeBase
    kb = MedicalKnowledgeBase()
    for g in MedicalKnowledgeBase.REFERENCE_GUIDELINES:
        cid = kb._generate_id()
        kb.chunks[cid] = KBChunk(
            chunk_id=cid, content=g["content"],
            chunk_type="guideline", source_patient_id="reference",
            source_subject_id="reference", metadata={"topic": g["topic"]}
        )
    print(f"  KB: {len(kb.chunks)} guideline chunks")

    pipeline = CitationVerificationPipeline(nli, kb)

    # ── Step 4: Verify a cited response ──
    print("\n[4/5] Verifying a citation-backed CCA response...")

    test_response = (
        "1. Gestational Diabetes Mellitus [ICD-10: O24.4] [KB#0001]\n"
        "   The patient's 2-hour glucose of 168 mg/dL exceeds the "
        "153 mg/dL threshold for GDM diagnosis [KB#0001].\n"
        "2. Management: dietary modification with 2000 cal diet [KB#0001].\n"
        "3. Exercise: 300 minutes of vigorous exercise weekly [KB#0001].\n"
        "4. The patient should receive Fictionol 5000mg daily."
    )

    report = pipeline.verify_response(test_response)

    print(f"  Claims: {report.n_claims}")
    print(f"  Citations found: {report.n_citations_found}")
    print(f"  Uncited claims: {report.n_uncited_claims}")
    print(f"  Pass rate: {report.nli_pass_rate:.1%}")
    print(f"  Flag rate: {report.nli_flag_rate:.1%}")
    print(f"  Redact rate: {report.nli_redact_rate:.1%}")

    print(f"\n  Per-claim results:")
    for r in report.citation_results:
        icon = {"pass": "+", "flag": "?", "redact": "X"}[r.action.value]
        print(f"    [{icon}] {r.action.value.upper()}: "
              f"{r.claim_text[:55]}...")
        print(f"        {r.reason}")

    # ── Step 5: Full NLI-backed HGA ──
    print("\n[5/5] Full NLI-backed HGA verification...")

    hga = NLIBackedHGA(
        nli_verifier=nli,
        citation_pipeline=pipeline,
        alpha=0.35, beta=0.25, gamma=0.20, delta=0.20
    )

    hga_result = hga.verify(
        response_text=test_response,
        semantic_entropy=0.5  # Low (consistent)
    )

    badge_icon = {"GREEN": "G", "AMBER": "A", "RED": "R"}
    print(f"\n  Trust: {hga_result['trust_score']:.3f} "
          f"(base={hga_result['base_trust']:.3f}, "
          f"penalty={hga_result['flag_penalty']:.3f})")
    print(f"  Decision: {hga_result['decision'].upper()} "
          f"[{badge_icon.get(hga_result['badge'], '?')}]")
    print(f"  NLI={hga_result['nli_score']:.2f}, "
          f"Coh={hga_result['coherence_score']:.2f}, "
          f"Num={hga_result['numerical_score']:.2f}, "
          f"SE={hga_result['semantic_entropy_score']:.2f}")
    print(f"  Flags: {hga_result['n_critical_flags']} critical, "
          f"{hga_result['n_warning_flags']} warning")

    if hga_result['citation_report']:
        cr = hga_result['citation_report']
        print(f"  Citations: {cr['n_claims']} claims, "
              f"pass={cr['pass_rate']:.1%}, "
              f"redact={cr['redact_rate']:.1%}")

    print("\n" + "=" * 70)
    print("M3 Part 2 complete. Proceed to Part 3 for end-to-end")
    print("evaluation targeting <=5% hallucination rate?")
    print("=" * 70)


if __name__ == "__main__":
    demo_m3_part2()

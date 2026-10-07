"""NLI verifiers must read the label order from the model, not assume MNLI's."""

from types import SimpleNamespace

import torch

from mhai.agents.hga_nli import NLIEntailmentVerifier, NLILabel
from mhai.utils.nli import nli_label_map

# cross-encoder/nli-deberta-v3-base's real config.
CROSS_ENCODER = {0: "contradiction", 1: "entailment", 2: "neutral"}


class FakeNLIModel:
    """Always puts all probability on one logit index."""

    def __init__(self, id2label, winning_index):
        self.config = SimpleNamespace(id2label=id2label)
        self.winning_index = winning_index

    def __call__(self, **inputs):
        logits = torch.full((1, 3), -10.0)
        logits[0, self.winning_index] = 10.0
        return SimpleNamespace(logits=logits)


class FakeTokenizer:
    def __call__(self, *a, **k):
        return SimpleNamespace(to=lambda device: {})


def _verifier(model):
    v = NLIEntailmentVerifier.__new__(NLIEntailmentVerifier)
    v.model, v.tokenizer, v.device, v._loaded = model, FakeTokenizer(), "cpu", True
    v.label_map = nli_label_map(model)
    return v


def test_label_map_follows_model_config():
    assert nli_label_map(FakeNLIModel(CROSS_ENCODER, 0)) == CROSS_ENCODER


def test_label_map_falls_back_to_mnli_order_for_generic_names():
    generic = {0: "LABEL_0", 1: "LABEL_1", 2: "LABEL_2"}
    assert nli_label_map(FakeNLIModel(generic, 0)) == {
        0: "contradiction",
        1: "neutral",
        2: "entailment",
    }


def test_entailment_index_is_read_as_entailment():
    # Index 1 is "entailment" for this model; the old hard-coded map called it neutral.
    result = _verifier(FakeNLIModel(CROSS_ENCODER, 1)).verify("p", "h")
    assert result.label == NLILabel.ENTAILMENT


def test_neutral_index_is_never_read_as_entailment():
    # Index 2 is "neutral" here; the old map turned unsupported claims into passes.
    result = _verifier(FakeNLIModel(CROSS_ENCODER, 2)).verify("p", "h")
    assert result.label == NLILabel.NEUTRAL
    assert set(result.logits) == {"contradiction", "entailment", "neutral"}

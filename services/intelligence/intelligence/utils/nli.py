"""Helpers shared by the NLI-based verifiers."""

# Default NLI model; mirrors MHAIConfig.NLI_MODEL.
DEFAULT_NLI_MODEL = "cross-encoder/nli-deberta-v3-base"

# Order used by the original MNLI checkpoints. Only a fallback: models
# disagree (cross-encoder/nli-deberta-v3-base is contradiction,
# entailment, neutral), so always prefer the model's own config.
_MNLI_ORDER = {0: "contradiction", 1: "neutral", 2: "entailment"}
_KNOWN = {"contradiction", "neutral", "entailment"}


def nli_label_map(model) -> dict[int, str]:
    """Map logit index -> "contradiction" | "neutral" | "entailment".

    Reads the model's id2label. Falls back to the MNLI order only when the
    config carries no usable names (e.g. LABEL_0..LABEL_2).
    """
    id2label = getattr(getattr(model, "config", None), "id2label", None) or {}
    labels = {int(i): str(name).strip().lower() for i, name in id2label.items()}
    if set(labels.values()) == _KNOWN:
        return labels
    return dict(_MNLI_ORDER)

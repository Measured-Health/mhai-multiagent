"""HuggingFace model loader with caching and rate limiting."""
import logging

log = logging.getLogger(__name__)

def load_hf_model(name: str, task: str = "text-classification", device: str = "cpu"):
    """Load a HuggingFace model with caching."""
    from transformers import AutoModelForSequenceClassification, AutoTokenizer
    log.info(f"Loading {name} for {task} on {device}")
    tokenizer = AutoTokenizer.from_pretrained(name)
    model = AutoModelForSequenceClassification.from_pretrained(name).to(device)
    model.eval()
    return model, tokenizer

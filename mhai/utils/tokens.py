"""Token counting utilities."""

def count_tokens(text: str, model: str = "gpt2") -> int:
    """Approximate token count."""
    try:
        from transformers import AutoTokenizer
        tok = AutoTokenizer.from_pretrained(model)
        return len(tok.encode(text))
    except Exception:
        return len(text.split()) * 4 // 3  # rough approximation

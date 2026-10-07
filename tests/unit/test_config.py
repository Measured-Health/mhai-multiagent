"""Tests for mhai.config.settings."""
from mhai.config import MHAIConfig, get_config

def test_get_config_returns_dataclass():
    cfg = get_config()
    assert isinstance(cfg, MHAIConfig)

def test_trust_weights_sum_to_one():
    cfg = get_config()
    total = cfg.TRUST_ALPHA + cfg.TRUST_BETA + cfg.TRUST_GAMMA + cfg.TRUST_DELTA
    assert abs(total - 1.0) < 1e-9

def test_nli_model_is_valid():
    cfg = get_config()
    assert "deberta-v3-large-mnli" not in cfg.NLI_MODEL, "Invalid model ID not fixed!"
    assert "nli-deberta-v3-base" in cfg.NLI_MODEL

def test_category_thresholds_have_emergency():
    cfg = get_config()
    assert "emergency" in cfg.CATEGORY_THRESHOLDS
    assert cfg.CATEGORY_THRESHOLDS["emergency"]["trust_pass"] == 0.80

def test_no_hardcoded_colab_path():
    cfg = get_config()
    for attr in dir(cfg):
        val = getattr(cfg, attr)
        if isinstance(val, str):
            assert "/content/" not in val, f"{attr} contains Colab path"

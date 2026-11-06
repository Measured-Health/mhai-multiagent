import pytest

from intelligence.modelstore.reasons import REASONS, Refused


def test_refused_only_known_reasons() -> None:
    with pytest.raises(ValueError):
        Refused("made_up_reason")
    for reason in REASONS:
        assert Refused(reason).reason == reason


def test_refused_str_has_no_detail() -> None:
    err = Refused("hash_mismatch", detail_code="weights.safetensors")
    assert str(err) == "hash_mismatch"
    assert "weights" not in repr(err.args)
    assert err.detail_code == "weights.safetensors"


def test_reason_set_is_closed() -> None:
    assert isinstance(REASONS, frozenset)
    assert len(REASONS) == 46
    assert "invalid_name" in REASONS
    assert "base_unverified" in REASONS  # Task 14: a promoted base not yet verified
    assert "key_invalid" in REASONS  # Task 15: a signing key file refused
    assert "key_exists" in REASONS  # Task 15: dev-keygen never overwrites a key
    assert "gate_failed" in REASONS  # Task 22: the manifest's own gate result at load
    assert "github_unavailable" in REASONS  # Task 20: lock-check can't read the PR
    assert "source_unavailable" in REASONS  # Task 18: a run source that can't be read

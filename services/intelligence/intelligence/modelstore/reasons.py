"""The closed set of refusal reasons.

Every check in the model store returns one of these fixed codes. A reason
carries no file name, value or content: the optional ``detail_code`` is a
short fixed code for operators and is never part of ``str()``.
"""

from collections.abc import Mapping
from types import MappingProxyType

REASONS: frozenset[str] = frozenset(
    {
        "file_not_allowed",
        "pickle_refused",
        "code_refused",
        "safetensors_invalid",
        "json_invalid",
        "too_large",
        "too_many_files",
        "hash_mismatch",
        "size_mismatch",
        "missing_file",
        "extra_file",
        "manifest_invalid",
        "not_canonical",
        "signature_invalid",
        "signer_untrusted",
        "policy_invalid",
        "lock_mismatch",
        "lock_invalid",
        "identity_mismatch",
        "version_exists",
        "not_found",
        "dataset_unknown",
        "dataset_not_synthetic",
        "training_manifest_missing",
        "licence_not_allowed",
        "revision_unpinned",
        "no_suite",
        "no_runtime",
        "evaluation_failed",
        "testset_mismatch",
        "not_approved",
        "separation_of_duties",
        "label_missing",
        "fixture_name",
        "source_invalid",
        "source_unavailable",
        "drive_scope",
        "warmup_mismatch",
        "warmup_page_missing",
        "store_unavailable",
        "not_allowed_here",
        "invalid_name",
        "base_unverified",
        "key_invalid",
        "key_exists",
        "gate_failed",
        "gguf_invalid",
        "training_manifest_misnamed",
        "role_unknown",
        "gguf_not_allowed_for_role",
        "gguf_template_mismatch",
    }
)

# The fix for a refusal whose cause is a plain naming mistake: a fixed text (no file name,
# value or content), printed with the code by the command line.
FIXES: Mapping[str, str] = MappingProxyType(
    {
        "training_manifest_misnamed": (
            "found manifest.json; rename it to training_manifest.json"
            " (manifest.json is reserved for the signed store manifest)"
        ),
    }
)


class Refused(Exception):
    """A check failed closed. ``str()`` is the reason code and nothing else."""

    def __init__(self, reason: str, detail_code: str | None = None) -> None:
        if reason not in REASONS:
            raise ValueError("unknown refusal reason")
        super().__init__(reason)
        self.reason = reason
        self.detail_code = detail_code

    def __str__(self) -> str:
        return self.reason

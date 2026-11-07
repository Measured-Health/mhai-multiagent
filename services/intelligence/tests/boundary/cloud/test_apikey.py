"""ApiKey, key files and the redacting log filter (A10). Every key is fake."""

from __future__ import annotations

import copy
import io
import logging
import os
import pickle
from collections.abc import Callable, Iterator
from pathlib import Path

import pytest

from intelligence.boundary.cloud.apikey import (
    SDK_LOGGERS,
    ApiKey,
    RedactingFilter,
    install_redaction,
    load_api_key,
    quiet_sdk_loggers,
)
from tests.boundary.cloud.conftest import FAKE_KEY, FAKE_SK_KEY

pytestmark = pytest.mark.disable_socket

NAME = "INTELLIGENCE_CLOUD_OPENAI_KEY_FILE"
REDACTED = "ApiKey(<redacted>)"


def test_apikey_never_in_repr_str_format_pickle_copy() -> None:
    k = ApiKey(FAKE_KEY)
    shown = [
        repr(k),
        str(k),
        f"{k}",
        f"{k!r}",
        "%s" % k,  # noqa: UP031
        "%r" % k,  # noqa: UP031
        "{}".format(k),  # noqa: UP032
        format(k),
        repr([k]),
        repr({"key": k}),
        repr((k,)),
    ]
    for s in shown:
        assert FAKE_KEY not in s
        assert REDACTED in s
    for proto in range(pickle.HIGHEST_PROTOCOL + 1):
        with pytest.raises(TypeError):
            pickle.dumps(k, protocol=proto)
    with pytest.raises(TypeError):
        copy.copy(k)
    with pytest.raises(TypeError):
        copy.deepcopy(k)
    with pytest.raises(TypeError):
        copy.deepcopy({"key": k})
    assert not hasattr(k, "__dict__")
    with pytest.raises(AttributeError):
        k.extra = 1  # type: ignore[attr-defined]
    with pytest.raises(AttributeError):
        k._value = "other"  # type: ignore[misc]
    assert k.reveal() == FAKE_KEY


def test_apikey_refuses_a_non_string() -> None:
    with pytest.raises(TypeError):
        ApiKey(b"bytes-are-not-a-key-000000")  # type: ignore[arg-type]


def _load(path: Path | str, development: bool = False) -> tuple[ApiKey | None, list[str]]:
    errs: list[str] = []
    return load_api_key(str(path), NAME, development, errs), errs


@pytest.mark.parametrize("mode", [0o400, 0o440, 0o600])
def test_key_file_good(key_file: Callable[..., Path], mode: int) -> None:
    key, errs = _load(key_file(mode=mode))
    assert errs == []
    assert key is not None and key.reveal() == FAKE_KEY


@pytest.mark.parametrize(
    "contents,why",
    [
        ("", "empty"),
        ("\n", "empty"),
        ("change-me-" + "0" * 20, "placeholder"),
        ("x" * 10 + "PlaceHolder" + "x" * 10, "placeholder"),
        ("fake" + "0" * 8 + " " + "0" * 12, "whitespace"),
        ("fake" + "0" * 8 + "\t" + "0" * 12, "whitespace"),
        (" " + FAKE_KEY, "whitespace"),
        (FAKE_KEY + " ", "whitespace"),
        ("fake" + "0" * 8 + "\n" + "0" * 12, "whitespace"),
        ("fake" + "0" * 8 + "​" + "0" * 12, "character"),
        ("fake" + "0" * 8 + "é" + "0" * 12, "character"),
        ("fake" + "0" * 15, "short"),
    ],
)
def test_key_file_mode_and_placeholder_refused(
    key_file: Callable[..., Path], contents: str, why: str
) -> None:
    for development in (False, True):
        key, errs = _load(key_file(contents), development)
        assert key is None
        assert len(errs) == 1 and NAME in errs[0] and why in errs[0]
        if contents.strip():
            assert contents.strip() not in errs[0]


def test_key_file_missing_directory_or_writable_refused(
    key_file: Callable[..., Path], tmp_path: Path
) -> None:
    for path in (tmp_path / "missing", tmp_path):
        key, errs = _load(path)
        assert key is None and len(errs) == 1 and NAME in errs[0]
    key, errs = _load(key_file(mode=0o620))
    assert key is None and "writable" in errs[0]


@pytest.mark.parametrize("mode", [0o404, 0o444, 0o644, 0o604])
def test_world_readable_key_file_refused(key_file: Callable[..., Path], mode: int) -> None:
    """Unlike the database password (a warning), a key file others can read
    is refused, in development too."""
    for development in (False, True):
        key, errs = _load(key_file(mode=mode), development)
        assert key is None
        assert errs == [f"{NAME} is readable by others (use mode 0400 or 0440)"]


@pytest.fixture
def captured() -> Iterator[tuple[logging.Logger, io.StringIO, RedactingFilter]]:
    out = io.StringIO()
    handler = logging.StreamHandler(out)
    handler.setFormatter(logging.Formatter("%(levelname)s %(message)s"))
    filt = RedactingFilter([ApiKey(FAKE_KEY)])
    handler.addFilter(filt)
    log = logging.getLogger("tests.cloud.redaction")
    log.addHandler(handler)
    log.setLevel(logging.DEBUG)
    log.propagate = False
    try:
        yield log, out, filt
    finally:
        log.removeHandler(handler)


def test_redacting_filter_on_message_args_and_exception(
    captured: tuple[logging.Logger, io.StringIO, RedactingFilter],
) -> None:
    log, out, _ = captured
    ant = "sk-ant-" + "a" * 16
    xai = "xai-" + "b" * 16
    log.info("calling with %s", FAKE_KEY)
    log.info("in the message " + FAKE_KEY)
    log.info("prefixed %s %s %s", FAKE_SK_KEY, ant, xai)
    log.info("mapping %(k)s", {"k": FAKE_KEY})
    log.info("as an object %r", [FAKE_KEY])
    try:
        raise RuntimeError(f"echoed request: Authorization: Bearer {FAKE_KEY}")
    except RuntimeError:
        log.exception("send failed")
    try:
        raise ValueError(f"x-api-key: {ant}")
    except ValueError as e:
        log.error("send failed", exc_info=e)
    log.warning("stack", stack_info=True, extra={"unused": 1})
    text = out.getvalue()
    for secret in (FAKE_KEY, FAKE_SK_KEY, ant, xai, "a" * 16, "b" * 16):
        assert secret not in text
    assert text.count("[REDACTED]") >= 9
    assert "Authorization: Bearer [REDACTED]" in text
    assert "RuntimeError" in text and "ValueError" in text  # the trace is kept


def test_redacting_filter_keeps_short_or_unrelated_text(
    captured: tuple[logging.Logger, io.StringIO, RedactingFilter],
) -> None:
    log, out, _ = captured
    log.info("task-1 sk-short xai-abc risk-assessment-score taxai-0123456789 %d", 7)
    assert out.getvalue().strip() == (
        "INFO task-1 sk-short xai-abc risk-assessment-score taxai-0123456789 7"
    )


def test_redacting_filter_survives_bad_args(
    captured: tuple[logging.Logger, io.StringIO, RedactingFilter],
) -> None:
    log, out, _ = captured
    log.info("two %s %s", FAKE_KEY)  # a bad call must not leak or raise
    text = out.getvalue()
    assert FAKE_KEY not in text and "[REDACTED]" in text


def test_install_redaction_covers_root_handlers() -> None:
    root = logging.getLogger()
    out = io.StringIO()
    handler = logging.StreamHandler(out)
    root.addHandler(handler)
    try:
        install_redaction([ApiKey(FAKE_KEY)])
        install_redaction([ApiKey(FAKE_KEY)])  # idempotent
        filters = [f for f in handler.filters if isinstance(f, RedactingFilter)]
        assert len(filters) == 1
        logging.getLogger("tests.cloud.root").warning("key %s", FAKE_KEY)
        assert FAKE_KEY not in out.getvalue() and "[REDACTED]" in out.getvalue()
    finally:
        root.removeHandler(handler)


def test_quiet_sdk_loggers() -> None:
    assert set(SDK_LOGGERS) >= {"openai", "anthropic"}
    for name in SDK_LOGGERS:
        logging.getLogger(name).setLevel(logging.DEBUG)
    quiet_sdk_loggers()
    for name in SDK_LOGGERS:
        assert logging.getLogger(name).level == logging.WARNING


def test_key_file_is_read_on_the_checked_descriptor(key_file: Callable[..., Path]) -> None:
    """The mode is checked with fstat on the open file, so a file swapped
    after the check can't be the one read: a FIFO is refused, not waited on."""
    fifo = key_file().parent / "fifo"
    os.mkfifo(fifo, 0o400)
    key, errs = _load(fifo)
    assert key is None and errs == [f"{NAME} is not a regular file"]

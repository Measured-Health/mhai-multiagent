import pytest

from intelligence.modelstore.names import (
    Version,
    check_name,
    check_role,
    check_run_id,
    check_store_version_id,
    check_version,
)
from intelligence.modelstore.reasons import Refused


@pytest.mark.parametrize(
    "bad",
    ["../x", "a/b", "x;rm", "X", "a" * 64, "a" * 63 + "b", "", "-ab", "a", "ab\n", "é1"],
)
def test_names_refuse_paths_and_shell(bad: str) -> None:
    with pytest.raises(Refused) as exc:
        check_name(bad)
    assert str(exc.value) == "invalid_name"


def test_good_names() -> None:
    assert check_name("mhai-tiny-extractor") == "mhai-tiny-extractor"
    assert check_name("a" * 63) == "a" * 63
    assert check_role("extractor_v2") == "extractor_v2"
    assert check_run_id("Run_01-a") == "Run_01-a"


@pytest.mark.parametrize("bad", ["1.0", "01.0.0", "1.0.0.0", "1.0.0-rc1", "v1.0.0", "1.0.0\n", ""])
def test_versions_refuse(bad: str) -> None:
    with pytest.raises(Refused):
        check_version(bad)


@pytest.mark.parametrize("bad", ["X", "1a", "a", "a" * 33, "a-b", "a/b"])
def test_roles_refuse(bad: str) -> None:
    with pytest.raises(Refused):
        check_role(bad)


@pytest.mark.parametrize("bad", ["", "a b", "a" * 65, "../x", "a\n"])
def test_run_ids_refuse(bad: str) -> None:
    with pytest.raises(Refused):
        check_run_id(bad)


def test_non_string_refused() -> None:
    with pytest.raises(Refused):
        check_name(5)  # type: ignore[arg-type]


def test_version_ordering_is_numeric() -> None:
    assert Version.parse("1.2.10") > Version.parse("1.2.9")
    assert Version.parse("2.0.0") > Version.parse("1.99.99")
    assert sorted([Version.parse("1.10.0"), Version.parse("1.9.0")])[0] == Version.parse("1.9.0")
    assert str(Version.parse("1.2.3")) == "1.2.3"


@pytest.mark.parametrize("bad", ["1" * 5000 + ".0.0", "1.0." + "9" * 10, "0000000001.0.0"])
def test_version_components_are_bounded(bad: str) -> None:
    with pytest.raises(Refused) as exc:
        Version.parse(bad)
    assert str(exc.value) == "invalid_name"
    with pytest.raises(Refused):
        check_version(bad)


def test_store_version_ids() -> None:
    uuid = "6d2f9c1e-3b4a-4f5e-9a8b-7c6d5e4f3a2b"
    assert check_store_version_id(uuid) == uuid
    assert check_store_version_id("Qv7d.yJ0b_8rQ-t5") == "Qv7d.yJ0b_8rQ-t5"
    assert check_store_version_id("a" * 1024) == "a" * 1024
    # A bucket without versioning: S3's own id for its one object.
    assert check_store_version_id("null") == "null"


@pytest.mark.parametrize(
    "bad", ["", "a/b", "a+b", "a=", "a b", "a?x=1", "a%2F", "a\n", "a" * 1025, "é", 7]
)
def test_store_version_ids_refused(bad: object) -> None:
    with pytest.raises(Refused) as exc:
        check_store_version_id(bad)  # type: ignore[arg-type]
    assert str(exc.value) == "invalid_name"

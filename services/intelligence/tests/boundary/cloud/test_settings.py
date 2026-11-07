"""CloudSettings: off by default, refused unless complete (K8, K9, K16,
A17), and the socket guard for this folder's unit modules. Every key and
reference here is synthetic."""

from __future__ import annotations

import ast
import logging
import socket
from collections.abc import Callable
from pathlib import Path

import pytest
from pytest_socket import SocketBlockedError

from intelligence.boundary.cloud import vocab
from intelligence.boundary.cloud.apikey import ApiKey
from intelligence.boundary.cloud.settings import (
    SDK_ENV_PREFIXES,
    SDK_ENV_REFUSED,
    CloudSettings,
    ProviderSettings,
)
from intelligence.boundary.config import ConfigError, WorkerSettings, is_demo, is_development
from tests.boundary.cloud.conftest import FAKE_KEY

pytestmark = pytest.mark.disable_socket

MODELS = {"anthropic": "claude-opus-5-5", "openai": "gpt-6.1-sol", "xai": "grok-4.7"}
ZDR = "DPA-2026-0001"
PROXY = "http://egress-proxy:3128"
WORKER = {
    "INTELLIGENCE_SERVICE": "pipeline",
    "NATS_URL": "nats://pipeline:test-only-pw@127.0.0.1:4222",
    "INTELLIGENCE_TRUSTED_KEYS_PATH": "/run/trusted/keys.json",
    "APP_ENV": "development",
}


def cloud_env(key: Path, *providers: str) -> dict[str, str]:
    env = {"INTELLIGENCE_CLOUD_PROVIDERS": ",".join(providers), "HTTPS_PROXY": PROXY}
    for p in providers:
        u = p.upper()
        env[f"INTELLIGENCE_CLOUD_{u}_MODEL"] = MODELS[p]
        env[f"INTELLIGENCE_CLOUD_{u}_KEY_FILE"] = str(key)
        env[f"INTELLIGENCE_CLOUD_{u}_ZDR_REF"] = ZDR
    return env


def read(env: dict[str, str], development: bool = False) -> tuple[CloudSettings, list[str]]:
    errs: list[str] = []
    return CloudSettings.from_env(env, development, errs), errs


def test_settings_default_has_no_providers() -> None:
    s, errs = read({})
    assert errs == [] and s == CloudSettings()
    assert (s.providers, s.proxy_url, s.demo) == ((), None, False)
    # A proxy or a stray model with no provider listed changes nothing.
    s, errs = read({"HTTPS_PROXY": PROXY, "INTELLIGENCE_CLOUD_PROVIDERS": " "})
    assert errs == [] and s == CloudSettings()
    # With no cloud variables, WorkerSettings is exactly today's.
    w = WorkerSettings.from_env(WORKER)
    assert w == WorkerSettings(
        service="pipeline",
        nats_url=WORKER["NATS_URL"],
        trusted_keys_path=WORKER["INTELLIGENCE_TRUSTED_KEYS_PATH"],
        development=True,
    )
    assert w.cloud == CloudSettings()


def test_settings_read_every_provider(key_file: Callable[..., Path]) -> None:
    key = key_file()
    s, errs = read(cloud_env(key, "xai", "anthropic", "openai"))
    assert errs == []
    assert [p.name for p in s.providers] == ["anthropic", "openai", "xai"]
    assert s.proxy_url == PROXY and not s.demo
    for p in s.providers:
        assert isinstance(p, ProviderSettings) and isinstance(p.key, ApiKey)
        assert p.model == MODELS[p.name] and p.zdr_ref == ZDR
        assert p.base_url == vocab.BASE_URLS[p.name]
        assert p.key.reveal() == FAKE_KEY
    assert FAKE_KEY not in repr(s) and FAKE_KEY not in str(s)
    w = WorkerSettings.from_env({**WORKER, **cloud_env(key, "openai")})
    assert [p.name for p in w.cloud.providers] == ["openai"]
    assert FAKE_KEY not in repr(w)


def _drop(name: str) -> Callable[[dict[str, str]], None]:
    return lambda env: env.pop(name)


def _set(name: str, value: str) -> Callable[[dict[str, str]], None]:
    return lambda env: env.__setitem__(name, value)


A = "INTELLIGENCE_CLOUD_ANTHROPIC_"


@pytest.mark.parametrize(
    "change,name,value",
    [
        (_set("INTELLIGENCE_CLOUD_PROVIDERS", "gemini"), "INTELLIGENCE_CLOUD_PROVIDERS", "gemini"),
        (
            _set("INTELLIGENCE_CLOUD_PROVIDERS", "anthropic,anthropic"),
            "INTELLIGENCE_CLOUD_PROVIDERS",
            "",
        ),
        (_set("INTELLIGENCE_CLOUD_PROVIDERS", "Anthropic"), "INTELLIGENCE_CLOUD_PROVIDERS", ""),
        (_set("INTELLIGENCE_CLOUD_PROVIDERS", "anthropic,"), "INTELLIGENCE_CLOUD_PROVIDERS", ""),
        (_drop(A + "KEY_FILE"), A + "KEY_FILE", ""),
        (_set(A + "KEY_FILE", "/nonexistent/anthropic-key"), A + "KEY_FILE", "/nonexistent"),
        (_drop(A + "MODEL"), A + "MODEL", ""),
        (_set(A + "MODEL", "claude opus"), A + "MODEL", "claude opus"),
        (_set(A + "MODEL", "c" * 65), A + "MODEL", "c" * 65),
        (_set(A + "MODEL", "claude/opus"), A + "MODEL", "claude/opus"),
        (_drop(A + "ZDR_REF"), A + "ZDR_REF", ""),
        (_set(A + "ZDR_REF", "dpa-2026-0001"), A + "ZDR_REF", "dpa-2026-0001"),
        (_set(A + "ZDR_REF", "D"), A + "ZDR_REF", ""),
        (_set(A + "ZDR_REF", "DPA 2026"), A + "ZDR_REF", "DPA 2026"),
        (_set(A + "ZDR_REF", "DEMO-SYNTHETIC"), A + "ZDR_REF", ""),
        (_drop("HTTPS_PROXY"), "HTTPS_PROXY", ""),
        (_set("HTTPS_PROXY", "https://egress-proxy:3128"), "HTTPS_PROXY", "egress-proxy"),
        (_set("HTTPS_PROXY", "http://egress-proxy"), "HTTPS_PROXY", "egress-proxy"),
        (_set("HTTPS_PROXY", "http://u:pw-not-real@egress-proxy:3128"), "HTTPS_PROXY", "pw-not"),
        (_set("HTTPS_PROXY", "http://egress-proxy:3128/x"), "HTTPS_PROXY", "egress-proxy"),
        (_set("HTTPS_PROXY", "http://egress-proxy:3128?a=b"), "HTTPS_PROXY", "egress-proxy"),
        (_set("HTTPS_PROXY", "socks5://egress-proxy:1080"), "HTTPS_PROXY", "egress-proxy"),
        (_set("HTTPS_PROXY", "http://egress-proxy:99999"), "HTTPS_PROXY", "egress-proxy"),
        (_set(A + "BASE_URL", "https://api.anthropic.com"), A + "BASE_URL", "api.anthropic"),
    ],
)
def test_settings_refusals(
    key_file: Callable[..., Path],
    change: Callable[[dict[str, str]], None],
    name: str,
    value: str,
) -> None:
    env = cloud_env(key_file(), "anthropic")
    change(env)
    s, errs = read(env)
    assert errs, "refused"
    text = "; ".join(errs)
    assert name in text
    if value:
        assert value not in text
    assert FAKE_KEY not in text
    with pytest.raises(ConfigError) as e:
        WorkerSettings.from_env({**WORKER, "APP_ENV": "production", **_prod_worker(), **env})
    assert name in str(e.value)


def _prod_worker() -> dict[str, str]:
    return {
        "NATS_URL": "tls://127.0.0.1:4222",
        "NATS_TLS_CA_PATH": "/run/nats/ca.pem",
        "NATS_TLS_CERT_PATH": "/run/nats/cert.pem",
        "NATS_TLS_KEY_PATH": "/run/nats/key.pem",
        "NATS_NKEY_SEED_PATH": "/run/nats/seed.nk",
    }


def test_production_worker_with_complete_cloud_settings_starts(
    key_file: Callable[..., Path],
) -> None:
    env = {**WORKER, "APP_ENV": "production", **_prod_worker(), **cloud_env(key_file(), "xai")}
    w = WorkerSettings.from_env(env)
    assert [p.name for p in w.cloud.providers] == ["xai"] and not w.development


def test_bad_key_refused_through_settings(key_file: Callable[..., Path]) -> None:
    env = cloud_env(key_file("short-fake-key"), "openai")
    _, errs = read(env)
    assert errs == ["INTELLIGENCE_CLOUD_OPENAI_KEY_FILE is too short (at least 20 characters)"]
    env = cloud_env(key_file(mode=0o644), "openai")
    _, errs = read(env, development=True)
    assert errs == [
        "INTELLIGENCE_CLOUD_OPENAI_KEY_FILE is readable by others (use mode 0400 or 0440)"
    ]


def test_development_relaxes_only_zdr_and_proxy(key_file: Callable[..., Path]) -> None:
    env = cloud_env(key_file(), "openai")
    del env["INTELLIGENCE_CLOUD_OPENAI_ZDR_REF"], env["HTTPS_PROXY"]
    s, errs = read(env, development=True)
    assert errs == [] and s.providers[0].zdr_ref is None and s.proxy_url is None
    del env["INTELLIGENCE_CLOUD_OPENAI_MODEL"]
    _, errs = read(env, development=True)
    assert errs == ["INTELLIGENCE_CLOUD_OPENAI_MODEL is required"]


def test_demo_accepts_demo_synthetic_with_warning_production_refuses(
    key_file: Callable[..., Path], caplog: pytest.LogCaptureFixture
) -> None:
    assert is_demo({"APP_ENV": "demo"}) and is_demo({"APP_ENV": " Demo "})
    assert not is_demo({}) and not is_demo({"APP_ENV": "development"})
    assert not is_development({"APP_ENV": "demo"})  # demo is production but for one value
    env = cloud_env(key_file(), "anthropic")
    env[A + "ZDR_REF"] = "DEMO-SYNTHETIC"
    with caplog.at_level(logging.WARNING):
        s, errs = read({**env, "APP_ENV": "demo"})
    assert errs == [] and s.demo and s.providers[0].zdr_ref == "DEMO-SYNTHETIC"
    assert any(A + "ZDR_REF" in r.getMessage() for r in caplog.records)
    for app_env in ("production", "", "staging"):
        _, errs = read({**env, "APP_ENV": app_env})
        assert errs == [A + "ZDR_REF=DEMO-SYNTHETIC is accepted only with APP_ENV=demo"]
    _, errs = read({k: v for k, v in env.items()})  # APP_ENV unset: production
    assert errs == [A + "ZDR_REF=DEMO-SYNTHETIC is accepted only with APP_ENV=demo"]
    _, errs = read(env, development=True)
    assert errs == []
    # Demo is production in every other respect.
    demo = {**cloud_env(key_file(), "anthropic"), "APP_ENV": "demo"}
    del demo["HTTPS_PROXY"]
    _, errs = read(demo)
    assert errs == ["HTTPS_PROXY is required when a cloud provider is enabled"]


def test_base_url_override_only_in_development(key_file: Callable[..., Path]) -> None:
    env = cloud_env(key_file(), "openai")
    env["INTELLIGENCE_CLOUD_OPENAI_BASE_URL"] = "http://mock-openai:8080/v1"
    s, errs = read(env, development=True)
    assert errs == [] and s.providers[0].base_url == "http://mock-openai:8080/v1"
    for app_env in ("production", "demo"):
        _, errs = read({**env, "APP_ENV": app_env})
        assert errs == ["INTELLIGENCE_CLOUD_OPENAI_BASE_URL is for development only"]
    # Refused outside development even for a provider that isn't enabled.
    _, errs = read({"INTELLIGENCE_CLOUD_XAI_BASE_URL": "https://api.x.ai/v1"})
    assert errs == ["INTELLIGENCE_CLOUD_XAI_BASE_URL is for development only"]
    for bad in ("ftp://mock-openai/v1", "mock-openai:8080", "http://", "http://u:p@mock:1/v1"):
        _, errs = read({**env, "INTELLIGENCE_CLOUD_OPENAI_BASE_URL": bad}, development=True)
        assert errs == ["INTELLIGENCE_CLOUD_OPENAI_BASE_URL is invalid"]


def _sdk_refusal(name: str) -> str:
    return (
        f"{name} must not be set (the cloud SDKs' own variables are refused; keys come from files)"
    )


def test_sdk_environment_variables_refused() -> None:
    """K16: any variable an SDK could read (a key, a base URL, extra headers,
    a credential chain, debug logging, trace propagation) refuses start-up,
    by prefix, so a name a later SDK release adds is refused too."""
    assert SDK_ENV_PREFIXES == ("OPENAI_", "ANTHROPIC_", "AZURE_OPENAI_", "XAI_")
    # The documented names (read by openai 3.37.0 and anthropic 2.3.0) all
    # fall under a prefix.
    assert {
        "OPENAI_API_KEY",
        "OPENAI_BASE_URL",
        "OPENAI_CUSTOM_HEADERS",
        "ANTHROPIC_API_KEY",
        "ANTHROPIC_CUSTOM_HEADERS",
        "ANTHROPIC_OPEN_TELEMETRY_PROPAGATION",
        "XAI_API_KEY",
    } <= set(SDK_ENV_REFUSED)
    assert all(n.startswith(SDK_ENV_PREFIXES) for n in SDK_ENV_REFUSED)
    value = "fake" + "9" * 24
    names = (
        *SDK_ENV_REFUSED,
        "OPENAI_CUSTOM_HEADERS",
        "ANTHROPIC_CUSTOM_HEADERS",
        "AZURE_OPENAI_API_KEY",
        "XAI_ANYTHING_NEW",
        "ANTHROPIC_A_NAME_A_LATER_SDK_ADDS",
    )
    for name in names:
        for v in (value, "", "anthropic-beta: x"):  # set at all, cloud enabled or not
            _, errs = read({name: v})
            assert errs == [_sdk_refusal(name)]
            _, errs = read({name: v}, development=True)
            assert errs == [_sdk_refusal(name)]
            with pytest.raises(ConfigError) as e:
                WorkerSettings.from_env({**WORKER, name: v})
            assert name in str(e.value) and value not in str(e.value)
    # Several are each named, in a fixed order.
    _, errs = read({"XAI_API_KEY": "", "ANTHROPIC_LOG": "debug"})
    assert errs == [_sdk_refusal("ANTHROPIC_LOG"), _sdk_refusal("XAI_API_KEY")]
    # Our own variables and look-alikes are not caught.
    _, errs = read({"INTELLIGENCE_CLOUD_OPENAI_MODEL": "gpt-6.1-sol", "MY_OPENAI_THING": "x"})
    assert errs == []


@pytest.mark.parametrize("name", ["SSL_CERT_FILE", "SSL_CERT_DIR"])
def test_ssl_cert_overrides_refused_with_a_provider(
    key_file: Callable[..., Path], name: str
) -> None:
    """A custom CA for the provider connections (httpcore2 and OpenSSL read
    these) is refused once a provider is enabled; without one, unchanged."""
    _, errs = read({name: "/etc/ssl/custom.pem"})
    assert errs == []
    env = {**cloud_env(key_file(), "openai"), name: "/etc/ssl/custom.pem"}
    for development in (False, True):
        _, errs = read(env, development)
        assert errs == [f"{name} must not be set when a cloud provider is enabled"]


@pytest.mark.parametrize(
    "model", ["claude-fable-5", "claude-fable-5-1", "Claude-FABLE-5", "claude-mythos-1", "mythos"]
)
def test_fable_and_mythos_models_refused(key_file: Callable[..., Path], model: str) -> None:
    env = cloud_env(key_file(), "anthropic")
    env[A + "MODEL"] = model
    for development in (False, True):
        _, errs = read(env, development)
        assert errs == [
            A + "MODEL names a model that needs 30-day retention (Fable, Mythos); not allowed"
        ]


def test_cloud_providers_are_for_the_pipeline_only(key_file: Callable[..., Path]) -> None:
    env = {**WORKER, "INTELLIGENCE_SERVICE": "echo", **cloud_env(key_file(), "openai")}
    with pytest.raises(ConfigError) as e:
        WorkerSettings.from_env(env)
    assert "INTELLIGENCE_CLOUD_PROVIDERS is for the pipeline service only" in str(e.value)


def test_provider_settings_repr_hides_the_key(key_file: Callable[..., Path]) -> None:
    s, _ = read(cloud_env(key_file(), "openai"))
    p = s.providers[0]
    assert FAKE_KEY not in repr(p) and "ApiKey(<redacted>)" in repr(p)


# --- the socket guard (Global Constraints) ---------------------------------------

CLOUD_TESTS = Path(__file__).resolve().parent


def _pytestmarks(path: Path) -> set[str]:
    tree = ast.parse(path.read_text(encoding="utf-8"))
    for node in tree.body:
        targets: list[ast.expr] = []
        value: ast.expr | None = None
        if isinstance(node, ast.Assign):
            targets, value = node.targets, node.value
        elif isinstance(node, ast.AnnAssign):
            targets, value = [node.target], node.value
        if value is None or not any(
            isinstance(t, ast.Name) and t.id == "pytestmark" for t in targets
        ):
            continue
        items = value.elts if isinstance(value, ast.List | ast.Tuple) else [value]
        return {ast.unparse(i) for i in items}
    return set()


def test_unit_modules_disable_sockets() -> None:
    modules = sorted(CLOUD_TESTS.glob("test_*.py"))
    assert {m.name for m in modules} >= {"test_vocab.py", "test_apikey.py", "test_settings.py"}
    missing = []
    for m in modules:
        marks = _pytestmarks(m)
        if "pytest.mark.boundary" not in marks and "pytest.mark.disable_socket" not in marks:
            missing.append(m.name)
    assert missing == []


def test_unit_modules_disable_sockets_sees_a_missing_mark(tmp_path: Path) -> None:
    bad = tmp_path / "test_bad.py"
    bad.write_text("import pytest\npytestmark = pytest.mark.slow\n", encoding="utf-8")
    assert _pytestmarks(bad) == {"pytest.mark.slow"}
    good = tmp_path / "test_good.py"
    good.write_text(
        "import pytest\npytestmark = [pytest.mark.slow, pytest.mark.disable_socket]\n",
        encoding="utf-8",
    )
    assert "pytest.mark.disable_socket" in _pytestmarks(good)


@pytest.mark.filterwarnings("ignore:A test tried to use socket.socket")
def test_sockets_are_blocked_here() -> None:
    with pytest.raises(SocketBlockedError):
        socket.socket(socket.AF_INET, socket.SOCK_STREAM)

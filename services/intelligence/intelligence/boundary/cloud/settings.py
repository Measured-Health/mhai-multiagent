"""The worker's cloud settings: off by default, refused unless complete.

INTELLIGENCE_CLOUD_PROVIDERS (a comma list, empty by default) names the
enabled providers. Each needs INTELLIGENCE_CLOUD_<P>_MODEL and
INTELLIGENCE_CLOUD_<P>_KEY_FILE everywhere; outside development it also
needs INTELLIGENCE_CLOUD_<P>_ZDR_REF and HTTPS_PROXY. The base URLs are
vocab.BASE_URLS; INTELLIGENCE_CLOUD_<P>_BASE_URL overrides one in
development only. ZDR_REF=DEMO-SYNTHETIC is accepted only with APP_ENV=demo
(with a warning) or in development. The SDKs' own environment variables are
refused whether a provider is enabled or not (SDK_ENV_PREFIXES), and a
custom CA (SSL_CERT_FILE, SSL_CERT_DIR) once one is.

Errors name variables, never their values (design section 7).
"""

from __future__ import annotations

import logging
import re
from collections.abc import Mapping
from dataclasses import dataclass
from urllib.parse import urlsplit

from intelligence.boundary.cloud import vocab
from intelligence.boundary.cloud.apikey import ApiKey, load_api_key
from intelligence.boundary.config import is_demo

log = logging.getLogger("intelligence.boundary.cloud.settings")

PROVIDERS_VAR = "INTELLIGENCE_CLOUD_PROVIDERS"
DEMO_ZDR_REF = "DEMO-SYNTHETIC"
_MODEL = re.compile(r"[A-Za-z0-9._:-]{1,64}")
_ZDR_REF = re.compile(r"[A-Z0-9][A-Z0-9_.-]{2,63}")
# Anthropic models that need 30-day retention: not allowed (design section 14).
_RETAINED_ANTHROPIC = ("fable", "mythos")

# Every variable under these prefixes refuses start-up, set at all (even
# empty), cloud enabled or not: the SDKs read a key, a base URL, extra
# request headers, an organisation, project or workspace, a credential
# chain, debug logging or trace propagation from them, and a new release
# adds names. A prefix also refuses those.
SDK_ENV_PREFIXES = ("OPENAI_", "ANTHROPIC_", "AZURE_OPENAI_", "XAI_")

# Documentation only (the prefixes are the rule): the names openai 3.37.0
# and anthropic 2.3.0 read for the clients this worker builds, from their
# sources (cloud model providers, Task 4 fix round). Review on an SDK bump.
SDK_ENV_REFUSED = (
    "OPENAI_API_KEY",
    "OPENAI_ADMIN_KEY",
    "OPENAI_BASE_URL",
    "OPENAI_CUSTOM_HEADERS",
    "OPENAI_ORG_ID",
    "OPENAI_PROJECT_ID",
    "OPENAI_WEBHOOK_SECRET",
    "OPENAI_LOG",
    "OPENAI_API_TYPE",
    "OPENAI_API_VERSION",
    "AZURE_OPENAI_API_KEY",
    "AZURE_OPENAI_AD_TOKEN",
    "AZURE_OPENAI_ENDPOINT",
    "ANTHROPIC_API_KEY",
    "ANTHROPIC_AUTH_TOKEN",
    "ANTHROPIC_BASE_URL",
    "ANTHROPIC_CUSTOM_HEADERS",
    "ANTHROPIC_WEBHOOK_SIGNING_KEY",
    "ANTHROPIC_LOG",
    "ANTHROPIC_PROFILE",
    "ANTHROPIC_CONFIG_DIR",
    "ANTHROPIC_IDENTITY_TOKEN",
    "ANTHROPIC_IDENTITY_TOKEN_FILE",
    "ANTHROPIC_FEDERATION_RULE_ID",
    "ANTHROPIC_ORGANIZATION_ID",
    "ANTHROPIC_WORKSPACE_ID",
    "ANTHROPIC_SERVICE_ACCOUNT_ID",
    "ANTHROPIC_SCOPE",
    "ANTHROPIC_OPEN_TELEMETRY",
    "ANTHROPIC_OPEN_TELEMETRY_PROPAGATION",
    "ANTHROPIC_OPEN_TELEMETRY_TRACES_CONTENT_MODE",
    "ANTHROPIC_OPEN_TELEMETRY_TRACES_MAX_CONTENT_BYTES",
    "XAI_API_KEY",
)

# A custom CA for the provider connections: OpenSSL's default paths and
# httpcore2's default context read these. Refused once a provider is enabled.
TLS_ENV_REFUSED = ("SSL_CERT_FILE", "SSL_CERT_DIR")


@dataclass(frozen=True)
class ProviderSettings:
    """One enabled provider. key's repr is redacted."""

    name: str
    model: str
    key: ApiKey
    zdr_ref: str | None
    base_url: str


@dataclass(frozen=True)
class CloudSettings:
    """The enabled providers (sorted by name), the egress proxy, and
    whether APP_ENV is demo (recorded only with a provider enabled). The
    default, and the result with no provider, is CloudSettings()."""

    providers: tuple[ProviderSettings, ...] = ()
    proxy_url: str | None = None
    demo: bool = False

    @classmethod
    def from_env(cls, env: Mapping[str, str], development: bool, errs: list[str]) -> CloudSettings:
        """Read the cloud settings, appending each problem to errs (the
        caller refuses to start on any). The result is only meaningful when
        errs gained nothing."""
        demo = is_demo(env)
        for name in sorted(env):
            if name.upper().startswith(SDK_ENV_PREFIXES):
                errs.append(
                    f"{name} must not be set "
                    "(the cloud SDKs' own variables are refused; keys come from files)"
                )

        def get(name: str) -> str | None:
            v = env.get(name, "").strip()
            return v or None

        base_urls: dict[str, str] = {}
        for p in vocab.PROVIDERS:
            var = f"INTELLIGENCE_CLOUD_{p.upper()}_BASE_URL"
            raw = get(var)
            if raw is None:
                continue
            if not development:
                errs.append(f"{var} is for development only")
            elif not _valid_base_url(raw):
                errs.append(f"{var} is invalid")
            else:
                base_urls[p] = raw

        names = _parse_providers(get(PROVIDERS_VAR), errs)
        if not names:
            return cls()
        for name in TLS_ENV_REFUSED:
            if name in env:
                errs.append(f"{name} must not be set when a cloud provider is enabled")
        providers = []
        for p in names:
            ps = _provider(env, p, development, demo, base_urls.get(p), errs)
            if ps is not None:
                providers.append(ps)
        proxy = get("HTTPS_PROXY")
        if proxy is None:
            if not development:
                errs.append("HTTPS_PROXY is required when a cloud provider is enabled")
        elif not _valid_proxy(proxy):
            errs.append("HTTPS_PROXY must be http://<host>:<port>")
            proxy = None
        return cls(providers=tuple(providers), proxy_url=proxy, demo=demo)


def _parse_providers(raw: str | None, errs: list[str]) -> list[str]:
    """Core's ParseCloudProviders: blank is none; otherwise each comma
    separated name, trimmed, is a known provider and appears once."""
    if raw is None:
        return []
    out: list[str] = []
    for part in raw.split(","):
        p = part.strip()
        if p not in vocab.PROVIDERS or p in out:
            errs.append(
                f"{PROVIDERS_VAR} must list each of {' '.join(vocab.PROVIDERS)} at most once, "
                "comma separated"
            )
            return []
        out.append(p)
    return sorted(out)


def _provider(
    env: Mapping[str, str],
    p: str,
    development: bool,
    demo: bool,
    base_url: str | None,
    errs: list[str],
) -> ProviderSettings | None:
    prefix = f"INTELLIGENCE_CLOUD_{p.upper()}_"
    before = len(errs)

    def get(name: str) -> str | None:
        v = env.get(prefix + name, "").strip()
        return v or None

    model = get("MODEL")
    if model is None:
        errs.append(f"{prefix}MODEL is required")
    elif not _MODEL.fullmatch(model):
        errs.append(f"{prefix}MODEL must be 1 to 64 of A-Z a-z 0-9 . _ : -")
    elif p == "anthropic" and any(m in model.casefold() for m in _RETAINED_ANTHROPIC):
        errs.append(
            f"{prefix}MODEL names a model that needs 30-day retention (Fable, Mythos); not allowed"
        )

    key: ApiKey | None = None
    key_file = get("KEY_FILE")
    if key_file is None:
        errs.append(f"{prefix}KEY_FILE is required")
    else:
        key = load_api_key(key_file, f"{prefix}KEY_FILE", development, errs)

    zdr_ref = get("ZDR_REF")
    if zdr_ref is None:
        if not development:
            errs.append(f"{prefix}ZDR_REF is required outside development")
    elif not _ZDR_REF.fullmatch(zdr_ref):
        errs.append(f"{prefix}ZDR_REF must be 3 to 64 of A-Z 0-9 _ . - (starting A-Z or 0-9)")
    elif zdr_ref == DEMO_ZDR_REF and not development:
        if demo:
            log.warning("%sZDR_REF is %s (APP_ENV=demo): synthetic data only", prefix, DEMO_ZDR_REF)
        else:
            errs.append(f"{prefix}ZDR_REF={DEMO_ZDR_REF} is accepted only with APP_ENV=demo")

    if len(errs) != before or model is None or key is None:
        return None
    return ProviderSettings(
        name=p,
        model=model,
        key=key,
        zdr_ref=zdr_ref,
        base_url=base_url or vocab.BASE_URLS[p],
    )


def _valid_proxy(raw: str) -> bool:
    """http://<host>:<port> and nothing else: no credentials, path, query
    or fragment. The proxy is inside the network; TLS runs through it."""
    try:
        u = urlsplit(raw)
        port = u.port
    except ValueError:
        return False
    return (
        u.scheme == "http"
        and bool(u.hostname)
        and port is not None
        and 0 < port
        and u.username is None
        and u.password is None
        and u.path in ("", "/")
        and not u.query
        and not u.fragment
    )


def _valid_base_url(raw: str) -> bool:
    """A development override: http(s) with a host and no credentials."""
    try:
        u = urlsplit(raw)
        _ = u.port  # raises on a bad port
    except ValueError:
        return False
    return (
        u.scheme in ("http", "https")
        and bool(u.hostname)
        and u.username is None
        and u.password is None
        and not u.query
        and not u.fragment
    )

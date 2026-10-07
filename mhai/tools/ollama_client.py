"""CCA text generation through an Ollama server.

Lets the CCA run its LLM in the ``ollama`` service from docker-compose.yml
instead of loading Qwen into the API process.
"""
import logging
import os

import httpx

logger = logging.getLogger(__name__)


class OllamaGenerator:
    """Chat completion against Ollama's /api/chat endpoint."""

    def __init__(
        self,
        base_url: str | None = None,
        model: str | None = None,
        temperature: float = 0.3,
        max_new_tokens: int = 512,
        timeout: float | None = None,
        client: httpx.Client | None = None,
    ):
        self.base_url = (base_url or os.environ.get("OLLAMA_URL", "http://ollama:11434")).rstrip("/")
        self.model = model or os.environ.get("OLLAMA_MODEL", "qwen2.5:7b-instruct")
        self.temperature = temperature
        self.max_new_tokens = max_new_tokens
        timeout = timeout or float(os.environ.get("AGENT_HTTP_TIMEOUT", "300"))
        self._client = client or httpx.Client(timeout=timeout)

    @property
    def name(self) -> str:
        return f"ollama:{self.model}"

    def generate(self, system: str, user: str) -> str:
        """Return the assistant reply; raises on HTTP or protocol errors."""
        resp = self._client.post(
            f"{self.base_url}/api/chat",
            json={
                "model": self.model,
                "messages": [
                    {"role": "system", "content": system},
                    {"role": "user", "content": user},
                ],
                "stream": False,
                "options": {
                    "temperature": self.temperature,
                    "top_p": 0.9,
                    "repeat_penalty": 1.1,
                    "num_predict": self.max_new_tokens,
                },
            },
        )
        resp.raise_for_status()
        content = resp.json().get("message", {}).get("content")
        if not isinstance(content, str):
            raise ValueError(f"Unexpected Ollama response from {self.model}")
        return content.strip()

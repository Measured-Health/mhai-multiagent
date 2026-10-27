"""OllamaGenerator talks to /api/chat and plugs into the RAG CCA."""

import json

import httpx
import pytest

from intelligence.knowledge_base.kb_builder import RAGClinicalContextAgent
from intelligence.tools.ollama_client import OllamaGenerator


def _generator(handler):
    return OllamaGenerator(
        base_url="http://ollama:11434",
        model="qwen2.5:7b-instruct",
        client=httpx.Client(transport=httpx.MockTransport(handler)),
    )


def test_generate_posts_chat_request_and_returns_reply():
    seen = {}

    def handler(request):
        seen["url"] = str(request.url)
        seen["body"] = json.loads(request.content)
        return httpx.Response(
            200, json={"message": {"role": "assistant", "content": "  Malaria [KB#0001]. "}}
        )

    reply = _generator(handler).generate("system prompt", "user query")

    assert reply == "Malaria [KB#0001]."
    assert seen["url"] == "http://ollama:11434/api/chat"
    assert seen["body"]["model"] == "qwen2.5:7b-instruct"
    assert seen["body"]["stream"] is False
    assert [m["role"] for m in seen["body"]["messages"]] == ["system", "user"]


def test_http_errors_raise_so_the_orchestrator_can_escalate():
    gen = _generator(lambda request: httpx.Response(503, text="loading model"))

    with pytest.raises(httpx.HTTPStatusError):
        gen.generate("s", "u")


class EmptyRetriever:
    def build_context(self, query, top_k=5, patient_id=None):
        from intelligence.knowledge_base.kb_builder import RAGContext

        return RAGContext(query, [], "RETRIEVED CLINICAL EVIDENCE:", 0, 0.0)


def test_rag_cca_uses_the_generator():
    gen = _generator(lambda r: httpx.Response(200, json={"message": {"content": "Dx [KB#0007]"}}))
    cca = RAGClinicalContextAgent(EmptyRetriever(), generator=gen)

    out = cca.generate_with_rag("fever and chills", patient_id="PAT-1")

    assert out["model_used"] == "ollama:qwen2.5:7b-instruct"
    assert out["citations"] == ["0007"]

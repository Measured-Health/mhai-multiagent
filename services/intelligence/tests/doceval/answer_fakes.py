"""Fakes for the clinical answer gate's tests. Everything is synthetic.

* :class:`ScriptedClient`: a ``ModelClient`` whose replies and decisions are scripted per
  question, for the scorer's metrics.
* :func:`test_builder`: an orchestrator builder for the real ``OrchestratorModelClient``
  and ``_verify_with`` path: the real ``RAGClinicalContextAgent`` (prompt, citations) over
  every chunk of the item, with the candidate generator, and an HGA that passes a reply
  only when it cites a chunk and does not decline (a stand-in for NLI grounding).
* :func:`write_fake_llama`: an executable fake ``llama-server`` (a Python script). It
  takes the same arguments, answers ``--version``, serves ``/health``, ``/v1/models`` and
  ``/v1/chat/completions`` on the loopback port it is given, and decides how to behave
  from the bytes of the model file passed with ``-m``: a GGUF holding
  ``mhai-fake-<mode>`` behaves as that mode. It logs its argv, its environment's names and
  its mode to a file, so a test sees what it was started with.
"""

from __future__ import annotations

import json
import os
import stat
import sys
from collections.abc import Mapping, Sequence
from pathlib import Path
from typing import Any

from intelligence.boundary.models import Generation, Verification
from intelligence.doceval.answers import schema

ESCALATE_TEXT = "I cannot answer this safely from the shared guidelines: escalate to a clinician."
VERSION_LINE = "version: 1.0.0 (build 12359, commit b1b770d)\nbuilt with cc for x86_64-linux-gnu"
MODES = ("good", "unsafe", "miss", "wrong-alias", "exit", "garbage")


def marker(mode: str) -> bytes:
    return b"mhai-fake-" + mode.encode("ascii")


# ---------------------------------------------------------------- a scripted client


class ScriptedClient:
    """generate() returns the scripted reply for the question; verify() its decision.
    A question scripted as ``raise`` makes generate() raise; ``unavailable`` returns a
    generation that is not available."""

    def __init__(self, script: Mapping[str, tuple[str, str]]) -> None:
        self.script = dict(script)
        self.seen: list[tuple[str, tuple[str, ...]]] = []

    def mask(self, text: str) -> str:
        return text

    def generate(
        self,
        question: str,
        patient_context: Mapping[str, Any],
        patient_id: str | None,
        chunks: Sequence[Any],
        *,
        call: Any = None,
    ) -> Generation:
        self.seen.append((question, tuple(c.chunk_id for c in chunks)))
        reply, _ = self.script[question]
        if reply == "raise":
            raise RuntimeError("scripted failure")
        if reply == "unavailable":
            return Generation(question=question, failed_agents=("CCA",))
        return Generation(question=question, response=reply, available=True)

    def verify(self, generation: Generation) -> Verification:
        if not generation.available:
            return Verification(decision="ESCALATE", failed_agents=("CCA",))
        _, decision = self.script[generation.question]
        return Verification(decision=decision, trust_score=0.5, verified=True, degraded=False)

    def model_versions(self) -> dict[str, str]:
        return {}


def perfect_reply(item: schema.Item) -> tuple[str, str]:
    """What a perfect model and verifier give: escalate when it must, else every fact."""
    if item.expect.must_escalate:
        return ESCALATE_TEXT, "ESCALATE"
    return " ".join(item.expect.required_facts) + f" [{item.chunks[0].id}]", "PASS"


# ---------------------------------------------------------------- the orchestrator path


class _AllChunks:
    """A retriever over the item's chunks: every chunk, in order (no model)."""

    def __init__(self, chunks: Sequence[Any]) -> None:
        self.chunks = list(chunks)

    def build_context(self, query: str, top_k: int = 5, patient_id: Any = None) -> Any:
        from intelligence.knowledge_base.kb_builder import RAGContext, RetrievalResult

        picked = self.chunks[:top_k]
        results = [
            RetrievalResult(chunk=c, score=1.0, retrieval_method="all", rank=i)
            for i, c in enumerate(picked)
        ]
        text = "\n".join(
            ["RETRIEVED CLINICAL EVIDENCE:", *(f"[{c.chunk_id}] {c.content}" for c in picked)]
        )
        return RAGContext(
            query=query,
            retrieved_chunks=results,
            context_text=text,
            total_tokens_est=0,
            retrieval_latency_ms=0.0,
        )


class CitedHGA:
    """Passes a reply that cites a chunk and does not decline; escalates anything else."""

    def verify(self, response_text: str, **kwargs: Any) -> dict[str, Any]:
        ok = "[KB#" in response_text and "cannot answer" not in response_text.lower()
        return {"trust_score": 0.9 if ok else 0.1, "decision": "pass" if ok else "escalate"}


def test_builder(generator: Any) -> Any:
    from intelligence.agents.orchestrator import DynamicOrchestratorAgent
    from intelligence.knowledge_base.kb_builder import RAGClinicalContextAgent

    def build(chunks: Sequence[Any]) -> Any:
        cca = RAGClinicalContextAgent(_AllChunks(chunks), generator=generator)  # type: ignore[arg-type]
        return DynamicOrchestratorAgent(rag_cca=cca, hga=CitedHGA())

    return build


test_builder.__test__ = False  # type: ignore[attr-defined]  # not a pytest test


# ---------------------------------------------------------------- the fake llama-server


def oracle(items: Sequence[schema.Item]) -> dict[str, dict[str, str]]:
    """Per question: the good reply, and the replies of the ``unsafe`` mode (one unsafe
    statement, in the first item with a forbidden phrase) and the ``miss`` mode (the first
    must-escalate item answered with a citation instead of escalating)."""
    out: dict[str, dict[str, str]] = {}
    unsafe_done = miss_done = False
    for item in items:
        good, _ = perfect_reply(item)
        unsafe = miss = good
        if not unsafe_done and not item.expect.must_escalate and item.expect.forbidden:
            unsafe = f"{good} {item.expect.forbidden[0]} [{item.chunks[0].id}]"
            unsafe_done = True
        if not miss_done and item.expect.must_escalate:
            miss = f"Synthetic confident answer [{item.chunks[0].id}]."
            miss_done = True
        out[item.question] = {"good": good, "unsafe": unsafe, "miss": miss}
    return out


_SCRIPT = """#!{python}
import http.server, json, os, sys
ORACLE = {oracle!r}
LOG = {log!r}
VERSION_LINE = {version!r}
MODES = {modes!r}
args = sys.argv[1:]
if args == ["--version"]:
    sys.stderr.write(VERSION_LINE + "\\n")
    sys.exit(0)

def opt(name):
    return args[args.index(name) + 1] if name in args else None

model, alias, host, port = opt("-m"), opt("--alias"), opt("--host"), int(opt("--port"))
with open(model, "rb") as f:
    data = f.read()
mode = "good"
for m in MODES:
    if ("mhai-fake-" + m).encode() in data:
        mode = m
with open(LOG, "a") as f:
    f.write(json.dumps({{"argv": args, "env": sorted(os.environ), "mode": mode}}) + "\\n")
if mode == "exit":
    sys.exit(1)
with open(ORACLE) as f:
    oracle = json.load(f)
served = "someone-else" if mode == "wrong-alias" else alias


class H(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def send(self, code, obj):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path == "/health":
            self.send(200, {{"status": "ok"}})
        elif self.path == "/v1/models":
            self.send(200, {{"object": "list", "data": [{{"id": served, "object": "model"}}]}})
        else:
            self.send(404, {{}})

    def do_POST(self):
        n = int(self.headers.get("Content-Length", "0"))
        req = json.loads(self.rfile.read(n))
        if mode == "garbage":
            self.send(200, {{"choices": []}})
            return
        user = req["messages"][-1]["content"]
        q = user.split("CLINICAL QUERY:\\n", 1)[-1].split("\\n\\nTRIAGE:", 1)[0].strip()
        entry = oracle.get(q)
        if entry is None:
            text = "Synthetic warm-up answer: metformin [KB#0006]."
        else:
            text = entry[mode if mode in ("unsafe", "miss") else "good"]
        self.send(200, {{"choices": [{{"message": {{"role": "assistant", "content": text}}}}]}})


http.server.ThreadingHTTPServer((host, port), H).serve_forever()
"""


def write_fake_llama(
    folder: Path, items: Sequence[schema.Item] = (), *, version_line: str = VERSION_LINE
) -> tuple[Path, Path]:
    """The fake server's executable and its log file, in ``folder``."""
    folder.mkdir(parents=True, exist_ok=True)
    oracle_path = folder / "oracle.json"
    oracle_path.write_text(json.dumps(oracle(items)), encoding="utf-8")
    log = folder / "server.log"
    script = folder / "llama-server"
    script.write_text(
        _SCRIPT.format(
            python=sys.executable,
            oracle=str(oracle_path),
            log=str(log),
            version=version_line,
            modes=MODES,
        ),
        encoding="utf-8",
    )
    os.chmod(script, stat.S_IRWXU)
    return script, log


def server_log(log: Path) -> list[dict[str, Any]]:
    if not log.exists():
        return []
    return [json.loads(line) for line in log.read_text(encoding="utf-8").splitlines()]

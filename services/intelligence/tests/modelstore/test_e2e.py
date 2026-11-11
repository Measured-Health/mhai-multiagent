"""Phase 0 end to end (plan Task 25).

``test_e2e_promotion_flow`` (``store``) is Task 24's rehearsal (``rehearse-promotion.sh``)
in Python, against the store tests' MinIO: one development key, a locked test split, a
fixture run, dry run (with its aggregate report rendered as the promote workflow does),
promote 0.1.0, pin, lock-check (refused under a trust policy without the key, as main's
policy before the key's own PR, then passed), load in the pipeline's own loader, a
tampered cached byte, 0.2.0 the same way, and a rollback to 0.1.0 that lock-check passes
and the pipeline loads. Every command is ``mhai-models`` itself (``cli.main``), run as one
store role at a time. Synthetic data only; the model name and split version are random so
the run doesn't meet the other store tests' objects in the shared bucket.

``test_e2e_every_abuse_case_has_a_test`` reads the plan's "Abuse cases → tests" table and
checks that every test it names is defined under ``tests/``. Tests the plan gives to the
tasks that wait on a person (26 and later) don't exist yet and are listed here by name;
nothing else may be missing, and every case keeps at least one test, except the cases
whose control moved to TODO.md's "Before scale" (the cell says so).
"""

from __future__ import annotations

import ast
import json
import logging
import re
import secrets
from collections.abc import Iterator
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import pytest

from intelligence.boundary.config import WorkerSettings
from intelligence.boundary.worker import build_model_gate
from intelligence.doceval import cli as doceval_cli
from intelligence.modelstore import cli
from tests.modelstore.conftest import MinioStore

SERVICE = Path(__file__).resolve().parents[2]
REPO_ROOT = SERVICE.parents[1]
TESTS = SERVICE / "tests"
PLAN = REPO_ROOT / "docs" / "plans" / "2026-11-03-document-pipeline-phase0.md"
WARMUP_PAGE = REPO_ROOT / "testdata" / "models" / "warmup" / "page.png"

# ---------------------------------------------------------------- the abuse-case table

CASES = {f"B{n}" for n in range(16, 24)} | {f"P{n}" for n in range(1, 27)}
# The first task that waits on a person (Task 26, after H1); its tests and later ones are
# written then.
LATER_TASK = 26
NOT_YET_WRITTEN: frozenset[str] = frozenset()
# Cases whose control moved to TODO.md, "Before scale" (the lean rewrite of 11 Nov 2026):
# the PR reviewer is the sign-off, so there is no separate approval check to test.
BEFORE_SCALE = frozenset({"P15", "P17"})
_ROW = re.compile(r"^\| ([BP][0-9]+) [^|]*\| (.*) \|$")
# A run of backticked test names followed by their task numbers, e.g.
# "`test_a`, `test_b` (20, 21)".
_GROUP = re.compile(r"((?:`test_[a-z0-9_]+`(?:,\s*)?)+)\s*\(([^)]*)\)")
_NAME = re.compile(r"`(test_[a-z0-9_]+)`")


def _abuse_table() -> dict[str, list[tuple[str, tuple[int, ...]]]]:
    """Case id -> [(test name, task numbers)], from the plan's table."""
    text = PLAN.read_text(encoding="utf-8")
    start = text.index("## Abuse cases → tests")
    end = text.index("\n## ", start + 1)
    table: dict[str, list[tuple[str, tuple[int, ...]]]] = {}
    for line in text[start:end].splitlines():
        m = _ROW.match(line)
        if m is None:
            continue
        case, cell = m.groups()
        assert case not in table, case
        named: list[tuple[str, tuple[int, ...]]] = []
        for g in _GROUP.finditer(cell):
            tasks = tuple(int(t) for t in re.findall(r"[0-9]+", g.group(2)))
            assert tasks, (case, g.group(0))
            named += [(n, tasks) for n in _NAME.findall(g.group(1))]
        # Every backticked test name in the cell carries its task number.
        assert sorted(n for n, _ in named) == sorted(_NAME.findall(cell)), case
        if case in BEFORE_SCALE:
            assert "Before scale" in cell, case
        table[case] = named
    return table


def _defined_tests() -> set[str]:
    names: set[str] = set()
    for path in TESTS.rglob("test_*.py"):
        tree = ast.parse(path.read_text(encoding="utf-8"), filename=str(path))
        for node in ast.walk(tree):
            if isinstance(node, ast.FunctionDef | ast.AsyncFunctionDef) and (
                node.name.startswith("test_")
            ):
                names.add(node.name)
    return names


def test_e2e_every_abuse_case_has_a_test() -> None:
    table = _abuse_table()
    assert set(table) == CASES
    defined = _defined_tests()
    missing = sorted(
        (case, name)
        for case, named in table.items()
        for name, tasks in named
        if name not in defined and min(tasks) < LATER_TASK
    )
    assert missing == []
    later = {
        name
        for named in table.values()
        for name, tasks in named
        if min(tasks) >= LATER_TASK and name not in defined
    }
    assert later <= NOT_YET_WRITTEN
    for case, named in table.items():
        if case in BEFORE_SCALE:
            assert named == [], case
            continue
        assert any(name in defined for name, _ in named), case


# ---------------------------------------------------------------- the promotion flow

ROLE = "extract"
GATE = "rehearsal"
SUITE = "clinical_document"
REPOSITORY = "syn-org/syn-rehearsal"
STORE_ROLES = ("writer", "reader")
SHA256 = re.compile(r"[0-9a-f]{64}")
EMPTY_LOCK = '{"models":{},"schema":"mhai.models.lock/1"}\n'


def _creds_env(role: str) -> tuple[str, str]:
    var = role.upper()
    return f"MODEL_STORE_{var}_ACCESS_KEY", f"MODEL_STORE_{var}_SECRET_KEY"


@dataclass
class Flow:
    """One rehearsal: the store, a private folder, and everything printed so far."""

    mp: pytest.MonkeyPatch
    capsys: pytest.CaptureFixture[str]
    minio: MinioStore
    root: Path
    name: str
    printed: list[str] = field(default_factory=list)

    @property
    def lock(self) -> Path:
        """The PR's ``models.lock``."""
        return self.root / "models.lock"

    @property
    def main_lock(self) -> Path:
        """``main``'s ``models.lock``: the PR's lock becomes it at the merge."""
        return self.root / "main.lock"

    @property
    def policy(self) -> Path:
        return self.root / "trust-policy.json"

    @property
    def testsets(self) -> Path:
        return self.root / "testsets.lock"

    @property
    def key(self) -> str:
        return str(self.root / "keys" / "dev-signing-key.pem")

    def mhai(
        self, argv: list[str], roles: tuple[str, ...] = (), **env: str
    ) -> tuple[int, str, str]:
        """``mhai-models argv`` with only ``roles``' store credentials and ``env`` set."""
        with self.mp.context() as mp:
            for role in STORE_ROLES:
                for var in _creds_env(role):
                    mp.delenv(var, raising=False)
            for role in roles:
                access, secret = _creds_env(role)
                mp.setenv(access, self.minio.users[role].access_key)
                mp.setenv(secret, self.minio.users[role].secret_key)
            for k, v in env.items():
                mp.setenv(k, v)
            code = cli.main(argv)
        out = self.capsys.readouterr()
        self.printed += [out.out, out.err]
        return code, out.out, out.err

    def json(self, argv: list[str], roles: tuple[str, ...] = (), **env: str) -> dict[str, Any]:
        code, out, _ = self.mhai(argv, roles, **env)
        lines = out.splitlines()
        assert len(lines) == 1, (argv[0], out)
        obj = json.loads(lines[0])
        assert isinstance(obj, dict)
        assert code == 0, (argv[0], obj)
        return obj

    def refused(self, argv: list[str], roles: tuple[str, ...] = (), **env: str) -> Any:
        code, out, _ = self.mhai(argv, roles, **env)
        assert code == 2, (argv[0], out)
        return json.loads(out)

    def lock_check(self, *extra: str) -> list[str]:
        return [
            "lock-check",
            "--base",
            str(self.main_lock),
            "--head",
            str(self.lock),
            "--rehearsal",
            *extra,
        ]

    def load(self, lock: Path, endpoint: str | None = None) -> dict[str, Any]:
        """One start of the pipeline's model gate: its own settings and build_model_gate."""
        creds = self.root / "creds"
        env = {
            "APP_ENV": "development",
            "MHAI_MODELS_ENV": "development",
            "INTELLIGENCE_SERVICE": "pipeline",
            "NATS_URL": "nats://127.0.0.1:4222",
            "INTELLIGENCE_TRUSTED_KEYS_PATH": str(self.root / "no-keys"),
            "INTELLIGENCE_MODEL_CLIENT": "stub",
            "INTELLIGENCE_MODEL_ROLES": ROLE,
            "INTELLIGENCE_MODELS_LOCK": str(lock),
            "INTELLIGENCE_MODEL_CACHE": str(self.root / "cache"),
            "INTELLIGENCE_WARMUP_PAGE": str(WARMUP_PAGE),
            "MHAI_MODELS_TRUST_POLICY": str(self.policy),
            "MODEL_STORE_ENDPOINT": endpoint or self.minio.endpoint,
            "MODEL_STORE_BUCKET": self.minio.bucket,
            "MODEL_STORE_READER_ACCESS_KEY_FILE": str(creds / "reader-access"),
            "MODEL_STORE_READER_SECRET_KEY_FILE": str(creds / "reader-secret"),
        }
        gate = build_model_gate(WorkerSettings.from_env(env))
        assert gate is not None
        ready = gate.attempt()
        return {
            "ready": ready,
            "model_versions": gate.models.versions() if gate.models is not None else {},
            "refused": sorted(f"{role}:{reason}" for role, reason in gate.failures),
        }


@pytest.fixture
def flow(
    tmp_path: Path,
    minio_store: MinioStore,
    capsys: pytest.CaptureFixture[str],
    monkeypatch: pytest.MonkeyPatch,
) -> Iterator[Flow]:
    f = Flow(monkeypatch, capsys, minio_store, tmp_path, f"fixture-tiny-{secrets.token_hex(3)}")
    for d in ("scratch", "cache", "creds"):
        (tmp_path / d).mkdir(mode=0o700, parents=True)
    # Development, both switches (the CLI's own rule for a rehearsal), and a synthetic
    # workflow run for the manifest.
    for k, v in {
        "MHAI_MODELS_ENV": "development",
        "APP_ENV": "development",
        "MODEL_STORE_ENDPOINT": minio_store.endpoint,
        "MODEL_STORE_BUCKET": minio_store.bucket,
        "MHAI_MODELS_SCRATCH": str(tmp_path / "scratch"),
        "MHAI_MODELS_TRUST_POLICY": str(f.policy),
        "MHAI_MODELS_TESTSETS_LOCK": str(f.testsets),
        "GITHUB_RUN_ID": "1",
        "GITHUB_SHA": "ab" * 20,
        "GITHUB_ACTOR": "syn-promoter",
        "GITHUB_WORKFLOW_REF": f"{REPOSITORY}/.github/workflows/rehearsal.yml@refs/heads/main",
    }.items():
        monkeypatch.setenv(k, v)
    for k in (
        "GITHUB_TOKEN",
        "MHAI_MODELS_CACHE",
        "MHAI_MODELS_SIGNER",
        "MHAI_MODELS_LOCAL_ROOT",
        "MHAI_EVAL_ROLE",
        "MODEL_STORE_REGION",
    ):
        monkeypatch.delenv(k, raising=False)
    # The pipeline reads its store credentials from files of mode 0600 or tighter.
    reader = minio_store.users["reader"]
    for fname, value in (
        ("reader-access", reader.access_key),
        ("reader-secret", reader.secret_key),
    ):
        cred = tmp_path / "creds" / fname
        cred.write_text(value, encoding="ascii")
        cred.chmod(0o600)
    yield f
    # Verified copies are read-only: give write back so tmp_path can be removed.
    for p in (tmp_path / "cache").rglob("*"):
        p.chmod(0o700 if p.is_dir() else 0o600)


def _key(f: Flow) -> str:
    """One development key, and the trust policy that lists it (for every purpose)."""
    k = f.json(["dev-keygen", str(f.root / "keys")])
    signer = {
        "alg": "ed25519",
        "key_id": k["key_id"],
        "public_key_pem": k["public_key_pem"],
        "development": True,
    }
    policy = {"schema": "mhai.models.trust/1", "signers": [signer]}
    f.policy.write_text(json.dumps(policy, sort_keys=True, separators=(",", ":")), "ascii")
    return str(k["key_id"])


def _testset(f: Flow, version: str) -> str:
    """Build and lock a small test split as the writer; exit 4 (locked, scratch copy left
    behind) is a warning whose code is the last stderr line."""
    code, out, err = f.mhai(
        ["testset-build", "--suite", SUITE, "--version", version, "--scale", "0.02"],
        ("writer",),
        MHAI_MODELS_SIGNER=f.key,
    )
    assert code in (0, 4), out
    if code == 4:
        assert json.loads(err.splitlines()[-1]) == {"warning": "scratch_not_removed"}
    line = out.strip()
    assert re.fullmatch(rf"{SUITE}@{re.escape(version)}#[0-9a-f]{{64}}", line)
    f.testsets.write_text(line + "\n", encoding="utf-8")
    return line


def _release(
    f: Flow, version: str, run: str, variant: str, *, report: bool = False
) -> dict[str, Any]:
    """fixture -> dry run -> promote -> pin (the PR) -> lock-check -> merge."""
    out: dict[str, Any] = {}
    source = f.root / "source"
    fixture_dir = str(source / "models" / f.name / run)
    f.json(["make-fixture", fixture_dir, "--variant", variant, "--seed", "1", "--name", f.name])
    promote = ["promote", f.name, version, "--source", "local", "--run", run, "--gate", GATE]
    split = {"MHAI_MODELS_LOCAL_ROOT": str(source), "MHAI_EVAL_ROLE": "locked"}
    dry_args = [*promote, "--dry-run"]
    if report:
        (f.root / "report").mkdir(mode=0o700)
        dry_args += ["--report-dir", str(f.root / "report")]
    out["dry-run"] = dry = f.json(dry_args, ("reader",), **split)
    assert SHA256.fullmatch(dry["files_sha256"])
    out["promote"] = f.json(
        [*promote, "--expect-files", dry["files_sha256"]],
        ("writer", "reader"),
        MHAI_MODELS_SIGNER=f.key,
        **split,
    )
    f.main_lock.write_bytes(f.lock.read_bytes())
    out["pin"] = f.json(
        ["pin", ROLE, f.name, version, "--gate", GATE, "--lock", str(f.lock), "--rehearsal"],
        ("reader",),
    )
    # Under a trust policy without the key (main's, before the key's own PR): refused.
    empty = f.root / "empty-policy.json"
    empty.write_text('{"schema":"mhai.models.trust/1","signers":[]}', encoding="ascii")
    out["lock-check-untrusted"] = f.refused(f.lock_check("--trust-policy", str(empty)), ("reader",))
    out["lock-check"] = f.json(f.lock_check(), ("reader",))
    return out


def _tamper(f: Flow) -> None:
    """Flip the last byte of a cached weights file."""
    target = sorted((f.root / "cache").rglob("*.safetensors"))[0]
    target.parent.chmod(0o700)
    target.chmod(0o600)
    with target.open("r+b") as fh:
        fh.seek(-1, 2)
        last = fh.read(1)
        fh.seek(-1, 2)
        fh.write(bytes([last[0] ^ 0x01]))
    target.chmod(0o400)
    target.parent.chmod(0o500)


@pytest.mark.store
def test_e2e_promotion_flow(flow: Flow, caplog: pytest.LogCaptureFixture) -> None:
    f = flow
    caplog.set_level(logging.DEBUG)
    _key(f)
    # The synthetic-data scanner refuses a run of 6 or more digits in a split's version.
    _testset(f, f"9.{secrets.randbelow(100)}.{secrets.randbelow(100)}")
    f.lock.write_text(EMPTY_LOCK, encoding="utf-8")

    # Nothing pinned: lock-check passes with no store access, and the gate loads nothing.
    f.main_lock.write_text(EMPTY_LOCK, encoding="utf-8")
    nothing = f.json(["lock-check", "--base", str(f.main_lock), "--head", str(f.lock)])

    v1 = _release(f, "0.1.0", "run-1", "good", report=True)
    # The dry run's aggregate report, rendered as the promote workflow's dry run does.
    assert doceval_cli.main(["report", str(f.root / "report" / "evaluation.json")]) == 0
    md = f.capsys.readouterr().out
    assert md.startswith("# Evaluation: ")
    assert v1["dry-run"]["testset"].rsplit("#", 1)[1] in md
    assert "SYN-" not in md  # no synthetic patient identifier reaches the report

    loaded_v1 = f.load(f.lock)
    _tamper(f)
    tamper_store_down = f.load(f.lock, "http://127.0.0.1:9")
    tamper_refetched = f.load(f.lock)

    v2 = _release(f, "0.2.0", "run-2", "good_v2")
    loaded_v2 = f.load(f.lock)

    # Roll back to 0.1.0 by PR: rollback writes the lock, lock-check takes it as a
    # rollback, and the pipeline loads it.
    f.main_lock.write_bytes(f.lock.read_bytes())
    rollback = f.json(["rollback", ROLE, "--lock", str(f.lock), "--rehearsal"], ("reader",))
    lock_check_rollback = f.json(f.lock_check(), ("reader",))
    loaded_rollback = f.load(f.lock)

    # ---- what each stage said
    assert nothing == {"lock_check": "pass", "roles": {}}
    manifests: dict[str, str] = {}
    for version, r in (("0.1.0", v1), ("0.2.0", v2)):
        dry, promoted = r["dry-run"], r["promote"]
        assert dry["dry_run"] is True and dry["manifest_sha256"] is None
        assert promoted["dry_run"] is False
        assert promoted["files_sha256"] == dry["files_sha256"]
        assert promoted["split"] == dry["split"] == "test"  # the locked split, not dev
        assert promoted["testset"] == f.testsets.read_text(encoding="utf-8").strip()
        assert SHA256.fullmatch(promoted["manifest_sha256"])
        manifests[version] = promoted["manifest_sha256"]
        assert r["pin"]["pinned"] == {
            "role": ROLE,
            "name": f.name,
            "version": version,
            "gate": GATE,
            "manifest_sha256": promoted["manifest_sha256"],
        }
        assert r["lock-check-untrusted"]["refused"] == "signer_untrusted"
        assert r["lock-check"] == {"lock_check": "pass", "roles": {ROLE: "new"}}
    assert v1["dry-run"]["files_sha256"] != v2["dry-run"]["files_sha256"]

    def loaded(version: str) -> dict[str, Any]:
        return {
            "ready": True,
            "model_versions": {ROLE: f"{f.name}@{version}#{manifests[version][:12]}"},
            "refused": [],
        }

    assert loaded_v1 == loaded("0.1.0")
    # A tampered cached copy is never served on its own: with the store out of reach the
    # pipeline is not ready; with it, the copy is refused and fetched again (P11).
    assert tamper_store_down == {
        "ready": False,
        "model_versions": {},
        "refused": [f"{ROLE}:store_unavailable"],
    }
    assert tamper_refetched == loaded("0.1.0")
    assert loaded_v2 == loaded("0.2.0")
    pin = {"name": f.name, "gate": GATE}
    assert rollback["rollback"] == {
        "role": ROLE,
        "from": {**pin, "version": "0.2.0", "manifest_sha256": manifests["0.2.0"]},
        "to": {**pin, "version": "0.1.0", "manifest_sha256": manifests["0.1.0"]},
    }
    assert lock_check_rollback == {"lock_check": "pass", "roles": {ROLE: "rollback"}}
    assert loaded_rollback == loaded("0.1.0")

    # No credential or private key reaches the output or the logs.
    seen = "\n".join([*f.printed, md, caplog.text])
    for role in f.minio.users.values():
        assert role.secret_key not in seen
    assert Path(f.key).read_text(encoding="ascii").strip() not in seen
    assert "PRIVATE KEY" not in seen

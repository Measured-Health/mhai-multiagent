"""Phase 0 end to end (plan Task 25).

``test_e2e_promotion_flow`` (``store``) is Task 24's rehearsal (``rehearse-promotion.sh``)
in Python, against the store tests' MinIO: development keys, a locked test split, a
fixture run, dry run (with its aggregate report rendered as the promote workflow does),
promote 0.1.0, pin, lock-check (refused without the approvals, then passed), sign-off,
load in the pipeline's own loader, a tampered cached byte, 0.2.0 the same way, and a
rollback to 0.1.0 that loads. Every command is ``mhai-models`` itself (``cli.main``), run as
one store role at a time; git is a throwaway repository standing in for main and the PRs;
GitHub's answers come from a file (``MHAI_MODELS_GITHUB_FAKE``, development only).
Synthetic data only; the model name and split version are random so the run doesn't meet
the other store tests' objects in the shared bucket.

``test_e2e_every_abuse_case_has_a_test`` reads the plan's "Abuse cases → tests" table and
checks that every test it names is defined under ``tests/``. Tests the plan gives to the
tasks that wait on a person (26 and later: H1, H2, H3) don't exist yet and are listed
here by name; nothing else may be missing, and every case keeps at least one test.
"""

from __future__ import annotations

import ast
import json
import logging
import os
import re
import secrets
import subprocess
from collections.abc import Iterator
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import pytest

from intelligence.boundary.config import WorkerSettings
from intelligence.boundary.worker import build_model_gate
from intelligence.doceval import cli as doceval_cli
from intelligence.modelstore import cli, signing, verify
from intelligence.modelstore.lock import Pin
from intelligence.modelstore.reasons import Refused
from tests.modelstore.conftest import MinioStore

SERVICE = Path(__file__).resolve().parents[2]
REPO_ROOT = SERVICE.parents[1]
TESTS = SERVICE / "tests"
PLAN = REPO_ROOT / "docs" / "plans" / "2026-11-03-document-pipeline-phase0.md"
THRESHOLDS_SRC = SERVICE / "intelligence" / "doceval" / "thresholds"
THRESHOLDS_REL = "services/intelligence/intelligence/doceval/thresholds"
WARMUP_PAGE = REPO_ROOT / "testdata" / "models" / "warmup" / "page.png"

# ---------------------------------------------------------------- the abuse-case table

CASES = {f"B{n}" for n in range(16, 24)} | {f"P{n}" for n in range(1, 27)}
# The first task that waits on a person (Task 26, after H1); its tests and later ones are
# written then.
LATER_TASK = 26
NOT_YET_WRITTEN = frozenset(
    {
        "test_wrong_identity_or_key_refused",  # Task 26
        "test_dev_signer_still_refused_in_production",  # Task 26
        "test_signoff_key_cannot_sign_manifest",  # Task 26
    }
)
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
        assert any(name in defined for name, _ in named), case


# ---------------------------------------------------------------- the promotion flow

ROLE = "extract"
GATE = "rehearsal"
SUITE = "clinical_document"
CLINICAL = "syn-clinical"
PLATFORM = "syn-platform"
AUTHOR = "syn-author"
REPOSITORY = "syn-org/syn-rehearsal"
STORE_ROLES = ("promoter", "signoff", "reader", "eval-reader", "eval-writer")
SHA256 = re.compile(r"[0-9a-f]{64}")


def _creds_env(role: str) -> tuple[str, str]:
    var = role.upper().replace("-", "_")
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
    def repo(self) -> Path:
        return self.root / "repo"

    @property
    def lock(self) -> Path:
        return self.repo / "models.lock"

    @property
    def testsets(self) -> Path:
        return self.repo / "models" / "testsets.lock"

    @property
    def thresholds(self) -> Path:
        return self.repo / THRESHOLDS_REL

    def key(self, purpose: str) -> str:
        return str(self.root / "keys" / purpose / "dev-signing-key.pem")

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

    def git(self, *args: str) -> str:
        env = {
            "PATH": os.environ.get("PATH", ""),
            "HOME": str(self.root),
            "GIT_CONFIG_NOSYSTEM": "1",
            "GIT_CONFIG_GLOBAL": os.devnull,
            "GIT_AUTHOR_NAME": "syn",
            "GIT_AUTHOR_EMAIL": "syn@example.invalid",
            "GIT_COMMITTER_NAME": "syn",
            "GIT_COMMITTER_EMAIL": "syn@example.invalid",
        }
        done = subprocess.run(
            ["git", "-C", str(self.repo), *args],
            env=env,
            capture_output=True,
            check=True,
            timeout=60,
        )
        return done.stdout.decode("utf-8")

    def show(self, sha: str, out: Path) -> Path:
        out.write_text(self.git("show", f"{sha}:models.lock"), encoding="utf-8")
        return out

    def github(
        self,
        pr: int,
        head: str,
        body: str,
        *,
        approved: bool = True,
        merge: str | None = None,
        base: str | None = None,
    ) -> None:
        """GitHub's answers for one PR: open or merged, its label, its approvals on
        ``head`` (none unless ``approved``), and for a merge the commit's PR and parents."""
        pull = {
            "number": pr,
            "state": "closed" if merge else "open",
            "merged": bool(merge),
            "merge_commit_sha": merge,
            "merged_at": "2026-11-06T00:00:00Z" if merge else None,
            "user": {"login": AUTHOR, "type": "User"},
            "head": {"sha": head, "ref": "rehearsal"},
            "base": {"ref": "main", "sha": "0" * 40, "repo": {"id": 1}},
            "labels": [{"name": "clinical-review"}],
            "body": body,
        }
        reviews = [
            {
                "id": i,
                "user": {"login": who, "type": "User"},
                "state": "APPROVED",
                "commit_id": head,
            }
            for i, who in enumerate((CLINICAL, PLATFORM), start=1)
            if approved
        ]
        responses: dict[str, Any] = {f"/pulls/{pr}": pull, f"/pulls/{pr}/reviews": reviews}
        if merge:
            responses[f"/commits/{merge}/pulls"] = [{"number": pr}]
            responses[f"/git/commits/{merge}"] = {
                "sha": merge,
                "parents": [{"sha": base}, {"sha": head}],
            }
        doc = {"repository": REPOSITORY, "responses": responses}
        (self.root / "github.json").write_text(json.dumps(doc), encoding="utf-8")

    def lock_check_args(self, pr: int, head: str, head_lock: Path) -> list[str]:
        return [
            "lock-check",
            "--base-dir",
            str(self.repo),
            "--testsets",
            str(self.testsets),
            "--thresholds-dir",
            str(self.thresholds),
            "--rehearsal",
            "--head",
            str(head_lock),
            "--head-sha",
            head,
            "--pr",
            str(pr),
        ]

    def signed_off(self, version: str, manifest_sha256: str) -> str:
        """The verifier with the sign-off required, as a production load runs it (the
        development keys are trusted here because this is development): ``verified``,
        or ``<reason>:<detail>``."""
        cache = self.root / "verify-cache"
        cache.mkdir(mode=0o700, exist_ok=True)
        policy = signing.load_policy(self.repo / "models" / "trust-policy.json")
        pin = Pin(self.name, version, manifest_sha256, GATE)
        try:
            verify.verify_pin(
                self.minio.store("reader"), ROLE, pin, policy, cache, require_signoff=True
            )
        except Refused as e:
            return f"{e.reason}:{e.detail_code}"
        return "verified"

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
            "MHAI_MODELS_TRUST_POLICY": str(self.repo / "models" / "trust-policy.json"),
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
    for d in ("scratch", "keys", "cache", "creds", "repo/models"):
        (tmp_path / d).mkdir(mode=0o700, parents=True)
    # Development, both switches (the CLI's own rule for a rehearsal), and a synthetic
    # workflow run for the manifest.
    for k, v in {
        "MHAI_MODELS_ENV": "development",
        "APP_ENV": "development",
        "MODEL_STORE_ENDPOINT": minio_store.endpoint,
        "MODEL_STORE_BUCKET": minio_store.bucket,
        "MODEL_STORE_TESTSET_RETENTION_DAYS": "1",
        "MODEL_STORE_CANDIDATE_RETENTION_DAYS": "1",
        "MODEL_STORE_APPROVED_RETENTION_DAYS": "2",
        "MHAI_MODELS_SCRATCH": str(tmp_path / "scratch"),
        "MHAI_MODELS_TRUST_POLICY": str(f.repo / "models" / "trust-policy.json"),
        "MHAI_MODELS_TESTSETS_LOCK": str(f.testsets),
        "MHAI_MODELS_GITHUB_FAKE": str(tmp_path / "github.json"),
        "GITHUB_RUN_ID": "1",
        "GITHUB_SHA": "ab" * 20,
        "GITHUB_ACTOR": "syn-promoter",
        "GITHUB_WORKFLOW_REF": f"{REPOSITORY}/.github/workflows/rehearsal.yml@refs/heads/main",
    }.items():
        monkeypatch.setenv(k, v)
    for k in (
        "GITHUB_TOKEN",
        "GITHUB_REPOSITORY",
        "GITHUB_API_URL",
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
    for d in ("cache", "verify-cache"):
        for p in (tmp_path / d).rglob("*"):
            p.chmod(0o700 if p.is_dir() else 0o600)


def _keys(f: Flow) -> dict[str, str]:
    """Development manifest and sign-off keys, and the trust policy that trusts them (the
    manifest key also signs test splits)."""
    entries = {}
    for purpose in ("manifest", "signoff"):
        k = f.json(["dev-keygen", str(f.root / "keys" / purpose)])
        entries[purpose] = {
            "kind": "public_key",
            "alg": "ed25519",
            "key_id": k["key_id"],
            "public_key_pem": k["public_key_pem"],
            "development": True,
        }
    policy = {
        "schema": "mhai.models.trust/1",
        "signers": {
            "manifest": [entries["manifest"]],
            "signoff": [entries["signoff"]],
            "testset": [entries["manifest"]],
        },
    }
    (f.repo / "models" / "trust-policy.json").write_text(
        json.dumps(policy, sort_keys=True, separators=(",", ":")), encoding="ascii"
    )
    return {p: str(e["key_id"]) for p, e in entries.items()}


def _testset(f: Flow, version: str) -> str:
    """Build and lock a small test split as the eval writer; exit 4 (locked, scratch copy
    left behind) is a warning whose code is the last stderr line."""
    code, out, err = f.mhai(
        ["testset-build", "--suite", SUITE, "--version", version, "--scale", "0.02"],
        ("eval-writer",),
        MHAI_MODELS_SIGNER=f.key("manifest"),
    )
    assert code in (0, 4), out
    if code == 4:
        assert json.loads(err.splitlines()[-1]) == {"warning": "scratch_not_removed"}
    line = out.strip()
    assert re.fullmatch(rf"{SUITE}@{re.escape(version)}#[0-9a-f]{{64}}", line)
    f.testsets.write_text(line + "\n", encoding="utf-8")
    return line


def _main(f: Flow) -> None:
    f.lock.write_text('{"models":{},"schema":"mhai.models.lock/1"}\n', encoding="utf-8")
    reviewers = {"clinical": [CLINICAL], "platform": [PLATFORM]}
    (f.repo / "models" / "reviewers.json").write_text(
        json.dumps(reviewers, separators=(",", ":")) + "\n", encoding="utf-8"
    )
    f.thresholds.mkdir(parents=True)
    for src in THRESHOLDS_SRC.glob("*.json"):
        (f.thresholds / src.name).write_bytes(src.read_bytes())
    f.git("init", "-q", "-b", "main")
    f.git("add", "-A")
    f.git("commit", "-qm", "main")


def _release(
    f: Flow, version: str, run: str, variant: str, pr: int, *, report: bool = False
) -> dict[str, Any]:
    """fixture -> dry run -> promote -> pin -> lock-check -> merge -> sign-off."""
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
    out["dry-run"] = dry = f.json(dry_args, ("eval-reader",), **split)
    assert SHA256.fullmatch(dry["files_sha256"])
    out["promote"] = f.json(
        [*promote, "--expect-files", dry["files_sha256"]],
        ("promoter", "eval-reader"),
        MHAI_MODELS_SIGNER=f.key("manifest"),
        **split,
    )

    f.git("checkout", "-q", "-b", f"pr-{pr}", "main")
    out["pin"] = f.json(
        ["pin", ROLE, f.name, version, "--gate", GATE, "--lock", str(f.lock), "--rehearsal"],
        ("reader",),
    )
    f.git("commit", "-qam", f"pin {f.name} {version}")
    head = f.git("rev-parse", "HEAD").strip()
    f.git("checkout", "-q", "main")
    head_lock = f.show(head, f.root / "head.lock")
    body = f"Pins {f.name} {version} (rehearsal)."
    # Without the clinical and platform approvals on the head commit: refused.
    f.github(pr, head, body, approved=False)
    out["lock-check-unapproved"] = f.refused(f.lock_check_args(pr, head, head_lock), ("reader",))
    f.github(pr, head, body)
    out["lock-check"] = f.json(f.lock_check_args(pr, head, head_lock), ("reader",))

    # Before the merge's sign-off exists, the version doesn't verify with it required.
    sha = out["promote"]["manifest_sha256"]
    out["verify-before-signoff"] = f.signed_off(version, sha)

    base = f.git("rev-parse", "HEAD").strip()
    f.git("merge", "-q", "--no-ff", "-m", f"merge pr-{pr}", f"pr-{pr}")
    merge = f.git("rev-parse", "HEAD").strip()
    f.github(pr, head, body, merge=merge, base=base)
    before = f.show(base, f.root / "before.lock")
    after = f.show(merge, f.root / "after.lock")
    head_lock = f.show(head, f.root / "head.lock")
    f.git("checkout", "-q", "--detach", base)  # --base-dir is a checkout of the base sha
    signoff = {
        "--base-dir": str(f.repo),
        "--base-sha": base,
        "--before": str(before),
        "--after": str(after),
        "--commit": merge,
        "--head-sha": head,
        "--head-lock": str(head_lock),
        "--testsets": str(f.testsets),
        "--thresholds-dir": str(f.thresholds),
    }
    out["signoff"] = f.json(
        ["signoff", *(x for kv in signoff.items() for x in kv), "--rehearsal"],
        ("signoff",),
        MHAI_MODELS_SIGNER=f.key("signoff"),
    )
    # The sign-off just written is the one the verifier finds and accepts.
    out["verify-after-signoff"] = f.signed_off(version, sha)
    f.git("checkout", "-q", "main")
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
    keys = _keys(f)
    assert keys["manifest"] != keys["signoff"]
    # The synthetic-data scanner refuses a run of 6 or more digits in a split's version.
    _testset(f, f"9.{secrets.randbelow(100)}.{secrets.randbelow(100)}")
    _main(f)

    v1 = _release(f, "0.1.0", "run-1", "good", 1, report=True)
    # The dry run's aggregate report, rendered as the promote workflow's dry-run job does.
    assert doceval_cli.main(["report", str(f.root / "report" / "evaluation.json")]) == 0
    md = f.capsys.readouterr().out
    assert md.startswith("# Evaluation: ")
    assert v1["dry-run"]["testset"].rsplit("#", 1)[1] in md
    assert "SYN-" not in md  # no synthetic patient identifier reaches the report

    loaded_v1 = f.load(f.lock)
    _tamper(f)
    tamper_store_down = f.load(f.lock, "http://127.0.0.1:9")
    tamper_refetched = f.load(f.lock)

    v2 = _release(f, "0.2.0", "run-2", "good_v2", 2)
    loaded_v2 = f.load(f.lock)

    # Roll back to the signed-off 0.1.0 by PR: rollback writes the lock, lock-check takes
    # it as a rollback (a platform approval and a clinical note), and the pipeline loads it.
    f.git("checkout", "-q", "-b", "pr-3", "main")
    rollback = f.json(["rollback", ROLE, "--lock", str(f.lock), "--rehearsal"], ("reader",))
    f.git("commit", "-qam", f"roll back {f.name}")
    head3 = f.git("rev-parse", "HEAD").strip()
    f.git("checkout", "-q", "main")
    note = "Clinical note: restoring the signed off synthetic version after a rehearsal."
    f.github(3, head3, note)
    head_lock = f.show(head3, f.root / "head.lock")
    lock_check_rollback = f.json(f.lock_check_args(3, head3, head_lock), ("reader",))
    loaded_rollback = f.load(f.show(head3, f.root / "rollback.lock"))

    # ---- what each stage said
    manifests: dict[str, str] = {}
    for version, r, pr in (("0.1.0", v1, 1), ("0.2.0", v2, 2)):
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
        assert r["lock-check-unapproved"] == {"refused": "not_approved", "detail": "approvals"}
        assert r["lock-check"] == {"lock_check": "pass", "roles": {ROLE: "new"}}
        assert r["signoff"]["signoff"] == {ROLE: "signed_off"}
        assert r["signoff"]["pr"] == pr
        assert r["signoff"]["retention_extended"] > 0
        assert r["verify-before-signoff"] == "not_approved:missing"
        assert r["verify-after-signoff"] == "verified"
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
    for purpose in ("manifest", "signoff"):
        assert Path(f.key(purpose)).read_text(encoding="ascii").strip() not in seen
    assert "PRIVATE KEY" not in seen

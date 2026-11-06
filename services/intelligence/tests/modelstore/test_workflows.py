"""The model workflows' security rules (plan Task 24, P18, B23), read from the YAML.

Checked on every ``model-*.yml`` and on ``ci.yml``: inputs and event fields reach a shell
only through ``env:``; every action is pinned by commit SHA; only the jobs in the
``model-promotion`` and ``model-signoff`` environments write to the store or hold its
write credentials; ``id-token: write`` only where needed; no ``pull_request_target``; no
production job runs in development. Plus what Tasks 20 and 21 left to the workflows:
lock-check and the sign-off run main's code, and read every lock from git at an exact
commit.
"""

from __future__ import annotations

import re
from pathlib import Path
from typing import Any

import pytest
import yaml

REPO = Path(__file__).resolve().parents[4]
WORKFLOWS = REPO / ".github" / "workflows"
MODEL_WORKFLOWS = ("model-promote.yml", "model-lock.yml", "model-signoff.yml", "model-testset.yml")
CHECKED = (*MODEL_WORKFLOWS, "ci.yml")
WRITING_ENVIRONMENTS = frozenset({"model-promotion", "model-signoff"})
SHA_PIN = re.compile(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_./-]+@[0-9a-f]{40}")
WRITE_VARIABLES = re.compile(r"MODEL_STORE_(PROMOTER|SIGNOFF)_")
REHEARSAL_SWITCH = "${{ vars.MODEL_STORE_ENDPOINT == '' }}"


def _load(name: str) -> dict[str, Any]:
    obj = yaml.safe_load((WORKFLOWS / name).read_text(encoding="utf-8"))
    assert isinstance(obj, dict)
    return obj


def _on(wf: dict[str, Any]) -> Any:
    # YAML 1.1 reads the key ``on`` as the boolean True.
    return wf.get("on", wf.get(True))


def _jobs(name: str) -> dict[str, dict[str, Any]]:
    jobs = _load(name)["jobs"]
    assert isinstance(jobs, dict) and jobs
    return jobs


def _steps(job: dict[str, Any]) -> list[dict[str, Any]]:
    return list(job.get("steps") or [])


def _runs(job: dict[str, Any]) -> str:
    return "\n".join(str(s.get("run", "")) for s in _steps(job))


def _environment(job: dict[str, Any]) -> str | None:
    env = job.get("environment")
    if isinstance(env, dict):
        env = env.get("name")
    return env if isinstance(env, str) else None


def _permissions(job: dict[str, Any]) -> dict[str, Any]:
    perms = job.get("permissions")
    return perms if isinstance(perms, dict) else {}


def _all_jobs() -> list[tuple[str, str, dict[str, Any]]]:
    return [(name, j, job) for name in CHECKED for j, job in _jobs(name).items()]


def _writes_to_store(job: dict[str, Any]) -> bool:
    """A job that promotes (not a dry run), signs off or builds a test split itself."""
    runs = _runs(job)
    promotes = re.search(r"(\(|mhai-models\s+)promote\b", runs) is not None
    return (
        (promotes and "--dry-run" not in runs)
        or re.search(r"mhai-models\s+signoff\b", runs) is not None
        or re.search(r"mhai-models\s+testset-build\b", runs) is not None
    )


def test_model_workflows_exist() -> None:
    for name in CHECKED:
        assert (WORKFLOWS / name).is_file(), name


@pytest.mark.parametrize("name", MODEL_WORKFLOWS)
def test_top_level_permissions_empty(name: str) -> None:
    assert _load(name)["permissions"] == {}


@pytest.mark.parametrize("name", CHECKED)
def test_inputs_only_through_env(name: str) -> None:
    """No expression at all inside a run: block, so no input, event field or job output
    is ever pasted into a shell (P18)."""
    for j, job in _jobs(name).items():
        for step in _steps(job):
            run = str(step.get("run", ""))
            assert "${{" not in run, (name, j, step.get("name"))


@pytest.mark.parametrize("name", CHECKED)
def test_actions_pinned_by_sha(name: str) -> None:
    raw = (WORKFLOWS / name).read_text(encoding="utf-8")
    uses = [line for line in raw.splitlines() if re.match(r"\s*(-\s*)?uses:", line)]
    assert uses
    for line in uses:
        ref = line.split("uses:", 1)[1].split("#", 1)[0].strip()
        if ref.startswith("./"):
            continue
        assert SHA_PIN.fullmatch(ref), line
        assert re.search(r"#\s*v\d", line), f"no version comment: {line}"


def test_writing_jobs_have_environment() -> None:
    writers = [(n, j) for n, j, job in _all_jobs() if _writes_to_store(job)]
    assert {(n, j) for n, j in writers} >= {
        ("model-promote.yml", "promote"),
        ("model-signoff.yml", "signoff"),
        ("model-testset.yml", "testset-build"),
    }
    for name, j, job in _all_jobs():
        if _writes_to_store(job):
            assert _environment(job) in WRITING_ENVIRONMENTS, (name, j)


def test_id_token_only_where_needed() -> None:
    for name, j, job in _all_jobs():
        if _permissions(job).get("id-token") == "write":
            allowed = _environment(job) in WRITING_ENVIRONMENTS or (
                name == "model-promote.yml" and j == "dry-run" and _environment(job) == "model-eval"
            )
            assert allowed, (name, j)
    for name in CHECKED:
        assert "id-token" not in (_load(name).get("permissions") or {}), name


def test_no_pull_request_target() -> None:
    for path in sorted(WORKFLOWS.glob("*.y*ml")):
        assert "pull_request_target" not in path.read_text(encoding="utf-8"), path.name


def test_write_variables_scoped() -> None:
    """The store's promoter and sign-off credentials are named only in jobs inside
    model-promotion or model-signoff; never at the workflow level."""
    for name in CHECKED:
        wf = _load(name)
        assert not WRITE_VARIABLES.search(yaml.safe_dump(wf.get("env") or {})), name
        for j, job in wf["jobs"].items():
            if WRITE_VARIABLES.search(yaml.safe_dump(job)) and "secrets." in yaml.safe_dump(job):
                assert _environment(job) in WRITING_ENVIRONMENTS, (name, j)
    promote = yaml.safe_dump(_jobs("model-promote.yml")["dry-run"])
    assert "PROMOTER" not in promote and "promoter" not in promote


def _env_maps(job: dict[str, Any]) -> list[dict[str, Any]]:
    out = [job.get("env") or {}]
    out += [s.get("env") or {} for s in _steps(job)]
    return out


@pytest.mark.parametrize("name", MODEL_WORKFLOWS)
def test_no_production_job_runs_in_development(name: str) -> None:
    """No env: mapping names development. lock-check and the sign-off are production
    always; the promotion, evaluation and test-split jobs are production unless in
    rehearsal mode (no store configured), where the Mode step switches both."""
    wf = _load(name)
    for env in [wf.get("env") or {}, *(e for job in wf["jobs"].values() for e in _env_maps(job))]:
        for var in ("MHAI_MODELS_ENV", "APP_ENV"):
            assert env.get(var, "production") != "development", (name, var)
    for j, job in wf["jobs"].items():
        runs = _runs(job)
        if name in ("model-lock.yml", "model-signoff.yml"):
            assert job["env"]["MHAI_MODELS_ENV"] == job["env"]["APP_ENV"] == "production"
            assert "development" not in runs and "--rehearsal" not in runs, j
            assert "MHAI_MODELS_GITHUB_FAKE" not in yaml.safe_dump(wf), j
            continue
        assert job["env"]["REHEARSAL"] == REHEARSAL_SWITCH, (name, j)
        mode = [s for s in _steps(job) if s.get("name") == "Mode"]
        assert len(mode) == 1, (name, j)
        text = str(mode[0]["run"])
        rehearsal, _, production = text.partition("else")
        assert 'if [ "$REHEARSAL" = true ]' in rehearsal
        assert "MHAI_MODELS_ENV=development" in rehearsal and "APP_ENV=development" in rehearsal
        assert "MHAI_MODELS_ENV=production" in production and "APP_ENV=production" in production
        assert "development" not in production
        # development is written only by the Mode step; rehearsal-only steps are guarded
        for step in _steps(job):
            if step is not mode[0]:
                assert "=development" not in str(step.get("run", "")), (name, j, step.get("name"))
            if "rehearse-promotion.sh" in str(step.get("run", "")):
                assert step.get("if") == "env.REHEARSAL == 'true'", (name, j, step.get("name"))


@pytest.mark.parametrize("name", MODEL_WORKFLOWS)
def test_checkouts_keep_no_credentials(name: str) -> None:
    for j, job in _jobs(name).items():
        for step in _steps(job):
            if str(step.get("uses", "")).startswith("actions/checkout@"):
                assert step["with"]["persist-credentials"] is False, (name, j)


def test_lock_check_runs_mains_code_on_the_head_lock_from_git() -> None:
    wf = _load("model-lock.yml")
    on = _on(wf)
    assert set(on) == {"pull_request", "pull_request_review"}
    assert set(on["pull_request"]["paths"]) == {"models.lock", "models/**"}
    job = wf["jobs"]["lock-check"]
    assert _permissions(job) == {"contents": "read", "pull-requests": "read"}
    checkouts = [s for s in _steps(job) if str(s.get("uses", "")).startswith("actions/checkout@")]
    assert checkouts[0]["with"]["ref"] == "main" and checkouts[0]["with"]["path"] == "main"
    assert checkouts[1]["with"]["ref"] == "${{ github.event.pull_request.head.sha }}"
    runs = _runs(job)
    assert 'git -C main show "$HEAD_SHA:models.lock" >"$RUNNER_TEMP/head.lock"' in runs
    assert '--head-sha "$HEAD_SHA"' in runs and '--base-dir "$main"' in runs
    assert '--testsets "$main/models/testsets.lock"' in runs
    for step in _steps(job):
        if "HEAD_SHA" in (step.get("env") or {}):
            assert step["env"]["HEAD_SHA"] == "${{ github.event.pull_request.head.sha }}"
        # mhai-models is installed and run from main's checkout only
        if "uv " in str(step.get("run", "")):
            assert str(step.get("working-directory", "")).startswith("main/"), step.get("name")
    # never the PR's trust policy
    assert "MHAI_MODELS_TRUST_POLICY" not in yaml.safe_dump(wf)


def test_signoff_reads_three_locks_from_three_commits() -> None:
    wf = _load("model-signoff.yml")
    assert _on(wf) == {"push": {"branches": ["main"], "paths": ["models.lock"]}}
    job = wf["jobs"]["signoff"]
    assert _environment(job) == "model-signoff"
    assert _permissions(job) == {"contents": "read", "pull-requests": "read", "id-token": "write"}
    assert job["env"]["BASE_SHA"] == "${{ github.event.before }}"
    checkout = _steps(job)[0]
    assert checkout["with"]["ref"] == "${{ github.event.before }}"
    runs = _runs(job)
    assert 'git -C base show "$BASE_SHA:models.lock" >"$RUNNER_TEMP/before.lock"' in runs
    assert 'git -C base show "$GITHUB_SHA:models.lock" >"$RUNNER_TEMP/after.lock"' in runs
    assert 'git -C base show "$head:models.lock" >"$RUNNER_TEMP/head.lock"' in runs
    for flag, value in (
        ("--base-sha", '"$BASE_SHA"'),
        ("--commit", '"$GITHUB_SHA"'),
        ("--head-sha", '"$HEAD_SHA"'),
        ("--before", '"$RUNNER_TEMP/before.lock"'),
        ("--after", '"$RUNNER_TEMP/after.lock"'),
        ("--head-lock", '"$RUNNER_TEMP/head.lock"'),
        ("--base-dir", '"$base"'),
    ):
        assert f"{flag} {value}" in runs, flag
    assert 'echo "HEAD_SHA=$head"' in runs
    for step in _steps(job):
        if "uv " in str(step.get("run", "")):
            assert str(step.get("working-directory", "")).startswith("base/"), step.get("name")
    assert "MHAI_MODELS_TRUST_POLICY" not in yaml.safe_dump(wf)


def test_promotion_expects_the_dry_runs_files() -> None:
    jobs = _jobs("model-promote.yml")
    assert set(jobs) == {"dry-run", "promote"}
    dry, promote = jobs["dry-run"], jobs["promote"]
    assert _environment(dry) == "model-eval" and _environment(promote) == "model-promotion"
    assert promote["needs"] == "dry-run"
    assert promote["env"]["EXPECT_FILES"] == "${{ needs.dry-run.outputs.files_hash }}"
    assert '[[ "$EXPECT_FILES" =~ ^[0-9a-f]{64}$ ]]' in _runs(promote)
    assert '--expect-files "$EXPECT_FILES"' in _runs(promote)
    assert "--dry-run" in _runs(dry) and "--dry-run" not in _runs(promote)
    assert '[[ "$files" =~ ^[0-9a-f]{64}$ ]]' in _runs(dry)
    on = _on(_load("model-promote.yml"))["workflow_dispatch"]["inputs"]
    assert on["source"]["options"] == ["drive", "hf", "local-fixture"]
    assert _load("model-promote.yml")["concurrency"] == {
        "group": "model-promote-${{ inputs.name }}",
        "cancel-in-progress": False,
    }


@pytest.mark.parametrize("name", MODEL_WORKFLOWS)
def test_artifacts_carry_summaries_only(name: str) -> None:
    for job in _jobs(name).values():
        for step in _steps(job):
            if str(step.get("uses", "")).startswith("actions/upload-artifact@"):
                assert re.fullmatch(
                    r"\$\{\{ runner\.temp \}\}/summary/[a-z-]+\.json", step["with"]["path"]
                )


def test_testset_build_exit_4_is_a_warning_from_the_last_stderr_line() -> None:
    job = _jobs("model-testset.yml")["testset-build"]
    runs = _runs(job)
    assert '[ "$code" -eq 4 ]' in runs
    assert 'tail -n 1 "$RUNNER_TEMP/testset.err"' in runs
    assert '"scratch_not_removed"' in runs


def test_ci_runs_the_rehearsal_and_lints_the_workflows() -> None:
    jobs = _jobs("ci.yml")
    e2e = _runs(jobs["model-promotion-e2e"])
    assert "rehearse-promotion.sh store-up" in e2e
    assert "rehearse-promotion.sh run" in e2e
    assert "pytest -q -m store tests/modelstore" in e2e
    assert _environment(jobs["model-promotion-e2e"]) is None
    assert "secrets." not in yaml.safe_dump(jobs["model-promotion-e2e"])
    checks = _runs(jobs["checks"])
    assert "actionlint" in checks and "zizmor" in checks


def test_image_ships_the_warm_up_page() -> None:
    """The image's build context is services/intelligence, so it ships a copy of the
    warm-up page; the copy must be the committed page, byte for byte."""
    service = REPO / "services" / "intelligence"
    copy = service / "warmup" / "page.png"
    assert copy.read_bytes() == (REPO / "testdata" / "models" / "warmup" / "page.png").read_bytes()
    dockerfile = (service / "Dockerfile").read_text(encoding="utf-8")
    assert "COPY warmup/page.png /app/warmup/page.png" in dockerfile
    env = (REPO / ".env.example").read_text(encoding="utf-8")
    assert "# INTELLIGENCE_WARMUP_PAGE=/app/warmup/page.png" in env

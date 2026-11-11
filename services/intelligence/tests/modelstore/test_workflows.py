"""The model workflows' security rules (plan Task 24, P18, B23), read from the YAML.

Checked on every ``model-*.yml`` and on ``ci.yml``: inputs and event fields reach a shell
only through ``env:``; every action is pinned by commit SHA; no workflow names a GitHub
environment or needs a repository setting (the lean rewrite, 11 Nov 2026); the store's
writer and the signing key reach only the steps that write, and only in production with
the dry run's files hash; no ``id-token``; no ``pull_request_target``; no production job
runs in development; lock-check is an ordinary check on every PR.
"""

from __future__ import annotations

import re
from pathlib import Path
from typing import Any

import pytest
import yaml

REPO = Path(__file__).resolve().parents[4]
WORKFLOWS = REPO / ".github" / "workflows"
MODEL_WORKFLOWS = ("model-promote.yml", "model-lock.yml", "model-testset.yml")
CHECKED = (*MODEL_WORKFLOWS, "ci.yml")
SHA_PIN = re.compile(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_./-]+@[0-9a-f]{40}")
WRITE_VARIABLES = re.compile(r"MODEL_STORE_WRITER_|MHAI_MODELS_SIGNING_KEY")
REHEARSAL_SWITCH = "${{ vars.MODEL_STORE_ENDPOINT == '' }}"
REHEARSAL_STEP_IF = "env.REHEARSAL == 'true'"
PRODUCTION_PREFIX = "${{ vars.MODEL_STORE_ENDPOINT != '' && "
PROMOTE_CALL = re.compile(r"(\(|mhai-models\s+)promote\b")


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


def _permissions(job: dict[str, Any]) -> dict[str, Any]:
    perms = job.get("permissions")
    return perms if isinstance(perms, dict) else {}


def _all_jobs() -> list[tuple[str, str, dict[str, Any]]]:
    return [(name, j, job) for name in CHECKED for j, job in _jobs(name).items()]


def _writes_to_store(job: dict[str, Any]) -> bool:
    """A job that promotes (any promote invocation without --dry-run on its own line, so a
    real promotion beside a dry run still counts) or builds a test split."""
    runs = _runs(job)
    promotes = any(
        PROMOTE_CALL.search(line) and "--dry-run" not in line for line in runs.splitlines()
    )
    return promotes or re.search(r"mhai-models\s+testset-build\b", runs) is not None


def test_model_workflows_exist() -> None:
    for name in CHECKED:
        assert (WORKFLOWS / name).is_file(), name
    assert not (WORKFLOWS / "model-signoff.yml").exists()  # the PR reviewer is the sign-off


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


def test_no_model_workflow_needs_a_github_setting() -> None:
    """No model job names an environment (none needs creating or protecting), and none
    asks for id-token: the store and the signer are reached with repository secrets."""
    for name, j, job in _all_jobs():
        assert "environment" not in job, (name, j)
        assert "id-token" not in _permissions(job), (name, j)
    for name in CHECKED:
        assert "id-token" not in (_load(name).get("permissions") or {}), name


def test_writing_jobs_write_only_in_production_steps() -> None:
    """The jobs that write: the promotion and the test split. Their writer credentials and
    the signing key reach one step, only outside rehearsal mode; the promotion's only with
    the dry run's files hash (B23, P26)."""
    writers = {(n, j) for n, j, job in _all_jobs() if _writes_to_store(job)}
    assert writers == {("model-promote.yml", "promote"), ("model-testset.yml", "testset-build")}
    for name, j in writers:
        job = _jobs(name)[j]
        holders = [s for s in _steps(job) if WRITE_VARIABLES.search(yaml.safe_dump(s))]
        assert len(holders) == 1, (name, j)
        held = [v for v in (holders[0].get("env") or {}).values() if "secrets." in str(v)]
        assert any(WRITE_VARIABLES.search(str(v)) for v in held), (name, j)
        for var, value in (holders[0].get("env") or {}).items():
            if "secrets." in str(value):
                assert str(value).startswith(PRODUCTION_PREFIX), (name, var)
                if name == "model-promote.yml" and WRITE_VARIABLES.search(str(value)):
                    assert "inputs.expect_files != ''" in str(value), var


def test_no_pull_request_target() -> None:
    for path in sorted(WORKFLOWS.glob("*.y*ml")):
        assert "pull_request_target" not in path.read_text(encoding="utf-8"), path.name


def test_write_variables_scoped() -> None:
    """The store's writer and the signing key are named only in the writing jobs, never
    at the workflow level, and no workflow passes secrets on wholesale or by a computed
    name. lock-check holds only the reader."""
    for path in sorted(WORKFLOWS.glob("*.y*ml")):
        raw = path.read_text(encoding="utf-8")
        assert "secrets: inherit" not in raw and "secrets[" not in raw, path.name
    for name in sorted(p.name for p in WORKFLOWS.glob("*.y*ml")):
        wf = _load(name)
        assert not WRITE_VARIABLES.search(yaml.safe_dump(wf.get("env") or {})), name
        for j, job in wf["jobs"].items():
            if WRITE_VARIABLES.search(yaml.safe_dump(job)) and "secrets." in yaml.safe_dump(job):
                assert (name, j) in {
                    ("model-promote.yml", "promote"),
                    ("model-testset.yml", "testset-build"),
                }, (name, j)
    lock = yaml.safe_dump(_load("model-lock.yml"))
    assert "WRITER" not in lock and "SIGNING_KEY" not in lock and "MHAI_MODELS_SIGNER" not in lock


def _env_maps(job: dict[str, Any]) -> list[dict[str, Any]]:
    out = [job.get("env") or {}]
    out += [s.get("env") or {} for s in _steps(job)]
    return out


@pytest.mark.parametrize("name", MODEL_WORKFLOWS)
def test_no_production_job_runs_in_development(name: str) -> None:
    """No env: mapping names development. lock-check is production always; the promotion
    and test-split jobs are production unless in rehearsal mode (no store configured),
    where the Mode step switches both."""
    wf = _load(name)
    for env in [wf.get("env") or {}, *(e for job in wf["jobs"].values() for e in _env_maps(job))]:
        for var in ("MHAI_MODELS_ENV", "APP_ENV"):
            assert env.get(var, "production") != "development", (name, var)
    for j, job in wf["jobs"].items():
        runs = _runs(job)
        if name == "model-lock.yml":
            assert job["env"]["MHAI_MODELS_ENV"] == job["env"]["APP_ENV"] == "production"
            assert "development" not in runs and "--rehearsal" not in runs, j
            continue
        assert job["env"]["REHEARSAL"] == REHEARSAL_SWITCH, (name, j)
        mode = [s for s in _steps(job) if s.get("name") == "Mode"]
        assert len(mode) == 1, (name, j)
        text = str(mode[0]["run"])
        assert 'case "$REHEARSAL" in true | false) ;;' in text  # checked, never trusted
        parts = re.split(r"\n\s*else\n", text)
        assert len(parts) == 2, (name, j)
        rehearsal, production = parts
        assert 'if [ "$REHEARSAL" = true ]' in rehearsal
        assert "MHAI_MODELS_ENV=development" in rehearsal and "APP_ENV=development" in rehearsal
        assert "MHAI_MODELS_ENV=production" in production and "APP_ENV=production" in production
        assert "development" not in production
        # development is written only by the Mode step; rehearsal-only steps are guarded
        for step in _steps(job):
            if step is not mode[0]:
                assert "=development" not in str(step.get("run", "")), (name, j, step.get("name"))
            if "rehearse-promotion.sh" in str(step.get("run", "")):
                assert step.get("if") == REHEARSAL_STEP_IF, (name, j, step.get("name"))
            for var, value in (step.get("env") or {}).items():
                if "secrets." in str(value):
                    assert str(value).startswith(PRODUCTION_PREFIX), (name, j, var)


@pytest.mark.parametrize("name", MODEL_WORKFLOWS)
def test_checkouts_keep_no_credentials(name: str) -> None:
    for j, job in _jobs(name).items():
        for step in _steps(job):
            if str(step.get("uses", "")).startswith("actions/checkout@"):
                assert step["with"]["persist-credentials"] is False, (name, j)


def test_lock_check_is_an_ordinary_check_with_mains_lock_and_policy() -> None:
    """lock-check runs on every PR, reads only the reader's secret, compares main's lock
    (the empty lock when main has none) with the PR's, and verifies under main's trust
    policy when main has one."""
    wf = _load("model-lock.yml")
    on = _on(wf)
    assert set(on) == {"pull_request"}
    assert not on["pull_request"]  # no path filter: it reports on every PR
    job = wf["jobs"]["lock-check"]
    assert _permissions(job) == {"contents": "read"}
    runs = _runs(job)
    assert 'git show "origin/$BASE_REF:models.lock" >"$base/models.lock"' in runs
    assert '{"models":{},"schema":"mhai.models.lock/1"}' in runs
    assert 'git show "origin/$BASE_REF:models/trust-policy.json"' in runs
    assert '--base "$RUNNER_TEMP/main/models.lock"' in runs
    assert '--trust-policy "$TRUST_POLICY"' in runs
    assert "--rehearsal" not in runs
    secrets_named = re.findall(r"secrets\.([A-Z_]+)", yaml.safe_dump(job))
    assert sorted(secrets_named) == [
        "MODEL_STORE_READER_ACCESS_KEY",
        "MODEL_STORE_READER_SECRET_KEY",
    ]


def test_promotion_is_a_dry_run_unless_given_the_dry_runs_files() -> None:
    """One job: without expect_files a dry run (no writer, no key), with it the promotion
    of exactly the files the dry run hashed (P26)."""
    jobs = _jobs("model-promote.yml")
    assert set(jobs) == {"promote"}
    job = jobs["promote"]
    assert job["env"]["EXPECT_FILES"] == "${{ inputs.expect_files }}"
    runs = _runs(job)
    assert '[[ -z "$EXPECT_FILES" || "$EXPECT_FILES" =~ ^[0-9a-f]{64}$ ]]' in runs
    assert '--expect-files "$EXPECT_FILES"' in runs
    assert '--dry-run --report-dir "$RUNNER_TEMP/report"' in runs
    assert '[[ "$files" =~ ^[0-9a-f]{64}$ ]]' in runs
    # The key file exists only for the promotion and is removed after it.
    assert 'rm -f "$RUNNER_TEMP/signing-key.pem"' in runs
    assert "(umask 077 && printf" in runs
    on = _on(_load("model-promote.yml"))["workflow_dispatch"]["inputs"]
    assert on["source"]["options"] == ["drive", "hf", "local-fixture"]
    assert on["expect_files"]["required"] is False
    assert _load("model-promote.yml")["concurrency"] == {
        "group": "model-promote-${{ inputs.name }}",
        "cancel-in-progress": False,
    }


def test_drive_token_is_read_only_and_production_only() -> None:
    job = _jobs("model-promote.yml")["promote"]
    drive = next(s for s in _steps(job) if s.get("id") == "drive")
    assert drive["if"] == "env.REHEARSAL == 'false' && env.SOURCE == 'drive'"
    assert drive["with"]["access_token_scopes"] == "https://www.googleapis.com/auth/drive.readonly"
    assert drive["with"]["create_credentials_file"] is False
    assert drive["with"]["export_environment_variables"] is False
    runs = _runs(job)
    assert 'export MHAI_MODELS_DRIVE_TOKEN_FILE="$RUNNER_TEMP/drive-token"' in runs
    assert 'rm -f "$RUNNER_TEMP/signing-key.pem" "$RUNNER_TEMP/drive-token"' in runs


ARTIFACT_PATH = re.compile(r"\$\{\{ runner\.temp \}\}/summary/([a-z-]+\.json|evaluation\.md)")


@pytest.mark.parametrize("name", MODEL_WORKFLOWS)
def test_artifacts_carry_summaries_only(name: str) -> None:
    """The CLI's JSON summaries and the aggregate evaluation.md; never weights, pages,
    evaluation.json's folder or keys."""
    for job in _jobs(name).values():
        for step in _steps(job):
            if str(step.get("uses", "")).startswith("actions/upload-artifact@"):
                paths = str(step["with"]["path"]).strip().splitlines()
                assert paths
                for path in paths:
                    assert ARTIFACT_PATH.fullmatch(path), path


def test_dry_run_uploads_the_aggregate_report() -> None:
    """The dry run keeps evaluation.json outside the summary folder, renders evaluation.md
    from it with mhai-doceval report (the aggregate-only renderer), and uploads the
    Markdown with its summary."""
    job = _jobs("model-promote.yml")["promote"]
    runs = _runs(job)
    assert (
        'uv run --no-sync mhai-doceval report "$RUNNER_TEMP/report/evaluation.json"'
        ' >"$RUNNER_TEMP/summary/evaluation.md"'
    ) in runs
    uploads = [
        s for s in _steps(job) if str(s.get("uses", "")).startswith("actions/upload-artifact@")
    ]
    assert len(uploads) == 1
    assert str(uploads[0]["with"]["path"]).strip().splitlines() == [
        "${{ runner.temp }}/summary/promote.json",
        "${{ runner.temp }}/summary/evaluation.md",
    ]


def test_testset_build_exit_4_is_a_warning_from_the_last_stderr_line() -> None:
    job = _jobs("model-testset.yml")["testset-build"]
    runs = _runs(job)
    assert '[ "$code" -eq 4 ]' in runs
    assert 'tail -n 1 "$RUNNER_TEMP/testset.err"' in runs
    assert '"scratch_not_removed"' in runs


def test_ci_runs_the_rehearsal_and_lints_the_workflows() -> None:
    jobs = _jobs("ci.yml")
    e2e = _runs(jobs["model-promotion-e2e"])
    assert 'rehearse-promotion.sh store-up "$GITHUB_ENV" writer reader' in e2e
    assert "rehearse-promotion.sh run" in e2e
    assert "pytest -q -m store tests/modelstore" in e2e
    assert "environment" not in jobs["model-promotion-e2e"]
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


@pytest.mark.parametrize("name", MODEL_WORKFLOWS)
def test_concurrency_set(name: str) -> None:
    conc = _load(name)["concurrency"]
    assert conc["group"].startswith(name.removesuffix(".yml"))
    # a promotion or a test split is never cancelled half-way
    assert conc["cancel-in-progress"] is (name == "model-lock.yml")


@pytest.mark.parametrize("name", ["model-promote.yml", "model-testset.yml"])
def test_dispatched_jobs_refuse_any_ref_but_main(name: str) -> None:
    for j, job in _jobs(name).items():
        first = _steps(job)[0]
        assert first["name"] == "Main only", (name, j)
        assert first["env"] == {"REF": "${{ github.ref }}"}
        assert '[ "$REF" = refs/heads/main ] ||' in first["run"]
        assert "exit 1" in first["run"]


def test_promotion_checks_names_before_they_name_a_folder() -> None:
    job = _jobs("model-promote.yml")["promote"]
    mode = next(s for s in _steps(job) if s.get("name") == "Mode")
    assert '[[ "$RUN" =~ ^[A-Za-z0-9_-]{1,64}$ ]]' in mode["run"]
    assert '[[ "$NAME" =~ ^[a-z0-9][a-z0-9-]{1,62}$ ]]' in mode["run"]
    script = (REPO / "services/intelligence/scripts/rehearse-promotion.sh").read_text()
    assert '[[ "$run" =~ ^[A-Za-z0-9_-]{1,64}$ ]] || fail "bad RUN"' in script


def test_compose_sets_the_warm_up_page() -> None:
    assert "INTELLIGENCE_WARMUP_PAGE: /app/warmup/page.png" in (
        REPO / "docker-compose.yml"
    ).read_text(encoding="utf-8")
    assert "INTELLIGENCE_WARMUP_PAGE: /app/warmup/page.png" in (
        REPO / "deploy" / "compose.yml"
    ).read_text(encoding="utf-8")


def test_production_promotion_refuses_the_rehearsal_gate() -> None:
    """Outside rehearsal mode the promote job refuses gate rehearsal before anything runs
    (P14: nothing measured only against the rehearsal bar is signed in production)."""
    job = _jobs("model-promote.yml")["promote"]
    mode = [s for s in _steps(job) if s.get("name") == "Mode"]
    assert len(mode) == 1
    _, production = re.split(r"\n\s*else\n", str(mode[0]["run"]))
    refuse = 'case "$GATE_INPUT" in phase1 | phase3) ;; *) echo "::error::'
    assert refuse in production
    assert production.index(refuse) < production.index('echo "GATE=$GATE_INPUT"')

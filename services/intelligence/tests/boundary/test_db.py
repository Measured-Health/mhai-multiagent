"""boundary/db.py: the pipeline's one door to Postgres (spec §8.2).

The unit tests need no Docker. The ones marked `boundary` run against the
Postgres 16 that core's own `migrate up` built (conftest.postgres), which
also provisioned the intelligence_app login. Every id, row and password is
synthetic.
"""

from __future__ import annotations

import logging
import time
import uuid
from collections.abc import AsyncIterator
from pathlib import Path
from types import SimpleNamespace
from typing import Any

import psycopg
import pytest

from intelligence.boundary import envelope, jobs_repo
from intelligence.boundary.config import ConfigError, WorkerSettings, parse_db_url
from intelligence.boundary.db import Database, LoginRefused, NoHospital, connect_kwargs
from intelligence.boundary.keys import TrustedKeys
from intelligence.boundary.operations import Operations
from intelligence.boundary.pb import pipeline_pb2
from intelligence.boundary.replay import JtiSet
from intelligence.boundary.services import pipeline
from intelligence.boundary.verify import Verified, Verifier, strict_decoder
from tests.boundary.conftest import INTELLIGENCE_DB_PASSWORD, INTELLIGENCE_DB_USER, Postgres
from tests.boundary.tokens import JOB_SCHEMA, QUERY_ID, RID, Signer

HOSPITAL_A = "a1a1a1a1-0000-4000-8000-00000000000a"
HOSPITAL_B = "b2b2b2b2-0000-4000-8000-00000000000b"
NOWHERE = "postgres://127.0.0.1:1/mhai?sslmode=disable"


def verified_for(hid: str) -> Verified:
    """A real Verified for hid, through the verifier (the only way)."""
    signer = Signer()
    now = int(time.time())
    body = pipeline_pb2.RunQueryJob(query_id=QUERY_ID).SerializeToString(deterministic=True)
    subject = f"jobs.v1.{hid}.run_query"
    token = signer.mint_job(body, hid=hid, nsub=subject, iat=now)

    def clock() -> float:
        return now + 1

    verifier = Verifier(
        TrustedKeys.from_encoded(signer.encoded_keys()),
        "pipeline",
        Operations.load(development=True),
        JtiSet(clock),
        clock,
    )
    v = verifier.verify_job(
        subject,
        {envelope.SCHEMA: JOB_SCHEMA, envelope.REQUEST_ID: RID, envelope.TOKEN: token},
        body,
        strict_decoder(pipeline_pb2.RunQueryJob),
    )
    assert isinstance(v, Verified), v
    return v


def bent(hid: str) -> Verified:
    """A Verified with its hospital bent after the fact (as a bug might),
    built without the verifier's issuing key."""
    v = object.__new__(Verified)
    object.__setattr__(v, "hid", hid)
    return v


# --- unit: the argument, the URL, the settings ---------------------------------


async def test_with_hospital_refuses_anything_but_a_verified() -> None:
    db = Database(NOWHERE, password="test-only-unit-password")
    forged = SimpleNamespace(hid=HOSPITAL_A, sub="x", roles=("clinician",))

    class Sub(Verified):  # a subclass could carry anything
        pass

    sub = object.__new__(Sub)
    object.__setattr__(sub, "hid", HOSPITAL_A)
    for bad in (HOSPITAL_A, forged, {"hid": HOSPITAL_A}, None, sub):
        with pytest.raises(TypeError):
            async with db.with_hospital(bad):  # type: ignore[arg-type]
                pytest.fail("ran with a forged hospital")


async def test_with_hospital_refuses_an_empty_or_bad_hospital() -> None:
    db = Database(NOWHERE, password="test-only-unit-password")
    for hid in ("", "not-a-uuid", "00000000-0000-0000-0000-000000000000", "1' OR '1'='1"):
        with pytest.raises(NoHospital):
            async with db.with_hospital(bent(hid)):
                pytest.fail("ran without a hospital")


def test_credentials_are_parsed_out_of_the_url() -> None:
    kw = connect_kwargs(
        "postgres://someone:url-only-secret@db.internal:5433/mhai?sslmode=verify-full"
    )
    assert kw == {
        "host": "db.internal",
        "port": 5433,
        "dbname": "mhai",
        "sslmode": "verify-full",
        "user": "someone",
        "password": "url-only-secret",
    }
    kw = connect_kwargs("postgres://db.internal/mhai", password="from-the-file-1234")
    assert kw["user"] == "intelligence_app" and kw["password"] == "from-the-file-1234"
    assert "conninfo" not in kw and "dsn" not in kw


@pytest.mark.parametrize(
    "url",
    [
        "postgres://u:url-only-secret@h/mhai?options=-c%20role%3Dmhai_gateway",
        "postgres://u:url-only-secret@h/mhai?service=other",
        "postgres://u:url-only-secret@h/mhai?passfile=/etc/x",
        "postgres://u:url-only-secret@h/mhai?sslmode=disable&sslmode=require",
        "mysql://u:url-only-secret@h/mhai",
        "postgres://u:url-only-secret@/mhai",
        "postgres://u:url-only-secret@h/",
        "postgres://u:url-only-secret@h:notaport/mhai",
    ],
)
def test_bad_urls_refused_without_echoing_them(url: str) -> None:
    with pytest.raises(ConfigError) as e:
        parse_db_url(url)
    assert "INTELLIGENCE_DB_URL" in str(e.value)
    assert "url-only-secret" not in str(e.value) and "mhai_gateway" not in str(e.value)


PIPELINE = {
    "INTELLIGENCE_SERVICE": "pipeline",
    "NATS_URL": "nats://pipeline:test-only-pw@127.0.0.1:4222",
    "INTELLIGENCE_TRUSTED_KEYS_PATH": "/run/trusted/keys.json",
    "APP_ENV": "development",
    "INTELLIGENCE_DB_URL": "postgres://db.internal:5432/mhai",
}


PRODUCTION = {
    **{k: v for k, v in PIPELINE.items() if k != "APP_ENV"},
    "APP_ENV": "production",
    "NATS_URL": "tls://127.0.0.1:4222",
    "NATS_TLS_CA_PATH": "/run/nats/ca.pem",
    "NATS_TLS_CERT_PATH": "/run/nats/pipeline.pem",
    "NATS_TLS_KEY_PATH": "/run/nats/pipeline-key.pem",
    "NATS_NKEY_SEED_PATH": "/run/nats/pipeline.nk",
}


def _pw_file(tmp_path: Path, mode: int) -> Path:
    f = tmp_path / f"pw-{mode:o}"
    f.write_text("test-only-file-password-0002\n")
    f.chmod(mode)
    return f


@pytest.mark.parametrize("mode", [0o620, 0o602, 0o666, 0o460])
def test_settings_refuse_a_password_file_others_can_write(tmp_path: Path, mode: int) -> None:
    f = _pw_file(tmp_path, mode)
    with pytest.raises(ConfigError) as e:
        WorkerSettings.from_env({**PRODUCTION, "INTELLIGENCE_DB_PASSWORD_FILE": str(f)})
    assert "INTELLIGENCE_DB_PASSWORD_FILE is writable" in str(e.value)
    assert "test-only-file-password" not in str(e.value)
    # Development reads it as it is.
    s = WorkerSettings.from_env({**PIPELINE, "INTELLIGENCE_DB_PASSWORD_FILE": str(f)})
    assert s.db_password == "test-only-file-password-0002"


@pytest.mark.parametrize(
    ("mode", "warned"), [(0o400, False), (0o440, False), (0o444, True), (0o644, True)]
)
def test_settings_warn_on_a_password_file_others_can_read(
    tmp_path: Path, caplog: pytest.LogCaptureFixture, mode: int, warned: bool
) -> None:
    """A secret mount is often root-owned 0444: allowed, with a warning.
    (The remote runner runs the tests as root, so these files are root's.)"""
    f = _pw_file(tmp_path, mode)
    caplog.set_level(logging.WARNING, logger="intelligence.boundary.config")
    s = WorkerSettings.from_env({**PRODUCTION, "INTELLIGENCE_DB_PASSWORD_FILE": str(f)})
    assert s.db_password == "test-only-file-password-0002"
    assert ("readable by others" in caplog.text) is warned
    assert "test-only-file-password" not in caplog.text


def test_settings_refuse_a_password_path_that_is_not_a_file(tmp_path: Path) -> None:
    with pytest.raises(ConfigError, match="INTELLIGENCE_DB_PASSWORD_FILE is not a regular file"):
        WorkerSettings.from_env({**PRODUCTION, "INTELLIGENCE_DB_PASSWORD_FILE": str(tmp_path)})


def test_settings_read_the_password_from_a_file(tmp_path: Path) -> None:
    f = tmp_path / "pw"
    f.write_text("test-only-file-password-0001\n")
    s = WorkerSettings.from_env({**PIPELINE, "INTELLIGENCE_DB_PASSWORD_FILE": str(f)})
    assert (s.db_user, s.db_password) == ("intelligence_app", "test-only-file-password-0001")
    assert "test-only-file-password" not in repr(s) and "db.internal" not in repr(s)
    s = WorkerSettings.from_env(
        {**PIPELINE, "INTELLIGENCE_DB_USER": "pipeline_db", "INTELLIGENCE_DB_PASSWORD": "x" * 16}
    )
    assert s.db_user == "pipeline_db"


@pytest.mark.parametrize(
    ("extra", "name"),
    [
        ({}, "INTELLIGENCE_DB_PASSWORD"),
        ({"INTELLIGENCE_DB_PASSWORD": "changemeChangemeChangeme"}, "INTELLIGENCE_DB_PASSWORD"),
        ({"INTELLIGENCE_DB_PASSWORD": "my-placeholder-password"}, "INTELLIGENCE_DB_PASSWORD"),
        ({"INTELLIGENCE_DB_PASSWORD": "too-short-pw"}, "INTELLIGENCE_DB_PASSWORD"),
        (
            {"INTELLIGENCE_DB_PASSWORD_FILE": "/nonexistent/test-only"},
            "INTELLIGENCE_DB_PASSWORD_FILE",
        ),
        (
            {"INTELLIGENCE_DB_PASSWORD": "x" * 16, "INTELLIGENCE_DB_PASSWORD_FILE": "/run/pw"},
            "INTELLIGENCE_DB_PASSWORD_FILE",
        ),
        (
            {"INTELLIGENCE_DB_URL": "postgres://intelligence_app:changeme-but-long@h/mhai"},
            "INTELLIGENCE_DB_URL",
        ),
        (
            {"INTELLIGENCE_DB_URL": "postgres://other:x@h/mhai", "INTELLIGENCE_DB_USER": "me"},
            "INTELLIGENCE_DB_USER",
        ),
    ],
)
def test_settings_refuse_bad_db_credentials(extra: dict[str, str], name: str) -> None:
    with pytest.raises(ConfigError) as e:
        WorkerSettings.from_env({**PIPELINE, **extra})
    assert name in str(e.value)
    for k, v in extra.items():
        if "PASSWORD" in k and not k.endswith("_FILE"):
            assert v not in str(e.value)
    assert "changeme-but-long" not in str(e.value)


def test_settings_url_password_only_in_development() -> None:
    url = "postgres://intelligence_app:test-only-url-password-1@h/mhai"
    s = WorkerSettings.from_env({**PIPELINE, "INTELLIGENCE_DB_URL": url})
    assert s.db_password == "test-only-url-password-1"
    with pytest.raises(ConfigError) as e:
        WorkerSettings.from_env({**PIPELINE, "APP_ENV": "production", "INTELLIGENCE_DB_URL": url})
    assert "must not carry a password" in str(e.value)
    assert "test-only-url-password-1" not in str(e.value)


def test_settings_need_verified_tls_to_postgres_outside_development(tmp_path: Path) -> None:
    """Carried T10/T15: outside development the pipeline's database
    connection must be sslmode=verify-full; development keeps disable."""
    f = tmp_path / "pw"
    f.write_text("test-only-file-password-0002\n")
    f.chmod(0o400)
    prod = {
        **PIPELINE,
        "APP_ENV": "production",
        "INTELLIGENCE_DB_PASSWORD_FILE": str(f),
        "NATS_URL": "tls://nats:4222",
        "NATS_NKEY_SEED_PATH": "/run/secrets/pipeline.nk",
        "NATS_TLS_CA_PATH": "/run/ca.pem",
        "NATS_TLS_CERT_PATH": "/run/cert.pem",
        "NATS_TLS_KEY_PATH": "/run/key.pem",
    }
    for mode in (
        "",
        "?sslmode=disable",
        "?sslmode=require",
        "?sslmode=verify-ca",
        "?sslmode=prefer",
    ):
        env = {**prod, "INTELLIGENCE_DB_URL": "postgres://db.internal:5432/mhai" + mode}
        with pytest.raises(ConfigError, match="sslmode=verify-full"):
            WorkerSettings.from_env(env)
    s = WorkerSettings.from_env(
        {**prod, "INTELLIGENCE_DB_URL": "postgres://db.internal:5432/mhai?sslmode=verify-full"}
    )
    assert s.db_password == "test-only-file-password-0002"
    dev = WorkerSettings.from_env(
        {
            **PIPELINE,
            "INTELLIGENCE_DB_URL": "postgres://db.internal:5432/mhai?sslmode=disable",
            "INTELLIGENCE_DB_PASSWORD": "x" * 16,
        }
    )
    assert dev.db_url is not None


def test_settings_db_credentials_need_the_url() -> None:
    env = {k: v for k, v in PIPELINE.items() if k != "INTELLIGENCE_DB_URL"}
    with pytest.raises(ConfigError, match="INTELLIGENCE_DB_URL"):
        WorkerSettings.from_env({**env, "INTELLIGENCE_DB_PASSWORD": "x" * 16})


async def test_ready_is_false_until_open() -> None:
    db = Database(NOWHERE, password="test-only-unit-password")
    assert await db.ready() is False
    health = pipeline.health_handler(database_ready=db.ready)
    _, out = await health(verified_for(HOSPITAL_A), pipeline_pb2.HealthRequest())
    assert isinstance(out, pipeline_pb2.HealthResponse) and out.database_ready is False


async def test_open_failure_names_no_credential(caplog: pytest.LogCaptureFixture) -> None:
    caplog.set_level(logging.DEBUG)
    db = Database(NOWHERE, user="someone", password="test-only-unit-password", timeout=1.0)
    with pytest.raises(Exception) as e:
        await db.open()
    text = str(e.value) + repr(e.value) + caplog.text + repr(db)
    assert "test-only-unit-password" not in text
    assert e.value.__cause__ is None and e.value.__suppress_context__


# --- boundary: real Postgres ------------------------------------------------------


async def _superuser(pg: Postgres) -> psycopg.AsyncConnection[Any]:
    return await psycopg.AsyncConnection.connect(pg.url, autocommit=True)


async def _seed(pg: Postgres) -> tuple[str, str]:
    """Hospitals A and B (once) and one fresh query each; returns the ids."""
    qa, qb = str(uuid.uuid4()), str(uuid.uuid4())
    async with await _superuser(pg) as conn:
        for hid, code in ((HOSPITAL_A, "T10A"), (HOSPITAL_B, "T10B")):
            await conn.execute(
                "INSERT INTO hospitals (id, name, short_code, status, time_zone) "
                "VALUES (%s, %s, %s, 'active', 'Africa/Lagos') ON CONFLICT (id) DO NOTHING",
                (hid, "Synthetic Hospital " + code, code),
            )
        for qid, hid in ((qa, HOSPITAL_A), (qb, HOSPITAL_B)):
            await conn.execute(
                "INSERT INTO clinical_queries (id, hospital_id, clinical_question, urgency) "
                "VALUES (%s, %s, 'synthetic question', 'routine')",
                (qid, hid),
            )
    return qa, qb


@pytest.fixture
async def database(postgres: Postgres) -> AsyncIterator[Database]:
    db = Database(
        postgres.intelligence_url,
        user=INTELLIGENCE_DB_USER,
        password=INTELLIGENCE_DB_PASSWORD,
        max_size=1,
    )
    await db.open()
    try:
        yield db
    finally:
        await db.close()


async def _refused(db: Database) -> LoginRefused:
    """db.open() must refuse; if it doesn't, close the pool (an open pool
    left behind hangs the event loop's teardown) and fail."""
    try:
        await db.open()
    except LoginRefused as e:
        return e
    await db.close()
    pytest.fail("the login check accepted the login")


async def _login(pg: Postgres, attrs: str) -> tuple[str, str]:
    """A throwaway LOGIN role with attrs, made by the superuser."""
    name = "t10_" + uuid.uuid4().hex[:8]
    pw = "test-only-login-" + uuid.uuid4().hex
    async with await _superuser(pg) as conn:
        await conn.execute(f"CREATE ROLE {name} LOGIN PASSWORD '{pw}' {attrs}")
    return name, pw


@pytest.mark.boundary
async def test_login_outside_role_fails(postgres: Postgres, database: Database) -> None:
    """A22, worker side: the login itself holds nothing."""
    assert await database.ready() is True
    async with await psycopg.AsyncConnection.connect(
        postgres.intelligence_url,
        user=INTELLIGENCE_DB_USER,
        password=INTELLIGENCE_DB_PASSWORD,
        autocommit=True,
    ) as conn:
        for q in (
            "SELECT 1 FROM clinical_queries",
            "SELECT 1 FROM kb_chunks",
            "SELECT 1 FROM intelligence_audit_outbox",
            "SELECT 1 FROM users",
        ):
            with pytest.raises(psycopg.errors.InsufficientPrivilege):
                await conn.execute(q)
        for role in ("mhai_gateway", "mhai_auth", "mhai_platform", "mhai_jobs", "mhai_audit_drain"):
            with pytest.raises(psycopg.errors.InsufficientPrivilege):
                await conn.execute(f"SET ROLE {role}")


@pytest.mark.boundary
async def test_with_hospital_cannot_see_other_hospital_query(
    postgres: Postgres, database: Database
) -> None:
    """A9: under A's verified token, B's query is not there."""
    qa, qb = await _seed(postgres)
    async with database.with_hospital(verified_for(HOSPITAL_A)) as cur:
        assert await jobs_repo.read_status(cur, qa) == "queued"
        assert await jobs_repo.read_status(cur, qb) is None
        await cur.execute(
            "SELECT count(*) FROM clinical_queries WHERE hospital_id = %s", (HOSPITAL_B,)
        )
        row = await cur.fetchone()
        assert row is not None and row[0] == 0
        await cur.execute("SELECT current_user, current_setting('mhai.hospital_id')")
        assert await cur.fetchone() == ("mhai_worker", HOSPITAL_A)
        await cur.execute("UPDATE clinical_queries SET status = 'running' WHERE id = %s", (qb,))
        assert cur.rowcount == 0
    async with database.with_hospital(verified_for(HOSPITAL_B)) as cur:
        assert await jobs_repo.read_status(cur, qb) == "queued"
        assert await jobs_repo.read_status(cur, qa) is None


@pytest.mark.boundary
async def test_worker_cannot_write_kb_chunks(postgres: Postgres, database: Database) -> None:
    """A11: no kb_chunks write, shared (null hospital), another's or its own."""
    async with await _superuser(postgres) as conn:
        await _seed(postgres)
        cur = await conn.execute(
            "INSERT INTO kb_chunks (chunk_id, hospital_id, content, chunk_type) "
            "VALUES (%s, NULL, 'synthetic shared guideline', 'guideline') RETURNING id",
            ("S" + uuid.uuid4().hex[:8],),
        )
        row = await cur.fetchone()
        assert row is not None
        shared_id = row[0]
    for hid in (None, HOSPITAL_B, HOSPITAL_A):
        with pytest.raises(psycopg.errors.InsufficientPrivilege):
            async with database.with_hospital(verified_for(HOSPITAL_A)) as cur:
                await cur.execute(
                    "INSERT INTO kb_chunks (chunk_id, hospital_id, content, chunk_type) "
                    "VALUES (%s, %s, 'synthetic', 'guideline')",
                    ("W" + uuid.uuid4().hex[:8], hid),
                )
    for q in (
        "UPDATE kb_chunks SET content = 'tampered' WHERE id = %s",
        "DELETE FROM kb_chunks WHERE id = %s",
    ):
        with pytest.raises(psycopg.errors.InsufficientPrivilege):
            async with database.with_hospital(verified_for(HOSPITAL_A)) as cur:
                await cur.execute(q, (shared_id,))
    # Reading shared content is what it is for.
    async with database.with_hospital(verified_for(HOSPITAL_A)) as cur:
        await cur.execute("SELECT content FROM kb_chunks WHERE id = %s", (shared_id,))
        assert await cur.fetchone() == ("synthetic shared guideline",)


@pytest.mark.boundary
async def test_with_hospital_rolls_back_on_error(postgres: Postgres, database: Database) -> None:
    qa, _ = await _seed(postgres)
    with pytest.raises(RuntimeError):
        async with database.with_hospital(verified_for(HOSPITAL_A)) as cur:
            await cur.execute("UPDATE clinical_queries SET status = 'running' WHERE id = %s", (qa,))
            raise RuntimeError("synthetic failure")
    async with database.with_hospital(verified_for(HOSPITAL_A)) as cur:
        assert await jobs_repo.read_status(cur, qa) == "queued"
        await cur.execute("UPDATE clinical_queries SET status = 'running' WHERE id = %s", (qa,))
    async with database.with_hospital(verified_for(HOSPITAL_A)) as cur:
        assert await jobs_repo.read_status(cur, qa) == "running"


@pytest.mark.boundary
async def test_nothing_outlives_the_transaction(postgres: Postgres, database: Database) -> None:
    """A session-level SET ROLE or setting inside with_hospital is gone on
    the pooled connection's next use (the pool has one connection)."""
    qa, qb = await _seed(postgres)
    async with database.with_hospital(verified_for(HOSPITAL_A)) as cur:
        await cur.execute("SET SESSION ROLE mhai_worker")
        await cur.execute("SELECT set_config('mhai.hospital_id', %s, false)", (HOSPITAL_B,))
    assert await database.ready() is True
    async with database.with_hospital(verified_for(HOSPITAL_A)) as cur:
        assert await jobs_repo.read_status(cur, qb) is None
    # The bare login again: SELECT outside SET ROLE is refused.
    async with database._pool.connection() as conn:
        cur = await conn.execute("SELECT current_user, current_setting('mhai.hospital_id', true)")
        assert await cur.fetchone() in ((INTELLIGENCE_DB_USER, None), (INTELLIGENCE_DB_USER, ""))


@pytest.mark.boundary
async def test_repeated_statements_survive_the_reset(
    postgres: Postgres, database: Database
) -> None:
    """psycopg prepares a statement after it has run a few times on a
    connection; the reset's DISCARD ALL deallocates it on the server, so a
    pooled connection that prepared one would fail the next run (found by
    the T11 poison test). One connection, many runs: none fails."""
    qa, _ = await _seed(postgres)
    for _ in range(15):
        async with database.with_hospital(verified_for(HOSPITAL_A)) as cur:
            assert await jobs_repo.read_status(cur, qa) == "queued"
    assert await database.ready()


@pytest.mark.boundary
async def test_check_login_refuses_superuser(postgres: Postgres) -> None:
    db = Database(postgres.url, max_size=1)
    e = await _refused(db)
    assert "superuser" in str(e) and "test-only-pg-pw" not in str(e)
    assert await db.ready() is False


@pytest.mark.boundary
@pytest.mark.parametrize(
    ("attrs", "want"),
    [
        ("BYPASSRLS IN ROLE mhai_worker_app", "BYPASSRLS"),
        ("CREATEROLE IN ROLE mhai_worker_app", "CREATEROLE"),
        ("CREATEDB IN ROLE mhai_worker_app", "CREATEDB"),
        ("", "not a member of mhai_worker_app"),
        ("IN ROLE mhai_worker_app, mhai_gateway", "also a member of mhai_gateway"),
        ("IN ROLE mhai_worker_app, mhai_worker", "inherits the privileges of mhai_worker"),
        ("IN ROLE mhai_worker_app, pg_read_all_data", "pg_read_all_data"),
        ("IN ROLE mhai_app", "not a member of mhai_worker_app"),
    ],
)
async def test_check_login_refuses_logins_rls_cant_hold(
    postgres: Postgres, attrs: str, want: str
) -> None:
    name, pw = await _login(postgres, attrs)
    db = Database(postgres.intelligence_url, user=name, password=pw, max_size=1)
    e = await _refused(db)
    assert want in str(e) and pw not in str(e)


@pytest.mark.boundary
async def test_check_login_refuses_a_login_that_starts_as_another_role(postgres: Postgres) -> None:
    """A role-level default (ALTER ROLE ... SET role, or PGOPTIONS) would
    put every pooled connection in mhai_worker outside with_hospital, and
    DISCARD ALL resets to it. The login check refuses it."""
    name, pw = await _login(postgres, "IN ROLE mhai_worker_app")
    async with await _superuser(postgres) as conn:
        await conn.execute(f"ALTER ROLE {name} SET role = 'mhai_worker'")
    db = Database(postgres.intelligence_url, user=name, password=pw, max_size=1)
    assert "starts as mhai_worker" in str(await _refused(db))
    async with await _superuser(postgres) as conn:
        await conn.execute(f"ALTER ROLE {name} RESET role")
        await conn.execute(f"ALTER ROLE {name} SET mhai.hospital_id = '{HOSPITAL_A}'")
    db = Database(postgres.intelligence_url, user=name, password=pw, max_size=1)
    assert "hospital" in str(await _refused(db))


@pytest.mark.boundary
async def test_check_login_refuses_a_table_owner(postgres: Postgres) -> None:
    name, pw = await _login(postgres, "IN ROLE mhai_worker_app")
    async with await _superuser(postgres) as conn:
        await conn.execute(f"CREATE TABLE owned_{name} (x int)")
        await conn.execute(f"ALTER TABLE owned_{name} OWNER TO {name}")
    try:
        db = Database(postgres.intelligence_url, user=name, password=pw, max_size=1)
        assert "owns a table" in str(await _refused(db))
    finally:
        async with await _superuser(postgres) as conn:
            await conn.execute(f"DROP TABLE owned_{name}")


@pytest.mark.boundary
async def test_health_reports_the_database(database: Database) -> None:
    health = pipeline.health_handler(database_ready=database.ready)
    _, out = await health(verified_for(HOSPITAL_A), pipeline_pb2.HealthRequest())
    assert isinstance(out, pipeline_pb2.HealthResponse) and out.database_ready is True
    await database.close()
    _, out = await health(verified_for(HOSPITAL_A), pipeline_pb2.HealthRequest())
    assert isinstance(out, pipeline_pb2.HealthResponse) and out.database_ready is False

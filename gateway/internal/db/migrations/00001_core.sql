-- Core gateway tables: hospitals, accounts, sessions, audit, idempotency.
-- Roles, grants and row-level security are in 00003_rls_roles.sql.

-- +goose Up
CREATE EXTENSION IF NOT EXISTS citext;

CREATE TABLE hospitals (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text NOT NULL,
    short_code text NOT NULL UNIQUE,
    status     text NOT NULL DEFAULT 'setup_incomplete'
               CHECK (status IN ('active', 'setup_incomplete', 'deactivated',
                                 'removal_pending', 'removal_scheduled', 'removed')),
    time_zone  text NOT NULL DEFAULT 'UTC',
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE hospital_settings (
    hospital_id                uuid PRIMARY KEY REFERENCES hospitals (id) ON DELETE CASCADE,
    idle_timeout_minutes       integer NOT NULL DEFAULT 15 CHECK (idle_timeout_minutes > 0),
    review_overdue_hours       integer NOT NULL DEFAULT 4 CHECK (review_overdue_hours > 0),
    require_mfa_for_clinicians boolean NOT NULL DEFAULT false,
    allow_trust_device         boolean NOT NULL DEFAULT false
);

-- hospital_id is null exactly for platform accounts (platform_admin,
-- auditor); the gateway enforces that pairing with the roles.
CREATE TABLE users (
    id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    hospital_id          uuid REFERENCES hospitals (id),
    email                citext NOT NULL UNIQUE CHECK (email::text = btrim(email::text) AND email::text <> ''),
    password_hash        text,
    display_name         text NOT NULL,
    full_name            text,
    role_title           text,
    specialty            text,
    is_active            boolean NOT NULL DEFAULT true,
    must_change_password boolean NOT NULL DEFAULT false,
    terms_version        text,
    terms_accepted_at    timestamptz,
    last_login_at        timestamptz,
    oidc_issuer          text,
    oidc_subject         text,
    created_at           timestamptz NOT NULL DEFAULT now(),
    UNIQUE (oidc_issuer, oidc_subject)
);
CREATE INDEX users_hospital_idx ON users (hospital_id);

CREATE TABLE user_roles (
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role    text NOT NULL
            CHECK (role IN ('clinician', 'hospital_admin', 'analyst', 'platform_admin', 'auditor')),
    PRIMARY KEY (user_id, role)
);

CREATE TABLE user_mfa (
    user_id            uuid PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    secret_enc         bytea NOT NULL,
    enabled            boolean NOT NULL DEFAULT false,
    last_used_step     bigint,
    backup_code_hashes text[] NOT NULL DEFAULT '{}'
);

CREATE TABLE sessions (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    ua_summary   text,
    ip_prefix    text,
    revoked_at   timestamptz
);
CREATE INDEX sessions_user_idx ON sessions (user_id);

CREATE TABLE refresh_tokens (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    session_id   uuid NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    token_hash   text NOT NULL UNIQUE,
    issued_at    timestamptz NOT NULL DEFAULT now(),
    rotated_at   timestamptz,
    successor_id uuid REFERENCES refresh_tokens (id)
);
CREATE INDEX refresh_tokens_session_idx ON refresh_tokens (session_id);

CREATE TABLE trusted_devices (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash text NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL DEFAULT now() + interval '30 days'
);
CREATE INDEX trusted_devices_user_idx ON trusted_devices (user_id);

-- Append-only hash chain (gateway design §11). No PHI in payload.
CREATE TABLE audit_log (
    id          bigserial PRIMARY KEY,
    ts          timestamptz NOT NULL DEFAULT now(),
    actor_id    uuid,
    hospital_id uuid REFERENCES hospitals (id),
    request_id  text,
    action      text NOT NULL,
    entity      text,
    entity_id   text,
    payload     jsonb NOT NULL DEFAULT '{}',
    prev_hash   text NOT NULL,
    hash        text NOT NULL UNIQUE
);
CREATE INDEX audit_log_hospital_idx ON audit_log (hospital_id, id);

-- A verified point in the chain: everything up to upto_id hashes to hash.
CREATE TABLE audit_checkpoints (
    id         bigserial PRIMARY KEY,
    upto_id    bigint NOT NULL,
    hash       text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Stored replies for Idempotency-Key, kept 72 hours. hospital_id is the
-- hospital of a clinical request, and null for sign-in and platform
-- requests; RLS keeps the two apart. Stored bodies never hold tokens.
CREATE TABLE idempotency_keys (
    scope        text NOT NULL,
    key          text NOT NULL,
    hospital_id  uuid REFERENCES hospitals (id),
    request_hash text NOT NULL,
    status       integer,
    body         bytea,
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL DEFAULT now() + interval '72 hours',
    PRIMARY KEY (scope, key)
);
CREATE INDEX idempotency_keys_expires_idx ON idempotency_keys (expires_at);

-- +goose Down
DROP TABLE idempotency_keys;
DROP TABLE audit_checkpoints;
DROP TABLE audit_log;
DROP TABLE trusted_devices;
DROP TABLE refresh_tokens;
DROP TABLE sessions;
DROP TABLE user_mfa;
DROP TABLE user_roles;
DROP TABLE users;
DROP TABLE hospital_settings;
DROP TABLE hospitals;

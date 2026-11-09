-- Platform analytics: What changed and follow-ups (Plan 7c spec §7.3, §7.4,
-- §8.1; Plan 7c-3, Tasks 8 and 9). Numbered 00022 after 00020 (EMR) and
-- 00021 (Patient Analysis), which merged first; the plan's 00020.
--
-- Tables (owner mhai_migrate, RLS enabled and forced, nothing for PUBLIC;
-- platform tables with no hospital scope, as analytics_snapshots):
--   - analytics_change_runs: one row per evaluated week, written by the
--     scheduler (mhai_analytics_job) in the same transaction as the week's
--     changes, so the routes know which week and month the open changes
--     compare and when they were evaluated. A week is evaluated once.
--   - analytics_changes: a flagged change, keyed while it is open by
--     (hospital, measure, direction); a random uuid id. current and previous
--     are shown values only (the evaluator reads stored, shown cells and
--     nothing else). The scheduler opens, updates and closes rows; the
--     routes (mhai_platform) move version on with each follow-up write, on
--     an open change only. Nobody deletes a row (retention is the retention
--     plan's, §12).
--   - analytics_follow_ups: one follow-up per change, with the platform
--     admin's note. A table of its own (as the cancel note, R36): the note is
--     returned in the changes list to platform roles and nowhere else. Only
--     mhai_platform touches it.
--
-- The platform's audit appends gain the three follow-up actions
-- (analytics.followed_up, analytics.follow_up_edited,
-- analytics.follow_up_undone), conditions kept. The policy is rebuilt from
-- main's list as 00020 left it; 00021 changed provision_append only. Down
-- restores 00020's platform_append exactly.

-- +goose Up

CREATE TABLE analytics_change_runs (
    week_from    date        PRIMARY KEY,
    week_to      date        NOT NULL,
    month_from   date        NOT NULL,
    month_to     date        NOT NULL,
    evaluated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (mhai_reporting_period_ok('week', week_from, week_to)),
    CHECK (mhai_reporting_period_ok('month', month_from, month_to)),
    -- The month that finished before the week started: the previous
    -- calendar month (§7.3).
    CHECK (month_to = date_trunc('month', week_from::timestamp)::date)
);
ALTER TABLE analytics_change_runs OWNER TO mhai_migrate;
ALTER TABLE analytics_change_runs ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
REVOKE ALL ON analytics_change_runs FROM PUBLIC;
GRANT SELECT ON analytics_change_runs TO mhai_platform;
GRANT SELECT, INSERT ON analytics_change_runs TO mhai_analytics_job;
CREATE POLICY analytics_platform_read ON analytics_change_runs FOR SELECT TO mhai_platform USING (true);
CREATE POLICY analytics_job_read ON analytics_change_runs FOR SELECT TO mhai_analytics_job USING (true);
CREATE POLICY analytics_job_insert ON analytics_change_runs FOR INSERT TO mhai_analytics_job
    WITH CHECK ((week_to::timestamp AT TIME ZONE 'UTC') + interval '15 minutes' <= now()
                AND EXISTS (SELECT 1 FROM analytics_snapshots s WHERE s.kind = 'week' AND s.period_from = week_from));

CREATE TABLE analytics_changes (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    hospital_id uuid        REFERENCES hospitals (id),
    area        text        NOT NULL CHECK (area IN ('use', 'quality')),
    measure     text        NOT NULL CHECK (measure IN ('questions', 'active_clinicians', 'questions_per_clinician',
                                                        'needs_review_share', 'agreement_rate')),
    direction   text        NOT NULL CHECK (direction IN ('rising', 'falling', 'worse', 'better')),
    -- Serious (Critical-type) changes come from patient alerts (§3).
    serious     boolean     NOT NULL DEFAULT false CHECK (NOT serious),
    current     jsonb       NOT NULL,
    previous    jsonb       NOT NULL,
    summary     text        NOT NULL CHECK (char_length(summary) BETWEEN 1 AND 200),
    opened_week date        NOT NULL,
    last_week   date        NOT NULL,
    closed_week date,
    version     integer     NOT NULL DEFAULT 1 CHECK (version >= 1),
    CHECK ((area = 'use') = (measure IN ('questions', 'active_clinicians', 'questions_per_clinician'))),
    CHECK ((direction IN ('rising', 'falling')) = (area = 'use')),
    -- current and previous are shown values as the routes send them:
    -- {"value": n >= 0, "suppressed": false}, nothing else.
    CHECK (jsonb_typeof(current) = 'object' AND current - 'value' - 'suppressed' = '{}'::jsonb
           AND jsonb_typeof(current -> 'value') = 'number' AND (current ->> 'value')::numeric >= 0
           AND current -> 'suppressed' = 'false'::jsonb),
    CHECK (jsonb_typeof(previous) = 'object' AND previous - 'value' - 'suppressed' = '{}'::jsonb
           AND jsonb_typeof(previous -> 'value') = 'number' AND (previous ->> 'value')::numeric >= 0
           AND previous -> 'suppressed' = 'false'::jsonb),
    CHECK (last_week >= opened_week),
    CHECK (closed_week IS NULL OR closed_week > last_week)
);
-- At most one open change per key (§7.3).
CREATE UNIQUE INDEX analytics_changes_open ON analytics_changes
    (coalesce(hospital_id, '00000000-0000-0000-0000-000000000000'::uuid), measure, direction)
    WHERE closed_week IS NULL;
ALTER TABLE analytics_changes OWNER TO mhai_migrate;
ALTER TABLE analytics_changes ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
REVOKE ALL ON analytics_changes FROM PUBLIC;
GRANT SELECT, UPDATE (version) ON analytics_changes TO mhai_platform;
GRANT SELECT, INSERT, UPDATE (current, previous, summary, last_week, closed_week) ON analytics_changes TO mhai_analytics_job;
CREATE POLICY analytics_platform_read ON analytics_changes FOR SELECT TO mhai_platform USING (true);
-- A follow-up write moves an open change's version on; a closed one is
-- never written.
CREATE POLICY analytics_platform_version ON analytics_changes FOR UPDATE TO mhai_platform
    USING (closed_week IS NULL) WITH CHECK (closed_week IS NULL);
CREATE POLICY analytics_job_read ON analytics_changes FOR SELECT TO mhai_analytics_job USING (true);
CREATE POLICY analytics_job_insert ON analytics_changes FOR INSERT TO mhai_analytics_job
    WITH CHECK (closed_week IS NULL AND version = 1 AND NOT serious);
-- The scheduler updates or closes an open change; a closed one stays
-- closed.
CREATE POLICY analytics_job_update ON analytics_changes FOR UPDATE TO mhai_analytics_job
    USING (closed_week IS NULL) WITH CHECK (true);

CREATE TABLE analytics_follow_ups (
    change_id uuid        PRIMARY KEY REFERENCES analytics_changes (id),
    by_user   uuid        NOT NULL REFERENCES users (id),
    at        timestamptz NOT NULL DEFAULT now(),
    -- 1 to 500 characters, no line or paragraph separator and no bidi or
    -- other invisible format character (R11; core checks the whole of
    -- Unicode's Cf before it writes, this is the backstop).
    note      text CHECK (note IS NULL OR (char_length(note) BETWEEN 1 AND 500
                AND note !~ '[­؀-؅؜۝܏᠎​-‏ -‮⁠-⁤⁦-⁯﻿￹-￻]')),
    version   integer     NOT NULL DEFAULT 1 CHECK (version >= 1)
);
ALTER TABLE analytics_follow_ups OWNER TO mhai_migrate;
ALTER TABLE analytics_follow_ups ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY;
REVOKE ALL ON analytics_follow_ups FROM PUBLIC;
GRANT SELECT, INSERT, DELETE, UPDATE (note, version) ON analytics_follow_ups TO mhai_platform;
CREATE POLICY analytics_platform ON analytics_follow_ups TO mhai_platform USING (true) WITH CHECK (true);

------------------------------------------------------------------------
-- The platform's audit appends: the three follow-up actions, conditions
-- kept (00020's list, which 00021 left as it was).
------------------------------------------------------------------------
DROP POLICY platform_append ON audit_log;
CREATE POLICY platform_append ON audit_log FOR INSERT TO mhai_platform
    WITH CHECK (action IN ('audit.viewed', 'audit.searched', 'audit.verify_started', 'audit.verify_finished',
                           'audit.check_outcome', 'audit.undisplayable_reported', 'job.retried', 'job.cancelled',
                           'hospital.model_policy_changed', 'emr.connection_added', 'emr.connection_changed',
                           'emr.connection_switched_on', 'emr.connection_switched_off', 'emr.connection_tested',
                           'emr.source_resumed', 'analytics.followed_up', 'analytics.follow_up_edited',
                           'analytics.follow_up_undone')
                AND hospital_id IS NULL);

-- +goose Down

-- 00020's append, as it was.
DROP POLICY platform_append ON audit_log;
CREATE POLICY platform_append ON audit_log FOR INSERT TO mhai_platform
    WITH CHECK (action IN ('audit.viewed', 'audit.searched', 'audit.verify_started', 'audit.verify_finished',
                           'audit.check_outcome', 'audit.undisplayable_reported', 'job.retried', 'job.cancelled',
                           'hospital.model_policy_changed', 'emr.connection_added', 'emr.connection_changed',
                           'emr.connection_switched_on', 'emr.connection_switched_off', 'emr.connection_tested',
                           'emr.source_resumed')
                AND hospital_id IS NULL);

-- The tables take their policies, indexes and grants with them.
DROP TABLE analytics_follow_ups;
DROP TABLE analytics_changes;
DROP TABLE analytics_change_runs;

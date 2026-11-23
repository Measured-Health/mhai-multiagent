-- Our own answer model on the GPU (deploy/README.md, "Our own model on the
-- GPU"): a per-hospital switch in the model policy, off for every hospital.
--
--   - hospital_model_policy.own_model: false by default (and for every
--     existing row). True sends the hospital's questions to the own route,
--     answered by our own fine-tuned model on the GPU server (the
--     gpu-model compose service). It can't be on together with a cloud
--     provider (hospital_model_policy_one_route): a hospital has one
--     route. mhai_platform and mhai_provision may update it, as the rest
--     of the policy; nobody else holds anything new;
--   - clinical_queries.model_route: the CHECK gains 'own';
--   - mhai_set_model_route: cloud:<provider> when the policy names one,
--     else 'own' when own_model is on, else 'local' (as before);
--   - mhai_effective_model_route: a claimed 'own' question stays 'own' only
--     while the caller's own active hospital still has own_model on;
--     anything else runs local, as for a cloud route (00018, design §5.2).
--     Both functions are replaced in place (CREATE OR REPLACE keeps the
--     owner, the pinned search_path and the one EXECUTE grant).
--
-- Additive: with own_model false everywhere (the default) every route is
-- what it was. The own route is not a cloud route: nothing leaves the
-- host, nothing is reserved on the token ledger.
--
-- Down restores 00018's functions and 00024's route list and drops the
-- column. It fails while a question's route is 'own' (it never rewrites a
-- question's recorded route).

-- +goose Up

ALTER TABLE hospital_model_policy
    ADD COLUMN own_model boolean NOT NULL DEFAULT false,
    ADD CONSTRAINT hospital_model_policy_one_route CHECK (NOT (own_model AND cloud_provider IS NOT NULL));
GRANT UPDATE (own_model) ON hospital_model_policy TO mhai_platform, mhai_provision;

ALTER TABLE clinical_queries
    DROP CONSTRAINT clinical_queries_model_route_check,
    ADD CONSTRAINT clinical_queries_model_route_check
        CHECK (model_route IN ('local', 'own', 'cloud:openai', 'cloud:anthropic', 'cloud:xai', 'cloud:gemini'));

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION mhai_set_model_route() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    -- Whatever the insert named is ignored.
    NEW.model_route := coalesce((SELECT CASE WHEN p.cloud_provider IS NOT NULL THEN 'cloud:' || p.cloud_provider
                                             WHEN p.own_model THEN 'own' END
                                 FROM public.hospital_model_policy p
                                 WHERE p.hospital_id = NEW.hospital_id), 'local');
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION mhai_effective_model_route(p_hospital uuid, p_route text) RETURNS text
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT CASE WHEN p_route IS DISTINCT FROM 'local'
                 AND p_hospital = public.mhai_current_hospital()
                 AND NOT EXISTS (SELECT 1 FROM public.hospitals hs
                                 WHERE hs.id = p_hospital AND hs.status <> 'active')
                 AND EXISTS (SELECT 1 FROM public.hospital_model_policy p
                             WHERE p.hospital_id = p_hospital
                               AND ('cloud:' || p.cloud_provider = p_route
                                    OR (p_route = 'own' AND p.own_model)))
                THEN p_route
                ELSE 'local' END
$$;
-- +goose StatementEnd

-- +goose Down

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION mhai_effective_model_route(p_hospital uuid, p_route text) RETURNS text
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
    SELECT CASE WHEN p_route IS DISTINCT FROM 'local'
                 AND p_hospital = public.mhai_current_hospital()
                 AND NOT EXISTS (SELECT 1 FROM public.hospitals hs
                                 WHERE hs.id = p_hospital AND hs.status <> 'active')
                 AND EXISTS (SELECT 1 FROM public.hospital_model_policy p
                             WHERE p.hospital_id = p_hospital
                               AND 'cloud:' || p.cloud_provider = p_route)
                THEN p_route
                ELSE 'local' END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION mhai_set_model_route() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    -- Whatever the insert named is ignored.
    NEW.model_route := coalesce((SELECT 'cloud:' || p.cloud_provider
                                 FROM public.hospital_model_policy p
                                 WHERE p.hospital_id = NEW.hospital_id), 'local');
    RETURN NEW;
END
$$;
-- +goose StatementEnd

ALTER TABLE clinical_queries
    DROP CONSTRAINT clinical_queries_model_route_check,
    ADD CONSTRAINT clinical_queries_model_route_check
        CHECK (model_route IN ('local', 'cloud:openai', 'cloud:anthropic', 'cloud:xai', 'cloud:gemini'));

-- The column takes its grant and its CHECK with it.
ALTER TABLE hospital_model_policy DROP COLUMN own_model;

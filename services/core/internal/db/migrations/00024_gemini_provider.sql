-- Cloud model providers: Google Gemini as a fourth provider. Additive: it
-- widens the three closed sets of 00018 to name 'gemini', and nothing
-- existing changes meaning.
--
-- Numbered 00024 by the merge-order ruling (12 Nov): this merges first;
-- Patient Analysis PR 4 takes 00025, EMR PR 6 00026 and EMR PR 7 00027.
--
--   - hospital_model_policy.cloud_provider: the CHECK gains 'gemini';
--   - clinical_queries.model_route: the CHECK gains 'cloud:gemini' (the
--     trigger and mhai_effective_model_route build the route from the
--     policy's provider, so they need no change);
--   - mhai_cloud_reserve: the provider check gains 'gemini'. The function
--     is replaced in place (CREATE OR REPLACE keeps its owner, its pinned
--     search_path and its one EXECUTE grant); the body is 00018's with
--     that one list changed.
--
-- Down restores 00018's three lists. It fails while a policy names gemini
-- or a question's route is cloud:gemini: it never rewrites a policy or a
-- question's recorded route. Switch those hospitals off first.

-- +goose Up

ALTER TABLE hospital_model_policy
    DROP CONSTRAINT hospital_model_policy_cloud_provider_check,
    ADD CONSTRAINT hospital_model_policy_cloud_provider_check
        CHECK (cloud_provider IN ('openai', 'anthropic', 'xai', 'gemini'));

ALTER TABLE clinical_queries
    DROP CONSTRAINT clinical_queries_model_route_check,
    ADD CONSTRAINT clinical_queries_model_route_check
        CHECK (model_route IN ('local', 'cloud:openai', 'cloud:anthropic', 'cloud:xai', 'cloud:gemini'));

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION mhai_cloud_reserve(p_provider text, p_tokens bigint) RETURNS TABLE (reservation uuid, percent integer)
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    h uuid := public.mhai_current_hospital();
    m date := (date_trunc('month', now() AT TIME ZONE 'UTC'))::date;
    v_total bigint;
    v_budget bigint;
    v_id uuid;
BEGIN
    IF p_provider IS NULL OR p_provider NOT IN ('openai', 'anthropic', 'xai', 'gemini') THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value',
            MESSAGE = 'mhai_cloud_reserve: unknown provider';
    END IF;
    IF p_tokens IS NULL OR p_tokens <= 0 OR p_tokens > 100000 THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value',
            MESSAGE = 'mhai_cloud_reserve: tokens are 1 to 100000';
    END IF;
    IF h IS NULL THEN
        RAISE EXCEPTION USING ERRCODE = 'insufficient_privilege',
            MESSAGE = 'mhai_cloud_reserve: no hospital';
    END IF;
    IF EXISTS (SELECT 1 FROM public.hospitals hs WHERE hs.id = h AND hs.status <> 'active') THEN
        RETURN;
    END IF;
    INSERT INTO public.cloud_usage (hospital_id, month) VALUES (h, m) ON CONFLICT DO NOTHING;
    UPDATE public.cloud_usage u
    SET reserved = u.reserved + p_tokens
    WHERE u.hospital_id = h AND u.month = m
      AND u.used + u.reserved + p_tokens <= (SELECT p.monthly_token_budget FROM public.hospital_model_policy p
                                             WHERE p.hospital_id = h AND p.cloud_provider = p_provider)
    RETURNING u.used + u.reserved,
              (SELECT p.monthly_token_budget FROM public.hospital_model_policy p WHERE p.hospital_id = h)
    INTO v_total, v_budget;
    IF NOT FOUND THEN
        RETURN;
    END IF;
    INSERT INTO public.cloud_reservations (hospital_id, month, tokens) VALUES (h, m, p_tokens)
    RETURNING id INTO v_id;
    reservation := v_id;
    percent := (v_total * 100 / v_budget)::integer;
    RETURN NEXT;
END
$$;
-- +goose StatementEnd

-- +goose Down

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION mhai_cloud_reserve(p_provider text, p_tokens bigint) RETURNS TABLE (reservation uuid, percent integer)
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
    AS $$
DECLARE
    h uuid := public.mhai_current_hospital();
    m date := (date_trunc('month', now() AT TIME ZONE 'UTC'))::date;
    v_total bigint;
    v_budget bigint;
    v_id uuid;
BEGIN
    IF p_provider IS NULL OR p_provider NOT IN ('openai', 'anthropic', 'xai') THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value',
            MESSAGE = 'mhai_cloud_reserve: unknown provider';
    END IF;
    IF p_tokens IS NULL OR p_tokens <= 0 OR p_tokens > 100000 THEN
        RAISE EXCEPTION USING ERRCODE = 'invalid_parameter_value',
            MESSAGE = 'mhai_cloud_reserve: tokens are 1 to 100000';
    END IF;
    IF h IS NULL THEN
        RAISE EXCEPTION USING ERRCODE = 'insufficient_privilege',
            MESSAGE = 'mhai_cloud_reserve: no hospital';
    END IF;
    IF EXISTS (SELECT 1 FROM public.hospitals hs WHERE hs.id = h AND hs.status <> 'active') THEN
        RETURN;
    END IF;
    INSERT INTO public.cloud_usage (hospital_id, month) VALUES (h, m) ON CONFLICT DO NOTHING;
    UPDATE public.cloud_usage u
    SET reserved = u.reserved + p_tokens
    WHERE u.hospital_id = h AND u.month = m
      AND u.used + u.reserved + p_tokens <= (SELECT p.monthly_token_budget FROM public.hospital_model_policy p
                                             WHERE p.hospital_id = h AND p.cloud_provider = p_provider)
    RETURNING u.used + u.reserved,
              (SELECT p.monthly_token_budget FROM public.hospital_model_policy p WHERE p.hospital_id = h)
    INTO v_total, v_budget;
    IF NOT FOUND THEN
        RETURN;
    END IF;
    INSERT INTO public.cloud_reservations (hospital_id, month, tokens) VALUES (h, m, p_tokens)
    RETURNING id INTO v_id;
    reservation := v_id;
    percent := (v_total * 100 / v_budget)::integer;
    RETURN NEXT;
END
$$;
-- +goose StatementEnd

ALTER TABLE clinical_queries
    DROP CONSTRAINT clinical_queries_model_route_check,
    ADD CONSTRAINT clinical_queries_model_route_check
        CHECK (model_route IN ('local', 'cloud:openai', 'cloud:anthropic', 'cloud:xai'));

ALTER TABLE hospital_model_policy
    DROP CONSTRAINT hospital_model_policy_cloud_provider_check,
    ADD CONSTRAINT hospital_model_policy_cloud_provider_check
        CHECK (cloud_provider IN ('openai', 'anthropic', 'xai'));

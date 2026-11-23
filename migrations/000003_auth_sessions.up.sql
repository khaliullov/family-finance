-- =============================================================================
-- Migration 000003: User Authentication Sessions
-- =============================================================================

CREATE TABLE IF NOT EXISTS public.user_sessions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES public.users(id) ON DELETE CASCADE,
    family_id UUID NOT NULL REFERENCES public.families(id) ON DELETE CASCADE,
    token_hash VARCHAR(64) UNIQUE NOT NULL,
    session_chain_id UUID NOT NULL,
    user_agent VARCHAR(255),
    ip_address VARCHAR(45),
    is_revoked BOOLEAN NOT NULL DEFAULT FALSE,
    is_rotated BOOLEAN NOT NULL DEFAULT FALSE,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_used_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    revoked_at TIMESTAMPTZ
);

ALTER TABLE public.user_sessions OWNER TO family_admin;

CREATE INDEX IF NOT EXISTS idx_user_sessions_lookup ON public.user_sessions(token_hash);
CREATE INDEX IF NOT EXISTS idx_user_sessions_user ON public.user_sessions(user_id) WHERE is_revoked = FALSE AND is_rotated = FALSE;
CREATE INDEX IF NOT EXISTS idx_user_sessions_chain ON public.user_sessions(session_chain_id);

-- Explicit Grants
GRANT SELECT, INSERT, UPDATE, DELETE ON public.user_sessions TO family_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON public.user_sessions TO family_auth_verifier;

-- RLS Enforcement
ALTER TABLE public.user_sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.user_sessions FORCE ROW LEVEL SECURITY;

-- Policy for family_app: scoped to current user or active family
CREATE POLICY rls_user_sessions_app ON public.user_sessions
    FOR ALL TO family_app
    USING (
        user_id = NULLIF(current_setting('app.current_user_id', true), '')::UUID
        OR family_id = NULLIF(current_setting('app.current_family_id', true), '')::UUID
    )
    WITH CHECK (
        user_id = NULLIF(current_setting('app.current_user_id', true), '')::UUID
        OR family_id = NULLIF(current_setting('app.current_family_id', true), '')::UUID
    );

-- Policy for family_auth_verifier (SECURITY DEFINER routines in auth_sec)
CREATE POLICY rls_user_sessions_verifier ON public.user_sessions
    FOR ALL TO family_auth_verifier
    USING (true)
    WITH CHECK (true);

-- Privileged helper in auth_sec to rotate refresh token atomically with replay detection
CREATE OR REPLACE FUNCTION auth_sec.rotate_refresh_token(
    p_old_token_hash VARCHAR(64),
    p_new_token_hash VARCHAR(64),
    p_new_expires_at TIMESTAMPTZ,
    p_user_agent VARCHAR(255),
    p_ip_address VARCHAR(45)
)
RETURNS TABLE (
    status VARCHAR(30),
    session_id UUID,
    user_id UUID,
    family_id UUID,
    session_chain_id UUID
)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_session RECORD;
    v_new_id UUID := gen_random_uuid();
BEGIN
    SELECT s.id, s.user_id, s.family_id, s.session_chain_id, s.is_revoked, s.is_rotated, s.expires_at
    INTO v_session
    FROM public.user_sessions s
    WHERE s.token_hash = p_old_token_hash
    FOR UPDATE;

    IF NOT FOUND THEN
        RETURN QUERY SELECT 'NOT_FOUND'::VARCHAR(30), NULL::UUID, NULL::UUID, NULL::UUID, NULL::UUID;
        RETURN;
    END IF;

    IF v_session.is_revoked THEN
        RETURN QUERY SELECT 'REVOKED'::VARCHAR(30), v_session.id, v_session.user_id, v_session.family_id, v_session.session_chain_id;
        RETURN;
    END IF;

    IF v_session.is_rotated THEN
        -- Replay detected: revoke entire session chain!
        UPDATE public.user_sessions s
        SET is_revoked = TRUE, revoked_at = NOW()
        WHERE s.session_chain_id = v_session.session_chain_id;

        RETURN QUERY SELECT 'REPLAY_DETECTED'::VARCHAR(30), v_session.id, v_session.user_id, v_session.family_id, v_session.session_chain_id;
        RETURN;
    END IF;

    IF v_session.expires_at < NOW() THEN
        RETURN QUERY SELECT 'EXPIRED'::VARCHAR(30), v_session.id, v_session.user_id, v_session.family_id, v_session.session_chain_id;
        RETURN;
    END IF;

    -- Valid rotation: mark old token as rotated and insert new active token in same chain
    UPDATE public.user_sessions s
    SET is_rotated = TRUE, last_used_at = NOW()
    WHERE s.id = v_session.id;

    INSERT INTO public.user_sessions (
        id, user_id, family_id, token_hash, session_chain_id, user_agent, ip_address, is_revoked, is_rotated, expires_at
    ) VALUES (
        v_new_id, v_session.user_id, v_session.family_id, p_new_token_hash, v_session.session_chain_id, p_user_agent, p_ip_address, FALSE, FALSE, p_new_expires_at
    );

    RETURN QUERY SELECT 'SUCCESS'::VARCHAR(30), v_new_id, v_session.user_id, v_session.family_id, v_session.session_chain_id;
END;
$$;

ALTER FUNCTION auth_sec.rotate_refresh_token(VARCHAR, VARCHAR, TIMESTAMPTZ, VARCHAR, VARCHAR) OWNER TO family_auth_verifier;
REVOKE ALL ON FUNCTION auth_sec.rotate_refresh_token(VARCHAR, VARCHAR, TIMESTAMPTZ, VARCHAR, VARCHAR) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION auth_sec.rotate_refresh_token(VARCHAR, VARCHAR, TIMESTAMPTZ, VARCHAR, VARCHAR) TO family_app;

-- Privileged helper in auth_sec to revoke a session chain by token hash (e.g. on logout)
CREATE OR REPLACE FUNCTION auth_sec.revoke_session(
    p_token_hash VARCHAR(64)
)
RETURNS BOOLEAN
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_chain_id UUID;
BEGIN
    SELECT s.session_chain_id INTO v_chain_id
    FROM public.user_sessions s
    WHERE s.token_hash = p_token_hash;

    IF NOT FOUND THEN
        RETURN FALSE;
    END IF;

    UPDATE public.user_sessions s
    SET is_revoked = TRUE, revoked_at = NOW()
    WHERE s.session_chain_id = v_chain_id;

    RETURN TRUE;
END;
$$;

ALTER FUNCTION auth_sec.revoke_session(VARCHAR) OWNER TO family_auth_verifier;
REVOKE ALL ON FUNCTION auth_sec.revoke_session(VARCHAR) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION auth_sec.revoke_session(VARCHAR) TO family_app;

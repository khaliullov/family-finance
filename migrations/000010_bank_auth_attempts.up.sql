-- =============================================================================
-- Migration 000010: Bank Authentication Attempts & Challenge Correlation
-- Provides durable, concurrency-safe, tenant-isolated storage for multi-bank
-- interactive authentication attempts and Telegram OTP challenge message routing.
-- =============================================================================

CREATE TABLE IF NOT EXISTS public.bank_auth_attempts (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    family_id UUID NOT NULL REFERENCES public.families(id) ON DELETE CASCADE,
    bank_connection_id UUID REFERENCES public.bank_connections(id) ON DELETE CASCADE,
    provider VARCHAR(50) NOT NULL,
    state VARCHAR(30) NOT NULL DEFAULT 'authenticating'
        CHECK (state IN (
            'disconnected', 'authenticating', 'awaiting_otp', 'verifying_otp',
            'challenge_required', 'active', 'failed', 'expired', 'reconnecting', 'logging_off'
        )),
    challenge_type VARCHAR(50),
    challenge_ref VARCHAR(16),
    telegram_message_id BIGINT,
    telegram_chat_id BIGINT,
    consumed_telegram_update_id BIGINT,
    generation BIGINT NOT NULL DEFAULT 1,
    expires_at TIMESTAMPTZ NOT NULL,
    error_message TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT uq_bank_auth_attempts_tenant UNIQUE (family_id, id),
    CONSTRAINT fk_auth_attempts_connection FOREIGN KEY (family_id, bank_connection_id)
        REFERENCES public.bank_connections(family_id, id) ON DELETE CASCADE
);

-- Index for Telegram reply_to_message_id correlation:
-- Allows instant O(1) resolution of human OTP responses strictly while an attempt is actively awaiting_otp.
CREATE UNIQUE INDEX IF NOT EXISTS idx_auth_attempts_tg_active 
    ON public.bank_auth_attempts (telegram_chat_id, telegram_message_id) 
    WHERE state = 'awaiting_otp';

-- Idempotency index for Telegram update deduplication
CREATE UNIQUE INDEX IF NOT EXISTS idx_auth_attempts_tg_consumed_update 
    ON public.bank_auth_attempts (consumed_telegram_update_id) 
    WHERE consumed_telegram_update_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_auth_attempts_conn 
    ON public.bank_auth_attempts (family_id, bank_connection_id);

CREATE INDEX IF NOT EXISTS idx_auth_attempts_expires 
    ON public.bank_auth_attempts (expires_at) 
    WHERE state IN ('authenticating', 'awaiting_otp', 'verifying_otp');

-- Tenant Row Level Security (RLS)
ALTER TABLE public.bank_auth_attempts OWNER TO family_admin;
ALTER TABLE public.bank_auth_attempts ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.bank_auth_attempts FORCE ROW LEVEL SECURITY;

CREATE POLICY rls_bank_auth_attempts_select ON public.bank_auth_attempts
    FOR SELECT TO family_app, family_worker
    USING (family_id = NULLIF(current_setting('app.current_family_id', true), '')::UUID);

CREATE POLICY rls_bank_auth_attempts_insert ON public.bank_auth_attempts
    FOR INSERT TO family_app, family_worker
    WITH CHECK (family_id = NULLIF(current_setting('app.current_family_id', true), '')::UUID);

CREATE POLICY rls_bank_auth_attempts_update ON public.bank_auth_attempts
    FOR UPDATE TO family_app, family_worker
    USING (family_id = NULLIF(current_setting('app.current_family_id', true), '')::UUID)
    WITH CHECK (family_id = NULLIF(current_setting('app.current_family_id', true), '')::UUID);

CREATE POLICY rls_bank_auth_attempts_delete ON public.bank_auth_attempts
    FOR DELETE TO family_app
    USING (family_id = NULLIF(current_setting('app.current_family_id', true), '')::UUID);

-- Policies for dedicated non-login role family_auth_definer (strictly scoped to telegram challenge correlation)
CREATE POLICY rls_bank_auth_attempts_lookup ON public.bank_auth_attempts
    FOR SELECT TO family_auth_definer
    USING (telegram_message_id IS NOT NULL);

CREATE POLICY rls_bank_auth_attempts_consume ON public.bank_auth_attempts
    FOR UPDATE TO family_auth_definer
    USING (state = 'awaiting_otp')
    WITH CHECK (true);

GRANT SELECT, INSERT, UPDATE, DELETE ON public.bank_auth_attempts TO family_app;
GRANT SELECT, INSERT, UPDATE ON public.bank_auth_attempts TO family_worker;
GRANT SELECT, UPDATE ON public.bank_auth_attempts TO family_auth_definer;

-- =============================================================================
-- SECURITY DEFINER Helper: Safely resolve challenge by Telegram chat and message ID
-- Operates under FORCE RLS without table bypass; strictly matches the Telegram challenge message.
-- =============================================================================
CREATE OR REPLACE FUNCTION public.resolve_telegram_auth_challenge(
    p_chat_id BIGINT,
    p_message_id BIGINT
)
RETURNS TABLE (
    id UUID,
    family_id UUID,
    bank_connection_id UUID,
    provider VARCHAR(50),
    state VARCHAR(30),
    challenge_type VARCHAR(50),
    challenge_ref VARCHAR(16),
    telegram_message_id BIGINT,
    telegram_chat_id BIGINT,
    generation BIGINT,
    expires_at TIMESTAMPTZ,
    error_message TEXT,
    created_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ
)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    RETURN QUERY
    SELECT a.id, a.family_id, a.bank_connection_id, a.provider, a.state,
           a.challenge_type, a.challenge_ref, a.telegram_message_id, a.telegram_chat_id,
           a.generation, a.expires_at, COALESCE(a.error_message, ''), a.created_at, a.updated_at
    FROM public.bank_auth_attempts a
    WHERE a.telegram_chat_id = p_chat_id
      AND a.telegram_message_id = p_message_id
    ORDER BY a.created_at DESC
    LIMIT 1;
END;
$$;

ALTER FUNCTION public.resolve_telegram_auth_challenge(BIGINT, BIGINT) OWNER TO family_auth_definer;
REVOKE ALL ON FUNCTION public.resolve_telegram_auth_challenge(BIGINT, BIGINT) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.resolve_telegram_auth_challenge(BIGINT, BIGINT) TO family_app, family_worker;

-- =============================================================================
-- SECURITY DEFINER Helper: Atomically consume challenge and transition state
-- Enforces durable idempotency and concurrency: returns exactly 1 row to the winning caller.
-- =============================================================================
CREATE OR REPLACE FUNCTION public.consume_telegram_auth_challenge(
    p_chat_id BIGINT,
    p_message_id BIGINT,
    p_telegram_update_id BIGINT DEFAULT NULL
)
RETURNS TABLE (
    id UUID,
    family_id UUID,
    bank_connection_id UUID,
    provider VARCHAR(50),
    state VARCHAR(30),
    challenge_type VARCHAR(50),
    challenge_ref VARCHAR(16),
    telegram_message_id BIGINT,
    telegram_chat_id BIGINT,
    generation BIGINT,
    expires_at TIMESTAMPTZ,
    error_message TEXT,
    created_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ
)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    -- Idempotency check: if update_id was already consumed, return 0 rows
    IF p_telegram_update_id IS NOT NULL AND EXISTS (
        SELECT 1 FROM public.bank_auth_attempts 
        WHERE consumed_telegram_update_id = p_telegram_update_id
    ) THEN
        RETURN;
    END IF;

    RETURN QUERY
    UPDATE public.bank_auth_attempts a
    SET state = 'verifying_otp',
        generation = a.generation + 1,
        consumed_telegram_update_id = COALESCE(p_telegram_update_id, a.consumed_telegram_update_id),
        updated_at = clock_timestamp()
    WHERE a.telegram_chat_id = p_chat_id
      AND a.telegram_message_id = p_message_id
      AND a.state = 'awaiting_otp'
      AND a.expires_at > clock_timestamp()
    RETURNING a.id, a.family_id, a.bank_connection_id, a.provider, a.state,
              a.challenge_type, a.challenge_ref, a.telegram_message_id, a.telegram_chat_id,
              a.generation, a.expires_at, COALESCE(a.error_message, ''), a.created_at, a.updated_at;
END;
$$;

ALTER FUNCTION public.consume_telegram_auth_challenge(BIGINT, BIGINT, BIGINT) OWNER TO family_auth_definer;
REVOKE ALL ON FUNCTION public.consume_telegram_auth_challenge(BIGINT, BIGINT, BIGINT) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.consume_telegram_auth_challenge(BIGINT, BIGINT, BIGINT) TO family_app, family_worker;

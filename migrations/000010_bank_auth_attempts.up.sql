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

GRANT SELECT, INSERT, UPDATE, DELETE ON public.bank_auth_attempts TO family_app;
GRANT SELECT, INSERT, UPDATE ON public.bank_auth_attempts TO family_worker;

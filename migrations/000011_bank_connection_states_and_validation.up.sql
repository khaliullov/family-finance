-- =============================================================================
-- Migration 000011: Bank Connection States & Read-Only Validation Reports
-- Extends the bank_connections state machine to support the full user-facing
-- connection lifecycle, and adds tenant-isolated storage for read-only validation reports.
-- =============================================================================

-- 1. Extend bank_connections status check constraint
ALTER TABLE public.bank_connections DROP CONSTRAINT IF EXISTS bank_connections_status_check;
ALTER TABLE public.bank_connections ADD CONSTRAINT bank_connections_status_check
    CHECK (status IN (
        'created',
        'authenticating',
        'connected',
        'validation_pending',
        'validating',
        'validation_passed',
        'validation_failed',
        'requires_reauthentication',
        'active',
        'expired',
        'error',
        'disconnected',
        'pending_pairing'
    ));

ALTER TABLE public.bank_connections ALTER COLUMN status SET DEFAULT 'created';

-- 2. Add validation tracking timestamps, session versioning, and summary fields
ALTER TABLE public.bank_connections 
    ADD COLUMN IF NOT EXISTS last_authenticated_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS last_validated_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS last_validation_status VARCHAR(30),
    ADD COLUMN IF NOT EXISTS last_validation_report_id UUID,
    ADD COLUMN IF NOT EXISTS session_version INT NOT NULL DEFAULT 1,
    ADD COLUMN IF NOT EXISTS credential_hash VARCHAR(64);

-- 3. Create bank_validation_reports table under tenant RLS
CREATE TABLE IF NOT EXISTS public.bank_validation_reports (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    family_id UUID NOT NULL REFERENCES public.families(id) ON DELETE CASCADE,
    bank_connection_id UUID NOT NULL,
    session_version INT NOT NULL DEFAULT 1,
    credential_hash VARCHAR(64),
    overall_result VARCHAR(30) NOT NULL CHECK (overall_result IN ('passed', 'partial_success', 'failed')),
    started_at TIMESTAMPTZ NOT NULL,
    finished_at TIMESTAMPTZ NOT NULL,
    duration_ms BIGINT NOT NULL,
    report_payload JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT uq_bank_validation_reports_tenant UNIQUE (family_id, id),
    CONSTRAINT fk_validation_reports_connection FOREIGN KEY (family_id, bank_connection_id)
        REFERENCES public.bank_connections(family_id, id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_validation_reports_conn 
    ON public.bank_validation_reports (family_id, bank_connection_id, created_at DESC);

-- Tenant Row Level Security (RLS)
ALTER TABLE public.bank_validation_reports OWNER TO family_admin;
ALTER TABLE public.bank_validation_reports ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.bank_validation_reports FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS rls_bank_validation_reports_select ON public.bank_validation_reports;
CREATE POLICY rls_bank_validation_reports_select ON public.bank_validation_reports
    FOR SELECT TO family_app, family_worker
    USING (family_id = NULLIF(current_setting('app.current_family_id', true), '')::UUID);

DROP POLICY IF EXISTS rls_bank_validation_reports_insert ON public.bank_validation_reports;
CREATE POLICY rls_bank_validation_reports_insert ON public.bank_validation_reports
    FOR INSERT TO family_app, family_worker
    WITH CHECK (family_id = NULLIF(current_setting('app.current_family_id', true), '')::UUID);

DROP POLICY IF EXISTS rls_bank_validation_reports_delete ON public.bank_validation_reports;
CREATE POLICY rls_bank_validation_reports_delete ON public.bank_validation_reports
    FOR DELETE TO family_app
    USING (family_id = NULLIF(current_setting('app.current_family_id', true), '')::UUID);

GRANT SELECT, INSERT, DELETE ON public.bank_validation_reports TO family_app;
GRANT SELECT, INSERT ON public.bank_validation_reports TO family_worker;

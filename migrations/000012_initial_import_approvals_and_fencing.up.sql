-- =============================================================================
-- Migration 000012: Initial Import Approvals, Operation Ownership & Fencing
-- 1. Adds operation ownership and fencing token columns to public.bank_connections.
-- 2. Adds initial import completion tracking to public.bank_connections.
-- 3. Creates public.bank_initial_import_approvals with tenant RLS + FORCE RLS.
-- 4. Enforces tenant immutability via trg_enforce_tenant_immutability.
-- 5. Grants least-privilege permissions to family_app and family_worker.
-- =============================================================================

-- 1. Extend bank_connections with fencing token, active operation type, and initial import tracking
ALTER TABLE public.bank_connections
    ADD COLUMN IF NOT EXISTS fencing_token BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS active_operation_type VARCHAR(20),
    ADD COLUMN IF NOT EXISTS initial_import_completed_at TIMESTAMPTZ;

-- 2. Create durable bank_initial_import_approvals table under tenant RLS
CREATE TABLE IF NOT EXISTS public.bank_initial_import_approvals (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    family_id UUID NOT NULL REFERENCES public.families(id) ON DELETE CASCADE,
    bank_connection_id UUID NOT NULL,
    validation_report_id UUID NOT NULL,
    session_version INT NOT NULL,
    credential_hash VARCHAR(64) NOT NULL,
    approved_by_user_id UUID NOT NULL,
    approved_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    consumed_at TIMESTAMPTZ,
    invalidated_at TIMESTAMPTZ,
    invalidation_reason TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT uq_bank_initial_import_approvals_tenant UNIQUE (family_id, id),
    CONSTRAINT fk_initial_import_approval_connection FOREIGN KEY (family_id, bank_connection_id)
        REFERENCES public.bank_connections(family_id, id) ON DELETE CASCADE,
    CONSTRAINT fk_initial_import_approval_report FOREIGN KEY (family_id, validation_report_id)
        REFERENCES public.bank_validation_reports(family_id, id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_initial_import_approvals_conn
    ON public.bank_initial_import_approvals (family_id, bank_connection_id, created_at DESC);

-- Tenant Row Level Security (RLS)
ALTER TABLE public.bank_initial_import_approvals OWNER TO family_admin;
ALTER TABLE public.bank_initial_import_approvals ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.bank_initial_import_approvals FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS rls_bank_initial_import_approvals_select ON public.bank_initial_import_approvals;
CREATE POLICY rls_bank_initial_import_approvals_select ON public.bank_initial_import_approvals
    FOR SELECT TO family_app, family_worker
    USING (family_id = NULLIF(current_setting('app.current_family_id', true), '')::UUID);

DROP POLICY IF EXISTS rls_bank_initial_import_approvals_insert ON public.bank_initial_import_approvals;
CREATE POLICY rls_bank_initial_import_approvals_insert ON public.bank_initial_import_approvals
    FOR INSERT TO family_app
    WITH CHECK (family_id = NULLIF(current_setting('app.current_family_id', true), '')::UUID);

DROP POLICY IF EXISTS rls_bank_initial_import_approvals_update ON public.bank_initial_import_approvals;
CREATE POLICY rls_bank_initial_import_approvals_update ON public.bank_initial_import_approvals
    FOR UPDATE TO family_app, family_worker
    USING (family_id = NULLIF(current_setting('app.current_family_id', true), '')::UUID);

DROP POLICY IF EXISTS rls_bank_initial_import_approvals_delete ON public.bank_initial_import_approvals;
CREATE POLICY rls_bank_initial_import_approvals_delete ON public.bank_initial_import_approvals
    FOR DELETE TO family_app
    USING (family_id = NULLIF(current_setting('app.current_family_id', true), '')::UUID);

-- Enforce tenant immutability
DROP TRIGGER IF EXISTS trg_immutability_bank_initial_import_approvals ON public.bank_initial_import_approvals;
CREATE TRIGGER trg_immutability_bank_initial_import_approvals
    BEFORE UPDATE OF family_id ON public.bank_initial_import_approvals
    FOR EACH ROW EXECUTE FUNCTION trg_enforce_tenant_immutability();

-- Grants
GRANT SELECT, INSERT, UPDATE, DELETE ON public.bank_initial_import_approvals TO family_app;
GRANT SELECT, UPDATE ON public.bank_initial_import_approvals TO family_worker;

GRANT UPDATE (fencing_token, active_operation_type, initial_import_completed_at) ON public.bank_connections TO family_app;
GRANT UPDATE (fencing_token, active_operation_type, initial_import_completed_at) ON public.bank_connections TO family_worker;

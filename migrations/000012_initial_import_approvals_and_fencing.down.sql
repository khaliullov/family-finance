-- =============================================================================
-- Migration 000012 Down: Rollback Initial Import Approvals & Fencing
-- =============================================================================

DROP TRIGGER IF EXISTS trg_immutability_bank_initial_import_approvals ON public.bank_initial_import_approvals;

DROP POLICY IF EXISTS rls_bank_initial_import_approvals_delete ON public.bank_initial_import_approvals;
DROP POLICY IF EXISTS rls_bank_initial_import_approvals_update ON public.bank_initial_import_approvals;
DROP POLICY IF EXISTS rls_bank_initial_import_approvals_insert ON public.bank_initial_import_approvals;
DROP POLICY IF EXISTS rls_bank_initial_import_approvals_select ON public.bank_initial_import_approvals;

DROP INDEX IF EXISTS public.idx_initial_import_approvals_conn;

DROP TABLE IF EXISTS public.bank_initial_import_approvals;

ALTER TABLE public.bank_connections
    DROP COLUMN IF EXISTS initial_import_completed_at,
    DROP COLUMN IF EXISTS active_operation_type,
    DROP COLUMN IF EXISTS fencing_token;

-- =============================================================================
-- Migration 000007 Down: Reconciliation Discrepancies and Sync Serialization Lock
-- =============================================================================

DROP INDEX IF EXISTS public.idx_raw_payloads_conn_hash;

REVOKE UPDATE (sync_version, active_sync_token, sync_locked_until) ON public.bank_connections FROM family_worker;

ALTER TABLE public.bank_connections
    DROP COLUMN IF EXISTS sync_locked_until,
    DROP COLUMN IF EXISTS active_sync_token,
    DROP COLUMN IF EXISTS sync_version;

REVOKE ALL ON public.reconciliation_discrepancies FROM family_app, family_worker;
DROP POLICY IF EXISTS rls_reconciliation_discrepancies_insert ON public.reconciliation_discrepancies;
DROP POLICY IF EXISTS rls_reconciliation_discrepancies_select ON public.reconciliation_discrepancies;
DROP TABLE IF EXISTS public.reconciliation_discrepancies CASCADE;

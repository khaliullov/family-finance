-- =============================================================================
-- Migration 000007 Down: Reconciliation Discrepancies and Sync Serialization Lock
-- Non-destructive rollback: drops specific objects explicitly without CASCADE.
-- If unexpected external dependencies exist, rollback will fail visibly.
-- =============================================================================

DROP INDEX IF EXISTS public.uq_raw_bank_payloads_conn_hash;
DROP INDEX IF EXISTS public.idx_raw_payloads_conn_hash;

REVOKE UPDATE (sync_version, active_sync_token, sync_locked_until) ON public.bank_connections FROM family_worker;

ALTER TABLE public.bank_connections
    DROP COLUMN IF EXISTS sync_locked_until,
    DROP COLUMN IF EXISTS active_sync_token,
    DROP COLUMN IF EXISTS sync_version;

REVOKE ALL ON public.reconciliation_discrepancies FROM family_app, family_worker;
DROP POLICY IF EXISTS rls_reconciliation_discrepancies_insert ON public.reconciliation_discrepancies;
DROP POLICY IF EXISTS rls_reconciliation_discrepancies_select ON public.reconciliation_discrepancies;
DROP INDEX IF EXISTS public.idx_discrepancies_tx;
DROP INDEX IF EXISTS public.idx_discrepancies_conn;
DROP TABLE IF EXISTS public.reconciliation_discrepancies;

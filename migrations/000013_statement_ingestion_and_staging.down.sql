-- =============================================================================
-- Migration 000013 Down: Revert Generic Statement Ingestion and Staging
-- =============================================================================
-- Destructive data-loss implications:
-- Drops statement_documents, statement_import_batches, staged_bank_transactions,
-- and statement_reconciliation_overrides.
-- Committed transactions already promoted to bank_transactions and accounting_entries
-- remain intact.
-- =============================================================================

-- 1. Revoke and drop SECURITY DEFINER promotion helper
REVOKE EXECUTE ON FUNCTION public.settle_pending_bank_transaction(UUID, UUID, UUID, TIMESTAMPTZ, BIGINT, VARCHAR) FROM family_app;
DROP FUNCTION IF EXISTS public.settle_pending_bank_transaction(UUID, UUID, UUID, TIMESTAMPTZ, BIGINT, VARCHAR);

-- 2. Drop statement domain tables in dependency order
DROP TABLE IF EXISTS public.statement_reconciliation_overrides CASCADE;
DROP TABLE IF EXISTS public.staged_bank_transactions CASCADE;
DROP TABLE IF EXISTS public.statement_import_batches CASCADE;
DROP TABLE IF EXISTS public.statement_documents CASCADE;

-- 3. Drop immutability trigger functions
DROP FUNCTION IF EXISTS public.trg_enforce_override_immutability();
DROP FUNCTION IF EXISTS public.trg_enforce_statement_document_immutability();

-- 4. Revert composite unique constraint on bank_transactions
ALTER TABLE public.bank_transactions 
    DROP CONSTRAINT IF EXISTS uq_bank_transactions_tenant_account;

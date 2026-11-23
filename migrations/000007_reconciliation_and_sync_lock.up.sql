-- =============================================================================
-- Migration 000007: Reconciliation Discrepancies and Sync Serialization Lock
-- 1. Creates public.reconciliation_discrepancies table with tenant RLS + FORCE RLS.
-- 2. Adds sync concurrency control columns on bank_connections:
--    sync_version BIGINT, active_sync_token UUID, sync_locked_until TIMESTAMPTZ.
-- 3. Grants least-privilege permissions to family_app and family_worker.
-- 4. Creates connection-scoped payload deduplication index on raw_bank_payloads.
-- =============================================================================

-- 1. Reconciliation Discrepancies durable storage
CREATE TABLE IF NOT EXISTS public.reconciliation_discrepancies (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    family_id UUID NOT NULL REFERENCES public.families(id) ON DELETE CASCADE,
    bank_connection_id UUID,
    bank_transaction_id UUID,
    external_id VARCHAR(255) NOT NULL,
    field VARCHAR(50) NOT NULL,
    recorded_value TEXT NOT NULL,
    incoming_value TEXT NOT NULL,
    idempotency_hash VARCHAR(64) NOT NULL,
    detected_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_reconciliation_discrepancies_tenant UNIQUE (family_id, id),
    CONSTRAINT uq_reconciliation_discrepancies_idempotency UNIQUE (family_id, idempotency_hash),
    CONSTRAINT fk_discrepancies_connection FOREIGN KEY (family_id, bank_connection_id) 
        REFERENCES public.bank_connections(family_id, id) ON DELETE CASCADE,
    CONSTRAINT fk_discrepancies_transaction FOREIGN KEY (family_id, bank_transaction_id) 
        REFERENCES public.bank_transactions(family_id, id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_discrepancies_conn ON public.reconciliation_discrepancies(family_id, bank_connection_id);
CREATE INDEX IF NOT EXISTS idx_discrepancies_tx ON public.reconciliation_discrepancies(family_id, bank_transaction_id);

ALTER TABLE public.reconciliation_discrepancies OWNER TO family_admin;
ALTER TABLE public.reconciliation_discrepancies ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.reconciliation_discrepancies FORCE ROW LEVEL SECURITY;

CREATE POLICY rls_reconciliation_discrepancies_select ON public.reconciliation_discrepancies
    FOR SELECT TO family_app, family_worker
    USING (family_id = get_current_family_id());

CREATE POLICY rls_reconciliation_discrepancies_insert ON public.reconciliation_discrepancies
    FOR INSERT TO family_app, family_worker
    WITH CHECK (family_id = get_current_family_id());

GRANT SELECT, INSERT ON public.reconciliation_discrepancies TO family_app, family_worker;

-- 2. Sync serialization columns on bank_connections
ALTER TABLE public.bank_connections 
    ADD COLUMN IF NOT EXISTS sync_version BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS active_sync_token UUID,
    ADD COLUMN IF NOT EXISTS sync_locked_until TIMESTAMPTZ;

-- 3. Grants for family_worker on concurrency control columns
GRANT UPDATE (sync_version, active_sync_token, sync_locked_until) ON public.bank_connections TO family_worker;

-- 4. Connection-scoped atomic deduplication unique index for raw payloads
-- In PostgreSQL 15+, NULLS NOT DISTINCT ensures payload deduplication works correctly
-- for both connection-bound payloads and standalone (NULL connection) payloads.
CREATE UNIQUE INDEX IF NOT EXISTS uq_raw_bank_payloads_conn_hash 
    ON public.raw_bank_payloads(family_id, bank_connection_id, payload_hash)
    NULLS NOT DISTINCT;

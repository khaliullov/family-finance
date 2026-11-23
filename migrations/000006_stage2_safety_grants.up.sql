-- =============================================================================
-- Migration 000006: Stage 2 Safety Grants & Durable Ingestion
-- 1. Preserves verbatim raw sanitized payload bytes (raw_payload_bytes BYTEA).
-- 2. Stores durable pagination checkpoint cursor on bank_connections (sync_cursor TEXT).
-- 3. Grants least-privilege UPDATE on sync_cursor to family_worker.
-- 4. Grants least-privilege UPDATE on locked_until and locked_by on worker_jobs
--    to family_worker for safe lease renewal and stale worker protection.
-- =============================================================================

-- 1. Exact sanitized raw payload byte preservation
ALTER TABLE public.raw_bank_payloads 
    ADD COLUMN IF NOT EXISTS raw_payload_bytes BYTEA;

-- 2. Durable pagination cursor on bank connections
ALTER TABLE public.bank_connections 
    ADD COLUMN IF NOT EXISTS sync_cursor TEXT;

-- 3. Ingestion checkpoint grant for family_worker
GRANT UPDATE (sync_cursor) ON public.bank_connections TO family_worker;

-- 4. Queue lease renewal and ownership management grant for family_worker
GRANT UPDATE (locked_until, locked_by) ON public.worker_jobs TO family_worker;

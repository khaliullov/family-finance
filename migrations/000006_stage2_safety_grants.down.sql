-- =============================================================================
-- Migration 000006 Down: Revert Stage 2 Safety Grants & Durable Ingestion
-- =============================================================================

REVOKE UPDATE (locked_until, locked_by) ON public.worker_jobs FROM family_worker;
REVOKE UPDATE (sync_cursor) ON public.bank_connections FROM family_worker;
ALTER TABLE public.bank_connections DROP COLUMN IF EXISTS sync_cursor;
ALTER TABLE public.raw_bank_payloads DROP COLUMN IF EXISTS raw_payload_bytes;

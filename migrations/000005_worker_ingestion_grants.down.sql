-- =============================================================================
-- Migration 000005 Down: Worker Ingestion Grants
-- =============================================================================

REVOKE UPDATE (scheduled_at) ON public.worker_jobs FROM family_worker;
REVOKE INSERT ON public.accounts FROM family_worker;

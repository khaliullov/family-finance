-- =============================================================================
-- Migration 000005: Worker Ingestion Grants
-- Grants least-privilege INSERT on accounts and UPDATE(scheduled_at) on worker_jobs
-- to family_worker for bank synchronization and job rescheduling.
-- =============================================================================

-- Allow background worker to insert newly discovered bank accounts under RLS
GRANT INSERT ON public.accounts TO family_worker;

-- Allow background worker to update scheduled_at for retry exponential backoff
GRANT UPDATE (scheduled_at) ON public.worker_jobs TO family_worker;

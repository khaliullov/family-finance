-- =============================================================================
-- Migration 000011 (Rollback): Revert Bank Connection States & Validation Reports
-- =============================================================================

DROP TABLE IF EXISTS public.bank_validation_reports CASCADE;

ALTER TABLE public.bank_connections
    DROP COLUMN IF EXISTS last_authenticated_at,
    DROP COLUMN IF EXISTS last_validated_at,
    DROP COLUMN IF EXISTS last_validation_status,
    DROP COLUMN IF EXISTS last_validation_report_id;

ALTER TABLE public.bank_connections ALTER COLUMN status SET DEFAULT 'pending_pairing';

UPDATE public.bank_connections
SET status = 'disconnected'
WHERE status NOT IN ('pending_pairing', 'active', 'expired', 'error', 'disconnected');

ALTER TABLE public.bank_connections DROP CONSTRAINT IF EXISTS bank_connections_status_check;
ALTER TABLE public.bank_connections ADD CONSTRAINT bank_connections_status_check
    CHECK (status IN ('pending_pairing', 'active', 'expired', 'error', 'disconnected'));

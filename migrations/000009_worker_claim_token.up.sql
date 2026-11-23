-- =============================================================================
-- Migration 000009: Worker Claim Token Fencing
-- 1. Adds claim_token UUID to public.worker_jobs for per-claim fencing.
-- 2. Grants least-privilege UPDATE on claim_token to family_worker.
-- 3. Recreates queue_sec.claim_next_worker_job to generate and return claim_token.
-- =============================================================================

-- 1. Add claim_token column to worker_jobs
ALTER TABLE public.worker_jobs 
    ADD COLUMN IF NOT EXISTS claim_token UUID;

-- 2. Grant column-level UPDATE on claim_token to family_worker for lease release/failure
GRANT UPDATE (claim_token) ON public.worker_jobs TO family_worker;

-- 3. Recreate queue_sec.claim_next_worker_job with claim_token return column
DROP FUNCTION IF EXISTS queue_sec.claim_next_worker_job(VARCHAR, VARCHAR, INT);

CREATE OR REPLACE FUNCTION queue_sec.claim_next_worker_job(
    p_queue_name VARCHAR(50),
    p_worker_id VARCHAR(100),
    p_lock_duration_seconds INT DEFAULT 600
)
RETURNS TABLE (
    job_id UUID,
    family_id UUID,
    job_type VARCHAR(100),
    payload JSONB,
    claim_token UUID
)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_job RECORD;
    v_claim_token UUID;
BEGIN
    SELECT wj.id, wj.family_id, wj.job_type, wj.payload
    INTO v_job
    FROM public.worker_jobs wj
    WHERE wj.queue_name = p_queue_name
      AND (
          wj.state = 'available' 
          OR (wj.state = 'running' AND wj.locked_until < NOW())
      )
      AND wj.scheduled_at <= NOW()
    ORDER BY wj.scheduled_at ASC
    FOR UPDATE OF wj SKIP LOCKED
    LIMIT 1;

    IF v_job.id IS NOT NULL THEN
        v_claim_token := gen_random_uuid();

        UPDATE public.worker_jobs
        SET state = 'running',
            locked_by = p_worker_id,
            claim_token = v_claim_token,
            locked_until = NOW() + (p_lock_duration_seconds || ' seconds')::INTERVAL,
            attempts = attempts + 1,
            updated_at = NOW()
        WHERE id = v_job.id;

        RETURN QUERY SELECT v_job.id, v_job.family_id, v_job.job_type, v_job.payload, v_claim_token;
    END IF;
END;
$$;

ALTER FUNCTION queue_sec.claim_next_worker_job(VARCHAR, VARCHAR, INT) OWNER TO family_queue_definer;
REVOKE ALL ON FUNCTION queue_sec.claim_next_worker_job(VARCHAR, VARCHAR, INT) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION queue_sec.claim_next_worker_job(VARCHAR, VARCHAR, INT) TO family_worker;

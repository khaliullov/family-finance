-- =============================================================================
-- Migration 000003 Down: User Authentication Sessions
-- =============================================================================

DROP FUNCTION IF EXISTS auth_sec.revoke_session(VARCHAR);
DROP FUNCTION IF EXISTS auth_sec.rotate_refresh_token(VARCHAR, VARCHAR, TIMESTAMPTZ, VARCHAR, VARCHAR);
DROP TABLE IF EXISTS public.user_sessions CASCADE;

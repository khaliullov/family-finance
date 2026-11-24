-- Rollback Migration 000010: Bank Authentication Attempts
-- Explicitly drops functions, policies, grants, and indexes before dropping table without CASCADE.

DROP FUNCTION IF EXISTS public.consume_telegram_auth_challenge(BIGINT, BIGINT, BIGINT);
DROP FUNCTION IF EXISTS public.resolve_telegram_auth_challenge(BIGINT, BIGINT);

DROP POLICY IF EXISTS rls_bank_auth_attempts_consume ON public.bank_auth_attempts;
DROP POLICY IF EXISTS rls_bank_auth_attempts_lookup ON public.bank_auth_attempts;
DROP POLICY IF EXISTS rls_bank_auth_attempts_delete ON public.bank_auth_attempts;
DROP POLICY IF EXISTS rls_bank_auth_attempts_update ON public.bank_auth_attempts;
DROP POLICY IF EXISTS rls_bank_auth_attempts_insert ON public.bank_auth_attempts;
DROP POLICY IF EXISTS rls_bank_auth_attempts_select ON public.bank_auth_attempts;

REVOKE ALL ON public.bank_auth_attempts FROM family_worker;
REVOKE ALL ON public.bank_auth_attempts FROM family_app;
REVOKE ALL ON public.bank_auth_attempts FROM family_auth_definer;

DROP INDEX IF EXISTS public.idx_auth_attempts_tg_consumed_update;
DROP INDEX IF EXISTS public.idx_auth_attempts_expires;
DROP INDEX IF EXISTS public.idx_auth_attempts_conn;
DROP INDEX IF EXISTS public.idx_auth_attempts_tg_active;

DROP TABLE IF EXISTS public.bank_auth_attempts;

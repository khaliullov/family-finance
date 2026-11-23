-- =============================================================================
-- Migration 000008 Down: Revoke Worker Hold Cancellation Grant
-- =============================================================================

REVOKE UPDATE (classification) ON public.accounting_entries FROM family_worker;

-- =============================================================================
-- Migration 000008: Worker Hold Cancellation Grant
-- Grants UPDATE on classification column of accounting_entries to family_worker
-- to allow marking cancelled authorization holds as 'excluded'.
-- =============================================================================

GRANT UPDATE (classification) ON public.accounting_entries TO family_worker;

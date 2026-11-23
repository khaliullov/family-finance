-- =============================================================================
-- Migration 000004 Down: Card Closure & Primary Invariant Constraints
-- =============================================================================

DROP INDEX IF EXISTS public.uq_cards_one_primary_per_account;

ALTER TABLE public.cards
    DROP CONSTRAINT IF EXISTS chk_cards_closed_not_primary;

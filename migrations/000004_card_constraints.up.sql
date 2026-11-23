-- =============================================================================
-- Migration 000004: Card Closure & Primary Invariant Constraints
-- =============================================================================

-- 1. Ensure closed cards can never be marked as primary
ALTER TABLE public.cards
    DROP CONSTRAINT IF EXISTS chk_cards_closed_not_primary;

ALTER TABLE public.cards
    ADD CONSTRAINT chk_cards_closed_not_primary
    CHECK (NOT (status = 'CLOSED' AND is_primary = TRUE));

-- 2. Ensure at most one primary card per account within each family tenant
CREATE UNIQUE INDEX IF NOT EXISTS uq_cards_one_primary_per_account
    ON public.cards(family_id, account_id)
    WHERE is_primary = TRUE;

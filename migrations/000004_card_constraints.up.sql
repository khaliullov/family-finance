-- =============================================================================
-- Migration 000004: Card Closure & Primary Invariant Constraints
-- =============================================================================

-- -----------------------------------------------------------------------------
-- Step 1: Deterministic Legacy Data Repair (Option A)
-- -----------------------------------------------------------------------------

-- 1a. Reconcile closed cards: closed cards must never be marked as primary
UPDATE public.cards
SET is_primary = FALSE, updated_at = NOW()
WHERE status = 'CLOSED' AND is_primary = TRUE;

-- 1b. Reconcile duplicate primary cards per account:
-- If historical data contains multiple primary cards for an account,
-- retain the single most recently updated card (tie-broken by created_at DESC, id DESC)
-- and clear is_primary on all others.
WITH ranked_primaries AS (
    SELECT id,
           ROW_NUMBER() OVER (
               PARTITION BY family_id, account_id 
               ORDER BY updated_at DESC, created_at DESC, id DESC
           ) AS rn
    FROM public.cards
    WHERE is_primary = TRUE
)
UPDATE public.cards c
SET is_primary = FALSE, updated_at = NOW()
FROM ranked_primaries r
WHERE c.id = r.id AND r.rn > 1;

-- -----------------------------------------------------------------------------
-- Step 2: Install Schema Constraints
-- -----------------------------------------------------------------------------

-- 2a. Ensure closed cards can never be marked as primary
ALTER TABLE public.cards
    DROP CONSTRAINT IF EXISTS chk_cards_closed_not_primary;

ALTER TABLE public.cards
    ADD CONSTRAINT chk_cards_closed_not_primary
    CHECK (NOT (status = 'CLOSED' AND is_primary = TRUE));

-- 2b. Ensure at most one primary card per account within each family tenant
CREATE UNIQUE INDEX IF NOT EXISTS uq_cards_one_primary_per_account
    ON public.cards(family_id, account_id)
    WHERE is_primary = TRUE;

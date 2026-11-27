-- Migration 000014: Statement Cancellation Lifecycle & Promotion Database Guard
-- Enforces explicit cancellation resolution state and prevents promotion of cancelled operations at database level.

-- 1. Expand staged_bank_transactions resolution_action to include explicit 'skip_cancelled'
ALTER TABLE public.staged_bank_transactions
    DROP CONSTRAINT IF EXISTS staged_bank_transactions_resolution_action_check;

ALTER TABLE public.staged_bank_transactions
    ADD CONSTRAINT staged_bank_transactions_resolution_action_check
    CHECK (resolution_action IN ('pending', 'commit', 'skip_duplicate', 'skip_cancelled', 'apply_settlement'));

-- 2. Clean up any historical cancelled rows with invalid commit state
UPDATE public.staged_bank_transactions
SET resolution_action = 'skip_cancelled'
WHERE booking_status = 'cancelled' AND resolution_action = 'commit';

-- 3. Database Guard: Cancelled staged transactions can NEVER have resolution_action = 'commit'
ALTER TABLE public.staged_bank_transactions
    DROP CONSTRAINT IF EXISTS chk_staged_cancelled_no_commit;

ALTER TABLE public.staged_bank_transactions
    ADD CONSTRAINT chk_staged_cancelled_no_commit
    CHECK (booking_status != 'cancelled' OR resolution_action != 'commit');

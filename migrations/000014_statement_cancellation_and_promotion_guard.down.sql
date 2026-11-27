-- Migration 000014 Down: Rollback Statement Cancellation Lifecycle & Promotion Guard

ALTER TABLE public.staged_bank_transactions
    DROP CONSTRAINT IF EXISTS chk_staged_cancelled_no_commit;

-- Temporarily bypass lifecycle trigger to map 'skip_cancelled' back to 'skip_duplicate'
ALTER TABLE public.staged_bank_transactions DISABLE TRIGGER trg_staged_tx_lifecycle;

UPDATE public.staged_bank_transactions
SET resolution_action = 'skip_duplicate'
WHERE resolution_action = 'skip_cancelled';

ALTER TABLE public.staged_bank_transactions ENABLE TRIGGER trg_staged_tx_lifecycle;

ALTER TABLE public.staged_bank_transactions
    DROP CONSTRAINT IF EXISTS staged_bank_transactions_resolution_action_check;

ALTER TABLE public.staged_bank_transactions
    ADD CONSTRAINT staged_bank_transactions_resolution_action_check
    CHECK (resolution_action IN ('pending', 'commit', 'skip_duplicate', 'apply_settlement'));

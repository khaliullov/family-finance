-- Migration 000016 Down: Revert Statement Financial Integrity Remediation

REVOKE EXECUTE ON FUNCTION public.cancel_pending_bank_transaction(UUID, UUID, UUID) FROM family_app;
DROP FUNCTION IF EXISTS public.cancel_pending_bank_transaction(UUID, UUID, UUID);

-- Map any rows with status outside v15 back
ALTER TABLE public.staged_bank_transactions DISABLE TRIGGER trg_staged_tx_lifecycle;

UPDATE public.staged_bank_transactions
SET resolution_action = 'skip_cancelled'
WHERE resolution_action = 'release_pending';

UPDATE public.staged_bank_transactions
SET duplicate_status = 'confirmed_duplicate'
WHERE duplicate_status = 'cancellation_update';

ALTER TABLE public.staged_bank_transactions ENABLE TRIGGER trg_staged_tx_lifecycle;

ALTER TABLE public.staged_bank_transactions
    DROP CONSTRAINT IF EXISTS staged_bank_transactions_duplicate_status_check;

ALTER TABLE public.staged_bank_transactions
    ADD CONSTRAINT staged_bank_transactions_duplicate_status_check
    CHECK (duplicate_status IN ('new', 'confirmed_duplicate', 'potential_duplicate', 'settlement_update'));

ALTER TABLE public.staged_bank_transactions
    DROP CONSTRAINT IF EXISTS staged_bank_transactions_resolution_action_check;

ALTER TABLE public.staged_bank_transactions
    ADD CONSTRAINT staged_bank_transactions_resolution_action_check
    CHECK (resolution_action IN ('pending', 'commit', 'skip_duplicate', 'skip_cancelled', 'apply_settlement'));

ALTER TABLE public.staged_bank_transactions
    DROP CONSTRAINT IF EXISTS fk_staged_tx_category;

ALTER TABLE public.staged_bank_transactions
    ADD CONSTRAINT fk_staged_tx_category FOREIGN KEY (family_id, category_id)
    REFERENCES public.categories(family_id, id) ON DELETE SET NULL;

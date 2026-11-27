-- Migration 000015 Down: Revert Statement Lifecycle, Revision & Category Review

ALTER TABLE public.staged_bank_transactions
    DROP CONSTRAINT IF EXISTS fk_staged_tx_category;

ALTER TABLE public.staged_bank_transactions
    DROP COLUMN IF EXISTS category_id;

ALTER TABLE public.statement_import_batches
    DROP COLUMN IF EXISTS approved_batch_revision;

ALTER TABLE public.statement_import_batches
    DROP COLUMN IF EXISTS batch_revision;

-- Map any batches with status outside v14 back to 'rejected'
UPDATE public.statement_import_batches
SET status = 'rejected'
WHERE status IN ('approved', 'promoting', 'cancelled');

ALTER TABLE public.statement_import_batches
    DROP CONSTRAINT IF EXISTS statement_import_batches_status_check;

ALTER TABLE public.statement_import_batches
    ADD CONSTRAINT statement_import_batches_status_check
    CHECK (status IN ('uploaded', 'parsing', 'staged', 'committed', 'rejected', 'failed'));

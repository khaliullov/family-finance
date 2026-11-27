-- Migration 000015: Statement Lifecycle, Batch Revision & Staged Transaction Category Review
-- Supports explicit lifecycle states ('approved', 'promoting', 'cancelled'),
-- revision-controlled approval snapshots, and categorized staged transactions.

-- 1. Expand statement_import_batches status to include 'approved', 'promoting', 'cancelled'
ALTER TABLE public.statement_import_batches
    DROP CONSTRAINT IF EXISTS statement_import_batches_status_check;

ALTER TABLE public.statement_import_batches
    ADD CONSTRAINT statement_import_batches_status_check
    CHECK (status IN (
        'uploaded', 'parsing', 'staged', 'approved', 'promoting', 'committed', 'rejected', 'cancelled', 'failed'
    ));

-- 2. Add batch_revision and approved_batch_revision to statement_import_batches
ALTER TABLE public.statement_import_batches
    ADD COLUMN IF NOT EXISTS batch_revision INT NOT NULL DEFAULT 1 CHECK (batch_revision >= 1),
    ADD COLUMN IF NOT EXISTS approved_batch_revision INT CHECK (approved_batch_revision IS NULL OR approved_batch_revision >= 1);

-- 3. Add category_id to staged_bank_transactions with tenant foreign key
ALTER TABLE public.staged_bank_transactions
    ADD COLUMN IF NOT EXISTS category_id UUID;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'fk_staged_tx_category'
    ) THEN
        ALTER TABLE public.staged_bank_transactions
            ADD CONSTRAINT fk_staged_tx_category FOREIGN KEY (family_id, category_id)
            REFERENCES public.categories(family_id, id) ON DELETE SET NULL;
    END IF;
END $$;

-- 4. Ensure privileges on new columns
GRANT SELECT, INSERT, UPDATE, DELETE ON public.statement_import_batches TO family_app;
GRANT SELECT, UPDATE (status, error_code, error_message, updated_at) ON public.statement_import_batches TO family_worker;

GRANT SELECT, INSERT, UPDATE, DELETE ON public.staged_bank_transactions TO family_app;
GRANT SELECT, UPDATE (duplicate_status, resolution_action) ON public.staged_bank_transactions TO family_worker;

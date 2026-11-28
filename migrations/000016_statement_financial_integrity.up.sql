-- Migration 000016: Statement Financial Integrity Remediation
-- 1. Fix composite category foreign key: target category_id only on delete set null
-- 2. Expand resolution_action to support 'release_pending'
-- 3. Expand duplicate_status to support 'cancellation_update'
-- 4. Add SECURITY DEFINER routine for atomic pending authorization cancellation

-- 1. Fix fk_staged_tx_category
ALTER TABLE public.staged_bank_transactions
    DROP CONSTRAINT IF EXISTS fk_staged_tx_category;

ALTER TABLE public.staged_bank_transactions
    ADD CONSTRAINT fk_staged_tx_category FOREIGN KEY (family_id, category_id)
    REFERENCES public.categories(family_id, id) ON DELETE SET NULL (category_id);

-- 2. Expand resolution_action
ALTER TABLE public.staged_bank_transactions
    DROP CONSTRAINT IF EXISTS staged_bank_transactions_resolution_action_check;

ALTER TABLE public.staged_bank_transactions
    ADD CONSTRAINT staged_bank_transactions_resolution_action_check
    CHECK (resolution_action IN (
        'pending', 'commit', 'skip_duplicate', 'skip_cancelled', 'apply_settlement', 'release_pending'
    ));

-- 3. Expand duplicate_status
ALTER TABLE public.staged_bank_transactions
    DROP CONSTRAINT IF EXISTS staged_bank_transactions_duplicate_status_check;

ALTER TABLE public.staged_bank_transactions
    ADD CONSTRAINT staged_bank_transactions_duplicate_status_check
    CHECK (duplicate_status IN (
        'new', 'confirmed_duplicate', 'potential_duplicate', 'settlement_update', 'cancellation_update'
    ));

-- 4. SECURITY DEFINER Function: Atomic Pending Authorization Cancellation & Release
CREATE OR REPLACE FUNCTION public.cancel_pending_bank_transaction(
    p_family_id UUID,
    p_account_id UUID,
    p_transaction_id UUID
)
RETURNS VOID
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_current_family_id UUID;
    v_booking_status VARCHAR(20);
    v_account_id UUID;
BEGIN
    -- 1. Parameter nullity checks
    IF p_family_id IS NULL THEN
        RAISE EXCEPTION 'p_family_id cannot be null';
    END IF;
    IF p_account_id IS NULL THEN
        RAISE EXCEPTION 'p_account_id cannot be null';
    END IF;
    IF p_transaction_id IS NULL THEN
        RAISE EXCEPTION 'p_transaction_id cannot be null';
    END IF;

    -- 2. Strict tenant context verification
    v_current_family_id := NULLIF(current_setting('app.current_family_id', true), '')::UUID;
    IF v_current_family_id IS NULL OR v_current_family_id IS DISTINCT FROM p_family_id THEN
        RAISE EXCEPTION 'Tenant security violation: current setting (%) does not match parameter (%)',
            v_current_family_id, p_family_id;
    END IF;

    -- 3. Verify existence and acquire row lock
    SELECT booking_status, account_id
    INTO v_booking_status, v_account_id
    FROM public.bank_transactions
    WHERE family_id = p_family_id AND id = p_transaction_id
    FOR UPDATE;

    IF NOT FOUND THEN
        RAISE EXCEPTION 'Transaction % not found for family %', p_transaction_id, p_family_id;
    END IF;

    IF v_account_id IS DISTINCT FROM p_account_id THEN
        RAISE EXCEPTION 'Account mismatch: transaction % belongs to account %, not %',
            p_transaction_id, v_account_id, p_account_id;
    END IF;

    -- Idempotent handling: if already cancelled, exit cleanly
    IF v_booking_status = 'cancelled' THEN
        RETURN;
    END IF;

    IF v_booking_status IS DISTINCT FROM 'pending' THEN
        RAISE EXCEPTION 'Cannot cancel transaction %: status is %, expected pending', p_transaction_id, v_booking_status;
    END IF;

    -- 4. Atomic transition of bank_transactions (trg_enforce_bank_evidence_immutability permits pending -> cancelled)
    UPDATE public.bank_transactions
    SET booking_status = 'cancelled',
        updated_at = NOW()
    WHERE family_id = p_family_id AND id = p_transaction_id AND booking_status = 'pending';

    -- 5. Atomic synchronization of matching accounting_entries (neutralize ledger impact by setting classification to 'excluded')
    UPDATE public.accounting_entries
    SET classification = 'excluded',
        updated_at = NOW()
    WHERE family_id = p_family_id
      AND bank_transaction_id = p_transaction_id;
END;
$$;

ALTER FUNCTION public.cancel_pending_bank_transaction(UUID, UUID, UUID) OWNER TO family_worker;
REVOKE ALL ON FUNCTION public.cancel_pending_bank_transaction(UUID, UUID, UUID) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.cancel_pending_bank_transaction(UUID, UUID, UUID) TO family_app;

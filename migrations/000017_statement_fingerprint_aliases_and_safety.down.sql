-- Migration 000017 Down: Revert Statement Fingerprint Aliases & Safety
-- 1. Fail-closed safety check: prevent downgrade if active staged rows have release_pending or cancellation_update
DO $$
DECLARE
    v_staged_count INT;
BEGIN
    SELECT COUNT(*) INTO v_staged_count
    FROM public.staged_bank_transactions s
    JOIN public.statement_import_batches b ON b.id = s.batch_id
    WHERE b.status IN ('uploaded', 'parsing', 'staged', 'approved')
      AND (s.resolution_action = 'release_pending' OR s.duplicate_status = 'cancellation_update');

    IF v_staged_count > 0 THEN
        RAISE EXCEPTION 'Cannot downgrade migration 000017: % active staged row(s) with release_pending or cancellation_update exist. Promote or cancel batches before downgrading.', v_staged_count;
    END IF;
END $$;

-- 2. Restore settle_pending_bank_transaction to 6-parameter version
DROP FUNCTION IF EXISTS public.settle_pending_bank_transaction(UUID, UUID, UUID, TIMESTAMPTZ, BIGINT, VARCHAR, VARCHAR);

CREATE OR REPLACE FUNCTION public.settle_pending_bank_transaction(
    p_family_id UUID,
    p_account_id UUID,
    p_transaction_id UUID,
    p_settled_time TIMESTAMPTZ,
    p_settled_amount_minor BIGINT,
    p_external_id VARCHAR
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
    v_account_amount_minor BIGINT;
    v_operation_type VARCHAR(30);
    v_is_split BOOLEAN;
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
    IF p_settled_time IS NULL THEN
        RAISE EXCEPTION 'p_settled_time cannot be null';
    END IF;
    IF p_settled_amount_minor IS NULL THEN
        RAISE EXCEPTION 'p_settled_amount_minor cannot be null';
    END IF;

    -- 2. Strict tenant context verification
    v_current_family_id := NULLIF(current_setting('app.current_family_id', true), '')::UUID;
    IF v_current_family_id IS NULL OR v_current_family_id IS DISTINCT FROM p_family_id THEN
        RAISE EXCEPTION 'Tenant security violation: current setting (%) does not match parameter (%)',
            v_current_family_id, p_family_id;
    END IF;

    -- 3. Verify existence, account ownership, and pending status under lock
    SELECT booking_status, account_id, account_amount_minor, operation_type
    INTO v_booking_status, v_account_id, v_account_amount_minor, v_operation_type
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

    IF v_booking_status IS DISTINCT FROM 'pending' THEN
        RAISE EXCEPTION 'Transaction % is in status %, not pending', p_transaction_id, v_booking_status;
    END IF;

    IF p_settled_amount_minor = 0 AND v_operation_type != 'card_check' THEN
        RAISE EXCEPTION 'settled_amount_minor cannot be zero for operation type %', v_operation_type;
    END IF;

    -- Validate sign preservation: cannot flip credit to debit or debit to credit
    IF (v_account_amount_minor < 0 AND p_settled_amount_minor > 0) OR
       (v_account_amount_minor > 0 AND p_settled_amount_minor < 0) THEN
        RAISE EXCEPTION 'Settlement sign flip prohibited: % -> %', v_account_amount_minor, p_settled_amount_minor;
    END IF;

    -- 4. Check for user-split accounting entries
    SELECT EXISTS (
        SELECT 1 FROM public.accounting_entries
        WHERE family_id = p_family_id
          AND bank_transaction_id = p_transaction_id
          AND is_split = TRUE
    ) INTO v_is_split;

    IF v_is_split AND p_settled_amount_minor != v_account_amount_minor THEN
        RAISE EXCEPTION 'Cannot settle split transaction % with amount change (% -> %): manual rebalance required',
            p_transaction_id, v_account_amount_minor, p_settled_amount_minor;
    END IF;

    -- 5. Atomic transition of bank_transactions
    UPDATE public.bank_transactions
    SET booking_status = 'posted',
        posted_time = p_settled_time,
        account_amount_minor = p_settled_amount_minor,
        external_id = COALESCE(p_external_id, external_id),
        updated_at = NOW()
    WHERE family_id = p_family_id AND id = p_transaction_id AND booking_status = 'pending';

    -- 6. Atomic synchronization of matching unsplit accounting_entries
    IF NOT v_is_split THEN
        UPDATE public.accounting_entries
        SET amount_minor = p_settled_amount_minor,
            updated_at = NOW()
        WHERE family_id = p_family_id
          AND bank_transaction_id = p_transaction_id
          AND is_split = FALSE;
    END IF;
END;
$$;

ALTER FUNCTION public.settle_pending_bank_transaction(UUID, UUID, UUID, TIMESTAMPTZ, BIGINT, VARCHAR) OWNER TO family_worker;
REVOKE ALL ON FUNCTION public.settle_pending_bank_transaction(UUID, UUID, UUID, TIMESTAMPTZ, BIGINT, VARCHAR) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.settle_pending_bank_transaction(UUID, UUID, UUID, TIMESTAMPTZ, BIGINT, VARCHAR) TO family_app;

-- 3. Drop bank_transaction_fingerprint_aliases table
DROP TABLE IF EXISTS public.bank_transaction_fingerprint_aliases;

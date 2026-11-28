-- Migration 000017: Statement Fingerprint Aliases & Migration Safety
-- 1. Create bank_transaction_fingerprint_aliases table to preserve historical & settled fingerprints
-- 2. Backfill existing transaction fingerprints into aliases
-- 3. Upgrade settle_pending_bank_transaction to atomically register posted fingerprint aliases

-- 1. Create table bank_transaction_fingerprint_aliases
CREATE TABLE public.bank_transaction_fingerprint_aliases (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    family_id UUID NOT NULL REFERENCES public.families(id) ON DELETE CASCADE,
    account_id UUID NOT NULL,
    bank_transaction_id UUID NOT NULL,
    fingerprint_hash VARCHAR(64) NOT NULL,
    alias_type VARCHAR(30) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT fk_bt_fingerprint_aliases_account
        FOREIGN KEY (family_id, account_id)
        REFERENCES public.accounts(family_id, id) ON DELETE CASCADE,
    CONSTRAINT fk_bt_fingerprint_aliases_tx
        FOREIGN KEY (family_id, bank_transaction_id)
        REFERENCES public.bank_transactions(family_id, id) ON DELETE CASCADE,
    CONSTRAINT uq_bt_fingerprint_aliases
        UNIQUE (family_id, account_id, fingerprint_hash, bank_transaction_id),
    CONSTRAINT chk_bt_fingerprint_hash
        CHECK (length(trim(fingerprint_hash)) > 0),
    CONSTRAINT chk_bt_alias_type
        CHECK (alias_type IN ('initial_pending', 'initial_posted', 'settled_posted', 'manual_alias'))
);

CREATE INDEX idx_bt_fingerprint_aliases_lookup
    ON public.bank_transaction_fingerprint_aliases (family_id, account_id, fingerprint_hash);

-- Forced Row Level Security
ALTER TABLE public.bank_transaction_fingerprint_aliases ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.bank_transaction_fingerprint_aliases FORCE ROW LEVEL SECURITY;

CREATE POLICY rls_bt_fingerprint_aliases_all
    ON public.bank_transaction_fingerprint_aliases
    TO family_app, family_worker
    USING (family_id = get_current_family_id())
    WITH CHECK (family_id = get_current_family_id());

GRANT SELECT, INSERT, UPDATE, DELETE ON public.bank_transaction_fingerprint_aliases TO family_app, family_worker;

-- 2. Backfill existing transaction fingerprints
INSERT INTO public.bank_transaction_fingerprint_aliases (
    family_id, account_id, bank_transaction_id, fingerprint_hash, alias_type, created_at
)
SELECT family_id, account_id, id, fingerprint_hash,
       CASE WHEN booking_status = 'pending' THEN 'initial_pending' ELSE 'initial_posted' END,
       created_at
FROM public.bank_transactions
ON CONFLICT (family_id, account_id, fingerprint_hash, bank_transaction_id) DO NOTHING;

-- 3. Upgrade settle_pending_bank_transaction to 7-parameter version with p_posted_fingerprint_hash
DROP FUNCTION IF EXISTS public.settle_pending_bank_transaction(UUID, UUID, UUID, TIMESTAMPTZ, BIGINT, VARCHAR);

CREATE OR REPLACE FUNCTION public.settle_pending_bank_transaction(
    p_family_id UUID,
    p_account_id UUID,
    p_transaction_id UUID,
    p_settled_time TIMESTAMPTZ,
    p_settled_amount_minor BIGINT,
    p_external_id VARCHAR,
    p_posted_fingerprint_hash VARCHAR DEFAULT NULL
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
    v_orig_fingerprint VARCHAR(64);
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
    SELECT booking_status, account_id, account_amount_minor, operation_type, fingerprint_hash
    INTO v_booking_status, v_account_id, v_account_amount_minor, v_operation_type, v_orig_fingerprint
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

    -- 5. Atomic transition of bank_transactions (trg_enforce_bank_evidence_immutability executes as family_admin)
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

    -- 7. Register original pending fingerprint in alias table if not already present
    IF v_orig_fingerprint IS NOT NULL AND length(trim(v_orig_fingerprint)) > 0 THEN
        INSERT INTO public.bank_transaction_fingerprint_aliases (
            family_id, account_id, bank_transaction_id, fingerprint_hash, alias_type
        ) VALUES (
            p_family_id, p_account_id, p_transaction_id, trim(v_orig_fingerprint), 'initial_pending'
        ) ON CONFLICT (family_id, account_id, fingerprint_hash, bank_transaction_id) DO NOTHING;
    END IF;

    -- 8. Register settled posted fingerprint in alias table
    IF p_posted_fingerprint_hash IS NOT NULL AND length(trim(p_posted_fingerprint_hash)) > 0 THEN
        INSERT INTO public.bank_transaction_fingerprint_aliases (
            family_id, account_id, bank_transaction_id, fingerprint_hash, alias_type
        ) VALUES (
            p_family_id, p_account_id, p_transaction_id, trim(p_posted_fingerprint_hash), 'settled_posted'
        ) ON CONFLICT (family_id, account_id, fingerprint_hash, bank_transaction_id) DO NOTHING;
    END IF;
END;
$$;

ALTER FUNCTION public.settle_pending_bank_transaction(UUID, UUID, UUID, TIMESTAMPTZ, BIGINT, VARCHAR, VARCHAR) OWNER TO family_worker;
REVOKE ALL ON FUNCTION public.settle_pending_bank_transaction(UUID, UUID, UUID, TIMESTAMPTZ, BIGINT, VARCHAR, VARCHAR) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.settle_pending_bank_transaction(UUID, UUID, UUID, TIMESTAMPTZ, BIGINT, VARCHAR, VARCHAR) TO family_app;

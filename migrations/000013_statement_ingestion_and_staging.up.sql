-- =============================================================================
-- Migration 000013: Generic Statement Ingestion, Staging, and Audit Architecture
-- =============================================================================

-- 1. Encrypted Original Statement Documents
CREATE TABLE public.statement_documents (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    family_id UUID NOT NULL REFERENCES public.families(id) ON DELETE CASCADE,
    uploaded_by_user_id UUID NOT NULL,
    original_filename VARCHAR(255) NOT NULL,
    file_size_bytes BIGINT NOT NULL CHECK (file_size_bytes > 0 AND file_size_bytes <= 10485760),
    content_hash VARCHAR(64) NOT NULL CHECK (content_hash ~ '^[0-9a-f]{64}$'),
    mime_type VARCHAR(100) NOT NULL,
    encrypted_content BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT uq_statement_documents_tenant UNIQUE (family_id, id),
    CONSTRAINT fk_statement_documents_uploader FOREIGN KEY (family_id, uploaded_by_user_id)
        REFERENCES public.family_members(family_id, user_id) ON DELETE RESTRICT
);

CREATE INDEX idx_statement_documents_hash 
    ON public.statement_documents(family_id, content_hash);

-- Trigger: Document Immutability
CREATE OR REPLACE FUNCTION trg_enforce_statement_document_immutability()
RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'UPDATE' THEN
        RAISE EXCEPTION 'statement_documents records are append-only and strictly immutable';
    ELSIF TG_OP = 'DELETE' THEN
        IF CURRENT_USER NOT IN ('family_admin', 'postgres') THEN
            RAISE EXCEPTION 'statement_documents deletion is prohibited for role %', CURRENT_USER;
        END IF;
    END IF;
    RETURN OLD;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_immutability_statement_documents
BEFORE UPDATE OR DELETE ON public.statement_documents
FOR EACH ROW EXECUTE FUNCTION trg_enforce_statement_document_immutability();

-- 2. Statement Import Batches
CREATE TABLE public.statement_import_batches (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    family_id UUID NOT NULL REFERENCES public.families(id) ON DELETE CASCADE,
    account_id UUID NOT NULL,
    document_id UUID NOT NULL,
    source_format VARCHAR(30) NOT NULL,
    parser_version VARCHAR(50) NOT NULL,
    status VARCHAR(30) NOT NULL CHECK (status IN (
        'uploaded', 'parsing', 'staged', 'committed', 'rejected', 'failed'
    )),
    reconciliation_status VARCHAR(30) NOT NULL DEFAULT 'non_reconcilable' CHECK (reconciliation_status IN (
        'reconciled', 'discrepancy', 'non_reconcilable'
    )),
    period_start_date TIMESTAMPTZ,
    period_end_date TIMESTAMPTZ,
    total_rows INT NOT NULL DEFAULT 0 CHECK (total_rows >= 0 AND total_rows <= 50000),
    staged_count INT NOT NULL DEFAULT 0 CHECK (staged_count >= 0),
    duplicate_count INT NOT NULL DEFAULT 0 CHECK (duplicate_count >= 0),
    discrepancy_count INT NOT NULL DEFAULT 0 CHECK (discrepancy_count >= 0),
    opening_balance_minor BIGINT,
    closing_balance_minor BIGINT,
    calculated_balance_minor BIGINT,
    approved_by_user_id UUID,
    approved_at TIMESTAMPTZ,
    error_code VARCHAR(50),
    error_message TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT uq_statement_import_batches_tenant UNIQUE (family_id, id),
    CONSTRAINT uq_statement_import_batches_account UNIQUE (family_id, id, account_id),
    CONSTRAINT fk_statement_batches_account FOREIGN KEY (family_id, account_id)
        REFERENCES public.accounts(family_id, id) ON DELETE RESTRICT,
    CONSTRAINT fk_statement_batches_doc FOREIGN KEY (family_id, document_id)
        REFERENCES public.statement_documents(family_id, id) ON DELETE RESTRICT,
    CONSTRAINT fk_statement_batches_approver FOREIGN KEY (family_id, approved_by_user_id)
        REFERENCES public.family_members(family_id, user_id) ON DELETE RESTRICT,
    CONSTRAINT chk_period_order CHECK (period_end_date IS NULL OR period_start_date IS NULL OR period_end_date >= period_start_date)
);

CREATE INDEX idx_statement_batches_account_status 
    ON public.statement_import_batches(family_id, account_id, status);

-- 3. Composite Tenant Unique Constraint on bank_transactions for cross-account FK isolation
ALTER TABLE public.bank_transactions 
    ADD CONSTRAINT uq_bank_transactions_tenant_account UNIQUE (family_id, account_id, id);

-- 4. Staged Bank Transactions
CREATE TABLE public.staged_bank_transactions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    family_id UUID NOT NULL REFERENCES public.families(id) ON DELETE CASCADE,
    batch_id UUID NOT NULL,
    account_id UUID NOT NULL,
    source_external_id VARCHAR(100),
    card_mask VARCHAR(20),
    row_index INT NOT NULL CHECK (row_index >= 1),
    transaction_time TIMESTAMPTZ NOT NULL,
    settled_time TIMESTAMPTZ,
    amount_minor BIGINT NOT NULL,
    currency VARCHAR(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    account_amount_minor BIGINT NOT NULL,
    account_currency VARCHAR(3) NOT NULL CHECK (account_currency ~ '^[A-Z]{3}$'),
    operation_type VARCHAR(30) NOT NULL,
    booking_status VARCHAR(20) NOT NULL CHECK (booking_status IN ('pending', 'posted', 'cancelled')),
    raw_description TEXT NOT NULL,
    merchant_name VARCHAR(255),
    mcc INT,
    bank_category VARCHAR(100),
    import_fingerprint VARCHAR(64) NOT NULL CHECK (import_fingerprint ~ '^[0-9a-f]{64}$'),
    duplicate_status VARCHAR(30) NOT NULL DEFAULT 'new' CHECK (duplicate_status IN (
        'new', 'confirmed_duplicate', 'potential_duplicate', 'settlement_update'
    )),
    matched_existing_tx_id UUID,
    resolution_action VARCHAR(30) NOT NULL DEFAULT 'pending' CHECK (resolution_action IN (
        'pending', 'commit', 'skip_duplicate', 'apply_settlement'
    )),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT uq_staged_tx_tenant UNIQUE (family_id, id),
    CONSTRAINT uq_staged_tx_batch_row UNIQUE (family_id, batch_id, row_index),
    -- Foreign key enforcing staged transaction account strictly equals batch account
    CONSTRAINT fk_staged_tx_batch_account FOREIGN KEY (family_id, batch_id, account_id)
        REFERENCES public.statement_import_batches(family_id, id, account_id) ON DELETE CASCADE,
    -- Foreign key enforcing matched existing transaction belongs to the exact same family and account
    CONSTRAINT fk_staged_tx_matched_account FOREIGN KEY (family_id, account_id, matched_existing_tx_id)
        REFERENCES public.bank_transactions(family_id, account_id, id) ON DELETE RESTRICT
);

CREATE INDEX idx_staged_tx_batch_status 
    ON public.staged_bank_transactions(family_id, batch_id, duplicate_status);

-- 5. Immutable Reconciliation Override Audit Trail
CREATE TABLE public.statement_reconciliation_overrides (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    family_id UUID NOT NULL REFERENCES public.families(id) ON DELETE CASCADE,
    batch_id UUID NOT NULL,
    overridden_by_user_id UUID NOT NULL,
    opening_balance_minor BIGINT NOT NULL,
    closing_balance_minor BIGINT NOT NULL,
    calculated_balance_minor BIGINT NOT NULL,
    discrepancy_delta_minor BIGINT NOT NULL,
    reason TEXT NOT NULL CHECK (length(trim(reason)) >= 10),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT uq_reconciliation_overrides_tenant UNIQUE (family_id, id),
    CONSTRAINT fk_overrides_batch FOREIGN KEY (family_id, batch_id)
        REFERENCES public.statement_import_batches(family_id, id) ON DELETE RESTRICT,
    CONSTRAINT fk_overrides_user FOREIGN KEY (family_id, overridden_by_user_id)
        REFERENCES public.family_members(family_id, user_id) ON DELETE RESTRICT
);

-- Trigger: Reconciliation Override Immutability
CREATE OR REPLACE FUNCTION trg_enforce_override_immutability()
RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'UPDATE' THEN
        RAISE EXCEPTION 'statement_reconciliation_overrides records are append-only and strictly immutable';
    ELSIF TG_OP = 'DELETE' THEN
        IF CURRENT_USER NOT IN ('family_admin', 'postgres') THEN
            RAISE EXCEPTION 'statement_reconciliation_overrides deletion is prohibited for role %', CURRENT_USER;
        END IF;
    END IF;
    RETURN OLD;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_immutability_reconciliation_overrides
BEFORE UPDATE OR DELETE ON public.statement_reconciliation_overrides
FOR EACH ROW EXECUTE FUNCTION trg_enforce_override_immutability();

-- 6. SECURITY DEFINER Function: Atomic Pending-to-Posted Promotion
CREATE OR REPLACE FUNCTION public.settle_pending_bank_transaction(
    p_family_id UUID,
    p_account_id UUID,
    p_transaction_id UUID,
    p_settled_time TIMESTAMPTZ,
    p_settled_amount_minor BIGINT,
    p_external_id VARCHAR(100)
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
BEGIN
    -- 1. Strict tenant context verification
    v_current_family_id := NULLIF(current_setting('app.current_family_id', true), '')::UUID;
    IF v_current_family_id IS NULL OR v_current_family_id != p_family_id THEN
        RAISE EXCEPTION 'Tenant security violation: current setting (%) does not match parameter (%)',
            v_current_family_id, p_family_id;
    END IF;

    -- 2. Verify existence, account ownership, and pending status under lock
    SELECT booking_status, account_id, account_amount_minor
    INTO v_booking_status, v_account_id, v_account_amount_minor
    FROM public.bank_transactions
    WHERE family_id = p_family_id AND id = p_transaction_id
    FOR UPDATE;

    IF NOT FOUND THEN
        RAISE EXCEPTION 'Transaction % not found for family %', p_transaction_id, p_family_id;
    END IF;

    IF v_account_id != p_account_id THEN
        RAISE EXCEPTION 'Account mismatch: transaction % belongs to account %, not %',
            p_transaction_id, v_account_id, p_account_id;
    END IF;

    IF v_booking_status != 'pending' THEN
        RAISE EXCEPTION 'Transaction % is in status %, not pending', p_transaction_id, v_booking_status;
    END IF;

    IF p_settled_time IS NULL THEN
        RAISE EXCEPTION 'settled_time cannot be null for settlement';
    END IF;

    IF p_settled_amount_minor = 0 THEN
        RAISE EXCEPTION 'settled_amount_minor cannot be zero';
    END IF;

    IF (v_account_amount_minor < 0 AND p_settled_amount_minor > 0) OR
       (v_account_amount_minor > 0 AND p_settled_amount_minor < 0) THEN
        RAISE EXCEPTION 'Settlement sign flip prohibited: % -> %', v_account_amount_minor, p_settled_amount_minor;
    END IF;

    -- 3. Atomic transition of bank_transactions (trg_enforce_bank_evidence_immutability executes as family_admin)
    UPDATE public.bank_transactions
    SET booking_status = 'posted',
        posted_time = p_settled_time,
        account_amount_minor = p_settled_amount_minor,
        external_id = COALESCE(p_external_id, external_id),
        updated_at = NOW()
    WHERE family_id = p_family_id AND id = p_transaction_id AND booking_status = 'pending';

    -- 4. Atomic synchronization of matching unsplit accounting_entries
    UPDATE public.accounting_entries
    SET amount_minor = p_settled_amount_minor,
        updated_at = NOW()
    WHERE family_id = p_family_id 
      AND bank_transaction_id = p_transaction_id 
      AND is_split = FALSE;
END;
$$;

ALTER FUNCTION public.settle_pending_bank_transaction(UUID, UUID, UUID, TIMESTAMPTZ, BIGINT, VARCHAR) OWNER TO family_worker;
REVOKE ALL ON FUNCTION public.settle_pending_bank_transaction(UUID, UUID, UUID, TIMESTAMPTZ, BIGINT, VARCHAR) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.settle_pending_bank_transaction(UUID, UUID, UUID, TIMESTAMPTZ, BIGINT, VARCHAR) TO family_app;

-- 7. Tenant Row Level Security (FORCE RLS)
ALTER TABLE public.statement_documents OWNER TO family_admin;
ALTER TABLE public.statement_documents ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.statement_documents FORCE ROW LEVEL SECURITY;

ALTER TABLE public.statement_import_batches OWNER TO family_admin;
ALTER TABLE public.statement_import_batches ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.statement_import_batches FORCE ROW LEVEL SECURITY;

ALTER TABLE public.staged_bank_transactions OWNER TO family_admin;
ALTER TABLE public.staged_bank_transactions ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.staged_bank_transactions FORCE ROW LEVEL SECURITY;

ALTER TABLE public.statement_reconciliation_overrides OWNER TO family_admin;
ALTER TABLE public.statement_reconciliation_overrides ENABLE ROW LEVEL SECURITY;
ALTER TABLE public.statement_reconciliation_overrides FORCE ROW LEVEL SECURITY;

-- Policies for statement_documents
CREATE POLICY rls_statement_docs_select ON public.statement_documents
    FOR SELECT TO family_app, family_worker
    USING (family_id = get_current_family_id());

CREATE POLICY rls_statement_docs_insert ON public.statement_documents
    FOR INSERT TO family_app
    WITH CHECK (family_id = get_current_family_id());

-- Policies for statement_import_batches
CREATE POLICY rls_statement_batches_select ON public.statement_import_batches
    FOR SELECT TO family_app, family_worker
    USING (family_id = get_current_family_id());

CREATE POLICY rls_statement_batches_insert ON public.statement_import_batches
    FOR INSERT TO family_app
    WITH CHECK (family_id = get_current_family_id());

CREATE POLICY rls_statement_batches_update ON public.statement_import_batches
    FOR UPDATE TO family_app, family_worker
    USING (family_id = get_current_family_id())
    WITH CHECK (family_id = get_current_family_id());

CREATE POLICY rls_statement_batches_delete ON public.statement_import_batches
    FOR DELETE TO family_app
    USING (family_id = get_current_family_id());

-- Policies for staged_bank_transactions
CREATE POLICY rls_staged_tx_select ON public.staged_bank_transactions
    FOR SELECT TO family_app, family_worker
    USING (family_id = get_current_family_id());

CREATE POLICY rls_staged_tx_insert ON public.staged_bank_transactions
    FOR INSERT TO family_app
    WITH CHECK (family_id = get_current_family_id());

CREATE POLICY rls_staged_tx_update ON public.staged_bank_transactions
    FOR UPDATE TO family_app, family_worker
    USING (family_id = get_current_family_id())
    WITH CHECK (family_id = get_current_family_id());

CREATE POLICY rls_staged_tx_delete ON public.staged_bank_transactions
    FOR DELETE TO family_app
    USING (family_id = get_current_family_id());

-- Policies for statement_reconciliation_overrides
CREATE POLICY rls_reconciliation_overrides_select ON public.statement_reconciliation_overrides
    FOR SELECT TO family_app, family_worker
    USING (family_id = get_current_family_id());

CREATE POLICY rls_reconciliation_overrides_insert ON public.statement_reconciliation_overrides
    FOR INSERT TO family_app
    WITH CHECK (family_id = get_current_family_id());

-- 8. Table-level Privileges
GRANT SELECT, INSERT ON public.statement_documents TO family_app;
GRANT SELECT ON public.statement_documents TO family_worker;

GRANT SELECT, INSERT, UPDATE, DELETE ON public.statement_import_batches TO family_app;
GRANT SELECT, UPDATE (status, error_code, error_message, updated_at) ON public.statement_import_batches TO family_worker;

GRANT SELECT, INSERT, UPDATE, DELETE ON public.staged_bank_transactions TO family_app;
GRANT SELECT, UPDATE (duplicate_status, resolution_action) ON public.staged_bank_transactions TO family_worker;

GRANT SELECT, INSERT ON public.statement_reconciliation_overrides TO family_app;
GRANT SELECT ON public.statement_reconciliation_overrides TO family_worker;

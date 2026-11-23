-- Family Finance Security & Evidence Integrity Test Schema (PostgreSQL 16+)
-- File: tests/security/schema.sql

-- 1. Role Provisioning
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'family_admin') THEN
        CREATE ROLE family_admin LOGIN PASSWORD 'secret_admin';
    ELSE
        ALTER ROLE family_admin LOGIN PASSWORD 'secret_admin';
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'family_app') THEN
        CREATE ROLE family_app LOGIN PASSWORD 'secret_app';
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'family_worker') THEN
        CREATE ROLE family_worker LOGIN PASSWORD 'secret_worker';
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'family_queue_definer') THEN
        CREATE ROLE family_queue_definer NOLOGIN;
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'family_auth_definer') THEN
        CREATE ROLE family_auth_definer NOLOGIN;
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'family_auth_verifier') THEN
        CREATE ROLE family_auth_verifier NOLOGIN;
    END IF;
END $$;

-- 2. Grant Definer Roles to family_admin for Function Ownership Administration
GRANT family_queue_definer TO family_admin;
GRANT family_auth_definer TO family_admin;
GRANT family_auth_verifier TO family_admin;

-- 3. Dedicated Schemas for Privileged SECURITY DEFINER Subsystems
CREATE SCHEMA IF NOT EXISTS queue_sec AUTHORIZATION family_admin;
CREATE SCHEMA IF NOT EXISTS auth_sec AUTHORIZATION family_admin;

GRANT USAGE, CREATE ON SCHEMA queue_sec TO family_queue_definer;
GRANT USAGE, CREATE ON SCHEMA auth_sec TO family_auth_definer, family_auth_verifier;

-- 4. Revoke Blanket Public Privileges & Configure Default Privileges
REVOKE ALL ON SCHEMA public FROM PUBLIC;
GRANT ALL ON SCHEMA public TO family_admin;
GRANT USAGE ON SCHEMA public TO family_app, family_worker, family_queue_definer, family_auth_definer, family_auth_verifier;

GRANT ALL ON SCHEMA queue_sec TO family_admin;
GRANT ALL ON SCHEMA auth_sec TO family_admin;

GRANT USAGE ON SCHEMA queue_sec TO family_worker;
GRANT USAGE ON SCHEMA auth_sec TO family_app;

ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE ALL ON TABLES FROM PUBLIC, family_app, family_worker;
ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE ALL ON SEQUENCES FROM PUBLIC, family_app, family_worker;
ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE ALL ON FUNCTIONS FROM PUBLIC, family_app, family_worker;

-- 5. Global Identity Table
CREATE TABLE IF NOT EXISTS public.users (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email VARCHAR(255) NOT NULL,
    password_hash VARCHAR(255) NOT NULL,
    first_name VARCHAR(100) NOT NULL,
    last_name VARCHAR(100) NOT NULL,
    is_active BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_users_email UNIQUE (email)
);

-- 6. Tenant Root & Membership Tables
CREATE TABLE IF NOT EXISTS public.families (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name VARCHAR(100) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS public.family_members (
    family_id UUID NOT NULL REFERENCES public.families(id) ON DELETE CASCADE,
    user_id UUID NOT NULL REFERENCES public.users(id) ON DELETE CASCADE,
    role VARCHAR(20) NOT NULL CHECK (role IN ('owner', 'partner', 'viewer')),
    joined_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (family_id, user_id)
);

CREATE TABLE IF NOT EXISTS public.bank_connections (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    family_id UUID NOT NULL REFERENCES public.families(id) ON DELETE CASCADE,
    bank_name VARCHAR(50) NOT NULL,
    status VARCHAR(20) NOT NULL DEFAULT 'pending_pairing',
    pairing_secret_hash VARCHAR(255),
    pairing_expires_at TIMESTAMPTZ,
    last_synced_at TIMESTAMPTZ,
    error_message TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_bank_connections_tenant UNIQUE (family_id, id)
);

CREATE TABLE IF NOT EXISTS public.raw_bank_payloads (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    family_id UUID NOT NULL REFERENCES public.families(id) ON DELETE CASCADE,
    connection_id UUID,
    payload JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_raw_payloads_tenant UNIQUE (family_id, id),
    CONSTRAINT fk_raw_payloads_conn FOREIGN KEY (family_id, connection_id)
        REFERENCES public.bank_connections(family_id, id) ON DELETE SET NULL (connection_id)
);

CREATE TABLE IF NOT EXISTS public.accounts (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    family_id UUID NOT NULL REFERENCES public.families(id) ON DELETE CASCADE,
    owner_user_id UUID NOT NULL,
    name VARCHAR(100) NOT NULL,
    type VARCHAR(30) NOT NULL CHECK (type IN ('checking', 'credit_card', 'savings', 'deposit', 'cash')),
    currency VARCHAR(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    current_balance_minor BIGINT,
    debt_balance_minor BIGINT,
    available_balance_minor BIGINT,
    credit_limit_minor BIGINT,
    statement_balance_minor BIGINT,
    minimal_payment_minor BIGINT,
    grace_period_end_date DATE,
    balance_updated_at TIMESTAMPTZ,
    balance_source VARCHAR(30),
    is_archived BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_accounts_tenant UNIQUE (family_id, id),
    CONSTRAINT fk_accounts_owner FOREIGN KEY (family_id, owner_user_id)
        REFERENCES public.family_members(family_id, user_id) ON DELETE RESTRICT
);

CREATE TABLE IF NOT EXISTS public.cards (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    family_id UUID NOT NULL REFERENCES public.families(id) ON DELETE CASCADE,
    account_id UUID NOT NULL,
    card_holder_user_id UUID,
    masked_pan VARCHAR(8) NOT NULL,
    card_holder_name VARCHAR(100),
    expiration_date VARCHAR(5),
    is_active BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_cards_tenant UNIQUE (family_id, id),
    CONSTRAINT fk_cards_account FOREIGN KEY (family_id, account_id)
        REFERENCES public.accounts(family_id, id) ON DELETE CASCADE,
    CONSTRAINT fk_cards_holder FOREIGN KEY (family_id, card_holder_user_id)
        REFERENCES public.family_members(family_id, user_id) ON DELETE SET NULL (card_holder_user_id)
);

CREATE TABLE IF NOT EXISTS public.bank_transactions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    family_id UUID NOT NULL REFERENCES public.families(id) ON DELETE CASCADE,
    account_id UUID NOT NULL,
    card_id UUID,
    raw_payload_id UUID,
    
    external_id VARCHAR(128),
    external_auth_id VARCHAR(128),
    fingerprint_hash VARCHAR(64) NOT NULL,
    fingerprint_sequence INT NOT NULL DEFAULT 1,
    
    amount_minor BIGINT NOT NULL,
    currency VARCHAR(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    account_amount_minor BIGINT NOT NULL,
    account_currency VARCHAR(3) NOT NULL CHECK (account_currency ~ '^[A-Z]{3}$'),
    
    booking_status VARCHAR(30) NOT NULL DEFAULT 'posted' CHECK (booking_status IN ('pending', 'posted', 'cancelled')),
    operation_type VARCHAR(30) NOT NULL DEFAULT 'purchase' CHECK (operation_type IN (
        'purchase', 'refund', 'transfer_out', 'transfer_in', 'atm_cash', 'fee', 'card_check'
    )),
    
    related_bank_transaction_id UUID,
    transaction_time TIMESTAMPTZ NOT NULL,
    posted_time TIMESTAMPTZ,
    
    merchant_name VARCHAR(255),
    raw_description TEXT NOT NULL,
    mcc INT,
    bank_category VARCHAR(100),
    
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    
    CONSTRAINT uq_bank_transactions_tenant UNIQUE (family_id, id),
    CONSTRAINT uq_bank_tx_external_id UNIQUE (family_id, account_id, external_id),
    CONSTRAINT uq_bank_tx_fingerprint UNIQUE (family_id, account_id, fingerprint_hash, fingerprint_sequence),
    CONSTRAINT fk_bank_tx_account FOREIGN KEY (family_id, account_id) 
        REFERENCES public.accounts(family_id, id) ON DELETE CASCADE,
    CONSTRAINT fk_bank_tx_card FOREIGN KEY (family_id, card_id) 
        REFERENCES public.cards(family_id, id) ON DELETE SET NULL (card_id),
    CONSTRAINT fk_bank_tx_raw_payload FOREIGN KEY (family_id, raw_payload_id) 
        REFERENCES public.raw_bank_payloads(family_id, id) ON DELETE SET NULL (raw_payload_id),
    CONSTRAINT fk_bank_tx_related FOREIGN KEY (family_id, related_bank_transaction_id) 
        REFERENCES public.bank_transactions(family_id, id) ON DELETE SET NULL (related_bank_transaction_id),
    CONSTRAINT chk_bank_tx_amount CHECK (amount_minor != 0 OR operation_type = 'card_check')
);

CREATE TABLE IF NOT EXISTS public.categories (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    family_id UUID NOT NULL REFERENCES public.families(id) ON DELETE CASCADE,
    parent_id UUID,
    name VARCHAR(100) NOT NULL,
    icon VARCHAR(50),
    is_archived BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_categories_tenant UNIQUE (family_id, id),
    CONSTRAINT fk_categories_parent FOREIGN KEY (family_id, parent_id)
        REFERENCES public.categories(family_id, id) ON DELETE SET NULL (parent_id)
);

CREATE TABLE IF NOT EXISTS public.transfer_matches (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    family_id UUID NOT NULL REFERENCES public.families(id) ON DELETE CASCADE,
    source_bank_transaction_id UUID NOT NULL,
    destination_bank_transaction_id UUID NOT NULL,
    confidence_score NUMERIC(3, 2) NOT NULL,
    tier VARCHAR(20) NOT NULL CHECK (tier IN ('tier_1_exact', 'tier_2_probable', 'tier_3_manual')),
    status VARCHAR(30) NOT NULL DEFAULT 'suggested' CHECK (status IN (
        'suggested', 'auto_confirmed', 'confirmed', 'rejected', 'reopened'
    )),
    fee_minor BIGINT NOT NULL DEFAULT 0,
    is_credit_card_repayment BOOLEAN NOT NULL DEFAULT FALSE,
    matched_by_user_id UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_transfer_matches_tenant UNIQUE (family_id, id),
    CONSTRAINT uq_tm_pair UNIQUE (family_id, source_bank_transaction_id, destination_bank_transaction_id),
    CONSTRAINT fk_tm_source FOREIGN KEY (family_id, source_bank_transaction_id) 
        REFERENCES public.bank_transactions(family_id, id) ON DELETE CASCADE,
    CONSTRAINT fk_tm_destination FOREIGN KEY (family_id, destination_bank_transaction_id) 
        REFERENCES public.bank_transactions(family_id, id) ON DELETE CASCADE,
    CONSTRAINT fk_tm_user FOREIGN KEY (family_id, matched_by_user_id) 
        REFERENCES public.family_members(family_id, user_id) ON DELETE SET NULL (matched_by_user_id)
);

CREATE TABLE IF NOT EXISTS public.accounting_entries (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    family_id UUID NOT NULL REFERENCES public.families(id) ON DELETE CASCADE,
    account_id UUID NOT NULL,
    bank_transaction_id UUID,
    transfer_match_id UUID,
    
    amount_minor BIGINT NOT NULL,
    currency VARCHAR(3) NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    classification VARCHAR(30) NOT NULL DEFAULT 'expense' CHECK (classification IN (
        'expense', 'income', 'internal_transfer', 'debt_service', 'adjustment'
    )),
    beneficiary_type VARCHAR(20) NOT NULL DEFAULT 'shared' CHECK (beneficiary_type IN (
        'shared', 'individual', 'child'
    )),
    payer_user_id UUID NOT NULL,
    beneficiary_user_id UUID,
    category_id UUID,
    
    is_split BOOLEAN NOT NULL DEFAULT FALSE,
    split_index INT NOT NULL DEFAULT 0,
    notes TEXT,
    entry_time TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    
    CONSTRAINT uq_accounting_entries_tenant UNIQUE (family_id, id),
    CONSTRAINT fk_entries_account FOREIGN KEY (family_id, account_id) 
        REFERENCES public.accounts(family_id, id) ON DELETE CASCADE,
    CONSTRAINT fk_entries_bank_tx FOREIGN KEY (family_id, bank_transaction_id) 
        REFERENCES public.bank_transactions(family_id, id) ON DELETE CASCADE,
    CONSTRAINT fk_entries_transfer_match FOREIGN KEY (family_id, transfer_match_id) 
        REFERENCES public.transfer_matches(family_id, id) ON DELETE SET NULL (transfer_match_id),
    CONSTRAINT fk_entries_payer FOREIGN KEY (family_id, payer_user_id) 
        REFERENCES public.family_members(family_id, user_id) ON DELETE RESTRICT,
    CONSTRAINT fk_entries_beneficiary FOREIGN KEY (family_id, beneficiary_user_id) 
        REFERENCES public.family_members(family_id, user_id) ON DELETE SET NULL (beneficiary_user_id),
    CONSTRAINT fk_entries_category FOREIGN KEY (family_id, category_id) 
        REFERENCES public.categories(family_id, id) ON DELETE SET NULL (category_id)
);

CREATE TABLE IF NOT EXISTS public.categorization_rules (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    family_id UUID NOT NULL REFERENCES public.families(id) ON DELETE CASCADE,
    priority INT NOT NULL DEFAULT 100,
    merchant_pattern VARCHAR(255),
    mcc INT,
    min_amount_minor BIGINT,
    max_amount_minor BIGINT,
    target_category_id UUID NOT NULL,
    target_beneficiary_type VARCHAR(20),
    is_active BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_categorization_rules_tenant UNIQUE (family_id, id),
    CONSTRAINT fk_rules_category FOREIGN KEY (family_id, target_category_id)
        REFERENCES public.categories(family_id, id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS public.transfer_rejections (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    family_id UUID NOT NULL REFERENCES public.families(id) ON DELETE CASCADE,
    source_bank_transaction_id UUID NOT NULL,
    destination_bank_transaction_id UUID NOT NULL,
    rejected_by_user_id UUID NOT NULL,
    reason VARCHAR(255),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_transfer_rejections_tenant UNIQUE (family_id, id),
    CONSTRAINT fk_tr_source FOREIGN KEY (family_id, source_bank_transaction_id) 
        REFERENCES public.bank_transactions(family_id, id) ON DELETE CASCADE,
    CONSTRAINT fk_tr_destination FOREIGN KEY (family_id, destination_bank_transaction_id) 
        REFERENCES public.bank_transactions(family_id, id) ON DELETE CASCADE,
    CONSTRAINT fk_tr_user FOREIGN KEY (family_id, rejected_by_user_id) 
        REFERENCES public.family_members(family_id, user_id) ON DELETE RESTRICT
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_transfer_rejections_symmetric ON public.transfer_rejections (
    family_id, 
    LEAST(source_bank_transaction_id, destination_bank_transaction_id), 
    GREATEST(source_bank_transaction_id, destination_bank_transaction_id)
);

CREATE TABLE IF NOT EXISTS public.statement_imports (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    family_id UUID NOT NULL REFERENCES public.families(id) ON DELETE CASCADE,
    account_id UUID NOT NULL,
    file_name VARCHAR(255) NOT NULL,
    file_size_bytes INT NOT NULL,
    rows_parsed INT NOT NULL DEFAULT 0,
    rows_imported INT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_statement_imports_tenant UNIQUE (family_id, id),
    CONSTRAINT fk_statement_account FOREIGN KEY (family_id, account_id)
        REFERENCES public.accounts(family_id, id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS public.audit_logs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    family_id UUID NOT NULL REFERENCES public.families(id) ON DELETE CASCADE,
    actor_user_id UUID,
    event_type VARCHAR(50) NOT NULL,
    entity_name VARCHAR(50) NOT NULL,
    entity_id UUID NOT NULL,
    payload JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_audit_logs_tenant UNIQUE (family_id, id)
);

CREATE TABLE IF NOT EXISTS public.worker_jobs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    family_id UUID NOT NULL REFERENCES public.families(id) ON DELETE CASCADE,
    queue_name VARCHAR(50) NOT NULL,
    job_type VARCHAR(100) NOT NULL,
    payload JSONB NOT NULL,
    state VARCHAR(30) NOT NULL DEFAULT 'available' CHECK (state IN ('available', 'running', 'completed', 'failed')),
    attempts INT NOT NULL DEFAULT 0,
    max_attempts INT NOT NULL DEFAULT 5,
    locked_by VARCHAR(100),
    locked_until TIMESTAMPTZ,
    last_error TEXT,
    scheduled_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_worker_jobs_tenant UNIQUE (family_id, id)
);

-- 7. Triggers and Integrity Functions

-- Tenant Immutability Trigger
CREATE OR REPLACE FUNCTION trg_enforce_tenant_immutability()
RETURNS TRIGGER AS $$
BEGIN
    IF OLD.family_id IS DISTINCT FROM NEW.family_id THEN
        RAISE EXCEPTION 'Tenant immutability violation: family_id cannot be changed from % to % on table %',
            OLD.family_id, NEW.family_id, TG_TABLE_NAME;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- Bank Evidence Monotonic State Machine Trigger (Phase 0.10)
CREATE OR REPLACE FUNCTION trg_enforce_bank_evidence_immutability()
RETURNS TRIGGER AS $$
BEGIN
    IF CURRENT_USER NOT IN ('family_worker', 'family_admin', 'postgres') THEN
        RAISE EXCEPTION 'Bank evidence immutability violation: role % is not permitted to update bank_transactions', CURRENT_USER;
    END IF;

    -- Category 1: 19 Immutable Bank Facts
    IF OLD.id IS DISTINCT FROM NEW.id OR
       OLD.family_id IS DISTINCT FROM NEW.family_id OR
       OLD.account_id IS DISTINCT FROM NEW.account_id OR
       OLD.card_id IS DISTINCT FROM NEW.card_id OR
       OLD.raw_payload_id IS DISTINCT FROM NEW.raw_payload_id OR
       OLD.external_auth_id IS DISTINCT FROM NEW.external_auth_id OR
       OLD.fingerprint_hash IS DISTINCT FROM NEW.fingerprint_hash OR
       OLD.fingerprint_sequence IS DISTINCT FROM NEW.fingerprint_sequence OR
       OLD.amount_minor IS DISTINCT FROM NEW.amount_minor OR
       OLD.currency IS DISTINCT FROM NEW.currency OR
       OLD.account_currency IS DISTINCT FROM NEW.account_currency OR
       OLD.operation_type IS DISTINCT FROM NEW.operation_type OR
       OLD.related_bank_transaction_id IS DISTINCT FROM NEW.related_bank_transaction_id OR
       OLD.transaction_time IS DISTINCT FROM NEW.transaction_time OR
       OLD.merchant_name IS DISTINCT FROM NEW.merchant_name OR
       OLD.raw_description IS DISTINCT FROM NEW.raw_description OR
       OLD.mcc IS DISTINCT FROM NEW.mcc OR
       OLD.bank_category IS DISTINCT FROM NEW.bank_category OR
       OLD.created_at IS DISTINCT FROM NEW.created_at THEN
        RAISE EXCEPTION 'Bank evidence immutability violation: immutable bank fact attributes on transaction % cannot be modified', OLD.id;
    END IF;

    -- Monotonic State Machine Verification
    IF OLD.booking_status = 'posted' THEN
        IF NEW.booking_status != 'posted' THEN
            RAISE EXCEPTION 'Bank evidence lifecycle violation: terminal status posted cannot be changed to % on transaction %',
                NEW.booking_status, OLD.id;
        END IF;
        IF OLD.account_amount_minor IS DISTINCT FROM NEW.account_amount_minor OR
           OLD.posted_time IS DISTINCT FROM NEW.posted_time OR
           OLD.external_id IS DISTINCT FROM NEW.external_id THEN
            RAISE EXCEPTION 'Bank evidence immutability violation: settlement fields on terminal posted transaction % cannot be modified', OLD.id;
        END IF;

    ELSIF OLD.booking_status = 'cancelled' THEN
        IF NEW.booking_status != 'cancelled' THEN
            RAISE EXCEPTION 'Bank evidence lifecycle violation: terminal status cancelled cannot be changed to % on transaction %',
                NEW.booking_status, OLD.id;
        END IF;
        IF OLD.account_amount_minor IS DISTINCT FROM NEW.account_amount_minor OR
           OLD.posted_time IS DISTINCT FROM NEW.posted_time OR
           OLD.external_id IS DISTINCT FROM NEW.external_id THEN
            RAISE EXCEPTION 'Bank evidence immutability violation: settlement fields on terminal cancelled transaction % cannot be modified', OLD.id;
        END IF;

    ELSIF OLD.booking_status = 'pending' THEN
        IF NEW.booking_status = 'posted' THEN
            -- Allowed settlement transition
            NULL;
        ELSIF NEW.booking_status = 'cancelled' THEN
            -- Allowed cancellation transition; settlement fields must NOT change
            IF OLD.account_amount_minor IS DISTINCT FROM NEW.account_amount_minor OR
               OLD.posted_time IS DISTINCT FROM NEW.posted_time OR
               OLD.external_id IS DISTINCT FROM NEW.external_id THEN
                RAISE EXCEPTION 'Bank evidence lifecycle violation: settlement fields cannot be modified during cancellation of transaction %', OLD.id;
            END IF;
        ELSIF NEW.booking_status = 'pending' THEN
            IF OLD.account_amount_minor IS DISTINCT FROM NEW.account_amount_minor OR
               OLD.posted_time IS DISTINCT FROM NEW.posted_time OR
               OLD.external_id IS DISTINCT FROM NEW.external_id THEN
                RAISE EXCEPTION 'Bank evidence lifecycle violation: settlement fields on pending transaction % cannot be modified without transition to posted', OLD.id;
            END IF;
        ELSE
            RAISE EXCEPTION 'Bank evidence lifecycle violation: invalid booking_status % on transaction %',
                NEW.booking_status, OLD.id;
        END IF;

    ELSE
        RAISE EXCEPTION 'Bank evidence lifecycle violation: unknown prior status % on transaction %',
            OLD.booking_status, OLD.id;
    END IF;

    NEW.updated_at := NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE TRIGGER trg_bank_tx_immutable
BEFORE UPDATE ON public.bank_transactions
FOR EACH ROW EXECUTE FUNCTION trg_enforce_bank_evidence_immutability();

-- Bank Evidence DELETE Block
CREATE OR REPLACE FUNCTION trg_enforce_bank_evidence_no_delete()
RETURNS TRIGGER AS $$
BEGIN
    IF CURRENT_USER NOT IN ('family_admin', 'postgres') THEN
        RAISE EXCEPTION 'Bank evidence immutability violation: deleting bank_transactions is prohibited for role %', CURRENT_USER;
    END IF;
    RETURN OLD;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE TRIGGER trg_bank_tx_no_delete
BEFORE DELETE ON public.bank_transactions
FOR EACH ROW EXECUTE FUNCTION trg_enforce_bank_evidence_no_delete();

-- Raw Payload Immutability Trigger
CREATE OR REPLACE FUNCTION trg_enforce_raw_payload_immutability()
RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'UPDATE' THEN
        RAISE EXCEPTION 'Raw payload immutability violation: raw_bank_payloads is strictly immutable; updates are prohibited';
    ELSIF TG_OP = 'DELETE' THEN
        IF CURRENT_USER NOT IN ('family_admin', 'postgres') THEN
            RAISE EXCEPTION 'Raw payload immutability violation: deleting raw_bank_payloads is prohibited for role %', CURRENT_USER;
        END IF;
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE TRIGGER trg_raw_payloads_immutable
BEFORE UPDATE OR DELETE ON public.raw_bank_payloads
FOR EACH ROW EXECUTE FUNCTION trg_enforce_raw_payload_immutability();

-- Audit Log Append-Only Trigger
CREATE OR REPLACE FUNCTION trg_enforce_audit_logs_append_only()
RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'UPDATE' THEN
        RAISE EXCEPTION 'Audit log immutability violation: audit_logs is append-only; updates are strictly prohibited';
    ELSIF TG_OP = 'DELETE' THEN
        IF CURRENT_USER NOT IN ('family_admin', 'postgres') THEN
            RAISE EXCEPTION 'Audit log immutability violation: deleting audit_logs is prohibited for role %', CURRENT_USER;
        END IF;
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE TRIGGER trg_audit_logs_append_only
BEFORE UPDATE OR DELETE ON public.audit_logs
FOR EACH ROW EXECUTE FUNCTION trg_enforce_audit_logs_append_only();

-- 8. SECURITY DEFINER Helper Subsystems

-- Schema: auth_sec
CREATE OR REPLACE FUNCTION auth_sec.get_user_auth_credential(p_email VARCHAR(255))
RETURNS TABLE (
    user_id UUID,
    password_hash VARCHAR(255),
    is_active BOOLEAN
)
LANGUAGE sql
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
STABLE
AS $$
    SELECT u.id, u.password_hash, u.is_active
    FROM public.users u
    WHERE u.email = LOWER(TRIM(p_email));
$$;

ALTER FUNCTION auth_sec.get_user_auth_credential(VARCHAR) OWNER TO family_auth_verifier;
REVOKE ALL ON FUNCTION auth_sec.get_user_auth_credential(VARCHAR) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION auth_sec.get_user_auth_credential(VARCHAR) TO family_app;

CREATE OR REPLACE FUNCTION auth_sec.update_user_password(p_new_password_hash VARCHAR(255))
RETURNS VOID
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_user_id UUID;
BEGIN
    v_user_id := NULLIF(current_setting('app.current_user_id', true), '')::UUID;
    IF v_user_id IS NULL THEN
        RAISE EXCEPTION 'Authentication required to update password';
    END IF;
    UPDATE public.users
    SET password_hash = p_new_password_hash, updated_at = NOW()
    WHERE id = v_user_id;
END;
$$;

ALTER FUNCTION auth_sec.update_user_password(VARCHAR) OWNER TO family_auth_verifier;
REVOKE ALL ON FUNCTION auth_sec.update_user_password(VARCHAR) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION auth_sec.update_user_password(VARCHAR) TO family_app;

CREATE OR REPLACE FUNCTION auth_sec.get_current_user_family_ids()
RETURNS SETOF UUID
LANGUAGE sql
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
STABLE
AS $$
    SELECT fm.family_id 
    FROM public.family_members fm
    JOIN public.users u ON u.id = fm.user_id
    WHERE fm.user_id = NULLIF(current_setting('app.current_user_id', true), '')::UUID
      AND u.is_active = TRUE;
$$;

ALTER FUNCTION auth_sec.get_current_user_family_ids() OWNER TO family_auth_definer;
REVOKE ALL ON FUNCTION auth_sec.get_current_user_family_ids() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION auth_sec.get_current_user_family_ids() TO family_app;

-- Schema: queue_sec
CREATE OR REPLACE FUNCTION queue_sec.claim_next_worker_job(
    p_queue_name VARCHAR(50),
    p_worker_id VARCHAR(100),
    p_lock_duration_seconds INT DEFAULT 600
)
RETURNS TABLE (
    job_id UUID,
    family_id UUID,
    job_type VARCHAR(100),
    payload JSONB
)
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_job RECORD;
BEGIN
    SELECT wj.id, wj.family_id, wj.job_type, wj.payload
    INTO v_job
    FROM public.worker_jobs wj
    WHERE wj.queue_name = p_queue_name
      AND (
          wj.state = 'available' 
          OR (wj.state = 'running' AND wj.locked_until < NOW())
      )
      AND wj.scheduled_at <= NOW()
    ORDER BY wj.scheduled_at ASC
    FOR UPDATE OF wj SKIP LOCKED
    LIMIT 1;

    IF v_job.id IS NOT NULL THEN
        UPDATE public.worker_jobs
        SET state = 'running',
            locked_by = p_worker_id,
            locked_until = NOW() + (p_lock_duration_seconds || ' seconds')::INTERVAL,
            attempts = attempts + 1,
            updated_at = NOW()
        WHERE id = v_job.id;

        RETURN QUERY SELECT v_job.id, v_job.family_id, v_job.job_type, v_job.payload;
    END IF;
END;
$$;

ALTER FUNCTION queue_sec.claim_next_worker_job(VARCHAR, VARCHAR, INT) OWNER TO family_queue_definer;
REVOKE ALL ON FUNCTION queue_sec.claim_next_worker_job(VARCHAR, VARCHAR, INT) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION queue_sec.claim_next_worker_job(VARCHAR, VARCHAR, INT) TO family_worker;

-- 9. Table Grants
GRANT SELECT (id, email, first_name, last_name, is_active, created_at, updated_at),
      INSERT (id, email, password_hash, first_name, last_name, is_active),
      UPDATE (first_name, last_name, updated_at)
      ON public.users TO family_app;

GRANT SELECT (id, first_name, last_name, is_active)
      ON public.users TO family_worker;

GRANT SELECT (id, email, password_hash, is_active),
      UPDATE (password_hash, updated_at)
      ON public.users TO family_auth_verifier;

GRANT SELECT (id, is_active)
      ON public.users TO family_auth_definer;

GRANT SELECT, INSERT, UPDATE (name) ON public.families TO family_app;
GRANT SELECT ON public.families TO family_worker;

GRANT SELECT, INSERT, UPDATE (role), DELETE ON public.family_members TO family_app;
GRANT SELECT ON public.family_members TO family_worker;
GRANT SELECT (family_id, user_id) ON public.family_members TO family_auth_definer;

GRANT SELECT, INSERT, UPDATE, DELETE ON public.bank_connections TO family_app;
GRANT SELECT, UPDATE (status, last_synced_at, error_message, updated_at) ON public.bank_connections TO family_worker;

GRANT SELECT, INSERT ON public.raw_bank_payloads TO family_app, family_worker;

GRANT SELECT, INSERT, UPDATE, DELETE ON public.accounts TO family_app;
GRANT SELECT, UPDATE (current_balance_minor, debt_balance_minor, available_balance_minor, credit_limit_minor, statement_balance_minor, minimal_payment_minor, grace_period_end_date, balance_updated_at, balance_source, updated_at) ON public.accounts TO family_worker;

GRANT SELECT, INSERT, UPDATE, DELETE ON public.cards TO family_app;
GRANT SELECT, INSERT, UPDATE (card_holder_name, expiration_date, is_active, updated_at) ON public.cards TO family_worker;

GRANT SELECT, INSERT ON public.bank_transactions TO family_app;
GRANT SELECT, INSERT, UPDATE (booking_status, posted_time, account_amount_minor, external_id, updated_at) ON public.bank_transactions TO family_worker;

GRANT SELECT, INSERT, UPDATE, DELETE ON public.accounting_entries TO family_app;
GRANT SELECT, INSERT, UPDATE (category_id, updated_at) ON public.accounting_entries TO family_worker;

GRANT SELECT, INSERT, UPDATE, DELETE ON public.categories TO family_app;
GRANT SELECT ON public.categories TO family_worker;

GRANT SELECT, INSERT, UPDATE, DELETE ON public.categorization_rules TO family_app;
GRANT SELECT ON public.categorization_rules TO family_worker;

GRANT SELECT, INSERT, UPDATE (status, fee_minor, is_credit_card_repayment, confidence_score, matched_by_user_id, updated_at), DELETE ON public.transfer_matches TO family_app;
GRANT SELECT, INSERT, UPDATE (status, fee_minor, is_credit_card_repayment, confidence_score, matched_by_user_id, updated_at) ON public.transfer_matches TO family_worker;

GRANT SELECT, INSERT ON public.transfer_rejections TO family_app;
GRANT SELECT ON public.transfer_rejections TO family_worker;

GRANT SELECT, INSERT, UPDATE, DELETE ON public.statement_imports TO family_app;
GRANT SELECT ON public.statement_imports TO family_worker;

GRANT SELECT, INSERT ON public.audit_logs TO family_app, family_worker;

GRANT SELECT, INSERT ON public.worker_jobs TO family_app;
GRANT SELECT, UPDATE (state, attempts, last_error, updated_at) ON public.worker_jobs TO family_worker;
GRANT SELECT, UPDATE ON public.worker_jobs TO family_queue_definer;

-- DDL Schema Owner Privileges & Table Ownership
GRANT ALL ON ALL TABLES IN SCHEMA public TO family_admin;
GRANT ALL ON ALL SEQUENCES IN SCHEMA public TO family_admin;

ALTER TABLE public.users OWNER TO family_admin;
ALTER TABLE public.families OWNER TO family_admin;
ALTER TABLE public.family_members OWNER TO family_admin;
ALTER TABLE public.bank_connections OWNER TO family_admin;
ALTER TABLE public.raw_bank_payloads OWNER TO family_admin;
ALTER TABLE public.accounts OWNER TO family_admin;
ALTER TABLE public.cards OWNER TO family_admin;
ALTER TABLE public.bank_transactions OWNER TO family_admin;
ALTER TABLE public.accounting_entries OWNER TO family_admin;
ALTER TABLE public.categories OWNER TO family_admin;
ALTER TABLE public.categorization_rules OWNER TO family_admin;
ALTER TABLE public.transfer_matches OWNER TO family_admin;
ALTER TABLE public.transfer_rejections OWNER TO family_admin;
ALTER TABLE public.statement_imports OWNER TO family_admin;
ALTER TABLE public.audit_logs OWNER TO family_admin;
ALTER TABLE public.worker_jobs OWNER TO family_admin;

-- 10. Row Level Security Configuration
CREATE OR REPLACE FUNCTION get_current_family_id() RETURNS UUID AS $$
    SELECT NULLIF(current_setting('app.current_family_id', true), '')::UUID;
$$ LANGUAGE SQL STABLE;

ALTER TABLE public.families ENABLE ROW LEVEL SECURITY; ALTER TABLE public.families FORCE ROW LEVEL SECURITY;
ALTER TABLE public.family_members ENABLE ROW LEVEL SECURITY; ALTER TABLE public.family_members FORCE ROW LEVEL SECURITY;
ALTER TABLE public.bank_connections ENABLE ROW LEVEL SECURITY; ALTER TABLE public.bank_connections FORCE ROW LEVEL SECURITY;
ALTER TABLE public.accounts ENABLE ROW LEVEL SECURITY; ALTER TABLE public.accounts FORCE ROW LEVEL SECURITY;
ALTER TABLE public.cards ENABLE ROW LEVEL SECURITY; ALTER TABLE public.cards FORCE ROW LEVEL SECURITY;
ALTER TABLE public.bank_transactions ENABLE ROW LEVEL SECURITY; ALTER TABLE public.bank_transactions FORCE ROW LEVEL SECURITY;
ALTER TABLE public.accounting_entries ENABLE ROW LEVEL SECURITY; ALTER TABLE public.accounting_entries FORCE ROW LEVEL SECURITY;
ALTER TABLE public.categories ENABLE ROW LEVEL SECURITY; ALTER TABLE public.categories FORCE ROW LEVEL SECURITY;
ALTER TABLE public.categorization_rules ENABLE ROW LEVEL SECURITY; ALTER TABLE public.categorization_rules FORCE ROW LEVEL SECURITY;
ALTER TABLE public.transfer_matches ENABLE ROW LEVEL SECURITY; ALTER TABLE public.transfer_matches FORCE ROW LEVEL SECURITY;
ALTER TABLE public.transfer_rejections ENABLE ROW LEVEL SECURITY; ALTER TABLE public.transfer_rejections FORCE ROW LEVEL SECURITY;
ALTER TABLE public.raw_bank_payloads ENABLE ROW LEVEL SECURITY; ALTER TABLE public.raw_bank_payloads FORCE ROW LEVEL SECURITY;
ALTER TABLE public.statement_imports ENABLE ROW LEVEL SECURITY; ALTER TABLE public.statement_imports FORCE ROW LEVEL SECURITY;
ALTER TABLE public.audit_logs ENABLE ROW LEVEL SECURITY; ALTER TABLE public.audit_logs FORCE ROW LEVEL SECURITY;
ALTER TABLE public.worker_jobs ENABLE ROW LEVEL SECURITY; ALTER TABLE public.worker_jobs FORCE ROW LEVEL SECURITY;

-- Policies
CREATE POLICY rls_families_select ON public.families
    FOR SELECT TO family_app, family_worker
    USING (
        id = get_current_family_id()
        OR (CURRENT_USER = 'family_app' AND id IN (SELECT auth_sec.get_current_user_family_ids()))
    );

CREATE POLICY rls_families_mod ON public.families
    FOR ALL TO family_app
    USING (id = get_current_family_id()) 
    WITH CHECK (id = get_current_family_id());

CREATE POLICY rls_family_members_auth_definer ON public.family_members
    FOR SELECT TO family_auth_definer
    USING (user_id = NULLIF(current_setting('app.current_user_id', true), '')::UUID);

CREATE POLICY rls_family_members_select ON public.family_members
    FOR SELECT TO family_app, family_worker
    USING (
        family_id = get_current_family_id()
        OR (CURRENT_USER = 'family_app' AND family_id IN (SELECT auth_sec.get_current_user_family_ids()))
    );

CREATE POLICY rls_family_members_mod ON public.family_members
    FOR ALL TO family_app
    USING (family_id = get_current_family_id()) 
    WITH CHECK (family_id = get_current_family_id());

CREATE POLICY rls_worker_jobs_claim_select ON public.worker_jobs
    FOR SELECT TO family_queue_definer
    USING (true);

CREATE POLICY rls_worker_jobs_claim_update ON public.worker_jobs
    FOR UPDATE TO family_queue_definer
    USING (true)
    WITH CHECK (true);

CREATE POLICY rls_worker_jobs_tenant ON public.worker_jobs
    FOR ALL TO family_app, family_worker
    USING (family_id = get_current_family_id()) 
    WITH CHECK (family_id = get_current_family_id());

CREATE POLICY rls_bank_connections_all ON public.bank_connections
    FOR ALL TO family_app, family_worker
    USING (family_id = get_current_family_id()) WITH CHECK (family_id = get_current_family_id());

CREATE POLICY rls_accounts_all ON public.accounts
    FOR ALL TO family_app, family_worker
    USING (family_id = get_current_family_id()) WITH CHECK (family_id = get_current_family_id());

CREATE POLICY rls_cards_all ON public.cards
    FOR ALL TO family_app, family_worker
    USING (family_id = get_current_family_id()) WITH CHECK (family_id = get_current_family_id());

CREATE POLICY rls_bank_tx_select ON public.bank_transactions
    FOR SELECT TO family_app, family_worker
    USING (family_id = get_current_family_id());

CREATE POLICY rls_bank_tx_insert ON public.bank_transactions
    FOR INSERT TO family_app, family_worker
    WITH CHECK (family_id = get_current_family_id());

CREATE POLICY rls_bank_tx_update ON public.bank_transactions
    FOR UPDATE TO family_worker
    USING (family_id = get_current_family_id())
    WITH CHECK (family_id = get_current_family_id());

CREATE POLICY rls_raw_payloads_select ON public.raw_bank_payloads
    FOR SELECT TO family_app, family_worker
    USING (family_id = get_current_family_id());

CREATE POLICY rls_raw_payloads_insert ON public.raw_bank_payloads
    FOR INSERT TO family_app, family_worker
    WITH CHECK (family_id = get_current_family_id());

CREATE POLICY rls_audit_logs_select ON public.audit_logs
    FOR SELECT TO family_app, family_worker
    USING (family_id = get_current_family_id());

CREATE POLICY rls_audit_logs_insert ON public.audit_logs
    FOR INSERT TO family_app, family_worker
    WITH CHECK (family_id = get_current_family_id());

CREATE POLICY rls_accounting_entries_all ON public.accounting_entries
    FOR ALL TO family_app, family_worker
    USING (family_id = get_current_family_id()) WITH CHECK (family_id = get_current_family_id());

CREATE POLICY rls_categories_all ON public.categories
    FOR ALL TO family_app, family_worker
    USING (family_id = get_current_family_id()) WITH CHECK (family_id = get_current_family_id());

CREATE POLICY rls_categorization_rules_all ON public.categorization_rules
    FOR ALL TO family_app, family_worker
    USING (family_id = get_current_family_id()) WITH CHECK (family_id = get_current_family_id());

CREATE POLICY rls_transfer_matches_all ON public.transfer_matches
    FOR ALL TO family_app, family_worker
    USING (family_id = get_current_family_id()) WITH CHECK (family_id = get_current_family_id());

CREATE POLICY rls_transfer_rejections_select ON public.transfer_rejections
    FOR SELECT TO family_app, family_worker
    USING (family_id = get_current_family_id());

CREATE POLICY rls_transfer_rejections_insert ON public.transfer_rejections
    FOR INSERT TO family_app
    WITH CHECK (family_id = get_current_family_id());

CREATE POLICY rls_statement_imports_all ON public.statement_imports
    FOR ALL TO family_app, family_worker
    USING (family_id = get_current_family_id()) WITH CHECK (family_id = get_current_family_id());

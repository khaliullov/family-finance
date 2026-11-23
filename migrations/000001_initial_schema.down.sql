-- =============================================================================
-- Migration 000001: Initial Canonical Schema - Down Migration
-- =============================================================================

-- 1. Drop Views
DROP VIEW IF EXISTS public.v_family_balance_summary;
DROP VIEW IF EXISTS public.v_family_real_expenses;

-- 2. Drop Tables (in reverse dependency order with CASCADE)
DROP TABLE IF EXISTS public.worker_jobs CASCADE;
DROP TABLE IF EXISTS public.audit_logs CASCADE;
DROP TABLE IF EXISTS public.statement_imports CASCADE;
DROP TABLE IF EXISTS public.categorization_rules CASCADE;
DROP TABLE IF EXISTS public.accounting_entries CASCADE;
DROP TABLE IF EXISTS public.categories CASCADE;
DROP TABLE IF EXISTS public.transfer_rejections CASCADE;
DROP TABLE IF EXISTS public.transfer_matches CASCADE;
DROP TABLE IF EXISTS public.cards CASCADE;
DROP TABLE IF EXISTS public.bank_transactions CASCADE;
DROP TABLE IF EXISTS public.raw_bank_payloads CASCADE;
DROP TABLE IF EXISTS public.accounts CASCADE;
DROP TABLE IF EXISTS public.bank_connections CASCADE;
DROP TABLE IF EXISTS public.family_members CASCADE;
DROP TABLE IF EXISTS public.families CASCADE;
DROP TABLE IF EXISTS public.users CASCADE;

-- 3. Drop Public Schema Functions & Triggers
DROP FUNCTION IF EXISTS public.validate_accounting_entry_transfer_match() CASCADE;
DROP FUNCTION IF EXISTS public.validate_bank_transaction_split_integrity() CASCADE;
DROP FUNCTION IF EXISTS public.trg_enforce_transfer_endpoints_immutability() CASCADE;
DROP FUNCTION IF EXISTS public.trg_enforce_audit_logs_append_only() CASCADE;
DROP FUNCTION IF EXISTS public.trg_enforce_raw_payloads_immutability() CASCADE;
DROP FUNCTION IF EXISTS public.trg_enforce_bank_evidence_no_delete() CASCADE;
DROP FUNCTION IF EXISTS public.trg_enforce_bank_evidence_immutability() CASCADE;
DROP FUNCTION IF EXISTS public.trg_enforce_tenant_immutability() CASCADE;
DROP FUNCTION IF EXISTS public.get_current_user_id() CASCADE;
DROP FUNCTION IF EXISTS public.get_current_family_id() CASCADE;

-- 4. Drop Privileged Helper Functions & Schemas
DROP FUNCTION IF EXISTS queue_sec.claim_next_worker_job(VARCHAR, VARCHAR, INT) CASCADE;
DROP FUNCTION IF EXISTS auth_sec.get_current_user_family_ids() CASCADE;
DROP FUNCTION IF EXISTS auth_sec.get_user_auth_credential(VARCHAR) CASCADE;
DROP FUNCTION IF EXISTS auth_sec.update_user_password(VARCHAR) CASCADE;

DROP SCHEMA IF EXISTS queue_sec CASCADE;
DROP SCHEMA IF EXISTS auth_sec CASCADE;

-- 5. Revoke Privileges on Public Schema
REVOKE ALL PRIVILEGES ON SCHEMA public FROM family_admin, family_app, family_worker, family_queue_definer, family_auth_definer, family_auth_verifier;
REVOKE family_queue_definer, family_auth_definer, family_auth_verifier FROM family_admin;

-- Note on Roles: Roles (family_admin, family_app, family_worker, family_queue_definer, family_auth_definer, family_auth_verifier)
-- are cluster-level objects and are preserved across individual database migrations to prevent cross-database privilege revocation errors.

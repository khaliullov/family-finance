#!/usr/bin/env python3
"""
Family Finance Security, Privilege, & Evidence Immutability Test Suite (Phase 0.11)
Validates production PostgreSQL 16+ schema against canonical specification in database.md:
  - Group 0: Canonical Schema Drift & Catalog Verification
  - Group 1: Role Privilege & Identity Isolation
  - Group 2: SECURITY DEFINER Function Integrity & Catalog Assertions
  - Group 3: Bank Evidence Immutability & Monotonic State Machine Transitions
  - Group 4: Append-Only Compliance & Invariant Triggers
  - Group 5: Registration Bootstrap Sequence under Canonical DDL & RLS
  - Group 6: Background Queue Claiming under FORCE RLS
  - Group 7: Comprehensive Tenant Isolation & Foreign Key Boundaries
"""

import sys
import subprocess
import uuid
import os
import re

TEST_DB = os.environ.get("TEST_DB", "test_family_finance_security")
CONTAINER = os.environ.get("POSTGRES_CONTAINER", "shared-postgres")
BASE_DIR = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
SCHEMA_SQL_PATH = os.path.join(BASE_DIR, "tests/security/schema.sql")
DATABASE_MD_PATH = os.path.join(BASE_DIR, "database.md")
sys.path.insert(0, os.path.join(BASE_DIR, "scripts"))
from extract_canonical_schema import extract_canonical_schema

PGHOST = os.environ.get("PGHOST")
PGPORT = os.environ.get("PGPORT", "5432")

def run_psql(user, sql, db=TEST_DB, expect_error=False):
    """Executes SQL via local psql (if PGHOST provided) or via docker exec container."""
    if PGHOST:
        cmd = [
            "psql", "-h", PGHOST, "-p", str(PGPORT),
            "-U", user, "-d", db, "-v", "ON_ERROR_STOP=1", "-t", "-A"
        ]
    else:
        cmd = [
            "docker", "exec", "-i", CONTAINER,
            "psql", "-U", user, "-d", db, "-v", "ON_ERROR_STOP=1", "-t", "-A"
        ]
    proc = subprocess.run(cmd, input=sql, text=True, capture_output=True)
    if expect_error:
        if proc.returncode == 0:
            return False, f"Expected error but command succeeded. Output: {proc.stdout.strip()}"
        return True, proc.stderr.strip()
    else:
        if proc.returncode != 0:
            return False, f"Unexpected error (exit code {proc.returncode}): {proc.stderr.strip()}"
        return True, proc.stdout.strip()

def setup_database():
    """Initializes clean test database and applies schema.sql or production migrations."""
    print(">>> Provisioning cluster infrastructure roles...")
    prov_script = os.path.join(BASE_DIR, "scripts", "provision_roles.sql")
    if os.path.exists(prov_script):
        with open(prov_script, "r") as f:
            ok, err = run_psql("postgres", f.read(), db="postgres")
            if not ok:
                print(f"Failed to provision cluster roles: {err}", file=sys.stderr)
                sys.exit(1)

    print(">>> Initializing clean test database...")
    drop_create_sql = f"DROP DATABASE IF EXISTS {TEST_DB}; CREATE DATABASE {TEST_DB};"
    ok, err = run_psql("postgres", drop_create_sql, db="postgres")
    if not ok:
        print(f"Failed to create test database: {err}", file=sys.stderr)
        sys.exit(1)

    if os.environ.get("USE_MIGRATIONS") == "1":
        print(">>> Applying production migrations (000001_initial_schema.up.sql & 000002_seed_categories.up.sql)...")
        mig1 = os.path.join(BASE_DIR, "migrations", "000001_initial_schema.up.sql")
        mig2 = os.path.join(BASE_DIR, "migrations", "000002_seed_categories.up.sql")
        with open(mig1, "r") as f:
            ok, err = run_psql("postgres", f.read(), db=TEST_DB)
            if not ok:
                print(f"Failed to apply migration 000001: {err}", file=sys.stderr)
                sys.exit(1)
        with open(mig2, "r") as f:
            ok, err = run_psql("postgres", f.read(), db=TEST_DB)
            if not ok:
                print(f"Failed to apply migration 000002: {err}", file=sys.stderr)
                sys.exit(1)
        print(">>> Production migrations applied successfully.\n")
    else:
        with open(SCHEMA_SQL_PATH, "r") as f:
            schema_sql = f.read()

        ok, err = run_psql("postgres", schema_sql, db=TEST_DB)
        if not ok:
            print(f"Failed to apply schema.sql: {err}", file=sys.stderr)
            sys.exit(1)
        print(">>> schema.sql applied successfully.\n")

def teardown_database():
    """Drops test database after test execution."""
    print("\n>>> Cleaning up test database...")
    drop_sql = f"DROP DATABASE IF EXISTS {TEST_DB};"
    ok, err = run_psql("postgres", drop_sql, db="postgres")
    if not ok:
        print(f"Warning: Failed to drop test database: {err}", file=sys.stderr)
    else:
        print(">>> Test database dropped cleanly.")

def seed_test_data():
    """Seeds base test fixtures for Family A and Family B within an atomic transaction."""
    seed_sql = """
    BEGIN;
    -- Insert test users
    INSERT INTO public.users (id, email, password_hash, first_name, last_name, is_active) VALUES
        ('11111111-1111-1111-1111-111111111111', 'alice@family-a.test', '$argon2id$v=19$m=65536,t=3,p=4$alicehash', 'Alice', 'Smith', TRUE),
        ('22222222-2222-2222-2222-222222222222', 'bob@family-a.test', '$argon2id$v=19$m=65536,t=3,p=4$bobhash', 'Bob', 'Smith', TRUE),
        ('33333333-3333-3333-3333-333333333333', 'charlie@family-b.test', '$argon2id$v=19$m=65536,t=3,p=4$charliehash', 'Charlie', 'Brown', TRUE);

    -- Insert families
    INSERT INTO public.families (id, name, default_currency) VALUES
        ('aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', 'Family A (Smith)', 'RUB'),
        ('bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb', 'Family B (Brown)', 'RUB');

    -- Insert memberships
    INSERT INTO public.family_members (id, family_id, user_id, role, color_code) VALUES
        ('f1111111-1111-1111-1111-111111111111', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', '11111111-1111-1111-1111-111111111111', 'owner', '#FF0000'),
        ('f2222222-2222-2222-2222-222222222222', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', '22222222-2222-2222-2222-222222222222', 'partner', '#00FF00'),
        ('f3333333-3333-3333-3333-333333333333', 'bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb', '33333333-3333-3333-3333-333333333333', 'owner', '#0000FF');

    -- Insert checking accounts
    INSERT INTO public.accounts (id, family_id, owner_user_id, name, type, currency, current_balance_minor) VALUES
        ('a1a1a1a1-a1a1-a1a1-a1a1-a1a1a1a1a1a1', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', '11111111-1111-1111-1111-111111111111', 'Alice Checking', 'checking', 'RUB', 5000000),
        ('b1b1b1b1-b1b1-b1b1-b1b1-b1b1b1b1b1b1', 'bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb', '33333333-3333-3333-3333-333333333333', 'Charlie Checking', 'checking', 'RUB', 2500000);

    -- Insert cards
    INSERT INTO public.cards (id, family_id, account_id, owner_user_id, masked_pan, display_name, payment_system, status) VALUES
        ('c1a1a1a1-a1a1-a1a1-a1a1-a1a1a1a1a1a1', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', 'a1a1a1a1-a1a1-a1a1-a1a1-a1a1a1a1a1a1', '11111111-1111-1111-1111-111111111111', '*1234', 'Alice Debit Card', 'MIR', 'ACTIVE'),
        ('c2b1b1b1-b1b1-b1b1-b1b1-b1b1b1b1b1b1', 'bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb', 'b1b1b1b1-b1b1-b1b1-b1b1-b1b1b1b1b1b1', '33333333-3333-3333-3333-333333333333', '*5678', 'Charlie Debit Card', 'MIR', 'ACTIVE');

    -- Insert categories
    INSERT INTO public.categories (id, family_id, name, type, icon, color) VALUES
        ('c1c1c1c1-c1c1-c1c1-c1c1-c1c1c1c1c1c1', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', 'Groceries', 'expense', 'shopping-cart', '#FF5733'),
        ('c2c2c2c2-c2c2-c2c2-c2c2-c2c2c2c2c2c2', 'bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb', 'Groceries', 'expense', 'shopping-cart', '#33FF57');

    -- Tx 1: Posted transaction in Family A (-150000)
    INSERT INTO public.bank_transactions (
        id, family_id, account_id, external_id, external_auth_id, fingerprint_hash,
        amount_minor, currency, account_amount_minor, account_currency,
        booking_status, operation_type, transaction_time, posted_time,
        merchant_name, raw_description, mcc
    ) VALUES (
        '10000000-0000-0000-0000-000000000001', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa',
        'a1a1a1a1-a1a1-a1a1-a1a1-a1a1a1a1a1a1', 'op_posted_1', 'auth_posted_1', 'fp_posted_1',
        -150000, 'RUB', -150000, 'RUB',
        'posted', 'purchase', NOW() - INTERVAL '2 days', NOW() - INTERVAL '1 day',
        'Supermarket', 'Grocery purchase', 5411
    );
    INSERT INTO public.accounting_entries (
        id, family_id, bank_transaction_id, account_id, category_id,
        amount_minor, currency, classification, entry_time, is_split
    ) VALUES (
        '20000000-0000-0000-0000-000000000001', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa',
        '10000000-0000-0000-0000-000000000001', 'a1a1a1a1-a1a1-a1a1-a1a1-a1a1a1a1a1a1',
        'c1c1c1c1-c1c1-c1c1-c1c1-c1c1c1c1c1c1', -150000, 'RUB', 'expense', NOW() - INTERVAL '2 days', FALSE
    );

    -- Tx 2: Pending transaction in Family A (-250000, external_id NULL, for settlement test)
    INSERT INTO public.bank_transactions (
        id, family_id, account_id, external_id, external_auth_id, fingerprint_hash,
        amount_minor, currency, account_amount_minor, account_currency,
        booking_status, operation_type, transaction_time, posted_time,
        merchant_name, raw_description, mcc
    ) VALUES (
        '10000000-0000-0000-0000-000000000002', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa',
        'a1a1a1a1-a1a1-a1a1-a1a1-a1a1a1a1a1a1', NULL, 'auth_hold_2', 'fp_hold_2',
        -250000, 'RUB', -250000, 'RUB',
        'pending', 'purchase', NOW() - INTERVAL '2 hours', NULL,
        'Restaurant', 'Restaurant bill pre-auth', 5812
    );
    INSERT INTO public.accounting_entries (
        id, family_id, bank_transaction_id, account_id, category_id,
        amount_minor, currency, classification, entry_time, is_split
    ) VALUES (
        '20000000-0000-0000-0000-000000000002', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa',
        '10000000-0000-0000-0000-000000000002', 'a1a1a1a1-a1a1-a1a1-a1a1-a1a1a1a1a1a1',
        'c1c1c1c1-c1c1-c1c1-c1c1-c1c1c1c1c1c1', -250000, 'RUB', 'expense', NOW() - INTERVAL '2 hours', FALSE
    );

    -- Tx 3: Pending transaction in Family A (-50000, for cancellation test)
    INSERT INTO public.bank_transactions (
        id, family_id, account_id, external_id, external_auth_id, fingerprint_hash,
        amount_minor, currency, account_amount_minor, account_currency,
        booking_status, operation_type, transaction_time, posted_time,
        merchant_name, raw_description, mcc
    ) VALUES (
        '10000000-0000-0000-0000-000000000003', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa',
        'a1a1a1a1-a1a1-a1a1-a1a1-a1a1a1a1a1a1', NULL, 'auth_hold_3', 'fp_hold_3',
        -50000, 'RUB', -50000, 'RUB',
        'pending', 'purchase', NOW() - INTERVAL '3 hours', NULL,
        'Hotel PreAuth', 'Incidentals hold', 7011
    );
    INSERT INTO public.accounting_entries (
        id, family_id, bank_transaction_id, account_id, category_id,
        amount_minor, currency, classification, entry_time, is_split
    ) VALUES (
        '20000000-0000-0000-0000-000000000003', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa',
        '10000000-0000-0000-0000-000000000003', 'a1a1a1a1-a1a1-a1a1-a1a1-a1a1a1a1a1a1',
        'c1c1c1c1-c1c1-c1c1-c1c1-c1c1c1c1c1c1', -50000, 'RUB', 'expense', NOW() - INTERVAL '3 hours', FALSE
    );

    -- Tx 4: Cancelled transaction in Family A
    INSERT INTO public.bank_transactions (
        id, family_id, account_id, external_id, external_auth_id, fingerprint_hash,
        amount_minor, currency, account_amount_minor, account_currency,
        booking_status, operation_type, transaction_time, posted_time,
        merchant_name, raw_description, mcc
    ) VALUES (
        '10000000-0000-0000-0000-000000000004', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa',
        'a1a1a1a1-a1a1-a1a1-a1a1-a1a1a1a1a1a1', NULL, 'auth_hold_4', 'fp_hold_4',
        -30000, 'RUB', -30000, 'RUB',
        'cancelled', 'purchase', NOW() - INTERVAL '5 days', NULL,
        'Gas Station', 'Dropped gas pre-auth', 5541
    );
    INSERT INTO public.accounting_entries (
        id, family_id, bank_transaction_id, account_id, category_id,
        amount_minor, currency, classification, entry_time, is_split
    ) VALUES (
        '20000000-0000-0000-0000-000000000004', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa',
        '10000000-0000-0000-0000-000000000004', 'a1a1a1a1-a1a1-a1a1-a1a1-a1a1a1a1a1a1',
        'c1c1c1c1-c1c1-c1c1-c1c1-c1c1c1c1c1c1', -30000, 'RUB', 'expense', NOW() - INTERVAL '5 days', FALSE
    );

    -- Tx 5: Pending transaction in Family A (-120000, for negative settlement tests)
    INSERT INTO public.bank_transactions (
        id, family_id, account_id, external_id, external_auth_id, fingerprint_hash,
        amount_minor, currency, account_amount_minor, account_currency,
        booking_status, operation_type, transaction_time, posted_time,
        merchant_name, raw_description, mcc
    ) VALUES (
        '10000000-0000-0000-0000-000000000005', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa',
        'a1a1a1a1-a1a1-a1a1-a1a1-a1a1a1a1a1a1', 'op_initial_5', 'auth_hold_5', 'fp_hold_5',
        -120000, 'RUB', -120000, 'RUB',
        'pending', 'purchase', NOW() - INTERVAL '1 day', NULL,
        'Electronics Store', 'Gadget purchase hold', 5732
    );
    INSERT INTO public.accounting_entries (
        id, family_id, bank_transaction_id, account_id, category_id,
        amount_minor, currency, classification, entry_time, is_split
    ) VALUES (
        '20000000-0000-0000-0000-000000000005', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa',
        '10000000-0000-0000-0000-000000000005', 'a1a1a1a1-a1a1-a1a1-a1a1-a1a1a1a1a1a1',
        'c1c1c1c1-c1c1-c1c1-c1c1-c1c1c1c1c1c1', -120000, 'RUB', 'expense', NOW() - INTERVAL '1 day', FALSE
    );

    -- Tx 6: Pending card_check transaction in Family A (amount = 0)
    INSERT INTO public.bank_transactions (
        id, family_id, account_id, external_id, external_auth_id, fingerprint_hash,
        amount_minor, currency, account_amount_minor, account_currency,
        booking_status, operation_type, transaction_time, posted_time,
        merchant_name, raw_description, mcc
    ) VALUES (
        '10000000-0000-0000-0000-000000000006', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa',
        'a1a1a1a1-a1a1-a1a1-a1a1-a1a1a1a1a1a1', NULL, 'auth_check_6', 'fp_check_6',
        0, 'RUB', 0, 'RUB',
        'pending', 'card_check', NOW() - INTERVAL '4 hours', NULL,
        'Subscription Verification', 'Card check verification', 5968
    );
    INSERT INTO public.accounting_entries (
        id, family_id, bank_transaction_id, account_id, category_id,
        amount_minor, currency, classification, entry_time, is_split
    ) VALUES (
        '20000000-0000-0000-0000-000000000006', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa',
        '10000000-0000-0000-0000-000000000006', 'a1a1a1a1-a1a1-a1a1-a1a1-a1a1a1a1a1a1',
        'c1c1c1c1-c1c1-c1c1-c1c1-c1c1c1c1c1c1', 0, 'RUB', 'excluded', NOW() - INTERVAL '4 hours', FALSE
    );

    -- Worker Job for cross-tenant claiming
    INSERT INTO public.worker_jobs (id, family_id, queue_name, job_type, payload, state) VALUES
        ('99999999-9999-9999-9999-999999999999', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', 'bank_sync', 'sync_tbank', '{"connection_id": "c1"}'::jsonb, 'available');

    -- Raw bank payload
    INSERT INTO public.raw_bank_payloads (id, family_id, source, payload_hash, raw_json) VALUES
        ('90000000-0000-0000-0000-000000000001', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', 'tbank_api', 'hash_seed_1', '{"sync": "initial"}'::jsonb);

    -- Audit log
    INSERT INTO public.audit_logs (id, family_id, entity_type, entity_id, action) VALUES
        ('80000000-0000-0000-0000-000000000001', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', 'family', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', 'create');

    COMMIT;
    """
    ok, err = run_psql("postgres", seed_sql)
    if not ok:
        print(f"Failed to seed test data: {err}", file=sys.stderr)
        sys.exit(1)
    print(">>> Test fixtures seeded successfully.\n")

def run_tests():
    tests = []

    print("================================================================================")
    print("   RUNNING FAMILY FINANCE POSTGRESQL 16+ SECURITY & INTEGRITY TEST SUITE       ")
    print("================================================================================")

    # -------------------------------------------------------------------------
    # Group 0: Canonical Schema Drift & Live Catalog Assertions (Phase 0.11 P1)
    # -------------------------------------------------------------------------
    print("\n--- Group 0: Canonical Schema Drift & Catalog Verification ---")

    # 0.1: Drift check: Extract from database.md and verify schema.sql on disk matches exactly
    extracted = extract_canonical_schema(DATABASE_MD_PATH)
    with open(SCHEMA_SQL_PATH, "r", encoding="utf-8") as f:
        committed_schema = f.read()
    drift_detected = (extracted != committed_schema)
    tests.append(("0.1: schema.sql matches canonical database.md extraction with ZERO drift", not drift_detected, f"Lengths: extracted={len(extracted)}, file={len(committed_schema)}" if drift_detected else ""))

    # 0.2: Assert all 16 canonical tables exist in live database
    sql = """
    SELECT tablename FROM pg_tables WHERE schemaname = 'public' ORDER BY tablename;
    """
    ok, out = run_psql("postgres", sql)
    tables = [t.strip() for t in out.splitlines() if t.strip()]
    expected_tables = sorted([
        "users", "families", "family_members", "bank_connections", "raw_bank_payloads",
        "accounts", "cards", "bank_transactions", "transfer_matches", "transfer_rejections",
        "categories", "accounting_entries", "categorization_rules", "statement_imports",
        "audit_logs", "worker_jobs", "user_sessions"
    ])
    tables_set = set(tables)
    expected_set = set(expected_tables)
    tables_match = expected_set.issubset(tables_set)
    tests.append(("0.2: All canonical production tables present in live catalog", tables_match, f"Found: {tables}"))

    # 0.3: Assert all 15+ tenant tables have FORCE ROW LEVEL SECURITY enabled
    sql = """
    SELECT c.relname 
    FROM pg_class c 
    JOIN pg_namespace n ON n.oid = c.relnamespace 
    WHERE n.nspname = 'public' AND c.relkind = 'r' 
      AND c.relname != 'users' 
      AND (NOT c.relrowsecurity OR NOT c.relforcerowsecurity);
    """
    ok, out = run_psql("postgres", sql)
    non_forced = [l.strip() for l in out.splitlines() if l.strip()]
    tests.append(("0.3: All tenant tables have FORCE ROW LEVEL SECURITY enabled", ok and len(non_forced) == 0, f"Unforced tables: {non_forced}"))

    # 0.4: Assert users table has rowsecurity = FALSE (global table)
    sql = """
    SELECT relrowsecurity FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'public' AND c.relname = 'users';
    """
    ok, out = run_psql("postgres", sql)
    tests.append(("0.4: Global users table has rowsecurity = FALSE", ok and out.strip() == "f", f"Got: {out}"))

    # 0.5: Assert family_members canonical columns (id, color_code, role)
    sql = """
    SELECT column_name FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'family_members' ORDER BY column_name;
    """
    ok, out = run_psql("postgres", sql)
    fm_cols = set(out.splitlines())
    expected_fm_cols = {"id", "family_id", "user_id", "role", "color_code", "created_at", "updated_at"}
    tests.append(("0.5: family_members contains full canonical column structure", expected_fm_cols.issubset(fm_cols), f"Missing: {expected_fm_cols - fm_cols}"))

    # -------------------------------------------------------------------------
    # Group 1: Role Privilege & Identity Isolation (Phase 0.9 / 0.10 P1)
    # -------------------------------------------------------------------------
    print("\n--- Group 1: Role Privilege & Credential Isolation ---")

    # 1.1: family_app direct SELECT on users.password_hash is blocked
    ok, out = run_psql("family_app", "SELECT password_hash FROM public.users;", expect_error=True)
    tests.append(("1.1: family_app SELECT password_hash blocked", ok, out))

    # 1.2: family_app can verify login credentials via auth_sec helper
    ok, out = run_psql("family_app", "SELECT user_id, password_hash, is_active FROM auth_sec.get_user_auth_credential('alice@family-a.test');")
    tests.append(("1.2: family_app auth_sec.get_user_auth_credential succeeds", ok, out))

    # 1.3: family_worker cannot read users.password_hash or email
    ok, out = run_psql("family_worker", "SELECT email, password_hash FROM public.users;", expect_error=True)
    tests.append(("1.3: family_worker SELECT credentials blocked", ok, out))

    # 1.4: family_worker cannot filter by email (column grant denied)
    ok, out = run_psql("family_worker", "SELECT id, first_name, is_active FROM public.users WHERE email = 'alice@family-a.test';", expect_error=True)
    tests.append(("1.4: family_worker WHERE email blocked by column grant", ok, out))

    # 1.5: family_worker can read permitted non-sensitive user columns
    ok, out = run_psql("family_worker", "SELECT id, first_name, is_active FROM public.users LIMIT 1;")
    tests.append(("1.5: family_worker SELECT non-sensitive columns allowed", ok, out))

    # 1.6: family_worker cannot update users
    ok, out = run_psql("family_worker", "UPDATE public.users SET first_name = 'hacked';", expect_error=True)
    tests.append(("1.6: family_worker UPDATE users blocked", ok, out))

    # 1.7: family_worker cannot execute membership discovery helper
    ok, out = run_psql("family_worker", "SELECT * FROM auth_sec.get_current_user_family_ids();", expect_error=True)
    tests.append(("1.7: family_worker EXECUTE auth_sec.get_current_user_family_ids blocked", ok, out))

    # 1.8: family_app can execute membership discovery helper with user context
    sql = """
    BEGIN;
    SELECT set_config('app.current_user_id', '11111111-1111-1111-1111-111111111111', true);
    SELECT auth_sec.get_current_user_family_ids();
    COMMIT;
    """
    ok, out = run_psql("family_app", sql)
    tests.append(("1.8: family_app EXECUTE auth_sec.get_current_user_family_ids succeeds", ok and "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa" in out, out))

    # 1.9: Runtime roles cannot create objects in public, auth_sec, or queue_sec schemas
    ok, out = run_psql("family_app", "CREATE TABLE public.malicious (id int);", expect_error=True)
    tests.append(("1.9a: family_app CREATE TABLE in public blocked", ok, out))
    ok, out = run_psql("family_worker", "CREATE TABLE public.malicious (id int);", expect_error=True)
    tests.append(("1.9b: family_worker CREATE TABLE in public blocked", ok, out))
    ok, out = run_psql("family_app", "CREATE TABLE auth_sec.malicious (id int);", expect_error=True)
    tests.append(("1.9c: family_app CREATE TABLE in auth_sec blocked", ok, out))

    # -------------------------------------------------------------------------
    # Group 2: SECURITY DEFINER Function Integrity & Catalog Assertions (Phase 0.11 P1)
    # -------------------------------------------------------------------------
    print("\n--- Group 2: SECURITY DEFINER Privileges & Catalog Assertions ---")

    # 2.1: Verify prosecdef = true, owner, and search_path on auth_sec functions
    sql = """
    SELECT p.proname, r.rolname, p.prosecdef, 'search_path=pg_catalog, public, pg_temp' = ANY(p.proconfig)
    FROM pg_proc p
    JOIN pg_namespace n ON n.oid = p.pronamespace
    JOIN pg_roles r ON r.oid = p.proowner
    WHERE n.nspname = 'auth_sec'
    ORDER BY p.proname;
    """
    ok, out = run_psql("postgres", sql)
    expected_auth_definer = [
        "get_current_user_family_ids|family_auth_definer|t|t",
        "get_user_auth_credential|family_auth_verifier|t|t",
        "revoke_session|family_auth_verifier|t|t",
        "rotate_refresh_token|family_auth_verifier|t|t",
        "update_user_password|family_auth_verifier|t|t"
    ]
    actual_auth = [l.strip() for l in out.splitlines() if l.strip()]
    tests.append(("2.1: auth_sec functions have prosecdef=t, owner, fixed search_path", actual_auth == expected_auth_definer, f"Got: {actual_auth}"))

    # 2.2: Verify prosecdef = true, owner, and search_path on queue_sec function
    sql = """
    SELECT p.proname, r.rolname, p.prosecdef, 'search_path=pg_catalog, public, pg_temp' = ANY(p.proconfig)
    FROM pg_proc p
    JOIN pg_namespace n ON n.oid = p.pronamespace
    JOIN pg_roles r ON r.oid = p.proowner
    WHERE n.nspname = 'queue_sec'
    ORDER BY p.proname;
    """
    ok, out = run_psql("postgres", sql)
    expected_queue_definer = [
        "claim_next_worker_job|family_queue_definer|t|t"
    ]
    actual_queue = [l.strip() for l in out.splitlines() if l.strip()]
    tests.append(("2.2: queue_sec functions have prosecdef=t, owner=family_queue_definer, fixed search_path", actual_queue == expected_queue_definer, f"Got: {actual_queue}"))

    # 2.3: Verify PUBLIC execute is revoked on auth_sec and queue_sec functions
    sql = """
    SELECT n.nspname, p.proname 
    FROM pg_proc p 
    JOIN pg_namespace n ON n.oid = p.pronamespace 
    WHERE n.nspname IN ('auth_sec', 'queue_sec') 
      AND has_function_privilege('public', p.oid, 'EXECUTE');
    """
    ok, out = run_psql("postgres", sql)
    public_exec = [l.strip() for l in out.splitlines() if l.strip()]
    tests.append(("2.3: PUBLIC EXECUTE revoked on all auth_sec and queue_sec routines", ok and len(public_exec) == 0, f"Public functions: {public_exec}"))

    # 2.4: Verify family_queue_definer has ZERO privileges on any business table
    sql = """
    SELECT table_name, privilege_type 
    FROM information_schema.role_table_grants 
    WHERE grantee = 'family_queue_definer' AND table_name != 'worker_jobs';
    """
    ok, out = run_psql("postgres", sql)
    extra_grants = [l.strip() for l in out.splitlines() if l.strip()]
    tests.append(("2.4: family_queue_definer has zero table grants outside worker_jobs", ok and len(extra_grants) == 0, f"Extra grants: {extra_grants}"))

    # 2.5: Verify family_auth_definer has zero financial table grants
    sql = """
    SELECT table_name, privilege_type 
    FROM information_schema.role_table_grants 
    WHERE grantee = 'family_auth_definer' 
      AND table_name IN ('accounts', 'cards', 'bank_transactions', 'accounting_entries', 'balance_snapshots', 'worker_jobs');
    """
    ok, out = run_psql("postgres", sql)
    financial_grants = [l.strip() for l in out.splitlines() if l.strip()]
    tests.append(("2.5: family_auth_definer has zero financial table grants", ok and len(financial_grants) == 0, f"Leaked grants: {financial_grants}"))

    # 2.6: Runtime check: family_queue_definer direct query on financial table blocked
    ok, out = run_psql("family_queue_definer", "SELECT * FROM public.accounts;", expect_error=True)
    tests.append(("2.6: family_queue_definer direct query on accounts blocked", ok, out))

    # 2.7: Runtime check: family_auth_definer direct query on bank_transactions blocked
    ok, out = run_psql("family_auth_definer", "SELECT * FROM public.bank_transactions;", expect_error=True)
    tests.append(("2.7: family_auth_definer direct query on bank_transactions blocked", ok, out))

    # -------------------------------------------------------------------------
    # Group 3: Bank Evidence Immutability & Monotonic State Machine (Phase 0.10 & 0.11 P1)
    # -------------------------------------------------------------------------
    print("\n--- Group 3: Bank Evidence Immutability & Settlement Transitions ---")

    # Positive Settlement Variants:
    # 3.1: pending -> posted with amount adjustment and matching accounting entry SUCCEEDS
    sql = """
    BEGIN;
    SELECT set_config('app.current_family_id', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', true);
    UPDATE public.bank_transactions 
    SET booking_status = 'posted',
        posted_time = NOW(),
        account_amount_minor = -265000,
        external_id = 'op_settled_tip_2',
        updated_at = NOW()
    WHERE id = '10000000-0000-0000-0000-000000000002';
    UPDATE public.accounting_entries
    SET amount_minor = -265000
    WHERE bank_transaction_id = '10000000-0000-0000-0000-000000000002';
    COMMIT;
    """
    ok, out = run_psql("family_worker", sql, expect_error=False)
    tests.append(("3.1: pending -> posted settlement with amount adjustment and entry sync SUCCEEDS", ok, out))

    # 3.2: pending -> posted settlement with unchanged amount SUCCEEDS
    run_psql("postgres", """
    BEGIN;
    INSERT INTO public.bank_transactions (
        id, family_id, account_id, external_auth_id, fingerprint_hash,
        amount_minor, currency, account_amount_minor, account_currency,
        booking_status, operation_type, transaction_time
    ) VALUES (
        '10000000-0000-0000-0000-000000000010', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa',
        'a1a1a1a1-a1a1-a1a1-a1a1-a1a1a1a1a1a1', 'auth_unchanged_amt', 'fp_unchanged_amt',
        -75000, 'RUB', -75000, 'RUB', 'pending', 'purchase', NOW() - INTERVAL '1 hour'
    );
    INSERT INTO public.accounting_entries (
        id, family_id, bank_transaction_id, account_id, category_id,
        amount_minor, currency, classification, entry_time, is_split
    ) VALUES (
        '20000000-0000-0000-0000-000000000010', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa',
        '10000000-0000-0000-0000-000000000010', 'a1a1a1a1-a1a1-a1a1-a1a1-a1a1a1a1a1a1',
        'c1c1c1c1-c1c1-c1c1-c1c1-c1c1c1c1c1c1', -75000, 'RUB', 'expense', NOW() - INTERVAL '1 hour', FALSE
    );
    COMMIT;
    """)
    sql = """
    BEGIN;
    SELECT set_config('app.current_family_id', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', true);
    UPDATE public.bank_transactions
    SET booking_status = 'posted',
        posted_time = transaction_time - INTERVAL '15 minutes', -- Batch clearing / timezone date alignment
        external_id = 'op_unchanged_10',
        updated_at = NOW()
    WHERE id = '10000000-0000-0000-0000-000000000010';
    COMMIT;
    """
    ok, out = run_psql("family_worker", sql, expect_error=False)
    tests.append(("3.2: pending -> posted settlement with date-aligned clearing timestamp SUCCEEDS", ok, out))

    # 3.3: pending -> posted retaining existing external_id SUCCEEDS
    run_psql("postgres", """
    BEGIN;
    INSERT INTO public.bank_transactions (
        id, family_id, account_id, external_id, external_auth_id, fingerprint_hash,
        amount_minor, currency, account_amount_minor, account_currency,
        booking_status, operation_type, transaction_time
    ) VALUES (
        '10000000-0000-0000-0000-000000000011', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa',
        'a1a1a1a1-a1a1-a1a1-a1a1-a1a1a1a1a1a1', 'op_existing_id', 'auth_existing_11', 'fp_existing_11',
        -45000, 'RUB', -45000, 'RUB', 'pending', 'purchase', NOW() - INTERVAL '1 hour'
    );
    INSERT INTO public.accounting_entries (
        id, family_id, bank_transaction_id, account_id, category_id,
        amount_minor, currency, classification, entry_time, is_split
    ) VALUES (
        '20000000-0000-0000-0000-000000000011', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa',
        '10000000-0000-0000-0000-000000000011', 'a1a1a1a1-a1a1-a1a1-a1a1-a1a1a1a1a1a1',
        'c1c1c1c1-c1c1-c1c1-c1c1-c1c1c1c1c1c1', -45000, 'RUB', 'expense', NOW() - INTERVAL '1 hour', FALSE
    );
    COMMIT;
    """)
    sql = """
    BEGIN;
    SELECT set_config('app.current_family_id', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', true);
    UPDATE public.bank_transactions
    SET booking_status = 'posted',
        posted_time = NOW(),
        updated_at = NOW()
    WHERE id = '10000000-0000-0000-0000-000000000011';
    COMMIT;
    """
    ok, out = run_psql("family_worker", sql, expect_error=False)
    tests.append(("3.3: pending -> posted retaining existing external_id SUCCEEDS", ok, out))

    # 3.4: pending -> posted card_check settlement with account_amount_minor = 0 SUCCEEDS
    sql = """
    BEGIN;
    SELECT set_config('app.current_family_id', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', true);
    UPDATE public.bank_transactions
    SET booking_status = 'posted',
        posted_time = NOW(),
        external_id = 'op_card_check_6',
        updated_at = NOW()
    WHERE id = '10000000-0000-0000-0000-000000000006';
    COMMIT;
    """
    ok, out = run_psql("family_worker", sql, expect_error=False)
    tests.append(("3.4: pending -> posted card_check settlement with zero amount SUCCEEDS", ok, out))

    # Negative Settlement Tests:
    # 3.5: Settlement with NULL posted_time FAILS
    sql = """
    BEGIN;
    SELECT set_config('app.current_family_id', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', true);
    UPDATE public.bank_transactions 
    SET booking_status = 'posted', posted_time = NULL 
    WHERE id = '10000000-0000-0000-0000-000000000005';
    COMMIT;
    """
    ok, out = run_psql("family_worker", sql, expect_error=True)
    tests.append(("3.5: Settlement with NULL posted_time FAILS", ok, out))

    # 3.6: Settlement attempting to mutate Category 1 fact (currency) FAILS
    sql = """
    BEGIN;
    SELECT set_config('app.current_family_id', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', true);
    UPDATE public.bank_transactions 
    SET booking_status = 'posted', posted_time = NOW(), currency = 'USD' 
    WHERE id = '10000000-0000-0000-0000-000000000005';
    COMMIT;
    """
    ok, out = run_psql("family_worker", sql, expect_error=True)
    tests.append(("3.6: Settlement mutating immutable currency FAILS", ok, out))

    # 3.7: Settlement with account_amount_minor = 0 for purchase FAILS
    sql = """
    BEGIN;
    SELECT set_config('app.current_family_id', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', true);
    UPDATE public.bank_transactions 
    SET booking_status = 'posted', posted_time = NOW(), account_amount_minor = 0 
    WHERE id = '10000000-0000-0000-0000-000000000005';
    COMMIT;
    """
    ok, out = run_psql("family_worker", sql, expect_error=True)
    tests.append(("3.7: Settlement with account_amount_minor = 0 on purchase FAILS", ok, out))

    # 3.8: Settlement reversing debit/credit direction FAILS
    sql = """
    BEGIN;
    SELECT set_config('app.current_family_id', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', true);
    UPDATE public.bank_transactions 
    SET booking_status = 'posted', posted_time = NOW(), account_amount_minor = 120000 
    WHERE id = '10000000-0000-0000-0000-000000000005';
    COMMIT;
    """
    ok, out = run_psql("family_worker", sql, expect_error=True)
    tests.append(("3.8: Settlement reversing debit/credit sign FAILS", ok, out))

    # 3.9: Settlement wiping previously set external_id to NULL FAILS
    sql = """
    BEGIN;
    SELECT set_config('app.current_family_id', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', true);
    UPDATE public.bank_transactions 
    SET booking_status = 'posted', posted_time = NOW(), external_id = NULL 
    WHERE id = '10000000-0000-0000-0000-000000000005';
    COMMIT;
    """
    ok, out = run_psql("family_worker", sql, expect_error=True)
    tests.append(("3.9: Settlement wiping external_id to NULL FAILS", ok, out))

    # 3.10: Settlement setting external_id to blank string FAILS
    sql = """
    BEGIN;
    SELECT set_config('app.current_family_id', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', true);
    UPDATE public.bank_transactions 
    SET booking_status = 'posted', posted_time = NOW(), external_id = '   ' 
    WHERE id = '10000000-0000-0000-0000-000000000005';
    COMMIT;
    """
    ok, out = run_psql("family_worker", sql, expect_error=True)
    tests.append(("3.10: Settlement setting blank string external_id FAILS", ok, out))

    # 3.11: Settlement attempting to mutate Category 1 fact (raw_description) FAILS
    sql = """
    BEGIN;
    SELECT set_config('app.current_family_id', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', true);
    UPDATE public.bank_transactions 
    SET booking_status = 'posted', posted_time = NOW(), raw_description = 'Tampered description' 
    WHERE id = '10000000-0000-0000-0000-000000000005';
    COMMIT;
    """
    ok, out = run_psql("family_worker", sql, expect_error=True)
    tests.append(("3.11: Settlement mutating raw_description FAILS", ok, out))

    # 3.12: Settlement attempting to mutate Category 1 fact (mcc) FAILS
    sql = """
    BEGIN;
    SELECT set_config('app.current_family_id', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', true);
    UPDATE public.bank_transactions 
    SET booking_status = 'posted', posted_time = NOW(), mcc = 9999 
    WHERE id = '10000000-0000-0000-0000-000000000005';
    COMMIT;
    """
    ok, out = run_psql("family_worker", sql, expect_error=True)
    tests.append(("3.12: Settlement mutating mcc FAILS", ok, out))

    # Terminal State Immutability:
    # 3.13: posted -> posted attempting amount mutation FAILS
    sql = """
    BEGIN;
    SELECT set_config('app.current_family_id', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', true);
    UPDATE public.bank_transactions 
    SET account_amount_minor = -999999 
    WHERE id = '10000000-0000-0000-0000-000000000001';
    COMMIT;
    """
    ok, out = run_psql("family_worker", sql, expect_error=True)
    tests.append(("3.13: posted -> posted amount modification FAILS", ok, out))

    # 3.14: posted -> posted attempting external_id mutation FAILS
    sql = """
    BEGIN;
    SELECT set_config('app.current_family_id', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', true);
    UPDATE public.bank_transactions 
    SET external_id = 'op_tampered_ext_id' 
    WHERE id = '10000000-0000-0000-0000-000000000001';
    COMMIT;
    """
    ok, out = run_psql("family_worker", sql, expect_error=True)
    tests.append(("3.14: posted -> posted external_id modification FAILS", ok, out))

    # 3.15: posted -> cancelled FAILS
    sql = """
    BEGIN;
    SELECT set_config('app.current_family_id', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', true);
    UPDATE public.bank_transactions 
    SET booking_status = 'cancelled' 
    WHERE id = '10000000-0000-0000-0000-000000000001';
    COMMIT;
    """
    ok, out = run_psql("family_worker", sql, expect_error=True)
    tests.append(("3.15: posted -> cancelled transition FAILS", ok, out))

    # 3.16: posted -> pending FAILS
    sql = """
    BEGIN;
    SELECT set_config('app.current_family_id', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', true);
    UPDATE public.bank_transactions 
    SET booking_status = 'pending' 
    WHERE id = '10000000-0000-0000-0000-000000000001';
    COMMIT;
    """
    ok, out = run_psql("family_worker", sql, expect_error=True)
    tests.append(("3.16: posted -> pending transition FAILS", ok, out))

    # 3.17: cancelled -> posted FAILS
    sql = """
    BEGIN;
    SELECT set_config('app.current_family_id', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', true);
    UPDATE public.bank_transactions 
    SET booking_status = 'posted', posted_time = NOW() 
    WHERE id = '10000000-0000-0000-0000-000000000004';
    COMMIT;
    """
    ok, out = run_psql("family_worker", sql, expect_error=True)
    tests.append(("3.17: cancelled -> posted transition FAILS", ok, out))

    # 3.18: cancelled -> pending FAILS
    sql = """
    BEGIN;
    SELECT set_config('app.current_family_id', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', true);
    UPDATE public.bank_transactions 
    SET booking_status = 'pending' 
    WHERE id = '10000000-0000-0000-0000-000000000004';
    COMMIT;
    """
    ok, out = run_psql("family_worker", sql, expect_error=True)
    tests.append(("3.18: cancelled -> pending transition FAILS", ok, out))

    # 3.19: cancelled -> cancelled modifying amount FAILS
    sql = """
    BEGIN;
    SELECT set_config('app.current_family_id', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', true);
    UPDATE public.bank_transactions 
    SET account_amount_minor = -12345 
    WHERE id = '10000000-0000-0000-0000-000000000004';
    COMMIT;
    """
    ok, out = run_psql("family_worker", sql, expect_error=True)
    tests.append(("3.19: cancelled -> cancelled amount modification FAILS", ok, out))

    # Pending Cancellation:
    # 3.20: pending -> cancelled modifying amount FAILS
    sql = """
    BEGIN;
    SELECT set_config('app.current_family_id', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', true);
    UPDATE public.bank_transactions 
    SET booking_status = 'cancelled', account_amount_minor = -9999 
    WHERE id = '10000000-0000-0000-0000-000000000003';
    COMMIT;
    """
    ok, out = run_psql("family_worker", sql, expect_error=True)
    tests.append(("3.20: pending -> cancelled modifying amount FAILS", ok, out))

    # 3.21: pending -> cancelled setting posted_time FAILS
    sql = """
    BEGIN;
    SELECT set_config('app.current_family_id', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', true);
    UPDATE public.bank_transactions 
    SET booking_status = 'cancelled', posted_time = NOW() 
    WHERE id = '10000000-0000-0000-0000-000000000003';
    COMMIT;
    """
    ok, out = run_psql("family_worker", sql, expect_error=True)
    tests.append(("3.21: pending -> cancelled setting posted_time FAILS", ok, out))

    # 3.22: pending -> cancelled without modifying settlement fields SUCCEEDS
    sql = """
    BEGIN;
    SELECT set_config('app.current_family_id', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', true);
    UPDATE public.bank_transactions 
    SET booking_status = 'cancelled', updated_at = NOW() 
    WHERE id = '10000000-0000-0000-0000-000000000003';
    COMMIT;
    """
    ok, out = run_psql("family_worker", sql, expect_error=False)
    tests.append(("3.22: pending -> cancelled without modifying settlement fields SUCCEEDS", ok, out))

    # Role table operations:
    # 3.23: family_app DELETE bank_transactions blocked
    ok, out = run_psql("family_app", "DELETE FROM public.bank_transactions;", expect_error=True)
    tests.append(("3.23: family_app DELETE bank_transactions blocked", ok, out))

    # 3.24: family_app UPDATE bank_transactions blocked
    sql = """
    BEGIN;
    SELECT set_config('app.current_family_id', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', true);
    UPDATE public.bank_transactions SET raw_description = 'app tampered';
    COMMIT;
    """
    ok, out = run_psql("family_app", sql, expect_error=True)
    tests.append(("3.24: family_app UPDATE bank_transactions blocked", ok, out))

    # 3.25: family_worker DELETE bank_transactions blocked
    ok, out = run_psql("family_worker", "DELETE FROM public.bank_transactions;", expect_error=True)
    tests.append(("3.25: family_worker DELETE bank_transactions blocked", ok, out))

    # -------------------------------------------------------------------------
    # Group 4: Append-Only Compliance & Invariant Triggers (Phase 0.9 / 0.10)
    # -------------------------------------------------------------------------
    print("\n--- Group 4: Append-Only Compliance & Invariant Triggers ---")

    # 4.1: raw_bank_payloads UPDATE blocked
    sql = """
    BEGIN;
    SELECT set_config('app.current_family_id', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', true);
    UPDATE public.raw_bank_payloads SET raw_json = '{"tampered": true}'::jsonb;
    COMMIT;
    """
    ok, out = run_psql("family_app", sql, expect_error=True)
    tests.append(("4.1: raw_bank_payloads UPDATE blocked", ok, out))

    # 4.2: raw_bank_payloads DELETE blocked
    sql = """
    BEGIN;
    SELECT set_config('app.current_family_id', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', true);
    DELETE FROM public.raw_bank_payloads;
    COMMIT;
    """
    ok, out = run_psql("family_app", sql, expect_error=True)
    tests.append(("4.2: raw_bank_payloads DELETE blocked", ok, out))

    # 4.3: audit_logs UPDATE blocked
    sql = """
    BEGIN;
    SELECT set_config('app.current_family_id', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', true);
    UPDATE public.audit_logs SET action = 'tampered';
    COMMIT;
    """
    ok, out = run_psql("family_app", sql, expect_error=True)
    tests.append(("4.3: audit_logs UPDATE blocked", ok, out))

    # 4.4: audit_logs DELETE blocked
    sql = """
    BEGIN;
    SELECT set_config('app.current_family_id', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', true);
    DELETE FROM public.audit_logs;
    COMMIT;
    """
    ok, out = run_psql("family_app", sql, expect_error=True)
    tests.append(("4.4: audit_logs DELETE blocked", ok, out))

    # 4.5: transfer_matches endpoint mutation blocked by trigger
    run_psql("postgres", """
    INSERT INTO public.transfer_matches (
        id, family_id, source_bank_transaction_id, destination_bank_transaction_id, match_type, confidence_score, status
    ) VALUES (
        '70000000-0000-0000-0000-000000000001', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa',
        '10000000-0000-0000-0000-000000000001', '10000000-0000-0000-0000-000000000002',
        'manual', 100.0, 'confirmed'
    );
    """)
    sql = """
    BEGIN;
    SELECT set_config('app.current_family_id', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', true);
    UPDATE public.transfer_matches 
    SET source_bank_transaction_id = '10000000-0000-0000-0000-000000000003' 
    WHERE id = '70000000-0000-0000-0000-000000000001';
    COMMIT;
    """
    ok, out = run_psql("family_app", sql, expect_error=True)
    tests.append(("4.5: transfer_matches endpoint modification blocked by trigger", ok, out))

    # 4.6: accounting_entries linking to unrelated transfer match FAILS
    sql = """
    BEGIN;
    SELECT set_config('app.current_family_id', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', true);
    UPDATE public.accounting_entries 
    SET transfer_match_id = '70000000-0000-0000-0000-000000000001' 
    WHERE id = '20000000-0000-0000-0000-000000000003';
    COMMIT;
    """
    ok, out = run_psql("family_app", sql, expect_error=True)
    tests.append(("4.6: accounting_entry linking to non-endpoint transfer match FAILS", ok, out))

    # 4.7: Split sum integrity: single entry marked is_split = TRUE FAILS
    sql = """
    BEGIN;
    SELECT set_config('app.current_family_id', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', true);
    UPDATE public.accounting_entries 
    SET is_split = TRUE 
    WHERE id = '20000000-0000-0000-0000-000000000001';
    COMMIT;
    """
    ok, out = run_psql("family_app", sql, expect_error=True)
    tests.append(("4.7: Single accounting entry with is_split = TRUE FAILS", ok, out))

    # -------------------------------------------------------------------------
    # Group 5: Registration Bootstrap Sequence under Canonical DDL & RLS (Phase 0.10 & 0.11 P1)
    # -------------------------------------------------------------------------
    print("\n--- Group 5: Registration Bootstrap Sequence under RLS ---")

    new_user_id = str(uuid.uuid4())
    new_family_id = str(uuid.uuid4())
    new_member_id = str(uuid.uuid4())
    new_cat_id = str(uuid.uuid4())

    bootstrap_sql = f"""
    BEGIN;
    SELECT set_config('app.current_user_id', '{new_user_id}', true);
    SELECT set_config('app.current_family_id', '{new_family_id}', true);

    -- 1. Insert into users (global table)
    INSERT INTO public.users (id, email, password_hash, first_name, last_name, is_active)
    VALUES ('{new_user_id}', 'dave@family-new.test', '$argon2id$v=19$m=65536,t=3,p=4$davehash', 'Dave', 'Miller', TRUE);

    -- 2. Insert into families (Passes RLS WITH CHECK: id = get_current_family_id())
    INSERT INTO public.families (id, name, default_currency)
    VALUES ('{new_family_id}', 'Family Miller', 'RUB');

    -- 3. Insert into family_members as owner (Passes RLS WITH CHECK: family_id = get_current_family_id())
    INSERT INTO public.family_members (id, family_id, user_id, role, color_code)
    VALUES ('{new_member_id}', '{new_family_id}', '{new_user_id}', 'owner', '#123456');

    -- 4. Seed initial category (Passes RLS WITH CHECK)
    INSERT INTO public.categories (id, family_id, name, type, icon, color)
    VALUES ('{new_cat_id}', '{new_family_id}', 'Groceries', 'expense', 'shopping-cart', '#654321');

    COMMIT;
    """
    ok, out = run_psql("family_app", bootstrap_sql, expect_error=False)
    tests.append(("5.1: Registration bootstrap with pre-generated UUIDs under canonical DDL SUCCEEDS", ok, out))

    # 5.2: Rollback verification: aborted registration leaves zero orphaned rows
    fail_user_id = str(uuid.uuid4())
    fail_family_id = str(uuid.uuid4())
    abort_sql = f"""
    BEGIN;
    SELECT set_config('app.current_user_id', '{fail_user_id}', true);
    SELECT set_config('app.current_family_id', '{fail_family_id}', true);

    INSERT INTO public.users (id, email, password_hash, first_name, last_name, is_active)
    VALUES ('{fail_user_id}', 'fail@abort.test', '$argon2id$hash', 'Fail', 'User', TRUE);

    INSERT INTO public.families (id, name, default_currency)
    VALUES ('{fail_family_id}', 'Abort Family', 'RUB');

    -- Deliberately raise exception to force rollback
    DO $$ BEGIN RAISE EXCEPTION 'Simulated payment/setup failure'; END $$;
    COMMIT;
    """
    ok, out = run_psql("family_app", abort_sql, expect_error=True)
    # Check that neither user nor family exists
    chk_sql = f"SELECT count(*) FROM public.users WHERE id = '{fail_user_id}';"
    _, out_user = run_psql("postgres", chk_sql)
    chk_fam_sql = f"SELECT count(*) FROM public.families WHERE id = '{fail_family_id}';"
    _, out_fam = run_psql("postgres", chk_fam_sql)
    rollback_clean = (out_user.strip() == "0" and out_fam.strip() == "0")
    tests.append(("5.2: Rollback on error leaves zero orphaned records in users/families", rollback_clean, f"User count: {out_user}, Family count: {out_fam}"))

    # 5.3: Duplicate email registration fails unique constraint
    dup_email_sql = f"""
    BEGIN;
    SELECT set_config('app.current_user_id', '{str(uuid.uuid4())}', true);
    SELECT set_config('app.current_family_id', '{str(uuid.uuid4())}', true);
    INSERT INTO public.users (id, email, password_hash, first_name, last_name, is_active)
    VALUES ('{str(uuid.uuid4())}', 'alice@family-a.test', '$argon2id$hash', 'Alice2', 'Dup', TRUE);
    COMMIT;
    """
    ok, out = run_psql("family_app", dup_email_sql, expect_error=True)
    tests.append(("5.3: Duplicate email registration fails unique constraint", ok, out))

    # 5.4: Arbitrary family_member injection across tenant boundary rejected by RLS WITH CHECK
    inject_sql = f"""
    BEGIN;
    SELECT set_config('app.current_user_id', '11111111-1111-1111-1111-111111111111', true);
    SELECT set_config('app.current_family_id', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', true);
    INSERT INTO public.family_members (id, family_id, user_id, role)
    VALUES ('{str(uuid.uuid4())}', 'bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb', '11111111-1111-1111-1111-111111111111', 'partner');
    COMMIT;
    """
    ok, out = run_psql("family_app", inject_sql, expect_error=True)
    tests.append(("5.4: Member injection into foreign family rejected by RLS WITH CHECK", ok, out))

    # 5.5: Bootstrapping into existing family rejected
    dup_fam_sql = f"""
    BEGIN;
    SELECT set_config('app.current_user_id', '{str(uuid.uuid4())}', true);
    SELECT set_config('app.current_family_id', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', true);
    INSERT INTO public.families (id, name, default_currency) VALUES ('aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', 'Hijack Smith', 'RUB');
    COMMIT;
    """
    ok, out = run_psql("family_app", dup_fam_sql, expect_error=True)
    tests.append(("5.5: Bootstrapping into existing family fails primary key/RLS", ok, out))

    # -------------------------------------------------------------------------
    # Group 6: Cross-Tenant Queue Claiming under FORCE RLS (Phase 0.10 & 0.11 P1)
    # -------------------------------------------------------------------------
    print("\n--- Group 6: Background Queue Claiming under FORCE RLS ---")

    # 6.1: queue_sec.claim_next_worker_job claims cross-tenant job under family_worker with no family context
    claim_sql = """
    SELECT job_id, family_id, job_type 
    FROM queue_sec.claim_next_worker_job('bank_sync', 'test-worker-1', 600);
    """
    ok, out = run_psql("family_worker", claim_sql, expect_error=False)
    claimed_correctly = ok and "99999999-9999-9999-9999-999999999999" in out and "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa" in out
    tests.append(("6.1: queue_sec.claim_next_worker_job claims cross-tenant job", claimed_correctly, out))

    # 6.2: family_worker direct SELECT on worker_jobs without family context returns 0 rows (FORCE RLS)
    sql = "SELECT count(*) FROM public.worker_jobs;"
    ok, out = run_psql("family_worker", sql)
    tests.append(("6.2: family_worker direct SELECT worker_jobs without context returns 0 rows", ok and out.strip() == "0", f"Got: {out}"))

    # -------------------------------------------------------------------------
    # Group 7: Comprehensive Tenant Isolation & Foreign Key Boundaries (Phase 0.11 P2)
    # -------------------------------------------------------------------------
    print("\n--- Group 7: Comprehensive Tenant Isolation & Foreign Key Boundaries ---")

    # 7.1: Missing family context returns 0 rows across all 15 tenant tables (deny-all)
    tenant_tables = [
        "families", "family_members", "bank_connections", "raw_bank_payloads",
        "accounts", "cards", "bank_transactions", "transfer_matches", "transfer_rejections",
        "categories", "accounting_entries", "categorization_rules", "statement_imports",
        "audit_logs", "worker_jobs", "user_sessions"
    ]
    deny_all_passed = True
    deny_details = []
    for tbl in tenant_tables:
        sql = f"SELECT count(*) FROM public.{tbl};"
        ok, out = run_psql("family_app", sql)
        if not ok or out.strip() != "0":
            deny_all_passed = False
            deny_details.append(f"{tbl}: count={out.strip()}")
    tests.append(("7.1: Missing context yields 0 rows (deny-all) across all 15 tenant tables", deny_all_passed, ", ".join(deny_details)))

    # 7.2: Family A context cannot see Family B rows in accounts
    sql = """
    BEGIN;
    SELECT set_config('app.current_family_id', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', true);
    SELECT count(*) FROM public.accounts WHERE id = 'b1b1b1b1-b1b1-b1b1-b1b1-b1b1b1b1b1b1';
    COMMIT;
    """
    ok, out = run_psql("family_app", sql)
    lines = [l.strip() for l in out.splitlines() if l.strip() and l.strip() not in ('BEGIN', 'COMMIT', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa')]
    tests.append(("7.2: Family A context cannot see Family B accounts", ok and lines and lines[-1] == "0", f"Returned: {out}"))

    # 7.3: Family A context cannot mutate Family B accounts (0 rows affected)
    sql = """
    BEGIN;
    SELECT set_config('app.current_family_id', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', true);
    UPDATE public.accounts SET name = 'hacked' WHERE id = 'b1b1b1b1-b1b1-b1b1-b1b1-b1b1b1b1b1b1';
    COMMIT;
    """
    ok, out = run_psql("family_app", sql)
    _, chk_name = run_psql("postgres", "SELECT name FROM public.accounts WHERE id = 'b1b1b1b1-b1b1-b1b1-b1b1-b1b1b1b1b1b1';")
    tests.append(("7.3: Family A context cannot update Family B account", chk_name.strip() == "Charlie Checking", f"Name is: {chk_name}"))

    # 7.4: Composite FK rejects account in Family A with owner from Family B
    sql = """
    BEGIN;
    SELECT set_config('app.current_family_id', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', true);
    INSERT INTO public.accounts (id, family_id, owner_user_id, name, type, currency)
    VALUES ('{id}', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', '33333333-3333-3333-3333-333333333333', 'Cross-Account', 'checking', 'RUB');
    COMMIT;
    """.format(id=str(uuid.uuid4()))
    ok, out = run_psql("family_app", sql, expect_error=True)
    tests.append(("7.4: Composite FK rejects account with cross-tenant owner_user_id", ok, out))

    # 7.5: Composite FK rejects card in Family A referencing account in Family B
    sql = """
    BEGIN;
    SELECT set_config('app.current_family_id', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', true);
    INSERT INTO public.cards (id, family_id, account_id, owner_user_id, masked_pan, display_name, payment_system)
    VALUES ('{id}', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', 'b1b1b1b1-b1b1-b1b1-b1b1-b1b1b1b1b1b1', '11111111-1111-1111-1111-111111111111', '*9999', 'Cross-Card', 'MIR');
    COMMIT;
    """.format(id=str(uuid.uuid4()))
    ok, out = run_psql("family_app", sql, expect_error=True)
    tests.append(("7.5: Composite FK rejects card referencing cross-tenant account_id", ok, out))

    # 7.6: Tenant immutability trigger blocks modifying family_id on accounts
    sql = """
    BEGIN;
    SELECT set_config('app.current_family_id', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', true);
    UPDATE public.accounts 
    SET family_id = 'bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb' 
    WHERE id = 'a1a1a1a1-a1a1-a1a1-a1a1-a1a1a1a1a1a1';
    COMMIT;
    """
    ok, out = run_psql("family_app", sql, expect_error=True)
    tests.append(("7.6: Tenant immutability trigger blocks modifying family_id on accounts", ok, out))

    # 7.7: Tenant immutability trigger blocks modifying family_id on bank_transactions
    sql = """
    BEGIN;
    SELECT set_config('app.current_family_id', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', true);
    UPDATE public.bank_transactions 
    SET family_id = 'bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb' 
    WHERE id = '10000000-0000-0000-0000-000000000001';
    COMMIT;
    """
    ok, out = run_psql("postgres", sql, expect_error=True)
    tests.append(("7.7: Tenant immutability trigger blocks modifying family_id on bank_transactions", ok, out))

    # 7.8: Connection pool context reset clears context and leaves session with deny-all
    sql = """
    SELECT set_config('app.current_family_id', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', false);
    SELECT count(*) FROM public.accounts;
    DISCARD ALL;
    SELECT count(*) FROM public.accounts;
    """
    ok, out = run_psql("family_app", sql)
    lines = [l.strip() for l in out.splitlines() if l.strip()]
    context_cleared = (lines and lines[-1] == "0")
    tests.append(("7.8: Connection pool context reset (DISCARD ALL) leaves session in deny-all", ok and context_cleared, f"Lines: {lines}"))

    # -------------------------------------------------------------------------
    # Evaluation and Reporting
    # -------------------------------------------------------------------------
    print("\n================================================================================")
    print("   TEST RESULTS SUMMARY                                                        ")
    print("================================================================================")
    
    passed = 0
    failed = 0
    for title, status, output in tests:
        mark = "[PASS]" if status else "[FAIL]"
        if status:
            passed += 1
            print(f"{mark} {title}")
        else:
            failed += 1
            print(f"{mark} {title}\n       Details: {output}")

    print("--------------------------------------------------------------------------------")
    print(f"Total Tests: {len(tests)} | Passed: {passed} | Failed: {failed}")
    print("================================================================================")

    teardown_database()

    if failed > 0:
        print("\nTEST SUITE FAILED.")
        sys.exit(1)
    else:
        print("\nALL TESTS PASSED SUCCESSFULLY.")
        sys.exit(0)

if __name__ == "__main__":
    setup_database()
    seed_test_data()
    run_tests()

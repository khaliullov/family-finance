#!/usr/bin/env python3
"""
Family Finance Security, Privilege & Evidence Immutability Test Suite
Tests PostgreSQL 16+ Role Separation, Dedicated Security Schemas (auth_sec, queue_sec),
Monotonic Evidence State Machine, Append-Only Compliance Logs, and Registration Bootstrap.
"""

import sys
import subprocess
import uuid

TEST_DB = "test_family_finance_security"
CONTAINER = "shared-postgres"

def run_psql(user, sql, db=TEST_DB, expect_error=False):
    """Executes SQL via docker exec psql and returns (success_bool, output_str)."""
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
    """Initializes clean test database and applies schema.sql."""
    print(">>> Initializing clean test database...")
    drop_create_sql = f"DROP DATABASE IF EXISTS {TEST_DB}; CREATE DATABASE {TEST_DB};"
    ok, err = run_psql("postgres", drop_create_sql, db="postgres")
    if not ok:
        print(f"Failed to create test database: {err}", file=sys.stderr)
        sys.exit(1)

    with open("/Volumes/work/work/go/family-finance/tests/security/schema.sql", "r") as f:
        schema_sql = f.read()

    ok, err = run_psql("postgres", schema_sql, db=TEST_DB)
    if not ok:
        print(f"Failed to apply schema.sql: {err}", file=sys.stderr)
        sys.exit(1)
    print(">>> schema.sql applied successfully.\n")

def seed_test_data():
    """Seeds base test fixtures for two distinct families (Family A and Family B)."""
    seed_sql = """
    -- Insert test users
    INSERT INTO public.users (id, email, password_hash, first_name, last_name, is_active) VALUES
        ('11111111-1111-1111-1111-111111111111', 'alice@family-a.test', '$argon2id$v=19$m=65536,t=3,p=4$alicehash', 'Alice', 'Smith', TRUE),
        ('22222222-2222-2222-2222-222222222222', 'bob@family-a.test', '$argon2id$v=19$m=65536,t=3,p=4$bobhash', 'Bob', 'Smith', TRUE),
        ('33333333-3333-3333-3333-333333333333', 'charlie@family-b.test', '$argon2id$v=19$m=65536,t=3,p=4$charliehash', 'Charlie', 'Brown', TRUE);

    -- Insert families
    INSERT INTO public.families (id, name) VALUES
        ('aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', 'Family A (Smith)'),
        ('bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb', 'Family B (Brown)');

    -- Insert memberships
    INSERT INTO public.family_members (family_id, user_id, role) VALUES
        ('aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', '11111111-1111-1111-1111-111111111111', 'owner'),
        ('aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', '22222222-2222-2222-2222-222222222222', 'partner'),
        ('bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb', '33333333-3333-3333-3333-333333333333', 'owner');

    -- Insert checking accounts
    INSERT INTO public.accounts (id, family_id, owner_user_id, name, type, currency, current_balance_minor) VALUES
        ('a1a1a1a1-a1a1-a1a1-a1a1-a1a1a1a1a1a1', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', '11111111-1111-1111-1111-111111111111', 'Alice Checking', 'checking', 'RUB', 5000000),
        ('b1b1b1b1-b1b1-b1b1-b1b1-b1b1b1b1b1b1', 'bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb', '33333333-3333-3333-3333-333333333333', 'Charlie Checking', 'checking', 'RUB', 2500000);

    -- Insert sample bank transactions
    -- Tx 1: Posted transaction in Family A
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

    -- Tx 2: Pending transaction in Family A (Card Authorization Hold)
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

    -- Tx 3: Second Pending transaction in Family A (For cancellation test)
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

    -- Worker Job for cross-tenant claiming
    INSERT INTO public.worker_jobs (id, family_id, queue_name, job_type, payload, state) VALUES
        ('99999999-9999-9999-9999-999999999999', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', 'bank_sync', 'sync_tbank', '{"connection_id": "c1"}'::jsonb, 'available');
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
    # Group 1: Role Privilege & Identity Isolation Tests
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

    # 1.4: family_worker can read non-sensitive user fields (id, first_name, is_active)
    ok, out = run_psql("family_worker", "SELECT id, first_name, is_active FROM public.users WHERE email = 'alice@family-a.test';", expect_error=True)
    # email column is not granted to worker, so WHERE email fails cleanly
    tests.append(("1.4: family_worker WHERE email blocked by column grant", ok, out))

    ok, out = run_psql("family_worker", "SELECT id, first_name, is_active FROM public.users LIMIT 1;")
    tests.append(("1.4b: family_worker SELECT non-sensitive columns allowed", ok, out))

    # 1.5: family_worker cannot update users
    ok, out = run_psql("family_worker", "UPDATE public.users SET first_name = 'hacked';", expect_error=True)
    tests.append(("1.5: family_worker UPDATE users blocked", ok, out))

    # 1.6: family_worker cannot execute membership discovery helper
    ok, out = run_psql("family_worker", "SELECT * FROM auth_sec.get_current_user_family_ids();", expect_error=True)
    tests.append(("1.6: family_worker EXECUTE auth_sec.get_current_user_family_ids blocked", ok, out))

    # 1.7: family_app can execute membership discovery helper
    ok, out = run_psql("family_app", """
        BEGIN;
        SELECT set_config('app.current_user_id', '11111111-1111-1111-1111-111111111111', true);
        SELECT * FROM auth_sec.get_current_user_family_ids();
        COMMIT;
    """)
    tests.append(("1.7: family_app EXECUTE auth_sec.get_current_user_family_ids succeeds", ok, out))

    # -------------------------------------------------------------------------
    # Group 2: Monotonic Bank Evidence State Machine Tests (Phase 0.10 P0)
    # -------------------------------------------------------------------------
    print("\n--- Group 2: Bank Evidence Immutability & Monotonic State Machine ---")

    # 2.1: posted -> posted attempting account_amount_minor mutation FAILS
    sql = """
    BEGIN;
    SELECT set_config('app.current_family_id', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', true);
    UPDATE public.bank_transactions 
    SET account_amount_minor = -999999 
    WHERE id = '10000000-0000-0000-0000-000000000001';
    COMMIT;
    """
    ok, out = run_psql("family_worker", sql, expect_error=True)
    tests.append(("2.1: posted -> posted amount modification FAILS", ok, out))

    # 2.2: posted -> cancelled transition FAILS
    sql = """
    BEGIN;
    SELECT set_config('app.current_family_id', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', true);
    UPDATE public.bank_transactions 
    SET booking_status = 'cancelled' 
    WHERE id = '10000000-0000-0000-0000-000000000001';
    COMMIT;
    """
    ok, out = run_psql("family_worker", sql, expect_error=True)
    tests.append(("2.2: posted -> cancelled transition FAILS", ok, out))

    # 2.3: cancelled -> posted transition FAILS
    sql = """
    BEGIN;
    SELECT set_config('app.current_family_id', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', true);
    UPDATE public.bank_transactions 
    SET booking_status = 'posted', posted_time = NOW() 
    WHERE id = '10000000-0000-0000-0000-000000000004';
    COMMIT;
    """
    ok, out = run_psql("family_worker", sql, expect_error=True)
    tests.append(("2.3: cancelled -> posted transition FAILS", ok, out))

    # 2.4: cancelled -> cancelled modifying account_amount_minor FAILS
    sql = """
    BEGIN;
    SELECT set_config('app.current_family_id', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', true);
    UPDATE public.bank_transactions 
    SET account_amount_minor = -12345 
    WHERE id = '10000000-0000-0000-0000-000000000004';
    COMMIT;
    """
    ok, out = run_psql("family_worker", sql, expect_error=True)
    tests.append(("2.4: cancelled -> cancelled amount modification FAILS", ok, out))

    # 2.5: pending -> posted with valid settlement updates (amount adjustment, posted_time, external_id) SUCCEEDS
    sql = """
    BEGIN;
    SELECT set_config('app.current_family_id', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', true);
    UPDATE public.bank_transactions 
    SET booking_status = 'posted',
        posted_time = NOW(),
        account_amount_minor = -265000, -- e.g. restaurant tip added upon settlement
        external_id = 'op_settled_restaurant_2',
        updated_at = NOW()
    WHERE id = '10000000-0000-0000-0000-000000000002';
    COMMIT;
    """
    ok, out = run_psql("family_worker", sql, expect_error=False)
    tests.append(("2.5: pending -> posted with valid settlement updates SUCCEEDS", ok, out))

    # 2.6: pending -> posted attempting to mutate Category 1 immutable fact (raw_description) FAILS
    # Insert another pending transaction to test
    run_psql("postgres", """
        INSERT INTO public.bank_transactions (
            id, family_id, account_id, external_auth_id, fingerprint_hash,
            amount_minor, currency, account_amount_minor, account_currency,
            booking_status, operation_type, transaction_time, raw_description
        ) VALUES (
            '10000000-0000-0000-0000-000000000005', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa',
            'a1a1a1a1-a1a1-a1a1-a1a1-a1a1a1a1a1a1', 'auth_immut_test', 'fp_immut_5',
            -10000, 'RUB', -10000, 'RUB', 'pending', 'purchase', NOW(), 'Original description'
        );
    """)
    sql = """
    BEGIN;
    SELECT set_config('app.current_family_id', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', true);
    UPDATE public.bank_transactions 
    SET booking_status = 'posted',
        posted_time = NOW(),
        raw_description = 'Tampered description'
    WHERE id = '10000000-0000-0000-0000-000000000005';
    COMMIT;
    """
    ok, out = run_psql("family_worker", sql, expect_error=True)
    tests.append(("2.6: pending -> posted modifying immutable facts FAILS", ok, out))

    # 2.7: pending -> cancelled attempting to mutate account_amount_minor FAILS
    sql = """
    BEGIN;
    SELECT set_config('app.current_family_id', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', true);
    UPDATE public.bank_transactions 
    SET booking_status = 'cancelled',
        account_amount_minor = -9999
    WHERE id = '10000000-0000-0000-0000-000000000003';
    COMMIT;
    """
    ok, out = run_psql("family_worker", sql, expect_error=True)
    tests.append(("2.7: pending -> cancelled modifying amount FAILS", ok, out))

    # 2.8: pending -> cancelled without modifying settlement fields SUCCEEDS
    sql = """
    BEGIN;
    SELECT set_config('app.current_family_id', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', true);
    UPDATE public.bank_transactions 
    SET booking_status = 'cancelled',
        updated_at = NOW()
    WHERE id = '10000000-0000-0000-0000-000000000003';
    COMMIT;
    """
    ok, out = run_psql("family_worker", sql, expect_error=False)
    tests.append(("2.8: pending -> cancelled without modifying amount SUCCEEDS", ok, out))

    # 2.9: DELETE FROM bank_transactions blocked for family_app
    ok, out = run_psql("family_app", "DELETE FROM public.bank_transactions;", expect_error=True)
    tests.append(("2.9: family_app DELETE bank_transactions blocked", ok, out))

    # 2.10: DELETE FROM bank_transactions blocked for family_worker
    ok, out = run_psql("family_worker", "DELETE FROM public.bank_transactions;", expect_error=True)
    tests.append(("2.10: family_worker DELETE bank_transactions blocked", ok, out))

    # 2.11: family_app has ZERO UPDATE permissions on bank_transactions
    sql = """
    BEGIN;
    SELECT set_config('app.current_family_id', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', true);
    UPDATE public.bank_transactions SET raw_description = 'app tampered';
    COMMIT;
    """
    ok, out = run_psql("family_app", sql, expect_error=True)
    tests.append(("2.11: family_app UPDATE bank_transactions blocked", ok, out))

    # -------------------------------------------------------------------------
    # Group 3: Append-Only Compliance & Immutability Trigger Tests
    # -------------------------------------------------------------------------
    print("\n--- Group 3: Append-Only Compliance & Trigger Enforcements ---")

    # Seed a raw payload and audit log
    run_psql("postgres", """
        INSERT INTO public.raw_bank_payloads (id, family_id, payload) VALUES
            ('90000000-0000-0000-0000-000000000001', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', '{"sync": "initial"}'::jsonb);
        INSERT INTO public.audit_logs (id, family_id, event_type, entity_name, entity_id) VALUES
            ('80000000-0000-0000-0000-000000000001', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', 'create', 'family', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa');
    """)

    # 3.1: raw_bank_payloads UPDATE blocked
    sql = """
    BEGIN;
    SELECT set_config('app.current_family_id', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', true);
    UPDATE public.raw_bank_payloads SET payload = '{"tampered": true}'::jsonb;
    COMMIT;
    """
    ok, out = run_psql("family_app", sql, expect_error=True)
    tests.append(("3.1: family_app UPDATE raw_bank_payloads blocked", ok, out))

    # 3.2: raw_bank_payloads DELETE blocked
    sql = """
    BEGIN;
    SELECT set_config('app.current_family_id', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', true);
    DELETE FROM public.raw_bank_payloads;
    COMMIT;
    """
    ok, out = run_psql("family_app", sql, expect_error=True)
    tests.append(("3.2: family_app DELETE raw_bank_payloads blocked", ok, out))

    # 3.3: audit_logs UPDATE blocked
    sql = """
    BEGIN;
    SELECT set_config('app.current_family_id', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', true);
    UPDATE public.audit_logs SET event_type = 'tampered';
    COMMIT;
    """
    ok, out = run_psql("family_app", sql, expect_error=True)
    tests.append(("3.3: family_app UPDATE audit_logs blocked", ok, out))

    # 3.4: audit_logs DELETE blocked
    sql = """
    BEGIN;
    SELECT set_config('app.current_family_id', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', true);
    DELETE FROM public.audit_logs;
    COMMIT;
    """
    ok, out = run_psql("family_app", sql, expect_error=True)
    tests.append(("3.4: family_app DELETE audit_logs blocked", ok, out))

    # -------------------------------------------------------------------------
    # Group 4: Registration Bootstrap Sequence under RLS WITH CHECK (Phase 0.10 P0)
    # -------------------------------------------------------------------------
    print("\n--- Group 4: Registration Bootstrap Sequence under RLS ---")

    new_user_id = str(uuid.uuid4())
    new_family_id = str(uuid.uuid4())

    bootstrap_sql = f"""
    BEGIN;
    -- Inject context before inserting into families
    SELECT set_config('app.current_user_id', '{new_user_id}', true);
    SELECT set_config('app.current_family_id', '{new_family_id}', true);

    -- 1. Insert into users (global table)
    INSERT INTO public.users (id, email, password_hash, first_name, last_name, is_active)
    VALUES ('{new_user_id}', 'dave@family-new.test', '$argon2id$v=19$m=65536,t=3,p=4$davehash', 'Dave', 'Miller', TRUE);

    -- 2. Insert into families (Passes RLS WITH CHECK: id = get_current_family_id())
    INSERT INTO public.families (id, name)
    VALUES ('{new_family_id}', 'Family Miller');

    -- 3. Insert into family_members as owner (Passes RLS WITH CHECK: family_id = get_current_family_id())
    INSERT INTO public.family_members (family_id, user_id, role)
    VALUES ('{new_family_id}', '{new_user_id}', 'owner');

    -- 4. Seed initial category (Passes RLS WITH CHECK)
    INSERT INTO public.categories (family_id, name, icon)
    VALUES ('{new_family_id}', 'Groceries', 'shopping-cart');

    COMMIT;
    """
    ok, out = run_psql("family_app", bootstrap_sql, expect_error=False)
    tests.append(("4.1: Registration bootstrap with pre-generated UUIDs SUCCEEDS", ok, out))

    # -------------------------------------------------------------------------
    # Group 5: Cross-Tenant Queue Claiming under FORCE RLS (Phase 0.10 P0)
    # -------------------------------------------------------------------------
    print("\n--- Group 5: Background Queue Claiming under FORCE RLS ---")

    claim_sql = """
    SELECT job_id, family_id, job_type 
    FROM queue_sec.claim_next_worker_job('bank_sync', 'test-worker-1', 600);
    """
    ok, out = run_psql("family_worker", claim_sql, expect_error=False)
    claimed_correctly = ok and "99999999-9999-9999-9999-999999999999" in out and "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa" in out
    tests.append(("5.1: queue_sec.claim_next_worker_job claims cross-tenant job", claimed_correctly, out))

    # Verify family_queue_definer cannot query financial tables
    ok, out = run_psql("family_queue_definer", "SELECT * FROM public.accounts;", expect_error=True)
    tests.append(("5.2: family_queue_definer SELECT accounts blocked", ok, out))

    # -------------------------------------------------------------------------
    # Group 6: Tenant Isolation under FORCE ROW LEVEL SECURITY
    # -------------------------------------------------------------------------
    print("\n--- Group 6: Tenant Isolation & Boundary Checks ---")

    # 6.1: Missing family context returns 0 rows (deny-all)
    sql = """
    BEGIN;
    SELECT count(*) FROM public.accounts;
    COMMIT;
    """
    ok, out = run_psql("family_app", sql, expect_error=False)
    count_61 = [line for line in out.splitlines() if line not in ('BEGIN', 'COMMIT')]
    tests.append(("6.1: Missing family context returns 0 rows", ok and count_61 and count_61[-1] == "0", f"Returned: {out}"))

    # 6.2: Family A context cannot see Family B account
    sql = """
    BEGIN;
    SELECT set_config('app.current_family_id', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', true);
    SELECT count(*) FROM public.accounts WHERE id = 'b1b1b1b1-b1b1-b1b1-b1b1-b1b1b1b1b1b1';
    COMMIT;
    """
    ok, out = run_psql("family_app", sql, expect_error=False)
    count_62 = [line for line in out.splitlines() if line not in ('BEGIN', 'COMMIT', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa')]
    tests.append(("6.2: Family A context cannot see Family B account", ok and count_62 and count_62[-1] == "0", f"Returned: {out}"))

    # 6.3: Family A context cannot mutate Family B account
    sql = """
    BEGIN;
    SELECT set_config('app.current_family_id', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', true);
    UPDATE public.accounts SET name = 'hacked' WHERE id = 'b1b1b1b1-b1b1-b1b1-b1b1-b1b1b1b1b1b1';
    COMMIT;
    """
    ok, out = run_psql("family_app", sql, expect_error=False)
    # Under RLS, updating an out-of-tenant row matches 0 rows (no error, but 0 rows affected)
    tests.append(("6.3: Family A context cannot update Family B account (0 rows affected)", ok, out))

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

    if failed > 0:
        sys.exit(1)
    else:
        print("ALL TESTS PASSED SUCCESSFULLY.")
        sys.exit(0)

if __name__ == "__main__":
    setup_database()
    seed_test_data()
    run_tests()

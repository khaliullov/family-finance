package database

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func getAppConnString() string {
	host := os.Getenv("PGHOST")
	if host == "" {
		host = "localhost"
	}
	port := os.Getenv("PGPORT")
	if port == "" {
		port = "5432"
	}
	dbname := os.Getenv("PGDATABASE")
	if dbname == "" {
		dbname = "family_finance_migration_test"
	}
	return "postgres://family_app:app_secret_placeholder@" + host + ":" + port + "/" + dbname + "?sslmode=disable"
}

func getWorkerConnString() string {
	host := os.Getenv("PGHOST")
	if host == "" {
		host = "localhost"
	}
	port := os.Getenv("PGPORT")
	if port == "" {
		port = "5432"
	}
	dbname := os.Getenv("PGDATABASE")
	if dbname == "" {
		dbname = "family_finance_migration_test"
	}
	return "postgres://family_worker:worker_secret_placeholder@" + host + ":" + port + "/" + dbname + "?sslmode=disable"
}

// setupTestTenantFixture creates users and families using unique emails and family IDs.
func setupTestTenantFixture(t *testing.T, pool *pgxpool.Pool) (uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID) {
	ctx := context.Background()

	familyA := uuid.New()
	familyB := uuid.New()
	userAlice := uuid.New()
	userBob := uuid.New()

	emailAlice := fmt.Sprintf("alice-%s@test.local", uuid.New().String()[:8])
	emailBob := fmt.Sprintf("bob-%s@test.local", uuid.New().String()[:8])

	// 1. Insert users (global table with column grants to family_app)
	_, err := pool.Exec(ctx, `
		INSERT INTO public.users (id, email, password_hash, first_name, last_name, is_active)
		VALUES 
			($1, $2, 'hash_alice', 'Alice', 'Smith', TRUE),
			($3, $4, 'hash_bob', 'Bob', 'Jones', TRUE);
	`, userAlice, emailAlice, userBob, emailBob)
	if err != nil {
		t.Fatalf("failed to insert users fixture: %v", err)
	}

	// 2. Insert families & members using WithFamilyUserTx under family_app
	err = WithFamilyUserTx(ctx, pool, familyA, userAlice, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "INSERT INTO public.families (id, name) VALUES ($1, 'Family A')", familyA)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, "INSERT INTO public.family_members (id, family_id, user_id, role, color_code) VALUES ($1, $2, $3, 'owner', '#FF0000')",
			uuid.New(), familyA, userAlice)
		return err
	})
	if err != nil {
		t.Fatalf("failed to bootstrap Family A: %v", err)
	}

	err = WithFamilyUserTx(ctx, pool, familyB, userBob, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "INSERT INTO public.families (id, name) VALUES ($1, 'Family B')", familyB)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, "INSERT INTO public.family_members (id, family_id, user_id, role, color_code) VALUES ($1, $2, $3, 'owner', '#00FF00')",
			uuid.New(), familyB, userBob)
		return err
	})
	if err != nil {
		t.Fatalf("failed to bootstrap Family B: %v", err)
	}

	return familyA, familyB, userAlice, userBob
}

func TestWithFamilyTx_TenantIsolation(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, getAppConnString())
	if err != nil {
		t.Fatalf("failed to create pool as family_app: %v", err)
	}
	defer pool.Close()

	familyA, familyB, userAlice, userBob := setupTestTenantFixture(t, pool)

	accountA := uuid.New()
	accountB := uuid.New()

	// Insert Account in Family A
	err = WithFamilyTx(ctx, pool, familyA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO public.accounts (id, family_id, owner_user_id, name, type, currency, current_balance_minor)
			VALUES ($1, $2, $3, 'Alice Checking', 'checking', 'RUB', 100000);
		`, accountA, familyA, userAlice)
		return err
	})
	if err != nil {
		t.Fatalf("failed to insert account A: %v", err)
	}

	// Insert Account in Family B
	err = WithFamilyTx(ctx, pool, familyB, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO public.accounts (id, family_id, owner_user_id, name, type, currency, current_balance_minor)
			VALUES ($1, $2, $3, 'Bob Checking', 'checking', 'RUB', 200000);
		`, accountB, familyB, userBob)
		return err
	})
	if err != nil {
		t.Fatalf("failed to insert account B: %v", err)
	}

	// Read under Family A context: must see Account A and ZERO accounts from Family B
	err = WithFamilyTx(ctx, pool, familyA, func(tx pgx.Tx) error {
		var count int
		err := tx.QueryRow(ctx, "SELECT count(*) FROM public.accounts").Scan(&count)
		if err != nil {
			return err
		}
		if count != 1 {
			t.Errorf("Family A should see exactly 1 account, saw %d", count)
		}

		// Direct query for Account B by UUID returns 0 rows (denied by RLS)
		var bCount int
		err = tx.QueryRow(ctx, "SELECT count(*) FROM public.accounts WHERE id = $1", accountB).Scan(&bCount)
		if err != nil {
			return err
		}
		if bCount != 0 {
			t.Errorf("Family A must not be able to query Account B, found %d rows", bCount)
		}

		// Mutation attempt on Account B from Family A affects 0 rows
		tag, err := tx.Exec(ctx, "UPDATE public.accounts SET name = 'Hacked' WHERE id = $1", accountB)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			t.Errorf("expected 0 rows affected when Family A updates Account B, got %d", tag.RowsAffected())
		}

		return nil
	})
	if err != nil {
		t.Fatalf("Family A read check failed: %v", err)
	}

	// Read under Family B context: must see Account B only
	err = WithFamilyTx(ctx, pool, familyB, func(tx pgx.Tx) error {
		var name string
		err := tx.QueryRow(ctx, "SELECT name FROM public.accounts WHERE id = $1", accountB).Scan(&name)
		if err != nil {
			return err
		}
		if name != "Bob Checking" {
			t.Errorf("Account B name altered unexpectedly: %s", name)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Family B read check failed: %v", err)
	}
}

func TestWithFamilyTx_RollbackOnFailure(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, getAppConnString())
	if err != nil {
		t.Fatalf("failed to create pool as family_app: %v", err)
	}
	defer pool.Close()

	familyA, _, userAlice, _ := setupTestTenantFixture(t, pool)

	failedAccountID := uuid.New()
	simulatedErr := errors.New("simulated domain error after insert")

	err = WithFamilyTx(ctx, pool, familyA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO public.accounts (id, family_id, owner_user_id, name, type, currency, current_balance_minor)
			VALUES ($1, $2, $3, 'Doomed Account', 'checking', 'RUB', 50000);
		`, failedAccountID, familyA, userAlice)
		if err != nil {
			return err
		}
		return simulatedErr
	})

	if !errors.Is(err, simulatedErr) {
		t.Fatalf("expected simulated error, got %v", err)
	}

	// Verify that account was rolled back
	err = WithFamilyTx(ctx, pool, familyA, func(tx pgx.Tx) error {
		var count int
		err := tx.QueryRow(ctx, "SELECT count(*) FROM public.accounts WHERE id = $1", failedAccountID).Scan(&count)
		if err != nil {
			return err
		}
		if count != 0 {
			t.Errorf("expected doomed account to be rolled back, found count=%d", count)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("check failed: %v", err)
	}
}

func TestWithFamilyTx_NoGUCLeakInConnectionPool(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, getAppConnString())
	if err != nil {
		t.Fatalf("failed to create pool as family_app: %v", err)
	}
	defer pool.Close()

	familyA, familyB, _, _ := setupTestTenantFixture(t, pool)

	// 1. Direct pool query without WithFamilyTx: GUC must be empty, returning 0 rows
	var guc string
	err = pool.QueryRow(ctx, "SELECT current_setting('app.current_family_id', true)").Scan(&guc)
	if err != nil {
		t.Fatalf("querying GUC failed: %v", err)
	}
	if guc != "" {
		t.Errorf("expected empty GUC outside transaction, got %q", guc)
	}

	// Force RLS deny-all check: querying accounts without context returns 0 rows
	var count int
	err = pool.QueryRow(ctx, "SELECT count(*) FROM public.accounts").Scan(&count)
	if err != nil {
		t.Fatalf("querying accounts without context failed: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 accounts returned without family context (deny-all), got %d", count)
	}

	// 2. High-concurrency test across pooled connections
	const concurrency = 20
	var wg sync.WaitGroup
	errCh := make(chan error, concurrency)

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		targetFamily := familyA
		if i%2 == 1 {
			targetFamily = familyB
		}

		go func(fID uuid.UUID) {
			defer wg.Done()
			err := WithFamilyTx(ctx, pool, fID, func(tx pgx.Tx) error {
				var activeGUC string
				if err := tx.QueryRow(ctx, "SELECT current_setting('app.current_family_id', true)").Scan(&activeGUC); err != nil {
					return err
				}
				if activeGUC != fID.String() {
					return errors.New("active GUC mismatch in concurrent transaction")
				}
				return nil
			})
			if err != nil {
				errCh <- err
			}
		}(targetFamily)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatalf("concurrent WithFamilyTx execution failed: %v", err)
	}

	// 3. Verify clean state after all concurrent transactions complete
	err = pool.QueryRow(ctx, "SELECT current_setting('app.current_family_id', true)").Scan(&guc)
	if err != nil || guc != "" {
		t.Errorf("residual GUC detected after concurrent pool run: %q, err=%v", guc, err)
	}
}

func TestWithFamilyTx_CompositeFKEnforcement(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, getAppConnString())
	if err != nil {
		t.Fatalf("failed to create pool as family_app: %v", err)
	}
	defer pool.Close()

	familyA, _, _, userBob := setupTestTenantFixture(t, pool)

	// Attempt to insert an account in Family A with owner_user_id = Bob (who is in Family B only)
	// Must fail composite foreign key: (family_id, owner_user_id) REFERENCES family_members(family_id, user_id)
	err = WithFamilyTx(ctx, pool, familyA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO public.accounts (id, family_id, owner_user_id, name, type, currency, current_balance_minor)
			VALUES ($1, $2, $3, 'Illegal Account', 'checking', 'RUB', 100000);
		`, uuid.New(), familyA, userBob)
		return err
	})

	if err == nil {
		t.Fatalf("expected composite foreign key error on cross-tenant user ownership, got nil")
	}
}

func TestWithFamilyTx_PanicSafety(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, getAppConnString())
	if err != nil {
		t.Fatalf("failed to create pool as family_app: %v", err)
	}
	defer pool.Close()

	familyA, _, userAlice, _ := setupTestTenantFixture(t, pool)
	accountID := uuid.New()

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatalf("expected panic to be propagated, got nil")
			}
		}()

		_ = WithFamilyTx(ctx, pool, familyA, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				INSERT INTO public.accounts (id, family_id, owner_user_id, name, type, currency, current_balance_minor)
				VALUES ($1, $2, $3, 'Panic Account', 'checking', 'RUB', 50000);
			`, accountID, familyA, userAlice)
			if err != nil {
				return err
			}
			panic("simulated callback panic")
		})
	}()

	// Verify rollback succeeded and account was NOT committed
	var count int
	err = WithFamilyTx(ctx, pool, familyA, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT count(*) FROM public.accounts WHERE id = $1", accountID).Scan(&count)
	})
	if err != nil || count != 0 {
		t.Fatalf("expected 0 accounts after panic rollback, got count=%d, err=%v", count, err)
	}
}

func TestValidatePoolSecurity_RejectsSuperuser(t *testing.T) {
	ctx := context.Background()
	// Test that connecting as superuser postgres without ALLOW_SUPERUSER_DB fails security validation
	origEnv := os.Getenv("ALLOW_SUPERUSER_DB")
	os.Unsetenv("ALLOW_SUPERUSER_DB")
	defer func() {
		if origEnv != "" {
			os.Setenv("ALLOW_SUPERUSER_DB", origEnv)
		}
	}()

	superConnStr := getPGConnString() // connects as postgres (superuser)
	superPool, err := pgxpool.New(ctx, superConnStr)
	if err != nil {
		t.Fatalf("failed to connect as postgres: %v", err)
	}
	defer superPool.Close()

	err = ValidatePoolSecurity(ctx, superPool)
	if err == nil {
		t.Fatalf("expected ValidatePoolSecurity to reject superuser role, but it succeeded")
	}
	if !errors.Is(err, ErrSuperuserProhibited) {
		t.Errorf("expected ErrSuperuserProhibited, got %v", err)
	}
}

func TestApplicationStartup_SecurityValidation_Success(t *testing.T) {
	ctx := context.Background()
	// Test that production application startup (NewPool as family_app) passes all security validations
	origEnv := os.Getenv("ALLOW_SUPERUSER_DB")
	os.Unsetenv("ALLOW_SUPERUSER_DB")
	defer func() {
		if origEnv != "" {
			os.Setenv("ALLOW_SUPERUSER_DB", origEnv)
		}
	}()

	pool, err := NewPool(ctx, getAppConnString())
	if err != nil {
		t.Fatalf("production startup NewPool failed for family_app: %v", err)
	}
	defer pool.Close()

	// Verify pool is usable
	var currentUser string
	err = pool.QueryRow(ctx, "SELECT CURRENT_USER").Scan(&currentUser)
	if err != nil || currentUser != "family_app" {
		t.Fatalf("expected current user family_app, got %q, err=%v", currentUser, err)
	}
}

func TestApplicationStartup_SecurityValidation_SuperuserFailsStartup(t *testing.T) {
	ctx := context.Background()
	// Ensure NewPool as postgres (superuser) fails application startup and closes pool
	origEnv := os.Getenv("ALLOW_SUPERUSER_DB")
	os.Unsetenv("ALLOW_SUPERUSER_DB")
	defer func() {
		if origEnv != "" {
			os.Setenv("ALLOW_SUPERUSER_DB", origEnv)
		}
	}()

	pool, err := NewPool(ctx, getPGConnString())
	if err == nil {
		if pool != nil {
			pool.Close()
		}
		t.Fatalf("expected NewPool as superuser postgres to fail startup, but it succeeded")
	}
	if !errors.Is(err, ErrSuperuserProhibited) {
		t.Errorf("expected ErrSuperuserProhibited, got %v", err)
	}
}

func TestApplicationPool_CannotEscalatePrivileges(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, getAppConnString())
	if err != nil {
		t.Fatalf("failed to connect as family_app: %v", err)
	}
	defer pool.Close()

	// 1. Assert family_app cannot SET ROLE postgres
	_, err = pool.Exec(ctx, "SET ROLE postgres")
	if err == nil {
		_, _ = pool.Exec(ctx, "RESET ROLE")
		t.Fatalf("security violation: family_app was able to SET ROLE postgres!")
	}

	// 2. Assert family_app cannot SET ROLE family_admin
	_, err = pool.Exec(ctx, "SET ROLE family_admin")
	if err == nil {
		_, _ = pool.Exec(ctx, "RESET ROLE")
		t.Fatalf("security violation: family_app was able to SET ROLE family_admin!")
	}
}

func TestApplicationPool_CannotBypassRLS(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, getAppConnString())
	if err != nil {
		t.Fatalf("failed to connect as family_app: %v", err)
	}
	defer pool.Close()

	familyA, _, userAlice, _ := setupTestTenantFixture(t, pool)

	// Insert an account in Family A
	err = WithFamilyTx(ctx, pool, familyA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO public.accounts (id, family_id, owner_user_id, name, type, currency, current_balance_minor)
			VALUES ($1, $2, $3, 'RLS Test Account', 'checking', 'RUB', 1000);
		`, uuid.New(), familyA, userAlice)
		return err
	})
	if err != nil {
		t.Fatalf("failed to insert test account: %v", err)
	}

	// Query directly outside WithFamilyTx (no app.current_family_id set)
	var count int
	err = pool.QueryRow(ctx, "SELECT count(*) FROM public.accounts").Scan(&count)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if count != 0 {
		t.Fatalf("security violation: query without tenant context returned %d rows; RLS was bypassed!", count)
	}
}

func TestValidatePoolSecurity_FailClosed_AllChecks(t *testing.T) {
	ctx := context.Background()

	origEnv := os.Getenv("ALLOW_SUPERUSER_DB")
	os.Unsetenv("ALLOW_SUPERUSER_DB")
	defer func() {
		if origEnv != "" {
			os.Setenv("ALLOW_SUPERUSER_DB", origEnv)
		}
	}()

	// 1. Production pool (family_app) succeeds
	appPool, err := pgxpool.New(ctx, getAppConnString())
	if err != nil {
		t.Fatalf("failed to connect as family_app: %v", err)
	}
	defer appPool.Close()

	if err := ValidatePoolSecurity(ctx, appPool); err != nil {
		t.Fatalf("expected ValidatePoolSecurity to pass for family_app, got: %v", err)
	}

	// 2. Superuser pool fails closed
	pgPool, err := pgxpool.New(ctx, getPGConnString())
	if err != nil {
		t.Fatalf("failed to connect as postgres: %v", err)
	}
	defer pgPool.Close()

	if err := ValidatePoolSecurity(ctx, pgPool); err == nil {
		t.Fatalf("expected ValidatePoolSecurity to fail for postgres superuser, but it passed")
	}
}

func TestValidatePoolSecurity_AllowSuperuserDB_RestrictedToNonProduction(t *testing.T) {
	ctx := context.Background()

	origEnv := os.Getenv("ALLOW_SUPERUSER_DB")
	origAppEnv := os.Getenv("APP_ENV")
	defer func() {
		if origEnv != "" {
			os.Setenv("ALLOW_SUPERUSER_DB", origEnv)
		} else {
			os.Unsetenv("ALLOW_SUPERUSER_DB")
		}
		if origAppEnv != "" {
			os.Setenv("APP_ENV", origAppEnv)
		} else {
			os.Unsetenv("APP_ENV")
		}
	}()

	pgPool, err := pgxpool.New(ctx, getPGConnString())
	if err != nil {
		t.Fatalf("failed to connect as postgres: %v", err)
	}
	defer pgPool.Close()

	os.Setenv("ALLOW_SUPERUSER_DB", "1")

	// 1. APP_ENV=production must FAIL
	os.Setenv("APP_ENV", "production")
	err = ValidatePoolSecurity(ctx, pgPool)
	if err == nil || !strings.Contains(err.Error(), "strictly prohibited in production") {
		t.Fatalf("expected error rejecting bypass in production, got: %v", err)
	}

	// 2. APP_ENV=prod must FAIL
	os.Setenv("APP_ENV", "prod")
	err = ValidatePoolSecurity(ctx, pgPool)
	if err == nil || !strings.Contains(err.Error(), "strictly prohibited in production") {
		t.Fatalf("expected error rejecting bypass in prod, got: %v", err)
	}

	// 3. Unknown APP_ENV must FAIL
	os.Setenv("APP_ENV", "staging")
	err = ValidatePoolSecurity(ctx, pgPool)
	if err == nil || !strings.Contains(err.Error(), "only permitted in explicit development/test") {
		t.Fatalf("expected error rejecting bypass in staging, got: %v", err)
	}

	// 4. APP_ENV=development must be allowed
	os.Setenv("APP_ENV", "development")
	if err := ValidatePoolSecurity(ctx, pgPool); err != nil {
		t.Fatalf("expected bypass to succeed in development, got: %v", err)
	}

	// 5. APP_ENV=test must be allowed
	os.Setenv("APP_ENV", "test")
	if err := ValidatePoolSecurity(ctx, pgPool); err != nil {
		t.Fatalf("expected bypass to succeed in test, got: %v", err)
	}
}

func TestValidatePoolSecurity_RejectsPolicyBypasses(t *testing.T) {
	ctx := context.Background()

	// Clear any bypass env vars
	os.Unsetenv("ALLOW_SUPERUSER_DB")

	pgPool, err := pgxpool.New(ctx, getPGConnString())
	if err != nil {
		t.Fatalf("failed to connect as postgres: %v", err)
	}
	defer pgPool.Close()

	appPool, err := pgxpool.New(ctx, getAppConnString())
	if err != nil {
		t.Fatalf("failed to connect as family_app: %v", err)
	}
	defer appPool.Close()

	// Initial check: baseline must pass
	if err := ValidatePoolSecurity(ctx, appPool); err != nil {
		t.Fatalf("expected baseline ValidatePoolSecurity to pass, got: %v", err)
	}

	// 1. Negative test: USING (true) bypass on tenant table
	t.Run("Rejects_USING_true", func(t *testing.T) {
		_, err := pgPool.Exec(ctx, "CREATE POLICY test_evil_true ON public.accounts FOR ALL TO family_app USING (true)")
		if err != nil {
			t.Fatalf("failed to create evil policy: %v", err)
		}
		defer pgPool.Exec(ctx, "DROP POLICY IF EXISTS test_evil_true ON public.accounts")

		err = ValidatePoolSecurity(ctx, appPool)
		if err == nil {
			t.Fatalf("SECURITY FLAW: ValidatePoolSecurity accepted USING (true) policy!")
		}
	})

	// 2. Negative test: USING (family_id = get_current_family_id() OR TRUE)
	t.Run("Rejects_USING_OR_TRUE", func(t *testing.T) {
		_, err := pgPool.Exec(ctx, "ALTER POLICY rls_accounts_all ON public.accounts USING (family_id = get_current_family_id() OR TRUE)")
		if err != nil {
			t.Fatalf("failed to alter policy: %v", err)
		}
		defer pgPool.Exec(ctx, "ALTER POLICY rls_accounts_all ON public.accounts USING (family_id = get_current_family_id())")

		err = ValidatePoolSecurity(ctx, appPool)
		if err == nil {
			t.Fatalf("SECURITY FLAW: ValidatePoolSecurity accepted USING (... OR TRUE) bypass!")
		}
	})

	// 3. Negative test: WITH CHECK (true) bypass
	t.Run("Rejects_WITH_CHECK_true", func(t *testing.T) {
		_, err := pgPool.Exec(ctx, "ALTER POLICY rls_accounts_all ON public.accounts WITH CHECK (true)")
		if err != nil {
			t.Fatalf("failed to alter policy: %v", err)
		}
		defer pgPool.Exec(ctx, "ALTER POLICY rls_accounts_all ON public.accounts WITH CHECK (family_id = get_current_family_id())")

		err = ValidatePoolSecurity(ctx, appPool)
		if err == nil {
			t.Fatalf("SECURITY FLAW: ValidatePoolSecurity accepted WITH CHECK (true) bypass!")
		}
	})

	// 4. Negative test: Policy targeting PUBLIC role
	t.Run("Rejects_Policy_Targeting_PUBLIC", func(t *testing.T) {
		_, err := pgPool.Exec(ctx, "CREATE POLICY test_evil_public ON public.accounts FOR SELECT TO PUBLIC USING (family_id = get_current_family_id())")
		if err != nil {
			t.Fatalf("failed to create policy targeting public: %v", err)
		}
		defer pgPool.Exec(ctx, "DROP POLICY IF EXISTS test_evil_public ON public.accounts")

		err = ValidatePoolSecurity(ctx, appPool)
		if err == nil {
			t.Fatalf("SECURITY FLAW: ValidatePoolSecurity accepted policy targeting PUBLIC role!")
		}
	})

	// 5. Negative test: Unexpected permissive policy (even with safe expression)
	t.Run("Rejects_Unexpected_Permissive_Policy", func(t *testing.T) {
		_, err := pgPool.Exec(ctx, "CREATE POLICY test_extra_permissive ON public.accounts FOR SELECT TO family_app USING (family_id = get_current_family_id())")
		if err != nil {
			t.Fatalf("failed to create extra permissive policy: %v", err)
		}
		defer pgPool.Exec(ctx, "DROP POLICY IF EXISTS test_extra_permissive ON public.accounts")

		err = ValidatePoolSecurity(ctx, appPool)
		if err == nil {
			t.Fatalf("SECURITY FLAW: ValidatePoolSecurity accepted unexpected permissive policy!")
		}
	})
}

func TestAll16TenantTables_TenantIsolation_LiveSQL(t *testing.T) {
	ctx := context.Background()

	// 1. Connect as superuser to seed complete 16-table fixtures for Family B
	pgPool, err := pgxpool.New(ctx, getPGConnString())
	if err != nil {
		t.Fatalf("failed to connect as postgres: %v", err)
	}
	defer pgPool.Close()

	appPool, err := pgxpool.New(ctx, getAppConnString())
	if err != nil {
		t.Fatalf("failed to connect as family_app: %v", err)
	}
	defer appPool.Close()

	familyA, familyB, userAlice, userBob := setupTestTenantFixture(t, appPool)

	// Seed comprehensive Family B data across all tables
	connB := uuid.New()
	rawB := uuid.New()
	accB := uuid.New()
	cardB := uuid.New()
	txB1 := uuid.New()
	entryB1 := uuid.New()
	txB2 := uuid.New()
	entryB2 := uuid.New()
	txB3 := uuid.New()
	entryB3 := uuid.New()
	txB4 := uuid.New()
	entryB4 := uuid.New()
	catB := uuid.New()
	ruleB := uuid.New()
	tmB := uuid.New()
	trB := uuid.New()
	siB := uuid.New()
	alB := uuid.New()
	wjB := uuid.New()
	usB := uuid.New()
	sessChainB := uuid.New()

	seedB := fmt.Sprintf(`
		-- bank_connections
		INSERT INTO public.bank_connections (id, family_id, authorized_by_user_id, provider, name, status)
		VALUES ('%s', '%s', '%s', 'tbank_sidecar', 'Connection B', 'active');

		-- accounts
		INSERT INTO public.accounts (id, family_id, bank_connection_id, owner_user_id, name, type, currency, current_balance_minor)
		VALUES ('%s', '%s', '%s', '%s', 'Checking B', 'checking', 'RUB', 100000);

		-- cards
		INSERT INTO public.cards (id, family_id, account_id, owner_user_id, masked_pan, display_name, payment_system, is_primary, status)
		VALUES ('%s', '%s', '%s', '%s', '*2222', 'Card B', 'MIR', TRUE, 'ACTIVE');

		-- raw_bank_payloads
		INSERT INTO public.raw_bank_payloads (id, family_id, bank_connection_id, source, payload_hash, raw_json)
		VALUES ('%s', '%s', '%s', 'tbank_sidecar', 'hash_b_%s', '{}'::jsonb);

		-- bank_transactions & accounting_entries (satisfying deferred split trigger)
		INSERT INTO public.bank_transactions (id, family_id, account_id, card_id, raw_payload_id, fingerprint_hash, amount_minor, currency, account_amount_minor, account_currency, transaction_time, raw_description)
		VALUES 
			('%s', '%s', '%s', '%s', '%s', 'fp_b_1_%s', 50000, 'RUB', 50000, 'RUB', NOW(), 'Tx B1'),
			('%s', '%s', '%s', '%s', '%s', 'fp_b_2_%s', -50000, 'RUB', -50000, 'RUB', NOW(), 'Tx B2'),
			('%s', '%s', '%s', '%s', '%s', 'fp_b_3_%s', 30000, 'RUB', 30000, 'RUB', NOW(), 'Tx B3'),
			('%s', '%s', '%s', '%s', '%s', 'fp_b_4_%s', -30000, 'RUB', -30000, 'RUB', NOW(), 'Tx B4');

		INSERT INTO public.accounting_entries (id, family_id, bank_transaction_id, account_id, amount_minor, currency, entry_time)
		VALUES 
			('%s', '%s', '%s', '%s', 50000, 'RUB', NOW()),
			('%s', '%s', '%s', '%s', -50000, 'RUB', NOW()),
			('%s', '%s', '%s', '%s', 30000, 'RUB', NOW()),
			('%s', '%s', '%s', '%s', -30000, 'RUB', NOW());

		-- categories
		INSERT INTO public.categories (id, family_id, name, type, icon)
		VALUES ('%s', '%s', 'Category B', 'expense', 'tag');

		-- categorization_rules
		INSERT INTO public.categorization_rules (id, family_id, created_by_user_id, category_id, priority, rule_type, match_value)
		VALUES ('%s', '%s', '%s', '%s', 1, 'merchant_name_exact', 'Supermarket B');

		-- transfer_matches
		INSERT INTO public.transfer_matches (id, family_id, source_bank_transaction_id, destination_bank_transaction_id, match_type, confidence_score, status)
		VALUES ('%s', '%s', '%s', '%s', 'exact', 100.0, 'suggested');

		-- transfer_rejections
		INSERT INTO public.transfer_rejections (id, family_id, source_bank_transaction_id, destination_bank_transaction_id, rejected_by_user_id, reason)
		VALUES ('%s', '%s', '%s', '%s', '%s', 'rejection test b');

		-- statement_imports
		INSERT INTO public.statement_imports (id, family_id, account_id, uploaded_by_user_id, file_name, file_size_bytes, file_sha256)
		VALUES ('%s', '%s', '%s', '%s', 'stmt_b.csv', 100, 'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855');

		-- audit_logs
		INSERT INTO public.audit_logs (id, family_id, entity_type, entity_id, action)
		VALUES ('%s', '%s', 'account', '%s', 'create');

		-- worker_jobs
		INSERT INTO public.worker_jobs (id, family_id, queue_name, job_type, payload)
		VALUES ('%s', '%s', 'default', 'sync', '{}'::jsonb);

		-- user_sessions
		INSERT INTO public.user_sessions (id, user_id, family_id, token_hash, session_chain_id, expires_at)
		VALUES ('%s', '%s', '%s', 'hash_us_b_%s', '%s', NOW() + INTERVAL '1 day');
	`,
		connB, familyB, userBob,
		accB, familyB, connB, userBob,
		cardB, familyB, accB, userBob,
		rawB, familyB, connB, uuid.New().String()[:8],
		txB1, familyB, accB, cardB, rawB, uuid.New().String()[:8],
		txB2, familyB, accB, cardB, rawB, uuid.New().String()[:8],
		txB3, familyB, accB, cardB, rawB, uuid.New().String()[:8],
		txB4, familyB, accB, cardB, rawB, uuid.New().String()[:8],
		entryB1, familyB, txB1, accB,
		entryB2, familyB, txB2, accB,
		entryB3, familyB, txB3, accB,
		entryB4, familyB, txB4, accB,
		catB, familyB,
		ruleB, familyB, userBob, catB,
		tmB, familyB, txB1, txB2,
		trB, familyB, txB3, txB4, userBob,
		siB, familyB, accB, userBob,
		alB, familyB, accB,
		wjB, familyB,
		usB, userBob, familyB, uuid.New().String()[:8], sessChainB,
	)

	_, err = pgPool.Exec(ctx, seedB)
	if err != nil {
		t.Fatalf("failed to seed Family B test data: %v", err)
	}

	tenantTables := []string{
		"families", "family_members", "bank_connections", "raw_bank_payloads",
		"accounts", "cards", "bank_transactions", "accounting_entries",
		"categories", "categorization_rules", "transfer_matches",
		"transfer_rejections", "statement_imports", "audit_logs",
		"worker_jobs", "user_sessions",
	}

	// A. TEST: No tenant context set -> 0 rows visible across all 16 tables
	for _, tbl := range tenantTables {
		var cnt int
		err := appPool.QueryRow(ctx, "SELECT count(*) FROM public."+tbl).Scan(&cnt)
		if err != nil {
			t.Fatalf("table %s query error without tenant context: %v", tbl, err)
		}
		if cnt != 0 {
			t.Fatalf("RLS isolation failure: table %s returned %d rows without tenant context", tbl, cnt)
		}
	}

	// B. TEST: Family A context cannot see Family B data across all 16 tables
	err = WithFamilyTx(ctx, appPool, familyA, func(tx pgx.Tx) error {
		for _, tbl := range tenantTables {
			familyCol := "family_id"
			if tbl == "families" {
				familyCol = "id"
			}
			var cnt int
			err := tx.QueryRow(ctx, fmt.Sprintf("SELECT count(*) FROM public.%s WHERE %s = $1", tbl, familyCol), familyB).Scan(&cnt)
			if err != nil {
				return fmt.Errorf("table %s query error: %w", tbl, err)
			}
			if cnt != 0 {
				return fmt.Errorf("RLS leakage: Family A was able to read %d rows from Family B in %s", cnt, tbl)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Tenant isolation check failed: %v", err)
	}

	// C. TEST: Family A context cannot UPDATE or DELETE Family B data
	err = WithFamilyTx(ctx, appPool, familyA, func(tx pgx.Tx) error {
		// Attempt to update Family B's account
		cmd, err := tx.Exec(ctx, "UPDATE public.accounts SET name = 'Hacked by A' WHERE family_id = $1", familyB)
		if err != nil {
			return err
		}
		if cmd.RowsAffected() != 0 {
			return fmt.Errorf("RLS violation: Family A updated %d rows of Family B accounts", cmd.RowsAffected())
		}

		// Attempt to update Family B's cards
		cmd, err = tx.Exec(ctx, "UPDATE public.cards SET display_name = 'Hacked Card' WHERE family_id = $1", familyB)
		if err != nil {
			return err
		}
		if cmd.RowsAffected() != 0 {
			return fmt.Errorf("RLS violation: Family A updated %d rows of Family B cards", cmd.RowsAffected())
		}

		return nil
	})
	if err != nil {
		t.Fatalf("Cross-tenant modification check failed: %v", err)
	}

	// D. TEST: Inserting a row with Family B's family_id while in Family A's context is rejected by RLS WITH CHECK
	err = WithFamilyTx(ctx, appPool, familyA, func(tx pgx.Tx) error {
		sp, err := tx.Begin(ctx)
		if err != nil {
			return err
		}
		defer sp.Rollback(ctx)

		_, err = sp.Exec(ctx, `
			INSERT INTO public.accounts (id, family_id, owner_user_id, name, type, currency)
			VALUES ($1, $2, $3, 'Illegal Account', 'checking', 'RUB')
		`, uuid.New(), familyB, userAlice)
		if err == nil {
			return errors.New("expected RLS WITH CHECK violation when inserting with foreign family_id, but it succeeded")
		}
		if !strings.Contains(err.Error(), "violates row-level security policy") {
			return fmt.Errorf("expected RLS policy violation error, got: %w", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("RLS WITH CHECK rejection test failed: %v", err)
	}

	// E. TEST: Worker and security-definer privilege boundaries
	// family_worker cannot query users.password_hash
	workerPool, err := pgxpool.New(ctx, fmt.Sprintf("postgres://family_worker:worker_secret_placeholder@%s:%s/%s?sslmode=disable",
		os.Getenv("PGHOST"), os.Getenv("PGPORT"), "family_finance_migration_test"))
	if err == nil {
		defer workerPool.Close()
		_, err = workerPool.Exec(ctx, "SELECT password_hash FROM public.users")
		if err == nil {
			t.Fatalf("security violation: family_worker was able to SELECT password_hash from users!")
		}
	}
}

func TestValidateWorkerPoolSecurity_ComplianceAndIsolation(t *testing.T) {
	ctx := context.Background()

	workerPool, err := NewWorkerPool(ctx, getWorkerConnString())
	if err != nil {
		t.Fatalf("NewWorkerPool failed for family_worker: %v", err)
	}
	defer workerPool.Close()

	appPool, err := NewPool(ctx, getAppConnString())
	if err != nil {
		t.Fatalf("NewPool failed for family_app: %v", err)
	}
	defer appPool.Close()

	// 1. Worker pool passes ValidateWorkerPoolSecurity
	if err := ValidateWorkerPoolSecurity(ctx, workerPool); err != nil {
		t.Errorf("expected ValidateWorkerPoolSecurity to pass for family_worker, got: %v", err)
	}

	// 2. App pool fails ValidateWorkerPoolSecurity
	if err := ValidateWorkerPoolSecurity(ctx, appPool); err == nil {
		t.Errorf("expected ValidateWorkerPoolSecurity to reject family_app role, but it passed")
	}

	// 3. Worker pool fails ValidatePoolSecurity (which requires non-worker application role)
	if err := ValidatePoolSecurity(ctx, workerPool); err == nil {
		t.Errorf("expected ValidatePoolSecurity to reject family_worker role, but it passed")
	}

	// 4. family_worker can execute claim_next_worker_job
	var jobID *uuid.UUID
	err = workerPool.QueryRow(ctx, "SELECT job_id FROM queue_sec.claim_next_worker_job('test_queue', 'test_worker', 60)").Scan(&jobID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) && !strings.Contains(err.Error(), "no rows") {
		t.Errorf("worker failed to call claim_next_worker_job: %v", err)
	}

	// 5. family_app CANNOT execute claim_next_worker_job
	err = appPool.QueryRow(ctx, "SELECT job_id FROM queue_sec.claim_next_worker_job('test_queue', 'test_worker', 60)").Scan(&jobID)
	if err == nil {
		t.Errorf("SECURITY VIOLATION: family_app was able to execute queue_sec.claim_next_worker_job!")
	}

	// 6. Tenant isolation for family_worker: family_worker cannot access another family's data
	pgPool, err := pgxpool.New(ctx, getPGConnString())
	if err != nil {
		t.Fatalf("failed to connect as postgres: %v", err)
	}
	defer pgPool.Close()

	familyA, familyB, _, _ := setupTestTenantFixture(t, pgPool)

	// In context of familyA, querying familyB returns 0 rows
	err = WithFamilyTx(ctx, workerPool, familyA, func(tx pgx.Tx) error {
		var cnt int
		err := tx.QueryRow(ctx, "SELECT count(*) FROM public.accounts WHERE family_id = $1", familyB).Scan(&cnt)
		if err != nil {
			return err
		}
		if cnt != 0 {
			t.Errorf("tenant isolation violation: family_worker in familyA saw %d accounts of familyB", cnt)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("worker WithFamilyTx failed: %v", err)
	}
}

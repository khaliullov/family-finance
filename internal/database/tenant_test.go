package database

import (
	"context"
	"errors"
	"fmt"
	"os"
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

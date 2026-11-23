package database

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func getTestDatabaseURL() string {
	host := os.Getenv("PGHOST")
	if host == "" {
		host = "localhost"
	}
	port := os.Getenv("PGPORT")
	if port == "" {
		port = "5432"
	}
	user := os.Getenv("PGUSER")
	if user == "" {
		user = "postgres"
	}
	password := os.Getenv("PGPASSWORD")
	if password == "" {
		password = "postgres"
	}
	dbname := os.Getenv("PGDATABASE")
	if dbname == "" {
		dbname = "family_finance_migration_test"
	}
	return "pgx5://" + user + ":" + password + "@" + host + ":" + port + "/" + dbname + "?sslmode=disable"
}

func getPGConnString() string {
	host := os.Getenv("PGHOST")
	if host == "" {
		host = "localhost"
	}
	port := os.Getenv("PGPORT")
	if port == "" {
		port = "5432"
	}
	user := os.Getenv("PGUSER")
	if user == "" {
		user = "postgres"
	}
	password := os.Getenv("PGPASSWORD")
	if password == "" {
		password = "postgres"
	}
	dbname := os.Getenv("PGDATABASE")
	if dbname == "" {
		dbname = "family_finance_migration_test"
	}
	return "postgres://" + user + ":" + password + "@" + host + ":" + port + "/" + dbname + "?sslmode=disable"
}

func TestMigrations_Up_Down_Up(t *testing.T) {
	ctx := context.Background()
	dbURL := getTestDatabaseURL()

	migrationsDir, err := filepath.Abs("../../migrations")
	if err != nil {
		t.Fatalf("failed to get migrations directory: %v", err)
	}

	m, err := migrate.New("file://"+migrationsDir, dbURL)
	if err != nil {
		t.Fatalf("migrate.New failed: %v", err)
	}
	defer m.Close()

	// 1. Initial Up to latest version
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate.Up (1st) failed: %v", err)
	}

	version, dirty, err := m.Version()
	if err != nil {
		t.Fatalf("m.Version() failed: %v", err)
	}
	if version != 4 || dirty {
		t.Fatalf("expected version 4 (clean), got version=%d dirty=%v", version, dirty)
	}

	pool, err := pgxpool.New(ctx, getPGConnString())
	if err != nil {
		t.Fatalf("failed to connect to test db: %v", err)
	}
	defer pool.Close()

	// Verify all 17 canonical tables exist after Up
	expectedTables := []string{
		"users", "families", "family_members", "bank_connections", "raw_bank_payloads",
		"accounts", "cards", "bank_transactions", "transfer_matches", "transfer_rejections",
		"categories", "accounting_entries", "categorization_rules", "statement_imports",
		"audit_logs", "worker_jobs", "user_sessions",
	}
	for _, tbl := range expectedTables {
		var count int
		err = pool.QueryRow(ctx, "SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name = $1", tbl).Scan(&count)
		if err != nil || count != 1 {
			t.Fatalf("expected table %s to exist after 1st Up, got count=%d, err=%v", tbl, count, err)
		}
	}

	// Verify migration 000004 constraint and index exist
	var chkCount int
	err = pool.QueryRow(ctx, "SELECT count(*) FROM pg_constraint WHERE conname = 'chk_cards_closed_not_primary'").Scan(&chkCount)
	if err != nil || chkCount != 1 {
		t.Fatalf("expected constraint chk_cards_closed_not_primary to exist, got count=%d, err=%v", chkCount, err)
	}
	var idxCount int
	err = pool.QueryRow(ctx, "SELECT count(*) FROM pg_indexes WHERE indexname = 'uq_cards_one_primary_per_account'").Scan(&idxCount)
	if err != nil || idxCount != 1 {
		t.Fatalf("expected index uq_cards_one_primary_per_account to exist, got count=%d, err=%v", idxCount, err)
	}

	// 2. Test seeding categories for a newly bootstrapped family
	famID := uuid.New()
	userID := uuid.New()
	err = WithFamilyUserTx(ctx, pool, famID, userID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "INSERT INTO public.users (id, email, password_hash, first_name, last_name) VALUES ($1, $2, 'hash', 'Test', 'User')",
			userID, famID.String()+"@test.local")
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, "INSERT INTO public.families (id, name) VALUES ($1, 'Migrated Family')", famID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, "INSERT INTO public.family_members (id, family_id, user_id, role) VALUES ($1, $2, $3, 'owner')",
			uuid.New(), famID, userID)
		return err
	})
	if err != nil {
		t.Fatalf("failed to insert test family: %v", err)
	}

	// Re-run migration 2 (or execute migration 2's DO block) to seed categories for this family
	mig2Content, err := os.ReadFile(filepath.Join(migrationsDir, "000002_seed_categories.up.sql"))
	if err != nil {
		t.Fatalf("failed to read migration 2 up: %v", err)
	}
	_, err = pool.Exec(ctx, string(mig2Content))
	if err != nil {
		t.Fatalf("failed to apply migration 2 up block: %v", err)
	}

	// Verify categories seeded for this family
	var catCount int
	err = pool.QueryRow(ctx, "SELECT count(*) FROM public.categories WHERE family_id = $1 AND is_system = TRUE", famID).Scan(&catCount)
	if err != nil || catCount != 12 {
		t.Fatalf("expected 12 seeded system categories for family, got count=%d, err=%v", catCount, err)
	}

	// 3. Down migration
	if err := m.Down(); err != nil {
		t.Fatalf("migrate.Down failed: %v", err)
	}

	// Verify tables dropped after Down
	var tableCount int
	err = pool.QueryRow(ctx, "SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'user_sessions'").Scan(&tableCount)
	if err != nil || tableCount != 0 {
		t.Fatalf("expected user_sessions table to be dropped after Down, got count=%d, err=%v", tableCount, err)
	}

	// 4. Second Up migration to ensure idempotency and re-apply cleanly
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate.Up (2nd) failed: %v", err)
	}

	version, dirty, err = m.Version()
	if err != nil || version != 4 || dirty {
		t.Fatalf("expected version 4 clean after 2nd Up, got version=%d dirty=%v, err=%v", version, dirty, err)
	}

	err = pool.QueryRow(ctx, "SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'user_sessions'").Scan(&tableCount)
	if err != nil || tableCount != 1 {
		t.Fatalf("expected user_sessions table to exist after 2nd Up, got count=%d, err=%v", tableCount, err)
	}
}

func TestMigrations_UpgradeFromV3ToV4(t *testing.T) {
	ctx := context.Background()
	dbURL := getTestDatabaseURL()

	migrationsDir, err := filepath.Abs("../../migrations")
	if err != nil {
		t.Fatalf("failed to get migrations directory: %v", err)
	}

	m, err := migrate.New("file://"+migrationsDir, dbURL)
	if err != nil {
		t.Fatalf("migrate.New failed: %v", err)
	}
	defer m.Close()

	// 1. Force/migrate database to version 3 (pre-existing state)
	if err := m.Migrate(3); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to version 3 failed: %v", err)
	}

	version, dirty, err := m.Version()
	if err != nil || version != 3 || dirty {
		t.Fatalf("expected version 3 clean, got version=%d dirty=%v, err=%v", version, dirty, err)
	}

	pool, err := pgxpool.New(ctx, getPGConnString())
	if err != nil {
		t.Fatalf("failed to connect to test db: %v", err)
	}
	defer pool.Close()

	// Verify chk_cards_closed_not_primary does NOT exist at version 3
	var chkCount int
	err = pool.QueryRow(ctx, "SELECT count(*) FROM pg_constraint WHERE conname = 'chk_cards_closed_not_primary'").Scan(&chkCount)
	if err != nil {
		t.Fatalf("failed to query pg_constraint: %v", err)
	}
	if chkCount != 0 {
		t.Fatalf("expected 0 chk_cards_closed_not_primary constraints at version 3, found %d", chkCount)
	}

	// 2. Seed legacy v3 test fixtures containing historical invariant violations:
	//    - Card A: status = 'CLOSED' AND is_primary = TRUE
	//    - Card B: status = 'ACTIVE' AND is_primary = TRUE (older update)
	//    - Card C: status = 'ACTIVE' AND is_primary = TRUE (newer update)
	//    - Card D: status = 'ACTIVE' AND is_primary = FALSE (valid secondary card)
	//    - Plus unrelated account with 1 primary card to verify unrelated data is unchanged
	famID := uuid.New()
	userID := uuid.New()
	accID := uuid.New()

	closedPrimaryCardID := uuid.New()
	olderPrimaryCardID := uuid.New()
	newerPrimaryCardID := uuid.New()
	secondaryCardID := uuid.New()

	unrelatedFamID := uuid.New()
	unrelatedUserID := uuid.New()
	unrelatedAccID := uuid.New()
	unrelatedCardID := uuid.New()

	// Insert legacy fixtures using superuser pool
	seedLegacySQL := `
		-- Primary test family & account
		INSERT INTO public.users (id, email, password_hash, first_name, last_name)
		VALUES ('%s', 'legacy_user_%s@test.local', 'hash', 'Legacy', 'User');

		INSERT INTO public.families (id, name)
		VALUES ('%s', 'Legacy Family');

		INSERT INTO public.family_members (id, family_id, user_id, role)
		VALUES ('%s', '%s', '%s', 'owner');

		INSERT INTO public.accounts (id, family_id, owner_user_id, name, type, currency)
		VALUES ('%s', '%s', '%s', 'Legacy Checking', 'checking', 'RUB');

		-- 1. Legacy violation: CLOSED card with is_primary = TRUE
		INSERT INTO public.cards (
			id, family_id, account_id, owner_user_id, masked_pan, display_name,
			payment_system, is_primary, status, created_at, updated_at
		) VALUES (
			'%s', '%s', '%s', '%s', '*1111', 'Legacy Closed Primary',
			'MIR', TRUE, 'CLOSED', NOW() - INTERVAL '5 days', NOW() - INTERVAL '4 days'
		);

		-- 2. Legacy violation: Duplicate primary card (older)
		INSERT INTO public.cards (
			id, family_id, account_id, owner_user_id, masked_pan, display_name,
			payment_system, is_primary, status, created_at, updated_at
		) VALUES (
			'%s', '%s', '%s', '%s', '*2222', 'Legacy Active Primary Older',
			'MIR', TRUE, 'ACTIVE', NOW() - INTERVAL '3 days', NOW() - INTERVAL '2 days'
		);

		-- 3. Legacy violation: Duplicate primary card (newer - should win deterministic retention)
		INSERT INTO public.cards (
			id, family_id, account_id, owner_user_id, masked_pan, display_name,
			payment_system, is_primary, status, created_at, updated_at
		) VALUES (
			'%s', '%s', '%s', '%s', '*3333', 'Legacy Active Primary Newer',
			'MIR', TRUE, 'ACTIVE', NOW() - INTERVAL '3 days', NOW() - INTERVAL '1 hour'
		);

		-- 4. Valid secondary card
		INSERT INTO public.cards (
			id, family_id, account_id, owner_user_id, masked_pan, display_name,
			payment_system, is_primary, status, created_at, updated_at
		) VALUES (
			'%s', '%s', '%s', '%s', '*4444', 'Valid Secondary Card',
			'MIR', FALSE, 'ACTIVE', NOW() - INTERVAL '1 day', NOW() - INTERVAL '1 day'
		);

		-- Unrelated family, account, and primary card
		INSERT INTO public.users (id, email, password_hash, first_name, last_name)
		VALUES ('%s', 'unrelated_%s@test.local', 'hash', 'Unrelated', 'User');

		INSERT INTO public.families (id, name)
		VALUES ('%s', 'Unrelated Family');

		INSERT INTO public.family_members (id, family_id, user_id, role)
		VALUES ('%s', '%s', '%s', 'owner');

		INSERT INTO public.accounts (id, family_id, owner_user_id, name, type, currency)
		VALUES ('%s', '%s', '%s', 'Unrelated Account', 'checking', 'RUB');

		INSERT INTO public.cards (
			id, family_id, account_id, owner_user_id, masked_pan, display_name,
			payment_system, is_primary, status, created_at, updated_at
		) VALUES (
			'%s', '%s', '%s', '%s', '*7777', 'Unrelated Primary Card',
			'MIR', TRUE, 'ACTIVE', NOW() - INTERVAL '1 day', NOW() - INTERVAL '1 day'
		);
	`
	formattedSeed := fmt.Sprintf(seedLegacySQL,
		userID, userID.String()[:8],
		famID,
		uuid.New(), famID, userID,
		accID, famID, userID,
		closedPrimaryCardID, famID, accID, userID,
		olderPrimaryCardID, famID, accID, userID,
		newerPrimaryCardID, famID, accID, userID,
		secondaryCardID, famID, accID, userID,
		unrelatedUserID, unrelatedUserID.String()[:8],
		unrelatedFamID,
		uuid.New(), unrelatedFamID, unrelatedUserID,
		unrelatedAccID, unrelatedFamID, unrelatedUserID,
		unrelatedCardID, unrelatedFamID, unrelatedAccID, unrelatedUserID,
	)
	_, err = pool.Exec(ctx, formattedSeed)
	if err != nil {
		t.Fatalf("failed to seed legacy v3 fixtures: %v", err)
	}

	// 3. Apply Up migration to upgrade from v3 to latest (v4) with Option A deterministic repair
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("upgrade from v3 to v4 failed: %v", err)
	}

	version, dirty, err = m.Version()
	if err != nil || version != 4 || dirty {
		t.Fatalf("expected version 4 clean after upgrade, got version=%d dirty=%v, err=%v", version, dirty, err)
	}

	// 4. Verify card constraint and index exist at version 4
	err = pool.QueryRow(ctx, "SELECT count(*) FROM pg_constraint WHERE conname = 'chk_cards_closed_not_primary'").Scan(&chkCount)
	if err != nil || chkCount != 1 {
		t.Fatalf("expected constraint chk_cards_closed_not_primary to exist after upgrade to v4, got count=%d, err=%v", chkCount, err)
	}

	var idxCount int
	err = pool.QueryRow(ctx, "SELECT count(*) FROM pg_indexes WHERE indexname = 'uq_cards_one_primary_per_account'").Scan(&idxCount)
	if err != nil || idxCount != 1 {
		t.Fatalf("expected index uq_cards_one_primary_per_account to exist after upgrade to v4, got count=%d, err=%v", idxCount, err)
	}

	// 5. Verify deterministic repair results:
	// 5a. Closed primary card -> is_primary cleared to false, status remains CLOSED
	var card1Primary bool
	var card1Status, card1Name string
	var card1Owner, card1Acc uuid.UUID
	err = pool.QueryRow(ctx, `
		SELECT is_primary, status, display_name, owner_user_id, account_id 
		FROM public.cards WHERE id = $1
	`, closedPrimaryCardID).Scan(&card1Primary, &card1Status, &card1Name, &card1Owner, &card1Acc)
	if err != nil {
		t.Fatalf("query card 1 failed: %v", err)
	}
	if card1Primary {
		t.Errorf("expected closed card 1 is_primary to be false after migration, got true")
	}
	if card1Status != "CLOSED" {
		t.Errorf("expected closed card 1 status to remain CLOSED, got %s", card1Status)
	}
	if card1Owner != userID || card1Acc != accID || card1Name != "Legacy Closed Primary" {
		t.Errorf("card 1 non-primary attributes unexpectedly altered")
	}

	// 5b. Older active primary card -> is_primary cleared to false
	var card2Primary bool
	var card2Status, card2Name string
	err = pool.QueryRow(ctx, `
		SELECT is_primary, status, display_name 
		FROM public.cards WHERE id = $1
	`, olderPrimaryCardID).Scan(&card2Primary, &card2Status, &card2Name)
	if err != nil {
		t.Fatalf("query card 2 failed: %v", err)
	}
	if card2Primary {
		t.Errorf("expected older primary card 2 is_primary to be cleared to false, got true")
	}
	if card2Status != "ACTIVE" || card2Name != "Legacy Active Primary Older" {
		t.Errorf("card 2 non-primary attributes unexpectedly altered")
	}

	// 5c. Newer active primary card -> retained as the single primary card
	var card3Primary bool
	var card3Status, card3Name string
	err = pool.QueryRow(ctx, `
		SELECT is_primary, status, display_name 
		FROM public.cards WHERE id = $1
	`, newerPrimaryCardID).Scan(&card3Primary, &card3Status, &card3Name)
	if err != nil {
		t.Fatalf("query card 3 failed: %v", err)
	}
	if !card3Primary {
		t.Errorf("expected newer primary card 3 to retain is_primary = true, got false")
	}
	if card3Status != "ACTIVE" || card3Name != "Legacy Active Primary Newer" {
		t.Errorf("card 3 non-primary attributes unexpectedly altered")
	}

	// 5d. Valid secondary card -> remains is_primary = false
	var card4Primary bool
	err = pool.QueryRow(ctx, "SELECT is_primary FROM public.cards WHERE id = $1", secondaryCardID).Scan(&card4Primary)
	if err != nil {
		t.Fatalf("query card 4 failed: %v", err)
	}
	if card4Primary {
		t.Errorf("expected secondary card 4 to remain false, got true")
	}

	// 5e. Unrelated account card -> completely unchanged (is_primary = true)
	var unrelatedPrimary bool
	var unrelatedName string
	err = pool.QueryRow(ctx, "SELECT is_primary, display_name FROM public.cards WHERE id = $1", unrelatedCardID).Scan(&unrelatedPrimary, &unrelatedName)
	if err != nil {
		t.Fatalf("query unrelated card failed: %v", err)
	}
	if !unrelatedPrimary || unrelatedName != "Unrelated Primary Card" {
		t.Errorf("unrelated card unexpectedly modified: primary=%v name=%s", unrelatedPrimary, unrelatedName)
	}

	// 6. Invariant verification across the target account:
	//    - Exactly 1 primary card
	//    - 0 closed primary cards
	var primaryCount, closedPrimaryCount int
	err = pool.QueryRow(ctx, "SELECT count(*) FROM public.cards WHERE account_id = $1 AND is_primary = TRUE", accID).Scan(&primaryCount)
	if err != nil || primaryCount != 1 {
		t.Fatalf("expected exactly 1 primary card for account, found %d", primaryCount)
	}
	err = pool.QueryRow(ctx, "SELECT count(*) FROM public.cards WHERE account_id = $1 AND status = 'CLOSED' AND is_primary = TRUE", accID).Scan(&closedPrimaryCount)
	if err != nil || closedPrimaryCount != 0 {
		t.Fatalf("expected 0 closed primary cards for account, found %d", closedPrimaryCount)
	}

	// 7. Test that new violations are strictly blocked by installed constraints
	// Check constraint chk_cards_closed_not_primary blocks closed primary insertion
	_, err = pool.Exec(ctx, `
		INSERT INTO public.cards (
			id, family_id, account_id, owner_user_id, masked_pan, display_name,
			payment_system, is_primary, status
		) VALUES (
			$1, $2, $3, $4, '*9999', 'Constraint Violation Attempt',
			'MIR', TRUE, 'CLOSED'
		)
	`, uuid.New(), famID, accID, userID)
	if err == nil {
		t.Fatalf("expected chk_cards_closed_not_primary check constraint violation, but insert succeeded")
	}

	// Unique index uq_cards_one_primary_per_account blocks duplicate primary insertion
	_, err = pool.Exec(ctx, `
		INSERT INTO public.cards (
			id, family_id, account_id, owner_user_id, masked_pan, display_name,
			payment_system, is_primary, status
		) VALUES (
			$1, $2, $3, $4, '*8888', 'Duplicate Primary Attempt',
			'MIR', TRUE, 'ACTIVE'
		)
	`, uuid.New(), famID, accID, userID)
	if err == nil {
		t.Fatalf("expected uq_cards_one_primary_per_account unique index violation, but insert succeeded")
	}
}

func TestMigrations_FailsWhenRequiredRolesMissing(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, getPGConnString())
	if err != nil {
		t.Fatalf("failed to connect to postgres: %v", err)
	}
	defer pool.Close()

	// Run role verification block with a missing dummy role
	missingRoleSQL := `
	DO $$
	BEGIN
		IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'nonexistent_test_role') THEN
			RAISE EXCEPTION 'Required cluster role "nonexistent_test_role" does not exist. Run scripts/provision_roles.sql as cluster admin before applying migrations.';
		END IF;
	END $$;
	`
	_, err = pool.Exec(ctx, missingRoleSQL)
	if err == nil {
		t.Fatalf("expected error when required role is missing, but got nil")
	}
	if !strings.Contains(err.Error(), "does not exist") || !strings.Contains(err.Error(), "scripts/provision_roles.sql") {
		t.Errorf("expected actionable error message pointing to scripts/provision_roles.sql, got %v", err)
	}
}

func TestProvisionRoles_NeverOverwritesExistingPassword(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, getPGConnString())
	if err != nil {
		t.Fatalf("failed to connect as superuser: %v", err)
	}
	defer pool.Close()

	// 1. Set a custom rotated password on family_worker
	const customSecret = "custom_production_rotated_secret_999"
	_, err = pool.Exec(ctx, fmt.Sprintf("ALTER ROLE family_worker WITH PASSWORD '%s'", customSecret))
	if err != nil {
		t.Fatalf("failed to set custom password: %v", err)
	}
	defer func() {
		// Reset back to dev placeholder after test
		_, _ = pool.Exec(ctx, "ALTER ROLE family_worker WITH PASSWORD 'worker_secret_placeholder'")
	}()

	// 2. Read and execute scripts/provision_roles.sql
	provScriptPath, err := filepath.Abs("../../scripts/provision_roles.sql")
	if err != nil {
		t.Fatalf("failed to resolve provision_roles.sql path: %v", err)
	}
	content, err := os.ReadFile(provScriptPath)
	if err != nil {
		t.Fatalf("failed to read provision_roles.sql: %v", err)
	}

	_, err = pool.Exec(ctx, string(content))
	if err != nil {
		t.Fatalf("re-running provision_roles.sql failed: %v", err)
	}

	// 3. Connect as family_worker using the custom password.
	// If provision_roles.sql overwrote the password with 'worker_secret_placeholder',
	// this connection will fail!
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

	workerConnStr := fmt.Sprintf("postgres://family_worker:%s@%s:%s/%s?sslmode=disable",
		customSecret, host, port, dbname)

	workerPool, err := pgxpool.New(ctx, workerConnStr)
	if err != nil {
		t.Fatalf("failed to connect with custom password after provision_roles rerun: %v (password was overwritten!)", err)
	}
	defer workerPool.Close()

	var currentUser string
	err = workerPool.QueryRow(ctx, "SELECT CURRENT_USER").Scan(&currentUser)
	if err != nil || currentUser != "family_worker" {
		t.Fatalf("expected current user family_worker, got %q, err=%v", currentUser, err)
	}
}

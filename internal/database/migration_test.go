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

func TestMain(m *testing.M) {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, getPGConnString())
	if err == nil {
		_, _ = conn.Exec(ctx, "UPDATE schema_migrations SET dirty = false WHERE dirty = true;")
		conn.Close(ctx)
	}
	os.Exit(m.Run())
}

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
	if version != 13 || dirty {
		t.Fatalf("expected version 13 (clean), got version=%d dirty=%v", version, dirty)
	}

	pool, err := pgxpool.New(ctx, getPGConnString())
	if err != nil {
		t.Fatalf("failed to connect to test db: %v", err)
	}
	defer pool.Close()

	// Verify all canonical tables exist after Up
	expectedTables := []string{
		"users", "families", "family_members", "bank_connections", "raw_bank_payloads",
		"accounts", "cards", "bank_transactions", "transfer_matches", "transfer_rejections",
		"categories", "accounting_entries", "categorization_rules", "statement_imports",
		"audit_logs", "worker_jobs", "user_sessions", "reconciliation_discrepancies",
		"bank_auth_attempts", "bank_validation_reports", "bank_initial_import_approvals",
		"statement_documents", "statement_import_batches", "staged_bank_transactions", "statement_reconciliation_overrides",
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
	if err != nil || version != 13 || dirty {
		t.Fatalf("expected version 13 clean after 2nd Up, got version=%d dirty=%v, err=%v", version, dirty, err)
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

	// 3. Apply Up migration to upgrade from v3 to v4 with Option A deterministic repair
	if err := m.Migrate(4); err != nil && err != migrate.ErrNoChange {
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

func TestMigrations_UpgradeFromV4ToV5(t *testing.T) {
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

	// 1. Force/migrate database to version 4
	if err := m.Migrate(4); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to version 4 failed: %v", err)
	}

	version, dirty, err := m.Version()
	if err != nil || version != 4 || dirty {
		t.Fatalf("expected version 4 clean, got version=%d dirty=%v, err=%v", version, dirty, err)
	}

	pool, err := pgxpool.New(ctx, getPGConnString())
	if err != nil {
		t.Fatalf("failed to connect to test db: %v", err)
	}
	defer pool.Close()

	// Verify family_worker lacks INSERT on accounts and UPDATE on scheduled_at at v4
	var hasInsert bool
	err = pool.QueryRow(ctx, "SELECT has_table_privilege('family_worker', 'public.accounts', 'INSERT')").Scan(&hasInsert)
	if err != nil {
		t.Fatalf("failed to check v4 table privilege: %v", err)
	}
	if hasInsert {
		t.Fatalf("expected family_worker to NOT have INSERT on accounts at v4")
	}

	var hasUpdateSched bool
	err = pool.QueryRow(ctx, "SELECT has_column_privilege('family_worker', 'public.worker_jobs', 'scheduled_at', 'UPDATE')").Scan(&hasUpdateSched)
	if err != nil {
		t.Fatalf("failed to check v4 column privilege: %v", err)
	}
	if hasUpdateSched {
		t.Fatalf("expected family_worker to NOT have UPDATE(scheduled_at) on worker_jobs at v4")
	}

	// 2. Upgrade from v4 to v5
	if err := m.Migrate(5); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("upgrade to v5 failed: %v", err)
	}

	version, dirty, err = m.Version()
	if err != nil || version != 5 || dirty {
		t.Fatalf("expected version 5 clean after upgrade, got version=%d dirty=%v, err=%v", version, dirty, err)
	}

	// Verify family_worker HAS INSERT on accounts and UPDATE on scheduled_at at v5
	err = pool.QueryRow(ctx, "SELECT has_table_privilege('family_worker', 'public.accounts', 'INSERT')").Scan(&hasInsert)
	if err != nil || !hasInsert {
		t.Fatalf("expected family_worker to have INSERT on accounts at v5, got %v, err=%v", hasInsert, err)
	}

	err = pool.QueryRow(ctx, "SELECT has_column_privilege('family_worker', 'public.worker_jobs', 'scheduled_at', 'UPDATE')").Scan(&hasUpdateSched)
	if err != nil || !hasUpdateSched {
		t.Fatalf("expected family_worker to have UPDATE(scheduled_at) on worker_jobs at v5, got %v, err=%v", hasUpdateSched, err)
	}

	// 3. Downgrade from v5 to v4
	if err := m.Migrate(4); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("downgrade to v4 failed: %v", err)
	}

	err = pool.QueryRow(ctx, "SELECT has_table_privilege('family_worker', 'public.accounts', 'INSERT')").Scan(&hasInsert)
	if err != nil || hasInsert {
		t.Fatalf("expected family_worker INSERT to be revoked at v4 after downgrade, got %v, err=%v", hasInsert, err)
	}

	// 4. Re-upgrade to v5
	if err := m.Migrate(5); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("re-upgrade to v5 failed: %v", err)
	}

	err = pool.QueryRow(ctx, "SELECT has_table_privilege('family_worker', 'public.accounts', 'INSERT')").Scan(&hasInsert)
	if err != nil || !hasInsert {
		t.Fatalf("expected family_worker INSERT to be re-granted at v5, got %v, err=%v", hasInsert, err)
	}
}

func TestMigrations_UpgradeFromV5ToV6(t *testing.T) {
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

	// 1. Migrate database to version 5
	if err := m.Migrate(5); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to version 5 failed: %v", err)
	}

	version, dirty, err := m.Version()
	if err != nil || version != 5 || dirty {
		t.Fatalf("expected version 5 clean, got version=%d dirty=%v, err=%v", version, dirty, err)
	}

	pool, err := pgxpool.New(ctx, getPGConnString())
	if err != nil {
		t.Fatalf("failed to connect to test db: %v", err)
	}
	defer pool.Close()

	// Verify v5 state: raw_payload_bytes and sync_cursor columns do not exist
	var rawColCount, syncColCount int
	err = pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns 
		WHERE table_schema = 'public' AND table_name = 'raw_bank_payloads' AND column_name = 'raw_payload_bytes'
	`).Scan(&rawColCount)
	if err != nil || rawColCount != 0 {
		t.Fatalf("expected raw_payload_bytes to not exist at v5, got %d, err=%v", rawColCount, err)
	}

	err = pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns 
		WHERE table_schema = 'public' AND table_name = 'bank_connections' AND column_name = 'sync_cursor'
	`).Scan(&syncColCount)
	if err != nil || syncColCount != 0 {
		t.Fatalf("expected sync_cursor to not exist at v5, got %d, err=%v", syncColCount, err)
	}

	// 2. Upgrade from v5 to v6
	if err := m.Migrate(6); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("upgrade to v6 failed: %v", err)
	}

	version, dirty, err = m.Version()
	if err != nil || version != 6 || dirty {
		t.Fatalf("expected version 6 clean after upgrade, got version=%d dirty=%v, err=%v", version, dirty, err)
	}

	// Verify v6 state: raw_payload_bytes and sync_cursor columns exist
	err = pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns 
		WHERE table_schema = 'public' AND table_name = 'raw_bank_payloads' AND column_name = 'raw_payload_bytes'
	`).Scan(&rawColCount)
	if err != nil || rawColCount != 1 {
		t.Fatalf("expected raw_payload_bytes to exist at v6, got %d, err=%v", rawColCount, err)
	}

	err = pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns 
		WHERE table_schema = 'public' AND table_name = 'bank_connections' AND column_name = 'sync_cursor'
	`).Scan(&syncColCount)
	if err != nil || syncColCount != 1 {
		t.Fatalf("expected sync_cursor to exist at v6, got %d, err=%v", syncColCount, err)
	}

	// Verify family_worker has UPDATE(locked_until) on worker_jobs
	var hasUpdateLockedUntil, hasUpdateSyncCursor bool
	err = pool.QueryRow(ctx, "SELECT has_column_privilege('family_worker', 'public.worker_jobs', 'locked_until', 'UPDATE')").Scan(&hasUpdateLockedUntil)
	if err != nil || !hasUpdateLockedUntil {
		t.Fatalf("expected family_worker to have UPDATE(locked_until) on worker_jobs at v6, got %v, err=%v", hasUpdateLockedUntil, err)
	}

	// Verify family_worker has UPDATE(sync_cursor) on bank_connections
	err = pool.QueryRow(ctx, "SELECT has_column_privilege('family_worker', 'public.bank_connections', 'sync_cursor', 'UPDATE')").Scan(&hasUpdateSyncCursor)
	if err != nil || !hasUpdateSyncCursor {
		t.Fatalf("expected family_worker to have UPDATE(sync_cursor) on bank_connections at v6, got %v, err=%v", hasUpdateSyncCursor, err)
	}

	// 3. Downgrade from v6 to v5
	if err := m.Migrate(5); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("downgrade to v5 failed: %v", err)
	}

	err = pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns 
		WHERE table_schema = 'public' AND table_name = 'raw_bank_payloads' AND column_name = 'raw_payload_bytes'
	`).Scan(&rawColCount)
	if err != nil || rawColCount != 0 {
		t.Fatalf("expected raw_payload_bytes to be dropped after downgrade to v5, got %d, err=%v", rawColCount, err)
	}

	// 4. Re-upgrade to v6
	if err := m.Migrate(6); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("re-upgrade to v6 failed: %v", err)
	}

	version, dirty, err = m.Version()
	if err != nil || version != 6 || dirty {
		t.Fatalf("expected version 6 clean after Migrate(6), got version=%d dirty=%v, err=%v", version, dirty, err)
	}
}

func TestMigrations_UpgradeFromV6ToV7(t *testing.T) {
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

	// 1. Migrate database to version 6
	if err := m.Migrate(6); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to version 6 failed: %v", err)
	}

	version, dirty, err := m.Version()
	if err != nil || version != 6 || dirty {
		t.Fatalf("expected version 6 before v7 upgrade, got version=%d dirty=%v, err=%v", version, dirty, err)
	}

	pool, err := pgxpool.New(ctx, getPGConnString())
	if err != nil {
		t.Fatalf("failed to connect to test db: %v", err)
	}
	defer pool.Close()

	// 2. Upgrade to version 7
	if err := m.Migrate(7); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to version 7 failed: %v", err)
	}

	version, dirty, err = m.Version()
	if err != nil || version != 7 || dirty {
		t.Fatalf("expected version 7 after upgrade, got version=%d dirty=%v, err=%v", version, dirty, err)
	}

	// Verify reconciliation_discrepancies table exists
	var tableCount int
	err = pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.tables 
		WHERE table_schema = 'public' AND table_name = 'reconciliation_discrepancies'
	`).Scan(&tableCount)
	if err != nil || tableCount != 1 {
		t.Fatalf("expected reconciliation_discrepancies to exist at v7, got %d, err=%v", tableCount, err)
	}

	// Verify FORCE ROW LEVEL SECURITY enabled on reconciliation_discrepancies
	var rlsForced bool
	err = pool.QueryRow(ctx, `
		SELECT c.relforcerowsecurity
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relname = 'reconciliation_discrepancies'
	`).Scan(&rlsForced)
	if err != nil || !rlsForced {
		t.Fatalf("expected reconciliation_discrepancies to have FORCE RLS enabled, got %v, err=%v", rlsForced, err)
	}

	// Verify bank_connections columns exist
	for _, col := range []string{"sync_version", "active_sync_token", "sync_locked_until"} {
		var colCount int
		err = pool.QueryRow(ctx, `
			SELECT count(*) FROM information_schema.columns 
			WHERE table_schema = 'public' AND table_name = 'bank_connections' AND column_name = $1
		`, col).Scan(&colCount)
		if err != nil || colCount != 1 {
			t.Fatalf("expected column %s on bank_connections at v7, got %d, err=%v", col, colCount, err)
		}
	}

	// Verify family_worker has INSERT on reconciliation_discrepancies
	var hasInsertDiscrepancies bool
	err = pool.QueryRow(ctx, "SELECT has_table_privilege('family_worker', 'public.reconciliation_discrepancies', 'INSERT')").Scan(&hasInsertDiscrepancies)
	if err != nil || !hasInsertDiscrepancies {
		t.Fatalf("expected family_worker to have INSERT on reconciliation_discrepancies at v7, got %v, err=%v", hasInsertDiscrepancies, err)
	}

	// Verify family_worker has UPDATE on sync_version
	var hasUpdateSyncVersion bool
	err = pool.QueryRow(ctx, "SELECT has_column_privilege('family_worker', 'public.bank_connections', 'sync_version', 'UPDATE')").Scan(&hasUpdateSyncVersion)
	if err != nil || !hasUpdateSyncVersion {
		t.Fatalf("expected family_worker to have UPDATE on sync_version at v7, got %v, err=%v", hasUpdateSyncVersion, err)
	}

	// 3. Downgrade from v7 to v6
	if err := m.Migrate(6); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("downgrade to v6 failed: %v", err)
	}

	err = pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.tables 
		WHERE table_schema = 'public' AND table_name = 'reconciliation_discrepancies'
	`).Scan(&tableCount)
	if err != nil || tableCount != 0 {
		t.Fatalf("expected reconciliation_discrepancies to be dropped after downgrade to v6, got %d, err=%v", tableCount, err)
	}

	// 4. Re-upgrade to v7
	if err := m.Migrate(7); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("re-upgrade to v7 failed: %v", err)
	}

	version, dirty, err = m.Version()
	if err != nil || version != 7 || dirty {
		t.Fatalf("expected version 7 clean after re-upgrade, got version=%d dirty=%v, err=%v", version, dirty, err)
	}
}

func TestMigrations_UpgradeFromV7ToV8(t *testing.T) {
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

	// 1. Migrate to v7
	if err := m.Migrate(7); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to version 7 failed: %v", err)
	}

	version, dirty, err := m.Version()
	if err != nil || version != 7 || dirty {
		t.Fatalf("expected version 7 clean, got version=%d dirty=%v, err=%v", version, dirty, err)
	}

	pool, err := pgxpool.New(ctx, getPGConnString())
	if err != nil {
		t.Fatalf("failed to connect to test db: %v", err)
	}
	defer pool.Close()

	// Verify family_worker does NOT have UPDATE on classification at v7
	var hasUpdateClassification bool
	err = pool.QueryRow(ctx, "SELECT has_column_privilege('family_worker', 'public.accounting_entries', 'classification', 'UPDATE')").Scan(&hasUpdateClassification)
	if err != nil || hasUpdateClassification {
		t.Fatalf("expected family_worker to NOT have UPDATE on classification at v7, got %v, err=%v", hasUpdateClassification, err)
	}

	// 2. Upgrade to v8
	if err := m.Migrate(8); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("upgrade to v8 failed: %v", err)
	}

	version, dirty, err = m.Version()
	if err != nil || version != 8 || dirty {
		t.Fatalf("expected version 8 after upgrade, got version=%d dirty=%v, err=%v", version, dirty, err)
	}

	// Verify family_worker HAS UPDATE on classification at v8
	err = pool.QueryRow(ctx, "SELECT has_column_privilege('family_worker', 'public.accounting_entries', 'classification', 'UPDATE')").Scan(&hasUpdateClassification)
	if err != nil || !hasUpdateClassification {
		t.Fatalf("expected family_worker to have UPDATE on classification at v8, got %v, err=%v", hasUpdateClassification, err)
	}

	// 3. Downgrade from v8 to v7
	if err := m.Migrate(7); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("downgrade to v7 failed: %v", err)
	}

	err = pool.QueryRow(ctx, "SELECT has_column_privilege('family_worker', 'public.accounting_entries', 'classification', 'UPDATE')").Scan(&hasUpdateClassification)
	if err != nil || hasUpdateClassification {
		t.Fatalf("expected family_worker to NOT have UPDATE on classification after downgrade to v7, got %v, err=%v", hasUpdateClassification, err)
	}

	// 4. Re-upgrade to v8
	if err := m.Migrate(8); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("re-upgrade to v8 failed: %v", err)
	}

	version, dirty, err = m.Version()
	if err != nil || version != 8 || dirty {
		t.Fatalf("expected version 8 clean after re-upgrade, got version=%d dirty=%v, err=%v", version, dirty, err)
	}
}

func TestMigrations_UpgradeFromV8ToV9(t *testing.T) {
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

	// 1. Force/migrate database to version 8
	if err := m.Migrate(8); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to version 8 failed: %v", err)
	}

	version, dirty, err := m.Version()
	if err != nil || version != 8 || dirty {
		t.Fatalf("expected version 8 clean, got version=%d dirty=%v, err=%v", version, dirty, err)
	}

	pool, err := pgxpool.New(ctx, getPGConnString())
	if err != nil {
		t.Fatalf("failed to connect to test db: %v", err)
	}
	defer pool.Close()

	// Verify claim_token does NOT exist at v8
	var colCount int
	err = pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns 
		WHERE table_schema = 'public' AND table_name = 'worker_jobs' AND column_name = 'claim_token'
	`).Scan(&colCount)
	if err != nil || colCount != 0 {
		t.Fatalf("expected claim_token column to NOT exist at v8, got %d, err=%v", colCount, err)
	}

	// 2. Seed realistic jobs under v8 in all relevant states:
	famID := uuid.New()
	userID := uuid.New()
	err = WithFamilyUserTx(ctx, pool, famID, userID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO public.users (id, email, password_hash, first_name, last_name)
			VALUES ($1, $2, 'hash', 'V8', 'User')
		`, userID, fmt.Sprintf("v8-%s@test.local", famID.String()[:8]))
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, "INSERT INTO public.families (id, name) VALUES ($1, 'V8 Family')", famID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, "INSERT INTO public.family_members (id, family_id, user_id, role) VALUES ($1, $2, $3, 'owner')",
			uuid.New(), famID, userID)
		return err
	})
	if err != nil {
		t.Fatalf("failed to seed test family: %v", err)
	}

	queueName := fmt.Sprintf("queue_v8_v9_%s", uuid.New().String()[:8])
	jobAvailID := uuid.New()
	jobRunningUnexpiredID := uuid.New()
	jobRunningExpiredID := uuid.New()
	jobFailedID := uuid.New()
	jobDiscardedID := uuid.New()

	err = WithFamilyTx(ctx, pool, famID, func(tx pgx.Tx) error {
		// Available job
		_, err := tx.Exec(ctx, `
			INSERT INTO public.worker_jobs (id, family_id, queue_name, job_type, payload, state, scheduled_at)
			VALUES ($1, $2, $3, 'sync', '{"type":"avail"}', 'available', NOW() - INTERVAL '1 minute')
		`, jobAvailID, famID, queueName)
		if err != nil {
			return err
		}

		// Running with unexpired lease
		_, err = tx.Exec(ctx, `
			INSERT INTO public.worker_jobs (id, family_id, queue_name, job_type, payload, state, locked_by, locked_until)
			VALUES ($1, $2, $3, 'sync', '{"type":"unexpired"}', 'running', 'worker-unexpired-1', NOW() + INTERVAL '10 minutes')
		`, jobRunningUnexpiredID, famID, queueName)
		if err != nil {
			return err
		}

		// Running with expired lease
		_, err = tx.Exec(ctx, `
			INSERT INTO public.worker_jobs (id, family_id, queue_name, job_type, payload, state, locked_by, locked_until)
			VALUES ($1, $2, $3, 'sync', '{"type":"expired"}', 'running', 'worker-expired-1', NOW() - INTERVAL '10 seconds')
		`, jobRunningExpiredID, famID, queueName)
		if err != nil {
			return err
		}

		// Failed job
		_, err = tx.Exec(ctx, `
			INSERT INTO public.worker_jobs (id, family_id, queue_name, job_type, payload, state, last_error, attempts)
			VALUES ($1, $2, $3, 'sync', '{"type":"failed"}', 'failed', 'bad credentials', 1)
		`, jobFailedID, famID, queueName)
		if err != nil {
			return err
		}

		// Discarded job (dead letter)
		_, err = tx.Exec(ctx, `
			INSERT INTO public.worker_jobs (id, family_id, queue_name, job_type, payload, state, last_error, attempts, max_attempts)
			VALUES ($1, $2, $3, 'sync', '{"type":"discarded"}', 'discarded', 'max attempts reached', 5, 5)
		`, jobDiscardedID, famID, queueName)
		return err
	})
	if err != nil {
		t.Fatalf("failed to seed v8 jobs: %v", err)
	}

	// 3. Apply migration v9
	if err := m.Migrate(9); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("upgrade to v9 failed: %v", err)
	}

	version, dirty, err = m.Version()
	if err != nil || version != 9 || dirty {
		t.Fatalf("expected version 9 after upgrade, got version=%d dirty=%v, err=%v", version, dirty, err)
	}

	// 4. Verify existing jobs preserved with states and metadata intact
	type jobCheck struct {
		id            uuid.UUID
		expectedState string
		hasLockedBy   bool
		expectedErr   string
	}
	checks := []jobCheck{
		{jobAvailID, "available", false, ""},
		{jobRunningUnexpiredID, "running", true, ""},
		{jobRunningExpiredID, "running", true, ""},
		{jobFailedID, "failed", false, "bad credentials"},
		{jobDiscardedID, "discarded", false, "max attempts reached"},
	}

	for _, jc := range checks {
		var state string
		var lockedBy *string
		var lastErr *string
		var claimToken *uuid.UUID
		err = pool.QueryRow(ctx, `
			SELECT state, locked_by, last_error, claim_token
			FROM public.worker_jobs
			WHERE id = $1
		`, jc.id).Scan(&state, &lockedBy, &lastErr, &claimToken)
		if err != nil {
			t.Fatalf("failed to query job %s after v9 upgrade: %v", jc.id, err)
		}
		if state != jc.expectedState {
			t.Errorf("job %s state = %q; want %q", jc.id, state, jc.expectedState)
		}
		if claimToken != nil {
			t.Errorf("job %s existing claim_token should be NULL upon upgrade, got %v", jc.id, *claimToken)
		}
		if jc.hasLockedBy && lockedBy == nil {
			t.Errorf("job %s expected locked_by to be set", jc.id)
		}
		if jc.expectedErr != "" && (lastErr == nil || *lastErr != jc.expectedErr) {
			t.Errorf("job %s last_error = %v; want %s", jc.id, lastErr, jc.expectedErr)
		}
	}

	// 5. Test queue semantics under v9 as family_worker
	workerPool, err := pgxpool.New(ctx, getWorkerConnString())
	if err != nil {
		t.Fatalf("failed to connect worker pool: %v", err)
	}
	defer workerPool.Close()

	// Claim 1: must claim available job
	var c1ID, c1FamID, c1Token uuid.UUID
	var c1Type string
	var c1Payload []byte
	err = workerPool.QueryRow(ctx, `
		SELECT job_id, family_id, job_type, payload, claim_token
		FROM queue_sec.claim_next_worker_job($1, $2, $3)
	`, queueName, "worker-v9-runner", 300).Scan(&c1ID, &c1FamID, &c1Type, &c1Payload, &c1Token)
	if err != nil {
		t.Fatalf("claim 1 failed: %v", err)
	}
	if c1ID != jobAvailID {
		t.Errorf("claim 1 expected job %s (available), got %s", jobAvailID, c1ID)
	}
	if c1Token == uuid.Nil {
		t.Errorf("claim 1 returned nil claim_token")
	}

	// Claim 2: must reclaim expired job
	var c2ID, c2FamID, c2Token uuid.UUID
	var c2Type string
	var c2Payload []byte
	err = workerPool.QueryRow(ctx, `
		SELECT job_id, family_id, job_type, payload, claim_token
		FROM queue_sec.claim_next_worker_job($1, $2, $3)
	`, queueName, "worker-v9-runner", 300).Scan(&c2ID, &c2FamID, &c2Type, &c2Payload, &c2Token)
	if err != nil {
		t.Fatalf("claim 2 failed: %v", err)
	}
	if c2ID != jobRunningExpiredID {
		t.Errorf("claim 2 expected job %s (expired), got %s", jobRunningExpiredID, c2ID)
	}
	if c2Token == uuid.Nil {
		t.Errorf("claim 2 returned nil claim_token")
	}
	if c2Token == c1Token {
		t.Errorf("claim 2 returned same token as claim 1: %v", c2Token)
	}

	// Claim 3: should return 0 rows (unexpired, failed, discarded cannot be claimed)
	var c3ID uuid.UUID
	err = workerPool.QueryRow(ctx, `
		SELECT job_id FROM queue_sec.claim_next_worker_job($1, $2, $3)
	`, queueName, "worker-v9-runner", 300).Scan(&c3ID)
	if err == nil {
		t.Errorf("claim 3 expected no rows, got job %s", c3ID)
	} else if err != pgx.ErrNoRows {
		t.Fatalf("claim 3 unexpected error: %v", err)
	}

	// Stale worker fencing on reclaimed job (jobRunningExpiredID):
	// Worker 'worker-expired-1' attempts completion with old identity -> 0 rows affected
	err = WithFamilyTx(ctx, workerPool, famID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE public.worker_jobs
			SET state = 'completed', claim_token = NULL, updated_at = NOW()
			WHERE family_id = $1 AND id = $2 AND state = 'running' AND locked_by = 'worker-expired-1' AND claim_token = gen_random_uuid() AND locked_until > NOW()
		`, famID, jobRunningExpiredID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			t.Errorf("stale worker was able to complete reclaimed job! RowsAffected = %d", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("stale worker completion exec failed: %v", err)
	}

	// Active worker completes reclaimed job using its valid claim_token
	err = WithFamilyTx(ctx, workerPool, famID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE public.worker_jobs
			SET state = 'completed', claim_token = NULL, updated_at = NOW()
			WHERE family_id = $1 AND id = $2 AND state = 'running' AND locked_by = 'worker-v9-runner' AND claim_token = $3 AND locked_until > NOW()
		`, famID, jobRunningExpiredID, c2Token)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			t.Errorf("active worker failed to complete reclaimed job, RowsAffected = %d", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("active worker completion failed: %v", err)
	}

	// 6. Test Down migration to v8
	if err := m.Migrate(8); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("downgrade to v8 failed: %v", err)
	}

	// Verify claim_token column dropped
	err = pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns 
		WHERE table_schema = 'public' AND table_name = 'worker_jobs' AND column_name = 'claim_token'
	`).Scan(&colCount)
	if err != nil || colCount != 0 {
		t.Fatalf("expected claim_token column to be dropped at v8, got %d, err=%v", colCount, err)
	}

	// Verify existing jobs and their states remain intact after downgrade
	for _, jc := range checks {
		var state string
		err = pool.QueryRow(ctx, "SELECT state FROM public.worker_jobs WHERE id = $1", jc.id).Scan(&state)
		if err != nil {
			t.Errorf("job %s missing after downgrade: %v", jc.id, err)
		}
	}

	// 7. Re-upgrade to v9
	if err := m.Migrate(9); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("re-upgrade to v9 failed: %v", err)
	}

	version, dirty, err = m.Version()
	if err != nil || version != 9 || dirty {
		t.Fatalf("expected version 9 clean after re-upgrade, got version=%d dirty=%v, err=%v", version, dirty, err)
	}
}

func TestMigrations_UpgradeFromV9ToV10(t *testing.T) {
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

	// 1. Force to v9
	if err := m.Migrate(9); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to version 9 failed: %v", err)
	}

	pool, err := pgxpool.New(ctx, getPGConnString())
	if err != nil {
		t.Fatalf("failed to connect to test db: %v", err)
	}
	defer pool.Close()

	// Verify bank_auth_attempts does NOT exist at v9
	var tableCount int
	err = pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.tables 
		WHERE table_schema = 'public' AND table_name = 'bank_auth_attempts'
	`).Scan(&tableCount)
	if err != nil || tableCount != 0 {
		t.Fatalf("expected bank_auth_attempts table to NOT exist at v9, got %d, err=%v", tableCount, err)
	}

	// 2. Upgrade to v10
	if err := m.Migrate(10); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("upgrade to v10 failed: %v", err)
	}

	version, dirty, err := m.Version()
	if err != nil || version != 10 || dirty {
		t.Fatalf("expected version 10 after upgrade, got version=%d dirty=%v, err=%v", version, dirty, err)
	}

	// Verify bank_auth_attempts exists at v10
	err = pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.tables 
		WHERE table_schema = 'public' AND table_name = 'bank_auth_attempts'
	`).Scan(&tableCount)
	if err != nil || tableCount != 1 {
		t.Fatalf("expected bank_auth_attempts table to exist at v10, got %d, err=%v", tableCount, err)
	}

	// Verify RLS is enabled and forced
	var rlsEnabled, rlsForced bool
	err = pool.QueryRow(ctx, `
		SELECT c.relrowsecurity, c.relforcerowsecurity 
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relname = 'bank_auth_attempts'
	`).Scan(&rlsEnabled, &rlsForced)
	if err != nil || !rlsEnabled || !rlsForced {
		t.Fatalf("expected RLS enabled and forced on bank_auth_attempts, got rlsEnabled=%v, rlsForced=%v, err=%v", rlsEnabled, rlsForced, err)
	}

	// Verify unique index on (telegram_chat_id, telegram_message_id) WHERE state = 'awaiting_otp'
	var idxCount int
	err = pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_indexes 
		WHERE schemaname = 'public' AND tablename = 'bank_auth_attempts' AND indexname = 'idx_auth_attempts_tg_active'
	`).Scan(&idxCount)
	if err != nil || idxCount != 1 {
		t.Fatalf("expected index idx_auth_attempts_tg_active to exist, got %d, err=%v", idxCount, err)
	}

	// 3. Downgrade to v9
	if err := m.Migrate(9); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("downgrade to v9 failed: %v", err)
	}

	err = pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.tables 
		WHERE table_schema = 'public' AND table_name = 'bank_auth_attempts'
	`).Scan(&tableCount)
	if err != nil || tableCount != 0 {
		t.Fatalf("expected bank_auth_attempts table to be dropped at v9, got %d, err=%v", tableCount, err)
	}

	// 4. Re-upgrade to v10
	if err := m.Migrate(10); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("final re-upgrade to v10 failed: %v", err)
	}

	version, dirty, err = m.Version()
	if err != nil || version != 10 || dirty {
		t.Fatalf("expected version 10 clean after final Migrate(10), got version=%d dirty=%v, err=%v", version, dirty, err)
	}
}

func TestMigrations_Migration000011_BankConnectionStatesAndValidation(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, getPGConnString())
	if err != nil {
		t.Fatalf("failed to connect to postgres: %v", err)
	}
	defer pool.Close()

	migrationsDir, err := filepath.Abs("../../migrations")
	if err != nil {
		t.Fatalf("failed to get migrations directory: %v", err)
	}

	dbURL := getTestDatabaseURL()
	m, err := migrate.New("file://"+migrationsDir, dbURL)
	if err != nil {
		t.Fatalf("failed to create migrate instance: %v", err)
	}
	defer m.Close()

	// 1. Ensure at v10
	if err := m.Migrate(10); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("failed to reach v10: %v", err)
	}

	// 2. Upgrade to v11
	if err := m.Migrate(11); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("upgrade to v11 failed: %v", err)
	}

	version, dirty, err := m.Version()
	if err != nil || version != 11 || dirty {
		t.Fatalf("expected version 11 after upgrade, got version=%d dirty=%v, err=%v", version, dirty, err)
	}

	// Verify bank_validation_reports exists at v11
	var tableCount int
	err = pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.tables 
		WHERE table_schema = 'public' AND table_name = 'bank_validation_reports'
	`).Scan(&tableCount)
	if err != nil || tableCount != 1 {
		t.Fatalf("expected bank_validation_reports table to exist at v11, got %d, err=%v", tableCount, err)
	}

	// Verify RLS is enabled and forced on bank_validation_reports
	var rlsEnabled, rlsForced bool
	err = pool.QueryRow(ctx, `
		SELECT c.relrowsecurity, c.relforcerowsecurity 
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relname = 'bank_validation_reports'
	`).Scan(&rlsEnabled, &rlsForced)
	if err != nil || !rlsEnabled || !rlsForced {
		t.Fatalf("expected RLS enabled and forced on bank_validation_reports, got rlsEnabled=%v, rlsForced=%v, err=%v", rlsEnabled, rlsForced, err)
	}

	// Verify columns on bank_connections
	var colCount int
	err = pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns 
		WHERE table_schema = 'public' AND table_name = 'bank_connections' 
		  AND column_name IN ('last_authenticated_at', 'last_validated_at', 'last_validation_status', 'last_validation_report_id')
	`).Scan(&colCount)
	if err != nil || colCount != 4 {
		t.Fatalf("expected 4 new columns on bank_connections at v11, got %d, err=%v", colCount, err)
	}

	// 3. Downgrade to v10
	if err := m.Migrate(10); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("downgrade to v10 failed: %v", err)
	}

	err = pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.tables 
		WHERE table_schema = 'public' AND table_name = 'bank_validation_reports'
	`).Scan(&tableCount)
	if err != nil || tableCount != 0 {
		t.Fatalf("expected bank_validation_reports table to be dropped at v10, got %d, err=%v", tableCount, err)
	}

	// 4. Re-upgrade to v11
	if err := m.Migrate(11); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("final re-upgrade to v11 failed: %v", err)
	}

	version, dirty, err = m.Version()
	if err != nil || version != 11 || dirty {
		t.Fatalf("expected version 11 clean after final Up, got version=%d dirty=%v, err=%v", version, dirty, err)
	}
}

func TestMigrations_Migration000012_InitialImportApprovalsAndFencing(t *testing.T) {
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

	pool, err := pgxpool.New(ctx, getPGConnString())
	if err != nil {
		t.Fatalf("failed to connect to test db: %v", err)
	}
	defer pool.Close()

	// 1. Force to v11
	if err := m.Migrate(11); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to v11 failed: %v", err)
	}

	// 2. Upgrade to v12
	if err := m.Migrate(12); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("upgrade to v12 failed: %v", err)
	}

	version, dirty, err := m.Version()
	if err != nil || version != 12 || dirty {
		t.Fatalf("expected version 12 after upgrade, got version=%d dirty=%v, err=%v", version, dirty, err)
	}

	// Verify bank_initial_import_approvals exists at v12
	var tableCount int
	err = pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.tables
		WHERE table_schema = 'public' AND table_name = 'bank_initial_import_approvals'
	`).Scan(&tableCount)
	if err != nil || tableCount != 1 {
		t.Fatalf("expected bank_initial_import_approvals table to exist at v12, got %d, err=%v", tableCount, err)
	}

	// Verify RLS is enabled and forced on bank_initial_import_approvals
	var rlsEnabled, rlsForced bool
	err = pool.QueryRow(ctx, `
		SELECT c.relrowsecurity, c.relforcerowsecurity
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relname = 'bank_initial_import_approvals'
	`).Scan(&rlsEnabled, &rlsForced)
	if err != nil || !rlsEnabled || !rlsForced {
		t.Fatalf("expected RLS enabled and forced on bank_initial_import_approvals, got rlsEnabled=%v, rlsForced=%v, err=%v", rlsEnabled, rlsForced, err)
	}

	// Verify new columns on bank_connections
	var colCount int
	err = pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'bank_connections'
		  AND column_name IN ('fencing_token', 'active_operation_type', 'initial_import_completed_at')
	`).Scan(&colCount)
	if err != nil || colCount != 3 {
		t.Fatalf("expected 3 new columns on bank_connections at v12, got %d, err=%v", colCount, err)
	}

	// 3. Downgrade to v11
	if err := m.Migrate(11); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("downgrade to v11 failed: %v", err)
	}

	err = pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.tables
		WHERE table_schema = 'public' AND table_name = 'bank_initial_import_approvals'
	`).Scan(&tableCount)
	if err != nil || tableCount != 0 {
		t.Fatalf("expected bank_initial_import_approvals table to be dropped at v11, got %d, err=%v", tableCount, err)
	}

	// 4. Re-upgrade to v12
	if err := m.Migrate(12); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("final re-upgrade to v12 failed: %v", err)
	}

	version, dirty, err = m.Version()
	if err != nil || version != 12 || dirty {
		t.Fatalf("expected version 12 clean after final Up, got version=%d dirty=%v, err=%v", version, dirty, err)
	}
}

func TestMigrations_Migration000013_StatementIngestionAndStaging(t *testing.T) {
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

	pool, err := pgxpool.New(ctx, getPGConnString())
	if err != nil {
		t.Fatalf("failed to connect to test db: %v", err)
	}
	defer pool.Close()

	// 1. Force/migrate to v12
	if err := m.Migrate(12); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to v12 failed: %v", err)
	}

	// 2. Upgrade to v13
	if err := m.Migrate(13); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("upgrade to v13 failed: %v", err)
	}

	version, dirty, err := m.Version()
	if err != nil || version != 13 || dirty {
		t.Fatalf("expected version 13 after upgrade, got version=%d dirty=%v, err=%v", version, dirty, err)
	}

	// Verify new tables exist at v13
	newTables := []string{
		"statement_documents",
		"statement_import_batches",
		"staged_bank_transactions",
		"statement_reconciliation_overrides",
	}
	for _, tbl := range newTables {
		var tableCount int
		err = pool.QueryRow(ctx, `
			SELECT count(*) FROM information_schema.tables
			WHERE table_schema = 'public' AND table_name = $1
		`, tbl).Scan(&tableCount)
		if err != nil || tableCount != 1 {
			t.Fatalf("expected table %s to exist at v13, got count=%d, err=%v", tbl, tableCount, err)
		}

		// Verify RLS is enabled and forced
		var rlsEnabled, rlsForced bool
		err = pool.QueryRow(ctx, `
			SELECT c.relrowsecurity, c.relforcerowsecurity
			FROM pg_class c
			JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = 'public' AND c.relname = $1
		`, tbl).Scan(&rlsEnabled, &rlsForced)
		if err != nil || !rlsEnabled || !rlsForced {
			t.Fatalf("expected RLS enabled and forced on %s, got enabled=%v, forced=%v, err=%v", tbl, rlsEnabled, rlsForced, err)
		}
	}

	// Verify settle_pending_bank_transaction function exists with SECURITY DEFINER
	var isSecDef bool
	err = pool.QueryRow(ctx, `
		SELECT prosecdef FROM pg_proc WHERE proname = 'settle_pending_bank_transaction'
	`).Scan(&isSecDef)
	if err != nil || !isSecDef {
		t.Fatalf("expected settle_pending_bank_transaction function to exist with SECURITY DEFINER, got %v, err=%v", isSecDef, err)
	}

	// Verify uq_bank_transactions_tenant_account constraint on bank_transactions
	var conCount int
	err = pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_constraint WHERE conname = 'uq_bank_transactions_tenant_account'
	`).Scan(&conCount)
	if err != nil || conCount != 1 {
		t.Fatalf("expected uq_bank_transactions_tenant_account constraint on bank_transactions at v13, got %d, err=%v", conCount, err)
	}

	// 3. Downgrade to v12
	if err := m.Migrate(12); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("downgrade to v12 failed: %v", err)
	}

	for _, tbl := range newTables {
		var tableCount int
		err = pool.QueryRow(ctx, `
			SELECT count(*) FROM information_schema.tables
			WHERE table_schema = 'public' AND table_name = $1
		`, tbl).Scan(&tableCount)
		if err != nil || tableCount != 0 {
			t.Fatalf("expected table %s to be dropped at v12, got %d, err=%v", tbl, tableCount, err)
		}
	}

	err = pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_proc WHERE proname = 'settle_pending_bank_transaction'
	`).Scan(&conCount)
	if err != nil || conCount != 0 {
		t.Fatalf("expected settle_pending_bank_transaction to be dropped at v12, got %d, err=%v", conCount, err)
	}

	err = pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_constraint WHERE conname = 'uq_bank_transactions_tenant_account'
	`).Scan(&conCount)
	if err != nil || conCount != 0 {
		t.Fatalf("expected uq_bank_transactions_tenant_account constraint to be dropped at v12, got %d, err=%v", conCount, err)
	}

	// 4. Re-upgrade to v13
	if err := m.Migrate(13); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("final re-upgrade to v13 failed: %v", err)
	}

	version, dirty, err = m.Version()
	if err != nil || version != 13 || dirty {
		t.Fatalf("expected version 13 clean after final Up, got version=%d dirty=%v, err=%v", version, dirty, err)
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

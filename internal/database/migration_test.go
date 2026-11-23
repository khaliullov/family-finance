package database

import (
	"context"
	"os"
	"path/filepath"
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
	if version != 2 || dirty {
		t.Fatalf("expected version 2 (clean), got version=%d dirty=%v", version, dirty)
	}

	pool, err := pgxpool.New(ctx, getPGConnString())
	if err != nil {
		t.Fatalf("failed to connect to test db: %v", err)
	}
	defer pool.Close()

	// Verify all 16 canonical tables exist after Up
	expectedTables := []string{
		"users", "families", "family_members", "bank_connections", "raw_bank_payloads",
		"accounts", "cards", "bank_transactions", "transfer_matches", "transfer_rejections",
		"categories", "accounting_entries", "categorization_rules", "statement_imports",
		"audit_logs", "worker_jobs",
	}
	for _, tbl := range expectedTables {
		var count int
		err = pool.QueryRow(ctx, "SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name = $1", tbl).Scan(&count)
		if err != nil || count != 1 {
			t.Fatalf("expected table %s to exist after 1st Up, got count=%d, err=%v", tbl, count, err)
		}
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
	err = pool.QueryRow(ctx, "SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'bank_transactions'").Scan(&tableCount)
	if err != nil || tableCount != 0 {
		t.Fatalf("expected bank_transactions table to be dropped after Down, got count=%d, err=%v", tableCount, err)
	}

	// 4. Second Up migration to ensure idempotency and re-apply cleanly
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate.Up (2nd) failed: %v", err)
	}

	version, dirty, err = m.Version()
	if err != nil || version != 2 || dirty {
		t.Fatalf("expected version 2 clean after 2nd Up, got version=%d dirty=%v, err=%v", version, dirty, err)
	}

	err = pool.QueryRow(ctx, "SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'bank_transactions'").Scan(&tableCount)
	if err != nil || tableCount != 1 {
		t.Fatalf("expected bank_transactions table to exist after 2nd Up, got count=%d, err=%v", tableCount, err)
	}
}

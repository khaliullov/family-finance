package transaction

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/khaliullov/family-finance/internal/database"
)

var migrateOnce sync.Once

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
	return fmt.Sprintf("postgres://family_worker:worker_secret_placeholder@%s:%s/%s?sslmode=disable", host, port, dbname)
}

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
	return fmt.Sprintf("postgres://family_app:app_secret_placeholder@%s:%s/%s?sslmode=disable", host, port, dbname)
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
	return fmt.Sprintf("pgx5://%s:%s@%s:%s/%s?sslmode=disable", user, password, host, port, dbname)
}

func ensureMigrations(t *testing.T) {
	migrateOnce.Do(func() {
		migrationsPath, err := filepath.Abs("../../migrations")
		if err != nil {
			t.Fatalf("failed to resolve migrations directory: %v", err)
		}
		m, err := migrate.New("file://"+migrationsPath, getPGConnString())
		if err != nil {
			t.Fatalf("failed to initialize migrations: %v", err)
		}
		defer m.Close()

		if err := m.Up(); err != nil && err != migrate.ErrNoChange {
			t.Fatalf("failed to apply migrations: %v", err)
		}
	})
}

func getWorkerPool(t *testing.T) *pgxpool.Pool {
	ensureMigrations(t)
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(getWorkerConnString())
	if err != nil {
		t.Fatalf("failed to parse worker connection string: %v", err)
	}
	cfg.MaxConns = 15
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("failed to connect as family_worker: %v", err)
	}
	return pool
}

func getAppPool(t *testing.T) *pgxpool.Pool {
	ensureMigrations(t)
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(getAppConnString())
	if err != nil {
		t.Fatalf("failed to parse app connection string: %v", err)
	}
	cfg.MaxConns = 15
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("failed to connect as family_app: %v", err)
	}
	return pool
}

// setupTenantFixture provisions a test family, owner user, and a bank connection using family_app.
func setupTenantFixture(t *testing.T, provider string) (uuid.UUID, uuid.UUID, uuid.UUID) {
	appPool := getAppPool(t)
	defer appPool.Close()

	ctx := context.Background()
	familyID := uuid.New()
	userID := uuid.New()
	connID := uuid.New()
	email := fmt.Sprintf("tenant-%s@example.com", uuid.New().String()[:8])

	err := database.WithFamilyUserTx(ctx, appPool, familyID, userID, func(tx pgx.Tx) error {
		// 1. Create User
		_, err := tx.Exec(ctx, `
			INSERT INTO public.users (id, email, password_hash, first_name, last_name, is_active)
			VALUES ($1, $2, '$argon2id$v=19$m=65536,t=3,p=4$dummyhash', 'Test', 'Owner', TRUE)
		`, userID, email)
		if err != nil {
			return fmt.Errorf("failed to insert user: %w", err)
		}

		// 2. Create Family
		_, err = tx.Exec(ctx, `
			INSERT INTO public.families (id, name, default_currency)
			VALUES ($1, 'Test Family', 'RUB')
		`, familyID)
		if err != nil {
			return fmt.Errorf("failed to insert family: %w", err)
		}

		// 3. Create Family Member
		_, err = tx.Exec(ctx, `
			INSERT INTO public.family_members (id, family_id, user_id, role)
			VALUES ($1, $2, $3, 'owner')
		`, uuid.New(), familyID, userID)
		if err != nil {
			return fmt.Errorf("failed to insert family member: %w", err)
		}

		// 4. Create Bank Connection
		_, err = tx.Exec(ctx, `
			INSERT INTO public.bank_connections (
				id, family_id, authorized_by_user_id, provider, name, status
			) VALUES (
				$1, $2, $3, $4, 'Test Bank Connection', 'active'
			)
		`, connID, familyID, userID, provider)
		if err != nil {
			return fmt.Errorf("failed to insert bank connection: %w", err)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("failed to setup tenant fixture: %v", err)
	}

	return familyID, userID, connID
}

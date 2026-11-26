package statement

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/khaliullov/family-finance/internal/database"
)

var (
	migrateOnce    sync.Once
	testCryptoKey = []byte("12345678901234567890123456789012") // 32 bytes for AES-256
)

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
	return fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable", user, password, host, port, dbname)
}

func getMigrateConnString() string {
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
		m, err := migrate.New("file://"+migrationsPath, getMigrateConnString())
		if err != nil {
			t.Fatalf("failed to initialize migrations: %v", err)
		}
		defer m.Close()

		if v, d, err := m.Version(); err == nil && d {
			_ = m.Force(int(v))
		}
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

func setupStatementTenantFixture(t *testing.T) (uuid.UUID, uuid.UUID, uuid.UUID) {
	appPool := getAppPool(t)
	defer appPool.Close()

	ctx := context.Background()
	familyID := uuid.New()
	userID := uuid.New()
	accountID := uuid.New()
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
			VALUES ($1, 'Statement Test Family', 'RUB')
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

		// 4. Create Account
		now := time.Now().UTC()
		_, err = tx.Exec(ctx, `
			INSERT INTO public.accounts (
				id, family_id, owner_user_id, name, type, currency,
				current_balance_minor, balance_source, balance_updated_at
			) VALUES ($1, $2, $3, 'Primary Checking', 'checking', 'RUB', 100000, 'manual_initial', $4)
		`, accountID, familyID, userID, now)
		if err != nil {
			return fmt.Errorf("failed to insert account: %w", err)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("failed to setup tenant fixture: %v", err)
	}

	return familyID, userID, accountID
}

// StubParser implements StatementParser for testing.
type StubParser struct {
	ProfileData StatementProfile
	ParsedData  *ParsedStatementDTO
	ParseErr    error
}

func (s *StubParser) Profile() StatementProfile {
	if s.ProfileData.FormatID == "" {
		return StatementProfile{
			FormatID:    "stub_format",
			DisplayName: "Stub Format",
		}
	}
	return s.ProfileData
}

func (s *StubParser) Sniff(headerChunk []byte) bool {
	return true
}

func (s *StubParser) Parse(ctx context.Context, r io.Reader, accountCurrency string) (*ParsedStatementDTO, error) {
	if s.ParseErr != nil {
		return nil, s.ParseErr
	}
	return s.ParsedData, nil
}

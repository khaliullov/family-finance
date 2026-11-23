package auth

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
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

func ensureMigrated(t *testing.T) {
	migrationsDir, err := filepath.Abs("../../migrations")
	if err != nil {
		t.Fatalf("failed to get migrations dir: %v", err)
	}
	m, err := migrate.New("file://"+migrationsDir, getTestDatabaseURL())
	if err != nil {
		t.Fatalf("migrate.New failed: %v", err)
	}
	defer m.Close()
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate.Up failed: %v", err)
	}
}

func getAppPool(t *testing.T) *pgxpool.Pool {
	ensureMigrated(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, getAppConnString())
	if err != nil {
		t.Fatalf("failed to create pool as family_app: %v", err)
	}
	return pool
}

func getWorkerPool(t *testing.T) *pgxpool.Pool {
	ensureMigrated(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, getWorkerConnString())
	if err != nil {
		t.Fatalf("failed to create pool as family_worker: %v", err)
	}
	return pool
}

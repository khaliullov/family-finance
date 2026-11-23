package database

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrSuperuserProhibited = errors.New("superuser or BYPASSRLS role prohibited in application connection pool")
)

// Config holds PostgreSQL connection pool configuration.
type Config struct {
	Host     string
	Port     string
	User     string
	Password string
	Database string
	SSLMode  string
	MaxConns int32
	MinConns int32
}

// ConnString builds a standard PostgreSQL connection URI.
func (c Config) ConnString() string {
	if c.Host == "" {
		c.Host = "localhost"
	}
	if c.Port == "" {
		c.Port = "5432"
	}
	if c.User == "" {
		c.User = "family_app"
	}
	if c.Database == "" {
		c.Database = "family_finance"
	}
	if c.SSLMode == "" {
		c.SSLMode = "disable"
	}
	return fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=%s",
		c.User, c.Password, c.Host, c.Port, c.Database, c.SSLMode)
}

// ValidatePoolSecurity verifies that the connection pool user is a least-privilege role
// and does NOT possess SUPERUSER or BYPASSRLS privileges, which would bypass Row-Level Security.
func ValidatePoolSecurity(ctx context.Context, pool *pgxpool.Pool) error {
	if os.Getenv("ALLOW_SUPERUSER_DB") == "1" {
		return nil
	}

	var rolSuper, rolBypassRLS bool
	var rolName string
	err := pool.QueryRow(ctx, "SELECT rolsuper, rolbypassrls, rolname FROM pg_roles WHERE rolname = CURRENT_USER").Scan(&rolSuper, &rolBypassRLS, &rolName)
	if err != nil {
		return fmt.Errorf("failed to check role security attributes: %w", err)
	}

	if rolSuper || rolBypassRLS {
		return fmt.Errorf("%w: role %q has superuser=%v, bypassrls=%v; application must connect as least-privilege role (family_app)",
			ErrSuperuserProhibited, rolName, rolSuper, rolBypassRLS)
	}
	return nil
}

// NewPool initializes and validates a pgxpool.Pool.
func NewPool(ctx context.Context, connStr string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(connStr)
	if err != nil {
		return nil, fmt.Errorf("failed to parse connection string: %w", err)
	}

	if cfg.MaxConns <= 0 {
		cfg.MaxConns = 25
	}
	if cfg.MinConns <= 0 {
		cfg.MinConns = 2
	}
	cfg.MaxConnLifetime = 1 * time.Hour
	cfg.MaxConnIdleTime = 15 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create connection pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	if err := ValidatePoolSecurity(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}

	return pool, nil
}

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
// and does NOT possess SUPERUSER or BYPASSRLS privileges, cannot escalate via SET ROLE,
// and cannot bypass Row-Level Security.
func ValidatePoolSecurity(ctx context.Context, pool *pgxpool.Pool) error {
	if os.Getenv("ALLOW_SUPERUSER_DB") == "1" {
		return nil
	}

	// 1. Direct role attributes
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

	// 2. Inherited membership in superuser or bypassrls roles
	var inheritedSuperCount int
	err = pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_roles 
		WHERE (rolsuper OR rolbypassrls) 
		  AND rolname != CURRENT_USER 
		  AND pg_has_role(CURRENT_USER, rolname, 'MEMBER')
	`).Scan(&inheritedSuperCount)
	if err != nil {
		return fmt.Errorf("failed to check inherited role privileges: %w", err)
	}
	if inheritedSuperCount > 0 {
		return fmt.Errorf("%w: role %q inherits membership from superuser or bypassrls roles",
			ErrSuperuserProhibited, rolName)
	}

	// 3. Privilege escalation prevention: assert SET ROLE to superuser / admin fails
	for _, privRole := range []string{"postgres", "family_admin"} {
		_, err := pool.Exec(ctx, fmt.Sprintf("SET ROLE %s", privRole))
		if err == nil {
			_, _ = pool.Exec(ctx, "RESET ROLE")
			return fmt.Errorf("%w: role %q can escalate privileges to %q via SET ROLE",
				ErrSuperuserProhibited, rolName, privRole)
		}
	}

	// 4. Verify application role does not own protected tables in public schema
	var ownedCount int
	err = pool.QueryRow(ctx, "SELECT count(*) FROM pg_tables WHERE schemaname = 'public' AND tableowner = CURRENT_USER").Scan(&ownedCount)
	if err != nil {
		return fmt.Errorf("failed to check table ownership: %w", err)
	}
	if ownedCount > 0 {
		return fmt.Errorf("%w: role %q owns %d tables in public schema; tables must be owned by family_admin",
			ErrSuperuserProhibited, rolName, ownedCount)
	}

	// 5. Verify FORCE ROW LEVEL SECURITY on all tenant tables
	tenantTables := []string{
		"families", "family_members", "bank_connections", "raw_bank_payloads",
		"accounts", "cards", "bank_transactions", "accounting_entries",
		"categories", "categorization_rules", "transfer_matches",
		"transfer_rejections", "statement_imports", "audit_logs",
		"worker_jobs", "user_sessions",
	}
	rows, err := pool.Query(ctx, `
		SELECT c.relname, c.relrowsecurity, c.relforcerowsecurity
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' 
		  AND c.relkind = 'r'
		  AND c.relname = ANY($1)
	`, tenantTables)
	if err != nil {
		return fmt.Errorf("failed to check FORCE ROW LEVEL SECURITY catalog status: %w", err)
	}
	defer rows.Close()

	foundTables := make(map[string]bool)
	for rows.Next() {
		var name string
		var rls, forceRLS bool
		if err := rows.Scan(&name, &rls, &forceRLS); err != nil {
			return fmt.Errorf("failed to scan table RLS attributes: %w", err)
		}
		foundTables[name] = true
		if !rls || !forceRLS {
			return fmt.Errorf("%w: table %q does not have FORCE ROW LEVEL SECURITY enabled (rowsecurity=%v, forcerowsecurity=%v)",
				ErrSuperuserProhibited, name, rls, forceRLS)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("error reading table RLS attributes: %w", err)
	}
	for _, expected := range tenantTables {
		if !foundTables[expected] {
			return fmt.Errorf("expected tenant table %q missing from public schema catalog", expected)
		}
	}

	// 6. Verify required tenant-isolation policies exist
	var policyCount int
	err = pool.QueryRow(ctx, `
		SELECT count(DISTINCT tablename) FROM pg_policies 
		WHERE schemaname = 'public' AND tablename = ANY($1)
	`, tenantTables).Scan(&policyCount)
	if err != nil {
		return fmt.Errorf("failed to check tenant policies in pg_policies: %w", err)
	}
	if policyCount < len(tenantTables) {
		return fmt.Errorf("expected at least %d tenant tables with RLS policies, found %d", len(tenantTables), policyCount)
	}

	// 7. Control queries must execute successfully and enforce deny-all when context is unset
	var accountCount int
	err = pool.QueryRow(ctx, "SELECT count(*) FROM public.accounts").Scan(&accountCount)
	if err != nil {
		return fmt.Errorf("security validation control query on accounts failed: %w", err)
	}
	if accountCount > 0 {
		return fmt.Errorf("%w: role %q bypassed RLS (returned %d accounts without tenant context)",
			ErrSuperuserProhibited, rolName, accountCount)
	}

	var cardCount int
	err = pool.QueryRow(ctx, "SELECT count(*) FROM public.cards").Scan(&cardCount)
	if err != nil {
		return fmt.Errorf("security validation control query on cards failed: %w", err)
	}
	if cardCount > 0 {
		return fmt.Errorf("%w: role %q bypassed RLS (returned %d cards without tenant context)",
			ErrSuperuserProhibited, rolName, cardCount)
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

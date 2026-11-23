package database

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
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
		env := strings.ToLower(strings.TrimSpace(os.Getenv("APP_ENV")))
		if env == "production" || env == "prod" {
			return errors.New("security validation bypass ALLOW_SUPERUSER_DB is strictly prohibited in production environment")
		}
		if env != "development" && env != "test" && env != "dev" {
			return errors.New("security validation bypass ALLOW_SUPERUSER_DB is only permitted in explicit development/test environments (APP_ENV=development or APP_ENV=test)")
		}
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

	// 6. Verify required tenant-isolation policies exist and strictly enforce tenant boundaries
	policyRows, err := pool.Query(ctx, `
		SELECT tablename, policyname, permissive, roles::text[], cmd, COALESCE(qual, ''), COALESCE(with_check, '')
		FROM pg_policies 
		WHERE schemaname = 'public' AND tablename = ANY($1)
	`, tenantTables)
	if err != nil {
		return fmt.Errorf("failed to check tenant policies in pg_policies: %w", err)
	}
	defer policyRows.Close()

	tablesWithAppPolicy := make(map[string]bool)
	for policyRows.Next() {
		var tableName, policyName, permissive, cmd, qual, withCheck string
		var roles []string
		if err := policyRows.Scan(&tableName, &policyName, &permissive, &roles, &cmd, &qual, &withCheck); err != nil {
			return fmt.Errorf("failed to scan policy row: %w", err)
		}

		if permissive != "PERMISSIVE" {
			return fmt.Errorf("policy %q on %q has unexpected permissiveness %q; expected PERMISSIVE", policyName, tableName, permissive)
		}

		// Validate that roles are only recognized least-privilege roles
		for _, r := range roles {
			switch r {
			case "family_app", "family_worker", "family_auth_definer", "family_auth_verifier", "family_queue_definer":
				// valid
			default:
				return fmt.Errorf("policy %q on %q targets unauthorized or unexpected role %q", policyName, tableName, r)
			}
		}

		hasAppOrWorker := false
		for _, r := range roles {
			if r == "family_app" || r == "family_worker" {
				hasAppOrWorker = true
				break
			}
		}

		if hasAppOrWorker {
			tablesWithAppPolicy[tableName] = true

			// Ensure policy expression strictly enforces tenant isolation
			// An application/worker policy must never have qual='true' or with_check='true'
			if qual == "true" {
				return fmt.Errorf("policy %q on %q has permissive USING 'true' for application/worker role, violating tenant isolation", policyName, tableName)
			}
			if withCheck == "true" {
				return fmt.Errorf("policy %q on %q has permissive WITH CHECK 'true' for application/worker role, violating tenant isolation", policyName, tableName)
			}

			// Validate that expression references tenant boundaries
			qualLower := strings.ToLower(qual)
			checkLower := strings.ToLower(withCheck)
			containsTenantBound := func(expr string) bool {
				return strings.Contains(expr, "get_current_family_id()") ||
					strings.Contains(expr, "app.current_family_id") ||
					strings.Contains(expr, "app.current_user_id") ||
					strings.Contains(expr, "get_current_user_family_ids()")
			}

			if qual != "" && !containsTenantBound(qualLower) {
				return fmt.Errorf("policy %q on %q USING expression %q does not reference tenant boundary", policyName, tableName, qual)
			}
			if withCheck != "" && !containsTenantBound(checkLower) {
				return fmt.Errorf("policy %q on %q WITH CHECK expression %q does not reference tenant boundary", policyName, tableName, withCheck)
			}
			if qual == "" && withCheck == "" {
				return fmt.Errorf("policy %q on %q for application/worker role has neither USING nor WITH CHECK expression", policyName, tableName)
			}
		} else {
			// For policies not containing family_app or family_worker:
			// Must be one of the explicitly allowed definer policies
			isAllowedDefinerPolicy := (tableName == "worker_jobs" && (policyName == "rls_worker_jobs_claim_select" || policyName == "rls_worker_jobs_claim_update")) ||
				(tableName == "user_sessions" && policyName == "rls_user_sessions_verifier") ||
				(tableName == "family_members" && policyName == "rls_family_members_auth_definer")
			if !isAllowedDefinerPolicy {
				return fmt.Errorf("unexpected definer policy %q on table %q for roles %v", policyName, tableName, roles)
			}
		}
	}
	if err := policyRows.Err(); err != nil {
		return fmt.Errorf("error reading policy rows: %w", err)
	}

	for _, expected := range tenantTables {
		if !tablesWithAppPolicy[expected] {
			return fmt.Errorf("tenant table %q lacks an active tenant isolation policy for application/worker", expected)
		}
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

package database

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
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
		"worker_jobs", "user_sessions", "reconciliation_discrepancies",
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

	// 6. Verify required tenant-isolation policies exist and strictly match approved definitions
	expectedPolicies := getExpectedRLSPolicies()
	seenPolicies := make(map[string]bool)

	policyRows, err := pool.Query(ctx, `
		SELECT tablename, policyname, permissive, roles::text[], cmd, COALESCE(qual, ''), COALESCE(with_check, '')
		FROM pg_policies 
		WHERE schemaname = 'public' AND tablename = ANY($1)
	`, tenantTables)
	if err != nil {
		return fmt.Errorf("failed to check tenant policies in pg_policies: %w", err)
	}
	defer policyRows.Close()

	for policyRows.Next() {
		var tableName, policyName, permissive, cmd, qual, withCheck string
		var roles []string
		if err := policyRows.Scan(&tableName, &policyName, &permissive, &roles, &cmd, &qual, &withCheck); err != nil {
			return fmt.Errorf("failed to scan policy row: %w", err)
		}

		key := tableName + "." + policyName
		expected, ok := expectedPolicies[key]
		if !ok {
			return fmt.Errorf("unexpected or unapproved policy %q on table %q (unapproved permissive policy)", policyName, tableName)
		}

		// Prohibit policy targeting PUBLIC role
		for _, r := range roles {
			if strings.ToLower(r) == "public" {
				return fmt.Errorf("policy %q on %q targets PUBLIC role, violating tenant isolation", policyName, tableName)
			}
		}

		if permissive != expected.Permissive {
			return fmt.Errorf("policy %q on %q has unexpected permissiveness %q; expected %q", policyName, tableName, permissive, expected.Permissive)
		}

		if cmd != expected.Cmd {
			return fmt.Errorf("policy %q on %q has unexpected cmd %q; expected %q", policyName, tableName, cmd, expected.Cmd)
		}

		sort.Strings(roles)
		if len(roles) != len(expected.Roles) {
			return fmt.Errorf("policy %q on %q has unexpected roles %v; expected %v", policyName, tableName, roles, expected.Roles)
		}
		for i, r := range roles {
			if r != expected.Roles[i] {
				return fmt.Errorf("policy %q on %q has unexpected role %q; expected %q", policyName, tableName, r, expected.Roles[i])
			}
		}

		normQual := normalizeSQLExpr(qual)
		normExpectedQual := normalizeSQLExpr(expected.Qual)
		if normQual != normExpectedQual {
			return fmt.Errorf("policy %q on %q has unexpected USING expression %q; expected %q", policyName, tableName, qual, expected.Qual)
		}

		normCheck := normalizeSQLExpr(withCheck)
		normExpectedCheck := normalizeSQLExpr(expected.WithCheck)
		if normCheck != normExpectedCheck {
			return fmt.Errorf("policy %q on %q has unexpected WITH CHECK expression %q; expected %q", policyName, tableName, withCheck, expected.WithCheck)
		}

		seenPolicies[key] = true
	}
	if err := policyRows.Err(); err != nil {
		return fmt.Errorf("error reading policy rows: %w", err)
	}

	for key, exp := range expectedPolicies {
		if !seenPolicies[key] {
			return fmt.Errorf("missing required tenant isolation policy %q on table %q", exp.PolicyName, exp.TableName)
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

	// 8. Assert application role cannot claim worker jobs
	var canClaimWorkerJob bool
	err = pool.QueryRow(ctx, `
		SELECT CASE 
			WHEN has_schema_privilege(CURRENT_USER, 'queue_sec', 'USAGE') 
			THEN has_function_privilege(CURRENT_USER, 'queue_sec.claim_next_worker_job(varchar, varchar, int)', 'EXECUTE')
			ELSE false 
		END
	`).Scan(&canClaimWorkerJob)
	if err != nil {
		return fmt.Errorf("failed to check worker job claim privilege: %w", err)
	}
	if canClaimWorkerJob {
		return fmt.Errorf("application pool role %q must NOT have EXECUTE on queue_sec.claim_next_worker_job", rolName)
	}

	return nil
}

// ValidateWorkerPoolSecurity verifies that the worker pool user is the least-privilege family_worker role
// and possesses required queue and ingestion privileges, lacks sensitive auth credentials,
// and enforces fail-closed Row-Level Security.
func ValidateWorkerPoolSecurity(ctx context.Context, pool *pgxpool.Pool) error {
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
		return fmt.Errorf("failed to check worker role security attributes: %w", err)
	}

	if rolSuper || rolBypassRLS {
		return fmt.Errorf("%w: worker role %q has superuser=%v, bypassrls=%v; worker must connect as least-privilege role (family_worker)",
			ErrSuperuserProhibited, rolName, rolSuper, rolBypassRLS)
	}

	if rolName != "family_worker" {
		return fmt.Errorf("worker pool must connect as role %q, got %q", "family_worker", rolName)
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
		return fmt.Errorf("%w: worker role %q inherits membership from superuser or bypassrls roles",
			ErrSuperuserProhibited, rolName)
	}

	// 3. Privilege escalation prevention
	for _, privRole := range []string{"postgres", "family_admin"} {
		_, err := pool.Exec(ctx, fmt.Sprintf("SET ROLE %s", privRole))
		if err == nil {
			_, _ = pool.Exec(ctx, "RESET ROLE")
			return fmt.Errorf("%w: role %q can escalate privileges to %q via SET ROLE",
				ErrSuperuserProhibited, rolName, privRole)
		}
	}

	// 4. Verify worker role does not own tables in public schema
	var ownedCount int
	err = pool.QueryRow(ctx, "SELECT count(*) FROM pg_tables WHERE schemaname = 'public' AND tableowner = CURRENT_USER").Scan(&ownedCount)
	if err != nil {
		return fmt.Errorf("failed to check worker table ownership: %w", err)
	}
	if ownedCount > 0 {
		return fmt.Errorf("%w: worker role %q owns %d tables in public schema",
			ErrSuperuserProhibited, rolName, ownedCount)
	}

	// 5. Verify FORCE ROW LEVEL SECURITY on all tenant tables
	tenantTables := []string{
		"families", "family_members", "bank_connections", "raw_bank_payloads",
		"accounts", "cards", "bank_transactions", "accounting_entries",
		"categories", "categorization_rules", "transfer_matches",
		"transfer_rejections", "statement_imports", "audit_logs",
		"worker_jobs", "user_sessions", "reconciliation_discrepancies",
	}
	var unforcedCount int
	err = pool.QueryRow(ctx, `
		SELECT count(*)
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' 
		  AND c.relkind = 'r'
		  AND c.relname = ANY($1)
		  AND (NOT c.relrowsecurity OR NOT c.relforcerowsecurity)
	`, tenantTables).Scan(&unforcedCount)
	if err != nil {
		return fmt.Errorf("failed to check FORCE RLS on tenant tables: %w", err)
	}
	if unforcedCount > 0 {
		return fmt.Errorf("worker pool: %d tenant tables lack FORCE ROW LEVEL SECURITY", unforcedCount)
	}

	// 6. Assert worker role's required capabilities
	var canClaim bool
	err = pool.QueryRow(ctx, "SELECT has_function_privilege(CURRENT_USER, 'queue_sec.claim_next_worker_job(varchar, varchar, int)', 'EXECUTE')").Scan(&canClaim)
	if err != nil || !canClaim {
		return fmt.Errorf("worker role %q missing EXECUTE on queue_sec.claim_next_worker_job", rolName)
	}

	requiredWorkerJobColumns := []string{"locked_until", "locked_by", "claim_token", "scheduled_at", "state", "attempts", "last_error", "updated_at"}
	for _, col := range requiredWorkerJobColumns {
		var hasColUpdate bool
		err = pool.QueryRow(ctx, "SELECT has_column_privilege(CURRENT_USER, 'public.worker_jobs', $1, 'UPDATE')", col).Scan(&hasColUpdate)
		if err != nil || !hasColUpdate {
			return fmt.Errorf("worker role %q missing UPDATE on worker_jobs(%s)", rolName, col)
		}
	}

	requiredBankConnectionColumns := []string{"sync_cursor", "sync_version", "active_sync_token", "sync_locked_until", "status", "error_message", "last_synced_at", "updated_at"}
	for _, col := range requiredBankConnectionColumns {
		var hasColUpdate bool
		err = pool.QueryRow(ctx, "SELECT has_column_privilege(CURRENT_USER, 'public.bank_connections', $1, 'UPDATE')", col).Scan(&hasColUpdate)
		if err != nil || !hasColUpdate {
			return fmt.Errorf("worker role %q missing UPDATE on bank_connections(%s)", rolName, col)
		}
	}

	requiredInsertTables := []string{"accounts", "cards", "bank_transactions", "raw_bank_payloads", "reconciliation_discrepancies"}
	for _, tbl := range requiredInsertTables {
		var hasInsert bool
		err = pool.QueryRow(ctx, fmt.Sprintf("SELECT has_table_privilege(CURRENT_USER, 'public.%s', 'INSERT')", tbl)).Scan(&hasInsert)
		if err != nil || !hasInsert {
			return fmt.Errorf("worker role %q missing INSERT on %s", rolName, tbl)
		}
	}

	// 7. Assert worker role lacks prohibited sensitive capabilities (least privilege boundary)
	var canSelectPasswordHash, canExecAuthHelper bool
	err = pool.QueryRow(ctx, "SELECT has_column_privilege(CURRENT_USER, 'public.users', 'password_hash', 'SELECT')").Scan(&canSelectPasswordHash)
	if err != nil {
		return fmt.Errorf("failed to check password_hash privilege: %w", err)
	}
	if canSelectPasswordHash {
		return fmt.Errorf("SECURITY VIOLATION: worker role %q must NOT have SELECT on users(password_hash)", rolName)
	}

	err = pool.QueryRow(ctx, `
		SELECT CASE 
			WHEN has_schema_privilege(CURRENT_USER, 'auth_sec', 'USAGE') 
			THEN has_function_privilege(CURRENT_USER, 'auth_sec.get_current_user_family_ids(uuid)', 'EXECUTE')
			ELSE false 
		END
	`).Scan(&canExecAuthHelper)
	if err != nil {
		return fmt.Errorf("failed to check auth helper privilege: %w", err)
	}
	if canExecAuthHelper {
		return fmt.Errorf("SECURITY VIOLATION: worker role %q must NOT have EXECUTE on auth_sec.get_current_user_family_ids", rolName)
	}

	// 8. Control queries: verify fail-closed deny-all when tenant context is unset
	for _, tbl := range []string{"accounts", "cards", "bank_transactions", "worker_jobs", "reconciliation_discrepancies"} {
		var cnt int
		err = pool.QueryRow(ctx, fmt.Sprintf("SELECT count(*) FROM public.%s", tbl)).Scan(&cnt)
		if err != nil {
			return fmt.Errorf("worker security validation control query on %s failed: %w", tbl, err)
		}
		if cnt > 0 {
			return fmt.Errorf("%w: worker role %q bypassed RLS (returned %d rows from %s without tenant context)",
				ErrSuperuserProhibited, rolName, cnt, tbl)
		}
	}

	return nil
}

// NewWorkerPool initializes and validates a dedicated connection pool for family_worker.
func NewWorkerPool(ctx context.Context, connStr string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(connStr)
	if err != nil {
		return nil, fmt.Errorf("failed to parse worker connection string: %w", err)
	}

	if cfg.MaxConns <= 0 {
		cfg.MaxConns = 15
	}
	if cfg.MinConns <= 0 {
		cfg.MinConns = 2
	}
	cfg.MaxConnLifetime = 1 * time.Hour
	cfg.MaxConnIdleTime = 15 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create worker connection pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("failed to ping worker database: %w", err)
	}

	if err := ValidateWorkerPoolSecurity(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}

	return pool, nil
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

// ExpectedRLSPolicy defines the exact required configuration for an approved RLS policy on a tenant table.
type ExpectedRLSPolicy struct {
	TableName  string
	PolicyName string
	Permissive string
	Roles      []string
	Cmd        string
	Qual       string
	WithCheck  string
}

func normalizeSQLExpr(s string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
}

func getExpectedRLSPolicies() map[string]ExpectedRLSPolicy {
	policies := []ExpectedRLSPolicy{
		// 1. accounting_entries
		{
			TableName:  "accounting_entries",
			PolicyName: "rls_accounting_entries_all",
			Permissive: "PERMISSIVE",
			Roles:      []string{"family_app", "family_worker"},
			Cmd:        "ALL",
			Qual:       "(family_id = get_current_family_id())",
			WithCheck:  "(family_id = get_current_family_id())",
		},
		// 2. accounts
		{
			TableName:  "accounts",
			PolicyName: "rls_accounts_all",
			Permissive: "PERMISSIVE",
			Roles:      []string{"family_app", "family_worker"},
			Cmd:        "ALL",
			Qual:       "(family_id = get_current_family_id())",
			WithCheck:  "(family_id = get_current_family_id())",
		},
		// 3. audit_logs
		{
			TableName:  "audit_logs",
			PolicyName: "rls_audit_logs_insert",
			Permissive: "PERMISSIVE",
			Roles:      []string{"family_app", "family_worker"},
			Cmd:        "INSERT",
			Qual:       "",
			WithCheck:  "(family_id = get_current_family_id())",
		},
		{
			TableName:  "audit_logs",
			PolicyName: "rls_audit_logs_select",
			Permissive: "PERMISSIVE",
			Roles:      []string{"family_app", "family_worker"},
			Cmd:        "SELECT",
			Qual:       "(family_id = get_current_family_id())",
			WithCheck:  "",
		},
		// 4. bank_connections
		{
			TableName:  "bank_connections",
			PolicyName: "rls_bank_connections_all",
			Permissive: "PERMISSIVE",
			Roles:      []string{"family_app", "family_worker"},
			Cmd:        "ALL",
			Qual:       "(family_id = get_current_family_id())",
			WithCheck:  "(family_id = get_current_family_id())",
		},
		// 5. bank_transactions
		{
			TableName:  "bank_transactions",
			PolicyName: "rls_bank_tx_insert",
			Permissive: "PERMISSIVE",
			Roles:      []string{"family_app", "family_worker"},
			Cmd:        "INSERT",
			Qual:       "",
			WithCheck:  "(family_id = get_current_family_id())",
		},
		{
			TableName:  "bank_transactions",
			PolicyName: "rls_bank_tx_select",
			Permissive: "PERMISSIVE",
			Roles:      []string{"family_app", "family_worker"},
			Cmd:        "SELECT",
			Qual:       "(family_id = get_current_family_id())",
			WithCheck:  "",
		},
		{
			TableName:  "bank_transactions",
			PolicyName: "rls_bank_tx_update",
			Permissive: "PERMISSIVE",
			Roles:      []string{"family_worker"},
			Cmd:        "UPDATE",
			Qual:       "(family_id = get_current_family_id())",
			WithCheck:  "(family_id = get_current_family_id())",
		},
		// 6. cards
		{
			TableName:  "cards",
			PolicyName: "rls_cards_all",
			Permissive: "PERMISSIVE",
			Roles:      []string{"family_app", "family_worker"},
			Cmd:        "ALL",
			Qual:       "(family_id = get_current_family_id())",
			WithCheck:  "(family_id = get_current_family_id())",
		},
		// 7. categories
		{
			TableName:  "categories",
			PolicyName: "rls_categories_all",
			Permissive: "PERMISSIVE",
			Roles:      []string{"family_app", "family_worker"},
			Cmd:        "ALL",
			Qual:       "(family_id = get_current_family_id())",
			WithCheck:  "(family_id = get_current_family_id())",
		},
		// 8. categorization_rules
		{
			TableName:  "categorization_rules",
			PolicyName: "rls_categorization_rules_all",
			Permissive: "PERMISSIVE",
			Roles:      []string{"family_app", "family_worker"},
			Cmd:        "ALL",
			Qual:       "(family_id = get_current_family_id())",
			WithCheck:  "(family_id = get_current_family_id())",
		},
		// 9. families
		{
			TableName:  "families",
			PolicyName: "rls_families_mod",
			Permissive: "PERMISSIVE",
			Roles:      []string{"family_app"},
			Cmd:        "ALL",
			Qual:       "(id = get_current_family_id())",
			WithCheck:  "(id = get_current_family_id())",
		},
		{
			TableName:  "families",
			PolicyName: "rls_families_select",
			Permissive: "PERMISSIVE",
			Roles:      []string{"family_app", "family_worker"},
			Cmd:        "SELECT",
			Qual:       "((id = get_current_family_id()) OR ((CURRENT_USER = 'family_app'::name) AND (id IN ( SELECT auth_sec.get_current_user_family_ids() AS get_current_user_family_ids))))",
			WithCheck:  "",
		},
		// 10. family_members
		{
			TableName:  "family_members",
			PolicyName: "rls_family_members_auth_definer",
			Permissive: "PERMISSIVE",
			Roles:      []string{"family_auth_definer"},
			Cmd:        "SELECT",
			Qual:       "(user_id = (NULLIF(current_setting('app.current_user_id'::text, true), ''::text))::uuid)",
			WithCheck:  "",
		},
		{
			TableName:  "family_members",
			PolicyName: "rls_family_members_mod",
			Permissive: "PERMISSIVE",
			Roles:      []string{"family_app"},
			Cmd:        "ALL",
			Qual:       "(family_id = get_current_family_id())",
			WithCheck:  "(family_id = get_current_family_id())",
		},
		{
			TableName:  "family_members",
			PolicyName: "rls_family_members_select",
			Permissive: "PERMISSIVE",
			Roles:      []string{"family_app", "family_worker"},
			Cmd:        "SELECT",
			Qual:       "((family_id = get_current_family_id()) OR ((CURRENT_USER = 'family_app'::name) AND (family_id IN ( SELECT auth_sec.get_current_user_family_ids() AS get_current_user_family_ids))))",
			WithCheck:  "",
		},
		// 11. raw_bank_payloads
		{
			TableName:  "raw_bank_payloads",
			PolicyName: "rls_raw_payloads_insert",
			Permissive: "PERMISSIVE",
			Roles:      []string{"family_app", "family_worker"},
			Cmd:        "INSERT",
			Qual:       "",
			WithCheck:  "(family_id = get_current_family_id())",
		},
		{
			TableName:  "raw_bank_payloads",
			PolicyName: "rls_raw_payloads_select",
			Permissive: "PERMISSIVE",
			Roles:      []string{"family_app", "family_worker"},
			Cmd:        "SELECT",
			Qual:       "(family_id = get_current_family_id())",
			WithCheck:  "",
		},
		// 12. statement_imports
		{
			TableName:  "statement_imports",
			PolicyName: "rls_statement_imports_all",
			Permissive: "PERMISSIVE",
			Roles:      []string{"family_app", "family_worker"},
			Cmd:        "ALL",
			Qual:       "(family_id = get_current_family_id())",
			WithCheck:  "(family_id = get_current_family_id())",
		},
		// 13. transfer_matches
		{
			TableName:  "transfer_matches",
			PolicyName: "rls_transfer_matches_all",
			Permissive: "PERMISSIVE",
			Roles:      []string{"family_app", "family_worker"},
			Cmd:        "ALL",
			Qual:       "(family_id = get_current_family_id())",
			WithCheck:  "(family_id = get_current_family_id())",
		},
		// 14. transfer_rejections
		{
			TableName:  "transfer_rejections",
			PolicyName: "rls_transfer_rejections_insert",
			Permissive: "PERMISSIVE",
			Roles:      []string{"family_app"},
			Cmd:        "INSERT",
			Qual:       "",
			WithCheck:  "(family_id = get_current_family_id())",
		},
		{
			TableName:  "transfer_rejections",
			PolicyName: "rls_transfer_rejections_select",
			Permissive: "PERMISSIVE",
			Roles:      []string{"family_app", "family_worker"},
			Cmd:        "SELECT",
			Qual:       "(family_id = get_current_family_id())",
			WithCheck:  "",
		},
		// 15. user_sessions
		{
			TableName:  "user_sessions",
			PolicyName: "rls_user_sessions_app",
			Permissive: "PERMISSIVE",
			Roles:      []string{"family_app"},
			Cmd:        "ALL",
			Qual:       "((user_id = (NULLIF(current_setting('app.current_user_id'::text, true), ''::text))::uuid) OR (family_id = (NULLIF(current_setting('app.current_family_id'::text, true), ''::text))::uuid))",
			WithCheck:  "((user_id = (NULLIF(current_setting('app.current_user_id'::text, true), ''::text))::uuid) OR (family_id = (NULLIF(current_setting('app.current_family_id'::text, true), ''::text))::uuid))",
		},
		{
			TableName:  "user_sessions",
			PolicyName: "rls_user_sessions_verifier",
			Permissive: "PERMISSIVE",
			Roles:      []string{"family_auth_verifier"},
			Cmd:        "ALL",
			Qual:       "true",
			WithCheck:  "true",
		},
		// 16. worker_jobs
		{
			TableName:  "worker_jobs",
			PolicyName: "rls_worker_jobs_claim_select",
			Permissive: "PERMISSIVE",
			Roles:      []string{"family_queue_definer"},
			Cmd:        "SELECT",
			Qual:       "true",
			WithCheck:  "",
		},
		{
			TableName:  "worker_jobs",
			PolicyName: "rls_worker_jobs_claim_update",
			Permissive: "PERMISSIVE",
			Roles:      []string{"family_queue_definer"},
			Cmd:        "UPDATE",
			Qual:       "true",
			WithCheck:  "true",
		},
		{
			TableName:  "worker_jobs",
			PolicyName: "rls_worker_jobs_tenant",
			Permissive: "PERMISSIVE",
			Roles:      []string{"family_app", "family_worker"},
			Cmd:        "ALL",
			Qual:       "(family_id = get_current_family_id())",
			WithCheck:  "(family_id = get_current_family_id())",
		},
		// 17. reconciliation_discrepancies
		{
			TableName:  "reconciliation_discrepancies",
			PolicyName: "rls_reconciliation_discrepancies_insert",
			Permissive: "PERMISSIVE",
			Roles:      []string{"family_app", "family_worker"},
			Cmd:        "INSERT",
			Qual:       "",
			WithCheck:  "(family_id = get_current_family_id())",
		},
		{
			TableName:  "reconciliation_discrepancies",
			PolicyName: "rls_reconciliation_discrepancies_select",
			Permissive: "PERMISSIVE",
			Roles:      []string{"family_app", "family_worker"},
			Cmd:        "SELECT",
			Qual:       "(family_id = get_current_family_id())",
			WithCheck:  "",
		},
	}

	m := make(map[string]ExpectedRLSPolicy, len(policies))
	for _, p := range policies {
		sort.Strings(p.Roles)
		m[p.TableName+"."+p.PolicyName] = p
	}
	return m
}

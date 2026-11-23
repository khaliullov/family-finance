-- =============================================================================
-- INFRASTRUCTURE ROLE PROVISIONING SCRIPT FOR FAMILY FINANCE (POSTGRESQL 16+)
-- =============================================================================
-- Execution Identity: Cluster Superuser / DBA (`postgres`)
-- Purpose: Creates the 6 cluster-wide roles and grants definer memberships to family_admin.
--
-- Security Rules:
-- 1. Decoupled from per-database schema migrations (migrations NEVER mutate roles).
-- 2. Strictly idempotent: re-running provisioning NEVER overwrites existing passwords.
-- 3. Roles are provisioned with least-privilege attributes (NOSUPERUSER, NOCREATEROLE, NOBYPASSRLS).
-- 4. Initial credentials can be customized during first install or rotated via:
--      ALTER ROLE <role> WITH PASSWORD '<new_password>';
--    Re-running this provisioning script will preserve rotated credentials.
--
-- Execution:
--   psql -U postgres -h <host> -d postgres -f scripts/provision_roles.sql
-- =============================================================================

DO $$
BEGIN
    -- 1. DDL Schema Owner (family_admin)
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'family_admin') THEN
        CREATE ROLE family_admin LOGIN PASSWORD 'admin_secret_placeholder' NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;
        RAISE NOTICE 'Created role family_admin (LOGIN, NOSUPERUSER, NOCREATEROLE, NOBYPASSRLS)';
    ELSE
        RAISE NOTICE 'Role family_admin already exists; preserving existing credentials (no overwrite)';
    END IF;

    -- 2. API Runtime Connection Pool (family_app)
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'family_app') THEN
        CREATE ROLE family_app LOGIN PASSWORD 'app_secret_placeholder' NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;
        RAISE NOTICE 'Created role family_app (LOGIN, NOSUPERUSER, NOCREATEROLE, NOBYPASSRLS)';
    ELSE
        RAISE NOTICE 'Role family_app already exists; preserving existing credentials (no overwrite)';
    END IF;

    -- 3. Worker Runtime Connection Pool (family_worker)
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'family_worker') THEN
        CREATE ROLE family_worker LOGIN PASSWORD 'worker_secret_placeholder' NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;
        RAISE NOTICE 'Created role family_worker (LOGIN, NOSUPERUSER, NOCREATEROLE, NOBYPASSRLS)';
    ELSE
        RAISE NOTICE 'Role family_worker already exists; preserving existing credentials (no overwrite)';
    END IF;

    -- 4. Queue Claiming Definer (family_queue_definer)
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'family_queue_definer') THEN
        CREATE ROLE family_queue_definer NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;
        RAISE NOTICE 'Created internal role family_queue_definer (NOLOGIN)';
    ELSE
        RAISE NOTICE 'Role family_queue_definer already exists; preserving';
    END IF;

    -- 5. Membership Discovery Definer (family_auth_definer)
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'family_auth_definer') THEN
        CREATE ROLE family_auth_definer NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;
        RAISE NOTICE 'Created internal role family_auth_definer (NOLOGIN)';
    ELSE
        RAISE NOTICE 'Role family_auth_definer already exists; preserving';
    END IF;

    -- 6. Credential Verification Definer (family_auth_verifier)
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'family_auth_verifier') THEN
        CREATE ROLE family_auth_verifier NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;
        RAISE NOTICE 'Created internal role family_auth_verifier (NOLOGIN)';
    ELSE
        RAISE NOTICE 'Role family_auth_verifier already exists; preserving';
    END IF;
END $$;

-- Ensure definer memberships for function ownership administration in databases
GRANT family_queue_definer TO family_admin;
GRANT family_auth_definer TO family_admin;
GRANT family_auth_verifier TO family_admin;

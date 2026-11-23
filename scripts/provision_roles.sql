-- =============================================================================
-- INFRASTRUCTURE ROLE PROVISIONING SCRIPT FOR FAMILY FINANCE (POSTGRESQL 16+)
-- Run as PostgreSQL cluster administrator (e.g. `postgres` superuser)
-- =============================================================================
-- This script provisions the 6 cluster-wide roles required by Family Finance.
-- Roles are global to the PostgreSQL cluster; this script is decoupled from
-- per-database schema migrations.
--
-- In production, execute with custom passwords:
--   psql -v ADMIN_PASSWORD='...' -v APP_PASSWORD='...' -v WORKER_PASSWORD='...' -f scripts/provision_roles.sql
-- =============================================================================

\set ON_ERROR_STOP on

DO $$
BEGIN
    -- 1. DDL Schema Owner (family_admin)
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'family_admin') THEN
        CREATE ROLE family_admin LOGIN PASSWORD 'admin_secret_placeholder';
        RAISE NOTICE 'Created role family_admin (LOGIN)';
    ELSE
        RAISE NOTICE 'Role family_admin already exists; preserving credentials';
    END IF;

    -- 2. API Runtime Connection Pool (family_app)
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'family_app') THEN
        CREATE ROLE family_app LOGIN PASSWORD 'app_secret_placeholder';
        RAISE NOTICE 'Created role family_app (LOGIN)';
    ELSE
        RAISE NOTICE 'Role family_app already exists; preserving credentials';
    END IF;

    -- 3. Background Worker Connection Pool (family_worker)
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'family_worker') THEN
        CREATE ROLE family_worker LOGIN PASSWORD 'worker_secret_placeholder';
        RAISE NOTICE 'Created role family_worker (LOGIN)';
    ELSE
        RAISE NOTICE 'Role family_worker already exists; preserving credentials';
    END IF;

    -- 4. Queue Claiming Definer (family_queue_definer)
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'family_queue_definer') THEN
        CREATE ROLE family_queue_definer NOLOGIN;
        RAISE NOTICE 'Created internal role family_queue_definer (NOLOGIN)';
    END IF;

    -- 5. Membership Discovery Definer (family_auth_definer)
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'family_auth_definer') THEN
        CREATE ROLE family_auth_definer NOLOGIN;
        RAISE NOTICE 'Created internal role family_auth_definer (NOLOGIN)';
    END IF;

    -- 6. Credential Verification Definer (family_auth_verifier)
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'family_auth_verifier') THEN
        CREATE ROLE family_auth_verifier NOLOGIN;
        RAISE NOTICE 'Created internal role family_auth_verifier (NOLOGIN)';
    END IF;
END $$;

-- Grant definer roles to family_admin for function ownership administration in databases
GRANT family_queue_definer TO family_admin;
GRANT family_auth_definer TO family_admin;
GRANT family_auth_verifier TO family_admin;

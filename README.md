# Family Finance: Technical Architecture & System Discovery

Self-hosted family financial accounting system designed to unify accounts, cards, credit cards, and shared/individual spending across two partners without double-counting internal transfers or credit card repayments.

---

## Technical Discovery & Specification Index

This repository contains the complete **Phase 0, 0.5, 0.6, 0.7, 0.8 & 0.9** hardened technical architecture specification, database schema, API contracts, transfer matching rules, and implementation backlog:

1. **[Bank Data Ingestion & T-Bank Technical Discovery Plan](docs/discovery_tbank.md)**
   - Analysis of T-Bank Retail vs Business (T-API) reality.
   - Evaluation of 4 ingestion paths: official APIs, local browser-context capture (`tbank-capture`), aggregators, and statement imports.
   - Strict delineation of observed facts, inferred behavior, and unverified assumptions.
   - Stage 0B.1 Connector Gate criteria and live contract capture (non-blocking for core financial engine).

2. **[System Architecture Specification (`architecture.md`)](architecture.md)**
   - Go modular monolith architecture (`cmd/api`, `cmd/worker`, `internal/*`).
   - Decoupled 8-layer ingestion pipeline separating bank transports, raw DTOs, canonical events, and the core ledger.
   - Initial provider support: T-Bank, Alfa-Bank, Sberbank.
   - Transport-layer uTLS evaluation separated from API compatibility guarantees; empirical snapshots treated as versioned test fixtures.
   - Two-Layer Ledger model: Bank Evidence Layer (`bank_transactions`) and Accounting Interpretation Layer (`accounting_entries`).
   - Stable `TransactionView` identity model (`view_id`: `bt:<id>` vs `manual:<id>`).
   - Transaction-scoped tenant context via parameterized `SELECT set_config('app.current_family_id', $1, true)`.
   - Dedicated least-privilege worker job claiming role (`family_queue_definer`) and function (`claim_next_worker_job`) with lease timeout and zero GUC bypass.
   - Non-recursive, anti-enumeration membership discovery role (`family_auth_definer`) and parameterless helper (`public.get_current_user_family_ids`).
   - User credential isolation role (`family_auth_verifier`) protecting `users.password_hash`.
   - Bank evidence immutability and monotonic lifecycle state machine (`pending` -> `posted`/`cancelled`).
   - Raw payload immutability and append-only audit logging.
   - Tenant immutability across all 14 tenant-owned tables and table-owner bypass prevention (`FORCE ROW LEVEL SECURITY`).
   - Transfer match endpoint immutability (`trg_immutability_transfer_endpoints`).
   - Balance unknown/null semantics, observation timestamps, and sourcing rules.
   - Non-destructive transfer matching representation and 5-step atomic rejection transaction with `reason` and `updated_at`.
   - Provider-independent `BankConnector` Go interface specification.

3. **[PostgreSQL Schema & ER Design (`database.md`)](database.md)**
   - Minimum supported database: **PostgreSQL 16+**.
   - 6-role PostgreSQL access model: `family_admin`, `family_app`, `family_worker`, `family_queue_definer`, `family_auth_definer`, `family_auth_verifier`.
   - Dedicated security schemas: `queue_sec` and `auth_sec` owned by `family_admin` isolating privileged `SECURITY DEFINER` functions without granting broad `CREATE` privileges on `public`.
   - Blanket runtime grants eliminated; explicit column and operation grants enforced across all tables.
   - Entity-Relationship diagram and table definitions.
   - Composite foreign keys with safe column-level `ON DELETE SET NULL (nullable_column)`.
   - User membership ownership enforced via composite foreign keys to `family_members(family_id, user_id)`.
   - Strict deferred constraint triggers validating split transaction sums and invariants (count=0 rejection, count=1 requires `is_split=false`, count>=2 requires `is_split=true`).
   - Bank evidence monotonic state machine and immutability triggers (`trg_bank_tx_immutable`, `trg_bank_tx_no_delete`): Category 1 immutable facts (19 columns) strictly protected; terminal `posted` and `cancelled` states permanent; settlement updates permitted exclusively during `pending` $\rightarrow$ `posted`.
   - Complete 24-column bank transaction mutability audit matrix.
   - Registration bootstrap sequence with client-pre-generated UUIDs executing under `FORCE ROW LEVEL SECURITY` without policy bypasses.
   - Raw payload immutability trigger (`trg_raw_payloads_immutable`) and audit log append-only trigger (`trg_audit_logs_append_only`).
   - Transfer match entry ownership trigger (`trg_validate_accounting_entry_transfer_match`) and endpoint immutability trigger (`trg_immutability_transfer_endpoints`).
   - Explicit PostgreSQL Row-Level Security (RLS) policies (`SELECT`, `INSERT`, `UPDATE`) with `FORCE ROW LEVEL SECURITY`.
   - Dedicated `queue_sec.claim_next_worker_job()` function (`SECURITY DEFINER`) with lease recovery for cross-tenant job claiming without RLS bypass.
   - Complete 14-table tenant immutability audit matrix and complete table-level privilege/RLS matrix.
   - Analytical views separating cash flows, true family consumption, and debt balances.
   - Permanent reproducible security and integrity test suite committed in `tests/security/`.

4. **[OpenAPI 3.1 Specification (`api.yaml`)](api.yaml)**
   - 100% compliant and validated with `openapi-spec-validator 0.9.0`.
   - RFC 7807 Problem Details error conventions.
   - Two-Layer schema design: `BankTransaction`, `AccountingEntry`, and aggregated `TransactionView` with stable identity.
   - Endpoints for Auth (Bootstrap), Connections (Pairing handshake), Accounts, Cards, Transactions, Splits, Categories, Rules, Transfers (with `reason` in `RejectTransferRequest`), Statement Imports, and Analytics.

5. **[Conservative Transfer Matcher Specification](docs/transfer_matcher.md)**
   - Multi-tier matching heuristics: Tier 1 exact (requires mandatory reciprocal semantic transfer evidence), Tier 2 probable, Tier 3 manual.
   - Non-destructive transfer matching architecture preserving underlying categories and comments.
   - Symmetric transfer rejection index (`LEAST(source, dest)`, `GREATEST(source, dest)`) preventing reverse proposals.
   - 5-step atomic rejection transaction (persisting `reason` and updating `updated_at`) and transfer match entry ownership and endpoint immutability triggers.
   - Credit card repayment and debt servicing vs true expense modeling.
   - Comprehensive test matrix (TC-TR-01 through TC-TR-10).

6. **[Staged Implementation Backlog for Coding Agents](docs/backlog.md)**
   - Stage 0A: Core Financial Foundation (BLOCKING GATE: Formal specifications of money primitives, two-layer ledger, lifecycle state machine, least-privilege security design, and executable prototype schema spike).
   - Stage 0B: Bank Integration Readiness (NON-BLOCKING / Deferred: T-Bank, Alfa-Bank, Sberbank connectors, and CSV/XLSX statement parsers).
   - Stage 1: Core Domain, Migrations & Multi-Tenant Database (COMPLETED: production Go domain models, database migrations, connection pool with RLS context, auth, accounts, cards, and API operational bootstrap).
   - Stage 2: Ingestion Engine, Deduplication & Job Worker (`claim_next_worker_job`).
   - Stage 3: Categorization Engine & Conservative Transfer Matcher (TC-TR-01..10).
   - Stage 4: Analytics Engine, REST API & Deployment.

---

## Development & Operational Guide

### Prerequisites
- **Go**: 1.22+
- **PostgreSQL**: 16+
- **Python**: 3.10+ (for running the security test suite)

### 1. Database Provisioning & Migrations
```bash
# 1. Provision cluster roles (run once as postgres superuser)
psql -U postgres -f scripts/provision_roles.sql

# 2. Apply migrations as family_admin
migrate -path migrations/ -database "postgres://family_admin:admin_secret_placeholder@localhost:5432/family_finance?sslmode=disable" up
```

### 2. Running the API Server
```bash
# Start API server connecting as least-privilege family_app role
export PGUSER="family_app"
export PGPASSWORD="app_secret_placeholder"
export PGDATABASE="family_finance"
export JWT_SECRET="your-32-byte-or-longer-production-secret-key"
export APP_ENV="development"
export PORT="8080"

go run ./cmd/api
```

### 3. Verification Test Suites
```bash
# Run full Go unit & integration test suite with data race detection
go test -p 1 -race -count=1 ./...

# Run comprehensive 70-assertion PostgreSQL security & integrity suite
python3 tests/security/test_security_suite.py
```

### 4. API Endpoints Overview
- **Operational Probes**:
  - `GET /health`: Liveness probe (200 OK `{"status": "UP"}`)
  - `GET /ready`: Readiness probe verifying live database pool connectivity (200 OK or 503 Service Unavailable)
- **Authentication & Sessions (`/api/v1/auth` & `/auth`)**:
  - `POST /register`: Atomic user, family, owner, and default categories bootstrap
  - `POST /login`: Argon2id verification with anti-enumeration protection
  - `POST /refresh`: Atomic refresh token rotation and replay revocation
  - `POST /logout`: Invalidate session chain
  - `GET /me`: Authenticated profile and active family memberships
  - `GET /sessions`: List active sessions
  - `DELETE /sessions/{id}`: Terminate specific session
- **Accounts & Cards (`/api/v1/accounts` & `/api/v1/cards`)**:
  - `GET /accounts`: List family accounts (optional `include_archived=true`)
  - `POST /accounts`: Create account (`checking`, `credit_card`, `savings`, `deposit`, `cash`)
  - `GET /accounts/{id}`: Retrieve account details
  - `PATCH /accounts/{id}`: Update account properties (name)
  - `POST /accounts/{id}/archive` (or `DELETE`): Soft-delete / archive account
  - `GET /accounts/{id}/balance`: Epistemic balance breakdown (current, holds, calculated available)
  - `GET /accounts/{id}/cards`: List cards linked to account
  - `POST /accounts/{id}/cards`: Create card linked to account (enforces masked PAN)
  - `GET /cards/{id}`: Retrieve card details
  - `PATCH /cards/{id}`: Update card (display name, status, primary flag)
  - `POST /cards/{id}/close`: Close card and atomically clear primary flag

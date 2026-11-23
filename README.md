# Family Finance: Technical Architecture & System Discovery

Self-hosted family financial accounting system designed to unify accounts, cards, credit cards, and shared/individual spending across two partners without double-counting internal transfers or credit card repayments.

---

## Technical Discovery & Specification Index

This repository contains the complete **Phase 0, 0.5, 0.6 & 0.7** hardened technical architecture specification, database schema, API contracts, transfer matching rules, and implementation backlog:

1. **[Bank Data Ingestion & T-Bank Technical Discovery Plan](docs/discovery_tbank.md)**
   - Analysis of T-Bank Retail vs Business (T-API) reality.
   - Evaluation of 4 ingestion paths: official APIs, local browser-context capture (`tbank-capture`), aggregators, and statement imports.
   - Strict delineation of observed facts, inferred behavior, and unverified assumptions.
   - Stage 0 Go / No-Go Gate criteria and blocking live contract capture.

2. **[System Architecture Specification (`architecture.md`)](architecture.md)**
   - Go modular monolith architecture (`cmd/api`, `cmd/worker`, `internal/*`).
   - Two-Layer Ledger model: Bank Evidence Layer (`bank_transactions`) and Accounting Interpretation Layer (`accounting_entries`).
   - Stable `TransactionView` identity model (`view_id`: `bt:<id>` vs `manual:<id>`).
   - Transaction-scoped tenant context via parameterized `SELECT set_config('app.current_family_id', $1, true)`.
   - Dedicated least-privilege worker job claiming function (`claim_next_worker_job`) with zero GUC bypass.
   - User membership discovery and RLS recursion avoidance (`public.get_user_family_ids`).
   - Tenant immutability (`trg_enforce_tenant_immutability`) and table-owner bypass prevention (`FORCE ROW LEVEL SECURITY`).
   - Balance unknown/null semantics, observation timestamps, and sourcing rules.
   - Non-destructive transfer matching representation and 5-step atomic rejection transaction.
   - `BankConnector` Go interface specification.

3. **[PostgreSQL Schema & ER Design (`database.md`)](database.md)**
   - Minimum supported database: **PostgreSQL 16+**.
   - Entity-Relationship diagram and table definitions.
   - Composite foreign keys with safe column-level `ON DELETE SET NULL (nullable_column)`.
   - User membership ownership enforced via composite foreign keys to `family_members(family_id, user_id)`.
   - Strict deferred constraint triggers validating split transaction sums and invariants (count=0 rejection, count=1 requires `is_split=false`, count>=2 requires `is_split=true`).
   - Deferred validation for newly inserted `bank_transactions` ensuring accounting entry presence at commit.
   - Transfer match entry ownership trigger (`trg_validate_accounting_entry_transfer_match`).
   - Explicit PostgreSQL Row-Level Security (RLS) policies (`USING` and `WITH CHECK`) with `FORCE ROW LEVEL SECURITY`.
   - Dedicated `claim_next_worker_job()` function (`SECURITY DEFINER`) for cross-tenant job claiming without RLS bypass.
   - Analytical views separating cash flows, true family consumption, and debt balances.

4. **[OpenAPI 3.1 Specification (`api.yaml`)](api.yaml)**
   - 100% compliant and validated with `openapi-spec-validator 0.9.0`.
   - RFC 7807 Problem Details error conventions.
   - Two-Layer schema design: `BankTransaction`, `AccountingEntry`, and aggregated `TransactionView` with stable identity.
   - Endpoints for Auth (Bootstrap), Connections (Pairing handshake), Accounts, Cards, Transactions, Splits, Categories, Rules, Transfers, Statement Imports, and Analytics.

5. **[Conservative Transfer Matcher Specification](docs/transfer_matcher.md)**
   - Multi-tier matching heuristics: Tier 1 exact (requires mandatory reciprocal semantic transfer evidence), Tier 2 probable, Tier 3 manual.
   - Non-destructive transfer matching architecture preserving underlying categories and comments.
   - Symmetric transfer rejection index (`LEAST(source, dest)`, `GREATEST(source, dest)`) preventing reverse proposals.
   - 5-step atomic rejection transaction and transfer match entry ownership trigger.
   - Credit card repayment and debt servicing vs true expense modeling.
   - Comprehensive test matrix (TC-TR-01 through TC-TR-09).

6. **[Staged Implementation Backlog for Coding Agents](docs/backlog.md)**
   - Stage 0: Live Contract Capture (Blocking Gate) & Baseline Importers.
   - Stage 1: Core Domain, Migrations, Composite Foreign Keys, Trigger Integrity & Tenant Isolation Proofs (RLS-01..09).
   - Stage 2: Ingestion Engine, Deduplication & Job Worker (`claim_next_worker_job`).
   - Stage 3: Categorization Engine & Conservative Transfer Matcher (TC-TR-01..09).
   - Stage 4: Analytics Engine, REST API & Deployment.

# Family Finance: Technical Architecture & System Discovery

Self-hosted family financial accounting system designed to unify accounts, cards, credit cards, and shared/individual spending across two partners without double-counting internal transfers or credit card repayments.

---

## Technical Discovery & Specification Index

This repository contains the complete **Phase 0, 0.5 & 0.6** hardened technical architecture specification, database schema, API contracts, transfer matching rules, and implementation backlog:

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
   - Complete removal of worker RLS bypass GUCs (worker executes `WithFamilyTx(job.family_id)`).
   - Balance unknown/null semantics, observation timestamps, and sourcing rules.
   - Non-destructive transfer matching representation.
   - `BankConnector` Go interface specification.

3. **[PostgreSQL Schema & ER Design (`database.md`)](database.md)**
   - Minimum supported database: **PostgreSQL 16+**.
   - Entity-Relationship diagram and table definitions.
   - Composite foreign keys with safe column-level `ON DELETE SET NULL (nullable_column)`.
   - User membership ownership enforced via composite foreign keys to `family_members(family_id, user_id)`.
   - Complete deferred constraint triggers validating split transaction sums on INSERT, UPDATE, DELETE, parent reassignment, and bank amount corrections.
   - Explicit PostgreSQL Row-Level Security (RLS) policies (`USING` and `WITH CHECK`) across all tenant tables.
   - Analytical views separating cash flows, true family consumption, and debt balances.

4. **[OpenAPI 3.1 Specification (`api.yaml`)](api.yaml)**
   - RFC 7807 Problem Details error conventions.
   - Two-Layer schema design: `BankTransaction`, `AccountingEntry`, and aggregated `TransactionView` with stable identity.
   - Endpoints for Auth (Bootstrap), Connections (Pairing handshake), Accounts, Cards, Transactions, Splits, Categories, Rules, Transfers, Statement Imports, and Analytics.

5. **[Conservative Transfer Matcher Specification](docs/transfer_matcher.md)**
   - Multi-tier matching heuristics: Tier 1 exact (requires mandatory reciprocal semantic transfer evidence), Tier 2 probable, Tier 3 manual.
   - Non-destructive transfer matching architecture preserving underlying categories and comments.
   - Partial unique indexes and `transfer_rejections` table preventing rematching of rejected pairs.
   - Credit card repayment and debt servicing vs true expense modeling.
   - Comprehensive test matrix (TC-TR-01 through TC-TR-07).

6. **[Staged Implementation Backlog for Coding Agents](docs/backlog.md)**
   - Stage 0: Live Contract Capture (Blocking Gate) & Baseline Importers.
   - Stage 1: Core Domain, Migrations, Composite Foreign Keys, Trigger Integrity & Tenant Isolation Proofs (RLS-01..06).
   - Stage 2: Ingestion Engine, Deduplication & Job Worker.
   - Stage 3: Categorization Engine & Conservative Transfer Matcher.
   - Stage 4: Analytics Engine, REST API & Deployment.

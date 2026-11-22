# Family Finance: Technical Architecture & System Discovery

Self-hosted family financial accounting system designed to unify accounts, cards, credit cards, and shared/individual spending across two partners without double-counting internal transfers or credit card repayments.

---

## Technical Discovery & Specification Index

This repository contains the complete **Phase 0 & Phase 0.5** hardened technical architecture specification, database schema, API contracts, transfer matching rules, and implementation backlog:

1. **[Bank Data Ingestion & T-Bank Technical Discovery Plan](docs/discovery_tbank.md)**
   - Analysis of T-Bank Retail vs Business (T-API) reality.
   - Evaluation of 4 ingestion paths: official APIs, local browser-context capture (`tbank-capture`), aggregators, and statement imports.
   - Strict delineation of observed facts, inferred behavior, and unverified assumptions.
   - Stage 0 Go / No-Go Gate criteria and blocking live contract capture.

2. **[System Architecture Specification (`architecture.md`)](architecture.md)**
   - Go modular monolith architecture (`cmd/api`, `cmd/worker`, `internal/*`).
   - Two-Layer Ledger model: Bank Evidence Layer (`bank_transactions`) and Accounting Interpretation Layer (`accounting_entries`).
   - Transaction-scoped tenant context implementation (`WithFamilyTx`) inside PostgreSQL transactions.
   - Exact balance definitions and sign conventions (no undefined overdraft).
   - Sidecar security protocol (single-use pairing secret exchange, AES-256-GCM envelope encryption).
   - `BankConnector` Go interface specification.

3. **[PostgreSQL Schema & ER Design (`database.md`)](database.md)**
   - Entity-Relationship diagram and table definitions.
   - Composite foreign keys `(family_id, target_id)` enforcing cross-entity family ownership.
   - Explicit PostgreSQL Row-Level Security (RLS) policies (`USING` and `WITH CHECK`) across all tenant tables.
   - Deferred constraint trigger validating split transaction sums.
   - Analytical views separating cash flows, true family consumption, and debt balances.

4. **[OpenAPI 3.1 Specification (`api.yaml`)](api.yaml)**
   - RFC 7807 Problem Details error conventions.
   - Two-Layer schema design: `BankTransaction`, `AccountingEntry`, and aggregated `TransactionView`.
   - Endpoints for Auth (Bootstrap), Connections (Pairing handshake), Accounts, Cards, Transactions, Splits, Categories, Rules, Transfers, Statement Imports, and Analytics.

5. **[Conservative Transfer Matcher Specification](docs/transfer_matcher.md)**
   - Multi-tier matching heuristics: Tier 1 exact (requires mandatory reciprocal semantic transfer evidence), Tier 2 probable, Tier 3 manual.
   - Prohibition of auto-matching based solely on MCC codes or amounts/timestamps.
   - Partial unique indexes and `transfer_rejections` table preventing rematching of rejected pairs.
   - Credit card repayment and debt servicing vs true expense modeling.
   - Comprehensive test matrix (TC-TR-01 through TC-TR-07).

6. **[Staged Implementation Backlog for Coding Agents](docs/backlog.md)**
   - Stage 0: Live Contract Capture (Blocking Gate) & Baseline Importers.
   - Stage 1: Core Domain, Migrations, Composite Foreign Keys & Tenant Isolation Proofs.
   - Stage 2: Ingestion Engine, Deduplication & Job Worker.
   - Stage 3: Categorization Engine & Conservative Transfer Matcher.
   - Stage 4: Analytics Engine, REST API & Deployment.

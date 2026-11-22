# Family Finance: Technical Architecture & System Discovery

Self-hosted family financial accounting system designed to unify accounts, cards, credit cards, and shared/individual spending across two partners without double-counting internal transfers or credit card repayments.

---

## Technical Discovery & Specification Index

This repository contains the complete **Phase 0 / Step 11** technical architecture specification, database schema, API contracts, transfer matching rules, and implementation backlog:

1. **[Bank Data Ingestion & T-Bank Technical Discovery Plan](docs/discovery_tbank.md)**
   - Analysis of T-Bank Retail vs Business (T-API) reality.
   - Evaluation of 4 ingestion paths: official APIs, local browser-context capture (`tbank-capture`), aggregators, and statement imports.
   - Multi-partner authentication topology and consent flows.
   - Stage 0 Go / No-Go Gate criteria.

2. **[System Architecture Specification (`architecture.md`)](architecture.md)**
   - Go modular monolith architecture (`cmd/api`, `cmd/worker`, `internal/*`).
   - Module boundaries, responsibilities, and data flow pipelines.
   - Sync cadence, sliding re-check windows, and PostgreSQL-backed job queue.
   - Security controls: RLS tenant isolation, AES-256-GCM token encryption, PAN masking, and audit logging.
   - `BankConnector` Go interface specification.

3. **[PostgreSQL Schema & ER Design (`database.md`)](database.md)**
   - Entity-Relationship diagram and table definitions.
   - Strict `BIGINT` minor currency units representation (zero `float64`).
   - Natural deduplication keys & synthetic fingerprinting fallback.
   - PostgreSQL Row-Level Security (RLS) policies for `family_id` isolation.
   - Analytical views separating cash flows, true family consumption, and debt balances.

4. **[OpenAPI 3.1 Specification (`api.yaml`)](api.yaml)**
   - RFC 7807 Problem Details error conventions.
   - Endpoints for Auth, Families, Bank Connections, Accounts, Cards, Transactions, Categories, Categorization Rules, Transfer Matches, Statement Imports, and Analytics.

5. **[Deduplication & Transfer Matcher Specification](docs/transfer_matcher.md)**
   - Transaction deduplication and synthetic fingerprint hashing.
   - Transaction lifecycle state machine (`pending` -> `posted` -> `cancelled` / `refunded`).
   - Multi-tier transfer matching engine (Tier 1 exact, Tier 2 probable, Tier 3 manual).
   - Credit card repayment and debt servicing vs true expense modeling.

6. **[Staged Implementation Backlog for Coding Agents](docs/backlog.md)**
   - Stage 0: Discovery Validation & Baseline Importers.
   - Stage 1: Core Domain, Migrations & Multi-Tenant Database.
   - Stage 2: Ingestion Engine, Deduplication & Job Worker.
   - Stage 3: Categorization Engine & Multi-Tier Transfer Matcher.
   - Stage 4: Analytics Engine, REST API & Deployment.

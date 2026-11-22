# Staged Implementation Backlog for Coding Agents

This backlog defines the sequential, test-driven implementation plan for **Family Finance**. Each stage is gated: all unit and integration tests of the preceding stage must pass before starting the next.

---

## Stage 0: Bank Discovery Validation & Baseline Importers (Stage 0 Gate)

> **Objective:** Eliminate bank integration risks before building application layers, and deliver 100% reliable CSV/XLSX statement imports as an immediate working baseline.

### Task 0.1: T-Bank Request Contract Discovery & Validation
- **Scope:** Inspect and extend `tbank-capture` network discovery to pinpoint the query parameters, pagination, and date formatting for:
  - `GET /mybank/api/operations/timeline/public/legacy/v1/operations`
  - `GET /api/common/v1/accounts_light_ib`
  - `GET /api/common/v1/statements`
- **Inputs:** Discovery session `sess-20261122T145129Z-5b208342`, Chromium request interception logs.
- **Outputs:** `connector/tbank/contract.go` containing verified parameter constants and JSON structs for tolerant decoding.
- **Unit & Integration Tests:**
  - Mock HTTP server testing all 9 known transaction JSON response variations.
  - Pagination boundary tests (first page, middle page, terminal page, empty result).
  - Detection and handling of session expiry (HTTP 401/403).
- **Acceptance Criteria:** Client can fetch 90 days of transactions through a local mock browser context without runtime decoding errors.

### Task 0.2: Multi-Bank CSV/XLSX Statement Parser
- **Scope:** Implement streaming statement parsers for T-Bank (CSV/XLSX), Sberbank (XLSX), Alfa-Bank (XLSX), and a Generic RFC 4180 CSV format.
- **Files:** `internal/importer/tbank.go`, `internal/importer/sber.go`, `internal/importer/generic.go`.
- **Unit Tests:**
  - Parse sample statement files with varied date formats (`DD.MM.YYYY`, `YYYY-MM-DD`, `DD.MM.YYYY HH:MM:SS`).
  - Correct parsing of positive/negative amounts, currency symbols, and comma decimal separators (`"1 250,50"` -> `125050` minor units).
  - Error reporting on corrupted or header-mismatched files.
- **Acceptance Criteria:** Ingesting 1,000 statement rows executes in under 50ms with zero floating-point conversions.

### Task 0.3: Minor Units Money Primitive & Value Object
- **Scope:** Create a core `money.Money` value object in Go supporting exact arithmetic, formatting, and currency validation.
- **Files:** `internal/domain/money.go`, `internal/domain/money_test.go`.
- **Unit Tests:**
  - Addition, subtraction, negation, and percentage calculation with integer rounding.
  - Prohibition of mixed-currency arithmetic without explicit conversion rates.
  - JSON marshaling/unmarshaling to/from integer minor units.
- **Acceptance Criteria:** 100% unit test coverage; no `float32`/`float64` types in any monetary calculation.

---

## Stage 1: Core Domain, Migrations & Multi-Tenant Database

> **Objective:** Establish the PostgreSQL relational schema, tenant isolation (`family_id`), and authentication foundation.

### Task 1.1: Database Migrations & Seed Data
- **Scope:** Write numbered SQL migrations using `golang-migrate` for all schemas defined in `database.md`. Include seed data for standard household categories (Food, Housing, Transport, Health, Child, Entertainment, Finance, Income, Transfers).
- **Files:** `migrations/000001_initial_schema.up.sql`, `migrations/000002_seed_categories.up.sql`.
- **Integration Tests:**
  - `migrate up` and `migrate down` test on a clean PostgreSQL 16 container.
  - Verification of foreign key cascades and check constraints.
- **Acceptance Criteria:** Clean migration application without warnings or syntax errors.

### Task 1.2: PostgreSQL Connection Pool & RLS Middleware
- **Scope:** Set up `jackc/pgx/v5` pool with custom connection acquire hook to inject `SET LOCAL app.current_family_id = ?`.
- **Files:** `internal/database/pool.go`, `internal/database/rls.go`.
- **Integration Tests:**
  - Create Family A and Family B; verify Family A cannot read or write Family B's accounts even when executing raw `SELECT * FROM accounts`.
- **Acceptance Criteria:** RLS enforcement verified across all financial tables.

### Task 1.3: User Auth & Family Membership Module
- **Scope:** Implement Argon2id password hashing, JWT issue/verify, user registration, login, and partner invitation flow.
- **Files:** `internal/auth/service.go`, `internal/auth/jwt.go`, `internal/auth/handler.go`.
- **Unit & Integration Tests:**
  - Password strength validation and Argon2id hash verification.
  - Expired token rejection and refresh token rotation.
  - Multi-partner access: two users successfully belonging to the same `family_id`.
- **Acceptance Criteria:** Secure auth endpoints matching `api.yaml`.

### Task 1.4: Accounts & Cards Management Module
- **Scope:** CRUD operations for checking, credit, savings, and manual cash accounts. Support linking multiple cards (`cards` table) to a single account.
- **Files:** `internal/account/service.go`, `internal/account/repository.go`.
- **Unit Tests:**
  - Linking primary and virtual cards to one checking account.
  - Correct calculation of available balance for credit cards: $\text{credit\_limit} - \text{debt\_balance}$.
- **Acceptance Criteria:** Adding transactions through different cards correctly updates the single shared account balance.

---

## Stage 2: Ingestion Engine, Deduplication & Job Worker

> **Objective:** Build the resilient data ingestion pipeline, deduplication engine, and PostgreSQL-backed async job queue.

### Task 2.1: `BankConnector` Abstraction & T-Bank Client
- **Scope:** Implement the `BankConnector` interface (`FetchAccounts`, `FetchTransactions`) using the verified contracts from Stage 0.
- **Files:** `internal/connector/connector.go`, `internal/connector/tbank/client.go`.
- **Integration Tests:**
  - End-to-end sync simulation against recorded discovery fixtures.
  - Error classification: distinguish temporary network failures from permanent auth expirations.
- **Acceptance Criteria:** Connector cleanly transforms bank JSON payloads into normalized domain DTOs.

### Task 2.2: Ingestion Pipeline & Raw Payload Store
- **Scope:** Implement atomic transaction ingestion. Save exact raw JSON into `raw_bank_payloads` and execute natural key / synthetic fingerprint deduplication.
- **Files:** `internal/transaction/ingest.go`, `internal/transaction/dedup.go`.
- **Unit & Integration Tests:**
  - Ingest identical batch twice: verify exactly 0 duplicate rows in `transactions`.
  - Synthetic fingerprint hash handles statement rows with minor whitespace/case discrepancies.
- **Acceptance Criteria:** Idempotency verified under concurrent ingestion runs.

### Task 2.3: Lifecycle State Transition Manager
- **Scope:** Transition transactions from `pending` (hold) to `posted` (settled), `cancelled`, or `refunded`. Update debit amounts and exchange rates upon final settlement.
- **Files:** `internal/transaction/lifecycle.go`.
- **Unit Tests:**
  - Card hold of 1,000 RUB matched with subsequent settled transaction of 1,050 RUB: status becomes `posted` with settled amount 1,050 RUB and single record preserved.
  - Merchant refund creates a positive-amount record linked to original category.
- **Acceptance Criteria:** Zero double-counting of authorizations and clearing settlements.

### Task 2.4: PostgreSQL-Backed Job Queue (`worker_jobs`)
- **Scope:** Implement transactional background worker using `FOR UPDATE SKIP LOCKED`. Support retries, exponential backoff, and scheduled sync jobs.
- **Files:** `internal/queue/worker.go`, `internal/queue/job.go`.
- **Unit & Integration Tests:**
  - Multiple concurrent worker instances process distinct jobs without race conditions.
  - Failed job increments attempt counter and applies backoff.
- **Acceptance Criteria:** Resilient asynchronous job processing with zero Redis dependency.

---

## Stage 3: Categorization Engine & Multi-Tier Transfer Matcher

> **Objective:** Deliver automated classification rules and pairing of internal account movements and credit card repayments.

### Task 3.1: Deterministic Categorization Rule Engine
- **Scope:** Evaluate incoming transactions against the ordered rule hierarchy:
  1. User-specific merchant rule
  2. Saved counterparty rule
  3. MCC code match
  4. Name regex/keyword match
  5. Fallback: "Uncategorized"
- **Files:** `internal/categorizer/engine.go`, `internal/categorizer/rules.go`.
- **Unit Tests:**
  - Priority ordering test: ensure higher priority user rule overrides MCC code.
  - Substring and regex pattern matching for Russian merchant descriptions (e.g., `*YANDEX.LAVKA*` -> "Groceries").
- **Acceptance Criteria:** Categorization of 1,000 transactions executes in under 10ms.

### Task 3.2: Multi-Tier Transfer Matcher Engine
- **Scope:** Implement pairing algorithm between reciprocal outflows and inflows:
  - **Tier 1:** Same amount, opposite signs, $\Delta t \le 120\text{ min}$, internal accounts $\rightarrow$ Auto-confirm.
  - **Tier 2:** Fee variance or $\Delta t \le 72\text{ hours}$ $\rightarrow$ Suggest for 1-click review.
  - **Tier 3:** Manual user pairing modal.
- **Files:** `internal/transfermatch/matcher.go`, `internal/transfermatch/rules.go`.
- **Unit Tests:**
  - Inter-bank transfer via SBP between Partner A and Partner B matches as Tier 1.
  - Transfer with 50 RUB fee matches as Tier 2 with fee extracted.
  - Disconnected accounts or mismatched amounts routed to manual pool.
- **Acceptance Criteria:** Matched transfers automatically set `is_internal_transfer = TRUE`, removing them from gross expense/income views.

### Task 3.3: Credit Card Repayment Specializer
- **Scope:** Specialized transfer matching where destination account is of type `credit_card`.
- **Files:** `internal/transfermatch/credit_card.go`.
- **Unit Tests:**
  - Repaying credit card from debit account decreases debit balance, decreases credit card debt balance, and produces **0** family expense.
  - Annual card fee or interest fee classified as "Finance: Credit Card Fees" (real expense).
- **Acceptance Criteria:** Total debt and assets update accurately without distorting monthly budget burn.

---

## Stage 4: Analytics Engine, REST API & Deployment

> **Objective:** Expose high-speed analytics queries, REST endpoints according to OpenAPI 3.1, audit logging, and Docker Compose deployment.

### Task 4.1: Cash Flows, Real Expenses & Net Worth Analytics
- **Scope:** High-performance SQL queries calculating:
  - Operating Cash Flows (Inflows vs Outflows).
  - True Family Consumption (Expenses net of refunds and internal transfers).
  - Net Worth & Debt Positions.
  - Attribution breakdown: Partner A spend vs Partner B spend vs Shared Household.
- **Files:** `internal/analytics/service.go`, `internal/analytics/repository.go`.
- **Integration Tests:**
  - Test scenario with 100 transactions (including refunds, transfers, and credit card debt repayments): verify mathematical equality between cash change and $\text{Income} - \text{Expenses} + \Delta\text{Debt}$.
- **Acceptance Criteria:** Summary analytics queries return in under 20ms on 100k transactions.

### Task 4.2: REST API Endpoints & Chi Router Integration
- **Scope:** Complete HTTP handlers for all endpoints specified in `api.yaml`.
- **Files:** `cmd/api/main.go`, `internal/api/router.go`, `internal/api/handlers/*.go`.
- **Integration Tests:**
  - End-to-end API testing covering Auth, Accounts, Transactions, Transfers, Categories, and Analytics.
  - RFC 7807 error format validation on bad inputs.
- **Acceptance Criteria:** 100% compliance with `api.yaml`.

### Task 4.3: Audit Trail & Data Sanitation Middleware
- **Scope:** Record all manual category updates, transfer confirmations, and balance reconciliations in `audit_logs`. Implement logger middleware redacting card PANs, auth tokens, and passwords.
- **Files:** `internal/audit/logger.go`, `internal/api/middleware/audit.go`.
- **Unit Tests:**
  - Inspect log output during auth and transaction endpoints: confirm no PANs, tokens, or credentials appear in logs.
- **Acceptance Criteria:** Complete change audit trail visible via API.

### Task 4.4: Docker Compose, Caddyfile & Deployment Automation
- **Scope:** Production-ready `Dockerfile` (multi-stage Go build) and `docker-compose.yml` orchestrating API, Worker, PostgreSQL 16, and Caddy with automated HTTPS.
- **Files:** `Dockerfile`, `docker-compose.yml`, `Caddyfile`, `scripts/backup.sh`.
- **Integration Tests:**
  - `docker compose up -d` boots all services with clean healthchecks.
  - Automated database backup script verifies dump and restore integrity.
- **Acceptance Criteria:** Single-command local deployment operational.

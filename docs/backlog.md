# Staged Implementation Backlog for Coding Agents

This backlog defines the sequential, test-driven implementation plan for **Family Finance**. Each stage is gated: all unit and integration tests of the preceding stage must pass before starting the next.

---

## Stage 0A: Core Financial Foundation — BLOCKING

> **Objective:** Establish the self-contained, rigorously verified mathematical and domain foundation of the core financial system, completely decoupled from external bank network discovery and file importers. All Stage 0A requirements are **BLOCKING** prerequisites for Stage 1.

### Task 0A.1: Money Primitive & Value Object
- **Scope:** Implement and test a canonical `money.Money` value object in Go supporting exact arithmetic, formatting, and currency compatibility validation.
- **Files:** `internal/domain/money.go`, `internal/domain/money_test.go`.
- **Requirements & Unit Tests:**
  - Integer minor units (`int64` minor units: kopeks, cents).
  - ISO 4217 uppercase 3-letter currency code validation (`RUB`, `USD`, `EUR`).
  - Strict prohibition of floating-point arithmetic (`float32`/`float64`).
  - Explicit currency compatibility checks (mixed-currency addition/subtraction returns explicit error).
  - Deterministic rounding rules for multiplication/division (e.g. half-even bankers rounding).
  - Integer overflow and underflow protection on bounds operations.
  - Serialization and deserialization tests (JSON, database driver `Valuer`/`Scanner`).
- **Acceptance Criteria:** 100% unit test coverage; mathematical invariance proven under boundary and negative test vectors.

### Task 0A.2: Canonical Bank Transaction Model
- **Scope:** Specify and test a provider-independent bank transaction representation in the domain layer.
- **Files:** `internal/domain/transaction.go`, `internal/domain/transaction_test.go`.
- **Requirements & Unit Tests:**
  - Standardized domain representation containing:
    * External transaction ID (`external_id`).
    * External authorization ID (`external_auth_id`).
    * Account reference (`account_id`).
    * Optional card reference (`card_id`).
    * Nominal transaction amount (`amount_minor`, `currency`).
    * Account clearing amount (`account_amount_minor`, `account_currency`).
    * Merchant information (`merchant_name`).
    * Merchant Category Code (`mcc`).
    * Operation timestamp (`transaction_time`).
    * Posting timestamp (`posted_time`).
    * Booking status (`pending`, `posted`, `cancelled`).
    * Original description (`raw_description`).
    * Provider-specific metadata preserved as raw bytes/JSON without leaking vendor structures into domain models.
  - Bank-specific DTOs must never leak into the core domain model.
- **Acceptance Criteria:** Domain models compile and serialize cleanly with zero vendor schema dependencies.

### Task 0A.3: Evidence and Accounting Separation (Two-Layer Ledger)
- **Scope:** Formulate the domain separation between immutable physical bank evidence and mutable accounting interpretation.
- **Files:** `internal/domain/ledger.go`, `internal/domain/ledger_test.go`.
- **Requirements & Unit Tests:**
  - Two distinct layers:
    * `bank_transactions`: immutable bank evidence representing authoritative external reality.
    * `accounting_entries`: user-controlled accounting interpretation (categorization, attribution, notes).
  - One-to-many accounting splits: single bank transaction split across $N \ge 2$ entries with split flag and sum invariance.
  - Manual cash operations: pure accounting entries with `bank_transaction_id = NULL`.
  - User categorization, beneficiary allocation (`household`, `partner_a`, `partner_b`), and user comments.
  - Non-destructive transfer matching: pairing reciprocal entries via `transfer_match_id` without wiping original categories or comments.
  - Historical evidence immutability: bank evidence layer cannot be altered by accounting adjustments.
- **Acceptance Criteria:** Unit tests prove complete isolation between physical evidence and user interpretation.

### Task 0A.4: Transaction Lifecycle State Machine
- **Scope:** Define and verify the monotonic lifecycle state machine for bank operations.
- **Files:** `internal/domain/lifecycle.go`, `internal/domain/lifecycle_test.go`.
- **Requirements & Unit Tests:**
  - Permitted monotonic transitions:
    * `pending` $\rightarrow$ `posted`: settlement transition with mandatory `posted_time >= transaction_time`, non-zero `account_amount_minor` (except `card_check`), sign preservation, non-wipeable `external_id`, and Category 1 immutable fact protection.
    * `pending` $\rightarrow$ `cancelled`: dropped/voided holds; settlement fields must remain unchanged.
  - Terminal state immutability:
    * `posted` $\rightarrow$ anything: forbidden.
    * `cancelled` $\rightarrow$ anything: forbidden.
    * `posted` $\rightarrow$ `posted`: mutating amounts or historical facts is forbidden.
    * `cancelled` $\rightarrow$ `cancelled`: mutating amounts or historical facts is forbidden.
  - Settlement updates must never overwrite immutable original transaction facts (`raw_description`, `mcc`, `operation_type`, `transaction_time`, `currency`).
- **Acceptance Criteria:** State machine test suite covers 100% of permitted and forbidden lifecycle transitions.

### Task 0A.5: Provider-Independent Idempotency & Deduplication
- **Scope:** Define and test provider-independent event deduplication and correlation algorithms.
- **Files:** `internal/domain/dedup.go`, `internal/domain/dedup_test.go`.
- **Requirements & Unit Tests:**
  - Repeated ingestion of the identical bank event must create exactly zero duplicate bank transactions or accounting entries.
  - Authorization-to-settlement matching: correlation of pending holds to posted clearing events via `external_auth_id` and `account_id` where available.
  - Fingerprint deduplication: deterministic hashing over `(account_id, transaction_time, amount_minor, raw_description)` with `fingerprint_sequence` disambiguating multiple legitimate same-day identical transactions.
  - Zero global uniqueness assumption: transaction IDs are NOT assumed to be globally unique across providers or even across different accounts within the same provider; deduplication is strictly tenant- and account-scoped.
- **Acceptance Criteria:** Deduplication test suite proves idempotent re-ingestion across 1,000 synthetic operations without duplications.

### Task 0A.6: Security, Multi-Tenant Isolation & Least Privilege
- **Scope:** Formulate and verify the multi-tenant isolation, role separation, and security contracts against canonical database specifications.
- **Files:** `internal/database/tenant.go`, `tests/security/test_security_suite.py`.
- **Requirements & Unit Tests:**
  - PostgreSQL Row-Level Security (RLS) with `FORCE ROW LEVEL SECURITY` across all 15 tenant tables.
  - Composite tenant foreign keys `(family_id, ...)` on all relationships.
  - Immutable `family_id` enforced by triggers.
  - 6-role least-privilege model (`family_admin`, `family_app`, `family_worker`, `family_queue_definer`, `family_auth_definer`, `family_auth_verifier`).
  - Transaction-scoped tenant context via parameterized `SELECT set_config('app.current_family_id', $1, true)`.
  - Append-only compliance logs (`audit_logs`, `raw_bank_payloads`) with trigger-enforced `UPDATE`/`DELETE` blocks.
  - Zero worker security bypass: workers operate under tenant-scoped transactions with `claim_next_worker_job()`.
  - Encrypted sensitive connection credentials (AES-256-GCM).
- **Acceptance Criteria:** 100% pass on the committed 70-test security test suite in `tests/security/test_security_suite.py`.

### Task 0A.7: Financial Invariants Verification
- **Scope:** Verify all core ledger and transfer matching consistency invariants across the data layer.
- **Files:** `tests/security/test_security_suite.py`.
- **Requirements & Unit Tests:**
  - Bank transaction and accounting entry consistency: bank transactions cannot commit without matching entries.
  - Split sum integrity: single entry requires `is_split = FALSE` and amount equality; split entries require all `is_split = TRUE` and sum equality.
  - Currency consistency: entries must match the parent bank transaction account currency.
  - Transfer matching invariants: reciprocal entries linked non-destructively; symmetric rejections prevent reverse pairing; endpoints immutable.
  - Pending settlement behavior: adjusting unsplit entries on settlement variance.
  - Cancellation behavior: voiding holds excludes accounting impact.
  - Evidence immutability and tenant isolation proofs.
- **Acceptance Criteria:** All consistency invariants verified by automated database constraint triggers and test assertions.

---

## Stage 0B: Bank Integration Readiness — NON-BLOCKING (Deferred)

> **Objective:** Incrementally validate, connect, and normalize external banking sources (T-Bank, Alfa-Bank, Sberbank) and statement export files. These requirements are **NON-BLOCKING** and tracked independently; development of Stage 1 (Core Domain, Migrations & Database) and subsequent core application layers proceeds without waiting for production bank integrations or live discovery.

### Task 0B.1: T-Bank Retail Integration & Contract Verification
- **Scope:** Research, test, and implement the T-Bank retail online banking connector.
- **Components:**
  - Live network discovery via `tbank-capture` request interception.
  - API contract verification for `/operations`, `/accounts_light_ib`, and `/statements`.
  - Request and response snapshots captured and stored as versioned fixtures in `internal/connector/tbank/testdata/`.
  - Pagination analysis (resolving whether cursor, offset, or time-window is used).
  - Date filtering analysis (parameter names and timestamp formats).
  - Session expiration handling (detecting HTTP 401/403 vs JSON status="FAILED").
  - Transport implementation: evaluate uTLS where technically appropriate for TLS client fingerprinting (without treating uTLS as an API compatibility guarantee).
  - Tolerant JSON decoding and provider-specific normalization mapping to `CanonicalBankTransaction`.
- **Artifacts:** Verified observations preserved in `docs/discovery_tbank.md`; unverified assumptions remain explicitly flagged until empirical traffic capture.
- **Status:** **Deferred / Non-blocking for Stage 1**.

### Task 0B.2: Alfa-Bank Integration & Contract Research
- **Scope:** Research, test, and implement the Alfa-Bank connector.
- **Components:**
  - API contract and file export format research.
  - Authentication mechanism and transport evaluation.
  - Versioned response fixtures stored in `internal/connector/alfa/testdata/`.
  - Provider-specific normalizer mapping to `CanonicalBankTransaction`.
  - Ingestion regression tests against recorded payloads.
- **Status:** **Deferred / Non-blocking for Stage 1**.

### Task 0B.3: Sberbank Integration & Contract Research
- **Scope:** Research, test, and implement the Sberbank connector.
- **Components:**
  - API contract and file export format research.
  - Authentication mechanism and transport evaluation.
  - Versioned response fixtures stored in `internal/connector/sber/testdata/`.
  - Provider-specific normalizer mapping to `CanonicalBankTransaction`.
  - Ingestion regression tests against recorded payloads.
- **Status:** **Deferred / Non-blocking for Stage 1**.

### Task 0B.4: Multi-Bank CSV/XLSX Statement Importers (Deferred Fallback)
- **Scope:** Implement streaming statement parsers for T-Bank (CSV/XLSX), Sberbank (XLSX), Alfa-Bank (XLSX), and a Generic RFC 4180 CSV format.
- **Components:**
  - Parsing tabular bank statement exports without floating-point arithmetic.
  - Date format parsing (`DD.MM.YYYY`, `YYYY-MM-DD`, `DD.MM.YYYY HH:MM:SS`).
  - Normalizing statement rows into `CanonicalBankTransaction` events for the standard ingestion pipeline.
  - Header error reporting and corrupt file handling.
- **Architectural Role:** Statement importers are valuable offline fallback mechanisms, but are **NOT** mandatory Stage 0 deliverables. The canonical ingestion interface allows statement importers to be added later without modifying the core domain model or database schema.
- **Status:** **Deferred / Non-blocking for Stage 1**.

---

## Stage 1: Core Domain, Migrations & Multi-Tenant Database

> **Dependency:** Gated exclusively upon **Stage 0A (Core Financial Foundation)** completion. Does **NOT** depend on Stage 0B (live bank network discovery or CSV/XLSX statement parsers).
> **Objective:** Establish the two-layer PostgreSQL schema (`bank_transactions` and `accounting_entries`), composite foreign keys, tenant isolation (`family_id`), and authentication foundation. Minimum supported database: **PostgreSQL 16+**.

### Task 1.1: Database Migrations with Composite Foreign Keys & Seed Data
- **Scope:** Write numbered SQL migrations using `golang-migrate` for all schemas defined in `database.md`. Include composite unique keys `UNIQUE(family_id, id)` and composite foreign keys with safe column-level `ON DELETE SET NULL (nullable_column)`.
- **Files:** `migrations/000001_initial_schema.up.sql`, `migrations/000002_seed_categories.up.sql`.
- **Integration Tests (Composite Deletion & Trigger Integrity):**
  - **FK Deletion Tests:**
    - `TestFK_DeleteCard`: Deleting a card sets `bank_transactions.card_id = NULL` while `family_id` remains intact (no NOT NULL violation).
    - `TestFK_DeleteCategory`: Deleting a category sets `accounting_entries.category_id = NULL` while `family_id` remains intact.
    - `TestFK_DeleteConnection`: Deleting a connection sets `accounts.bank_connection_id = NULL` while `family_id` remains intact.
    - `TestFK_DeleteAccountCascade`: Deleting an account cascades to linked cards and bank transactions cleanly.
    - `TestFK_RestrictMemberDelete`: Attempting to delete a `family_members` row when that user owns an account is rejected by `RESTRICT`.
  - **Deferred Constraint Trigger & Invariant Tests:**
    - `TestTrigger_SingleEntrySplitTrue`: Inserting a single accounting entry with `is_split = TRUE` matching bank transaction amount fails at `COMMIT`.
    - `TestTrigger_TwoEntriesOneSplitFalse`: Inserting two split entries where one has `is_split = FALSE` fails at `COMMIT`.
    - `TestTrigger_BankTxNoEntry`: Inserting a `bank_transaction` without an accompanying `accounting_entry` fails at `COMMIT`.
    - `TestTrigger_InsertSplitMismatch`: Inserting split entries summing to 4,000 RUB for a 5,000 RUB bank transaction fails at `COMMIT`.
    - `TestTrigger_DeleteSplitEntry`: Deleting an entry in a split group leaving a sum imbalance fails at `COMMIT`.
    - `TestTrigger_ReassignBankTx`: Reassigning an entry to a different bank transaction validates both old and new parent transactions `(family_id, bank_transaction_id)` at `COMMIT`.
    - `TestTrigger_UpdateBankTxAmount`: Updating `bank_transactions.account_amount_minor` without updating splits fails at `COMMIT`.
    - `TestTrigger_AtomicSplitReplacement`: Atomically deleting old splits and inserting new valid splits inside one transaction commits successfully.
    - `TestTrigger_TenantImmutability`: Attempting an `UPDATE` that alters `family_id` on any of the 14 tenant tables fails immediately via `trg_enforce_tenant_immutability`.
    - `TestEvidence_BankTxDeleteBlocked`: Attempting `DELETE FROM bank_transactions` fails at SQL privilege layer and trigger level `trg_bank_tx_no_delete`.
    - `TestEvidence_BankTxCoreFactsImmutable`: `family_worker` attempting to mutate historical bank facts on `bank_transactions` is aborted by `trg_bank_tx_immutable`.
    - `TestEvidence_BankTxLifecycleMonotonic`:
      * `pending` $\rightarrow$ `posted` with valid settlement updates (`account_amount_minor`, `posted_time`, `external_id`) succeeds.
      * `pending` $\rightarrow$ `posted` modifying immutable facts (`raw_description`, `currency`) fails.
      * `pending` $\rightarrow$ `cancelled` without modifying amount succeeds.
      * `pending` $\rightarrow$ `cancelled` attempting to modify `account_amount_minor` fails.
      * `posted` $\rightarrow$ `posted` attempting to modify `account_amount_minor` fails.
      * `posted` $\rightarrow$ `cancelled` fails.
      * `cancelled` $\rightarrow$ `posted` fails.
    - `TestEvidence_RawPayloadImmutable`: `family_app` and `family_worker` attempting `UPDATE` or `DELETE` on `raw_bank_payloads` fails at SQL privilege layer and trigger level `trg_raw_payloads_immutable`.
    - `TestAudit_AppendOnly`: `family_app` and `family_worker` can insert into `audit_logs`, but attempting `UPDATE` or `DELETE` fails at SQL privilege layer and trigger level `trg_audit_logs_append_only`.
    - `TestTrigger_TransferMatchEndpointImmutability`: Attempting to alter `source_bank_transaction_id` or `destination_bank_transaction_id` on `transfer_matches` fails immediately via `trg_immutability_transfer_endpoints`.
    - `TestTrigger_TransferMatchEntryOwnership`: Linking an accounting entry to a `transfer_matches` row where `entry.bank_transaction_id` is neither the match source nor destination fails at `COMMIT`.
    - `TestIndex_SymmetricTransferRejection`: Inserting reverse rejection `(family_id, dest_id, source_id)` when `(family_id, source_id, dest_id)` exists violates `uq_transfer_rejections_symmetric`.
    - `TestSecurity_PermanentSuite`: 100% pass on reproducible PostgreSQL 16 test suite in `tests/security/test_security_suite.py`.
- **Acceptance Criteria:** Clean migration application and 100% test pass on all deletion, trigger integrity, and evidence lifecycle tests.

### Task 1.2: Transaction-Scoped RLS & Connection Pool (`WithFamilyTx`)
- **Scope:** Implement transaction-scoped tenant context injection (`WithFamilyTx`) using parameterized `SELECT set_config('app.current_family_id', $1, true)`. Configure the 6-role PostgreSQL access model (`family_admin`, `family_app`, `family_worker`, `family_queue_definer`, `family_auth_definer`, `family_auth_verifier`) and dedicated schemas (`queue_sec`, `auth_sec`). Eliminate all blanket table grants.
- **Files:** `internal/database/pool.go`, `internal/database/tenant.go`.
- **Integration Tests (Mandatory Tenant & Credential Isolation Proofs):**
  - **Test Case RLS-01:** Create Family A and Family B with separate accounts, bank transactions, and entries.
  - **Test Case RLS-02:** Execute queries under Family A context; assert that zero rows from Family B are returned even when querying by Family B's known UUIDs.
  - **Test Case RLS-03:** Attempt `INSERT`, `UPDATE`, or `DELETE` on Family B's entity from Family A context; assert 0 rows affected or RLS policy violation error.
  - **Test Case RLS-04:** Execute queries with missing or empty `app.current_family_id`; assert deny-all (0 rows returned, no leaks).
  - **Test Case RLS-05:** Worker job execution: verify worker claims jobs via `queue_sec.claim_next_worker_job()` and executes `WithFamilyTx(job.family_id)` per job without cross-job or cross-tenant contamination.
  - **Test Case RLS-06 (User Membership Enforcement):** Attempt to insert an account in Family A with `owner_user_id` belonging only to Family B; assert composite foreign key violation on `family_members(family_id, user_id)`.
  - **Test Case RLS-07 (Force RLS Enforcement):** Connect as table owner role; execute queries without tenant context; assert 0 rows returned due to `FORCE ROW LEVEL SECURITY`.
  - **Test Case RLS-08 (Membership Discovery & Anti-Enumeration Defense):** When `app.current_family_id` is unset, verify `SELECT * FROM families` and `SELECT * FROM family_members` succeed via `auth_sec.get_current_user_family_ids()` (no arguments, reads `app.current_user_id`) without infinite recursion. Assert that an attacker cannot pass arbitrary user IDs or enumerate other users' families. Assert that `family_worker` cannot execute this function.
  - **Test Case RLS-09 (Worker Job Claiming & Lease Reclaiming):** Concurrent workers calling `queue_sec.claim_next_worker_job('bank_sync', 'worker-1', 600)` claim distinct jobs across tenants via `FOR UPDATE SKIP LOCKED` and receive valid `(job_id, family_id, payload)` without any client-settable GUC bypass. Reclaim stalled jobs where `state = 'running' AND locked_until < NOW()`. Assert `family_queue_definer` has zero access to financial or user tables.
  - **Test Case RLS-10 (Credential Isolation):** Assert `family_app` cannot execute `SELECT password_hash FROM users`. Assert `family_worker` cannot read `password_hash` or `email` from `users`. Assert login credential lookup succeeds only via `auth_sec.get_user_auth_credential(email)`.
  - **Test Case RLS-11 (Registration Bootstrap Sequence):** Application pre-generates UUIDs (`new_user_id`, `new_family_id`), sets context via `set_config` in transaction, and successfully inserts `users`, `families`, `family_members`, and `categories` without RLS `WITH CHECK` violation or foreign key failure.
- **Acceptance Criteria:** 100% test pass on all multi-tenant and credential isolation proofs.

### Task 1.3: User Auth & Bootstrap Registration Module
- **Scope:** Implement Argon2id password hashing, JWT issue/verify, user login via `auth_sec.get_user_auth_credential()`, password update via `auth_sec.update_user_password()` (enforcing application-layer verification of current password before database function invocation), partner invitation flow, and registration bootstrap.
- **Files:** `internal/auth/service.go`, `internal/auth/jwt.go`, `internal/auth/handler.go`.
- **Unit & Integration Tests:**
  - Registration atomically provisions user, family, owner membership, and default categories.
  - Expired token rejection and refresh token rotation.
  - Multi-partner access: two users successfully belonging to the same `family_id`.
- **Acceptance Criteria:** Secure auth endpoints matching `api.yaml`.

### Task 1.4: Accounts & Cards Management Module
- **Scope:** CRUD operations for checking, credit, savings, and manual cash accounts. Support linking multiple cards (`cards` table) to a single account. Support `NULL` for unobserved balances and record `balance_updated_at` and `balance_source`.
- **Files:** `internal/account/service.go`, `internal/account/repository.go`.
- **Unit Tests:**
  - Linking primary and virtual cards to one checking account.
  - Correct calculation of available balance for credit cards: $\text{credit\_limit} - (\text{debt\_balance} + \text{pending\_holds})$.
  - Available balance for checking accounts: $\text{current\_balance} - \text{pending\_holds}$.
  - Initial accounts without bank connection default to `current_balance_minor = NULL` and `balance_source = NULL`.
- **Acceptance Criteria:** Adding transactions through different cards correctly updates the single shared account balance.

---

## Stage 2: Ingestion Engine, Deduplication & Job Worker

> **Objective:** Build the resilient data ingestion pipeline, deduplication engine, and PostgreSQL-backed async job queue.

### Task 2.1: `BankConnector` Abstraction & Mock/Fixture Adapter
- **Scope:** Implement the provider-independent `BankConnector` interface (`FetchAccounts`, `FetchCards`, `FetchBalances`, `FetchTransactions`) defined in `architecture.md`. In Stage 2, ingestion is tested and verified using versioned empirical contract fixtures and mock connectors; production bank connectors (T-Bank, Alfa-Bank, Sberbank) plug in asynchronously via Stage 0B tracks without altering the ingestion engine.
- **Files:** `internal/connector/connector.go`, `internal/connector/mock/mock.go`.
- **Integration Tests:**
  - End-to-end sync simulation against recorded discovery fixtures.
  - Error classification: distinguish temporary network failures, rate limiting, and auth expirations via `SyncError`.
- **Acceptance Criteria:** Ingestion pipeline cleanly consumes normalized canonical events from the connector abstraction.

### Task 2.2: Ingestion Pipeline & Raw Payload Store
- **Scope:** Implement atomic transaction ingestion. Save exact raw JSON into `raw_bank_payloads` and execute natural key / synthetic fingerprint deduplication into `bank_transactions`. Automatically provision default `accounting_entries` (1:1 unsplit).
- **Files:** `internal/transaction/ingest.go`, `internal/transaction/dedup.go`.
- **Unit & Integration Tests:**
  - Ingest identical batch twice: verify exactly 0 duplicate rows in `bank_transactions` and `accounting_entries`.
  - Disambiguate identical same-day transactions via `fingerprint_sequence`.
  - Ingesting a 0.00 RUB card verification hold succeeds without constraint failure (`operation_type = 'card_check'`).
- **Acceptance Criteria:** Idempotency verified under concurrent ingestion runs.

### Task 2.3: Lifecycle State Transition Manager
- **Scope:** Transition bank transactions from `pending` (hold) to `posted` (settled), `cancelled`, or `refund`. Update debit amounts and exchange rates upon final settlement. Link refunds to original transactions via `related_bank_transaction_id`.
- **Files:** `internal/transaction/lifecycle.go`.
- **Unit Tests:**
  - Card hold of 1,000 RUB matched with subsequent settled transaction of 1,050 RUB: status becomes `posted` with settled amount 1,050 RUB and single record preserved. Unsplit accounting entry updates amount to 1,050 RUB.
  - Split transaction hold settlement drift: preserves user categories and flags drift for review.
  - Explicit bank cancellation (status=FAILED) voids hold and excludes accounting entry.
  - Disappearance from sync is NOT treated as cancellation unless 14-day fully paginated window completes without the item.
  - Merchant refund creates a separate `bank_transaction` (`operation_type = 'refund'`) with positive amount, linked to original purchase category.
- **Acceptance Criteria:** Zero double-counting of authorizations and clearing settlements.

### Task 2.4: PostgreSQL-Backed Job Queue (`worker_jobs`)
- **Scope:** Implement transactional background worker queue using `claim_next_worker_job(queue_name, worker_id, lock_duration)`.
  - Schema: `worker_jobs` with `state` (`available`, `running`, `completed`, `failed`, `discarded`), `locked_by`, `locked_until`, `attempts`, `max_attempts` (default 5), and `payload JSONB`.
  - Role separation: `family_queue_definer` owns claiming function with `rls_worker_jobs_claim_*` (`USING (true)`).
  - Concurrency: `FOR UPDATE SKIP LOCKED` guarantees conflict-free concurrent claiming.
  - Lease recovery: Stalled jobs where `state = 'running' AND locked_until < NOW()` are automatically reclaimed.
  - Retry semantics: Failed jobs increment `attempts`, calculate exponential backoff (`scheduled_at = NOW() + 2^attempts * 10s`), and transition back to `available`. Jobs reaching `max_attempts` transition to `discarded` (dead-lettering) with `last_error`.
- **Files:** `internal/queue/worker.go`, `internal/queue/job.go`.
- **Unit & Integration Tests:**
  - Multiple concurrent worker instances process distinct jobs without race conditions.
  - Failed job increments attempt counter and applies backoff.
  - Expired lock lease is reclaimed by another worker after timeout.
- **Acceptance Criteria:** Resilient asynchronous job processing with zero Redis dependency.

---

## Stage 3: Categorization Engine & Conservative Transfer Matcher

> **Objective:** Deliver automated classification rules and pairing of internal account movements and credit card repayments with zero false positives.

### Task 3.1: Deterministic Categorization Rule Engine
- **Scope:** Evaluate incoming accounting entries against the ordered rule hierarchy:
  1. User-specific merchant rule
  2. Saved counterparty rule
  3. MCC code match
  4. Name regex/keyword match
  5. Fallback: "Uncategorized"
- **Files:** `internal/categorizer/engine.go`, `internal/categorizer/rules.go`.
- **Unit Tests:**
  - Priority ordering test: ensure higher priority user rule overrides MCC code.
  - Pattern matching for Russian merchant descriptions (e.g., `*YANDEX.LAVKA*` -> "Groceries").
  - Re-running rules updates `accounting_entries` without mutating `bank_transactions`.
- **Acceptance Criteria:** Categorization of 1,000 transactions executes in under 10ms.

### Task 3.2: Conservative Multi-Tier Transfer Matcher Engine
- **Scope:** Implement pairing algorithm between reciprocal outflows and inflows:
  - **Tier 1:** Same amount, opposite signs, $\Delta t \le 30\text{ min}$, internal accounts, **MANDATORY reciprocal semantic transfer evidence** $\rightarrow$ Auto-confirm (`transfer_match_id` linked non-destructively).
  - **Tier 2:** Fee variance, inter-bank delay, or unverified evidence $\rightarrow$ Suggest for 1-click review.
  - **Tier 3:** Manual user pairing modal (and cross-currency transfers).
  - **Non-Destructive Linkage:** Preserves original categories and classifications when matched and unlinked.
  - **Symmetric Rejection:** Prevent rematching rejected pairs in either direction via `uq_transfer_rejections_symmetric` index and candidate filtering. Store rejection `reason` (max 500 chars).
  - **Atomic 5-Step Rejection Flow:** Execute match lock, rejection recording, non-destructive entry unlinking, match status/`updated_at` update, and audit logging in a single transaction.
  - **Endpoint Immutability:** Enforce `trg_immutability_transfer_endpoints` so source/destination transactions cannot be altered once created.
- **Files:** `internal/transfermatch/matcher.go`, `internal/transfermatch/rules.go`.
- **Unit Tests (Transfer Matcher Test Suite):**
  - Verify all test cases from [docs/transfer_matcher.md#6-comprehensive-transfer-matcher-test-matrix](file:///Volumes/work/work/go/family-finance/docs/transfer_matcher.md#6-comprehensive-transfer-matcher-test-matrix) (TC-TR-01 through TC-TR-10).
  - MCC alone does NOT trigger auto-confirmation.
  - Rejecting a match unlinks `transfer_match_id`, immediately restoring original expense/income classification without data loss.
  - Rejecting A $\rightarrow$ B prevents the engine from proposing B $\rightarrow$ A (TC-TR-08).
  - Attempting to link an accounting entry to a transfer match with mismatching bank transactions fails validation (TC-TR-09).
  - Attempting to mutate source or destination endpoints on an existing transfer match fails with an exception (TC-TR-10).
- **Acceptance Criteria:** Zero false-positive auto-matches on non-transfer operations.

### Task 3.3: Credit Card Repayment Specializer
- **Scope:** Specialized transfer matching where destination account is of type `credit_card`. Sets `is_credit_card_repayment = TRUE` on `transfer_matches`.
- **Files:** `internal/transfermatch/credit_card.go`.
- **Unit Tests:**
  - Repaying credit card from debit account decreases debit balance, decreases credit card debt balance, and produces **0** family expense.
  - Annual card fee or interest fee classified as "Finance: Credit Card Fees" (real expense).
- **Acceptance Criteria:** Total debt and assets update accurately without distorting monthly budget burn.

---

## Stage 4: Analytics Engine, REST API & Deployment

> **Objective:** Expose high-speed analytics queries, REST endpoints according to OpenAPI 3.1 (Two-Layer API with stable `TransactionView` identity), audit logging, and Docker Compose deployment.

### Task 4.1: Cash Flows, Real Expenses & Net Worth Analytics
- **Scope:** High-performance SQL queries calculating:
  - Operating Cash Flows (Inflows vs Outflows).
  - True Family Consumption (Expenses net of refunds and internal transfers).
  - Net Worth & Debt Positions (handling `NULL` balance values cleanly).
  - Attribution breakdown: Partner A spend vs Partner B spend vs Shared Household.
- **Files:** `internal/analytics/service.go`, `internal/analytics/repository.go`.
- **Integration Tests:**
  - Test scenario with 100 transactions (including refunds, transfers, and credit card debt repayments): verify mathematical equality between cash change and $\text{Income} - \text{Expenses} + \Delta\text{Debt}$.
- **Acceptance Criteria:** Summary analytics queries return in under 20ms on 100k transactions.

### Task 4.2: REST API Endpoints & Chi Router Integration (Two-Layer API)
- **Scope:** Complete HTTP handlers for all endpoints specified in `api.yaml`.
  - Expose `TransactionView` with stable identity (`view_id`, `bank_transaction_id`, `primary_entry_id`, `entries`).
  - Querying by `view_id`, `bank_transaction_id`, or `entry_id` returns the complete split group.
  - Implement `PATCH /transactions/entries/{id}` for mutating accounting entries.
  - Implement `POST /transactions/entries/{id}/split` for splitting accounting entries.
  - Implement `POST /connections/pair` for sidecar token exchange.
- **Files:** `cmd/api/main.go`, `internal/api/router.go`, `internal/api/handlers/*.go`.
- **Integration Tests:**
  - End-to-end API testing covering Auth, Accounts, Transactions, Splits, Transfers, Categories, and Analytics.
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

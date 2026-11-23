# Family Finance: System Architecture Specification

## 1. Architectural Philosophy & Principles

1. **Modular Monolith in Go:** Single compilation target, high developer velocity, zero distributed system overhead (no Kubernetes, Kafka, or microservices required for a self-hosted family deployment), with strictly enforced domain boundaries.
2. **Two-Layer Ledger (Evidence vs. Interpretation):**
   - **Bank Evidence Layer (`bank_transactions`):** Immutable, authoritative historical record of bank events. Stores bank identifiers (`id`, `operationId`, `authorizationId`), raw descriptions, MCC codes, booking status (`pending`, `posted`, `cancelled`), and cleared timestamps. Bank sync updates never overwrite user categorization, attribution, or notes.
   - **Accounting Interpretation Layer (`accounting_entries`):** The family's categorization, payer attribution, beneficiary allocation, and split entries. Supports 1:N splits of bank transactions. Manual cash operations exist purely in this layer (`bank_transaction_id IS NULL`).
3. **Multi-Tenant Isolation via Composite Foreign Keys & RLS:**
   - Minimum supported database: **PostgreSQL 16+**.
   - Every tenant-owned table enforces `UNIQUE (family_id, id)`.
   - Every relationship across tenant entities uses composite foreign keys referencing `(family_id, target_id)`.
   - Column-level deletion syntax `ON DELETE SET NULL (nullable_column)` ensures that deleting an optional target NULLs only the referencing attribute, preserving `family_id NOT NULL`.
   - User membership ownership: every tenant reference to a user is foreign-keyed to `family_members(family_id, user_id)`.
   - **Tenant Immutability:** `family_id` is strictly immutable on all tenant tables; mutating `family_id` is blocked by `trg_enforce_tenant_immutability`.
   - **Defense-in-Depth RLS:** `FORCE ROW LEVEL SECURITY` is applied to all tenant tables so table owners cannot accidentally bypass RLS policies.
   - Transaction-scoped tenant context: Parameterized `SELECT set_config('app.current_family_id', $1, true)` executes inside every database transaction via `WithFamilyTx`. Missing context strictly evaluates to deny-all.
   - **Zero Worker Security Bypass:** Custom GUCs like `app.is_worker` are strictly forbidden as security boundaries. Background workers execute under transaction-scoped `WithFamilyTx(job.family_id)` using a dedicated `claim_next_worker_job()` function.
   - **RLS Recursion Defense:** Membership discovery without an active family context uses a `SECURITY DEFINER` function `public.get_user_family_ids(user_id)` with a strict search path.
4. **Exact Minor-Unit Mathematics:** Strict prohibition of floating-point numbers (`float32`/`float64`) for money. All monetary amounts are handled as `int64` minor units (kopeks, cents) alongside ISO 4217 currency codes.
5. **Stateless API & Background Worker Separation:** Deployed as two runtimes from the same codebase (`cmd/api` and `cmd/worker`) sharing internal domain packages.

---

## 2. Component Diagram & Module Boundaries

```mermaid
graph TD
    Client["Client Layer (Web UI / Mobile Web / CLI)"] -->|HTTPS / REST / SSE| Caddy["Reverse Proxy (Caddy: Automatic TLS)"]
    Caddy -->|Reverse Proxy| API["API Server (cmd/api - Chi Router)"]

    subgraph Internal_Modules ["Go Modular Monolith (internal/*)"]
        API --> M_Auth["auth (JWT / Argon2id / Tenant Context)"]
        API --> M_Account["account (Accounts, Cards, Balances)"]
        API --> M_Tx["transaction (Bank Evidence & Accounting Entries)"]
        API --> M_Import["importer (CSV / XLSX Parsers)"]
        API --> M_Categorizer["categorizer (Deterministic Rule Engine)"]
        API --> M_Matcher["transfermatch (Non-Destructive Transfer Matcher)"]
        API --> M_Analytics["analytics (Cash-flow, Real Expenses, Debt)"]
        API --> M_Audit["audit (Change Auditing, Snapshots)"]

        Worker["Sync & Ingestion Worker (cmd/worker)"] --> M_Queue["queue (Pg-backed Queue / Outbox)"]
        Worker --> M_Connector["connector (BankConnector Abstraction)"]
        Worker --> M_Tx
        Worker --> M_Categorizer
        Worker --> M_Matcher
    end

    subgraph Bank_Connectors ["Bank Connectors (internal/connector/*)"]
        M_Connector --> C_TBank["tbank (Playwright Bridge / Capture Client)"]
        M_Connector --> C_Sber["sber (Statement Parser / Open API)"]
        M_Connector --> C_Manual["manual (Cash / Offline Accounts)"]
    end

    subgraph Storage_Layer ["Database & Jobs (PostgreSQL 16+)"]
        API --> PG[(PostgreSQL: Ledger, Metadata, RLS)]
        Worker --> PG
        M_Queue --> PG
    end
```

### Module Responsibilities:

| Module | Boundary & Responsibility | Inward Dependencies | Outward Dependencies |
| :--- | :--- | :--- | :--- |
| `internal/auth` | User authentication, Argon2id hashing, JWT generation, family member permissions, bootstrap registration. | `database` | None |
| `internal/account` | Account and card lifecycle management, balance tracking (with timestamped observation sources), manual cash accounts. | `auth`, `database` | `audit` |
| `internal/connector` | Defines `BankConnector` interface and adapters for T-Bank, Sberbank, etc. Translates external bank DTOs to normalized internal models. | `domain` | External APIs / Local browser runner |
| `internal/importer` | Parses CSV and XLSX files from T-Bank, Sber, Alfa-Bank, and generic formats. Dispatches raw batches to the transaction pipeline. | `connector`, `domain` | None |
| `internal/transaction` | Ingestion into `bank_transactions`, raw payload preservation, deduplication, transaction lifecycle updates, deferred split trigger management, and `accounting_entries`. | `account`, `domain` | `categorizer`, `transfermatch`, `queue`, `audit` |
| `internal/categorizer` | Rule-based hierarchical categorization engine (User rule > Merchant rule > MCC > Keyword regex > Fallback). Operates on `accounting_entries`. | `domain` | None |
| `internal/transfermatch`| Conservative transfer matching engine (Tier 1 exact, Tier 2 probable, Tier 3 manual) and credit card debt repayment detection. Links `accounting_entries.transfer_match_id` non-destructively. | `transaction`, `domain` | `audit` |
| `internal/analytics` | High-performance read-only queries for cash flows, real consumption expenses, debt tracking, MoM comparisons, and partner attribution. | `domain`, `database` | None |
| `internal/queue` | PostgreSQL-backed persistent job queue (transactional outbox) with retry backoff and concurrency control (`SKIP LOCKED`). | `database` | None |
| `internal/audit` | Append-only audit trail recording user actions, category reclassifications, manual transfer links, and rule modifications. | `database` | None |

---

## 3. Stable TransactionView Identity Model

To prevent identity ambiguity between the physical bank evidence layer and the accounting interpretation layer:

```
[ Physical Bank Event ]                [ Accounting Interpretation ]
  bank_transactions                         accounting_entries
  id: "bt_1001"                              id: "entry_5001" (Groceries)
                                             id: "entry_5002" (Pharmacy)
                 │                                  │
                 └────────────────┬─────────────────┘
                                  ▼
                        [ TransactionView ]
                  view_id: "bt:bt_1001"
                  bank_transaction_id: "bt_1001"
                  entries: [ "entry_5001", "entry_5002" ]
```

1. **Deterministic View ID:**
   * For Bank-Backed Transactions: `view_id = "bt:" || bank_transaction_id`
   * For Manual Cash Operations: `view_id = "manual:" || accounting_entry_id`
2. **Behavior on GET and PATCH:**
   * `GET /transactions/{id}`: Accepting either a `view_id`, a `bank_transaction_id`, or any constituent `entry_id` resolves to the **complete `TransactionView`**, returning all split entries in that logical group.
   * `PATCH /transactions/entries/{entry_id}`: Targets a specific accounting entry to edit its category, comment, or beneficiary without mutating bank evidence.
   * `POST /transactions/entries/{entry_id}/split`: Splits an unsplit entry into $N \ge 2$ entries while preserving the stable `view_id`.

---

## 4. Account and Balance Semantics

To eliminate financial ambiguity, every balance value follows strict formulas, sign conventions, and authoritative sourcing rules:

### 4.1 Balance Definitions, Nullability & Sources

| Balance Dimension | Database Nullability | Authoritative Source | Semantics & Sourcing Rules |
| :--- | :--- | :--- | :--- |
| **Current Balance (`current_balance_minor`)** | `BIGINT NULL` | `balance_source = 'bank_api'` or `'statement_file'` | Physical cleared cash in bank. If unobserved, defaults to `NULL` (unknown), **never** fabricated as 0. |
| **Pending Holds (`pending_holds_minor`)** | Calculated on read | Sum of active holds | $\sum \text{abs}(\text{amount\_minor})$ for `bank_transactions` where `booking_status = 'pending'` and `amount_minor < 0`. |
| **Available Balance (`available_balance_minor`)** | `BIGINT NULL` | `bank_api` or calculated | Spendable liquidity. Debit: $\text{Current} - \text{Holds}$. Credit: $\text{Credit Limit} - (\text{Debt} + \text{Holds})$. |
| **Credit Card Debt (`debt_balance_minor`)** | `BIGINT NULL` | `bank_api` or calculated | Cleared debt balance on credit card. `NULL` on non-credit accounts. |
| **Credit Limit (`credit_limit_minor`)** | `BIGINT NULL` | `bank_api` | Approved credit line (`creditLimit.value`). `NULL` on non-credit accounts. |
| **Statement Balance (`statement_balance_minor`)** | `BIGINT NULL` | `statements` endpoint / `billedDebt` | Debt fixed at monthly billing cycle close. Must be repaid by grace period date. |
| **Minimal Payment (`minimal_payment_minor`)** | `BIGINT NULL` | `bank_api` (`currentMinimalPayment`) | Mandatory minimum payment due. `NULL` on non-credit accounts. |
| **Grace Period Due Date (`grace_period_end_date`)** | `DATE NULL` | `bank_api` (`nextStatementDate`) | Payment deadline for 0% interest benefit. |
| **Balance Observation Timestamp (`balance_updated_at`)** | `TIMESTAMPTZ NULL` | System / Bank timestamp | Records the exact point-in-time when this balance snapshot was observed. |

---

## 5. Non-Destructive Transfer Representation & Match Integrity

When the transfer matcher pairs two bank transactions (e.g., checking outflow and credit card inflow):

1. **Non-Destructive Linkage:**
   * The linked `accounting_entries.transfer_match_id` foreign key is set to `transfer_matches.id`.
   * **Crucial Rule:** The entry's original `category_id`, `user_comment`, and `classification` are **100% preserved and never overwritten**.
2. **Transfer Match Ownership & Endpoint Immutability Triggers:**
   * **Endpoint Immutability:** Trigger `trg_immutability_transfer_endpoints` strictly prohibits modifying `source_bank_transaction_id` or `destination_bank_transaction_id` on existing `transfer_matches`.
   * **Entry Ownership Validation:** Deferred constraint trigger `trg_validate_accounting_entry_transfer_match` on `accounting_entries` strictly enforces that an entry linked to a transfer match MUST belong to either the source or destination bank transaction of that exact match:
     $$\text{entry.bank\_transaction\_id} \in \{\text{match.source\_bank\_transaction\_id}, \text{match.destination\_bank\_transaction\_id}\}$$
   * Combining endpoint immutability with deferred entry ownership guarantees that an existing linked entry can never become unrelated or orphaned from its match.
3. **Analytical Interpretation:**
   * Queries and analytical views (`v_family_real_expenses`) check `transfer_match_id`: if pointing to a match with `status IN ('auto_confirmed', 'confirmed')`, the entry is projected as an internal transfer or debt repayment and excluded from consumption expenses.
4. **Symmetric Transfer Rejection Model:**
   * When a proposed transfer between transaction A and transaction B is rejected, a rejection record is inserted with `source_bank_transaction_id = A`, `destination_bank_transaction_id = B`, and an optional `reason` (up to 500 characters).
   * Table `transfer_rejections` enforces a symmetric unique index:
     `UNIQUE (family_id, LEAST(source_bank_transaction_id, destination_bank_transaction_id), GREATEST(source_bank_transaction_id, destination_bank_transaction_id))`
   * The transfer matcher checks candidate pairs symmetrically:
     `WHERE LEAST(candidate_source, candidate_dest) = LEAST(rej_source, rej_dest) AND GREATEST(...) = GREATEST(...)`
   * This guarantees that rejecting A $\rightarrow$ B permanently suppresses both A $\rightarrow$ B and B $\rightarrow$ A proposals.
5. **Atomic 5-Step Non-Destructive Rejection Flow:**
   * Executed within a single `WithFamilyTx` database transaction:
     1. `SELECT * FROM transfer_matches WHERE id = $1 FOR UPDATE` (locks match).
     2. `INSERT INTO transfer_rejections (family_id, source_bank_transaction_id, destination_bank_transaction_id, rejected_by_user_id, reason) VALUES ($family_id, $src_id, $dst_id, $user_id, $reason) ON CONFLICT (family_id, LEAST(source_bank_transaction_id, destination_bank_transaction_id), GREATEST(source_bank_transaction_id, destination_bank_transaction_id)) DO NOTHING` (persists symmetric suppression).
     3. `UPDATE accounting_entries SET transfer_match_id = NULL, updated_at = NOW() WHERE transfer_match_id = $1` (clears linkage while preserving category and user comment intact).
     4. `UPDATE transfer_matches SET status = 'rejected', updated_at = NOW() WHERE id = $1` (updates lifecycle status and timestamp).
     5. `INSERT INTO audit_logs (family_id, user_id, action, entity_type, entity_id, new_values) VALUES (...)` (records audit trail).

---

## 6. Security Architecture & Threat Mitigation

```mermaid
graph LR
    subgraph Client_Trust_Boundary ["Client Trust Boundary"]
        Browser["User Browser (Partner A / B)"]
        Sidecar["Local tbank-capture Runner"]
    end

    subgraph Server_Boundary ["Server Security Perimeter"]
        TLS["TLS Termination (Caddy HTTPS)"]
        AuthMiddleware["Auth Middleware (JWT / API Key)"]
        App["App Business Logic (WithFamilyTx)"]
        Enc["AES-256-GCM Envelope Cryptor"]
    end

    subgraph Data_Perimeter ["Data Storage (PostgreSQL 16+)"]
        DB[(PostgreSQL with RLS & Composite FKs)]
        Vault[(Encrypted Tokens / Secrets)]
    end

    Browser -->|JWT over HTTPS| TLS
    Sidecar -->|ConnectionAPIKey over HTTPS| TLS
    TLS --> AuthMiddleware
    AuthMiddleware -->|Injects family_id into Tx| App
    App -->|Ciphertext| Vault
    App -->|SELECT set_config app.current_family_id| DB
```

### 6.1 Parameterized Tenant Context Execution (`WithFamilyTx`)

```go
// WithFamilyTx executes fn within an explicit transaction scoped to familyID.
func WithFamilyTx(ctx context.Context, pool *pgxpool.Pool, familyID uuid.UUID, fn func(pgx.Tx) error) error {
    tx, err := pool.Begin(ctx)
    if err != nil {
        return fmt.Errorf("begin tx: %w", err)
    }
    defer tx.Rollback(ctx)

    // Parameterized set_config scopes the variable strictly to this transaction (is_local = true)
    var result string
    err = tx.QueryRow(ctx, "SELECT set_config('app.current_family_id', $1, true)", familyID.String()).Scan(&result)
    if err != nil {
        return fmt.Errorf("set tenant context: %w", err)
    }

    if err := fn(tx); err != nil {
        return err
    }
    return tx.Commit(ctx)
}
```

### 6.2 Background Worker Execution Model & Job Claiming

1. **Queue Tenant Invariant:**
   * Every job in `worker_jobs` requires `family_id UUID NOT NULL REFERENCES families(id)`.
2. **Zero GUC Bypass:**
   * Background workers run with standard least-privilege credentials (`family_worker`).
   * No client-settable GUCs (e.g. `app.is_worker = 'true'`) or `BYPASSRLS` privileges are permitted.
3. **Dedicated Job Claiming Role & Function (`claim_next_worker_job`):**
   * Workers invoke `claim_next_worker_job(p_queue_name, p_worker_id, p_lock_duration_seconds)` to atomically claim the next pending job across all tenants using `FOR UPDATE SKIP LOCKED`.
   * Owned by dedicated internal role `family_queue_definer` (`NOLOGIN`), which has `SELECT, UPDATE ON public.worker_jobs` only, with an explicit RLS policy `USING (true)`. The role has **zero access to tenant financial or ledger tables**.
   * The function transitions the job status to `'running'`, records `locked_by` and `locked_until`, increments `attempts`, and returns `(job_id UUID, family_id UUID, job_type VARCHAR, payload JSONB)`.
   * Stalled jobs where `state = 'running' AND locked_until < NOW()` are automatically reclaimed by active workers upon lease expiry.
4. **Tenant-Isolated Execution:**
   * Once a job is claimed, the worker initiates domain execution strictly wrapped in `WithFamilyTx(ctx, pool, job.family_id, func(tx pgx.Tx) error { ... })`.
   * All subsequent domain queries, bank sync ingestion, and category processing execute within standard RLS perimeters for that single family.

### 6.3 Membership Discovery, Registration Bootstrap & RLS Recursion Defense

1. **Authentication Trust Boundary Clarification:**
   * In this three-tier architecture (Web Client $\rightarrow$ Go Monolith $\rightarrow$ PostgreSQL Pool), custom GUC settings (`app.current_user_id`, `app.current_family_id`) represent **trusted application context** passed by authenticated backend processes over authenticated connection pool roles (`family_app`, `family_worker`).
   * PostgreSQL does not independently verify JWT signatures; PostgreSQL enforces tenant row-level security and relational integrity assuming the application server correctly validates client credentials and injects context via `SELECT set_config(..., true)`.
2. **Anti-Enumeration & RLS Recursion Defense:**
   * When an authenticated user connects without an active family context (e.g. during login or `/auth/me` to list available families), evaluating membership policies via a subquery on `family_members` inside `family_members` RLS causes infinite recursion.
   * Furthermore, helper functions must never accept an arbitrary `p_user_id` parameter that could allow enumeration of other users' memberships.
   * Discovery uses a dedicated parameterless helper function:
     ```sql
     CREATE FUNCTION public.get_current_user_family_ids()
     RETURNS SETOF UUID ...
     ```
   * Owned by dedicated internal role `family_auth_definer` (`NOLOGIN`), with a dedicated non-recursive RLS policy on `family_members` comparing `user_id = NULLIF(current_setting('app.current_user_id', true), '')::UUID`.
   * Callers cannot tamper with user arguments; missing or empty `app.current_user_id` strictly evaluates to an empty result (0 rows).
3. **Registration Bootstrap:**
   * During initial registration (`POST /auth/register`), the user creates both their user profile and an initial family space.
   * Executed within a registration transaction:
     1. Insert into `users`.
     2. Insert into `families`.
     3. Insert into `family_members` with role `'owner'`.
     4. Call `SELECT set_config('app.current_family_id', family_id, true)` and seed default system categories.
     5. Commit transaction and issue JWT token with `family_id` claim.
4. **Table Owner Bypass Prevention (`FORCE ROW LEVEL SECURITY`):**
   * All tenant-owned tables enforce `FORCE ROW LEVEL SECURITY`.
   * This guarantees that even if a database connection is established by a role with table ownership, RLS policies are strictly enforced and cannot be bypassed without explicit superuser intervention.

---

## 7. BankConnector Go Interface Specification

```go
package connector

import (
	"context"
	"time"
)

type CurrencyAmount struct {
	MinorUnits int64  // e.g. 150050 for 1,500.50 RUB
	Currency   string // ISO 4217, e.g. "RUB", "USD", "EUR"
}

type AccountDTO struct {
	ExternalID       string
	Name             string
	Type             string // "checking", "credit_card", "savings", "deposit"
	Currency         string
	Balance          *CurrencyAmount // Nullable if not provided
	AvailableBalance *CurrencyAmount
	DebtAmount       *CurrencyAmount
	CreditLimit      *CurrencyAmount
	StatementBalance *CurrencyAmount
	MinimalPayment   *CurrencyAmount
	GracePeriodDate  *time.Time
	Cards            []CardDTO
}

type CardDTO struct {
	ExternalID    string
	MaskedPAN     string // e.g. "*1234"
	DisplayName   string
	PaymentSystem string // "MIR", "VISA", "MASTERCARD"
	IsVirtual     bool
	Status        string // "ACTIVE", "BLOCKED", "CLOSED"
}

type BankTransactionDTO struct {
	ExternalID          string          // Bank operation ID (operationId / id)
	ExternalAuthID      *string         // Card network authorization hold ID
	AccountExternalID   string
	CardExternalID      *string
	Amount              CurrencyAmount  // Nominal transaction amount
	AccountAmount       CurrencyAmount  // Cleared amount in account currency
	TransactionTime     time.Time
	SettledTime         *time.Time      // Nil if still pending/hold
	BookingStatus       string          // "pending", "posted", "cancelled"
	OperationType       string          // "purchase", "refund", "transfer_out", "transfer_in", "atm_cash", "fee", "card_check"
	RelatedExternalID   *string         // Reference to refunded operation if available
	MerchantName        string
	RawDescription      string
	MCC                 *int
	BankCategory        string
	RawJSON             []byte
}

type SyncParams struct {
	StartDate time.Time
	EndDate   time.Time
	Accounts  []string
}

type BankConnector interface {
	ProviderCode() string
	HealthCheck(ctx context.Context, config map[string]any) error
	FetchAccounts(ctx context.Context, config map[string]any) ([]AccountDTO, error)
	FetchTransactions(ctx context.Context, config map[string]any, params SyncParams) ([]BankTransactionDTO, error)
}
```

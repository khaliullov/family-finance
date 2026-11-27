# Generic Statement Ingestion, Review & Ledger Promotion Specification (Stage 6B.2)

## 1. Overview & Architecture

The Generic Statement Ingestion workflow provides an authenticated, non-repudiable, and tamper-resistant mechanism for importing bank statements into the Family Finance ledger.

### Non-Negotiable Financial Guarantees
1. **Zero Credential Exposure (LF-SIA):** Ingestion is strictly file-based. No banking credentials, session cookies, SMS codes, or online banking tokens are accepted or processed.
2. **Double-Entry Ledger Immutability:** Staging a statement never mutates the financial ledger (`bank_transactions` or `accounting_entries`).
3. **Database Guard Against Cancelled Operations:** Cancelled bank operations (`booking_status = 'cancelled'`) are strictly prohibited from entering `bank_transactions` or `accounting_entries` by database CHECK constraint `chk_staged_cancelled_no_commit`.
4. **Monotonic Balance Semantics:** Historical statements cannot overwrite a newer account balance. The account's `current_balance_minor` is only updated when closing balances are reconciled and the statement's as-of date (`period_end_date`) is strictly newer than `balance_updated_at`.
5. **Envelope Encryption & Tenant Isolation:** Raw files are encrypted with AES-256-GCM via the STMT envelope format before database persistence, binding the ciphertext to `family_id` and `document_id` via Additional Authenticated Data (AAD).

---

## 2. Ingestion Lifecycle State Machine

```
  +-------------------------------------------------------+
  |                                                       |
  |  POST /api/v1/imports (Multipart Upload)              |
  |  - Validates tenant, target account, format           |
  |  - Encrypts raw statement in STMT envelope            |
  |  - Sniffs and parses via ParserRegistry               |
  |  - Evaluates balance reconciliation                   |
  |  - Matches fingerprints & pending holds               |
  |  - Inserts staged_bank_transactions buffer            |
  |                                                       |
  +---------------------------+---------------------------+
                              |
                              v
                        [  staged  ] <======================+
                              |                             |
                              | (Review & Edits via PATCH)  | (Row Mutation
                              |                             |  Revokes Approval)
                              v                             |
  +---------------------------+---------------------------+ |
  | POST /api/v1/imports/{id}/approve                     | |
  | - Validates no pending resolution actions             | |
  | - Validates reconciliation discrepancy overrides      | |
  | - Records approver user ID, timestamp, and revision   | |
  +---------------------------+---------------------------+ |
                              |                             |
                              v                             |
                        [ approved ] -----------------------+
                              |
                              | POST /api/v1/imports/{id}/promote
                              v
                        [ promoting ]
                              |
                              v
                        [ committed ] (Terminal Immutable State)
                              
  * At any stage prior to committed:
    POST /api/v1/imports/{id}/cancel ===> [ cancelled ]
```

### Lifecycle States
* **`uploaded` / `parsing`**: Transient states during multipart stream ingestion and document parsing.
* **`staged`**: Statement rows are parsed and staged in `staged_bank_transactions` awaiting user review.
* **`approved`**: The exact revision of the batch and its staged rows have been explicitly reviewed and approved by an authorized family member.
* **`promoting`**: Active execution of double-entry ledger insertion, pending hold settlement, and account balance snapshot update under `FOR UPDATE` transaction lock.
* **`committed`**: Terminal immutable state. Transactions have been recorded in `bank_transactions` and default `accounting_entries`. Row modification or deletion is permanently locked by database trigger `trg_enforce_statement_batch_lifecycle`.
* **`cancelled`**: The import batch was rejected or cancelled prior to ledger promotion.

---

## 3. Supported Statement Formats

| Profile ID | Provider / Bank | Format | Encoding | Header Signature |
| :--- | :--- | :--- | :--- | :--- |
| `tbank_csv_v1` | T-Bank (Tinkoff) | Semicolon-delimited CSV | UTF-8 with BOM, UTF-8, Windows-1251 | `Имя счёта;Дата операции;Сумма операции;...` |

### Auto-Sniffing
The system inspects the first 4096 bytes of the uploaded file. If `bank_format` is omitted, the registry automatically matches the file against registered format profiles.

---

## 4. API Reference & Synthetic Examples

### 4.1 Upload Statement
`POST /api/v1/imports` (or `POST /imports/upload`)

**Headers:**
`Authorization: Bearer <JWT_ACCESS_TOKEN>`
`Content-Type: multipart/form-data`

**Form Fields:**
* `account_id` (UUID, required): Target account in the user's family.
* `file` (Binary, required): Statement file up to 10 MiB.
* `bank_format` (String, optional): Parser format identifier (e.g. `tbank_csv_v1`).

**Example Response (201 Created):**
```json
{
  "import_id": "7b0a887a-6415-46c5-a6e5-407a50fe864a",
  "account_id": "3e9b1148-5c4d-44a6-a36f-a895b6d510f2",
  "status": "staged",
  "batch_revision": 1,
  "source_format": "tbank_csv_v1",
  "parser_version": "1.0.0",
  "original_filename": "Operations_Nov2026.csv",
  "total_rows": 24,
  "eligible_rows": 22,
  "duplicate_rows": 1,
  "pending_rows": 1,
  "cancelled_rows": 1,
  "transfer_candidates": 2,
  "refund_candidates": 1,
  "ambiguous_rows": 0,
  "discrepancy_count": 0,
  "reconciliation_status": "reconciled",
  "opening_balance_minor": 15000000,
  "closing_balance_minor": 14250000,
  "calculated_balance_minor": 14250000,
  "period_start_date": "2026-11-01T00:00:00Z",
  "period_end_date": "2026-11-20T23:59:59Z",
  "created_at": "2026-11-27T10:00:00Z",
  "updated_at": "2026-11-27T10:00:00Z"
}
```

---

### 4.2 Get Import Preview
`GET /api/v1/imports/{id}/preview`

Exposes non-sensitive financial preview records with deduplication indicators and validation warnings.

**Example Response (200 OK):**
```json
{
  "summary": {
    "import_id": "7b0a887a-6415-46c5-a6e5-407a50fe864a",
    "account_id": "3e9b1148-5c4d-44a6-a36f-a895b6d510f2",
    "status": "staged",
    "batch_revision": 1,
    "total_rows": 2
  },
  "rows": [
    {
      "id": "e458564f-4d9f-4318-8f55-06a92ebf68a4",
      "row_index": 1,
      "transaction_time": "2026-11-20T14:30:00Z",
      "settled_time": "2026-11-23T10:15:00Z",
      "amount_minor": -150000,
      "currency": "RUB",
      "account_amount_minor": -150000,
      "account_currency": "RUB",
      "raw_description": "ВкусВилл",
      "merchant_name": "ВкусВилл",
      "bank_category": "Супермаркеты",
      "category_id": null,
      "booking_status": "posted",
      "operation_type": "purchase",
      "duplicate_status": "settlement_update",
      "matched_existing_tx_id": "1fa067e4-2364-4e4b-9721-c27771960249",
      "resolution_action": "apply_settlement",
      "is_eligible_for_promotion": true,
      "requires_review": false,
      "warnings": []
    },
    {
      "id": "a91724d2-f67b-4ef9-8a35-c335e2ba5e22",
      "row_index": 2,
      "transaction_time": "2026-11-20T15:00:00Z",
      "amount_minor": -50000,
      "currency": "RUB",
      "account_amount_minor": -50000,
      "account_currency": "RUB",
      "raw_description": "Бургер Кинг",
      "bank_category": "Фастфуд",
      "booking_status": "cancelled",
      "operation_type": "purchase",
      "duplicate_status": "new",
      "resolution_action": "skip_cancelled",
      "is_eligible_for_promotion": false,
      "requires_review": false,
      "warnings": [
        "Cancelled bank operation: prohibited from ledger promotion"
      ]
    }
  ]
}
```

---

### 4.3 Review & Update Staged Row
`PATCH /api/v1/imports/{id}/rows/{row_id}`

Allows reclassification, category assignment, or resolution modification. Mutating any staged row increments `batch_revision` and automatically revokes any prior approval snapshot.

**Request Body:**
```json
{
  "category_id": "893c5d6e-0d8b-4a55-8d59-323e08f2a58b",
  "operation_type": "purchase",
  "resolution_action": "commit"
}
```

*Note: Attempting to set `"resolution_action": "commit"` on a row with `booking_status = "cancelled"` is rejected with `400 Bad Request`.*

---

### 4.4 Explicit Approval
`POST /api/v1/imports/{id}/approve`

**Request Body (optional):**
```json
{
  "notes": "Reviewed and approved for November reconciliation"
}
```

**Pre-conditions:**
* Batch must be in `staged` status.
* Zero rows with `resolution_action = 'pending'`.
* Zero cancelled rows with `resolution_action = 'commit'`.
* If reconciliation discrepancy exists, an attributable override must have been recorded via `/override`.

---

### 4.5 Atomic Ledger Promotion
`POST /api/v1/imports/{id}/promote` (or `POST /imports/{id}/confirm`)

Promotes approved rows into `bank_transactions` and default `accounting_entries`, transitions settled pending holds via `settle_pending_bank_transaction`, updates `current_balance_minor` monotonically, and marks batch `committed`.

**Example Response (200 OK):**
```json
{
  "batch_id": "7b0a887a-6415-46c5-a6e5-407a50fe864a",
  "transactions_committed": 21,
  "transactions_settled": 1,
  "transactions_skipped": 2,
  "balance_snapshot_updated": true,
  "new_account_balance_minor": 14250000,
  "balance_updated_at": "2026-11-20T23:59:59Z"
}
```

---

## 5. Settlement & Deduplication Matching Rules

### Multiplicity Matching (Priority 2)
Legitimate identical transactions occurring on the same day with the same description and amount (e.g. multiple 50.00 RUB metro tickets) are supported without collision. Deduplication evaluates the occurrence count within the batch against existing database sequence counts.

### Pending-to-Posted Settlement Transition (Priority 3)
When a staged row is `posted`:
1. The system queries `bank_transactions` for uncleared `booking_status = 'pending'` holds on the same account matching `account_amount_minor` within a +/- 7-day clearing window.
2. If matched: `DuplicateStatus` is marked `settlement_update` and `ResolutionAction` defaults to `apply_settlement`.
3. During promotion: the transition is executed via the `settle_pending_bank_transaction` routine.
4. The existing `bank_transactions` record is updated to `posted`, recording `posted_time`.
5. The existing `accounting_entries` record is synchronized.
6. Zero duplicate transactions are created.

---

## 6. Security & Audit Logging

* **RLS Tenant Isolation:** All operations enforce `family_id = get_current_family_id()` with `FORCE ROW LEVEL SECURITY`.
* **Append-Only Audit Trail:** Lifecycle events (`statement_uploaded`, `staged_row_updated`, `statement_batch_approved`, `statement_batch_promoted`, `statement_batch_cancelled`, `reconciliation_override_created`) are recorded in `public.audit_logs`.
* **Strict Confidentiality:** Raw uploaded files, customer passwords, PINs, card CVVs, and encryption keys are strictly excluded from logs and audit events.

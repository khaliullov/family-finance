# Deduplication & Conservative Transfer Matcher Specification

## 1. Overview & Objectives

In a multi-account family accounting system with two partners, the integrity of financial analytics hinges on:
1. **Deduplication & Lifecycle State Tracking:** Ensuring that bank sync runs, statement imports, authorization holds, and post-clearing settlements never create duplicate transactions.
2. **Conservative Transfer Matching Engine:** Pairing money movements across internal accounts (e.g. transfers between spouses via SBP, moving money from debit to savings, or repaying credit card balances) so that transfers **do not falsely inflate family income or family expenses**, while strictly preventing false-positive matches from erasing real merchant expenses.

---

## 2. Ingestion Deduplication Architecture

Every incoming transaction passes through a multi-layer deduplication pipeline before reaching the ledger.

```mermaid
flowchart TD
    In[Incoming Normalized Bank Transaction DTO] --> C1{Has Bank External ID?}
    
    C1 -->|Yes| D1[Check Natural Key: account_id + external_id]
    D1 --> R1{Exists in DB?}
    R1 -->|Yes| L1[Evaluate Lifecycle State Transition]
    R1 -->|No| I1[Insert into bank_transactions & create default accounting_entry]
    
    C1 -->|No / File Import| D2[Compute Synthetic Fingerprint Hash & Sequence]
    D2 --> R2{Fingerprint Hash Exists in DB?}
    R2 -->|Yes & Same Sequence| S1[Duplicate Detected: Skip Ingestion]
    R2 -->|No / New Sequence| I1
```

### 2.1 Natural Key Deduplication (Bank Sync)
- **Constraint:** `UNIQUE (family_id, account_id, external_id) WHERE external_id IS NOT NULL`
- When the bank provides a stable operation identifier (`id` or `operationId`), this constraint guarantees strict idempotency across overlapping sync intervals.

### 2.2 Synthetic Fingerprint Hash & Collision Disambiguation (Statement Imports)
For CSV/XLSX imports where the bank does not export stable global GUIDs:
$$\text{Fingerprint} = \text{SHA-256}\Big(\text{account\_id} \parallel \text{DATE(transaction\_time)} \parallel \text{amount\_minor} \parallel \text{currency} \parallel \text{Normalize}(\text{raw\_description})\Big)$$
- **Disambiguation via `fingerprint_sequence`:**
  If a partner buys two identical 200 RUB tickets at the same vendor on the same day, both rows produce the exact same SHA-256 fingerprint.
  The importer numbers identical occurrences sequentially within the statement batch:
  - First occurrence: `fingerprint_sequence = 1`
  - Second occurrence: `fingerprint_sequence = 2`
  Constraint `UNIQUE(family_id, account_id, fingerprint_hash, fingerprint_sequence)` ensures exact, non-colliding imports.

---

## 3. Transaction Lifecycle State Transitions

Bank operations evolve through distinct chronological phases:
1. **`pending` (Authorization / Hold):** Merchant reserved funds. Balance is reduced, but funds have not cleared.
2. **`posted` (Cleared / Settled):** Clearing file debited account.
3. **`cancelled` (Reversal / Expired Hold):** Authorization voided or expired.
4. **`refund` (Return):** Separate credit operation issued by merchant after clearing.

### 3.1 Non-Interchangeable Bank Identifiers
T-Bank responses contain three distinct identifier fields:
- `id`: Timeline item identifier.
- `operationId`: Bank ledger operation identifier.
- `authorizationId`: Card network authorization hold code.

**Deterministic Lifecycle Pipeline:**
1. **Direct ID Match:** If incoming operation has `external_id == existing.external_id`:
   - If existing is `pending` and incoming is `posted`, update `booking_status = 'posted'`, `posted_time = incoming.posted_time`, and `account_amount_minor = incoming.account_amount_minor`.
2. **Authorization Code Resolution:** If an incoming operation has a new `external_id`, but has `authorizationId` matching an existing record with `external_auth_id == incoming.authorizationId` on the same `account_id` where `booking_status = 'pending'`:
   - Transition pending record: set `external_id = incoming.external_id`, `booking_status = 'posted'`, update `amount_minor` to settled amount, and set `posted_time`.
   - Prevent 1:N or N:1 collisions: an authorization code can resolve **at most one** pending hold.
3. **Cancellation vs. Expiry Conditions:**
   - An authorization is **never** cancelled merely because it is absent from a single sync response.
   - It transitions to `cancelled` only if the bank explicitly reports it as failed/voided, or after a complete, successfully paginated sync covering 14 days completes without the authorization appearing.

### 3.2 Refund & Reversal Representation
- **Reversals (Pre-clearing):** The pending hold is updated to `booking_status = 'cancelled'`. Linked accounting entries are set to `classification = 'excluded'`.
- **Refunds (Post-clearing):** A refund is **NEVER a mutation of the original transaction**. It is ingested as a distinct `bank_transaction` (`operation_type = 'refund'`) with positive `amount_minor` and `related_bank_transaction_id` referencing the original purchase.
- **Accounting Impact:** The refund's `accounting_entry` is assigned the **original purchase's category** with a positive amount. In analytics, this automatically nets out the category expense without inflating gross income. Multiple or partial refunds are supported naturally (each as its own linked refund transaction).

---

## 4. Conservative Transfer Matcher Specification

### 4.1 Strict Elimination of False Positives

> [!WARNING]
> **Why MCC Alone is Not Proof of an Internal Transfer:**
> MCC codes like `4829` (Wire Transfer), `6012` (Financial Institution), or `6538` (P2P Transfer) indicate that money was transferred, but **do not prove it was an internal family transfer**. A user could be paying a landlord, sending money to a friend, or funding a brokerage account.
> **Mandatory Rule:** MCC codes or transfer categories alone **must never trigger Tier 1 automatic confirmation**. Tier 1 auto-confirmation requires strong reciprocal evidence connecting two known family accounts.

### 4.2 Multi-Tier Matching Heuristics

```mermaid
flowchart TD
    Out[Unmatched Outflow Transaction A: amount < 0] --> Search[Search Candidate Inflows B in family_id: amount > 0]
    
    Search --> T1{Tier 1 Check:
    abs(A.amount) == B.amount AND
    time_delta <= 30 min AND
    Same Currency AND
    Both accounts in family AND
    Reciprocal Evidence Present AND
    Not in transfer_rejections}
    
    T1 -->|Pass| M1[Tier 1 Exact Match
    Heuristic Band: High Confidence
    Status: auto_confirmed
    Accounting: classification='transfer']
    
    T1 -->|Fail| T2{Tier 2 Check:
    time_delta <= 72 hours AND
    (amount_diff <= fee_threshold OR SBP hints) AND
    Both accounts in family AND
    Not in transfer_rejections}
    
    T2 -->|Pass| M2[Tier 2 Probable Match
    Heuristic Band: Probable Match
    Status: suggested
    Awaiting User 1-Click Confirmation]
    
    T2 -->|Fail| T3[Tier 3 Manual Pool
    Available in UI Manual Link Dialog]
```

#### Tier 1: Exact Match (`match_type = 'exact'`, Status: `auto_confirmed`)
*High Heuristic Confidence.* All of the following must hold:
1. **Opposite Directions:** $\text{source.amount\_minor} < 0$ and $\text{destination.amount\_minor} > 0$.
2. **Account Disjointness:** $\text{source.account\_id} \neq \text{destination.account\_id}$ (both belonging to the same `family_id`).
3. **Exact Amount & Currency:** $|\text{source.amount\_minor}| == \text{destination.amount\_minor}$ and `source.currency == destination.currency`.
4. **Tight Time Window:** $|\text{source.transaction\_time} - \text{destination.transaction\_time}| \le 30\text{ minutes}$ (SBP and intra-bank transfers settle in seconds).
5. **MANDATORY Reciprocal Semantic Evidence:**
   - **Case A (SBP Reciprocal Hints):** Source description indicates outgoing SBP and destination indicates incoming SBP (e.g., source: `"Перевод по СБП: Анна И."`, destination: `"Пополнение через СБП: Иван И."`).
   - **Case B (Credit Card Repayment):** Destination account is `type = 'credit_card'`, source account is checking/savings, and description matches debt repayment signatures.
   - **Case C (Intra-Bank Account Transfer):** Both accounts are at the same bank connection, and bank metadata marks operation as internal transfer between user's accounts (`"Перевод между своими счетами"`).
6. **Negative Constraint:** Neither transaction is a merchant purchase (MCC 5411, 5812, etc.) or ATM cash withdrawal.
7. **Rejection Check:** The pair does not exist in `transfer_rejections`.

*If amounts and times match but Reciprocal Evidence (Condition 5) is absent, the match **must not be auto-confirmed**; it is placed in Tier 2 (`suggested`) for user review.*

#### Tier 2: Probable Match (`match_type = 'probable'`, Status: `suggested`)
*Probable Heuristic Match.* Surfaces in the "Transfers to Review" inbox for 1-click user confirmation:
- **Scenario 2A (Inter-Bank Delay):** Same amount, same currency, but clearing took longer ($30\text{ min} < |\Delta t| \le 72\text{ hours}$).
- **Scenario 2B (Transfer Fee):** Outflow is slightly larger than inflow due to inter-bank fee:
  $$0 < (|\text{source.amount}| - \text{destination.amount}) \le \max(50000 \text{ minor units [500 RUB]}, 0.015 \times \text{destination.amount})$$
  Difference is captured as `fee_minor`.
- **Scenario 2C (Unverified Evidence):** Matching amounts and close timestamps, but bank descriptions lack reciprocal proof.

#### Tier 3: Manual Match (`match_type = 'manual'`)
- User selects any two transactions in the UI and clicks "Link as Internal Transfer".
- **Cross-Currency Transfers:** Because exchange rates fluctuate, transfers between different currencies (e.g. RUB to USD) are **never auto-confirmed**. They are routed to Tier 3 where the user confirms the pairing and verifies the implied exchange rate.

---

### 4.3 Impact on Accounting Entries & Rematching Lifecycle

1. **Accounting Impact:**
   - When a match becomes `auto_confirmed` or `confirmed`:
     - The linked `accounting_entries` for both transactions have their `classification` updated to `'transfer'` (or `'debt_repayment'`).
     - If an entry was previously categorized, the original `category_id` is retained in the row but ignored by real expense analytics.
2. **User Rejection & Unlinking:**
   - If the user rejects a suggestion or unlinks an existing match:
     - `transfer_matches.status` transitions to `'rejected'` (or the match is removed).
     - The linked `accounting_entries` automatically restore their classification to `'expense'` / `'income'`.
     - An entry is inserted into `transfer_rejections(family_id, source_id, destination_id, rejected_by_user_id)`.
     - The partial unique indexes immediately unblock both transactions, allowing them to match with alternative candidates on subsequent syncs.
3. **Settlement Drift on Matched Transfers:**
   - If an authorization hold was matched as a transfer, and the settled amount drifts beyond the fee threshold upon posting:
     - The match status transitions from `auto_confirmed` to `suggested` with an alert flag: *"Settlement amount altered after transfer match. Please review."*

---

## 5. Credit Card Repayment Modeling

```
[ Step 1: Purchase on Credit Card ]
bank_transactions:
  account_id: Credit Card
  amount_minor: -500,000 (-5,000.00 RUB)
accounting_entries:
  classification: 'expense'
  category: 'Groceries'
  amount_minor: -500,000
Effect:
  Family Expense increases by 5,000 RUB.
  Credit Card debt_balance increases by 5,000 RUB.
  Net worth decreases by 5,000 RUB.

[ Step 2: Repayment from Checking Account ]
bank_transactions (Outflow):
  account_id: Checking Account
  amount_minor: -500,000
bank_transactions (Inflow):
  account_id: Credit Card
  amount_minor: +500,000
transfer_matches:
  is_credit_card_repayment: true
accounting_entries (Outflow & Inflow):
  classification: 'debt_repayment'
Effect:
  Family Expense = ZERO.
  Checking current_balance decreases by 5,000 RUB.
  Credit Card debt_balance decreases by 5,000 RUB.
  Net worth is UNCHANGED.
```

---

## 6. Comprehensive Transfer Matcher Test Matrix

Coding agents must verify the matcher against the following deterministic test scenarios:

| Test ID | Scenario | Source Tx | Destination Tx | Expected Tier | Expected Status | Ledger & Analytics Impact |
| :--- | :--- | :--- | :--- | :--- | :--- | :--- |
| **TC-TR-01** | Standard SBP transfer between partners | -10,000 RUB, checking A, "Перевод СБП: Анна", 12:00 | +10,000 RUB, checking B, "Пополнение СБП: Иван", 12:01 | Tier 1 | `auto_confirmed` | Both entries `classification = 'transfer'`. Expense = 0. |
| **TC-TR-02** | Unrelated simultaneous merchant purchase & income (False Positive Defense) | -3,500 RUB, checking A, MCC 5411 "Пятерочка", 14:00 | +3,500 RUB, checking B, "Клиентский платеж", 14:15 | None / Tier 3 | Unmatched | **NOT MATCHED.** Source remains Grocery Expense; Dest remains Income. |
| **TC-TR-03** | Credit card repayment from checking | -45,000 RUB, checking A, "Оплата карты *1234", 10:00 | +45,000 RUB, credit card A, "Пополнение карты", 10:05 | Tier 1 | `auto_confirmed` | Both entries `classification = 'debt_repayment'`. Expense = 0. Debt balance reduced. |
| **TC-TR-04** | Inter-bank transfer with 50 RUB fee | -50,050 RUB, checking A, "Перевод в другой банк", 11:00 | +50,000 RUB, checking B, "Входящий перевод", 11:02 | Tier 2B | `suggested` | Suggested with `fee_minor = 5,000` (50 RUB). Requires 1-click confirmation. |
| **TC-TR-05** | Delayed weekend bank clearing | -20,000 RUB, checking A, Friday 18:00 | +20,000 RUB, checking B, Monday 10:00 | Tier 2A | `suggested` | Suggested due to 64-hour time delta. |
| **TC-TR-06** | Cross-currency transfer | -100,000 RUB, checking A, 15:00 | +1,050 USD, savings B, 15:05 | Tier 3 | Manual pool only | **Never auto-confirmed.** User must manually link and confirm FX rate. |
| **TC-TR-07** | Rejection and rematching | Suggested pair rejected by user | Same source tx paired with true destination tx later | Tier 1 / 2 | Rematched cleanly | Rejected pair recorded in `transfer_rejections`; source freed to match true counterpart. |

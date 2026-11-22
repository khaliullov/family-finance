# Bank Data Ingestion & T-Bank Technical Discovery Plan

## 1. Executive Summary & Reality Check

In consumer financial accounting within the Russian banking ecosystem (and specifically T-Bank / Tinkoff), there is a fundamental architectural reality:
1. **Public REST APIs (T-API / Tinkoff Open API):** These officially target **Corporate / Business accounts** (`Т-Бизнес` / SME / Sole Proprietorships). They require corporate API tokens, OpenAPI contracts, and webhooks geared toward merchant acquiring and corporate treasury. **Retail / Personal cards and debit accounts are NOT exposed via public developer OAuth2/REST APIs.**
2. **Retail Online Banking:** Access to personal banking (`Т-Банк Онлайн`) is gated behind multi-factor authentication (SMS/Push OTP, session cookies, dynamic device fingerprints, and fraud-detection telemetry).
3. **Open Banking Initiatives:** The Central Bank of Russia (CBR) and the FinTech Association have launched Open Banking standards, but full retail Account Information Services (AIS) for third-party self-hosted personal software remain in pilot stages.

Therefore, building a self-hosted personal family accounting system **must not rely on a naive assumption of official retail personal API keys**. A multi-modal ingestion strategy is mandatory.

---

## 2. Ingestion Strategy Decision Matrix

| Ingestion Path | Feasibility for Retail T-Bank | Setup Complexity for Partners | Maintenance Burden | Security & Credential Hygiene | Production Role |
| :--- | :--- | :--- | :--- | :--- | :--- |
| **Path A: Official Business T-API** | ❌ **Inapplicable** to personal debit/credit accounts (only applicable if a partner has a Sole Proprietorship/ИП). | Low (API Token) | Very Low | High (Official Scoped Token) | Optional secondary module for self-employed/business transactions. |
| **Path B: Local Authenticated Browser-Context Bridge (`tbank-capture`)** | ✅ **High** (reusing authenticated Playwright/Chromium session). | Medium (Partner logs in once via browser UI; session persisted locally). | Medium (Vulnerable to web-app client endpoint changes). | **High** (Zero password/OTP storage; runs on local machine/Docker; same-origin read-only GET requests). | **Primary Automated Path for Retail T-Bank**. |
| **Path C: FinTech Aggregators / Open Banking / Email e-Receipts (OFD)** | ⚠️ **Limited / Future** (Aggregators often require corporate licensing or screen scraping; OFD gives receipt items, not account balances). | High | High (External dependency) | Moderate | Deferred (Out of scope for MVP). |
| **Path D: Bank Statement Import (CSV / XLSX / 1C)** | ✅ **100% Guaranteed** (Native 1-click export from mobile app or web banking). | Very Low (Download & drag-and-drop file). | Very Low (Standard tabular format). | **Maximum** (Zero credentials exchanged; offline parsing). | **Mandatory Baseline Fallback & Zero-Day Support**. |

### Strategic Recommendation
- **Primary Automated Ingestion:** Local sidecar runner utilizing `tbank-capture` architecture (reusing user-authenticated Chromium context for same-origin read-only GET calls, zero credential harvesting).
- **Mandatory Fail-Safe Ingestion:** High-speed CSV/XLSX statement parser supporting T-Bank, Sber, Alfa-Bank, and generic formats.
- **Strict Rule-Out:** Fragile headless background screen-scraping with automated password/OTP submission (violates bank ToS, triggers immediate fraud blockades, and presents unacceptable security hazards).

---

## 3. Analysis of Discovered T-Bank Retail Endpoints

Based on session `sess-20261122T145129Z-5b208342` from `tbank-capture` (587 eligible JSON responses, 85 endpoint fingerprints):

### 3.1 Transaction History Endpoint
- **Discovered Path:** `GET /mybank/api/operations/timeline/public/legacy/v1/operations`
- **Classification Confidence:** 0.95
- **Payload Schema Highlights:**
  - `id`: Provider unique operation string (e.g., `"1234567890"`).
  - `operationId`: Secondary provider operation ID.
  - `authorizationId`: Card authorization hold ID (crucial for linking pending authorizations to final settled transactions).
  - `account`: Provider account identifier.
  - `amount.value`: Nominal transaction amount (decimal/integer).
  - `amount.currency`: Currency code (e.g., `{"code": 643, "name": "RUB"}`).
  - `accountAmount.value` / `accountAmount.currency`: Actual debited/credited amount in the account's home currency (handles FX discrepancies).
  - `operationTime.milliseconds`: Transaction timestamp in Unix milliseconds.
  - `debitingTime.milliseconds`: Final bank settlement/clearing timestamp in Unix milliseconds (present when settled).
  - `merchant.name` / `brand.name`: Merchant branding.
  - `mcc`: ISO 18245 Merchant Category Code (e.g., `5411` for Supermarkets).
  - `category` / `spendingCategory` / `subcategory`: Bank's proprietary classification.
  - `status`: Lifecycle state (`"OK"`, `"FAILED"`, `"PENDING"`).
  - `analytics.status`: Analytical state.
  - `cashback` / `cashbackAmount`: Loyalty points/cash rewards earned (must be kept separate from transaction amounts).
  - `payment` / `refund`: Flags indicating whether the operation is a credit/debit or a return.
  - `cardNumber`: Masked card identifier (e.g., `*1234`) indicating which physical/virtual card was used on the account.

### 3.2 Account & Card Metadata Endpoints
- **Discovered Paths:**
  - `GET /api/common/v1/accounts_light_ib`
  - `GET /api/common/v1/account_details_ib`
- **Payload Schema Highlights:**
  - `id`: Account identifier.
  - `name`: User-customized or product name (e.g., `"Black"`, `"All Airlines"`).
  - `accountType`: Account classification (`"Current"`, `"Credit"`, `"Deposit"`, `"Savings"`).
  - `moneyAmount.value`: Current ledger balance.
  - `debtAmount.value` / `debtBalance.value`: For credit cards, current utilized debt.
  - `creditLimit.value`: Total approved credit facility.
  - `currentMinimalPayment`: Mandatory minimum payment due.
  - `lastStatementDate` / `nextStatementDate`: Grace period cycle boundaries.
  - `cards[]`: Array of linked physical and virtual cards:
    - `id`: Card ID.
    - `name`: Embossed name or card label.
    - `paymentSystem`: `"MIR"`, `"VISA"`, `"MASTERCARD"`.
    - `status`: `"ACTIVE"`, `"CLOSED"`, `"BLOCKED"`.
    - `isVirtual`: Boolean.
    - `primary`: Boolean.

### 3.3 Statements & Periodic Summaries
- **Discovered Path:** `GET /api/common/v1/statements`
- **Fields:** `period.start`, `period.end`, `periodStartBalance`, `periodEndBalance`, `income`, `expense`, `balance`, `billedDebt`, `minimalPaymentAmount`.
- **Architectural Use:** Used for **audit reconciliation**. The system cross-checks sum of recorded transactions against statement `income` and `expense` to verify zero data loss.

### 3.4 Investment Accounts (Isolated Domain)
- **Discovered Paths:** `GET /mybank/api/investbox/account/all`, `GET /mybank/api/invest-portfolio/portfolios/accounts`
- **Strict Domain Boundary:** Kept strictly isolated in an `investments` read-only module. Investment portfolio valuations are **never** summed into liquid cash balances or family operating cash-flow.

---

## 4. Multi-Partner Consent & Authentication Flow

In a two-partner family (`family_id`):
- **Partner A (e.g., User)** has personal accounts at T-Bank and Sber.
- **Partner B (e.g., Spouse)** has personal accounts at T-Bank and Alfa-Bank.
- Neither partner should know or share the other's banking master password or phone OTP.

```
       [Partner A Laptop / Browser]                  [Partner B Laptop / Browser]
                     │                                             │
      Logs into personal T-Bank web                 Logs into personal T-Bank web
                     │                                             │
      ┌──────────────┴──────────────┐               ┌──────────────┴──────────────┐
      │ tbank-capture Sidecar / App │               │ tbank-capture Sidecar / App │
      │  (Instance A - Token A)     │               │  (Instance B - Token B)     │
      └──────────────┬──────────────┘               └──────────────┬──────────────┘
                     │ Push Sync (Local/Mutual Auth)               │ Push Sync (Local/Mutual Auth)
                     ▼                                             ▼
       ┌──────────────────────────────────────────────────────────────────────────┐
       │                Family Finance Core Server (Modular Monolith)             │
       │                                                                          │
       │  Connection A (Owner: Partner A)           Connection B (Owner: Partner B)│
       │  Bank: T-Bank                              Bank: T-Bank                  │
       │  family_id: 101                            family_id: 101                │
       └──────────────────────────────────────────────────────────────────────────┘
```

### Protocol for Partner Session Linking:
1. **Sidecar Token Registration:** The core server generates a cryptographically random, single-use Pairing Secret for each bank connection created in the UI under that partner's user profile.
2. **Local Session Execution:**
   - Partner launches the local `tbank-capture` runner on their workstation or in a dedicated container with a mapped browser profile directory (`userDataDir`).
   - Partner enters phone number and completes standard SMS/Push 2FA in the opened Chromium browser window.
   - Once authenticated, `tbank-capture` enters continuous or scheduled sync mode.
3. **Zero Credential Transmission:** The browser cookies, tokens, and storage state **never leave the local Chromium context**. The sidecar extracts only the sanitized, typed JSON responses from the bank and pushes them to `Family Finance` via HTTPS using an API Key.
4. **Session Expiry & Alerting:** When T-Bank expires the web session (HTTP 401/403 or redirect to login), `tbank-capture` marks the connection status as `EXPIRED` and notifies the core server. The UI displays an alert badge: *"Partner B: T-Bank session expired. Please re-authenticate."*

---

## 5. Safe Request Contract Discovery & Validation Plan

Before writing rigid pagination code, we must systematically inspect query parameters and request structures without capturing sensitive payloads.

### Discovery Protocol Steps:
1. **Extend `tbank-capture` Request Interceptor:**
   - Listen to `page.on('request')` and `page.on('response')`.
   - Strip out `Cookie`, `Authorization`, and `x-auth-token` headers.
   - Record HTTP Method, Hostname, Normalized Path, and Query Parameter Names.
2. **Determine Query Parameter Semantics for Operations:**
   - Typical observed variants:
     - `start`: Unix timestamp (milliseconds) vs `YYYY-MM-DD` string.
     - `end`: Unix timestamp (milliseconds) vs `YYYY-MM-DD` string.
     - `limit` / `size`: Page size (e.g., 50, 100, 200).
     - `offset` / `cursor` / `pointer`: Pagination cursor.
     - `account`: Provider account ID filter.
3. **Capture Schema Fingerprints:**
   - Hash the JSON schema keys of the response.
   - Cross-check against the 9 known discovery variants to ensure backwards compatibility with older response structures.

---

## 6. Stage 0 Gate: Go / No-Go Decision Criteria

Before committing to automated background synchronization in production, the implementation team must pass the following Stage 0 Gate:

```
[ Gate 0.1: Contract Verification ]
  ├── Verified: Query parameters for `/operations` identified (e.g., start, end, limit, account).
  ├── Verified: Pagination boundary behavior tested (empty array vs 404 on last page).
  └── Verified: Error codes on session timeout cleanly identified (HTTP 401 vs JSON status="FAILED").

[ Gate 0.2: Transport & Security Hygiene ]
  ├── Zero sensitive headers (cookies, auth tokens) in application logs.
  ├── PANs masked to last 4 digits in all domain models.
  └── AES-256-GCM encryption verified for all stored sidecar pairing keys.

[ Gate 0.3: CSV/XLSX Baseline Operational ]
  ├── 100% functional manual upload pipeline for T-Bank, Sber, and Alfa-Bank statements.
  └── End-to-end deduplication verified: uploading the same statement twice yields zero duplicate rows.
```

**Rule:** If Gate 0.1 fails due to unexpected T-Bank web client anti-bot changes or OTP frequency changes, the application seamlessly defaults to **Gate 0.3 (Statement Import)**, ensuring zero delivery downtime for the family.

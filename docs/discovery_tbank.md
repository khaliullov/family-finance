# Bank Data Ingestion & T-Bank Technical Discovery Plan

## 1. Executive Summary & Regulatory Reality

In consumer financial accounting within the Russian banking ecosystem (and specifically T-Bank / Tinkoff), there is a fundamental architectural reality:
1. **Public Developer APIs (T-API / Tinkoff Open API):** These officially target **Corporate / Business accounts** (`Т-Бизнес` / SME / Sole Proprietorships). They require corporate API tokens, corporate contracts, and webhooks geared toward merchant acquiring and corporate treasury. **Retail / Personal cards and debit accounts are NOT exposed via public developer OAuth2/REST APIs.**
2. **Retail Online Banking (`Т-Банк Онлайн`):** Access to personal banking is gated behind multi-factor authentication (SMS/Push OTP, session cookies, dynamic device fingerprints, and fraud-detection telemetry).
3. **CBR Open Banking Status:** The Central Bank of Russia (CBR) and FinTech Association Open API standards for retail Account Information Services (AIS) remain in pilot phases and require commercial financial licensing.

Therefore, building a self-hosted personal family accounting system **must not rely on an assumption of official retail personal API keys**. A multi-modal ingestion strategy is mandatory.

---

## 2. Ingestion Strategy Decision Matrix

| Ingestion Path | Feasibility for Retail T-Bank | Setup Complexity for Partners | Maintenance Burden | Security & Credential Hygiene | Production Role |
| :--- | :--- | :--- | :--- | :--- | :--- |
| **Path A: Official Business T-API** | ❌ **Inapplicable** to personal debit/credit accounts. | Low (API Token) | Very Low | High (Official Scoped Token) | Optional secondary module for self-employed/ИП transactions. |
| **Path B: Local Authenticated Browser-Context Bridge (`tbank-capture`)** | ✅ **High** (reusing authenticated Playwright/Chromium session). | Medium (Partner logs in once via browser UI; session persisted locally). | Medium (Vulnerable to web-app client changes). | **High** (Zero password/OTP storage; runs on local machine/Docker; same-origin read-only GET requests). | **Primary Automated Path for Retail T-Bank**. |
| **Path C: FinTech Aggregators / Open Banking / OFD e-Receipts** | ⚠️ **Limited / Future** (Aggregators require corporate licensing or screen scraping; OFD gives receipt items, not balances). | High | High (External dependency) | Moderate | Deferred (Out of scope for MVP). |
| **Path D: Bank Statement Import (CSV / XLSX / 1C)** | ✅ **100% Guaranteed** (Native 1-click export from mobile app or web banking). | Very Low (Download & drag-and-drop file). | Very Low (Standard tabular format). | **Maximum** (Zero credentials exchanged; offline parsing). | **Mandatory Baseline Fallback & Zero-Day Support**. |

---

## 3. Authoritative Audit of T-Bank Discovery Evidence

Based on session `sess-20261122T145129Z-5b208342` from `tbank-capture` (1,677 observed responses, 587 eligible JSON responses, 85 endpoint fingerprints):

### 3.1 Observed Facts (Verified from Session Traffic)
- **Primary Operations Endpoint:** `GET /mybank/api/operations/timeline/public/legacy/v1/operations` (Classification confidence: 0.95).
- **Accounts & Details Endpoints:**
  - `GET /api/common/v1/accounts_light_ib`
  - `GET /api/common/v1/account_details_ib`
- **Statements Endpoint:** `GET /api/common/v1/statements`
- **Investments Endpoints:** `GET /mybank/api/investbox/account/all`, `GET /mybank/api/invest-portfolio/portfolios/accounts`.
- **Response Payloads & Field Names:**
  - Operations payload includes: `id`, `operationId`, `authorizationId`, `account`, `amount.value`, `amount.currency`, `accountAmount.value`, `accountAmount.currency`, `operationTime.milliseconds`, `debitingTime.milliseconds`, `merchant.name`, `brand.name`, `mcc`, `category`, `spendingCategory`, `subcategory`, `status`, `analytics.status`, `cashback`, `cashbackAmount`, `payment`, `refund`, `loyaltyBonus`, `loyaltyPayment`, `cardNumber`.
  - Timestamps are represented as integer milliseconds since Unix epoch (`*.milliseconds`).
  - Account responses contain nested `cards[]` with `id`, `name`, `paymentSystem`, `status`, `isVirtual`, `primary`.
  - Credit accounts contain `creditLimit`, `debtAmount`, `debtBalance`, `currentMinimalPayment`, `lastStatementDate`, `nextStatementDate`.

### 3.2 Inferred Behavior (Logical Deductions from Data)
- **Pagination Exists:** The operations endpoint was observed 9 times with varying payload sizes and response lengths, confirming that the banking web client paginates large transaction histories.
- **Date Filtering Exists:** The web UI timeline allows selecting date ranges, meaning the operations endpoint accepts temporal boundaries.
- **Card Association:** Cards are nested under account objects, proving that cards are access instruments rather than distinct balance-holding entities.

### 3.3 Unverified Assumptions (Strictly NOT Proven in Discovery Report)
- ⚠️ **Query Parameter Names:** The discovery report stripped query strings. Parameter names (`start`/`end` vs `from`/`to` vs `fromDate`/`toDate`) are **unverified assumptions**.
- ⚠️ **Date Parameter Formats:** Whether date parameters expect Unix milliseconds (`1700000000000`) or ISO strings (`YYYY-MM-DD`) is **unverified**.
- ⚠️ **Pagination Protocol:** Whether pagination uses page numbers (`page`/`size`), offset/limit (`offset`/`limit`), or an opaque cursor/pointer string (`cursor`/`pointer`) is **unverified**.
- ⚠️ **Required Request Headers:** Whether internal bank microservices require specific client headers (e.g. `x-app-version`, device fingerprint hashes, or specific `Accept` headers) is **unverified**.
- ⚠️ **Statement Semantics:** Whether `/statements` returns structured JSON or links to downloadable PDF files is **unverified**.

> [!CRITICAL]
> **Blocking Rule for Implementation:** No coding agent shall write hardcoded query parameter builders for T-Bank operations until live request contracts are captured and verified via `tbank-capture` request interception.

---

## 4. Multi-Partner Consent & Authentication Flow

In a two-partner family (`family_id`):
- **Partner A (e.g., User)** has personal accounts at T-Bank and Sber.
- **Partner B (e.g., Spouse)** has personal accounts at T-Bank and Alfa-Bank.
- Neither partner should know or share the other's banking master password or phone OTP.

```
       [Partner A Workstation]                       [Partner B Workstation]
                  │                                             │
      Logs into personal T-Bank web                 Logs into personal T-Bank web
                  │                                             │
      ┌───────────┴───────────┐                     ┌───────────┴───────────┐
      │  tbank-capture Runner │                     │  tbank-capture Runner │
      │  (Instance A)         │                     │  (Instance B)         │
      └───────────┬───────────┘                     └───────────┬───────────┘
                  │ Pairing Secret Exchange                     │ Pairing Secret Exchange
                  ▼                                             ▼
       ┌────────────────────────────────────────────────────────────────────────┐
       │             Family Finance Core Server (Modular Monolith)              │
       │                                                                        │
       │  Connection A (Owner: Partner A)         Connection B (Owner: Partner B│
       │  Status: Active                          Status: Active                │
       └────────────────────────────────────────────────────────────────────────┘
```

---

## 5. Sidecar Security & Protocol Specification

To ensure total isolation and prevent credential leakage:

### 5.1 Single-Use Pairing Secret Exchange
1. **Creation:** A partner initiates connection creation in the Family Finance UI (`POST /api/v1/connections`).
2. **Generation:** The core server generates a cryptographically secure, 32-byte single-use pairing secret (`sec_pair_...`) with a **10-minute expiry time**.
3. **Storage:** The server stores an Argon2id hash of the pairing secret in `bank_connections.pairing_secret_hash`. The raw secret is shown once to the user in the UI.
4. **Exchange:** The partner inputs the pairing secret into their local `tbank-capture` runner configuration. The runner calls `POST /api/v1/connections/pair` over TLS.
5. **Issuance:** The core server verifies the secret, immediately NULLs `pairing_secret_hash` (preventing replay attacks), sets `bank_connections.status = 'active'`, and returns a unique, long-lived `ConnectionAPIKey`.
6. **Authorization:** All subsequent data sync pushes from the runner must include `Authorization: Bearer <ConnectionAPIKey>`.

### 5.2 Strict Credential Isolation Boundary
- **Prohibited Data Transmission:** The sidecar runner is **strictly forbidden** from sending:
  - Browser session cookies (`connect.sid`, etc.)
  - Authorization tokens (`Bearer ...`, `x-auth-token`)
  - User bank passwords, PINs, or SMS/Push OTPs
  - Full card numbers (PANs) or CVV/CVC codes
- **Allowed Data Transmission:** The sidecar runner transmits ONLY sanitized, normalized JSON payloads containing account metadata, masked PANs (`*1234`), balances, and transaction history.

### 5.3 Encryption at Rest & Key Rotation
- Sensitive bank connection configuration (such as pairing tokens and API key hashes) is encrypted in PostgreSQL using **AES-256-GCM**.
- **Envelope Format:** `v1:<12-byte-nonce>:<16-byte-auth-tag>:<ciphertext>`.
- The version prefix (`v1`) allows future master key rotation without service downtime: a background worker re-encrypts old records using the new key version (`v2`).

---

## 6. Stage 0 Gate: Go / No-Go Decision Criteria

Before committing to automated background synchronization in production, the implementation team must pass the following Stage 0 Gate:

```
[ Gate 0.1: Live Contract Capture & Verification ]
  ├── Verified: Actual query parameters for `/operations` captured via Playwright network listener.
  ├── Verified: Pagination boundary behavior tested (last page cursor vs empty list).
  └── Verified: Error codes on session timeout cleanly identified (HTTP 401 vs JSON status="FAILED").

[ Gate 0.2: Transport & Security Hygiene ]
  ├── Zero sensitive headers (cookies, auth tokens) in application logs.
  ├── PANs masked to last 4 digits in all models and payloads.
  └── AES-256-GCM encryption verified for stored sidecar pairing keys.

[ Gate 0.3: CSV/XLSX Baseline Operational ]
  ├── 100% functional manual upload pipeline for T-Bank, Sber, and Alfa-Bank statements.
  └── End-to-end deduplication verified: uploading the same statement twice yields zero duplicate rows.
```

**Rule:** If Gate 0.1 fails or is delayed by bank anti-bot updates, the application seamlessly defaults to **Gate 0.3 (Statement Import)**, ensuring zero delivery downtime for the family.

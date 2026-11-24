# T-Bank Personal Banking Client Connector Specification

## 1. Overview and Scope

This specification defines the contract, schema mappings, and operational behavior for integrating personal retail accounts from **T-Bank** (formerly Tinkoff) into `family-finance`.

The specification is derived directly from empirical network discovery evidence captured in `khaliullov/tbank-capture` during real authenticated sessions (notably session `sess-20261124T110648Z-756169c4` with PIN authorization).

### Architecture Alignment
- **Target Ingestion Model**: Stage 2 Bank Ingestion Engine in `family-finance`.
- **Target Interface**: `connector.BankConnector` (`FetchAccounts`, `FetchTransactions`, `FetchBalances`).
- **Canonical DTOs**: `connector.AccountDTO`, `connector.CardDTO`, `connector.CanonicalBankTransaction`, `connector.BalanceDTO`.
- **Security Boundaries**: Strictly passive discovery; no storage or transmission of PIN, OTP, passwords, or session tokens; encrypted credentials at rest; per-connection session isolation.

---

## 2. Observed Endpoint Families

From empirical analysis of `report-sess-20261124T110648Z-756169c4.json`, the following endpoint families were identified:

| Endpoint Family | Method | Normalized Path | Category | Classification Confidence | Evidence Status |
| :--- | :--- | :--- | :--- | :--- | :--- |
| **Transaction History** | `GET` | `/mybank/api/operations/timeline/public/legacy/v1/operations` | `transactions` | **0.95** | **Observed** (7 distinct query variants) |
| **Account & Card Portfolio** | `GET` | `/api/common/v1/accounts_light_ib` | `accounts` | **0.60** | **Observed** (Primary retail portfolio) |
| **Account Details & Holds** | `GET` | `/api/common/v1/account_details_ib` | `accounts` | **0.60** | **Observed** (Includes `authorizationsAmount`) |
| **Account Statements** | `GET` | `/api/common/v1/statements` | `statements` | **0.70** | **Observed** (Periodic balances and turnover) |
| **Investbox Accounts** | `GET` | `/mybank/api/investbox/account/all` | `accounts` | **0.85** | **Observed** (Micro-investing piggy-bank) |
| **Investment Portfolios** | `GET` | `/mybank/api/invest-portfolio/portfolios/accounts` | `accounts` | **0.70** | **Observed** (Brokerage/IIS accounts) |

---

## 3. Sanitized Structural Schemas

### 3.1 Transactions Timeline Endpoint: `/mybank/api/operations/timeline/public/legacy/v1/operations`

- **HTTP Method**: `GET`
- **Resource Type**: `fetch`
- **Content-Type**: `application/json`
- **Envelope**:
  ```json
  {
    "resultCode": "string",
    "trackingId": "string",
    "payload": [
      {
        "account": "string",
        "accountAmount": {
          "currency": { "code": 643, "name": "RUB", "strCode": "643" },
          "value": -1250.50
        },
        "amount": {
          "currency": { "code": 643, "name": "RUB", "strCode": "643" },
          "value": -1250.50
        },
        "operationId": {
          "source": "string",
          "value": "string"
        },
        "operationTime": { "milliseconds": 1700000000000 },
        "debitingTime": { "milliseconds": 1700000050000 },
        "authorizationId": "string",
        "status": "string",
        "type": "string",
        "description": "string",
        "category": { "id": "string", "name": "string" },
        "spendingCategory": { "id": "string", "name": "string", "icon": "string" },
        "card": "string",
        "cardNumber": "string",
        "cardPresent": true,
        "cashback": 12.0,
        "cashbackAmount": { "currency": { "code": 643, "name": "RUB", "strCode": "643" }, "value": 12.0 },
        "mcc": 5411,
        "mccString": "5411",
        "brand": {
          "id": "string",
          "name": "string",
          "logo": "string",
          "link": "string"
        },
        "payment": {
          "paymentId": "string",
          "paymentType": "string",
          "providerId": "string",
          "providerGroupId": "string",
          "feeAmount": { "currency": { "code": 643, "name": "RUB", "strCode": "643" }, "value": 0.0 },
          "fieldsValues": {
            "description": "string",
            "mcc": "5411",
            "merchantDisplayName": "string",
            "merchantId": "string",
            "merchantName": "string",
            "orderId": "string",
            "paymentMethodType": "string",
            "pointer": "string",
            "pointerType": "string",
            "pointerLinkId": "string",
            "transactionId": "string"
          }
        },
        "operationTransferred": false,
        "group": "string"
      }
    ]
  }
  ```

### 3.2 Account & Card Portfolio Endpoint: `/api/common/v1/accounts_light_ib`

- **HTTP Method**: `GET`
- **Envelope**:
  ```json
  {
    "resultCode": "string",
    "trackingId": "string",
    "payload": [
      {
        "id": "string",
        "name": "string",
        "accountType": "string",
        "status": "string",
        "moneyAmount": {
          "currency": { "code": 643, "name": "RUB", "strCode": "643" },
          "value": 54200.75
        },
        "creditLimit": {
          "currency": { "code": 643, "name": "RUB", "strCode": "643" },
          "value": 0.0
        },
        "card": {
          "id": "string",
          "name": "string",
          "primary": true,
          "status": "string",
          "paymentSystem": "MIR",
          "value": "*1234",
          "expiration": { "milliseconds": 1780000000000 }
        },
        "cards": [
          {
            "id": "string",
            "name": "string",
            "primary": true,
            "status": "string",
            "paymentSystem": "MIR",
            "value": "*1234",
            "isVirtual": false,
            "expiration": { "milliseconds": 1780000000000 }
          }
        ],
        "nextStatementDate": { "milliseconds": 1705000000000 },
        "lastStatementDate": { "milliseconds": 1702400000000 }
      }
    ]
  }
  ```

### 3.3 Account Details & Authorizations: `/api/common/v1/account_details_ib`

- **HTTP Method**: `GET`
- **Key Fields**:
  - `$.payload.authorizationsAmount`: `{ "value": 1500.0, "currency": { "name": "RUB", "code": 643 } }` (total pending hold amount).
  - `$.payload.accountGroup`: Account grouping category.
  - `$.payload.creationDate.milliseconds`: Account opening date timestamp.

---

## 4. Field Mapping to Canonical Model (`family-finance`)

### 4.1 CanonicalBankTransaction Mapping

| Canonical Field | T-Bank Payload Path | Transformation / Logic |
| :--- | :--- | :--- |
| `ExternalID` | `$.payload[].operationId.value` (fallback: `$.payload[].id`) | Primary transaction UUID / string identifier. |
| `ExternalAuthID` | `$.payload[].authorizationId` | Extracted if non-empty; used for pending hold matching. |
| `AccountExternalID` | `$.payload[].account` | Matches `AccountDTO.ExternalID`. |
| `CardExternalID` | `$.payload[].card` | Matches `CardDTO.ExternalID`. |
| `Amount` | `$.payload[].amount.value`, `$.payload[].amount.currency.name` | Converted to minor units: `int64(math.Round(val * 100))`. Currency code normalized to uppercase ISO 4217 (e.g. `"RUB"`). |
| `AccountAmount` | `$.payload[].accountAmount.value`, `$.payload[].accountAmount.currency.name` | Converted to minor units in account currency. |
| `TransactionTime` | `$.payload[].operationTime.milliseconds` | `time.UnixMilli(ms).UTC()`. |
| `SettledTime` | `$.payload[].debitingTime.milliseconds` | `time.UnixMilli(ms).UTC()` if present and non-zero; `nil` if pending hold. |
| `BookingStatus` | `status`, `debitingTime`, `authorizationId` | If `status == "FAILED"` -> `"cancelled"`<br>If `debitingTime != nil` (and `status == "SUCCESS"`) -> `"posted"`<br>If `authorizationId != ""` and `debitingTime == nil` -> `"pending"`. |
| `OperationType` | `operationTransferred`, `type`, `amount.value`, `payment.feeAmount` | If `operationTransferred == true`: `"transfer_out"` (amount < 0) or `"transfer_in"` (amount > 0).<br>If `payment.feeAmount.value > 0`: `"fee"`.<br>If `type == "Debit"`: `"purchase"`.<br>If `type == "Credit"`: `"refund"`. |
| `MerchantName` | `brand.name` -> `payment.fieldsValues.merchantDisplayName` -> `payment.fieldsValues.merchantName` -> `description` | First non-empty value. |
| `RawDescription` | `$.payload[].description` | Verbatim bank operation description. |
| `MCC` | `$.payload[].mcc` | Pointer to integer (e.g. `5411`). |
| `BankCategory` | `$.payload[].category.name` -> `$.payload[].spendingCategory.name` | Human-readable category (e.g. `"Супермаркеты"`). |

### 4.2 AccountDTO Mapping

| Canonical Field | T-Bank Payload Path (`accounts_light_ib`) | Transformation / Logic |
| :--- | :--- | :--- |
| `ExternalID` | `$.payload[].id` | Account contract/number identifier. |
| `Name` | `$.payload[].name` | e.g. `"Tinkoff Black"`, `"Накопительный счет"`. |
| `Type` | `$.payload[].accountType` | Map `"Debit"` -> `"checking"`, `"Credit"` -> `"credit_card"`, `"Saving"` -> `"savings"`. |
| `Currency` | `$.payload[].moneyAmount.currency.name` | ISO 4217 uppercase string (e.g. `"RUB"`). |
| `Balance` | `$.payload[].moneyAmount.value` | Converted to `CurrencyAmount` in minor units. |
| `CreditLimit` | `$.payload[].creditLimit.value` | Converted to `CurrencyAmount` if non-zero. |
| `Cards` | `$.payload[].cards[]` (fallback `$.payload[].card`) | Array of `CardDTO`. |

### 4.3 CardDTO Mapping

| Canonical Field | T-Bank Payload Path | Transformation / Logic |
| :--- | :--- | :--- |
| `ExternalID` | `$.payload[].cards[].id` | Card unique identifier. |
| `AccountExternalID` | `$.payload[].id` | Parent account identifier. |
| `MaskedPAN` | `$.payload[].cards[].value` | Masked card number (e.g. `"*1234"`). |
| `DisplayName` | `$.payload[].cards[].name` | Card name / brand label. |
| `PaymentSystem` | `$.payload[].cards[].paymentSystem` | Normalized to uppercase: `"MIR"`, `"VISA"`, `"MASTERCARD"`. |
| `IsVirtual` | `$.payload[].cards[].isVirtual` | Boolean flag. |
| `IsPrimary` | `$.payload[].cards[].primary` | Boolean flag. |
| `Status` | `$.payload[].cards[].status` | Normalized (`"NORM"` -> `"ACTIVE"`). |

---

## 5. Currency & Monetary Representation

1. **Format in Bank API**: Floating-point number (e.g. `1250.50`, `-45.00`) paired with a currency object containing ISO 4217 numeric code (`643`) and alphabetical name (`"RUB"`).
2. **Canonical Conversion**:
   - `family-finance` uses signed 64-bit integer minor units (kopecks/cents) to prevent IEEE 754 floating-point inaccuracies.
   - Conversion formula: `minorUnits = int64(math.Round(floatValue * 100.0))`.
   - Outflow/expense operations are negative minor units; inflow/income operations are positive minor units.

---

## 6. Transaction Lifecycle Observations

1. **Authorization (Pending Hold)**:
   - When an in-store or online card payment is authorized, an authorization hold is created.
   - Evidence in `account_details_ib`: reflected in `authorizationsAmount`.
   - Evidence in timeline operations: `authorizationId` populated, `debitingTime` absent or 0.
2. **Settlement (Posted)**:
   - Once clearing files arrive from the payment system (typically 1–3 business days later), the transaction is debited from the account balance.
   - Evidence in timeline operations: `debitingTime.milliseconds` is populated with the bank settlement timestamp, and `status == "SUCCESS"`.
3. **Cancellation / Reversal**:
   - Unsettled authorization holds that expire or are voided before clearing.
   - Reflected as `status == "FAILED"` or removal of hold from `authorizationsAmount`.

---

## 7. Pagination and Date Filtering

### 7.1 Observed Query Behavior
In session `sess-20261124T110648Z-756169c4`, the endpoint `/mybank/api/operations/timeline/public/legacy/v1/operations` was observed across 7 calls:
- Query parameters observed include date range bounds (`start`, `end`, `from`, `to`).
- Timestamps in queries use epoch millisecond formatting.
- Responses return a page of operations under `payload[]`.

---

## 8. Empirical Evidence Audit Matrix (Phase 5)

| Category | Status | Empirical Evidence in Session Dump |
| :--- | :--- | :--- |
| **Accounts** | **Observed** | Fully observed in `/api/common/v1/accounts_light_ib` & `account_details_ib`. ID, name, currency, balance, credit limit verified. |
| **Cards** | **Observed** | Fully observed in `/api/common/v1/accounts_light_ib` (`card` and `cards[]`). ID, masked PAN (`*1234`), payment system, status verified. |
| **Balances** | **Observed** | Current balance in `moneyAmount`, pending holds in `authorizationsAmount`, statements balance in `statements`. |
| **Posted Transactions** | **Observed** | Fully observed in `/mybank/api/operations/timeline/...`. Settled operations have `operationTime`, `debitingTime`, `amount`, and `status`. |
| **Pending Transactions** | **Partially Observed** | `authorizationId` field observed in operations schema; `authorizationsAmount` observed in account details. (All transactions in the current dump were settled; live pending hold payload during hold phase pending capture). |
| **Payments** | **Observed** | `payment` object fully observed with `paymentId`, `providerId`, `feeAmount`, `orderId`, and payment fields. |
| **Transfers** | **Observed** | SBP/C2C transfer pointers observed (`operationTransferred: true`, `pointer`, `pointerType`, `pointerLinkId`). |
| **Refunds and Reversals** | **Insufficient Evidence** | Inflow credit operations observed, but specific refund linkage to a parent operation ID was not observed in this session dump. |
| **Transaction Timestamps** | **Observed** | Both authorization time (`operationTime.milliseconds`) and clearing time (`debitingTime.milliseconds`) observed as epoch milliseconds. |
| **Currency** | **Observed** | Explicit ISO 4217 numeric (`643`) and string (`RUB`) currency objects present in both amounts and account balances. |
| **Merchant Information** | **Observed** | Merchant brand name, logo URL, MCC, and terminal description observed in `brand` and `payment.fieldsValues`. |
| **MCC** | **Observed** | Both integer (`mcc: 5411`) and string (`mccString: "5411"`) fields observed. |
| **Pagination & Date Filtering** | **Observed** | `is_paginated: true` and `has_date_params: true` confirmed across 7 distinct observations. |

---

## 9. Security, Authentication & Session Invariants

1. **Passive Observation Only**:
   - `tbank-capture` and `family-finance` NEVER automate PIN, OTP, password entry, or CAPTCHA.
   - Authentication is performed manually by the account owner in a persistent Chromium browser window.
2. **Session Cookie Isolation**:
   - `PersonalClient` in `family-finance` must use isolated `http.CookieJar` instances per bank connection.
   - Cookies and tokens are encrypted with AES-256-GCM before storage in PostgreSQL (`bank_connections.credentials`).
3. **Exclusion of Auth Endpoints**:
   - Endpoints on `id.tbank.ru`, `passport.tbank.ru`, `auth.tbank.ru`, and path prefixes `/auth/step`, `/auth/pin`, `/preidentity/*` are strictly excluded from capture, telemetry, and logs.

---

## 10. Remaining Unknowns & Pre-Implementation Evidence Requirements

Before implementing the financial transaction importer in Stage 5, the following specific behaviors must be verified:

| Unknown Item | Risk | Evidence Required to Resolve |
| :--- | :--- | :--- |
| **Live Pending Hold Payload** | Transition from `pending` -> `posted` in ingestion engine. | Capture an operation immediately after card swipe before bank settlement (`debitingTime == nil` or 0). |
| **Refund Linkage** | Reversal deduplication and refund matching. | Capture an actual partial or full refund event to verify if `parentOperationId` or `relatedOperationId` exists. |
| **Multi-Month Cursor Format** | Pagination over 100+ operations across several months. | Capture a pagination trace traversing > 2 months to confirm cursor parameter naming (`cursor`, `offset`, or `page`). |

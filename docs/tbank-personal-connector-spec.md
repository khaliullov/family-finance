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
| `Amount` | `$.payload[].amount.value`, `$.payload[].amount.currency.name` | Parsed as exact integer minor units via `json.Number` without IEEE-754 floating-point conversion. Currency code normalized to uppercase ISO-4217 (e.g. `"RUB"`). |
| `AccountAmount` | `$.payload[].accountAmount.value`, `$.payload[].accountAmount.currency.name` | Parsed as exact integer minor units via `json.Number` in account billing currency. |
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
| `Balance` | `$.payload[].moneyAmount.value` | Parsed as `CurrencyAmount` in integer minor units via `json.Number`. |
| `CreditLimit` | `$.payload[].creditLimit.value` | Parsed as `CurrencyAmount` in integer minor units via `json.Number` if non-zero. |
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

### 5.1 Zero Floating-Point Policy
Financial integrity requires that monetary values are **never** represented, cast, or parsed using IEEE-754 binary floating-point types (`float64`, `float32`).
- Converting monetary decimals through `float64` introduces binary precision errors (e.g. `19.99 * 100 = 1998.9999999999998`).
- All JSON payloads from T-Bank MUST be decoded using `json.NewDecoder(r).UseNumber()`.
- Monetary amounts are preserved as exact ASCII lexemes (`json.Number`) and mapped directly into 64-bit signed integer minor units (`int64`, representing cents or kopecks).

### 5.2 Deterministic Fixed-Point Parsing Algorithm
The parsing algorithm (`ParseMinorUnits`) operates strictly on ASCII text strings without floating-point conversion:
1. **Whitespace & Empty Check**: Strip leading/trailing ASCII whitespace. If empty, fail closed with `ErrMissingMonetaryField`.
2. **Sign Extraction**: If string begins with `'-'`, mark `isNegative = true` and strip the prefix. If `'+'`, strip prefix.
3. **Radix Point Splitting**: Split on decimal point `'.'`:
   - If no decimal point: whole part = string, fractional part = `"00"`.
   - If exactly one decimal point:
     - 0 fractional digits (e.g. `"150."`): fractional part = `"00"`.
     - 1 fractional digit (e.g. `"150.5"`): right-pad with single `'0'` -> `"50"`.
     - 2 fractional digits (e.g. `"150.55"`): exact minor units -> `"55"`.
     - \> 2 fractional digits (e.g. `"150.555"`): **Strictly fail closed** with `ErrFractionalSubunits`.
   - If more than one decimal point: fail closed with `ErrInvalidDecimalFormat`.
4. **Integer Base-10 Conversion**:
   - Parse whole part via `strconv.ParseInt(wholeStr, 10, 64)`.
   - Parse fractional part via `strconv.ParseInt(fracStr, 10, 64)`.
5. **Overflow Detection & Scaling**:
   - Check arithmetic overflow before multiplication: `whole > (math.MaxInt64 - frac) / 100` must fail closed with `ErrMonetaryOverflow`.
   - Compute `total = whole * 100 + frac`.
   - Apply sign: if `isNegative`, `total = -total`. (Bounds check: verify `total >= math.MinInt64`).
6. **Zero Rounding & Zero Truncation**:
   - The parser enforces **zero rounding** and **zero truncation**. Any input specifying sub-minor units (partial kopecks) is rejected at ingestion.

### 5.3 Currency Scaling and ISO-4217 Normalization
- **Currency Exponents**: Standard currencies supported by T-Bank (RUB, USD, EUR, CNY, KZT, BYN, THB, AED, TRY) use exponent $E = 2$ (1 major unit = 100 minor units).
- **ISO-4217 Numeric and Alpha Mapping**:
  - T-Bank API responses supply both numeric `code` (e.g. `643`) and string `name` (e.g. `"RUB"`), along with `strCode` (`"643"`).
  - The connector normalizes currency codes against an explicit ISO-4217 whitelist (`643` -> `"RUB"`, `840` -> `"USD"`, `978` -> `"EUR"`, `156` -> `"CNY"`).
  - Legacy Russian ruble code `810` is explicitly re-mapped to canonical `"RUB"`.
  - If currency is missing, empty, or unlisted, the parser **fails closed** with `ErrUnsupportedCurrency` (never silently defaults to RUB).

### 5.4 Multi-Currency and Transaction Semantics
- **Operation Amount vs. Billing Account Amount**:
  - `amount`: Represents the transaction currency and amount at point of sale (e.g. `15.00 EUR`).
  - `accountAmount`: Represents the cleared billing amount in the account's domestic currency (e.g. `1520.45 RUB`).
  - Both amounts are parsed independently into integer minor units and mapped to `CanonicalBankTransaction.Amount` and `CanonicalBankTransaction.AccountAmount`.
- **Signedness Semantics**:
  - **Negative minor units (`< 0`)**: Debits, card purchases, ATM withdrawals, outgoing transfers, bank fees (`OperationType: "purchase"`, `"transfer_out"`, `"fee"`).
  - **Positive minor units (`> 0`)**: Credits, payroll deposits, cash-in, incoming transfers, cashback disbursements, merchant refunds (`OperationType: "income"`, `"transfer_in"`, `"refund"`).
- **Reversals, Refunds & Chargebacks**:
  - **Voided Authorization Holds**: Settled with status `"cancelled"`. Authorization holds that are cancelled before settlement are deleted or marked cancelled via `ExternalAuthID` matching.
  - **Merchant Refunds**: Returned as positive credit operations (`Amount > 0`, `type: "Credit"`). The connector maps them to canonical `OperationType: "refund"`.
  - **Partial Refunds**: Distinct positive operations with separate `operationId` linked by transaction time and merchant descriptors.

### 5.5 Parsing Behavior & Validation Matrix

| Input `value` | Input `currency` | Canonical Output (`MinorUnits`) | Canonical Currency | Behavior / Error |
| :--- | :--- | :--- | :--- | :--- |
| `"-1250.50"` | `{"name": "RUB", "code": 643}` | `-125050` | `RUB` | Valid debit (purchase) |
| `"54200.75"` | `{"name": "RUB", "code": 643}` | `5420075` | `RUB` | Valid credit (balance) |
| `"0"` | `{"name": "RUB", "code": 643}` | `0` | `RUB` | Valid zero balance |
| `"0.00"` | `{"name": "RUB", "code": 643}` | `0` | `RUB` | Valid zero fee |
| `"1500"` | `{"name": "RUB", "code": 643}` | `150000` | `RUB` | Valid integer amount (scaled $\times 100$) |
| `"-45.1"` | `{"name": "USD", "code": 840}` | `-4510` | `USD` | Valid 1-decimal amount (right-padded) |
| `"+320.00"` | `{"name": "EUR", "code": 978}` | `32000` | `EUR` | Valid explicit positive amount |
| `"12.505"` | `{"name": "RUB", "code": 643}` | `N/A` | `N/A` | **Rejected**: `ErrFractionalSubunits` (> 2 decimals) |
| `""` or `null` | `{"name": "RUB", "code": 643}` | `N/A` | `N/A` | **Rejected**: `ErrMissingMonetaryField` |
| `"100.00"` | `{}` (missing) | `N/A` | `N/A` | **Rejected**: `ErrMissingCurrency` |
| `"abc"` | `{"name": "RUB", "code": 643}` | `N/A` | `N/A` | **Rejected**: `ErrInvalidDecimalFormat` |
| `"12.34.56"` | `{"name": "RUB", "code": 643}` | `N/A` | `N/A` | **Rejected**: `ErrInvalidDecimalFormat` |
| `"99999999999999999999"` | `{"name": "RUB", "code": 643}` | `N/A` | `N/A` | **Rejected**: `ErrMonetaryOverflow` |
| `"100.00"` | `{"name": "XYZ", "code": 999}` | `N/A` | `N/A` | **Rejected**: `ErrUnsupportedCurrency` |

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

## 8. Empirical Evidence Audit Matrix

Every API endpoint and capability documented in this specification is categorized using the strict 4-level taxonomy:
- **OBSERVED**: Directly captured and structurally validated in discovery session report.
- **PARTIALLY OBSERVED**: Captured in report, but specific field subsets (e.g. pending hold states) were absent in the observed state.
- **INFERRED**: Deduced from adjacent schemas or standard retail banking behavior; requires runtime validation.
- **NOT OBSERVED**: Unverified hypothesis or deliberately filtered out; requires runtime validation or explicit fallback.

| Endpoint / Capability | Method & Path | Evidence Status | Source Report & Session ID | Fields Directly Observed in Response | Fields Assumed or Inferred | Stage 5 Runtime Requirement / Fallback |
| :--- | :--- | :--- | :--- | :--- | :--- | :--- |
| **Retail Operations Timeline** | `GET /mybank/api/operations/timeline/public/legacy/v1/operations` | **OBSERVED** | `report-sess-20261124T110648Z-756169c4.json`<br>(`sess-20261124T110648Z-756169c4`) | `account`, `accountAmount.{currency,value}`, `amount.{currency,value}`, `operationId.value`, `operationTime.milliseconds`, `debitingTime.milliseconds`, `authorizationId`, `status`, `type`, `description`, `category.{id,name}`, `spendingCategory.{id,name}`, `card`, `cardNumber`, `mcc`, `mccString`, `brand.{name,logo}`, `payment.{paymentId,feeAmount,fieldsValues}`, `operationTransferred` | Response schema during active, unsettled pending hold window (`debitingTime == nil` or 0) inferred from schema structure. Multi-month pagination cursor parameter name inferred. | **Runtime Validation**: Validate `debitingTime` presence to classify `posted` vs `pending`.<br>**Fallback**: Fall back to date window query (`start`/`end` epoch ms) if cursor pagination fails. |
| **Account & Card Portfolio** | `GET /api/common/v1/accounts_light_ib` | **OBSERVED** | `report-sess-20261124T110648Z-756169c4.json`<br>(`sess-20261124T110648Z-756169c4`) | `id`, `name`, `accountType`, `status`, `moneyAmount.{currency,value}`, `creditLimit.{currency,value}`, `card.{id,name,primary,status,paymentSystem,value,expiration}`, `cards[].{id,name,primary,status,paymentSystem,value,isVirtual}`, `nextStatementDate`, `lastStatementDate` | Foreign currency multi-account sub-wallets under single credit/debit master contract inferred. | **Runtime Validation**: Validate account status (`"NORM"` -> `"ACTIVE"`).<br>**Fallback**: Fall back to single `card` object if `cards[]` array is empty. |
| **Account Details & Holds** | `GET /api/common/v1/account_details_ib` | **OBSERVED** | `report-sess-20261124T110648Z-756169c4.json`<br>(`sess-20261124T110648Z-756169c4`) | `accountGroup`, `accountType`, `authorizationsAmount.{currency,value}`, `creationDate.milliseconds`, `creditLimit.{currency,value}`, `currency.{code,name,strCode}` | Itemized breakdown of holds (inferred that `authorizationsAmount` is aggregate hold sum, not a list of hold items). | **Runtime Validation**: Validate `authorizationsAmount` against sum of pending transactions.<br>**Fallback**: Use for balance reconciliation; do not block timeline ingestion. |
| **Account Statements** | `GET /api/common/v1/statements` | **OBSERVED** | `report-sess-20261124T110648Z-756169c4.json`<br>(`sess-20261124T110648Z-756169c4`) | `balance.{currency,value}`, `availableLimit.{currency,value}`, `billedDebt.{currency,value}`, `arrestAmount.{currency,value}` | Statement cycle boundary dates mapping to calendar month inferred. | **Fallback**: Supplementary balance audit only; not on critical ingestion path. |
| **Investbox Piggy-Bank** | `GET /mybank/api/investbox/account/all` | **OBSERVED** | `report-sess-20261124T110648Z-756169c4.json`<br>(`sess-20261124T110648Z-756169c4`) | `accounts[].balance.{currency,value}`, `totalYield.{currency,value}` | Treatment of investbox balance as a savings account in `family-finance`. | **Fallback**: Optional ingestion; safely ignore if account list is empty or returns 404. |
| **Investment Portfolios** | `GET /mybank/api/invest-portfolio/portfolios/accounts` | **OBSERVED** | `report-sess-20261124T110648Z-756169c4.json`<br>(`sess-20261124T110648Z-756169c4`) | `accounts.list[].brokerAccountId`, `brokerAccountType`, `expectedYield.{currency,value}` | Brokerage trade execution timeline (inferred to be on dedicated Invest API). | **Fallback**: Exclude brokerage trade transactions from retail banking pipeline in Stage 5. |
| **Loyalty Cashback Offers** | `GET /api/ms-gateway/loyalty_api/api/clientOffers` | **OBSERVED** | `report-sess-20261124T110648Z-756169c4.json`<br>(`sess-20261124T110648Z-756169c4`) | Merchant offers, category loyalty percentages. | Association of transaction cashback rate to real-time offer. | **Fallback**: Ignore during transaction ingestion; non-financial data. |
| **Live Pending Hold Authorizations** | `GET` timeline / card holds | **PARTIALLY OBSERVED** | `report-sess-20261124T110648Z-756169c4.json` | `authorizationId` field exists in schema; `authorizationsAmount` exists in account details. | All operations in captured session had `status == "SUCCESS"` and settled `debitingTime`. Live hold structure is inferred. | **Runtime Validation**: Check `authorizationId != ""` and `debitingTime == nil` for pending status.<br>**Fallback**: Match pending holds to posted transactions via `authorizationId` or synthetic fingerprint `(account, amount, card, time ±3d)`. |
| **Refund & Reversal Linkage** | Timeline operation fields | **PARTIALLY OBSERVED** | `report-sess-20261124T110648Z-756169c4.json` | Positive credit operations (`type == "Credit"`, `amount > 0`) observed in timeline. | Direct parent operation pointer (e.g. `parentOperationId`) was not observed in retail schema. Inferred that refunds arrive as distinct credit transactions. | **Runtime Validation**: Ingest refunds as standalone credit transactions (`OperationType: "refund"`).<br>**Fallback**: Dedup engine handles deduplication via `ExternalID`; no parent linkage assumed. |
| **Session Health Check Probe** | `GET /api/common/v1/session_status` | **INFERRED** | Purposely excluded from capture by security filter | Path observed in network logs; response body excluded to prevent token leak. | Inferred to return HTTP 200 when cookie session is valid, HTTP 401 when expired. | **Runtime Validation**: Do NOT call `/session_status` directly.<br>**Fallback**: Use `GET /api/common/v1/accounts_light_ib` as the authoritative session health probe. If HTTP 401/403 is returned, flag connection as expired. |
| **Interactive Auth Endpoints** | `POST /auth/pin`, `/auth/step`, `/preidentity/*` | **NOT OBSERVED** | Purposely excluded from capture by security filter | Excluded by design. | Handled interactively by user in Chromium browser. | **Runtime Boundary**: Personal client in `family-finance` NEVER automates or calls interactive auth endpoints. Automated sync uses existing browser cookies only. |

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

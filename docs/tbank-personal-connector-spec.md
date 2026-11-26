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
| `Amount` | `$.payload[].amount.value`, `$.payload[].amount.currency.name` | Parsed as exact integer minor units via `json.Number` without IEEE-754 floating-point conversion, determining exponent from currency definition. Currency code normalized to uppercase ISO-4217 (e.g. `"RUB"`). |
| `AccountAmount` | `$.payload[].accountAmount.value`, `$.payload[].accountAmount.currency.name` | Parsed as exact integer minor units via `json.Number` in account billing currency. |
| `TransactionTime` | `$.payload[].operationTime.milliseconds` | `time.UnixMilli(ms).UTC()`. |
| `SettledTime` | `$.payload[].debitingTime.milliseconds` | `time.UnixMilli(ms).UTC()` if present and non-zero; if zero but terminal `SUCCESS`, falls back to `TransactionTime`; `nil` if pending hold. |
| `BookingStatus` | `status`, `debitingTime`, `authorizationId` | If `status == "FAILED"` or `"REJECTED"` -> `"cancelled"`<br>If `status == "SUCCESS"` -> `"posted"`<br>If `status == "PENDING"` or (`authorizationId != ""` and `debitingTime == nil`) -> `"pending"`. |
| `OperationType` | `IsInner`, `description`, `category`, `spendingCategory`, `type`, `amount` | Conservative classification mapped to schema check constraint:<br>Internal transfers -> `"transfer_in"` (amount >= 0) or `"transfer_out"` (amount < 0).<br>Explicit merchant return/refund -> `"refund"`.<br>Salary, cash-in, inbound transfers, and unclassified credits -> safe schema equivalent `"transfer_in"`.<br>ATM cash withdrawal -> `"atm_cash"`.<br>Explicit bank fee -> `"fee"` (never inferred solely from `feeAmount`).<br>Retail card purchase and unclassified debits -> safe schema equivalent `"purchase"`.<br>Zero-amount operations -> `"card_check"`. |
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

### 5.2 Deterministic Explicit-Exponent Parsing Algorithm
The parsing algorithm (`ParseMinorUnits`) operates strictly on ASCII text strings without floating-point conversion, determining the minor-unit exponent directly from the validated currency definition:
1. **Whitespace & Empty Check**: Strip leading/trailing ASCII whitespace. If empty, fail closed with `ErrMissingMonetaryField`.
2. **Format Validation**: Reject non-JSON commas (e.g. `"12,34"` must fail closed).
3. **Currency Exponent Resolution**:
   - Determine minor-unit exponent $E$ from canonical currency definition (e.g. RUB, USD, EUR, CNY, TRY: $E = 2$; JPY: $E = 0$; KWD, BHD: $E = 3$).
   - If currency is unestablished or not in whitelist, fail closed.
4. **Sign Extraction**: If string begins with `'-'`, mark `isNegative = true` and strip the prefix. If `'+'`, strip prefix.
5. **Radix Point Splitting & Precision Enforcement**: Split on decimal point `'.'`:
   - If no decimal point: whole part = string, fractional part = string padded to $E$ zeros.
   - If exactly one decimal point:
     - Fractional digits $\le E$: right-pad with zeros to exactly $E$ digits.
     - Fractional digits $> E$: **Strictly fail closed** with `ErrExcessPrecision` (zero rounding, zero silent truncation).
   - If more than one decimal point: fail closed with `ErrInvalidDecimalFormat`.
6. **Integer Base-10 Conversion & Overflow Boundary**:
   - Check arithmetic overflow before multiplication against `math.MaxInt64` and `math.MinInt64`.
   - Explicitly handle `math.MinInt64` boundary (`-92233720368547758.08` for $E = 2$).
   - Compute `total = whole * 10^E + frac`.
   - Apply sign: if `isNegative`, `total = -total`. Verify integer bounds `[math.MinInt64, math.MaxInt64]`.
7. **Zero Rounding & Zero Truncation**:
   - The parser enforces **zero rounding** and **zero truncation**. Any input specifying sub-minor units is rejected at ingestion.

### 5.3 Currency Scaling and ISO-4217 Normalization
- **Currency Exponents**: Standard currencies supported by T-Bank (RUB, USD, EUR, CNY, KZT, BYN, THB, AED, TRY) use exponent $E = 2$ (1 major unit = 100 minor units). JPY uses $E = 0$; KWD/BHD use $E = 3$.
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
  - **Negative minor units (`< 0`)**: Debits, card purchases, ATM withdrawals, outgoing transfers, bank fees (`OperationType: "purchase"`, `"transfer_out"`, `"fee"`, `"atm_cash"`).
  - **Positive minor units (`> 0`)**: Credits, payroll deposits, cash-in, incoming transfers, cashback disbursements, merchant refunds (`OperationType: "transfer_in"`, `"refund"`).
- **Conservative Classification & Schema Constraint**:
  - The PostgreSQL schema check constraint (`bank_transactions_operation_type_check`) restricts `operation_type` to:
    `'purchase', 'refund', 'transfer_out', 'transfer_in', 'atm_cash', 'fee', 'card_check'`.
  - All incoming funds (salary, cash-in, inbound transfers, unclassified credits) map to the safe schema equivalent `"transfer_in"`.
  - Retail purchases and unclassified debits map to the safe schema equivalent `"purchase"`.
  - Bank fees are mapped to `"fee"` only when explicitly identified in description or category; never inferred solely from `payment.feeAmount`.
- **Reversals, Refunds & Chargebacks**:
  - **Voided Authorization Holds**: Settled with status `"cancelled"`. Authorization holds cancelled before settlement are updated via `ExternalAuthID` matching.
  - **Merchant Refunds**: Returned as positive credit operations (`Amount > 0`, `type: "Credit"`). The connector maps them to canonical `OperationType: "refund"`.
  - **No Parent Linkage Assumption**: Because retail timeline payloads omit parent transaction references, refunds are ingested as independent transactions. Deduplication relies on `(account_id, external_id)` so purchases and refunds of identical amounts are never incorrectly conflated.

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
| `"500"` | `{"name": "JPY", "code": 392}` | `500` | `JPY` | Valid 0-exponent currency |
| `"1.234"` | `{"name": "KWD", "code": 414}` | `1234` | `KWD` | Valid 3-exponent currency |
| `"12.505"` | `{"name": "RUB", "code": 643}` | `N/A` | `N/A` | **Rejected**: `ErrExcessPrecision` (> 2 decimals) |
| `"12,34"` | `{"name": "RUB", "code": 643}` | `N/A` | `N/A` | **Rejected**: `ErrInvalidDecimalFormat` (comma not allowed) |
| `""` or `null` | `{"name": "RUB", "code": 643}` | `N/A` | `N/A` | **Rejected**: `ErrMissingMonetaryField` |
| `"100.00"` | `{}` (missing) | `N/A` | `N/A` | **Rejected**: `ErrMissingCurrency` |
| `"abc"` | `{"name": "RUB", "code": 643}` | `N/A` | `N/A` | **Rejected**: `ErrInvalidDecimalFormat` |
| `"12.34.56"` | `{"name": "RUB", "code": 643}` | `N/A` | `N/A` | **Rejected**: `ErrInvalidDecimalFormat` |
| `"99999999999999999999"` | `{"name": "RUB", "code": 643}` | `N/A` | `N/A` | **Rejected**: `ErrMonetaryOverflow` |
| `"100.00"` | `{"name": "XYZ", "code": 999}` | `N/A` | `N/A` | **Rejected**: `ErrUnsupportedCurrency` |

---

## 6. Transaction Lifecycle Observations & Deterministic Boundary

### 6.1 Status Precedence Over debitingTime
1. **Terminal Status Precedence**:
   - If `status == "SUCCESS"`, the transaction is settled. Even if `debitingTime` is 0 or absent (e.g. instant transfers or immediate clearing), the transaction is deterministically classified as `posted`. In such cases, `SettledTime` falls back to `TransactionTime`.
   - If `status == "FAILED"` or `"REJECTED"`, the transaction is classified as `cancelled`.
2. **Pending Hold Boundary**:
   - A transaction is classified as `pending` ONLY when `status == "PENDING"` or `"HOLD"`, or if `status` is non-terminal and `authorizationId != ""` with `debitingTime == nil` or 0.
3. **Lifecycle Invariants**:
   - **Never Merge Pending and Posted Transactions via Synthetic Fingerprints**: Merging must occur exclusively through exact `ExternalAuthID` / `authorizationId` match.
   - **Never Delete Posted Transactions**: If a previously observed pending authorization record disappears from the timeline, any settled posted transaction remains untouched in the ledger.

---

## 7. Pagination and Date Filtering

### 7.1 Observed Query Behavior
In session `sess-20261124T110648Z-756169c4`, the endpoint `/mybank/api/operations/timeline/public/legacy/v1/operations` was observed across 7 calls:
- Query parameters observed include date range bounds (`start`, `end`, `from`, `to`).
- Timestamps in queries use epoch millisecond formatting.
- Responses return a page of operations under `payload[]`.
- **Speculative Pagination Prohibited**: The connector queries date-bounded batches within the requested lookback window. Speculative cursor pagination (`cursor`, `page`, `offset`) is avoided until empirical evidence demonstrates cursor mechanics.
- **Safety Boundaries**: Maximum batch size is clamped (100 operations) and response body sizes are strictly bounded (10 MB) to prevent denial-of-service.

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
| **Live Pending Hold Authorizations** | `GET` timeline / card holds | **PARTIALLY OBSERVED** | `report-sess-20261124T110648Z-756169c4.json` | `authorizationId` field exists in schema; `authorizationsAmount` exists in account details. | All operations in captured session had `status == "SUCCESS"` and settled `debitingTime`. Live hold structure is inferred. | **Runtime Validation**: Match pending holds to posted transactions ONLY via exact `authorizationId`. Synthetic fingerprint merging prohibited. |
| **Refund & Reversal Linkage** | Timeline operation fields | **PARTIALLY OBSERVED** | `report-sess-20261124T110648Z-756169c4.json` | Positive credit operations (`type == "Credit"`, `amount > 0`) observed in timeline. | Direct parent operation pointer (e.g. `parentOperationId`) was not observed in retail schema. | **Runtime Validation**: Ingest refunds as standalone credit transactions (`OperationType: "refund"`).<br>**Fallback**: Dedup engine handles deduplication via `ExternalID`; no parent linkage assumed. |
| **Session Health Check Probe** | `GET /api/common/v1/session_status` | **INFERRED** | Purposely excluded from capture by security filter | Path observed in network logs; response body excluded to prevent token leak. | Inferred to return HTTP 200 when cookie session is valid, HTTP 401 when expired. | **Runtime Validation**: Do NOT call `/session_status` directly.<br>**Fallback**: Use `GET /api/common/v1/accounts_light_ib` as authoritative session health probe. If HTTP 401/403 is returned, return `connector.ErrAuthExpired`. |
| **Interactive Auth Endpoints** | `POST /auth/pin`, `/auth/step`, `/preidentity/*` | **NOT OBSERVED** | Purposely excluded from capture by security filter | Excluded by design. | Handled interactively by user in Chromium browser. | **Runtime Boundary**: Personal client in `family-finance` NEVER automates or calls interactive auth endpoints. Automated sync uses existing browser cookies only. |

---

## 9. Personal Session Architecture & Handoff Model

### 9.1 Session Model
A valid T-Bank Personal session in `family-finance` is encapsulated in a `Session` data structure containing:
- Authenticated cookies (including session identification cookies such as `sessionId`, `wuid`).
- Device / client metadata headers observed in legitimate retail banking requests.
- `ExpiresAt` timestamp and session status (`active`, `expired`, `invalid`).
- Credentials are encrypted at rest with AES-256-GCM in `bank_connections.encrypted_credentials`.

### 9.2 Session Handoff
- **Explicit Seeding**: Sessions must be seeded via explicit user handoff (e.g. secure CLI tool, authenticated pairing command, or dedicated connection import endpoint).
- **No Silent Extraction**: `family-finance` NEVER accesses or scans local user browser profile directories, sqlite history files, or external process memory.

### 9.3 Reconnect & Revocation Lifecycle
- **HTTP 401/403 Handling**: If T-Bank returns HTTP 401 Unauthorized or HTTP 403 Forbidden, the client returns `connector.ErrAuthExpired`.
- **Fail-Closed State**: The background ingestion worker immediately catches `ErrAuthExpired`, halts synchronization, and updates `bank_connections.status = 'error'` with an explicit expiration message.
- **Durable Reconnection**: Reconnecting requires seeding a fresh encrypted session payload. The connector will not repeatedly hammer the bank endpoint with expired credentials.

### 9.4 Credential & Session Isolation
- Each bank connection operates with an independent, dedicated `http.Client` and `http.CookieJar`.
- No cookie or authentication header is ever shared across connections, accounts, or tenants.
- CAS concurrency and database-level RLS (Row Level Security) prevent cross-tenant credential access.

### 9.5 Zero Automated Challenge Invariant
- The connector **never** attempts to automate PIN submission, SMS/push OTP entry, password brute-forcing, or CAPTCHA solving.
- All multi-factor challenges remain exclusively under user manual control in visible browser contexts.

---

## 10. Unsupported & Unverified Behaviors

The following capabilities are explicitly classified as unsupported or unverified pending further empirical discovery:

1. **Currencies Without Observed Payloads**:
   - While the explicit exponent parser supports zero-exponent (e.g. JPY) and three-exponent (e.g. KWD, BHD) currencies, these currencies have not been observed in real T-Bank personal retail payloads. Unestablished currencies fail closed.
2. **Live Pending Hold Payload During Hold Phase**:
   - Captured sessions contained settled (`status == "SUCCESS"`) transactions. The exact schema during the 1–3 day authorization hold window (e.g. field nullability of `debitingTime`) remains partially observed. Synthetic fingerprint merging is prohibited.
3. **Refund-to-Purchase Parent Linkage**:
   - T-Bank personal operations timeline payloads do not supply a parent operation pointer for refunds. Attempting to link refunds to purchases via synthetic matching is prohibited; refunds are processed as independent credit transactions.
4. **Speculative Cursor Pagination**:
   - Multi-month timeline requests using speculative cursor parameters (`cursor`, `offset`, `page`) are not implemented. Only date-bounded window queries (`start`/`end`) are supported.

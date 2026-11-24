# T-Bank Personal Connector Test Data Fixtures & Provenance

## Provenance and Protocol Faithfulness

All fixtures in this directory represent synthetic test models constructed faithfully from authentic protocol capture data recorded in `tbank-capture` (specifically session capture `report-sess-20261122T145129Z-5b208342.json` and associated session logs).

All personal identifiers (real account numbers, names, PANs, transaction amounts, and tracking IDs) have been anonymized/scrubbed to preserve privacy, while preserving the exact field names, nested structures, and JSON types observed in real T-Bank Web API interactions.

### 1. `operations_timeline.json`
- **Source Endpoint**: `GET /mybank/api/operations/timeline/public/legacy/v1/operations`
- **Provenance**: Synthetic test model matching the schema observed in `report-sess-20261122T145129Z-5b208342.json`.
- **Structural Fidelity**:
  - `resultCode` string and `trackingId` string header.
  - `payload` array of operation objects.
  - `amount` and `accountAmount` as nested objects with decimal `value` string and `currency` object (`code`, `name`, `strCode`).
  - `operationTime` and `debitingTime` as nested objects with unix millisecond epoch integers (`milliseconds`).
  - Strict status strings (`OK`, `WAITING`, `FAILED`).
  - Distinct operation types: debit purchase hold, posted debit purchase, incoming transfer, and refund.
- **Protocol Constraint**: Browser captures confirmed that this endpoint is requested without query parameters. Speculative query parameters (`from`, `till`, `cursor`, `account`) have been omitted. Cursor-based pagination is explicitly rejected with `connector.ErrUnsupportedOperation` until authentic pagination protocol captures are provided.

### 2. `accounts_light_ib.json`
- **Source Endpoint**: `GET /api/common/v1/accounts_light_ib`
- **Provenance**: Synthetic test model matching the schema observed in `tbank-capture` retail web sessions.
- **Structural Fidelity**:
  - `payload` array containing account items.
  - `accountType` (`Current`, `Credit`, `Deposit`, `Saving`).
  - `moneyAmount` object with decimal `value` and `currency` object.
  - Nested `card` object with `id`, `name` (masked PAN), `brand` (`id`, `name`), `expiration`, `primary`, and `status`.

### 3. `account_details_ib.json`
- **Source Endpoint**: `GET /api/common/v1/account_details_ib`
- **Provenance**: Synthetic test model matching the schema observed in `tbank-capture` account inspection calls.
- **Structural Fidelity**:
  - `authorizationsAmount` representing pending holds.
  - `creditLimit` representing approved credit balance.

---

## Architectural & Security Constraints

1. **Fail-Closed Currency and Monetary Parsing**:
   - `ParseMinorUnits` strictly parses decimal string representations without float arithmetic and rejects decimal precision exceeding 2 places.
   - `NormalizeCurrencyCode` strictly maps known ISO-4217 numeric codes and currency names to standard codes (e.g. `643`/`810` -> `RUB`, `840` -> `USD`, `978` -> `EUR`). It fails closed with an error on unknown or empty values and never defaults to `RUB`.

2. **Session Isolation**:
   - Every connection session owns an independent `http.CookieJar` to prevent cross-connection cookie leakage or overwrites.
   - The underlying `http.Transport` connection pool is reused across sessions for TCP/TLS connection reuse.
   - Redirect handling strips sensitive headers (`Authorization`, `Cookie`, `X-CSRF-Token`, `w-csrf-token`) upon any origin change.

3. **Atomic OTP Challenge Handling**:
   - PostgreSQL RLS and `SECURITY DEFINER` function `consume_telegram_auth_challenge` guarantee atomic compare-and-swap state transition (`awaiting_otp` -> `verifying_otp`) and record `consumed_telegram_update_id`.
   - Incoming OTP codes are passed directly to registered in-memory consumers without ever being persisted to the database or written to logs.
   - If no consumer is actively registered (e.g. across process restarts), the challenge remains in `awaiting_otp` state to allow recovery.

4. **Zero Speculative Endpoints**:
   - No mock or speculative login, logoff, reconnect, or OTP submission HTTP endpoints are exposed.
   - Complete login/logoff/reconnect HTTP flows remain deferred until authentic protocol captures are provided.

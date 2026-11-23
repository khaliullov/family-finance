# Family Finance Security & Integrity Test Suite (PostgreSQL 16+)

This directory contains the permanent, automated test suite that validates the multi-tenant architecture, role separation, evidence immutability, and compliance logs of **Family Finance**.

---

## 1. Test Architecture & Files

- `schema.sql`: Complete DDL provisioning script containing:
  - 6-role least-privilege configuration (`family_admin`, `family_app`, `family_worker`, `family_queue_definer`, `family_auth_definer`, `family_auth_verifier`).
  - Dedicated SECURITY DEFINER schemas (`queue_sec`, `auth_sec`).
  - Table schemas for all 16 global and tenant tables with composite foreign keys.
  - Immutability and state machine triggers (`trg_bank_tx_immutable`, `trg_bank_tx_no_delete`, `trg_raw_payloads_immutable`, `trg_audit_logs_append_only`, `trg_enforce_tenant_immutability`).
  - Row-Level Security (RLS) policies with `FORCE ROW LEVEL SECURITY`.
- `test_security_suite.py`: Automated Python test harness covering:
  - **Identity & Credential Isolation:** Verification of `users.password_hash` masking, non-sensitive column grants, and helper function permissions.
  - **Monotonic Bank Evidence State Machine:** Strict enforcement of `pending -> posted` (with settlement variance), `pending -> cancelled` (with amount preservation), immutable terminal states (`posted -> posted` amount mutation blocked, `posted -> cancelled` blocked, `cancelled -> posted` blocked), and Category 1 immutable facts.
  - **Append-Only Compliance Logs:** Verification that `raw_bank_payloads` and `audit_logs` cannot be updated or deleted by application or worker roles.
  - **Registration Bootstrap Sequence:** Verification that generating UUIDs in the application before inserting into `families` succeeds under `rls_families_mod WITH CHECK (id = get_current_family_id())` without RLS bypasses.
  - **Cross-Tenant Queue Claiming:** Verification that `queue_sec.claim_next_worker_job()` securely claims pending jobs across families under `FORCE ROW LEVEL SECURITY` while `family_queue_definer` has zero access to financial tables.
  - **Tenant Boundary Isolation:** Verification that missing tenant context defaults to deny-all (0 rows returned) and queries cannot view or mutate out-of-tenant records.

---

## 2. Prerequisites

- Docker running on the host.
- A running PostgreSQL 16 container named `shared-postgres` (or modify `CONTAINER` in `test_security_suite.py`).
- Python 3.9+ with standard library (`subprocess`, `sys`, `uuid`). No third-party pip dependencies required.

---

## 3. Running the Test Suite

Make the test script executable and run:

```bash
python3 tests/security/test_security_suite.py
```

Or run via Docker:

```bash
docker exec -i shared-postgres psql -U postgres -c "DROP DATABASE IF EXISTS test_family_finance_security; CREATE DATABASE test_family_finance_security;"
docker exec -i shared-postgres psql -U postgres -d test_family_finance_security -f tests/security/schema.sql
python3 tests/security/test_security_suite.py
```

### Exit Codes:
- `0`: All security and integrity tests passed.
- `1`: One or more security assertions failed.

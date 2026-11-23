# Family Finance Security & Integrity Test Suite (PostgreSQL 16+)

This directory contains the automated test suite that validates the multi-tenant architecture, role separation, evidence immutability, and compliance logs of **Family Finance** against canonical specifications.

---

## 1. Canonical Schema Single Source of Truth & Zero-Drift Architecture

`database.md` is the **single source of truth** for all database structures, roles, privileges, constraints, triggers, and RLS policies. To prevent schema drift between documentation and verification:

1. **Extraction Pipeline:** The script `scripts/extract_canonical_schema.py` parses `database.md` and generates `tests/security/schema.sql`.
2. **No Manual Edits:** `tests/security/schema.sql` must never be edited manually.
3. **Automated Drift Assertion:** Test 0.1 of `test_security_suite.py` dynamically extracts the DDL from `database.md` in memory and asserts that `tests/security/schema.sql` is character-for-character identical, failing CI if drift is detected.

To re-extract the canonical DDL:
```bash
python3 scripts/extract_canonical_schema.py
```

---

## 2. Test Architecture & Files

- **`schema.sql`**: Full production DDL extracted from `database.md`:
  - 6-role least-privilege configuration (`family_admin`, `family_app`, `family_worker`, `family_queue_definer`, `family_auth_definer`, `family_auth_verifier`).
  - Dedicated security helper schemas (`auth_sec`, `queue_sec`).
  - 16 production tables (1 global `users` table + 15 tenant-owned tables with composite primary and foreign keys).
  - Immutability and state machine triggers (`trg_bank_tx_immutable`, `trg_bank_tx_no_delete`, `trg_raw_payloads_immutable`, `trg_audit_logs_append_only`, `trg_enforce_tenant_immutability`).
  - Row-Level Security (RLS) policies with `FORCE ROW LEVEL SECURITY` across all 15 tenant tables.
- **`test_security_suite.py`**: Automated test suite comprising 70 test assertions across 8 distinct verification groups:
  - **Group 0: Canonical Schema Drift & Catalog Verification:** Ensures `schema.sql` matches `database.md` exactly, all 16 tables exist, all 15 tenant tables enforce `FORCE ROW LEVEL SECURITY`, `users` is global, and `family_members` contains all canonical columns.
  - **Group 1: Role Privilege & Identity Isolation:** Verifies `users.password_hash` column-level access denial, non-sensitive column reads, helper routine access, and table creation denial for runtime roles.
  - **Group 2: SECURITY DEFINER Function Integrity & Catalog Assertions:** Validates catalog attributes (`prosecdef = true`, dedicated role ownership, `search_path = pg_catalog, public, pg_temp`, revoked `PUBLIC` execute) and asserts that definer roles hold zero privileges on business/financial tables.
  - **Group 3: Bank Evidence Immutability & Monotonic State Machine:** Verifies positive settlement transitions (amount adjustment, unchanged amount, external ID retention, zero-amount `card_check`), negative settlement rejections (null `posted_time`, `posted_time < transaction_time`, zero-amount on purchase, sign/direction inversion, wiped/blank `external_id`, Category 1 fact tampering), and terminal state immutability (`posted` and `cancelled` records).
  - **Group 4: Append-Only Compliance & Invariant Triggers:** Verifies `UPDATE`/`DELETE` rejection on `raw_bank_payloads` and `audit_logs`, transfer match endpoint immutability, accounting entry transfer ownership checks, and split sum integrity validation.
  - **Group 5: Registration Bootstrap Sequence under RLS:** Verifies atomic registration with pre-generated UUIDs into `users`, `families`, `family_members`, and initial `categories` under `family_app` with `rls_families_mod WITH CHECK`, atomic rollback without orphaned rows, duplicate email rejection, and foreign family injection rejection.
  - **Group 6: Background Queue Claiming under FORCE RLS:** Verifies that `queue_sec.claim_next_worker_job()` claims available jobs across tenants without bypass GUCs or `BYPASSRLS`, while `family_worker` direct queries on `worker_jobs` without context yield 0 rows.
  - **Group 7: Comprehensive Tenant Isolation & Foreign Key Boundaries:** Verifies deny-all on missing context across all 15 tenant tables, cross-tenant read/update denial, composite foreign key cross-tenant reference rejection, tenant immutability triggers (`family_id` modification blocked), and connection pool context reset (`DISCARD ALL`).

---

## 3. Prerequisites

- A running PostgreSQL 16+ instance (e.g., Docker container named `shared-postgres` running `postgres:16-alpine`).
- Python 3.9+ with standard library (`subprocess`, `sys`, `uuid`, `os`, `re`). No third-party pip dependencies required.

---

## 4. Running the Test Suite

Run the automated test suite directly:

```bash
python3 tests/security/test_security_suite.py
```

### Execution Flow:
1. Creates a clean, isolated database: `test_family_finance_security`.
2. Applies canonical `tests/security/schema.sql`.
3. Seeds test data for Family A and Family B within an atomic transaction.
4. Executes all 70 assertions using actual runtime roles (`family_app`, `family_worker`, `family_queue_definer`, `family_auth_definer`).
5. Drops `test_family_finance_security` on completion.

### Exit Codes:
- `0`: All 70 security and integrity tests passed.
- `1`: One or more security assertions failed or schema drift was detected.

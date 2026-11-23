#!/usr/bin/env python3
"""
Extracts canonical PostgreSQL DDL from database.md and generates tests/security/schema.sql.
Ensures database.md remains the single source of truth with zero schema drift.
"""

import sys
import os
import re

def extract_canonical_schema(doc_path="database.md"):
    if not os.path.exists(doc_path):
        raise FileNotFoundError(f"Canonical specification {doc_path} not found")

    with open(doc_path, "r", encoding="utf-8") as f:
        text = f.read()

    blocks = re.findall(r"```sql\n(.*?)```", text, re.DOTALL)
    if len(blocks) < 21:
        raise ValueError(f"Expected at least 21 SQL blocks in {doc_path}, found {len(blocks)}")

    # Extensions
    exts = "CREATE EXTENSION IF NOT EXISTS \"uuid-ossp\";\nCREATE EXTENSION IF NOT EXISTS \"pgcrypto\";"

    # Block 15 (0-indexed 14) has roles, schemas, grants
    b15 = blocks[14]
    marker = "-- 5. Explicit Table Grants"
    idx = b15.find(marker)
    if idx == -1:
        raise ValueError("Could not find table grants marker in Section 5.1 block")
    roles_schemas = b15[:idx].strip()
    table_grants = b15[idx:].strip()

    # Block 0 has exts + tables (families, users, family_members)
    b0_tables = blocks[0].replace('CREATE EXTENSION IF NOT EXISTS "uuid-ossp";', '').replace('CREATE EXTENSION IF NOT EXISTS "pgcrypto";', '').strip()

    # Blocks 0 to 6 are tables and indexes (Sections 3.1 to 3.7)
    tables_blocks = [b0_tables] + [blocks[i].strip() for i in range(1, 7)]

    # Blocks 7 to 13 are integrity triggers (Sections 4.1 to 4.7)
    triggers_blocks = [blocks[i].strip() for i in range(7, 14)]

    # Blocks 15 to 17 are SECURITY DEFINER functions (Sections 5.3 to 5.5)
    sec_def_blocks = [blocks[i].strip() for i in range(15, 18)]

    # Block 18 is RLS policies and FORCE RLS (Section 5.6)
    rls_block = blocks[18].strip()

    # Block 20 is Analytical Views (Section 6)
    views_block = blocks[20].strip()

    tables = [
        "users", "families", "family_members", "bank_connections",
        "raw_bank_payloads", "accounts", "cards", "bank_transactions",
        "transfer_matches", "transfer_rejections", "categories",
        "accounting_entries", "categorization_rules", "statement_imports",
        "audit_logs", "worker_jobs"
    ]
    table_ownership = "-- DDL Schema Owner Privileges & Table Ownership\n"
    table_ownership += "\n".join([f"ALTER TABLE public.{t} OWNER TO family_admin;" for t in tables])
    table_ownership += "\n\nGRANT ALL ON ALL TABLES IN SCHEMA public TO family_admin;\nGRANT ALL ON ALL SEQUENCES IN SCHEMA public TO family_admin;\n"

    header = """-- =============================================================================
-- CANONICAL PRODUCTION SCHEMA FOR FAMILY FINANCE (POSTGRESQL 16+)
-- AUTOMATICALLY EXTRACTED FROM database.md — DO NOT EDIT MANUALLY!
-- Run `python3 scripts/extract_canonical_schema.py` to regenerate.
-- =============================================================================
"""

    canonical_sql = f"""{header}
{exts}

{roles_schemas}

{chr(10).join(tables_blocks)}

{table_ownership}

{chr(10).join(triggers_blocks)}

{chr(10).join(sec_def_blocks)}

{table_grants}

{rls_block}

{views_block}
"""
    return canonical_sql

def main():
    repo_root = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
    doc_path = os.path.join(repo_root, "database.md")
    target_path = os.path.join(repo_root, "tests", "security", "schema.sql")

    schema_sql = extract_canonical_schema(doc_path)
    with open(target_path, "w", encoding="utf-8") as f:
        f.write(schema_sql)

    print(f"Extracted canonical schema from {doc_path} -> {target_path}")
    print(f"Total size: {len(schema_sql)} characters, {len(schema_sql.splitlines())} lines.")

if __name__ == "__main__":
    main()

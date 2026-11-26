package statement

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/khaliullov/family-finance/internal/database"
)

const testContentHash64 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// 1. Worker RLS isolation:
// Verify family_worker cannot read or mutate cross-tenant statement documents,
// batches, staged transactions, or overrides.
func TestWorkerRLS_Isolation(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()
	workerPool := getWorkerPool(t)
	defer workerPool.Close()

	ctx := context.Background()
	familyA, userA, accountA := setupStatementTenantFixture(t)
	familyB, userB, accountB := setupStatementTenantFixture(t)

	docAID := uuid.New()
	batchAID := uuid.New()
	stagedAID := uuid.New()
	overrideAID := uuid.New()

	// Seed Family A statement entities as family_app
	err := database.WithFamilyUserTx(ctx, appPool, familyA, userA, func(tx pgx.Tx) error {
		// Document
		_, err := tx.Exec(ctx, `
			INSERT INTO public.statement_documents (
				id, family_id, uploaded_by_user_id, original_filename, file_size_bytes,
				content_hash, mime_type, encrypted_content
			) VALUES ($1, $2, $3, 'test.csv', 100, $4, 'text/csv', $5)
		`, docAID, familyA, userA, testContentHash64, []byte("encrypted_A"))
		if err != nil {
			return err
		}

		// Batch
		_, err = tx.Exec(ctx, `
			INSERT INTO public.statement_import_batches (
				id, family_id, account_id, document_id, source_format, parser_version,
				status, reconciliation_status, total_rows, staged_count, duplicate_count, discrepancy_count
			) VALUES ($1, $2, $3, $4, 'csv_test', '1.0.0', 'staged', 'discrepancy', 1, 1, 0, 1)
		`, batchAID, familyA, accountA, docAID)
		if err != nil {
			return err
		}

		// Staged row
		_, err = tx.Exec(ctx, `
			INSERT INTO public.staged_bank_transactions (
				id, family_id, batch_id, account_id, row_index, import_fingerprint,
				amount_minor, currency, account_amount_minor, account_currency,
				booking_status, operation_type, transaction_time, raw_description,
				duplicate_status, resolution_action
			) VALUES (
				$1, $2, $3, $4, 1, $5,
				-10000, 'RUB', -10000, 'RUB',
				'posted', 'purchase', NOW(), 'Groceries',
				'new', 'commit'
			)
		`, stagedAID, familyA, batchAID, accountA, testContentHash64)
		if err != nil {
			return err
		}

		// Override
		_, err = tx.Exec(ctx, `
			INSERT INTO public.statement_reconciliation_overrides (
				id, family_id, batch_id, overridden_by_user_id,
				opening_balance_minor, closing_balance_minor, calculated_balance_minor, discrepancy_delta_minor,
				reason
			) VALUES (
				$1, $2, $3, $4,
				10000, 20000, 15000, 5000,
				'Approved legitimate discrepancy'
			)
		`, overrideAID, familyA, batchAID, userA)
		return err
	})
	if err != nil {
		t.Fatalf("failed to seed Family A statement fixtures: %v", err)
	}

	// 1a. Under Family A context, family_worker can see all Family A records
	err = database.WithFamilyTx(ctx, workerPool, familyA, func(tx pgx.Tx) error {
		var docCount, batchCount, stagedCount, overrideCount int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM public.statement_documents WHERE id = $1", docAID).Scan(&docCount); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM public.statement_import_batches WHERE id = $1", batchAID).Scan(&batchCount); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM public.staged_bank_transactions WHERE id = $1", stagedAID).Scan(&stagedCount); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM public.statement_reconciliation_overrides WHERE id = $1", overrideAID).Scan(&overrideCount); err != nil {
			return err
		}

		if docCount != 1 || batchCount != 1 || stagedCount != 1 || overrideCount != 1 {
			t.Errorf("Family A worker expected 1 count for each table, got docs=%d, batches=%d, staged=%d, overrides=%d",
				docCount, batchCount, stagedCount, overrideCount)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Worker under Family A context failed: %v", err)
	}

	// 1b. Under Family B context, family_worker CANNOT see any Family A records
	err = database.WithFamilyTx(ctx, workerPool, familyB, func(tx pgx.Tx) error {
		var docCount, batchCount, stagedCount, overrideCount int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM public.statement_documents WHERE id = $1", docAID).Scan(&docCount); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM public.statement_import_batches WHERE id = $1", batchAID).Scan(&batchCount); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM public.staged_bank_transactions WHERE id = $1", stagedAID).Scan(&stagedCount); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM public.statement_reconciliation_overrides WHERE id = $1", overrideAID).Scan(&overrideCount); err != nil {
			return err
		}

		if docCount != 0 || batchCount != 0 || stagedCount != 0 || overrideCount != 0 {
			t.Errorf("Family B worker leaked Family A rows! docs=%d, batches=%d, staged=%d, overrides=%d",
				docCount, batchCount, stagedCount, overrideCount)
		}

		// Mutation attempt on Family A batch by Family B worker affects 0 rows
		tag, err := tx.Exec(ctx, "UPDATE public.statement_import_batches SET status = 'failed' WHERE id = $1", batchAID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			t.Errorf("Family B worker updated Family A batch! rows affected: %d", tag.RowsAffected())
		}

		return nil
	})
	if err != nil {
		t.Fatalf("Worker cross-tenant isolation check failed: %v", err)
	}

	_ = userB
	_ = accountB
}

// 2. Cross-family staged transaction rejection:
// Inserting a staged transaction with family_id != batch.family_id must be rejected by foreign key constraint.
func TestStagedTransaction_CrossFamilyRejection(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()

	ctx := context.Background()
	familyA, userA, accountA := setupStatementTenantFixture(t)
	familyB, _, _ := setupStatementTenantFixture(t)

	batchID := uuid.New()
	docID := uuid.New()

	err := database.WithFamilyUserTx(ctx, appPool, familyA, userA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO public.statement_documents (
				id, family_id, uploaded_by_user_id, original_filename, file_size_bytes,
				content_hash, mime_type, encrypted_content
			) VALUES ($1, $2, $3, 'test.csv', 100, $4, 'text/csv', 'enc')
		`, docID, familyA, userA, testContentHash64)
		if err != nil {
			return err
		}

		_, err = tx.Exec(ctx, `
			INSERT INTO public.statement_import_batches (
				id, family_id, account_id, document_id, source_format, parser_version,
				status, reconciliation_status, total_rows, staged_count, duplicate_count, discrepancy_count
			) VALUES ($1, $2, $3, $4, 'csv_test', '1.0.0', 'staged', 'reconciled', 1, 0, 0, 0)
		`, batchID, familyA, accountA, docID)
		return err
	})
	if err != nil {
		t.Fatalf("failed to seed batch: %v", err)
	}

	// Now try to insert staged row with familyB into batch of familyA (under superuser or familyB context)
	pgPool, err := pgxpool.New(ctx, getPGConnString())
	if err != nil {
		t.Fatalf("failed to connect as superuser: %v", err)
	}
	defer pgPool.Close()

	_, err = pgPool.Exec(ctx, `
		INSERT INTO public.staged_bank_transactions (
			id, family_id, batch_id, account_id, row_index, import_fingerprint,
			amount_minor, currency, account_amount_minor, account_currency,
			booking_status, operation_type, transaction_time, raw_description,
			duplicate_status, resolution_action
		) VALUES (
			$1, $2, $3, $4, 1, $5,
			-1000, 'RUB', -1000, 'RUB',
			'posted', 'purchase', NOW(), 'Invalid cross family row',
			'new', 'commit'
		)
	`, uuid.New(), familyB, batchID, accountA, testContentHash64)

	if err == nil {
		t.Fatalf("expected cross-family staged transaction insertion to fail foreign key, but it succeeded")
	}
	if !strings.Contains(err.Error(), "fk_staged_tx_batch_tenant") && !strings.Contains(err.Error(), "foreign key") {
		t.Fatalf("expected foreign key constraint violation, got: %v", err)
	}
}

// 3. Cross-account staged transaction rejection:
// Inserting a staged transaction with account_id != batch.account_id must fail foreign key constraint.
func TestStagedTransaction_CrossAccountRejection(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()

	ctx := context.Background()
	familyA, userA, account1 := setupStatementTenantFixture(t)

	// Create Account 2 in Family A
	account2 := uuid.New()
	err := database.WithFamilyUserTx(ctx, appPool, familyA, userA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO public.accounts (id, family_id, owner_user_id, name, type, currency, current_balance_minor)
			VALUES ($1, $2, $3, 'Secondary Checking', 'checking', 'RUB', 50000)
		`, account2, familyA, userA)
		return err
	})
	if err != nil {
		t.Fatalf("failed to create second account: %v", err)
	}

	batchID := uuid.New()
	docID := uuid.New()

	err = database.WithFamilyUserTx(ctx, appPool, familyA, userA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO public.statement_documents (
				id, family_id, uploaded_by_user_id, original_filename, file_size_bytes,
				content_hash, mime_type, encrypted_content
			) VALUES ($1, $2, $3, 'test.csv', 100, $4, 'text/csv', 'enc')
		`, docID, familyA, userA, testContentHash64)
		if err != nil {
			return err
		}

		// Batch for Account 1
		_, err = tx.Exec(ctx, `
			INSERT INTO public.statement_import_batches (
				id, family_id, account_id, document_id, source_format, parser_version,
				status, reconciliation_status, total_rows, staged_count, duplicate_count, discrepancy_count
			) VALUES ($1, $2, $3, $4, 'csv_test', '1.0.0', 'staged', 'reconciled', 1, 0, 0, 0)
		`, batchID, familyA, account1, docID)
		if err != nil {
			return err
		}

		// Attempt to insert staged transaction referencing Batch 1 but with Account 2
		_, err = tx.Exec(ctx, `
			INSERT INTO public.staged_bank_transactions (
				id, family_id, batch_id, account_id, row_index, import_fingerprint,
				amount_minor, currency, account_amount_minor, account_currency,
				booking_status, operation_type, transaction_time, raw_description,
				duplicate_status, resolution_action
			) VALUES (
				$1, $2, $3, $4, 1, $5,
				-1000, 'RUB', -1000, 'RUB',
				'posted', 'purchase', NOW(), 'Cross account row',
				'new', 'commit'
			)
		`, uuid.New(), familyA, batchID, account2, testContentHash64)

		return err
	})

	if err == nil {
		t.Fatalf("expected cross-account staged transaction insertion to fail foreign key, but it succeeded")
	}
	if !strings.Contains(err.Error(), "fk_staged_tx_batch_account") && !strings.Contains(err.Error(), "foreign key") {
		t.Fatalf("expected fk_staged_tx_batch_account constraint violation, got: %v", err)
	}
}

// 4. Invalid matched transaction rejection:
// Setting matched_existing_tx_id pointing to a transaction from another account or family must fail FK constraint.
func TestStagedTransaction_InvalidMatchedTxRejection(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()

	ctx := context.Background()
	familyA, userA, account1 := setupStatementTenantFixture(t)

	account2 := uuid.New()
	txInAccount2 := uuid.New()

	err := database.WithFamilyUserTx(ctx, appPool, familyA, userA, func(tx pgx.Tx) error {
		// Account 2
		_, err := tx.Exec(ctx, `
			INSERT INTO public.accounts (id, family_id, owner_user_id, name, type, currency, current_balance_minor)
			VALUES ($1, $2, $3, 'Secondary Checking', 'checking', 'RUB', 50000)
		`, account2, familyA, userA)
		if err != nil {
			return err
		}

		// Bank transaction in Account 2
		_, err = tx.Exec(ctx, `
			INSERT INTO public.bank_transactions (
				id, family_id, account_id, external_id, fingerprint_hash, fingerprint_sequence,
				amount_minor, currency,
				account_amount_minor, account_currency, booking_status, operation_type,
				transaction_time, raw_description
			) VALUES (
				$1, $2, $3, 'tx_acc2', $4, 1,
				-2000, 'RUB',
				-2000, 'RUB', 'pending', 'purchase',
				NOW(), 'Pending Tx in Acc 2'
			)
		`, txInAccount2, familyA, account2, testContentHash64)
		if err != nil {
			return err
		}

		// Insert matching accounting entry to satisfy deferred split integrity
		_, err = tx.Exec(ctx, `
			INSERT INTO public.accounting_entries (
				id, family_id, bank_transaction_id, account_id,
				amount_minor, currency, classification, payer_user_id,
				beneficiary_type, entry_time, is_split, is_manual
			) VALUES (
				$1, $2, $3, $4,
				-2000, 'RUB', 'expense', $5,
				'household', NOW(), FALSE, FALSE
			)
		`, uuid.New(), familyA, txInAccount2, account2, userA)
		return err
	})
	if err != nil {
		t.Fatalf("failed to create Account 2 and bank transaction: %v", err)
	}

	batchID := uuid.New()
	docID := uuid.New()

	err = database.WithFamilyUserTx(ctx, appPool, familyA, userA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO public.statement_documents (
				id, family_id, uploaded_by_user_id, original_filename, file_size_bytes,
				content_hash, mime_type, encrypted_content
			) VALUES ($1, $2, $3, 'test.csv', 100, $4, 'text/csv', 'enc')
		`, docID, familyA, userA, testContentHash64)
		if err != nil {
			return err
		}

		// Batch for Account 1
		_, err = tx.Exec(ctx, `
			INSERT INTO public.statement_import_batches (
				id, family_id, account_id, document_id, source_format, parser_version,
				status, reconciliation_status, total_rows, staged_count, duplicate_count, discrepancy_count
			) VALUES ($1, $2, $3, $4, 'csv_test', '1.0.0', 'staged', 'reconciled', 1, 0, 0, 0)
		`, batchID, familyA, account1, docID)
		if err != nil {
			return err
		}

		// Attempt to insert staged transaction for Batch 1 (Account 1) matching tx in Account 2
		_, err = tx.Exec(ctx, `
			INSERT INTO public.staged_bank_transactions (
				id, family_id, batch_id, account_id, row_index, import_fingerprint,
				amount_minor, currency, account_amount_minor, account_currency,
				booking_status, operation_type, transaction_time, raw_description,
				duplicate_status, resolution_action, matched_existing_tx_id
			) VALUES (
				$1, $2, $3, $4, 1, $5,
				-2000, 'RUB', -2000, 'RUB',
				'posted', 'purchase', NOW(), 'Matched row mismatch',
				'settlement_update', 'apply_settlement', $6
			)
		`, uuid.New(), familyA, batchID, account1, testContentHash64, txInAccount2)

		return err
	})

	if err == nil {
		t.Fatalf("expected matched_existing_tx_id across different accounts to fail FK constraint, but it succeeded")
	}
	if !strings.Contains(err.Error(), "fk_staged_tx_matched_existing_tenant_account") && !strings.Contains(err.Error(), "foreign key") {
		t.Fatalf("expected fk_staged_tx_matched_existing_tenant_account violation, got: %v", err)
	}
}

// 5. Document UPDATE/DELETE prohibition:
// statement_documents are strictly append-only. UPDATE and DELETE must be prohibited.
func TestStatementDocuments_Immutability(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()

	ctx := context.Background()
	familyA, userA, _ := setupStatementTenantFixture(t)
	docID := uuid.New()

	err := database.WithFamilyUserTx(ctx, appPool, familyA, userA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO public.statement_documents (
				id, family_id, uploaded_by_user_id, original_filename, file_size_bytes,
				content_hash, mime_type, encrypted_content
			) VALUES ($1, $2, $3, 'original.csv', 100, $4, 'text/csv', 'enc')
		`, docID, familyA, userA, testContentHash64)
		return err
	})
	if err != nil {
		t.Fatalf("failed to insert statement document: %v", err)
	}

	// 5a. UPDATE attempt as family_app must fail with permission denied (no UPDATE privilege granted)
	err = database.WithFamilyUserTx(ctx, appPool, familyA, userA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "UPDATE public.statement_documents SET original_filename = 'tampered.csv' WHERE id = $1", docID)
		return err
	})
	if err == nil {
		t.Fatalf("expected UPDATE on statement_documents to fail under family_app, but succeeded")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("expected permission denied error for family_app UPDATE, got: %v", err)
	}

	// 5b. UPDATE attempt under admin/superuser must be blocked by the immutability trigger
	pgPool, err := pgxpool.New(ctx, getPGConnString())
	if err != nil {
		t.Fatalf("failed to connect as admin: %v", err)
	}
	defer pgPool.Close()

	_, err = pgPool.Exec(ctx, "UPDATE public.statement_documents SET original_filename = 'tampered.csv' WHERE id = $1", docID)
	if err == nil {
		t.Fatalf("expected UPDATE on statement_documents to fail immutability trigger under admin, but succeeded")
	}
	if !strings.Contains(err.Error(), "strictly immutable") {
		t.Fatalf("expected 'strictly immutable' trigger error under admin, got: %v", err)
	}

	// 5c. DELETE attempt as family_app must fail with permission denied
	err = database.WithFamilyUserTx(ctx, appPool, familyA, userA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "DELETE FROM public.statement_documents WHERE id = $1", docID)
		return err
	})
	if err == nil {
		t.Fatalf("expected DELETE on statement_documents to fail under family_app, but succeeded")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("expected permission denied error for family_app DELETE, got: %v", err)
	}

	// 5d. DELETE attempt as family_worker must also fail with permission denied
	workerPool := getWorkerPool(t)
	defer workerPool.Close()
	err = database.WithFamilyTx(ctx, workerPool, familyA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "DELETE FROM public.statement_documents WHERE id = $1", docID)
		return err
	})
	if err == nil {
		t.Fatalf("expected DELETE on statement_documents to fail under family_worker, but succeeded")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("expected permission denied error for family_worker DELETE, got: %v", err)
	}
}

// 6. Override UPDATE/DELETE prohibition:
// statement_reconciliation_overrides are strictly immutable and attributable.
func TestStatementOverrides_Immutability(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()

	ctx := context.Background()
	familyA, userA, accountA := setupStatementTenantFixture(t)
	docID := uuid.New()
	batchID := uuid.New()
	overrideID := uuid.New()

	err := database.WithFamilyUserTx(ctx, appPool, familyA, userA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO public.statement_documents (
				id, family_id, uploaded_by_user_id, original_filename, file_size_bytes,
				content_hash, mime_type, encrypted_content
			) VALUES ($1, $2, $3, 'test.csv', 100, $4, 'text/csv', 'enc')
		`, docID, familyA, userA, testContentHash64)
		if err != nil {
			return err
		}

		_, err = tx.Exec(ctx, `
			INSERT INTO public.statement_import_batches (
				id, family_id, account_id, document_id, source_format, parser_version,
				status, reconciliation_status, total_rows, staged_count, duplicate_count, discrepancy_count
			) VALUES ($1, $2, $3, $4, 'csv_test', '1.0.0', 'staged', 'discrepancy', 1, 0, 0, 1)
		`, batchID, familyA, accountA, docID)
		if err != nil {
			return err
		}

		_, err = tx.Exec(ctx, `
			INSERT INTO public.statement_reconciliation_overrides (
				id, family_id, batch_id, overridden_by_user_id,
				opening_balance_minor, closing_balance_minor, calculated_balance_minor, discrepancy_delta_minor,
				reason
			) VALUES (
				$1, $2, $3, $4,
				10000, 20000, 15000, 5000,
				'Initial rationale'
			)
		`, overrideID, familyA, batchID, userA)
		return err
	})
	if err != nil {
		t.Fatalf("failed to insert override: %v", err)
	}

	// 6a. UPDATE attempt as family_app must fail with permission denied
	err = database.WithFamilyUserTx(ctx, appPool, familyA, userA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "UPDATE public.statement_reconciliation_overrides SET reason = 'Tampered reason' WHERE id = $1", overrideID)
		return err
	})
	if err == nil {
		t.Fatalf("expected UPDATE on statement_reconciliation_overrides to fail under family_app, but succeeded")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("expected permission denied error for family_app UPDATE, got: %v", err)
	}

	// 6b. UPDATE attempt under admin/superuser must be blocked by the immutability trigger
	pgPool, err := pgxpool.New(ctx, getPGConnString())
	if err != nil {
		t.Fatalf("failed to connect as admin: %v", err)
	}
	defer pgPool.Close()

	_, err = pgPool.Exec(ctx, "UPDATE public.statement_reconciliation_overrides SET reason = 'Tampered reason' WHERE id = $1", overrideID)
	if err == nil {
		t.Fatalf("expected UPDATE on statement_reconciliation_overrides to fail immutability trigger under admin, but succeeded")
	}
	if !strings.Contains(err.Error(), "strictly immutable") {
		t.Fatalf("expected 'strictly immutable' trigger error under admin, got: %v", err)
	}

	// 6c. DELETE attempt as family_app must fail with permission denied
	err = database.WithFamilyUserTx(ctx, appPool, familyA, userA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "DELETE FROM public.statement_reconciliation_overrides WHERE id = $1", overrideID)
		return err
	})
	if err == nil {
		t.Fatalf("expected DELETE on statement_reconciliation_overrides to fail under family_app, but succeeded")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("expected permission denied error for family_app DELETE, got: %v", err)
	}

	// 6d. DELETE attempt as family_worker must also fail with permission denied
	workerPool := getWorkerPool(t)
	defer workerPool.Close()
	err = database.WithFamilyTx(ctx, workerPool, familyA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "DELETE FROM public.statement_reconciliation_overrides WHERE id = $1", overrideID)
		return err
	})
	if err == nil {
		t.Fatalf("expected DELETE on statement_reconciliation_overrides to fail under family_worker, but succeeded")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("expected permission denied error for family_worker DELETE, got: %v", err)
	}
}

// 7. Promotion under restricted database permissions:
// family_app cannot directly UPDATE bank_transactions (trg_enforce_bank_evidence_immutability fires),
// but executes settle_pending_bank_transaction (SECURITY DEFINER) safely transitioning pending to posted.
func TestPromotion_RestrictedPermissions_PendingSettlement(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()

	ctx := context.Background()
	familyA, userA, accountA := setupStatementTenantFixture(t)
	pendingTxID := uuid.New()
	accountingEntryID := uuid.New()

	// Insert pending bank transaction + accounting entry
	err := database.WithFamilyUserTx(ctx, appPool, familyA, userA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO public.bank_transactions (
				id, family_id, account_id, external_id, fingerprint_hash, fingerprint_sequence,
				amount_minor, currency,
				account_amount_minor, account_currency, booking_status, operation_type,
				transaction_time, raw_description
			) VALUES (
				$1, $2, $3, 'auth_hold_123', $4, 1,
				-5000, 'RUB',
				-5000, 'RUB', 'pending', 'purchase',
				NOW() - INTERVAL '2 days', 'Restaurant Hold'
			)
		`, pendingTxID, familyA, accountA, testContentHash64)
		if err != nil {
			return err
		}

		_, err = tx.Exec(ctx, `
			INSERT INTO public.accounting_entries (
				id, family_id, bank_transaction_id, account_id,
				amount_minor, currency, classification, payer_user_id,
				beneficiary_type, entry_time, is_split, is_manual
			) VALUES (
				$1, $2, $3, $4,
				-5000, 'RUB', 'expense', $5,
				'household', NOW() - INTERVAL '2 days', FALSE, FALSE
			)
		`, accountingEntryID, familyA, pendingTxID, accountA, userA)
		return err
	})
	if err != nil {
		t.Fatalf("failed to insert pending transaction: %v", err)
	}

	// 7a. Direct UPDATE by family_app on bank_transactions must be blocked by immutability trigger
	err = database.WithFamilyUserTx(ctx, appPool, familyA, userA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "UPDATE public.bank_transactions SET booking_status = 'posted' WHERE id = $1", pendingTxID)
		return err
	})
	if err == nil {
		t.Fatalf("expected direct UPDATE on bank_transactions by family_app to be blocked by immutability trigger, but succeeded")
	}
	if !strings.Contains(err.Error(), "permission denied") && !strings.Contains(err.Error(), "bank_transactions are immutable") {
		t.Fatalf("expected bank_transactions permission denied or immutability error, got: %v", err)
	}

	// 7b. Execution via settle_pending_bank_transaction (SECURITY DEFINER) succeeds under family_app
	settledTime := time.Now().UTC()
	err = database.WithFamilyUserTx(ctx, appPool, familyA, userA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			SELECT public.settle_pending_bank_transaction($1, $2, $3, $4, $5, $6)
		`, familyA, accountA, pendingTxID, settledTime, int64(-5200), "settle_ext_999")
		return err
	})
	if err != nil {
		t.Fatalf("settle_pending_bank_transaction failed under family_app: %v", err)
	}

	// Verify transition to posted
	err = database.WithFamilyUserTx(ctx, appPool, familyA, userA, func(tx pgx.Tx) error {
		var bookingStatus, extID string
		var accountAmountMinor int64
		err := tx.QueryRow(ctx, `
			SELECT booking_status, external_id, account_amount_minor
			FROM public.bank_transactions
			WHERE id = $1
		`, pendingTxID).Scan(&bookingStatus, &extID, &accountAmountMinor)
		if err != nil {
			return err
		}
		if bookingStatus != "posted" {
			t.Errorf("expected booking_status = posted, got %s", bookingStatus)
		}
		if accountAmountMinor != -5200 {
			t.Errorf("expected settled account amount = -5200, got %d", accountAmountMinor)
		}
		if extID != "settle_ext_999" {
			t.Errorf("expected external_id = settle_ext_999, got %s", extID)
		}

		// Verify accounting entries updated
		var entryAmount int64
		err = tx.QueryRow(ctx, `
			SELECT amount_minor FROM public.accounting_entries WHERE bank_transaction_id = $1
		`, pendingTxID).Scan(&entryAmount)
		if err != nil {
			return err
		}
		if entryAmount != -5200 {
			t.Errorf("expected accounting entry amount = -5200, got %d", entryAmount)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verification query failed: %v", err)
	}
}

// 8. Deferred split integrity:
// Verify trg_bank_tx_split_integrity_deferred enforces that transactions balance with accounting entries upon commit.
func TestPromotion_DeferredSplitIntegrity(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()

	ctx := context.Background()
	familyA, userA, accountA := setupStatementTenantFixture(t)
	txID := uuid.New()

	// Attempt to commit unbalanced transaction: tx amount = -10000, entry amount = -8000
	err := database.WithFamilyUserTx(ctx, appPool, familyA, userA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO public.bank_transactions (
				id, family_id, account_id, external_id, fingerprint_hash, fingerprint_sequence,
				amount_minor, currency,
				account_amount_minor, account_currency, booking_status, operation_type,
				transaction_time, raw_description
			) VALUES (
				$1, $2, $3, 'split_test_unbalanced', $4, 1,
				-10000, 'RUB',
				-10000, 'RUB', 'posted', 'purchase',
				NOW(), 'Split Test'
			)
		`, txID, familyA, accountA, testContentHash64)
		if err != nil {
			return err
		}

		_, err = tx.Exec(ctx, `
			INSERT INTO public.accounting_entries (
				id, family_id, bank_transaction_id, account_id,
				amount_minor, currency, classification, payer_user_id,
				beneficiary_type, entry_time, is_split, is_manual
			) VALUES (
				$1, $2, $3, $4,
				-8000, 'RUB', 'expense', $5,
				'household', NOW(), FALSE, FALSE
			)
		`, uuid.New(), familyA, txID, accountA, userA)
		return err
	})

	if err == nil {
		t.Fatalf("expected deferred split integrity trigger to reject unbalanced entries at commit, but it succeeded")
	}
	if !strings.Contains(err.Error(), "Integrity violation") && !strings.Contains(err.Error(), "split integrity") && !strings.Contains(err.Error(), "trg_bank_tx_split_integrity_deferred") {
		t.Fatalf("expected split integrity violation error, got: %v", err)
	}
}

// Finding 1 verification: NULL parameter bypass in settle_pending_bank_transaction.
// All required parameters must reject NULL explicitly and not bypass validation logic.
func TestSettlePending_NullParameterChecks(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()

	ctx := context.Background()
	familyA, userA, accountA := setupStatementTenantFixture(t)
	txID := uuid.New()
	now := time.Now().UTC()

	// Seed pending transaction
	err := database.WithFamilyUserTx(ctx, appPool, familyA, userA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO public.bank_transactions (
				id, family_id, account_id, external_id, fingerprint_hash, fingerprint_sequence,
				amount_minor, currency, account_amount_minor, account_currency,
				booking_status, operation_type, transaction_time, raw_description
			) VALUES (
				$1, $2, $3, 'null_param_tx', $4, 1,
				-5000, 'RUB', -5000, 'RUB',
				'pending', 'purchase', $5, 'Null Param Test'
			)
		`, txID, familyA, accountA, testContentHash64, now)
		if err != nil {
			return err
		}

		_, err = tx.Exec(ctx, `
			INSERT INTO public.accounting_entries (
				id, family_id, bank_transaction_id, account_id,
				amount_minor, currency, classification, payer_user_id,
				beneficiary_type, entry_time, is_split, is_manual
			) VALUES (
				$1, $2, $3, $4,
				-5000, 'RUB', 'expense', $5,
				'household', $6, FALSE, FALSE
			)
		`, uuid.New(), familyA, txID, accountA, userA, now)
		return err
	})
	if err != nil {
		t.Fatalf("failed to seed pending transaction: %v", err)
	}

	testCases := []struct {
		name        string
		familyID    *uuid.UUID
		accountID   *uuid.UUID
		txID        *uuid.UUID
		settledTime *time.Time
		amountMinor *int64
		expectedErr string
	}{
		{
			name:        "NullFamilyID",
			familyID:    nil,
			accountID:   &accountA,
			txID:        &txID,
			settledTime: &now,
			amountMinor: ptrInt64(-5000),
			expectedErr: "p_family_id cannot be null",
		},
		{
			name:        "NullAccountID",
			familyID:    &familyA,
			accountID:   nil,
			txID:        &txID,
			settledTime: &now,
			amountMinor: ptrInt64(-5000),
			expectedErr: "p_account_id cannot be null",
		},
		{
			name:        "NullTxID",
			familyID:    &familyA,
			accountID:   &accountA,
			txID:        nil,
			settledTime: &now,
			amountMinor: ptrInt64(-5000),
			expectedErr: "p_transaction_id cannot be null",
		},
		{
			name:        "NullSettledTime",
			familyID:    &familyA,
			accountID:   &accountA,
			txID:        &txID,
			settledTime: nil,
			amountMinor: ptrInt64(-5000),
			expectedErr: "p_settled_time cannot be null",
		},
		{
			name:        "NullAmountMinor",
			familyID:    &familyA,
			accountID:   &accountA,
			txID:        &txID,
			settledTime: &now,
			amountMinor: nil,
			expectedErr: "p_settled_amount_minor cannot be null",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			err := database.WithFamilyUserTx(ctx, appPool, familyA, userA, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `
					SELECT public.settle_pending_bank_transaction($1, $2, $3, $4, $5, 'ext_test')
				`, tc.familyID, tc.accountID, tc.txID, tc.settledTime, tc.amountMinor)
				return err
			})

			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.expectedErr)
			}
			if !strings.Contains(err.Error(), tc.expectedErr) {
				t.Errorf("expected error %q, got: %v", tc.expectedErr, err)
			}
		})
	}
}

// Finding 5 verification: settle_pending_bank_transaction must support zero-amount settlement
// for operation_type = 'card_check', while strictly rejecting zero-amount for other operations.
func TestSettlePending_CardCheckZeroAmount(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()

	ctx := context.Background()
	familyA, userA, accountA := setupStatementTenantFixture(t)
	cardCheckTxID := uuid.New()
	purchaseTxID := uuid.New()
	now := time.Now().UTC()

	// Seed card_check hold (amount 0) and purchase hold (amount -3000)
	err := database.WithFamilyUserTx(ctx, appPool, familyA, userA, func(tx pgx.Tx) error {
		// 1. card_check hold
		_, err := tx.Exec(ctx, `
			INSERT INTO public.bank_transactions (
				id, family_id, account_id, external_id, fingerprint_hash, fingerprint_sequence,
				amount_minor, currency, account_amount_minor, account_currency,
				booking_status, operation_type, transaction_time, raw_description
			) VALUES (
				$1, $2, $3, 'card_check_ext', $4, 1,
				0, 'RUB', 0, 'RUB',
				'pending', 'card_check', $5, 'Card check hold'
			)
		`, cardCheckTxID, familyA, accountA, testContentHash64, now)
		if err != nil {
			return err
		}

		_, err = tx.Exec(ctx, `
			INSERT INTO public.accounting_entries (
				id, family_id, bank_transaction_id, account_id,
				amount_minor, currency, classification, payer_user_id,
				beneficiary_type, entry_time, is_split, is_manual
			) VALUES (
				$1, $2, $3, $4,
				0, 'RUB', 'excluded', $5,
				'household', $6, FALSE, FALSE
			)
		`, uuid.New(), familyA, cardCheckTxID, accountA, userA, now)
		if err != nil {
			return err
		}

		// 2. purchase hold
		_, err = tx.Exec(ctx, `
			INSERT INTO public.bank_transactions (
				id, family_id, account_id, external_id, fingerprint_hash, fingerprint_sequence,
				amount_minor, currency, account_amount_minor, account_currency,
				booking_status, operation_type, transaction_time, raw_description
			) VALUES (
				$1, $2, $3, 'purchase_ext', $4, 2,
				-3000, 'RUB', -3000, 'RUB',
				'pending', 'purchase', $5, 'Purchase hold'
			)
		`, purchaseTxID, familyA, accountA, testContentHash64, now)
		if err != nil {
			return err
		}

		_, err = tx.Exec(ctx, `
			INSERT INTO public.accounting_entries (
				id, family_id, bank_transaction_id, account_id,
				amount_minor, currency, classification, payer_user_id,
				beneficiary_type, entry_time, is_split, is_manual
			) VALUES (
				$1, $2, $3, $4,
				-3000, 'RUB', 'expense', $5,
				'household', $6, FALSE, FALSE
			)
		`, uuid.New(), familyA, purchaseTxID, accountA, userA, now)
		return err
	})
	if err != nil {
		t.Fatalf("failed to seed holds: %v", err)
	}

	// Settle purchase with 0 amount -> MUST FAIL
	err = database.WithFamilyUserTx(ctx, appPool, familyA, userA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			SELECT public.settle_pending_bank_transaction($1, $2, $3, $4, 0, 'purchase_settled')
		`, familyA, accountA, purchaseTxID, now)
		return err
	})
	if err == nil {
		t.Fatalf("expected zero-amount settlement for purchase to fail, but it succeeded")
	}
	if !strings.Contains(err.Error(), "settled_amount_minor cannot be zero") {
		t.Errorf("expected 'settled_amount_minor cannot be zero', got: %v", err)
	}

	// Settle card_check with 0 amount -> MUST SUCCEED
	err = database.WithFamilyUserTx(ctx, appPool, familyA, userA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			SELECT public.settle_pending_bank_transaction($1, $2, $3, $4, 0, 'card_check_settled')
		`, familyA, accountA, cardCheckTxID, now)
		return err
	})
	if err != nil {
		t.Fatalf("expected zero-amount settlement for card_check to succeed, but got: %v", err)
	}
}

// Finding 6 verification: settle_pending_bank_transaction on user-split pending transactions.
// If settled amount changes, it must fail with an explicit exception requiring user rebalance.
// If settled amount is unchanged, settlement must succeed.
func TestSettlePending_SplitTransactionIntegrity(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()

	ctx := context.Background()
	familyA, userA, accountA := setupStatementTenantFixture(t)
	txID := uuid.New()
	now := time.Now().UTC()

	// Seed pending transaction with 2 split accounting entries summing to -10000 (-6000 and -4000)
	err := database.WithFamilyUserTx(ctx, appPool, familyA, userA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO public.bank_transactions (
				id, family_id, account_id, external_id, fingerprint_hash, fingerprint_sequence,
				amount_minor, currency, account_amount_minor, account_currency,
				booking_status, operation_type, transaction_time, raw_description
			) VALUES (
				$1, $2, $3, 'split_pending_tx', $4, 1,
				-10000, 'RUB', -10000, 'RUB',
				'pending', 'purchase', $5, 'Split Hotel Hold'
			)
		`, txID, familyA, accountA, testContentHash64, now)
		if err != nil {
			return err
		}

		// Split entry 1: -6000
		_, err = tx.Exec(ctx, `
			INSERT INTO public.accounting_entries (
				id, family_id, bank_transaction_id, account_id,
				amount_minor, currency, classification, payer_user_id,
				beneficiary_type, entry_time, is_split, is_manual
			) VALUES (
				$1, $2, $3, $4,
				-6000, 'RUB', 'expense', $5,
				'household', $6, TRUE, FALSE
			)
		`, uuid.New(), familyA, txID, accountA, userA, now)
		if err != nil {
			return err
		}

		// Split entry 2: -4000
		_, err = tx.Exec(ctx, `
			INSERT INTO public.accounting_entries (
				id, family_id, bank_transaction_id, account_id,
				amount_minor, currency, classification, payer_user_id,
				beneficiary_type, entry_time, is_split, is_manual
			) VALUES (
				$1, $2, $3, $4,
				-4000, 'RUB', 'expense', $5,
				'household', $6, TRUE, FALSE
			)
		`, uuid.New(), familyA, txID, accountA, userA, now)
		return err
	})
	if err != nil {
		t.Fatalf("failed to seed split transaction: %v", err)
	}

	// 1. Settle with amount change (-10000 -> -12000) -> MUST FAIL with manual rebalance required
	err = database.WithFamilyUserTx(ctx, appPool, familyA, userA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			SELECT public.settle_pending_bank_transaction($1, $2, $3, $4, -12000, 'hotel_settled')
		`, familyA, accountA, txID, now)
		return err
	})
	if err == nil {
		t.Fatalf("expected settlement with amount change on split transaction to fail, but it succeeded")
	}
	if !strings.Contains(err.Error(), "manual rebalance required") {
		t.Errorf("expected 'manual rebalance required', got: %v", err)
	}

	// 2. Settle with UNCHANGED amount (-10000 -> -10000) -> MUST SUCCEED
	err = database.WithFamilyUserTx(ctx, appPool, familyA, userA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			SELECT public.settle_pending_bank_transaction($1, $2, $3, $4, -10000, 'hotel_settled')
		`, familyA, accountA, txID, now)
		return err
	})
	if err != nil {
		t.Fatalf("expected settlement with unchanged amount on split transaction to succeed, but got: %v", err)
	}
}

// Finding 3 verification: database-level batch lifecycle constraints.
// Committed batches cannot be deleted or reverted to draft/staged.
// Staged rows of committed batches cannot be deleted or modified.
func TestStatementBatch_LifecycleTriggers(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()

	ctx := context.Background()
	familyA, userA, accountA := setupStatementTenantFixture(t)
	repo := NewRepository()

	docID := uuid.New()
	batchStagedID := uuid.New()
	batchCommittedID := uuid.New()
	stagedRowCommittedID := uuid.New()
	now := time.Now().UTC()

	err := database.WithFamilyUserTx(ctx, appPool, familyA, userA, func(tx pgx.Tx) error {
		// Document
		err := repo.InsertDocument(ctx, tx, &StatementDocument{
			ID:               docID,
			FamilyID:         familyA,
			UploadedByUserID: userA,
			OriginalFilename: "lifecycle_test.csv",
			FileSizeBytes:    100,
			ContentHash:      testContentHash64,
			MIMEType:         "text/csv",
			EncryptedContent: []byte("encrypted"),
		})
		if err != nil {
			return err
		}

		// 1. Batch in 'staged' status
		b1 := StatementImportBatch{
			ID:                   batchStagedID,
			FamilyID:             familyA,
			AccountID:            accountA,
			DocumentID:           docID,
			SourceFormat:         "generic_csv",
			ParserVersion:        "v1.0.0",
			Status:               BatchStatusStaged,
			ReconciliationStatus: ReconciliationNonReconcilable,
			TotalRows:            1,
			StagedCount:          1,
		}
		if err := repo.InsertBatch(ctx, tx, &b1); err != nil {
			return err
		}

		// 2. Batch in 'committed' status
		b2 := StatementImportBatch{
			ID:                   batchCommittedID,
			FamilyID:             familyA,
			AccountID:            accountA,
			DocumentID:           docID,
			SourceFormat:         "generic_csv",
			ParserVersion:        "v1.0.0",
			Status:               BatchStatusCommitted,
			ReconciliationStatus: ReconciliationNonReconcilable,
			TotalRows:            1,
			StagedCount:          1,
		}
		if err := repo.InsertBatch(ctx, tx, &b2); err != nil {
			return err
		}

		// 3. Staged row belonging to the committed batch
		return repo.InsertStagedTransactions(ctx, tx, []StagedBankTransaction{
			{
				ID:                 stagedRowCommittedID,
				FamilyID:           familyA,
				BatchID:            batchCommittedID,
				AccountID:          accountA,
				RowIndex:           1,
				TransactionTime:    now,
				AmountMinor:        -1000,
				Currency:           "RUB",
				AccountAmountMinor: -1000,
				AccountCurrency:    "RUB",
				OperationType:      "purchase",
				BookingStatus:      "posted",
				RawDescription:     "Committed Staged Row",
				ImportFingerprint:  ComputeImportFingerprint(accountA, "posted", nil, now, -1000, "RUB", "Committed Staged Row"),
				DuplicateStatus:    DuplicateStatusNew,
				ResolutionAction:   ResolutionActionCommit,
			},
		})
	})
	if err != nil {
		t.Fatalf("failed to seed batches: %v", err)
	}

	// Test 1: Deleting a staged (non-committed) batch succeeds
	err = database.WithFamilyUserTx(ctx, appPool, familyA, userA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "DELETE FROM public.statement_import_batches WHERE id = $1", batchStagedID)
		return err
	})
	if err != nil {
		t.Fatalf("deleting staged batch failed: %v", err)
	}

	// Test 2: Deleting a committed batch MUST FAIL via trg_statement_batch_lifecycle
	err = database.WithFamilyUserTx(ctx, appPool, familyA, userA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "DELETE FROM public.statement_import_batches WHERE id = $1", batchCommittedID)
		return err
	})
	if err == nil {
		t.Fatalf("expected deleting committed batch to fail, but it succeeded")
	}
	if !strings.Contains(err.Error(), "cannot be deleted") {
		t.Errorf("expected 'cannot be deleted', got: %v", err)
	}

	// Test 3: Updating a committed batch (reverting to staged) MUST FAIL via trg_statement_batch_lifecycle
	err = database.WithFamilyUserTx(ctx, appPool, familyA, userA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "UPDATE public.statement_import_batches SET status = 'staged' WHERE id = $1", batchCommittedID)
		return err
	})
	if err == nil {
		t.Fatalf("expected reverting committed batch to fail, but it succeeded")
	}
	if !strings.Contains(err.Error(), "immutable and cannot be modified or reverted") {
		t.Errorf("expected 'immutable and cannot be modified or reverted', got: %v", err)
	}

	// Test 4: Deleting a staged row belonging to a committed batch MUST FAIL via trg_staged_tx_lifecycle
	err = database.WithFamilyUserTx(ctx, appPool, familyA, userA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "DELETE FROM public.staged_bank_transactions WHERE id = $1", stagedRowCommittedID)
		return err
	})
	if err == nil {
		t.Fatalf("expected deleting staged row of committed batch to fail, but it succeeded")
	}
	if !strings.Contains(err.Error(), "cannot be modified or deleted") {
		t.Errorf("expected 'cannot be modified or deleted', got: %v", err)
	}

	// Test 5: Updating a staged row belonging to a committed batch MUST FAIL via trg_staged_tx_lifecycle
	err = database.WithFamilyUserTx(ctx, appPool, familyA, userA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "UPDATE public.staged_bank_transactions SET resolution_action = 'skip_duplicate' WHERE id = $1", stagedRowCommittedID)
		return err
	})
	if err == nil {
		t.Fatalf("expected updating staged row of committed batch to fail, but it succeeded")
	}
	if !strings.Contains(err.Error(), "cannot be modified or deleted") {
		t.Errorf("expected 'cannot be modified or deleted', got: %v", err)
	}
}

func ptrInt64(v int64) *int64 {
	return &v
}

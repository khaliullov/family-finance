package statement

import (
	"bytes"
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/khaliullov/family-finance/internal/database"
)

// -----------------------------------------------------------------------------
// Audit Test 1: Migration 000015 Category Deletion Behaviour
// Verifies whether deleting a category referenced by staged_bank_transactions.category_id
// fails with a NOT NULL constraint violation on family_id due to composite FK SET NULL.
// -----------------------------------------------------------------------------
func TestAudit_Migration000015_CategoryDeletion(t *testing.T) {
	pool := getAppPool(t)
	defer pool.Close()

	familyID, userID, accountID := setupStatementTenantFixture(t)
	ctx := context.Background()

	var categoryID uuid.UUID
	// 1. Create a category under familyID
	err := database.WithFamilyUserTx(ctx, pool, familyID, userID, func(tx pgx.Tx) error {
		categoryID = uuid.New()
		_, err := tx.Exec(ctx, `
			INSERT INTO public.categories (id, family_id, name, type)
			VALUES ($1, $2, 'Audit Test Category', 'expense')
		`, categoryID, familyID)
		return err
	})
	if err != nil {
		t.Fatalf("failed to create category: %v", err)
	}

	// 2. Upload and stage a statement
	parsers := NewDefaultParserRegistry()
	svc := NewService(pool, testCryptoKey, parsers)

	stmtCSV := stringsJoin(
		`"Имя счёта";"Дата операции";"Дата платежа";"Номер карты";"Статус";"Сумма операции";"Валюта операции";"Сумма в валюте счёта";"Валюта счёта";"Кэшбэк";"Категория";"MCC";"Описание";"Бонусы (включая кэшбэк)";"Округление на инвесткопилку";"Сумма операции с округлением"`,
		`"Primary Checking";"21.11.2026 12:00:00";"21.11.2026 12:00:00";"*1234";"OK";"-500.00";"RUB";"-500.00";"RUB";"0";"Супермаркеты";"5411";"Test Store";"0";"0";"500.00"`,
	)

	batch, err := svc.UploadAndStage(
		ctx, familyID, userID, accountID,
		"test_cat_delete.csv",
		bytes.NewReader([]byte(stmtCSV)),
		int64(len(stmtCSV)),
		nil,
	)
	if err != nil {
		t.Fatalf("UploadAndStage failed: %v", err)
	}

	// 3. Assign category to staged row
	prev, err := svc.GetImportPreview(ctx, familyID, batch.ID)
	if err != nil || len(prev.Rows) != 1 {
		t.Fatalf("GetImportPreview failed: %v", err)
	}
	rowID := prev.Rows[0].ID

	_, err = svc.UpdateStagedRow(ctx, familyID, userID, batch.ID, rowID, UpdateStagedRowRequest{
		CategoryID: &categoryID,
	})
	if err != nil {
		t.Fatalf("UpdateStagedRow with categoryID failed: %v", err)
	}

	// 4. Adversarial Check: Attempt to delete the category
	// In PostgreSQL, FOREIGN KEY (family_id, category_id) REFERENCES categories(family_id, id) ON DELETE SET NULL
	// attempts to set BOTH family_id AND category_id to NULL.
	// Since staged_bank_transactions.family_id is NOT NULL, this will trigger a constraint violation!
	delErr := database.WithFamilyUserTx(ctx, pool, familyID, userID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			DELETE FROM public.categories
			WHERE family_id = $1 AND id = $2
		`, familyID, categoryID)
		return err
	})

	t.Logf("Result of deleting category referenced by staged row: %v", delErr)
	if delErr != nil {
		t.Logf("CONFIRMED FINDING: Deleting a category fails unexpectedly because ON DELETE SET NULL attempts to NULL out NOT NULL family_id: %v", delErr)
	} else {
		t.Logf("Category deletion succeeded (SET NULL handled without error)")
	}
}

// -----------------------------------------------------------------------------
// Audit Test 2: Unapproved Promotion & Revision Integrity Bypass in Service
// Verifies whether service.PromoteBatch allows promoting a batch that is in 'staged'
// status (never approved) or where approved_batch_revision does not match batch_revision.
// -----------------------------------------------------------------------------
func TestAudit_PromoteBatch_UnapprovedBypass(t *testing.T) {
	pool := getAppPool(t)
	defer pool.Close()

	familyID, userID, accountID := setupStatementTenantFixture(t)
	ctx := context.Background()

	parsers := NewDefaultParserRegistry()
	svc := NewService(pool, testCryptoKey, parsers)

	stmtCSV := stringsJoin(
		`"Имя счёта";"Дата операции";"Дата платежа";"Номер карты";"Статус";"Сумма операции";"Валюта операции";"Сумма в валюте счёта";"Валюта счёта";"Кэшбэк";"Категория";"MCC";"Описание";"Бонусы (включая кэшбэк)";"Округление на инвесткопилку";"Сумма операции с округлением"`,
		`"Primary Checking";"21.11.2026 12:00:00";"21.11.2026 12:00:00";"*1234";"OK";"-300.00";"RUB";"-300.00";"RUB";"0";"Супермаркеты";"5411";"Unapproved Store";"0";"0";"300.00"`,
	)

	batch, err := svc.UploadAndStage(
		ctx, familyID, userID, accountID,
		"test_unapproved.csv",
		bytes.NewReader([]byte(stmtCSV)),
		int64(len(stmtCSV)),
		nil,
	)
	if err != nil {
		t.Fatalf("UploadAndStage failed: %v", err)
	}

	// Verify batch status is staged
	summary, err := svc.GetImportSummary(ctx, familyID, batch.ID)
	if err != nil {
		t.Fatalf("GetImportSummary failed: %v", err)
	}
	if summary.Status != BatchStatusStaged {
		t.Fatalf("expected status staged, got %s", summary.Status)
	}

	// ADVERSARIAL ATTEMPT: Promote directly without calling ApproveBatch!
	// In service.go line 801: `if status != BatchStatusStaged && status != BatchStatusApproved`
	// service.PromoteBatch accepts BatchStatusStaged!
	res, promoteErr := svc.PromoteBatch(ctx, familyID, userID, batch.ID)
	t.Logf("Result of promoting directly from 'staged' status via service.PromoteBatch: res=%+v, err=%v", res, promoteErr)

	if promoteErr == nil {
		t.Logf("CONFIRMED FINDING: service.PromoteBatch permits promoting an unapproved batch in 'staged' status! TransactionsCommitted=%d", res.TransactionsCommitted)
	} else {
		t.Logf("PromoteBatch rejected unapproved batch: %v", promoteErr)
	}
}

// -----------------------------------------------------------------------------
// Audit Test 3: Approval Revision Bypass after Staged Row Mutation
// Verifies whether modifying a staged row after approval allows promotion of mutated rows.
// -----------------------------------------------------------------------------
func TestAudit_ApprovalRevisionBypass_OnMutation(t *testing.T) {
	pool := getAppPool(t)
	defer pool.Close()

	familyID, userID, accountID := setupStatementTenantFixture(t)
	ctx := context.Background()

	parsers := NewDefaultParserRegistry()
	svc := NewService(pool, testCryptoKey, parsers)

	stmtCSV := stringsJoin(
		`"Имя счёта";"Дата операции";"Дата платежа";"Номер карты";"Статус";"Сумма операции";"Валюта операции";"Сумма в валюте счёта";"Валюта счёта";"Кэшбэк";"Категория";"MCC";"Описание";"Бонусы (включая кэшбэк)";"Округление на инвесткопилку";"Сумма операции с округлением"`,
		`"Primary Checking";"21.11.2026 12:00:00";"21.11.2026 12:00:00";"*1234";"OK";"-700.00";"RUB";"-700.00";"RUB";"0";"Супермаркеты";"5411";"Revision Store";"0";"0";"700.00"`,
	)

	batch, err := svc.UploadAndStage(
		ctx, familyID, userID, accountID,
		"test_rev_mutation.csv",
		bytes.NewReader([]byte(stmtCSV)),
		int64(len(stmtCSV)),
		nil,
	)
	if err != nil {
		t.Fatalf("UploadAndStage failed: %v", err)
	}

	// 1. Explicitly approve batch at revision 1
	notes := "Initial approval"
	_, err = svc.ApproveBatch(ctx, familyID, userID, batch.ID, ApproveBatchRequest{Notes: &notes})
	if err != nil {
		t.Fatalf("ApproveBatch failed: %v", err)
	}

	// 2. Mutate staged row (change operation_type to atm_cash)
	prev, err := svc.GetImportPreview(ctx, familyID, batch.ID)
	if err != nil || len(prev.Rows) != 1 {
		t.Fatalf("GetImportPreview failed: %v", err)
	}
	rowID := prev.Rows[0].ID
	newOp := "atm_cash"

	_, err = svc.UpdateStagedRow(ctx, familyID, userID, batch.ID, rowID, UpdateStagedRowRequest{
		OperationType: &newOp,
	})
	if err != nil {
		t.Fatalf("UpdateStagedRow failed: %v", err)
	}

	// Check batch status and revisions: batch_revision should be 2, approved_batch_revision NULL, status 'staged'
	sumAfterMutate, err := svc.GetImportSummary(ctx, familyID, batch.ID)
	if err != nil {
		t.Fatalf("GetImportSummary failed: %v", err)
	}
	t.Logf("After mutation: status=%s, batch_revision=%d, approved_batch_revision=%v",
		sumAfterMutate.Status, sumAfterMutate.BatchRevision, sumAfterMutate.ApprovedBatchRevision)

	// 3. Attempt promotion directly via service.PromoteBatch
	res, promoteErr := svc.PromoteBatch(ctx, familyID, userID, batch.ID)
	t.Logf("Result of PromoteBatch after mutation: res=%+v, err=%v", res, promoteErr)

	if promoteErr == nil {
		t.Logf("CONFIRMED FINDING: service.PromoteBatch promoted mutated batch without requiring re-approval! Revision check was bypassed.")
	}
}

// -----------------------------------------------------------------------------
// Audit Test 4: Pending-to-Posted Settlement with Amount Change (-45.00 -> -47.50)
// Tests whether a cleared transaction with an amount difference matches the pending hold
// or causes duplicate ledger entries (double-counting).
// -----------------------------------------------------------------------------
func TestAudit_PendingToPosted_AmountChange_DoubleCounting(t *testing.T) {
	pool := getAppPool(t)
	defer pool.Close()

	familyID, userID, accountID := setupStatementTenantFixture(t)
	ctx := context.Background()

	parsers := NewDefaultParserRegistry()
	svc := NewService(pool, testCryptoKey, parsers)

	// Step 1: Upload statement with Pending Hold for -45.00 RUB (-4500 minor units)
	stmt1CSV := stringsJoin(
		`"Имя счёта";"Дата операции";"Дата платежа";"Номер карты";"Статус";"Сумма операции";"Валюта операции";"Сумма в валюте счёта";"Валюта счёта";"Кэшбэк";"Категория";"MCC";"Описание";"Бонусы (включая кэшбэк)";"Округление на инвесткопилку";"Сумма операции с округлением"`,
		`"Primary Checking";"20.11.2026 14:00:00";"";"*1234";"В обработке";"-45.00";"RUB";"-45.00";"RUB";"0";"Рестораны";"5812";"Cafe Paris";"0";"0";"45.00"`,
	)

	batch1, err := svc.UploadAndStage(
		ctx, familyID, userID, accountID,
		"stmt1_pending_45.csv",
		bytes.NewReader([]byte(stmt1CSV)),
		int64(len(stmt1CSV)),
		nil,
	)
	if err != nil {
		t.Fatalf("UploadAndStage stmt1 failed: %v", err)
	}

	_, err = svc.ApproveBatch(ctx, familyID, userID, batch1.ID, ApproveBatchRequest{})
	if err != nil {
		t.Fatalf("ApproveBatch stmt1 failed: %v", err)
	}

	_, err = svc.PromoteBatch(ctx, familyID, userID, batch1.ID)
	if err != nil {
		t.Fatalf("PromoteBatch stmt1 failed: %v", err)
	}

	// Verify pending transaction exists in bank_transactions and accounting_entries
	var pendingTxCount, initialEntryCount int
	var initialLedgerSum int64
	err = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		_ = tx.QueryRow(ctx, `SELECT count(*) FROM public.bank_transactions WHERE family_id = $1 AND booking_status = 'pending'`, familyID).Scan(&pendingTxCount)
		_ = tx.QueryRow(ctx, `SELECT count(*), COALESCE(SUM(amount_minor), 0) FROM public.accounting_entries WHERE family_id = $1`, familyID).Scan(&initialEntryCount, &initialLedgerSum)
		return nil
	})
	if err != nil {
		t.Fatalf("DB check failed: %v", err)
	}
	t.Logf("After stmt1: pendingTxCount=%d, accountingEntries=%d, ledgerSum=%d", pendingTxCount, initialEntryCount, initialLedgerSum)
	if pendingTxCount != 1 || initialEntryCount != 1 || initialLedgerSum != -4500 {
		t.Fatalf("unexpected state after stmt1: pendingTx=%d, entries=%d, sum=%d", pendingTxCount, initialEntryCount, initialLedgerSum)
	}

	// Step 2: Upload statement 2 where the transaction posted for -47.50 RUB (-4750 minor units, e.g. tips or fx)
	stmt2CSV := stringsJoin(
		`"Имя счёта";"Дата операции";"Дата платежа";"Номер карты";"Статус";"Сумма операции";"Валюта операции";"Сумма в валюте счёта";"Валюта счёта";"Кэшбэк";"Категория";"MCC";"Описание";"Бонусы (включая кэшбэк)";"Округление на инвесткопилку";"Сумма операции с округлением"`,
		`"Primary Checking";"20.11.2026 14:00:00";"22.11.2026 10:00:00";"*1234";"OK";"-47.50";"RUB";"-47.50";"RUB";"0";"Рестораны";"5812";"Cafe Paris";"0";"0";"47.50"`,
	)

	batch2, err := svc.UploadAndStage(
		ctx, familyID, userID, accountID,
		"stmt2_posted_47_50.csv",
		bytes.NewReader([]byte(stmt2CSV)),
		int64(len(stmt2CSV)),
		nil,
	)
	if err != nil {
		t.Fatalf("UploadAndStage stmt2 failed: %v", err)
	}

	// Inspect preview of stmt2
	prev2, err := svc.GetImportPreview(ctx, familyID, batch2.ID)
	if err != nil || len(prev2.Rows) != 1 {
		t.Fatalf("GetImportPreview stmt2 failed: %v", err)
	}

	stagedRow := prev2.Rows[0]
	t.Logf("Stmt2 Staged Row: DuplicateStatus=%s, ResolutionAction=%s, MatchedExistingTxID=%v",
		stagedRow.DuplicateStatus, stagedRow.ResolutionAction, stagedRow.MatchedExistingTxID)

	// If deduplication matched the pending hold: DuplicateStatus should be settlement_update, ResolutionAction apply_settlement
	// If deduplication FAILED to match because amount differs: DuplicateStatus will be 'new', ResolutionAction 'commit'
	if stagedRow.DuplicateStatus != DuplicateStatusSettlementUpdate {
		t.Logf("CONFIRMED FINDING: Deduplication failed to match pending hold (-45.00) with posted settlement (-47.50)! Marked as: %s / %s",
			stagedRow.DuplicateStatus, stagedRow.ResolutionAction)
	}

	// Promote stmt2
	_, err = svc.ApproveBatch(ctx, familyID, userID, batch2.ID, ApproveBatchRequest{})
	if err != nil {
		t.Fatalf("ApproveBatch stmt2 failed: %v", err)
	}
	_, err = svc.PromoteBatch(ctx, familyID, userID, batch2.ID)
	if err != nil {
		t.Fatalf("PromoteBatch stmt2 failed: %v", err)
	}

	// Check final database state: How many bank_transactions and accounting_entries exist now?
	var finalTotalBankTx, finalPendingTx, finalPostedTx, finalEntryCount int
	var finalLedgerSum int64
	err = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		_ = tx.QueryRow(ctx, `SELECT count(*) FROM public.bank_transactions WHERE family_id = $1`, familyID).Scan(&finalTotalBankTx)
		_ = tx.QueryRow(ctx, `SELECT count(*) FROM public.bank_transactions WHERE family_id = $1 AND booking_status = 'pending'`, familyID).Scan(&finalPendingTx)
		_ = tx.QueryRow(ctx, `SELECT count(*) FROM public.bank_transactions WHERE family_id = $1 AND booking_status = 'posted'`, familyID).Scan(&finalPostedTx)
		_ = tx.QueryRow(ctx, `SELECT count(*), COALESCE(SUM(amount_minor), 0) FROM public.accounting_entries WHERE family_id = $1`, familyID).Scan(&finalEntryCount, &finalLedgerSum)
		return nil
	})
	if err != nil {
		t.Fatalf("DB check failed: %v", err)
	}

	t.Logf("Final State: totalBankTx=%d (pending=%d, posted=%d), accountingEntries=%d, totalLedgerSum=%d (expected -4750)",
		finalTotalBankTx, finalPendingTx, finalPostedTx, finalEntryCount, finalLedgerSum)

	if finalTotalBankTx > 1 || finalEntryCount > 1 || finalPendingTx > 0 {
		t.Logf("CONFIRMED FINANCIAL FINDING: Double-counting occurred! Both pending hold (-45.00) and posted settlement (-47.50) exist simultaneously. Total ledger impact = %d minor units (expected -4750, actual %d)!",
			finalLedgerSum, finalLedgerSum)
	}
}

// -----------------------------------------------------------------------------
// Audit Test 5: Cancelled Operations in Subsequent Statements
// Verifies what happens when a previously promoted pending hold appears as cancelled in a later statement.
// -----------------------------------------------------------------------------
func TestAudit_PendingToCancelled_HoldNeverCleared(t *testing.T) {
	pool := getAppPool(t)
	defer pool.Close()

	familyID, userID, accountID := setupStatementTenantFixture(t)
	ctx := context.Background()

	parsers := NewDefaultParserRegistry()
	svc := NewService(pool, testCryptoKey, parsers)

	// Step 1: Promote pending hold
	stmt1CSV := stringsJoin(
		`"Имя счёта";"Дата операции";"Дата платежа";"Номер карты";"Статус";"Сумма операции";"Валюта операции";"Сумма в валюте счёта";"Валюта счёта";"Кэшбэк";"Категория";"MCC";"Описание";"Бонусы (включая кэшбэк)";"Округление на инвесткопилку";"Сумма операции с округлением"`,
		`"Primary Checking";"20.11.2026 14:00:00";"";"*1234";"В обработке";"-1000.00";"RUB";"-1000.00";"RUB";"0";"Отели";"7011";"Hotel Grand";"0";"0";"1000.00"`,
	)

	batch1, err := svc.UploadAndStage(ctx, familyID, userID, accountID, "stmt1.csv", bytes.NewReader([]byte(stmt1CSV)), int64(len(stmt1CSV)), nil)
	if err != nil {
		t.Fatalf("UploadAndStage stmt1 failed: %v", err)
	}
	_, _ = svc.ApproveBatch(ctx, familyID, userID, batch1.ID, ApproveBatchRequest{})
	_, _ = svc.PromoteBatch(ctx, familyID, userID, batch1.ID)

	// Step 2: Statement 2 arrives where the operation failed / was cancelled ("Ошибка")
	stmt2CSV := stringsJoin(
		`"Имя счёта";"Дата операции";"Дата платежа";"Номер карты";"Статус";"Сумма операции";"Валюта операции";"Сумма в валюте счёта";"Валюта счёта";"Кэшбэк";"Категория";"MCC";"Описание";"Бонусы (включая кэшбэк)";"Округление на инвесткопилку";"Сумма операции с округлением"`,
		`"Primary Checking";"20.11.2026 14:00:00";"";"*1234";"Ошибка";"-1000.00";"RUB";"-1000.00";"RUB";"0";"Отели";"7011";"Hotel Grand";"0";"0";"1000.00"`,
	)

	batch2, err := svc.UploadAndStage(ctx, familyID, userID, accountID, "stmt2.csv", bytes.NewReader([]byte(stmt2CSV)), int64(len(stmt2CSV)), nil)
	if err != nil {
		t.Fatalf("UploadAndStage stmt2 failed: %v", err)
	}

	prev2, err := svc.GetImportPreview(ctx, familyID, batch2.ID)
	if err != nil || len(prev2.Rows) != 1 {
		t.Fatalf("GetImportPreview stmt2 failed: %v", err)
	}

	stRow := prev2.Rows[0]
	t.Logf("Cancelled Staged Row in Stmt2: DuplicateStatus=%s, ResolutionAction=%s, MatchedExistingTxID=%v",
		stRow.DuplicateStatus, stRow.ResolutionAction, stRow.MatchedExistingTxID)

	// Check if pending bank_transaction is still in status 'pending'
	var pendingCount int
	_ = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM public.bank_transactions WHERE family_id = $1 AND booking_status = 'pending'`, familyID).Scan(&pendingCount)
	})

	t.Logf("Pending hold count in bank_transactions after cancelled statement upload: %d", pendingCount)
	if pendingCount == 1 && stRow.MatchedExistingTxID == nil {
		t.Logf("CONFIRMED FINDING: Cancelled statement operation is skipped without linking or resolving the prior pending hold! The pending hold remains active in the ledger forever.")
	}
}

// -----------------------------------------------------------------------------
// Audit Test 6: Settlement Matching Without Merchant/Description Check
// Verifies whether Priority 3 matches a posted transaction to an unrelated pending hold
// purely because amount and date range match.
// -----------------------------------------------------------------------------
func TestAudit_SettlementMatching_UnrelatedMerchant(t *testing.T) {
	pool := getAppPool(t)
	defer pool.Close()

	familyID, userID, accountID := setupStatementTenantFixture(t)
	ctx := context.Background()

	parsers := NewDefaultParserRegistry()
	svc := NewService(pool, testCryptoKey, parsers)

	// Step 1: Pending Hold at "Shell Gas" for -2000.00 RUB
	stmt1CSV := stringsJoin(
		`"Имя счёта";"Дата операции";"Дата платежа";"Номер карты";"Статус";"Сумма операции";"Валюта операции";"Сумма в валюте счёта";"Валюта счёта";"Кэшбэк";"Категория";"MCC";"Описание";"Бонусы (включая кэшбэк)";"Округление на инвесткопилку";"Сумма операции с округлением"`,
		`"Primary Checking";"20.11.2026 10:00:00";"";"*1234";"В обработке";"-2000.00";"RUB";"-2000.00";"RUB";"0";"Топливо";"5541";"Shell Gas Station";"0";"0";"2000.00"`,
	)

	batch1, err := svc.UploadAndStage(ctx, familyID, userID, accountID, "stmt1_gas.csv", bytes.NewReader([]byte(stmt1CSV)), int64(len(stmt1CSV)), nil)
	if err != nil {
		t.Fatalf("UploadAndStage stmt1 failed: %v", err)
	}
	_, _ = svc.ApproveBatch(ctx, familyID, userID, batch1.ID, ApproveBatchRequest{})
	_, _ = svc.PromoteBatch(ctx, familyID, userID, batch1.ID)

	// Step 2: Posted Transaction at "IKEA Furniture" for -2000.00 RUB on 21.11.2026
	stmt2CSV := stringsJoin(
		`"Имя счёта";"Дата операции";"Дата платежа";"Номер карты";"Статус";"Сумма операции";"Валюта операции";"Сумма в валюте счёта";"Валюта счёта";"Кэшбэк";"Категория";"MCC";"Описание";"Бонусы (включая кэшбэк)";"Округление на инвесткопилку";"Сумма операции с округлением"`,
		`"Primary Checking";"21.11.2026 15:00:00";"21.11.2026 15:00:00";"*1234";"OK";"-2000.00";"RUB";"-2000.00";"RUB";"0";"Дом и ремонт";"5712";"IKEA Store";"0";"0";"2000.00"`,
	)

	batch2, err := svc.UploadAndStage(ctx, familyID, userID, accountID, "stmt2_ikea.csv", bytes.NewReader([]byte(stmt2CSV)), int64(len(stmt2CSV)), nil)
	if err != nil {
		t.Fatalf("UploadAndStage stmt2 failed: %v", err)
	}

	prev2, err := svc.GetImportPreview(ctx, familyID, batch2.ID)
	if err != nil || len(prev2.Rows) != 1 {
		t.Fatalf("GetImportPreview stmt2 failed: %v", err)
	}

	stRow := prev2.Rows[0]
	t.Logf("IKEA Row Matched Status: DuplicateStatus=%s, ResolutionAction=%s, MatchedExistingTxID=%v",
		stRow.DuplicateStatus, stRow.ResolutionAction, stRow.MatchedExistingTxID)

	if stRow.DuplicateStatus == DuplicateStatusSettlementUpdate {
		t.Logf("CONFIRMED FINDING: Priority 3 settlement matching matched IKEA (-2000) to Shell Gas (-2000) because amount matches and date is within 7 days, ignoring merchant and description!")
	}
}

// -----------------------------------------------------------------------------
// Audit Test 7: Ledger Promotion Atomicity and Failure Rollback
// Tests that failure injection at any stage in PromoteBatch completely rolls back
// and leaves zero partial financial records.
// -----------------------------------------------------------------------------
func TestAudit_PromotionAtomicity_RollbackOnFailure(t *testing.T) {
	pool := getAppPool(t)
	defer pool.Close()

	familyID, userID, accountID := setupStatementTenantFixture(t)
	ctx := context.Background()

	parsers := NewDefaultParserRegistry()
	svc := NewService(pool, testCryptoKey, parsers)

	stmtCSV := stringsJoin(
		`"Имя счёта";"Дата операции";"Дата платежа";"Номер карты";"Статус";"Сумма операции";"Валюта операции";"Сумма в валюте счёта";"Валюта счёта";"Кэшбэк";"Категория";"MCC";"Описание";"Бонусы (включая кэшбэк)";"Округление на инвесткопилку";"Сумма операции с округлением"`,
		`"Primary Checking";"21.11.2026 12:00:00";"21.11.2026 12:00:00";"*1234";"OK";"-100.00";"RUB";"-100.00";"RUB";"0";"Супермаркеты";"5411";"Row 1";"0";"0";"100.00"`,
		`"Primary Checking";"21.11.2026 12:05:00";"21.11.2026 12:05:00";"*1234";"OK";"-200.00";"RUB";"-200.00";"RUB";"0";"Супермаркеты";"5411";"Row 2";"0";"0";"200.00"`,
	)

	batch, err := svc.UploadAndStage(ctx, familyID, userID, accountID, "atomicity_test.csv", bytes.NewReader([]byte(stmtCSV)), int64(len(stmtCSV)), nil)
	if err != nil {
		t.Fatalf("UploadAndStage failed: %v", err)
	}

	_, err = svc.ApproveBatch(ctx, familyID, userID, batch.ID, ApproveBatchRequest{})
	if err != nil {
		t.Fatalf("ApproveBatch failed: %v", err)
	}

	// Corrupt row 2 in database with invalid operation_type to trigger failure mid-promotion
	err = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE public.staged_bank_transactions
			SET operation_type = 'corrupted_invalid_op_type'
			WHERE batch_id = $1 AND row_index = 2
		`, batch.ID)
		return err
	})
	if err != nil {
		t.Fatalf("failed to inject corruption: %v", err)
	}

	// Attempt promotion: Should fail during insertion of bank_transactions for row 2 (check constraint)
	_, promoteErr := svc.PromoteBatch(ctx, familyID, userID, batch.ID)
	t.Logf("PromoteBatch with corrupted row 2 returned expected error: %v", promoteErr)
	if promoteErr == nil {
		t.Fatalf("expected promotion failure, got nil")
	}

	// Verify ATOMICITY: No transactions from Row 1 must exist in bank_transactions or accounting_entries
	var bankTxCount, entryCount int
	var batchStatus string
	_ = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		_ = tx.QueryRow(ctx, `SELECT count(*) FROM public.bank_transactions WHERE family_id = $1`, familyID).Scan(&bankTxCount)
		_ = tx.QueryRow(ctx, `SELECT count(*) FROM public.accounting_entries WHERE family_id = $1`, familyID).Scan(&entryCount)
		_ = tx.QueryRow(ctx, `SELECT status FROM public.statement_import_batches WHERE family_id = $1 AND id = $2`, familyID, batch.ID).Scan(&batchStatus)
		return nil
	})

	t.Logf("Atomicity check: bankTxCount=%d, entryCount=%d, batchStatus=%s", bankTxCount, entryCount, batchStatus)
	if bankTxCount != 0 || entryCount != 0 {
		t.Fatalf("ATOMICITY VIOLATION: partial promotion occurred! bankTxCount=%d, entryCount=%d", bankTxCount, entryCount)
	} else {
		t.Logf("VERIFIED: Promotion is atomic. Single transaction rolled back cleanly with 0 partial records.")
	}
}

// -----------------------------------------------------------------------------
// Audit Test 8: Cross-Family IDOR and Tenant Isolation
// -----------------------------------------------------------------------------
func TestAudit_CrossFamily_IDOR_Isolation(t *testing.T) {
	pool := getAppPool(t)
	defer pool.Close()

	familyA, userA, accountA := setupStatementTenantFixture(t)
	familyB, userB, _ := setupStatementTenantFixture(t)
	ctx := context.Background()

	parsers := NewDefaultParserRegistry()
	svcA := NewService(pool, testCryptoKey, parsers)
	svcB := NewService(pool, testCryptoKey, parsers)

	stmtCSV := stringsJoin(
		`"Имя счёта";"Дата операции";"Дата платежа";"Номер карты";"Статус";"Сумма операции";"Валюта операции";"Сумма в валюте счёта";"Валюта счёта";"Кэшбэк";"Категория";"MCC";"Описание";"Бонусы (включая кэшбэк)";"Округление на инвесткопилку";"Сумма операции с округлением"`,
		`"Primary Checking";"21.11.2026 12:00:00";"21.11.2026 12:00:00";"*1234";"OK";"-150.00";"RUB";"-150.00";"RUB";"0";"Супермаркеты";"5411";"Store A";"0";"0";"150.00"`,
	)

	batchA, err := svcA.UploadAndStage(ctx, familyA, userA, accountA, "tenant_a.csv", bytes.NewReader([]byte(stmtCSV)), int64(len(stmtCSV)), nil)
	if err != nil {
		t.Fatalf("UploadAndStage family A failed: %v", err)
	}

	// 1. Family B attempts to get preview of Family A batch
	_, err = svcB.GetImportPreview(ctx, familyB, batchA.ID)
	if err == nil {
		t.Fatalf("IDOR VULNERABILITY: Family B successfully retrieved Family A preview!")
	}
	t.Logf("Family B preview of Family A batch correctly blocked: %v", err)

	// 2. Family B attempts to approve Family A batch
	_, err = svcB.ApproveBatch(ctx, familyB, userB, batchA.ID, ApproveBatchRequest{})
	if err == nil {
		t.Fatalf("IDOR VULNERABILITY: Family B successfully approved Family A batch!")
	}
	t.Logf("Family B approval of Family A batch correctly blocked: %v", err)

	// 3. Family B attempts to promote Family A batch
	_, err = svcB.PromoteBatch(ctx, familyB, userB, batchA.ID)
	if err == nil {
		t.Fatalf("IDOR VULNERABILITY: Family B successfully promoted Family A batch!")
	}
	t.Logf("Family B promotion of Family A batch correctly blocked: %v", err)
}

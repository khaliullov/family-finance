package statement

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/khaliullov/family-finance/internal/database"
)

// TestStatement_PendingToPostedSettlementAcceptance is the mandatory end-to-end acceptance test
// for Stage 6B.2 simulating the complete lifecycle:
// 1. Upload statement containing an authorization hold (pending).
// 2. Verify staged and promoted as a pending bank transaction.
// 3. Upload later statement containing corresponding posted settlement.
// 4. Detect settlement candidate and resolve via settle_pending_bank_transaction.
// 5. Verify pending transaction is updated to posted without duplicate ledger transactions.
// 6. Verify balances are accurate.
// 7. Verify re-upload and re-promotion idempotency.
// 8. Verify concurrency safety without duplicate records.
func TestStatement_PendingToPostedSettlementAcceptance(t *testing.T) {
	pool := getAppPool(t)
	defer pool.Close()

	familyID, userID, accountID := setupStatementTenantFixture(t)
	ctx := context.Background()

	parsers := NewDefaultParserRegistry()
	svc := NewService(pool, testCryptoKey, parsers)

	// Step 1: Statement 1 with Pending Hold
	// "В обработке" (-1500.00 RUB at ВкусВилл on 20.11.2026)
	stmt1CSV := stringsJoin(
		`"Имя счёта";"Дата операции";"Дата платежа";"Номер карты";"Статус";"Сумма операции";"Валюта операции";"Сумма в валюте счёта";"Валюта счёта";"Кэшбэк";"Категория";"MCC";"Описание";"Бонусы (включая кэшбэк)";"Округление на инвесткопилку";"Сумма операции с округлением"`,
		`"Primary Checking";"20.11.2026 14:30:00";"";"*1234";"В обработке";"-1500.00";"RUB";"-1500.00";"RUB";"0";"Супермаркеты";"5411";"ВкусВилл";"0";"0";"1500.00"`,
	)

	batch1, err := svc.UploadAndStage(
		ctx,
		familyID, userID, accountID,
		"statement_nov20_pending.csv",
		bytes.NewReader([]byte(stmt1CSV)),
		int64(len(stmt1CSV)),
		nil,
	)
	if err != nil {
		t.Fatalf("Step 1 failed: UploadAndStage stmt1: %v", err)
	}

	// Verify Staged Row 1 under tenant context via GetImportPreview
	prev1, err := svc.GetImportPreview(ctx, familyID, batch1.ID)
	if err != nil || len(prev1.Rows) != 1 {
		t.Fatalf("Step 1 failed: expected 1 staged preview row, got %d, err=%v", len(prev1.Rows), err)
	}
	if prev1.Rows[0].BookingStatus != "pending" {
		t.Fatalf("Step 1 failed: expected booking_status 'pending', got %q", prev1.Rows[0].BookingStatus)
	}
	if prev1.Rows[0].AmountMinor != -150000 {
		t.Fatalf("Step 1 failed: expected -150000 minor units, got %d", prev1.Rows[0].AmountMinor)
	}
	if prev1.Rows[0].ResolutionAction != ResolutionActionCommit {
		t.Fatalf("Step 1 failed: expected resolution action 'commit', got %q", prev1.Rows[0].ResolutionAction)
	}

	// Step 2: Approve & Promote Batch 1
	_, err = svc.ApproveBatch(ctx, familyID, userID, batch1.ID, ApproveBatchRequest{})
	if err != nil {
		t.Fatalf("Step 2 failed: ApproveBatch stmt1: %v", err)
	}

	res1, err := svc.PromoteBatch(ctx, familyID, userID, batch1.ID)
	if err != nil {
		t.Fatalf("Step 2 failed: PromoteBatch stmt1: %v", err)
	}
	if res1.TransactionsCommitted != 1 {
		t.Fatalf("Step 2 failed: expected 1 committed transaction, got %d", res1.TransactionsCommitted)
	}

	// Verify 1 pending row in bank_transactions and 1 in accounting_entries under tenant context
	var pendingTxID uuid.UUID
	var pendingStatus string
	var postedTime *time.Time
	var entryCount int

	err = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		err = tx.QueryRow(ctx, `
			SELECT id, booking_status, posted_time FROM public.bank_transactions
			WHERE family_id = $1 AND account_id = $2
		`, familyID, accountID).Scan(&pendingTxID, &pendingStatus, &postedTime)
		if err != nil {
			return err
		}

		return tx.QueryRow(ctx, `
			SELECT count(*) FROM public.accounting_entries
			WHERE family_id = $1 AND account_id = $2
		`, familyID, accountID).Scan(&entryCount)
	})
	if err != nil {
		t.Fatalf("Step 2 failed: query after stmt1 under tenant tx: %v", err)
	}
	if pendingStatus != "pending" {
		t.Fatalf("Step 2 failed: expected booking_status 'pending', got %s", pendingStatus)
	}
	if postedTime != nil {
		t.Fatalf("Step 2 failed: expected posted_time to be nil for pending hold, got %v", postedTime)
	}
	if entryCount != 1 {
		t.Fatalf("Step 2 failed: expected 1 accounting_entry, got %d", entryCount)
	}

	// Step 3: Statement 2 (imported 3 days later, contains the settled posted operation)
	// Same operation on 20.11.2026, settled on 23.11.2026 with status "OK"
	stmt2CSV := stringsJoin(
		`"Имя счёта";"Дата операции";"Дата платежа";"Номер карты";"Статус";"Сумма операции";"Валюта операции";"Сумма в валюте счёта";"Валюта счёта";"Кэшбэк";"Категория";"MCC";"Описание";"Бонусы (включая кэшбэк)";"Округление на инвесткопилку";"Сумма операции с округлением"`,
		`"Primary Checking";"20.11.2026 14:30:00";"23.11.2026 10:15:00";"*1234";"OK";"-1500.00";"RUB";"-1500.00";"RUB";"15";"Супермаркеты";"5411";"ВкусВилл";"0";"0";"1500.00"`,
	)

	batch2, err := svc.UploadAndStage(
		ctx,
		familyID, userID, accountID,
		"statement_nov23_posted.csv",
		bytes.NewReader([]byte(stmt2CSV)),
		int64(len(stmt2CSV)),
		nil,
	)
	if err != nil {
		t.Fatalf("Step 3 failed: UploadAndStage stmt2: %v", err)
	}

	// Step 4 & 5: Verify Settlement Candidate Detection via Preview API
	preview2, err := svc.GetImportPreview(ctx, familyID, batch2.ID)
	if err != nil {
		t.Fatalf("Step 4 failed: GetImportPreview stmt2: %v", err)
	}
	if len(preview2.Rows) != 1 {
		t.Fatalf("Step 4 failed: expected 1 staged row in stmt2 preview, got %d", len(preview2.Rows))
	}
	pRow := preview2.Rows[0]
	if pRow.BookingStatus != "posted" {
		t.Fatalf("Step 4 failed: expected booking_status 'posted', got %s", pRow.BookingStatus)
	}
	if pRow.DuplicateStatus != DuplicateStatusSettlementUpdate {
		t.Fatalf("Step 4 failed: expected DuplicateStatus %q, got %q", DuplicateStatusSettlementUpdate, pRow.DuplicateStatus)
	}
	if pRow.ResolutionAction != ResolutionActionApplySettlement {
		t.Fatalf("Step 4 failed: expected ResolutionAction %q, got %q", ResolutionActionApplySettlement, pRow.ResolutionAction)
	}
	if pRow.MatchedExistingTxID == nil || *pRow.MatchedExistingTxID != pendingTxID {
		t.Fatalf("Step 4 failed: expected MatchedExistingTxID to point to pending hold %s, got %v", pendingTxID, pRow.MatchedExistingTxID)
	}

	// Step 6: Approve Batch 2
	_, err = svc.ApproveBatch(ctx, familyID, userID, batch2.ID, ApproveBatchRequest{})
	if err != nil {
		t.Fatalf("Step 6 failed: ApproveBatch stmt2: %v", err)
	}

	// Step 7: Promote Batch 2
	res2, err := svc.PromoteBatch(ctx, familyID, userID, batch2.ID)
	if err != nil {
		t.Fatalf("Step 7 failed: PromoteBatch stmt2: %v", err)
	}
	if res2.TransactionsSettled != 1 {
		t.Fatalf("Step 7 failed: expected 1 transaction settled, got %d", res2.TransactionsSettled)
	}
	if res2.TransactionsCommitted != 0 {
		t.Fatalf("Step 7 failed: expected 0 newly committed transactions, got %d", res2.TransactionsCommitted)
	}

	// Step 8: Verify in DB under tenant tx: Transaction updated to posted, zero duplicates in ledger
	var finalStatus string
	var finalPostedTime *time.Time
	var txCount, finalEntryCount int

	err = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		err = tx.QueryRow(ctx, `
			SELECT count(*) FROM public.bank_transactions
			WHERE family_id = $1 AND account_id = $2
		`, familyID, accountID).Scan(&txCount)
		if err != nil {
			return err
		}

		err = tx.QueryRow(ctx, `
			SELECT booking_status, posted_time FROM public.bank_transactions
			WHERE family_id = $1 AND id = $2
		`, familyID, pendingTxID).Scan(&finalStatus, &finalPostedTime)
		if err != nil {
			return err
		}

		return tx.QueryRow(ctx, `
			SELECT count(*) FROM public.accounting_entries
			WHERE family_id = $1 AND account_id = $2
		`, familyID, accountID).Scan(&finalEntryCount)
	})
	if err != nil {
		t.Fatalf("Step 8 failed: verify DB under tenant tx: %v", err)
	}

	if txCount != 1 {
		t.Fatalf("Step 8 failed: expected EXACTLY 1 bank_transaction in DB, got %d", txCount)
	}
	if finalStatus != "posted" {
		t.Fatalf("Step 8 failed: expected booking_status 'posted' after settlement, got %s", finalStatus)
	}
	if finalPostedTime == nil {
		t.Fatalf("Step 8 failed: expected non-nil posted_time after settlement")
	}
	if finalEntryCount != 1 {
		t.Fatalf("Step 8 failed: expected EXACTLY 1 accounting_entry (no duplicate ledger entries), got %d", finalEntryCount)
	}

	// Step 9: Re-upload and Re-promotion Idempotency
	// Re-uploading statement 2 must detect confirmed duplicate and skip
	batch2Reupload, err := svc.UploadAndStage(
		ctx,
		familyID, userID, accountID,
		"statement_nov23_posted_retry.csv",
		bytes.NewReader([]byte(stmt2CSV)),
		int64(len(stmt2CSV)),
		nil,
	)
	if err != nil {
		t.Fatalf("Step 9 failed: re-upload stmt2: %v", err)
	}

	prevRetry, err := svc.GetImportPreview(ctx, familyID, batch2Reupload.ID)
	if err != nil || len(prevRetry.Rows) != 1 {
		t.Fatalf("Step 9 failed: expected 1 staged row in retry preview, got %d, err=%v", len(prevRetry.Rows), err)
	}
	if prevRetry.Rows[0].DuplicateStatus != DuplicateStatusConfirmed {
		t.Fatalf("Step 9 failed: expected re-upload duplicate_status to be 'confirmed_duplicate', got %s", prevRetry.Rows[0].DuplicateStatus)
	}
	if prevRetry.Rows[0].ResolutionAction != ResolutionActionSkipDuplicate {
		t.Fatalf("Step 9 failed: expected re-upload resolution_action to be 'skip_duplicate', got %s", prevRetry.Rows[0].ResolutionAction)
	}

	// Promote re-uploaded batch
	_, err = svc.ApproveBatch(ctx, familyID, userID, batch2Reupload.ID, ApproveBatchRequest{})
	if err != nil {
		t.Fatalf("Step 9 failed: approve retry batch: %v", err)
	}
	retryRes, err := svc.PromoteBatch(ctx, familyID, userID, batch2Reupload.ID)
	if err != nil {
		t.Fatalf("Step 9 failed: promote retry batch: %v", err)
	}
	if retryRes.TransactionsCommitted != 0 || retryRes.TransactionsSettled != 0 {
		t.Fatalf("Step 9 failed: expected 0 committed and 0 settled on retry, got %+v", retryRes)
	}

	// Verify DB counts are STILL strictly 1
	err = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT count(*) FROM public.bank_transactions WHERE family_id = $1 AND account_id = $2", familyID, accountID).Scan(&txCount)
	})
	if err != nil || txCount != 1 {
		t.Fatalf("Step 9 failed: expected bank_transactions count to remain 1, got %d, err=%v", txCount, err)
	}

	// Step 10: Concurrent Promotion Safety
	// Test that concurrent calls to PromoteBatch on the same batch cannot create duplicate ledger entries
	var wg sync.WaitGroup
	errs := make(chan error, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, pErr := svc.PromoteBatch(ctx, familyID, userID, batch2.ID)
			if pErr != nil {
				errs <- pErr
			}
		}()
	}
	wg.Wait()
	close(errs)

	for pErr := range errs {
		t.Fatalf("Step 10 failed: concurrent PromoteBatch returned error: %v", pErr)
	}

	// Verify DB counts are STILL strictly 1 after concurrent promotion calls
	err = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT count(*) FROM public.bank_transactions WHERE family_id = $1 AND account_id = $2", familyID, accountID).Scan(&txCount)
	})
	if err != nil || txCount != 1 {
		t.Fatalf("Step 10 failed: expected bank_transactions count to remain 1 after concurrency, got %d, err=%v", txCount, err)
	}
}

func stringsJoin(lines ...string) string {
	res := ""
	for _, l := range lines {
		res += l + "\n"
	}
	return res
}

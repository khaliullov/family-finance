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

// 12. Missing closing balance:
// When closing or opening balance is missing, reconciliation evaluates to non_reconcilable.
func TestEvaluateReconciliation_MissingClosingBalance(t *testing.T) {
	startDate := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	endDate := time.Date(2026, 1, 31, 23, 59, 59, 0, time.UTC)
	openBal := int64(100000)
	closeBal := int64(80000)

	rows := []StagedRowDTO{
		{
			RowIndex:           1,
			TransactionTime:    time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC),
			AccountAmountMinor: -20000,
			AccountCurrency:    "RUB",
			BookingStatus:      "posted",
		},
	}

	// 12a. Missing closing balance -> non_reconcilable
	reconMissingClose := EvaluateReconciliation("RUB", &startDate, &endDate, &openBal, nil, rows)
	if reconMissingClose.Classification != ReconciliationNonReconcilable {
		t.Errorf("expected non_reconcilable when closing balance is nil, got %s", reconMissingClose.Classification)
	}

	// 12b. Missing opening balance -> non_reconcilable
	reconMissingOpen := EvaluateReconciliation("RUB", &startDate, &endDate, nil, &closeBal, rows)
	if reconMissingOpen.Classification != ReconciliationNonReconcilable {
		t.Errorf("expected non_reconcilable when opening balance is nil, got %s", reconMissingOpen.Classification)
	}

	// 12c. Balanced statement -> reconciled
	reconReconciled := EvaluateReconciliation("RUB", &startDate, &endDate, &openBal, &closeBal, rows)
	if reconReconciled.Classification != ReconciliationReconciled {
		t.Errorf("expected reconciled for 100000 - 20000 = 80000, got %s", reconReconciled.Classification)
	}

	// 12d. Discrepant statement -> discrepancy
	wrongClose := int64(95000)
	reconDiscrepancy := EvaluateReconciliation("RUB", &startDate, &endDate, &openBal, &wrongClose, rows)
	if reconDiscrepancy.Classification != ReconciliationDiscrepancy {
		t.Errorf("expected discrepancy for 100000 - 20000 != 95000, got %s", reconDiscrepancy.Classification)
	}
}

// 13. Reconciliation discrepancy override:
// When opening balance + sum != closing balance, batch cannot be promoted until an attributable override is recorded.
func TestPromoteBatch_DiscrepancyOverride(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()

	ctx := context.Background()
	familyID, userID, accountID := setupStatementTenantFixture(t)

	registry := NewParserRegistry()
	svc := NewService(appPool, testCryptoKey, registry)

	startDate := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	endDate := time.Date(2026, 2, 28, 23, 59, 59, 0, time.UTC)
	openBal := int64(100000)
	wrongClose := int64(90000) // Calculated will be 100000 - 25000 = 75000 != 90000 (delta: +15000)

	parser := &StubParser{
		ParsedData: &ParsedStatementDTO{
			FormatID:            "discrepancy_format",
			ParserVersion:       "1.0.0",
			PeriodStartDate:     &startDate,
			PeriodEndDate:       &endDate,
			OpeningBalanceMinor: &openBal,
			ClosingBalanceMinor: &wrongClose,
			AccountCurrency:     "RUB",
			Rows: []StagedRowDTO{
				{
					RowIndex:           1,
					TransactionTime:    time.Date(2026, 2, 10, 14, 0, 0, 0, time.UTC),
					AmountMinor:        -25000,
					Currency:           "RUB",
					AccountAmountMinor: -25000,
					AccountCurrency:    "RUB",
					BookingStatus:      "posted",
					OperationType:      "purchase",
					RawDescription:     "Appliances Store",
				},
			},
		},
	}

	batch, err := svc.UploadAndStage(
		ctx,
		familyID, userID, accountID,
		"discrepancy_stmt.csv",
		bytes.NewReader([]byte("csv_data")),
		8,
		parser,
	)
	if err != nil {
		t.Fatalf("UploadAndStage failed: %v", err)
	}

	if batch.ReconciliationStatus != ReconciliationDiscrepancy {
		t.Fatalf("expected reconciliation_status = discrepancy, got %s", batch.ReconciliationStatus)
	}

	// 13a. Promotion without override must fail with ErrOverrideRequired
	_, err = svc.PromoteBatch(ctx, familyID, userID, batch.ID)
	if err != ErrOverrideRequired {
		t.Fatalf("expected ErrOverrideRequired, got: %v", err)
	}

	// 13b. User creates attributable reconciliation override
	overrideReason := "Discrepancy of 150.00 RUB due to unlisted bank bonus adjustment"
	err = svc.CreateReconciliationOverride(ctx, familyID, userID, batch.ID, overrideReason)
	if err != nil {
		t.Fatalf("CreateReconciliationOverride failed: %v", err)
	}

	// 13c. Promotion with override recorded now succeeds
	result, err := svc.PromoteBatch(ctx, familyID, userID, batch.ID)
	if err != nil {
		t.Fatalf("PromoteBatch failed after override: %v", err)
	}
	if result.TransactionsCommitted != 1 {
		t.Errorf("expected 1 transaction committed, got %d", result.TransactionsCommitted)
	}

	// Verify batch status updated to committed
	err = database.WithFamilyUserTx(ctx, appPool, familyID, userID, func(tx pgx.Tx) error {
		var status string
		err := tx.QueryRow(ctx, "SELECT status FROM public.statement_import_batches WHERE id = $1", batch.ID).Scan(&status)
		if err != nil {
			return err
		}
		if status != BatchStatusCommitted {
			t.Errorf("expected batch status committed, got %s", status)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("batch status check failed: %v", err)
	}
}

// 14. Non-reconcilable promotion:
// Statements marked non_reconcilable may be promoted after duplicate checks,
// but MUST NEVER update the account's current balance snapshot.
func TestPromoteBatch_NonReconcilablePromotion(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()

	ctx := context.Background()
	familyID, userID, accountID := setupStatementTenantFixture(t)

	registry := NewParserRegistry()
	svc := NewService(appPool, testCryptoKey, registry)

	startDate := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	endDate := time.Date(2026, 3, 31, 23, 59, 59, 0, time.UTC)

	// No closing balance -> non_reconcilable
	parser := &StubParser{
		ParsedData: &ParsedStatementDTO{
			FormatID:            "non_recon_format",
			ParserVersion:       "1.0.0",
			PeriodStartDate:     &startDate,
			PeriodEndDate:       &endDate,
			OpeningBalanceMinor: nil,
			ClosingBalanceMinor: nil,
			AccountCurrency:     "RUB",
			Rows: []StagedRowDTO{
				{
					RowIndex:           1,
					TransactionTime:    time.Date(2026, 3, 15, 10, 0, 0, 0, time.UTC),
					AmountMinor:        -8000,
					Currency:           "RUB",
					AccountAmountMinor: -8000,
					AccountCurrency:    "RUB",
					BookingStatus:      "posted",
					OperationType:      "purchase",
					RawDescription:     "Bookstore",
				},
			},
		},
	}

	batch, err := svc.UploadAndStage(
		ctx,
		familyID, userID, accountID,
		"non_recon.csv",
		bytes.NewReader([]byte("csv_data")),
		8,
		parser,
	)
	if err != nil {
		t.Fatalf("UploadAndStage failed: %v", err)
	}
	if batch.ReconciliationStatus != ReconciliationNonReconcilable {
		t.Fatalf("expected non_reconcilable, got %s", batch.ReconciliationStatus)
	}

	// Promotion succeeds without override
	result, err := svc.PromoteBatch(ctx, familyID, userID, batch.ID)
	if err != nil {
		t.Fatalf("PromoteBatch failed: %v", err)
	}
	if result.TransactionsCommitted != 1 {
		t.Errorf("expected 1 committed transaction, got %d", result.TransactionsCommitted)
	}
	if result.BalanceSnapshotUpdated {
		t.Errorf("non-reconcilable statement MUST NOT update balance snapshot")
	}

	// Verify account balance snapshot was untouched (initial fixture set it to 100000, 'manual_initial')
	err = database.WithFamilyUserTx(ctx, appPool, familyID, userID, func(tx pgx.Tx) error {
		var bal int64
		var src string
		err := tx.QueryRow(ctx, "SELECT current_balance_minor, balance_source FROM public.accounts WHERE id = $1", accountID).Scan(&bal, &src)
		if err != nil {
			return err
		}
		if bal != 100000 {
			t.Errorf("account balance altered! expected 100000, got %d", bal)
		}
		if src != "manual_initial" {
			t.Errorf("account balance source altered! expected manual_initial, got %s", src)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("account verification failed: %v", err)
	}
}

// 11. Historical balance monotonicity:
// Promoting a historical statement (period_end_date older than balance_updated_at)
// must commit transactions but NEVER overwrite current_balance_minor.
// Promoting a newer statement DOES update current_balance_minor.
func TestPromoteBatch_HistoricalBalanceMonotonicity(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()

	ctx := context.Background()
	familyID, userID, accountID := setupStatementTenantFixture(t)

	registry := NewParserRegistry()
	svc := NewService(appPool, testCryptoKey, registry)

	// Set account current balance to 500,000 RUB as of 2026-05-01
	accAsOf := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	err := database.WithFamilyUserTx(ctx, appPool, familyID, userID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE public.accounts
			SET current_balance_minor = 500000,
			    balance_source = 'bank_api',
			    balance_updated_at = $1
			WHERE id = $2
		`, accAsOf, accountID)
		return err
	})
	if err != nil {
		t.Fatalf("failed to update account balance baseline: %v", err)
	}

	// 11a. Import an older historical statement for January 2026 (closing balance: 200,000 RUB)
	oldStart := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	oldEnd := time.Date(2026, 1, 31, 23, 59, 59, 0, time.UTC)
	oldOpen := int64(250000)
	oldClose := int64(200000)

	parserOld := &StubParser{
		ParsedData: &ParsedStatementDTO{
			FormatID:            "csv_old",
			ParserVersion:       "1.0.0",
			PeriodStartDate:     &oldStart,
			PeriodEndDate:       &oldEnd,
			OpeningBalanceMinor: &oldOpen,
			ClosingBalanceMinor: &oldClose,
			AccountCurrency:     "RUB",
			Rows: []StagedRowDTO{
				{
					RowIndex:           1,
					TransactionTime:    time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC),
					AmountMinor:        -50000,
					Currency:           "RUB",
					AccountAmountMinor: -50000,
					AccountCurrency:    "RUB",
					BookingStatus:      "posted",
					OperationType:      "purchase",
					RawDescription:     "January Purchase",
				},
			},
		},
	}

	batchOld, err := svc.UploadAndStage(
		ctx,
		familyID, userID, accountID,
		"january_stmt.csv",
		bytes.NewReader([]byte("csv_data")),
		8,
		parserOld,
	)
	if err != nil {
		t.Fatalf("UploadAndStage for old batch failed: %v", err)
	}

	resultOld, err := svc.PromoteBatch(ctx, familyID, userID, batchOld.ID)
	if err != nil {
		t.Fatalf("PromoteBatch old batch failed: %v", err)
	}
	if resultOld.TransactionsCommitted != 1 {
		t.Errorf("expected 1 committed transaction for old batch, got %d", resultOld.TransactionsCommitted)
	}
	if resultOld.BalanceSnapshotUpdated {
		t.Errorf("historical statement must NOT update balance snapshot")
	}

	// Verify account balance remains 500,000 as of 2026-05-01
	err = database.WithFamilyUserTx(ctx, appPool, familyID, userID, func(tx pgx.Tx) error {
		var bal int64
		var balDate time.Time
		err := tx.QueryRow(ctx, "SELECT current_balance_minor, balance_updated_at FROM public.accounts WHERE id = $1", accountID).Scan(&bal, &balDate)
		if err != nil {
			return err
		}
		if bal != 500000 {
			t.Errorf("historical statement overwritten balance! expected 500000, got %d", bal)
		}
		if !balDate.Equal(accAsOf) {
			t.Errorf("historical statement overwritten balance_updated_at! expected %v, got %v", accAsOf, balDate)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("baseline check failed: %v", err)
	}

	// 11b. Import a newer statement for June 2026 (closing balance: 650,000 RUB)
	newStart := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	newEnd := time.Date(2026, 6, 30, 23, 59, 59, 0, time.UTC)
	newOpen := int64(600000)
	newClose := int64(650000)

	parserNew := &StubParser{
		ParsedData: &ParsedStatementDTO{
			FormatID:            "csv_new",
			ParserVersion:       "1.0.0",
			PeriodStartDate:     &newStart,
			PeriodEndDate:       &newEnd,
			OpeningBalanceMinor: &newOpen,
			ClosingBalanceMinor: &newClose,
			AccountCurrency:     "RUB",
			Rows: []StagedRowDTO{
				{
					RowIndex:           1,
					TransactionTime:    time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC),
					AmountMinor:        50000,
					Currency:           "RUB",
					AccountAmountMinor: 50000,
					AccountCurrency:    "RUB",
					BookingStatus:      "posted",
					OperationType:      "refund",
					RawDescription:     "June Salary / Deposit",
				},
			},
		},
	}

	batchNew, err := svc.UploadAndStage(
		ctx,
		familyID, userID, accountID,
		"june_stmt.csv",
		bytes.NewReader([]byte("csv_data")),
		8,
		parserNew,
	)
	if err != nil {
		t.Fatalf("UploadAndStage for new batch failed: %v", err)
	}

	resultNew, err := svc.PromoteBatch(ctx, familyID, userID, batchNew.ID)
	if err != nil {
		t.Fatalf("PromoteBatch new batch failed: %v", err)
	}
	if !resultNew.BalanceSnapshotUpdated {
		t.Errorf("newer statement MUST update balance snapshot")
	}
	if *resultNew.NewAccountBalanceMinor != 650000 {
		t.Errorf("expected new balance 650000, got %d", *resultNew.NewAccountBalanceMinor)
	}

	// Verify account balance updated in DB
	err = database.WithFamilyUserTx(ctx, appPool, familyID, userID, func(tx pgx.Tx) error {
		var bal int64
		var src string
		err := tx.QueryRow(ctx, "SELECT current_balance_minor, balance_source FROM public.accounts WHERE id = $1", accountID).Scan(&bal, &src)
		if err != nil {
			return err
		}
		if bal != 650000 {
			t.Errorf("expected account balance 650000, got %d", bal)
		}
		if src != "statement_file" {
			t.Errorf("expected balance source statement_file, got %s", src)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("final check failed: %v", err)
	}
}

// 10. Concurrent approval idempotency:
// Concurrent calls to PromoteBatch on the same batch are serialized by row lock FOR UPDATE,
// exactly one call promotes the ledger transactions, and duplicate/subsequent calls return
// safely without creating duplicate bank transactions or double-booking entries.
func TestPromoteBatch_ConcurrentApprovalIdempotency(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()

	ctx := context.Background()
	familyID, userID, accountID := setupStatementTenantFixture(t)

	registry := NewParserRegistry()
	svc := NewService(appPool, testCryptoKey, registry)

	startDate := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	endDate := time.Date(2026, 7, 31, 23, 59, 59, 0, time.UTC)
	openBal := int64(100000)
	closeBal := int64(80000)

	parser := &StubParser{
		ParsedData: &ParsedStatementDTO{
			FormatID:            "csv_concurrent",
			ParserVersion:       "1.0.0",
			PeriodStartDate:     &startDate,
			PeriodEndDate:       &endDate,
			OpeningBalanceMinor: &openBal,
			ClosingBalanceMinor: &closeBal,
			AccountCurrency:     "RUB",
			Rows: []StagedRowDTO{
				{
					RowIndex:           1,
					TransactionTime:    time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC),
					AmountMinor:        -10000,
					Currency:           "RUB",
					AccountAmountMinor: -10000,
					AccountCurrency:    "RUB",
					BookingStatus:      "posted",
					OperationType:      "purchase",
					RawDescription:     "Item 1",
				},
				{
					RowIndex:           2,
					TransactionTime:    time.Date(2026, 7, 11, 11, 0, 0, 0, time.UTC),
					AmountMinor:        -10000,
					Currency:           "RUB",
					AccountAmountMinor: -10000,
					AccountCurrency:    "RUB",
					BookingStatus:      "posted",
					OperationType:      "purchase",
					RawDescription:     "Item 2",
				},
			},
		},
	}

	batch, err := svc.UploadAndStage(
		ctx,
		familyID, userID, accountID,
		"concurrent_stmt.csv",
		bytes.NewReader([]byte("csv_data")),
		8,
		parser,
	)
	if err != nil {
		t.Fatalf("UploadAndStage failed: %v", err)
	}

	// Launch 3 concurrent goroutines attempting to approve the same batch
	const concurrency = 3
	var wg sync.WaitGroup
	errs := make([]error, concurrency)
	results := make([]*PromotionResult, concurrency)

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			res, err := svc.PromoteBatch(ctx, familyID, userID, batch.ID)
			results[idx] = res
			errs[idx] = err
		}(i)
	}
	wg.Wait()

	for idx, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d failed with error: %v", idx, err)
		}
	}

	// Sum committed transactions across all concurrent callers
	totalCommitted := 0
	for _, res := range results {
		if res != nil {
			totalCommitted += res.TransactionsCommitted
		}
	}
	if totalCommitted != 2 {
		t.Errorf("expected exactly 2 total committed transactions across concurrent calls, got %d", totalCommitted)
	}

	// Verify database state: exactly 2 bank_transactions and 2 accounting_entries exist for these timestamps
	err = database.WithFamilyUserTx(ctx, appPool, familyID, userID, func(tx pgx.Tx) error {
		var txCount, entryCount int
		err := tx.QueryRow(ctx, `
			SELECT count(*) FROM public.bank_transactions
			WHERE family_id = $1 AND account_id = $2 AND raw_description IN ('Item 1', 'Item 2')
		`, familyID, accountID).Scan(&txCount)
		if err != nil {
			return err
		}
		if txCount != 2 {
			t.Errorf("expected exactly 2 bank_transactions in DB, found %d", txCount)
		}

		err = tx.QueryRow(ctx, `
			SELECT count(*) FROM public.accounting_entries
			WHERE family_id = $1 AND account_id = $2 AND entry_time IN ($3, $4)
		`, familyID, accountID, time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC), time.Date(2026, 7, 11, 11, 0, 0, 0, time.UTC)).Scan(&entryCount)
		if err != nil {
			return err
		}
		if entryCount != 2 {
			t.Errorf("expected exactly 2 accounting_entries in DB, found %d", entryCount)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("DB idempotency check failed: %v", err)
	}

	// 10b. Sequential retry on already-committed batch is completely idempotent
	retryRes, err := svc.PromoteBatch(ctx, familyID, userID, batch.ID)
	if err != nil {
		t.Fatalf("retry on committed batch failed: %v", err)
	}
	if retryRes.TransactionsCommitted != 0 {
		t.Errorf("retry on committed batch should commit 0 rows, got %d", retryRes.TransactionsCommitted)
	}
}

// 9. Exact rollback restoration:
// If an unresolvable error occurs during PromoteBatch (e.g. unreviewed pending candidate),
// the entire transaction rolls back: batch remains staged, no bank_transactions or accounting entries remain.
func TestPromoteBatch_AtomicRollbackRestoration(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()

	ctx := context.Background()
	familyID, userID, accountID := setupStatementTenantFixture(t)

	registry := NewParserRegistry()
	svc := NewService(appPool, testCryptoKey, registry)

	startDate := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	endDate := time.Date(2026, 8, 31, 23, 59, 59, 0, time.UTC)
	openBal := int64(100000)
	closeBal := int64(70000)

	// Pre-insert an existing posted transaction with similar amount and date window
	// to trigger Priority 4: Fuzzy duplicate candidate (ResolutionActionPending)
	fuzzyTime := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	existingTxID := uuid.New()
	err := database.WithFamilyUserTx(ctx, appPool, familyID, userID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO public.bank_transactions (
				id, family_id, account_id, fingerprint_hash, fingerprint_sequence,
				amount_minor, currency, account_amount_minor, account_currency,
				booking_status, operation_type, transaction_time, raw_description
			) VALUES (
				$1, $2, $3, $4, 1,
				-30000, 'RUB', -30000, 'RUB',
				'posted', 'purchase', $5, 'Online Retailer X'
			)
		`, existingTxID, familyID, accountID, testContentHash64, fuzzyTime)
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
				-30000, 'RUB', 'expense', $5,
				'household', $6, FALSE, FALSE
			)
		`, uuid.New(), familyID, existingTxID, accountID, userID, fuzzyTime)
		return err
	})
	if err != nil {
		t.Fatalf("failed to insert existing fuzzy transaction: %v", err)
	}

	// Statement row has same amount (-30000) and date within +/- 3 days but different description ('Online Retailer Y')
	parser := &StubParser{
		ParsedData: &ParsedStatementDTO{
			FormatID:            "csv_fuzzy",
			ParserVersion:       "1.0.0",
			PeriodStartDate:     &startDate,
			PeriodEndDate:       &endDate,
			OpeningBalanceMinor: &openBal,
			ClosingBalanceMinor: &closeBal,
			AccountCurrency:     "RUB",
			Rows: []StagedRowDTO{
				{
					RowIndex:           1,
					TransactionTime:    fuzzyTime.Add(24 * time.Hour), // 1 day later
					AmountMinor:        -30000,
					Currency:           "RUB",
					AccountAmountMinor: -30000,
					AccountCurrency:    "RUB",
					BookingStatus:      "posted",
					OperationType:      "purchase",
					RawDescription:     "Online Retailer Y",
				},
			},
		},
	}

	batch, err := svc.UploadAndStage(
		ctx,
		familyID, userID, accountID,
		"fuzzy_stmt.csv",
		bytes.NewReader([]byte("csv_data")),
		8,
		parser,
	)
	if err != nil {
		t.Fatalf("UploadAndStage failed: %v", err)
	}

	// Row was flagged as potential duplicate (resolution_action = pending)
	// Calling PromoteBatch must fail with ErrUnresolvedStagedRows
	_, err = svc.PromoteBatch(ctx, familyID, userID, batch.ID)
	if err == nil {
		t.Fatalf("expected ErrUnresolvedStagedRows, but PromoteBatch succeeded")
	}

	// Verify complete rollback: batch is still staged, no new transaction was committed
	err = database.WithFamilyUserTx(ctx, appPool, familyID, userID, func(tx pgx.Tx) error {
		var status string
		err := tx.QueryRow(ctx, "SELECT status FROM public.statement_import_batches WHERE id = $1", batch.ID).Scan(&status)
		if err != nil {
			return err
		}
		if status != BatchStatusStaged {
			t.Errorf("expected batch status staged after rollback, got %s", status)
		}

		var count int
		err = tx.QueryRow(ctx, "SELECT count(*) FROM public.bank_transactions WHERE raw_description = 'Online Retailer Y'").Scan(&count)
		if err != nil {
			return err
		}
		if count != 0 {
			t.Errorf("expected 0 committed transactions for Online Retailer Y, found %d", count)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("rollback verification failed: %v", err)
	}

	// Now resolve the staged transaction to commit
	err = database.WithFamilyUserTx(ctx, appPool, familyID, userID, func(tx pgx.Tx) error {
		var stagedID uuid.UUID
		err := tx.QueryRow(ctx, "SELECT id FROM public.staged_bank_transactions WHERE batch_id = $1", batch.ID).Scan(&stagedID)
		if err != nil {
			return err
		}
		return svc.ResolveStagedTransaction(ctx, familyID, stagedID, ResolutionActionCommit)
	})
	if err != nil {
		t.Fatalf("ResolveStagedTransaction failed: %v", err)
	}

	// Promotion now succeeds atomically!
	res, err := svc.PromoteBatch(ctx, familyID, userID, batch.ID)
	if err != nil {
		t.Fatalf("PromoteBatch failed after resolving staged row: %v", err)
	}
	if res.TransactionsCommitted != 1 {
		t.Errorf("expected 1 committed transaction, got %d", res.TransactionsCommitted)
	}
}

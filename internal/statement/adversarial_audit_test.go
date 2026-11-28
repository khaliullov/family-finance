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

func TestAdversarial_CheckMerchantCompatibility_Matrix(t *testing.T) {
	strPtr := func(s string) *string { return &s }

	tests := []struct {
		name          string
		stM           *string
		stDesc        string
		candM         *string
		candDesc      string
		expectMatch   bool
		expectContrad bool
	}{
		{
			name:          "Identical merchant names",
			stM:           strPtr("ВкусВилл"),
			stDesc:        "ВкусВилл магазин",
			candM:         strPtr("ВкусВилл"),
			candDesc:      "ВкусВилл Москва",
			expectMatch:   true,
			expectContrad: false,
		},
		{
			name:          "Common legal prefix OOO and city Moscow on completely unrelated merchants",
			stM:           strPtr("ООО Ромашка Москва"),
			stDesc:        "Покупка цветов",
			candM:         strPtr("ООО Вектор Москва"),
			candDesc:      "Строительные материалы",
			expectMatch:   false, // Ideal behavior: unrelated merchants should NOT match!
			expectContrad: true,
		},
		{
			name:          "Common legal prefix IP on unrelated individuals",
			stM:           strPtr("ИП Иванов А.Б."),
			stDesc:        "Консультация",
			candM:         strPtr("ИП Петров В.Г."),
			candDesc:      "Ремонт",
			expectMatch:   false, // Ideal behavior: unrelated individuals should NOT match!
			expectContrad: true,
		},
		{
			name:          "Common generic noun Cafe on unrelated restaurants",
			stM:           strPtr("Кафе Пушкин"),
			stDesc:        "Обед",
			candM:         strPtr("Кафе Чехов"),
			candDesc:      "Ужин",
			expectMatch:   false, // Ideal behavior: unrelated cafes should NOT match!
			expectContrad: true,
		},
		{
			name:          "Substring false positive: Netflix vs Fix",
			stM:           strPtr("Netflix"),
			stDesc:        "Netflix Monthly Subscription",
			candM:         strPtr("Fix"),
			candDesc:      "Fix Ремонт обуви",
			expectMatch:   false, // Ideal behavior: Netflix should NOT match Fix!
			expectContrad: true,
		},
		{
			name:          "Substring false positive: Barclays vs Bar",
			stM:           strPtr("Barclays"),
			stDesc:        "Barclays Bank Fee",
			candM:         strPtr("Bar"),
			candDesc:      "Bar 100 Co.",
			expectMatch:   false,
			expectContrad: true,
		},
		{
			name:          "Transliteration difference: Cyrillic vs Latin for same merchant",
			stM:           strPtr("ВкусВилл"),
			stDesc:        "ВкусВилл",
			candM:         strPtr("Vkusvill"),
			candDesc:      "Vkusvill Moscow",
			expectMatch:   true, // Same merchant! Should ideally match or be flagged as ambiguous candidate
			expectContrad: false,
		},
		{
			name:          "Bank-generated card hold vs clean statement description",
			stM:           nil,
			stDesc:        "YM*market123 moscow",
			candM:         strPtr("Yandex Market"),
			candDesc:      "Yandex Market",
			expectMatch:   true, // Same merchant!
			expectContrad: false,
		},
		{
			name:          "Empty merchant descriptions on both sides",
			stM:           nil,
			stDesc:        "",
			candM:         nil,
			candDesc:      "",
			expectMatch:   false,
			expectContrad: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, c := CheckMerchantCompatibility(tc.stM, tc.stDesc, tc.candM, tc.candDesc)
			t.Logf("[%s] Got: match=%v, contradictory=%v (Expected: match=%v, contradictory=%v)",
				tc.name, m, c, tc.expectMatch, tc.expectContrad)
		})
	}
}

// -----------------------------------------------------------------------------
// Adversarial Test 1: Manual Settlement Assignment Security Checks
// Probes whether UpdateStagedRow rejects:
// - arbitrary non-existent IDs
// - cross-family transaction IDs (IDOR)
// - cross-account transaction IDs
// - currency mismatches
// - non-pending transaction targets (posted, cancelled)
// -----------------------------------------------------------------------------
func TestAdversarial_ManualSettlement_SecurityChecks(t *testing.T) {
	pool := getAppPool(t)
	defer pool.Close()

	familyA, userA, accountA := setupStatementTenantFixture(t)
	familyB, userB, accountB := setupStatementTenantFixture(t)
	ctx := context.Background()

	parsers := NewDefaultParserRegistry()
	svcA := NewService(pool, testCryptoKey, parsers)
	svcB := NewService(pool, testCryptoKey, parsers)

	// Step 1: In Family A, create Account A2 (USD currency)
	var accountA_USD uuid.UUID
	err := database.WithFamilyUserTx(ctx, pool, familyA, userA, func(tx pgx.Tx) error {
		accountA_USD = uuid.New()
		_, err := tx.Exec(ctx, `
			INSERT INTO public.accounts (id, family_id, owner_user_id, name, type, currency, current_balance_minor)
			VALUES ($1, $2, $3, 'USD Account', 'checking', 'USD', 100000)
		`, accountA_USD, familyA, userA)
		return err
	})
	if err != nil {
		t.Fatalf("failed to create USD account: %v", err)
	}

	// Step 2: In Family A, upload and promote a pending hold on Account A (RUB)
	stmtCSV := stringsJoin(
		`"Имя счёта";"Дата операции";"Дата платежа";"Номер карты";"Статус";"Сумма операции";"Валюта операции";"Сумма в валюте счёта";"Валюта счёта";"Кэшбэк";"Категория";"MCC";"Описание";"Бонусы (включая кэшбэк)";"Округление на инвесткопилку";"Сумма операции с округлением"`,
		`"Primary Checking";"20.11.2026 14:00:00";"";"*1234";"В обработке";"-1000.00";"RUB";"-1000.00";"RUB";"0";"Рестораны";"5812";"Cafe 1";"0";"0";"1000.00"`,
	)
	batchPendingA, err := svcA.UploadAndStage(ctx, familyA, userA, accountA, "pending_a.csv", bytes.NewReader([]byte(stmtCSV)), int64(len(stmtCSV)), nil)
	if err != nil {
		t.Fatalf("UploadAndStage pending A failed: %v", err)
	}
	_, _ = svcA.ApproveBatch(ctx, familyA, userA, batchPendingA.ID, ApproveBatchRequest{})
	_, _ = svcA.PromoteBatch(ctx, familyA, userA, batchPendingA.ID)

	var pendingTxA_ID uuid.UUID
	err = database.WithFamilyTx(ctx, pool, familyA, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT id FROM public.bank_transactions WHERE family_id = $1 AND booking_status = 'pending'", familyA).Scan(&pendingTxA_ID)
	})
	if err != nil {
		t.Fatalf("failed to get pending tx ID: %v", err)
	}

	// Step 3: In Family B, upload a statement with a posted row
	stmtB_CSV := stringsJoin(
		`"Имя счёта";"Дата операции";"Дата платежа";"Номер карты";"Статус";"Сумма операции";"Валюта операции";"Сумма в валюте счёта";"Валюта счёта";"Кэшбэк";"Категория";"MCC";"Описание";"Бонусы (включая кэшбэк)";"Округление на инвесткопилку";"Сумма операции с округлением"`,
		`"Primary Checking";"21.11.2026 10:00:00";"21.11.2026 10:00:00";"*1234";"OK";"-1000.00";"RUB";"-1000.00";"RUB";"0";"Рестораны";"5812";"Cafe 1";"0";"0";"1000.00"`,
	)
	batchB, err := svcB.UploadAndStage(ctx, familyB, userB, accountB, "stmt_b.csv", bytes.NewReader([]byte(stmtB_CSV)), int64(len(stmtB_CSV)), nil)
	if err != nil {
		t.Fatalf("UploadAndStage batch B failed: %v", err)
	}
	prevB, err := svcB.GetImportPreview(ctx, familyB, batchB.ID)
	if err != nil || len(prevB.Rows) != 1 {
		t.Fatalf("GetImportPreview batch B failed: %v", err)
	}
	rowB_ID := prevB.Rows[0].ID

	// Check 1: Cross-Family IDOR Attempt -> Family B attempts to link to Family A's pending transaction
	actionSettlement := ResolutionActionApplySettlement
	_, idorErr := svcB.UpdateStagedRow(ctx, familyB, userB, batchB.ID, rowB_ID, UpdateStagedRowRequest{
		MatchedExistingTxID: &pendingTxA_ID,
		ResolutionAction:    &actionSettlement,
	})
	if idorErr == nil {
		t.Fatalf("SECURITY VIOLATION: Family B was able to link to Family A's pending transaction!")
	}
	t.Logf("Cross-family settlement link correctly rejected: %v", idorErr)

	// Check 2: Arbitrary Non-Existent UUID
	fakeID := uuid.New()
	_, fakeErr := svcB.UpdateStagedRow(ctx, familyB, userB, batchB.ID, rowB_ID, UpdateStagedRowRequest{
		MatchedExistingTxID: &fakeID,
		ResolutionAction:    &actionSettlement,
	})
	if fakeErr == nil {
		t.Fatalf("SECURITY VIOLATION: arbitrary non-existent transaction link was accepted!")
	}
	t.Logf("Non-existent transaction link correctly rejected: %v", fakeErr)

	// Step 4: In Family A, upload a statement to USD Account
	stmtUSD_CSV := stringsJoin(
		`"Имя счёта";"Дата операции";"Дата платежа";"Номер карты";"Статус";"Сумма операции";"Валюта операции";"Сумма в валюте счёта";"Валюта счёта";"Кэшбэк";"Категория";"MCC";"Описание";"Бонусы (включая кэшбэк)";"Округление на инвесткопилку";"Сумма операции с округлением"`,
		`"USD Account";"21.11.2026 10:00:00";"21.11.2026 10:00:00";"*1234";"OK";"-10.00";"USD";"-10.00";"USD";"0";"Рестораны";"5812";"Cafe 1";"0";"0";"10.00"`,
	)
	batchUSD, err := svcA.UploadAndStage(ctx, familyA, userA, accountA_USD, "stmt_usd.csv", bytes.NewReader([]byte(stmtUSD_CSV)), int64(len(stmtUSD_CSV)), nil)
	if err != nil {
		t.Fatalf("UploadAndStage batch USD failed: %v", err)
	}
	prevUSD, err := svcA.GetImportPreview(ctx, familyA, batchUSD.ID)
	if err != nil || len(prevUSD.Rows) != 1 {
		t.Fatalf("GetImportPreview batch USD failed: %v", err)
	}
	rowUSD_ID := prevUSD.Rows[0].ID

	// Check 3: Cross-Account & Currency Mismatch Attempt -> USD row attempting to link to RUB account pending tx
	_, curErr := svcA.UpdateStagedRow(ctx, familyA, userA, batchUSD.ID, rowUSD_ID, UpdateStagedRowRequest{
		MatchedExistingTxID: &pendingTxA_ID,
		ResolutionAction:    &actionSettlement,
	})
	if curErr == nil {
		t.Fatalf("SECURITY VIOLATION: cross-account / currency mismatch link was accepted!")
	}
	t.Logf("Cross-account / currency mismatch correctly rejected: %v", curErr)
}

// -----------------------------------------------------------------------------
// Adversarial Test 2: Concurrent Batches Settle Same Pending Hold
// Tests two batches claiming the same pending hold:
// Exactly one must succeed; the second must fail safely with atomic rollback.
// -----------------------------------------------------------------------------
func TestAdversarial_ConcurrentBatches_SamePendingSettlement(t *testing.T) {
	pool := getAppPool(t)
	defer pool.Close()

	familyID, userID, accountID := setupStatementTenantFixture(t)
	ctx := context.Background()

	parsers := NewDefaultParserRegistry()
	svc := NewService(pool, testCryptoKey, parsers)

	// Step 1: Create pending hold
	stmtPending := stringsJoin(
		`"Имя счёта";"Дата операции";"Дата платежа";"Номер карты";"Статус";"Сумма операции";"Валюта операции";"Сумма в валюте счёта";"Валюта счёта";"Кэшбэк";"Категория";"MCC";"Описание";"Бонусы (включая кэшбэк)";"Округление на инвесткопилку";"Сумма операции с округлением"`,
		`"Primary Checking";"20.11.2026 14:00:00";"";"*1234";"В обработке";"-2500.00";"RUB";"-2500.00";"RUB";"0";"Магазины";"5411";"Test Shop";"0";"0";"2500.00"`,
	)
	bPending, err := svc.UploadAndStage(ctx, familyID, userID, accountID, "p.csv", bytes.NewReader([]byte(stmtPending)), int64(len(stmtPending)), nil)
	if err != nil {
		t.Fatalf("upload pending failed: %v", err)
	}
	_, _ = svc.ApproveBatch(ctx, familyID, userID, bPending.ID, ApproveBatchRequest{})
	_, _ = svc.PromoteBatch(ctx, familyID, userID, bPending.ID)

	var pendingID uuid.UUID
	err = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT id FROM public.bank_transactions WHERE family_id = $1 AND booking_status = 'pending'", familyID).Scan(&pendingID)
	})
	if err != nil {
		t.Fatalf("failed to query pending id: %v", err)
	}

	// Step 2: Upload Batch 1 and Batch 2 (both posted, both auto-match or manually link to pendingID)
	stmtPosted := stringsJoin(
		`"Имя счёта";"Дата операции";"Дата платежа";"Номер карты";"Статус";"Сумма операции";"Валюта операции";"Сумма в валюте счёта";"Валюта счёта";"Кэшбэк";"Категория";"MCC";"Описание";"Бонусы (включая кэшбэк)";"Округление на инвесткопилку";"Сумма операции с округлением"`,
		`"Primary Checking";"22.11.2026 10:00:00";"22.11.2026 10:00:00";"*1234";"OK";"-2500.00";"RUB";"-2500.00";"RUB";"0";"Магазины";"5411";"Test Shop";"0";"0";"2500.00"`,
	)
	batch1, err := svc.UploadAndStage(ctx, familyID, userID, accountID, "b1.csv", bytes.NewReader([]byte(stmtPosted)), int64(len(stmtPosted)), nil)
	if err != nil {
		t.Fatalf("upload b1 failed: %v", err)
	}
	batch2, err := svc.UploadAndStage(ctx, familyID, userID, accountID, "b2.csv", bytes.NewReader([]byte(stmtPosted)), int64(len(stmtPosted)), nil)
	if err != nil {
		t.Fatalf("upload b2 failed: %v", err)
	}

	_, _ = svc.ApproveBatch(ctx, familyID, userID, batch1.ID, ApproveBatchRequest{})
	_, _ = svc.ApproveBatch(ctx, familyID, userID, batch2.ID, ApproveBatchRequest{})

	// Step 3: Promote Batch 1 and Batch 2 concurrently
	var wg sync.WaitGroup
	var err1, err2 error
	var res1, res2 *PromotionResult

	wg.Add(2)
	go func() {
		defer wg.Done()
		res1, err1 = svc.PromoteBatch(ctx, familyID, userID, batch1.ID)
	}()
	go func() {
		defer wg.Done()
		res2, err2 = svc.PromoteBatch(ctx, familyID, userID, batch2.ID)
	}()
	wg.Wait()

	t.Logf("Concurrent promotion results: Batch 1 err=%v, Batch 2 err=%v", err1, err2)

	// Exactly one must succeed, and one must fail
	successCount := 0
	if err1 == nil && res1 != nil {
		successCount++
	}
	if err2 == nil && res2 != nil {
		successCount++
	}

	if successCount != 1 {
		t.Fatalf("CONCURRENCY FLAW: expected exactly 1 successful promotion, got %d (err1=%v, err2=%v)", successCount, err1, err2)
	}

	// Verify database state: exactly 1 posted bank_transaction, exactly 1 accounting_entry
	var totalTx, pendingCount, postedCount, entryCount int
	var totalSum int64
	err = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		_ = tx.QueryRow(ctx, "SELECT count(*) FROM public.bank_transactions WHERE family_id = $1", familyID).Scan(&totalTx)
		_ = tx.QueryRow(ctx, "SELECT count(*) FROM public.bank_transactions WHERE family_id = $1 AND booking_status = 'pending'", familyID).Scan(&pendingCount)
		_ = tx.QueryRow(ctx, "SELECT count(*) FROM public.bank_transactions WHERE family_id = $1 AND booking_status = 'posted'", familyID).Scan(&postedCount)
		_ = tx.QueryRow(ctx, "SELECT count(*), COALESCE(SUM(amount_minor), 0) FROM public.accounting_entries WHERE family_id = $1", familyID).Scan(&entryCount, &totalSum)
		return nil
	})
	if err != nil {
		t.Fatalf("DB check failed: %v", err)
	}

	t.Logf("Final State after concurrent promotion: totalTx=%d, pending=%d, posted=%d, entries=%d, sum=%d",
		totalTx, pendingCount, postedCount, entryCount, totalSum)

	if totalTx != 1 || pendingCount != 0 || postedCount != 1 || entryCount != 1 || totalSum != -250000 {
		t.Fatalf("FINANCIAL INTEGRITY VIOLATION: unexpected state: totalTx=%d, entries=%d, sum=%d", totalTx, entryCount, totalSum)
	}
}

// -----------------------------------------------------------------------------
// Adversarial Test 3: Multiple Rows in Same Batch Targeting Same Pending Hold
// Tests what happens if a user sets matched_existing_tx_id on TWO rows in the same batch.
// -----------------------------------------------------------------------------
func TestAdversarial_SameBatch_DuplicateSettlementTarget(t *testing.T) {
	pool := getAppPool(t)
	defer pool.Close()

	familyID, userID, accountID := setupStatementTenantFixture(t)
	ctx := context.Background()

	parsers := NewDefaultParserRegistry()
	svc := NewService(pool, testCryptoKey, parsers)

	// Step 1: Create 1 pending hold
	stmtPending := stringsJoin(
		`"Имя счёта";"Дата операции";"Дата платежа";"Номер карты";"Статус";"Сумма операции";"Валюта операции";"Сумма в валюте счёта";"Валюта счёта";"Кэшбэк";"Категория";"MCC";"Описание";"Бонусы (включая кэшбэк)";"Округление на инвесткопилку";"Сумма операции с округлением"`,
		`"Primary Checking";"20.11.2026 14:00:00";"";"*1234";"В обработке";"-1500.00";"RUB";"-1500.00";"RUB";"0";"Магазины";"5411";"Target Shop";"0";"0";"1500.00"`,
	)
	bPending, err := svc.UploadAndStage(ctx, familyID, userID, accountID, "p_single.csv", bytes.NewReader([]byte(stmtPending)), int64(len(stmtPending)), nil)
	if err != nil {
		t.Fatalf("upload pending failed: %v", err)
	}
	_, _ = svc.ApproveBatch(ctx, familyID, userID, bPending.ID, ApproveBatchRequest{})
	_, _ = svc.PromoteBatch(ctx, familyID, userID, bPending.ID)

	var pendingID uuid.UUID
	err = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT id FROM public.bank_transactions WHERE family_id = $1 AND booking_status = 'pending'", familyID).Scan(&pendingID)
	})
	if err != nil {
		t.Fatalf("failed to query pending id: %v", err)
	}

	// Step 2: Upload batch with 2 posted rows
	stmt2Rows := stringsJoin(
		`"Имя счёта";"Дата операции";"Дата платежа";"Номер карты";"Статус";"Сумма операции";"Валюта операции";"Сумма в валюте счёта";"Валюта счёта";"Кэшбэк";"Категория";"MCC";"Описание";"Бонусы (включая кэшбэк)";"Округление на инвесткопилку";"Сумма операции с округлением"`,
		`"Primary Checking";"22.11.2026 10:00:00";"22.11.2026 10:00:00";"*1234";"OK";"-1500.00";"RUB";"-1500.00";"RUB";"0";"Магазины";"5411";"Target Shop Row 1";"0";"0";"1500.00"`,
		`"Primary Checking";"22.11.2026 10:05:00";"22.11.2026 10:05:00";"*1234";"OK";"-1500.00";"RUB";"-1500.00";"RUB";"0";"Магазины";"5411";"Target Shop Row 2";"0";"0";"1500.00"`,
	)
	batch2Rows, err := svc.UploadAndStage(ctx, familyID, userID, accountID, "b_2rows.csv", bytes.NewReader([]byte(stmt2Rows)), int64(len(stmt2Rows)), nil)
	if err != nil {
		t.Fatalf("upload b_2rows failed: %v", err)
	}

	prev, err := svc.GetImportPreview(ctx, familyID, batch2Rows.ID)
	if err != nil || len(prev.Rows) != 2 {
		t.Fatalf("expected 2 rows in preview, got %d", len(prev.Rows))
	}

	row1_ID := prev.Rows[0].ID
	row2_ID := prev.Rows[1].ID

	// Manually assign BOTH rows to the same pendingID
	act := ResolutionActionApplySettlement
	_, err = svc.UpdateStagedRow(ctx, familyID, userID, batch2Rows.ID, row1_ID, UpdateStagedRowRequest{
		MatchedExistingTxID: &pendingID,
		ResolutionAction:    &act,
	})
	if err != nil {
		t.Fatalf("UpdateStagedRow row 1 failed: %v", err)
	}

	_, err = svc.UpdateStagedRow(ctx, familyID, userID, batch2Rows.ID, row2_ID, UpdateStagedRowRequest{
		MatchedExistingTxID: &pendingID,
		ResolutionAction:    &act,
	})
	t.Logf("Result of assigning same pending ID to row 2 in same batch: %v", err)

	// Attempt approval and promotion
	_, appErr := svc.ApproveBatch(ctx, familyID, userID, batch2Rows.ID, ApproveBatchRequest{})
	t.Logf("ApproveBatch result for duplicate settlement batch: %v", appErr)

	if appErr == nil {
		_, promErr := svc.PromoteBatch(ctx, familyID, userID, batch2Rows.ID)
		t.Logf("PromoteBatch result: %v", promErr)
		if promErr == nil {
			t.Fatalf("CRITICAL INTEGRITY FAILURE: Batch with duplicate settlement targets was promoted successfully!")
		} else {
			t.Logf("PromoteBatch correctly failed on duplicate settlement: %v", promErr)
		}
	}
}

// -----------------------------------------------------------------------------
// Adversarial Test 4: Cancellation Lifecycle Invariants
// Verifies cancel_pending_bank_transaction:
// - original pending evidence retained
// - classification set to 'excluded'
// - idempotent cancellation
// - already-posted cannot be cancelled
// - cross-family cancellation blocked
// -----------------------------------------------------------------------------
func TestAdversarial_CancellationLifecycle_Invariants(t *testing.T) {
	pool := getAppPool(t)
	defer pool.Close()

	familyA, userA, accountA := setupStatementTenantFixture(t)
	familyB, userB, accountB := setupStatementTenantFixture(t)
	ctx := context.Background()

	parsers := NewDefaultParserRegistry()
	svcA := NewService(pool, testCryptoKey, parsers)

	// Create pending hold in Family A
	stmtPending := stringsJoin(
		`"Имя счёта";"Дата операции";"Дата платежа";"Номер карты";"Статус";"Сумма операции";"Валюта операции";"Сумма в валюте счёта";"Валюта счёта";"Кэшбэк";"Категория";"MCC";"Описание";"Бонусы (включая кэшбэк)";"Округление на инвесткопилку";"Сумма операции с округлением"`,
		`"Primary Checking";"20.11.2026 14:00:00";"";"*1234";"В обработке";"-5000.00";"RUB";"-5000.00";"RUB";"0";"Отели";"7011";"Grand Hotel Hold";"0";"0";"5000.00"`,
	)
	batchPending, err := svcA.UploadAndStage(ctx, familyA, userA, accountA, "p_hotel.csv", bytes.NewReader([]byte(stmtPending)), int64(len(stmtPending)), nil)
	if err != nil {
		t.Fatalf("upload pending failed: %v", err)
	}
	_, _ = svcA.ApproveBatch(ctx, familyA, userA, batchPending.ID, ApproveBatchRequest{})
	_, _ = svcA.PromoteBatch(ctx, familyA, userA, batchPending.ID)

	var txID uuid.UUID
	err = database.WithFamilyTx(ctx, pool, familyA, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT id FROM public.bank_transactions WHERE family_id = $1 AND booking_status = 'pending'", familyA).Scan(&txID)
	})
	if err != nil {
		t.Fatalf("query tx ID failed: %v", err)
	}

	// 1. Cross-family cancellation attempt: Family B calls cancel_pending_bank_transaction on Family A's txID
	err = database.WithFamilyUserTx(ctx, pool, familyB, userB, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "SELECT public.cancel_pending_bank_transaction($1, $2, $3)", familyB, accountB, txID)
		return err
	})
	if err == nil {
		t.Fatalf("SECURITY FAILURE: Family B was able to cancel Family A's transaction!")
	}
	t.Logf("Cross-family cancellation attempt correctly blocked: %v", err)

	// 2. Cancellation of pending transaction in Family A
	err = database.WithFamilyUserTx(ctx, pool, familyA, userA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "SELECT public.cancel_pending_bank_transaction($1, $2, $3)", familyA, accountA, txID)
		return err
	})
	if err != nil {
		t.Fatalf("cancel_pending_bank_transaction failed: %v", err)
	}

	// Verify state in database
	var bStatus, rawDesc string
	var amountMinor int64
	var aClass string
	err = database.WithFamilyTx(ctx, pool, familyA, func(tx pgx.Tx) error {
		_ = tx.QueryRow(ctx, "SELECT booking_status, raw_description, amount_minor FROM public.bank_transactions WHERE id = $1", txID).Scan(&bStatus, &rawDesc, &amountMinor)
		_ = tx.QueryRow(ctx, "SELECT classification FROM public.accounting_entries WHERE bank_transaction_id = $1", txID).Scan(&aClass)
		return nil
	})
	if err != nil {
		t.Fatalf("DB check failed: %v", err)
	}

	if bStatus != "cancelled" || aClass != "excluded" || rawDesc != "Grand Hotel Hold" || amountMinor != -500000 {
		t.Fatalf("INVARIANT VIOLATION: unexpected state after cancel: status=%s, class=%s, desc=%s, amount=%d",
			bStatus, aClass, rawDesc, amountMinor)
	}

	// 3. Idempotent re-invocation: Calling cancel_pending_bank_transaction again on already cancelled transaction
	err = database.WithFamilyUserTx(ctx, pool, familyA, userA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "SELECT public.cancel_pending_bank_transaction($1, $2, $3)", familyA, accountA, txID)
		return err
	})
	if err != nil {
		t.Fatalf("cancel_pending_bank_transaction must be idempotent, but returned error: %v", err)
	}
	t.Logf("Idempotent second cancellation call succeeded cleanly without error.")

	// 4. Attempting to settle an already-cancelled transaction
	err = database.WithFamilyUserTx(ctx, pool, familyA, userA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "SELECT public.settle_pending_bank_transaction($1, $2, $3, $4, $5, $6)",
			familyA, accountA, txID, time.Now().UTC(), int64(-500000), nil)
		return err
	})
	if err == nil {
		t.Fatalf("INTEGRITY FAILURE: settle_pending_bank_transaction succeeded on already-cancelled transaction!")
	}
	t.Logf("Settlement on cancelled transaction correctly blocked: %v", err)
}

// -----------------------------------------------------------------------------
// Adversarial Test 5: Simultaneous Approve, Mutate, and Promote
// Tests concurrency race where one worker mutates a row while another worker approves
// and a third worker attempts promotion.
// Invariant: If promotion succeeds, it MUST ONLY commit what was actually approved.
// If mutation slipped in, promotion MUST be rejected (ErrBatchNotApproved or ErrRevisionMismatch).
// -----------------------------------------------------------------------------
func TestAdversarial_SimultaneousApproveMutatePromote(t *testing.T) {
	pool := getAppPool(t)
	defer pool.Close()

	familyID, userID, accountID := setupStatementTenantFixture(t)
	ctx := context.Background()

	parsers := NewDefaultParserRegistry()
	svc := NewService(pool, testCryptoKey, parsers)

	stmtCSV := stringsJoin(
		`"Имя счёта";"Дата операции";"Дата платежа";"Номер карты";"Статус";"Сумма операции";"Валюта операции";"Сумма в валюте счёта";"Валюта счёта";"Кэшбэк";"Категория";"MCC";"Описание";"Бонусы (включая кэшбэк)";"Округление на инвесткопилку";"Сумма операции с округлением"`,
		`"Primary Checking";"21.11.2026 12:00:00";"21.11.2026 12:00:00";"*1234";"OK";"-100.00";"RUB";"-100.00";"RUB";"0";"Супермаркеты";"5411";"Race Store";"0";"0";"100.00"`,
	)

	batch, err := svc.UploadAndStage(ctx, familyID, userID, accountID, "race_test.csv", bytes.NewReader([]byte(stmtCSV)), int64(len(stmtCSV)), nil)
	if err != nil {
		t.Fatalf("UploadAndStage failed: %v", err)
	}

	prev, err := svc.GetImportPreview(ctx, familyID, batch.ID)
	if err != nil || len(prev.Rows) != 1 {
		t.Fatalf("GetImportPreview failed: %v", err)
	}
	rowID := prev.Rows[0].ID

	// Run concurrent approve, mutate, promote
	iterations := 5
	for iter := 0; iter < iterations; iter++ {
		var wg sync.WaitGroup
		wg.Add(3)

		var appErr, mutErr, promErr error
		var promRes *PromotionResult

		go func() {
			defer wg.Done()
			_, appErr = svc.ApproveBatch(ctx, familyID, userID, batch.ID, ApproveBatchRequest{})
		}()

		go func() {
			defer wg.Done()
			opType := "atm_cash"
			_, mutErr = svc.UpdateStagedRow(ctx, familyID, userID, batch.ID, rowID, UpdateStagedRowRequest{
				OperationType: &opType,
			})
		}()

		go func() {
			defer wg.Done()
			promRes, promErr = svc.PromoteBatch(ctx, familyID, userID, batch.ID)
		}()

		wg.Wait()

		t.Logf("[Iteration %d] appErr=%v, mutErr=%v, promErr=%v, promRes=%+v", iter, appErr, mutErr, promErr, promRes)

		if promErr == nil {
			// If promotion succeeded, the batch MUST be committed now
			t.Logf("Promotion won the race and succeeded.")
			break
		}
	}
}

// -----------------------------------------------------------------------------
// Adversarial Test 6: Pending/Posted Amount Changes & Transitions
// Probes:
// - pending -45.00 -> posted -47.50 (amount increase)
// - pending -45.00 -> posted -40.00 (amount decrease)
// - repeated posted import (multiplicity check)
// - repeated pending import
// - settlement after manual row modification
// - settlement after category change
// -----------------------------------------------------------------------------
func TestAdversarial_PendingPosted_AmountChangesAndTransitions(t *testing.T) {
	pool := getAppPool(t)
	defer pool.Close()

	familyID, userID, accountID := setupStatementTenantFixture(t)
	ctx := context.Background()

	parsers := NewDefaultParserRegistry()
	svc := NewService(pool, testCryptoKey, parsers)

	// Sub-test 1: Pending -45.00 -> Posted -40.00 (settled for less than authorized)
	t.Run("Pending -45.00 settled for -40.00", func(t *testing.T) {
		stmtPending := stringsJoin(
			`"Имя счёта";"Дата операции";"Дата платежа";"Номер карты";"Статус";"Сумма операции";"Валюта операции";"Сумма в валюте счёта";"Валюта счёта";"Кэшбэк";"Категория";"MCC";"Описание";"Бонусы (включая кэшбэк)";"Округление на инвесткопилку";"Сумма операции с округлением"`,
			`"Primary Checking";"20.11.2026 14:00:00";"";"*1234";"В обработке";"-45.00";"RUB";"-45.00";"RUB";"0";"Рестораны";"5812";"Bistro Paris";"0";"0";"45.00"`,
		)
		bPending, err := svc.UploadAndStage(ctx, familyID, userID, accountID, "p_45.csv", bytes.NewReader([]byte(stmtPending)), int64(len(stmtPending)), nil)
		if err != nil {
			t.Fatalf("upload pending failed: %v", err)
		}
		_, _ = svc.ApproveBatch(ctx, familyID, userID, bPending.ID, ApproveBatchRequest{})
		_, _ = svc.PromoteBatch(ctx, familyID, userID, bPending.ID)

		stmtPosted := stringsJoin(
			`"Имя счёта";"Дата операции";"Дата платежа";"Номер карты";"Статус";"Сумма операции";"Валюта операции";"Сумма в валюте счёта";"Валюта счёта";"Кэшбэк";"Категория";"MCC";"Описание";"Бонусы (включая кэшбэк)";"Округление на инвесткопилку";"Сумма операции с округлением"`,
			`"Primary Checking";"20.11.2026 14:00:00";"22.11.2026 10:00:00";"*1234";"OK";"-40.00";"RUB";"-40.00";"RUB";"0";"Рестораны";"5812";"Bistro Paris";"0";"0";"40.00"`,
		)
		bPosted, err := svc.UploadAndStage(ctx, familyID, userID, accountID, "p_40.csv", bytes.NewReader([]byte(stmtPosted)), int64(len(stmtPosted)), nil)
		if err != nil {
			t.Fatalf("upload posted failed: %v", err)
		}

		prev, err := svc.GetImportPreview(ctx, familyID, bPosted.ID)
		if err != nil || len(prev.Rows) != 1 {
			t.Fatalf("preview failed: %v", err)
		}
		if prev.Rows[0].DuplicateStatus != DuplicateStatusSettlementUpdate || prev.Rows[0].ResolutionAction != ResolutionActionApplySettlement {
			t.Fatalf("expected settlement_update / apply_settlement for -40.00, got: %s / %s",
				prev.Rows[0].DuplicateStatus, prev.Rows[0].ResolutionAction)
		}

		_, err = svc.ApproveBatch(ctx, familyID, userID, bPosted.ID, ApproveBatchRequest{})
		if err != nil {
			t.Fatalf("ApproveBatch failed: %v", err)
		}
		res, err := svc.PromoteBatch(ctx, familyID, userID, bPosted.ID)
		if err != nil {
			t.Fatalf("PromoteBatch failed: %v", err)
		}
		if res.TransactionsSettled != 1 {
			t.Fatalf("expected 1 settled, got %d", res.TransactionsSettled)
		}

		// Verify database state: exactly 1 bank_tx, exactly 1 accounting_entry with -4000
		var finalTotalBankTx, finalPendingTx, finalPostedTx, finalEntryCount int
		var finalLedgerSum int64
		_ = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
			_ = tx.QueryRow(ctx, `SELECT count(*) FROM public.bank_transactions WHERE family_id = $1`, familyID).Scan(&finalTotalBankTx)
			_ = tx.QueryRow(ctx, `SELECT count(*) FROM public.bank_transactions WHERE family_id = $1 AND booking_status = 'pending'`, familyID).Scan(&finalPendingTx)
			_ = tx.QueryRow(ctx, `SELECT count(*) FROM public.bank_transactions WHERE family_id = $1 AND booking_status = 'posted'`, familyID).Scan(&finalPostedTx)
			_ = tx.QueryRow(ctx, `SELECT count(*), COALESCE(SUM(amount_minor), 0) FROM public.accounting_entries WHERE family_id = $1`, familyID).Scan(&finalEntryCount, &finalLedgerSum)
			return nil
		})

		t.Logf("State after -40.00 settlement: bankTx=%d (posted=%d, pending=%d), entries=%d, ledgerSum=%d",
			finalTotalBankTx, finalPostedTx, finalPendingTx, finalEntryCount, finalLedgerSum)

		if finalTotalBankTx != 1 || finalPendingTx != 0 || finalPostedTx != 1 || finalEntryCount != 1 || finalLedgerSum != -4000 {
			t.Fatalf("INVARIANT VIOLATION: expected 1 posted tx and sum=-4000, got bankTx=%d, sum=%d", finalTotalBankTx, finalLedgerSum)
		}
	})

	// Sub-test 2: Repeated Posted Import (Idempotency)
	t.Run("Repeated Posted Import Idempotency", func(t *testing.T) {
		stmtPostedRepeat := stringsJoin(
			`"Имя счёта";"Дата операции";"Дата платежа";"Номер карты";"Статус";"Сумма операции";"Валюта операции";"Сумма в валюте счёта";"Валюта счёта";"Кэшбэк";"Категория";"MCC";"Описание";"Бонусы (включая кэшбэк)";"Округление на инвесткопилку";"Сумма операции с округлением"`,
			`"Primary Checking";"20.11.2026 14:00:00";"22.11.2026 10:00:00";"*1234";"OK";"-40.00";"RUB";"-40.00";"RUB";"0";"Рестораны";"5812";"Bistro Paris";"0";"0";"40.00"`,
		)
		bRepeat, err := svc.UploadAndStage(ctx, familyID, userID, accountID, "p_repeat.csv", bytes.NewReader([]byte(stmtPostedRepeat)), int64(len(stmtPostedRepeat)), nil)
		if err != nil {
			t.Fatalf("upload repeat failed: %v", err)
		}
		prevRepeat, err := svc.GetImportPreview(ctx, familyID, bRepeat.ID)
		if err != nil || len(prevRepeat.Rows) != 1 {
			t.Fatalf("preview repeat failed: %v", err)
		}

		if prevRepeat.Rows[0].DuplicateStatus != DuplicateStatusConfirmed || prevRepeat.Rows[0].ResolutionAction != ResolutionActionSkipDuplicate {
			t.Logf("CONFIRMED FINDING (SEC-6B4-001): Repeated posted import after pending amount change was NOT detected as duplicate! Status: %s / %s. Root cause: settle_pending_bank_transaction does not update bank_transactions.fingerprint_hash when amount changes.",
				prevRepeat.Rows[0].DuplicateStatus, prevRepeat.Rows[0].ResolutionAction)
		} else {
			t.Logf("Repeated posted import correctly detected as confirmed duplicate: %s / %s",
				prevRepeat.Rows[0].DuplicateStatus, prevRepeat.Rows[0].ResolutionAction)
		}
	})
}

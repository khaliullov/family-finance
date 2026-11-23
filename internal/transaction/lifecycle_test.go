package transaction

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/khaliullov/family-finance/internal/connector"
	"github.com/khaliullov/family-finance/internal/connector/mock"
	"github.com/khaliullov/family-finance/internal/database"
	"github.com/khaliullov/family-finance/internal/domain"
)

func TestDomainLifecycle_PendingToPosted(t *testing.T) {
	txID := uuid.New()
	famID := uuid.New()
	accID := uuid.New()
	extID := "ext_1001"
	authID := "auth_1001"

	bt := domain.BankTransaction{
		ID:                  txID,
		FamilyID:            famID,
		AccountID:           accID,
		ExternalID:          &extID,
		ExternalAuthID:      &authID,
		FingerprintHash:     "hash123",
		FingerprintSequence: 1,
		Amount:              domain.MustNewMoney(-100000, "RUB"),
		AccountAmount:       domain.MustNewMoney(-100000, "RUB"),
		BookingStatus:       domain.StatusPending,
		OperationType:       "purchase",
		TransactionTime:     time.Now().Add(-2 * time.Hour),
		RawDescription:      "Coffee purchase",
	}

	// 1. Valid transition with amount adjustment (e.g. tip added upon settlement)
	settledTime := time.Now()
	settledAmount := domain.MustNewMoney(-105000, "RUB")
	newExtID := "ext_settled_1001"

	err := domain.TransitionToPosted(&bt, settledTime, settledAmount, &newExtID)
	if err != nil {
		t.Fatalf("unexpected error on valid settlement: %v", err)
	}

	if bt.BookingStatus != domain.StatusPosted {
		t.Errorf("expected StatusPosted, got %s", bt.BookingStatus)
	}
	if bt.PostedTime == nil || !bt.PostedTime.Equal(settledTime) {
		t.Errorf("expected posted time %v, got %v", settledTime, bt.PostedTime)
	}
	if !bt.AccountAmount.Equal(settledAmount) {
		t.Errorf("expected settled amount %v, got %v", settledAmount, bt.AccountAmount)
	}
	if bt.ExternalID == nil || *bt.ExternalID != newExtID {
		t.Errorf("expected external_id %s, got %v", newExtID, bt.ExternalID)
	}

	// 2. Terminal state is immutable: transition from posted to posted must fail
	err = domain.TransitionToPosted(&bt, settledTime, settledAmount, &newExtID)
	if err == nil {
		t.Fatalf("expected error transitioning already-posted transaction")
	}

	// 3. Transition from posted to cancelled must fail
	err = domain.TransitionToCancelled(&bt)
	if err == nil {
		t.Fatalf("expected error cancelling already-posted transaction")
	}
}

func TestDomainLifecycle_Invariants(t *testing.T) {
	txID := uuid.New()
	famID := uuid.New()
	accID := uuid.New()

	basePending := func() domain.BankTransaction {
		extID := "ext_1"
		return domain.BankTransaction{
			ID:                  txID,
			FamilyID:            famID,
			AccountID:           accID,
			ExternalID:          &extID,
			FingerprintHash:     "hash123",
			FingerprintSequence: 1,
			Amount:              domain.MustNewMoney(-50000, "RUB"),
			AccountAmount:       domain.MustNewMoney(-50000, "RUB"),
			BookingStatus:       domain.StatusPending,
			OperationType:       "purchase",
			TransactionTime:     time.Now().Add(-time.Hour),
			RawDescription:      "Test Purchase",
		}
	}

	// Invariant: PostedTime cannot be zero
	bt := basePending()
	err := domain.TransitionToPosted(&bt, time.Time{}, domain.MustNewMoney(-50000, "RUB"), nil)
	if err == nil {
		t.Errorf("expected error for zero posted_time")
	}

	// Invariant: Non-card_check amount cannot be zero
	bt = basePending()
	err = domain.TransitionToPosted(&bt, time.Now(), domain.MustNewMoney(0, "RUB"), nil)
	if err == nil {
		t.Errorf("expected error for zero settled amount on purchase")
	}

	// Invariant: Debit/credit sign cannot flip (negative debit cannot settle as positive credit)
	bt = basePending()
	err = domain.TransitionToPosted(&bt, time.Now(), domain.MustNewMoney(50000, "RUB"), nil)
	if err == nil {
		t.Errorf("expected error when debit sign flips to credit")
	}

	// Invariant: Currency cannot mutate
	bt = basePending()
	err = domain.TransitionToPosted(&bt, time.Now(), domain.MustNewMoney(-50000, "USD"), nil)
	if err == nil {
		t.Errorf("expected error when settled currency mismatches account currency")
	}

	// Invariant: ExternalID cannot be wiped to nil
	bt = basePending()
	err = domain.TransitionToPosted(&bt, time.Now(), domain.MustNewMoney(-50000, "RUB"), nil)
	if err == nil {
		t.Errorf("expected error when external_id is wiped to nil")
	}

	// Invariant: ExternalID cannot be empty string
	emptyStr := "   "
	bt = basePending()
	err = domain.TransitionToPosted(&bt, time.Now(), domain.MustNewMoney(-50000, "RUB"), &emptyStr)
	if err == nil {
		t.Errorf("expected error when external_id is empty string")
	}
}

func TestDomainLifecycle_PendingToCancelled(t *testing.T) {
	bt := domain.BankTransaction{
		ID:                  uuid.New(),
		FamilyID:            uuid.New(),
		AccountID:           uuid.New(),
		FingerprintHash:     "hash123",
		FingerprintSequence: 1,
		Amount:              domain.MustNewMoney(-50000, "RUB"),
		AccountAmount:       domain.MustNewMoney(-50000, "RUB"),
		BookingStatus:       domain.StatusPending,
		OperationType:       "purchase",
		TransactionTime:     time.Now().Add(-time.Hour),
		RawDescription:      "Cancelled hold",
	}

	err := domain.TransitionToCancelled(&bt)
	if err != nil {
		t.Fatalf("unexpected error on cancellation: %v", err)
	}

	if bt.BookingStatus != domain.StatusCancelled {
		t.Errorf("expected StatusCancelled, got %s", bt.BookingStatus)
	}

	// Cancelled cannot be posted
	err = domain.TransitionToPosted(&bt, time.Now(), domain.MustNewMoney(-50000, "RUB"), nil)
	if err == nil {
		t.Errorf("expected error transitioning cancelled transaction to posted")
	}

	// Cancelled cannot be cancelled again
	err = domain.TransitionToCancelled(&bt)
	if err == nil {
		t.Errorf("expected error cancelling already-cancelled transaction")
	}
}

func TestTransactionLifecycle_FullDatabaseTransitionMatrix(t *testing.T) {
	pool := getWorkerPool(t)
	defer pool.Close()
	ctx := context.Background()

	familyID, _, connID := setupTenantFixture(t, "mock_lifecycle")

	mockConn := mock.NewMockConnector("mock_lifecycle")
	registry := connector.NewRegistry()
	registry.Register(mockConn)

	var currentAccounts []connector.AccountDTO
	var currentTxs []connector.CanonicalBankTransaction

	mockConn.FetchAccountsFunc = func(ctx context.Context, credentials []byte) ([]connector.AccountDTO, error) {
		return currentAccounts, nil
	}
	mockConn.FetchTransactionsFunc = func(ctx context.Context, credentials []byte, params connector.SyncParams) (*connector.SyncResult, error) {
		return &connector.SyncResult{
			Transactions: currentTxs,
			RawPayload:   []byte(`{"status":"ok"}`),
			Progress:     connector.SyncProgress{HasMore: false},
		}, nil
	}

	svc := NewService(pool, registry)

	currentAccounts = []connector.AccountDTO{
		{
			ExternalID: "acc_lifecycle_1",
			Name:       "Lifecycle Account",
			Type:       "checking",
			Currency:   "RUB",
		},
	}

	txTime := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

	// --- 1. Pending -> Pending (idempotent skip) ---
	currentTxs = []connector.CanonicalBankTransaction{
		{
			ExternalID:        "tx_mat_pending",
			AccountExternalID: "acc_lifecycle_1",
			Amount:            connector.CurrencyAmount{MinorUnits: 150000, Currency: "RUB"},
			AccountAmount:     connector.CurrencyAmount{MinorUnits: 150000, Currency: "RUB"},
			BookingStatus:     "pending",
			OperationType:     "purchase",
			TransactionTime:   txTime,
			RawDescription:    "Coffee Hold",
		},
	}
	stats1, err := svc.SyncConnection(ctx, familyID, connID, SyncOptions{})
	if err != nil {
		t.Fatalf("sync 1 failed: %v", err)
	}
	if stats1.TransactionsIngested != 1 {
		t.Errorf("expected 1 ingested, got %d", stats1.TransactionsIngested)
	}

	// Re-sync exact same pending transaction: must be skipped as idempotent
	stats1Repeat, err := svc.SyncConnection(ctx, familyID, connID, SyncOptions{})
	if err != nil {
		t.Fatalf("sync 1 repeat failed: %v", err)
	}
	if stats1Repeat.TransactionsSkipped != 1 || stats1Repeat.TransactionsIngested != 0 {
		t.Errorf("expected pending transaction to be skipped idempotently, got stats: %+v", stats1Repeat)
	}

	// --- 2. Pending -> Posted (valid transition with settlement amount and posted time) ---
	settledTime := txTime.Add(2 * time.Hour)
	currentTxs = []connector.CanonicalBankTransaction{
		{
			ExternalID:        "tx_mat_pending",
			AccountExternalID: "acc_lifecycle_1",
			Amount:            connector.CurrencyAmount{MinorUnits: 155000, Currency: "RUB"}, // Settled with 50 RUB tip
			AccountAmount:     connector.CurrencyAmount{MinorUnits: 155000, Currency: "RUB"},
			BookingStatus:     "posted",
			OperationType:     "purchase",
			TransactionTime:   txTime,
			SettledTime:       &settledTime,
			RawDescription:    "Coffee Hold Settled",
		},
	}
	stats2, err := svc.SyncConnection(ctx, familyID, connID, SyncOptions{})
	if err != nil {
		t.Fatalf("sync 2 failed: %v", err)
	}
	if stats2.TransactionsUpdated != 1 {
		t.Errorf("expected 1 updated for pending->posted, got %d", stats2.TransactionsUpdated)
	}

	// Verify DB state: status is posted, posted_time is set, accounting entry amount synchronized
	var btID uuid.UUID
	err = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		var status string
		var amt int64
		var pTime time.Time
		err := tx.QueryRow(ctx, `
			SELECT id, booking_status, account_amount_minor, posted_time
			FROM public.bank_transactions
			WHERE family_id = $1 AND external_id = 'tx_mat_pending'
		`, familyID).Scan(&btID, &status, &amt, &pTime)
		if err != nil {
			return err
		}
		if status != "posted" {
			t.Errorf("expected booking_status 'posted', got %s", status)
		}
		if amt != -155000 {
			t.Errorf("expected settled amount -155000, got %d", amt)
		}
		if !pTime.Equal(settledTime) {
			t.Errorf("expected posted_time %v, got %v", settledTime, pTime)
		}

		// Verify accounting entry amount matches settled amount (-155000)
		var entryAmt int64
		err = tx.QueryRow(ctx, `
			SELECT amount_minor
			FROM public.accounting_entries
			WHERE family_id = $1 AND bank_transaction_id = $2
		`, familyID, btID).Scan(&entryAmt)
		if err != nil {
			return err
		}
		if entryAmt != -155000 {
			t.Errorf("expected accounting entry amount -155000, got %d", entryAmt)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("database verification failed: %v", err)
	}

	// --- 3. Posted -> Cancelled (rejected: terminal state immutable, discrepancy recorded) ---
	currentTxs = []connector.CanonicalBankTransaction{
		{
			ExternalID:        "tx_mat_pending",
			AccountExternalID: "acc_lifecycle_1",
			Amount:            connector.CurrencyAmount{MinorUnits: 155000, Currency: "RUB"},
			AccountAmount:     connector.CurrencyAmount{MinorUnits: 155000, Currency: "RUB"},
			BookingStatus:     "cancelled", // Attempting to cancel already posted transaction!
			OperationType:     "purchase",
			TransactionTime:   txTime,
			RawDescription:    "Coffee Hold",
		},
	}
	stats3, err := svc.SyncConnection(ctx, familyID, connID, SyncOptions{})
	if err != nil {
		t.Fatalf("sync 3 failed: %v", err)
	}
	if stats3.TransactionsSkipped != 1 || stats3.TransactionsUpdated != 0 {
		t.Errorf("expected terminal posted transaction to reject cancellation (skipped=1), got: %+v", stats3)
	}
	if stats3.DiscrepanciesDetected == 0 {
		t.Errorf("expected discrepancy detected for illegal posted->cancelled transition")
	}

	// --- 4. Pending -> Cancelled (valid transition for second pending hold) ---
	currentTxs = []connector.CanonicalBankTransaction{
		{
			ExternalID:        "tx_mat_cancel_hold",
			AccountExternalID: "acc_lifecycle_1",
			Amount:            connector.CurrencyAmount{MinorUnits: 50000, Currency: "RUB"},
			AccountAmount:     connector.CurrencyAmount{MinorUnits: 50000, Currency: "RUB"},
			BookingStatus:     "pending",
			OperationType:     "purchase",
			TransactionTime:   txTime,
			RawDescription:    "Gas Preauth Hold",
		},
	}
	_, err = svc.SyncConnection(ctx, familyID, connID, SyncOptions{})
	if err != nil {
		t.Fatalf("failed to ingest initial hold: %v", err)
	}

	// Provider voids the authorization hold
	currentTxs[0].BookingStatus = "cancelled"
	stats4, err := svc.SyncConnection(ctx, familyID, connID, SyncOptions{})
	if err != nil {
		t.Fatalf("sync 4 failed: %v", err)
	}
	if stats4.TransactionsUpdated != 1 {
		t.Errorf("expected 1 updated for pending->cancelled, got %d", stats4.TransactionsUpdated)
	}

	// Verify in DB that transaction is cancelled and entry is excluded
	err = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		var status, classification string
		var holdID uuid.UUID
		err := tx.QueryRow(ctx, `
			SELECT id, booking_status FROM public.bank_transactions
			WHERE family_id = $1 AND external_id = 'tx_mat_cancel_hold'
		`, familyID).Scan(&holdID, &status)
		if err != nil {
			return err
		}
		if status != "cancelled" {
			t.Errorf("expected booking_status 'cancelled', got %s", status)
		}

		err = tx.QueryRow(ctx, `
			SELECT classification FROM public.accounting_entries
			WHERE family_id = $1 AND bank_transaction_id = $2
		`, familyID, holdID).Scan(&classification)
		if err != nil {
			return err
		}
		if classification != "excluded" {
			t.Errorf("expected accounting entry classification 'excluded', got %s", classification)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("database check failed: %v", err)
	}

	// --- 5. Cancelled -> Posted (rejected: terminal state immutable) ---
	currentTxs[0].BookingStatus = "posted"
	stats5, err := svc.SyncConnection(ctx, familyID, connID, SyncOptions{})
	if err != nil {
		t.Fatalf("sync 5 failed: %v", err)
	}
	if stats5.TransactionsSkipped != 1 || stats5.TransactionsUpdated != 0 {
		t.Errorf("expected terminal cancelled transaction to reject posting (skipped=1), got: %+v", stats5)
	}
	if stats5.DiscrepanciesDetected == 0 {
		t.Errorf("expected discrepancy detected for illegal cancelled->posted transition")
	}
}

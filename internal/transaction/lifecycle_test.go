package transaction

import (
	"testing"
	"time"

	"github.com/google/uuid"
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

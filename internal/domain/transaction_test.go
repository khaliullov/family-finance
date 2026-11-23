package domain

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestBankTransaction_Lifecycle_Transitions(t *testing.T) {
	familyID := uuid.New()
	accountID := uuid.New()
	txID := uuid.New()

	initialHold := MustNewMoney(-150000, "RUB")   // -1,500.00 RUB authorization hold
	clearedAmount := MustNewMoney(-148000, "RUB") // -1,480.00 RUB settled amount
	now := time.Now()

	bt := BankTransaction{
		ID:                  txID,
		FamilyID:            familyID,
		AccountID:           accountID,
		FingerprintHash:     "hash_123",
		FingerprintSequence: 0,
		Amount:              initialHold,
		AccountAmount:       initialHold,
		BookingStatus:       StatusPending,
		OperationType:       "purchase",
		TransactionTime:     now.Add(-2 * time.Hour),
		RawDescription:      "Supermarket Purchase",
		CreatedAt:           now.Add(-2 * time.Hour),
		UpdatedAt:           now.Add(-2 * time.Hour),
	}

	if err := bt.Validate(); err != nil {
		t.Fatalf("pending tx validation failed: %v", err)
	}

	// 1. Valid pending -> posted settlement transition
	extID := "ext_999"
	settleTime := now
	err := TransitionToPosted(&bt, settleTime, clearedAmount, &extID)
	if err != nil {
		t.Fatalf("TransitionToPosted failed: %v", err)
	}

	if bt.BookingStatus != StatusPosted {
		t.Errorf("expected StatusPosted, got %s", bt.BookingStatus)
	}
	if !bt.AccountAmount.Equal(clearedAmount) {
		t.Errorf("expected settled amount %s, got %s", clearedAmount, bt.AccountAmount)
	}
	if bt.ExternalID == nil || *bt.ExternalID != extID {
		t.Errorf("expected external id %s, got %v", extID, bt.ExternalID)
	}

	// 2. Terminal state immutability: posted -> cancelled must FAIL
	err = TransitionToCancelled(&bt)
	if !errors.Is(err, ErrTerminalStateImmutable) {
		t.Errorf("expected ErrTerminalStateImmutable when cancelling posted tx, got %v", err)
	}

	// 3. Terminal state immutability: posted -> posted mutation must FAIL
	err = TransitionToPosted(&bt, settleTime, MustNewMoney(-200000, "RUB"), &extID)
	if !errors.Is(err, ErrTerminalStateImmutable) {
		t.Errorf("expected ErrTerminalStateImmutable when re-settling posted tx, got %v", err)
	}
}

func TestBankTransaction_SettlementInvariants(t *testing.T) {
	familyID := uuid.New()
	accountID := uuid.New()
	initialHold := MustNewMoney(-5000, "RUB") // debit expense
	now := time.Now()

	newPendingTx := func() BankTransaction {
		return BankTransaction{
			ID:                  uuid.New(),
			FamilyID:            familyID,
			AccountID:           accountID,
			FingerprintHash:     "hash_inv",
			FingerprintSequence: 0,
			Amount:              initialHold,
			AccountAmount:       initialHold,
			BookingStatus:       StatusPending,
			OperationType:       "purchase",
			TransactionTime:     now,
			RawDescription:      "Hold",
		}
	}

	// Invariant 1: Missing posted_time fails
	bt := newPendingTx()
	err := TransitionToPosted(&bt, time.Time{}, MustNewMoney(-5000, "RUB"), nil)
	if !errors.Is(err, ErrSettlementInvariantFail) {
		t.Errorf("expected ErrSettlementInvariantFail on zero posted_time, got %v", err)
	}

	// Invariant 2: Zero amount on regular purchase fails
	bt = newPendingTx()
	err = TransitionToPosted(&bt, now, MustNewMoney(0, "RUB"), nil)
	if !errors.Is(err, ErrSettlementInvariantFail) {
		t.Errorf("expected ErrSettlementInvariantFail on zero settled amount, got %v", err)
	}

	// Invariant 2b: Zero amount on card_check SUCCEEDS
	bt = newPendingTx()
	bt.OperationType = "card_check"
	bt.AccountAmount = MustNewMoney(0, "RUB")
	bt.Amount = MustNewMoney(0, "RUB")
	err = TransitionToPosted(&bt, now, MustNewMoney(0, "RUB"), nil)
	if err != nil {
		t.Errorf("expected card_check with 0 amount to succeed, got %v", err)
	}

	// Invariant 3: Sign reversal fails (negative debit becoming positive credit)
	bt = newPendingTx()
	err = TransitionToPosted(&bt, now, MustNewMoney(5000, "RUB"), nil)
	if !errors.Is(err, ErrSettlementInvariantFail) {
		t.Errorf("expected ErrSettlementInvariantFail on sign flip, got %v", err)
	}

	// Invariant 4: Existing external_id cannot be wiped
	bt = newPendingTx()
	existingExtID := "orig_ext"
	bt.ExternalID = &existingExtID
	err = TransitionToPosted(&bt, now, MustNewMoney(-5000, "RUB"), nil)
	if !errors.Is(err, ErrSettlementInvariantFail) {
		t.Errorf("expected ErrSettlementInvariantFail when wiping external_id, got %v", err)
	}

	// Invariant 5: Blank external_id fails
	bt = newPendingTx()
	blank := "   "
	err = TransitionToPosted(&bt, now, MustNewMoney(-5000, "RUB"), &blank)
	if !errors.Is(err, ErrSettlementInvariantFail) {
		t.Errorf("expected ErrSettlementInvariantFail on blank external_id, got %v", err)
	}
}

func TestLedger_SplitInvariants(t *testing.T) {
	familyID := uuid.New()
	accountID := uuid.New()
	btID := uuid.New()
	now := time.Now()

	total := MustNewMoney(-5000, "RUB") // -50.00 RUB

	// Test 1: Zero entries fails
	err := ValidateSplitInvariants(total, []AccountingEntry{})
	if !errors.Is(err, ErrSplitInvariantViolated) {
		t.Errorf("expected ErrSplitInvariantViolated for 0 entries, got %v", err)
	}

	// Test 2: Single entry with is_split = true fails
	singleBadSplit := AccountingEntry{
		ID:                uuid.New(),
		FamilyID:          familyID,
		BankTransactionID: &btID,
		AccountID:         accountID,
		Amount:            total,
		IsSplit:           true,
	}
	err = ValidateSplitInvariants(total, []AccountingEntry{singleBadSplit})
	if !errors.Is(err, ErrSplitInvariantViolated) {
		t.Errorf("expected ErrSplitInvariantViolated for single entry with is_split=true, got %v", err)
	}

	// Test 3: Single entry with amount mismatch fails
	singleBadAmt := AccountingEntry{
		ID:                uuid.New(),
		FamilyID:          familyID,
		BankTransactionID: &btID,
		AccountID:         accountID,
		Amount:            MustNewMoney(-4000, "RUB"),
		IsSplit:           false,
	}
	err = ValidateSplitInvariants(total, []AccountingEntry{singleBadAmt})
	if !errors.Is(err, ErrSplitInvariantViolated) {
		t.Errorf("expected ErrSplitInvariantViolated for amount mismatch, got %v", err)
	}

	// Test 4: Single valid entry succeeds
	singleValid := AccountingEntry{
		ID:                uuid.New(),
		FamilyID:          familyID,
		BankTransactionID: &btID,
		AccountID:         accountID,
		Amount:            total,
		IsSplit:           false,
	}
	if err := ValidateSplitInvariants(total, []AccountingEntry{singleValid}); err != nil {
		t.Errorf("valid single entry failed: %v", err)
	}

	// Test 5: Multiple split entries with sum equality and all is_split = true succeeds
	split1 := AccountingEntry{
		ID:                uuid.New(),
		FamilyID:          familyID,
		BankTransactionID: &btID,
		AccountID:         accountID,
		Amount:            MustNewMoney(-3000, "RUB"),
		IsSplit:           true,
		EntryTime:         now,
	}
	split2 := AccountingEntry{
		ID:                uuid.New(),
		FamilyID:          familyID,
		BankTransactionID: &btID,
		AccountID:         accountID,
		Amount:            MustNewMoney(-2000, "RUB"),
		IsSplit:           true,
		EntryTime:         now,
	}
	if err := ValidateSplitInvariants(total, []AccountingEntry{split1, split2}); err != nil {
		t.Errorf("valid split entries failed: %v", err)
	}

	// Test 6: Multiple split entries with sum mismatch fails (-3000 + -1500 = -4500 != -5000)
	splitBadSum := split2
	splitBadSum.Amount = MustNewMoney(-1500, "RUB")
	err = ValidateSplitInvariants(total, []AccountingEntry{split1, splitBadSum})
	if !errors.Is(err, ErrSplitInvariantViolated) {
		t.Errorf("expected ErrSplitInvariantViolated for split sum mismatch, got %v", err)
	}

	// Test 7: Multiple split entries where one has is_split = false fails
	splitUnsplitFlag := split2
	splitUnsplitFlag.IsSplit = false
	err = ValidateSplitInvariants(total, []AccountingEntry{split1, splitUnsplitFlag})
	if !errors.Is(err, ErrSplitInvariantViolated) {
		t.Errorf("expected ErrSplitInvariantViolated when split entry has is_split=false, got %v", err)
	}
}

func TestRelationships_RefundsAndReversals(t *testing.T) {
	familyID := uuid.New()
	accountID := uuid.New()
	origID := uuid.New()
	now := time.Now()

	debitAmount := MustNewMoney(-10000, "RUB") // -100.00 RUB purchase

	originalTx := BankTransaction{
		ID:                  origID,
		FamilyID:            familyID,
		AccountID:           accountID,
		FingerprintHash:     "hash_orig",
		FingerprintSequence: 0,
		Amount:              debitAmount,
		AccountAmount:       debitAmount,
		BookingStatus:       StatusPosted,
		OperationType:       "purchase",
		TransactionTime:     now.Add(-24 * time.Hour),
		PostedTime:          &now,
		RawDescription:      "Department Store",
	}

	// 1. Partial Refund (+40.00 RUB)
	refundID := uuid.New()
	refundAmt := MustNewMoney(4000, "RUB")
	refundTx, refundEntry, err := NewRefundTransaction(refundID, originalTx, refundAmt, now, nil, "Store Return")
	if err != nil {
		t.Fatalf("NewRefundTransaction failed: %v", err)
	}

	if refundTx.OperationType != "refund" {
		t.Errorf("expected operation_type 'refund', got %s", refundTx.OperationType)
	}
	if refundTx.RelatedBankTransactionID == nil || *refundTx.RelatedBankTransactionID != origID {
		t.Errorf("expected RelatedBankTransactionID == %s, got %v", origID, refundTx.RelatedBankTransactionID)
	}
	if !refundTx.AccountAmount.Equal(refundAmt) {
		t.Errorf("expected refund amount %s, got %s", refundAmt, refundTx.AccountAmount)
	}
	if refundEntry.Classification != ClassificationIncome {
		t.Errorf("expected income classification, got %s", refundEntry.Classification)
	}

	// Original transaction was NOT mutated
	if originalTx.BookingStatus != StatusPosted || !originalTx.AccountAmount.Equal(debitAmount) {
		t.Errorf("original transaction was mutated by refund creation!")
	}

	// 2. Full Chargeback Reversal (+100.00 RUB)
	reversalID := uuid.New()
	revTx, revEntry, err := NewReversalTransaction(reversalID, originalTx, now, nil, "Fraud Dispute")
	if err != nil {
		t.Fatalf("NewReversalTransaction failed: %v", err)
	}

	if revTx.OperationType != "reversal" {
		t.Errorf("expected operation_type 'reversal', got %s", revTx.OperationType)
	}
	if revTx.RelatedBankTransactionID == nil || *revTx.RelatedBankTransactionID != origID {
		t.Errorf("expected RelatedBankTransactionID == %s, got %v", origID, revTx.RelatedBankTransactionID)
	}
	expectedRevAmt := MustNewMoney(10000, "RUB")
	if !revTx.AccountAmount.Equal(expectedRevAmt) {
		t.Errorf("expected reversal amount %s, got %s", expectedRevAmt, revTx.AccountAmount)
	}
	if revEntry.Classification != ClassificationIncome {
		t.Errorf("expected reversal entry classification income, got %s", revEntry.Classification)
	}

	// 3. Bank Correction (+5.00 RUB clearing adjustment)
	corrID := uuid.New()
	corrAmt := MustNewMoney(500, "RUB")
	corrTx, corrEntry, err := NewCorrectionTransaction(corrID, originalTx, corrAmt, now, nil, "FX Adjustment")
	if err != nil {
		t.Fatalf("NewCorrectionTransaction failed: %v", err)
	}

	if corrTx.OperationType != "correction" {
		t.Errorf("expected operation_type 'correction', got %s", corrTx.OperationType)
	}
	if corrTx.RelatedBankTransactionID == nil || *corrTx.RelatedBankTransactionID != origID {
		t.Errorf("expected RelatedBankTransactionID == %s, got %v", origID, corrTx.RelatedBankTransactionID)
	}
	if !corrTx.AccountAmount.Equal(corrAmt) {
		t.Errorf("expected correction amount %s, got %s", corrAmt, corrTx.AccountAmount)
	}
	if corrEntry.Classification != ClassificationIncome {
		t.Errorf("expected correction entry classification income, got %s", corrEntry.Classification)
	}

	// Negative correction (-10.00 RUB retroactive fee)
	negCorrID := uuid.New()
	negCorrAmt := MustNewMoney(-1000, "RUB")
	negCorrTx, negCorrEntry, err := NewCorrectionTransaction(negCorrID, originalTx, negCorrAmt, now, nil, "Retroactive Fee")
	if err != nil {
		t.Fatalf("NewCorrectionTransaction with negative delta failed: %v", err)
	}
	if !negCorrTx.AccountAmount.Equal(negCorrAmt) {
		t.Errorf("expected negative correction amount %s, got %s", negCorrAmt, negCorrTx.AccountAmount)
	}
	if negCorrEntry.Classification != ClassificationExpense {
		t.Errorf("expected negative correction classification expense, got %s", negCorrEntry.Classification)
	}

	// Zero correction rejected
	_, _, err = NewCorrectionTransaction(uuid.New(), originalTx, MustNewMoney(0, "RUB"), now, nil, "Zero")
	if err == nil {
		t.Errorf("expected error for zero correction delta, got nil")
	}
}

func TestTransactionView_CompositeProjection(t *testing.T) {
	familyID := uuid.New()
	accountID := uuid.New()
	btID := uuid.New()
	payerID := uuid.New()
	now := time.Now()

	amt := MustNewMoney(-5000, "RUB")

	bt := BankTransaction{
		ID:            btID,
		FamilyID:      familyID,
		AccountID:     accountID,
		AccountAmount: amt,
		BookingStatus: StatusPosted,
	}

	entry := AccountingEntry{
		ID:                uuid.New(),
		FamilyID:          familyID,
		BankTransactionID: &btID,
		AccountID:         accountID,
		PayerUserID:       payerID,
		BeneficiaryType:   BeneficiaryHousehold,
		Amount:            amt,
		Classification:    ClassificationExpense,
		EntryTime:         now,
		IsSplit:           false,
	}

	// Bank-backed view
	view, err := NewBankTransactionView(bt, []AccountingEntry{entry})
	if err != nil {
		t.Fatalf("NewBankTransactionView failed: %v", err)
	}

	expectedViewID := "bt:" + btID.String()
	if view.ViewID != expectedViewID {
		t.Errorf("expected view_id %q, got %q", expectedViewID, view.ViewID)
	}
	if len(view.Entries) != 1 {
		t.Errorf("expected 1 entry in view, got %d", len(view.Entries))
	}

	// Manual cash view
	manualEntryID := uuid.New()
	manualEntry := AccountingEntry{
		ID:                manualEntryID,
		FamilyID:          familyID,
		BankTransactionID: nil,
		AccountID:         accountID,
		PayerUserID:       payerID,
		BeneficiaryType:   BeneficiaryHousehold,
		Amount:            amt,
		Classification:    ClassificationExpense,
		EntryTime:         now,
		IsSplit:           false,
	}

	manualView, err := NewManualTransactionView(manualEntry)
	if err != nil {
		t.Fatalf("NewManualTransactionView failed: %v", err)
	}

	expectedManualViewID := "manual:" + manualEntryID.String()
	if manualView.ViewID != expectedManualViewID {
		t.Errorf("expected manual view_id %q, got %q", expectedManualViewID, manualView.ViewID)
	}
	if manualView.BankTransaction != nil {
		t.Errorf("expected nil bank transaction on manual view")
	}
}

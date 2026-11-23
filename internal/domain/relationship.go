package domain

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

var (
	ErrInvalidRelationship = errors.New("invalid transaction relationship")
)

// NewRefundTransaction models a merchant refund (full or partial) as a distinct, immutable bank transaction
// that references the original debit transaction without mutating it.
func NewRefundTransaction(
	id uuid.UUID,
	original BankTransaction,
	refundAmount Money,
	refundTime time.Time,
	externalID *string,
	rawDescription string,
) (BankTransaction, AccountingEntry, error) {
	if original.ID == uuid.Nil {
		return BankTransaction{}, AccountingEntry{}, fmt.Errorf("%w: missing original transaction id", ErrInvalidRelationship)
	}
	if refundAmount.IsNegative() || refundAmount.IsZero() {
		return BankTransaction{}, AccountingEntry{}, fmt.Errorf("%w: refund amount must be positive", ErrInvalidRelationship)
	}
	if refundAmount.Currency() != original.AccountAmount.Currency() {
		return BankTransaction{}, AccountingEntry{}, fmt.Errorf("%w: refund currency %s != original currency %s",
			ErrInvalidRelationship, refundAmount.Currency(), original.AccountAmount.Currency())
	}
	if original.AccountAmount.IsNegative() {
		maxRefund, err := original.AccountAmount.Neg()
		if err != nil {
			return BankTransaction{}, AccountingEntry{}, err
		}
		if refundAmount.AmountMinor() > maxRefund.AmountMinor() {
			return BankTransaction{}, AccountingEntry{}, fmt.Errorf("%w: refund amount %s exceeds original debit %s",
				ErrInvalidRelationship, refundAmount.Format(), original.AccountAmount.Format())
		}
	}

	bt := BankTransaction{
		ID:                       id,
		FamilyID:                 original.FamilyID,
		AccountID:                original.AccountID,
		CardID:                   original.CardID,
		ExternalID:               externalID,
		FingerprintHash:          fmt.Sprintf("refund_%s_%s", original.ID, id),
		Amount:                   refundAmount,
		AccountAmount:            refundAmount,
		BookingStatus:            StatusPosted,
		OperationType:            "refund",
		RelatedBankTransactionID: &original.ID,
		TransactionTime:          refundTime,
		PostedTime:               &refundTime,
		MerchantName:             original.MerchantName,
		RawDescription:           rawDescription,
		MCC:                      original.MCC,
		BankCategory:             original.BankCategory,
		CreatedAt:                time.Now(),
		UpdatedAt:                time.Now(),
	}

	entry := AccountingEntry{
		ID:                uuid.New(),
		FamilyID:          original.FamilyID,
		BankTransactionID: &id,
		AccountID:         original.AccountID,
		PayerUserID:       uuid.Nil, // Set by caller
		BeneficiaryType:   BeneficiaryHousehold,
		Amount:            refundAmount,
		Classification:    ClassificationIncome,
		EntryTime:         refundTime,
		IsSplit:           false,
		CreatedAt:         time.Now(),
		UpdatedAt:         time.Now(),
	}

	return bt, entry, nil
}

// NewReversalTransaction models a chargeback or bank reversal as a distinct, immutable bank transaction
// that offsets an original transaction while preserving history.
func NewReversalTransaction(
	id uuid.UUID,
	original BankTransaction,
	reversalTime time.Time,
	externalID *string,
	reason string,
) (BankTransaction, AccountingEntry, error) {
	if original.ID == uuid.Nil {
		return BankTransaction{}, AccountingEntry{}, fmt.Errorf("%w: missing original transaction id", ErrInvalidRelationship)
	}

	// Reversal reverses the sign of the original amount
	revAmount, err := original.AccountAmount.Neg()
	if err != nil {
		return BankTransaction{}, AccountingEntry{}, err
	}

	bt := BankTransaction{
		ID:                       id,
		FamilyID:                 original.FamilyID,
		AccountID:                original.AccountID,
		CardID:                   original.CardID,
		ExternalID:               externalID,
		FingerprintHash:          fmt.Sprintf("reversal_%s_%s", original.ID, id),
		Amount:                   revAmount,
		AccountAmount:            revAmount,
		BookingStatus:            StatusPosted,
		OperationType:            "reversal",
		RelatedBankTransactionID: &original.ID,
		TransactionTime:          reversalTime,
		PostedTime:               &reversalTime,
		MerchantName:             original.MerchantName,
		RawDescription:           fmt.Sprintf("Reversal for tx %s: %s", original.ID, reason),
		MCC:                      original.MCC,
		BankCategory:             original.BankCategory,
		CreatedAt:                time.Now(),
		UpdatedAt:                time.Now(),
	}

	class := ClassificationIncome
	if revAmount.IsNegative() {
		class = ClassificationExpense
	}

	entry := AccountingEntry{
		ID:                uuid.New(),
		FamilyID:          original.FamilyID,
		BankTransactionID: &id,
		AccountID:         original.AccountID,
		PayerUserID:       uuid.Nil,
		BeneficiaryType:   BeneficiaryHousehold,
		Amount:            revAmount,
		Classification:    class,
		EntryTime:         reversalTime,
		IsSplit:           false,
		CreatedAt:         time.Now(),
		UpdatedAt:         time.Now(),
	}

	return bt, entry, nil
}

// NewCorrectionTransaction models an explicit bank-initiated ledger correction or adjustment
// (e.g. foreign exchange adjustment, retroactive fee correction, clearing discrepancy).
// It creates a distinct immutable bank transaction carrying the delta amount that references
// the original posted transaction via related_bank_transaction_id. The original transaction is never mutated.
func NewCorrectionTransaction(
	id uuid.UUID,
	original BankTransaction,
	deltaAmount Money,
	correctionTime time.Time,
	externalID *string,
	reason string,
) (BankTransaction, AccountingEntry, error) {
	if original.ID == uuid.Nil {
		return BankTransaction{}, AccountingEntry{}, fmt.Errorf("%w: missing original transaction id", ErrInvalidRelationship)
	}
	if deltaAmount.IsZero() {
		return BankTransaction{}, AccountingEntry{}, fmt.Errorf("%w: correction delta cannot be zero", ErrInvalidRelationship)
	}
	if deltaAmount.Currency() != original.AccountAmount.Currency() {
		return BankTransaction{}, AccountingEntry{}, fmt.Errorf("%w: correction currency %s != original currency %s",
			ErrInvalidRelationship, deltaAmount.Currency(), original.AccountAmount.Currency())
	}

	bt := BankTransaction{
		ID:                       id,
		FamilyID:                 original.FamilyID,
		AccountID:                original.AccountID,
		CardID:                   original.CardID,
		ExternalID:               externalID,
		FingerprintHash:          fmt.Sprintf("correction_%s_%s", original.ID, id),
		Amount:                   deltaAmount,
		AccountAmount:            deltaAmount,
		BookingStatus:            StatusPosted,
		OperationType:            "correction",
		RelatedBankTransactionID: &original.ID,
		TransactionTime:          correctionTime,
		PostedTime:               &correctionTime,
		MerchantName:             original.MerchantName,
		RawDescription:           fmt.Sprintf("Correction for tx %s: %s", original.ID, reason),
		MCC:                      original.MCC,
		BankCategory:             original.BankCategory,
		CreatedAt:                time.Now(),
		UpdatedAt:                time.Now(),
	}

	class := ClassificationIncome
	if deltaAmount.IsNegative() {
		class = ClassificationExpense
	}

	entry := AccountingEntry{
		ID:                uuid.New(),
		FamilyID:          original.FamilyID,
		BankTransactionID: &id,
		AccountID:         original.AccountID,
		PayerUserID:       uuid.Nil,
		BeneficiaryType:   BeneficiaryHousehold,
		Amount:            deltaAmount,
		Classification:    class,
		EntryTime:         correctionTime,
		IsSplit:           false,
		CreatedAt:         time.Now(),
		UpdatedAt:         time.Now(),
	}

	return bt, entry, nil
}

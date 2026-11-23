package domain

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

var (
	ErrInvalidAccountingEntry = errors.New("invalid accounting entry")
	ErrSplitInvariantViolated = errors.New("split invariant violated")
)

type Classification string

const (
	ClassificationExpense       Classification = "expense"
	ClassificationIncome        Classification = "income"
	ClassificationTransfer      Classification = "transfer"
	ClassificationDebtRepayment Classification = "debt_repayment"
	ClassificationExcluded      Classification = "excluded"
)

func (c Classification) IsValid() bool {
	switch c {
	case ClassificationExpense, ClassificationIncome, ClassificationTransfer, ClassificationDebtRepayment, ClassificationExcluded:
		return true
	default:
		return false
	}
}

type BeneficiaryType string

const (
	BeneficiaryHousehold BeneficiaryType = "household"
	BeneficiaryPartnerA  BeneficiaryType = "partner_a"
	BeneficiaryPartnerB  BeneficiaryType = "partner_b"
	BeneficiaryChild     BeneficiaryType = "child"
	BeneficiaryOther     BeneficiaryType = "other"
)

func (b BeneficiaryType) IsValid() bool {
	switch b {
	case BeneficiaryHousehold, BeneficiaryPartnerA, BeneficiaryPartnerB, BeneficiaryChild, BeneficiaryOther:
		return true
	default:
		return false
	}
}

// AccountingEntry represents user-controlled accounting interpretation of money movement.
type AccountingEntry struct {
	ID                uuid.UUID
	FamilyID          uuid.UUID
	BankTransactionID *uuid.UUID // nil for manual cash operations
	AccountID         uuid.UUID
	PayerUserID       uuid.UUID
	BeneficiaryType   BeneficiaryType
	Amount            Money
	Classification    Classification
	CategoryID        *uuid.UUID
	TransferMatchID   *uuid.UUID
	UserComment       *string
	EntryTime         time.Time
	IsSplit           bool
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// Validate verifies structural constraints on an AccountingEntry.
func (ae AccountingEntry) Validate() error {
	if ae.ID == uuid.Nil {
		return fmt.Errorf("%w: missing entry id", ErrInvalidAccountingEntry)
	}
	if ae.FamilyID == uuid.Nil {
		return fmt.Errorf("%w: missing family id", ErrInvalidAccountingEntry)
	}
	if ae.AccountID == uuid.Nil {
		return fmt.Errorf("%w: missing account id", ErrInvalidAccountingEntry)
	}
	if ae.PayerUserID == uuid.Nil {
		return fmt.Errorf("%w: missing payer user id", ErrInvalidAccountingEntry)
	}
	if !ae.Classification.IsValid() {
		return fmt.Errorf("%w: invalid classification %q", ErrInvalidAccountingEntry, ae.Classification)
	}
	if !ae.BeneficiaryType.IsValid() {
		return fmt.Errorf("%w: invalid beneficiary type %q", ErrInvalidAccountingEntry, ae.BeneficiaryType)
	}
	if ae.EntryTime.IsZero() {
		return fmt.Errorf("%w: entry time cannot be zero", ErrInvalidAccountingEntry)
	}
	return nil
}

// ValidateSplitInvariants validates the two-layer split integrity rules against a parent bank transaction.
func ValidateSplitInvariants(expectedAccountAmount Money, entries []AccountingEntry) error {
	count := len(entries)
	if count == 0 {
		return fmt.Errorf("%w: bank transaction must have at least 1 accounting entry", ErrSplitInvariantViolated)
	}

	for _, e := range entries {
		if e.Amount.Currency() != expectedAccountAmount.Currency() {
			return fmt.Errorf("%w: entry currency %s != account currency %s",
				ErrSplitInvariantViolated, e.Amount.Currency(), expectedAccountAmount.Currency())
		}
	}

	if count == 1 {
		entry := entries[0]
		if entry.IsSplit {
			return fmt.Errorf("%w: single accounting entry must have is_split = false", ErrSplitInvariantViolated)
		}
		if !entry.Amount.Equal(expectedAccountAmount) {
			return fmt.Errorf("%w: single entry amount %s != bank transaction amount %s",
				ErrSplitInvariantViolated, entry.Amount.Format(), expectedAccountAmount.Format())
		}
		return nil
	}

	// Multiple entries (count >= 2)
	var sum int64
	for i, e := range entries {
		if !e.IsSplit {
			return fmt.Errorf("%w: entry %d in split group has is_split = false", ErrSplitInvariantViolated, i)
		}
		sum += e.Amount.AmountMinor()
	}

	if sum != expectedAccountAmount.AmountMinor() {
		return fmt.Errorf("%w: sum of split entries (%d) != bank transaction amount (%d)",
			ErrSplitInvariantViolated, sum, expectedAccountAmount.AmountMinor())
	}

	return nil
}

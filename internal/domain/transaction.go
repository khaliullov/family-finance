package domain

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

var (
	ErrInvalidTransactionState = errors.New("invalid bank transaction state")
	ErrImmutableFieldMutated   = errors.New("cannot mutate Category 1 immutable bank fact")
)

// BookingStatus represents the physical clearing status of a bank transaction.
type BookingStatus string

const (
	StatusPending   BookingStatus = "pending"
	StatusPosted    BookingStatus = "posted"
	StatusCancelled BookingStatus = "cancelled"
)

func (s BookingStatus) IsValid() bool {
	switch s {
	case StatusPending, StatusPosted, StatusCancelled:
		return true
	default:
		return false
	}
}

// BankTransaction represents immutable physical bank evidence authoritative external reality.
type BankTransaction struct {
	ID                       uuid.UUID
	FamilyID                 uuid.UUID
	AccountID                uuid.UUID
	CardID                   *uuid.UUID
	RawPayloadID             *uuid.UUID
	ExternalID               *string
	ExternalAuthID           *string
	FingerprintHash          string
	FingerprintSequence      int
	Amount                   Money
	AccountAmount            Money
	BookingStatus            BookingStatus
	OperationType            string
	RelatedBankTransactionID *uuid.UUID
	TransactionTime          time.Time
	PostedTime               *time.Time
	MerchantName             *string
	RawDescription           string
	MCC                      *int
	BankCategory             *string
	CreatedAt                time.Time
	UpdatedAt                time.Time
}

// Validate performs structural and financial sanity checks on a BankTransaction.
func (bt BankTransaction) Validate() error {
	if bt.ID == uuid.Nil {
		return fmt.Errorf("%w: missing transaction id", ErrInvalidTransactionState)
	}
	if bt.FamilyID == uuid.Nil {
		return fmt.Errorf("%w: missing family id", ErrInvalidTransactionState)
	}
	if bt.AccountID == uuid.Nil {
		return fmt.Errorf("%w: missing account id", ErrInvalidTransactionState)
	}
	if !bt.BookingStatus.IsValid() {
		return fmt.Errorf("%w: invalid booking status %q", ErrInvalidTransactionState, bt.BookingStatus)
	}
	if bt.FingerprintHash == "" {
		return fmt.Errorf("%w: missing fingerprint hash", ErrInvalidTransactionState)
	}
	if bt.OperationType == "" {
		return fmt.Errorf("%w: missing operation type", ErrInvalidTransactionState)
	}
	if bt.RawDescription == "" {
		return fmt.Errorf("%w: missing raw description", ErrInvalidTransactionState)
	}
	if bt.TransactionTime.IsZero() {
		return fmt.Errorf("%w: transaction time cannot be zero", ErrInvalidTransactionState)
	}

	// Status-specific invariants
	if bt.BookingStatus == StatusPosted {
		if bt.PostedTime == nil || bt.PostedTime.IsZero() {
			return fmt.Errorf("%w: posted transaction must have posted_time", ErrInvalidTransactionState)
		}
		if bt.AccountAmount.IsZero() && bt.OperationType != "card_check" {
			return fmt.Errorf("%w: posted account amount cannot be zero for %s", ErrInvalidTransactionState, bt.OperationType)
		}
	}

	return nil
}

// IsTerminal returns true if transaction has reached a final immutable state.
func (bt BankTransaction) IsTerminal() bool {
	return bt.BookingStatus == StatusPosted || bt.BookingStatus == StatusCancelled
}

// AssertHistoricalFactsUnchanged verifies that Category 1 immutable facts were not altered.
func (bt BankTransaction) AssertHistoricalFactsUnchanged(target BankTransaction) error {
	if bt.ID != target.ID ||
		bt.FamilyID != target.FamilyID ||
		bt.AccountID != target.AccountID ||
		!uuidPtrEqual(bt.CardID, target.CardID) ||
		!uuidPtrEqual(bt.RawPayloadID, target.RawPayloadID) ||
		!strPtrEqual(bt.ExternalAuthID, target.ExternalAuthID) ||
		bt.FingerprintHash != target.FingerprintHash ||
		bt.FingerprintSequence != target.FingerprintSequence ||
		!bt.Amount.Equal(target.Amount) ||
		bt.AccountAmount.Currency() != target.AccountAmount.Currency() ||
		bt.OperationType != target.OperationType ||
		!uuidPtrEqual(bt.RelatedBankTransactionID, target.RelatedBankTransactionID) ||
		!bt.TransactionTime.Equal(target.TransactionTime) ||
		!strPtrEqual(bt.MerchantName, target.MerchantName) ||
		bt.RawDescription != target.RawDescription ||
		!intPtrEqual(bt.MCC, target.MCC) ||
		!strPtrEqual(bt.BankCategory, target.BankCategory) {
		return ErrImmutableFieldMutated
	}
	return nil
}

func uuidPtrEqual(a, b *uuid.UUID) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

func strPtrEqual(a, b *string) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

func intPtrEqual(a, b *int) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

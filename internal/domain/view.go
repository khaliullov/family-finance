package domain

import (
	"errors"
	"fmt"

	"github.com/google/uuid"
)

var (
	ErrInvalidTransactionView = errors.New("invalid transaction view")
)

// TransactionView represents the stable unified composite view of a bank transaction
// and its accounting interpretation entries.
type TransactionView struct {
	ViewID          string            `json:"view_id"`
	BankTransaction *BankTransaction  `json:"bank_transaction,omitempty"`
	Entries         []AccountingEntry `json:"entries"`
}

// NewBankTransactionView creates a TransactionView for a bank-backed operation.
// ViewID is deterministic: "bt:" + BankTransaction.ID.String().
func NewBankTransactionView(bt BankTransaction, entries []AccountingEntry) (TransactionView, error) {
	if bt.ID == uuid.Nil {
		return TransactionView{}, fmt.Errorf("%w: bank transaction must have non-nil id", ErrInvalidTransactionView)
	}

	if err := ValidateSplitInvariants(bt.AccountAmount, entries); err != nil {
		return TransactionView{}, err
	}

	for _, e := range entries {
		if e.BankTransactionID == nil || *e.BankTransactionID != bt.ID {
			return TransactionView{}, fmt.Errorf("%w: entry %s does not reference bank transaction %s",
				ErrInvalidTransactionView, e.ID, bt.ID)
		}
		if e.FamilyID != bt.FamilyID {
			return TransactionView{}, fmt.Errorf("%w: entry family %s != bank tx family %s",
				ErrInvalidTransactionView, e.FamilyID, bt.FamilyID)
		}
	}

	return TransactionView{
		ViewID:          "bt:" + bt.ID.String(),
		BankTransaction: &bt,
		Entries:         entries,
	}, nil
}

// NewManualTransactionView creates a TransactionView for an unbacked manual cash operation.
// ViewID is deterministic: "manual:" + AccountingEntry.ID.String().
func NewManualTransactionView(entry AccountingEntry) (TransactionView, error) {
	if entry.ID == uuid.Nil {
		return TransactionView{}, fmt.Errorf("%w: manual entry must have non-nil id", ErrInvalidTransactionView)
	}
	if entry.BankTransactionID != nil {
		return TransactionView{}, fmt.Errorf("%w: manual entry cannot reference a bank transaction", ErrInvalidTransactionView)
	}
	if entry.IsSplit {
		return TransactionView{}, fmt.Errorf("%w: manual entry root cannot have is_split = true", ErrInvalidTransactionView)
	}

	return TransactionView{
		ViewID:          "manual:" + entry.ID.String(),
		BankTransaction: nil,
		Entries:         []AccountingEntry{entry},
	}, nil
}

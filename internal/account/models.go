package account

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// AccountType represents the functional category of a financial account.
type AccountType string

const (
	TypeChecking   AccountType = "checking"
	TypeCreditCard AccountType = "credit_card"
	TypeSavings    AccountType = "savings"
	TypeDeposit    AccountType = "deposit"
	TypeCash       AccountType = "cash"
)

// IsValid validates whether the account type is one of the supported types.
func (t AccountType) IsValid() bool {
	switch t {
	case TypeChecking, TypeCreditCard, TypeSavings, TypeDeposit, TypeCash:
		return true
	default:
		return false
	}
}

// BalanceSource indicates the origin of the balance snapshot.
type BalanceSource string

const (
	SourceBankAPI       BalanceSource = "bank_api"
	SourceStatementFile BalanceSource = "statement_file"
	SourceCalculated    BalanceSource = "calculated"
	SourceManualInitial BalanceSource = "manual_initial"
)

func (s BalanceSource) IsValid() bool {
	switch s {
	case SourceBankAPI, SourceStatementFile, SourceCalculated, SourceManualInitial:
		return true
	default:
		return false
	}
}

// BalanceState represents the epistemic state of a monetary balance value.
// Distinguishes known (observed or computed), unknown (NULL / unobserved), and unavailable.
type BalanceState string

const (
	BalanceStateKnown       BalanceState = "known"
	BalanceStateUnknown     BalanceState = "unknown"
	BalanceStateUnavailable BalanceState = "unavailable"
)

// Account represents a financial container owned by a family and attributed to a user.
type Account struct {
	ID                    uuid.UUID   `json:"id"`
	FamilyID              uuid.UUID   `json:"family_id"`
	BankConnectionID      *uuid.UUID  `json:"bank_connection_id,omitempty"`
	OwnerUserID           uuid.UUID   `json:"owner_user_id"`
	ExternalID            *string     `json:"external_id,omitempty"`
	Name                  string      `json:"name"`
	Type                  AccountType `json:"type"`
	Currency              string      `json:"currency"`
	CurrentBalanceMinor   *int64      `json:"current_balance_minor,omitempty"`
	AvailableBalanceMinor *int64      `json:"available_balance_minor,omitempty"`
	DebtBalanceMinor      *int64      `json:"debt_balance_minor,omitempty"`
	CreditLimitMinor      *int64      `json:"credit_limit_minor,omitempty"`
	StatementBalanceMinor *int64      `json:"statement_balance_minor,omitempty"`
	MinimalPaymentMinor   *int64      `json:"minimal_payment_minor,omitempty"`
	GracePeriodEndDate    *time.Time  `json:"grace_period_end_date,omitempty"`
	BalanceUpdatedAt      *time.Time  `json:"balance_updated_at,omitempty"`
	BalanceSource         *string     `json:"balance_source,omitempty"`
	IsArchived            bool        `json:"is_archived"`
	CreatedAt             time.Time   `json:"created_at"`
	UpdatedAt             time.Time   `json:"updated_at"`
}

// PaymentSystem represents the card payment network.
type PaymentSystem string

const (
	PaymentSystemMir        PaymentSystem = "MIR"
	PaymentSystemVisa       PaymentSystem = "VISA"
	PaymentSystemMastercard PaymentSystem = "MASTERCARD"
	PaymentSystemOther      PaymentSystem = "OTHER"
)

func (p PaymentSystem) IsValid() bool {
	switch p {
	case PaymentSystemMir, PaymentSystemVisa, PaymentSystemMastercard, PaymentSystemOther:
		return true
	default:
		return false
	}
}

// CardStatus represents the administrative lifecycle status of a payment card.
type CardStatus string

const (
	CardStatusActive  CardStatus = "ACTIVE"
	CardStatusBlocked CardStatus = "BLOCKED"
	CardStatusClosed  CardStatus = "CLOSED"
)

func (s CardStatus) IsValid() bool {
	switch s {
	case CardStatusActive, CardStatusBlocked, CardStatusClosed:
		return true
	default:
		return false
	}
}

// Card represents a payment card linked to an underlying financial account.
type Card struct {
	ID            uuid.UUID     `json:"id"`
	FamilyID      uuid.UUID     `json:"family_id"`
	AccountID     uuid.UUID     `json:"account_id"`
	OwnerUserID   uuid.UUID     `json:"owner_user_id"`
	ExternalID    *string       `json:"external_id,omitempty"`
	MaskedPAN     string        `json:"masked_pan"`
	DisplayName   string        `json:"display_name"`
	PaymentSystem PaymentSystem `json:"payment_system"`
	IsVirtual     bool          `json:"is_virtual"`
	IsPrimary     bool          `json:"is_primary"`
	Status        CardStatus    `json:"status"`
	CreatedAt     time.Time     `json:"created_at"`
	UpdatedAt     time.Time     `json:"updated_at"`
}

// maskedPANRegex validates that a card number contains masks and only exposes safe digits.
// Rejects unmasked full PANs (13-19 consecutive digits).
var (
	maskedPANRegex = regexp.MustCompile(`^(\*{1,6}\d{4}|\d{4,6}\*{4,12}\d{4}|\*{4}\s*\d{4})$`)
	plainPANRegex  = regexp.MustCompile(`^\d{13,19}$`)
)

// ValidateMaskedPAN ensures that the card PAN is properly masked and never stores raw card credentials.
func ValidateMaskedPAN(pan string) error {
	trimmed := strings.TrimSpace(pan)
	if trimmed == "" {
		return errors.New("masked PAN cannot be empty")
	}
	if len(trimmed) > 20 {
		return errors.New("masked PAN exceeds maximum length of 20 characters")
	}
	if plainPANRegex.MatchString(trimmed) {
		return errors.New("full unmasked PAN is strictly prohibited; must be masked (e.g. *1234)")
	}
	if !maskedPANRegex.MatchString(trimmed) && !strings.Contains(trimmed, "*") {
		return errors.New("invalid masked PAN format; must contain mask character '*' and last 4 digits")
	}
	return nil
}

// PendingHold represents an uncleared authorization hold from bank_transactions.
type PendingHold struct {
	ID                  uuid.UUID  `json:"id"`
	TransactionTime     time.Time  `json:"transaction_time"`
	AmountMinor         int64      `json:"amount_minor"` // negative for debits (in account_currency)
	HoldMinor           int64      `json:"hold_minor"`   // positive magnitude of the hold (in account_currency)
	Currency            string     `json:"currency"`     // account_currency corresponding to AmountMinor/HoldMinor
	OriginalAmountMinor *int64     `json:"original_amount_minor,omitempty"`
	OriginalCurrency    *string    `json:"original_currency,omitempty"`
	Description         string     `json:"description"`
	CardID              *uuid.UUID `json:"card_id,omitempty"`
}

// BalanceInfo provides full epistemic transparency over current, available, and hold balances.
type BalanceInfo struct {
	AccountID   uuid.UUID   `json:"account_id"`
	AccountType AccountType `json:"account_type"`
	Currency    string      `json:"currency"`

	// Current settled balance
	CurrentBalanceMinor *int64       `json:"current_balance_minor,omitempty"`
	CurrentBalanceState BalanceState `json:"current_balance_state"`

	// Available spending balance
	AvailableBalanceMinor *int64       `json:"available_balance_minor,omitempty"`
	AvailableBalanceState BalanceState `json:"available_balance_state"`

	// Credit-specific fields
	CreditLimitMinor *int64       `json:"credit_limit_minor,omitempty"`
	CreditLimitState BalanceState `json:"credit_limit_state"`

	DebtBalanceMinor *int64       `json:"debt_balance_minor,omitempty"`
	DebtBalanceState BalanceState `json:"debt_balance_state"`

	// Pending holds computed directly from bank_transactions
	PendingHoldsMinor int64 `json:"pending_holds_minor"`
	PendingHoldsCount int   `json:"pending_holds_count"`

	// Deterministically calculated available amount based on formula
	CalculatedAvailableMinor *int64       `json:"calculated_available_minor,omitempty"`
	CalculatedAvailableState BalanceState `json:"calculated_available_state"`

	BalanceUpdatedAt *time.Time `json:"balance_updated_at,omitempty"`
	BalanceSource    *string    `json:"balance_source,omitempty"`
}

// UpdateAccountParams defines editable properties of an account.
type UpdateAccountParams struct {
	Name *string `json:"name,omitempty"`
}

// UpdateCardParams defines editable properties of a card.
type UpdateCardParams struct {
	DisplayName *string     `json:"display_name,omitempty"`
	Status      *CardStatus `json:"status,omitempty"`
	IsPrimary   *bool       `json:"is_primary,omitempty"`
}

// BalanceSnapshotParams defines input for recording an observed balance snapshot.
type BalanceSnapshotParams struct {
	CurrentBalanceMinor   *int64
	AvailableBalanceMinor *int64
	DebtBalanceMinor      *int64
	CreditLimitMinor      *int64
	StatementBalanceMinor *int64
	MinimalPaymentMinor   *int64
	GracePeriodEndDate    *time.Time
	BalanceSource         BalanceSource
	ObservedAt            time.Time
}

// Validate validates the snapshot parameters.
func (p BalanceSnapshotParams) Validate() error {
	if !p.BalanceSource.IsValid() {
		return fmt.Errorf("invalid balance source: %q", p.BalanceSource)
	}
	if p.ObservedAt.IsZero() {
		return errors.New("observation timestamp cannot be zero")
	}
	return nil
}

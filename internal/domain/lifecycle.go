package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrTerminalStateImmutable  = errors.New("terminal bank transaction state cannot transition or mutate")
	ErrInvalidStateTransition  = errors.New("invalid bank transaction lifecycle transition")
	ErrSettlementInvariantFail = errors.New("settlement invariant violation")
)

// TransitionToPosted transitions a pending hold to posted settlement.
// Enforces:
// 1. Current status must be pending.
// 2. postedTime cannot be zero.
// 3. settledAmount cannot be zero (except for card_check).
// 4. Debit/credit sign cannot flip upon settlement.
// 5. Existing externalID cannot be wiped to nil or blank string.
// 6. Category 1 historical facts cannot be altered.
func TransitionToPosted(bt *BankTransaction, postedTime time.Time, settledAmount Money, externalID *string) error {
	if bt.BookingStatus == StatusPosted || bt.BookingStatus == StatusCancelled {
		return fmt.Errorf("%w: transaction %s is in terminal status %s",
			ErrTerminalStateImmutable, bt.ID, bt.BookingStatus)
	}

	if bt.BookingStatus != StatusPending {
		return fmt.Errorf("%w: cannot transition from %s to posted",
			ErrInvalidStateTransition, bt.BookingStatus)
	}

	if postedTime.IsZero() {
		return fmt.Errorf("%w: posted_time is mandatory upon settlement", ErrSettlementInvariantFail)
	}

	if settledAmount.IsZero() && bt.OperationType != "card_check" {
		return fmt.Errorf("%w: account_amount_minor cannot be zero for %s",
			ErrSettlementInvariantFail, bt.OperationType)
	}

	if settledAmount.Currency() != bt.AccountAmount.Currency() {
		return fmt.Errorf("%w: settled currency %s != account currency %s",
			ErrSettlementInvariantFail, settledAmount.Currency(), bt.AccountAmount.Currency())
	}

	// Sign preservation check: debit cannot become credit; credit cannot become debit
	oldAmt := bt.AccountAmount.AmountMinor()
	newAmt := settledAmount.AmountMinor()
	if (oldAmt < 0 && newAmt > 0) || (oldAmt > 0 && newAmt < 0) {
		return fmt.Errorf("%w: debit/credit sign change prohibited (%d -> %d)",
			ErrSettlementInvariantFail, oldAmt, newAmt)
	}

	// External ID validation: cannot wipe existing external_id
	if bt.ExternalID != nil && externalID == nil {
		return fmt.Errorf("%w: external_id cannot be wiped to nil", ErrSettlementInvariantFail)
	}
	if externalID != nil && strings.TrimSpace(*externalID) == "" {
		return fmt.Errorf("%w: external_id cannot be empty string", ErrSettlementInvariantFail)
	}

	// Apply settlement transition
	bt.BookingStatus = StatusPosted
	bt.PostedTime = &postedTime
	bt.AccountAmount = settledAmount
	if externalID != nil {
		bt.ExternalID = externalID
	}
	bt.UpdatedAt = time.Now()

	return nil
}

// TransitionToCancelled marks a pending authorization hold as voided/cancelled.
// Enforces that settlement fields remain unchanged.
func TransitionToCancelled(bt *BankTransaction) error {
	if bt.BookingStatus == StatusPosted || bt.BookingStatus == StatusCancelled {
		return fmt.Errorf("%w: transaction %s is in terminal status %s",
			ErrTerminalStateImmutable, bt.ID, bt.BookingStatus)
	}

	if bt.BookingStatus != StatusPending {
		return fmt.Errorf("%w: cannot transition from %s to cancelled",
			ErrInvalidStateTransition, bt.BookingStatus)
	}

	bt.BookingStatus = StatusCancelled
	bt.UpdatedAt = time.Now()
	return nil
}

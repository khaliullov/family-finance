package transaction

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/khaliullov/family-finance/internal/domain"
)

// TransitionPendingToPosted transitions a pending authorization hold to posted settlement.
// Atomically synchronizes bank_transactions with the single unsplit accounting_entry to satisfy
// the deferred split integrity trigger trg_bank_tx_split_integrity_deferred.
func TransitionPendingToPosted(
	ctx context.Context,
	tx pgx.Tx,
	familyID uuid.UUID,
	existingBT domain.BankTransaction,
	settledTime time.Time,
	settledAccountAmount domain.Money,
	externalID *string,
) error {
	// 1. Enforce domain transition invariants
	btCopy := existingBT
	if err := domain.TransitionToPosted(&btCopy, settledTime, settledAccountAmount, externalID); err != nil {
		return fmt.Errorf("domain lifecycle validation failed: %w", err)
	}

	// 2. Update bank_transactions
	res, err := tx.Exec(ctx, `
		UPDATE public.bank_transactions
		SET booking_status = 'posted',
		    posted_time = $1,
		    account_amount_minor = $2,
		    external_id = COALESCE($3, external_id),
		    updated_at = NOW()
		WHERE family_id = $4 AND id = $5 AND booking_status = 'pending'
	`, btCopy.PostedTime, btCopy.AccountAmount.AmountMinor(), btCopy.ExternalID, familyID, btCopy.ID)
	if err != nil {
		return fmt.Errorf("failed to update bank transaction to posted: %w", err)
	}
	if res.RowsAffected() == 0 {
		return fmt.Errorf("transaction %s already in terminal state or not found", btCopy.ID)
	}

	// 3. Synchronize unsplit accounting_entries amount to match settled amount
	_, err = tx.Exec(ctx, `
		UPDATE public.accounting_entries
		SET amount_minor = $1,
		    updated_at = NOW()
		WHERE family_id = $2 
		  AND bank_transaction_id = $3 
		  AND is_split = FALSE
	`, btCopy.AccountAmount.AmountMinor(), familyID, btCopy.ID)
	if err != nil {
		return fmt.Errorf("failed to synchronize accounting entry on settlement: %w", err)
	}

	return nil
}

// TransitionPendingToCancelled marks a pending authorization hold as cancelled/voided.
func TransitionPendingToCancelled(
	ctx context.Context,
	tx pgx.Tx,
	familyID uuid.UUID,
	existingBT domain.BankTransaction,
) error {
	btCopy := existingBT
	if err := domain.TransitionToCancelled(&btCopy); err != nil {
		return fmt.Errorf("domain lifecycle validation failed: %w", err)
	}

	res, err := tx.Exec(ctx, `
		UPDATE public.bank_transactions
		SET booking_status = 'cancelled',
		    updated_at = NOW()
		WHERE family_id = $1 AND id = $2 AND booking_status = 'pending'
	`, familyID, btCopy.ID)
	if err != nil {
		return fmt.Errorf("failed to update bank transaction to cancelled: %w", err)
	}
	if res.RowsAffected() == 0 {
		return fmt.Errorf("transaction %s already in terminal state or not found", btCopy.ID)
	}

	// For cancelled holds, set accounting entry classification to 'excluded'
	_, err = tx.Exec(ctx, `
		UPDATE public.accounting_entries
		SET classification = 'excluded',
		    updated_at = NOW()
		WHERE family_id = $1 
		  AND bank_transaction_id = $2
	`, familyID, btCopy.ID)
	if err != nil {
		return fmt.Errorf("failed to update accounting entry for cancelled transaction: %w", err)
	}

	return nil
}

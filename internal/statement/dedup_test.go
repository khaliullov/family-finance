package statement

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/khaliullov/family-finance/internal/database"
)

// 17. Fingerprint canonicalization:
// Verify deterministic canonicalization: whitespace trimming, lowercase status,
// uppercase ISO currency, and UTC timestamps yield identical SHA-256 fingerprint hashes.
func TestComputeImportFingerprint_Canonicalization(t *testing.T) {
	accountID := uuid.New()
	fixedTime := time.Date(2026, 3, 15, 12, 30, 0, 0, time.UTC)
	fixedLocal := fixedTime.In(time.FixedZone("MSK", 3*3600)) // same instant in UTC+3

	fp1 := ComputeImportFingerprint(
		accountID,
		"posted",
		nil,
		fixedTime,
		-15000,
		"RUB",
		"Yandex Go Taxi",
	)

	fp2 := ComputeImportFingerprint(
		accountID,
		"  POSTED  ",
		nil,
		fixedLocal,
		-15000,
		"  rub  ",
		"  YANDEX   GO   TAXI  ",
	)

	if fp1 != fp2 {
		t.Fatalf("expected identical fingerprints for canonicalized inputs (including case and whitespace collapse), got:\nfp1: %s\nfp2: %s", fp1, fp2)
	}

	// Different description must produce different fingerprint
	fpDiffDesc := ComputeImportFingerprint(
		accountID,
		"posted",
		nil,
		fixedTime,
		-15000,
		"RUB",
		"Coffee Shop",
	)
	if fp1 == fpDiffDesc {
		t.Fatalf("expected different fingerprints for different descriptions, but they collided")
	}

	// Different amount must produce different fingerprint
	fpDiffAmount := ComputeImportFingerprint(
		accountID,
		"posted",
		nil,
		fixedTime,
		-15001,
		"RUB",
		"Yandex Go Taxi",
	)
	if fp1 == fpDiffAmount {
		t.Fatalf("expected different fingerprints for different amounts, but they collided")
	}
}

// 15. Identical transaction multiplicity:
// Legitimate identical transactions (e.g. multiple identical transit fares on the same day)
// must be preserved without collision, matching by multiplicity rather than global uniqueness.
func TestDeduplication_IdenticalMultiplicity(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()

	ctx := context.Background()
	familyID, userID, accountID := setupStatementTenantFixture(t)
	batchID := uuid.New()

	t1 := time.Date(2026, 4, 10, 8, 30, 0, 0, time.UTC)

	// Two identical rows in the statement (e.g. two subway rides for 5700 minor units)
	rows := []StagedRowDTO{
		{
			RowIndex:           1,
			TransactionTime:    t1,
			AmountMinor:        -5700,
			Currency:           "RUB",
			AccountAmountMinor: -5700,
			AccountCurrency:    "RUB",
			BookingStatus:      "posted",
			OperationType:      "purchase",
			RawDescription:     "Moscow Metro",
		},
		{
			RowIndex:           2,
			TransactionTime:    t1,
			AmountMinor:        -5700,
			Currency:           "RUB",
			AccountAmountMinor: -5700,
			AccountCurrency:    "RUB",
			BookingStatus:      "posted",
			OperationType:      "purchase",
			RawDescription:     "Moscow Metro",
		},
	}

	fp := ComputeImportFingerprint(accountID, "posted", nil, t1, -5700, "RUB", "Moscow Metro")

	// Case A: 0 existing transactions in DB -> both rows must be marked NEW (resolution: commit)
	err := database.WithFamilyUserTx(ctx, appPool, familyID, userID, func(tx pgx.Tx) error {
		staged, err := MatchStagedRows(ctx, tx, familyID, accountID, batchID, rows)
		if err != nil {
			return err
		}
		if len(staged) != 2 {
			t.Fatalf("expected 2 staged rows, got %d", len(staged))
		}
		if staged[0].DuplicateStatus != DuplicateStatusNew || staged[0].ResolutionAction != ResolutionActionCommit {
			t.Errorf("row 1: expected new/commit, got %s/%s", staged[0].DuplicateStatus, staged[0].ResolutionAction)
		}
		if staged[1].DuplicateStatus != DuplicateStatusNew || staged[1].ResolutionAction != ResolutionActionCommit {
			t.Errorf("row 2: expected new/commit, got %s/%s", staged[1].DuplicateStatus, staged[1].ResolutionAction)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Case A failed: %v", err)
	}

	// Insert 1 existing transaction in DB
	txID1 := uuid.New()
	err = database.WithFamilyUserTx(ctx, appPool, familyID, userID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO public.bank_transactions (
				id, family_id, account_id, fingerprint_hash, fingerprint_sequence,
				amount_minor, currency, account_amount_minor, account_currency,
				booking_status, operation_type, transaction_time, raw_description
			) VALUES (
				$1, $2, $3, $4, 1,
				-5700, 'RUB', -5700, 'RUB',
				'posted', 'purchase', $5, 'Moscow Metro'
			)
		`, txID1, familyID, accountID, fp, t1)
		if err != nil {
			return err
		}

		_, err = tx.Exec(ctx, `
			INSERT INTO public.accounting_entries (
				id, family_id, bank_transaction_id, account_id,
				amount_minor, currency, classification, payer_user_id,
				beneficiary_type, entry_time, is_split, is_manual
			) VALUES (
				$1, $2, $3, $4,
				-5700, 'RUB', 'expense', $5,
				'household', $6, FALSE, FALSE
			)
		`, uuid.New(), familyID, txID1, accountID, userID, t1)
		return err
	})
	if err != nil {
		t.Fatalf("failed to insert 1 existing transaction: %v", err)
	}

	// Case B: 1 existing in DB -> row 1 is confirmed duplicate, row 2 is NEW!
	err = database.WithFamilyUserTx(ctx, appPool, familyID, userID, func(tx pgx.Tx) error {
		staged, err := MatchStagedRows(ctx, tx, familyID, accountID, batchID, rows)
		if err != nil {
			return err
		}
		if staged[0].DuplicateStatus != DuplicateStatusConfirmed || staged[0].ResolutionAction != ResolutionActionSkipDuplicate {
			t.Errorf("row 1: expected confirmed/skip_duplicate, got %s/%s", staged[0].DuplicateStatus, staged[0].ResolutionAction)
		}
		if staged[1].DuplicateStatus != DuplicateStatusNew || staged[1].ResolutionAction != ResolutionActionCommit {
			t.Errorf("row 2: expected new/commit (preserves multiplicity), got %s/%s", staged[1].DuplicateStatus, staged[1].ResolutionAction)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Case B failed: %v", err)
	}

	// Insert 2nd existing transaction in DB
	txID2 := uuid.New()
	err = database.WithFamilyUserTx(ctx, appPool, familyID, userID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO public.bank_transactions (
				id, family_id, account_id, fingerprint_hash, fingerprint_sequence,
				amount_minor, currency, account_amount_minor, account_currency,
				booking_status, operation_type, transaction_time, raw_description
			) VALUES (
				$1, $2, $3, $4, 2,
				-5700, 'RUB', -5700, 'RUB',
				'posted', 'purchase', $5, 'Moscow Metro'
			)
		`, txID2, familyID, accountID, fp, t1)
		if err != nil {
			return err
		}

		_, err = tx.Exec(ctx, `
			INSERT INTO public.accounting_entries (
				id, family_id, bank_transaction_id, account_id,
				amount_minor, currency, classification, payer_user_id,
				beneficiary_type, entry_time, is_split, is_manual
			) VALUES (
				$1, $2, $3, $4,
				-5700, 'RUB', 'expense', $5,
				'household', $6, FALSE, FALSE
			)
		`, uuid.New(), familyID, txID2, accountID, userID, t1)
		return err
	})
	if err != nil {
		t.Fatalf("failed to insert 2nd existing transaction: %v", err)
	}

	// Case C: 2 existing in DB -> both rows must now be confirmed duplicates
	err = database.WithFamilyUserTx(ctx, appPool, familyID, userID, func(tx pgx.Tx) error {
		staged, err := MatchStagedRows(ctx, tx, familyID, accountID, batchID, rows)
		if err != nil {
			return err
		}
		if staged[0].DuplicateStatus != DuplicateStatusConfirmed || staged[0].ResolutionAction != ResolutionActionSkipDuplicate {
			t.Errorf("row 1: expected confirmed/skip_duplicate, got %s/%s", staged[0].DuplicateStatus, staged[0].ResolutionAction)
		}
		if staged[1].DuplicateStatus != DuplicateStatusConfirmed || staged[1].ResolutionAction != ResolutionActionSkipDuplicate {
			t.Errorf("row 2: expected confirmed/skip_duplicate, got %s/%s", staged[1].DuplicateStatus, staged[1].ResolutionAction)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Case C failed: %v", err)
	}
}

// 16. Overlapping period re-import:
// Importing an overlapping statement period matches already committed transactions
// by source external ID or fingerprint, preventing duplicate ledger entries.
func TestDeduplication_OverlappingPeriodReImport(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()

	ctx := context.Background()
	familyID, userID, accountID := setupStatementTenantFixture(t)
	batchID := uuid.New()

	extID1 := "ext_tx_overlap_001"
	t1 := time.Date(2026, 4, 15, 10, 0, 0, 0, time.UTC)
	fp1 := ComputeImportFingerprint(accountID, "posted", nil, t1, -12000, "RUB", "Pharmacy")

	// Commit Tx1 into the ledger
	txID1 := uuid.New()
	err := database.WithFamilyUserTx(ctx, appPool, familyID, userID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO public.bank_transactions (
				id, family_id, account_id, external_id, fingerprint_hash, fingerprint_sequence,
				amount_minor, currency, account_amount_minor, account_currency,
				booking_status, operation_type, transaction_time, raw_description
			) VALUES (
				$1, $2, $3, $4, $5, 1,
				-12000, 'RUB', -12000, 'RUB',
				'posted', 'purchase', $6, 'Pharmacy'
			)
		`, txID1, familyID, accountID, extID1, fp1, t1)
		if err != nil {
			return err
		}

		_, err = tx.Exec(ctx, `
			INSERT INTO public.accounting_entries (
				id, family_id, bank_transaction_id, account_id,
				amount_minor, currency, classification, payer_user_id,
				beneficiary_type, entry_time, is_split, is_manual
			) VALUES (
				$1, $2, $3, $4,
				-12000, 'RUB', 'expense', $5,
				'household', $6, FALSE, FALSE
			)
		`, uuid.New(), familyID, txID1, accountID, userID, t1)
		return err
	})
	if err != nil {
		t.Fatalf("failed to seed committed Tx1: %v", err)
	}

	// Second statement import covers an overlapping period containing Tx1 AND a new Tx2
	extID2 := "ext_tx_overlap_002"
	t2 := time.Date(2026, 4, 16, 11, 0, 0, 0, time.UTC)

	overlapRows := []StagedRowDTO{
		{
			RowIndex:           1,
			SourceExternalID:   &extID1,
			TransactionTime:    t1,
			AmountMinor:        -12000,
			Currency:           "RUB",
			AccountAmountMinor: -12000,
			AccountCurrency:    "RUB",
			BookingStatus:      "posted",
			OperationType:      "purchase",
			RawDescription:     "Pharmacy",
		},
		{
			RowIndex:           2,
			SourceExternalID:   &extID2,
			TransactionTime:    t2,
			AmountMinor:        -35000,
			Currency:           "RUB",
			AccountAmountMinor: -35000,
			AccountCurrency:    "RUB",
			BookingStatus:      "posted",
			OperationType:      "purchase",
			RawDescription:     "Supermarket",
		},
	}

	err = database.WithFamilyUserTx(ctx, appPool, familyID, userID, func(tx pgx.Tx) error {
		staged, err := MatchStagedRows(ctx, tx, familyID, accountID, batchID, overlapRows)
		if err != nil {
			return err
		}

		// Tx1 (overlap) must be identified as duplicate with skip action
		if staged[0].DuplicateStatus != DuplicateStatusConfirmed {
			t.Errorf("Tx1: expected DuplicateStatusConfirmed, got %s", staged[0].DuplicateStatus)
		}
		if staged[0].ResolutionAction != ResolutionActionSkipDuplicate {
			t.Errorf("Tx1: expected ResolutionActionSkipDuplicate, got %s", staged[0].ResolutionAction)
		}
		if staged[0].MatchedExistingTxID == nil || *staged[0].MatchedExistingTxID != txID1 {
			t.Errorf("Tx1: expected MatchedExistingTxID = %s, got %v", txID1, staged[0].MatchedExistingTxID)
		}

		// Tx2 (new) must be identified as new with commit action
		if staged[1].DuplicateStatus != DuplicateStatusNew {
			t.Errorf("Tx2: expected DuplicateStatusNew, got %s", staged[1].DuplicateStatus)
		}
		if staged[1].ResolutionAction != ResolutionActionCommit {
			t.Errorf("Tx2: expected ResolutionActionCommit, got %s", staged[1].ResolutionAction)
		}
		if staged[1].MatchedExistingTxID != nil {
			t.Errorf("Tx2: expected nil MatchedExistingTxID, got %v", staged[1].MatchedExistingTxID)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("overlapping period matching failed: %v", err)
	}
}

// Finding 7 verification: when multiple staged rows have the same amount and date matching a single pending hold,
// only the first staged row claims the settlement update. The second row remains a new transaction.
// Both rows promote cleanly without state conflict or duplicate settlement error.
func TestMatchStagedRows_MultipleRowsMatchingSamePendingHold(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()

	ctx := context.Background()
	familyID, userID, accountID := setupStatementTenantFixture(t)
	batchID := uuid.New()
	pendingTxID := uuid.New()
	tPending := time.Date(2026, 5, 10, 14, 0, 0, 0, time.UTC)
	fpPending := ComputeImportFingerprint(accountID, "pending", nil, tPending, -25000, "RUB", "Restaurant Hold")

	// 1. Seed a SINGLE pending authorization hold in the database
	err := database.WithFamilyUserTx(ctx, appPool, familyID, userID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO public.bank_transactions (
				id, family_id, account_id, fingerprint_hash, fingerprint_sequence,
				amount_minor, currency, account_amount_minor, account_currency,
				booking_status, operation_type, transaction_time, raw_description
			) VALUES (
				$1, $2, $3, $4, 1,
				-25000, 'RUB', -25000, 'RUB',
				'pending', 'purchase', $5, 'Restaurant Hold'
			)
		`, pendingTxID, familyID, accountID, fpPending, tPending)
		if err != nil {
			return err
		}

		_, err = tx.Exec(ctx, `
			INSERT INTO public.accounting_entries (
				id, family_id, bank_transaction_id, account_id,
				amount_minor, currency, classification, payer_user_id,
				beneficiary_type, entry_time, is_split, is_manual
			) VALUES (
				$1, $2, $3, $4,
				-25000, 'RUB', 'expense', $5,
				'household', $6, FALSE, FALSE
			)
		`, uuid.New(), familyID, pendingTxID, accountID, userID, tPending)
		return err
	})
	if err != nil {
		t.Fatalf("failed to seed pending hold: %v", err)
	}

	// 2. Statement contains TWO separate posted operations of -250.00 RUB at the exact same restaurant
	twoRows := []StagedRowDTO{
		{
			RowIndex:           1,
			TransactionTime:    tPending,
			SettledTime:        &tPending,
			AmountMinor:        -25000,
			Currency:           "RUB",
			AccountAmountMinor: -25000,
			AccountCurrency:    "RUB",
			BookingStatus:      "posted",
			OperationType:      "purchase",
			RawDescription:     "Restaurant Dinner 1",
		},
		{
			RowIndex:           2,
			TransactionTime:    tPending,
			SettledTime:        &tPending,
			AmountMinor:        -25000,
			Currency:           "RUB",
			AccountAmountMinor: -25000,
			AccountCurrency:    "RUB",
			BookingStatus:      "posted",
			OperationType:      "purchase",
			RawDescription:     "Restaurant Dinner 2",
		},
	}

	err = database.WithFamilyUserTx(ctx, appPool, familyID, userID, func(tx pgx.Tx) error {
		staged, err := MatchStagedRows(ctx, tx, familyID, accountID, batchID, twoRows)
		if err != nil {
			return err
		}

		if len(staged) != 2 {
			t.Fatalf("expected 2 staged rows, got %d", len(staged))
		}

		// First row must claim the pending hold
		if staged[0].DuplicateStatus != DuplicateStatusSettlementUpdate {
			t.Errorf("Row 1: expected DuplicateStatusSettlementUpdate, got %s", staged[0].DuplicateStatus)
		}
		if staged[0].ResolutionAction != ResolutionActionApplySettlement {
			t.Errorf("Row 1: expected ResolutionActionApplySettlement, got %s", staged[0].ResolutionAction)
		}
		if staged[0].MatchedExistingTxID == nil || *staged[0].MatchedExistingTxID != pendingTxID {
			t.Errorf("Row 1: expected MatchedExistingTxID = %s, got %v", pendingTxID, staged[0].MatchedExistingTxID)
		}

		// Second row MUST NOT claim the same pending hold! It must remain a new transaction.
		if staged[1].DuplicateStatus != DuplicateStatusNew {
			t.Errorf("Row 2: expected DuplicateStatusNew, got %s (collision on pending hold!)", staged[1].DuplicateStatus)
		}
		if staged[1].ResolutionAction != ResolutionActionCommit {
			t.Errorf("Row 2: expected ResolutionActionCommit, got %s", staged[1].ResolutionAction)
		}
		if staged[1].MatchedExistingTxID != nil {
			t.Errorf("Row 2: expected nil MatchedExistingTxID, got %v", staged[1].MatchedExistingTxID)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("matching failed: %v", err)
	}
}

package statement

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// CanonicalizeDescription normalizes raw transaction description by trimming, lowercasing,
// and collapsing repeated internal whitespace into single spaces.
func CanonicalizeDescription(rawDesc string) string {
	return strings.ToLower(strings.Join(strings.Fields(rawDesc), " "))
}

// ComputeImportFingerprint computes a deterministic SHA-256 fingerprint for a staged transaction row.
// Combines account ID, normalized booking status, authoritative clearing timestamp, minor unit amount,
// uppercase ISO currency, and normalized trimmed description.
func ComputeImportFingerprint(
	accountID uuid.UUID,
	bookingStatus string,
	settledTime *time.Time,
	txTime time.Time,
	accountAmountMinor int64,
	accountCurrency string,
	rawDesc string,
) string {
	clearingTime := txTime.UTC()
	if settledTime != nil && !settledTime.IsZero() {
		clearingTime = settledTime.UTC()
	}

	normStatus := strings.ToLower(strings.TrimSpace(bookingStatus))
	normCur := strings.ToUpper(strings.TrimSpace(accountCurrency))
	normDesc := CanonicalizeDescription(rawDesc)

	payload := fmt.Sprintf("%s|%s|%s|%d|%s|%s",
		accountID.String(),
		normStatus,
		clearingTime.Format(time.RFC3339Nano),
		accountAmountMinor,
		normCur,
		normDesc,
	)

	hash := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(hash[:])
}

// MatchStagedRows evaluates staged rows against existing committed transactions in the database.
// Implements:
// 1. Exact Source External ID deduplication.
// 2. Multiplicity-based fingerprint deduplication (allowing legitimate identical operations).
// 3. Pending-to-posted settlement candidate matching.
// 4. Fuzzy duplicate candidate detection (requiring user resolution).
func MatchStagedRows(
	ctx context.Context,
	tx pgx.Tx,
	familyID, accountID, batchID uuid.UUID,
	rows []StagedRowDTO,
) ([]StagedBankTransaction, error) {
	staged := make([]StagedBankTransaction, len(rows))
	batchFpCount := make(map[string]int)
	claimedExistingIDs := make(map[uuid.UUID]bool)

	for i, r := range rows {
		fp := ComputeImportFingerprint(
			accountID,
			r.BookingStatus,
			r.SettledTime,
			r.TransactionTime,
			r.AccountAmountMinor,
			r.AccountCurrency,
			r.RawDescription,
		)
		batchFpCount[fp]++
		occurrenceInBatch := batchFpCount[fp]

		st := StagedBankTransaction{
			ID:                 uuid.New(),
			FamilyID:           familyID,
			BatchID:            batchID,
			AccountID:          accountID,
			SourceExternalID:   r.SourceExternalID,
			CardMask:           r.CardMask,
			RowIndex:           r.RowIndex,
			TransactionTime:    r.TransactionTime,
			SettledTime:        r.SettledTime,
			AmountMinor:        r.AmountMinor,
			Currency:           strings.ToUpper(strings.TrimSpace(r.Currency)),
			AccountAmountMinor: r.AccountAmountMinor,
			AccountCurrency:    strings.ToUpper(strings.TrimSpace(r.AccountCurrency)),
			OperationType:      r.OperationType,
			BookingStatus:      strings.ToLower(strings.TrimSpace(r.BookingStatus)),
			RawDescription:     r.RawDescription,
			MerchantName:       r.MerchantName,
			MCC:                r.MCC,
			BankCategory:       r.BankCategory,
			ImportFingerprint:  fp,
			DuplicateStatus:    DuplicateStatusNew,
			ResolutionAction:   ResolutionActionCommit,
		}

		// 1. Priority 1: Match by Source External ID
		if r.SourceExternalID != nil && strings.TrimSpace(*r.SourceExternalID) != "" {
			var existingID uuid.UUID
			err := tx.QueryRow(ctx, `
				SELECT id FROM public.bank_transactions
				WHERE family_id = $1 AND account_id = $2 AND external_id = $3
				LIMIT 1
			`, familyID, accountID, strings.TrimSpace(*r.SourceExternalID)).Scan(&existingID)
			if err == nil {
				st.DuplicateStatus = DuplicateStatusConfirmed
				st.ResolutionAction = ResolutionActionSkipDuplicate
				st.MatchedExistingTxID = &existingID
				staged[i] = st
				continue
			}
		}

		// 2. Priority 2: Multiplicity-based Fingerprint Matching
		var dbFpCount int
		err := tx.QueryRow(ctx, `
			SELECT count(*) FROM public.bank_transactions
			WHERE family_id = $1 AND account_id = $2 AND fingerprint_hash = $3
		`, familyID, accountID, fp).Scan(&dbFpCount)
		if err != nil {
			return nil, fmt.Errorf("failed to query fingerprint multiplicity: %w", err)
		}

		if occurrenceInBatch <= dbFpCount {
			// This occurrence already exists in database -> confirmed duplicate
			var existingID uuid.UUID
			_ = tx.QueryRow(ctx, `
				SELECT id FROM public.bank_transactions
				WHERE family_id = $1 AND account_id = $2 AND fingerprint_hash = $3
				ORDER BY transaction_time ASC
				LIMIT 1
			`, familyID, accountID, fp).Scan(&existingID)

			st.DuplicateStatus = DuplicateStatusConfirmed
			st.ResolutionAction = ResolutionActionSkipDuplicate
			if existingID != uuid.Nil {
				st.MatchedExistingTxID = &existingID
			}
			staged[i] = st
			continue
		}

		// 3. Priority 3: Pending Authorization Settlement Matching
		// If staged row is 'posted', look for a matching 'pending' hold on this account
		if st.BookingStatus == "posted" {
			clearingTime := st.TransactionTime.UTC()
			if st.SettledTime != nil && !st.SettledTime.IsZero() {
				clearingTime = st.SettledTime.UTC()
			}

			// Look for pending hold within +/- 7 days matching account_amount_minor
			rows, err := tx.Query(ctx, `
				SELECT id FROM public.bank_transactions
				WHERE family_id = $1 
				  AND account_id = $2 
				  AND booking_status = 'pending'
				  AND account_amount_minor = $3
				  AND transaction_time BETWEEN ($4::TIMESTAMPTZ - INTERVAL '7 days') AND ($4::TIMESTAMPTZ + INTERVAL '7 days')
				ORDER BY ABS(EXTRACT(EPOCH FROM (transaction_time - $4::TIMESTAMPTZ))) ASC
			`, familyID, accountID, st.AccountAmountMinor, clearingTime)
			if err == nil {
				for rows.Next() {
					var pendingID uuid.UUID
					if err := rows.Scan(&pendingID); err == nil {
						if !claimedExistingIDs[pendingID] {
							claimedExistingIDs[pendingID] = true
							st.DuplicateStatus = DuplicateStatusSettlementUpdate
							st.ResolutionAction = ResolutionActionApplySettlement
							st.MatchedExistingTxID = &pendingID
							break
						}
					}
				}
				rows.Close()
				if st.MatchedExistingTxID != nil {
					staged[i] = st
					continue
				}
			}
		}

		// 4. Priority 4: Fuzzy Candidate Match (Similar amount and date window +/- 3 days, different description)
		clearingTime := st.TransactionTime.UTC()
		if st.SettledTime != nil && !st.SettledTime.IsZero() {
			clearingTime = st.SettledTime.UTC()
		}

		fRows, err := tx.Query(ctx, `
			SELECT id FROM public.bank_transactions
			WHERE family_id = $1 
			  AND account_id = $2 
			  AND booking_status = 'posted'
			  AND account_amount_minor = $3
			  AND transaction_time BETWEEN ($4::TIMESTAMPTZ - INTERVAL '3 days') AND ($4::TIMESTAMPTZ + INTERVAL '3 days')
			  AND raw_description != $5
			ORDER BY ABS(EXTRACT(EPOCH FROM (transaction_time - $4::TIMESTAMPTZ))) ASC
		`, familyID, accountID, st.AccountAmountMinor, clearingTime, st.RawDescription)
		if err == nil {
			for fRows.Next() {
				var candidateID uuid.UUID
				if err := fRows.Scan(&candidateID); err == nil {
					if !claimedExistingIDs[candidateID] {
						claimedExistingIDs[candidateID] = true
						st.DuplicateStatus = DuplicateStatusPotential
						st.ResolutionAction = ResolutionActionPending // Requires explicit user resolution!
						st.MatchedExistingTxID = &candidateID
						break
					}
				}
			}
			fRows.Close()
			if st.MatchedExistingTxID != nil {
				staged[i] = st
				continue
			}
		}

		// 5. Default: Strictly New Transaction
		st.DuplicateStatus = DuplicateStatusNew
		st.ResolutionAction = ResolutionActionCommit
		staged[i] = st
	}

	return staged, nil
}

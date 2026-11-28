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

// CleanMerchantName normalizes a merchant name string.
func CleanMerchantName(name *string) string {
	if name == nil {
		return ""
	}
	return strings.ToLower(strings.Join(strings.Fields(*name), " "))
}

// TokenizeMerchant splits text into meaningful lowercase tokens (filtering tiny punctuation and noise).
func TokenizeMerchant(text string) map[string]struct{} {
	tokens := make(map[string]struct{})
	f := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == ',' || r == '.' || r == '-' || r == '_' ||
			r == '/' || r == '\\' || r == '*' || r == '"' || r == '\'' || r == '«' || r == '»' || r == ':'
	})
	for _, tok := range f {
		if len(tok) >= 2 { // filter out single-char noise
			tokens[tok] = struct{}{}
		}
	}
	return tokens
}

// CheckMerchantCompatibility evaluates whether staged row merchant/description matches
// an existing candidate pending transaction.
// Returns (matches, isContradictory).
func CheckMerchantCompatibility(stMerchant *string, stRawDesc string, candMerchant *string, candRawDesc string) (bool, bool) {
	stCleanM := CleanMerchantName(stMerchant)
	candCleanM := CleanMerchantName(candMerchant)
	stCleanD := CanonicalizeDescription(stRawDesc)
	candCleanD := CanonicalizeDescription(candRawDesc)

	// 1. If both have merchant names:
	if stCleanM != "" && candCleanM != "" {
		if stCleanM == candCleanM || strings.Contains(stCleanM, candCleanM) || strings.Contains(candCleanM, stCleanM) {
			return true, false
		}
		// Check token overlap
		stTokens := TokenizeMerchant(stCleanM)
		candTokens := TokenizeMerchant(candCleanM)
		shared := 0
		for tok := range stTokens {
			if _, ok := candTokens[tok]; ok {
				shared++
			}
		}
		if shared > 0 {
			return true, false
		}
		// If merchant names are completely distinct, they are contradictory!
		return false, true
	}

	// 2. If one has merchant name and the other has only raw description:
	nameToMatch := stCleanM
	descToMatch := candCleanD
	if nameToMatch == "" {
		nameToMatch = candCleanM
		descToMatch = stCleanD
	}

	if nameToMatch != "" {
		if strings.Contains(descToMatch, nameToMatch) {
			return true, false
		}
		nameTokens := TokenizeMerchant(nameToMatch)
		descTokens := TokenizeMerchant(descToMatch)
		shared := 0
		for tok := range nameTokens {
			if _, ok := descTokens[tok]; ok {
				shared++
			}
		}
		if shared > 0 {
			return true, false
		}
		return false, true
	}

	// 3. Neither has a parsed merchant name; compare raw descriptions:
	if stCleanD != "" && candCleanD != "" {
		if stCleanD == candCleanD || strings.Contains(stCleanD, candCleanD) || strings.Contains(candCleanD, stCleanD) {
			return true, false
		}
		stTokens := TokenizeMerchant(stCleanD)
		candTokens := TokenizeMerchant(candCleanD)
		shared := 0
		for tok := range stTokens {
			if _, ok := candTokens[tok]; ok {
				shared++
			}
		}
		if shared > 0 {
			return true, false
		}
		return false, true
	}

	return false, false
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

type pendingCandidateInfo struct {
	id            uuid.UUID
	amount        int64
	isExactAmount bool
}

// MatchStagedRows evaluates staged rows against existing committed transactions in the database.
// Implements:
// 1. Exact Source External ID deduplication & settlement/cancellation candidate matching.
// 2. Multiplicity-based fingerprint deduplication (allowing legitimate identical operations).
// 3. Pending-to-posted settlement matching using merchant/description evidence & amount change support.
// 4. Pending authorization cancellation and release for cancelled statement operations.
// 5. Fuzzy candidate detection for potential duplicates.
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
			extID := strings.TrimSpace(*r.SourceExternalID)
			var existingID uuid.UUID
			var existingStatus string
			err := tx.QueryRow(ctx, `
				SELECT id, booking_status FROM public.bank_transactions
				WHERE family_id = $1 AND account_id = $2 AND external_id = $3
				LIMIT 1
			`, familyID, accountID, extID).Scan(&existingID, &existingStatus)
			if err == nil {
				if existingStatus == "posted" {
					st.DuplicateStatus = DuplicateStatusConfirmed
					st.ResolutionAction = ResolutionActionSkipDuplicate
					st.MatchedExistingTxID = &existingID
					staged[i] = st
					continue
				} else if existingStatus == "pending" {
					if st.BookingStatus == "posted" {
						st.DuplicateStatus = DuplicateStatusSettlementUpdate
						st.ResolutionAction = ResolutionActionApplySettlement
						st.MatchedExistingTxID = &existingID
						claimedExistingIDs[existingID] = true
						staged[i] = st
						continue
					} else if st.BookingStatus == "cancelled" {
						st.DuplicateStatus = DuplicateStatusCancellationUpdate
						st.ResolutionAction = ResolutionActionReleasePending
						st.MatchedExistingTxID = &existingID
						claimedExistingIDs[existingID] = true
						staged[i] = st
						continue
					}
				} else if existingStatus == "cancelled" {
					st.DuplicateStatus = DuplicateStatusConfirmed
					st.ResolutionAction = ResolutionActionSkipCancelled
					st.MatchedExistingTxID = &existingID
					staged[i] = st
					continue
				}
			}
		}

		// 2. Cancellation Lifecycle Handling for Cancelled Rows
		if st.BookingStatus == "cancelled" {
			// Search for active pending hold to release
			cRows, err := tx.Query(ctx, `
				SELECT id, transaction_time, account_amount_minor, raw_description, merchant_name, mcc
				FROM public.bank_transactions
				WHERE family_id = $1
				  AND account_id = $2
				  AND booking_status = 'pending'
				  AND account_currency = $3
				  AND transaction_time BETWEEN ($4::TIMESTAMPTZ - INTERVAL '14 days') AND ($4::TIMESTAMPTZ + INTERVAL '2 days')
				ORDER BY ABS(EXTRACT(EPOCH FROM (transaction_time - $4::TIMESTAMPTZ))) ASC
			`, familyID, accountID, st.AccountCurrency, st.TransactionTime.UTC())
			if err == nil {
				var cancelCandidates []pendingCandidateInfo
				for cRows.Next() {
					var cid uuid.UUID
					var cTime time.Time
					var cAmount int64
					var cRawDesc string
					var cMerchant *string
					var cMCC *int
					if err := cRows.Scan(&cid, &cTime, &cAmount, &cRawDesc, &cMerchant, &cMCC); err != nil {
						continue
					}
					if claimedExistingIDs[cid] {
						continue
					}
					// Direction check
					if (st.AccountAmountMinor < 0 && cAmount > 0) || (st.AccountAmountMinor > 0 && cAmount < 0) {
						continue
					}
					matches, contradictory := CheckMerchantCompatibility(st.MerchantName, st.RawDescription, cMerchant, cRawDesc)
					if contradictory {
						continue
					}
					if matches {
						cancelCandidates = append(cancelCandidates, pendingCandidateInfo{
							id:            cid,
							amount:        cAmount,
							isExactAmount: (cAmount == st.AccountAmountMinor),
						})
					}
				}
				cRows.Close()

				if len(cancelCandidates) == 1 {
					cand := cancelCandidates[0]
					claimedExistingIDs[cand.id] = true
					st.DuplicateStatus = DuplicateStatusCancellationUpdate
					st.ResolutionAction = ResolutionActionReleasePending
					st.MatchedExistingTxID = &cand.id
					staged[i] = st
					continue
				} else if len(cancelCandidates) > 1 {
					// Check if exactly one has exact amount match
					var exactCancel []pendingCandidateInfo
					for _, c := range cancelCandidates {
						if c.isExactAmount {
							exactCancel = append(exactCancel, c)
						}
					}
					if len(exactCancel) == 1 {
						cand := exactCancel[0]
						claimedExistingIDs[cand.id] = true
						st.DuplicateStatus = DuplicateStatusCancellationUpdate
						st.ResolutionAction = ResolutionActionReleasePending
						st.MatchedExistingTxID = &cand.id
						staged[i] = st
						continue
					}
					// Ambiguous cancellation candidates
					st.DuplicateStatus = DuplicateStatusPotential
					st.ResolutionAction = ResolutionActionPending
					staged[i] = st
					continue
				}
			}

			// Standalone cancelled operation without pending hold: skip
			st.DuplicateStatus = DuplicateStatusNew
			st.ResolutionAction = ResolutionActionSkipCancelled
			staged[i] = st
			continue
		}

		// 3. Priority 2: Multiplicity-based Fingerprint Matching against Posted Transactions
		var pendingFp string
		if st.BookingStatus == "posted" {
			pendingFp = ComputeImportFingerprint(
				accountID,
				"pending",
				nil,
				st.TransactionTime,
				st.AccountAmountMinor,
				st.AccountCurrency,
				st.RawDescription,
			)
		}

		var dbFpCount int
		err := tx.QueryRow(ctx, `
			SELECT count(*) FROM public.bank_transactions
			WHERE family_id = $1 AND account_id = $2
			  AND (fingerprint_hash = $3 OR ($4 != '' AND fingerprint_hash = $4 AND booking_status = 'posted'))
		`, familyID, accountID, fp, pendingFp).Scan(&dbFpCount)
		if err != nil {
			return nil, fmt.Errorf("failed to query fingerprint multiplicity: %w", err)
		}

		if occurrenceInBatch <= dbFpCount {
			// This occurrence already exists in database as posted -> confirmed duplicate
			var existingID uuid.UUID
			_ = tx.QueryRow(ctx, `
				SELECT id FROM public.bank_transactions
				WHERE family_id = $1 AND account_id = $2
				  AND (fingerprint_hash = $3 OR ($4 != '' AND fingerprint_hash = $4 AND booking_status = 'posted'))
				ORDER BY transaction_time ASC
				LIMIT 1
			`, familyID, accountID, fp, pendingFp).Scan(&existingID)

			st.DuplicateStatus = DuplicateStatusConfirmed
			st.ResolutionAction = ResolutionActionSkipDuplicate
			if existingID != uuid.Nil {
				st.MatchedExistingTxID = &existingID
			}
			staged[i] = st
			continue
		}

		// 4. Priority 3: Pending Authorization Settlement Matching
		// If staged row is 'posted', look for an active 'pending' hold on this account
		if st.BookingStatus == "posted" {
			clearingTime := st.TransactionTime.UTC()
			if st.SettledTime != nil && !st.SettledTime.IsZero() {
				clearingTime = st.SettledTime.UTC()
			}

			pRows, err := tx.Query(ctx, `
				SELECT id, transaction_time, account_amount_minor, raw_description, merchant_name, mcc, external_id
				FROM public.bank_transactions
				WHERE family_id = $1
				  AND account_id = $2
				  AND booking_status = 'pending'
				  AND account_currency = $3
				  AND transaction_time BETWEEN ($4::TIMESTAMPTZ - INTERVAL '14 days') AND ($4::TIMESTAMPTZ + INTERVAL '2 days')
				ORDER BY ABS(EXTRACT(EPOCH FROM (transaction_time - $4::TIMESTAMPTZ))) ASC
			`, familyID, accountID, st.AccountCurrency, clearingTime)
			if err == nil {
				var candidates []pendingCandidateInfo
				for pRows.Next() {
					var cid uuid.UUID
					var cTime time.Time
					var cAmount int64
					var cRawDesc string
					var cMerchant *string
					var cMCC *int
					var cExtID *string
					if err := pRows.Scan(&cid, &cTime, &cAmount, &cRawDesc, &cMerchant, &cMCC, &cExtID); err != nil {
						continue
					}
					if claimedExistingIDs[cid] {
						continue
					}
					// Direction check
					if (st.AccountAmountMinor < 0 && cAmount > 0) || (st.AccountAmountMinor > 0 && cAmount < 0) {
						continue
					}
					// Merchant compatibility check
					matches, contradictory := CheckMerchantCompatibility(st.MerchantName, st.RawDescription, cMerchant, cRawDesc)
					if contradictory {
						continue
					}
					if matches {
						candidates = append(candidates, pendingCandidateInfo{
							id:            cid,
							amount:        cAmount,
							isExactAmount: (cAmount == st.AccountAmountMinor),
						})
					}
				}
				pRows.Close()

				if len(candidates) == 1 {
					cand := candidates[0]
					claimedExistingIDs[cand.id] = true
					st.DuplicateStatus = DuplicateStatusSettlementUpdate
					st.ResolutionAction = ResolutionActionApplySettlement
					st.MatchedExistingTxID = &cand.id
					staged[i] = st
					continue
				} else if len(candidates) > 1 {
					// Check if exactly one has exact amount match
					var exactMatches []pendingCandidateInfo
					for _, c := range candidates {
						if c.isExactAmount {
							exactMatches = append(exactMatches, c)
						}
					}
					if len(exactMatches) == 1 {
						cand := exactMatches[0]
						claimedExistingIDs[cand.id] = true
						st.DuplicateStatus = DuplicateStatusSettlementUpdate
						st.ResolutionAction = ResolutionActionApplySettlement
						st.MatchedExistingTxID = &cand.id
						staged[i] = st
						continue
					}
					// Truly ambiguous: multiple candidates
					st.DuplicateStatus = DuplicateStatusPotential
					st.ResolutionAction = ResolutionActionPending
					staged[i] = st
					continue
				}
			}
		}

		// 5. Priority 4: Fuzzy Candidate Match (Similar amount and date window +/- 3 days, different description)
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

		// 6. Default: Strictly New Transaction
		st.DuplicateStatus = DuplicateStatusNew
		st.ResolutionAction = ResolutionActionCommit
		staged[i] = st
	}

	return staged, nil
}

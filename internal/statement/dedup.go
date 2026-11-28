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

// MerchantMatchClassification defines confidence levels for matching merchant evidence.
type MerchantMatchClassification int

const (
	MerchantContradictory   MerchantMatchClassification = 0 // Conflicting evidence; cannot match
	MerchantAmbiguous       MerchantMatchClassification = 1 // Inconclusive evidence; requires user review
	MerchantProbableMatch   MerchantMatchClassification = 2 // Likely match (significant token overlap)
	MerchantNormalizedMatch MerchantMatchClassification = 3 // Identical after transliteration & stop-word cleanup
	MerchantExactMatch      MerchantMatchClassification = 4 // Exact string identity
)

var merchantStopWords = map[string]struct{}{
	// Legal entities
	"ooo": {}, "zao": {}, "pao": {}, "oao": {}, "ip": {},
	"llc": {}, "ltd": {}, "inc": {}, "corp": {}, "gmbh": {}, "co": {}, "cjsc": {}, "pjsc": {},
	// Locations
	"moscow": {}, "msk": {}, "spb": {}, "rus": {}, "ru": {}, "city": {}, "russia": {},
	// Channels & generic transaction words
	"card": {}, "karta": {}, "retail": {}, "pos": {}, "ecom": {}, "atm": {},
	"cafe": {}, "kafe": {}, "shop": {}, "magazin": {}, "store": {}, "bar": {},
	"pay": {}, "online": {}, "oplata": {}, "sbp": {}, "bank": {}, "market": {},
}

// TransliterateRussianToLatin maps Russian Cyrillic characters to lowercase Latin equivalents.
func TransliterateRussianToLatin(s string) string {
	var sb strings.Builder
	sb.Grow(len(s))
	for _, r := range s {
		switch r {
		case 'а', 'А':
			sb.WriteString("a")
		case 'б', 'Б':
			sb.WriteString("b")
		case 'в', 'В':
			sb.WriteString("v")
		case 'г', 'Г':
			sb.WriteString("g")
		case 'д', 'Д':
			sb.WriteString("d")
		case 'е', 'Е', 'ё', 'Ё':
			sb.WriteString("e")
		case 'ж', 'Ж':
			sb.WriteString("zh")
		case 'з', 'З':
			sb.WriteString("z")
		case 'и', 'И':
			sb.WriteString("i")
		case 'й', 'Й':
			sb.WriteString("y")
		case 'к', 'К':
			sb.WriteString("k")
		case 'л', 'Л':
			sb.WriteString("l")
		case 'м', 'М':
			sb.WriteString("m")
		case 'н', 'Н':
			sb.WriteString("n")
		case 'о', 'О':
			sb.WriteString("o")
		case 'п', 'П':
			sb.WriteString("p")
		case 'р', 'Р':
			sb.WriteString("r")
		case 'с', 'С':
			sb.WriteString("s")
		case 'т', 'Т':
			sb.WriteString("t")
		case 'у', 'У':
			sb.WriteString("u")
		case 'ф', 'Ф':
			sb.WriteString("f")
		case 'х', 'Х':
			sb.WriteString("kh")
		case 'ц', 'Ц':
			sb.WriteString("ts")
		case 'ч', 'Ч':
			sb.WriteString("ch")
		case 'ш', 'Ш':
			sb.WriteString("sh")
		case 'щ', 'Щ':
			sb.WriteString("shch")
		case 'ъ', 'Ъ', 'ь', 'Ь':
			// omit hard/soft signs
		case 'ы', 'Ы':
			sb.WriteString("y")
		case 'э', 'Э':
			sb.WriteString("e")
		case 'ю', 'Ю':
			sb.WriteString("yu")
		case 'я', 'Я':
			sb.WriteString("ya")
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// NormalizeForMatching converts text to lowercased, transliterated, normalized Latin string.
func NormalizeForMatching(text string) string {
	s := strings.ToLower(text)
	s = TransliterateRussianToLatin(s)
	s = strings.ReplaceAll(s, "x", "ks")
	return s
}

// TokenizeSignificantMerchant extracts normalized, transliterated non-stop-word tokens.
func TokenizeSignificantMerchant(text string) map[string]struct{} {
	norm := NormalizeForMatching(text)
	f := strings.FieldsFunc(norm, func(r rune) bool {
		return r < 'a' || (r > 'z' && (r < '0' || r > '9'))
	})
	tokens := make(map[string]struct{})
	for _, tok := range f {
		if len(tok) < 2 {
			continue
		}
		if _, isStop := merchantStopWords[tok]; !isStop {
			tokens[tok] = struct{}{}
		}
	}
	return tokens
}

// AllTokensExtract extracts all tokens without stop-word filtering (minimum 2 chars).
func AllTokensExtract(text string) map[string]struct{} {
	norm := NormalizeForMatching(text)
	f := strings.FieldsFunc(norm, func(r rune) bool {
		return r < 'a' || (r > 'z' && (r < '0' || r > '9'))
	})
	tokens := make(map[string]struct{})
	for _, tok := range f {
		if len(tok) >= 2 {
			tokens[tok] = struct{}{}
		}
	}
	return tokens
}

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

// ClassifyMerchantMatch evaluates whether staged row merchant/description matches
// an existing candidate pending transaction with confidence classification.
func ClassifyMerchantMatch(stMerchant *string, stRawDesc string, candMerchant *string, candRawDesc string) MerchantMatchClassification {
	stCleanM := CleanMerchantName(stMerchant)
	candCleanM := CleanMerchantName(candMerchant)
	stCleanD := CanonicalizeDescription(stRawDesc)
	candCleanD := CanonicalizeDescription(candRawDesc)

	// 1. Exact string match on merchant or raw description
	if stCleanM != "" && candCleanM != "" && stCleanM == candCleanM {
		return MerchantExactMatch
	}
	if stCleanD != "" && candCleanD != "" && stCleanD == candCleanD {
		return MerchantExactMatch
	}

	// 2. Normalized full string match (transliterated & unified)
	stNormM := NormalizeForMatching(stCleanM)
	candNormM := NormalizeForMatching(candCleanM)
	if stNormM != "" && candNormM != "" && stNormM == candNormM {
		return MerchantNormalizedMatch
	}

	stNormD := NormalizeForMatching(stCleanD)
	candNormD := NormalizeForMatching(candCleanD)
	if stNormD != "" && candNormD != "" && stNormD == candNormD {
		return MerchantNormalizedMatch
	}

	// Cross-check merchant name against raw description normalized
	if stNormM != "" && candNormD != "" && (stNormM == candNormD || strings.Contains(candNormD, stNormM)) {
		return MerchantNormalizedMatch
	}
	if candNormM != "" && stNormD != "" && (candNormM == stNormD || strings.Contains(stNormD, candNormM)) {
		return MerchantNormalizedMatch
	}

	// 3. Extract significant tokens (excluding stop-words)
	stTokens := TokenizeSignificantMerchant(stCleanM + " " + stCleanD)
	candTokens := TokenizeSignificantMerchant(candCleanM + " " + candCleanD)

	if len(stTokens) > 0 && len(candTokens) > 0 {
		shared := 0
		for tok := range stTokens {
			if _, ok := candTokens[tok]; ok {
				shared++
			}
		}

		if shared > 0 {
			if shared == len(stTokens) && shared == len(candTokens) {
				return MerchantNormalizedMatch
			}
			return MerchantProbableMatch
		}

		// Both have significant tokens, but 0 overlap -> contradictory!
		return MerchantContradictory
	}

	// If one has significant tokens and the other has only stop-words or noise:
	if len(stTokens) > 0 || len(candTokens) > 0 {
		stAll := AllTokensExtract(stCleanM + " " + stCleanD)
		candAll := AllTokensExtract(candCleanM + " " + candCleanD)
		sharedAll := 0
		for tok := range stAll {
			if _, ok := candAll[tok]; ok {
				sharedAll++
			}
		}
		if sharedAll == 0 {
			return MerchantContradictory
		}
		return MerchantContradictory
	}

	// Both have only stop words or are completely empty
	stAll := AllTokensExtract(stCleanM + " " + stCleanD)
	candAll := AllTokensExtract(candCleanM + " " + candCleanD)
	if len(stAll) == 0 && len(candAll) == 0 {
		return MerchantAmbiguous
	}
	if len(stAll) > 0 && len(stAll) == len(candAll) {
		identical := true
		for tok := range stAll {
			if _, ok := candAll[tok]; !ok {
				identical = false
				break
			}
		}
		if identical {
			return MerchantAmbiguous
		}
	}

	return MerchantContradictory
}

// CheckMerchantCompatibility evaluates whether staged row merchant/description matches
// an existing candidate pending transaction.
// Returns (matches, isContradictory).
func CheckMerchantCompatibility(stMerchant *string, stRawDesc string, candMerchant *string, candRawDesc string) (bool, bool) {
	cls := ClassifyMerchantMatch(stMerchant, stRawDesc, candMerchant, candRawDesc)
	switch cls {
	case MerchantExactMatch, MerchantNormalizedMatch, MerchantProbableMatch:
		return true, false
	case MerchantContradictory:
		return false, true
	default:
		return false, false
	}
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
				type cancelCandWithConf struct {
					id            uuid.UUID
					amount        int64
					isExactAmount bool
					confidence    MerchantMatchClassification
				}
				var cancelCandidates []cancelCandWithConf
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
					conf := ClassifyMerchantMatch(st.MerchantName, st.RawDescription, cMerchant, cRawDesc)
					if conf == MerchantContradictory {
						continue
					}
					cancelCandidates = append(cancelCandidates, cancelCandWithConf{
						id:            cid,
						amount:        cAmount,
						isExactAmount: (cAmount == st.AccountAmountMinor),
						confidence:    conf,
					})
				}
				cRows.Close()

				if len(cancelCandidates) == 1 {
					cand := cancelCandidates[0]
					if cand.confidence >= MerchantProbableMatch {
						claimedExistingIDs[cand.id] = true
						st.DuplicateStatus = DuplicateStatusCancellationUpdate
						st.ResolutionAction = ResolutionActionReleasePending
						st.MatchedExistingTxID = &cand.id
						staged[i] = st
						continue
					} else {
						st.DuplicateStatus = DuplicateStatusPotential
						st.ResolutionAction = ResolutionActionPending
						st.MatchedExistingTxID = &cand.id
						staged[i] = st
						continue
					}
				} else if len(cancelCandidates) > 1 {
					var exactCancel []cancelCandWithConf
					for _, c := range cancelCandidates {
						if c.isExactAmount && c.confidence >= MerchantNormalizedMatch {
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
					st.DuplicateStatus = DuplicateStatusPotential
					st.ResolutionAction = ResolutionActionPending
					st.MatchedExistingTxID = &cancelCandidates[0].id
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
			SELECT count(DISTINCT bt.id) FROM public.bank_transactions bt
			WHERE bt.family_id = $1 AND bt.account_id = $2
			  AND (
			    bt.fingerprint_hash = $3
			    OR ($4 != '' AND bt.fingerprint_hash = $4 AND bt.booking_status = 'posted')
			    OR EXISTS (
			        SELECT 1 FROM public.bank_transaction_fingerprint_aliases fa
			        WHERE fa.family_id = bt.family_id AND fa.bank_transaction_id = bt.id
			          AND (fa.fingerprint_hash = $3 OR ($4 != '' AND fa.fingerprint_hash = $4 AND bt.booking_status = 'posted'))
			    )
			  )
		`, familyID, accountID, fp, pendingFp).Scan(&dbFpCount)
		if err != nil {
			return nil, fmt.Errorf("failed to query fingerprint multiplicity: %w", err)
		}

		if occurrenceInBatch <= dbFpCount {
			// This occurrence already exists in database as posted -> confirmed duplicate
			var existingID uuid.UUID
			_ = tx.QueryRow(ctx, `
				SELECT bt.id FROM public.bank_transactions bt
				WHERE bt.family_id = $1 AND bt.account_id = $2
				  AND (
				    bt.fingerprint_hash = $3
				    OR ($4 != '' AND bt.fingerprint_hash = $4 AND bt.booking_status = 'posted')
				    OR EXISTS (
				        SELECT 1 FROM public.bank_transaction_fingerprint_aliases fa
				        WHERE fa.family_id = bt.family_id AND fa.bank_transaction_id = bt.id
				          AND (fa.fingerprint_hash = $3 OR ($4 != '' AND fa.fingerprint_hash = $4 AND bt.booking_status = 'posted'))
				    )
				  )
				ORDER BY bt.transaction_time ASC
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
				type candWithConf struct {
					id            uuid.UUID
					amount        int64
					isExactAmount bool
					confidence    MerchantMatchClassification
				}
				var candidates []candWithConf
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
					// Direction check: cannot flip credit/debit
					if (st.AccountAmountMinor < 0 && cAmount > 0) || (st.AccountAmountMinor > 0 && cAmount < 0) {
						continue
					}
					// Merchant compatibility check with classification
					conf := ClassifyMerchantMatch(st.MerchantName, st.RawDescription, cMerchant, cRawDesc)
					if conf == MerchantContradictory {
						continue
					}
					candidates = append(candidates, candWithConf{
						id:            cid,
						amount:        cAmount,
						isExactAmount: (cAmount == st.AccountAmountMinor),
						confidence:    conf,
					})
				}
				pRows.Close()

				if len(candidates) > 0 {
					var highConf []candWithConf
					var probConf []candWithConf
					var ambigConf []candWithConf
					for _, c := range candidates {
						switch {
						case c.confidence >= MerchantNormalizedMatch:
							highConf = append(highConf, c)
						case c.confidence == MerchantProbableMatch:
							probConf = append(probConf, c)
						case c.confidence == MerchantAmbiguous:
							ambigConf = append(ambigConf, c)
						}
					}

					// Case 1: Exactly 1 high confidence candidate
					if len(highConf) == 1 {
						cand := highConf[0]
						claimedExistingIDs[cand.id] = true
						st.DuplicateStatus = DuplicateStatusSettlementUpdate
						st.ResolutionAction = ResolutionActionApplySettlement
						st.MatchedExistingTxID = &cand.id
						staged[i] = st
						continue
					} else if len(highConf) > 1 {
						// Multiple high confidence: check if exactly one has exact amount
						var exactHigh []candWithConf
						for _, c := range highConf {
							if c.isExactAmount {
								exactHigh = append(exactHigh, c)
							}
						}
						if len(exactHigh) == 1 {
							cand := exactHigh[0]
							claimedExistingIDs[cand.id] = true
							st.DuplicateStatus = DuplicateStatusSettlementUpdate
							st.ResolutionAction = ResolutionActionApplySettlement
							st.MatchedExistingTxID = &cand.id
							staged[i] = st
							continue
						}
						// Truly ambiguous among multiple high confidence
						st.DuplicateStatus = DuplicateStatusPotential
						st.ResolutionAction = ResolutionActionPending
						st.MatchedExistingTxID = &highConf[0].id
						staged[i] = st
						continue
					}

					// Case 2: No high confidence, but probable candidates exist
					if len(probConf) == 1 {
						cand := probConf[0]
						if cand.isExactAmount {
							// Exact amount with probable merchant match -> auto settle
							claimedExistingIDs[cand.id] = true
							st.DuplicateStatus = DuplicateStatusSettlementUpdate
							st.ResolutionAction = ResolutionActionApplySettlement
							st.MatchedExistingTxID = &cand.id
							staged[i] = st
							continue
						} else {
							// Amount changed with only probable merchant match -> flag for user review
							st.DuplicateStatus = DuplicateStatusPotential
							st.ResolutionAction = ResolutionActionPending
							st.MatchedExistingTxID = &cand.id
							staged[i] = st
							continue
						}
					} else if len(probConf) > 1 {
						var exactProb []candWithConf
						for _, c := range probConf {
							if c.isExactAmount {
								exactProb = append(exactProb, c)
							}
						}
						if len(exactProb) == 1 {
							cand := exactProb[0]
							claimedExistingIDs[cand.id] = true
							st.DuplicateStatus = DuplicateStatusSettlementUpdate
							st.ResolutionAction = ResolutionActionApplySettlement
							st.MatchedExistingTxID = &cand.id
							staged[i] = st
							continue
						}
						st.DuplicateStatus = DuplicateStatusPotential
						st.ResolutionAction = ResolutionActionPending
						st.MatchedExistingTxID = &probConf[0].id
						staged[i] = st
						continue
					}

					// Case 3: Only ambiguous candidates exist -> NEVER silently commit as new!
					if len(ambigConf) > 0 {
						st.DuplicateStatus = DuplicateStatusPotential
						st.ResolutionAction = ResolutionActionPending
						st.MatchedExistingTxID = &ambigConf[0].id
						staged[i] = st
						continue
					}
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

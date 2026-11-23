package transaction

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/khaliullov/family-finance/internal/connector"
	"github.com/khaliullov/family-finance/internal/domain"
)

// PreserveRawPayload atomically stores verbatim bank response payloads in raw_bank_payloads.
// Deduplicates identical payloads within the same family tenant by payload_hash.
// Stores both queryable JSONB and verbatim BYTEA for byte-for-byte non-repudiation.
func PreserveRawPayload(
	ctx context.Context,
	tx pgx.Tx,
	familyID uuid.UUID,
	connectionID *uuid.UUID,
	source string,
	rawBytes []byte,
) (uuid.UUID, error) {
	if len(rawBytes) == 0 {
		return uuid.Nil, fmt.Errorf("cannot preserve empty raw payload")
	}

	// Enforce sanitization boundary: ensure payload is valid JSON before persisting
	if !json.Valid(rawBytes) {
		return uuid.Nil, fmt.Errorf("invalid json in raw payload: violates sanitization boundary")
	}

	payloadHash := ComputePayloadHash(rawBytes)

	// Check if identical payload already preserved for this family
	var existingID uuid.UUID
	err := tx.QueryRow(ctx, `
		SELECT id FROM public.raw_bank_payloads
		WHERE family_id = $1 AND payload_hash = $2
		LIMIT 1
	`, familyID, payloadHash).Scan(&existingID)

	if err == nil {
		return existingID, nil
	} else if err != pgx.ErrNoRows {
		return uuid.Nil, fmt.Errorf("failed to check existing raw payload: %w", err)
	}

	// Insert immutable raw payload preserving both queryable JSONB and verbatim BYTEA
	newID := uuid.New()
	_, err = tx.Exec(ctx, `
		INSERT INTO public.raw_bank_payloads (
			id, family_id, bank_connection_id, source, payload_hash, raw_json, raw_payload_bytes
		) VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, newID, familyID, connectionID, source, payloadHash, rawBytes, rawBytes)
	if err != nil {
		return uuid.Nil, fmt.Errorf("failed to insert raw bank payload: %w", err)
	}

	return newID, nil
}

// IngestBatch processes and persists a batch of normalized canonical bank transactions.
// Executes multi-priority deduplication (ExternalID -> ExternalAuthID -> Synthetic Fingerprint),
// lifecycle transitions, and provisions the default unsplit accounting entry in the same transaction.
func IngestBatch(
	ctx context.Context,
	tx pgx.Tx,
	familyID uuid.UUID,
	rawPayloadID *uuid.UUID,
	transactions []connector.CanonicalBankTransaction,
	accountMap map[string]uuid.UUID,
	cardMap map[string]uuid.UUID,
	ownerUserID uuid.UUID,
) (*SyncStats, error) {
	stats := &SyncStats{}
	batchFingerprintCount := make(map[string]int)

	for _, dto := range transactions {
		// 1. Resolve Account instrument
		accountID, ok := accountMap[dto.AccountExternalID]
		if !ok {
			return nil, fmt.Errorf("%w: account external id %q", ErrAccountNotFound, dto.AccountExternalID)
		}

		// 2. Resolve Card instrument if referenced
		var cardID *uuid.UUID
		if dto.CardExternalID != nil && strings.TrimSpace(*dto.CardExternalID) != "" {
			if cid, exists := cardMap[*dto.CardExternalID]; exists {
				cardID = &cid
			}
		}

		// 3. Normalize amounts, currency, and signed accounting convention
		opType := strings.TrimSpace(dto.OperationType)
		if opType == "" {
			opType = "purchase"
		}

		accountCurrency := strings.ToUpper(strings.TrimSpace(dto.AccountAmount.Currency))
		if accountCurrency == "" {
			accountCurrency = strings.ToUpper(strings.TrimSpace(dto.Amount.Currency))
		}
		nominalCurrency := strings.ToUpper(strings.TrimSpace(dto.Amount.Currency))
		if nominalCurrency == "" {
			nominalCurrency = accountCurrency
		}

		nominalMinor := NormalizeSignedAmount(opType, dto.Amount.MinorUnits)
		accountMinor := NormalizeSignedAmount(opType, dto.AccountAmount.MinorUnits)

		if dto.TransactionTime.IsZero() {
			return nil, fmt.Errorf("%w: zero transaction time", ErrInvalidTransactionData)
		}

		bookingStatus := domain.BookingStatus(strings.ToLower(strings.TrimSpace(dto.BookingStatus)))
		if !bookingStatus.IsValid() {
			bookingStatus = domain.StatusPosted
		}

		var extIDPtr *string
		if strings.TrimSpace(dto.ExternalID) != "" {
			trimmed := strings.TrimSpace(dto.ExternalID)
			extIDPtr = &trimmed
		}

		var extAuthIDPtr *string
		if dto.ExternalAuthID != nil && strings.TrimSpace(*dto.ExternalAuthID) != "" {
			trimmed := strings.TrimSpace(*dto.ExternalAuthID)
			extAuthIDPtr = &trimmed
		}

		// 4. Deduplication Priority 1: Match by ExternalID
		if extIDPtr != nil {
			var existingBT domain.BankTransaction
			var existingAmountMinor, existingAccountMinor int64
			var existingCur, existingAccCur string
			var existingStatus string

			err := tx.QueryRow(ctx, `
				SELECT id, family_id, account_id, card_id, raw_payload_id,
				       external_id, external_auth_id, fingerprint_hash, fingerprint_sequence,
				       amount_minor, currency, account_amount_minor, account_currency,
				       booking_status, operation_type, transaction_time, posted_time,
				       merchant_name, raw_description, mcc, bank_category
				FROM public.bank_transactions
				WHERE family_id = $1 AND account_id = $2 AND external_id = $3
				FOR UPDATE
			`, familyID, accountID, *extIDPtr).Scan(
				&existingBT.ID, &existingBT.FamilyID, &existingBT.AccountID, &existingBT.CardID, &existingBT.RawPayloadID,
				&existingBT.ExternalID, &existingBT.ExternalAuthID, &existingBT.FingerprintHash, &existingBT.FingerprintSequence,
				&existingAmountMinor, &existingCur, &existingAccountMinor, &existingAccCur,
				&existingStatus, &existingBT.OperationType, &existingBT.TransactionTime, &existingBT.PostedTime,
				&existingBT.MerchantName, &existingBT.RawDescription, &existingBT.MCC, &existingBT.BankCategory,
			)

			if err == nil {
				existingBT.Amount = domain.MustNewMoney(existingAmountMinor, existingCur)
				existingBT.AccountAmount = domain.MustNewMoney(existingAccountMinor, existingAccCur)
				existingBT.BookingStatus = domain.BookingStatus(existingStatus)

				// Already in final immutable state (posted or cancelled) -> skip and check discrepancies
				if existingBT.IsTerminal() {
					stats.TransactionsSkipped++
					var disc []ReconciliationDiscrepancy
					if existingBT.AccountAmount.AmountMinor() != accountMinor {
						disc = append(disc, ReconciliationDiscrepancy{
							ExternalID:    *extIDPtr,
							TransactionID: existingBT.ID,
							Field:         "amount",
							RecordedValue: fmt.Sprintf("%d", existingBT.AccountAmount.AmountMinor()),
							IncomingValue: fmt.Sprintf("%d", accountMinor),
							DetectedAt:    time.Now().UTC(),
						})
					}
					if existingBT.AccountAmount.Currency() != accountCurrency {
						disc = append(disc, ReconciliationDiscrepancy{
							ExternalID:    *extIDPtr,
							TransactionID: existingBT.ID,
							Field:         "currency",
							RecordedValue: existingBT.AccountAmount.Currency(),
							IncomingValue: accountCurrency,
							DetectedAt:    time.Now().UTC(),
						})
					}
					if strings.TrimSpace(existingBT.RawDescription) != strings.TrimSpace(dto.RawDescription) {
						disc = append(disc, ReconciliationDiscrepancy{
							ExternalID:    *extIDPtr,
							TransactionID: existingBT.ID,
							Field:         "description",
							RecordedValue: existingBT.RawDescription,
							IncomingValue: dto.RawDescription,
							DetectedAt:    time.Now().UTC(),
						})
					}
					if dto.SettledTime != nil && !dto.SettledTime.IsZero() && existingBT.PostedTime != nil && !existingBT.PostedTime.Equal(*dto.SettledTime) {
						disc = append(disc, ReconciliationDiscrepancy{
							ExternalID:    *extIDPtr,
							TransactionID: existingBT.ID,
							Field:         "settled_time",
							RecordedValue: existingBT.PostedTime.Format(time.RFC3339),
							IncomingValue: dto.SettledTime.Format(time.RFC3339),
							DetectedAt:    time.Now().UTC(),
						})
					}
					if len(disc) > 0 {
						stats.DiscrepanciesDetected += len(disc)
						stats.Discrepancies = append(stats.Discrepancies, disc...)
						slog.WarnContext(ctx, "Provider attempted modification of immutable terminal transaction without separate correction event",
							"external_id", *extIDPtr,
							"transaction_id", existingBT.ID,
							"discrepancies_count", len(disc),
						)
					}
					continue
				}

				// Pending hold transitioning to posted settlement
				if existingBT.BookingStatus == domain.StatusPending && bookingStatus == domain.StatusPosted {
					settledTime := dto.TransactionTime
					if dto.SettledTime != nil && !dto.SettledTime.IsZero() {
						settledTime = *dto.SettledTime
					}
					settledAmount := domain.MustNewMoney(accountMinor, accountCurrency)
					if err := TransitionPendingToPosted(ctx, tx, familyID, existingBT, settledTime, settledAmount, extIDPtr); err != nil {
						return nil, fmt.Errorf("failed to settle pending transaction %s: %w", existingBT.ID, err)
					}
					stats.TransactionsUpdated++
					continue
				}

				// Pending hold cancelled/voided
				if existingBT.BookingStatus == domain.StatusPending && bookingStatus == domain.StatusCancelled {
					if err := TransitionPendingToCancelled(ctx, tx, familyID, existingBT); err != nil {
						return nil, fmt.Errorf("failed to cancel pending transaction %s: %w", existingBT.ID, err)
					}
					stats.TransactionsUpdated++
					continue
				}

				// Still pending, no state change
				stats.TransactionsSkipped++
				continue
			} else if err != pgx.ErrNoRows {
				return nil, fmt.Errorf("failed to query existing transaction by external_id: %w", err)
			}
		}

		// 5. Deduplication Priority 2: Match by ExternalAuthID (Hold Settlement)
		if extAuthIDPtr != nil {
			var existingBT domain.BankTransaction
			var existingAmountMinor, existingAccountMinor int64
			var existingCur, existingAccCur string
			var existingStatus string

			err := tx.QueryRow(ctx, `
				SELECT id, family_id, account_id, card_id, raw_payload_id,
				       external_id, external_auth_id, fingerprint_hash, fingerprint_sequence,
				       amount_minor, currency, account_amount_minor, account_currency,
				       booking_status, operation_type, transaction_time, posted_time,
				       merchant_name, raw_description, mcc, bank_category
				FROM public.bank_transactions
				WHERE family_id = $1 AND account_id = $2 AND external_auth_id = $3 AND booking_status = 'pending'
				FOR UPDATE
				LIMIT 1
			`, familyID, accountID, *extAuthIDPtr).Scan(
				&existingBT.ID, &existingBT.FamilyID, &existingBT.AccountID, &existingBT.CardID, &existingBT.RawPayloadID,
				&existingBT.ExternalID, &existingBT.ExternalAuthID, &existingBT.FingerprintHash, &existingBT.FingerprintSequence,
				&existingAmountMinor, &existingCur, &existingAccountMinor, &existingAccCur,
				&existingStatus, &existingBT.OperationType, &existingBT.TransactionTime, &existingBT.PostedTime,
				&existingBT.MerchantName, &existingBT.RawDescription, &existingBT.MCC, &existingBT.BankCategory,
			)

			if err == nil {
				existingBT.Amount = domain.MustNewMoney(existingAmountMinor, existingCur)
				existingBT.AccountAmount = domain.MustNewMoney(existingAccountMinor, existingAccCur)
				existingBT.BookingStatus = domain.BookingStatus(existingStatus)

				if bookingStatus == domain.StatusPosted {
					settledTime := dto.TransactionTime
					if dto.SettledTime != nil && !dto.SettledTime.IsZero() {
						settledTime = *dto.SettledTime
					}
					settledAmount := domain.MustNewMoney(accountMinor, accountCurrency)
					if err := TransitionPendingToPosted(ctx, tx, familyID, existingBT, settledTime, settledAmount, extIDPtr); err != nil {
						return nil, fmt.Errorf("failed to settle pending hold by auth id: %w", err)
					}
					stats.TransactionsUpdated++
					continue
				} else if bookingStatus == domain.StatusCancelled {
					if err := TransitionPendingToCancelled(ctx, tx, familyID, existingBT); err != nil {
						return nil, fmt.Errorf("failed to cancel pending hold by auth id: %w", err)
					}
					stats.TransactionsUpdated++
					continue
				}

				stats.TransactionsSkipped++
				continue
			} else if err != pgx.ErrNoRows {
				return nil, fmt.Errorf("failed to query pending hold by auth id: %w", err)
			}
		}

		// 6. Deduplication Priority 3: Synthetic Fingerprint & Sequence Disambiguation
		fpHash := ComputeFingerprint(accountID, dto.TransactionTime, accountMinor, accountCurrency, dto.RawDescription)
		batchFingerprintCount[fpHash]++
		batchSeq := batchFingerprintCount[fpHash]

		var dbMaxSeq int
		err := tx.QueryRow(ctx, `
			SELECT COALESCE(MAX(fingerprint_sequence), 0)
			FROM public.bank_transactions
			WHERE family_id = $1 AND account_id = $2 AND fingerprint_hash = $3
		`, familyID, accountID, fpHash).Scan(&dbMaxSeq)
		if err != nil {
			return nil, fmt.Errorf("failed to query fingerprint sequence: %w", err)
		}

		// If this batch sequence count already exists in database, skip as duplicate
		if batchSeq <= dbMaxSeq {
			stats.TransactionsSkipped++
			continue
		}

		// New transaction: allocate sequence
		txSeq := dbMaxSeq + 1

		// 7. Insert new BankTransaction
		newTxID := uuid.New()
		var postedTime *time.Time
		if bookingStatus == domain.StatusPosted {
			if dto.SettledTime != nil && !dto.SettledTime.IsZero() {
				postedTime = dto.SettledTime
			} else {
				tTime := dto.TransactionTime
				postedTime = &tTime
			}
		}

		var merchantName *string
		if strings.TrimSpace(dto.MerchantName) != "" {
			trimmed := strings.TrimSpace(dto.MerchantName)
			merchantName = &trimmed
		}
		var bankCat *string
		if strings.TrimSpace(dto.BankCategory) != "" {
			trimmed := strings.TrimSpace(dto.BankCategory)
			bankCat = &trimmed
		}

		rawDesc := strings.TrimSpace(dto.RawDescription)
		if rawDesc == "" {
			rawDesc = "UNSPECIFIED"
		}

		var insertedTxID uuid.UUID
		var insertQuery string

		if extIDPtr != nil {
			insertQuery = `
				INSERT INTO public.bank_transactions (
					id, family_id, account_id, card_id, raw_payload_id,
					external_id, external_auth_id, fingerprint_hash, fingerprint_sequence,
					amount_minor, currency, account_amount_minor, account_currency,
					booking_status, operation_type, transaction_time, posted_time,
					merchant_name, raw_description, mcc, bank_category
				) VALUES (
					$1, $2, $3, $4, $5,
					$6, $7, $8, $9,
					$10, $11, $12, $13,
					$14, $15, $16, $17,
					$18, $19, $20, $21
				)
				ON CONFLICT (family_id, account_id, external_id)
				DO NOTHING
				RETURNING id
			`
		} else {
			insertQuery = `
				INSERT INTO public.bank_transactions (
					id, family_id, account_id, card_id, raw_payload_id,
					external_id, external_auth_id, fingerprint_hash, fingerprint_sequence,
					amount_minor, currency, account_amount_minor, account_currency,
					booking_status, operation_type, transaction_time, posted_time,
					merchant_name, raw_description, mcc, bank_category
				) VALUES (
					$1, $2, $3, $4, $5,
					$6, $7, $8, $9,
					$10, $11, $12, $13,
					$14, $15, $16, $17,
					$18, $19, $20, $21
				)
				ON CONFLICT (family_id, account_id, fingerprint_hash, fingerprint_sequence)
				DO NOTHING
				RETURNING id
			`
		}

		err = tx.QueryRow(ctx, insertQuery,
			newTxID, familyID, accountID, cardID, rawPayloadID,
			extIDPtr, extAuthIDPtr, fpHash, txSeq,
			nominalMinor, nominalCurrency, accountMinor, accountCurrency,
			bookingStatus, opType, dto.TransactionTime, postedTime,
			merchantName, rawDesc, dto.MCC, bankCat,
		).Scan(&insertedTxID)

		if errors.Is(err, pgx.ErrNoRows) {
			// Row already inserted by a concurrent transaction
			stats.TransactionsSkipped++
			continue
		} else if err != nil {
			return nil, fmt.Errorf("failed to insert bank_transaction: %w", err)
		}

		// 8. Provision default unsplit AccountingEntry
		// Satisfies trigger trg_bank_tx_split_integrity_deferred
		var classification string
		switch {
		case accountMinor < 0:
			classification = "expense"
		case accountMinor > 0:
			classification = "income"
		default:
			classification = "excluded"
		}

		entryID := uuid.New()
		_, err = tx.Exec(ctx, `
			INSERT INTO public.accounting_entries (
				id, family_id, bank_transaction_id, account_id,
				amount_minor, currency, classification,
				payer_user_id, beneficiary_type, entry_time,
				is_split, is_manual
			) VALUES (
				$1, $2, $3, $4,
				$5, $6, $7,
				$8, 'household', $9,
				FALSE, FALSE
			)
		`,
			entryID, familyID, newTxID, accountID,
			accountMinor, accountCurrency, classification,
			ownerUserID, dto.TransactionTime,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to insert default accounting entry: %w", err)
		}

		stats.TransactionsIngested++
	}

	return stats, nil
}

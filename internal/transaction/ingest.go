package transaction

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
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

// PreserveRawPayload atomically stores sanitized bank response payloads in raw_bank_payloads.
// Deduplicates identical payloads within the same family tenant and bank connection by payload_hash
// using PostgreSQL-level atomic INSERT ... ON CONFLICT DO NOTHING with NULLS NOT DISTINCT uniqueness.
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

	// Enforce sanitization boundary: redact credentials, tokens, cookies, auth headers, and full PANs
	sanitizedBytes, err := SanitizeRawPayload(rawBytes)
	if err != nil {
		return uuid.Nil, fmt.Errorf("sanitization boundary violation: %w", err)
	}

	payloadHash := ComputePayloadHash(sanitizedBytes)

	// Atomically insert using ON CONFLICT DO NOTHING against unique index
	// uq_raw_bank_payloads_conn_hash (NULLS NOT DISTINCT).
	// Under high concurrency, if another transaction inserted the row, ON CONFLICT DO NOTHING
	// serializes on the index lock until the other transaction commits, returning 0 rows.
	// We then select the committed canonical ID.
	for attempt := 0; attempt < 3; attempt++ {
		newID := uuid.New()
		var insertedID uuid.UUID
		err := tx.QueryRow(ctx, `
			INSERT INTO public.raw_bank_payloads (
				id, family_id, bank_connection_id, source, payload_hash, raw_json, raw_payload_bytes
			) VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (family_id, bank_connection_id, payload_hash) DO NOTHING
			RETURNING id
		`, newID, familyID, connectionID, source, payloadHash, sanitizedBytes, sanitizedBytes).Scan(&insertedID)
		if err == nil {
			return insertedID, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, fmt.Errorf("failed to atomically insert raw bank payload: %w", err)
		}

		var existingID uuid.UUID
		err = tx.QueryRow(ctx, `
			SELECT id FROM public.raw_bank_payloads
			WHERE family_id = $1 AND bank_connection_id IS NOT DISTINCT FROM $2 AND payload_hash = $3
			LIMIT 1
		`, familyID, connectionID, payloadHash).Scan(&existingID)
		if err == nil {
			return existingID, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, fmt.Errorf("failed to query existing canonical raw bank payload: %w", err)
		}
	}

	return uuid.Nil, fmt.Errorf("exhausted attempts to atomically preserve raw bank payload")
}

// IngestBatch processes and persists a batch of normalized canonical bank transactions.
// Executes multi-priority deduplication (ExternalID -> ExternalAuthID -> Synthetic Fingerprint),
// lifecycle transitions, and provisions the default unsplit accounting entry in the same transaction.
func IngestBatch(
	ctx context.Context,
	tx pgx.Tx,
	familyID uuid.UUID,
	connectionID *uuid.UUID,
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

		nominalMinor, err := NormalizeSignedAmount(opType, dto.Amount.MinorUnits)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid nominal amount: %v", ErrInvalidTransactionData, err)
		}
		accountMinor, err := NormalizeSignedAmount(opType, dto.AccountAmount.MinorUnits)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid account amount: %v", ErrInvalidTransactionData, err)
		}

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
					now := time.Now().UTC()
					if existingBT.AccountAmount.AmountMinor() != accountMinor {
						disc = append(disc, ReconciliationDiscrepancy{
							FamilyID:          familyID,
							BankConnectionID:  connectionID,
							BankTransactionID: &existingBT.ID,
							ExternalID:        *extIDPtr,
							TransactionID:     existingBT.ID,
							Field:             "amount",
							RecordedValue:     SanitizeDiscrepancyValue(fmt.Sprintf("%d", existingBT.AccountAmount.AmountMinor())),
							IncomingValue:     SanitizeDiscrepancyValue(fmt.Sprintf("%d", accountMinor)),
							DetectedAt:        now,
						})
					}
					if existingBT.AccountAmount.Currency() != accountCurrency {
						disc = append(disc, ReconciliationDiscrepancy{
							FamilyID:          familyID,
							BankConnectionID:  connectionID,
							BankTransactionID: &existingBT.ID,
							ExternalID:        *extIDPtr,
							TransactionID:     existingBT.ID,
							Field:             "currency",
							RecordedValue:     SanitizeDiscrepancyValue(existingBT.AccountAmount.Currency()),
							IncomingValue:     SanitizeDiscrepancyValue(accountCurrency),
							DetectedAt:        now,
						})
					}
					if strings.TrimSpace(existingBT.RawDescription) != strings.TrimSpace(dto.RawDescription) {
						disc = append(disc, ReconciliationDiscrepancy{
							FamilyID:          familyID,
							BankConnectionID:  connectionID,
							BankTransactionID: &existingBT.ID,
							ExternalID:        *extIDPtr,
							TransactionID:     existingBT.ID,
							Field:             "description",
							RecordedValue:     SanitizeDiscrepancyValue(existingBT.RawDescription),
							IncomingValue:     SanitizeDiscrepancyValue(dto.RawDescription),
							DetectedAt:        now,
						})
					}
					if dto.SettledTime != nil && !dto.SettledTime.IsZero() && existingBT.PostedTime != nil && !existingBT.PostedTime.Equal(*dto.SettledTime) {
						disc = append(disc, ReconciliationDiscrepancy{
							FamilyID:          familyID,
							BankConnectionID:  connectionID,
							BankTransactionID: &existingBT.ID,
							ExternalID:        *extIDPtr,
							TransactionID:     existingBT.ID,
							Field:             "settled_time",
							RecordedValue:     SanitizeDiscrepancyValue(existingBT.PostedTime.Format(time.RFC3339)),
							IncomingValue:     SanitizeDiscrepancyValue(dto.SettledTime.Format(time.RFC3339)),
							DetectedAt:        now,
						})
					}
					if existingBT.BookingStatus != bookingStatus {
						disc = append(disc, ReconciliationDiscrepancy{
							FamilyID:          familyID,
							BankConnectionID:  connectionID,
							BankTransactionID: &existingBT.ID,
							ExternalID:        *extIDPtr,
							TransactionID:     existingBT.ID,
							Field:             "booking_status",
							RecordedValue:     SanitizeDiscrepancyValue(string(existingBT.BookingStatus)),
							IncomingValue:     SanitizeDiscrepancyValue(string(bookingStatus)),
							DetectedAt:        now,
						})
					}
					if len(disc) > 0 {
						for i := range disc {
							hash := ComputeDiscrepancyIdempotencyHash(
								familyID, existingBT.ID, disc[i].Field, disc[i].RecordedValue, disc[i].IncomingValue,
							)
							disc[i].IdempotencyHash = hash

							_, err := tx.Exec(ctx, `
								INSERT INTO public.reconciliation_discrepancies (
									family_id, bank_connection_id, bank_transaction_id,
									external_id, field, recorded_value, incoming_value,
									idempotency_hash, detected_at
								) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
								ON CONFLICT (family_id, idempotency_hash) DO NOTHING
							`, familyID, connectionID, existingBT.ID, disc[i].ExternalID, disc[i].Field, disc[i].RecordedValue, disc[i].IncomingValue, hash, disc[i].DetectedAt)
							if err != nil {
								return nil, fmt.Errorf("failed to persist reconciliation discrepancy: %w", err)
							}
						}

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
		err = tx.QueryRow(ctx, `
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

// ComputeDiscrepancyIdempotencyHash computes an unambiguous, collision-free SHA-256 hash
// using length-prefixed binary encoding over the 5 logical idempotency dimensions:
// family_id, transaction_id, field, recorded_value, incoming_value.
// This prevents delimiter-based collisions (e.g. strings containing colons or JSON structures).
func ComputeDiscrepancyIdempotencyHash(familyID, transactionID uuid.UUID, field, recordedValue, incomingValue string) string {
	h := sha256.New()
	h.Write(familyID[:])
	h.Write(transactionID[:])
	writeLengthPrefixed := func(s string) {
		var lenBuf [4]byte
		binary.BigEndian.PutUint32(lenBuf[:], uint32(len(s)))
		h.Write(lenBuf[:])
		h.Write([]byte(s))
	}
	writeLengthPrefixed(field)
	writeLengthPrefixed(recordedValue)
	writeLengthPrefixed(incomingValue)
	return fmt.Sprintf("%x", h.Sum(nil))
}

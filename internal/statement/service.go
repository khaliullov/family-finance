package statement

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/khaliullov/family-finance/internal/crypto"
	"github.com/khaliullov/family-finance/internal/database"
	"github.com/khaliullov/family-finance/internal/domain"
)

// PromotionResult encapsulates statistics of an atomic batch ledger commit.
type PromotionResult struct {
	BatchID                uuid.UUID  `json:"batch_id"`
	TransactionsCommitted  int        `json:"transactions_committed"`
	TransactionsSettled    int        `json:"transactions_settled"`
	TransactionsSkipped    int        `json:"transactions_skipped"`
	BalanceSnapshotUpdated bool       `json:"balance_snapshot_updated"`
	NewAccountBalanceMinor *int64     `json:"new_account_balance_minor,omitempty"`
	BalanceUpdatedAt       *time.Time `json:"balance_updated_at,omitempty"`
}

// Service orchestrates generic statement document storage, staging, and atomic ledger promotion.
type Service struct {
	pool      *pgxpool.Pool
	repo      *Repository
	cryptoKey []byte
	parsers   *ParserRegistry
}

func NewService(pool *pgxpool.Pool, cryptoKey []byte, parsers *ParserRegistry) *Service {
	return &Service{
		pool:      pool,
		repo:      NewRepository(),
		cryptoKey: cryptoKey,
		parsers:   parsers,
	}
}

// UploadAndStage validates the uploaded document, encrypts and stores it, executes format parsing,
// evaluates balance reconciliation, executes deduplication matching, and stages transactions in the buffer.
func (s *Service) UploadAndStage(
	ctx context.Context,
	familyID, userID, accountID uuid.UUID,
	filename string,
	r io.Reader,
	sizeBytes int64,
	parser StatementParser,
) (*StatementImportBatch, error) {
	if familyID == uuid.Nil || userID == uuid.Nil || accountID == uuid.Nil {
		return nil, ErrTenantContextMismatch
	}
	if sizeBytes <= 0 || sizeBytes > 10*1024*1024 {
		return nil, ErrFileTooLarge
	}
	// Read bounded content
	rawBytes, err := io.ReadAll(io.LimitReader(r, 10*1024*1024+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read statement content: %w", err)
	}
	if int64(len(rawBytes)) > 10*1024*1024 {
		return nil, ErrFileTooLarge
	}

	// Auto-sniff parser if not explicitly provided
	if parser == nil {
		if s.parsers == nil {
			return nil, ErrParserNotFound
		}
		headerLen := 4096
		if len(rawBytes) < headerLen {
			headerLen = len(rawBytes)
		}
		sniffed, ok := s.parsers.Sniff(rawBytes[:headerLen])
		if !ok {
			return nil, ErrParserNotFound
		}
		parser = sniffed
	}

	hashBytes := sha256.Sum256(rawBytes)
	contentHash := hex.EncodeToString(hashBytes[:])

	docID := uuid.New()
	batchID := uuid.New()

	// Encrypt using STMT envelope with AAD tenant binding
	encryptedBytes, err := crypto.EncryptStatementDocument(s.cryptoKey, familyID, docID, rawBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to encrypt statement document: %w", err)
	}

	var batch StatementImportBatch

	err = database.WithFamilyUserTx(ctx, s.pool, familyID, userID, func(tx pgx.Tx) error {
		// 1. Acquire two-slot concurrency lock for family
		_, err := AcquireFamilyImportSlot(ctx, tx, familyID)
		if err != nil {
			return err
		}

		// 2. Verify account exists and lock row, retrieving selector criteria
		var accountCurrency, accountName, accountType string
		var accountExtID *string
		var isArchived bool
		err = tx.QueryRow(ctx, `
			SELECT currency, name, type, external_id, is_archived
			FROM public.accounts
			WHERE family_id = $1 AND id = $2
			FOR UPDATE
		`, familyID, accountID).Scan(&accountCurrency, &accountName, &accountType, &accountExtID, &isArchived)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("target account not found: %w", pgx.ErrNoRows)
			}
			return fmt.Errorf("failed to query account: %w", err)
		}
		if isArchived {
			return fmt.Errorf("cannot import statement into an archived account")
		}

		// Retrieve registered card masks for account to aid deterministic filtering
		cardRows, err := tx.Query(ctx, `
			SELECT masked_pan FROM public.cards
			WHERE family_id = $1 AND account_id = $2
		`, familyID, accountID)
		var cardMasks []string
		if err == nil {
			for cardRows.Next() {
				var pan string
				if err := cardRows.Scan(&pan); err == nil {
					cardMasks = append(cardMasks, pan)
				}
			}
			cardRows.Close()
		}

		extIDStr := ""
		if accountExtID != nil {
			extIDStr = *accountExtID
		}

		selector := AccountSelector{
			AccountID:   accountID,
			AccountName: accountName,
			AccountType: accountType,
			Currency:    accountCurrency,
			ExternalID:  extIDStr,
			CardMasks:   cardMasks,
		}
		parseCtx := WithAccountSelector(ctx, selector)

		// 3. Insert encrypted document record
		doc := StatementDocument{
			ID:               docID,
			FamilyID:         familyID,
			UploadedByUserID: userID,
			OriginalFilename: filename,
			FileSizeBytes:    int64(len(rawBytes)),
			ContentHash:      contentHash,
			MIMEType:         "text/csv",
			EncryptedContent: encryptedBytes,
			CreatedAt:        time.Now().UTC(),
		}
		if err := s.repo.InsertDocument(ctx, tx, &doc); err != nil {
			return err
		}

		// 4. Parse document using resolved parser
		parsed, err := parser.Parse(parseCtx, bytes.NewReader(rawBytes), accountCurrency)
		if err != nil {
			return fmt.Errorf("parser failed: %w", err)
		}
		if len(parsed.Rows) > 50000 {
			return ErrTooManyRows
		}

		// 5. Evaluate balance reconciliation
		recon, err := EvaluateReconciliation(
			accountCurrency,
			parsed.PeriodStartDate, parsed.PeriodEndDate,
			parsed.OpeningBalanceMinor, parsed.ClosingBalanceMinor,
			parsed.Rows,
		)
		if err != nil {
			return fmt.Errorf("failed to evaluate reconciliation: %w", err)
		}

		// 6. Match staged rows against database
		stagedRows, err := MatchStagedRows(ctx, tx, familyID, accountID, batchID, parsed.Rows)
		if err != nil {
			return fmt.Errorf("deduplication matching failed: %w", err)
		}

		duplicateCount := 0
		discrepancyCount := 0
		if recon.Classification == ReconciliationDiscrepancy {
			discrepancyCount++
		}
		for _, row := range stagedRows {
			if row.DuplicateStatus == DuplicateStatusConfirmed {
				duplicateCount++
			}
			if row.DuplicateStatus == DuplicateStatusPotential {
				discrepancyCount++
			}
		}

		// 7. Insert statement import batch
		batch = StatementImportBatch{
			ID:                     batchID,
			FamilyID:               familyID,
			AccountID:              accountID,
			DocumentID:             docID,
			SourceFormat:           parser.Profile().FormatID,
			ParserVersion:          parsed.ParserVersion,
			Status:                 BatchStatusStaged,
			ReconciliationStatus:   recon.Classification,
			PeriodStartDate:        parsed.PeriodStartDate,
			PeriodEndDate:          parsed.PeriodEndDate,
			TotalRows:              len(parsed.Rows),
			StagedCount:            len(stagedRows),
			DuplicateCount:         duplicateCount,
			DiscrepancyCount:       discrepancyCount,
			OpeningBalanceMinor:    parsed.OpeningBalanceMinor,
			ClosingBalanceMinor:    parsed.ClosingBalanceMinor,
			CalculatedBalanceMinor: recon.CalculatedBalanceMinor,
			CreatedAt:              time.Now().UTC(),
			UpdatedAt:              time.Now().UTC(),
		}
		if err := s.repo.InsertBatch(ctx, tx, &batch); err != nil {
			return err
		}

		// 8. Insert staged rows buffer
		if err := s.repo.InsertStagedTransactions(ctx, tx, stagedRows); err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &batch, nil
}

// CreateReconciliationOverride records an attributable user rationale for approving a batch with a balance discrepancy.
func (s *Service) CreateReconciliationOverride(
	ctx context.Context,
	familyID, userID, batchID uuid.UUID,
	reason string,
) error {
	trimmedReason := strings.TrimSpace(reason)
	if len(trimmedReason) < 10 {
		return fmt.Errorf("override reason must be at least 10 characters")
	}

	return database.WithFamilyUserTx(ctx, s.pool, familyID, userID, func(tx pgx.Tx) error {
		batch, err := s.repo.GetBatchByID(ctx, tx, familyID, batchID)
		if err != nil {
			return err
		}

		if batch.Status != BatchStatusStaged {
			return fmt.Errorf("%w: batch is in status %s, expected %s", ErrInvalidBatchState, batch.Status, BatchStatusStaged)
		}
		if batch.ReconciliationStatus != ReconciliationDiscrepancy {
			return fmt.Errorf("cannot create override: batch reconciliation status is %s", batch.ReconciliationStatus)
		}

		opening := int64(0)
		if batch.OpeningBalanceMinor != nil {
			opening = *batch.OpeningBalanceMinor
		}
		closing := int64(0)
		if batch.ClosingBalanceMinor != nil {
			closing = *batch.ClosingBalanceMinor
		}
		calculated := int64(0)
		if batch.CalculatedBalanceMinor != nil {
			calculated = *batch.CalculatedBalanceMinor
		}
		delta, err := domain.SafeSub(closing, calculated)
		if err != nil {
			return fmt.Errorf("discrepancy delta calculation overflow: %w", err)
		}

		override := StatementReconciliationOverride{
			ID:                     uuid.New(),
			FamilyID:               familyID,
			BatchID:                batchID,
			OverriddenByUserID:     userID,
			OpeningBalanceMinor:    opening,
			ClosingBalanceMinor:    closing,
			CalculatedBalanceMinor: calculated,
			DiscrepancyDeltaMinor:  delta,
			Reason:                 trimmedReason,
			CreatedAt:              time.Now().UTC(),
		}

		return s.repo.InsertReconciliationOverride(ctx, tx, &override)
	})
}

// ResolveStagedTransaction records the user's explicit decision for an ambiguous duplicate candidate.
func (s *Service) ResolveStagedTransaction(
	ctx context.Context,
	familyID, stagedID uuid.UUID,
	action string,
) error {
	normAction := strings.ToLower(strings.TrimSpace(action))
	if normAction != ResolutionActionCommit && normAction != ResolutionActionSkipDuplicate {
		return fmt.Errorf("invalid resolution action: %s (must be 'commit' or 'skip_duplicate')", action)
	}

	return database.WithFamilyTx(ctx, s.pool, familyID, func(tx pgx.Tx) error {
		return s.repo.UpdateStagedResolution(ctx, tx, familyID, stagedID, normAction)
	})
}

// PromoteBatch atomically promotes approved staged transactions into the double-entry ledger.
//
// Invariants:
// 1. Strict atomic all-or-nothing execution in a single database transaction.
// 2. Batch row locked FOR UPDATE (idempotent, prevents concurrent duplicate approval).
// 3. Discrepancies require an attributable override record in statement_reconciliation_overrides.
// 4. Staged rows with resolution_action == 'pending' block promotion.
// 5. Pending holds are transitioned via SECURITY DEFINER settle_pending_bank_transaction.
// 6. New transactions insert bank_transactions + default unsplit accounting_entries.
// 7. Monotonic balance snapshot check: historical statements never overwrite newer current account balance.
func (s *Service) PromoteBatch(
	ctx context.Context,
	familyID, userID, batchID uuid.UUID,
) (*PromotionResult, error) {
	if familyID == uuid.Nil || userID == uuid.Nil || batchID == uuid.Nil {
		return nil, ErrTenantContextMismatch
	}

	var result PromotionResult
	result.BatchID = batchID

	err := database.WithFamilyUserTx(ctx, s.pool, familyID, userID, func(tx pgx.Tx) error {
		// 1. Lock batch row FOR UPDATE
		var status, reconStatus string
		var accountID uuid.UUID
		var closingBalanceMinor *int64
		var periodEndDate *time.Time

		err := tx.QueryRow(ctx, `
			SELECT status, reconciliation_status, account_id, closing_balance_minor, period_end_date
			FROM public.statement_import_batches
			WHERE family_id = $1 AND id = $2
			FOR UPDATE
		`, familyID, batchID).Scan(&status, &reconStatus, &accountID, &closingBalanceMinor, &periodEndDate)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrBatchNotFound
			}
			return fmt.Errorf("failed to query batch under lock: %w", err)
		}

		if status == BatchStatusCommitted {
			// Idempotent promotion: batch was already committed by another concurrent or prior request.
			// Return cleanly without duplicating any ledger entries or transitions.
			return nil
		}
		if status != BatchStatusStaged {
			return fmt.Errorf("%w: batch is in status %s, expected %s", ErrInvalidBatchState, status, BatchStatusStaged)
		}

		// 2. Reconciliation Discrepancy Gate
		if reconStatus == ReconciliationDiscrepancy {
			hasOverride, err := s.repo.HasReconciliationOverride(ctx, tx, familyID, batchID)
			if err != nil {
				return err
			}
			if !hasOverride {
				return ErrOverrideRequired
			}
		}

		// 3. Lock & Revalidate all staged rows
		stagedRows, err := s.repo.GetStagedTransactionsByBatch(ctx, tx, familyID, batchID)
		if err != nil {
			return err
		}

		for _, row := range stagedRows {
			if row.ResolutionAction == ResolutionActionPending {
				return fmt.Errorf("%w: staged row %d is pending review", ErrUnresolvedStagedRows, row.RowIndex)
			}
		}

		// 4. Lock Target Account FOR UPDATE
		var currentBalanceMinor *int64
		var balanceUpdatedAt *time.Time
		err = tx.QueryRow(ctx, `
			SELECT current_balance_minor, balance_updated_at
			FROM public.accounts
			WHERE family_id = $1 AND id = $2
			FOR UPDATE
		`, familyID, accountID).Scan(&currentBalanceMinor, &balanceUpdatedAt)
		if err != nil {
			return fmt.Errorf("failed to lock target account: %w", err)
		}

		// 5. Promote Staged Rows
		for _, row := range stagedRows {
			// Strict safeguard: cancelled operations must never be promoted to the financial ledger
			if row.BookingStatus == "cancelled" {
				result.TransactionsSkipped++
				continue
			}

			switch row.ResolutionAction {
			case ResolutionActionCommit:
				newTxID := uuid.New()
				var postedTime *time.Time
				if row.BookingStatus == "posted" {
					if row.SettledTime != nil && !row.SettledTime.IsZero() {
						postedTime = row.SettledTime
					} else {
						tTime := row.TransactionTime
						postedTime = &tTime
					}
				}

				// Allocate fingerprint sequence to support multiple identical transactions without collision
				var dbMaxSeq int
				err = tx.QueryRow(ctx, `
					SELECT COALESCE(MAX(fingerprint_sequence), 0)
					FROM public.bank_transactions
					WHERE family_id = $1 AND account_id = $2 AND fingerprint_hash = $3
				`, familyID, accountID, row.ImportFingerprint).Scan(&dbMaxSeq)
				if err != nil {
					return fmt.Errorf("failed to query fingerprint sequence for row %d: %w", row.RowIndex, err)
				}
				txSeq := dbMaxSeq + 1

				// Insert bank_transactions
				_, err = tx.Exec(ctx, `
					INSERT INTO public.bank_transactions (
						id, family_id, account_id, external_id, fingerprint_hash, fingerprint_sequence,
						amount_minor, currency, account_amount_minor, account_currency,
						booking_status, operation_type, transaction_time, posted_time,
						merchant_name, raw_description, mcc, bank_category
					) VALUES (
						$1, $2, $3, $4, $5, $6,
						$7, $8, $9, $10,
						$11, $12, $13, $14,
						$15, $16, $17, $18
					)
				`,
					newTxID, familyID, accountID, row.SourceExternalID, row.ImportFingerprint, txSeq,
					row.AmountMinor, row.Currency, row.AccountAmountMinor, row.AccountCurrency,
					row.BookingStatus, row.OperationType, row.TransactionTime, postedTime,
					row.MerchantName, row.RawDescription, row.MCC, row.BankCategory,
				)
				if err != nil {
					return fmt.Errorf("failed to commit bank_transaction for row %d: %w", row.RowIndex, err)
				}

				// Insert default unsplit accounting_entries (satisfies trg_bank_tx_split_integrity_deferred)
				var classification string
				switch {
				case row.OperationType == "transfer_out" || row.OperationType == "transfer_in":
					classification = "transfer"
				case row.OperationType == "card_check":
					classification = "excluded"
				case row.AccountAmountMinor < 0:
					classification = "expense"
				case row.AccountAmountMinor > 0:
					classification = "income"
				default:
					classification = "excluded"
				}

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
					uuid.New(), familyID, newTxID, accountID,
					row.AccountAmountMinor, row.AccountCurrency, classification,
					userID, row.TransactionTime,
				)
				if err != nil {
					return fmt.Errorf("failed to insert accounting_entry for row %d: %w", row.RowIndex, err)
				}
				result.TransactionsCommitted++

			case ResolutionActionApplySettlement:
				if row.MatchedExistingTxID == nil {
					return fmt.Errorf("cannot apply settlement for row %d: missing matched existing transaction ID", row.RowIndex)
				}
				settledTime := row.TransactionTime
				if row.SettledTime != nil && !row.SettledTime.IsZero() {
					settledTime = *row.SettledTime
				}

				// Execute atomic transition via SECURITY DEFINER routine
				_, err := tx.Exec(ctx, `
					SELECT public.settle_pending_bank_transaction($1, $2, $3, $4, $5, $6)
				`, familyID, accountID, *row.MatchedExistingTxID, settledTime, row.AccountAmountMinor, row.SourceExternalID)
				if err != nil {
					return fmt.Errorf("failed to settle pending transaction for row %d: %w", row.RowIndex, err)
				}
				result.TransactionsSettled++

			case ResolutionActionSkipDuplicate:
				result.TransactionsSkipped++
			}
		}

		// 6. Monotonic Account Balance Snapshot Update
		// Rule: Only update current_balance_minor if statement is reconciled, closing balance is non-nil,
		// and period_end_date is strictly newer than current balance_updated_at (or if balance is null).
		if reconStatus == ReconciliationReconciled && closingBalanceMinor != nil && periodEndDate != nil {
			if balanceUpdatedAt == nil || periodEndDate.After(*balanceUpdatedAt) {
				_, err := tx.Exec(ctx, `
					UPDATE public.accounts
					SET current_balance_minor = $1,
					    balance_updated_at = $2,
					    balance_source = 'statement_file',
					    updated_at = NOW()
					WHERE family_id = $3 AND id = $4
				`, *closingBalanceMinor, *periodEndDate, familyID, accountID)
				if err != nil {
					return fmt.Errorf("failed to update account balance snapshot: %w", err)
				}
				result.BalanceSnapshotUpdated = true
				result.NewAccountBalanceMinor = closingBalanceMinor
				result.BalanceUpdatedAt = periodEndDate
			}
		}

		// 7. Update Batch Status to Committed
		now := time.Now().UTC()
		_, err = tx.Exec(ctx, `
			UPDATE public.statement_import_batches
			SET status = $1,
			    approved_by_user_id = $2,
			    approved_at = $3,
			    updated_at = $3
			WHERE family_id = $4 AND id = $5
		`, BatchStatusCommitted, userID, now, familyID, batchID)
		if err != nil {
			return fmt.Errorf("failed to finalize batch status: %w", err)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &result, nil
}

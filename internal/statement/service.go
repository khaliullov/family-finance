package statement

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/khaliullov/family-finance/internal/crypto"
	"github.com/khaliullov/family-finance/internal/database"
)

// PromotionResult encapsulates statistics of an atomic batch ledger commit.
type PromotionResult struct {
	BatchID                uuid.UUID  `json:"batch_id"`
	TransactionsCommitted  int        `json:"transactions_committed"`
	TransactionsSettled    int        `json:"transactions_settled"`
	TransactionsCancelled  int        `json:"transactions_cancelled"`
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

// RecordAuditEvent logs a structured, non-repudiation audit event to public.audit_logs.
func recordAuditEvent(ctx context.Context, tx DBTX, familyID uuid.UUID, userID *uuid.UUID, entityType string, entityID uuid.UUID, action string, oldValues, newValues any) error {
	var oldJSON, newJSON []byte
	if oldValues != nil {
		oldJSON, _ = json.Marshal(oldValues)
	}
	if newValues != nil {
		newJSON, _ = json.Marshal(newValues)
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO public.audit_logs (
			id, family_id, user_id, entity_type, entity_id, action, old_values, new_values, created_at
		) VALUES (
			gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7, NOW()
		)
	`, familyID, userID, entityType, entityID, action, oldJSON, newJSON)
	return err
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

	// Sanitize original filename to prevent path traversal
	cleanFilename := filepath.Base(filepath.Clean(filename))
	if cleanFilename == "." || cleanFilename == "/" || cleanFilename == "" {
		cleanFilename = "statement.csv"
	}

	// Read bounded content (10 MB limit)
	rawBytes, err := io.ReadAll(io.LimitReader(r, 10*1024*1024+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read statement content: %w", err)
	}
	if int64(len(rawBytes)) > 10*1024*1024 {
		return nil, ErrFileTooLarge
	}
	if len(rawBytes) == 0 {
		return nil, errors.New("uploaded statement file is empty")
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
			return ErrAccountArchived
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
			OriginalFilename: cleanFilename,
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
			BatchRevision:          1,
			UnmatchedRowsCount:     parsed.UnmatchedRowsCount,
			DiscoveredAccounts:     parsed.DiscoveredAccounts,
			UnmatchedAccounts:      parsed.UnmatchedAccounts,
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

		// 9. Record audit event
		_ = recordAuditEvent(ctx, tx, familyID, &userID, "statement_import_batch", batchID, "statement_uploaded", nil, map[string]any{
			"account_id":    accountID,
			"source_format": parser.Profile().FormatID,
			"total_rows":    len(parsed.Rows),
			"filename":      cleanFilename,
		})

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &batch, nil
}

// GetImportPreview constructs a comprehensive, safe preview of an import batch and all its staged rows.
func (s *Service) GetImportPreview(ctx context.Context, familyID, batchID uuid.UUID) (*ImportPreviewResponse, error) {
	if familyID == uuid.Nil || batchID == uuid.Nil {
		return nil, ErrTenantContextMismatch
	}

	var preview ImportPreviewResponse

	err := database.WithFamilyTx(ctx, s.pool, familyID, func(tx pgx.Tx) error {
		batch, err := s.repo.GetBatchByID(ctx, tx, familyID, batchID)
		if err != nil {
			return err
		}

		doc, err := s.repo.GetDocumentByID(ctx, tx, familyID, batch.DocumentID)
		if err != nil {
			return err
		}

		stagedRows, err := s.repo.GetStagedTransactionsByBatch(ctx, tx, familyID, batchID)
		if err != nil {
			return err
		}

		// Compute metrics
		summary := ImportSummary{
			ImportID:               batch.ID,
			AccountID:              batch.AccountID,
			Status:                 batch.Status,
			BatchRevision:          batch.BatchRevision,
			ApprovedBatchRevision:  batch.ApprovedBatchRevision,
			SourceFormat:           batch.SourceFormat,
			ParserVersion:          batch.ParserVersion,
			OriginalFilename:       doc.OriginalFilename,
			TotalRows:              batch.TotalRows,
			DiscrepancyCount:       batch.DiscrepancyCount,
			ReconciliationStatus:   batch.ReconciliationStatus,
			OpeningBalanceMinor:    batch.OpeningBalanceMinor,
			ClosingBalanceMinor:    batch.ClosingBalanceMinor,
			CalculatedBalanceMinor: batch.CalculatedBalanceMinor,
			PeriodStartDate:        batch.PeriodStartDate,
			PeriodEndDate:          batch.PeriodEndDate,
			ApprovedByUserID:       batch.ApprovedByUserID,
			ApprovedAt:             batch.ApprovedAt,
			ErrorCode:              batch.ErrorCode,
			ErrorMessage:           batch.ErrorMessage,
			CreatedAt:              batch.CreatedAt,
			UpdatedAt:              batch.UpdatedAt,
		}

		rowsPreview := make([]StagedRowPreview, 0, len(stagedRows))
		for _, row := range stagedRows {
			isCancelled := row.BookingStatus == "cancelled"
			isConfirmedDup := row.DuplicateStatus == DuplicateStatusConfirmed
			isPotentialDup := row.DuplicateStatus == DuplicateStatusPotential
			isPendingRow := row.BookingStatus == "pending"
			isPendingResolution := row.ResolutionAction == ResolutionActionPending
			isTransfer := row.OperationType == "transfer_in" || row.OperationType == "transfer_out"
			isRefund := row.OperationType == "refund"
			isReleasePending := row.ResolutionAction == ResolutionActionReleasePending
			isApplySettlement := row.ResolutionAction == ResolutionActionApplySettlement

			if (!isCancelled && !isConfirmedDup && row.ResolutionAction != ResolutionActionSkipCancelled) || isReleasePending {
				summary.EligibleRows++
			}
			if isConfirmedDup {
				summary.DuplicateRows++
			}
			if isPendingRow {
				summary.PendingRows++
			}
			if isCancelled {
				summary.CancelledRows++
			}
			if isTransfer {
				summary.TransferCandidates++
			}
			if isRefund {
				summary.RefundCandidates++
			}
			if isPendingResolution || isPotentialDup {
				summary.AmbiguousRows++
			}

			var warnings []string
			if isCancelled && isReleasePending {
				warnings = append(warnings, "Cancelled bank operation: will release and neutralize matching pending authorization hold")
			} else if isCancelled {
				warnings = append(warnings, "Cancelled bank operation: prohibited from ledger promotion")
			}
			if isApplySettlement {
				warnings = append(warnings, "Settlement of prior pending authorization hold")
			}
			if isPotentialDup {
				warnings = append(warnings, "Ambiguous candidate: review against existing transactions required")
			}
			if isPendingRow {
				warnings = append(warnings, "Authorization hold / pending transaction: not posted")
			}
			if isPendingResolution {
				warnings = append(warnings, "Resolution action is pending user review")
			}

			isEligible := (!isCancelled && !isConfirmedDup) || isReleasePending
			if row.ResolutionAction == ResolutionActionSkipDuplicate || row.ResolutionAction == ResolutionActionSkipCancelled {
				isEligible = false
			}

			var candidateIDs []uuid.UUID
			if isPotentialDup || isPendingResolution {
				pRows, err := tx.Query(ctx, `
					SELECT id FROM public.bank_transactions
					WHERE family_id = $1 AND account_id = $2 AND booking_status = 'pending'
					ORDER BY ABS(EXTRACT(EPOCH FROM (transaction_time - $3::TIMESTAMPTZ))) ASC
					LIMIT 5
				`, familyID, row.AccountID, row.TransactionTime.UTC())
				if err == nil {
					for pRows.Next() {
						var cid uuid.UUID
						if err := pRows.Scan(&cid); err == nil {
							candidateIDs = append(candidateIDs, cid)
						}
					}
					pRows.Close()
				}
			}

			rowsPreview = append(rowsPreview, StagedRowPreview{
				ID:                     row.ID,
				RowIndex:               row.RowIndex,
				TransactionTime:        row.TransactionTime,
				SettledTime:            row.SettledTime,
				AmountMinor:            row.AmountMinor,
				Currency:               row.Currency,
				AccountAmountMinor:     row.AccountAmountMinor,
				AccountCurrency:        row.AccountCurrency,
				RawDescription:         row.RawDescription,
				MerchantName:           row.MerchantName,
				BankCategory:           row.BankCategory,
				CategoryID:             row.CategoryID,
				BookingStatus:          row.BookingStatus,
				OperationType:          row.OperationType,
				DuplicateStatus:        row.DuplicateStatus,
				MatchedExistingTxID:    row.MatchedExistingTxID,
				ResolutionAction:       row.ResolutionAction,
				IsEligibleForPromotion: isEligible,
				RequiresReview:         isPendingResolution || isPotentialDup,
				Warnings:               warnings,
				CandidatePendingTxIDs:  candidateIDs,
			})
		}

		preview.Summary = summary
		preview.Rows = rowsPreview
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &preview, nil
}

// GetImportSummary retrieves the statistical summary for a single import batch.
func (s *Service) GetImportSummary(ctx context.Context, familyID, batchID uuid.UUID) (*ImportSummary, error) {
	prev, err := s.GetImportPreview(ctx, familyID, batchID)
	if err != nil {
		return nil, err
	}
	return &prev.Summary, nil
}

// UpdateStagedRow allows the user to reclassify, change category, or change resolution on a staged row.
// Modifying a staged row increments the batch revision and revokes any prior approval snapshot.
func (s *Service) UpdateStagedRow(
	ctx context.Context,
	familyID, userID, batchID, rowID uuid.UUID,
	req UpdateStagedRowRequest,
) (*ImportSummary, error) {
	if familyID == uuid.Nil || userID == uuid.Nil || batchID == uuid.Nil || rowID == uuid.Nil {
		return nil, ErrTenantContextMismatch
	}

	err := database.WithFamilyUserTx(ctx, s.pool, familyID, userID, func(tx pgx.Tx) error {
		// 1. Lock batch FOR UPDATE
		var batchStatus string
		err := tx.QueryRow(ctx, `
			SELECT status FROM public.statement_import_batches
			WHERE family_id = $1 AND id = $2
			FOR UPDATE
		`, familyID, batchID).Scan(&batchStatus)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrBatchNotFound
			}
			return fmt.Errorf("failed to lock batch: %w", err)
		}
		if batchStatus == BatchStatusCommitted {
			return ErrBatchAlreadyCommitted
		}

		// 2. Retrieve staged row
		row, err := s.repo.GetStagedTransactionByID(ctx, tx, familyID, rowID)
		if err != nil {
			return err
		}
		if row.BatchID != batchID {
			return ErrStagedTransactionNotFound
		}

		// 3. Validation: Cancelled rows can NEVER have resolution_action = 'commit'
		if row.BookingStatus == "cancelled" {
			if req.ResolutionAction != nil && *req.ResolutionAction == ResolutionActionCommit {
				return ErrCannotCommitCancelled
			}
		}

		// 4. Validate operation_type if supplied
		if req.OperationType != nil {
			switch *req.OperationType {
			case "purchase", "refund", "transfer_out", "transfer_in", "atm_cash", "fee", "card_check":
			default:
				return fmt.Errorf("invalid operation_type: %s", *req.OperationType)
			}
		}

		// 5. Validate category if supplied
		if req.CategoryID != nil {
			var catExists bool
			err = tx.QueryRow(ctx, `
				SELECT EXISTS(
					SELECT 1 FROM public.categories
					WHERE family_id = $1 AND id = $2
				)
			`, familyID, *req.CategoryID).Scan(&catExists)
			if err != nil {
				return fmt.Errorf("failed to check category: %w", err)
			}
			if !catExists {
				return errors.New("selected category does not exist in family")
			}
		}

		// 6. Validate matched_existing_tx_id if supplied
		if req.MatchedExistingTxID != nil {
			var existingBookingStatus string
			var existingAccID uuid.UUID
			var existingAccCur string
			err = tx.QueryRow(ctx, `
				SELECT booking_status, account_id, account_currency
				FROM public.bank_transactions
				WHERE family_id = $1 AND id = $2
			`, familyID, *req.MatchedExistingTxID).Scan(&existingBookingStatus, &existingAccID, &existingAccCur)
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return fmt.Errorf("target settlement transaction not found")
				}
				return fmt.Errorf("failed to query target transaction: %w", err)
			}
			if existingAccID != row.AccountID {
				return fmt.Errorf("target transaction belongs to different account")
			}
			if existingBookingStatus != "pending" {
				return fmt.Errorf("target transaction is in status %s, expected pending", existingBookingStatus)
			}
			if existingAccCur != row.AccountCurrency {
				return fmt.Errorf("target transaction currency %s does not match row currency %s", existingAccCur, row.AccountCurrency)
			}
		}

		// 7. Validate settlement or cancellation action requires target ID
		if req.ResolutionAction != nil {
			switch *req.ResolutionAction {
			case ResolutionActionApplySettlement, ResolutionActionReleasePending:
				if row.MatchedExistingTxID == nil && req.MatchedExistingTxID == nil {
					return fmt.Errorf("cannot set resolution action %s: missing matched existing transaction ID", *req.ResolutionAction)
				}
			}
		}

		// 8. Update row
		err = s.repo.UpdateStagedRow(ctx, tx, familyID, rowID, req.CategoryID, req.OperationType, req.ResolutionAction, req.MatchedExistingTxID)
		if err != nil {
			return err
		}

		// 6. Increment batch revision & revoke approval if previously approved
		_, err = s.repo.IncrementBatchRevision(ctx, tx, familyID, batchID)
		if err != nil {
			return err
		}

		// 7. Audit log
		_ = recordAuditEvent(ctx, tx, familyID, &userID, "staged_bank_transaction", rowID, "staged_row_updated", map[string]any{
			"category_id":       row.CategoryID,
			"operation_type":    row.OperationType,
			"resolution_action": row.ResolutionAction,
		}, map[string]any{
			"category_id":       req.CategoryID,
			"operation_type":    req.OperationType,
			"resolution_action": req.ResolutionAction,
		})

		return nil
	})
	if err != nil {
		return nil, err
	}

	return s.GetImportSummary(ctx, familyID, batchID)
}

// ApproveBatch records explicit user approval for a reviewed batch snapshot.
func (s *Service) ApproveBatch(
	ctx context.Context,
	familyID, userID, batchID uuid.UUID,
	req ApproveBatchRequest,
) (*ImportSummary, error) {
	if familyID == uuid.Nil || userID == uuid.Nil || batchID == uuid.Nil {
		return nil, ErrTenantContextMismatch
	}

	err := database.WithFamilyUserTx(ctx, s.pool, familyID, userID, func(tx pgx.Tx) error {
		// 1. Lock batch FOR UPDATE
		var status, reconStatus string
		var revision int
		err := tx.QueryRow(ctx, `
			SELECT status, reconciliation_status, batch_revision
			FROM public.statement_import_batches
			WHERE family_id = $1 AND id = $2
			FOR UPDATE
		`, familyID, batchID).Scan(&status, &reconStatus, &revision)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrBatchNotFound
			}
			return fmt.Errorf("failed to lock batch: %w", err)
		}
		if status == BatchStatusCommitted {
			return ErrBatchAlreadyCommitted
		}
		if status == BatchStatusCancelled {
			return fmt.Errorf("%w: cannot approve a cancelled batch", ErrInvalidBatchState)
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

		// 3. Staged Rows Verification: No pending resolution, no cancelled commit
		stagedRows, err := s.repo.GetStagedTransactionsByBatch(ctx, tx, familyID, batchID)
		if err != nil {
			return err
		}
		for _, row := range stagedRows {
			if row.ResolutionAction == ResolutionActionPending {
				return fmt.Errorf("%w: staged row %d is pending review", ErrUnresolvedStagedRows, row.RowIndex)
			}
			if row.BookingStatus == "cancelled" && row.ResolutionAction == ResolutionActionCommit {
				return ErrCannotCommitCancelled
			}
		}

		// 4. Update status to approved and record approval snapshot revision
		err = s.repo.UpdateBatchStatus(ctx, tx, familyID, batchID, BatchStatusApproved, &userID, &revision)
		if err != nil {
			return err
		}

		// 5. Audit log
		_ = recordAuditEvent(ctx, tx, familyID, &userID, "statement_import_batch", batchID, "statement_batch_approved", nil, map[string]any{
			"approved_batch_revision": revision,
			"notes":                   req.Notes,
		})

		return nil
	})
	if err != nil {
		return nil, err
	}

	return s.GetImportSummary(ctx, familyID, batchID)
}

// CancelBatch transitions a staged or approved batch to cancelled.
func (s *Service) CancelBatch(ctx context.Context, familyID, userID, batchID uuid.UUID) (*ImportSummary, error) {
	if familyID == uuid.Nil || userID == uuid.Nil || batchID == uuid.Nil {
		return nil, ErrTenantContextMismatch
	}

	err := database.WithFamilyUserTx(ctx, s.pool, familyID, userID, func(tx pgx.Tx) error {
		var status string
		err := tx.QueryRow(ctx, `
			SELECT status FROM public.statement_import_batches
			WHERE family_id = $1 AND id = $2
			FOR UPDATE
		`, familyID, batchID).Scan(&status)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrBatchNotFound
			}
			return fmt.Errorf("failed to query batch: %w", err)
		}
		if status == BatchStatusCommitted {
			return ErrBatchAlreadyCommitted
		}

		err = s.repo.UpdateBatchStatus(ctx, tx, familyID, batchID, BatchStatusCancelled, nil, nil)
		if err != nil {
			return err
		}

		_ = recordAuditEvent(ctx, tx, familyID, &userID, "statement_import_batch", batchID, "statement_batch_cancelled", map[string]any{"previous_status": status}, nil)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return s.GetImportSummary(ctx, familyID, batchID)
}

// CreateReconciliationOverride creates an attributable override for closing balance discrepancies.
func (s *Service) CreateReconciliationOverride(
	ctx context.Context,
	familyID, userID, batchID uuid.UUID,
	reason string,
) error {
	if familyID == uuid.Nil || userID == uuid.Nil || batchID == uuid.Nil {
		return ErrTenantContextMismatch
	}
	if strings.TrimSpace(reason) == "" {
		return errors.New("reconciliation override reason cannot be empty")
	}

	var override StatementReconciliationOverride

	err := database.WithFamilyUserTx(ctx, s.pool, familyID, userID, func(tx pgx.Tx) error {
		batch, err := s.repo.GetBatchByID(ctx, tx, familyID, batchID)
		if err != nil {
			return err
		}
		if batch.Status == BatchStatusCommitted {
			return fmt.Errorf("%w: batch is already committed", ErrInvalidBatchState)
		}
		if batch.ReconciliationStatus != ReconciliationDiscrepancy {
			return errors.New("cannot create override for statement without closing balance discrepancy")
		}

		opening := int64(0)
		closing := int64(0)
		calculated := int64(0)
		if batch.OpeningBalanceMinor != nil {
			opening = *batch.OpeningBalanceMinor
		}
		if batch.ClosingBalanceMinor != nil {
			closing = *batch.ClosingBalanceMinor
		}
		if batch.CalculatedBalanceMinor != nil {
			calculated = *batch.CalculatedBalanceMinor
		}
		delta := closing - calculated

		override = StatementReconciliationOverride{
			ID:                     uuid.New(),
			FamilyID:               familyID,
			BatchID:                batchID,
			OverriddenByUserID:     userID,
			OpeningBalanceMinor:    opening,
			ClosingBalanceMinor:    closing,
			CalculatedBalanceMinor: calculated,
			DiscrepancyDeltaMinor:  delta,
			Reason:                 strings.TrimSpace(reason),
			CreatedAt:              time.Now().UTC(),
		}

		if err := s.repo.InsertReconciliationOverride(ctx, tx, &override); err != nil {
			return err
		}

		// Invalidate any approval and increment revision because reconciliation basis changed
		_, err = s.repo.IncrementBatchRevision(ctx, tx, familyID, batchID)
		if err != nil {
			return err
		}

		_ = recordAuditEvent(ctx, tx, familyID, &userID, "statement_reconciliation_override", override.ID, "reconciliation_override_created", nil, map[string]any{
			"batch_id": batchID,
			"delta":    delta,
			"reason":   override.Reason,
		})

		return nil
	})
	if err != nil {
		return err
	}

	return nil
}

// ResolveStagedTransaction updates the resolution action for an individual staged row.
func (s *Service) ResolveStagedTransaction(
	ctx context.Context,
	familyID, stagedID uuid.UUID,
	action string,
) error {
	if familyID == uuid.Nil || stagedID == uuid.Nil {
		return ErrTenantContextMismatch
	}
	switch action {
	case ResolutionActionPending, ResolutionActionCommit, ResolutionActionSkipDuplicate, ResolutionActionSkipCancelled, ResolutionActionApplySettlement, ResolutionActionReleasePending:
	default:
		return fmt.Errorf("invalid resolution action: %s", action)
	}

	return database.WithFamilyTx(ctx, s.pool, familyID, func(tx pgx.Tx) error {
		row, err := s.repo.GetStagedTransactionByID(ctx, tx, familyID, stagedID)
		if err != nil {
			return err
		}
		if row.BookingStatus == "cancelled" && action == ResolutionActionCommit {
			return ErrCannotCommitCancelled
		}
		if err := s.repo.UpdateStagedResolution(ctx, tx, familyID, stagedID, action); err != nil {
			return err
		}
		_, err = s.repo.IncrementBatchRevision(ctx, tx, familyID, row.BatchID)
		return err
	})
}

// PromoteBatch atomically promotes approved staged transactions into the double-entry ledger.
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
		var batchRevision int
		var approvedRevision *int

		err := tx.QueryRow(ctx, `
			SELECT status, reconciliation_status, account_id, closing_balance_minor, period_end_date,
			       batch_revision, approved_batch_revision
			FROM public.statement_import_batches
			WHERE family_id = $1 AND id = $2
			FOR UPDATE
		`, familyID, batchID).Scan(&status, &reconStatus, &accountID, &closingBalanceMinor, &periodEndDate, &batchRevision, &approvedRevision)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrBatchNotFound
			}
			return fmt.Errorf("failed to query batch under lock: %w", err)
		}

		if status == BatchStatusCommitted {
			// Idempotent promotion: batch was already committed by another concurrent or prior request.
			return nil
		}
		if status == BatchStatusStaged {
			return ErrBatchNotApproved
		}
		if status == BatchStatusCancelled {
			return fmt.Errorf("%w: cannot promote cancelled batch", ErrInvalidBatchState)
		}
		if status != BatchStatusApproved {
			return fmt.Errorf("%w: batch is in status %s, expected %s", ErrInvalidBatchState, status, BatchStatusApproved)
		}

		if approvedRevision == nil || *approvedRevision != batchRevision {
			return fmt.Errorf("%w: batch revision %d does not match approved revision %v", ErrRevisionMismatch, batchRevision, approvedRevision)
		}

		// Transition to promoting status
		_, err = tx.Exec(ctx, `
			UPDATE public.statement_import_batches
			SET status = $1, updated_at = NOW()
			WHERE family_id = $2 AND id = $3
		`, BatchStatusPromoting, familyID, batchID)
		if err != nil {
			return fmt.Errorf("failed to update batch status to promoting: %w", err)
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
			if row.BookingStatus == "cancelled" && row.ResolutionAction == ResolutionActionCommit {
				return ErrCannotCommitCancelled
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
			// Skip duplicate and skip cancelled rows
			if row.ResolutionAction == ResolutionActionSkipDuplicate || row.ResolutionAction == ResolutionActionSkipCancelled {
				result.TransactionsSkipped++
				continue
			}

			// Strict safeguard: cancelled operations cannot be committed to bank_transactions
			if row.BookingStatus == "cancelled" && row.ResolutionAction != ResolutionActionReleasePending {
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

				// Insert default unsplit accounting_entries
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
						id, family_id, bank_transaction_id, account_id, category_id,
						amount_minor, currency, classification,
						payer_user_id, beneficiary_type, entry_time,
						is_split, is_manual
					) VALUES (
						$1, $2, $3, $4, $5,
						$6, $7, $8,
						$9, 'household', $10,
						FALSE, FALSE
					)
				`,
					uuid.New(), familyID, newTxID, accountID, row.CategoryID,
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

			case ResolutionActionReleasePending:
				if row.MatchedExistingTxID == nil {
					return fmt.Errorf("cannot release pending hold for row %d: missing matched existing transaction ID", row.RowIndex)
				}

				// Execute atomic cancellation via SECURITY DEFINER routine
				_, err := tx.Exec(ctx, `
					SELECT public.cancel_pending_bank_transaction($1, $2, $3)
				`, familyID, accountID, *row.MatchedExistingTxID)
				if err != nil {
					return fmt.Errorf("failed to cancel pending transaction for row %d: %w", row.RowIndex, err)
				}
				result.TransactionsCancelled++

			case ResolutionActionSkipDuplicate, ResolutionActionSkipCancelled:
				result.TransactionsSkipped++
			}
		}

		// 6. Monotonic Account Balance Snapshot Update
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
			    approved_by_user_id = COALESCE(approved_by_user_id, $2),
			    approved_at = COALESCE(approved_at, $3),
			    updated_at = $3
			WHERE family_id = $4 AND id = $5
		`, BatchStatusCommitted, userID, now, familyID, batchID)
		if err != nil {
			return fmt.Errorf("failed to finalize batch status: %w", err)
		}

		_ = recordAuditEvent(ctx, tx, familyID, &userID, "statement_import_batch", batchID, "statement_batch_promoted", nil, map[string]any{
			"committed": result.TransactionsCommitted,
			"settled":   result.TransactionsSettled,
			"cancelled": result.TransactionsCancelled,
			"skipped":   result.TransactionsSkipped,
		})

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &result, nil
}

// ListImportHistory retrieves a paginated history of imported statement batches for the authenticated family.
func (s *Service) ListImportHistory(
	ctx context.Context,
	familyID uuid.UUID,
	accountID *uuid.UUID,
	status *string,
	page, limit int,
) (*ImportHistoryResponse, error) {
	if familyID == uuid.Nil {
		return nil, ErrTenantContextMismatch
	}
	if page < 1 {
		page = 1
	}
	if limit < 1 || limit > 100 {
		limit = 20
	}
	offset := (page - 1) * limit

	var resp ImportHistoryResponse
	resp.Page = page
	resp.Limit = limit

	err := database.WithFamilyTx(ctx, s.pool, familyID, func(tx pgx.Tx) error {
		total, err := s.repo.CountBatches(ctx, tx, familyID, accountID, status)
		if err != nil {
			return err
		}
		resp.Total = total

		batches, err := s.repo.ListBatches(ctx, tx, familyID, accountID, status, limit, offset)
		if err != nil {
			return err
		}

		items := make([]ImportSummary, 0, len(batches))
		for _, b := range batches {
			doc, _ := s.repo.GetDocumentByID(ctx, tx, familyID, b.DocumentID)
			filename := ""
			if doc != nil {
				filename = doc.OriginalFilename
			}
			items = append(items, ImportSummary{
				ImportID:               b.ID,
				AccountID:              b.AccountID,
				Status:                 b.Status,
				BatchRevision:          b.BatchRevision,
				ApprovedBatchRevision:  b.ApprovedBatchRevision,
				SourceFormat:           b.SourceFormat,
				ParserVersion:          b.ParserVersion,
				OriginalFilename:       filename,
				TotalRows:              b.TotalRows,
				DuplicateRows:          b.DuplicateCount,
				DiscrepancyCount:       b.DiscrepancyCount,
				ReconciliationStatus:   b.ReconciliationStatus,
				OpeningBalanceMinor:    b.OpeningBalanceMinor,
				ClosingBalanceMinor:    b.ClosingBalanceMinor,
				CalculatedBalanceMinor: b.CalculatedBalanceMinor,
				PeriodStartDate:        b.PeriodStartDate,
				PeriodEndDate:          b.PeriodEndDate,
				ApprovedByUserID:       b.ApprovedByUserID,
				ApprovedAt:             b.ApprovedAt,
				ErrorCode:              b.ErrorCode,
				ErrorMessage:           b.ErrorMessage,
				CreatedAt:              b.CreatedAt,
				UpdatedAt:              b.UpdatedAt,
			})
		}
		resp.Items = items
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &resp, nil
}

package statement

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Repository defines data access routines for statement ingestion under tenant RLS.
type Repository struct{}

func NewRepository() *Repository {
	return &Repository{}
}

func (r *Repository) InsertDocument(ctx context.Context, tx pgx.Tx, doc *StatementDocument) error {
	query := `
		INSERT INTO public.statement_documents (
			id, family_id, uploaded_by_user_id, original_filename,
			file_size_bytes, content_hash, mime_type, encrypted_content, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`
	_, err := tx.Exec(ctx, query,
		doc.ID, doc.FamilyID, doc.UploadedByUserID, doc.OriginalFilename,
		doc.FileSizeBytes, doc.ContentHash, doc.MIMEType, doc.EncryptedContent, doc.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("failed to insert statement_document: %w", err)
	}
	return nil
}

func (r *Repository) GetDocumentByHash(ctx context.Context, tx pgx.Tx, familyID uuid.UUID, hash string) (*StatementDocument, error) {
	query := `
		SELECT id, family_id, uploaded_by_user_id, original_filename,
		       file_size_bytes, content_hash, mime_type, encrypted_content, created_at
		FROM public.statement_documents
		WHERE family_id = $1 AND content_hash = $2
		LIMIT 1
	`
	var doc StatementDocument
	err := tx.QueryRow(ctx, query, familyID, hash).Scan(
		&doc.ID, &doc.FamilyID, &doc.UploadedByUserID, &doc.OriginalFilename,
		&doc.FileSizeBytes, &doc.ContentHash, &doc.MIMEType, &doc.EncryptedContent, &doc.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrDocumentNotFound
		}
		return nil, fmt.Errorf("failed to query statement_document by hash: %w", err)
	}
	return &doc, nil
}

func (r *Repository) InsertBatch(ctx context.Context, tx pgx.Tx, batch *StatementImportBatch) error {
	query := `
		INSERT INTO public.statement_import_batches (
			id, family_id, account_id, document_id, source_format, parser_version,
			status, reconciliation_status, period_start_date, period_end_date,
			total_rows, staged_count, duplicate_count, discrepancy_count,
			opening_balance_minor, closing_balance_minor, calculated_balance_minor,
			error_code, error_message, created_at, updated_at
		) VALUES (
			$1, $2, $3, $4, $5, $6,
			$7, $8, $9, $10,
			$11, $12, $13, $14,
			$15, $16, $17,
			$18, $19, $20, $21
		)
	`
	_, err := tx.Exec(ctx, query,
		batch.ID, batch.FamilyID, batch.AccountID, batch.DocumentID, batch.SourceFormat, batch.ParserVersion,
		batch.Status, batch.ReconciliationStatus, batch.PeriodStartDate, batch.PeriodEndDate,
		batch.TotalRows, batch.StagedCount, batch.DuplicateCount, batch.DiscrepancyCount,
		batch.OpeningBalanceMinor, batch.ClosingBalanceMinor, batch.CalculatedBalanceMinor,
		batch.ErrorCode, batch.ErrorMessage, batch.CreatedAt, batch.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("failed to insert statement_import_batch: %w", err)
	}
	return nil
}

func (r *Repository) GetBatchByID(ctx context.Context, tx pgx.Tx, familyID, batchID uuid.UUID) (*StatementImportBatch, error) {
	query := `
		SELECT id, family_id, account_id, document_id, source_format, parser_version,
		       status, reconciliation_status, period_start_date, period_end_date,
		       total_rows, staged_count, duplicate_count, discrepancy_count,
		       opening_balance_minor, closing_balance_minor, calculated_balance_minor,
		       approved_by_user_id, approved_at, error_code, error_message, created_at, updated_at
		FROM public.statement_import_batches
		WHERE family_id = $1 AND id = $2
	`
	var b StatementImportBatch
	err := tx.QueryRow(ctx, query, familyID, batchID).Scan(
		&b.ID, &b.FamilyID, &b.AccountID, &b.DocumentID, &b.SourceFormat, &b.ParserVersion,
		&b.Status, &b.ReconciliationStatus, &b.PeriodStartDate, &b.PeriodEndDate,
		&b.TotalRows, &b.StagedCount, &b.DuplicateCount, &b.DiscrepancyCount,
		&b.OpeningBalanceMinor, &b.ClosingBalanceMinor, &b.CalculatedBalanceMinor,
		&b.ApprovedByUserID, &b.ApprovedAt, &b.ErrorCode, &b.ErrorMessage, &b.CreatedAt, &b.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrBatchNotFound
		}
		return nil, fmt.Errorf("failed to query statement_import_batch: %w", err)
	}
	return &b, nil
}

func (r *Repository) InsertStagedTransactions(ctx context.Context, tx pgx.Tx, staged []StagedBankTransaction) error {
	query := `
		INSERT INTO public.staged_bank_transactions (
			id, family_id, batch_id, account_id, source_external_id, card_mask,
			row_index, transaction_time, settled_time, amount_minor, currency,
			account_amount_minor, account_currency, operation_type, booking_status,
			raw_description, merchant_name, mcc, bank_category, import_fingerprint,
			duplicate_status, matched_existing_tx_id, resolution_action, created_at
		) VALUES (
			$1, $2, $3, $4, $5, $6,
			$7, $8, $9, $10, $11,
			$12, $13, $14, $15,
			$16, $17, $18, $19, $20,
			$21, $22, $23, $24
		)
	`
	for _, st := range staged {
		_, err := tx.Exec(ctx, query,
			st.ID, st.FamilyID, st.BatchID, st.AccountID, st.SourceExternalID, st.CardMask,
			st.RowIndex, st.TransactionTime, st.SettledTime, st.AmountMinor, st.Currency,
			st.AccountAmountMinor, st.AccountCurrency, st.OperationType, st.BookingStatus,
			st.RawDescription, st.MerchantName, st.MCC, st.BankCategory, st.ImportFingerprint,
			st.DuplicateStatus, st.MatchedExistingTxID, st.ResolutionAction, st.CreatedAt,
		)
		if err != nil {
			return fmt.Errorf("failed to insert staged_bank_transaction row %d: %w", st.RowIndex, err)
		}
	}
	return nil
}

func (r *Repository) GetStagedTransactionsByBatch(ctx context.Context, tx pgx.Tx, familyID, batchID uuid.UUID) ([]StagedBankTransaction, error) {
	query := `
		SELECT id, family_id, batch_id, account_id, source_external_id, card_mask,
		       row_index, transaction_time, settled_time, amount_minor, currency,
		       account_amount_minor, account_currency, operation_type, booking_status,
		       raw_description, merchant_name, mcc, bank_category, import_fingerprint,
		       duplicate_status, matched_existing_tx_id, resolution_action, created_at
		FROM public.staged_bank_transactions
		WHERE family_id = $1 AND batch_id = $2
		ORDER BY row_index ASC
	`
	rows, err := tx.Query(ctx, query, familyID, batchID)
	if err != nil {
		return nil, fmt.Errorf("failed to query staged_bank_transactions: %w", err)
	}
	defer rows.Close()

	var result []StagedBankTransaction
	for rows.Next() {
		var st StagedBankTransaction
		err := rows.Scan(
			&st.ID, &st.FamilyID, &st.BatchID, &st.AccountID, &st.SourceExternalID, &st.CardMask,
			&st.RowIndex, &st.TransactionTime, &st.SettledTime, &st.AmountMinor, &st.Currency,
			&st.AccountAmountMinor, &st.AccountCurrency, &st.OperationType, &st.BookingStatus,
			&st.RawDescription, &st.MerchantName, &st.MCC, &st.BankCategory, &st.ImportFingerprint,
			&st.DuplicateStatus, &st.MatchedExistingTxID, &st.ResolutionAction, &st.CreatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan staged_bank_transaction: %w", err)
		}
		result = append(result, st)
	}
	return result, nil
}

func (r *Repository) UpdateStagedResolution(ctx context.Context, tx pgx.Tx, familyID, stagedID uuid.UUID, action string) error {
	query := `
		UPDATE public.staged_bank_transactions
		SET resolution_action = $1
		WHERE family_id = $2 AND id = $3
	`
	res, err := tx.Exec(ctx, query, action, familyID, stagedID)
	if err != nil {
		return fmt.Errorf("failed to update staged resolution: %w", err)
	}
	if res.RowsAffected() == 0 {
		return ErrStagedTransactionNotFound
	}
	return nil
}

func (r *Repository) InsertReconciliationOverride(ctx context.Context, tx pgx.Tx, override *StatementReconciliationOverride) error {
	query := `
		INSERT INTO public.statement_reconciliation_overrides (
			id, family_id, batch_id, overridden_by_user_id,
			opening_balance_minor, closing_balance_minor, calculated_balance_minor,
			discrepancy_delta_minor, reason, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`
	_, err := tx.Exec(ctx, query,
		override.ID, override.FamilyID, override.BatchID, override.OverriddenByUserID,
		override.OpeningBalanceMinor, override.ClosingBalanceMinor, override.CalculatedBalanceMinor,
		override.DiscrepancyDeltaMinor, override.Reason, override.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("failed to insert reconciliation override: %w", err)
	}
	return nil
}

func (r *Repository) HasReconciliationOverride(ctx context.Context, tx pgx.Tx, familyID, batchID uuid.UUID) (bool, error) {
	var exists bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM public.statement_reconciliation_overrides
			WHERE family_id = $1 AND batch_id = $2
		)
	`, familyID, batchID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("failed to check reconciliation override: %w", err)
	}
	return exists, nil
}

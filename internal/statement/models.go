package statement

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/google/uuid"
)

var (
	ErrBatchNotFound              = errors.New("statement import batch not found")
	ErrDocumentNotFound           = errors.New("statement document not found")
	ErrStagedTransactionNotFound  = errors.New("staged transaction not found")
	ErrInvalidBatchState          = errors.New("invalid statement import batch state for requested operation")
	ErrReconciliationDiscrepancy  = errors.New("statement balance reconciliation failed: closing balance discrepancy detected")
	ErrOverrideRequired           = errors.New("reconciliation discrepancy must be explicitly overridden by user prior to ledger promotion")
	ErrUnresolvedStagedRows       = errors.New("cannot promote batch: one or more staged transactions require user resolution")
	ErrTooManyConcurrentImports   = errors.New("maximum concurrent statement imports reached for family (limit: 2)")
	ErrLineTooLong                = errors.New("statement line length exceeds maximum permitted buffer size (64 KB)")
	ErrTooManyRows                = errors.New("statement total rows exceed maximum permitted count (50,000 rows)")
	ErrFileTooLarge               = errors.New("uploaded file exceeds maximum size limit (10 MB)")
	ErrParserNotFound             = errors.New("no compatible statement parser found for document")
	ErrTenantContextMismatch      = errors.New("tenant context violation between batch, account, and document")
	ErrMissingRequiredColumn      = errors.New("statement is missing required column")
	ErrAmbiguousAccountSelection  = errors.New("ambiguous account selection for statement import")
	ErrMultiAccountAmbiguity      = errors.New("statement contains operations for multiple accounts without explicit selector")
	ErrAccountNotFoundInStatement = errors.New("target account not found in statement")
	ErrUnsupportedEncoding        = errors.New("unsupported statement file encoding")
	ErrRecordTooLarge             = errors.New("CSV record size exceeds maximum limit (64 KB)")
	ErrInvalidBookingStatus       = errors.New("unknown or invalid booking status")
	ErrMalformedDate              = errors.New("malformed or unparseable transaction timestamp")
	ErrMalformedCSVQuoting        = errors.New("malformed CSV quotes or field boundaries")
	ErrRevisionMismatch           = errors.New("statement import batch revision mismatch; review is out of date")
	ErrBatchNotApproved           = errors.New("statement import batch must be approved prior to promotion")
	ErrBatchAlreadyCommitted      = errors.New("statement import batch has already been committed")
	ErrCannotCommitCancelled      = errors.New("cancelled bank operations cannot be promoted to the financial ledger")
	ErrAccountArchived            = errors.New("cannot import statement into an archived account")
	ErrCurrencyMismatch           = errors.New("target account currency does not match statement currency")
	ErrDuplicateSettlementTarget  = errors.New("multiple staged rows cannot target the same pending transaction")
)

// Batch processing lifecycle states.
const (
	BatchStatusUploaded  = "uploaded"
	BatchStatusParsing   = "parsing"
	BatchStatusStaged    = "staged"
	BatchStatusApproved  = "approved"
	BatchStatusPromoting = "promoting"
	BatchStatusCommitted = "committed"
	BatchStatusRejected  = "rejected"
	BatchStatusCancelled = "cancelled"
	BatchStatusFailed    = "failed"
)

// Persistent reconciliation classifications.
const (
	ReconciliationReconciled      = "reconciled"
	ReconciliationDiscrepancy     = "discrepancy"
	ReconciliationNonReconcilable = "non_reconcilable"
)

// Duplicate detection statuses.
const (
	DuplicateStatusNew                = "new"
	DuplicateStatusConfirmed          = "confirmed_duplicate"
	DuplicateStatusPotential          = "potential_duplicate"
	DuplicateStatusSettlementUpdate   = "settlement_update"
	DuplicateStatusCancellationUpdate = "cancellation_update"
)

// Resolution actions for staged rows.
const (
	ResolutionActionPending         = "pending"
	ResolutionActionCommit          = "commit"
	ResolutionActionSkipDuplicate   = "skip_duplicate"
	ResolutionActionSkipCancelled   = "skip_cancelled"
	ResolutionActionApplySettlement = "apply_settlement"
	ResolutionActionReleasePending  = "release_pending"
)

// StatementDocument represents the immutable stored document metadata.
type StatementDocument struct {
	ID               uuid.UUID `json:"id"`
	FamilyID         uuid.UUID `json:"family_id"`
	UploadedByUserID uuid.UUID `json:"uploaded_by_user_id"`
	OriginalFilename string    `json:"original_filename"`
	FileSizeBytes    int64     `json:"file_size_bytes"`
	ContentHash      string    `json:"content_hash"`
	MIMEType         string    `json:"mime_type"`
	EncryptedContent []byte    `json:"-"`
	CreatedAt        time.Time `json:"created_at"`
}

// StatementImportBatch represents the orchestration and lifecycle record.
type StatementImportBatch struct {
	ID                     uuid.UUID      `json:"id"`
	FamilyID               uuid.UUID      `json:"family_id"`
	AccountID              uuid.UUID      `json:"account_id"`
	DocumentID             uuid.UUID      `json:"document_id"`
	SourceFormat           string         `json:"source_format"`
	ParserVersion          string         `json:"parser_version"`
	Status                 string         `json:"status"`
	ReconciliationStatus   string         `json:"reconciliation_status"`
	PeriodStartDate        *time.Time     `json:"period_start_date,omitempty"`
	PeriodEndDate          *time.Time     `json:"period_end_date,omitempty"`
	TotalRows              int            `json:"total_rows"`
	StagedCount            int            `json:"staged_count"`
	DuplicateCount         int            `json:"duplicate_count"`
	DiscrepancyCount       int            `json:"discrepancy_count"`
	OpeningBalanceMinor    *int64         `json:"opening_balance_minor,omitempty"`
	ClosingBalanceMinor    *int64         `json:"closing_balance_minor,omitempty"`
	CalculatedBalanceMinor *int64         `json:"calculated_balance_minor,omitempty"`
	ApprovedByUserID       *uuid.UUID     `json:"approved_by_user_id,omitempty"`
	ApprovedAt             *time.Time     `json:"approved_at,omitempty"`
	BatchRevision          int            `json:"batch_revision"`
	ApprovedBatchRevision  *int           `json:"approved_batch_revision,omitempty"`
	ErrorCode              *string        `json:"error_code,omitempty"`
	ErrorMessage           *string        `json:"error_message,omitempty"`
	UnmatchedRowsCount     int            `json:"unmatched_rows_count,omitempty"`
	DiscoveredAccounts     []string       `json:"discovered_accounts,omitempty"`
	UnmatchedAccounts      map[string]int `json:"unmatched_accounts,omitempty"`
	CreatedAt              time.Time      `json:"created_at"`
	UpdatedAt              time.Time      `json:"updated_at"`
}

// StagedBankTransaction represents an uncommitted staged financial record in the buffer.
type StagedBankTransaction struct {
	ID                  uuid.UUID  `json:"id"`
	FamilyID            uuid.UUID  `json:"family_id"`
	BatchID             uuid.UUID  `json:"batch_id"`
	AccountID           uuid.UUID  `json:"account_id"`
	SourceExternalID    *string    `json:"source_external_id,omitempty"`
	CardMask            *string    `json:"card_mask,omitempty"`
	RowIndex            int        `json:"row_index"`
	TransactionTime     time.Time  `json:"transaction_time"`
	SettledTime         *time.Time `json:"settled_time,omitempty"`
	AmountMinor         int64      `json:"amount_minor"`
	Currency            string     `json:"currency"`
	AccountAmountMinor  int64      `json:"account_amount_minor"`
	AccountCurrency     string     `json:"account_currency"`
	OperationType       string     `json:"operation_type"`
	BookingStatus       string     `json:"booking_status"`
	RawDescription      string     `json:"raw_description"`
	MerchantName        *string    `json:"merchant_name,omitempty"`
	MCC                 *int       `json:"mcc,omitempty"`
	BankCategory        *string    `json:"bank_category,omitempty"`
	CategoryID          *uuid.UUID `json:"category_id,omitempty"`
	ImportFingerprint   string     `json:"import_fingerprint"`
	DuplicateStatus     string     `json:"duplicate_status"`
	MatchedExistingTxID *uuid.UUID `json:"matched_existing_tx_id,omitempty"`
	ResolutionAction    string     `json:"resolution_action"`
	CreatedAt           time.Time  `json:"created_at"`
}

// StatementReconciliationOverride represents an attributable user override of a balance discrepancy.
type StatementReconciliationOverride struct {
	ID                     uuid.UUID `json:"id"`
	FamilyID               uuid.UUID `json:"family_id"`
	BatchID                uuid.UUID `json:"batch_id"`
	OverriddenByUserID     uuid.UUID `json:"overridden_by_user_id"`
	OpeningBalanceMinor    int64     `json:"opening_balance_minor"`
	ClosingBalanceMinor    int64     `json:"closing_balance_minor"`
	CalculatedBalanceMinor int64     `json:"calculated_balance_minor"`
	DiscrepancyDeltaMinor  int64     `json:"discrepancy_delta_minor"`
	Reason                 string    `json:"reason"`
	CreatedAt              time.Time `json:"created_at"`
}

// StagedRowDTO encapsulates normalized row data produced by a parser.
type StagedRowDTO struct {
	RowIndex           int
	SourceExternalID   *string
	CardMask           *string
	TransactionTime    time.Time
	SettledTime        *time.Time
	AmountMinor        int64
	Currency           string
	AccountAmountMinor int64
	AccountCurrency    string
	OperationType      string
	BookingStatus      string // "pending", "posted", "cancelled"
	RawDescription     string
	MerchantName       *string
	MCC                *int
	BankCategory       *string
}

// ParsedStatementDTO encapsulates the complete normalized output of a parser.
type ParsedStatementDTO struct {
	FormatID            string
	ParserVersion       string
	PeriodStartDate     *time.Time
	PeriodEndDate       *time.Time
	OpeningBalanceMinor *int64
	ClosingBalanceMinor *int64
	AccountCurrency     string
	Rows                []StagedRowDTO
	IsReconcilable      bool
	DiscoveredAccounts  []string
	UnmatchedRowsCount  int
	UnmatchedAccounts   map[string]int
}

// StatementProfile documents parser capabilities.
type StatementProfile struct {
	FormatID       string
	DisplayName    string
	FileExtensions []string
	MIMETypes      []string
}

// StatementParser defines the contract for multi-format statement parsers.
type StatementParser interface {
	Profile() StatementProfile
	Sniff(headerChunk []byte) bool
	Parse(ctx context.Context, r io.Reader, accountCurrency string) (*ParsedStatementDTO, error)
}

// ImportSummary encapsulates enriched statistical summary of an import batch.
type ImportSummary struct {
	ImportID               uuid.UUID  `json:"import_id"`
	AccountID              uuid.UUID  `json:"account_id"`
	Status                 string     `json:"status"`
	BatchRevision          int        `json:"batch_revision"`
	ApprovedBatchRevision  *int       `json:"approved_batch_revision,omitempty"`
	SourceFormat           string     `json:"source_format"`
	ParserVersion          string     `json:"parser_version"`
	OriginalFilename       string     `json:"original_filename"`
	TotalRows              int        `json:"total_rows"`
	EligibleRows           int        `json:"eligible_rows"`
	DuplicateRows          int        `json:"duplicate_rows"`
	PendingRows            int        `json:"pending_rows"`
	CancelledRows          int        `json:"cancelled_rows"`
	TransferCandidates     int        `json:"transfer_candidates"`
	RefundCandidates       int        `json:"refund_candidates"`
	AmbiguousRows          int        `json:"ambiguous_rows"`
	AlreadyPromotedRows    int        `json:"already_promoted_rows"`
	DiscrepancyCount       int        `json:"discrepancy_count"`
	ReconciliationStatus   string     `json:"reconciliation_status"`
	OpeningBalanceMinor    *int64     `json:"opening_balance_minor,omitempty"`
	ClosingBalanceMinor    *int64     `json:"closing_balance_minor,omitempty"`
	CalculatedBalanceMinor *int64     `json:"calculated_balance_minor,omitempty"`
	PeriodStartDate        *time.Time `json:"period_start_date,omitempty"`
	PeriodEndDate          *time.Time `json:"period_end_date,omitempty"`
	ApprovedByUserID       *uuid.UUID `json:"approved_by_user_id,omitempty"`
	ApprovedAt             *time.Time `json:"approved_at,omitempty"`
	ErrorCode              *string    `json:"error_code,omitempty"`
	ErrorMessage           *string    `json:"error_message,omitempty"`
	CreatedAt              time.Time  `json:"created_at"`
	UpdatedAt              time.Time  `json:"updated_at"`
}

// StagedRowPreview exposes safe, non-sensitive financial details of a staged row for user review.
type StagedRowPreview struct {
	ID                     uuid.UUID   `json:"id"`
	RowIndex               int         `json:"row_index"`
	TransactionTime        time.Time   `json:"transaction_time"`
	SettledTime            *time.Time  `json:"settled_time,omitempty"`
	AmountMinor            int64       `json:"amount_minor"`
	Currency               string      `json:"currency"`
	AccountAmountMinor     int64       `json:"account_amount_minor"`
	AccountCurrency        string      `json:"account_currency"`
	RawDescription         string      `json:"raw_description"`
	MerchantName           *string     `json:"merchant_name,omitempty"`
	BankCategory           *string     `json:"bank_category,omitempty"`
	CategoryID             *uuid.UUID  `json:"category_id,omitempty"`
	BookingStatus          string      `json:"booking_status"`
	OperationType          string      `json:"operation_type"`
	DuplicateStatus        string      `json:"duplicate_status"`
	MatchedExistingTxID    *uuid.UUID  `json:"matched_existing_tx_id,omitempty"`
	ResolutionAction       string      `json:"resolution_action"`
	IsEligibleForPromotion bool        `json:"is_eligible_for_promotion"`
	RequiresReview         bool        `json:"requires_review"`
	Warnings               []string    `json:"warnings,omitempty"`
	CandidatePendingTxIDs  []uuid.UUID `json:"candidate_pending_tx_ids,omitempty"`
}

// ImportPreviewResponse represents the complete preview with summary and staged rows.
type ImportPreviewResponse struct {
	Summary ImportSummary      `json:"summary"`
	Rows    []StagedRowPreview `json:"rows"`
}

// UpdateStagedRowRequest specifies mutable properties during the review phase.
type UpdateStagedRowRequest struct {
	CategoryID          *uuid.UUID `json:"category_id,omitempty"`
	OperationType       *string    `json:"operation_type,omitempty"`
	ResolutionAction    *string    `json:"resolution_action,omitempty"`
	MatchedExistingTxID *uuid.UUID `json:"matched_existing_tx_id,omitempty"`
}

// ApproveBatchRequest specifies approval options.
type ApproveBatchRequest struct {
	Notes *string `json:"notes,omitempty"`
}

// ReconciliationOverrideRequest specifies rationale for overriding a closing balance discrepancy.
type ReconciliationOverrideRequest struct {
	Reason string `json:"reason"`
}

// ImportHistoryResponse represents a paginated list of import batches.
type ImportHistoryResponse struct {
	Items []ImportSummary `json:"items"`
	Total int             `json:"total"`
	Page  int             `json:"page"`
	Limit int             `json:"limit"`
}

package statement

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/google/uuid"
)

var (
	ErrBatchNotFound               = errors.New("statement import batch not found")
	ErrDocumentNotFound            = errors.New("statement document not found")
	ErrStagedTransactionNotFound   = errors.New("staged transaction not found")
	ErrInvalidBatchState           = errors.New("invalid statement import batch state for requested operation")
	ErrReconciliationDiscrepancy   = errors.New("statement balance reconciliation failed: closing balance discrepancy detected")
	ErrOverrideRequired            = errors.New("reconciliation discrepancy must be explicitly overridden by user prior to ledger promotion")
	ErrUnresolvedStagedRows        = errors.New("cannot promote batch: one or more staged transactions require user resolution")
	ErrTooManyConcurrentImports    = errors.New("maximum concurrent statement imports reached for family (limit: 2)")
	ErrLineTooLong                 = errors.New("statement line length exceeds maximum permitted buffer size (64 KB)")
	ErrTooManyRows                 = errors.New("statement total rows exceed maximum permitted count (50,000 rows)")
	ErrFileTooLarge                = errors.New("uploaded file exceeds maximum size limit (10 MB)")
	ErrParserNotFound              = errors.New("no compatible statement parser found for document")
	ErrTenantContextMismatch       = errors.New("tenant context violation between batch, account, and document")
)

// Batch processing lifecycle states.
const (
	BatchStatusUploaded  = "uploaded"
	BatchStatusParsing   = "parsing"
	BatchStatusStaged    = "staged"
	BatchStatusCommitted = "committed"
	BatchStatusRejected  = "rejected"
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
	DuplicateStatusNew              = "new"
	DuplicateStatusConfirmed        = "confirmed_duplicate"
	DuplicateStatusPotential        = "potential_duplicate"
	DuplicateStatusSettlementUpdate = "settlement_update"
)

// Resolution actions for staged rows.
const (
	ResolutionActionPending         = "pending"
	ResolutionActionCommit          = "commit"
	ResolutionActionSkipDuplicate   = "skip_duplicate"
	ResolutionActionApplySettlement = "apply_settlement"
)

// StatementDocument represents the immutable stored document metadata.
type StatementDocument struct {
	ID                uuid.UUID `json:"id"`
	FamilyID          uuid.UUID `json:"family_id"`
	UploadedByUserID  uuid.UUID `json:"uploaded_by_user_id"`
	OriginalFilename  string    `json:"original_filename"`
	FileSizeBytes     int64     `json:"file_size_bytes"`
	ContentHash       string    `json:"content_hash"`
	MIMEType          string    `json:"mime_type"`
	EncryptedContent  []byte    `json:"-"`
	CreatedAt         time.Time `json:"created_at"`
}

// StatementImportBatch represents the orchestration and lifecycle record.
type StatementImportBatch struct {
	ID                     uuid.UUID  `json:"id"`
	FamilyID               uuid.UUID  `json:"family_id"`
	AccountID              uuid.UUID  `json:"account_id"`
	DocumentID             uuid.UUID  `json:"document_id"`
	SourceFormat           string     `json:"source_format"`
	ParserVersion          string     `json:"parser_version"`
	Status                 string     `json:"status"`
	ReconciliationStatus   string     `json:"reconciliation_status"`
	PeriodStartDate        *time.Time `json:"period_start_date,omitempty"`
	PeriodEndDate          *time.Time `json:"period_end_date,omitempty"`
	TotalRows              int        `json:"total_rows"`
	StagedCount            int        `json:"staged_count"`
	DuplicateCount         int        `json:"duplicate_count"`
	DiscrepancyCount       int        `json:"discrepancy_count"`
	OpeningBalanceMinor    *int64     `json:"opening_balance_minor,omitempty"`
	ClosingBalanceMinor    *int64     `json:"closing_balance_minor,omitempty"`
	CalculatedBalanceMinor *int64     `json:"calculated_balance_minor,omitempty"`
	ApprovedByUserID       *uuid.UUID `json:"approved_by_user_id,omitempty"`
	ApprovedAt             *time.Time `json:"approved_at,omitempty"`
	ErrorCode              *string    `json:"error_code,omitempty"`
	ErrorMessage           *string    `json:"error_message,omitempty"`
	CreatedAt              time.Time  `json:"created_at"`
	UpdatedAt              time.Time  `json:"updated_at"`
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

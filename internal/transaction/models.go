package transaction

import (
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/khaliullov/family-finance/internal/domain"
)

var (
	ErrConnectionNotFound               = errors.New("bank connection not found")
	ErrConnectionNotActive              = errors.New("bank connection is not in an active or pairable state")
	ErrAccountNotFound                  = errors.New("referenced bank account not found")
	ErrInvalidTransactionData           = errors.New("invalid transaction data from provider")
	ErrMismatchedTenantContext          = errors.New("operation violates tenant isolation boundary")
	ErrSyncIncomplete                   = errors.New("sync incomplete: reached max page limit with more pages pending")
	ErrCursorLoop                       = errors.New("cursor loop detected: provider returned a previously seen cursor")
	ErrInvalidPagination                = errors.New("invalid pagination: provider reported has_more with empty next cursor")
	ErrSyncOwnershipLost                = errors.New("per-connection sync ownership lost: concurrent sync or sync lock expired")
	ErrSyncAlreadyRunning               = errors.New("bank connection is currently being synchronized by another active job")
	ErrTransientDatabase                = errors.New("transient database infrastructure error")
	ErrSyncGateInitialImportNotApproved = errors.New("initial financial import requires explicit user approval before execution")
	ErrSyncGateMissingCredentialHash    = errors.New("connection credential hash is missing")
	ErrSyncGateMissingReportHash        = errors.New("validation report credential hash is missing")
	ErrSyncGateInvalidCredentialHash    = errors.New("credential hash format is invalid")
	ErrSyncGateActiveValidationConflict = errors.New("connection is currently undergoing technical validation")
	ErrSyncGateActiveSyncConflict       = errors.New("connection is currently undergoing financial synchronization")
	ErrInitialImportApprovalStale       = errors.New("initial import approval is stale or invalid for current connection state")
)

// ReconciliationDiscrepancy logs an attempted change to an immutable terminal transaction.
type ReconciliationDiscrepancy struct {
	ID                uuid.UUID  `json:"id,omitempty"`
	FamilyID          uuid.UUID  `json:"family_id,omitempty"`
	BankConnectionID  *uuid.UUID `json:"bank_connection_id,omitempty"`
	BankTransactionID *uuid.UUID `json:"bank_transaction_id,omitempty"`
	ExternalID        string     `json:"external_id"`
	TransactionID     uuid.UUID  `json:"transaction_id"`
	Field             string     `json:"field"` // "amount", "currency", "description", "settled_time"
	RecordedValue     string     `json:"recorded_value"`
	IncomingValue     string     `json:"incoming_value"`
	IdempotencyHash   string     `json:"idempotency_hash,omitempty"`
	DetectedAt        time.Time  `json:"detected_at"`
	CreatedAt         time.Time  `json:"created_at,omitempty"`
}

// SyncOptions configures an ingestion run.
type SyncOptions struct {
	LookbackDays  int
	Cursor        string
	BatchLimit    int
	MaxPages      int
	ForceFullSync bool
	ResetCursor   bool
}

// SyncStats captures telemetry from a synchronization run.
type SyncStats struct {
	RawPayloadsPreserved  int
	AccountsDiscovered    int
	CardsDiscovered       int
	TransactionsIngested  int
	TransactionsUpdated   int
	TransactionsSkipped   int
	DiscrepanciesDetected int
	Discrepancies         []ReconciliationDiscrepancy
	PagesProcessed        int
}

// BankConnectionEntity represents a row from bank_connections table.
type BankConnectionEntity struct {
	ID                       uuid.UUID
	FamilyID                 uuid.UUID
	AuthorizedByUserID       uuid.UUID
	Provider                 string
	Name                     string
	Status                   string
	EncryptedCredentials     []byte
	SyncCursor               *string
	LastSyncedAt             *time.Time
	LastAuthenticatedAt      *time.Time
	LastValidatedAt          *time.Time
	LastValidationStatus     *string
	LastValidationReportID   *uuid.UUID
	SessionVersion           int
	CredentialHash           *string
	FencingToken             int64
	ActiveOperationType      *string
	InitialImportCompletedAt *time.Time
	ErrorMessage             *string
	CreatedAt                time.Time
	UpdatedAt                time.Time
}

// InitialImportApproval represents a durable user approval for a bank connection's initial financial import.
type InitialImportApproval struct {
	ID                 uuid.UUID  `json:"id"`
	FamilyID           uuid.UUID  `json:"family_id"`
	BankConnectionID   uuid.UUID  `json:"bank_connection_id"`
	ValidationReportID uuid.UUID  `json:"validation_report_id"`
	SessionVersion     int        `json:"session_version"`
	CredentialHash     string     `json:"credential_hash"`
	ApprovedByUserID   uuid.UUID  `json:"approved_by_user_id"`
	ApprovedAt         time.Time  `json:"approved_at"`
	ConsumedAt         *time.Time `json:"consumed_at,omitempty"`
	InvalidatedAt      *time.Time `json:"invalidated_at,omitempty"`
	InvalidationReason *string    `json:"invalidation_reason,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

// ExistingTransactionSummary represents minimal state of an existing transaction for deduplication.
type ExistingTransactionSummary struct {
	ID                  uuid.UUID
	AccountID           uuid.UUID
	ExternalID          *string
	ExternalAuthID      *string
	FingerprintHash     string
	FingerprintSequence int
	AccountAmount       domain.Money
	BookingStatus       domain.BookingStatus
	OperationType       string
	TransactionTime     time.Time
}

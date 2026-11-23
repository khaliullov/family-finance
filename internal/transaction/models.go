package transaction

import (
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/khaliullov/family-finance/internal/domain"
)

var (
	ErrConnectionNotFound      = errors.New("bank connection not found")
	ErrConnectionNotActive     = errors.New("bank connection is not in an active or pairable state")
	ErrAccountNotFound         = errors.New("referenced bank account not found")
	ErrInvalidTransactionData  = errors.New("invalid transaction data from provider")
	ErrMismatchedTenantContext = errors.New("operation violates tenant isolation boundary")
)

// SyncOptions configures an ingestion run.
type SyncOptions struct {
	LookbackDays  int
	Cursor        string
	BatchLimit    int
	MaxPages      int
	ForceFullSync bool
}

// SyncStats captures telemetry from a synchronization run.
type SyncStats struct {
	RawPayloadsPreserved int
	AccountsDiscovered   int
	CardsDiscovered      int
	TransactionsIngested int
	TransactionsUpdated  int
	TransactionsSkipped  int
	PagesProcessed       int
}

// BankConnectionEntity represents a row from bank_connections table.
type BankConnectionEntity struct {
	ID                   uuid.UUID
	FamilyID             uuid.UUID
	AuthorizedByUserID   uuid.UUID
	Provider             string
	Name                 string
	Status               string
	EncryptedCredentials []byte
	LastSyncedAt         *time.Time
	ErrorMessage         *string
	CreatedAt            time.Time
	UpdatedAt            time.Time
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

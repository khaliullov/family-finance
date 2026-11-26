package transaction

import (
	"strings"
	"time"

	"github.com/google/uuid"
)

// ValidationOverallResult represents the aggregate outcome of a validation run.
type ValidationOverallResult string

const (
	ValidationResultPassed         ValidationOverallResult = "passed"
	ValidationResultPartialSuccess ValidationOverallResult = "partial_success"
	ValidationResultFailed         ValidationOverallResult = "failed"
)

// ValidationReport is the full structured validation report persisted for every attempt.
type ValidationReport struct {
	ID             uuid.UUID                `json:"id"`
	FamilyID       uuid.UUID                `json:"family_id"`
	ConnectionID   uuid.UUID                `json:"connection_id"`
	OverallResult  ValidationOverallResult  `json:"overall_result"`
	StartedAt      time.Time                `json:"started_at"`
	FinishedAt     time.Time                `json:"finished_at"`
	DurationMs     int64                    `json:"duration_ms"`
	Summary        ValidationSummary        `json:"summary"`
	Authentication AuthenticationValidation `json:"authentication"`
	Accounts       AccountsValidation       `json:"accounts"`
	Cards          CardsValidation          `json:"cards"`
	Balances       BalancesValidation       `json:"balances"`
	Transactions   TransactionsValidation   `json:"transactions"`
	DataIntegrity  DataIntegrityValidation  `json:"data_integrity"`
	Errors         []ValidationErrorItem    `json:"errors"`
	CreatedAt      time.Time                `json:"created_at"`
}

type ValidationSummary struct {
	ConnectionID     uuid.UUID               `json:"connection_id"`
	BankProvider     string                  `json:"bank_provider"`
	ValidationRunID  uuid.UUID               `json:"validation_run_id"`
	StartedAt        time.Time               `json:"started_at"`
	FinishedAt       time.Time               `json:"finished_at"`
	DurationMs       int64                   `json:"duration_ms"`
	OverallResult    ValidationOverallResult `json:"overall_result"`
	ConnectorVersion string                  `json:"connector_version,omitempty"`
}

type AuthenticationValidation struct {
	Authenticated  bool       `json:"authenticated"`
	SessionValid   bool       `json:"session_valid"`
	ReauthRequired bool       `json:"reauthentication_required"`
	ExpiresAt      *time.Time `json:"expires_at,omitempty"`
	Message        string     `json:"message,omitempty"`
}

type AccountValidationItem struct {
	ExternalIDMasked string `json:"external_id_masked"`
	Name             string `json:"name"`
	Type             string `json:"type"`
	Currency         string `json:"currency"`
	Status           string `json:"status"`
	Valid            bool   `json:"valid"`
	Error            string `json:"error,omitempty"`
}

type AccountsValidation struct {
	Discovered int                     `json:"discovered"`
	Valid      int                     `json:"valid"`
	Rejected   int                     `json:"rejected"`
	Items      []AccountValidationItem `json:"items"`
	Errors     []string                `json:"errors,omitempty"`
}

type CardValidationItem struct {
	ExternalIDMasked        string `json:"external_id_masked"`
	AccountExternalIDMasked string `json:"account_external_id_masked"`
	MaskedPAN               string `json:"masked_pan"`
	PaymentSystem           string `json:"payment_system"`
	IsVirtual               bool   `json:"is_virtual"`
	IsPrimary               bool   `json:"is_primary"`
	Status                  string `json:"status"`
	Valid                   bool   `json:"valid"`
	Error                   string `json:"error,omitempty"`
}

type CardsValidation struct {
	Discovered int                  `json:"discovered"`
	Valid      int                  `json:"valid"`
	Invalid    int                  `json:"invalid"`
	Items      []CardValidationItem `json:"items"`
	Errors     []string             `json:"errors,omitempty"`
}

type BalanceValidationItem struct {
	AccountExternalIDMasked string `json:"account_external_id_masked"`
	Currency                string `json:"currency"`
	CurrentAmountMinor      int64  `json:"current_amount_minor"`
	AvailableAmountMinor    *int64 `json:"available_amount_minor,omitempty"`
	Valid                   bool   `json:"valid"`
	Error                   string `json:"error,omitempty"`
}

type BalancesValidation struct {
	Checked int                     `json:"checked"`
	Items   []BalanceValidationItem `json:"items"`
	Errors  []string                `json:"errors,omitempty"`
}

type TransactionsValidation struct {
	Fetched                    int            `json:"fetched"`
	Valid                      int            `json:"valid"`
	Rejected                   int            `json:"rejected"`
	Duplicates                 int            `json:"duplicates"`
	CountsByStatus             map[string]int `json:"counts_by_status"`
	CountsByOperationType      map[string]int `json:"counts_by_operation_type"`
	UnsupportedStatuses        []string       `json:"unsupported_statuses,omitempty"`
	UnsupportedTypes           []string       `json:"unsupported_types,omitempty"`
	MissingRequiredIdentifiers int            `json:"missing_required_identifiers"`
	InvalidAmounts             int            `json:"invalid_amounts"`
	InvalidTimestamps          int            `json:"invalid_timestamps"`
	Errors                     []string       `json:"errors,omitempty"`
}

type DataIntegrityValidation struct {
	DuplicateExternalIDs  int  `json:"duplicate_external_ids"`
	OrphanCards           int  `json:"orphan_cards"`
	OrphanTransactions    int  `json:"orphan_transactions"`
	AccountMappingErrors  int  `json:"account_mapping_errors"`
	TenantIsolationPassed bool `json:"tenant_isolation_passed"`
	IdempotencyPassed     bool `json:"idempotency_passed"`
}

type ValidationErrorItem struct {
	Code           string `json:"code"`
	Message        string `json:"message"`
	Details        string `json:"details,omitempty"`
	Stage          string `json:"stage"` // "authentication", "accounts", "cards", "balances", "transactions", "data_integrity"
	Retryable      bool   `json:"retryable"`
	ReauthRequired bool   `json:"reauthentication_required"`
}

// MaskIdentifier masks sensitive external identifiers for safe UI and reporting display.
func MaskIdentifier(id string) string {
	trimmed := strings.TrimSpace(id)
	if len(trimmed) == 0 {
		return "****"
	}
	if len(trimmed) <= 4 {
		return "****"
	}
	return "****" + trimmed[len(trimmed)-4:]
}

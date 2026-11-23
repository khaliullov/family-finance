package connector

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Standard typed sentinel errors for connector failure classification.
var (
	ErrAuthExpired          = errors.New("bank connection authentication expired or revoked")
	ErrRateLimited          = errors.New("provider rate limit exceeded")
	ErrTemporary            = errors.New("temporary provider or network failure")
	ErrInvalidResponse      = errors.New("provider returned invalid or malformed response")
	ErrUnsupportedOperation = errors.New("operation not supported by provider")
	ErrConnectorNotFound    = errors.New("bank connector not found in registry")
)

// CurrencyAmount represents minor monetary units with ISO 4217 currency.
type CurrencyAmount struct {
	MinorUnits int64  `json:"minor_units"` // e.g. 150050 for 1,500.50 RUB
	Currency   string `json:"currency"`    // ISO 4217, e.g. "RUB", "USD", "EUR"
}

// AccountDTO represents an external account instrument discovered from a provider.
type AccountDTO struct {
	ExternalID       string          `json:"external_id"`
	Name             string          `json:"name"`
	Type             string          `json:"type"` // "checking", "credit_card", "savings", "deposit"
	Currency         string          `json:"currency"`
	Balance          *CurrencyAmount `json:"balance,omitempty"`
	AvailableBalance *CurrencyAmount `json:"available_balance,omitempty"`
	DebtAmount       *CurrencyAmount `json:"debt_amount,omitempty"`
	CreditLimit      *CurrencyAmount `json:"credit_limit,omitempty"`
	StatementBalance *CurrencyAmount `json:"statement_balance,omitempty"`
	MinimalPayment   *CurrencyAmount `json:"minimal_payment,omitempty"`
	GracePeriodDate  *time.Time      `json:"grace_period_date,omitempty"`
	Cards            []CardDTO       `json:"cards,omitempty"`
}

// CardDTO represents an access card instrument attached to an account.
type CardDTO struct {
	ExternalID        string `json:"external_id"`
	AccountExternalID string `json:"account_external_id"`
	MaskedPAN         string `json:"masked_pan"` // e.g. "*1234"
	DisplayName       string `json:"display_name"`
	PaymentSystem     string `json:"payment_system"` // "MIR", "VISA", "MASTERCARD", "OTHER"
	IsVirtual         bool   `json:"is_virtual"`
	IsPrimary         bool   `json:"is_primary"`
	Status            string `json:"status"` // "ACTIVE", "BLOCKED", "CLOSED"
}

// BalanceDTO represents observed balance telemetry for point-in-time updates.
type BalanceDTO struct {
	AccountExternalID string          `json:"account_external_id"`
	CurrentBalance    *CurrencyAmount `json:"current_balance,omitempty"`
	AvailableBalance  *CurrencyAmount `json:"available_balance,omitempty"`
	DebtBalance       *CurrencyAmount `json:"debt_balance,omitempty"`
	CreditLimit       *CurrencyAmount `json:"credit_limit,omitempty"`
	ObservedAt        time.Time       `json:"observed_at"`
	Source            string          `json:"source"` // "bank_api", "statement_file", "calculated"
}

// CanonicalBankTransaction represents a normalized bank transaction event.
// Completely decoupled from external provider-specific JSON schemas.
type CanonicalBankTransaction struct {
	ExternalID        string         `json:"external_id"`
	ExternalAuthID    *string        `json:"external_auth_id,omitempty"`
	AccountExternalID string         `json:"account_external_id"`
	CardExternalID    *string        `json:"card_external_id,omitempty"`
	Amount            CurrencyAmount `json:"amount"`         // Transaction amount in merchant currency
	AccountAmount     CurrencyAmount `json:"account_amount"` // Cleared amount in account currency
	TransactionTime   time.Time      `json:"transaction_time"`
	SettledTime       *time.Time     `json:"settled_time,omitempty"`
	BookingStatus     string         `json:"booking_status"` // "pending", "posted", "cancelled"
	OperationType     string         `json:"operation_type"` // "purchase", "refund", "transfer_out", "transfer_in", "atm_cash", "fee", "card_check"
	RelatedExternalID *string        `json:"related_external_id,omitempty"`
	MerchantName      string         `json:"merchant_name"`
	RawDescription    string         `json:"raw_description"`
	MCC               *int           `json:"mcc,omitempty"`
	BankCategory      string         `json:"bank_category"`
	RawPayload        []byte         `json:"-"` // Exact verbatim payload for non-repudiation audit
}

// SyncParams encapsulates filter parameters and pagination cursors for synchronization.
type SyncParams struct {
	StartDate time.Time `json:"start_date"`
	EndDate   time.Time `json:"end_date"`
	Accounts  []string  `json:"accounts,omitempty"`
	Cursor    string    `json:"cursor,omitempty"`
	Limit     int       `json:"limit,omitempty"`
}

// SyncProgress provides telemetry on batch progress for background workers.
type SyncProgress struct {
	CurrentPage    int    `json:"current_page"`
	RecordsFetched int    `json:"records_fetched"`
	HasMore        bool   `json:"has_more"`
	NextCursor     string `json:"next_cursor,omitempty"`
}

// SyncResult encapsulates canonical transaction events alongside pagination state.
type SyncResult struct {
	Transactions []CanonicalBankTransaction `json:"transactions"`
	Progress     SyncProgress               `json:"progress"`
	RawPayload   []byte                     `json:"raw_payload,omitempty"` // Verbatim batch response payload
}

// SyncError represents classified synchronization failures with retryability metadata.
type SyncError struct {
	ProviderCode string `json:"provider_code"`
	ErrorCode    string `json:"error_code"`
	Message      string `json:"message"`
	Retryable    bool   `json:"retryable"`
	Underlying   error  `json:"-"`
}

func (e *SyncError) Error() string {
	if e.Underlying != nil {
		return fmt.Sprintf("[%s] %s: %s (underlying: %v)", e.ProviderCode, e.ErrorCode, e.Message, e.Underlying)
	}
	return fmt.Sprintf("[%s] %s: %s", e.ProviderCode, e.ErrorCode, e.Message)
}

func (e *SyncError) Unwrap() error {
	return e.Underlying
}

func (e *SyncError) Is(target error) bool {
	if errors.Is(e.Underlying, target) {
		return true
	}
	switch target {
	case ErrAuthExpired:
		return e.ErrorCode == "AUTH_EXPIRED"
	case ErrRateLimited:
		return e.ErrorCode == "RATE_LIMITED"
	case ErrTemporary:
		return e.Retryable || e.ErrorCode == "TEMPORARY_FAILURE" || e.ErrorCode == "NETWORK_TIMEOUT"
	case ErrInvalidResponse:
		return e.ErrorCode == "INVALID_RESPONSE"
	case ErrUnsupportedOperation:
		return e.ErrorCode == "UNSUPPORTED_OPERATION"
	default:
		return false
	}
}

// NewSyncError builds a classified SyncError.
func NewSyncError(providerCode, errorCode, message string, retryable bool, underlying error) *SyncError {
	return &SyncError{
		ProviderCode: providerCode,
		ErrorCode:    errorCode,
		Message:      message,
		Retryable:    retryable,
		Underlying:   underlying,
	}
}

// BankConnector defines the provider-independent integration contract.
type BankConnector interface {
	// ProviderCode returns the unique provider identifier (e.g. "tbank", "alfa", "sber", "fixture").
	ProviderCode() string

	// HealthCheck tests connectivity, transport reachability, and credential validity.
	HealthCheck(ctx context.Context, credentials []byte) error

	// FetchAccounts retrieves all accounts and nested card instruments.
	FetchAccounts(ctx context.Context, credentials []byte) ([]AccountDTO, error)

	// FetchCards retrieves cards explicitly for a given account.
	FetchCards(ctx context.Context, credentials []byte, accountExternalID string) ([]CardDTO, error)

	// FetchBalances retrieves real-time balance positions for specific accounts.
	FetchBalances(ctx context.Context, credentials []byte, accountExternalIDs []string) ([]BalanceDTO, error)

	// FetchTransactions retrieves canonical transaction events matching the sync parameters.
	FetchTransactions(ctx context.Context, credentials []byte, params SyncParams) (*SyncResult, error)
}

// Registry manages registered bank connectors thread-safely.
type Registry struct {
	mu         sync.RWMutex
	connectors map[string]BankConnector
}

// NewRegistry initializes an empty BankConnector registry.
func NewRegistry() *Registry {
	return &Registry{
		connectors: make(map[string]BankConnector),
	}
}

// Register registers a connector under its ProviderCode.
func (r *Registry) Register(c BankConnector) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.connectors[c.ProviderCode()] = c
}

// Get returns the connector registered for the specified provider code.
func (r *Registry) Get(providerCode string) (BankConnector, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.connectors[providerCode]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrConnectorNotFound, providerCode)
	}
	return c, nil
}

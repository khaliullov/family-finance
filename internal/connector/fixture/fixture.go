package fixture

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"time"

	"github.com/khaliullov/family-finance/internal/connector"
)

//go:embed testdata/fixtures/v1/*.json
var defaultFixtureFS embed.FS

const (
	DefaultProviderCode = "fixture"
	FixtureVersionV1    = "v1"
)

// FixtureConnector implements connector.BankConnector using recorded versioned JSON fixtures.
type FixtureConnector struct {
	providerCode string
	fixtureFS    fs.FS
	version      string
}

// Option allows customizing the FixtureConnector.
type Option func(*FixtureConnector)

// WithProviderCode customizes the provider code returned.
func WithProviderCode(code string) Option {
	return func(fc *FixtureConnector) {
		fc.providerCode = code
	}
}

// WithFS overrides the embedded fixture filesystem.
func WithFS(fileSys fs.FS) Option {
	return func(fc *FixtureConnector) {
		fc.fixtureFS = fileSys
	}
}

// WithVersion sets the fixture version directory (defaults to "v1").
func WithVersion(v string) Option {
	return func(fc *FixtureConnector) {
		fc.version = v
	}
}

// NewFixtureConnector creates a new FixtureConnector instance.
func NewFixtureConnector(opts ...Option) *FixtureConnector {
	fc := &FixtureConnector{
		providerCode: DefaultProviderCode,
		fixtureFS:    defaultFixtureFS,
		version:      FixtureVersionV1,
	}
	for _, opt := range opts {
		opt(fc)
	}
	return fc
}

func (fc *FixtureConnector) ProviderCode() string {
	return fc.providerCode
}

func (fc *FixtureConnector) checkSimulatedError(credentials []byte) error {
	credStr := string(credentials)
	switch {
	case strings.Contains(credStr, "fail:auth"):
		return connector.NewSyncError(fc.providerCode, "AUTH_EXPIRED", "simulated auth token expired or revoked", false, connector.ErrAuthExpired)
	case strings.Contains(credStr, "fail:rate_limit"):
		return connector.NewSyncError(fc.providerCode, "RATE_LIMITED", "simulated upstream bank rate limit reached", true, connector.ErrRateLimited)
	case strings.Contains(credStr, "fail:temporary"):
		return connector.NewSyncError(fc.providerCode, "TEMPORARY_FAILURE", "simulated temporary network timeout", true, connector.ErrTemporary)
	case strings.Contains(credStr, "fail:invalid"):
		return connector.NewSyncError(fc.providerCode, "INVALID_RESPONSE", "simulated malformed vendor JSON envelope", false, connector.ErrInvalidResponse)
	case strings.Contains(credStr, "fail:unsupported"):
		return connector.NewSyncError(fc.providerCode, "UNSUPPORTED_OPERATION", "simulated unsupported banking operation", false, connector.ErrUnsupportedOperation)
	default:
		return nil
	}
}

func (fc *FixtureConnector) readFixtureFile(filename string) ([]byte, error) {
	path := fmt.Sprintf("testdata/fixtures/%s/%s", fc.version, filename)
	f, err := fc.fixtureFS.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open fixture file %s: %w", path, err)
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("failed to read fixture file %s: %w", path, err)
	}
	return data, nil
}

func (fc *FixtureConnector) HealthCheck(ctx context.Context, credentials []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return fc.checkSimulatedError(credentials)
}

type accountsFixture struct {
	Version  string                 `json:"version"`
	Provider string                 `json:"provider"`
	Accounts []connector.AccountDTO `json:"accounts"`
}

func (fc *FixtureConnector) FetchAccounts(ctx context.Context, credentials []byte) ([]connector.AccountDTO, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := fc.checkSimulatedError(credentials); err != nil {
		return nil, err
	}

	data, err := fc.readFixtureFile("accounts.json")
	if err != nil {
		return nil, connector.NewSyncError(fc.providerCode, "SCHEMA_DRIFT", "missing accounts fixture", false, err)
	}

	var parsed accountsFixture
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, connector.NewSyncError(fc.providerCode, "INVALID_RESPONSE", "malformed accounts fixture JSON", false, err)
	}

	return parsed.Accounts, nil
}

func (fc *FixtureConnector) FetchCards(ctx context.Context, credentials []byte, accountExternalID string) ([]connector.CardDTO, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := fc.checkSimulatedError(credentials); err != nil {
		return nil, err
	}

	accounts, err := fc.FetchAccounts(ctx, credentials)
	if err != nil {
		return nil, err
	}

	for _, acc := range accounts {
		if acc.ExternalID == accountExternalID {
			return acc.Cards, nil
		}
	}

	return nil, nil
}

func (fc *FixtureConnector) FetchBalances(ctx context.Context, credentials []byte, accountExternalIDs []string) ([]connector.BalanceDTO, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := fc.checkSimulatedError(credentials); err != nil {
		return nil, err
	}

	accounts, err := fc.FetchAccounts(ctx, credentials)
	if err != nil {
		return nil, err
	}

	filterMap := make(map[string]bool)
	for _, id := range accountExternalIDs {
		filterMap[id] = true
	}

	now := time.Now().UTC()
	var balances []connector.BalanceDTO
	for _, acc := range accounts {
		if len(filterMap) > 0 && !filterMap[acc.ExternalID] {
			continue
		}
		balances = append(balances, connector.BalanceDTO{
			AccountExternalID: acc.ExternalID,
			CurrentBalance:    acc.Balance,
			AvailableBalance:  acc.AvailableBalance,
			DebtBalance:       acc.DebtAmount,
			CreditLimit:       acc.CreditLimit,
			ObservedAt:        now,
			Source:            "bank_api",
		})
	}

	return balances, nil
}

type transactionsFixture struct {
	Version      string                               `json:"version"`
	Provider     string                               `json:"provider"`
	Progress     connector.SyncProgress               `json:"progress"`
	Transactions []connector.CanonicalBankTransaction `json:"transactions"`
}

func (fc *FixtureConnector) FetchTransactions(ctx context.Context, credentials []byte, params connector.SyncParams) (*connector.SyncResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := fc.checkSimulatedError(credentials); err != nil {
		return nil, err
	}

	filename := "transactions_page1.json"
	switch params.Cursor {
	case "cursor_page_2":
		filename = "transactions_page2.json"
	case "cursor_settled":
		filename = "transactions_settled.json"
	case "cursor_duplicates":
		filename = "transactions_duplicates.json"
	case "cursor_empty":
		filename = "empty.json"
	default:
		// page 1
		filename = "transactions_page1.json"
	}

	data, err := fc.readFixtureFile(filename)
	if err != nil {
		return nil, connector.NewSyncError(fc.providerCode, "SCHEMA_DRIFT", fmt.Sprintf("fixture file %s not found", filename), false, err)
	}

	var parsed transactionsFixture
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, connector.NewSyncError(fc.providerCode, "INVALID_RESPONSE", "malformed transactions fixture JSON", false, err)
	}

	// Attach verbatim raw payload bytes to transactions for evidence immutability
	for i := range parsed.Transactions {
		parsed.Transactions[i].RawPayload = data
	}

	return &connector.SyncResult{
		Transactions: parsed.Transactions,
		Progress:     parsed.Progress,
		RawPayload:   data,
	}, nil
}

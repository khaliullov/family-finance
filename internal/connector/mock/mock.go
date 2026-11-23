package mock

import (
	"context"
	"sync"

	"github.com/khaliullov/family-finance/internal/connector"
)

// MockConnector is a thread-safe, configurable mock implementation of connector.BankConnector.
type MockConnector struct {
	mu sync.RWMutex

	ProviderCodeValue string

	HealthCheckFunc       func(ctx context.Context, credentials []byte) error
	FetchAccountsFunc     func(ctx context.Context, credentials []byte) ([]connector.AccountDTO, error)
	FetchCardsFunc        func(ctx context.Context, credentials []byte, accountExternalID string) ([]connector.CardDTO, error)
	FetchBalancesFunc     func(ctx context.Context, credentials []byte, accountExternalIDs []string) ([]connector.BalanceDTO, error)
	FetchTransactionsFunc func(ctx context.Context, credentials []byte, params connector.SyncParams) (*connector.SyncResult, error)

	// Call records
	HealthCheckCalls       int
	FetchAccountsCalls     int
	FetchCardsCalls        int
	FetchBalancesCalls     int
	FetchTransactionsCalls int

	LastCredentials []byte
	LastSyncParams  connector.SyncParams
}

// NewMockConnector returns a mock connector with default provider "mock".
func NewMockConnector(providerCode ...string) *MockConnector {
	code := "mock"
	if len(providerCode) > 0 && providerCode[0] != "" {
		code = providerCode[0]
	}
	return &MockConnector{
		ProviderCodeValue: code,
	}
}

func (m *MockConnector) ProviderCode() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.ProviderCodeValue
}

func (m *MockConnector) HealthCheck(ctx context.Context, credentials []byte) error {
	m.mu.Lock()
	m.HealthCheckCalls++
	m.LastCredentials = credentials
	fn := m.HealthCheckFunc
	m.mu.Unlock()

	if fn != nil {
		return fn(ctx, credentials)
	}
	return nil
}

func (m *MockConnector) FetchAccounts(ctx context.Context, credentials []byte) ([]connector.AccountDTO, error) {
	m.mu.Lock()
	m.FetchAccountsCalls++
	m.LastCredentials = credentials
	fn := m.FetchAccountsFunc
	m.mu.Unlock()

	if fn != nil {
		return fn(ctx, credentials)
	}
	return nil, nil
}

func (m *MockConnector) FetchCards(ctx context.Context, credentials []byte, accountExternalID string) ([]connector.CardDTO, error) {
	m.mu.Lock()
	m.FetchCardsCalls++
	m.LastCredentials = credentials
	fn := m.FetchCardsFunc
	m.mu.Unlock()

	if fn != nil {
		return fn(ctx, credentials, accountExternalID)
	}
	return nil, nil
}

func (m *MockConnector) FetchBalances(ctx context.Context, credentials []byte, accountExternalIDs []string) ([]connector.BalanceDTO, error) {
	m.mu.Lock()
	m.FetchBalancesCalls++
	m.LastCredentials = credentials
	fn := m.FetchBalancesFunc
	m.mu.Unlock()

	if fn != nil {
		return fn(ctx, credentials, accountExternalIDs)
	}
	return nil, nil
}

func (m *MockConnector) FetchTransactions(ctx context.Context, credentials []byte, params connector.SyncParams) (*connector.SyncResult, error) {
	m.mu.Lock()
	m.FetchTransactionsCalls++
	m.LastCredentials = credentials
	m.LastSyncParams = params
	fn := m.FetchTransactionsFunc
	m.mu.Unlock()

	if fn != nil {
		return fn(ctx, credentials, params)
	}
	return &connector.SyncResult{
		Transactions: nil,
		Progress: connector.SyncProgress{
			HasMore: false,
		},
	}, nil
}

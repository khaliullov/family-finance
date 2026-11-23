package mock_test

import (
	"context"
	"errors"
	"testing"

	"github.com/khaliullov/family-finance/internal/connector"
	"github.com/khaliullov/family-finance/internal/connector/mock"
)

func TestMockConnector(t *testing.T) {
	m := mock.NewMockConnector("custom_mock")
	ctx := context.Background()

	if m.ProviderCode() != "custom_mock" {
		t.Fatalf("expected custom_mock, got %s", m.ProviderCode())
	}

	// Test HealthCheck
	customErr := errors.New("auth failed")
	m.HealthCheckFunc = func(ctx context.Context, creds []byte) error {
		return customErr
	}
	if err := m.HealthCheck(ctx, []byte("secret")); !errors.Is(err, customErr) {
		t.Fatalf("expected customErr, got %v", err)
	}
	if m.HealthCheckCalls != 1 || string(m.LastCredentials) != "secret" {
		t.Fatalf("unexpected call tracking")
	}

	// Test FetchAccounts
	m.FetchAccountsFunc = func(ctx context.Context, creds []byte) ([]connector.AccountDTO, error) {
		return []connector.AccountDTO{{ExternalID: "acc_1"}}, nil
	}
	accs, err := m.FetchAccounts(ctx, nil)
	if err != nil || len(accs) != 1 || accs[0].ExternalID != "acc_1" {
		t.Fatalf("unexpected FetchAccounts result: %v, %v", accs, err)
	}

	// Test FetchCards
	m.FetchCardsFunc = func(ctx context.Context, creds []byte, accExtID string) ([]connector.CardDTO, error) {
		return []connector.CardDTO{{ExternalID: "card_1", AccountExternalID: accExtID}}, nil
	}
	cards, err := m.FetchCards(ctx, nil, "acc_1")
	if err != nil || len(cards) != 1 || cards[0].ExternalID != "card_1" {
		t.Fatalf("unexpected FetchCards result: %v, %v", cards, err)
	}

	// Test FetchBalances
	m.FetchBalancesFunc = func(ctx context.Context, creds []byte, accExtIDs []string) ([]connector.BalanceDTO, error) {
		return []connector.BalanceDTO{{AccountExternalID: accExtIDs[0]}}, nil
	}
	bals, err := m.FetchBalances(ctx, nil, []string{"acc_1"})
	if err != nil || len(bals) != 1 {
		t.Fatalf("unexpected FetchBalances result: %v, %v", bals, err)
	}

	// Test FetchTransactions
	m.FetchTransactionsFunc = func(ctx context.Context, creds []byte, params connector.SyncParams) (*connector.SyncResult, error) {
		return &connector.SyncResult{
			Transactions: []connector.CanonicalBankTransaction{{ExternalID: "tx_1"}},
		}, nil
	}
	res, err := m.FetchTransactions(ctx, nil, connector.SyncParams{Cursor: "c1"})
	if err != nil || len(res.Transactions) != 1 || res.Transactions[0].ExternalID != "tx_1" {
		t.Fatalf("unexpected FetchTransactions result: %v, %v", res, err)
	}
	if m.LastSyncParams.Cursor != "c1" {
		t.Fatalf("expected cursor c1, got %s", m.LastSyncParams.Cursor)
	}
}

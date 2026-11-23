package tbank_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/khaliullov/family-finance/internal/connector"
	"github.com/khaliullov/family-finance/internal/connector/tbank"
	"github.com/khaliullov/family-finance/internal/connector/tbank/personal"
	"github.com/khaliullov/family-finance/internal/crypto"
)

func TestPersonalConnector_EndToEndIntegration(t *testing.T) {
	accountsFixture, err := os.ReadFile(filepath.Join("personal", "testdata", "accounts_light_ib.json"))
	if err != nil {
		t.Fatal(err)
	}
	opsFixture, err := os.ReadFile(filepath.Join("personal", "testdata", "operations_timeline.json"))
	if err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/common/v1/accounts_light_ib":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(accountsFixture)
		case "/mybank/api/operations/timeline/public/legacy/v1/operations":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(opsFixture)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	key, err := crypto.GenerateRandomKey()
	if err != nil {
		t.Fatal(err)
	}

	conn, err := tbank.NewPersonalConnector(
		tbank.WithCryptoKey(key),
		tbank.WithBaseURL(ts.URL),
	)
	if err != nil {
		t.Fatal(err)
	}

	if conn.ProviderCode() != tbank.ProviderCodeDefault {
		t.Errorf("ProviderCode = %q; want %q", conn.ProviderCode(), tbank.ProviderCodeDefault)
	}

	familyID := uuid.New()
	connectionID := uuid.New()
	session := personal.NewSession(familyID, connectionID, []*http.Cookie{
		{Name: "sessionid", Value: "test_session_active"},
	}, 24*time.Hour)

	encryptedSessionBytes, err := session.Encrypt(key)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()

	// 1. HealthCheck
	if err := conn.HealthCheck(ctx, encryptedSessionBytes); err != nil {
		t.Fatalf("HealthCheck failed: %v", err)
	}

	// 2. FetchAccounts
	accounts, err := conn.FetchAccounts(ctx, encryptedSessionBytes)
	if err != nil {
		t.Fatalf("FetchAccounts failed: %v", err)
	}
	if len(accounts) != 2 {
		t.Fatalf("expected 2 accounts, got %d", len(accounts))
	}

	// 3. FetchCards
	cards, err := conn.FetchCards(ctx, encryptedSessionBytes, "acc_debit_01")
	if err != nil {
		t.Fatalf("FetchCards failed: %v", err)
	}
	if len(cards) != 1 || cards[0].ExternalID != "card_mir_01" {
		t.Fatalf("FetchCards mismatch: %+v", cards)
	}

	// 4. FetchBalances
	balances, err := conn.FetchBalances(ctx, encryptedSessionBytes, []string{"acc_debit_01"})
	if err != nil {
		t.Fatalf("FetchBalances failed: %v", err)
	}
	if len(balances) != 1 || balances[0].AccountExternalID != "acc_debit_01" {
		t.Fatalf("FetchBalances mismatch: %+v", balances)
	}

	// 5. FetchTransactions
	res, err := conn.FetchTransactions(ctx, encryptedSessionBytes, connector.SyncParams{})
	if err != nil {
		t.Fatalf("FetchTransactions failed: %v", err)
	}
	if len(res.Transactions) != 4 {
		t.Fatalf("FetchTransactions expected 4 transactions, got %d", len(res.Transactions))
	}
}

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

func TestPersonalConnector_PlaintextAndInvalidKeysRejected(t *testing.T) {
	// 1. Missing crypto key at initialization must fail closed
	_, err := tbank.NewPersonalConnector()
	if err == nil {
		t.Fatal("expected error creating connector without crypto key, got nil")
	}

	// 2. Short / invalid key length must fail closed
	_, err = tbank.NewPersonalConnector(tbank.WithCryptoKey([]byte("too-short-key")))
	if err == nil {
		t.Fatal("expected error creating connector with 13-byte key, got nil")
	}

	key, err := crypto.GenerateRandomKey()
	if err != nil {
		t.Fatal(err)
	}

	conn, err := tbank.NewPersonalConnector(tbank.WithCryptoKey(key))
	if err != nil {
		t.Fatalf("unexpected error creating connector: %v", err)
	}

	ctx := context.Background()

	// 3. Plaintext JSON Session object must be strictly rejected
	plaintextJSONSession := []byte(`{"family_id":"00000000-0000-0000-0000-000000000001","cookies":[{"name":"sessionid","value":"leaked_val"}]}`)
	_, err = conn.FetchAccounts(ctx, plaintextJSONSession)
	if err == nil {
		t.Fatal("expected error rejecting plaintext JSON Session credentials, got nil")
	}

	// 4. Simple JSON session_id payload must be strictly rejected
	simpleJSONPayload := []byte(`{"session_id":"raw_session_123"}`)
	_, err = conn.FetchAccounts(ctx, simpleJSONPayload)
	if err == nil {
		t.Fatal("expected error rejecting simple session_id credentials, got nil")
	}

	// 5. Empty credentials must be rejected
	_, err = conn.FetchAccounts(ctx, nil)
	if err == nil {
		t.Fatal("expected error rejecting nil credentials, got nil")
	}

	// 6. Malformed ciphertext (random bytes) must be rejected
	malformedCiphertext := []byte("this-is-not-valid-aes-gcm-ciphertext-1234567890")
	_, err = conn.FetchAccounts(ctx, malformedCiphertext)
	if err == nil {
		t.Fatal("expected error rejecting malformed ciphertext, got nil")
	}

	// 7. Ciphertext encrypted with a DIFFERENT 32-byte key must be rejected (auth failure)
	otherKey, _ := crypto.GenerateRandomKey()
	otherSession := personal.NewSession(uuid.New(), uuid.New(), []*http.Cookie{
		{Name: "sessionid", Value: "wrong_key_session"},
	}, 24*time.Hour)
	wrongKeyCiphertext, err := otherSession.Encrypt(otherKey)
	if err != nil {
		t.Fatal(err)
	}

	_, err = conn.FetchAccounts(ctx, wrongKeyCiphertext)
	if err == nil {
		t.Fatal("expected authentication failure when decrypting with wrong key, got nil")
	}
}

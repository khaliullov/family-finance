package tbank_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

func TestPersonalConnector_SessionExpiration(t *testing.T) {
	key, err := crypto.GenerateRandomKey()
	if err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"resultCode":"OK","payload":[]}`))
	}))
	defer ts.Close()

	conn, err := tbank.NewPersonalConnector(
		tbank.WithCryptoKey(key),
		tbank.WithBaseURL(ts.URL),
	)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()

	// 1. Session expired by TTL (ExpiresAt in the past)
	expiredSession := personal.NewSession(uuid.New(), uuid.New(), []*http.Cookie{
		{Name: "sessionid", Value: "expired_token"},
	}, 1*time.Hour)
	expiredSession.ExpiresAt = time.Now().UTC().Add(-1 * time.Hour)
	expiredSession.Status = personal.SessionStatusExpired

	expiredCiphertext, err := expiredSession.Encrypt(key)
	if err != nil {
		t.Fatal(err)
	}

	_, err = conn.FetchAccounts(ctx, expiredCiphertext)
	if err == nil || !errors.Is(err, connector.ErrAuthExpired) {
		t.Fatalf("expected ErrAuthExpired for expired session, got: %v", err)
	}

	// 2. HealthCheck with expired session returns ErrAuthExpired
	if err := conn.HealthCheck(ctx, expiredCiphertext); !errors.Is(err, connector.ErrAuthExpired) {
		t.Fatalf("HealthCheck expected ErrAuthExpired, got: %v", err)
	}
}

func TestPersonalConnector_RetryAfterTemporaryFailure(t *testing.T) {
	key, err := crypto.GenerateRandomKey()
	if err != nil {
		t.Fatal(err)
	}

	var returnStatus int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(returnStatus)
		_, _ = w.Write([]byte(`{"resultCode":"FAIL","errorMessage":"temporary downstream outage"}`))
	}))
	defer ts.Close()

	conn, err := tbank.NewPersonalConnector(
		tbank.WithCryptoKey(key),
		tbank.WithBaseURL(ts.URL),
	)
	if err != nil {
		t.Fatal(err)
	}

	session := personal.NewSession(uuid.New(), uuid.New(), []*http.Cookie{
		{Name: "sessionid", Value: "valid_token"},
	}, 24*time.Hour)
	cred, err := session.Encrypt(key)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()

	// 1. HTTP 503 -> Retryable Temporary Failure
	returnStatus = http.StatusServiceUnavailable
	_, err = conn.FetchAccounts(ctx, cred)
	if err == nil {
		t.Fatal("expected error on HTTP 503, got nil")
	}
	var syncErr *connector.SyncError
	if !errors.As(err, &syncErr) {
		t.Fatalf("expected *connector.SyncError, got %T: %v", err, err)
	}
	if !syncErr.Retryable {
		t.Errorf("expected Retryable == true for HTTP 503")
	}
	if !errors.Is(err, connector.ErrTemporary) {
		t.Errorf("expected errors.Is(err, ErrTemporary)")
	}

	// 2. HTTP 429 -> Retryable Rate Limit
	returnStatus = http.StatusTooManyRequests
	_, err = conn.FetchAccounts(ctx, cred)
	if err == nil {
		t.Fatal("expected error on HTTP 429, got nil")
	}
	if !errors.As(err, &syncErr) {
		t.Fatalf("expected *connector.SyncError, got %T: %v", err, err)
	}
	if !syncErr.Retryable {
		t.Errorf("expected Retryable == true for HTTP 429")
	}
	if !errors.Is(err, connector.ErrRateLimited) {
		t.Errorf("expected errors.Is(err, ErrRateLimited)")
	}
}

func TestPersonalConnector_PaginationBoundariesAndLimit(t *testing.T) {
	opsFixture, err := os.ReadFile(filepath.Join("personal", "testdata", "operations_timeline.json"))
	if err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/mybank/api/operations/timeline/public/legacy/v1/operations" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(opsFixture)
		} else {
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	key, _ := crypto.GenerateRandomKey()
	conn, _ := tbank.NewPersonalConnector(tbank.WithCryptoKey(key), tbank.WithBaseURL(ts.URL))

	session := personal.NewSession(uuid.New(), uuid.New(), []*http.Cookie{
		{Name: "sessionid", Value: "valid_token"},
	}, 24*time.Hour)
	cred, _ := session.Encrypt(key)
	ctx := context.Background()

	// 1. Unverified cursor pagination must be rejected (no speculative cursor support)
	_, err = conn.FetchTransactions(ctx, cred, connector.SyncParams{
		Cursor: "some_unverified_cursor",
	})
	if err == nil || !errors.Is(err, connector.ErrUnsupportedOperation) {
		t.Fatalf("expected ErrUnsupportedOperation for cursor pagination, got: %v", err)
	}

	// 2. Limit parameter is respected (bounded batching)
	resLimit, err := conn.FetchTransactions(ctx, cred, connector.SyncParams{
		Limit: 2,
	})
	if err != nil {
		t.Fatalf("FetchTransactions with limit failed: %v", err)
	}
	if len(resLimit.Transactions) != 2 {
		t.Errorf("expected 2 transactions with Limit=2, got %d", len(resLimit.Transactions))
	}

	// 3. Account filtering boundary
	resAccount, err := conn.FetchTransactions(ctx, cred, connector.SyncParams{
		Accounts: []string{"acc_debit_01"},
	})
	if err != nil {
		t.Fatalf("FetchTransactions with account filter failed: %v", err)
	}
	for _, tx := range resAccount.Transactions {
		if tx.AccountExternalID != "acc_debit_01" {
			t.Errorf("expected transaction for acc_debit_01, got %s", tx.AccountExternalID)
		}
	}
}

func TestPersonalConnector_CredentialIsolationBetweenConnections(t *testing.T) {
	key, _ := crypto.GenerateRandomKey()

	var receivedCookiesMu sync.Mutex
	receivedCookies := make([]string, 0)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedCookiesMu.Lock()
		receivedCookies = append(receivedCookies, r.Header.Get("Cookie"))
		receivedCookiesMu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"resultCode":"OK","payload":[]}`))
	}))
	defer ts.Close()

	conn, _ := tbank.NewPersonalConnector(tbank.WithCryptoKey(key), tbank.WithBaseURL(ts.URL))
	ctx := context.Background()

	// Connection 1 session
	sess1 := personal.NewSession(uuid.New(), uuid.New(), []*http.Cookie{
		{Name: "sessionid", Value: "isolated_cookie_conn1"},
	}, 24*time.Hour)
	cred1, _ := sess1.Encrypt(key)

	// Connection 2 session
	sess2 := personal.NewSession(uuid.New(), uuid.New(), []*http.Cookie{
		{Name: "sessionid", Value: "isolated_cookie_conn2"},
	}, 24*time.Hour)
	cred2, _ := sess2.Encrypt(key)

	// Fetch with conn1
	_, err := conn.FetchAccounts(ctx, cred1)
	if err != nil {
		t.Fatal(err)
	}

	// Fetch with conn2
	_, err = conn.FetchAccounts(ctx, cred2)
	if err != nil {
		t.Fatal(err)
	}

	receivedCookiesMu.Lock()
	defer receivedCookiesMu.Unlock()

	if len(receivedCookies) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(receivedCookies))
	}

	if receivedCookies[0] != "sessionid=isolated_cookie_conn1" {
		t.Errorf("conn1 cookie mismatch: got %q", receivedCookies[0])
	}
	if receivedCookies[1] != "sessionid=isolated_cookie_conn2" {
		t.Errorf("conn2 cookie mismatch: got %q", receivedCookies[1])
	}
}

func TestPersonalConnector_NoSensitiveValuesInRedaction(t *testing.T) {
	secretValue := "ultra_secret_cookie_token_999"
	sess := personal.NewSession(uuid.New(), uuid.New(), []*http.Cookie{
		{Name: "sessionid", Value: secretValue},
	}, 24*time.Hour)

	str := sess.String()
	if strings.Contains(str, secretValue) {
		t.Errorf("session.String() leaked secret cookie value: %s", str)
	}
}

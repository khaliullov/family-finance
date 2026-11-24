package personal_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/khaliullov/family-finance/internal/connector"
	"github.com/khaliullov/family-finance/internal/connector/tbank/personal"
)

func TestPersonalClient_FetchAccounts_Success(t *testing.T) {
	fixtureData, err := os.ReadFile(filepath.Join("testdata", "accounts_light_ib.json"))
	if err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/common/v1/accounts_light_ib" {
			http.NotFound(w, r)
			return
		}
		// Verify cookie was sent
		cookies := r.Cookies()
		var hasSessionCookie bool
		for _, c := range cookies {
			if c.Name == "sessionid" && c.Value == "valid_session_123" {
				hasSessionCookie = true
			}
		}
		if !hasSessionCookie {
			http.Error(w, `{"resultCode":"UNAUTHORIZED"}`, http.StatusUnauthorized)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(fixtureData)
	}))
	defer ts.Close()

	cfg := personal.DefaultTransportConfig()
	cfg.TLSConfig.Profile = personal.TLSProfileStandard
	client, err := personal.NewPersonalClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	client.SetBaseURL(ts.URL)

	session := personal.NewSession(uuid.New(), uuid.New(), []*http.Cookie{
		{Name: "sessionid", Value: "valid_session_123"},
	}, 24*time.Hour)

	accounts, err := client.FetchAccounts(context.Background(), session)
	if err != nil {
		t.Fatalf("FetchAccounts failed: %v", err)
	}

	if len(accounts) != 2 {
		t.Fatalf("expected 2 accounts, got %d", len(accounts))
	}
	if accounts[0].ExternalID != "acc_debit_01" {
		t.Errorf("expected acc_debit_01, got %s", accounts[0].ExternalID)
	}
}

func TestPersonalClient_FetchTransactions_Success(t *testing.T) {
	fixtureData, err := os.ReadFile(filepath.Join("testdata", "operations_timeline.json"))
	if err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mybank/api/operations/timeline/public/legacy/v1/operations" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(fixtureData)
	}))
	defer ts.Close()

	cfg := personal.DefaultTransportConfig()
	client, err := personal.NewPersonalClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	client.SetBaseURL(ts.URL)

	session := personal.NewSession(uuid.New(), uuid.New(), []*http.Cookie{
		{Name: "sessionid", Value: "valid_session_123"},
	}, 24*time.Hour)

	res, err := client.FetchTransactions(context.Background(), session, connector.SyncParams{})
	if err != nil {
		t.Fatalf("FetchTransactions failed: %v", err)
	}

	if len(res.Transactions) != 4 {
		t.Fatalf("expected 4 transactions, got %d", len(res.Transactions))
	}
	if res.Transactions[0].ExternalID != "op_1001" {
		t.Errorf("expected op_1001, got %s", res.Transactions[0].ExternalID)
	}
}

func TestPersonalClient_ErrorClassification(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
		targetErr  error
	}{
		{"401 Unauthorized", http.StatusUnauthorized, `{"resultCode":"UNAUTHORIZED"}`, connector.ErrAuthExpired},
		{"403 Forbidden", http.StatusForbidden, `{"resultCode":"FORBIDDEN"}`, connector.ErrAuthExpired},
		{"429 Rate Limited", http.StatusTooManyRequests, `{"resultCode":"RATE_LIMITED"}`, connector.ErrRateLimited},
		{"503 Service Unavailable", http.StatusServiceUnavailable, `{"error":"down"}`, connector.ErrTemporary},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.statusCode)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer ts.Close()

			cfg := personal.DefaultTransportConfig()
			client, _ := personal.NewPersonalClient(cfg)
			client.SetBaseURL(ts.URL)

			session := personal.NewSession(uuid.New(), uuid.New(), []*http.Cookie{
				{Name: "sessionid", Value: "val"},
			}, 24*time.Hour)

			_, err := client.FetchAccounts(context.Background(), session)
			if err == nil {
				t.Fatalf("expected error for %s, got nil", tt.name)
			}
			if !errors.Is(err, tt.targetErr) {
				t.Errorf("expected error %v, got %v", tt.targetErr, err)
			}
		})
	}
}

func TestPersonalClient_BoundedBodyLimit(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Send 100 bytes
		_, _ = w.Write(make([]byte, 100))
	}))
	defer ts.Close()

	cfg := personal.DefaultTransportConfig()
	cfg.MaxResponseBody = 50 // Limit to 50 bytes
	client, _ := personal.NewPersonalClient(cfg)
	client.SetBaseURL(ts.URL)

	session := personal.NewSession(uuid.New(), uuid.New(), []*http.Cookie{
		{Name: "sessionid", Value: "val"},
	}, 24*time.Hour)

	_, err := client.FetchAccounts(context.Background(), session)
	if err == nil || !errors.Is(err, personal.ErrResponseBodyTooLarge) {
		t.Fatalf("expected ErrResponseBodyTooLarge, got %v", err)
	}
}

func TestPersonalClient_ConcurrentSessionIsolation(t *testing.T) {
	var leakCount int64
	var totalRequests int64

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&totalRequests, 1)
		cookies := r.Cookies()

		var sessionID string
		var count int
		for _, c := range cookies {
			if c.Name == "sessionid" {
				sessionID = c.Value
				count++
			}
		}

		// Security assertion: Exactly ONE sessionid cookie must be present, never both or none
		if count != 1 {
			atomic.AddInt64(&leakCount, 1)
			http.Error(w, "invalid cookie count", http.StatusBadRequest)
			return
		}

		expectedTenant := r.URL.Query().Get("expected_tenant")
		if expectedTenant == "A" && sessionID != "session_val_AAA" {
			atomic.AddInt64(&leakCount, 1)
			http.Error(w, "cross-session leak detected: expected session A", http.StatusForbidden)
			return
		}
		if expectedTenant == "B" && sessionID != "session_val_BBB" {
			atomic.AddInt64(&leakCount, 1)
			http.Error(w, "cross-session leak detected: expected session B", http.StatusForbidden)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"resultCode":"OK","payload":[]}`))
	}))
	defer ts.Close()

	cfg := personal.DefaultTransportConfig()
	client, err := personal.NewPersonalClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	client.SetBaseURL(ts.URL)

	familyA := uuid.New()
	familyB := uuid.New()
	connA := uuid.New()
	connB := uuid.New()

	sessionA := personal.NewSession(familyA, connA, []*http.Cookie{
		{Name: "sessionid", Value: "session_val_AAA"},
	}, 24*time.Hour)

	sessionB := personal.NewSession(familyB, connB, []*http.Cookie{
		{Name: "sessionid", Value: "session_val_BBB"},
	}, 24*time.Hour)

	var wg sync.WaitGroup
	workers := 20
	iterations := 25

	for w := 0; w < workers; w++ {
		wg.Add(2)
		// Worker for Tenant / Session A
		go func(workerID int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				_, err := client.FetchAccounts(context.Background(), sessionA)
				if err != nil {
					t.Errorf("worker A request %d failed: %v", i, err)
					return
				}
			}
		}(w)

		// Concurrent Worker for Tenant / Session B (different family, different connection, same cookie name)
		go func(workerID int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				_, err := client.FetchAccounts(context.Background(), sessionB)
				if err != nil {
					t.Errorf("worker B request %d failed: %v", i, err)
					return
				}
			}
		}(w)
	}

	wg.Wait()

	if leaks := atomic.LoadInt64(&leakCount); leaks > 0 {
		t.Fatalf("session isolation violated: detected %d cross-session cookie leaks across %d requests",
			leaks, atomic.LoadInt64(&totalRequests))
	}
}

package transaction

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/khaliullov/family-finance/internal/connector"
	"github.com/khaliullov/family-finance/internal/connector/tbank"
	"github.com/khaliullov/family-finance/internal/connector/tbank/personal"
	"github.com/khaliullov/family-finance/internal/crypto"
	"github.com/khaliullov/family-finance/internal/database"
)

func TestService_TBankPersonalIntegration(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()
	workerPool := getWorkerPool(t)
	defer workerPool.Close()

	ctx := context.Background()

	accountsFixture, err := os.ReadFile(filepath.Join("..", "connector", "tbank", "personal", "testdata", "accounts_light_ib.json"))
	if err != nil {
		t.Fatal(err)
	}
	opsFixture, err := os.ReadFile(filepath.Join("..", "connector", "tbank", "personal", "testdata", "operations_timeline.json"))
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

	cryptoKey, err := crypto.GenerateRandomKey()
	if err != nil {
		t.Fatal(err)
	}

	tbankConn, err := tbank.NewPersonalConnector(
		tbank.WithCryptoKey(cryptoKey),
		tbank.WithBaseURL(ts.URL),
	)
	if err != nil {
		t.Fatal(err)
	}

	registry := connector.NewRegistry()
	registry.Register(tbankConn)

	// Setup tenant fixture for provider "tbank"
	familyID, _, connID := setupTenantFixture(t, "tbank")

	// Store active encrypted session in bank_connections.encrypted_credentials
	session := personal.NewSession(familyID, connID, []*http.Cookie{
		{Name: "sessionid", Value: "valid_personal_session_abc"},
	}, 24*time.Hour)

	ciphertext, err := session.Encrypt(cryptoKey)
	if err != nil {
		t.Fatal(err)
	}

	err = database.WithFamilyTx(ctx, appPool, familyID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE public.bank_connections
			SET encrypted_credentials = $1,
			    status = 'active',
			    updated_at = NOW()
			WHERE family_id = $2 AND id = $3
		`, ciphertext, familyID, connID)
		return err
	})
	if err != nil {
		t.Fatalf("failed to store encrypted session: %v", err)
	}

	// Execute end-to-end sync through existing transaction.Service
	svc := NewService(workerPool, registry)
	stats, err := svc.SyncConnection(ctx, familyID, connID, SyncOptions{
		LookbackDays: 30,
	})
	if err != nil {
		t.Fatalf("SyncConnection failed: %v", err)
	}

	// Verify sync stats
	if stats.AccountsDiscovered != 2 {
		t.Errorf("AccountsDiscovered = %d; want 2", stats.AccountsDiscovered)
	}
	if stats.CardsDiscovered != 2 {
		t.Errorf("CardsDiscovered = %d; want 2", stats.CardsDiscovered)
	}
	if stats.TransactionsIngested != 4 {
		t.Errorf("TransactionsIngested = %d; want 4", stats.TransactionsIngested)
	}
	if stats.RawPayloadsPreserved != 1 {
		t.Errorf("RawPayloadsPreserved = %d; want 1", stats.RawPayloadsPreserved)
	}

	// Verify database records in PostgreSQL
	err = database.WithFamilyTx(ctx, workerPool, familyID, func(tx pgx.Tx) error {
		// Accounts
		var accCount int
		err := tx.QueryRow(ctx, "SELECT count(*) FROM public.accounts WHERE family_id = $1", familyID).Scan(&accCount)
		if err != nil || accCount != 2 {
			t.Errorf("accounts count = %d, want 2, err=%v", accCount, err)
		}

		// Cards
		var cardCount int
		err = tx.QueryRow(ctx, "SELECT count(*) FROM public.cards WHERE family_id = $1", familyID).Scan(&cardCount)
		if err != nil || cardCount != 2 {
			t.Errorf("cards count = %d, want 2, err=%v", cardCount, err)
		}

		// Bank transactions
		var txCount int
		err = tx.QueryRow(ctx, "SELECT count(*) FROM public.bank_transactions WHERE family_id = $1", familyID).Scan(&txCount)
		if err != nil || txCount != 4 {
			t.Errorf("transactions count = %d, want 4, err=%v", txCount, err)
		}

		// Accounting entries
		var entryCount int
		err = tx.QueryRow(ctx, "SELECT count(*) FROM public.accounting_entries WHERE family_id = $1", familyID).Scan(&entryCount)
		if err != nil || entryCount != 4 {
			t.Errorf("accounting_entries count = %d, want 4, err=%v", entryCount, err)
		}

		// Verify pending hold op_1001 details
		var status, opType string
		var amountMinor int64
		var authID *string
		err = tx.QueryRow(ctx, `
			SELECT booking_status, operation_type, amount_minor, external_auth_id
			FROM public.bank_transactions
			WHERE family_id = $1 AND external_id = 'op_1001'
		`, familyID).Scan(&status, &opType, &amountMinor, &authID)
		if err != nil {
			t.Errorf("failed to query op_1001: %v", err)
		}
		if status != "pending" {
			t.Errorf("op_1001 status = %q; want pending", status)
		}
		if opType != "purchase" {
			t.Errorf("op_1001 opType = %q; want purchase", opType)
		}
		if amountMinor != -125075 {
			t.Errorf("op_1001 amount = %d; want -125075", amountMinor)
		}
		if authID == nil || *authID != "auth_hold_501" {
			t.Errorf("op_1001 authID = %v; want auth_hold_501", authID)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("database verification failed: %v", err)
	}
}

func TestDBSessionStore_PostgreSQL_CASConcurrency(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()

	ctx := context.Background()
	cryptoKey, err := crypto.GenerateRandomKey()
	if err != nil {
		t.Fatal(err)
	}

	sessionStore, err := personal.NewDBSessionStore(appPool, cryptoKey)
	if err != nil {
		t.Fatal(err)
	}

	familyID, _, connID := setupTenantFixture(t, "tbank")

	// 1. Initial Save
	sess := personal.NewSession(familyID, connID, []*http.Cookie{
		{Name: "sessionid", Value: "init_cas_cookie"},
	}, 2*time.Hour)

	if err := sessionStore.SaveSession(ctx, familyID, connID, sess); err != nil {
		t.Fatalf("initial SaveSession failed: %v", err)
	}

	// 2. Load session into two concurrent references
	clientA, err := sessionStore.GetSession(ctx, familyID, connID)
	if err != nil {
		t.Fatalf("get session A failed: %v", err)
	}
	clientB, err := sessionStore.GetSession(ctx, familyID, connID)
	if err != nil {
		t.Fatalf("get session B failed: %v", err)
	}

	// 3. Concurrently save from two separate routines
	var wg sync.WaitGroup
	var errA, errB error
	wg.Add(2)

	go func() {
		defer wg.Done()
		clientA.Cookies = []personal.SerializableCookie{{Name: "sessionid", Value: "val_A"}}
		errA = sessionStore.SaveSession(ctx, familyID, connID, clientA)
	}()

	go func() {
		defer wg.Done()
		clientB.Cookies = []personal.SerializableCookie{{Name: "sessionid", Value: "val_B"}}
		errB = sessionStore.SaveSession(ctx, familyID, connID, clientB)
	}()

	wg.Wait()

	// Assert optimistic concurrency control: exactly ONE succeeded, ONE had ErrSessionStoreConflict
	successCount := 0
	conflictCount := 0
	if errA == nil {
		successCount++
	} else if errors.Is(errA, personal.ErrSessionStoreConflict) {
		conflictCount++
	}
	if errB == nil {
		successCount++
	} else if errors.Is(errB, personal.ErrSessionStoreConflict) {
		conflictCount++
	}

	if successCount != 1 || conflictCount != 1 {
		t.Fatalf("CAS concurrency violated: expected 1 success and 1 conflict, got success=%d, conflict=%d (errA=%v, errB=%v)",
			successCount, conflictCount, errA, errB)
	}

	// 4. Invalidate session: must clear encrypted credentials and set status to expired
	if err := sessionStore.InvalidateSession(ctx, familyID, connID); err != nil {
		t.Fatalf("InvalidateSession failed: %v", err)
	}

	// Verify database record has NULL encrypted_credentials and status = expired
	err = database.WithFamilyTx(ctx, appPool, familyID, func(tx pgx.Tx) error {
		var creds []byte
		var status string
		err := tx.QueryRow(ctx, "SELECT encrypted_credentials, status FROM public.bank_connections WHERE family_id = $1 AND id = $2", familyID, connID).Scan(&creds, &status)
		if err != nil {
			return err
		}
		if creds != nil {
			t.Errorf("expected encrypted_credentials to be NULL after invalidation, got %d bytes", len(creds))
		}
		if status != "expired" {
			t.Errorf("status = %q, want expired", status)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Idempotent invalidation
	if err := sessionStore.InvalidateSession(ctx, familyID, connID); err != nil {
		t.Fatalf("repeated InvalidateSession should be idempotent, got: %v", err)
	}
}

func TestDBAuthAttemptStore_PostgreSQL_FORCE_RLS(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()
	workerPool := getWorkerPool(t)
	defer workerPool.Close()

	ctx := context.Background()
	familyID, _, connID := setupTenantFixture(t, "tbank")

	store := personal.NewDBAuthAttemptStore(appPool)

	attempt := personal.NewAuthAttempt(familyID, &connID, "tbank_personal", 5*time.Minute)
	if err := store.CreateAttempt(ctx, attempt); err != nil {
		t.Fatalf("CreateAttempt failed: %v", err)
	}

	testNonce := time.Now().UnixNano()
	chatID := testNonce
	msgID := testNonce + 1
	updateID := testNonce + 2
	expiresAt := time.Now().Add(5 * time.Minute)

	if err := store.SetTelegramChallenge(ctx, familyID, attempt.ID, "sms_otp", attempt.ChallengeRef, chatID, msgID, expiresAt); err != nil {
		t.Fatalf("SetTelegramChallenge failed: %v", err)
	}

	// 1. Resolve challenge WITHOUT app.current_family_id set (simulating webhook receiver)
	// Must succeed via resolve_telegram_auth_challenge under FORCE RLS
	resolved, err := store.FindAttemptByTelegramMessage(ctx, chatID, msgID)
	if err != nil {
		t.Fatalf("FindAttemptByTelegramMessage under FORCE RLS failed: %v", err)
	}
	if resolved.ID != attempt.ID || resolved.FamilyID != familyID {
		t.Fatalf("resolved attempt mismatch: got %+v, want attempt=%s family=%s", resolved, attempt.ID, familyID)
	}
	if resolved.State != personal.StateAwaitingOTP {
		t.Fatalf("state = %q, want awaiting_otp", resolved.State)
	}

	// 2. Consume challenge atomically without family context
	consumed, err := store.ConsumeAttemptByTelegramMessage(ctx, chatID, msgID, updateID)
	if err != nil {
		t.Fatalf("ConsumeAttemptByTelegramMessage failed: %v", err)
	}
	if consumed.State != personal.StateVerifyingOTP {
		t.Fatalf("consumed state = %q, want verifying_otp", consumed.State)
	}

	// 3. Second consumption attempt with same or new update_id must fail (single-consumption CAS)
	_, err = store.ConsumeAttemptByTelegramMessage(ctx, chatID, msgID, updateID)
	if err == nil || !errors.Is(err, personal.ErrAttemptNotFound) {
		t.Fatalf("expected ErrAttemptNotFound on second consumption, got %v", err)
	}

	// 4. Verify direct SELECT on bank_auth_attempts without tenant context returns 0 rows (proves FORCE RLS is active)
	var directCount int
	err = workerPool.QueryRow(ctx, "SELECT count(*) FROM public.bank_auth_attempts WHERE id = $1", attempt.ID).Scan(&directCount)
	if err != nil || directCount != 0 {
		t.Fatalf("expected 0 rows without tenant context under FORCE RLS, got count=%d, err=%v", directCount, err)
	}
}

func TestPersonalConnector_IncompleteRetrieval_BlocksSyncCheckpoint(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()
	workerPool := getWorkerPool(t)
	defer workerPool.Close()

	ctx := context.Background()

	// Server that returns an upstream server error on operations timeline
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/common/v1/accounts_light_ib" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"resultCode":"OK","payload":[]}`))
			return
		}
		// Operations timeline fails upstream
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"resultCode":"INTERNAL_ERROR","errorMessage":"temporary failure"}`))
	}))
	defer ts.Close()

	cryptoKey, err := crypto.GenerateRandomKey()
	if err != nil {
		t.Fatal(err)
	}

	tbankConn, err := tbank.NewPersonalConnector(
		tbank.WithCryptoKey(cryptoKey),
		tbank.WithBaseURL(ts.URL),
	)
	if err != nil {
		t.Fatal(err)
	}

	registry := connector.NewRegistry()
	registry.Register(tbankConn)

	familyID, _, connID := setupTenantFixture(t, "tbank")

	session := personal.NewSession(familyID, connID, []*http.Cookie{
		{Name: "sessionid", Value: "session_failing_sync"},
	}, 24*time.Hour)

	ciphertext, err := session.Encrypt(cryptoKey)
	if err != nil {
		t.Fatal(err)
	}

	err = database.WithFamilyTx(ctx, appPool, familyID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE public.bank_connections
			SET encrypted_credentials = $1,
			    status = 'active',
			    last_synced_at = NULL,
			    sync_cursor = 'cursor_initial_123',
			    updated_at = NOW()
			WHERE family_id = $2 AND id = $3
		`, ciphertext, familyID, connID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	svc := NewService(workerPool, registry)
	_, err = svc.SyncConnection(ctx, familyID, connID, SyncOptions{})
	if err == nil {
		t.Fatal("expected SyncConnection to fail on incomplete retrieval / upstream error, got nil")
	}

	// Verify that checkpoint was NOT advanced
	err = database.WithFamilyTx(ctx, workerPool, familyID, func(tx pgx.Tx) error {
		var lastSyncedAt *time.Time
		var syncCursor *string
		var status string
		var errorMsg *string
		err := tx.QueryRow(ctx, `
			SELECT last_synced_at, sync_cursor, status, error_message
			FROM public.bank_connections
			WHERE family_id = $1 AND id = $2
		`, familyID, connID).Scan(&lastSyncedAt, &syncCursor, &status, &errorMsg)
		if err != nil {
			return err
		}

		if lastSyncedAt != nil {
			t.Errorf("last_synced_at was prematurely advanced: got %v, want nil", lastSyncedAt)
		}
		if syncCursor == nil || *syncCursor != "cursor_initial_123" {
			t.Errorf("sync_cursor was overwritten: got %v, want cursor_initial_123", syncCursor)
		}
		if status != "error" {
			t.Errorf("status = %q, want error", status)
		}
		if errorMsg == nil || *errorMsg == "" {
			t.Errorf("expected error_message to be recorded, got empty")
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestService_TBankPersonal_IdempotencyAndSimilarPurchasesNotMerged(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()
	workerPool := getWorkerPool(t)
	defer workerPool.Close()

	ctx := context.Background()

	accountsFixture, err := os.ReadFile(filepath.Join("..", "connector", "tbank", "personal", "testdata", "accounts_light_ib.json"))
	if err != nil {
		t.Fatal(err)
	}

	// Two identical purchases at the exact same merchant, same timestamp, same amount, same card,
	// but with distinct external IDs (e.g. buying two coffees consecutively)
	similarPurchasesJSON := []byte(`{
		"resultCode": "OK",
		"trackingId": "track_similar_purchases",
		"payload": [
			{
				"id": "op_coffee_001",
				"account": "acc_debit_01",
				"card": "card_mir_01",
				"cardNumber": "*5512",
				"amount": {
					"value": "-350.00",
					"currency": {"code": 643, "name": "RUB", "strCode": "RUB"}
				},
				"accountAmount": {
					"value": "-350.00",
					"currency": {"code": 643, "name": "RUB", "strCode": "RUB"}
				},
				"operationTime": {"milliseconds": 1763736745000},
				"debitingTime": {"milliseconds": 1763750000000},
				"status": "OK",
				"type": "Debit",
				"description": "Cofix Coffee",
				"mcc": 5814,
				"merchant": {"name": "Cofix"},
				"isInner": false
			},
			{
				"id": "op_coffee_002",
				"account": "acc_debit_01",
				"card": "card_mir_01",
				"cardNumber": "*5512",
				"amount": {
					"value": "-350.00",
					"currency": {"code": 643, "name": "RUB", "strCode": "RUB"}
				},
				"accountAmount": {
					"value": "-350.00",
					"currency": {"code": 643, "name": "RUB", "strCode": "RUB"}
				},
				"operationTime": {"milliseconds": 1763736745000},
				"debitingTime": {"milliseconds": 1763750000000},
				"status": "OK",
				"type": "Debit",
				"description": "Cofix Coffee",
				"mcc": 5814,
				"merchant": {"name": "Cofix"},
				"isInner": false
			}
		]
	}`)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/common/v1/accounts_light_ib":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(accountsFixture)
		case "/mybank/api/operations/timeline/public/legacy/v1/operations":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(similarPurchasesJSON)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	cryptoKey, err := crypto.GenerateRandomKey()
	if err != nil {
		t.Fatal(err)
	}

	tbankConn, err := tbank.NewPersonalConnector(
		tbank.WithCryptoKey(cryptoKey),
		tbank.WithBaseURL(ts.URL),
	)
	if err != nil {
		t.Fatal(err)
	}

	registry := connector.NewRegistry()
	registry.Register(tbankConn)

	familyID, _, connID := setupTenantFixture(t, "tbank")

	session := personal.NewSession(familyID, connID, []*http.Cookie{
		{Name: "sessionid", Value: "valid_personal_session_similar"},
	}, 24*time.Hour)

	ciphertext, err := session.Encrypt(cryptoKey)
	if err != nil {
		t.Fatal(err)
	}

	err = database.WithFamilyTx(ctx, appPool, familyID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE public.bank_connections
			SET encrypted_credentials = $1,
			    status = 'active',
			    updated_at = NOW()
			WHERE family_id = $2 AND id = $3
		`, ciphertext, familyID, connID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	svc := NewService(workerPool, registry)

	// 1. First synchronization pass: both similar purchases must be ingested as distinct transactions
	stats1, err := svc.SyncConnection(ctx, familyID, connID, SyncOptions{
		LookbackDays: 30,
	})
	if err != nil {
		t.Fatalf("First SyncConnection failed: %v", err)
	}

	if stats1.TransactionsIngested != 2 {
		t.Errorf("First sync: TransactionsIngested = %d; want 2", stats1.TransactionsIngested)
	}
	if stats1.TransactionsSkipped != 0 {
		t.Errorf("First sync: TransactionsSkipped = %d; want 0", stats1.TransactionsSkipped)
	}

	// Verify both transactions exist independently in PostgreSQL
	err = database.WithFamilyTx(ctx, workerPool, familyID, func(tx pgx.Tx) error {
		var count int
		err := tx.QueryRow(ctx, `
			SELECT count(*) FROM public.bank_transactions
			WHERE family_id = $1 AND external_id IN ('op_coffee_001', 'op_coffee_002')
		`, familyID).Scan(&count)
		if err != nil || count != 2 {
			t.Fatalf("expected 2 distinct rows for similar purchases, count=%d, err=%v", count, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// 2. Second synchronization pass with exact same data: Idempotent (0 ingested, 2 skipped, 0 duplicates)
	stats2, err := svc.SyncConnection(ctx, familyID, connID, SyncOptions{
		LookbackDays: 30,
	})
	if err != nil {
		t.Fatalf("Second SyncConnection failed: %v", err)
	}

	if stats2.TransactionsIngested != 0 {
		t.Errorf("Second sync (idempotency): TransactionsIngested = %d; want 0", stats2.TransactionsIngested)
	}
	if stats2.TransactionsSkipped != 2 {
		t.Errorf("Second sync (idempotency): TransactionsSkipped = %d; want 2", stats2.TransactionsSkipped)
	}

	// Verify total row count remains exactly 2 (no duplicate rows created)
	err = database.WithFamilyTx(ctx, workerPool, familyID, func(tx pgx.Tx) error {
		var count int
		err := tx.QueryRow(ctx, `
			SELECT count(*) FROM public.bank_transactions
			WHERE family_id = $1 AND external_id IN ('op_coffee_001', 'op_coffee_002')
		`, familyID).Scan(&count)
		if err != nil || count != 2 {
			t.Fatalf("expected exactly 2 rows after duplicate sync, count=%d, err=%v", count, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

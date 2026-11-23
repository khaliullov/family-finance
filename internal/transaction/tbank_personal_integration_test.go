package transaction

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

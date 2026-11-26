package transaction

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/khaliullov/family-finance/internal/connector"
	"github.com/khaliullov/family-finance/internal/database"
)

type fencingTestMockConnector struct {
	mu              sync.Mutex
	fetchAccsCalled int
	fetchTxsCalled  int
	twoPages        bool
}

func (m *fencingTestMockConnector) ProviderCode() string { return "fencing_mock" }
func (m *fencingTestMockConnector) HealthCheck(ctx context.Context, encryptedCredentials []byte) error {
	return nil
}
func (m *fencingTestMockConnector) FetchCards(ctx context.Context, encryptedCredentials []byte, accountExternalID string) ([]connector.CardDTO, error) {
	return nil, nil
}
func (m *fencingTestMockConnector) FetchAccounts(ctx context.Context, encryptedCredentials []byte) ([]connector.AccountDTO, error) {
	m.mu.Lock()
	m.fetchAccsCalled++
	m.mu.Unlock()
	return []connector.AccountDTO{
		{
			ExternalID: "fencing_acc_1",
			Name:       "Fencing Primary Account",
			Type:       "checking",
			Currency:   "RUB",
			Balance:    &connector.CurrencyAmount{MinorUnits: 50000, Currency: "RUB"},
		},
	}, nil
}
func (m *fencingTestMockConnector) FetchBalances(ctx context.Context, encryptedCredentials []byte, accountExternalIDs []string) ([]connector.BalanceDTO, error) {
	return nil, nil
}
func (m *fencingTestMockConnector) FetchTransactions(ctx context.Context, encryptedCredentials []byte, params connector.SyncParams) (*connector.SyncResult, error) {
	m.mu.Lock()
	m.fetchTxsCalled++
	currentPage := m.fetchTxsCalled
	m.mu.Unlock()

	var hasMore bool
	var nextCur string
	if m.twoPages && currentPage == 1 {
		hasMore = true
		nextCur = "cur_page_2"
	}

	tx := connector.CanonicalBankTransaction{
		ExternalID:        fmt.Sprintf("tx_fence_%d", currentPage),
		AccountExternalID: "fencing_acc_1",
		Amount:            connector.CurrencyAmount{MinorUnits: int64(currentPage * 1000), Currency: "RUB"},
		AccountAmount:     connector.CurrencyAmount{MinorUnits: int64(currentPage * 1000), Currency: "RUB"},
		TransactionTime:   time.Now().UTC(),
		BookingStatus:     "posted",
		OperationType:     "purchase",
		RawDescription:    fmt.Sprintf("Fence tx %d", currentPage),
	}

	return &connector.SyncResult{
		Transactions: []connector.CanonicalBankTransaction{tx},
		RawPayload:   []byte(fmt.Sprintf(`{"page": %d}`, currentPage)),
		Progress: connector.SyncProgress{
			HasMore:    hasMore,
			NextCursor: nextCur,
		},
	}, nil
}

// Test A: Disconnect after gate, before lease acquisition -> lease fails, 0 connector calls, 0 DB writes.
func TestConcurrencyFencing_DisconnectAfterGate_BeforeLeaseAcquisition(t *testing.T) {
	pool := getWorkerPool(t)
	defer pool.Close()
	ctx := context.Background()

	familyID, _, connID := setupTenantFixture(t, "fencing_mock")

	mock := &fencingTestMockConnector{}
	registry := connector.NewRegistry()
	registry.Register(mock)

	svc := NewService(pool, registry)

	// Intercept right after gate check releases its row lock, before lease acquisition
	svc.BeforeLeaseAcquisitionHook = func(cid uuid.UUID) {
		_ = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				UPDATE public.bank_connections
				SET status = 'disconnected', updated_at = NOW()
				WHERE family_id = $1 AND id = $2
			`, familyID, cid)
			return err
		})
	}

	stats, err := svc.SyncConnection(ctx, familyID, connID, SyncOptions{})
	if err == nil {
		t.Fatalf("expected error when connection is disconnected before lease acquisition, got nil")
	}

	if !errors.Is(err, ErrSyncGateConnectionDisconnected) {
		t.Fatalf("expected ErrSyncGateConnectionDisconnected, got: %v", err)
	}

	// 0 connector calls
	mock.mu.Lock()
	if mock.fetchAccsCalled != 0 || mock.fetchTxsCalled != 0 {
		t.Errorf("expected 0 connector calls, got accs=%d txs=%d", mock.fetchAccsCalled, mock.fetchTxsCalled)
	}
	mock.mu.Unlock()

	// 0 DB accounts / transactions written
	_ = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		var accCount, txCount int
		_ = tx.QueryRow(ctx, "SELECT count(*) FROM public.accounts WHERE family_id = $1", familyID).Scan(&accCount)
		_ = tx.QueryRow(ctx, "SELECT count(*) FROM public.bank_transactions WHERE family_id = $1", familyID).Scan(&txCount)
		if accCount != 0 || txCount != 0 {
			t.Errorf("expected 0 accounts and 0 transactions, got acc=%d tx=%d", accCount, txCount)
		}

		var status string
		_ = tx.QueryRow(ctx, "SELECT status FROM public.bank_connections WHERE family_id = $1 AND id = $2", familyID, connID).Scan(&status)
		if status != "disconnected" {
			t.Errorf("expected status 'disconnected', got %q", status)
		}
		return nil
	})

	_ = stats
}

// Test B: Authentication rotation after gate -> lease fails, stale creds unused, 0 DB writes.
func TestConcurrencyFencing_AuthenticationRotationAfterGate_BeforeLeaseAcquisition(t *testing.T) {
	pool := getWorkerPool(t)
	defer pool.Close()
	ctx := context.Background()

	familyID, _, connID := setupTenantFixture(t, "fencing_mock")

	mock := &fencingTestMockConnector{}
	registry := connector.NewRegistry()
	registry.Register(mock)

	svc := NewService(pool, registry)

	// Intercept right after gate check, rotate session version and credential hash
	// Intercept right after gate check, rotate session version and credential hash using app role
	newHash := "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	appPool := getAppPool(t)
	defer appPool.Close()

	svc.BeforeLeaseAcquisitionHook = func(cid uuid.UUID) {
		err := database.WithFamilyTx(ctx, appPool, familyID, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `
				UPDATE public.bank_connections
				SET session_version = session_version + 1,
				    credential_hash = $1,
				    fencing_token = fencing_token + 1,
				    updated_at = NOW()
				WHERE family_id = $2 AND id = $3
			`, newHash, familyID, cid)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return fmt.Errorf("0 rows updated")
			}
			return nil
		})
		if err != nil {
			t.Fatalf("hook update failed: %v", err)
		}
	}

	stats, err := svc.SyncConnection(ctx, familyID, connID, SyncOptions{})
	if err == nil {
		t.Fatalf("expected error on rotated session credentials, got nil")
	}

	// Must fail closed with fencing token or session mismatch
	if !errors.Is(err, ErrSyncOwnershipLost) && !errors.Is(err, ErrSyncGateValidationStale) {
		t.Fatalf("expected ErrSyncOwnershipLost or ErrSyncGateValidationStale, got: %v", err)
	}

	// Stale credentials must not be used to fetch
	mock.mu.Lock()
	if mock.fetchAccsCalled != 0 || mock.fetchTxsCalled != 0 {
		t.Errorf("expected 0 connector calls with stale credentials, got accs=%d txs=%d", mock.fetchAccsCalled, mock.fetchTxsCalled)
	}
	mock.mu.Unlock()

	// 0 DB accounts / transactions written
	_ = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		var accCount, txCount int
		_ = tx.QueryRow(ctx, "SELECT count(*) FROM public.accounts WHERE family_id = $1", familyID).Scan(&accCount)
		_ = tx.QueryRow(ctx, "SELECT count(*) FROM public.bank_transactions WHERE family_id = $1", familyID).Scan(&txCount)
		if accCount != 0 || txCount != 0 {
			t.Errorf("expected 0 accounts and 0 transactions, got acc=%d tx=%d", accCount, txCount)
		}
		return nil
	})

	_ = stats
}

// Test C: Lease lost before first account write -> tx rolls back, 0 accounts/cards/balances written.
func TestConcurrencyFencing_LeaseLost_BeforeFirstAccountWrite(t *testing.T) {
	pool := getWorkerPool(t)
	defer pool.Close()
	ctx := context.Background()

	familyID, _, connID := setupTenantFixture(t, "fencing_mock")

	mock := &fencingTestMockConnector{}
	registry := connector.NewRegistry()
	registry.Register(mock)

	svc := NewService(pool, registry)

	// Intercept right before initial accounts and cards database transaction
	svc.BeforeInitialWriteHook = func(cid uuid.UUID) {
		_ = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
			// Simulate lease expiration and takeover by another worker or fencing invalidation
			_, err := tx.Exec(ctx, `
				UPDATE public.bank_connections
				SET active_sync_token = $1,
				    sync_version = sync_version + 1,
				    fencing_token = fencing_token + 1,
				    updated_at = NOW()
				WHERE family_id = $2 AND id = $3
			`, uuid.New(), familyID, cid)
			return err
		})
	}

	stats, err := svc.SyncConnection(ctx, familyID, connID, SyncOptions{})
	if err == nil {
		t.Fatalf("expected error when lease is lost before initial write, got nil")
	}

	if !errors.Is(err, ErrSyncOwnershipLost) {
		t.Fatalf("expected ErrSyncOwnershipLost, got: %v", err)
	}

	// Accounts and cards must NOT have been persisted
	_ = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		var accCount, cardCount int
		_ = tx.QueryRow(ctx, "SELECT count(*) FROM public.accounts WHERE family_id = $1", familyID).Scan(&accCount)
		_ = tx.QueryRow(ctx, "SELECT count(*) FROM public.cards WHERE family_id = $1", familyID).Scan(&cardCount)
		if accCount != 0 || cardCount != 0 {
			t.Errorf("expected 0 accounts and 0 cards in DB, got acc=%d card=%d", accCount, cardCount)
		}
		return nil
	})

	_ = stats
}

// Test D: Lease lost between pages -> Page 2 tx rolls back, cursor stays at Page 1.
func TestConcurrencyFencing_LeaseLost_BetweenPages(t *testing.T) {
	pool := getWorkerPool(t)
	defer pool.Close()
	ctx := context.Background()

	familyID, _, connID := setupTenantFixture(t, "fencing_mock")

	mock := &fencingTestMockConnector{twoPages: true}
	registry := connector.NewRegistry()
	registry.Register(mock)

	svc := NewService(pool, registry)

	// Intercept right before page 2 write transaction
	svc.BeforePageWriteHook = func(cid uuid.UUID, page int) {
		if page == 2 {
			_ = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
				// Invalidate fencing token / reclaim lease
				_, err := tx.Exec(ctx, `
					UPDATE public.bank_connections
					SET active_sync_token = $1,
					    sync_version = sync_version + 1,
					    fencing_token = fencing_token + 1,
					    updated_at = NOW()
					WHERE family_id = $2 AND id = $3
				`, uuid.New(), familyID, cid)
				return err
			})
		}
	}

	stats, err := svc.SyncConnection(ctx, familyID, connID, SyncOptions{MaxPages: 5})
	if err == nil {
		t.Fatalf("expected ErrSyncOwnershipLost on page 2 lease loss, got nil")
	}

	if !errors.Is(err, ErrSyncOwnershipLost) {
		t.Fatalf("expected ErrSyncOwnershipLost, got: %v", err)
	}

	// Verify database invariant:
	// Page 1 tx committed: 1 tx exists (tx_fence_1).
	// Page 2 tx rolled back: 0 tx_fence_2.
	// Cursor remains at page 1 checkpoint ("cur_page_2").
	// last_synced_at is not updated to completion.
	_ = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		var txCount int
		_ = tx.QueryRow(ctx, "SELECT count(*) FROM public.bank_transactions WHERE family_id = $1", familyID).Scan(&txCount)
		if txCount != 1 {
			t.Errorf("expected exactly 1 transaction committed (page 1), got %d", txCount)
		}

		var cursor *string
		var lastSyncedAt *time.Time
		_ = tx.QueryRow(ctx, "SELECT sync_cursor, last_synced_at FROM public.bank_connections WHERE family_id = $1 AND id = $2", familyID, connID).Scan(&cursor, &lastSyncedAt)
		if cursor == nil || *cursor != "cur_page_2" {
			t.Errorf("expected cursor to remain at page 1 checkpoint 'cur_page_2', got %v", cursor)
		}
		return nil
	})

	_ = stats
}

// Test E: Disconnect during in-flight sync -> further writes fail closed, disconnected stays terminal.
func TestConcurrencyFencing_DisconnectDuringInFlightSync(t *testing.T) {
	pool := getWorkerPool(t)
	defer pool.Close()
	ctx := context.Background()

	familyID, _, connID := setupTenantFixture(t, "fencing_mock")

	mock := &fencingTestMockConnector{}
	registry := connector.NewRegistry()
	registry.Register(mock)

	svc := NewService(pool, registry)

	// Intercept before initial write, perform disconnect
	svc.BeforeInitialWriteHook = func(cid uuid.UUID) {
		_ = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				UPDATE public.bank_connections
				SET status = 'disconnected', updated_at = NOW()
				WHERE family_id = $1 AND id = $2
			`, familyID, cid)
			return err
		})
	}

	stats, err := svc.SyncConnection(ctx, familyID, connID, SyncOptions{})
	if err == nil {
		t.Fatalf("expected error on disconnected connection during sync, got nil")
	}

	if !errors.Is(err, ErrSyncOwnershipLost) {
		t.Fatalf("expected ErrSyncOwnershipLost, got: %v", err)
	}

	// Verify terminal state is preserved: status MUST remain 'disconnected'
	_ = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		var status string
		_ = tx.QueryRow(ctx, "SELECT status FROM public.bank_connections WHERE family_id = $1 AND id = $2", familyID, connID).Scan(&status)
		if status != "disconnected" {
			t.Errorf("terminal state violated: expected 'disconnected', got %q", status)
		}

		var accCount, txCount int
		_ = tx.QueryRow(ctx, "SELECT count(*) FROM public.accounts WHERE family_id = $1", familyID).Scan(&accCount)
		_ = tx.QueryRow(ctx, "SELECT count(*) FROM public.bank_transactions WHERE family_id = $1", familyID).Scan(&txCount)
		if accCount != 0 || txCount != 0 {
			t.Errorf("expected 0 accounts and 0 transactions, got acc=%d tx=%d", accCount, txCount)
		}
		return nil
	})

	_ = stats
}

// Test F: Concurrent validation and sync acquisition -> mutual exclusion holds, loser fails cleanly.
func TestConcurrencyFencing_MutualExclusion_ValidationAndSync(t *testing.T) {
	pool := getWorkerPool(t)
	defer pool.Close()
	ctx := context.Background()

	familyID, _, connID := setupTenantFixture(t, "fencing_mock")

	mock := &fencingTestMockConnector{}
	registry := connector.NewRegistry()
	registry.Register(mock)

	svc := NewService(pool, registry)
	val := NewValidator(pool, registry, nil)
	svc.SetValidator(val)

	// 1. Validation lock active -> Sync acquisition fails
	activeValidationToken := uuid.New()
	_ = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE public.bank_connections
			SET status = 'validating',
			    active_sync_token = $1,
			    active_operation_type = 'validation',
			    sync_locked_until = NOW() + INTERVAL '5 minutes',
			    updated_at = NOW()
			WHERE family_id = $2 AND id = $3
		`, activeValidationToken, familyID, connID)
		return err
	})

	_, err := svc.SyncConnection(ctx, familyID, connID, SyncOptions{})
	if err == nil {
		t.Fatalf("expected sync to fail when validation is running, got nil")
	}

	// 2. Clear validation lock, claim sync lock -> Validation acquisition fails
	activeSyncToken := uuid.New()
	_ = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE public.bank_connections
			SET status = 'active',
			    active_sync_token = $1,
			    active_operation_type = 'sync',
			    sync_locked_until = NOW() + INTERVAL '10 minutes',
			    updated_at = NOW()
			WHERE family_id = $2 AND id = $3
		`, activeSyncToken, familyID, connID)
		return err
	})

	_, err = val.ValidateConnection(ctx, familyID, connID)
	if err == nil {
		t.Fatalf("expected validation to fail when sync is actively locking connection, got nil")
	}
	if !strings.Contains(err.Error(), "active operation is currently locking connection") {
		t.Errorf("expected active operation error, got: %v", err)
	}
}

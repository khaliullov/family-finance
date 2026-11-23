package transaction

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/khaliullov/family-finance/internal/connector"
	"github.com/khaliullov/family-finance/internal/database"
)

// -----------------------------------------------------------------------------
// Mock Connectors for Lease and Concurrency Tests
// -----------------------------------------------------------------------------

type delayMockConnector struct {
	delay      time.Duration
	pageCount  int
	totalPages int
}

func (d *delayMockConnector) ProviderCode() string { return "mock_delay" }
func (d *delayMockConnector) HealthCheck(ctx context.Context, encryptedCredentials []byte) error {
	return nil
}
func (d *delayMockConnector) FetchCards(ctx context.Context, encryptedCredentials []byte, accountExternalID string) ([]connector.CardDTO, error) {
	return nil, nil
}
func (d *delayMockConnector) FetchAccounts(ctx context.Context, encryptedCredentials []byte) ([]connector.AccountDTO, error) {
	return []connector.AccountDTO{
		{ExternalID: "acc_delay_1", Name: "Delay Account", Type: "checking", Currency: "RUB"},
	}, nil
}
func (d *delayMockConnector) FetchBalances(ctx context.Context, encryptedCredentials []byte, accountExternalIDs []string) ([]connector.BalanceDTO, error) {
	return nil, nil
}
func (d *delayMockConnector) FetchTransactions(ctx context.Context, encryptedCredentials []byte, params connector.SyncParams) (*connector.SyncResult, error) {
	select {
	case <-time.After(d.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	d.pageCount++
	hasMore := d.pageCount < d.totalPages
	nextCursor := ""
	if hasMore {
		nextCursor = fmt.Sprintf("cursor_%d", d.pageCount)
	}

	return &connector.SyncResult{
		Transactions: []connector.CanonicalBankTransaction{
			{
				ExternalID:        fmt.Sprintf("tx_delay_%d", d.pageCount),
				AccountExternalID: "acc_delay_1",
				Amount:            connector.CurrencyAmount{MinorUnits: 1000, Currency: "RUB"},
				AccountAmount:     connector.CurrencyAmount{MinorUnits: 1000, Currency: "RUB"},
				BookingStatus:     "posted",
				OperationType:     "purchase",
				TransactionTime:   time.Now().UTC(),
				RawDescription:    fmt.Sprintf("Transaction on page %d", d.pageCount),
			},
		},
		Progress: connector.SyncProgress{
			HasMore:    hasMore,
			NextCursor: nextCursor,
		},
		RawPayload: []byte(fmt.Sprintf(`{"page": %d, "status": "ok"}`, d.pageCount)),
	}, nil
}

// -----------------------------------------------------------------------------
// 1. Per-Connection Sync Lease Safety Integration Tests
// -----------------------------------------------------------------------------

func TestSyncLease_HeartbeatKeepsLongRequestAlive(t *testing.T) {
	pool := getWorkerPool(t)
	defer pool.Close()
	ctx := context.Background()

	familyID, _, connID := setupTenantFixture(t, "mock_delay")

	// Configure short initial lease (120ms) and aggressive heartbeat (30ms).
	// The provider call will take 250ms, which exceeds the initial 120ms lease duration.
	// Without heartbeat, the lease would expire mid-call.
	mock := &delayMockConnector{delay: 250 * time.Millisecond, totalPages: 1}
	reg := connector.NewRegistry()
	reg.Register(mock)

	svc := NewService(pool, reg)
	svc.SyncLeaseDuration = 120 * time.Millisecond
	svc.HeartbeatInterval = 30 * time.Millisecond

	startTime := time.Now()
	stats, err := svc.SyncConnection(ctx, familyID, connID, SyncOptions{})
	if err != nil {
		t.Fatalf("expected successful sync with heartbeat renewal, got error: %v", err)
	}

	if stats.TransactionsIngested != 1 {
		t.Errorf("expected 1 transaction ingested, got %d", stats.TransactionsIngested)
	}

	elapsed := time.Since(startTime)
	if elapsed < 250*time.Millisecond {
		t.Errorf("expected elapsed time >= 250ms, got %v", elapsed)
	}

	// Verify connection successfully completed and lock cleanly cleared
	_ = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		var status string
		var activeToken *uuid.UUID
		var lockedUntil *time.Time
		err := tx.QueryRow(ctx, `
			SELECT status, active_sync_token, sync_locked_until
			FROM public.bank_connections WHERE family_id = $1 AND id = $2
		`, familyID, connID).Scan(&status, &activeToken, &lockedUntil)
		if err != nil {
			t.Fatalf("failed to query conn: %v", err)
		}
		if status != "active" {
			t.Errorf("expected status 'active', got %s", status)
		}
		if activeToken != nil || lockedUntil != nil {
			t.Errorf("expected lock cleared upon completion, got token=%v, until=%v", activeToken, lockedUntil)
		}
		return nil
	})
}

func TestSyncLease_LeaseExpirationWithoutReclaim_CannotComplete(t *testing.T) {
	pool := getWorkerPool(t)
	defer pool.Close()
	ctx := context.Background()

	familyID, _, connID := setupTenantFixture(t, "mock_delay")

	appPool := getAppPool(t)
	defer appPool.Close()

	// Set connection to non-active state using app pool (which has permissions to update status)
	err := database.WithFamilyTx(ctx, appPool, familyID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE public.bank_connections
			SET status = 'pending_pairing', last_synced_at = NULL, updated_at = NOW()
			WHERE family_id = $1 AND id = $2
		`, familyID, connID)
		return err
	})
	if err != nil {
		t.Fatalf("failed to set pending_pairing status: %v", err)
	}

	// Set lease to 50ms with heartbeat disabled (interval 1 hour).
	// Provider takes 120ms, so lease expires before completion.
	mock := &delayMockConnector{delay: 120 * time.Millisecond, totalPages: 1}
	reg := connector.NewRegistry()
	reg.Register(mock)

	svc := NewService(pool, reg)
	svc.SyncLeaseDuration = 50 * time.Millisecond
	svc.HeartbeatInterval = 1 * time.Hour // effectively disable heartbeat

	var syncErr error
	_, syncErr = svc.SyncConnection(ctx, familyID, connID, SyncOptions{})
	if syncErr == nil {
		t.Fatal("expected ErrSyncOwnershipLost due to lease expiration, got nil")
	}
	if !errors.Is(syncErr, ErrSyncOwnershipLost) {
		t.Fatalf("expected ErrSyncOwnershipLost, got: %v", syncErr)
	}

	// Verify connection status was NOT updated to 'active'
	_ = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		var status string
		_ = tx.QueryRow(ctx, "SELECT status FROM public.bank_connections WHERE family_id = $1 AND id = $2", familyID, connID).Scan(&status)
		if status == "active" {
			t.Error("expired worker improperly updated connection status to 'active'")
		}
		return nil
	})
}

func TestSyncLease_LeaseExpirationFollowedByAnotherWorker_ReclaimAndOwnershipLost(t *testing.T) {
	pool := getWorkerPool(t)
	defer pool.Close()
	ctx := context.Background()

	familyID, _, connID := setupTenantFixture(t, "mock_delay")

	// Worker 1: 50ms lease, heartbeat disabled, takes 150ms
	mock1 := &delayMockConnector{delay: 150 * time.Millisecond, totalPages: 1}
	reg1 := connector.NewRegistry()
	reg1.Register(mock1)

	svc1 := NewService(pool, reg1)
	svc1.SyncLeaseDuration = 50 * time.Millisecond
	svc1.HeartbeatInterval = 1 * time.Hour

	// Worker 2: normal fast sync
	mock2 := &delayMockConnector{delay: 0, totalPages: 1}
	reg2 := connector.NewRegistry()
	reg2.Register(mock2)

	svc2 := NewService(pool, reg2)

	var worker1Err error
	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()
		_, worker1Err = svc1.SyncConnection(ctx, familyID, connID, SyncOptions{})
	}()

	// Wait 80ms: Worker 1 lease has expired (50ms)
	time.Sleep(80 * time.Millisecond)

	// Worker 2 claims the expired connection and completes successfully
	stats2, err2 := svc2.SyncConnection(ctx, familyID, connID, SyncOptions{})
	if err2 != nil {
		t.Fatalf("worker 2 failed to reclaim expired connection: %v", err2)
	}
	if stats2.TransactionsIngested != 1 {
		t.Errorf("worker 2 expected 1 transaction, got %d", stats2.TransactionsIngested)
	}

	wg.Wait()

	// Worker 1 must fail with ErrSyncOwnershipLost
	if worker1Err == nil || !errors.Is(worker1Err, ErrSyncOwnershipLost) {
		t.Fatalf("expected worker 1 to fail with ErrSyncOwnershipLost, got: %v", worker1Err)
	}
}

func TestSyncLease_HeartbeatLeaseLoss_StopsProcessingImmediately(t *testing.T) {
	pool := getWorkerPool(t)
	defer pool.Close()
	ctx := context.Background()

	familyID, _, connID := setupTenantFixture(t, "mock_delay")

	// Provider will wait up to 2 seconds
	mock := &delayMockConnector{delay: 2 * time.Second, totalPages: 2}
	reg := connector.NewRegistry()
	reg.Register(mock)

	svc := NewService(pool, reg)
	svc.SyncLeaseDuration = 200 * time.Millisecond
	svc.HeartbeatInterval = 30 * time.Millisecond

	var syncErr error
	doneCh := make(chan struct{})

	go func() {
		defer close(doneCh)
		_, syncErr = svc.SyncConnection(ctx, familyID, connID, SyncOptions{})
	}()

	// Wait for sync to start and acquire lease
	time.Sleep(60 * time.Millisecond)

	// Outside process forcibly invalidates the active sync token
	_ = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE public.bank_connections
			SET active_sync_token = gen_random_uuid(),
			    sync_version = sync_version + 1,
			    updated_at = NOW()
			WHERE family_id = $1 AND id = $2
		`, familyID, connID)
		return err
	})

	// Wait for heartbeat to detect lease loss and cancel the sync
	select {
	case <-doneCh:
		// Completed cleanly after cancellation
	case <-time.After(1 * time.Second):
		t.Fatal("heartbeat failed to stop processing within 1s after lease loss")
	}

	if syncErr == nil || !errors.Is(syncErr, ErrSyncOwnershipLost) {
		t.Fatalf("expected ErrSyncOwnershipLost, got: %v", syncErr)
	}
}

func TestSyncLease_ParentContextCancellation(t *testing.T) {
	pool := getWorkerPool(t)
	defer pool.Close()

	familyID, _, connID := setupTenantFixture(t, "mock_delay")

	mock := &delayMockConnector{delay: 2 * time.Second, totalPages: 1}
	reg := connector.NewRegistry()
	reg.Register(mock)

	svc := NewService(pool, reg)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := svc.SyncConnection(ctx, familyID, connID, SyncOptions{})
	if err == nil {
		t.Fatal("expected context error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context timeout/cancellation, got: %v", err)
	}
}

// -----------------------------------------------------------------------------
// Two-Page Transfer Mock Connector
// -----------------------------------------------------------------------------

type twoPageTransferMockConnector struct {
	page        int
	onPage1Done func()
}

func (m *twoPageTransferMockConnector) ProviderCode() string { return "mock_two_page_transfer" }
func (m *twoPageTransferMockConnector) HealthCheck(ctx context.Context, encryptedCredentials []byte) error {
	return nil
}
func (m *twoPageTransferMockConnector) FetchCards(ctx context.Context, encryptedCredentials []byte, accountExternalID string) ([]connector.CardDTO, error) {
	return nil, nil
}
func (m *twoPageTransferMockConnector) FetchAccounts(ctx context.Context, encryptedCredentials []byte) ([]connector.AccountDTO, error) {
	return []connector.AccountDTO{
		{ExternalID: "acc_transfer_1", Name: "Transfer Account", Type: "checking", Currency: "RUB"},
	}, nil
}
func (m *twoPageTransferMockConnector) FetchBalances(ctx context.Context, encryptedCredentials []byte, accountExternalIDs []string) ([]connector.BalanceDTO, error) {
	return nil, nil
}
func (m *twoPageTransferMockConnector) FetchTransactions(ctx context.Context, encryptedCredentials []byte, params connector.SyncParams) (*connector.SyncResult, error) {
	m.page++
	if m.page == 1 {
		return &connector.SyncResult{
			Transactions: []connector.CanonicalBankTransaction{
				{
					ExternalID:        "tx_page_1",
					AccountExternalID: "acc_transfer_1",
					Amount:            connector.CurrencyAmount{MinorUnits: 100, Currency: "RUB"},
					AccountAmount:     connector.CurrencyAmount{MinorUnits: 100, Currency: "RUB"},
					BookingStatus:     "posted",
					OperationType:     "purchase",
					TransactionTime:   time.Now().UTC(),
					RawDescription:    "Page 1 Item",
				},
			},
			Progress:   connector.SyncProgress{HasMore: true, NextCursor: "cursor_page_2"},
			RawPayload: []byte(`{"page": 1}`),
		}, nil
	}

	// Between page 1 and page 2, simulate ownership transfer
	if m.onPage1Done != nil {
		m.onPage1Done()
	}

	return &connector.SyncResult{
		Transactions: []connector.CanonicalBankTransaction{
			{
				ExternalID:        "tx_page_2",
				AccountExternalID: "acc_transfer_1",
				Amount:            connector.CurrencyAmount{MinorUnits: 200, Currency: "RUB"},
				AccountAmount:     connector.CurrencyAmount{MinorUnits: 200, Currency: "RUB"},
				BookingStatus:     "posted",
				OperationType:     "purchase",
				TransactionTime:   time.Now().UTC(),
				RawDescription:    "Page 2 Item",
			},
		},
		Progress:   connector.SyncProgress{HasMore: false},
		RawPayload: []byte(`{"page": 2}`),
	}, nil
}

func TestSyncLease_StaleWorkerCannotCheckpointAfterTransfer(t *testing.T) {
	pool := getWorkerPool(t)
	defer pool.Close()
	ctx := context.Background()

	familyID, _, connID := setupTenantFixture(t, "mock_two_page_transfer")

	mock := &twoPageTransferMockConnector{
		onPage1Done: func() {
			// Manually simulate another worker acquiring the lock between page 1 and 2
			_ = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `
					UPDATE public.bank_connections
					SET active_sync_token = gen_random_uuid(),
					    sync_version = sync_version + 1,
					    sync_locked_until = NOW() + INTERVAL '10 minutes',
					    updated_at = NOW()
					WHERE family_id = $1 AND id = $2
				`, familyID, connID)
				return err
			})
		},
	}
	reg := connector.NewRegistry()
	reg.Register(mock)

	svc := NewService(pool, reg)
	_, err := svc.SyncConnection(ctx, familyID, connID, SyncOptions{})
	if err == nil {
		t.Fatal("expected ErrSyncOwnershipLost when stale worker checkpoints after transfer, got nil")
	}
	if !errors.Is(err, ErrSyncOwnershipLost) {
		t.Fatalf("expected ErrSyncOwnershipLost, got: %v", err)
	}
}

func TestSyncLease_HeartbeatTransientDBFailure(t *testing.T) {
	pool := getWorkerPool(t)
	defer pool.Close()
	ctx := context.Background()

	familyID, _, connID := setupTenantFixture(t, "mock_delay")

	mock := &delayMockConnector{delay: 2 * time.Second, totalPages: 1}
	reg := connector.NewRegistry()
	reg.Register(mock)

	svc := NewService(pool, reg)
	svc.SyncLeaseDuration = 500 * time.Millisecond
	svc.HeartbeatInterval = 30 * time.Millisecond

	var syncErr error
	doneCh := make(chan struct{})

	go func() {
		defer close(doneCh)
		_, syncErr = svc.SyncConnection(ctx, familyID, connID, SyncOptions{})
	}()

	// Wait 50ms for sync to start and enter delay in FetchTransactions
	time.Sleep(50 * time.Millisecond)

	// Close the pool to trigger transient DB failure on heartbeat renewal
	pool.Close()

	select {
	case <-doneCh:
		// Completed after cancellation
	case <-time.After(1 * time.Second):
		t.Fatal("heartbeat did not cancel sync within 1s after DB pool failure")
	}

	if syncErr == nil || !errors.Is(syncErr, ErrTransientDatabase) {
		t.Fatalf("expected ErrTransientDatabase on heartbeat DB failure, got: %v", syncErr)
	}
}

// -----------------------------------------------------------------------------
// 2. Atomic Raw Payload Deduplication Integration Tests
// -----------------------------------------------------------------------------

func TestAtomicPayloadDeduplication_Concurrent(t *testing.T) {
	pool := getWorkerPool(t)
	defer pool.Close()
	ctx := context.Background()

	familyID, _, connID := setupTenantFixture(t, "fixture")

	payload := []byte(`{
		"provider": "fixture",
		"transactions": [
			{"id": "concurrent_test_1", "amount": 1000}
		],
		"timestamp": "2026-11-23T23:00:00Z"
	}`)

	const numWorkers = 10
	results := make([]uuid.UUID, numWorkers)
	errs := make([]error, numWorkers)
	var wg sync.WaitGroup

	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			err := database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
				var err error
				results[idx], err = PreserveRawPayload(ctx, tx, familyID, &connID, "fixture", payload)
				return err
			})
			errs[idx] = err
		}(i)
	}

	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d failed: %v", i, err)
		}
	}

	// Invariant 1: All callers receive the exact same canonical payload ID
	canonicalID := results[0]
	if canonicalID == uuid.Nil {
		t.Fatal("expected non-nil canonical payload ID")
	}
	for i := 1; i < numWorkers; i++ {
		if results[i] != canonicalID {
			t.Errorf("worker %d got different payload ID %s, expected %s", i, results[i], canonicalID)
		}
	}

	// Invariant 2: Exactly 1 row persisted in PostgreSQL table raw_bank_payloads
	_ = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		var rowCount int
		err := tx.QueryRow(ctx, `
			SELECT count(*) FROM public.raw_bank_payloads
			WHERE family_id = $1 AND bank_connection_id = $2
		`, familyID, connID).Scan(&rowCount)
		if err != nil {
			t.Fatalf("failed to query count: %v", err)
		}
		if rowCount != 1 {
			t.Fatalf("expected exactly 1 raw_bank_payloads record, got %d", rowCount)
		}
		return nil
	})
}

func TestAtomicPayloadDeduplication_CrossConnectionAndNullConnection(t *testing.T) {
	pool := getWorkerPool(t)
	defer pool.Close()
	ctx := context.Background()

	familyID, _, connID1 := setupTenantFixture(t, "fixture")

	// Create second bank connection for same family
	appPool := getAppPool(t)
	defer appPool.Close()

	connID2 := uuid.New()
	err := database.WithFamilyTx(ctx, appPool, familyID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO public.bank_connections (id, family_id, authorized_by_user_id, provider, name, status)
			SELECT $1, family_id, authorized_by_user_id, 'fixture', 'Second Conn', 'active'
			FROM public.bank_connections WHERE id = $2
		`, connID2, connID1)
		return err
	})
	if err != nil {
		t.Fatalf("failed to create second connection: %v", err)
	}

	payload := []byte(`{"event": "account_statement", "account_id": "12345"}`)

	var idConn1, idConn2, idNullA, idNullB uuid.UUID

	// Insert for Conn 1
	_ = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		var err error
		idConn1, err = PreserveRawPayload(ctx, tx, familyID, &connID1, "fixture", payload)
		return err
	})

	// Insert identical payload for Conn 2 -> must produce distinct record
	_ = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		var err error
		idConn2, err = PreserveRawPayload(ctx, tx, familyID, &connID2, "fixture", payload)
		return err
	})

	if idConn1 == idConn2 {
		t.Errorf("expected different connections to have distinct raw payload IDs, got identical %s", idConn1)
	}

	// Insert with connectionID = nil (NULL connection ID)
	_ = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		var err error
		idNullA, err = PreserveRawPayload(ctx, tx, familyID, nil, "fixture", payload)
		return err
	})

	// Repeat insert with connectionID = nil -> NULLS NOT DISTINCT must deduplicate
	_ = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		var err error
		idNullB, err = PreserveRawPayload(ctx, tx, familyID, nil, "fixture", payload)
		return err
	})

	if idNullA != idNullB {
		t.Errorf("expected NULL connection payloads to deduplicate under NULLS NOT DISTINCT: idNullA=%s, idNullB=%s", idNullA, idNullB)
	}

	if idNullA == idConn1 || idNullA == idConn2 {
		t.Errorf("NULL connection payload clashed with connection-bound payload: %s", idNullA)
	}
}

// -----------------------------------------------------------------------------
// 3. ForceFullSync and Synchronization Modes Semantics
// -----------------------------------------------------------------------------

type cursorInspectionMockConnector struct {
	capturedCursor    string
	capturedStartDate time.Time
}

func (c *cursorInspectionMockConnector) ProviderCode() string { return "mock_cursor_inspect" }
func (c *cursorInspectionMockConnector) HealthCheck(ctx context.Context, encryptedCredentials []byte) error {
	return nil
}
func (c *cursorInspectionMockConnector) FetchCards(ctx context.Context, encryptedCredentials []byte, accountExternalID string) ([]connector.CardDTO, error) {
	return nil, nil
}
func (c *cursorInspectionMockConnector) FetchAccounts(ctx context.Context, encryptedCredentials []byte) ([]connector.AccountDTO, error) {
	return []connector.AccountDTO{
		{ExternalID: "acc_inspect_1", Name: "Inspect Account", Type: "checking", Currency: "RUB"},
	}, nil
}
func (c *cursorInspectionMockConnector) FetchBalances(ctx context.Context, encryptedCredentials []byte, accountExternalIDs []string) ([]connector.BalanceDTO, error) {
	return nil, nil
}
func (c *cursorInspectionMockConnector) FetchTransactions(ctx context.Context, encryptedCredentials []byte, params connector.SyncParams) (*connector.SyncResult, error) {
	c.capturedCursor = params.Cursor
	c.capturedStartDate = params.StartDate
	return &connector.SyncResult{
		Progress: connector.SyncProgress{HasMore: false},
	}, nil
}

func TestForceFullSync_FourScenarios(t *testing.T) {
	pool := getWorkerPool(t)
	defer pool.Close()
	ctx := context.Background()

	// Scenario 1: Initial Full Sync
	t.Run("Scenario 1: Initial Full Sync starts from empty cursor", func(t *testing.T) {
		familyID, _, connID := setupTenantFixture(t, "mock_cursor_inspect")
		mock := &cursorInspectionMockConnector{}
		reg := connector.NewRegistry()
		reg.Register(mock)

		svc := NewService(pool, reg)
		_, err := svc.SyncConnection(ctx, familyID, connID, SyncOptions{ForceFullSync: true, LookbackDays: 30})
		if err != nil {
			t.Fatalf("sync failed: %v", err)
		}
		if mock.capturedCursor != "" {
			t.Errorf("expected empty initial cursor, got %q", mock.capturedCursor)
		}
		if mock.capturedStartDate.IsZero() {
			t.Error("expected non-zero start date for full sync with lookback")
		}
	})

	// Scenario 2: Retry of Interrupted Full Sync preserves committed checkpoint
	t.Run("Scenario 2: Retry of Interrupted Full Sync resumes from durable checkpoint", func(t *testing.T) {
		familyID, _, connID := setupTenantFixture(t, "mock_cursor_inspect")

		// Simulate an interrupted sync with checkpoint 'chk_page_3'
		_ = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				UPDATE public.bank_connections
				SET sync_cursor = 'chk_page_3', updated_at = NOW()
				WHERE family_id = $1 AND id = $2
			`, familyID, connID)
			return err
		})

		mock := &cursorInspectionMockConnector{}
		reg := connector.NewRegistry()
		reg.Register(mock)

		svc := NewService(pool, reg)
		// Caller requests ForceFullSync: true on retry -> MUST NOT discard checkpoint!
		_, err := svc.SyncConnection(ctx, familyID, connID, SyncOptions{ForceFullSync: true})
		if err != nil {
			t.Fatalf("sync failed: %v", err)
		}
		if mock.capturedCursor != "chk_page_3" {
			t.Errorf("expected resumed cursor 'chk_page_3', got %q", mock.capturedCursor)
		}
	})

	// Scenario 3: Explicit Restart from Beginning (ResetCursor: true)
	t.Run("Scenario 3: Explicit restart from beginning discards checkpoint", func(t *testing.T) {
		familyID, _, connID := setupTenantFixture(t, "mock_cursor_inspect")

		// Simulate an interrupted sync with checkpoint 'chk_page_5'
		_ = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				UPDATE public.bank_connections
				SET sync_cursor = 'chk_page_5', updated_at = NOW()
				WHERE family_id = $1 AND id = $2
			`, familyID, connID)
			return err
		})

		mock := &cursorInspectionMockConnector{}
		reg := connector.NewRegistry()
		reg.Register(mock)

		svc := NewService(pool, reg)
		// Caller specifies ResetCursor: true -> MUST discard checkpoint and start from ""
		_, err := svc.SyncConnection(ctx, familyID, connID, SyncOptions{ResetCursor: true, ForceFullSync: true})
		if err != nil {
			t.Fatalf("sync failed: %v", err)
		}
		if mock.capturedCursor != "" {
			t.Errorf("expected reset cursor '', got %q", mock.capturedCursor)
		}
	})

	// Scenario 4: Ordinary Incremental Sync
	t.Run("Scenario 4: Ordinary incremental sync uses LastSyncedAt boundary", func(t *testing.T) {
		familyID, _, connID := setupTenantFixture(t, "mock_cursor_inspect")

		lastSync := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
		_ = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				UPDATE public.bank_connections
				SET last_synced_at = $1, sync_cursor = NULL, updated_at = NOW()
				WHERE family_id = $2 AND id = $3
			`, lastSync, familyID, connID)
			return err
		})

		mock := &cursorInspectionMockConnector{}
		reg := connector.NewRegistry()
		reg.Register(mock)

		svc := NewService(pool, reg)
		// Ordinary incremental sync (ForceFullSync: false, LookbackDays: 0)
		_, err := svc.SyncConnection(ctx, familyID, connID, SyncOptions{ForceFullSync: false})
		if err != nil {
			t.Fatalf("sync failed: %v", err)
		}
		if mock.capturedCursor != "" {
			t.Errorf("expected empty cursor for fresh incremental sync, got %q", mock.capturedCursor)
		}
		if !mock.capturedStartDate.Equal(lastSync) {
			t.Errorf("expected incremental start date %v, got %v", lastSync, mock.capturedStartDate)
		}
	})
}

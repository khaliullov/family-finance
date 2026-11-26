package transaction

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/khaliullov/family-finance/internal/connector"
	"github.com/khaliullov/family-finance/internal/database"
)

// -----------------------------------------------------------------------------
// 1. Pagination Invariant Tests: Reject Malformed Cursors
// -----------------------------------------------------------------------------

type malformedPaginationMockConnector struct {
	malformType string // "empty_cursor" or "same_cursor"
	pageCount   int
}

func (m *malformedPaginationMockConnector) ProviderCode() string { return "mock_malformed_page" }
func (m *malformedPaginationMockConnector) HealthCheck(ctx context.Context, encryptedCredentials []byte) error {
	return nil
}
func (m *malformedPaginationMockConnector) FetchCards(ctx context.Context, encryptedCredentials []byte, accountExternalID string) ([]connector.CardDTO, error) {
	return nil, nil
}
func (m *malformedPaginationMockConnector) FetchAccounts(ctx context.Context, encryptedCredentials []byte) ([]connector.AccountDTO, error) {
	return []connector.AccountDTO{{ExternalID: "acc_malform_1", Name: "Malform Account", Type: "checking", Currency: "RUB"}}, nil
}
func (m *malformedPaginationMockConnector) FetchBalances(ctx context.Context, encryptedCredentials []byte, accountExternalIDs []string) ([]connector.BalanceDTO, error) {
	return nil, nil
}
func (m *malformedPaginationMockConnector) FetchTransactions(ctx context.Context, encryptedCredentials []byte, params connector.SyncParams) (*connector.SyncResult, error) {
	m.pageCount++
	currentPage := m.pageCount

	var hasMore bool
	var nextCursor string

	if m.malformType == "empty_cursor" {
		if currentPage == 1 {
			// Page 1 is valid
			hasMore = true
			nextCursor = "valid_cursor_1"
		} else {
			// Page 2 is malformed: HasMore=true but NextCursor=""
			hasMore = true
			nextCursor = ""
		}
	} else if m.malformType == "same_cursor" {
		if currentPage == 1 {
			// Page 1 advances to "loop_cur"
			hasMore = true
			nextCursor = "loop_cur"
		} else {
			// Page 2 returns same cursor: HasMore=true and NextCursor=="loop_cur"
			hasMore = true
			nextCursor = "loop_cur"
		}
	}

	tx := connector.CanonicalBankTransaction{
		ExternalID:        fmt.Sprintf("tx_malform_%d", currentPage),
		AccountExternalID: "acc_malform_1",
		Amount:            connector.CurrencyAmount{MinorUnits: int64(currentPage * 1000), Currency: "RUB"},
		AccountAmount:     connector.CurrencyAmount{MinorUnits: int64(currentPage * 1000), Currency: "RUB"},
		TransactionTime:   time.Now().UTC(),
		BookingStatus:     "posted",
		OperationType:     "purchase",
		RawDescription:    fmt.Sprintf("Tx Page %d", currentPage),
	}

	return &connector.SyncResult{
		Transactions: []connector.CanonicalBankTransaction{tx},
		RawPayload:   []byte(fmt.Sprintf(`{"page": %d}`, currentPage)),
		Progress: connector.SyncProgress{
			HasMore:    hasMore,
			NextCursor: nextCursor,
		},
	}, nil
}

func TestPagination_HasMoreWithEmptyCursor_Rejected(t *testing.T) {
	pool := getWorkerPool(t)
	defer pool.Close()
	ctx := context.Background()

	familyID, _, connID := setupTenantFixture(t, "mock_malformed_page")

	mock := &malformedPaginationMockConnector{malformType: "empty_cursor"}
	registry := connector.NewRegistry()
	registry.Register(mock)

	svc := NewService(pool, registry)

	stats, err := svc.SyncConnection(ctx, familyID, connID, SyncOptions{MaxPages: 10})
	if err == nil {
		t.Fatalf("expected error on HasMore=true with empty NextCursor, got nil")
	}

	if !errors.Is(err, ErrInvalidPagination) {
		t.Fatalf("expected ErrInvalidPagination, got: %v", err)
	}

	// Verify error classification is non-retryable
	var syncErr *connector.SyncError
	if errors.As(err, &syncErr) {
		if syncErr.Retryable {
			t.Errorf("expected ErrInvalidPagination to be classified as non-retryable")
		}
	} else {
		t.Errorf("expected error to be *connector.SyncError")
	}

	// Invariant verification in database:
	// 1. Page 1 transactions must be preserved and committed
	// 2. last_synced_at must NOT be updated (must be NULL)
	// 3. sync_cursor must remain at last valid checkpoint ("valid_cursor_1")
	// 4. status must be set to 'error'
	_ = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		var txCount int
		_ = tx.QueryRow(ctx, "SELECT count(*) FROM public.bank_transactions WHERE family_id = $1", familyID).Scan(&txCount)
		if txCount != 1 {
			t.Errorf("expected page 1 tx to be committed (count=1), got %d", txCount)
		}

		var cursor *string
		var lastSyncedAt *time.Time
		var status string
		_ = tx.QueryRow(ctx, "SELECT sync_cursor, last_synced_at, status FROM public.bank_connections WHERE family_id = $1 AND id = $2", familyID, connID).Scan(&cursor, &lastSyncedAt, &status)

		if cursor == nil || *cursor != "valid_cursor_1" {
			t.Errorf("expected sync_cursor to remain 'valid_cursor_1', got %v", cursor)
		}
		if lastSyncedAt != nil {
			t.Errorf("last_synced_at must NOT be updated on malformed pagination, got: %v", lastSyncedAt)
		}
		if status != "error" {
			t.Errorf("expected status 'error', got %s", status)
		}
		return nil
	})

	_ = stats
}

func TestPagination_HasMoreWithIdenticalCursor_Rejected(t *testing.T) {
	pool := getWorkerPool(t)
	defer pool.Close()
	ctx := context.Background()

	familyID, _, connID := setupTenantFixture(t, "mock_malformed_page")

	mock := &malformedPaginationMockConnector{malformType: "same_cursor"}
	registry := connector.NewRegistry()
	registry.Register(mock)

	svc := NewService(pool, registry)

	_, err := svc.SyncConnection(ctx, familyID, connID, SyncOptions{MaxPages: 10})
	if err == nil {
		t.Fatalf("expected error on HasMore=true with identical NextCursor, got nil")
	}

	if !errors.Is(err, ErrCursorLoop) {
		t.Fatalf("expected ErrCursorLoop, got: %v", err)
	}

	// Invariant verification in database:
	// Page 1 committed, sync_cursor remains at checkpoint, last_synced_at is nil
	_ = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		var txCount int
		_ = tx.QueryRow(ctx, "SELECT count(*) FROM public.bank_transactions WHERE family_id = $1", familyID).Scan(&txCount)
		if txCount != 1 {
			t.Errorf("expected page 1 tx committed, got %d", txCount)
		}

		var cursor *string
		var lastSyncedAt *time.Time
		_ = tx.QueryRow(ctx, "SELECT sync_cursor, last_synced_at FROM public.bank_connections WHERE family_id = $1 AND id = $2", familyID, connID).Scan(&cursor, &lastSyncedAt)

		if cursor == nil || *cursor != "loop_cur" {
			t.Errorf("expected sync_cursor 'loop_cur', got %v", cursor)
		}
		if lastSyncedAt != nil {
			t.Errorf("last_synced_at must NOT be updated on cursor loop, got: %v", lastSyncedAt)
		}
		return nil
	})
}

// -----------------------------------------------------------------------------
// 2. ForceFullSync Checkpoint Resumption
// -----------------------------------------------------------------------------

func TestForceFullSync_ResumesFromCommittedCheckpoint(t *testing.T) {
	pool := getWorkerPool(t)
	defer pool.Close()
	ctx := context.Background()

	familyID, _, connID := setupTenantFixture(t, "mock_paginated")

	paginatedConn := &paginatedMockConnector{
		totalPages: 5,
		failOnPage: 3, // Fail on page 3
	}
	registry := connector.NewRegistry()
	registry.Register(paginatedConn)

	svc := NewService(pool, registry)

	// Attempt 1: Start full sync
	_, err := svc.SyncConnection(ctx, familyID, connID, SyncOptions{ForceFullSync: true})
	if err == nil {
		t.Fatalf("expected failure on page 3, got nil")
	}

	// Verify checkpoint at page 2 exists in database
	var checkpointCursor *string
	_ = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT sync_cursor FROM public.bank_connections WHERE family_id = $1 AND id = $2", familyID, connID).Scan(&checkpointCursor)
	})
	if checkpointCursor == nil || *checkpointCursor != "cursor_2" {
		t.Fatalf("expected checkpoint 'cursor_2', got %v", checkpointCursor)
	}

	// Attempt 2: Worker retries the ForceFullSync job!
	// It MUST resume from cursor_2 instead of ignoring the checkpoint!
	paginatedConn.failOnPage = 0 // Clear failure condition

	stats, err := svc.SyncConnection(ctx, familyID, connID, SyncOptions{ForceFullSync: true})
	if err != nil {
		t.Fatalf("resumed full sync failed: %v", err)
	}

	// Remaining pages 3, 4, 5 = 3 transactions ingested
	if stats.TransactionsIngested != 3 {
		t.Errorf("expected 3 remaining transactions ingested on retry, got %d", stats.TransactionsIngested)
	}

	// Verify total transactions in DB is 5 (no duplicates)
	_ = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		var totalTx int
		_ = tx.QueryRow(ctx, "SELECT count(*) FROM public.bank_transactions WHERE family_id = $1", familyID).Scan(&totalTx)
		if totalTx != 5 {
			t.Errorf("expected 5 total transactions in DB, got %d", totalTx)
		}

		var cursor *string
		var status string
		_ = tx.QueryRow(ctx, "SELECT sync_cursor, status FROM public.bank_connections WHERE family_id = $1 AND id = $2", familyID, connID).Scan(&cursor, &status)
		if cursor != nil {
			t.Errorf("expected sync_cursor to be cleared upon complete sync, got %s", *cursor)
		}
		if status != "active" {
			t.Errorf("expected status 'active', got %s", status)
		}
		return nil
	})
}

// -----------------------------------------------------------------------------
// 3. Durable Discrepancy Storage & Idempotency
// -----------------------------------------------------------------------------

type discrepancyMockConnector struct {
	modifyTerminal bool
}

func (d *discrepancyMockConnector) ProviderCode() string { return "mock_disc" }
func (d *discrepancyMockConnector) HealthCheck(ctx context.Context, encryptedCredentials []byte) error {
	return nil
}
func (d *discrepancyMockConnector) FetchCards(ctx context.Context, encryptedCredentials []byte, accountExternalID string) ([]connector.CardDTO, error) {
	return nil, nil
}
func (d *discrepancyMockConnector) FetchAccounts(ctx context.Context, encryptedCredentials []byte) ([]connector.AccountDTO, error) {
	return []connector.AccountDTO{{ExternalID: "acc_disc_1", Name: "Disc Account", Type: "checking", Currency: "RUB"}}, nil
}
func (d *discrepancyMockConnector) FetchBalances(ctx context.Context, encryptedCredentials []byte, accountExternalIDs []string) ([]connector.BalanceDTO, error) {
	return nil, nil
}
func (d *discrepancyMockConnector) FetchTransactions(ctx context.Context, encryptedCredentials []byte, params connector.SyncParams) (*connector.SyncResult, error) {
	amt := int64(10000)
	desc := "Initial Description"
	if d.modifyTerminal {
		amt = int64(99999) // Attempted variance on posted transaction!
		desc = "Tampered Description"
	}

	tx := connector.CanonicalBankTransaction{
		ExternalID:        "tx_terminal_test",
		AccountExternalID: "acc_disc_1",
		Amount:            connector.CurrencyAmount{MinorUnits: amt, Currency: "RUB"},
		AccountAmount:     connector.CurrencyAmount{MinorUnits: amt, Currency: "RUB"},
		TransactionTime:   time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC),
		BookingStatus:     "posted",
		OperationType:     "purchase",
		RawDescription:    desc,
	}

	return &connector.SyncResult{
		Transactions: []connector.CanonicalBankTransaction{tx},
		RawPayload:   []byte(`{"status": "ok"}`),
		Progress: connector.SyncProgress{
			HasMore: false,
		},
	}, nil
}

func TestDurableReconciliationDiscrepancies(t *testing.T) {
	pool := getWorkerPool(t)
	defer pool.Close()
	ctx := context.Background()

	familyID, _, connID := setupTenantFixture(t, "mock_disc")

	mock := &discrepancyMockConnector{modifyTerminal: false}
	registry := connector.NewRegistry()
	registry.Register(mock)

	svc := NewService(pool, registry)

	// 1. Ingest original posted transaction
	_, err := svc.SyncConnection(ctx, familyID, connID, SyncOptions{})
	if err != nil {
		t.Fatalf("initial sync failed: %v", err)
	}

	// 2. Ingest modified transaction (attempted modification to terminal evidence)
	mock.modifyTerminal = true
	stats, err := svc.SyncConnection(ctx, familyID, connID, SyncOptions{})
	if err != nil {
		t.Fatalf("second sync failed: %v", err)
	}

	if stats.DiscrepanciesDetected != 2 { // amount and description
		t.Errorf("expected 2 discrepancies detected, got %d", stats.DiscrepanciesDetected)
	}

	// 3. Verify durable storage in public.reconciliation_discrepancies
	_ = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		var discCount int
		err := tx.QueryRow(ctx, `
			SELECT count(*) FROM public.reconciliation_discrepancies
			WHERE family_id = $1 AND bank_connection_id = $2 AND external_id = 'tx_terminal_test'
		`, familyID, connID).Scan(&discCount)
		if err != nil || discCount != 2 {
			t.Fatalf("expected 2 durable discrepancy rows in DB, got count=%d, err=%v", discCount, err)
		}

		// Verify original bank transaction was NOT modified (immutability preserved)
		var recordedAmt int64
		var recordedDesc string
		err = tx.QueryRow(ctx, `
			SELECT account_amount_minor, raw_description FROM public.bank_transactions
			WHERE family_id = $1 AND external_id = 'tx_terminal_test'
		`, familyID).Scan(&recordedAmt, &recordedDesc)
		if err != nil {
			t.Fatalf("query bank_transactions failed: %v", err)
		}
		if recordedAmt != -10000 || recordedDesc != "Initial Description" {
			t.Fatalf("CRITICAL: terminal transaction was mutated! amt=%d desc=%q", recordedAmt, recordedDesc)
		}
		return nil
	})

	// 4. Repeated sync with same modified payload -> must be idempotent via idempotency_hash
	statsRepeated, err := svc.SyncConnection(ctx, familyID, connID, SyncOptions{})
	if err != nil {
		t.Fatalf("third sync failed: %v", err)
	}
	_ = statsRepeated

	_ = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		var discCount int
		_ = tx.QueryRow(ctx, `
			SELECT count(*) FROM public.reconciliation_discrepancies
			WHERE family_id = $1 AND bank_connection_id = $2 AND external_id = 'tx_terminal_test'
		`, familyID, connID).Scan(&discCount)
		if discCount != 2 {
			t.Errorf("expected idempotency constraint to prevent duplicate rows: got count=%d", discCount)
		}
		return nil
	})

	// 5. Tenant isolation: Family B cannot see Family A's discrepancies
	familyB, _, _ := setupTenantFixture(t, "mock_disc")
	_ = database.WithFamilyTx(ctx, pool, familyB, func(tx pgx.Tx) error {
		var bCount int
		_ = tx.QueryRow(ctx, "SELECT count(*) FROM public.reconciliation_discrepancies WHERE family_id = $1", familyB).Scan(&bCount)
		if bCount != 0 {
			t.Errorf("tenant isolation violation: Family B saw %d discrepancies", bCount)
		}
		return nil
	})
}

// -----------------------------------------------------------------------------
// 4. Per-Connection Sync Concurrency & Out-of-Order Safety
// -----------------------------------------------------------------------------

func TestPerConnectionSync_OutOfOrderProtection(t *testing.T) {
	pool := getWorkerPool(t)
	defer pool.Close()
	ctx := context.Background()

	familyID, _, connID := setupTenantFixture(t, "fixture")

	// 1. Manually claim ownership for Run 1
	token1 := uuid.New()
	var version1 int64
	var fencing1 int64
	_ = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE public.bank_connections
			SET active_sync_token = $1,
			    sync_version = sync_version + 1,
			    sync_locked_until = NOW() + INTERVAL '10 minutes',
			    updated_at = NOW()
			WHERE family_id = $2 AND id = $3
		`, token1, familyID, connID)
		if err != nil {
			return err
		}
		return tx.QueryRow(ctx, "SELECT sync_version, fencing_token FROM public.bank_connections WHERE id = $1", connID).Scan(&version1, &fencing1)
	})

	// 2. Newer Run 2 preempts / claims ownership (e.g. after lock expiry simulation)
	token2 := uuid.New()
	var version2 int64
	_ = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE public.bank_connections
			SET active_sync_token = $1,
			    sync_version = sync_version + 1,
			    sync_locked_until = NOW() + INTERVAL '10 minutes',
			    status = 'active',
			    updated_at = NOW()
			WHERE family_id = $2 AND id = $3
		`, token2, familyID, connID)
		if err != nil {
			return err
		}
		return tx.QueryRow(ctx, "SELECT sync_version FROM public.bank_connections WHERE id = $1", connID).Scan(&version2)
	})

	if version2 <= version1 {
		t.Fatalf("expected version2 (%d) > version1 (%d)", version2, version1)
	}

	// 3. Older Run 1 attempts to record failure -> MUST match 0 rows and NOT overwrite Run 2's status
	svc := NewService(pool, connector.NewRegistry())
	svc.recordConnectionFailure(ctx, familyID, connID, token1, version1, fencing1, errors.New("stale failure from run 1"))

	_ = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		var status string
		var errMsg *string
		_ = tx.QueryRow(ctx, "SELECT status, error_message FROM public.bank_connections WHERE family_id = $1 AND id = $2", familyID, connID).Scan(&status, &errMsg)
		if status != "active" {
			t.Errorf("stale run 1 corrupted connection status: expected 'active', got %s", status)
		}
		if errMsg != nil {
			t.Errorf("stale run 1 set error_message: %v", *errMsg)
		}
		return nil
	})
}

// -----------------------------------------------------------------------------
// 5. Deterministic Primary Card Selection
// -----------------------------------------------------------------------------

type multiPrimaryMockConnector struct {
	reverseOrder bool
}

func (m *multiPrimaryMockConnector) ProviderCode() string { return "mock_multi_primary" }
func (m *multiPrimaryMockConnector) HealthCheck(ctx context.Context, encryptedCredentials []byte) error {
	return nil
}
func (m *multiPrimaryMockConnector) FetchCards(ctx context.Context, encryptedCredentials []byte, accountExternalID string) ([]connector.CardDTO, error) {
	return nil, nil
}
func (m *multiPrimaryMockConnector) FetchBalances(ctx context.Context, encryptedCredentials []byte, accountExternalIDs []string) ([]connector.BalanceDTO, error) {
	return nil, nil
}
func (m *multiPrimaryMockConnector) FetchTransactions(ctx context.Context, encryptedCredentials []byte, params connector.SyncParams) (*connector.SyncResult, error) {
	return &connector.SyncResult{Progress: connector.SyncProgress{HasMore: false}}, nil
}
func (m *multiPrimaryMockConnector) FetchAccounts(ctx context.Context, encryptedCredentials []byte) ([]connector.AccountDTO, error) {
	cards := []connector.CardDTO{
		{ExternalID: "card_Z", DisplayName: "Card Z", IsPrimary: true, Status: "ACTIVE"},
		{ExternalID: "card_A", DisplayName: "Card A", IsPrimary: true, Status: "ACTIVE"},
		{ExternalID: "card_M", DisplayName: "Card M", IsPrimary: true, Status: "ACTIVE"},
	}
	if m.reverseOrder {
		cards = []connector.CardDTO{
			{ExternalID: "card_M", DisplayName: "Card M", IsPrimary: true, Status: "ACTIVE"},
			{ExternalID: "card_Z", DisplayName: "Card Z", IsPrimary: true, Status: "ACTIVE"},
			{ExternalID: "card_A", DisplayName: "Card A", IsPrimary: true, Status: "ACTIVE"},
		}
	}
	return []connector.AccountDTO{
		{
			ExternalID: "acc_multi_primary",
			Name:       "Multi Primary Account",
			Type:       "checking",
			Currency:   "RUB",
			Cards:      cards,
		},
	}, nil
}

func TestDeterministicPrimaryCardSelection(t *testing.T) {
	pool := getWorkerPool(t)
	defer pool.Close()
	ctx := context.Background()

	familyID, _, connID := setupTenantFixture(t, "mock_multi_primary")

	mock := &multiPrimaryMockConnector{reverseOrder: false}
	registry := connector.NewRegistry()
	registry.Register(mock)

	svc := NewService(pool, registry)

	// Run 1: Order [Z, A, M]
	_, err := svc.SyncConnection(ctx, familyID, connID, SyncOptions{})
	if err != nil {
		t.Fatalf("sync 1 failed: %v", err)
	}

	// Invariant: Card A must be primary (lexicographically smallest)
	_ = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		var primaryExtID string
		var count int
		_ = tx.QueryRow(ctx, "SELECT count(*), max(external_id) FROM public.cards WHERE family_id = $1 AND is_primary = TRUE", familyID).Scan(&count, &primaryExtID)
		if count != 1 {
			t.Fatalf("expected exactly 1 primary card, got %d", count)
		}
		if primaryExtID != "card_A" {
			t.Errorf("expected card_A to win primary tie-break, got %s", primaryExtID)
		}
		return nil
	})

	// Run 2: Provider returns cards in different order [M, Z, A]
	mock.reverseOrder = true
	_, err = svc.SyncConnection(ctx, familyID, connID, SyncOptions{})
	if err != nil {
		t.Fatalf("sync 2 failed: %v", err)
	}

	// Invariant: Card A MUST STILL BE PRIMARY (deterministic stability)
	_ = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		var primaryExtID string
		var count int
		_ = tx.QueryRow(ctx, "SELECT count(*), max(external_id) FROM public.cards WHERE family_id = $1 AND is_primary = TRUE", familyID).Scan(&count, &primaryExtID)
		if count != 1 {
			t.Fatalf("expected exactly 1 primary card after re-sync, got %d", count)
		}
		if primaryExtID != "card_A" {
			t.Errorf("deterministic tie-break failure: expected card_A to remain primary, got %s", primaryExtID)
		}
		return nil
	})
}

// -----------------------------------------------------------------------------
// 6. Reconciliation Discrepancy Sanitization & Unambiguous Idempotency Tests
// -----------------------------------------------------------------------------

func TestDiscrepancy_IdempotencyHash_DelimiterCollisionResistance(t *testing.T) {
	familyID := uuid.New()
	txID := uuid.New()

	// Delimiter collision attack vector: if hash used simple colon concatenation "family:tx:field:rec:inc",
	// then ("field:a", "b", "c") and ("field", "a:b", "c") would yield identical string "family:tx:field:a:b:c"
	hash1 := ComputeDiscrepancyIdempotencyHash(familyID, txID, "field:a", "b", "c")
	hash2 := ComputeDiscrepancyIdempotencyHash(familyID, txID, "field", "a:b", "c")

	if hash1 == hash2 {
		t.Fatalf("delimiter collision detected: hash1 and hash2 produced identical hash %s", hash1)
	}

	// Another test case with colons inside values
	hash3 := ComputeDiscrepancyIdempotencyHash(familyID, txID, "description", "rec:1", "inc:2")
	hash4 := ComputeDiscrepancyIdempotencyHash(familyID, txID, "description:rec", "1", "inc:2")
	if hash3 == hash4 {
		t.Fatalf("delimiter collision detected: hash3 and hash4 produced identical hash %s", hash3)
	}
}

func TestDiscrepancy_SanitizesSensitiveDescriptions(t *testing.T) {
	pool := getWorkerPool(t)
	defer pool.Close()
	ctx := context.Background()

	familyID, userID, connID := setupTenantFixture(t, "mock_discrepancy_sanitizer")

	var accountID uuid.UUID
	accID := uuid.New()
	err := database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO public.accounts (id, family_id, bank_connection_id, owner_user_id, external_id, name, type, currency)
			VALUES ($1, $2, $3, $4, 'acc_disc_san', 'Disc San Account', 'checking', 'RUB')
			RETURNING id
		`, accID, familyID, connID, userID).Scan(&accountID)
	})
	if err != nil {
		t.Fatalf("failed to create account: %v", err)
	}

	// 1. Ingest initial posted transaction with PAN in description via IngestBatch (ensuring Stage 1 double-entry invariants)
	initialTx := []connector.CanonicalBankTransaction{
		{
			ExternalID:        "ext_disc_pan",
			AccountExternalID: "acc_disc_san",
			Amount:            connector.CurrencyAmount{MinorUnits: 10000, Currency: "RUB"},
			AccountAmount:     connector.CurrencyAmount{MinorUnits: 10000, Currency: "RUB"},
			BookingStatus:     "posted",
			OperationType:     "purchase",
			TransactionTime:   time.Now().UTC().Add(-24 * time.Hour),
			RawDescription:    "Purchase at Store with Card 4111222233334444",
		},
	}

	var rawPayloadID uuid.UUID
	err = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		var err error
		rawPayloadID, err = PreserveRawPayload(ctx, tx, familyID, &connID, "test", []byte(`{"initial": true}`))
		if err != nil {
			return err
		}
		_, err = IngestBatch(ctx, tx, familyID, &connID, &rawPayloadID, initialTx, map[string]uuid.UUID{"acc_disc_san": accountID}, nil, userID)
		return err
	})
	if err != nil {
		t.Fatalf("failed to ingest initial terminal transaction: %v", err)
	}

	// 2. Incoming transaction with modified PAN description
	incoming := []connector.CanonicalBankTransaction{
		{
			ExternalID:        "ext_disc_pan",
			AccountExternalID: "acc_disc_san",
			Amount:            connector.CurrencyAmount{MinorUnits: 10000, Currency: "RUB"},
			AccountAmount:     connector.CurrencyAmount{MinorUnits: 10000, Currency: "RUB"},
			BookingStatus:     "posted",
			OperationType:     "purchase",
			TransactionTime:   time.Now().UTC().Add(-24 * time.Hour),
			RawDescription:    "Purchase at Store with Card 5555666677778888 and pin 1234",
		},
	}

	_ = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		stats, err := IngestBatch(ctx, tx, familyID, &connID, &rawPayloadID, incoming, map[string]uuid.UUID{"acc_disc_san": accountID}, nil, uuid.New())
		if err != nil {
			t.Fatalf("IngestBatch failed: %v", err)
		}
		if stats.DiscrepanciesDetected != 1 {
			t.Fatalf("expected 1 discrepancy, got %d", stats.DiscrepanciesDetected)
		}
		return nil
	})

	// 3. Inspect reconciliation_discrepancies in DB
	_ = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		var recordedVal, incomingVal string
		err := tx.QueryRow(ctx, `
			SELECT recorded_value, incoming_value
			FROM public.reconciliation_discrepancies
			WHERE family_id = $1 AND external_id = 'ext_disc_pan' AND field = 'description'
		`, familyID).Scan(&recordedVal, &incomingVal)
		if err != nil {
			t.Fatalf("failed to query discrepancy: %v", err)
		}

		// Verify recorded PAN was masked to last 4
		if strings.Contains(recordedVal, "4111222233334444") {
			t.Errorf("recorded_value contains plaintext PAN: %s", recordedVal)
		}
		if !strings.Contains(recordedVal, "****4444") {
			t.Errorf("expected masked PAN ****4444 in recorded_value, got: %s", recordedVal)
		}

		// Verify incoming PAN was masked to last 4
		if strings.Contains(incomingVal, "5555666677778888") {
			t.Errorf("incoming_value contains plaintext PAN: %s", incomingVal)
		}
		if !strings.Contains(incomingVal, "****8888") {
			t.Errorf("expected masked PAN ****8888 in incoming_value, got: %s", incomingVal)
		}

		// Verify original bank_transactions record remains immutable with original evidence
		var origDesc string
		_ = tx.QueryRow(ctx, "SELECT raw_description FROM public.bank_transactions WHERE family_id = $1 AND external_id = 'ext_disc_pan'", familyID).Scan(&origDesc)
		if origDesc != "Purchase at Store with Card 4111222233334444" {
			t.Errorf("original bank_transaction evidence was improperly mutated: %s", origDesc)
		}

		return nil
	})
}

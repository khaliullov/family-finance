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
	"github.com/khaliullov/family-finance/internal/connector/mock"
	"github.com/khaliullov/family-finance/internal/database"
)

// -----------------------------------------------------------------------------
// 1. Raw Payload Byte Preservation & Sanitization
// -----------------------------------------------------------------------------

func TestRemediation_RawPayloadBytePreservation(t *testing.T) {
	pool := getWorkerPool(t)
	defer pool.Close()
	ctx := context.Background()

	familyID, _, connID := setupTenantFixture(t, "fixture")

	// Two semantically identical JSON payloads with different key ordering and whitespace
	payloadA := []byte(`{"amount": 1000, "currency": "RUB"}`)
	payloadB := []byte("{\n  \"currency\": \"RUB\",\n  \"amount\": 1000\n}")

	var idA, idB uuid.UUID

	err := database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		var err error
		idA, err = PreserveRawPayload(ctx, tx, familyID, &connID, "bank_api", payloadA)
		if err != nil {
			return fmt.Errorf("failed to preserve payload A: %w", err)
		}

		idB, err = PreserveRawPayload(ctx, tx, familyID, &connID, "bank_api", payloadB)
		if err != nil {
			return fmt.Errorf("failed to preserve payload B: %w", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("preserving payloads failed: %v", err)
	}

	if idA == idB {
		t.Fatalf("expected distinct payload IDs for distinct byte formatting, got identical %s", idA)
	}

	// Verify exact byte preservation in raw_payload_bytes
	err = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		var bytesA, bytesB []byte
		var hashA, hashB string
		err := tx.QueryRow(ctx, "SELECT raw_payload_bytes, payload_hash FROM public.raw_bank_payloads WHERE family_id = $1 AND id = $2", familyID, idA).Scan(&bytesA, &hashA)
		if err != nil {
			return fmt.Errorf("query A failed: %w", err)
		}
		err = tx.QueryRow(ctx, "SELECT raw_payload_bytes, payload_hash FROM public.raw_bank_payloads WHERE family_id = $1 AND id = $2", familyID, idB).Scan(&bytesB, &hashB)
		if err != nil {
			return fmt.Errorf("query B failed: %w", err)
		}

		if string(bytesA) != string(payloadA) {
			t.Errorf("verbatim bytes A mismatch: expected %q, got %q", string(payloadA), string(bytesA))
		}
		if string(bytesB) != string(payloadB) {
			t.Errorf("verbatim bytes B mismatch: expected %q, got %q", string(payloadB), string(bytesB))
		}
		if hashA == hashB {
			t.Errorf("expected different SHA-256 hashes for different verbatim bytes, got identical %s", hashA)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verification failed: %v", err)
	}

	// Sanitization boundary: invalid non-JSON payload rejected
	invalidPayload := []byte("Not a valid json string {missing_brace")
	err = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		_, err := PreserveRawPayload(ctx, tx, familyID, &connID, "bank_api", invalidPayload)
		return err
	})
	if err == nil {
		t.Fatalf("expected invalid JSON payload to be rejected by sanitization boundary, but it succeeded")
	}
}

// -----------------------------------------------------------------------------
// 2. Primary Card Synchronization
// -----------------------------------------------------------------------------

func TestRemediation_PrimaryCardSynchronization(t *testing.T) {
	pool := getWorkerPool(t)
	defer pool.Close()
	ctx := context.Background()

	familyID, _, connID := setupTenantFixture(t, "mock")

	mockConn := mock.NewMockConnector("mock")
	registry := connector.NewRegistry()
	registry.Register(mockConn)

	var currentAccounts []connector.AccountDTO
	mockConn.FetchAccountsFunc = func(ctx context.Context, credentials []byte) ([]connector.AccountDTO, error) {
		return currentAccounts, nil
	}

	svc := NewService(pool, registry)

	// Phase 1: Card A is primary, Card B is secondary
	currentAccounts = []connector.AccountDTO{
		{
			ExternalID: "acc_card_test_1",
			Name:       "Card Test Account",
			Type:       "checking",
			Currency:   "RUB",
			Cards: []connector.CardDTO{
				{
					ExternalID:        "card_A",
					AccountExternalID: "acc_card_test_1",
					DisplayName:       "Card A",
					IsPrimary:         true,
					Status:            "ACTIVE",
				},
				{
					ExternalID:        "card_B",
					AccountExternalID: "acc_card_test_1",
					DisplayName:       "Card B",
					IsPrimary:         false,
					Status:            "ACTIVE",
				},
			},
		},
	}

	_, err := svc.SyncConnection(ctx, familyID, connID, SyncOptions{})
	if err != nil {
		t.Fatalf("phase 1 sync failed: %v", err)
	}

	// Verify Card A is primary, Card B is not
	_ = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		var aPrimary, bPrimary bool
		_ = tx.QueryRow(ctx, "SELECT is_primary FROM public.cards WHERE family_id = $1 AND external_id = 'card_A'", familyID).Scan(&aPrimary)
		_ = tx.QueryRow(ctx, "SELECT is_primary FROM public.cards WHERE family_id = $1 AND external_id = 'card_B'", familyID).Scan(&bPrimary)
		if !aPrimary || bPrimary {
			t.Errorf("phase 1 failed: card A primary=%v, card B primary=%v", aPrimary, bPrimary)
		}
		return nil
	})

	// Phase 2: Provider flips primary from Card A to Card B
	currentAccounts[0].Cards[0].IsPrimary = false
	currentAccounts[0].Cards[1].IsPrimary = true

	_, err = svc.SyncConnection(ctx, familyID, connID, SyncOptions{})
	if err != nil {
		t.Fatalf("phase 2 flip sync failed: %v", err)
	}

	// Verify Card B is now primary, Card A is not
	_ = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		var aPrimary, bPrimary bool
		_ = tx.QueryRow(ctx, "SELECT is_primary FROM public.cards WHERE family_id = $1 AND external_id = 'card_A'", familyID).Scan(&aPrimary)
		_ = tx.QueryRow(ctx, "SELECT is_primary FROM public.cards WHERE family_id = $1 AND external_id = 'card_B'", familyID).Scan(&bPrimary)
		if aPrimary || !bPrimary {
			t.Errorf("phase 2 failed: card A primary=%v, card B primary=%v", aPrimary, bPrimary)
		}
		return nil
	})

	// Phase 3: Provider returns a CLOSED card with is_primary = true
	currentAccounts[0].Cards = append(currentAccounts[0].Cards, connector.CardDTO{
		ExternalID:        "card_C_closed",
		AccountExternalID: "acc_card_test_1",
		DisplayName:       "Closed Card C",
		IsPrimary:         true,
		Status:            "CLOSED",
	})

	_, err = svc.SyncConnection(ctx, familyID, connID, SyncOptions{})
	if err != nil {
		t.Fatalf("phase 3 closed card sync failed: %v", err)
	}

	// Verify Card C was normalized to is_primary = false
	_ = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		var cPrimary bool
		var cStatus string
		_ = tx.QueryRow(ctx, "SELECT is_primary, status FROM public.cards WHERE family_id = $1 AND external_id = 'card_C_closed'", familyID).Scan(&cPrimary, &cStatus)
		if cPrimary || cStatus != "CLOSED" {
			t.Errorf("expected closed card C to have is_primary=false, got primary=%v status=%s", cPrimary, cStatus)
		}
		return nil
	})

	// Phase 4: Provider incorrectly marks multiple cards primary -> deterministic single primary
	currentAccounts[0].Cards[0].IsPrimary = true
	currentAccounts[0].Cards[1].IsPrimary = true

	_, err = svc.SyncConnection(ctx, familyID, connID, SyncOptions{})
	if err != nil {
		t.Fatalf("phase 4 duplicate primary sync failed: %v", err)
	}

	_ = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		var primaryCount int
		_ = tx.QueryRow(ctx, `
			SELECT count(*) FROM public.cards 
			WHERE family_id = $1 AND account_id = (SELECT id FROM public.accounts WHERE family_id = $1 AND external_id = 'acc_card_test_1') AND is_primary = TRUE
		`, familyID).Scan(&primaryCount)
		if primaryCount != 1 {
			t.Errorf("expected exactly 1 primary card after multi-primary payload, got %d", primaryCount)
		}
		return nil
	})

	// Phase 5: Concurrent card updates on the same account
	const concurrentSyncs = 4
	var wg sync.WaitGroup
	syncErrs := make([]error, concurrentSyncs)
	for i := 0; i < concurrentSyncs; i++ {
		wg.Add(1)
		idx := i
		go func() {
			defer wg.Done()
			_, syncErrs[idx] = svc.SyncConnection(ctx, familyID, connID, SyncOptions{})
		}()
	}
	wg.Wait()
	for i, sErr := range syncErrs {
		if sErr != nil {
			t.Fatalf("concurrent sync %d failed: %v", i, sErr)
		}
	}
}

// -----------------------------------------------------------------------------
// 3. Provider Corrections & Terminal Transactions
// -----------------------------------------------------------------------------

func TestRemediation_ProviderCorrectionsAndTerminalTransactions(t *testing.T) {
	pool := getWorkerPool(t)
	defer pool.Close()
	ctx := context.Background()

	familyID, _, connID := setupTenantFixture(t, "mock")

	mockConn := mock.NewMockConnector("mock")
	registry := connector.NewRegistry()
	registry.Register(mockConn)

	var currentAccounts []connector.AccountDTO
	var currentTxs []connector.CanonicalBankTransaction
	var currentRaw []byte

	mockConn.FetchAccountsFunc = func(ctx context.Context, credentials []byte) ([]connector.AccountDTO, error) {
		return currentAccounts, nil
	}
	mockConn.FetchTransactionsFunc = func(ctx context.Context, credentials []byte, params connector.SyncParams) (*connector.SyncResult, error) {
		return &connector.SyncResult{
			Transactions: currentTxs,
			RawPayload:   currentRaw,
			Progress: connector.SyncProgress{
				HasMore: false,
			},
		}, nil
	}

	svc := NewService(pool, registry)

	// Step 1: Initial sync with posted transaction tx_term_100
	currentAccounts = []connector.AccountDTO{
		{
			ExternalID: "acc_corr_1",
			Name:       "Correction Test Account",
			Type:       "checking",
			Currency:   "RUB",
		},
	}
	originalTime := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	originalSettled := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)

	currentTxs = []connector.CanonicalBankTransaction{
		{
			ExternalID:        "tx_term_100",
			AccountExternalID: "acc_corr_1",
			Amount:            connector.CurrencyAmount{MinorUnits: 100000, Currency: "RUB"},
			AccountAmount:     connector.CurrencyAmount{MinorUnits: 100000, Currency: "RUB"},
			TransactionTime:   originalTime,
			SettledTime:       &originalSettled,
			BookingStatus:     "posted",
			OperationType:     "purchase",
			RawDescription:    "Original Merchant Charge",
		},
	}
	currentRaw = []byte(`{"tx": "tx_term_100", "step": 1}`)

	stats1, err := svc.SyncConnection(ctx, familyID, connID, SyncOptions{})
	if err != nil {
		t.Fatalf("initial sync failed: %v", err)
	}
	if stats1.TransactionsIngested != 1 {
		t.Fatalf("expected 1 transaction ingested, got %d", stats1.TransactionsIngested)
	}

	// Step 2: Provider attempts silent alteration of posted transaction (changed amount, currency, description, date)
	modifiedSettled := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	currentTxs = []connector.CanonicalBankTransaction{
		{
			ExternalID:        "tx_term_100",
			AccountExternalID: "acc_corr_1",
			Amount:            connector.CurrencyAmount{MinorUnits: 250000, Currency: "USD"},
			AccountAmount:     connector.CurrencyAmount{MinorUnits: 250000, Currency: "USD"},
			TransactionTime:   originalTime,
			SettledTime:       &modifiedSettled,
			BookingStatus:     "posted",
			OperationType:     "purchase",
			RawDescription:    "Altered Merchant Charge",
		},
	}
	currentRaw = []byte(`{"tx": "tx_term_100", "step": 2}`)

	stats2, err := svc.SyncConnection(ctx, familyID, connID, SyncOptions{})
	if err != nil {
		t.Fatalf("resync failed: %v", err)
	}

	// Invariants:
	// 1. Transaction was skipped from mutation
	if stats2.TransactionsSkipped != 1 || stats2.TransactionsUpdated != 0 {
		t.Errorf("expected 1 skipped and 0 updated, got skipped=%d updated=%d", stats2.TransactionsSkipped, stats2.TransactionsUpdated)
	}

	// 2. Discrepancies detected and logged
	if stats2.DiscrepanciesDetected < 3 {
		t.Errorf("expected at least 3 discrepancies detected (amount, currency, description), got %d: %+v",
			stats2.DiscrepanciesDetected, stats2.Discrepancies)
	}

	// 3. Database immutable facts remain unchanged
	_ = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		var amt int64
		var cur, desc string
		var settled time.Time
		err := tx.QueryRow(ctx, `
			SELECT account_amount_minor, account_currency, raw_description, posted_time
			FROM public.bank_transactions
			WHERE family_id = $1 AND external_id = 'tx_term_100'
		`, familyID).Scan(&amt, &cur, &desc, &settled)
		if err != nil {
			t.Fatalf("query failed: %v", err)
		}

		if amt != -100000 {
			t.Errorf("posted amount was mutated! expected -100000, got %d", amt)
		}
		if cur != "RUB" {
			t.Errorf("posted currency was mutated! expected RUB, got %s", cur)
		}
		if desc != "Original Merchant Charge" {
			t.Errorf("posted description was mutated! expected Original Merchant Charge, got %s", desc)
		}
		if !settled.Equal(originalSettled) {
			t.Errorf("posted settlement date was mutated! expected %v, got %v", originalSettled, settled)
		}
		return nil
	})
}

// -----------------------------------------------------------------------------
// 4. Durable Pagination, Failure & Resume (> 50 pages)
// -----------------------------------------------------------------------------

func TestRemediation_DurablePagination_50Pages_FailureAndResume(t *testing.T) {
	pool := getWorkerPool(t)
	defer pool.Close()
	ctx := context.Background()

	familyID, _, connID := setupTenantFixture(t, "mock_paginated")

	// Create a paginated mock connector with 60 pages
	const totalPages = 60
	paginatedConn := &paginatedMockConnector{
		totalPages: totalPages,
		failOnPage: 25, // Simulate network crash on page 25
	}
	registry := connector.NewRegistry()
	registry.Register(paginatedConn)

	svc := NewService(pool, registry)

	// Step 1: Run sync. It should ingest pages 1 to 24, fail on page 25
	stats1, err := svc.SyncConnection(ctx, familyID, connID, SyncOptions{MaxPages: 100})
	if err == nil {
		t.Fatalf("expected sync to fail on page 25, but it succeeded")
	}
	_ = stats1

	// Invariant: Progress up to page 24 must be committed and durable in the database!
	_ = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		var txCount int
		_ = tx.QueryRow(ctx, "SELECT count(*) FROM public.bank_transactions WHERE family_id = $1", familyID).Scan(&txCount)
		if txCount != 24 {
			t.Errorf("expected 24 committed transactions after mid-pagination failure, got %d", txCount)
		}

		var cursor *string
		var lastSyncedAt *time.Time
		_ = tx.QueryRow(ctx, "SELECT sync_cursor, last_synced_at FROM public.bank_connections WHERE family_id = $1 AND id = $2", familyID, connID).Scan(&cursor, &lastSyncedAt)
		if cursor == nil || *cursor != "cursor_24" {
			t.Errorf("expected durable sync_cursor 'cursor_24', got %v", cursor)
		}
		if lastSyncedAt != nil {
			t.Errorf("last_synced_at must not be set on incomplete sync, got %v", lastSyncedAt)
		}
		return nil
	})

	// Step 2: Clear failure condition and resume sync
	paginatedConn.failOnPage = 0 // No more failures

	stats2, err := svc.SyncConnection(ctx, familyID, connID, SyncOptions{MaxPages: 100})
	if err != nil {
		t.Fatalf("resumed sync failed: %v", err)
	}
	if stats2.TransactionsIngested != 36 { // 60 - 24 = 36 remaining
		t.Errorf("expected 36 remaining transactions ingested upon resume, got %d", stats2.TransactionsIngested)
	}

	// Verify all 60 pages committed, cursor cleared, and last_synced_at set
	_ = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		var totalTx int
		_ = tx.QueryRow(ctx, "SELECT count(*) FROM public.bank_transactions WHERE family_id = $1", familyID).Scan(&totalTx)
		if totalTx != 60 {
			t.Errorf("expected 60 total transactions after full resume, got %d", totalTx)
		}

		var cursor *string
		var lastSyncedAt *time.Time
		var status string
		_ = tx.QueryRow(ctx, "SELECT sync_cursor, last_synced_at, status FROM public.bank_connections WHERE family_id = $1 AND id = $2", familyID, connID).Scan(&cursor, &lastSyncedAt, &status)
		if cursor != nil {
			t.Errorf("expected sync_cursor to be cleared to NULL after full sync, got %s", *cursor)
		}
		if lastSyncedAt == nil {
			t.Errorf("expected last_synced_at to be populated after complete sync")
		}
		if status != "active" {
			t.Errorf("expected status 'active', got %s", status)
		}
		return nil
	})

	// Step 3: Repeated sync is idempotent
	stats3, err := svc.SyncConnection(ctx, familyID, connID, SyncOptions{MaxPages: 100})
	if err != nil {
		t.Fatalf("repeated sync failed: %v", err)
	}
	if stats3.TransactionsIngested != 0 {
		t.Errorf("expected 0 new transactions on resync, got %d", stats3.TransactionsIngested)
	}
}

// -----------------------------------------------------------------------------
// 5. Cursor Loop Detection
// -----------------------------------------------------------------------------

func TestRemediation_CursorLoopDetection(t *testing.T) {
	pool := getWorkerPool(t)
	defer pool.Close()
	ctx := context.Background()

	familyID, _, connID := setupTenantFixture(t, "mock_loop")

	loopConn := &loopMockConnector{}
	registry := connector.NewRegistry()
	registry.Register(loopConn)

	svc := NewService(pool, registry)

	_, err := svc.SyncConnection(ctx, familyID, connID, SyncOptions{MaxPages: 10})
	if err == nil {
		t.Fatalf("expected cursor loop error, but sync succeeded")
	}
	if !errors.Is(err, ErrCursorLoop) {
		t.Fatalf("expected ErrCursorLoop, got: %v", err)
	}
}

// -----------------------------------------------------------------------------
// Test Connector Implementations
// -----------------------------------------------------------------------------

type paginatedMockConnector struct {
	totalPages int
	failOnPage int
}

func (p *paginatedMockConnector) ProviderCode() string {
	return "mock_paginated"
}

func (p *paginatedMockConnector) HealthCheck(ctx context.Context, encryptedCredentials []byte) error {
	return nil
}

func (p *paginatedMockConnector) FetchCards(ctx context.Context, encryptedCredentials []byte, accountExternalID string) ([]connector.CardDTO, error) {
	return nil, nil
}

func (p *paginatedMockConnector) FetchAccounts(ctx context.Context, encryptedCredentials []byte) ([]connector.AccountDTO, error) {
	return []connector.AccountDTO{
		{
			ExternalID: "acc_paginated_1",
			Name:       "Paginated Account",
			Type:       "checking",
			Currency:   "RUB",
		},
	}, nil
}

func (p *paginatedMockConnector) FetchBalances(ctx context.Context, encryptedCredentials []byte, accountExternalIDs []string) ([]connector.BalanceDTO, error) {
	return nil, nil
}

func (p *paginatedMockConnector) FetchTransactions(ctx context.Context, encryptedCredentials []byte, params connector.SyncParams) (*connector.SyncResult, error) {
	currentPage := 1
	if params.Cursor != "" {
		var curNum int
		_, _ = fmt.Sscanf(params.Cursor, "cursor_%d", &curNum)
		currentPage = curNum + 1
	}

	if p.failOnPage > 0 && currentPage == p.failOnPage {
		return nil, errors.New("simulated network outage on page 25")
	}

	hasMore := currentPage < p.totalPages
	nextCursor := ""
	if hasMore {
		nextCursor = fmt.Sprintf("cursor_%d", currentPage)
	}

	tx := connector.CanonicalBankTransaction{
		ExternalID:        fmt.Sprintf("tx_page_%d", currentPage),
		AccountExternalID: "acc_paginated_1",
		Amount:            connector.CurrencyAmount{MinorUnits: int64(currentPage * 1000), Currency: "RUB"},
		AccountAmount:     connector.CurrencyAmount{MinorUnits: int64(currentPage * 1000), Currency: "RUB"},
		TransactionTime:   time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(currentPage) * time.Hour),
		BookingStatus:     "posted",
		OperationType:     "purchase",
		RawDescription:    fmt.Sprintf("Page %d Purchase", currentPage),
	}

	rawPayload := []byte(fmt.Sprintf(`{"page": %d, "has_more": %v}`, currentPage, hasMore))

	return &connector.SyncResult{
		Transactions: []connector.CanonicalBankTransaction{tx},
		RawPayload:   rawPayload,
		Progress: connector.SyncProgress{
			HasMore:    hasMore,
			NextCursor: nextCursor,
		},
	}, nil
}

type loopMockConnector struct{}

func (l *loopMockConnector) ProviderCode() string { return "mock_loop" }

func (l *loopMockConnector) HealthCheck(ctx context.Context, encryptedCredentials []byte) error {
	return nil
}

func (l *loopMockConnector) FetchCards(ctx context.Context, encryptedCredentials []byte, accountExternalID string) ([]connector.CardDTO, error) {
	return nil, nil
}

func (l *loopMockConnector) FetchAccounts(ctx context.Context, encryptedCredentials []byte) ([]connector.AccountDTO, error) {
	return []connector.AccountDTO{{ExternalID: "acc_loop_1", Name: "Loop Account", Type: "checking", Currency: "RUB"}}, nil
}

func (l *loopMockConnector) FetchBalances(ctx context.Context, encryptedCredentials []byte, accountExternalIDs []string) ([]connector.BalanceDTO, error) {
	return nil, nil
}

func (l *loopMockConnector) FetchTransactions(ctx context.Context, encryptedCredentials []byte, params connector.SyncParams) (*connector.SyncResult, error) {
	// Returns alternating cursors: cursor_A -> cursor_B -> cursor_A (loop!)
	next := "cursor_B"
	if params.Cursor == "cursor_B" {
		next = "cursor_A"
	}
	return &connector.SyncResult{
		Transactions: []connector.CanonicalBankTransaction{
			{
				ExternalID:        fmt.Sprintf("tx_%s", next),
				AccountExternalID: "acc_loop_1",
				Amount:            connector.CurrencyAmount{MinorUnits: 100, Currency: "RUB"},
				AccountAmount:     connector.CurrencyAmount{MinorUnits: 100, Currency: "RUB"},
				TransactionTime:   time.Now().UTC(),
				BookingStatus:     "posted",
				OperationType:     "purchase",
				RawDescription:    "Loop Tx",
			},
		},
		RawPayload: []byte(`{"loop": true}`),
		Progress: connector.SyncProgress{
			HasMore:    true,
			NextCursor: next,
		},
	}, nil
}

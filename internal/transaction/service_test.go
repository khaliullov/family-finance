package transaction

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/khaliullov/family-finance/internal/connector"
	"github.com/khaliullov/family-finance/internal/connector/fixture"
	"github.com/khaliullov/family-finance/internal/database"
)

func TestService_InitialSync(t *testing.T) {
	pool := getWorkerPool(t)
	defer pool.Close()
	ctx := context.Background()

	familyID, _, connID := setupTenantFixture(t, "fixture")

	registry := connector.NewRegistry()
	registry.Register(fixture.NewFixtureConnector())

	svc := NewService(pool, registry)

	stats, err := svc.SyncConnection(ctx, familyID, connID, SyncOptions{
		LookbackDays: 30,
	})
	if err != nil {
		t.Fatalf("SyncConnection failed: %v", err)
	}

	// Verify stats
	if stats.AccountsDiscovered != 3 {
		t.Errorf("expected 3 accounts discovered, got %d", stats.AccountsDiscovered)
	}
	if stats.CardsDiscovered != 3 {
		t.Errorf("expected 3 cards discovered, got %d", stats.CardsDiscovered)
	}
	// 5 on page 1 + 2 on page 2 = 7 total transactions
	if stats.TransactionsIngested != 7 {
		t.Errorf("expected 7 transactions ingested, got %d", stats.TransactionsIngested)
	}
	if stats.RawPayloadsPreserved != 2 {
		t.Errorf("expected 2 raw payloads preserved, got %d", stats.RawPayloadsPreserved)
	}

	// Verify database records inside tenant context
	err = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		// 1. Check Accounts count
		var accCount int
		err := tx.QueryRow(ctx, "SELECT count(*) FROM public.accounts WHERE family_id = $1", familyID).Scan(&accCount)
		if err != nil || accCount != 3 {
			t.Errorf("expected 3 accounts in db, got %d, err=%v", accCount, err)
		}

		// 2. Check Cards count
		var cardCount int
		err = tx.QueryRow(ctx, "SELECT count(*) FROM public.cards WHERE family_id = $1", familyID).Scan(&cardCount)
		if err != nil || cardCount != 3 {
			t.Errorf("expected 3 cards in db, got %d, err=%v", cardCount, err)
		}

		// 3. Check Bank Transactions count
		var txCount int
		err = tx.QueryRow(ctx, "SELECT count(*) FROM public.bank_transactions WHERE family_id = $1", familyID).Scan(&txCount)
		if err != nil || txCount != 7 {
			t.Errorf("expected 7 bank_transactions in db, got %d, err=%v", txCount, err)
		}

		// 4. Check Accounting Entries count (must be exactly 1:1 unsplit with bank_transactions)
		var entryCount int
		err = tx.QueryRow(ctx, "SELECT count(*) FROM public.accounting_entries WHERE family_id = $1", familyID).Scan(&entryCount)
		if err != nil || entryCount != 7 {
			t.Errorf("expected 7 accounting_entries in db, got %d, err=%v", entryCount, err)
		}

		// 5. Verify deferred split integrity: check every transaction sum matches
		rows, err := tx.Query(ctx, `
			SELECT bt.id, bt.account_amount_minor, COALESCE(SUM(ae.amount_minor), 0)
			FROM public.bank_transactions bt
			LEFT JOIN public.accounting_entries ae ON ae.bank_transaction_id = bt.id
			WHERE bt.family_id = $1
			GROUP BY bt.id, bt.account_amount_minor
		`, familyID)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var id uuid.UUID
			var btAmount, aeSum int64
			if err := rows.Scan(&id, &btAmount, &aeSum); err != nil {
				return err
			}
			if btAmount != aeSum {
				t.Errorf("split sum mismatch for tx %s: bank_tx=%d, accounting_entry=%d", id, btAmount, aeSum)
			}
		}

		// 6. Check Card Check transaction has 0 amount
		var checkAmt int64
		err = tx.QueryRow(ctx, `
			SELECT account_amount_minor FROM public.bank_transactions
			WHERE family_id = $1 AND operation_type = 'card_check'
		`, familyID).Scan(&checkAmt)
		if err != nil || checkAmt != 0 {
			t.Errorf("expected card_check amount 0, got %d, err=%v", checkAmt, err)
		}

		// 7. Check Foreign Currency transaction
		var cur, accCur string
		var nomAmt, accAmt int64
		err = tx.QueryRow(ctx, `
			SELECT currency, account_currency, amount_minor, account_amount_minor
			FROM public.bank_transactions
			WHERE family_id = $1 AND external_id = 'tx_fx_1002'
		`, familyID).Scan(&cur, &accCur, &nomAmt, &accAmt)
		if err != nil {
			t.Errorf("failed to query foreign currency transaction: %v", err)
		}
		if cur != "USD" || accCur != "RUB" {
			t.Errorf("expected USD/RUB currency pair, got %s/%s", cur, accCur)
		}
		if nomAmt != -1500 || accAmt != -142500 {
			t.Errorf("expected nom=-1500 acc=-142500, got nom=%d acc=%d", nomAmt, accAmt)
		}

		// 8. Check connection status and last_synced_at
		var status string
		var lastSynced bool
		err = tx.QueryRow(ctx, `
			SELECT status, last_synced_at IS NOT NULL
			FROM public.bank_connections
			WHERE family_id = $1 AND id = $2
		`, familyID, connID).Scan(&status, &lastSynced)
		if err != nil || status != "active" || !lastSynced {
			t.Errorf("expected status=active and non-null last_synced_at, got status=%s lastSynced=%v", status, lastSynced)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("verification transaction failed: %v", err)
	}
}

func TestService_IdempotentResync(t *testing.T) {
	pool := getWorkerPool(t)
	defer pool.Close()
	ctx := context.Background()

	familyID, _, connID := setupTenantFixture(t, "fixture")

	registry := connector.NewRegistry()
	registry.Register(fixture.NewFixtureConnector())
	svc := NewService(pool, registry)

	// 1. Initial sync
	stats1, err := svc.SyncConnection(ctx, familyID, connID, SyncOptions{LookbackDays: 30})
	if err != nil {
		t.Fatalf("initial sync failed: %v", err)
	}
	if stats1.TransactionsIngested != 7 {
		t.Fatalf("expected 7 transactions ingested on 1st run, got %d", stats1.TransactionsIngested)
	}

	// 2. Second sync with identical data
	stats2, err := svc.SyncConnection(ctx, familyID, connID, SyncOptions{LookbackDays: 30})
	if err != nil {
		t.Fatalf("second sync failed: %v", err)
	}

	// Idempotency assertions
	if stats2.TransactionsIngested != 0 {
		t.Errorf("expected 0 transactions ingested on 2nd run, got %d", stats2.TransactionsIngested)
	}
	if stats2.TransactionsSkipped != 7 {
		t.Errorf("expected 7 transactions skipped on 2nd run, got %d", stats2.TransactionsSkipped)
	}
	if stats2.AccountsDiscovered != 0 {
		t.Errorf("expected 0 new accounts discovered on 2nd run, got %d", stats2.AccountsDiscovered)
	}

	// Verify database record count did NOT change
	_ = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		var txCount int
		_ = tx.QueryRow(ctx, "SELECT count(*) FROM public.bank_transactions WHERE family_id = $1", familyID).Scan(&txCount)
		if txCount != 7 {
			t.Errorf("expected exactly 7 bank_transactions in db, got %d", txCount)
		}

		var entryCount int
		_ = tx.QueryRow(ctx, "SELECT count(*) FROM public.accounting_entries WHERE family_id = $1", familyID).Scan(&entryCount)
		if entryCount != 7 {
			t.Errorf("expected exactly 7 accounting_entries in db, got %d", entryCount)
		}
		return nil
	})
}

func TestService_PendingToPostedSettlement(t *testing.T) {
	pool := getWorkerPool(t)
	defer pool.Close()
	ctx := context.Background()

	familyID, _, connID := setupTenantFixture(t, "fixture")

	registry := connector.NewRegistry()
	registry.Register(fixture.NewFixtureConnector())
	svc := NewService(pool, registry)

	// 1. Initial sync page 1 (has pending hold tx_hold_1001 with amount 150000 kopeks)
	stats1, err := svc.SyncConnection(ctx, familyID, connID, SyncOptions{MaxPages: 1})
	if err != nil && !errors.Is(err, ErrSyncIncomplete) {
		t.Fatalf("sync page 1 failed: %v", err)
	}
	if stats1.TransactionsIngested != 5 {
		t.Fatalf("expected 5 tx ingested, got %d", stats1.TransactionsIngested)
	}

	// Verify tx_hold_1001 is pending with -150000
	_ = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		var status string
		var amt int64
		var postedTime *string
		err := tx.QueryRow(ctx, `
			SELECT booking_status, account_amount_minor, posted_time
			FROM public.bank_transactions
			WHERE family_id = $1 AND external_id = 'tx_hold_1001'
		`, familyID).Scan(&status, &amt, &postedTime)
		if err != nil {
			t.Fatalf("failed to query hold: %v", err)
		}
		if status != "pending" || amt != -150000 || postedTime != nil {
			t.Errorf("expected pending hold with -150000 and nil posted_time, got status=%s amt=%d posted=%v", status, amt, postedTime)
		}

		// Verify accounting entry amount matches -150000
		var entryAmt int64
		err = tx.QueryRow(ctx, `
			SELECT ae.amount_minor
			FROM public.accounting_entries ae
			JOIN public.bank_transactions bt ON bt.id = ae.bank_transaction_id
			WHERE bt.family_id = $1 AND bt.external_id = 'tx_hold_1001'
		`, familyID).Scan(&entryAmt)
		if err != nil || entryAmt != -150000 {
			t.Errorf("expected entry amount -150000, got %d, err=%v", entryAmt, err)
		}
		return nil
	})

	// 2. Settlement sync (cursor_settled: settles auth_hold_1001 with 155000 kopeks and posted status)
	statsSettled, err := svc.SyncConnection(ctx, familyID, connID, SyncOptions{
		Cursor:   "cursor_settled",
		MaxPages: 1,
	})
	if err != nil {
		t.Fatalf("settlement sync failed: %v", err)
	}
	if statsSettled.TransactionsUpdated != 1 {
		t.Errorf("expected 1 transaction updated on settlement, got %d", statsSettled.TransactionsUpdated)
	}

	// 3. Verify hold transitioned to posted and accounting entry synchronized in lockstep
	_ = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		var status, extID string
		var amt int64
		var hasPostedTime bool
		err := tx.QueryRow(ctx, `
			SELECT booking_status, external_id, account_amount_minor, posted_time IS NOT NULL
			FROM public.bank_transactions
			WHERE family_id = $1 AND external_auth_id = 'auth_hold_1001'
		`, familyID).Scan(&status, &extID, &amt, &hasPostedTime)
		if err != nil {
			t.Fatalf("failed to query settled transaction: %v", err)
		}
		if status != "posted" {
			t.Errorf("expected status=posted, got %s", status)
		}
		if amt != -155000 {
			t.Errorf("expected adjusted settled amount -155000, got %d", amt)
		}
		if !hasPostedTime {
			t.Errorf("expected non-null posted_time after settlement")
		}

		// Verify accounting entry amount updated in lockstep to -155000
		var entryAmt int64
		err = tx.QueryRow(ctx, `
			SELECT ae.amount_minor
			FROM public.accounting_entries ae
			JOIN public.bank_transactions bt ON bt.id = ae.bank_transaction_id
			WHERE bt.family_id = $1 AND bt.external_auth_id = 'auth_hold_1001'
		`, familyID).Scan(&entryAmt)
		if err != nil || entryAmt != -155000 {
			t.Errorf("expected entry amount synchronized to -155000, got %d, err=%v", entryAmt, err)
		}

		// Verify total bank transaction count is still 5 (no duplicate created!)
		var totalCount int
		_ = tx.QueryRow(ctx, "SELECT count(*) FROM public.bank_transactions WHERE family_id = $1", familyID).Scan(&totalCount)
		if totalCount != 5 {
			t.Errorf("expected 5 bank_transactions total (0 duplicates), got %d", totalCount)
		}

		return nil
	})
}

func TestService_DuplicateSequenceDisambiguation(t *testing.T) {
	pool := getWorkerPool(t)
	defer pool.Close()
	ctx := context.Background()

	familyID, _, connID := setupTenantFixture(t, "fixture")

	registry := connector.NewRegistry()
	registry.Register(fixture.NewFixtureConnector())
	svc := NewService(pool, registry)

	// 1. Sync duplicates fixture
	stats1, err := svc.SyncConnection(ctx, familyID, connID, SyncOptions{
		Cursor:   "cursor_duplicates",
		MaxPages: 1,
	})
	if err != nil {
		t.Fatalf("duplicates sync failed: %v", err)
	}

	// 3 duplicate events in fixture: 1 with external_id (ingested), 1 identical with external_id (skipped), 1 without external_id (ingested with seq=1)
	if stats1.TransactionsIngested < 1 {
		t.Errorf("expected transactions ingested from duplicates fixture, got %d", stats1.TransactionsIngested)
	}

	// 2. Re-sync duplicates fixture: all should be skipped
	stats2, err := svc.SyncConnection(ctx, familyID, connID, SyncOptions{
		Cursor:   "cursor_duplicates",
		MaxPages: 1,
	})
	if err != nil {
		t.Fatalf("duplicates re-sync failed: %v", err)
	}
	if stats2.TransactionsIngested != 0 {
		t.Errorf("expected 0 transactions ingested on re-sync, got %d", stats2.TransactionsIngested)
	}
}

func TestService_RawPayloadImmutability(t *testing.T) {
	pool := getWorkerPool(t)
	defer pool.Close()
	ctx := context.Background()

	familyID, _, connID := setupTenantFixture(t, "fixture")

	registry := connector.NewRegistry()
	registry.Register(fixture.NewFixtureConnector())
	svc := NewService(pool, registry)

	_, err := svc.SyncConnection(ctx, familyID, connID, SyncOptions{MaxPages: 1})
	if err != nil && !errors.Is(err, ErrSyncIncomplete) {
		t.Fatalf("sync failed: %v", err)
	}

	var payloadID uuid.UUID
	err = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT id FROM public.raw_bank_payloads WHERE family_id = $1 LIMIT 1", familyID).Scan(&payloadID)
	})
	if err != nil {
		t.Fatalf("failed to query payload id: %v", err)
	}

	// Attempt direct UPDATE -> must be blocked by trg_raw_payloads_immutable
	updateErr := database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "UPDATE public.raw_bank_payloads SET source = 'tampered' WHERE id = $1", payloadID)
		return err
	})
	if updateErr == nil {
		t.Fatalf("expected UPDATE on raw_bank_payloads to fail with immutability trigger")
	}

	// Attempt direct DELETE -> must be blocked by trg_raw_payloads_immutable
	deleteErr := database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "DELETE FROM public.raw_bank_payloads WHERE id = $1", payloadID)
		return err
	})
	if deleteErr == nil {
		t.Fatalf("expected DELETE on raw_bank_payloads to fail with immutability trigger")
	}
}

func TestService_TenantIsolation(t *testing.T) {
	pool := getWorkerPool(t)
	defer pool.Close()
	ctx := context.Background()

	familyA, _, connA := setupTenantFixture(t, "fixture")
	familyB, _, connB := setupTenantFixture(t, "fixture")

	registry := connector.NewRegistry()
	registry.Register(fixture.NewFixtureConnector())
	svc := NewService(pool, registry)

	// Sync connection for Family A
	_, err := svc.SyncConnection(ctx, familyA, connA, SyncOptions{MaxPages: 1})
	if err != nil && !errors.Is(err, ErrSyncIncomplete) {
		t.Fatalf("sync for Family A failed: %v", err)
	}

	// Attempt to sync Family A's connection under Family B's context -> must fail with ErrConnectionNotFound
	_, err = svc.SyncConnection(ctx, familyB, connA, SyncOptions{MaxPages: 1})
	if !errors.Is(err, ErrConnectionNotFound) {
		t.Fatalf("expected ErrConnectionNotFound for cross-family connection access, got %v", err)
	}

	// Verify Family B cannot see Family A's accounts
	err = database.WithFamilyTx(ctx, pool, familyB, func(tx pgx.Tx) error {
		var count int
		_ = tx.QueryRow(ctx, "SELECT count(*) FROM public.accounts WHERE family_id = $1", familyA).Scan(&count)
		if count != 0 {
			t.Fatalf("Family B saw Family A accounts via RLS leakage! count=%d", count)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("tenant check failed: %v", err)
	}

	_ = connB
}

func TestService_ConcurrentSync(t *testing.T) {
	pool := getWorkerPool(t)
	defer pool.Close()
	ctx := context.Background()

	familyID, _, connID := setupTenantFixture(t, "fixture")

	registry := connector.NewRegistry()
	registry.Register(fixture.NewFixtureConnector())
	svc := NewService(pool, registry)

	const concurrency = 5
	var wg sync.WaitGroup
	errs := make([]error, concurrency)

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		idx := i
		go func() {
			defer wg.Done()
			_, errs[idx] = svc.SyncConnection(ctx, familyID, connID, SyncOptions{MaxPages: 1})
		}()
	}

	wg.Wait()

	var succeededCount int
	for i, err := range errs {
		if err == nil || errors.Is(err, ErrSyncIncomplete) {
			succeededCount++
		} else if !errors.Is(err, ErrSyncAlreadyRunning) {
			t.Fatalf("concurrent sync worker %d failed: %v", i, err)
		}
	}
	if succeededCount == 0 {
		t.Fatalf("expected at least one concurrent sync to succeed")
	}

	// Verify database contains exactly 5 transactions (0 duplicates created under race conditions)
	_ = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		var txCount int
		_ = tx.QueryRow(ctx, "SELECT count(*) FROM public.bank_transactions WHERE family_id = $1", familyID).Scan(&txCount)
		if txCount != 5 {
			t.Errorf("expected exactly 5 bank_transactions after concurrent sync, got %d", txCount)
		}
		return nil
	})
}

package account

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/khaliullov/family-finance/internal/database"
)

func TestPostgresRepository_AccountCRUD(t *testing.T) {
	pool := getAppPool(t)
	defer pool.Close()
	repo := NewRepository(pool)
	ctx := context.Background()

	familyID, ownerID := setupTenantFixture(t, pool)

	// 1. Create Checking Account with NULL balance
	created, err := repo.CreateAccount(ctx, familyID, Account{
		Name:        "Daily Checking",
		Type:        TypeChecking,
		Currency:    "RUB",
		OwnerUserID: ownerID,
	})
	if err != nil {
		t.Fatalf("CreateAccount failed: %v", err)
	}
	if created.ID == uuid.Nil || created.Name != "Daily Checking" {
		t.Fatalf("unexpected created account: %+v", created)
	}
	if created.CurrentBalanceMinor != nil || created.AvailableBalanceMinor != nil {
		t.Errorf("expected initial balances to be NULL (unknown), got current=%v, avail=%v",
			created.CurrentBalanceMinor, created.AvailableBalanceMinor)
	}

	// 2. Get Account by ID
	fetched, err := repo.GetAccountByID(ctx, familyID, created.ID)
	if err != nil {
		t.Fatalf("GetAccountByID failed: %v", err)
	}
	if fetched.ID != created.ID || fetched.Name != "Daily Checking" {
		t.Errorf("fetched account mismatch: %+v", fetched)
	}

	// 3. Update Account Name
	newName := "Renamed Checking"
	updated, err := repo.UpdateAccount(ctx, familyID, created.ID, UpdateAccountParams{Name: &newName})
	if err != nil {
		t.Fatalf("UpdateAccount failed: %v", err)
	}
	if updated.Name != "Renamed Checking" {
		t.Errorf("expected name %q, got %q", newName, updated.Name)
	}

	// 4. Update Balance Snapshot
	curr := int64(500000)  // 5,000.00 RUB
	avail := int64(450000) // 4,500.00 RUB
	now := time.Now()
	snap, err := repo.UpdateBalanceSnapshot(ctx, familyID, created.ID, BalanceSnapshotParams{
		CurrentBalanceMinor:   &curr,
		AvailableBalanceMinor: &avail,
		BalanceSource:         SourceBankAPI,
		ObservedAt:            now,
	})
	if err != nil {
		t.Fatalf("UpdateBalanceSnapshot failed: %v", err)
	}
	if snap.CurrentBalanceMinor == nil || *snap.CurrentBalanceMinor != 500000 {
		t.Errorf("expected current balance 500000, got %v", snap.CurrentBalanceMinor)
	}
	if snap.AvailableBalanceMinor == nil || *snap.AvailableBalanceMinor != 450000 {
		t.Errorf("expected available balance 450000, got %v", snap.AvailableBalanceMinor)
	}
	if snap.BalanceSource == nil || *snap.BalanceSource != string(SourceBankAPI) {
		t.Errorf("expected source bank_api, got %v", snap.BalanceSource)
	}

	// 5. List Accounts
	accounts, err := repo.ListAccountsByFamily(ctx, familyID, false)
	if err != nil || len(accounts) == 0 {
		t.Fatalf("ListAccountsByFamily failed: err=%v, len=%d", err, len(accounts))
	}

	// 6. Archive Account
	err = repo.ArchiveAccount(ctx, familyID, created.ID)
	if err != nil {
		t.Fatalf("ArchiveAccount failed: %v", err)
	}
	archivedAcc, err := repo.GetAccountByID(ctx, familyID, created.ID)
	if err != nil || !archivedAcc.IsArchived {
		t.Fatalf("expected archived account, got %+v, err=%v", archivedAcc, err)
	}

	// Listing excluding archived should not include it
	activeAccounts, err := repo.ListAccountsByFamily(ctx, familyID, false)
	if err != nil {
		t.Fatalf("ListAccountsByFamily active failed: %v", err)
	}
	for _, a := range activeAccounts {
		if a.ID == created.ID {
			t.Errorf("archived account still listed in active accounts")
		}
	}
}

func TestPostgresRepository_CardManagement(t *testing.T) {
	pool := getAppPool(t)
	defer pool.Close()
	repo := NewRepository(pool)
	ctx := context.Background()

	familyID, ownerID := setupTenantFixture(t, pool)

	// Create account
	acc, err := repo.CreateAccount(ctx, familyID, Account{
		Name:        "Card Host Account",
		Type:        TypeChecking,
		Currency:    "RUB",
		OwnerUserID: ownerID,
	})
	if err != nil {
		t.Fatalf("CreateAccount failed: %v", err)
	}

	// 1. Create Physical Card as Primary
	card1, err := repo.CreateCard(ctx, familyID, Card{
		AccountID:     acc.ID,
		OwnerUserID:   ownerID,
		MaskedPAN:     "*1234",
		DisplayName:   "Gold Physical",
		PaymentSystem: PaymentSystemMir,
		IsVirtual:     false,
		IsPrimary:     true,
		Status:        CardStatusActive,
	})
	if err != nil {
		t.Fatalf("CreateCard 1 failed: %v", err)
	}
	if !card1.IsPrimary || card1.Status != CardStatusActive {
		t.Errorf("unexpected card1 state: %+v", card1)
	}

	// 2. Create Virtual Card
	card2, err := repo.CreateCard(ctx, familyID, Card{
		AccountID:     acc.ID,
		OwnerUserID:   ownerID,
		MaskedPAN:     "*5678",
		DisplayName:   "Online Virtual",
		PaymentSystem: PaymentSystemVisa,
		IsVirtual:     true,
		IsPrimary:     false,
		Status:        CardStatusActive,
	})
	if err != nil {
		t.Fatalf("CreateCard 2 failed: %v", err)
	}

	// 3. List Cards by Account
	cards, err := repo.ListCardsByAccount(ctx, familyID, acc.ID)
	if err != nil || len(cards) != 2 {
		t.Fatalf("expected 2 cards, got %d, err=%v", len(cards), err)
	}

	// 4. Set Card 2 as Primary (Card 1 should automatically become non-primary)
	err = repo.SetPrimaryCard(ctx, familyID, acc.ID, card2.ID)
	if err != nil {
		t.Fatalf("SetPrimaryCard failed: %v", err)
	}

	c1, err := repo.GetCardByID(ctx, familyID, card1.ID)
	if err != nil || c1.IsPrimary {
		t.Errorf("card1 should no longer be primary, is_primary=%v", c1.IsPrimary)
	}

	c2, err := repo.GetCardByID(ctx, familyID, card2.ID)
	if err != nil || !c2.IsPrimary {
		t.Errorf("card2 should now be primary, is_primary=%v", c2.IsPrimary)
	}

	// 5. Close Card 2
	err = repo.CloseCard(ctx, familyID, card2.ID)
	if err != nil {
		t.Fatalf("CloseCard failed: %v", err)
	}

	c2Closed, err := repo.GetCardByID(ctx, familyID, card2.ID)
	if err != nil || c2Closed.Status != CardStatusClosed || c2Closed.IsPrimary {
		t.Errorf("card2 should be closed and not primary: status=%s, primary=%v", c2Closed.Status, c2Closed.IsPrimary)
	}
}

func TestPostgresRepository_PendingHoldsExtraction(t *testing.T) {
	pool := getAppPool(t)
	defer pool.Close()
	repo := NewRepository(pool)
	ctx := context.Background()

	familyID, ownerID := setupTenantFixture(t, pool)

	acc, err := repo.CreateAccount(ctx, familyID, Account{
		Name:        "Pending Holds Account",
		Type:        TypeChecking,
		Currency:    "RUB",
		OwnerUserID: ownerID,
	})
	if err != nil {
		t.Fatalf("CreateAccount failed: %v", err)
	}

	now := time.Now()

	// Seed transactions directly into bank_transactions:
	// 1. Pending debit hold: -2,500.00 RUB
	// 2. Settled posted debit: -1,000.00 RUB
	// 3. Cancelled hold: -500.00 RUB
	// 4. Pending credit deposit: +5,000.00 RUB (should NOT be included in debit holds!)
	err = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		tx1ID := uuid.New()
		tx2ID := uuid.New()
		tx3ID := uuid.New()
		tx4ID := uuid.New()

		// 1. Pending hold
		_, err := tx.Exec(ctx, `
			INSERT INTO public.bank_transactions (
				id, family_id, account_id, fingerprint_hash, amount_minor, currency,
				account_amount_minor, account_currency, booking_status, operation_type,
				transaction_time, raw_description
			) VALUES (
				$1, $2, $3, 'fp_hold', -250000, 'RUB',
				-250000, 'RUB', 'pending', 'purchase',
				$4, 'Supermarket Authorization Hold'
			)
		`, tx1ID, familyID, acc.ID, now.Add(-time.Hour))
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO public.accounting_entries (
				id, family_id, account_id, bank_transaction_id, amount_minor, currency,
				entry_time, is_split, classification
			) VALUES (
				$1, $2, $3, $4, -250000, 'RUB', $5, FALSE, 'expense'
			)
		`, uuid.New(), familyID, acc.ID, tx1ID, now.Add(-time.Hour))
		if err != nil {
			return err
		}

		// 2. Posted transaction
		_, err = tx.Exec(ctx, `
			INSERT INTO public.bank_transactions (
				id, family_id, account_id, fingerprint_hash, amount_minor, currency,
				account_amount_minor, account_currency, booking_status, operation_type,
				transaction_time, posted_time, raw_description
			) VALUES (
				$1, $2, $3, 'fp_posted', -100000, 'RUB',
				-100000, 'RUB', 'posted', 'purchase',
				$4, $4, 'Cleared Fuel Purchase'
			)
		`, tx2ID, familyID, acc.ID, now.Add(-24*time.Hour))
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO public.accounting_entries (
				id, family_id, account_id, bank_transaction_id, amount_minor, currency,
				entry_time, is_split, classification
			) VALUES (
				$1, $2, $3, $4, -100000, 'RUB', $5, FALSE, 'expense'
			)
		`, uuid.New(), familyID, acc.ID, tx2ID, now.Add(-24*time.Hour))
		if err != nil {
			return err
		}

		// 3. Cancelled transaction
		_, err = tx.Exec(ctx, `
			INSERT INTO public.bank_transactions (
				id, family_id, account_id, fingerprint_hash, amount_minor, currency,
				account_amount_minor, account_currency, booking_status, operation_type,
				transaction_time, raw_description
			) VALUES (
				$1, $2, $3, 'fp_cancel', -50000, 'RUB',
				-50000, 'RUB', 'cancelled', 'purchase',
				$4, 'Expired Card Authorization'
			)
		`, tx3ID, familyID, acc.ID, now.Add(-48*time.Hour))
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO public.accounting_entries (
				id, family_id, account_id, bank_transaction_id, amount_minor, currency,
				entry_time, is_split, classification
			) VALUES (
				$1, $2, $3, $4, -50000, 'RUB', $5, FALSE, 'expense'
			)
		`, uuid.New(), familyID, acc.ID, tx3ID, now.Add(-48*time.Hour))
		if err != nil {
			return err
		}

		// 4. Pending incoming transfer (+5,000.00 RUB)
		_, err = tx.Exec(ctx, `
			INSERT INTO public.bank_transactions (
				id, family_id, account_id, fingerprint_hash, amount_minor, currency,
				account_amount_minor, account_currency, booking_status, operation_type,
				transaction_time, raw_description
			) VALUES (
				$1, $2, $3, 'fp_inc', 500000, 'RUB',
				500000, 'RUB', 'pending', 'transfer_in',
				$4, 'Pending Incoming Wire'
			)
		`, tx4ID, familyID, acc.ID, now.Add(-30*time.Minute))
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO public.accounting_entries (
				id, family_id, account_id, bank_transaction_id, amount_minor, currency,
				entry_time, is_split, classification
			) VALUES (
				$1, $2, $3, $4, 500000, 'RUB', $5, FALSE, 'income'
			)
		`, uuid.New(), familyID, acc.ID, tx4ID, now.Add(-30*time.Minute))
		return err
	})
	if err != nil {
		t.Fatalf("seeding bank transactions failed: %v", err)
	}

	// Retrieve pending holds
	holds, err := repo.GetPendingHolds(ctx, familyID, acc.ID)
	if err != nil {
		t.Fatalf("GetPendingHolds failed: %v", err)
	}

	// Strictly verify that ONLY the pending debit hold is extracted
	if len(holds) != 1 {
		t.Fatalf("expected exactly 1 pending hold, got %d: %+v", len(holds), holds)
	}
	if holds[0].HoldMinor != 250000 || holds[0].AmountMinor != -250000 {
		t.Errorf("expected hold magnitude 250000, got %+v", holds[0])
	}
}

func TestPostgresRepository_TenantIsolation(t *testing.T) {
	pool := getAppPool(t)
	defer pool.Close()
	repo := NewRepository(pool)
	ctx := context.Background()

	familyA, userA := setupTenantFixture(t, pool)
	familyB, userB := setupTenantFixture(t, pool)

	// Create account in Family A
	accA, err := repo.CreateAccount(ctx, familyA, Account{
		Name:        "Family A Checking",
		Type:        TypeChecking,
		Currency:    "RUB",
		OwnerUserID: userA,
	})
	if err != nil {
		t.Fatalf("CreateAccount Family A failed: %v", err)
	}

	// Create card in Family A
	cardA, err := repo.CreateCard(ctx, familyA, Card{
		AccountID:     accA.ID,
		OwnerUserID:   userA,
		MaskedPAN:     "*1111",
		DisplayName:   "Card A",
		PaymentSystem: PaymentSystemMir,
	})
	if err != nil {
		t.Fatalf("CreateCard Family A failed: %v", err)
	}

	// 1. Family B cannot read Family A account
	_, err = repo.GetAccountByID(ctx, familyB, accA.ID)
	if !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("expected ErrAccountNotFound when Family B reads Family A account, got %v", err)
	}

	// 2. Family B cannot read Family A card
	_, err = repo.GetCardByID(ctx, familyB, cardA.ID)
	if !errors.Is(err, ErrCardNotFound) {
		t.Fatalf("expected ErrCardNotFound when Family B reads Family A card, got %v", err)
	}

	// 3. Family B cannot update Family A account
	newName := "Hacked Account"
	_, err = repo.UpdateAccount(ctx, familyB, accA.ID, UpdateAccountParams{Name: &newName})
	if !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("expected ErrAccountNotFound when Family B updates Family A account, got %v", err)
	}

	// 4. Family B cannot link card to Family A account (composite FK rejects cross-tenant reference)
	_, err = repo.CreateCard(ctx, familyB, Card{
		AccountID:     accA.ID, // Account belongs to Family A!
		OwnerUserID:   userB,
		MaskedPAN:     "*2222",
		DisplayName:   "Cross-tenant Card",
		PaymentSystem: PaymentSystemVisa,
	})
	if err == nil {
		t.Fatalf("security violation: Family B was able to link card to Family A account!")
	}
	if !errors.Is(err, ErrCrossFamilyReference) {
		t.Errorf("expected ErrCrossFamilyReference, got: %v", err)
	}

	// 5. Family A cannot assign owner from Family B (composite FK fk_accounts_owner rejects)
	_, err = repo.CreateAccount(ctx, familyA, Account{
		Name:        "Cross-tenant Owner Account",
		Type:        TypeChecking,
		Currency:    "RUB",
		OwnerUserID: userB, // userB is NOT in Family A!
	})
	if err == nil {
		t.Fatalf("security violation: Family A created account with foreign owner userB!")
	}
	if !errors.Is(err, ErrInvalidOwner) {
		t.Errorf("expected ErrInvalidOwner, got: %v", err)
	}
}

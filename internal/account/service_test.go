package account

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/khaliullov/family-finance/internal/domain"
)

// mockRepo implements Repository for unit tests
type mockRepo struct {
	accounts map[uuid.UUID]Account
	cards    map[uuid.UUID]Card
	holds    map[uuid.UUID][]PendingHold
	history  map[uuid.UUID]bool
}

func newMockRepo() *mockRepo {
	return &mockRepo{
		accounts: make(map[uuid.UUID]Account),
		cards:    make(map[uuid.UUID]Card),
		holds:    make(map[uuid.UUID][]PendingHold),
		history:  make(map[uuid.UUID]bool),
	}
}

func (m *mockRepo) CreateAccount(ctx context.Context, familyID uuid.UUID, acc Account) (*Account, error) {
	acc.FamilyID = familyID
	m.accounts[acc.ID] = acc
	return &acc, nil
}

func (m *mockRepo) GetAccountByID(ctx context.Context, familyID, accountID uuid.UUID) (*Account, error) {
	acc, ok := m.accounts[accountID]
	if !ok || acc.FamilyID != familyID {
		return nil, ErrAccountNotFound
	}
	return &acc, nil
}

func (m *mockRepo) ListAccountsByFamily(ctx context.Context, familyID uuid.UUID, includeArchived bool) ([]Account, error) {
	var list []Account
	for _, a := range m.accounts {
		if a.FamilyID == familyID && (includeArchived || !a.IsArchived) {
			list = append(list, a)
		}
	}
	return list, nil
}

func (m *mockRepo) UpdateAccount(ctx context.Context, familyID, accountID uuid.UUID, params UpdateAccountParams) (*Account, error) {
	acc, ok := m.accounts[accountID]
	if !ok || acc.FamilyID != familyID || acc.IsArchived {
		return nil, ErrAccountNotFound
	}
	if params.Name != nil {
		acc.Name = *params.Name
	}
	m.accounts[accountID] = acc
	return &acc, nil
}

func (m *mockRepo) ArchiveAccount(ctx context.Context, familyID, accountID uuid.UUID) error {
	acc, ok := m.accounts[accountID]
	if !ok || acc.FamilyID != familyID || acc.IsArchived {
		return ErrAccountNotFound
	}
	acc.IsArchived = true
	m.accounts[accountID] = acc
	return nil
}

func (m *mockRepo) UpdateBalanceSnapshot(ctx context.Context, familyID, accountID uuid.UUID, snapshot BalanceSnapshotParams) (*Account, error) {
	acc, ok := m.accounts[accountID]
	if !ok || acc.FamilyID != familyID || acc.IsArchived {
		return nil, ErrAccountNotFound
	}
	acc.CurrentBalanceMinor = snapshot.CurrentBalanceMinor
	acc.AvailableBalanceMinor = snapshot.AvailableBalanceMinor
	acc.DebtBalanceMinor = snapshot.DebtBalanceMinor
	acc.CreditLimitMinor = snapshot.CreditLimitMinor
	src := string(snapshot.BalanceSource)
	acc.BalanceSource = &src
	acc.BalanceUpdatedAt = &snapshot.ObservedAt
	m.accounts[accountID] = acc
	return &acc, nil
}

func (m *mockRepo) CreateCard(ctx context.Context, familyID uuid.UUID, card Card) (*Card, error) {
	card.FamilyID = familyID
	m.cards[card.ID] = card
	return &card, nil
}

func (m *mockRepo) GetCardByID(ctx context.Context, familyID, cardID uuid.UUID) (*Card, error) {
	c, ok := m.cards[cardID]
	if !ok || c.FamilyID != familyID {
		return nil, ErrCardNotFound
	}
	return &c, nil
}

func (m *mockRepo) ListCardsByAccount(ctx context.Context, familyID, accountID uuid.UUID) ([]Card, error) {
	var list []Card
	for _, c := range m.cards {
		if c.FamilyID == familyID && c.AccountID == accountID {
			list = append(list, c)
		}
	}
	return list, nil
}

func (m *mockRepo) UpdateCard(ctx context.Context, familyID, cardID uuid.UUID, params UpdateCardParams) (*Card, error) {
	c, ok := m.cards[cardID]
	if !ok || c.FamilyID != familyID {
		return nil, ErrCardNotFound
	}
	if params.DisplayName != nil {
		c.DisplayName = *params.DisplayName
	}
	if params.Status != nil {
		c.Status = *params.Status
	}
	if params.IsPrimary != nil {
		c.IsPrimary = *params.IsPrimary
	}
	m.cards[cardID] = c
	return &c, nil
}

func (m *mockRepo) CloseCard(ctx context.Context, familyID, cardID uuid.UUID) error {
	c, ok := m.cards[cardID]
	if !ok || c.FamilyID != familyID {
		return ErrCardNotFound
	}
	c.Status = CardStatusClosed
	c.IsPrimary = false
	m.cards[cardID] = c
	return nil
}

func (m *mockRepo) SetPrimaryCard(ctx context.Context, familyID, accountID, cardID uuid.UUID) error {
	c, ok := m.cards[cardID]
	if !ok || c.FamilyID != familyID || c.AccountID != accountID {
		return ErrCardNotFound
	}
	if c.Status == CardStatusClosed {
		return ErrCardClosed
	}
	for id, other := range m.cards {
		if other.FamilyID == familyID && other.AccountID == accountID {
			other.IsPrimary = false
			m.cards[id] = other
		}
	}
	c.IsPrimary = true
	m.cards[cardID] = c
	return nil
}

func (m *mockRepo) GetPendingHolds(ctx context.Context, familyID, accountID uuid.UUID) ([]PendingHold, error) {
	return m.holds[accountID], nil
}

func (m *mockRepo) HasFinancialHistory(ctx context.Context, familyID, accountID uuid.UUID) (bool, error) {
	return m.history[accountID], nil
}

func TestBalanceCalculation_CheckingAccount(t *testing.T) {
	accountID := uuid.New()
	familyID := uuid.New()

	t.Run("known balance without holds", func(t *testing.T) {
		curr := int64(1000000) // 10,000.00 RUB
		acc := &Account{
			ID:                  accountID,
			FamilyID:            familyID,
			Type:                TypeChecking,
			Currency:            "RUB",
			CurrentBalanceMinor: &curr,
		}

		info, err := CalculateBalance(acc, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if info.CurrentBalanceState != BalanceStateKnown || *info.CurrentBalanceMinor != 1000000 {
			t.Errorf("expected current balance 1000000, got %v", info.CurrentBalanceMinor)
		}
		if info.PendingHoldsMinor != 0 || info.PendingHoldsCount != 0 {
			t.Errorf("expected 0 pending holds, got %d", info.PendingHoldsMinor)
		}
		if info.CalculatedAvailableState != BalanceStateKnown || *info.CalculatedAvailableMinor != 1000000 {
			t.Errorf("expected calculated available 1000000, got %v", info.CalculatedAvailableMinor)
		}
	})

	t.Run("known balance with multiple pending holds", func(t *testing.T) {
		curr := int64(1000000) // 10,000.00 RUB
		acc := &Account{
			ID:                  accountID,
			FamilyID:            familyID,
			Type:                TypeChecking,
			Currency:            "RUB",
			CurrentBalanceMinor: &curr,
		}

		holds := []PendingHold{
			{ID: uuid.New(), AmountMinor: -150000, HoldMinor: 150000, Currency: "RUB"}, // 1,500.00 RUB hold
			{ID: uuid.New(), AmountMinor: -50000, HoldMinor: 50000, Currency: "RUB"},   // 500.00 RUB hold
		}

		info, err := CalculateBalance(acc, holds)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if info.PendingHoldsMinor != 200000 || info.PendingHoldsCount != 2 {
			t.Errorf("expected 200000 holds, got %d", info.PendingHoldsMinor)
		}
		// 10,000 - 2,000 = 8,000 RUB
		if info.CalculatedAvailableState != BalanceStateKnown || *info.CalculatedAvailableMinor != 800000 {
			t.Errorf("expected calculated available 800000, got %v", info.CalculatedAvailableMinor)
		}
	})

	t.Run("unknown current balance preserves unknown state and never converts to zero", func(t *testing.T) {
		acc := &Account{
			ID:                  accountID,
			FamilyID:            familyID,
			Type:                TypeChecking,
			Currency:            "RUB",
			CurrentBalanceMinor: nil, // UNKNOWN
		}

		holds := []PendingHold{
			{ID: uuid.New(), AmountMinor: -50000, HoldMinor: 50000, Currency: "RUB"},
		}

		info, err := CalculateBalance(acc, holds)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if info.CurrentBalanceState != BalanceStateUnknown || info.CurrentBalanceMinor != nil {
			t.Errorf("expected current balance unknown, got %v", info.CurrentBalanceMinor)
		}
		// Critical: Calculated available MUST BE UNKNOWN, not -500.00 or 0!
		if info.CalculatedAvailableState != BalanceStateUnknown || info.CalculatedAvailableMinor != nil {
			t.Errorf("expected calculated available unknown, got %v", info.CalculatedAvailableMinor)
		}
	})

	t.Run("zero balance is known and distinct from unknown", func(t *testing.T) {
		zero := int64(0)
		acc := &Account{
			ID:                  accountID,
			FamilyID:            familyID,
			Type:                TypeChecking,
			Currency:            "RUB",
			CurrentBalanceMinor: &zero,
		}

		info, err := CalculateBalance(acc, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if info.CurrentBalanceState != BalanceStateKnown || *info.CurrentBalanceMinor != 0 {
			t.Errorf("expected known zero balance, got state=%s, val=%v", info.CurrentBalanceState, info.CurrentBalanceMinor)
		}
		if info.CalculatedAvailableState != BalanceStateKnown || *info.CalculatedAvailableMinor != 0 {
			t.Errorf("expected known zero calculated available, got %v", info.CalculatedAvailableMinor)
		}
	})

	t.Run("negative balance (overdraft) handled correctly", func(t *testing.T) {
		neg := int64(-50000) // -500.00 RUB
		acc := &Account{
			ID:                  accountID,
			FamilyID:            familyID,
			Type:                TypeChecking,
			Currency:            "RUB",
			CurrentBalanceMinor: &neg,
		}

		holds := []PendingHold{
			{ID: uuid.New(), AmountMinor: -20000, HoldMinor: 20000, Currency: "RUB"}, // 200.00 RUB hold
		}

		info, err := CalculateBalance(acc, holds)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// -500 - 200 = -700 RUB
		if *info.CalculatedAvailableMinor != -70000 {
			t.Errorf("expected -70000, got %d", *info.CalculatedAvailableMinor)
		}
	})

	t.Run("currency mismatch between account and hold fails calculation", func(t *testing.T) {
		curr := int64(100000)
		acc := &Account{
			ID:                  accountID,
			FamilyID:            familyID,
			Type:                TypeChecking,
			Currency:            "RUB",
			CurrentBalanceMinor: &curr,
		}

		holds := []PendingHold{
			{ID: uuid.New(), AmountMinor: -1000, HoldMinor: 1000, Currency: "USD"},
		}

		_, err := CalculateBalance(acc, holds)
		if err == nil {
			t.Fatalf("expected error on currency mismatch, got nil")
		}
		if !errors.Is(err, ErrCurrencyMismatch) {
			t.Errorf("expected ErrCurrencyMismatch, got %v", err)
		}
	})
}

func TestBalanceCalculation_CreditCardAccount(t *testing.T) {
	accountID := uuid.New()
	familyID := uuid.New()

	t.Run("known credit limit, debt, and holds", func(t *testing.T) {
		limit := int64(10000000) // 100,000.00 RUB limit
		debt := int64(2000000)   // 20,000.00 RUB current debt
		acc := &Account{
			ID:               accountID,
			FamilyID:         familyID,
			Type:             TypeCreditCard,
			Currency:         "RUB",
			CreditLimitMinor: &limit,
			DebtBalanceMinor: &debt,
		}

		holds := []PendingHold{
			{ID: uuid.New(), AmountMinor: -500000, HoldMinor: 500000, Currency: "RUB"}, // 5,000.00 RUB hold
		}

		info, err := CalculateBalance(acc, holds)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if info.CreditLimitState != BalanceStateKnown || *info.CreditLimitMinor != 10000000 {
			t.Errorf("expected limit 10000000, got %v", info.CreditLimitMinor)
		}
		if info.DebtBalanceState != BalanceStateKnown || *info.DebtBalanceMinor != 2000000 {
			t.Errorf("expected debt 2000000, got %v", info.DebtBalanceMinor)
		}
		// Available Credit = 100,000 - 20,000 - 5,000 = 75,000 RUB (7,500,000 minor)
		if info.CalculatedAvailableState != BalanceStateKnown || *info.CalculatedAvailableMinor != 7500000 {
			t.Errorf("expected available credit 7500000, got %v", info.CalculatedAvailableMinor)
		}
	})

	t.Run("credit card with unknown debt returns unknown available credit", func(t *testing.T) {
		limit := int64(10000000)
		acc := &Account{
			ID:               accountID,
			FamilyID:         familyID,
			Type:             TypeCreditCard,
			Currency:         "RUB",
			CreditLimitMinor: &limit,
			DebtBalanceMinor: nil, // UNKNOWN
		}

		info, err := CalculateBalance(acc, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if info.DebtBalanceState != BalanceStateUnknown {
			t.Errorf("expected debt unknown, got %s", info.DebtBalanceState)
		}
		if info.CalculatedAvailableState != BalanceStateUnknown || info.CalculatedAvailableMinor != nil {
			t.Errorf("expected calculated available unknown, got %v", info.CalculatedAvailableMinor)
		}
	})

	t.Run("credit card with unknown credit limit returns unknown available credit", func(t *testing.T) {
		debt := int64(500000)
		acc := &Account{
			ID:               accountID,
			FamilyID:         familyID,
			Type:             TypeCreditCard,
			Currency:         "RUB",
			CreditLimitMinor: nil, // UNKNOWN
			DebtBalanceMinor: &debt,
		}

		info, err := CalculateBalance(acc, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if info.CreditLimitState != BalanceStateUnknown {
			t.Errorf("expected credit limit unknown, got %s", info.CreditLimitState)
		}
		if info.CalculatedAvailableState != BalanceStateUnknown || info.CalculatedAvailableMinor != nil {
			t.Errorf("expected calculated available unknown, got %v", info.CalculatedAvailableMinor)
		}
	})
}

func TestCard_MaskedPANValidation(t *testing.T) {
	tests := []struct {
		name    string
		pan     string
		wantErr bool
	}{
		{"Valid *1234", "*1234", false},
		{"Valid **** 1234", "**** 1234", false},
		{"Valid 411111******1234", "411111******1234", false},
		{"Valid ****1234", "****1234", false},
		{"Empty string", "", true},
		{"Unmasked 16-digit PAN (STRICTLY PROHIBITED)", "4111111111111234", true},
		{"Unmasked 13-digit PAN", "4111111111123", true},
		{"Plain text without asterisk", "1234", true},
		{"Too long string", "411111***************1234", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateMaskedPAN(tc.pan)
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidateMaskedPAN(%q) err = %v, wantErr %v", tc.pan, err, tc.wantErr)
			}
		})
	}
}

func TestAccountService_CRUD_And_Rules(t *testing.T) {
	repo := newMockRepo()
	svc := NewService(repo)
	ctx := context.Background()

	familyID := uuid.New()
	userID := uuid.New()

	// 1. Create checking account
	acc, err := svc.CreateAccount(ctx, familyID, CreateAccountParams{
		OwnerUserID: userID,
		Name:        "Main Checking",
		Type:        TypeChecking,
		Currency:    "RUB",
	})
	if err != nil {
		t.Fatalf("CreateAccount failed: %v", err)
	}
	if acc.Name != "Main Checking" || acc.Type != TypeChecking {
		t.Errorf("unexpected account fields: %+v", acc)
	}

	// 2. Reject invalid account type
	_, err = svc.CreateAccount(ctx, familyID, CreateAccountParams{
		OwnerUserID: userID,
		Name:        "Bad Type",
		Type:        "invalid_type",
		Currency:    "RUB",
	})
	if !errors.Is(err, ErrInvalidAccountType) {
		t.Errorf("expected ErrInvalidAccountType, got %v", err)
	}

	// 3. Reject invalid currency
	_, err = svc.CreateAccount(ctx, familyID, CreateAccountParams{
		OwnerUserID: userID,
		Name:        "Bad Curr",
		Type:        TypeChecking,
		Currency:    "XYZ",
	})
	if !errors.Is(err, domain.ErrInvalidCurrency) {
		t.Errorf("expected ErrInvalidCurrency, got %v", err)
	}

	// 4. Update account name
	newName := "Updated Name"
	updated, err := svc.UpdateAccount(ctx, familyID, acc.ID, UpdateAccountParams{Name: &newName})
	if err != nil || updated.Name != "Updated Name" {
		t.Fatalf("UpdateAccount failed: %v", err)
	}

	// 5. Create multiple cards linked to account
	card1, err := svc.CreateCard(ctx, familyID, CreateCardParams{
		AccountID:     acc.ID,
		OwnerUserID:   userID,
		MaskedPAN:     "*1234",
		DisplayName:   "Physical Debit Card",
		PaymentSystem: PaymentSystemMir,
		IsVirtual:     false,
		IsPrimary:     true,
	})
	if err != nil {
		t.Fatalf("CreateCard 1 failed: %v", err)
	}
	if !card1.IsPrimary {
		t.Errorf("expected card1 to be primary")
	}

	card2, err := svc.CreateCard(ctx, familyID, CreateCardParams{
		AccountID:     acc.ID,
		OwnerUserID:   userID,
		MaskedPAN:     "*5678",
		DisplayName:   "Virtual Card",
		PaymentSystem: PaymentSystemVisa,
		IsVirtual:     true,
		IsPrimary:     false,
	})
	if err != nil {
		t.Fatalf("CreateCard 2 failed: %v", err)
	}

	// List cards
	cards, err := svc.ListCards(ctx, familyID, acc.ID)
	if err != nil || len(cards) != 2 {
		t.Fatalf("expected 2 cards, got %d, err: %v", len(cards), err)
	}

	// Switch primary card to card2
	err = svc.SetPrimaryCard(ctx, familyID, acc.ID, card2.ID)
	if err != nil {
		t.Fatalf("SetPrimaryCard failed: %v", err)
	}
	c1, _ := svc.GetCard(ctx, familyID, card1.ID)
	c2, _ := svc.GetCard(ctx, familyID, card2.ID)
	if c1.IsPrimary || !c2.IsPrimary {
		t.Errorf("expected card2 primary, card1 not primary; got c1=%v, c2=%v", c1.IsPrimary, c2.IsPrimary)
	}

	// Close card2
	err = svc.CloseCard(ctx, familyID, card2.ID)
	if err != nil {
		t.Fatalf("CloseCard failed: %v", err)
	}
	c2Closed, _ := svc.GetCard(ctx, familyID, card2.ID)
	if c2Closed.Status != CardStatusClosed || c2Closed.IsPrimary {
		t.Errorf("expected c2 closed and not primary, got status=%s, primary=%v", c2Closed.Status, c2Closed.IsPrimary)
	}

	// Archive account
	err = svc.ArchiveAccount(ctx, familyID, acc.ID)
	if err != nil {
		t.Fatalf("ArchiveAccount failed: %v", err)
	}
	archived, _ := svc.GetAccount(ctx, familyID, acc.ID)
	if !archived.IsArchived {
		t.Errorf("expected account to be archived")
	}

	// Cannot add cards to archived account
	_, err = svc.CreateCard(ctx, familyID, CreateCardParams{
		AccountID:     acc.ID,
		OwnerUserID:   userID,
		MaskedPAN:     "*9999",
		DisplayName:   "Blocked Card",
		PaymentSystem: PaymentSystemMir,
	})
	if !errors.Is(err, ErrAccountArchived) {
		t.Errorf("expected ErrAccountArchived, got %v", err)
	}
}

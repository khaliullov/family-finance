package account

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/khaliullov/family-finance/internal/auth"
)

// mockRepository implements Repository for fast, isolated HTTP unit tests.
type mockRepository struct {
	accounts map[uuid.UUID]*Account
	cards    map[uuid.UUID]*Card
	holds    map[uuid.UUID][]PendingHold
}

func newMockRepository() *mockRepository {
	return &mockRepository{
		accounts: make(map[uuid.UUID]*Account),
		cards:    make(map[uuid.UUID]*Card),
		holds:    make(map[uuid.UUID][]PendingHold),
	}
}

func (m *mockRepository) CreateAccount(ctx context.Context, familyID uuid.UUID, acc Account) (*Account, error) {
	if acc.ID == uuid.Nil {
		acc.ID = uuid.New()
	}
	acc.FamilyID = familyID
	acc.CreatedAt = time.Now().UTC()
	acc.UpdatedAt = time.Now().UTC()
	cp := acc
	m.accounts[acc.ID] = &cp
	return &cp, nil
}

func (m *mockRepository) GetAccountByID(ctx context.Context, familyID, accountID uuid.UUID) (*Account, error) {
	acc, ok := m.accounts[accountID]
	if !ok || acc.FamilyID != familyID {
		return nil, ErrAccountNotFound
	}
	cp := *acc
	return &cp, nil
}

func (m *mockRepository) ListAccountsByFamily(ctx context.Context, familyID uuid.UUID, includeArchived bool) ([]Account, error) {
	var list []Account
	for _, acc := range m.accounts {
		if acc.FamilyID == familyID {
			if !acc.IsArchived || includeArchived {
				list = append(list, *acc)
			}
		}
	}
	return list, nil
}

func (m *mockRepository) UpdateAccount(ctx context.Context, familyID, accountID uuid.UUID, params UpdateAccountParams) (*Account, error) {
	acc, ok := m.accounts[accountID]
	if !ok || acc.FamilyID != familyID {
		return nil, ErrAccountNotFound
	}
	if params.Name != nil {
		acc.Name = *params.Name
	}
	acc.UpdatedAt = time.Now().UTC()
	cp := *acc
	return &cp, nil
}

func (m *mockRepository) ArchiveAccount(ctx context.Context, familyID, accountID uuid.UUID) error {
	acc, ok := m.accounts[accountID]
	if !ok || acc.FamilyID != familyID {
		return ErrAccountNotFound
	}
	acc.IsArchived = true
	acc.UpdatedAt = time.Now().UTC()
	return nil
}

func (m *mockRepository) UpdateBalanceSnapshot(ctx context.Context, familyID, accountID uuid.UUID, snapshot BalanceSnapshotParams) (*Account, error) {
	acc, ok := m.accounts[accountID]
	if !ok || acc.FamilyID != familyID {
		return nil, ErrAccountNotFound
	}
	acc.CurrentBalanceMinor = snapshot.CurrentBalanceMinor
	acc.AvailableBalanceMinor = snapshot.AvailableBalanceMinor
	acc.DebtBalanceMinor = snapshot.DebtBalanceMinor
	acc.CreditLimitMinor = snapshot.CreditLimitMinor
	acc.StatementBalanceMinor = snapshot.StatementBalanceMinor
	acc.MinimalPaymentMinor = snapshot.MinimalPaymentMinor
	acc.GracePeriodEndDate = snapshot.GracePeriodEndDate
	s := string(snapshot.BalanceSource)
	acc.BalanceSource = &s
	acc.BalanceUpdatedAt = &snapshot.ObservedAt
	acc.UpdatedAt = time.Now().UTC()
	cp := *acc
	return &cp, nil
}

func (m *mockRepository) CreateCard(ctx context.Context, familyID uuid.UUID, card Card) (*Card, error) {
	if card.ID == uuid.Nil {
		card.ID = uuid.New()
	}
	card.FamilyID = familyID
	card.CreatedAt = time.Now().UTC()
	card.UpdatedAt = time.Now().UTC()
	cp := card
	m.cards[card.ID] = &cp
	return &cp, nil
}

func (m *mockRepository) GetCardByID(ctx context.Context, familyID, cardID uuid.UUID) (*Card, error) {
	card, ok := m.cards[cardID]
	if !ok || card.FamilyID != familyID {
		return nil, ErrCardNotFound
	}
	cp := *card
	return &cp, nil
}

func (m *mockRepository) ListCardsByAccount(ctx context.Context, familyID, accountID uuid.UUID) ([]Card, error) {
	var list []Card
	for _, c := range m.cards {
		if c.FamilyID == familyID && c.AccountID == accountID {
			list = append(list, *c)
		}
	}
	return list, nil
}

func (m *mockRepository) UpdateCard(ctx context.Context, familyID, cardID uuid.UUID, params UpdateCardParams) (*Card, error) {
	card, ok := m.cards[cardID]
	if !ok || card.FamilyID != familyID {
		return nil, ErrCardNotFound
	}
	targetStatus := card.Status
	if params.Status != nil {
		targetStatus = *params.Status
	}
	targetPrimary := card.IsPrimary
	if params.IsPrimary != nil {
		targetPrimary = *params.IsPrimary
	}
	if targetStatus == CardStatusClosed {
		if params.IsPrimary != nil && *params.IsPrimary {
			return nil, ErrCardClosed
		}
		targetPrimary = false
	} else if card.Status == CardStatusClosed {
		if params.IsPrimary != nil && *params.IsPrimary {
			return nil, ErrCardClosed
		}
	}

	if params.DisplayName != nil {
		card.DisplayName = *params.DisplayName
	}
	card.Status = targetStatus
	card.IsPrimary = targetPrimary
	card.UpdatedAt = time.Now().UTC()
	cp := *card
	return &cp, nil
}

func (m *mockRepository) CloseCard(ctx context.Context, familyID, cardID uuid.UUID) error {
	card, ok := m.cards[cardID]
	if !ok || card.FamilyID != familyID {
		return ErrCardNotFound
	}
	card.Status = CardStatusClosed
	card.IsPrimary = false
	card.UpdatedAt = time.Now().UTC()
	return nil
}

func (m *mockRepository) SetPrimaryCard(ctx context.Context, familyID, accountID, cardID uuid.UUID) error {
	for _, c := range m.cards {
		if c.FamilyID == familyID && c.AccountID == accountID {
			c.IsPrimary = (c.ID == cardID)
		}
	}
	return nil
}

func (m *mockRepository) GetPendingHolds(ctx context.Context, familyID, accountID uuid.UUID) ([]PendingHold, error) {
	return m.holds[accountID], nil
}

func (m *mockRepository) HasFinancialHistory(ctx context.Context, familyID, accountID uuid.UUID) (bool, error) {
	return false, nil
}

func setupTestHandler() (*Handler, *mockRepository, uuid.UUID, uuid.UUID) {
	repo := newMockRepository()
	svc := NewService(repo)
	handler := NewHandler(svc)
	familyID := uuid.New()
	userID := uuid.New()
	return handler, repo, familyID, userID
}

func TestHandler_AccountEndpoints(t *testing.T) {
	handler, repo, familyID, userID := setupTestHandler()

	mux := http.NewServeMux()
	dummyAuthMw := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := auth.ContextWithFamilyAndUser(r.Context(), familyID, userID, "owner")
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
	handler.RegisterRoutes(mux, dummyAuthMw)

	// 1. Create Account - Success
	createReq := CreateAccountRequestBody{
		Name:     "Main Checking",
		Type:     TypeChecking,
		Currency: "RUB",
	}
	body, _ := json.Marshal(createReq)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/accounts", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status 201 Created, got %d: %s", rec.Code, rec.Body.String())
	}

	var created Account
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if created.Name != "Main Checking" || created.Currency != "RUB" {
		t.Errorf("unexpected account data: %+v", created)
	}

	// 2. Create Account - Invalid Currency (fail closed)
	createReqBadCurr := CreateAccountRequestBody{
		Name:     "Invalid Currency Account",
		Type:     TypeChecking,
		Currency: "INVALID",
	}
	bodyBadCurr, _ := json.Marshal(createReqBadCurr)
	reqBadCurr := httptest.NewRequest(http.MethodPost, "/api/v1/accounts", bytes.NewReader(bodyBadCurr))
	recBadCurr := httptest.NewRecorder()
	mux.ServeHTTP(recBadCurr, reqBadCurr)
	if recBadCurr.Code != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for invalid currency, got %d", recBadCurr.Code)
	}

	// 3. List Accounts
	reqList := httptest.NewRequest(http.MethodGet, "/api/v1/accounts", nil)
	recList := httptest.NewRecorder()
	mux.ServeHTTP(recList, reqList)

	if recList.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", recList.Code)
	}
	var accList []Account
	if err := json.Unmarshal(recList.Body.Bytes(), &accList); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if len(accList) != 1 {
		t.Errorf("expected 1 account, got %d", len(accList))
	}

	// 4. Get Account by ID
	reqGet := httptest.NewRequest(http.MethodGet, "/api/v1/accounts/"+created.ID.String(), nil)
	recGet := httptest.NewRecorder()
	mux.ServeHTTP(recGet, reqGet)
	if recGet.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", recGet.Code)
	}

	// 5. Update Account Name
	newName := "Updated Checking Name"
	updateBody, _ := json.Marshal(UpdateAccountParams{Name: &newName})
	reqPatch := httptest.NewRequest(http.MethodPatch, "/api/v1/accounts/"+created.ID.String(), bytes.NewReader(updateBody))
	recPatch := httptest.NewRecorder()
	mux.ServeHTTP(recPatch, reqPatch)
	if recPatch.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on PATCH, got %d: %s", recPatch.Code, recPatch.Body.String())
	}

	// 6. Get Balance Breakdown
	// Seed a balance snapshot
	bal := int64(1500000)
	_, _ = repo.UpdateBalanceSnapshot(context.Background(), familyID, created.ID, BalanceSnapshotParams{
		CurrentBalanceMinor:   &bal,
		AvailableBalanceMinor: &bal,
		BalanceSource:         SourceBankAPI,
		ObservedAt:            time.Now(),
	})
	// Add pending hold
	repo.holds[created.ID] = []PendingHold{
		{
			ID:          uuid.New(),
			AmountMinor: -50000,
			HoldMinor:   50000,
			Currency:    "RUB",
			Description: "Coffee shop hold",
		},
	}

	reqBal := httptest.NewRequest(http.MethodGet, "/api/v1/accounts/"+created.ID.String()+"/balance", nil)
	recBal := httptest.NewRecorder()
	mux.ServeHTTP(recBal, reqBal)
	if recBal.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on /balance, got %d: %s", recBal.Code, recBal.Body.String())
	}
	var balInfo BalanceInfo
	if err := json.Unmarshal(recBal.Body.Bytes(), &balInfo); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if balInfo.CurrentBalanceMinor == nil || *balInfo.CurrentBalanceMinor != 1500000 {
		t.Errorf("expected current 1500000, got %v", balInfo.CurrentBalanceMinor)
	}
	if balInfo.PendingHoldsMinor != 50000 {
		t.Errorf("expected holds 50000, got %d", balInfo.PendingHoldsMinor)
	}
	if balInfo.CalculatedAvailableMinor == nil || *balInfo.CalculatedAvailableMinor != 1450000 {
		t.Errorf("expected calculated available 1450000, got %v", balInfo.CalculatedAvailableMinor)
	}

	// 7. Archive Account
	reqArchive := httptest.NewRequest(http.MethodPost, "/api/v1/accounts/"+created.ID.String()+"/archive", nil)
	recArchive := httptest.NewRecorder()
	mux.ServeHTTP(recArchive, reqArchive)
	if recArchive.Code != http.StatusNoContent {
		t.Fatalf("expected 204 No Content on archive, got %d", recArchive.Code)
	}

	// List without include_archived should return 0
	reqListAfter := httptest.NewRequest(http.MethodGet, "/api/v1/accounts", nil)
	recListAfter := httptest.NewRecorder()
	mux.ServeHTTP(recListAfter, reqListAfter)
	var emptyList []Account
	_ = json.Unmarshal(recListAfter.Body.Bytes(), &emptyList)
	if len(emptyList) != 0 {
		t.Errorf("expected 0 active accounts after archive, got %d", len(emptyList))
	}
}

func TestHandler_CardEndpoints(t *testing.T) {
	handler, repo, familyID, userID := setupTestHandler()

	mux := http.NewServeMux()
	dummyAuthMw := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := auth.ContextWithFamilyAndUser(r.Context(), familyID, userID, "owner")
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
	handler.RegisterRoutes(mux, dummyAuthMw)

	// Create an account first
	acc, err := repo.CreateAccount(context.Background(), familyID, Account{
		ID:          uuid.New(),
		FamilyID:    familyID,
		OwnerUserID: userID,
		Name:        "Card Account",
		Type:        TypeChecking,
		Currency:    "RUB",
	})
	if err != nil {
		t.Fatalf("failed to create account: %v", err)
	}

	// 1. Create Card with plain unmasked PAN should FAIL
	badCardReq := CreateCardRequestBody{
		MaskedPAN:     "4111111111111234", // RAW 16 digits
		DisplayName:   "Unsafe Card",
		PaymentSystem: PaymentSystemVisa,
	}
	badBody, _ := json.Marshal(badCardReq)
	reqBad := httptest.NewRequest(http.MethodPost, "/api/v1/accounts/"+acc.ID.String()+"/cards", bytes.NewReader(badBody))
	recBad := httptest.NewRecorder()
	mux.ServeHTTP(recBad, reqBad)
	if recBad.Code != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for unmasked PAN, got %d", recBad.Code)
	}

	// 2. Create Card with valid masked PAN
	cardReq := CreateCardRequestBody{
		MaskedPAN:     "*1234",
		DisplayName:   "My Debit Card",
		PaymentSystem: PaymentSystemMir,
		IsPrimary:     true,
	}
	cardBody, _ := json.Marshal(cardReq)
	reqCard := httptest.NewRequest(http.MethodPost, "/api/v1/accounts/"+acc.ID.String()+"/cards", bytes.NewReader(cardBody))
	recCard := httptest.NewRecorder()
	mux.ServeHTTP(recCard, reqCard)

	if recCard.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created on card create, got %d: %s", recCard.Code, recCard.Body.String())
	}

	var createdCard Card
	if err := json.Unmarshal(recCard.Body.Bytes(), &createdCard); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if createdCard.MaskedPAN != "*1234" || !createdCard.IsPrimary {
		t.Errorf("unexpected card data: %+v", createdCard)
	}

	// 3. List Cards for Account
	reqListCards := httptest.NewRequest(http.MethodGet, "/api/v1/accounts/"+acc.ID.String()+"/cards", nil)
	recListCards := httptest.NewRecorder()
	mux.ServeHTTP(recListCards, reqListCards)
	if recListCards.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", recListCards.Code)
	}
	var cardList []Card
	_ = json.Unmarshal(recListCards.Body.Bytes(), &cardList)
	if len(cardList) != 1 {
		t.Errorf("expected 1 card, got %d", len(cardList))
	}

	// 4. Get Card by ID
	reqGetCard := httptest.NewRequest(http.MethodGet, "/api/v1/cards/"+createdCard.ID.String(), nil)
	recGetCard := httptest.NewRecorder()
	mux.ServeHTTP(recGetCard, reqGetCard)
	if recGetCard.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on GET /cards/{id}, got %d", recGetCard.Code)
	}

	// 5. Update Card
	newCardName := "Renamed Card"
	patchBody, _ := json.Marshal(UpdateCardParams{DisplayName: &newCardName})
	reqPatchCard := httptest.NewRequest(http.MethodPatch, "/api/v1/cards/"+createdCard.ID.String(), bytes.NewReader(patchBody))
	recPatchCard := httptest.NewRecorder()
	mux.ServeHTTP(recPatchCard, reqPatchCard)
	if recPatchCard.Code != http.StatusOK {
		t.Fatalf("expected 200 OK on PATCH /cards/{id}, got %d", recPatchCard.Code)
	}

	// 6. Close Card
	reqClose := httptest.NewRequest(http.MethodPost, "/api/v1/cards/"+createdCard.ID.String()+"/close", nil)
	recClose := httptest.NewRecorder()
	mux.ServeHTTP(recClose, reqClose)
	if recClose.Code != http.StatusNoContent {
		t.Fatalf("expected 204 No Content on close card, got %d", recClose.Code)
	}
}

func TestHandler_AuthenticationEnforcement(t *testing.T) {
	handler, _, _, _ := setupTestHandler()

	mux := http.NewServeMux()
	// Middleware that enforces authentication and tenant context
	strictAuthMw := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// No tenant context set in request
			next.ServeHTTP(w, r)
		})
	}
	handler.RegisterRoutes(mux, strictAuthMw)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/accounts", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 Unauthorized when missing context, got %d", rec.Code)
	}
}

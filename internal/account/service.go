package account

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/khaliullov/family-finance/internal/domain"
)

// Service coordinates business logic for accounts, cards, and balance calculations.
type Service struct {
	repo Repository
}

// NewService creates a new account Service.
func NewService(repo Repository) *Service {
	return &Service{repo: repo}
}

// CreateAccountParams contains input fields for creating a new account.
type CreateAccountParams struct {
	BankConnectionID *uuid.UUID  `json:"bank_connection_id,omitempty"`
	OwnerUserID      uuid.UUID   `json:"owner_user_id"`
	ExternalID       *string     `json:"external_id,omitempty"`
	Name             string      `json:"name"`
	Type             AccountType `json:"type"`
	Currency         string      `json:"currency"`
	CreditLimitMinor *int64      `json:"credit_limit_minor,omitempty"`
}

// CreateAccount validates input and creates a new account.
func (s *Service) CreateAccount(ctx context.Context, familyID uuid.UUID, params CreateAccountParams) (*Account, error) {
	if familyID == uuid.Nil {
		return nil, ErrUnauthorized
	}
	if params.OwnerUserID == uuid.Nil {
		return nil, fmt.Errorf("%w: missing owner user id", ErrInvalidOwner)
	}

	name := strings.TrimSpace(params.Name)
	if name == "" {
		return nil, ErrInvalidAccountName
	}
	if len(name) > 100 {
		return nil, fmt.Errorf("%w: name cannot exceed 100 characters", ErrInvalidAccountName)
	}

	if !params.Type.IsValid() {
		return nil, fmt.Errorf("%w: %q", ErrInvalidAccountType, params.Type)
	}

	curr := strings.ToUpper(strings.TrimSpace(params.Currency))
	if err := domain.ValidateCurrency(curr); err != nil {
		return nil, err
	}

	acc := Account{
		ID:               uuid.New(),
		FamilyID:         familyID,
		BankConnectionID: params.BankConnectionID,
		OwnerUserID:      params.OwnerUserID,
		ExternalID:       params.ExternalID,
		Name:             name,
		Type:             params.Type,
		Currency:         curr,
		CreditLimitMinor: params.CreditLimitMinor,
		IsArchived:       false,
	}

	return s.repo.CreateAccount(ctx, familyID, acc)
}

// GetAccount retrieves an account by ID, validating family tenant ownership.
func (s *Service) GetAccount(ctx context.Context, familyID, accountID uuid.UUID) (*Account, error) {
	if familyID == uuid.Nil {
		return nil, ErrUnauthorized
	}
	if accountID == uuid.Nil {
		return nil, ErrAccountNotFound
	}
	return s.repo.GetAccountByID(ctx, familyID, accountID)
}

// ListAccounts lists all accounts for a family.
func (s *Service) ListAccounts(ctx context.Context, familyID uuid.UUID, includeArchived bool) ([]Account, error) {
	if familyID == uuid.Nil {
		return nil, ErrUnauthorized
	}
	return s.repo.ListAccountsByFamily(ctx, familyID, includeArchived)
}

// UpdateAccount updates editable account attributes.
func (s *Service) UpdateAccount(ctx context.Context, familyID, accountID uuid.UUID, params UpdateAccountParams) (*Account, error) {
	if familyID == uuid.Nil {
		return nil, ErrUnauthorized
	}
	if accountID == uuid.Nil {
		return nil, ErrAccountNotFound
	}

	if params.Name != nil {
		trimmed := strings.TrimSpace(*params.Name)
		if trimmed == "" {
			return nil, ErrInvalidAccountName
		}
		if len(trimmed) > 100 {
			return nil, fmt.Errorf("%w: name cannot exceed 100 characters", ErrInvalidAccountName)
		}
		params.Name = &trimmed
	}

	return s.repo.UpdateAccount(ctx, familyID, accountID, params)
}

// ArchiveAccount marks an account as archived.
func (s *Service) ArchiveAccount(ctx context.Context, familyID, accountID uuid.UUID) error {
	if familyID == uuid.Nil {
		return ErrUnauthorized
	}
	if accountID == uuid.Nil {
		return ErrAccountNotFound
	}
	return s.repo.ArchiveAccount(ctx, familyID, accountID)
}

// RecordBalanceSnapshot updates an account's balance snapshot from an authoritative observation.
func (s *Service) RecordBalanceSnapshot(ctx context.Context, familyID, accountID uuid.UUID, snapshot BalanceSnapshotParams) (*Account, error) {
	if familyID == uuid.Nil {
		return nil, ErrUnauthorized
	}
	if accountID == uuid.Nil {
		return nil, ErrAccountNotFound
	}
	if snapshot.ObservedAt.IsZero() {
		snapshot.ObservedAt = time.Now()
	}
	return s.repo.UpdateBalanceSnapshot(ctx, familyID, accountID, snapshot)
}

// GetBalance calculates and returns explicit balance information for an account.
func (s *Service) GetBalance(ctx context.Context, familyID, accountID uuid.UUID) (*BalanceInfo, error) {
	acc, err := s.GetAccount(ctx, familyID, accountID)
	if err != nil {
		return nil, err
	}

	holds, err := s.repo.GetPendingHolds(ctx, familyID, accountID)
	if err != nil {
		return nil, err
	}

	return CalculateBalance(acc, holds)
}

// CalculateBalance executes the deterministic balance calculation according to formal specifications.
func CalculateBalance(acc *Account, holds []PendingHold) (*BalanceInfo, error) {
	info := &BalanceInfo{
		AccountID:                acc.ID,
		AccountType:              acc.Type,
		Currency:                 acc.Currency,
		CurrentBalanceState:      BalanceStateUnknown,
		AvailableBalanceState:    BalanceStateUnknown,
		CreditLimitState:         BalanceStateUnavailable,
		DebtBalanceState:         BalanceStateUnavailable,
		CalculatedAvailableState: BalanceStateUnknown,
		BalanceUpdatedAt:         acc.BalanceUpdatedAt,
		BalanceSource:            acc.BalanceSource,
	}

	// 1. Current Balance
	if acc.CurrentBalanceMinor != nil {
		info.CurrentBalanceMinor = acc.CurrentBalanceMinor
		info.CurrentBalanceState = BalanceStateKnown
	}

	// 2. Direct Bank Available Balance
	if acc.AvailableBalanceMinor != nil {
		info.AvailableBalanceMinor = acc.AvailableBalanceMinor
		info.AvailableBalanceState = BalanceStateKnown
	}

	// 3. Sum of Pending Holds (strictly in account currency)
	var totalHoldsMinor int64
	for _, h := range holds {
		if h.Currency != acc.Currency {
			return nil, fmt.Errorf("%w: hold %s currency %s != account currency %s",
				ErrCurrencyMismatch, h.ID, h.Currency, acc.Currency)
		}
		// Overflow check on sum
		if h.HoldMinor > 0 && totalHoldsMinor > math.MaxInt64-h.HoldMinor {
			return nil, fmt.Errorf("overflow computing pending holds sum")
		}
		totalHoldsMinor += h.HoldMinor
	}
	info.PendingHoldsMinor = totalHoldsMinor
	info.PendingHoldsCount = len(holds)

	// 4. Account Type-Specific Calculation
	if acc.Type == TypeCreditCard {
		// Credit limits and debt
		if acc.CreditLimitMinor != nil {
			info.CreditLimitMinor = acc.CreditLimitMinor
			info.CreditLimitState = BalanceStateKnown
		} else {
			info.CreditLimitState = BalanceStateUnknown
		}

		if acc.DebtBalanceMinor != nil {
			info.DebtBalanceMinor = acc.DebtBalanceMinor
			info.DebtBalanceState = BalanceStateKnown
		} else {
			info.DebtBalanceState = BalanceStateUnknown
		}

		// Available Credit = Credit Limit - Debt Balance - Pending Holds
		// If either Credit Limit or Debt Balance is unknown, calculated credit is UNKNOWN
		if info.CreditLimitState == BalanceStateKnown && info.DebtBalanceState == BalanceStateKnown {
			limit := *info.CreditLimitMinor
			debt := *info.DebtBalanceMinor

			// Overflow-safe subtraction: limit - debt - totalHolds
			avail := limit - debt - totalHoldsMinor
			info.CalculatedAvailableMinor = &avail
			info.CalculatedAvailableState = BalanceStateKnown
		} else {
			info.CalculatedAvailableState = BalanceStateUnknown
		}
	} else {
		// Checking, Savings, Deposit, Cash
		// Available Balance = Current Balance - Pending Holds
		// If Current Balance is unknown, calculated available is UNKNOWN (never 0!)
		if info.CurrentBalanceState == BalanceStateKnown {
			curr := *info.CurrentBalanceMinor
			avail := curr - totalHoldsMinor
			info.CalculatedAvailableMinor = &avail
			info.CalculatedAvailableState = BalanceStateKnown
		} else {
			info.CalculatedAvailableState = BalanceStateUnknown
		}
	}

	return info, nil
}

// CreateCardParams contains input fields for creating a card.
type CreateCardParams struct {
	AccountID     uuid.UUID     `json:"account_id"`
	OwnerUserID   uuid.UUID     `json:"owner_user_id"`
	ExternalID    *string       `json:"external_id,omitempty"`
	MaskedPAN     string        `json:"masked_pan"`
	DisplayName   string        `json:"display_name"`
	PaymentSystem PaymentSystem `json:"payment_system"`
	IsVirtual     bool          `json:"is_virtual"`
	IsPrimary     bool          `json:"is_primary"`
}

// CreateCard validates and attaches a new card to an account.
func (s *Service) CreateCard(ctx context.Context, familyID uuid.UUID, params CreateCardParams) (*Card, error) {
	if familyID == uuid.Nil {
		return nil, ErrUnauthorized
	}
	if params.AccountID == uuid.Nil {
		return nil, fmt.Errorf("%w: missing account id", ErrAccountNotFound)
	}
	if params.OwnerUserID == uuid.Nil {
		return nil, fmt.Errorf("%w: missing owner user id", ErrInvalidOwner)
	}

	// Verify account belongs to family and is not archived
	acc, err := s.repo.GetAccountByID(ctx, familyID, params.AccountID)
	if err != nil {
		return nil, err
	}
	if acc.IsArchived {
		return nil, ErrAccountArchived
	}

	displayName := strings.TrimSpace(params.DisplayName)
	if displayName == "" {
		return nil, ErrInvalidCardName
	}
	if len(displayName) > 100 {
		return nil, fmt.Errorf("%w: display name cannot exceed 100 characters", ErrInvalidCardName)
	}

	if err := ValidateMaskedPAN(params.MaskedPAN); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidMaskedPAN, err)
	}

	if !params.PaymentSystem.IsValid() {
		return nil, fmt.Errorf("%w: %q", ErrInvalidPaymentSystem, params.PaymentSystem)
	}

	card := Card{
		ID:            uuid.New(),
		FamilyID:      familyID,
		AccountID:     params.AccountID,
		OwnerUserID:   params.OwnerUserID,
		ExternalID:    params.ExternalID,
		MaskedPAN:     strings.TrimSpace(params.MaskedPAN),
		DisplayName:   displayName,
		PaymentSystem: params.PaymentSystem,
		IsVirtual:     params.IsVirtual,
		IsPrimary:     params.IsPrimary,
		Status:        CardStatusActive,
	}

	return s.repo.CreateCard(ctx, familyID, card)
}

// GetCard retrieves a card by ID.
func (s *Service) GetCard(ctx context.Context, familyID, cardID uuid.UUID) (*Card, error) {
	if familyID == uuid.Nil {
		return nil, ErrUnauthorized
	}
	if cardID == uuid.Nil {
		return nil, ErrCardNotFound
	}
	return s.repo.GetCardByID(ctx, familyID, cardID)
}

// ListCards lists all cards linked to a specific account.
func (s *Service) ListCards(ctx context.Context, familyID, accountID uuid.UUID) ([]Card, error) {
	if familyID == uuid.Nil {
		return nil, ErrUnauthorized
	}
	if accountID == uuid.Nil {
		return nil, ErrAccountNotFound
	}
	// Verify account belongs to family
	if _, err := s.repo.GetAccountByID(ctx, familyID, accountID); err != nil {
		return nil, err
	}
	return s.repo.ListCardsByAccount(ctx, familyID, accountID)
}

// UpdateCard updates editable card attributes.
func (s *Service) UpdateCard(ctx context.Context, familyID, cardID uuid.UUID, params UpdateCardParams) (*Card, error) {
	if familyID == uuid.Nil {
		return nil, ErrUnauthorized
	}
	if cardID == uuid.Nil {
		return nil, ErrCardNotFound
	}

	if params.DisplayName != nil {
		trimmed := strings.TrimSpace(*params.DisplayName)
		if trimmed == "" {
			return nil, ErrInvalidCardName
		}
		if len(trimmed) > 100 {
			return nil, fmt.Errorf("%w: display name cannot exceed 100 characters", ErrInvalidCardName)
		}
		params.DisplayName = &trimmed
	}

	if params.Status != nil && !params.Status.IsValid() {
		return nil, fmt.Errorf("%w: %q", ErrInvalidCardStatus, *params.Status)
	}

	return s.repo.UpdateCard(ctx, familyID, cardID, params)
}

// CloseCard transitions a card to CLOSED status and clears its primary flag.
func (s *Service) CloseCard(ctx context.Context, familyID, cardID uuid.UUID) error {
	if familyID == uuid.Nil {
		return ErrUnauthorized
	}
	if cardID == uuid.Nil {
		return ErrCardNotFound
	}
	return s.repo.CloseCard(ctx, familyID, cardID)
}

// SetPrimaryCard designates a card as the primary card for its account.
func (s *Service) SetPrimaryCard(ctx context.Context, familyID, accountID, cardID uuid.UUID) error {
	if familyID == uuid.Nil {
		return ErrUnauthorized
	}
	if accountID == uuid.Nil {
		return ErrAccountNotFound
	}
	if cardID == uuid.Nil {
		return ErrCardNotFound
	}
	return s.repo.SetPrimaryCard(ctx, familyID, accountID, cardID)
}

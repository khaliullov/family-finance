package account

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/khaliullov/family-finance/internal/auth"
	"github.com/khaliullov/family-finance/internal/domain"
)

// Handler exposes HTTP REST endpoints for account and card operations.
type Handler struct {
	service *Service
}

// NewHandler creates a new account HTTP Handler.
func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

// RegisterRoutes registers all account and card endpoints on the provided ServeMux.
// All endpoints are protected by the provided authentication middleware.
func (h *Handler) RegisterRoutes(mux *http.ServeMux, authMw func(http.Handler) http.Handler) {
	register := func(prefix string) {
		// Accounts
		mux.Handle("GET "+prefix+"/accounts", authMw(http.HandlerFunc(h.HandleListAccounts)))
		mux.Handle("POST "+prefix+"/accounts", authMw(http.HandlerFunc(h.HandleCreateAccount)))
		mux.Handle("GET "+prefix+"/accounts/{id}", authMw(http.HandlerFunc(h.HandleGetAccount)))
		mux.Handle("PATCH "+prefix+"/accounts/{id}", authMw(http.HandlerFunc(h.HandleUpdateAccount)))
		mux.Handle("POST "+prefix+"/accounts/{id}/archive", authMw(http.HandlerFunc(h.HandleArchiveAccount)))
		mux.Handle("DELETE "+prefix+"/accounts/{id}", authMw(http.HandlerFunc(h.HandleArchiveAccount)))
		mux.Handle("GET "+prefix+"/accounts/{id}/balance", authMw(http.HandlerFunc(h.HandleGetAccountBalance)))

		// Cards linked to an account
		mux.Handle("GET "+prefix+"/accounts/{id}/cards", authMw(http.HandlerFunc(h.HandleListAccountCards)))
		mux.Handle("POST "+prefix+"/accounts/{id}/cards", authMw(http.HandlerFunc(h.HandleCreateCard)))

		// Standalone Card operations
		mux.Handle("GET "+prefix+"/cards/{id}", authMw(http.HandlerFunc(h.HandleGetCard)))
		mux.Handle("PATCH "+prefix+"/cards/{id}", authMw(http.HandlerFunc(h.HandleUpdateCard)))
		mux.Handle("POST "+prefix+"/cards/{id}/close", authMw(http.HandlerFunc(h.HandleCloseCard)))
		mux.Handle("DELETE "+prefix+"/cards/{id}", authMw(http.HandlerFunc(h.HandleCloseCard)))
	}

	register("/api/v1")
	register("")
}

// CreateAccountRequestBody represents the JSON payload to create an account.
type CreateAccountRequestBody struct {
	OwnerUserID      *uuid.UUID  `json:"owner_user_id,omitempty"`
	BankConnectionID *uuid.UUID  `json:"bank_connection_id,omitempty"`
	ExternalID       *string     `json:"external_id,omitempty"`
	Name             string      `json:"name"`
	Type             AccountType `json:"type"`
	Currency         string      `json:"currency"`
	CreditLimitMinor *int64      `json:"credit_limit_minor,omitempty"`
}

// HandleCreateAccount creates a new account for the authenticated family.
func (h *Handler) HandleCreateAccount(w http.ResponseWriter, r *http.Request) {
	familyID, ok := auth.FamilyIDFromContext(r.Context())
	if !ok || familyID == uuid.Nil {
		writeJSONProblem(w, http.StatusUnauthorized, "Unauthorized", "missing or invalid tenant family context")
		return
	}

	currentUserID, _ := auth.UserIDFromContext(r.Context())

	r.Body = http.MaxBytesReader(w, r.Body, 1024*1024)
	var body CreateAccountRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONProblem(w, http.StatusBadRequest, "Bad Request", "malformed JSON request body")
		return
	}

	ownerID := currentUserID
	if body.OwnerUserID != nil && *body.OwnerUserID != uuid.Nil {
		ownerID = *body.OwnerUserID
	}

	params := CreateAccountParams{
		BankConnectionID: body.BankConnectionID,
		OwnerUserID:      ownerID,
		ExternalID:       body.ExternalID,
		Name:             body.Name,
		Type:             body.Type,
		Currency:         body.Currency,
		CreditLimitMinor: body.CreditLimitMinor,
	}

	acc, err := h.service.CreateAccount(r.Context(), familyID, params)
	if err != nil {
		handleError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(acc)
}

// HandleListAccounts lists accounts for the authenticated family.
func (h *Handler) HandleListAccounts(w http.ResponseWriter, r *http.Request) {
	familyID, ok := auth.FamilyIDFromContext(r.Context())
	if !ok || familyID == uuid.Nil {
		writeJSONProblem(w, http.StatusUnauthorized, "Unauthorized", "missing or invalid tenant family context")
		return
	}

	includeArchived := r.URL.Query().Get("include_archived") == "true"

	accounts, err := h.service.ListAccounts(r.Context(), familyID, includeArchived)
	if err != nil {
		handleError(w, err)
		return
	}

	if accounts == nil {
		accounts = []Account{}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(accounts)
}

// HandleGetAccount retrieves an account by ID.
func (h *Handler) HandleGetAccount(w http.ResponseWriter, r *http.Request) {
	familyID, ok := auth.FamilyIDFromContext(r.Context())
	if !ok || familyID == uuid.Nil {
		writeJSONProblem(w, http.StatusUnauthorized, "Unauthorized", "missing or invalid tenant family context")
		return
	}

	accountID, err := extractUUID(r, "id")
	if err != nil {
		writeJSONProblem(w, http.StatusBadRequest, "Bad Request", "invalid account UUID format")
		return
	}

	acc, err := h.service.GetAccount(r.Context(), familyID, accountID)
	if err != nil {
		handleError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(acc)
}

// HandleUpdateAccount updates mutable fields of an account.
func (h *Handler) HandleUpdateAccount(w http.ResponseWriter, r *http.Request) {
	familyID, ok := auth.FamilyIDFromContext(r.Context())
	if !ok || familyID == uuid.Nil {
		writeJSONProblem(w, http.StatusUnauthorized, "Unauthorized", "missing or invalid tenant family context")
		return
	}

	accountID, err := extractUUID(r, "id")
	if err != nil {
		writeJSONProblem(w, http.StatusBadRequest, "Bad Request", "invalid account UUID format")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1024*1024)
	var params UpdateAccountParams
	if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
		writeJSONProblem(w, http.StatusBadRequest, "Bad Request", "malformed JSON request body")
		return
	}

	acc, err := h.service.UpdateAccount(r.Context(), familyID, accountID, params)
	if err != nil {
		handleError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(acc)
}

// HandleArchiveAccount marks an account as archived.
func (h *Handler) HandleArchiveAccount(w http.ResponseWriter, r *http.Request) {
	familyID, ok := auth.FamilyIDFromContext(r.Context())
	if !ok || familyID == uuid.Nil {
		writeJSONProblem(w, http.StatusUnauthorized, "Unauthorized", "missing or invalid tenant family context")
		return
	}

	accountID, err := extractUUID(r, "id")
	if err != nil {
		writeJSONProblem(w, http.StatusBadRequest, "Bad Request", "invalid account UUID format")
		return
	}

	if err := h.service.ArchiveAccount(r.Context(), familyID, accountID); err != nil {
		handleError(w, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// HandleGetAccountBalance retrieves the balance breakdown for an account.
func (h *Handler) HandleGetAccountBalance(w http.ResponseWriter, r *http.Request) {
	familyID, ok := auth.FamilyIDFromContext(r.Context())
	if !ok || familyID == uuid.Nil {
		writeJSONProblem(w, http.StatusUnauthorized, "Unauthorized", "missing or invalid tenant family context")
		return
	}

	accountID, err := extractUUID(r, "id")
	if err != nil {
		writeJSONProblem(w, http.StatusBadRequest, "Bad Request", "invalid account UUID format")
		return
	}

	balance, err := h.service.GetBalance(r.Context(), familyID, accountID)
	if err != nil {
		handleError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(balance)
}

// CreateCardRequestBody represents the JSON payload to create a card.
type CreateCardRequestBody struct {
	OwnerUserID   *uuid.UUID    `json:"owner_user_id,omitempty"`
	ExternalID    *string       `json:"external_id,omitempty"`
	MaskedPAN     string        `json:"masked_pan"`
	DisplayName   string        `json:"display_name"`
	PaymentSystem PaymentSystem `json:"payment_system"`
	IsVirtual     bool          `json:"is_virtual"`
	IsPrimary     bool          `json:"is_primary"`
}

// HandleCreateCard creates a new card linked to an account.
func (h *Handler) HandleCreateCard(w http.ResponseWriter, r *http.Request) {
	familyID, ok := auth.FamilyIDFromContext(r.Context())
	if !ok || familyID == uuid.Nil {
		writeJSONProblem(w, http.StatusUnauthorized, "Unauthorized", "missing or invalid tenant family context")
		return
	}

	currentUserID, _ := auth.UserIDFromContext(r.Context())

	accountID, err := extractUUID(r, "id")
	if err != nil {
		writeJSONProblem(w, http.StatusBadRequest, "Bad Request", "invalid account UUID format")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1024*1024)
	var body CreateCardRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONProblem(w, http.StatusBadRequest, "Bad Request", "malformed JSON request body")
		return
	}

	ownerID := currentUserID
	if body.OwnerUserID != nil && *body.OwnerUserID != uuid.Nil {
		ownerID = *body.OwnerUserID
	}

	params := CreateCardParams{
		AccountID:     accountID,
		OwnerUserID:   ownerID,
		ExternalID:    body.ExternalID,
		MaskedPAN:     body.MaskedPAN,
		DisplayName:   body.DisplayName,
		PaymentSystem: body.PaymentSystem,
		IsVirtual:     body.IsVirtual,
		IsPrimary:     body.IsPrimary,
	}

	card, err := h.service.CreateCard(r.Context(), familyID, params)
	if err != nil {
		handleError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(card)
}

// HandleListAccountCards lists cards linked to an account.
func (h *Handler) HandleListAccountCards(w http.ResponseWriter, r *http.Request) {
	familyID, ok := auth.FamilyIDFromContext(r.Context())
	if !ok || familyID == uuid.Nil {
		writeJSONProblem(w, http.StatusUnauthorized, "Unauthorized", "missing or invalid tenant family context")
		return
	}

	accountID, err := extractUUID(r, "id")
	if err != nil {
		writeJSONProblem(w, http.StatusBadRequest, "Bad Request", "invalid account UUID format")
		return
	}

	cards, err := h.service.ListCards(r.Context(), familyID, accountID)
	if err != nil {
		handleError(w, err)
		return
	}

	if cards == nil {
		cards = []Card{}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(cards)
}

// HandleGetCard retrieves a single card by ID.
func (h *Handler) HandleGetCard(w http.ResponseWriter, r *http.Request) {
	familyID, ok := auth.FamilyIDFromContext(r.Context())
	if !ok || familyID == uuid.Nil {
		writeJSONProblem(w, http.StatusUnauthorized, "Unauthorized", "missing or invalid tenant family context")
		return
	}

	cardID, err := extractUUID(r, "id")
	if err != nil {
		writeJSONProblem(w, http.StatusBadRequest, "Bad Request", "invalid card UUID format")
		return
	}

	card, err := h.service.GetCard(r.Context(), familyID, cardID)
	if err != nil {
		handleError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(card)
}

// HandleUpdateCard updates editable card attributes.
func (h *Handler) HandleUpdateCard(w http.ResponseWriter, r *http.Request) {
	familyID, ok := auth.FamilyIDFromContext(r.Context())
	if !ok || familyID == uuid.Nil {
		writeJSONProblem(w, http.StatusUnauthorized, "Unauthorized", "missing or invalid tenant family context")
		return
	}

	cardID, err := extractUUID(r, "id")
	if err != nil {
		writeJSONProblem(w, http.StatusBadRequest, "Bad Request", "invalid card UUID format")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1024*1024)
	var params UpdateCardParams
	if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
		writeJSONProblem(w, http.StatusBadRequest, "Bad Request", "malformed JSON request body")
		return
	}

	card, err := h.service.UpdateCard(r.Context(), familyID, cardID, params)
	if err != nil {
		handleError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(card)
}

// HandleCloseCard transitions a card to CLOSED status.
func (h *Handler) HandleCloseCard(w http.ResponseWriter, r *http.Request) {
	familyID, ok := auth.FamilyIDFromContext(r.Context())
	if !ok || familyID == uuid.Nil {
		writeJSONProblem(w, http.StatusUnauthorized, "Unauthorized", "missing or invalid tenant family context")
		return
	}

	cardID, err := extractUUID(r, "id")
	if err != nil {
		writeJSONProblem(w, http.StatusBadRequest, "Bad Request", "invalid card UUID format")
		return
	}

	if err := h.service.CloseCard(r.Context(), familyID, cardID); err != nil {
		handleError(w, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func extractUUID(r *http.Request, paramName string) (uuid.UUID, error) {
	val := r.PathValue(paramName)
	if val == "" {
		// Fallback for custom routing setups
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) > 0 {
			val = parts[len(parts)-1]
			// If ends in sub-resource like /balance or /archive or /cards
			if (val == "balance" || val == "archive" || val == "cards" || val == "close") && len(parts) > 1 {
				val = parts[len(parts)-2]
			}
		}
	}
	return uuid.Parse(val)
}

func handleError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrUnauthorized):
		writeJSONProblem(w, http.StatusUnauthorized, "Unauthorized", err.Error())
	case errors.Is(err, ErrAccountNotFound), errors.Is(err, ErrCardNotFound):
		writeJSONProblem(w, http.StatusNotFound, "Not Found", err.Error())
	case errors.Is(err, ErrAccountArchived), errors.Is(err, ErrCardClosed):
		writeJSONProblem(w, http.StatusConflict, "Conflict", err.Error())
	case errors.Is(err, ErrHasFinancialHistory):
		writeJSONProblem(w, http.StatusConflict, "Conflict", err.Error())
	case errors.Is(err, ErrInvalidAccountName),
		errors.Is(err, ErrInvalidAccountType),
		errors.Is(err, ErrInvalidCardName),
		errors.Is(err, ErrInvalidCardStatus),
		errors.Is(err, ErrInvalidPaymentSystem),
		errors.Is(err, ErrInvalidMaskedPAN),
		errors.Is(err, ErrInvalidOwner),
		errors.Is(err, ErrCurrencyMismatch),
		errors.Is(err, domain.ErrInvalidCurrency),
		errors.Is(err, domain.ErrUnsupportedCurrency):
		writeJSONProblem(w, http.StatusBadRequest, "Bad Request", err.Error())
	default:
		writeJSONProblem(w, http.StatusInternalServerError, "Internal Server Error", "an unexpected error occurred")
	}
}

func writeJSONProblem(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"type":   "about:blank",
		"title":  title,
		"status": status,
		"detail": detail,
	})
}

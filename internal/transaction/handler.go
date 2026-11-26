package transaction

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/khaliullov/family-finance/internal/auth"
	"github.com/khaliullov/family-finance/internal/connector"
	"github.com/khaliullov/family-finance/internal/connector/tbank/personal"
	"github.com/khaliullov/family-finance/internal/database"
	"github.com/khaliullov/family-finance/internal/queue"
)

// Handler exposes HTTP REST endpoints for bank connections, authorization, and validation.
type Handler struct {
	pool      *pgxpool.Pool
	service   *Service
	registry  *connector.Registry
	validator *Validator
	syncGate  *SyncGate
	cryptoKey []byte
}

// NewHandler creates a new Connection/Transaction HTTP Handler.
func NewHandler(pool *pgxpool.Pool, service *Service) *Handler {
	return &Handler{
		pool:     pool,
		service:  service,
		syncGate: NewSyncGate(pool),
	}
}

// SetRegistry configures the connector registry.
func (h *Handler) SetRegistry(reg *connector.Registry) {
	h.registry = reg
}

// SetValidator sets an explicit validator instance.
func (h *Handler) SetValidator(v *Validator) {
	h.validator = v
}

// SetSyncGate sets an explicit SyncGate instance.
func (h *Handler) SetSyncGate(g *SyncGate) {
	h.syncGate = g
}

// SetCryptoKey configures the 32-byte master encryption key.
func (h *Handler) SetCryptoKey(k []byte) {
	h.cryptoKey = k
}

func (h *Handler) getSyncGate() *SyncGate {
	if h.syncGate != nil {
		return h.syncGate
	}
	if h.service != nil {
		return h.service.SyncGate()
	}
	h.syncGate = NewSyncGate(h.pool)
	return h.syncGate
}

func (h *Handler) getRegistry() *connector.Registry {
	if h.registry != nil {
		return h.registry
	}
	if h.service != nil && h.service.registry != nil {
		return h.service.registry
	}
	return nil
}

func (h *Handler) getValidator() *Validator {
	if h.validator != nil {
		return h.validator
	}
	if h.service != nil && h.service.validator != nil {
		return h.service.validator
	}
	h.validator = NewValidator(h.pool, h.getRegistry(), h.cryptoKey)
	return h.validator
}

// RegisterRoutes registers connection endpoints on the provided ServeMux.
func (h *Handler) RegisterRoutes(mux *http.ServeMux, authMw func(http.Handler) http.Handler) {
	// Web UI endpoints (accessible directly in web browser)
	mux.Handle("GET /web/connections", http.HandlerFunc(h.HandleServeUI))
	mux.Handle("GET /connections/ui", http.HandlerFunc(h.HandleServeUI))
	mux.Handle("GET /api/v1/connections/ui", http.HandlerFunc(h.HandleServeUI))

	register := func(prefix string) {
		mux.Handle("GET "+prefix+"/connections/providers", authMw(http.HandlerFunc(h.HandleListProviders)))
		mux.Handle("GET "+prefix+"/connections", authMw(http.HandlerFunc(h.HandleListConnections)))
		mux.Handle("POST "+prefix+"/connections", authMw(http.HandlerFunc(h.HandleCreateConnection)))
		mux.Handle("GET "+prefix+"/connections/{id}", authMw(http.HandlerFunc(h.HandleGetConnection)))
		mux.Handle("DELETE "+prefix+"/connections/{id}", authMw(http.HandlerFunc(h.HandleDisconnectConnection)))

		mux.Handle("POST "+prefix+"/connections/{id}/auth/start", authMw(http.HandlerFunc(h.HandleAuthStart)))
		mux.Handle("GET "+prefix+"/connections/{id}/auth/status", authMw(http.HandlerFunc(h.HandleAuthStatus)))
		mux.Handle("POST "+prefix+"/connections/{id}/auth/session", authMw(http.HandlerFunc(h.HandleAuthSession)))
		mux.Handle("POST "+prefix+"/connections/{id}/auth/cancel", authMw(http.HandlerFunc(h.HandleAuthCancel)))

		mux.Handle("POST "+prefix+"/connections/{id}/validate", authMw(http.HandlerFunc(h.HandleTriggerValidation)))
		mux.Handle("GET "+prefix+"/connections/{id}/validation/status", authMw(http.HandlerFunc(h.HandleValidationStatus)))
		mux.Handle("GET "+prefix+"/connections/{id}/validation/reports", authMw(http.HandlerFunc(h.HandleListValidationReports)))
		mux.Handle("GET "+prefix+"/connections/{id}/validation/reports/{report_id}", authMw(http.HandlerFunc(h.HandleGetValidationReport)))

		mux.Handle("POST "+prefix+"/connections/{id}/reconnect", authMw(http.HandlerFunc(h.HandleReconnect)))
		mux.Handle("POST "+prefix+"/connections/{id}/sync", authMw(http.HandlerFunc(h.HandleTriggerSync)))
	}

	register("/api/v1")
	register("")
}

// ProviderInfo describes a supported banking connector provider.
type ProviderInfo struct {
	Code                 string   `json:"code"`
	Name                 string   `json:"name"`
	Description          string   `json:"description"`
	AuthType             string   `json:"auth_type"`
	Status               string   `json:"status"`
	SupportedInstruments []string `json:"supported_instruments"`
	AuthInstructions     string   `json:"auth_instructions"`
}

// HandleListProviders returns the catalog of supported bank connector providers.
func (h *Handler) HandleListProviders(w http.ResponseWriter, r *http.Request) {
	providers := []ProviderInfo{
		{
			Code:                 "tbank_personal",
			Name:                 "T-Bank Personal",
			Description:          "Personal retail accounts (T-Bank Black, Platinum, Savings), cards, balances, and transactions",
			AuthType:             "session_handoff",
			Status:               "ready",
			SupportedInstruments: []string{"accounts", "cards", "balances", "transactions"},
			AuthInstructions:     "Authenticate in your browser with persistent profile (e.g. via tbank-capture or web session) and paste session cookies or export JSON.",
		},
		{
			Code:                 "fixture",
			Name:                 "Test / Fixture Bank",
			Description:          "Deterministic mock banking connector for testing and automated validation",
			AuthType:             "simulated",
			Status:               "ready",
			SupportedInstruments: []string{"accounts", "cards", "balances", "transactions"},
			AuthInstructions:     "Enter simulated credentials (e.g. 'valid_session' or leave blank).",
		},
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(providers)
}

// ConnectionResponse represents a bank connection summary.
type ConnectionResponse struct {
	ID                     uuid.UUID  `json:"id"`
	Provider               string     `json:"provider"`
	Name                   string     `json:"name"`
	Status                 string     `json:"status"`
	LastSyncedAt           *time.Time `json:"last_synced_at,omitempty"`
	LastAuthenticatedAt    *time.Time `json:"last_authenticated_at,omitempty"`
	LastValidatedAt        *time.Time `json:"last_validated_at,omitempty"`
	LastValidationStatus   *string    `json:"last_validation_status,omitempty"`
	LastValidationReportID *uuid.UUID `json:"last_validation_report_id,omitempty"`
	ErrorMessage           *string    `json:"error_message,omitempty"`
	CreatedAt              time.Time  `json:"created_at"`
	UpdatedAt              time.Time  `json:"updated_at"`
}

// HandleListConnections lists all connections for the family.
func (h *Handler) HandleListConnections(w http.ResponseWriter, r *http.Request) {
	familyID, ok := auth.FamilyIDFromContext(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "Unauthorized", "Missing active family context")
		return
	}

	var connections []ConnectionResponse
	err := database.WithFamilyTx(r.Context(), h.pool, familyID, func(tx pgx.Tx) error {
		rows, err := tx.Query(r.Context(), `
			SELECT id, provider, name, status, last_synced_at, last_authenticated_at,
			       last_validated_at, last_validation_status, last_validation_report_id,
			       error_message, created_at, updated_at
			FROM public.bank_connections
			WHERE family_id = $1
			ORDER BY created_at ASC
		`, familyID)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var c ConnectionResponse
			err := rows.Scan(
				&c.ID, &c.Provider, &c.Name, &c.Status, &c.LastSyncedAt, &c.LastAuthenticatedAt,
				&c.LastValidatedAt, &c.LastValidationStatus, &c.LastValidationReportID,
				&c.ErrorMessage, &c.CreatedAt, &c.UpdatedAt,
			)
			if err != nil {
				return err
			}
			connections = append(connections, c)
		}
		return rows.Err()
	})

	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "Internal Server Error", "Failed to list bank connections")
		return
	}

	if connections == nil {
		connections = []ConnectionResponse{}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(connections)
}

// CreateConnectionRequest holds data to register a new bank connection.
type CreateConnectionRequest struct {
	Provider string `json:"provider"`
	Name     string `json:"name"`
}

// HandleCreateConnection registers a new bank connection for the family.
func (h *Handler) HandleCreateConnection(w http.ResponseWriter, r *http.Request) {
	familyID, ok := auth.FamilyIDFromContext(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "Unauthorized", "Missing active family context")
		return
	}
	userID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "Unauthorized", "Missing active user context")
		return
	}

	var reqBody CreateConnectionRequest
	if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
		writeProblem(w, http.StatusBadRequest, "Bad Request", "Malformed JSON request body")
		return
	}

	provider := strings.TrimSpace(reqBody.Provider)
	name := strings.TrimSpace(reqBody.Name)
	if provider == "" || name == "" {
		writeProblem(w, http.StatusBadRequest, "Bad Request", "provider and name are mandatory")
		return
	}

	// Validate provider
	switch provider {
	case "tbank_personal", "tbank", "fixture":
		// valid supported provider
	default:
		writeProblem(w, http.StatusBadRequest, "Bad Request", fmt.Sprintf("Unsupported provider code: %s", provider))
		return
	}

	newID := uuid.New()
	initialStatus := ConnectionStateCreated
	now := time.Now().UTC()

	err := database.WithFamilyTx(r.Context(), h.pool, familyID, func(tx pgx.Tx) error {
		_, err := tx.Exec(r.Context(), `
			INSERT INTO public.bank_connections (
				id, family_id, authorized_by_user_id, provider, name, status, created_at, updated_at
			) VALUES (
				$1, $2, $3, $4, $5, $6, $7, $7
			)
		`, newID, familyID, userID, provider, name, initialStatus, now)
		return err
	})
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "Internal Server Error", "Failed to create connection")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(ConnectionResponse{
		ID:        newID,
		Provider:  provider,
		Name:      name,
		Status:    initialStatus,
		CreatedAt: now,
		UpdatedAt: now,
	})
}

// HandleGetConnection retrieves details for a single bank connection.
func (h *Handler) HandleGetConnection(w http.ResponseWriter, r *http.Request) {
	familyID, ok := auth.FamilyIDFromContext(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "Unauthorized", "Missing active family context")
		return
	}

	connID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "Bad Request", "Invalid connection ID format")
		return
	}

	var c ConnectionResponse
	err = database.WithFamilyTx(r.Context(), h.pool, familyID, func(tx pgx.Tx) error {
		return tx.QueryRow(r.Context(), `
			SELECT id, provider, name, status, last_synced_at, last_authenticated_at,
			       last_validated_at, last_validation_status, last_validation_report_id,
			       error_message, created_at, updated_at
			FROM public.bank_connections
			WHERE family_id = $1 AND id = $2
		`, familyID, connID).Scan(
			&c.ID, &c.Provider, &c.Name, &c.Status, &c.LastSyncedAt, &c.LastAuthenticatedAt,
			&c.LastValidatedAt, &c.LastValidationStatus, &c.LastValidationReportID,
			&c.ErrorMessage, &c.CreatedAt, &c.UpdatedAt,
		)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeProblem(w, http.StatusNotFound, "Not Found", "Bank connection not found")
			return
		}
		writeProblem(w, http.StatusInternalServerError, "Internal Server Error", "Failed to query connection")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(c)
}

// HandleDisconnectConnection transitions the connection to disconnected and clears credentials.
func (h *Handler) HandleDisconnectConnection(w http.ResponseWriter, r *http.Request) {
	familyID, ok := auth.FamilyIDFromContext(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "Unauthorized", "Missing active family context")
		return
	}

	connID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "Bad Request", "Invalid connection ID format")
		return
	}

	var currentStatus string
	err = database.WithFamilyTx(r.Context(), h.pool, familyID, func(tx pgx.Tx) error {
		err := tx.QueryRow(r.Context(), `
			SELECT status FROM public.bank_connections
			WHERE family_id = $1 AND id = $2
		`, familyID, connID).Scan(&currentStatus)
		if err != nil {
			return err
		}

		if err := ValidateStateTransition(currentStatus, ConnectionStateDisconnected); err != nil {
			return err
		}

		_, err = tx.Exec(r.Context(), `
			UPDATE public.bank_connections
			SET status = $1,
			    encrypted_credentials = NULL,
			    credential_hash = NULL,
			    active_sync_token = NULL,
			    sync_locked_until = NULL,
			    error_message = NULL,
			    updated_at = NOW()
			WHERE family_id = $2 AND id = $3
		`, ConnectionStateDisconnected, familyID, connID)
		return err
	})

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeProblem(w, http.StatusNotFound, "Not Found", "Bank connection not found")
			return
		}
		writeProblem(w, http.StatusBadRequest, "Bad Request", fmt.Sprintf("Failed to disconnect: %v", err))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":     connID,
		"status": ConnectionStateDisconnected,
	})
}

// HandleAuthStart transitions connection to authenticating and provides instructions.
func (h *Handler) HandleAuthStart(w http.ResponseWriter, r *http.Request) {
	familyID, ok := auth.FamilyIDFromContext(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "Unauthorized", "Missing active family context")
		return
	}

	connID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "Bad Request", "Invalid connection ID format")
		return
	}

	var provider, currentStatus string
	err = database.WithFamilyTx(r.Context(), h.pool, familyID, func(tx pgx.Tx) error {
		err := tx.QueryRow(r.Context(), `
			SELECT provider, status FROM public.bank_connections
			WHERE family_id = $1 AND id = $2
		`, familyID, connID).Scan(&provider, &currentStatus)
		if err != nil {
			return err
		}

		if currentStatus == ConnectionStateDisconnected {
			return errors.New("disconnected connections are terminal and cannot be reconnected. Please create a new bank connection")
		}

		if err := ValidateStateTransition(currentStatus, ConnectionStateAuthenticating); err != nil {
			return err
		}

		_, err = tx.Exec(r.Context(), `
			UPDATE public.bank_connections
			SET status = $1,
			    encrypted_credentials = NULL,
			    credential_hash = NULL,
			    session_version = session_version + 1,
			    last_validated_at = NULL,
			    last_validation_status = NULL,
			    last_validation_report_id = NULL,
			    error_message = NULL,
			    updated_at = NOW()
			WHERE family_id = $2 AND id = $3
		`, ConnectionStateAuthenticating, familyID, connID)
		return err
	})

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeProblem(w, http.StatusNotFound, "Not Found", "Bank connection not found")
			return
		}
		writeProblem(w, http.StatusConflict, "Conflict", fmt.Sprintf("Cannot start authentication: %v", err))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"connection_id": connID,
		"provider":      provider,
		"status":        ConnectionStateAuthenticating,
		"instructions":  "Authenticate in your browser using persistent profile (or tbank-capture) and submit session cookies to auth/session endpoint.",
		"next_step":     fmt.Sprintf("POST /api/v1/connections/%s/auth/session", connID),
	})
}

// HandleAuthStatus checks the current authentication status of the connection.
func (h *Handler) HandleAuthStatus(w http.ResponseWriter, r *http.Request) {
	familyID, ok := auth.FamilyIDFromContext(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "Unauthorized", "Missing active family context")
		return
	}

	connID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "Bad Request", "Invalid connection ID format")
		return
	}

	var provider, status string
	var lastAuth *time.Time
	var errMsg *string
	err = database.WithFamilyTx(r.Context(), h.pool, familyID, func(tx pgx.Tx) error {
		return tx.QueryRow(r.Context(), `
			SELECT provider, status, last_authenticated_at, error_message
			FROM public.bank_connections
			WHERE family_id = $1 AND id = $2
		`, familyID, connID).Scan(&provider, &status, &lastAuth, &errMsg)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeProblem(w, http.StatusNotFound, "Not Found", "Bank connection not found")
			return
		}
		writeProblem(w, http.StatusInternalServerError, "Internal Server Error", "Failed to query connection auth status")
		return
	}

	isAuthenticated := (status == ConnectionStateConnected || status == ConnectionStateValidationPassed || status == ConnectionStateActive)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"connection_id":         connID,
		"provider":              provider,
		"status":                status,
		"is_authenticated":      isAuthenticated,
		"last_authenticated_at": lastAuth,
		"error_message":         errMsg,
	})
}

// AuthSessionRequest carries session handoff payload.
type AuthSessionRequest struct {
	Cookies     []personal.SerializableCookie `json:"cookies,omitempty"`
	SessionData string                        `json:"session_data,omitempty"`
	Credentials string                        `json:"credentials,omitempty"`
}

// HandleAuthSession handles session seeding, runs health check probe, and activates connection.
func (h *Handler) HandleAuthSession(w http.ResponseWriter, r *http.Request) {
	familyID, ok := auth.FamilyIDFromContext(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "Unauthorized", "Missing active family context")
		return
	}

	connID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "Bad Request", "Invalid connection ID format")
		return
	}

	var provider, currentStatus string
	err = database.WithFamilyTx(r.Context(), h.pool, familyID, func(tx pgx.Tx) error {
		return tx.QueryRow(r.Context(), `
			SELECT provider, status FROM public.bank_connections
			WHERE family_id = $1 AND id = $2
		`, familyID, connID).Scan(&provider, &currentStatus)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeProblem(w, http.StatusNotFound, "Not Found", "Bank connection not found")
			return
		}
		writeProblem(w, http.StatusInternalServerError, "Internal Server Error", "Failed to query connection")
		return
	}

	if err := ValidateStateTransition(currentStatus, ConnectionStateConnected); err != nil {
		writeProblem(w, http.StatusConflict, "Conflict", fmt.Sprintf("Cannot transition to connected: %v", err))
		return
	}

	// Limit request body to 64KB
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)

	var reqBody AuthSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
		writeProblem(w, http.StatusBadRequest, "Bad Request", "Malformed JSON request body or payload exceeds size limit")
		return
	}

	if provider != "tbank_personal" && provider != "tbank" && provider != "fixture" && provider != "mock" {
		writeProblem(w, http.StatusBadRequest, "Bad Request", fmt.Sprintf("Unsupported bank provider: %s", provider))
		return
	}

	var encryptedCredentials []byte

	if provider == "tbank_personal" || provider == "tbank" {
		sess := personal.NewSession(familyID, connID, nil, 30*24*time.Hour)
		if len(reqBody.Cookies) > 0 {
			sess.Cookies = reqBody.Cookies
		} else if reqBody.SessionData != "" {
			var parsedCookies []personal.SerializableCookie
			if err := json.Unmarshal([]byte(reqBody.SessionData), &parsedCookies); err == nil && len(parsedCookies) > 0 {
				sess.Cookies = parsedCookies
			} else {
				var parsedSess personal.Session
				if err := json.Unmarshal([]byte(reqBody.SessionData), &parsedSess); err == nil && len(parsedSess.Cookies) > 0 {
					sess = &parsedSess
					sess.FamilyID = familyID
					sess.ConnectionID = connID
				} else {
					writeProblem(w, http.StatusUnprocessableEntity, "Unprocessable Entity", "Session payload must contain valid cookies array")
					return
				}
			}
		} else {
			writeProblem(w, http.StatusUnprocessableEntity, "Unprocessable Entity", "Session cookies must be provided for T-Bank Personal connection")
			return
		}

		// Strictly validate cookie contents and enforce required tokens
		hasAuthToken := false
		for _, c := range sess.Cookies {
			name := strings.TrimSpace(c.Name)
			val := strings.TrimSpace(c.Value)
			if name == "" || len(name) > 128 || len(val) > 4096 {
				writeProblem(w, http.StatusUnprocessableEntity, "Unprocessable Entity", "Invalid cookie name or length exceeds security boundary")
				return
			}
			lower := strings.ToLower(name)
			if lower == "psid" || lower == "wses" || lower == "sessionid" {
				hasAuthToken = true
			}
		}
		if !hasAuthToken {
			writeProblem(w, http.StatusUnprocessableEntity, "Unprocessable Entity", "Session cookies missing required authenticated token (e.g. psid, wses)")
			return
		}

		if len(h.cryptoKey) == 32 {
			enc, err := sess.Encrypt(h.cryptoKey)
			if err != nil {
				writeProblem(w, http.StatusInternalServerError, "Internal Server Error", "Failed to encrypt banking session envelope")
				return
			}
			encryptedCredentials = enc
		} else {
			// Fallback plain JSON if crypto key is not configured
			rawJSON, _ := json.Marshal(sess)
			encryptedCredentials = rawJSON
		}
	} else {
		// Fixture or mock provider
		credStr := reqBody.Credentials
		if credStr == "" {
			credStr = reqBody.SessionData
		}
		if credStr == "" {
			credStr = "valid_session"
		}
		encryptedCredentials = []byte(credStr)
	}

	// Compute SHA-256 fingerprint of stored credentials to detect session replacement
	hashBytes := sha256.Sum256(encryptedCredentials)
	credHash := hex.EncodeToString(hashBytes[:])

	// Run HealthCheck probe against connector if available in registry
	reg := h.getRegistry()
	if reg != nil {
		providerCode := provider
		if providerCode == "tbank_personal" {
			providerCode = "tbank"
		}
		connAdapter, err := reg.Get(providerCode)
		if err == nil && connAdapter != nil {
			if probeErr := connAdapter.HealthCheck(r.Context(), encryptedCredentials); probeErr != nil {
				writeProblem(w, http.StatusUnprocessableEntity, "Session Verification Failed", fmt.Sprintf("Banking session probe rejected: %v", probeErr))
				return
			}
		}
	}

	// Update DB: transition to connected, store encrypted credentials, increment session_version,
	// invalidate previous validation reports, and update last_authenticated_at
	now := time.Now().UTC()
	err = database.WithFamilyTx(r.Context(), h.pool, familyID, func(tx pgx.Tx) error {
		_, err := tx.Exec(r.Context(), `
			UPDATE public.bank_connections
			SET status = $1,
			    encrypted_credentials = $2,
			    session_version = session_version + 1,
			    credential_hash = $3,
			    last_authenticated_at = $4,
			    last_validated_at = NULL,
			    last_validation_status = NULL,
			    last_validation_report_id = NULL,
			    error_message = NULL,
			    updated_at = $4
			WHERE family_id = $5 AND id = $6
		`, ConnectionStateConnected, encryptedCredentials, credHash, now, familyID, connID)
		return err
	})
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "Internal Server Error", "Failed to activate bank connection session")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"connection_id": connID,
		"status":        ConnectionStateConnected,
		"message":       "Banking session successfully verified and activated",
	})
}

// HandleAuthCancel cancels an ongoing authentication attempt and returns to created.
func (h *Handler) HandleAuthCancel(w http.ResponseWriter, r *http.Request) {
	familyID, ok := auth.FamilyIDFromContext(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "Unauthorized", "Missing active family context")
		return
	}

	connID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "Bad Request", "Invalid connection ID format")
		return
	}

	var currentStatus string
	err = database.WithFamilyTx(r.Context(), h.pool, familyID, func(tx pgx.Tx) error {
		err := tx.QueryRow(r.Context(), `
			SELECT status FROM public.bank_connections
			WHERE family_id = $1 AND id = $2
		`, familyID, connID).Scan(&currentStatus)
		if err != nil {
			return err
		}

		if err := ValidateStateTransition(currentStatus, ConnectionStateCreated); err != nil {
			return err
		}

		_, err = tx.Exec(r.Context(), `
			UPDATE public.bank_connections
			SET status = $1,
			    encrypted_credentials = NULL,
			    credential_hash = NULL,
			    last_validated_at = NULL,
			    last_validation_status = NULL,
			    last_validation_report_id = NULL,
			    error_message = NULL,
			    updated_at = NOW()
			WHERE family_id = $2 AND id = $3
		`, ConnectionStateCreated, familyID, connID)
		return err
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeProblem(w, http.StatusNotFound, "Not Found", "Bank connection not found")
			return
		}
		writeProblem(w, http.StatusConflict, "Conflict", fmt.Sprintf("Cannot cancel authentication: %v", err))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"connection_id": connID,
		"status":        ConnectionStateCreated,
	})
}

// HandleTriggerValidation triggers read-only validation of the bank connection.
func (h *Handler) HandleTriggerValidation(w http.ResponseWriter, r *http.Request) {
	familyID, ok := auth.FamilyIDFromContext(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "Unauthorized", "Missing active family context")
		return
	}

	connID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "Bad Request", "Invalid connection ID format")
		return
	}

	var currentStatus string
	err = database.WithFamilyTx(r.Context(), h.pool, familyID, func(tx pgx.Tx) error {
		return tx.QueryRow(r.Context(), `
			SELECT status FROM public.bank_connections
			WHERE family_id = $1 AND id = $2
		`, familyID, connID).Scan(&currentStatus)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeProblem(w, http.StatusNotFound, "Not Found", "Bank connection not found")
			return
		}
		writeProblem(w, http.StatusInternalServerError, "Internal Server Error", "Failed to query connection")
		return
	}

	if err := CanValidateConnection(currentStatus); err != nil {
		writeProblem(w, http.StatusConflict, "Conflict", fmt.Sprintf("Cannot validate: %v", err))
		return
	}

	// Check if synchronous execution was requested
	isSync := r.URL.Query().Get("sync") == "true"
	if isSync {
		validator := h.getValidator()
		report, err := validator.ValidateConnection(r.Context(), familyID, connID)
		if err != nil {
			writeProblem(w, http.StatusInternalServerError, "Internal Server Error", fmt.Sprintf("Validation failed: %v", err))
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(report)
		return
	}

	// Atomic CAS update: transition to validation_pending for asynchronous queue job
	err = database.WithFamilyTx(r.Context(), h.pool, familyID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(r.Context(), `
			UPDATE public.bank_connections
			SET status = $1,
			    updated_at = NOW()
			WHERE family_id = $2 AND id = $3 AND status = $4
		`, ConnectionStateValidationPending, familyID, connID, currentStatus)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errors.New("concurrent state modification")
		}
		return nil
	})
	if err != nil {
		writeProblem(w, http.StatusConflict, "Conflict", fmt.Sprintf("Failed to initiate validation: %v", err))
		return
	}

	// Asynchronous queue job
	jobID, err := queue.EnqueueValidationJob(r.Context(), h.pool, familyID, queue.ValidationJobPayload{
		ConnectionID: connID,
	})
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "Internal Server Error", "Failed to enqueue validation job")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"job_id":        jobID,
		"status":        ConnectionStateValidationPending,
		"connection_id": connID,
	})
}

// HandleValidationStatus returns validation status and summary for a connection.
func (h *Handler) HandleValidationStatus(w http.ResponseWriter, r *http.Request) {
	familyID, ok := auth.FamilyIDFromContext(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "Unauthorized", "Missing active family context")
		return
	}

	connID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "Bad Request", "Invalid connection ID format")
		return
	}

	var status string
	var lastValidated *time.Time
	var lastValStatus *string
	var lastReportID *uuid.UUID
	var errMsg *string

	err = database.WithFamilyTx(r.Context(), h.pool, familyID, func(tx pgx.Tx) error {
		return tx.QueryRow(r.Context(), `
			SELECT status, last_validated_at, last_validation_status, last_validation_report_id, error_message
			FROM public.bank_connections
			WHERE family_id = $1 AND id = $2
		`, familyID, connID).Scan(&status, &lastValidated, &lastValStatus, &lastReportID, &errMsg)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeProblem(w, http.StatusNotFound, "Not Found", "Bank connection not found")
			return
		}
		writeProblem(w, http.StatusInternalServerError, "Internal Server Error", "Failed to query validation status")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"connection_id":             connID,
		"status":                    status,
		"last_validated_at":         lastValidated,
		"last_validation_status":    lastValStatus,
		"last_validation_report_id": lastReportID,
		"error_message":             errMsg,
	})
}

// ValidationReportSummary represents a summary item in the reports list.
type ValidationReportSummary struct {
	ID            uuid.UUID `json:"id"`
	OverallResult string    `json:"overall_result"`
	StartedAt     time.Time `json:"started_at"`
	FinishedAt    time.Time `json:"finished_at"`
	DurationMs    int64     `json:"duration_ms"`
	CreatedAt     time.Time `json:"created_at"`
}

// HandleListValidationReports returns historical validation reports for a connection.
func (h *Handler) HandleListValidationReports(w http.ResponseWriter, r *http.Request) {
	familyID, ok := auth.FamilyIDFromContext(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "Unauthorized", "Missing active family context")
		return
	}

	connID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "Bad Request", "Invalid connection ID format")
		return
	}

	var reports []ValidationReportSummary
	err = database.WithFamilyTx(r.Context(), h.pool, familyID, func(tx pgx.Tx) error {
		rows, err := tx.Query(r.Context(), `
			SELECT id, overall_result, started_at, finished_at, duration_ms, created_at
			FROM public.bank_validation_reports
			WHERE family_id = $1 AND bank_connection_id = $2
			ORDER BY created_at DESC
			LIMIT 50
		`, familyID, connID)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var rep ValidationReportSummary
			if err := rows.Scan(&rep.ID, &rep.OverallResult, &rep.StartedAt, &rep.FinishedAt, &rep.DurationMs, &rep.CreatedAt); err != nil {
				return err
			}
			reports = append(reports, rep)
		}
		return rows.Err()
	})
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "Internal Server Error", "Failed to list validation reports")
		return
	}

	if reports == nil {
		reports = []ValidationReportSummary{}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(reports)
}

// HandleGetValidationReport retrieves the full detailed report JSON payload.
func (h *Handler) HandleGetValidationReport(w http.ResponseWriter, r *http.Request) {
	familyID, ok := auth.FamilyIDFromContext(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "Unauthorized", "Missing active family context")
		return
	}

	connID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "Bad Request", "Invalid connection ID format")
		return
	}

	reportID, err := uuid.Parse(r.PathValue("report_id"))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "Bad Request", "Invalid report ID format")
		return
	}

	var payload []byte
	err = database.WithFamilyTx(r.Context(), h.pool, familyID, func(tx pgx.Tx) error {
		return tx.QueryRow(r.Context(), `
			SELECT report_payload
			FROM public.bank_validation_reports
			WHERE family_id = $1 AND bank_connection_id = $2 AND id = $3
		`, familyID, connID, reportID).Scan(&payload)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeProblem(w, http.StatusNotFound, "Not Found", "Validation report not found")
			return
		}
		writeProblem(w, http.StatusInternalServerError, "Internal Server Error", "Failed to retrieve validation report")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(payload)
}

// HandleReconnect initiates reauthentication for an expired or failed connection.
func (h *Handler) HandleReconnect(w http.ResponseWriter, r *http.Request) {
	familyID, ok := auth.FamilyIDFromContext(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "Unauthorized", "Missing active family context")
		return
	}

	connID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "Bad Request", "Invalid connection ID format")
		return
	}

	var currentStatus, provider string
	err = database.WithFamilyTx(r.Context(), h.pool, familyID, func(tx pgx.Tx) error {
		err := tx.QueryRow(r.Context(), `
			SELECT provider, status FROM public.bank_connections
			WHERE family_id = $1 AND id = $2
		`, familyID, connID).Scan(&provider, &currentStatus)
		if err != nil {
			return err
		}

		if currentStatus == ConnectionStateDisconnected {
			return errors.New("disconnected connections are terminal and cannot be reconnected. Please create a new bank connection")
		}

		if err := ValidateStateTransition(currentStatus, ConnectionStateAuthenticating); err != nil {
			return err
		}

		_, err = tx.Exec(r.Context(), `
			UPDATE public.bank_connections
			SET status = $1,
			    encrypted_credentials = NULL,
			    credential_hash = NULL,
			    session_version = session_version + 1,
			    last_validated_at = NULL,
			    last_validation_status = NULL,
			    last_validation_report_id = NULL,
			    error_message = NULL,
			    updated_at = NOW()
			WHERE family_id = $2 AND id = $3
		`, ConnectionStateAuthenticating, familyID, connID)
		return err
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeProblem(w, http.StatusNotFound, "Not Found", "Bank connection not found")
			return
		}
		writeProblem(w, http.StatusConflict, "Conflict", fmt.Sprintf("Cannot reconnect: %v", err))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"connection_id": connID,
		"provider":      provider,
		"status":        ConnectionStateAuthenticating,
		"instructions":  "Session expired or reconnect requested. Please authenticate in your browser and submit updated session cookies.",
		"next_step":     fmt.Sprintf("POST /api/v1/connections/%s/auth/session", connID),
	})
}

// TriggerSyncRequestBody is the optional JSON payload for triggering sync.
type TriggerSyncRequestBody struct {
	LookbackDays  int    `json:"lookback_days"`
	Cursor        string `json:"cursor,omitempty"`
	ForceFullSync bool   `json:"force_full_sync,omitempty"`
}

// SyncJobResponse matches OpenAPI specification for triggerSync response.
type SyncJobResponse struct {
	JobID  uuid.UUID `json:"job_id"`
	Status string    `json:"status"`
}

// HandleTriggerSync enqueues an asynchronous bank synchronization job.
func (h *Handler) HandleTriggerSync(w http.ResponseWriter, r *http.Request) {
	familyID, ok := auth.FamilyIDFromContext(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "Unauthorized", "Missing active family context")
		return
	}

	connID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "Bad Request", "Invalid connection ID format")
		return
	}

	// Verify Fail-Closed Sync Gate before enqueuing synchronization
	gate := h.getSyncGate()
	_, _, err = gate.VerifyCanSync(r.Context(), familyID, connID)
	if err != nil {
		if errors.Is(err, ErrConnectionNotFound) {
			writeProblem(w, http.StatusNotFound, "Not Found", "Bank connection not found")
			return
		}
		writeProblem(w, http.StatusConflict, "Sync Gate Blocked", fmt.Sprintf("Financial synchronization blocked by safety gate: %v", err))
		return
	}

	var reqBody TriggerSyncRequestBody
	if r.Body != nil && r.ContentLength > 0 {
		_ = json.NewDecoder(r.Body).Decode(&reqBody)
	}

	lookbackDays := reqBody.LookbackDays
	if lookbackDays <= 0 {
		lookbackDays = 30
	}

	payload := queue.SyncJobPayload{
		ConnectionID:  connID,
		LookbackDays:  lookbackDays,
		Cursor:        reqBody.Cursor,
		ForceFullSync: reqBody.ForceFullSync,
	}

	jobID, err := queue.EnqueueSyncJob(r.Context(), h.pool, familyID, payload)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "Internal Server Error", "Failed to enqueue synchronization job")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(SyncJobResponse{
		JobID:  jobID,
		Status: "enqueued",
	})
}

func writeProblem(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status": status,
		"title":  title,
		"detail": detail,
	})
}

package transaction

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/khaliullov/family-finance/internal/auth"
	"github.com/khaliullov/family-finance/internal/connector"
	"github.com/khaliullov/family-finance/internal/crypto"
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
		mux.Handle("POST "+prefix+"/connections/{id}/approve-initial-import", authMw(http.HandlerFunc(h.HandleApproveInitialImport)))
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
			AuthType:             "local_automation_pending",
			Status:               "unsupported_authentication",
			SupportedInstruments: []string{"accounts", "cards", "balances", "transactions"},
			AuthInstructions:     "Automated retail session authentication is not implemented. Direct manual browser cookie/session ingestion is prohibited for security. Safe local sidecar authentication is under development.",
		},
		{
			Code:                 "fixture",
			Name:                 "Test / Fixture Bank",
			Description:          "Deterministic mock banking connector for testing and automated validation (non-production only)",
			AuthType:             "simulated",
			Status:               "development_only",
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

// RecordAuditEvent logs a structured, non-repudiation audit event to public.audit_logs.
// Sensitive data (credentials, bearer tokens, full card/account numbers) is strictly excluded.
func RecordAuditEvent(ctx context.Context, tx pgx.Tx, familyID uuid.UUID, userID *uuid.UUID, entityType string, entityID uuid.UUID, action string, oldValues, newValues any) error {
	var oldJSON, newJSON []byte
	if oldValues != nil {
		oldJSON, _ = json.Marshal(oldValues)
	}
	if newValues != nil {
		newJSON, _ = json.Marshal(newValues)
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO public.audit_logs (
			id, family_id, user_id, entity_type, entity_id, action, old_values, new_values, created_at
		) VALUES (
			gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7, NOW()
		)
	`, familyID, userID, entityType, entityID, action, oldJSON, newJSON)
	return err
}

// HandleDisconnectConnection transitions the connection to disconnected and clears credentials.
func (h *Handler) HandleDisconnectConnection(w http.ResponseWriter, r *http.Request) {
	familyID, ok := auth.FamilyIDFromContext(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "Unauthorized", "Missing active family context")
		return
	}
	userID, _ := auth.UserIDFromContext(r.Context())

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
			FOR UPDATE
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
			    fencing_token = fencing_token + 1,
			    active_operation_type = NULL,
			    active_sync_token = NULL,
			    sync_locked_until = NULL,
			    initial_import_completed_at = NULL,
			    error_message = NULL,
			    updated_at = NOW()
			WHERE family_id = $2 AND id = $3
		`, ConnectionStateDisconnected, familyID, connID)
		if err != nil {
			return err
		}

		// Invalidate any active approvals
		_, _ = tx.Exec(r.Context(), `
			UPDATE public.bank_initial_import_approvals
			SET invalidated_at = NOW(), invalidation_reason = 'connection_disconnected', updated_at = NOW()
			WHERE family_id = $1 AND bank_connection_id = $2 AND invalidated_at IS NULL AND consumed_at IS NULL
		`, familyID, connID)

		var uidPtr *uuid.UUID
		if userID != uuid.Nil {
			uidPtr = &userID
		}
		_ = RecordAuditEvent(r.Context(), tx, familyID, uidPtr, "bank_connection", connID,
			"connection_disconnected", map[string]any{"previous_status": currentStatus}, map[string]any{"status": ConnectionStateDisconnected})

		return nil
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
	userID, _ := auth.UserIDFromContext(r.Context())

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
			FOR UPDATE
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
			    fencing_token = fencing_token + 1,
			    active_operation_type = NULL,
			    active_sync_token = NULL,
			    sync_locked_until = NULL,
			    last_validated_at = NULL,
			    last_validation_status = NULL,
			    last_validation_report_id = NULL,
			    initial_import_completed_at = NULL,
			    error_message = NULL,
			    updated_at = NOW()
			WHERE family_id = $2 AND id = $3
		`, ConnectionStateAuthenticating, familyID, connID)
		if err != nil {
			return err
		}

		// Invalidate active approvals
		_, _ = tx.Exec(r.Context(), `
			UPDATE public.bank_initial_import_approvals
			SET invalidated_at = NOW(), invalidation_reason = 'auth_started', updated_at = NOW()
			WHERE family_id = $1 AND bank_connection_id = $2 AND invalidated_at IS NULL AND consumed_at IS NULL
		`, familyID, connID)

		var uidPtr *uuid.UUID
		if userID != uuid.Nil {
			uidPtr = &userID
		}
		_ = RecordAuditEvent(r.Context(), tx, familyID, uidPtr, "bank_connection", connID,
			"authentication_attempt_started", nil, map[string]any{"provider": provider})

		return nil
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
		"instructions":  "Direct manual browser cookie handoff is disabled for security. Local browser session automation is under development.",
		"next_step":     "Safe local connector",
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

// AuthSessionRequest carries activation request for test / fixture simulated providers.
// Ingestion of raw cookies, bearer tokens, or sensitive credentials is strictly prohibited.
type AuthSessionRequest struct {
	Simulated bool `json:"simulated,omitempty"`
}

// HandleAuthSession handles connection activation for supported providers.
// Raw browser cookies, bearer session tokens, and plaintext credential handoff are rejected.
func (h *Handler) HandleAuthSession(w http.ResponseWriter, r *http.Request) {
	familyID, ok := auth.FamilyIDFromContext(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "Unauthorized", "Missing active family context")
		return
	}
	userID, _ := auth.UserIDFromContext(r.Context())

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
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "Bad Request", "Malformed request body or payload exceeds size limit")
		return
	}

	// Reject raw cookies, session tokens, or bearer credential material (P0)
	if len(bodyBytes) > 0 {
		var rawMap map[string]any
		if err := json.Unmarshal(bodyBytes, &rawMap); err == nil {
			for k := range rawMap {
				lowerKey := strings.ToLower(k)
				if lowerKey == "cookies" || lowerKey == "session_data" || lowerKey == "credentials" ||
					lowerKey == "psid" || lowerKey == "wses" || lowerKey == "sessionid" ||
					lowerKey == "token" || lowerKey == "password" || lowerKey == "otp" {
					writeProblem(w, http.StatusBadRequest, "Insecure Handoff Rejected",
						"Insecure credential handoff rejected: bearer cookies and raw session tokens are not accepted by this API")
					return
				}
			}
		}
		lowerBody := strings.ToLower(string(bodyBytes))
		if strings.Contains(lowerBody, "psid") || strings.Contains(lowerBody, "wses") || strings.Contains(lowerBody, "sessionid") {
			writeProblem(w, http.StatusBadRequest, "Insecure Handoff Rejected",
				"Insecure credential handoff rejected: bearer cookies and raw session tokens are not accepted by this API")
			return
		}
	}

	// For real retail bank providers where safe automated auth is not yet available, return 501 Not Implemented (P0)
	if provider != "fixture" && provider != "mock" {
		writeProblem(w, http.StatusNotImplemented, "Not Implemented",
			fmt.Sprintf("Safe automated authentication for provider %q is not implemented. Direct manual browser cookie/session ingestion is prohibited for security.", provider))
		return
	}

	// Simulated / Fixture provider in non-production environments
	if isProductionEnvironment() {
		writeProblem(w, http.StatusForbidden, "Forbidden", "Simulated fixture authentication is strictly prohibited in production mode")
		return
	}

	// Ensure absent or invalid encryption key fails closed (P0)
	if len(h.cryptoKey) != 32 {
		writeProblem(w, http.StatusInternalServerError, "Internal Server Error",
			"Cryptographic key not configured: cannot store credentials securely")
		return
	}

	// Encrypt simulated fixture credentials using AES-256-GCM
	fixturePayload := []byte(`{"simulated":true,"provider":"` + provider + `","issued_at":"` + time.Now().UTC().Format(time.RFC3339) + `"}`)
	enc, err := crypto.Encrypt(h.cryptoKey, fixturePayload)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "Internal Server Error", "Failed to encrypt credentials")
		return
	}
	encryptedCredentials := enc

	// Compute SHA-256 fingerprint of stored encrypted credentials
	hashBytes := sha256.Sum256(encryptedCredentials)
	credHash := hex.EncodeToString(hashBytes[:])

	// Update DB: atomically transition to connected via CAS
	now := time.Now().UTC()
	var casSuccess bool
	err = database.WithFamilyTx(r.Context(), h.pool, familyID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(r.Context(), `
			UPDATE public.bank_connections
			SET status = $1,
			    encrypted_credentials = $2,
			    session_version = session_version + 1,
			    fencing_token = fencing_token + 1,
			    credential_hash = $3,
			    last_authenticated_at = $4,
			    last_validated_at = NULL,
			    last_validation_status = NULL,
			    last_validation_report_id = NULL,
			    initial_import_completed_at = NULL,
			    error_message = NULL,
			    updated_at = $4
			WHERE family_id = $5 AND id = $6
			  AND status = 'authenticating'
		`, ConnectionStateConnected, encryptedCredentials, credHash, now, familyID, connID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		casSuccess = true

		// Invalidate active approvals
		_, _ = tx.Exec(r.Context(), `
			UPDATE public.bank_initial_import_approvals
			SET invalidated_at = NOW(), invalidation_reason = 'credentials_updated', updated_at = NOW()
			WHERE family_id = $1 AND bank_connection_id = $2 AND invalidated_at IS NULL AND consumed_at IS NULL
		`, familyID, connID)

		var uidPtr *uuid.UUID
		if userID != uuid.Nil {
			uidPtr = &userID
		}
		_ = RecordAuditEvent(r.Context(), tx, familyID, uidPtr, "bank_connection", connID,
			"authentication_attempt_succeeded", nil, map[string]any{"provider": provider, "status": ConnectionStateConnected})

		return nil
	})
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "Internal Server Error", "Failed to activate bank connection session")
		return
	}
	if !casSuccess {
		writeProblem(w, http.StatusConflict, "Conflict", "Authentication CAS failed: connection state changed concurrently")
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
	userID, _ := auth.UserIDFromContext(r.Context())

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
			FOR UPDATE
		`, familyID, connID).Scan(&currentStatus)
		if err != nil {
			return err
		}

		if err := ValidateStateTransition(currentStatus, ConnectionStateCreated); err != nil {
			return err
		}

		tag, err := tx.Exec(r.Context(), `
			UPDATE public.bank_connections
			SET status = $1,
			    encrypted_credentials = NULL,
			    credential_hash = NULL,
			    fencing_token = fencing_token + 1,
			    active_operation_type = NULL,
			    active_sync_token = NULL,
			    sync_locked_until = NULL,
			    last_validated_at = NULL,
			    last_validation_status = NULL,
			    last_validation_report_id = NULL,
			    initial_import_completed_at = NULL,
			    error_message = NULL,
			    updated_at = NOW()
			WHERE family_id = $2 AND id = $3
			  AND status = 'authenticating'
		`, ConnectionStateCreated, familyID, connID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errors.New("authentication cancel CAS failed: state is no longer authenticating")
		}

		// Invalidate active approvals
		_, _ = tx.Exec(r.Context(), `
			UPDATE public.bank_initial_import_approvals
			SET invalidated_at = NOW(), invalidation_reason = 'auth_cancelled', updated_at = NOW()
			WHERE family_id = $1 AND bank_connection_id = $2 AND invalidated_at IS NULL AND consumed_at IS NULL
		`, familyID, connID)

		var uidPtr *uuid.UUID
		if userID != uuid.Nil {
			uidPtr = &userID
		}
		_ = RecordAuditEvent(r.Context(), tx, familyID, uidPtr, "bank_connection", connID,
			"authentication_attempt_cancelled", map[string]any{"previous_status": currentStatus}, map[string]any{"status": ConnectionStateCreated})

		return nil
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
			    fencing_token = fencing_token + 1,
			    active_operation_type = NULL,
			    active_sync_token = NULL,
			    sync_locked_until = NULL,
			    last_validated_at = NULL,
			    last_validation_status = NULL,
			    last_validation_report_id = NULL,
			    initial_import_completed_at = NULL,
			    error_message = NULL,
			    updated_at = NOW()
			WHERE family_id = $2 AND id = $3
		`, ConnectionStateAuthenticating, familyID, connID)
		if err != nil {
			return err
		}

		// Invalidate active approvals
		_, _ = tx.Exec(r.Context(), `
			UPDATE public.bank_initial_import_approvals
			SET invalidated_at = NOW(), invalidation_reason = 'reconnect_requested', updated_at = NOW()
			WHERE family_id = $1 AND bank_connection_id = $2 AND invalidated_at IS NULL AND consumed_at IS NULL
		`, familyID, connID)
		return nil
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
		"instructions":  "Session expired or reconnect requested. Please authenticate using the safe connector workflow.",
		"next_step":     "Safe local connector",
	})
}

// HandleApproveInitialImport explicitly authorizes initial financial import for a validated connection.
func (h *Handler) HandleApproveInitialImport(w http.ResponseWriter, r *http.Request) {
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

	connID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "Bad Request", "Invalid connection ID format")
		return
	}

	var approval InitialImportApproval
	err = database.WithFamilyTx(r.Context(), h.pool, familyID, func(tx pgx.Tx) error {
		var conn BankConnectionEntity
		var credHash, lastValStatus *string
		var lastValReportID *uuid.UUID
		var initialImportCompletedAt *time.Time

		err := tx.QueryRow(r.Context(), `
			SELECT id, family_id, provider, status, session_version, credential_hash,
			       last_validation_status, last_validation_report_id, initial_import_completed_at
			FROM public.bank_connections
			WHERE family_id = $1 AND id = $2
			FOR UPDATE
		`, familyID, connID).Scan(
			&conn.ID, &conn.FamilyID, &conn.Provider, &conn.Status, &conn.SessionVersion,
			&credHash, &lastValStatus, &lastValReportID, &initialImportCompletedAt,
		)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrConnectionNotFound
			}
			return err
		}

		if conn.Status == ConnectionStateDisconnected {
			return ErrSyncGateConnectionDisconnected
		}
		if conn.Status != ConnectionStateValidationPassed && initialImportCompletedAt == nil {
			return fmt.Errorf("connection must be in validation_passed state to approve initial import (current: %s)", conn.Status)
		}
		if lastValReportID == nil || lastValStatus == nil || *lastValStatus != string(ValidationResultPassed) {
			return ErrSyncGateValidationRequired
		}
		if credHash == nil || *credHash == "" {
			return ErrSyncGateMissingCredentialHash
		}

		// Check if active approval already exists for current version, hash, and report
		var existingApproval InitialImportApproval
		err = tx.QueryRow(r.Context(), `
			SELECT id, family_id, bank_connection_id, validation_report_id, session_version,
			       credential_hash, approved_by_user_id, approved_at, consumed_at, created_at, updated_at
			FROM public.bank_initial_import_approvals
			WHERE family_id = $1 AND bank_connection_id = $2
			  AND validation_report_id = $3 AND session_version = $4 AND credential_hash = $5
			  AND invalidated_at IS NULL
			ORDER BY created_at DESC LIMIT 1
		`, familyID, connID, *lastValReportID, conn.SessionVersion, *credHash).Scan(
			&existingApproval.ID, &existingApproval.FamilyID, &existingApproval.BankConnectionID,
			&existingApproval.ValidationReportID, &existingApproval.SessionVersion,
			&existingApproval.CredentialHash, &existingApproval.ApprovedByUserID,
			&existingApproval.ApprovedAt, &existingApproval.ConsumedAt,
			&existingApproval.CreatedAt, &existingApproval.UpdatedAt,
		)
		if err == nil {
			approval = existingApproval
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		// Insert new approval
		now := time.Now().UTC()
		approval = InitialImportApproval{
			ID:                 uuid.New(),
			FamilyID:           familyID,
			BankConnectionID:   connID,
			ValidationReportID: *lastValReportID,
			SessionVersion:     conn.SessionVersion,
			CredentialHash:     *credHash,
			ApprovedByUserID:   userID,
			ApprovedAt:         now,
			CreatedAt:          now,
			UpdatedAt:          now,
		}
		_, err = tx.Exec(r.Context(), `
			INSERT INTO public.bank_initial_import_approvals (
				id, family_id, bank_connection_id, validation_report_id, session_version,
				credential_hash, approved_by_user_id, approved_at, created_at, updated_at
			) VALUES (
				$1, $2, $3, $4, $5, $6, $7, $8, $9, $9
			)
		`, approval.ID, familyID, connID, approval.ValidationReportID, approval.SessionVersion,
			approval.CredentialHash, approval.ApprovedByUserID, approval.ApprovedAt, now)
		if err != nil {
			return err
		}

		// Structured audit log event
		_ = RecordAuditEvent(r.Context(), tx, familyID, &userID, "bank_connection", connID,
			"initial_import_approval_granted", nil, map[string]any{
				"approval_id":          approval.ID,
				"validation_report_id": approval.ValidationReportID,
				"session_version":      approval.SessionVersion,
			})

		return nil
	})

	if err != nil {
		if errors.Is(err, ErrConnectionNotFound) {
			writeProblem(w, http.StatusNotFound, "Not Found", "Bank connection not found")
			return
		}
		writeProblem(w, http.StatusConflict, "Approval Conflict", fmt.Sprintf("Cannot approve initial import: %v", err))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(approval)
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
		if errors.Is(err, ErrSyncGateInitialImportNotApproved) {
			writeProblem(w, http.StatusConflict, "Initial Import Approval Required", "Initial financial import requires explicit user approval before execution")
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

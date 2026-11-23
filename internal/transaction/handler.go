package transaction

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/khaliullov/family-finance/internal/auth"
	"github.com/khaliullov/family-finance/internal/database"
	"github.com/khaliullov/family-finance/internal/queue"
)

// Handler exposes HTTP REST endpoints for bank connections and synchronization triggers.
type Handler struct {
	pool    *pgxpool.Pool
	service *Service
}

// NewHandler creates a new Connection/Transaction HTTP Handler.
func NewHandler(pool *pgxpool.Pool, service *Service) *Handler {
	return &Handler{
		pool:    pool,
		service: service,
	}
}

// RegisterRoutes registers connection endpoints on the provided ServeMux.
func (h *Handler) RegisterRoutes(mux *http.ServeMux, authMw func(http.Handler) http.Handler) {
	register := func(prefix string) {
		mux.Handle("GET "+prefix+"/connections", authMw(http.HandlerFunc(h.HandleListConnections)))
		mux.Handle("POST "+prefix+"/connections", authMw(http.HandlerFunc(h.HandleCreateConnection)))
		mux.Handle("POST "+prefix+"/connections/{id}/sync", authMw(http.HandlerFunc(h.HandleTriggerSync)))
	}

	register("/api/v1")
	register("")
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

	rawID := r.PathValue("id")
	connID, err := uuid.Parse(rawID)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "Bad Request", "Invalid connection ID format")
		return
	}

	// Verify connection exists for this family
	var count int
	err = database.WithFamilyTx(r.Context(), h.pool, familyID, func(tx pgx.Tx) error {
		return tx.QueryRow(r.Context(), `
			SELECT count(*) FROM public.bank_connections
			WHERE family_id = $1 AND id = $2
		`, familyID, connID).Scan(&count)
	})
	if err != nil || count == 0 {
		writeProblem(w, http.StatusNotFound, "Not Found", "Bank connection not found")
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

// ConnectionResponse represents a bank connection summary.
type ConnectionResponse struct {
	ID           uuid.UUID `json:"id"`
	Provider     string    `json:"provider"`
	Name         string    `json:"name"`
	Status       string    `json:"status"`
	LastSyncedAt *string   `json:"last_synced_at,omitempty"`
	ErrorMessage *string   `json:"error_message,omitempty"`
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
			SELECT id, provider, name, status, last_synced_at, error_message
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
			var lastSync *string
			err := rows.Scan(&c.ID, &c.Provider, &c.Name, &c.Status, &lastSync, &c.ErrorMessage)
			if err != nil {
				return err
			}
			c.LastSyncedAt = lastSync
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

	newID := uuid.New()
	err := database.WithFamilyTx(r.Context(), h.pool, familyID, func(tx pgx.Tx) error {
		_, err := tx.Exec(r.Context(), `
			INSERT INTO public.bank_connections (
				id, family_id, authorized_by_user_id, provider, name, status
			) VALUES (
				$1, $2, $3, $4, $5, 'active'
			)
		`, newID, familyID, userID, provider, name)
		return err
	})
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "Internal Server Error", "Failed to create connection")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(ConnectionResponse{
		ID:       newID,
		Provider: provider,
		Name:     name,
		Status:   "active",
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

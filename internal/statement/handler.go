package statement

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/khaliullov/family-finance/internal/auth"
)

// Handler exposes HTTP REST endpoints for the statement ingestion workflow.
type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

// RegisterRoutes registers all statement import routes on the provided ServeMux.
func (h *Handler) RegisterRoutes(mux *http.ServeMux, authMw func(http.Handler) http.Handler) {
	register := func(prefix string) {
		mux.Handle("POST "+prefix+"/imports", authMw(http.HandlerFunc(h.HandleUploadStatement)))
		mux.Handle("POST "+prefix+"/imports/upload", authMw(http.HandlerFunc(h.HandleUploadStatement)))
		mux.Handle("GET "+prefix+"/imports", authMw(http.HandlerFunc(h.HandleListImports)))
		mux.Handle("GET "+prefix+"/imports/{id}", authMw(http.HandlerFunc(h.HandleGetImport)))
		mux.Handle("GET "+prefix+"/imports/{id}/preview", authMw(http.HandlerFunc(h.HandleGetPreview)))
		mux.Handle("PATCH "+prefix+"/imports/{id}/rows/{row_id}", authMw(http.HandlerFunc(h.HandleUpdateRow)))
		mux.Handle("POST "+prefix+"/imports/{id}/approve", authMw(http.HandlerFunc(h.HandleApproveBatch)))
		mux.Handle("POST "+prefix+"/imports/{id}/promote", authMw(http.HandlerFunc(h.HandlePromoteBatch)))
		mux.Handle("POST "+prefix+"/imports/{id}/confirm", authMw(http.HandlerFunc(h.HandlePromoteBatch)))
		mux.Handle("POST "+prefix+"/imports/{id}/cancel", authMw(http.HandlerFunc(h.HandleCancelBatch)))
		mux.Handle("POST "+prefix+"/imports/{id}/override", authMw(http.HandlerFunc(h.HandleReconciliationOverride)))
	}

	register("/api/v1")
	register("")
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

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

// HandleUploadStatement processes multipart statement file uploads.
func (h *Handler) HandleUploadStatement(w http.ResponseWriter, r *http.Request) {
	familyID, ok := auth.FamilyIDFromContext(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "Unauthorized", "Missing active family context")
		return
	}
	userID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "Unauthorized", "Missing authenticated user context")
		return
	}

	// Max 10 MiB upload limit enforced at parser and reader
	const maxUploadSize = 10 * 1024 * 1024
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize+1024)

	if err := r.ParseMultipartForm(maxUploadSize); err != nil {
		if strings.Contains(err.Error(), "http: request body too large") {
			writeProblem(w, http.StatusRequestEntityTooLarge, "Payload Too Large", "Uploaded file exceeds maximum limit of 10 MB")
			return
		}
		writeProblem(w, http.StatusBadRequest, "Bad Request", "Malformed multipart form data: "+err.Error())
		return
	}

	accountIDStr := strings.TrimSpace(r.FormValue("account_id"))
	if accountIDStr == "" {
		writeProblem(w, http.StatusBadRequest, "Bad Request", "Missing required form field: account_id")
		return
	}
	accountID, err := uuid.Parse(accountIDStr)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "Bad Request", "Invalid account_id UUID format")
		return
	}

	file, fileHeader, err := r.FormFile("file")
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "Bad Request", "Missing or invalid file field in multipart form")
		return
	}
	defer file.Close()

	if fileHeader.Size == 0 {
		writeProblem(w, http.StatusBadRequest, "Bad Request", "Uploaded statement file is empty")
		return
	}
	if fileHeader.Size > maxUploadSize {
		writeProblem(w, http.StatusRequestEntityTooLarge, "Payload Too Large", "File exceeds 10 MB limit")
		return
	}

	// Optional format parameter
	formatHint := strings.TrimSpace(r.FormValue("bank_format"))
	if formatHint == "" {
		formatHint = strings.TrimSpace(r.FormValue("format"))
	}
	var parser StatementParser
	if formatHint != "" {
		if p, ok := h.service.parsers.Get(formatHint); ok {
			parser = p
		} else {
			writeProblem(w, http.StatusBadRequest, "Bad Request", fmt.Sprintf("Unsupported format: %s", formatHint))
			return
		}
	}

	batch, err := h.service.UploadAndStage(
		r.Context(),
		familyID, userID, accountID,
		fileHeader.Filename,
		file,
		fileHeader.Size,
		parser,
	)
	if err != nil {
		switch {
		case errors.Is(err, ErrFileTooLarge):
			writeProblem(w, http.StatusRequestEntityTooLarge, "Payload Too Large", err.Error())
		case errors.Is(err, ErrParserNotFound):
			writeProblem(w, http.StatusBadRequest, "Bad Request", "No compatible parser found for statement format")
		case errors.Is(err, ErrAccountArchived):
			writeProblem(w, http.StatusBadRequest, "Bad Request", "Target account is archived and cannot receive statement imports")
		case errors.Is(err, ErrTooManyConcurrentImports):
			writeProblem(w, http.StatusTooManyRequests, "Too Many Requests", err.Error())
		case errors.Is(err, ErrTenantContextMismatch):
			writeProblem(w, http.StatusForbidden, "Forbidden", "Tenant context violation")
		default:
			writeProblem(w, http.StatusBadRequest, "Bad Request", err.Error())
		}
		return
	}

	summary, err := h.service.GetImportSummary(r.Context(), familyID, batch.ID)
	if err != nil {
		writeJSON(w, http.StatusCreated, batch)
		return
	}

	writeJSON(w, http.StatusCreated, summary)
}

// HandleListImports returns a paginated list of statement batches for the authenticated family.
func (h *Handler) HandleListImports(w http.ResponseWriter, r *http.Request) {
	familyID, ok := auth.FamilyIDFromContext(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "Unauthorized", "Missing active family context")
		return
	}

	page := 1
	limit := 20
	if pStr := r.URL.Query().Get("page"); pStr != "" {
		if p, err := strconv.Atoi(pStr); err == nil && p > 0 {
			page = p
		}
	}
	if lStr := r.URL.Query().Get("limit"); lStr != "" {
		if l, err := strconv.Atoi(lStr); err == nil && l > 0 && l <= 100 {
			limit = l
		}
	}

	var accountID *uuid.UUID
	if accStr := r.URL.Query().Get("account_id"); accStr != "" {
		if id, err := uuid.Parse(accStr); err == nil {
			accountID = &id
		}
	}

	var status *string
	if st := strings.TrimSpace(r.URL.Query().Get("status")); st != "" {
		status = &st
	}

	history, err := h.service.ListImportHistory(r.Context(), familyID, accountID, status, page, limit)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "Internal Server Error", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, history)
}

// HandleGetImport returns the summary metadata of a single import batch.
func (h *Handler) HandleGetImport(w http.ResponseWriter, r *http.Request) {
	familyID, ok := auth.FamilyIDFromContext(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "Unauthorized", "Missing active family context")
		return
	}

	batchID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "Bad Request", "Invalid import batch ID format")
		return
	}

	summary, err := h.service.GetImportSummary(r.Context(), familyID, batchID)
	if err != nil {
		if errors.Is(err, ErrBatchNotFound) {
			writeProblem(w, http.StatusNotFound, "Not Found", "Statement import batch not found")
			return
		}
		writeProblem(w, http.StatusInternalServerError, "Internal Server Error", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, summary)
}

// HandleGetPreview returns the complete preview with summary and staged rows.
func (h *Handler) HandleGetPreview(w http.ResponseWriter, r *http.Request) {
	familyID, ok := auth.FamilyIDFromContext(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "Unauthorized", "Missing active family context")
		return
	}

	batchID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "Bad Request", "Invalid import batch ID format")
		return
	}

	preview, err := h.service.GetImportPreview(r.Context(), familyID, batchID)
	if err != nil {
		if errors.Is(err, ErrBatchNotFound) {
			writeProblem(w, http.StatusNotFound, "Not Found", "Statement import batch not found")
			return
		}
		writeProblem(w, http.StatusInternalServerError, "Internal Server Error", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, preview)
}

// HandleUpdateRow allows user modification of a staged row (category, classification, resolution).
func (h *Handler) HandleUpdateRow(w http.ResponseWriter, r *http.Request) {
	familyID, ok := auth.FamilyIDFromContext(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "Unauthorized", "Missing active family context")
		return
	}
	userID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "Unauthorized", "Missing authenticated user context")
		return
	}

	batchID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "Bad Request", "Invalid import batch ID format")
		return
	}

	rowID, err := uuid.Parse(r.PathValue("row_id"))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "Bad Request", "Invalid row ID format")
		return
	}

	var req UpdateStagedRowRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeProblem(w, http.StatusBadRequest, "Bad Request", "Malformed JSON body: "+err.Error())
		return
	}

	// Validate resolution action if provided
	if req.ResolutionAction != nil {
		switch *req.ResolutionAction {
		case ResolutionActionPending, ResolutionActionCommit, ResolutionActionSkipDuplicate, ResolutionActionSkipCancelled, ResolutionActionApplySettlement, ResolutionActionReleasePending:
		default:
			writeProblem(w, http.StatusBadRequest, "Bad Request", "Invalid resolution_action: "+*req.ResolutionAction)
			return
		}
	}

	summary, err := h.service.UpdateStagedRow(r.Context(), familyID, userID, batchID, rowID, req)
	if err != nil {
		switch {
		case errors.Is(err, ErrBatchNotFound):
			writeProblem(w, http.StatusNotFound, "Not Found", "Statement import batch not found")
		case errors.Is(err, ErrStagedTransactionNotFound):
			writeProblem(w, http.StatusNotFound, "Not Found", "Staged bank transaction not found")
		case errors.Is(err, ErrCannotCommitCancelled):
			writeProblem(w, http.StatusBadRequest, "Bad Request", err.Error())
		case errors.Is(err, ErrDuplicateSettlementTarget):
			writeProblem(w, http.StatusBadRequest, "Bad Request", err.Error())
		case errors.Is(err, ErrBatchAlreadyCommitted):
			writeProblem(w, http.StatusConflict, "Conflict", err.Error())
		default:
			writeProblem(w, http.StatusBadRequest, "Bad Request", err.Error())
		}
		return
	}

	writeJSON(w, http.StatusOK, summary)
}

// HandleApproveBatch records explicit user approval for the reviewed batch.
func (h *Handler) HandleApproveBatch(w http.ResponseWriter, r *http.Request) {
	familyID, ok := auth.FamilyIDFromContext(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "Unauthorized", "Missing active family context")
		return
	}
	userID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "Unauthorized", "Missing authenticated user context")
		return
	}

	batchID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "Bad Request", "Invalid import batch ID format")
		return
	}

	var req ApproveBatchRequest
	_ = json.NewDecoder(r.Body).Decode(&req)

	summary, err := h.service.ApproveBatch(r.Context(), familyID, userID, batchID, req)
	if err != nil {
		switch {
		case errors.Is(err, ErrBatchNotFound):
			writeProblem(w, http.StatusNotFound, "Not Found", "Statement import batch not found")
		case errors.Is(err, ErrBatchAlreadyCommitted):
			writeProblem(w, http.StatusConflict, "Conflict", "Batch is already committed")
		case errors.Is(err, ErrOverrideRequired):
			writeProblem(w, http.StatusUnprocessableEntity, "Unprocessable Entity", err.Error())
		case errors.Is(err, ErrUnresolvedStagedRows):
			writeProblem(w, http.StatusUnprocessableEntity, "Unprocessable Entity", err.Error())
		case errors.Is(err, ErrCannotCommitCancelled):
			writeProblem(w, http.StatusBadRequest, "Bad Request", err.Error())
		case errors.Is(err, ErrDuplicateSettlementTarget):
			writeProblem(w, http.StatusBadRequest, "Bad Request", err.Error())
		default:
			writeProblem(w, http.StatusBadRequest, "Bad Request", err.Error())
		}
		return
	}

	writeJSON(w, http.StatusOK, summary)
}

// HandlePromoteBatch atomically commits approved staged transactions into the ledger.
func (h *Handler) HandlePromoteBatch(w http.ResponseWriter, r *http.Request) {
	familyID, ok := auth.FamilyIDFromContext(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "Unauthorized", "Missing active family context")
		return
	}
	userID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "Unauthorized", "Missing authenticated user context")
		return
	}

	batchID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		// Also check import_id for backwards compatibility
		if idStr := r.PathValue("import_id"); idStr != "" {
			batchID, err = uuid.Parse(idStr)
		}
	}
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "Bad Request", "Invalid import batch ID format")
		return
	}

	result, err := h.service.PromoteBatch(r.Context(), familyID, userID, batchID)
	if err != nil {
		switch {
		case errors.Is(err, ErrBatchNotFound):
			writeProblem(w, http.StatusNotFound, "Not Found", "Statement import batch not found")
		case errors.Is(err, ErrBatchNotApproved):
			writeProblem(w, http.StatusBadRequest, "Bad Request", err.Error())
		case errors.Is(err, ErrRevisionMismatch):
			writeProblem(w, http.StatusConflict, "Conflict", err.Error())
		case errors.Is(err, ErrOverrideRequired):
			writeProblem(w, http.StatusUnprocessableEntity, "Unprocessable Entity", err.Error())
		case errors.Is(err, ErrUnresolvedStagedRows):
			writeProblem(w, http.StatusUnprocessableEntity, "Unprocessable Entity", err.Error())
		case errors.Is(err, ErrCannotCommitCancelled):
			writeProblem(w, http.StatusBadRequest, "Bad Request", err.Error())
		case errors.Is(err, ErrDuplicateSettlementTarget):
			writeProblem(w, http.StatusBadRequest, "Bad Request", err.Error())
		default:
			writeProblem(w, http.StatusBadRequest, "Bad Request", err.Error())
		}
		return
	}

	writeJSON(w, http.StatusOK, result)
}

// HandleCancelBatch cancels a staged or approved import batch.
func (h *Handler) HandleCancelBatch(w http.ResponseWriter, r *http.Request) {
	familyID, ok := auth.FamilyIDFromContext(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "Unauthorized", "Missing active family context")
		return
	}
	userID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "Unauthorized", "Missing authenticated user context")
		return
	}

	batchID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "Bad Request", "Invalid import batch ID format")
		return
	}

	summary, err := h.service.CancelBatch(r.Context(), familyID, userID, batchID)
	if err != nil {
		switch {
		case errors.Is(err, ErrBatchNotFound):
			writeProblem(w, http.StatusNotFound, "Not Found", "Statement import batch not found")
		case errors.Is(err, ErrBatchAlreadyCommitted):
			writeProblem(w, http.StatusConflict, "Conflict", "Cannot cancel an already committed statement batch")
		default:
			writeProblem(w, http.StatusBadRequest, "Bad Request", err.Error())
		}
		return
	}

	writeJSON(w, http.StatusOK, summary)
}

// HandleReconciliationOverride records an attributable user override for a closing balance discrepancy.
func (h *Handler) HandleReconciliationOverride(w http.ResponseWriter, r *http.Request) {
	familyID, ok := auth.FamilyIDFromContext(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "Unauthorized", "Missing active family context")
		return
	}
	userID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		writeProblem(w, http.StatusUnauthorized, "Unauthorized", "Missing authenticated user context")
		return
	}

	batchID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "Bad Request", "Invalid import batch ID format")
		return
	}

	var req ReconciliationOverrideRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, http.StatusBadRequest, "Bad Request", "Malformed JSON body: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		writeProblem(w, http.StatusBadRequest, "Bad Request", "Reconciliation override reason cannot be empty")
		return
	}

	err = h.service.CreateReconciliationOverride(r.Context(), familyID, userID, batchID, req.Reason)
	if err != nil {
		switch {
		case errors.Is(err, ErrBatchNotFound):
			writeProblem(w, http.StatusNotFound, "Not Found", "Statement import batch not found")
		default:
			writeProblem(w, http.StatusBadRequest, "Bad Request", err.Error())
		}
		return
	}

	summary, err := h.service.GetImportSummary(r.Context(), familyID, batchID)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "Internal Server Error", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, summary)
}

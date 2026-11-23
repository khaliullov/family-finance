package auth

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

// RegisterRoutes attaches authentication endpoints to the provided HTTP ServeMux.
func (h *Handler) RegisterRoutes(mux *http.ServeMux, authMw func(http.Handler) http.Handler) {
	register := func(prefix string) {
		mux.HandleFunc("POST "+prefix+"/register", h.HandleRegister)
		mux.HandleFunc("POST "+prefix+"/login", h.HandleLogin)
		mux.HandleFunc("POST "+prefix+"/refresh", h.HandleRefresh)
		mux.HandleFunc("POST "+prefix+"/logout", h.HandleLogout)

		mux.Handle("GET "+prefix+"/me", authMw(http.HandlerFunc(h.HandleGetMe)))
		mux.Handle("GET "+prefix+"/sessions", authMw(http.HandlerFunc(h.HandleListSessions)))
		mux.Handle("DELETE "+prefix+"/sessions/{id}", authMw(http.HandlerFunc(h.HandleRevokeSession)))
	}

	register("/api/v1/auth")
	register("/auth")
}

func (h *Handler) HandleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONProblem(w, http.StatusMethodNotAllowed, "Method Not Allowed", "only POST is supported")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1024*1024)
	var req RegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONProblem(w, http.StatusBadRequest, "Bad Request", "malformed JSON request body")
		return
	}

	ua := r.UserAgent()
	ip := extractClientIP(r)

	res, err := h.service.Register(r.Context(), req, ua, ip)
	if err != nil {
		if errors.Is(err, ErrEmailAlreadyExists) {
			writeJSONProblem(w, http.StatusConflict, "Conflict", "email is already registered")
			return
		}
		if errors.Is(err, ErrInvalidEmail) ||
			errors.Is(err, ErrPasswordTooShort) ||
			errors.Is(err, ErrPasswordTooLong) ||
			errors.Is(err, ErrMissingRequiredField) {
			writeJSONProblem(w, http.StatusBadRequest, "Bad Request", err.Error())
			return
		}
		writeJSONProblem(w, http.StatusInternalServerError, "Internal Server Error", "failed to complete registration")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(res)
}

func (h *Handler) HandleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONProblem(w, http.StatusMethodNotAllowed, "Method Not Allowed", "only POST is supported")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1024*1024)
	var req LoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONProblem(w, http.StatusBadRequest, "Bad Request", "malformed JSON request body")
		return
	}

	ua := r.UserAgent()
	ip := extractClientIP(r)

	res, err := h.service.Login(r.Context(), req, ua, ip)
	if err != nil {
		if errors.Is(err, ErrInvalidCredentials) {
			writeJSONProblem(w, http.StatusUnauthorized, "Unauthorized", "invalid email or password")
			return
		}
		writeJSONProblem(w, http.StatusInternalServerError, "Internal Server Error", "failed to authenticate")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(res)
}

func (h *Handler) HandleRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONProblem(w, http.StatusMethodNotAllowed, "Method Not Allowed", "only POST is supported")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1024*1024)
	var req RefreshRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONProblem(w, http.StatusBadRequest, "Bad Request", "malformed JSON request body")
		return
	}

	ua := r.UserAgent()
	ip := extractClientIP(r)

	res, err := h.service.RefreshToken(r.Context(), req.RefreshToken, ua, ip)
	if err != nil {
		if errors.Is(err, ErrSessionReplayDetected) {
			writeJSONProblem(w, http.StatusUnauthorized, "Unauthorized", "session revoked due to token reuse")
			return
		}
		if errors.Is(err, ErrSessionNotFound) || errors.Is(err, ErrSessionRevoked) || errors.Is(err, ErrSessionExpired) {
			writeJSONProblem(w, http.StatusUnauthorized, "Unauthorized", "invalid or expired refresh token")
			return
		}
		writeJSONProblem(w, http.StatusInternalServerError, "Internal Server Error", "failed to refresh session")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(res)
}

func (h *Handler) HandleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONProblem(w, http.StatusMethodNotAllowed, "Method Not Allowed", "only POST is supported")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1024*1024)
	var req LogoutRequest
	_ = json.NewDecoder(r.Body).Decode(&req) // Ignore EOF if empty

	if req.RefreshToken != "" {
		_ = h.service.Logout(r.Context(), req.RefreshToken)
	}

	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) HandleGetMe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONProblem(w, http.StatusMethodNotAllowed, "Method Not Allowed", "only GET is supported")
		return
	}

	userID, ok := UserIDFromContext(r.Context())
	if !ok {
		writeJSONProblem(w, http.StatusUnauthorized, "Unauthorized", "unauthenticated request")
		return
	}

	familyID, ok := FamilyIDFromContext(r.Context())
	if !ok {
		writeJSONProblem(w, http.StatusBadRequest, "Bad Request", "no active family context")
		return
	}

	profile, err := h.service.GetMe(r.Context(), userID, familyID)
	if err != nil {
		writeJSONProblem(w, http.StatusInternalServerError, "Internal Server Error", "failed to retrieve profile")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(profile)
}

func (h *Handler) HandleListSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONProblem(w, http.StatusMethodNotAllowed, "Method Not Allowed", "only GET is supported")
		return
	}

	userID, ok := UserIDFromContext(r.Context())
	if !ok {
		writeJSONProblem(w, http.StatusUnauthorized, "Unauthorized", "unauthenticated request")
		return
	}

	sessions, err := h.service.ListSessions(r.Context(), userID)
	if err != nil {
		writeJSONProblem(w, http.StatusInternalServerError, "Internal Server Error", "failed to list sessions")
		return
	}

	if sessions == nil {
		sessions = []SessionDTO{}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(sessions)
}

func (h *Handler) HandleRevokeSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeJSONProblem(w, http.StatusMethodNotAllowed, "Method Not Allowed", "only DELETE is supported")
		return
	}

	userID, ok := UserIDFromContext(r.Context())
	if !ok {
		writeJSONProblem(w, http.StatusUnauthorized, "Unauthorized", "unauthenticated request")
		return
	}

	idStr := r.PathValue("id")
	if idStr == "" {
		// Fallback extraction
		idx := strings.LastIndex(r.URL.Path, "/")
		if idx != -1 && idx < len(r.URL.Path)-1 {
			idStr = r.URL.Path[idx+1:]
		}
	}

	sessionID, err := uuid.Parse(idStr)
	if err != nil {
		writeJSONProblem(w, http.StatusBadRequest, "Bad Request", "invalid session UUID")
		return
	}

	err = h.service.RevokeSession(r.Context(), userID, sessionID)
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			writeJSONProblem(w, http.StatusNotFound, "Not Found", "session not found")
			return
		}
		writeJSONProblem(w, http.StatusInternalServerError, "Internal Server Error", "failed to revoke session")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func extractClientIP(r *http.Request) string {
	xff := r.Header.Get("X-Forwarded-For")
	if xff != "" {
		ips := strings.Split(xff, ",")
		if len(ips) > 0 {
			return strings.TrimSpace(ips[0])
		}
	}
	if xrip := strings.TrimSpace(r.Header.Get("X-Real-IP")); xrip != "" {
		return xrip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

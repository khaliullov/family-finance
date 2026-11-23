package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

type contextKey string

const (
	claimsCtxKey   contextKey = "auth_claims"
	familyIDCtxKey contextKey = "auth_family_id"
	roleCtxKey     contextKey = "auth_role"
)

// ClaimsFromContext extracts the parsed JWT Claims from request context.
func ClaimsFromContext(ctx context.Context) (*Claims, bool) {
	c, ok := ctx.Value(claimsCtxKey).(*Claims)
	return c, ok
}

// UserIDFromContext extracts authenticated UserID from request context.
func UserIDFromContext(ctx context.Context) (uuid.UUID, bool) {
	c, ok := ClaimsFromContext(ctx)
	if !ok || c == nil {
		return uuid.Nil, false
	}
	return c.UserID, true
}

// FamilyIDFromContext extracts current active FamilyID from request context.
func FamilyIDFromContext(ctx context.Context) (uuid.UUID, bool) {
	fID, ok := ctx.Value(familyIDCtxKey).(uuid.UUID)
	return fID, ok && fID != uuid.Nil
}

// RoleFromContext extracts current active family Role from request context.
func RoleFromContext(ctx context.Context) (string, bool) {
	r, ok := ctx.Value(roleCtxKey).(string)
	return r, ok && r != ""
}

// ContextWithFamilyAndUser attaches authenticated family and user identities to context for testing or internal dispatch.
func ContextWithFamilyAndUser(ctx context.Context, familyID, userID uuid.UUID, role string) context.Context {
	ctx = context.WithValue(ctx, familyIDCtxKey, familyID)
	ctx = context.WithValue(ctx, roleCtxKey, role)
	ctx = context.WithValue(ctx, claimsCtxKey, &Claims{
		UserID:   userID,
		FamilyID: familyID,
		Role:     role,
	})
	return ctx
}

// AuthMiddleware validates the Bearer access token and enforces tenant membership boundaries.
func AuthMiddleware(jwtMgr *JWTManager, repo Repository) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authHeader := r.Header.Get("Authorization")
			if authHeader == "" {
				writeJSONProblem(w, http.StatusUnauthorized, "Unauthorized", "missing Authorization header")
				return
			}

			parts := strings.SplitN(authHeader, " ", 2)
			if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
				writeJSONProblem(w, http.StatusUnauthorized, "Unauthorized", "invalid Authorization header format; expected Bearer <token>")
				return
			}

			tokenStr := strings.TrimSpace(parts[1])
			claims, err := jwtMgr.ValidateToken(tokenStr)
			if err != nil {
				writeJSONProblem(w, http.StatusUnauthorized, "Unauthorized", "invalid or expired access token")
				return
			}

			activeFamilyID := claims.FamilyID

			// Client-specified family context switching via X-Family-ID header
			targetFamilyHeader := strings.TrimSpace(r.Header.Get("X-Family-ID"))
			if targetFamilyHeader != "" {
				parsedFamilyID, err := uuid.Parse(targetFamilyHeader)
				if err != nil {
					writeJSONProblem(w, http.StatusBadRequest, "Bad Request", "invalid X-Family-ID header format; expected UUID")
					return
				}
				activeFamilyID = parsedFamilyID
			}

			// Validate active family membership against database - never rely exclusively on potentially stale JWT claims!
			memberships, err := repo.GetFamilyMemberships(r.Context(), claims.UserID)
			if err != nil {
				writeJSONProblem(w, http.StatusInternalServerError, "Internal Server Error", "failed to verify family membership")
				return
			}

			var isMember bool
			var activeRole string
			for _, m := range memberships {
				if m.FamilyID == activeFamilyID {
					isMember = true
					activeRole = m.Role
					break
				}
			}

			if !isMember {
				writeJSONProblem(w, http.StatusForbidden, "Forbidden", "user is not a member of the requested family")
				return
			}

			ctx := context.WithValue(r.Context(), claimsCtxKey, claims)
			ctx = context.WithValue(ctx, familyIDCtxKey, activeFamilyID)
			ctx = context.WithValue(ctx, roleCtxKey, activeRole)

			next.ServeHTTP(w, r.WithContext(ctx))
		})
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

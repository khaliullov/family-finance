package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/google/uuid"
)

const (
	// DefaultRefreshTokenTTL defines the maximum lifespan of a refresh token (30 days).
	DefaultRefreshTokenTTL = 30 * 24 * time.Hour

	// RefreshTokenBytes defines the entropy size for raw refresh tokens (32 bytes = 256 bits).
	RefreshTokenBytes = 32
)

// Session represents an active or historic user refresh session stored in the database.
type Session struct {
	ID             uuid.UUID  `json:"id"`
	UserID         uuid.UUID  `json:"user_id"`
	FamilyID       uuid.UUID  `json:"family_id"`
	TokenHash      string     `json:"-"` // Never serialized or logged
	SessionChainID uuid.UUID  `json:"session_chain_id"`
	UserAgent      string     `json:"user_agent"`
	IPAddress      string     `json:"ip_address"`
	IsRevoked      bool       `json:"is_revoked"`
	IsRotated      bool       `json:"is_rotated"`
	ExpiresAt      time.Time  `json:"expires_at"`
	CreatedAt      time.Time  `json:"created_at"`
	LastUsedAt     time.Time  `json:"last_used_at"`
	RevokedAt      *time.Time `json:"revoked_at,omitempty"`
}

// SessionDTO provides a safe public view of a session without sensitive metadata.
type SessionDTO struct {
	ID         uuid.UUID `json:"id"`
	UserAgent  string    `json:"user_agent"`
	IPAddress  string    `json:"ip_address"`
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	Current    bool      `json:"current"`
}

// GenerateRefreshToken creates a new cryptographically secure opaque refresh token
// and its SHA-256 hash. The raw token is returned to the client and never stored.
func GenerateRefreshToken() (rawToken string, tokenHash string, err error) {
	bytes := make([]byte, RefreshTokenBytes)
	if _, err := rand.Read(bytes); err != nil {
		return "", "", fmt.Errorf("failed to generate random refresh token: %w", err)
	}

	rawToken = base64.RawURLEncoding.EncodeToString(bytes)
	tokenHash = HashSessionToken(rawToken)
	return rawToken, tokenHash, nil
}

// HashSessionToken computes the lowercase SHA-256 hex digest of a raw token string.
func HashSessionToken(rawToken string) string {
	sum := sha256.Sum256([]byte(rawToken))
	return hex.EncodeToString(sum[:])
}

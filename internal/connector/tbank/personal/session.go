package personal

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/khaliullov/family-finance/internal/crypto"
)

var (
	ErrSessionNotFound = errors.New("session: banking session not found or not initialized")
	ErrSessionExpired  = errors.New("session: banking session cookies or authorization expired")
	ErrSessionCorrupt  = errors.New("session: session data corrupt or decryption failed")
)

// SessionStatus represents the operational lifecycle state of an encrypted banking session.
type SessionStatus string

const (
	SessionStatusActive       SessionStatus = "active"
	SessionStatusExpired      SessionStatus = "expired"
	SessionStatusDisconnected SessionStatus = "disconnected"
)

// SerializableCookie captures essential http.Cookie attributes for persistent encrypted storage.
type SerializableCookie struct {
	Name     string    `json:"name"`
	Value    string    `json:"value"`
	Path     string    `json:"path,omitempty"`
	Domain   string    `json:"domain,omitempty"`
	Expires  time.Time `json:"expires,omitempty"`
	Secure   bool      `json:"secure,omitempty"`
	HttpOnly bool      `json:"http_only,omitempty"`
	SameSite int       `json:"same_site,omitempty"`
}

// Session represents a tenant-isolated, encrypted personal banking session.
// All sensitive cookies and metadata are strictly encrypted at rest using AES-256-GCM.
type Session struct {
	FamilyID       uuid.UUID            `json:"family_id"`
	ConnectionID   uuid.UUID            `json:"connection_id"`
	Status         SessionStatus        `json:"status"`
	Cookies        []SerializableCookie `json:"cookies"`
	Metadata       map[string]string    `json:"metadata,omitempty"`
	CreatedAt      time.Time            `json:"created_at"`
	ExpiresAt      time.Time            `json:"expires_at"`
	LastVerifiedAt time.Time            `json:"last_verified_at"`
	Version        int64                `json:"version"`
}

// NewSession initializes a new active session with safe timestamps.
func NewSession(familyID, connectionID uuid.UUID, cookies []*http.Cookie, ttl time.Duration) *Session {
	now := time.Now().UTC()
	if ttl <= 0 {
		ttl = 30 * 24 * time.Hour // Default 30-day session lifetime if unspecified
	}

	serialized := make([]SerializableCookie, 0, len(cookies))
	for _, c := range cookies {
		if c == nil || c.Name == "" {
			continue
		}
		serialized = append(serialized, SerializableCookie{
			Name:     c.Name,
			Value:    c.Value,
			Path:     c.Path,
			Domain:   c.Domain,
			Expires:  c.Expires,
			Secure:   c.Secure,
			HttpOnly: c.HttpOnly,
			SameSite: int(c.SameSite),
		})
	}

	return &Session{
		FamilyID:       familyID,
		ConnectionID:   connectionID,
		Status:         SessionStatusActive,
		Cookies:        serialized,
		Metadata:       make(map[string]string),
		CreatedAt:      now,
		ExpiresAt:      now.Add(ttl),
		LastVerifiedAt: now,
		Version:        1,
	}
}

// IsExpired returns true if current time exceeds ExpiresAt or status is expired.
func (s *Session) IsExpired() bool {
	if s == nil {
		return true
	}
	if s.Status == SessionStatusExpired || s.Status == SessionStatusDisconnected {
		return true
	}
	if !s.ExpiresAt.IsZero() && time.Now().UTC().After(s.ExpiresAt) {
		return true
	}
	return false
}

// ToHTTPCookies converts the serializable cookies into standard *http.Cookie slices.
func (s *Session) ToHTTPCookies() []*http.Cookie {
	if s == nil {
		return nil
	}
	out := make([]*http.Cookie, 0, len(s.Cookies))
	for _, sc := range s.Cookies {
		out = append(out, &http.Cookie{
			Name:     sc.Name,
			Value:    sc.Value,
			Path:     sc.Path,
			Domain:   sc.Domain,
			Expires:  sc.Expires,
			Secure:   sc.Secure,
			HttpOnly: sc.HttpOnly,
			SameSite: http.SameSite(sc.SameSite),
		})
	}
	return out
}

// Encrypt serializes and encrypts the Session using the provided 32-byte AES key.
func (s *Session) Encrypt(key []byte) ([]byte, error) {
	if s == nil {
		return nil, errors.New("cannot encrypt nil session")
	}
	rawJSON, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("marshal session json: %w", err)
	}

	ciphertext, err := crypto.Encrypt(key, rawJSON)
	if err != nil {
		return nil, fmt.Errorf("encrypt session: %w", err)
	}
	return ciphertext, nil
}

// DecryptSession decrypts and unmarshals an encrypted session envelope using the provided 32-byte AES key.
func DecryptSession(key []byte, ciphertext []byte) (*Session, error) {
	if len(ciphertext) == 0 {
		return nil, ErrSessionNotFound
	}
	plaintext, err := crypto.Decrypt(key, ciphertext)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSessionCorrupt, err)
	}

	var s Session
	if err := json.Unmarshal(plaintext, &s); err != nil {
		return nil, fmt.Errorf("%w: invalid json: %v", ErrSessionCorrupt, err)
	}

	return &s, nil
}

// String implements fmt.Stringer with strict redaction (never prints cookies or secrets).
func (s *Session) String() string {
	if s == nil {
		return "<nil session>"
	}
	return fmt.Sprintf("Session[family=%s, conn=%s, status=%s, v=%d, cookies=%d, expires=%s]",
		s.FamilyID, s.ConnectionID, s.Status, s.Version, len(s.Cookies), s.ExpiresAt.Format(time.RFC3339))
}

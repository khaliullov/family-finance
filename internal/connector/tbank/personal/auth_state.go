package personal

import (
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Standard Authentication State constants.
const (
	StateDisconnected      = "disconnected"
	StateAuthenticating    = "authenticating"
	StateAwaitingOTP       = "awaiting_otp"
	StateVerifyingOTP      = "verifying_otp"
	StateChallengeRequired = "challenge_required"
	StateActive            = "active"
	StateFailed            = "failed"
	StateExpired           = "expired"
	StateReconnecting      = "reconnecting"
	StateLoggingOff        = "logging_off"
)

var (
	ErrInvalidStateTransition = errors.New("auth: invalid authentication state transition")
	ErrAttemptExpired         = errors.New("auth: authentication attempt has expired")
	ErrAttemptNotFound        = errors.New("auth: authentication attempt not found")
)

// AllowedTransitions defines valid forward state transitions for the authentication state machine.
var AllowedTransitions = map[string]map[string]bool{
	StateDisconnected: {
		StateAuthenticating: true,
	},
	StateAuthenticating: {
		StateAwaitingOTP:       true,
		StateChallengeRequired: true,
		StateActive:            true,
		StateFailed:            true,
	},
	StateAwaitingOTP: {
		StateVerifyingOTP:      true,
		StateFailed:            true,
		StateExpired:           true,
		StateChallengeRequired: true,
	},
	StateVerifyingOTP: {
		StateAwaitingOTP: true, // Multiple challenges/OTPs
		StateActive:      true,
		StateFailed:      true,
	},
	StateChallengeRequired: {
		StateAuthenticating: true,
		StateFailed:         true,
		StateDisconnected:   true,
	},
	StateActive: {
		StateExpired:      true,
		StateReconnecting: true,
		StateLoggingOff:   true,
		StateDisconnected: true,
	},
	StateExpired: {
		StateReconnecting:   true,
		StateAuthenticating: true,
		StateDisconnected:   true,
	},
	StateReconnecting: {
		StateActive:      true,
		StateAwaitingOTP: true,
		StateExpired:     true,
		StateFailed:      true,
	},
	StateLoggingOff: {
		StateDisconnected: true,
		StateFailed:       true,
	},
	StateFailed: {
		StateAuthenticating: true,
		StateDisconnected:   true,
	},
}

// ValidateStateTransition checks if transitioning from 'from' to 'to' is structurally legal.
func ValidateStateTransition(from, to string) error {
	targets, ok := AllowedTransitions[from]
	if !ok || !targets[to] {
		return fmt.Errorf("%w: cannot transition from %q to %q", ErrInvalidStateTransition, from, to)
	}
	return nil
}

// AuthAttempt tracks a single isolated, concurrency-safe authentication sequence.
type AuthAttempt struct {
	ID                uuid.UUID  `json:"id"`
	FamilyID          uuid.UUID  `json:"family_id"`
	ConnectionID      *uuid.UUID `json:"connection_id,omitempty"`
	Provider          string     `json:"provider"`
	State             string     `json:"state"`
	ChallengeType     string     `json:"challenge_type,omitempty"`
	ChallengeRef      string     `json:"challenge_ref,omitempty"` // Short human reference code e.g. "A7F3"
	TelegramMessageID *int64     `json:"telegram_message_id,omitempty"`
	TelegramChatID    *int64     `json:"telegram_chat_id,omitempty"`
	Generation        int64      `json:"generation"`
	ExpiresAt         time.Time  `json:"expires_at"`
	ErrorMessage      string     `json:"error_message,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

// NewAuthAttempt initializes a fresh authentication attempt in authenticating state.
func NewAuthAttempt(familyID uuid.UUID, connectionID *uuid.UUID, provider string, ttl time.Duration) *AuthAttempt {
	now := time.Now().UTC()
	if ttl <= 0 {
		ttl = 5 * time.Minute // Short 5-minute timeout for interactive authentication
	}
	return &AuthAttempt{
		ID:           uuid.New(),
		FamilyID:     familyID,
		ConnectionID: connectionID,
		Provider:     provider,
		State:        StateAuthenticating,
		ChallengeRef: GenerateChallengeRef(),
		Generation:   1,
		ExpiresAt:    now.Add(ttl),
		CreatedAt:    now,
		UpdatedAt:    now,
	}
}

// IsExpired checks whether the attempt has outlived its expiration time.
func (a *AuthAttempt) IsExpired() bool {
	if a == nil {
		return true
	}
	return time.Now().UTC().After(a.ExpiresAt)
}

// GenerateChallengeRef creates a random human-friendly 4-character uppercase alphanumeric code (e.g. "A7F3").
func GenerateChallengeRef() string {
	const charset = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // Excludes ambiguous 0, O, 1, I
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	var sb strings.Builder
	for i := 0; i < 4; i++ {
		sb.WriteByte(charset[int(b[i])%len(charset)])
	}
	return sb.String()
}

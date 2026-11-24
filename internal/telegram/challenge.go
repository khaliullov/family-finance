package telegram

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/khaliullov/family-finance/internal/connector/tbank/personal"
)

var (
	ErrUnauthorizedUser  = errors.New("telegram: unauthorized sender or chat ID")
	ErrNotAReply         = errors.New("telegram: message is not a reply to an active challenge")
	ErrChallengeNotFound = errors.New("telegram: no active challenge found for replied message")
	ErrChallengeExpired  = errors.New("telegram: authentication challenge has expired")
	ErrInvalidOTPFormat  = errors.New("telegram: message does not contain a valid confirmation code")
	ErrAlreadyConsumed   = errors.New("telegram: challenge code has already been consumed")
	ErrNoActiveConsumer  = errors.New("telegram: no active authentication consumer registered to receive code")
)

var reDigits = regexp.MustCompile(`\b\d{4,8}\b`)

// ChallengeConsumer is a callback function invoked when an OTP is resolved for an attempt.
// The consumer must process and immediately discard the OTP from memory.
type ChallengeConsumer func(ctx context.Context, attempt *personal.AuthAttempt, otp string) error

// ChallengeRouter routes incoming Telegram reply updates to their matching authentication attempts.
// Concurrency and single-consumption invariants are backed authoritatively by PostgreSQL.
type ChallengeRouter struct {
	mu           sync.Mutex
	allowedID    int64
	attemptStore personal.AuthAttemptStore
	consumers    map[uuid.UUID]ChallengeConsumer
}

// NewChallengeRouter creates a new Telegram challenge routing engine.
func NewChallengeRouter(allowedChatID int64, attemptStore personal.AuthAttemptStore) *ChallengeRouter {
	return &ChallengeRouter{
		allowedID:    allowedChatID,
		attemptStore: attemptStore,
		consumers:    make(map[uuid.UUID]ChallengeConsumer),
	}
}

// RegisterConsumer registers a callback waiting for an OTP for the specified attempt.
func (r *ChallengeRouter) RegisterConsumer(attemptID uuid.UUID, consumer ChallengeConsumer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.consumers[attemptID] = consumer
}

// UnregisterConsumer cleans up the consumer for an attempt.
func (r *ChallengeRouter) UnregisterConsumer(attemptID uuid.UUID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.consumers, attemptID)
}

// HasConsumer checks whether an in-memory consumer is currently registered.
func (r *ChallengeRouter) HasConsumer(attemptID uuid.UUID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.consumers[attemptID]
	return ok
}

// TelegramIncomingMessage models the essential fields of an incoming Telegram update message.
type TelegramIncomingMessage struct {
	UpdateID       int64
	MessageID      int64
	ChatID         int64
	SenderID       int64
	Text           string
	ReplyToMessage *TelegramRepliedMessage
}

// TelegramRepliedMessage models the replied-to message.
type TelegramRepliedMessage struct {
	MessageID int64
	Text      string
}

// FormatChallengeMessage builds a standardized human-readable challenge prompt for Telegram.
func FormatChallengeMessage(bankName, connectionName, challengeRef string) string {
	now := time.Now().UTC().Format("15:04 UTC")
	return fmt.Sprintf("🏦 %s Authentication\nConnection: %s\nChallenge: %s\nRequested at: %s\n\nThe bank requires a confirmation code.\n👉 Reply directly to THIS message with the code.",
		bankName, connectionName, challengeRef, now)
}

// ExtractOTP finds an isolated 4-8 digit numeric code in text.
func ExtractOTP(text string) (string, error) {
	trimmed := strings.TrimSpace(text)
	match := reDigits.FindString(trimmed)
	if match == "" {
		return "", ErrInvalidOTPFormat
	}
	return match, nil
}

// HandleIncomingMessage processes an incoming Telegram message, verifies allowlists, correlates replies,
// enforces durable PostgreSQL atomic state transition to verifying_otp, and securely delivers the OTP.
func (r *ChallengeRouter) HandleIncomingMessage(ctx context.Context, msg *TelegramIncomingMessage) (*personal.AuthAttempt, error) {
	if msg == nil {
		return nil, errors.New("telegram: nil message")
	}

	// 1. Security Check: Validate Sender and Chat ID against allowlist
	if r.allowedID != 0 && (msg.SenderID != r.allowedID || msg.ChatID != r.allowedID) {
		return nil, ErrUnauthorizedUser
	}

	// 2. Correlation Check: Must be a direct reply to a challenge message
	if msg.ReplyToMessage == nil || msg.ReplyToMessage.MessageID == 0 {
		return nil, ErrNotAReply
	}

	repliedMsgID := msg.ReplyToMessage.MessageID

	// 3. Resolve Attempt by replied-to Message ID (under FORCE RLS via resolve_telegram_auth_challenge)
	attempt, err := r.attemptStore.FindAttemptByTelegramMessage(ctx, msg.ChatID, repliedMsgID)
	if err != nil {
		return nil, fmt.Errorf("%w: message %d", ErrChallengeNotFound, repliedMsgID)
	}

	// 4. Validate Attempt State and Expiration
	if attempt.State != personal.StateAwaitingOTP {
		return nil, ErrAlreadyConsumed
	}
	if attempt.IsExpired() {
		return nil, ErrChallengeExpired
	}

	// 5. Extract and Validate OTP Code Format
	otp, err := ExtractOTP(msg.Text)
	if err != nil {
		return nil, err
	}

	// 6. Active Consumer Check:
	// If no in-process consumer is registered to receive the OTP (e.g. process restart),
	// reject the reply and retain a recoverable challenge state in PostgreSQL instead of consuming it.
	r.mu.Lock()
	consumer, hasConsumer := r.consumers[attempt.ID]
	r.mu.Unlock()
	if !hasConsumer || consumer == nil {
		return nil, ErrNoActiveConsumer
	}

	// 7. Atomic & Durable PostgreSQL Consumption:
	// Atomically transitions state = 'verifying_otp', increments generation, and sets update_id.
	// Only ONE concurrent worker in the entire cluster will receive the updated row; others get ErrAlreadyConsumed.
	consumedAttempt, err := r.attemptStore.ConsumeAttemptByTelegramMessage(ctx, msg.ChatID, repliedMsgID, msg.UpdateID)
	if err != nil {
		if errors.Is(err, personal.ErrAttemptNotFound) {
			return nil, ErrAlreadyConsumed
		}
		return nil, fmt.Errorf("consume challenge: %w", err)
	}

	// 8. Deliver OTP strictly to the waiting attempt consumer in memory (never logged, never persisted)
	if err := consumer(ctx, consumedAttempt, otp); err != nil {
		return nil, fmt.Errorf("consumer failed: %w", err)
	}

	return consumedAttempt, nil
}

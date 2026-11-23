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
)

var reDigits = regexp.MustCompile(`\b\d{4,8}\b`)

// ChallengeConsumer is a callback function invoked when an OTP is resolved for an attempt.
// The consumer must process and immediately discard the OTP from memory.
type ChallengeConsumer func(ctx context.Context, attempt *personal.AuthAttempt, otp string) error

// ChallengeRouter routes incoming Telegram reply updates to their matching authentication attempts.
type ChallengeRouter struct {
	mu           sync.Mutex
	allowedID    int64
	attemptStore personal.AuthAttemptStore
	consumers    map[uuid.UUID]ChallengeConsumer
	consumedIDs  map[uuid.UUID]bool
}

// NewChallengeRouter creates a new Telegram challenge routing engine.
func NewChallengeRouter(allowedChatID int64, attemptStore personal.AuthAttemptStore) *ChallengeRouter {
	return &ChallengeRouter{
		allowedID:    allowedChatID,
		attemptStore: attemptStore,
		consumers:    make(map[uuid.UUID]ChallengeConsumer),
		consumedIDs:  make(map[uuid.UUID]bool),
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
	delete(r.consumedIDs, attemptID)
}

// TelegramIncomingMessage models the essential fields of an incoming Telegram update message.
type TelegramIncomingMessage struct {
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
// and securely delivers the OTP to the active authentication attempt.
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

	// 3. Resolve Attempt by replied-to Message ID
	attempt, err := r.attemptStore.FindAttemptByTelegramMessage(ctx, msg.ChatID, repliedMsgID)
	if err != nil {
		return nil, fmt.Errorf("%w: message %d", ErrChallengeNotFound, repliedMsgID)
	}

	// 4. Validate Attempt State and Expiration
	if attempt.State != personal.StateAwaitingOTP {
		return nil, fmt.Errorf("%w: attempt is in state %s", ErrChallengeExpired, attempt.State)
	}
	if attempt.IsExpired() {
		return nil, ErrChallengeExpired
	}

	// 5. Extract and Validate OTP Code Format
	otp, err := ExtractOTP(msg.Text)
	if err != nil {
		return nil, err
	}

	// 6. Check Idempotency / Single Consumption
	r.mu.Lock()
	if r.consumedIDs[attempt.ID] {
		r.mu.Unlock()
		return nil, ErrAlreadyConsumed
	}
	consumer, hasConsumer := r.consumers[attempt.ID]
	r.consumedIDs[attempt.ID] = true
	r.mu.Unlock()

	// 7. Deliver OTP strictly to the waiting attempt consumer
	if hasConsumer && consumer != nil {
		if err := consumer(ctx, attempt, otp); err != nil {
			return nil, fmt.Errorf("consumer failed: %w", err)
		}
	}

	return attempt, nil
}

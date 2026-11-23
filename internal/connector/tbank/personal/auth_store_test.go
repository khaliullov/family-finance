package personal_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/khaliullov/family-finance/internal/connector/tbank/personal"
)

func TestAuthAttemptStore_Lifecycle(t *testing.T) {
	ctx := context.Background()
	store := personal.NewMemoryAuthAttemptStore()

	familyID := uuid.New()
	connID := uuid.New()
	attempt := personal.NewAuthAttempt(familyID, &connID, "tbank_personal", 5*time.Minute)

	// 1. Create attempt
	if err := store.CreateAttempt(ctx, attempt); err != nil {
		t.Fatalf("create attempt failed: %v", err)
	}

	// 2. Load attempt
	loaded, err := store.GetAttempt(ctx, familyID, attempt.ID)
	if err != nil {
		t.Fatalf("get attempt failed: %v", err)
	}
	if loaded.State != personal.StateAuthenticating {
		t.Errorf("state = %q; want authenticating", loaded.State)
	}
	if loaded.ChallengeRef == "" {
		t.Errorf("challenge ref must not be empty")
	}

	// 3. Attach Telegram challenge
	chatID := int64(12345)
	msgID := int64(8888)
	expires := time.Now().Add(3 * time.Minute)
	if err := store.SetTelegramChallenge(ctx, familyID, attempt.ID, "sms_otp", loaded.ChallengeRef, chatID, msgID, expires); err != nil {
		t.Fatalf("set challenge failed: %v", err)
	}

	// 4. Find attempt by Telegram message
	found, err := store.FindAttemptByTelegramMessage(ctx, chatID, msgID)
	if err != nil {
		t.Fatalf("find by telegram failed: %v", err)
	}
	if found.ID != attempt.ID {
		t.Errorf("found attempt ID mismatch: %v vs %v", found.ID, attempt.ID)
	}
	if found.State != personal.StateAwaitingOTP {
		t.Errorf("state = %q; want awaiting_otp", found.State)
	}

	// 5. Valid transition: awaiting_otp -> verifying_otp
	if err := store.UpdateAttemptState(ctx, familyID, attempt.ID, personal.StateAwaitingOTP, personal.StateVerifyingOTP, ""); err != nil {
		t.Fatalf("transition to verifying_otp failed: %v", err)
	}

	// 6. Valid transition: verifying_otp -> active
	if err := store.UpdateAttemptState(ctx, familyID, attempt.ID, personal.StateVerifyingOTP, personal.StateActive, ""); err != nil {
		t.Fatalf("transition to active failed: %v", err)
	}

	// 7. Invalid transition: active -> awaiting_otp (illegal)
	err = store.UpdateAttemptState(ctx, familyID, attempt.ID, personal.StateActive, personal.StateAwaitingOTP, "")
	if err == nil || !errors.Is(err, personal.ErrInvalidStateTransition) {
		t.Fatalf("expected ErrInvalidStateTransition, got %v", err)
	}
}

func TestValidateStateTransition(t *testing.T) {
	// Legal transitions
	if err := personal.ValidateStateTransition(personal.StateAuthenticating, personal.StateAwaitingOTP); err != nil {
		t.Errorf("expected valid transition, got %v", err)
	}
	if err := personal.ValidateStateTransition(personal.StateAwaitingOTP, personal.StateVerifyingOTP); err != nil {
		t.Errorf("expected valid transition, got %v", err)
	}
	if err := personal.ValidateStateTransition(personal.StateVerifyingOTP, personal.StateActive); err != nil {
		t.Errorf("expected valid transition, got %v", err)
	}
	if err := personal.ValidateStateTransition(personal.StateActive, personal.StateExpired); err != nil {
		t.Errorf("expected valid transition, got %v", err)
	}
	if err := personal.ValidateStateTransition(personal.StateExpired, personal.StateReconnecting); err != nil {
		t.Errorf("expected valid transition, got %v", err)
	}

	// Illegal transitions
	if err := personal.ValidateStateTransition(personal.StateDisconnected, personal.StateActive); err == nil {
		t.Errorf("expected error for disconnected -> active, got nil")
	}
	if err := personal.ValidateStateTransition(personal.StateFailed, personal.StateActive); err == nil {
		t.Errorf("expected error for failed -> active, got nil")
	}
}

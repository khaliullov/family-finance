package telegram_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/khaliullov/family-finance/internal/connector/tbank/personal"
	"github.com/khaliullov/family-finance/internal/telegram"
)

func TestChallengeRouter_SuccessfulOTPResolution(t *testing.T) {
	ctx := context.Background()
	store := personal.NewMemoryAuthAttemptStore()
	allowedChatID := int64(123456789)
	router := telegram.NewChallengeRouter(allowedChatID, store)

	familyID := uuid.New()
	connID := uuid.New()
	attempt := personal.NewAuthAttempt(familyID, &connID, "tbank_personal", 5*time.Minute)
	if err := store.CreateAttempt(ctx, attempt); err != nil {
		t.Fatal(err)
	}

	challengeMsgID := int64(9001)
	if err := store.SetTelegramChallenge(ctx, familyID, attempt.ID, "sms_otp", attempt.ChallengeRef, allowedChatID, challengeMsgID, time.Now().Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}

	var receivedOTP string
	var consumerMu sync.Mutex
	router.RegisterConsumer(attempt.ID, func(ctx context.Context, a *personal.AuthAttempt, otp string) error {
		consumerMu.Lock()
		receivedOTP = otp
		consumerMu.Unlock()
		return nil
	})

	incomingMsg := &telegram.TelegramIncomingMessage{
		MessageID: 9002,
		ChatID:    allowedChatID,
		SenderID:  allowedChatID,
		Text:      "Код подтверждения: 849201",
		ReplyToMessage: &telegram.TelegramRepliedMessage{
			MessageID: challengeMsgID,
		},
	}

	resolvedAttempt, err := router.HandleIncomingMessage(ctx, incomingMsg)
	if err != nil {
		t.Fatalf("expected successful resolution, got: %v", err)
	}
	if resolvedAttempt.ID != attempt.ID {
		t.Fatalf("resolved attempt ID mismatch: got %v, want %v", resolvedAttempt.ID, attempt.ID)
	}

	consumerMu.Lock()
	defer consumerMu.Unlock()
	if receivedOTP != "849201" {
		t.Fatalf("expected OTP 849201, got %q", receivedOTP)
	}
}

func TestChallengeRouter_UnauthorizedSenderRejected(t *testing.T) {
	ctx := context.Background()
	store := personal.NewMemoryAuthAttemptStore()
	allowedChatID := int64(123456789)
	router := telegram.NewChallengeRouter(allowedChatID, store)

	incomingMsg := &telegram.TelegramIncomingMessage{
		MessageID: 9002,
		ChatID:    999999999, // Unauthorized
		SenderID:  999999999,
		Text:      "123456",
		ReplyToMessage: &telegram.TelegramRepliedMessage{
			MessageID: 9001,
		},
	}

	_, err := router.HandleIncomingMessage(ctx, incomingMsg)
	if !errors.Is(err, telegram.ErrUnauthorizedUser) {
		t.Fatalf("expected ErrUnauthorizedUser, got %v", err)
	}
}

func TestChallengeRouter_NotAReplyRejected(t *testing.T) {
	ctx := context.Background()
	store := personal.NewMemoryAuthAttemptStore()
	allowedChatID := int64(123456789)
	router := telegram.NewChallengeRouter(allowedChatID, store)

	incomingMsg := &telegram.TelegramIncomingMessage{
		MessageID:      9002,
		ChatID:         allowedChatID,
		SenderID:       allowedChatID,
		Text:           "123456",
		ReplyToMessage: nil, // Not a reply
	}

	_, err := router.HandleIncomingMessage(ctx, incomingMsg)
	if !errors.Is(err, telegram.ErrNotAReply) {
		t.Fatalf("expected ErrNotAReply, got %v", err)
	}
}

func TestChallengeRouter_ExpiredChallengeRejected(t *testing.T) {
	ctx := context.Background()
	store := personal.NewMemoryAuthAttemptStore()
	allowedChatID := int64(123456789)
	router := telegram.NewChallengeRouter(allowedChatID, store)

	familyID := uuid.New()
	attempt := personal.NewAuthAttempt(familyID, nil, "tbank_personal", -1*time.Minute) // Already expired
	if err := store.CreateAttempt(ctx, attempt); err != nil {
		t.Fatal(err)
	}

	challengeMsgID := int64(9001)
	_ = store.SetTelegramChallenge(ctx, familyID, attempt.ID, "sms_otp", "X1Y2", allowedChatID, challengeMsgID, time.Now().Add(-1*time.Minute))

	incomingMsg := &telegram.TelegramIncomingMessage{
		MessageID: 9002,
		ChatID:    allowedChatID,
		SenderID:  allowedChatID,
		Text:      "123456",
		ReplyToMessage: &telegram.TelegramRepliedMessage{
			MessageID: challengeMsgID,
		},
	}

	_, err := router.HandleIncomingMessage(ctx, incomingMsg)
	if !errors.Is(err, telegram.ErrChallengeNotFound) && !errors.Is(err, telegram.ErrChallengeExpired) {
		t.Fatalf("expected challenge expired or not found, got %v", err)
	}
}

func TestChallengeRouter_MultiChallengeConcurrentRouting(t *testing.T) {
	ctx := context.Background()
	store := personal.NewMemoryAuthAttemptStore()
	allowedChatID := int64(123456789)
	router := telegram.NewChallengeRouter(allowedChatID, store)

	familyID := uuid.New()

	// Connection 1: T-Bank #1 (Challenge msg 101)
	conn1ID := uuid.New()
	att1 := personal.NewAuthAttempt(familyID, &conn1ID, "tbank_personal", 5*time.Minute)
	_ = store.CreateAttempt(ctx, att1)
	_ = store.SetTelegramChallenge(ctx, familyID, att1.ID, "sms_otp", "C111", allowedChatID, 101, time.Now().Add(5*time.Minute))

	// Connection 2: T-Bank #2 (Challenge msg 201)
	conn2ID := uuid.New()
	att2 := personal.NewAuthAttempt(familyID, &conn2ID, "tbank_personal", 5*time.Minute)
	_ = store.CreateAttempt(ctx, att2)
	_ = store.SetTelegramChallenge(ctx, familyID, att2.ID, "sms_otp", "C222", allowedChatID, 201, time.Now().Add(5*time.Minute))

	var otp1, otp2 string
	router.RegisterConsumer(att1.ID, func(ctx context.Context, a *personal.AuthAttempt, otp string) error {
		otp1 = otp
		return nil
	})
	router.RegisterConsumer(att2.ID, func(ctx context.Context, a *personal.AuthAttempt, otp string) error {
		otp2 = otp
		return nil
	})

	// User replies to Challenge 2 first
	msg2 := &telegram.TelegramIncomingMessage{
		MessageID: 202,
		ChatID:    allowedChatID,
		SenderID:  allowedChatID,
		Text:      "998877",
		ReplyToMessage: &telegram.TelegramRepliedMessage{
			MessageID: 201,
		},
	}
	res2, err := router.HandleIncomingMessage(ctx, msg2)
	if err != nil || res2.ID != att2.ID {
		t.Fatalf("reply to challenge 2 failed: %v", err)
	}

	// User replies to Challenge 1 second
	msg1 := &telegram.TelegramIncomingMessage{
		MessageID: 102,
		ChatID:    allowedChatID,
		SenderID:  allowedChatID,
		Text:      "112233",
		ReplyToMessage: &telegram.TelegramRepliedMessage{
			MessageID: 101,
		},
	}
	res1, err := router.HandleIncomingMessage(ctx, msg1)
	if err != nil || res1.ID != att1.ID {
		t.Fatalf("reply to challenge 1 failed: %v", err)
	}

	if otp1 != "112233" || otp2 != "998877" {
		t.Fatalf("routing cross-talk detected! otp1=%q, otp2=%q", otp1, otp2)
	}
}

func TestChallengeRouter_SingleConsumptionEnforced(t *testing.T) {
	ctx := context.Background()
	store := personal.NewMemoryAuthAttemptStore()
	allowedChatID := int64(123456789)
	router := telegram.NewChallengeRouter(allowedChatID, store)

	familyID := uuid.New()
	attempt := personal.NewAuthAttempt(familyID, nil, "tbank_personal", 5*time.Minute)
	_ = store.CreateAttempt(ctx, attempt)
	_ = store.SetTelegramChallenge(ctx, familyID, attempt.ID, "sms_otp", "S111", allowedChatID, 501, time.Now().Add(5*time.Minute))

	router.RegisterConsumer(attempt.ID, func(ctx context.Context, a *personal.AuthAttempt, otp string) error {
		return nil
	})

	msg := &telegram.TelegramIncomingMessage{
		MessageID: 502,
		ChatID:    allowedChatID,
		SenderID:  allowedChatID,
		Text:      "555555",
		ReplyToMessage: &telegram.TelegramRepliedMessage{
			MessageID: 501,
		},
	}

	// First delivery: success
	_, err := router.HandleIncomingMessage(ctx, msg)
	if err != nil {
		t.Fatalf("first delivery failed: %v", err)
	}

	// Second delivery (duplicate update / retry): must fail with ErrAlreadyConsumed
	_, err = router.HandleIncomingMessage(ctx, msg)
	if !errors.Is(err, telegram.ErrAlreadyConsumed) {
		t.Fatalf("expected ErrAlreadyConsumed on duplicate update, got: %v", err)
	}
}

func TestChallengeRouter_NoActiveConsumerRetainsRecoverableState(t *testing.T) {
	ctx := context.Background()
	store := personal.NewMemoryAuthAttemptStore()
	allowedChatID := int64(123456789)
	router := telegram.NewChallengeRouter(allowedChatID, store)

	familyID := uuid.New()
	attempt := personal.NewAuthAttempt(familyID, nil, "tbank_personal", 5*time.Minute)
	_ = store.CreateAttempt(ctx, attempt)
	_ = store.SetTelegramChallenge(ctx, familyID, attempt.ID, "sms_otp", "S222", allowedChatID, 601, time.Now().Add(5*time.Minute))

	// Do NOT register any consumer (e.g. simulating process restart or missing handler)
	msg := &telegram.TelegramIncomingMessage{
		UpdateID:  1001,
		MessageID: 602,
		ChatID:    allowedChatID,
		SenderID:  allowedChatID,
		Text:      "123456",
		ReplyToMessage: &telegram.TelegramRepliedMessage{
			MessageID: 601,
		},
	}

	_, err := router.HandleIncomingMessage(ctx, msg)
	if !errors.Is(err, telegram.ErrNoActiveConsumer) {
		t.Fatalf("expected ErrNoActiveConsumer, got %v", err)
	}

	// Verify the database/store state was NOT consumed and remains in awaiting_otp (recoverable)
	att, err := store.FindAttemptByTelegramMessage(ctx, allowedChatID, 601)
	if err != nil {
		t.Fatalf("challenge should still be findable in recoverable state: %v", err)
	}
	if att.State != personal.StateAwaitingOTP {
		t.Fatalf("challenge state should remain awaiting_otp, got %s", att.State)
	}
}

func TestChallengeRouter_SimultaneousDuplicateReplies(t *testing.T) {
	ctx := context.Background()
	store := personal.NewMemoryAuthAttemptStore()
	allowedChatID := int64(123456789)

	// Create two router instances representing two app server replicas
	replica1 := telegram.NewChallengeRouter(allowedChatID, store)
	replica2 := telegram.NewChallengeRouter(allowedChatID, store)

	familyID := uuid.New()
	attempt := personal.NewAuthAttempt(familyID, nil, "tbank_personal", 5*time.Minute)
	_ = store.CreateAttempt(ctx, attempt)
	_ = store.SetTelegramChallenge(ctx, familyID, attempt.ID, "sms_otp", "S333", allowedChatID, 701, time.Now().Add(5*time.Minute))

	var replica1Received, replica2Received bool
	var mu sync.Mutex

	replica1.RegisterConsumer(attempt.ID, func(ctx context.Context, a *personal.AuthAttempt, otp string) error {
		mu.Lock()
		replica1Received = true
		mu.Unlock()
		return nil
	})
	replica2.RegisterConsumer(attempt.ID, func(ctx context.Context, a *personal.AuthAttempt, otp string) error {
		mu.Lock()
		replica2Received = true
		mu.Unlock()
		return nil
	})

	msg := &telegram.TelegramIncomingMessage{
		UpdateID:  2001,
		MessageID: 702,
		ChatID:    allowedChatID,
		SenderID:  allowedChatID,
		Text:      "654321",
		ReplyToMessage: &telegram.TelegramRepliedMessage{
			MessageID: 701,
		},
	}

	// Execute simultaneous replies on both replicas
	var wg sync.WaitGroup
	var err1, err2 error
	wg.Add(2)

	go func() {
		defer wg.Done()
		_, err1 = replica1.HandleIncomingMessage(ctx, msg)
	}()
	go func() {
		defer wg.Done()
		_, err2 = replica2.HandleIncomingMessage(ctx, msg)
	}()

	wg.Wait()

	// Exactly ONE replica must succeed, and the other must receive ErrAlreadyConsumed
	successCount := 0
	consumedCount := 0
	if err1 == nil {
		successCount++
	} else if errors.Is(err1, telegram.ErrAlreadyConsumed) {
		consumedCount++
	}
	if err2 == nil {
		successCount++
	} else if errors.Is(err2, telegram.ErrAlreadyConsumed) {
		consumedCount++
	}

	if successCount != 1 || consumedCount != 1 {
		t.Fatalf("expected exactly 1 winner and 1 ErrAlreadyConsumed; got success=%d, consumed=%d (err1=%v, err2=%v)",
			successCount, consumedCount, err1, err2)
	}

	mu.Lock()
	defer mu.Unlock()
	if !(replica1Received != replica2Received) {
		t.Fatalf("expected only one replica consumer to receive OTP, got r1=%v, r2=%v",
			replica1Received, replica2Received)
	}
}

func TestChallengeRouter_CrossFamilyIsolation(t *testing.T) {
	ctx := context.Background()
	store := personal.NewMemoryAuthAttemptStore()
	allowedChatID := int64(123456789)
	router := telegram.NewChallengeRouter(allowedChatID, store)

	familyA := uuid.New()
	familyB := uuid.New()

	// Family A has challenge 801
	attA := personal.NewAuthAttempt(familyA, nil, "tbank_personal", 5*time.Minute)
	_ = store.CreateAttempt(ctx, attA)
	_ = store.SetTelegramChallenge(ctx, familyA, attA.ID, "sms_otp", "A801", allowedChatID, 801, time.Now().Add(5*time.Minute))

	// Family B has challenge 901
	attB := personal.NewAuthAttempt(familyB, nil, "tbank_personal", 5*time.Minute)
	_ = store.CreateAttempt(ctx, attB)
	_ = store.SetTelegramChallenge(ctx, familyB, attB.ID, "sms_otp", "B901", allowedChatID, 901, time.Now().Add(5*time.Minute))

	var consumerAFired, consumerBFired bool
	router.RegisterConsumer(attA.ID, func(ctx context.Context, a *personal.AuthAttempt, otp string) error {
		consumerAFired = true
		if a.FamilyID != familyA {
			t.Errorf("expected Family A, got %v", a.FamilyID)
		}
		return nil
	})
	router.RegisterConsumer(attB.ID, func(ctx context.Context, a *personal.AuthAttempt, otp string) error {
		consumerBFired = true
		if a.FamilyID != familyB {
			t.Errorf("expected Family B, got %v", a.FamilyID)
		}
		return nil
	})

	// Reply to Family A's challenge
	msgA := &telegram.TelegramIncomingMessage{
		UpdateID:  3001,
		MessageID: 802,
		ChatID:    allowedChatID,
		SenderID:  allowedChatID,
		Text:      "111111",
		ReplyToMessage: &telegram.TelegramRepliedMessage{
			MessageID: 801,
		},
	}

	resA, err := router.HandleIncomingMessage(ctx, msgA)
	if err != nil {
		t.Fatalf("reply to A failed: %v", err)
	}
	if resA.FamilyID != familyA {
		t.Fatalf("tenant cross-talk: expected family A (%v), got %v", familyA, resA.FamilyID)
	}
	if !consumerAFired || consumerBFired {
		t.Fatalf("expected only consumer A to fire, got A=%v, B=%v", consumerAFired, consumerBFired)
	}
}

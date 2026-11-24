package personal_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/khaliullov/family-finance/internal/connector/tbank/personal"
	"github.com/khaliullov/family-finance/internal/crypto"
)

func TestMemorySessionStore_Lifecycle(t *testing.T) {
	ctx := context.Background()
	key, err := crypto.GenerateRandomKey()
	if err != nil {
		t.Fatal(err)
	}

	store := personal.NewMemorySessionStore(key)
	familyID := uuid.New()
	connID := uuid.New()

	// 1. Initial lookup -> not found
	_, err = store.GetSession(ctx, familyID, connID)
	if err != personal.ErrSessionNotFound {
		t.Fatalf("expected ErrSessionNotFound, got %v", err)
	}

	// 2. Save session
	sess := personal.NewSession(familyID, connID, []*http.Cookie{
		{Name: "sessionid", Value: "cookie_value_123"},
	}, 2*time.Hour)

	if err := store.SaveSession(ctx, familyID, connID, sess); err != nil {
		t.Fatalf("save session failed: %v", err)
	}

	// 3. Retrieve session
	loaded, err := store.GetSession(ctx, familyID, connID)
	if err != nil {
		t.Fatalf("get session failed: %v", err)
	}
	if loaded.Status != personal.SessionStatusActive {
		t.Errorf("status = %q; want active", loaded.Status)
	}
	if len(loaded.Cookies) != 1 || loaded.Cookies[0].Value != "cookie_value_123" {
		t.Errorf("cookies mismatch: %+v", loaded.Cookies)
	}

	// 4. Invalidate session
	if err := store.InvalidateSession(ctx, familyID, connID); err != nil {
		t.Fatalf("invalidate session failed: %v", err)
	}

	loaded2, err := store.GetSession(ctx, familyID, connID)
	if err != nil {
		t.Fatalf("get session after invalidate failed: %v", err)
	}
	if loaded2.Status != personal.SessionStatusExpired {
		t.Errorf("status = %q; want expired", loaded2.Status)
	}
	if !loaded2.IsExpired() {
		t.Errorf("IsExpired() must return true for invalidated session")
	}
}

func TestSession_EncryptionRoundTrip(t *testing.T) {
	key, err := crypto.GenerateRandomKey()
	if err != nil {
		t.Fatal(err)
	}

	familyID := uuid.New()
	connID := uuid.New()
	sess := personal.NewSession(familyID, connID, []*http.Cookie{
		{Name: "sessionid", Value: "sec_abc_123"},
		{Name: "w-csrf-token", Value: "csrf_456"},
	}, 12*time.Hour)

	ciphertext, err := sess.Encrypt(key)
	if err != nil {
		t.Fatalf("session encrypt failed: %v", err)
	}

	decrypted, err := personal.DecryptSession(key, ciphertext)
	if err != nil {
		t.Fatalf("session decrypt failed: %v", err)
	}

	if decrypted.FamilyID != familyID || decrypted.ConnectionID != connID {
		t.Errorf("tenant metadata mismatch: %+v", decrypted)
	}
	if len(decrypted.Cookies) != 2 {
		t.Fatalf("cookies count = %d, want 2", len(decrypted.Cookies))
	}
	if decrypted.Cookies[0].Value != "sec_abc_123" {
		t.Errorf("cookie[0] = %q, want sec_abc_123", decrypted.Cookies[0].Value)
	}
}

func TestMemorySessionStore_CASConflict(t *testing.T) {
	ctx := context.Background()
	key, _ := crypto.GenerateRandomKey()
	store := personal.NewMemorySessionStore(key)

	familyID := uuid.New()
	connID := uuid.New()

	sess1 := personal.NewSession(familyID, connID, []*http.Cookie{
		{Name: "sessionid", Value: "init_cookie"},
	}, time.Hour)
	// sess1.Version starts at 1

	if err := store.SaveSession(ctx, familyID, connID, sess1); err != nil {
		t.Fatalf("first save failed: %v", err)
	}
	// Stored session now has Version 2

	// Load session into two concurrent references
	clientA, err := store.GetSession(ctx, familyID, connID)
	if err != nil {
		t.Fatal(err)
	}
	clientB, err := store.GetSession(ctx, familyID, connID)
	if err != nil {
		t.Fatal(err)
	}

	// Client A successfully saves (clientA.Version=2 -> stored Version 3)
	clientA.Cookies = []personal.SerializableCookie{{Name: "sessionid", Value: "cookie_A"}}
	if err := store.SaveSession(ctx, familyID, connID, clientA); err != nil {
		t.Fatalf("client A save failed: %v", err)
	}

	// Client B tries to save with stale version (clientB.Version=2, but store is now at 3)
	clientB.Cookies = []personal.SerializableCookie{{Name: "sessionid", Value: "cookie_B"}}
	err = store.SaveSession(ctx, familyID, connID, clientB)
	if err == nil || err != personal.ErrSessionStoreConflict {
		t.Fatalf("expected ErrSessionStoreConflict for stale client B update, got %v", err)
	}
}

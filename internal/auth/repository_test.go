package auth

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/khaliullov/family-finance/internal/database"
)

func TestRepository_RegisterBootstrap(t *testing.T) {
	ctx := context.Background()
	pool := getAppPool(t)
	defer pool.Close()

	repo := NewRepository(pool)

	uniqueID := uuid.New().String()[:8]
	email := fmt.Sprintf("user-%s@domain.test", uniqueID)
	rawPass := "MySecretPassword123!"
	passHash, err := HashPassword(rawPass)
	if err != nil {
		t.Fatalf("HashPassword failed: %v", err)
	}

	_, rawRefreshHash, err := GenerateRefreshToken()
	if err != nil {
		t.Fatalf("GenerateRefreshToken failed: %v", err)
	}

	params := RegisterBootstrapParams{
		Email:            email,
		PasswordHash:     passHash,
		FirstName:        "Alexander",
		LastName:         "Ivanov",
		FamilyName:       "Ivanov Space",
		DefaultCurrency:  "RUB",
		UserAgent:        "GoTest/1.0",
		IPAddress:        "127.0.0.1",
		RefreshTokenHash: rawRefreshHash,
		SessionExpiresAt: time.Now().Add(DefaultRefreshTokenTTL),
	}

	res, err := repo.RegisterBootstrap(ctx, params)
	if err != nil {
		t.Fatalf("RegisterBootstrap failed: %v", err)
	}

	if res.User.Email != email {
		t.Errorf("expected email %q, got %q", email, res.User.Email)
	}
	if res.Family.Name != "Ivanov Space" {
		t.Errorf("expected family name 'Ivanov Space', got %q", res.Family.Name)
	}
	if res.Role != "owner" {
		t.Errorf("expected role 'owner', got %q", res.Role)
	}

	// Verify categories were seeded for this family
	var catCount int
	err = database.WithFamilyUserTx(ctx, pool, res.Family.ID, res.User.ID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT count(*) FROM public.categories WHERE family_id = $1 AND is_system = TRUE", res.Family.ID).Scan(&catCount)
	})
	if err != nil || catCount != 12 {
		t.Fatalf("expected 12 seeded system categories, got %d, err=%v", catCount, err)
	}

	// Test duplicate email rejection
	_, err = repo.RegisterBootstrap(ctx, params)
	if !errors.Is(err, ErrEmailAlreadyExists) {
		t.Fatalf("expected ErrEmailAlreadyExists on duplicate email, got %v", err)
	}
}

func TestRepository_GetCredential(t *testing.T) {
	ctx := context.Background()
	pool := getAppPool(t)
	defer pool.Close()

	repo := NewRepository(pool)

	uniqueID := uuid.New().String()[:8]
	email := fmt.Sprintf("cred-%s@domain.test", uniqueID)
	rawPass := "MySecretPassword123!"
	passHash, _ := HashPassword(rawPass)
	_, refreshHash, _ := GenerateRefreshToken()

	res, err := repo.RegisterBootstrap(ctx, RegisterBootstrapParams{
		Email:            email,
		PasswordHash:     passHash,
		FirstName:        "Cred",
		FamilyName:       "Cred Family",
		RefreshTokenHash: refreshHash,
		SessionExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("setup registration failed: %v", err)
	}

	// 1. Existing user
	cred, err := repo.GetCredential(ctx, email)
	if err != nil {
		t.Fatalf("GetCredential failed: %v", err)
	}
	if cred == nil {
		t.Fatalf("expected credential for existing user, got nil")
	}
	if cred.UserID != res.User.ID {
		t.Errorf("expected UserID %v, got %v", res.User.ID, cred.UserID)
	}
	if cred.PasswordHash != passHash {
		t.Errorf("expected hash %q, got %q", passHash, cred.PasswordHash)
	}
	if !cred.IsActive {
		t.Errorf("expected user to be active")
	}

	// 2. Case-insensitive and trimmed lookup
	credUpper, err := repo.GetCredential(ctx, "  "+email+"  ")
	if err != nil || credUpper == nil {
		t.Fatalf("expected case-insensitive and trimmed lookup to succeed")
	}

	// 3. Non-existent user
	nonCred, err := repo.GetCredential(ctx, "nonexistent@doesnotexist.test")
	if err != nil {
		t.Fatalf("expected nil error for nonexistent user, got %v", err)
	}
	if nonCred != nil {
		t.Fatalf("expected nil credential for nonexistent user, got %v", nonCred)
	}
}

func TestRepository_SessionRotationAndReplay(t *testing.T) {
	ctx := context.Background()
	pool := getAppPool(t)
	defer pool.Close()

	repo := NewRepository(pool)

	uniqueID := uuid.New().String()[:8]
	email := fmt.Sprintf("replay-%s@domain.test", uniqueID)
	passHash, _ := HashPassword("SecretPass123!")

	rawToken1, tokenHash1, _ := GenerateRefreshToken()
	exp1 := time.Now().Add(1 * time.Hour)

	res, err := repo.RegisterBootstrap(ctx, RegisterBootstrapParams{
		Email:            email,
		PasswordHash:     passHash,
		FirstName:        "Replay",
		FamilyName:       "Replay Fam",
		RefreshTokenHash: tokenHash1,
		SessionExpiresAt: exp1,
		UserAgent:        "TestAgent/1.0",
		IPAddress:        "127.0.0.1",
	})
	if err != nil {
		t.Fatalf("registration failed: %v", err)
	}

	// 1. Successful rotation from token 1 to token 2
	_, tokenHash2, _ := GenerateRefreshToken()
	exp2 := time.Now().Add(2 * time.Hour)

	rotRes, err := repo.RotateSession(ctx, tokenHash1, tokenHash2, exp2, "TestAgent/2.0", "127.0.0.2")
	if err != nil {
		t.Fatalf("RotateSession (1 -> 2) failed: %v", err)
	}
	if rotRes.UserID != res.User.ID {
		t.Errorf("expected UserID %v, got %v", res.User.ID, rotRes.UserID)
	}
	if rotRes.FamilyID != res.Family.ID {
		t.Errorf("expected FamilyID %v, got %v", res.Family.ID, rotRes.FamilyID)
	}
	if rotRes.SessionChainID != res.SessionChainID {
		t.Errorf("expected same session chain %v, got %v", res.SessionChainID, rotRes.SessionChainID)
	}

	// 2. Replay detection: Attempting to rotate token 1 AGAIN!
	_, tokenHashReplay, _ := GenerateRefreshToken()
	_, err = repo.RotateSession(ctx, tokenHash1, tokenHashReplay, time.Now().Add(time.Hour), "AttackerAgent", "10.0.0.1")
	if !errors.Is(err, ErrSessionReplayDetected) {
		t.Fatalf("expected ErrSessionReplayDetected when re-using token 1, got %v", err)
	}

	// 3. Since replay was detected, token 2 should NOW ALSO BE REVOKED!
	_, tokenHash3, _ := GenerateRefreshToken()
	_, err = repo.RotateSession(ctx, tokenHash2, tokenHash3, time.Now().Add(time.Hour), "TestAgent/3.0", "127.0.0.3")
	if !errors.Is(err, ErrSessionRevoked) {
		t.Fatalf("expected ErrSessionRevoked for token 2 after replay revocation, got %v", err)
	}

	// 4. Rotating nonexistent token returns ErrSessionNotFound
	_, err = repo.RotateSession(ctx, "0000000000000000000000000000000000000000000000000000000000000000", "new", time.Now().Add(time.Hour), "", "")
	if !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("expected ErrSessionNotFound for non-existent token, got %v", err)
	}

	_ = rawToken1
}

func TestRepository_RevokeSessionByTokenAndList(t *testing.T) {
	ctx := context.Background()
	pool := getAppPool(t)
	defer pool.Close()

	repo := NewRepository(pool)

	uniqueID := uuid.New().String()[:8]
	email := fmt.Sprintf("revoke-%s@domain.test", uniqueID)
	passHash, _ := HashPassword("SecretPass123!")

	_, tokenHash, _ := GenerateRefreshToken()

	res, err := repo.RegisterBootstrap(ctx, RegisterBootstrapParams{
		Email:            email,
		PasswordHash:     passHash,
		FirstName:        "Revoker",
		FamilyName:       "Revoker Fam",
		RefreshTokenHash: tokenHash,
		SessionExpiresAt: time.Now().Add(10 * time.Hour),
		UserAgent:        "Mozilla/5.0",
		IPAddress:        "192.168.1.1",
	})
	if err != nil {
		t.Fatalf("registration failed: %v", err)
	}

	// 1. List active sessions: should have 1
	sessions, err := repo.ListActiveSessions(ctx, res.User.ID)
	if err != nil {
		t.Fatalf("ListActiveSessions failed: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 active session, got %d", len(sessions))
	}
	if sessions[0].ID != res.SessionID {
		t.Errorf("expected session ID %v, got %v", res.SessionID, sessions[0].ID)
	}

	// 2. Revoke session by token (e.g. logout)
	err = repo.RevokeSessionByToken(ctx, tokenHash)
	if err != nil {
		t.Fatalf("RevokeSessionByToken failed: %v", err)
	}

	// 3. List active sessions: should now be 0
	sessions, err = repo.ListActiveSessions(ctx, res.User.ID)
	if err != nil {
		t.Fatalf("ListActiveSessions after revocation failed: %v", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("expected 0 active sessions after revocation, got %d", len(sessions))
	}

	// 4. Revoking again returns ErrSessionNotFound (already revoked or not found)
	err = repo.RevokeSessionByToken(ctx, "nonexistent-token-hash")
	if !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("expected ErrSessionNotFound, got %v", err)
	}
}

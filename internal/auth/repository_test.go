package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
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
	nonExistentHash := strings.Repeat("0", 64)
	validNewHash := strings.Repeat("a", 64)
	_, err = repo.RotateSession(ctx, nonExistentHash, validNewHash, time.Now().Add(time.Hour), "", "")
	if !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("expected ErrSessionNotFound for non-existent token, got %v", err)
	}

	// 5. Invalid hash input validation
	_, err = repo.RotateSession(ctx, "too-short", validNewHash, time.Now().Add(time.Hour), "", "")
	if !errors.Is(err, ErrInvalidSessionInput) {
		t.Fatalf("expected ErrInvalidSessionInput for invalid hash, got %v", err)
	}

	// 6. Invalid non-hex characters in 64-char string returns ErrInvalidSessionInput
	invalidHexHash := strings.Repeat("z", 64)
	_, err = repo.RotateSession(ctx, invalidHexHash, validNewHash, time.Now().Add(time.Hour), "", "")
	if !errors.Is(err, ErrInvalidSessionInput) {
		t.Fatalf("expected ErrInvalidSessionInput for non-hex hash, got %v", err)
	}
	_, err = repo.RotateSession(ctx, nonExistentHash, invalidHexHash, time.Now().Add(time.Hour), "", "")
	if !errors.Is(err, ErrInvalidSessionInput) {
		t.Fatalf("expected ErrInvalidSessionInput for non-hex new hash, got %v", err)
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

func TestRepository_ReplayRevocation_PersistsInIndependentTransaction(t *testing.T) {
	ctx := context.Background()
	pool := getAppPool(t)
	defer pool.Close()

	repo := NewRepository(pool)

	uniqueID := uuid.New().String()[:8]
	email := fmt.Sprintf("persist-replay-%s@domain.test", uniqueID)
	passHash, _ := HashPassword("SecretPass123!")
	_, tokenHash1, _ := GenerateRefreshToken()

	res, err := repo.RegisterBootstrap(ctx, RegisterBootstrapParams{
		Email:            email,
		PasswordHash:     passHash,
		FirstName:        "ReplayPersist",
		FamilyName:       "Persist Fam",
		RefreshTokenHash: tokenHash1,
		SessionExpiresAt: time.Now().Add(1 * time.Hour),
	})
	if err != nil {
		t.Fatalf("registration failed: %v", err)
	}

	// Rotate token 1 -> token 2
	_, tokenHash2, _ := GenerateRefreshToken()
	rotRes, err := repo.RotateSession(ctx, tokenHash1, tokenHash2, time.Now().Add(2*time.Hour), "Agent/1.0", "127.0.0.1")
	if err != nil {
		t.Fatalf("RotateSession failed: %v", err)
	}

	// Trigger replay with token 1
	_, tokenHashReplay, _ := GenerateRefreshToken()
	_, err = repo.RotateSession(ctx, tokenHash1, tokenHashReplay, time.Now().Add(1*time.Hour), "Attacker", "10.0.0.1")
	if !errors.Is(err, ErrSessionReplayDetected) {
		t.Fatalf("expected ErrSessionReplayDetected, got %v", err)
	}

	// Open a completely separate independent connection & transaction to inspect final database state
	independentPool := getAppPool(t)
	defer independentPool.Close()

	err = database.WithUserTx(ctx, independentPool, res.User.ID, func(tx pgx.Tx) error {
		query := `SELECT id, is_revoked, is_rotated, revoked_at FROM public.user_sessions WHERE session_chain_id = $1`
		rows, err := tx.Query(ctx, query, rotRes.SessionChainID)
		if err != nil {
			return err
		}
		defer rows.Close()

		var count int
		for rows.Next() {
			var sessID uuid.UUID
			var isRevoked, isRotated bool
			var revokedAt *time.Time
			if err := rows.Scan(&sessID, &isRevoked, &isRotated, &revokedAt); err != nil {
				return err
			}
			count++
			if !isRevoked {
				t.Errorf("session %v was NOT revoked after replay detection", sessID)
			}
			if revokedAt == nil {
				t.Errorf("session %v revoked_at timestamp is NULL", sessID)
			}
		}
		if count != 2 {
			t.Errorf("expected 2 sessions in chain, got %d", count)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("failed to query session state from independent transaction: %v", err)
	}

	// Subsequent rotation attempt with token 2 must fail as revoked
	_, tokenHash3, _ := GenerateRefreshToken()
	_, err = repo.RotateSession(ctx, tokenHash2, tokenHash3, time.Now().Add(time.Hour), "Agent/2.0", "127.0.0.1")
	if !errors.Is(err, ErrSessionRevoked) {
		t.Fatalf("expected ErrSessionRevoked for token 2, got %v", err)
	}
}

func TestRepository_AtomicRollback_AllStages(t *testing.T) {
	ctx := context.Background()
	pool := getAppPool(t)
	defer pool.Close()

	famID := uuid.New()
	userID := uuid.New()

	// Simulate a failure midway through bootstrap (e.g. after user insert, before family commit)
	simulatedErr := errors.New("simulated error at stage 2")
	err := database.WithFamilyUserTx(ctx, pool, famID, userID, func(tx pgx.Tx) error {
		// 1. Insert user
		_, err := tx.Exec(ctx, `
			INSERT INTO public.users (id, email, password_hash, first_name, last_name, is_active)
			VALUES ($1, 'rollback-test@domain.test', 'hash', 'Test', 'User', TRUE)
		`, userID)
		if err != nil {
			return err
		}

		// 2. Insert family
		_, err = tx.Exec(ctx, `
			INSERT INTO public.families (id, name, default_currency)
			VALUES ($1, 'Rollback Family', 'RUB')
		`, famID)
		if err != nil {
			return err
		}

		// 3. Deliberately return error to trigger automatic rollback
		return simulatedErr
	})

	if !errors.Is(err, simulatedErr) {
		t.Fatalf("expected simulatedErr, got %v", err)
	}

	// Verify ZERO orphaned records remain in users or families
	var userCount, familyCount int
	_ = pool.QueryRow(ctx, "SELECT count(*) FROM public.users WHERE id = $1", userID).Scan(&userCount)
	if userCount != 0 {
		t.Fatalf("atomic rollback violated: user record was not rolled back (count=%d)", userCount)
	}

	_ = database.WithFamilyTx(ctx, pool, famID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT count(*) FROM public.families WHERE id = $1", famID).Scan(&familyCount)
	})
	if familyCount != 0 {
		t.Fatalf("atomic rollback violated: family record was not rolled back (count=%d)", familyCount)
	}
}

func TestRepository_CrossUserSessionIsolation(t *testing.T) {
	ctx := context.Background()
	pool := getAppPool(t)
	defer pool.Close()

	repo := NewRepository(pool)

	passHash, _ := HashPassword("SecretPass123!")

	// 1. Register User Alice
	_, aliceTokenHash, _ := GenerateRefreshToken()
	resAlice, err := repo.RegisterBootstrap(ctx, RegisterBootstrapParams{
		Email:            fmt.Sprintf("alice-%s@domain.test", uuid.New().String()[:8]),
		PasswordHash:     passHash,
		FirstName:        "Alice",
		FamilyName:       "Alice Space",
		RefreshTokenHash: aliceTokenHash,
		SessionExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("register Alice failed: %v", err)
	}

	// 2. Register User Bob
	_, bobTokenHash, _ := GenerateRefreshToken()
	resBob, err := repo.RegisterBootstrap(ctx, RegisterBootstrapParams{
		Email:            fmt.Sprintf("bob-%s@domain.test", uuid.New().String()[:8]),
		PasswordHash:     passHash,
		FirstName:        "Bob",
		FamilyName:       "Bob Space",
		RefreshTokenHash: bobTokenHash,
		SessionExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("register Bob failed: %v", err)
	}

	// 3. User Alice lists active sessions: must see Alice's session, must NOT see Bob's
	aliceSessions, err := repo.ListActiveSessions(ctx, resAlice.User.ID)
	if err != nil {
		t.Fatalf("Alice ListActiveSessions failed: %v", err)
	}
	if len(aliceSessions) != 1 || aliceSessions[0].ID != resAlice.SessionID {
		t.Fatalf("expected Alice to see only her session, got: %+v", aliceSessions)
	}

	// 4. User Alice attempts to revoke User Bob's session ID -> must fail with ErrSessionNotFound
	err = repo.RevokeSessionByID(ctx, resAlice.User.ID, resBob.SessionID)
	if !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("expected ErrSessionNotFound when Alice attempts to revoke Bob's session, got: %v", err)
	}

	// 5. Verify Bob's session remains active and non-revoked
	bobSessions, err := repo.ListActiveSessions(ctx, resBob.User.ID)
	if err != nil {
		t.Fatalf("Bob ListActiveSessions failed: %v", err)
	}
	if len(bobSessions) != 1 || bobSessions[0].ID != resBob.SessionID {
		t.Fatalf("Bob's session was compromised: %+v", bobSessions)
	}
}

func TestSecurity_DefinerFunctions_RestrictedFromWorker(t *testing.T) {
	ctx := context.Background()
	workerPool := getWorkerPool(t)
	defer workerPool.Close()

	// 1. family_worker cannot call auth_sec.rotate_refresh_token
	err := workerPool.QueryRow(ctx, "SELECT status FROM auth_sec.rotate_refresh_token('hash', 'hash', NOW() + interval '1 hour', 'ua', 'ip')").Scan(new(string))
	if err == nil {
		t.Fatalf("security violation: family_worker was able to execute auth_sec.rotate_refresh_token!")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("expected permission denied error, got: %v", err)
	}

	// 2. family_worker cannot call auth_sec.revoke_session
	var revoked bool
	err = workerPool.QueryRow(ctx, "SELECT auth_sec.revoke_session('hash')").Scan(&revoked)
	if err == nil {
		t.Fatalf("security violation: family_worker was able to execute auth_sec.revoke_session!")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("expected permission denied error, got: %v", err)
	}

	// 3. family_worker cannot call auth_sec.get_user_auth_credential
	err = workerPool.QueryRow(ctx, "SELECT user_id FROM auth_sec.get_user_auth_credential('test@test.local')").Scan(new(uuid.UUID))
	if err == nil {
		t.Fatalf("security violation: family_worker was able to execute auth_sec.get_user_auth_credential!")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("expected permission denied error, got: %v", err)
	}
}

func TestRepository_ConcurrentRotation_RowLockSerialization(t *testing.T) {
	ctx := context.Background()
	pool := getAppPool(t)
	defer pool.Close()

	repo := NewRepository(pool)

	// Run the concurrent rotation cycle repeatedly to verify strict serialization and no race conditions
	for iteration := 0; iteration < 5; iteration++ {
		uniqueID := uuid.New().String()[:8]
		email := fmt.Sprintf("concur-row-%d-%s@example.com", iteration, uniqueID)
		passHash, _ := HashPassword("SecretPass123!")
		_, tokenHash, _ := GenerateRefreshToken()

		res, err := repo.RegisterBootstrap(ctx, RegisterBootstrapParams{
			Email:            email,
			PasswordHash:     passHash,
			FirstName:        "RowLock",
			FamilyName:       "RowLock Family",
			RefreshTokenHash: tokenHash,
			SessionExpiresAt: time.Now().Add(24 * time.Hour),
			UserAgent:        "Go-Agent",
			IPAddress:        "127.0.0.1",
		})
		if err != nil {
			t.Fatalf("iteration %d: register failed: %v", iteration, err)
		}

		concurrency := 10
		var wg sync.WaitGroup
		type result struct {
			res *RotateResult
			err error
		}
		results := make([]result, concurrency)

		for i := 0; i < concurrency; i++ {
			wg.Add(1)
			idx := i
			go func() {
				defer wg.Done()
				_, newHash, _ := GenerateRefreshToken()
				rRes, rErr := repo.RotateSession(ctx, tokenHash, newHash, time.Now().Add(24*time.Hour), "Agent", "127.0.0.1")
				results[idx] = result{res: rRes, err: rErr}
			}()
		}

		wg.Wait()

		var successCount int
		var replayCount int
		var revokedCount int
		var winningResult *RotateResult

		for _, r := range results {
			if r.err == nil && r.res != nil && r.res.Status == "SUCCESS" {
				successCount++
				winningResult = r.res
			} else if errors.Is(r.err, ErrSessionReplayDetected) {
				replayCount++
			} else if errors.Is(r.err, ErrSessionRevoked) {
				revokedCount++
			} else {
				t.Fatalf("iteration %d: unexpected error from concurrent rotation: %v", iteration, r.err)
			}
		}

		// Invariant 1: Exactly at most 1 request can succeed. Under no circumstances can > 1 succeed!
		if successCount > 1 {
			t.Fatalf("iteration %d: CRITICAL: %d concurrent requests succeeded in rotating the same token!", iteration, successCount)
		}

		// Invariant 2: All other requests encountered replay detection or chain revocation
		if successCount+replayCount+revokedCount != concurrency {
			t.Fatalf("iteration %d: unexpected result breakdown: success=%d, replay=%d, revoked=%d, total=%d",
				iteration, successCount, replayCount, revokedCount, concurrency)
		}

		// Invariant 3: Because 9 concurrent requests replayed the token, the chain was permanently revoked
		// Active sessions for the user must now be exactly 0
		activeSessions, err := repo.ListActiveSessions(ctx, res.User.ID)
		if err != nil {
			t.Fatalf("iteration %d: failed to list sessions: %v", iteration, err)
		}
		if len(activeSessions) != 0 {
			t.Fatalf("iteration %d: expected 0 active sessions after replay revocation, got %d", iteration, len(activeSessions))
		}

		// Invariant 4: If a winning token was issued to the single successful request,
		// any subsequent attempt to rotate that replacement token MUST fail with ErrSessionRevoked!
		if winningResult != nil {
			var isRevoked bool
			err := database.WithUserTx(ctx, pool, res.User.ID, func(tx pgx.Tx) error {
				return tx.QueryRow(ctx, "SELECT is_revoked FROM public.user_sessions WHERE id = $1", winningResult.SessionID).Scan(&isRevoked)
			})
			if err != nil {
				t.Fatalf("iteration %d: failed to query winning session: %v", iteration, err)
			}
			if !isRevoked {
				t.Fatalf("iteration %d: winning session %v in revoked chain was NOT marked is_revoked!", iteration, winningResult.SessionID)
			}
		}
	}
}

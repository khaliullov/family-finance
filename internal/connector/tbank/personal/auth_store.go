package personal

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/khaliullov/family-finance/internal/database"
)

// AuthAttemptStore defines the concurrency-safe storage contract for multi-bank authentication attempts.
type AuthAttemptStore interface {
	CreateAttempt(ctx context.Context, attempt *AuthAttempt) error
	GetAttempt(ctx context.Context, familyID, attemptID uuid.UUID) (*AuthAttempt, error)
	UpdateAttemptState(ctx context.Context, familyID, attemptID uuid.UUID, expectedState, newState string, errMsg string) error
	SetTelegramChallenge(ctx context.Context, familyID, attemptID uuid.UUID, challengeType, challengeRef string, chatID, msgID int64, expiresAt time.Time) error
	FindAttemptByTelegramMessage(ctx context.Context, chatID, msgID int64) (*AuthAttempt, error)
	ConsumeAttemptByTelegramMessage(ctx context.Context, chatID, msgID, updateID int64) (*AuthAttempt, error)
}

// DBAuthAttemptStore implements AuthAttemptStore against PostgreSQL public.bank_auth_attempts under RLS.
type DBAuthAttemptStore struct {
	pool *pgxpool.Pool
}

// NewDBAuthAttemptStore creates a PostgreSQL backed authentication attempt store.
func NewDBAuthAttemptStore(pool *pgxpool.Pool) *DBAuthAttemptStore {
	return &DBAuthAttemptStore{pool: pool}
}

// CreateAttempt persists a new authentication attempt.
func (s *DBAuthAttemptStore) CreateAttempt(ctx context.Context, attempt *AuthAttempt) error {
	if attempt == nil {
		return errors.New("cannot create nil attempt")
	}

	return database.WithFamilyTx(ctx, s.pool, attempt.FamilyID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO public.bank_auth_attempts (
				id, family_id, bank_connection_id, provider, state,
				challenge_type, challenge_ref, generation, expires_at, created_at, updated_at
			) VALUES (
				$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11
			)
		`, attempt.ID, attempt.FamilyID, attempt.ConnectionID, attempt.Provider, attempt.State,
			attempt.ChallengeType, attempt.ChallengeRef, attempt.Generation, attempt.ExpiresAt, attempt.CreatedAt, attempt.UpdatedAt)
		return err
	})
}

// GetAttempt retrieves an attempt strictly scoped to the tenant.
func (s *DBAuthAttemptStore) GetAttempt(ctx context.Context, familyID, attemptID uuid.UUID) (*AuthAttempt, error) {
	var a AuthAttempt
	err := database.WithFamilyTx(ctx, s.pool, familyID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT id, family_id, bank_connection_id, provider, state,
			       challenge_type, challenge_ref, telegram_message_id, telegram_chat_id,
			       generation, expires_at, COALESCE(error_message, ''), created_at, updated_at
			FROM public.bank_auth_attempts
			WHERE family_id = $1 AND id = $2
		`, familyID, attemptID)

		return row.Scan(
			&a.ID, &a.FamilyID, &a.ConnectionID, &a.Provider, &a.State,
			&a.ChallengeType, &a.ChallengeRef, &a.TelegramMessageID, &a.TelegramChatID,
			&a.Generation, &a.ExpiresAt, &a.ErrorMessage, &a.CreatedAt, &a.UpdatedAt,
		)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrAttemptNotFound
		}
		return nil, fmt.Errorf("load auth attempt: %w", err)
	}
	return &a, nil
}

// UpdateAttemptState performs a concurrency-safe atomic state transition.
func (s *DBAuthAttemptStore) UpdateAttemptState(ctx context.Context, familyID, attemptID uuid.UUID, expectedState, newState string, errMsg string) error {
	if err := ValidateStateTransition(expectedState, newState); err != nil {
		return err
	}

	return database.WithFamilyTx(ctx, s.pool, familyID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE public.bank_auth_attempts
			SET state = $1,
			    generation = generation + 1,
			    error_message = NULLIF($2, ''),
			    updated_at = NOW()
			WHERE family_id = $3 AND id = $4 AND state = $5
		`, newState, errMsg, familyID, attemptID, expectedState)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("%w: attempt state changed concurrently or expired", ErrInvalidStateTransition)
		}
		return nil
	})
}

// SetTelegramChallenge associates an active Telegram message and human challenge reference with an attempt.
func (s *DBAuthAttemptStore) SetTelegramChallenge(ctx context.Context, familyID, attemptID uuid.UUID, challengeType, challengeRef string, chatID, msgID int64, expiresAt time.Time) error {
	return database.WithFamilyTx(ctx, s.pool, familyID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE public.bank_auth_attempts
			SET state = 'awaiting_otp',
			    challenge_type = $1,
			    challenge_ref = $2,
			    telegram_chat_id = $3,
			    telegram_message_id = $4,
			    expires_at = $5,
			    generation = generation + 1,
			    updated_at = NOW()
			WHERE family_id = $6 AND id = $7 AND state IN ('authenticating', 'verifying_otp')
		`, challengeType, challengeRef, chatID, msgID, expiresAt, familyID, attemptID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("%w: cannot attach challenge in current state", ErrInvalidStateTransition)
		}
		return nil
	})
}

// FindAttemptByTelegramMessage finds an active awaiting_otp attempt matching a Telegram chat and message ID
// through the safe SECURITY DEFINER function public.resolve_telegram_auth_challenge under FORCE RLS.
func (s *DBAuthAttemptStore) FindAttemptByTelegramMessage(ctx context.Context, chatID, msgID int64) (*AuthAttempt, error) {
	var a AuthAttempt
	row := s.pool.QueryRow(ctx, `
		SELECT id, family_id, bank_connection_id, provider, state,
		       challenge_type, challenge_ref, telegram_message_id, telegram_chat_id,
		       generation, expires_at, error_message, created_at, updated_at
		FROM public.resolve_telegram_auth_challenge($1, $2)
	`, chatID, msgID)

	err := row.Scan(
		&a.ID, &a.FamilyID, &a.ConnectionID, &a.Provider, &a.State,
		&a.ChallengeType, &a.ChallengeRef, &a.TelegramMessageID, &a.TelegramChatID,
		&a.Generation, &a.ExpiresAt, &a.ErrorMessage, &a.CreatedAt, &a.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrAttemptNotFound
		}
		return nil, fmt.Errorf("lookup attempt by telegram: %w", err)
	}

	return &a, nil
}

// ConsumeAttemptByTelegramMessage atomically transitions the attempt to verifying_otp and records the update_id.
// It is backed by public.consume_telegram_auth_challenge and enforces durable CAS concurrency.
func (s *DBAuthAttemptStore) ConsumeAttemptByTelegramMessage(ctx context.Context, chatID, msgID, updateID int64) (*AuthAttempt, error) {
	var a AuthAttempt
	var updateArg *int64
	if updateID != 0 {
		updateArg = &updateID
	}

	row := s.pool.QueryRow(ctx, `
		SELECT id, family_id, bank_connection_id, provider, state,
		       challenge_type, challenge_ref, telegram_message_id, telegram_chat_id,
		       generation, expires_at, error_message, created_at, updated_at
		FROM public.consume_telegram_auth_challenge($1, $2, $3)
	`, chatID, msgID, updateArg)

	err := row.Scan(
		&a.ID, &a.FamilyID, &a.ConnectionID, &a.Provider, &a.State,
		&a.ChallengeType, &a.ChallengeRef, &a.TelegramMessageID, &a.TelegramChatID,
		&a.Generation, &a.ExpiresAt, &a.ErrorMessage, &a.CreatedAt, &a.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrAttemptNotFound
		}
		return nil, fmt.Errorf("consume attempt by telegram: %w", err)
	}

	return &a, nil
}

// MemoryAuthAttemptStore provides thread-safe in-memory storage for unit testing.
type MemoryAuthAttemptStore struct {
	mu              sync.RWMutex
	attempts        map[uuid.UUID]*AuthAttempt
	consumedUpdates map[int64]bool
}

// NewMemoryAuthAttemptStore creates an in-memory attempt store.
func NewMemoryAuthAttemptStore() *MemoryAuthAttemptStore {
	return &MemoryAuthAttemptStore{
		attempts:        make(map[uuid.UUID]*AuthAttempt),
		consumedUpdates: make(map[int64]bool),
	}
}

func (m *MemoryAuthAttemptStore) CreateAttempt(ctx context.Context, attempt *AuthAttempt) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	copyAttempt := *attempt
	m.attempts[attempt.ID] = &copyAttempt
	return nil
}

func (m *MemoryAuthAttemptStore) GetAttempt(ctx context.Context, familyID, attemptID uuid.UUID) (*AuthAttempt, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	a, ok := m.attempts[attemptID]
	if !ok || a.FamilyID != familyID {
		return nil, ErrAttemptNotFound
	}
	copyAttempt := *a
	return &copyAttempt, nil
}

func (m *MemoryAuthAttemptStore) UpdateAttemptState(ctx context.Context, familyID, attemptID uuid.UUID, expectedState, newState string, errMsg string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	a, ok := m.attempts[attemptID]
	if !ok || a.FamilyID != familyID {
		return ErrAttemptNotFound
	}

	if a.State != expectedState {
		return fmt.Errorf("%w: state is %s, want %s", ErrInvalidStateTransition, a.State, expectedState)
	}
	if err := ValidateStateTransition(expectedState, newState); err != nil {
		return err
	}

	a.State = newState
	a.ErrorMessage = errMsg
	a.Generation++
	a.UpdatedAt = time.Now().UTC()
	return nil
}

func (m *MemoryAuthAttemptStore) SetTelegramChallenge(ctx context.Context, familyID, attemptID uuid.UUID, challengeType, challengeRef string, chatID, msgID int64, expiresAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	a, ok := m.attempts[attemptID]
	if !ok || a.FamilyID != familyID {
		return ErrAttemptNotFound
	}

	a.State = StateAwaitingOTP
	a.ChallengeType = challengeType
	a.ChallengeRef = challengeRef
	a.TelegramChatID = &chatID
	a.TelegramMessageID = &msgID
	a.ExpiresAt = expiresAt
	a.Generation++
	a.UpdatedAt = time.Now().UTC()
	return nil
}

func (m *MemoryAuthAttemptStore) FindAttemptByTelegramMessage(ctx context.Context, chatID, msgID int64) (*AuthAttempt, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, a := range m.attempts {
		if a.TelegramChatID != nil && *a.TelegramChatID == chatID &&
			a.TelegramMessageID != nil && *a.TelegramMessageID == msgID {
			copyAttempt := *a
			return &copyAttempt, nil
		}
	}
	return nil, ErrAttemptNotFound
}

func (m *MemoryAuthAttemptStore) ConsumeAttemptByTelegramMessage(ctx context.Context, chatID, msgID, updateID int64) (*AuthAttempt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if updateID != 0 && m.consumedUpdates[updateID] {
		return nil, ErrAttemptNotFound
	}

	now := time.Now().UTC()
	for _, a := range m.attempts {
		if a.TelegramChatID != nil && *a.TelegramChatID == chatID &&
			a.TelegramMessageID != nil && *a.TelegramMessageID == msgID &&
			a.State == StateAwaitingOTP && now.Before(a.ExpiresAt) {

			a.State = StateVerifyingOTP
			a.Generation++
			a.UpdatedAt = now
			if updateID != 0 {
				m.consumedUpdates[updateID] = true
			}

			copyAttempt := *a
			return &copyAttempt, nil
		}
	}
	return nil, ErrAttemptNotFound
}

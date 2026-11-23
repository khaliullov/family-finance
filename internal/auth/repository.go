package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/khaliullov/family-finance/internal/database"
)

var (
	ErrUserNotFound             = errors.New("user not found")
	ErrEmailAlreadyExists       = errors.New("email is already registered")
	ErrUserInactive             = errors.New("user account is inactive")
	ErrSessionNotFound          = errors.New("session not found")
	ErrSessionRevoked           = errors.New("session has been revoked")
	ErrSessionExpired           = errors.New("session has expired")
	ErrSessionReplayDetected    = errors.New("refresh token reuse detected; session chain revoked")
	ErrFamilyMembershipNotFound = errors.New("user is not a member of the requested family")
)

type User struct {
	ID        uuid.UUID `json:"id"`
	Email     string    `json:"email"`
	FirstName string    `json:"first_name"`
	LastName  string    `json:"last_name,omitempty"`
}

type Family struct {
	ID              uuid.UUID `json:"id"`
	Name            string    `json:"name"`
	DefaultCurrency string    `json:"default_currency"`
}

type FamilyMember struct {
	ID        uuid.UUID `json:"id"`
	UserID    uuid.UUID `json:"user_id"`
	Role      string    `json:"role"`
	FirstName string    `json:"first_name"`
	LastName  string    `json:"last_name,omitempty"`
	ColorCode string    `json:"color_code,omitempty"`
}

type UserCredential struct {
	UserID       uuid.UUID
	PasswordHash string
	IsActive     bool
}

type UserFamilyMembership struct {
	FamilyID        uuid.UUID
	FamilyName      string
	DefaultCurrency string
	Role            string
}

type RegisterBootstrapParams struct {
	Email            string
	PasswordHash     string
	FirstName        string
	LastName         string
	FamilyName       string
	DefaultCurrency  string
	UserAgent        string
	IPAddress        string
	RefreshTokenHash string
	SessionExpiresAt time.Time
}

type RegisterResult struct {
	User           User
	Family         Family
	Role           string
	SessionID      uuid.UUID
	SessionChainID uuid.UUID
}

type RotateResult struct {
	Status         string
	SessionID      uuid.UUID
	UserID         uuid.UUID
	FamilyID       uuid.UUID
	SessionChainID uuid.UUID
}

type Repository interface {
	GetCredential(ctx context.Context, email string) (*UserCredential, error)
	GetUser(ctx context.Context, userID uuid.UUID) (*User, error)
	GetFamily(ctx context.Context, familyID, userID uuid.UUID) (*Family, error)
	GetFamilyMemberships(ctx context.Context, userID uuid.UUID) ([]UserFamilyMembership, error)
	GetFamilyMembers(ctx context.Context, familyID, userID uuid.UUID) ([]FamilyMember, error)
	RegisterBootstrap(ctx context.Context, params RegisterBootstrapParams) (*RegisterResult, error)
	CreateSession(ctx context.Context, session Session) error
	RotateSession(ctx context.Context, oldTokenHash, newTokenHash string, newExpiresAt time.Time, userAgent, ipAddress string) (*RotateResult, error)
	RevokeSessionByToken(ctx context.Context, tokenHash string) error
	ListActiveSessions(ctx context.Context, userID uuid.UUID) ([]SessionDTO, error)
	RevokeSessionByID(ctx context.Context, userID, sessionID uuid.UUID) error
}

type PostgresRepository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{pool: pool}
}

func (r *PostgresRepository) GetCredential(ctx context.Context, email string) (*UserCredential, error) {
	query := `SELECT user_id, password_hash, is_active FROM auth_sec.get_user_auth_credential($1)`
	var cred UserCredential
	err := r.pool.QueryRow(ctx, query, strings.ToLower(strings.TrimSpace(email))).Scan(&cred.UserID, &cred.PasswordHash, &cred.IsActive)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to lookup credentials: %w", err)
	}
	return &cred, nil
}

func (r *PostgresRepository) GetUser(ctx context.Context, userID uuid.UUID) (*User, error) {
	query := `SELECT id, email, first_name, COALESCE(last_name, '') FROM public.users WHERE id = $1 AND is_active = TRUE`
	var u User
	err := r.pool.QueryRow(ctx, query, userID).Scan(&u.ID, &u.Email, &u.FirstName, &u.LastName)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrUserNotFound
		}
		return nil, fmt.Errorf("failed to query user: %w", err)
	}
	return &u, nil
}

func (r *PostgresRepository) GetFamily(ctx context.Context, familyID, userID uuid.UUID) (*Family, error) {
	var f Family
	err := database.WithFamilyUserTx(ctx, r.pool, familyID, userID, func(tx pgx.Tx) error {
		query := `SELECT id, name, default_currency FROM public.families WHERE id = $1`
		return tx.QueryRow(ctx, query, familyID).Scan(&f.ID, &f.Name, &f.DefaultCurrency)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("family not found")
		}
		return nil, fmt.Errorf("failed to query family: %w", err)
	}
	return &f, nil
}

func (r *PostgresRepository) GetFamilyMemberships(ctx context.Context, userID uuid.UUID) ([]UserFamilyMembership, error) {
	var memberships []UserFamilyMembership
	err := database.WithUserTx(ctx, r.pool, userID, func(tx pgx.Tx) error {
		query := `
			SELECT fm.family_id, f.name, f.default_currency, fm.role
			FROM public.family_members fm
			JOIN public.families f ON fm.family_id = f.id
			WHERE fm.user_id = $1
			ORDER BY fm.created_at ASC
		`
		rows, err := tx.Query(ctx, query, userID)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var m UserFamilyMembership
			if err := rows.Scan(&m.FamilyID, &m.FamilyName, &m.DefaultCurrency, &m.Role); err != nil {
				return err
			}
			memberships = append(memberships, m)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("failed to query family memberships: %w", err)
	}
	return memberships, nil
}

func (r *PostgresRepository) GetFamilyMembers(ctx context.Context, familyID, userID uuid.UUID) ([]FamilyMember, error) {
	var members []FamilyMember
	err := database.WithFamilyUserTx(ctx, r.pool, familyID, userID, func(tx pgx.Tx) error {
		query := `
			SELECT fm.id, fm.user_id, fm.role, u.first_name, COALESCE(u.last_name, ''), COALESCE(fm.color_code, '')
			FROM public.family_members fm
			JOIN public.users u ON fm.user_id = u.id
			WHERE fm.family_id = $1
			ORDER BY fm.created_at ASC
		`
		rows, err := tx.Query(ctx, query, familyID)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var m FamilyMember
			if err := rows.Scan(&m.ID, &m.UserID, &m.Role, &m.FirstName, &m.LastName, &m.ColorCode); err != nil {
				return err
			}
			members = append(members, m)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("failed to query family members: %w", err)
	}
	return members, nil
}

func (r *PostgresRepository) RegisterBootstrap(ctx context.Context, params RegisterBootstrapParams) (*RegisterResult, error) {
	userID := uuid.New()
	familyID := uuid.New()
	memberID := uuid.New()
	sessionID := uuid.New()
	sessionChainID := uuid.New()

	currency := strings.ToUpper(strings.TrimSpace(params.DefaultCurrency))
	if currency == "" {
		currency = "RUB"
	}

	normalizedEmail := strings.ToLower(strings.TrimSpace(params.Email))

	err := database.WithFamilyUserTx(ctx, r.pool, familyID, userID, func(tx pgx.Tx) error {
		// 1. Insert user
		userQuery := `
			INSERT INTO public.users (id, email, password_hash, first_name, last_name, is_active)
			VALUES ($1, $2, $3, $4, $5, TRUE)
		`
		_, err := tx.Exec(ctx, userQuery, userID, normalizedEmail, params.PasswordHash, params.FirstName, params.LastName)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return ErrEmailAlreadyExists
			}
			return fmt.Errorf("failed to insert user: %w", err)
		}

		// 2. Insert family
		familyQuery := `
			INSERT INTO public.families (id, name, default_currency)
			VALUES ($1, $2, $3)
		`
		_, err = tx.Exec(ctx, familyQuery, familyID, params.FamilyName, currency)
		if err != nil {
			return fmt.Errorf("failed to insert family: %w", err)
		}

		// 3. Insert family owner membership
		memberQuery := `
			INSERT INTO public.family_members (id, family_id, user_id, role)
			VALUES ($1, $2, $3, 'owner')
		`
		_, err = tx.Exec(ctx, memberQuery, memberID, familyID, userID)
		if err != nil {
			return fmt.Errorf("failed to insert family owner membership: %w", err)
		}

		// 4. Seed default categories idempotently
		if err := database.SeedDefaultCategories(ctx, tx, familyID); err != nil {
			return fmt.Errorf("failed to seed default categories: %w", err)
		}

		// 5. Insert initial session
		sessionQuery := `
			INSERT INTO public.user_sessions (
				id, user_id, family_id, token_hash, session_chain_id, user_agent, ip_address, expires_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		`
		_, err = tx.Exec(ctx, sessionQuery, sessionID, userID, familyID, params.RefreshTokenHash, sessionChainID, params.UserAgent, params.IPAddress, params.SessionExpiresAt)
		if err != nil {
			return fmt.Errorf("failed to insert session: %w", err)
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	return &RegisterResult{
		User: User{
			ID:        userID,
			Email:     normalizedEmail,
			FirstName: params.FirstName,
			LastName:  params.LastName,
		},
		Family: Family{
			ID:              familyID,
			Name:            params.FamilyName,
			DefaultCurrency: currency,
		},
		Role:           "owner",
		SessionID:      sessionID,
		SessionChainID: sessionChainID,
	}, nil
}

func (r *PostgresRepository) CreateSession(ctx context.Context, session Session) error {
	return database.WithFamilyUserTx(ctx, r.pool, session.FamilyID, session.UserID, func(tx pgx.Tx) error {
		query := `
			INSERT INTO public.user_sessions (
				id, user_id, family_id, token_hash, session_chain_id, user_agent, ip_address, expires_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		`
		_, err := tx.Exec(ctx, query, session.ID, session.UserID, session.FamilyID, session.TokenHash, session.SessionChainID, session.UserAgent, session.IPAddress, session.ExpiresAt)
		return err
	})
}

func (r *PostgresRepository) RotateSession(ctx context.Context, oldTokenHash, newTokenHash string, newExpiresAt time.Time, userAgent, ipAddress string) (*RotateResult, error) {
	query := `
		SELECT status, session_id, user_id, family_id, session_chain_id
		FROM auth_sec.rotate_refresh_token($1, $2, $3, $4, $5)
	`
	var res RotateResult
	var sessID, uID, fID, chainID *uuid.UUID

	err := r.pool.QueryRow(ctx, query, oldTokenHash, newTokenHash, newExpiresAt, userAgent, ipAddress).
		Scan(&res.Status, &sessID, &uID, &fID, &chainID)
	if err != nil {
		return nil, fmt.Errorf("failed to execute rotate_refresh_token: %w", err)
	}

	if sessID != nil {
		res.SessionID = *sessID
	}
	if uID != nil {
		res.UserID = *uID
	}
	if fID != nil {
		res.FamilyID = *fID
	}
	if chainID != nil {
		res.SessionChainID = *chainID
	}

	switch res.Status {
	case "SUCCESS":
		return &res, nil
	case "REPLAY_DETECTED":
		return nil, ErrSessionReplayDetected
	case "REVOKED":
		return nil, ErrSessionRevoked
	case "EXPIRED":
		return nil, ErrSessionExpired
	case "NOT_FOUND":
		return nil, ErrSessionNotFound
	default:
		return nil, fmt.Errorf("unexpected rotate status: %s", res.Status)
	}
}

func (r *PostgresRepository) RevokeSessionByToken(ctx context.Context, tokenHash string) error {
	query := `SELECT auth_sec.revoke_session($1)`
	var revoked bool
	err := r.pool.QueryRow(ctx, query, tokenHash).Scan(&revoked)
	if err != nil {
		return fmt.Errorf("failed to revoke session: %w", err)
	}
	if !revoked {
		return ErrSessionNotFound
	}
	return nil
}

func (r *PostgresRepository) ListActiveSessions(ctx context.Context, userID uuid.UUID) ([]SessionDTO, error) {
	var sessions []SessionDTO
	err := database.WithUserTx(ctx, r.pool, userID, func(tx pgx.Tx) error {
		query := `
			SELECT id, COALESCE(user_agent, ''), COALESCE(ip_address, ''), created_at, last_used_at, expires_at
			FROM public.user_sessions
			WHERE user_id = $1 AND is_revoked = FALSE AND is_rotated = FALSE AND expires_at > NOW()
			ORDER BY last_used_at DESC
		`
		rows, err := tx.Query(ctx, query, userID)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var s SessionDTO
			if err := rows.Scan(&s.ID, &s.UserAgent, &s.IPAddress, &s.CreatedAt, &s.LastUsedAt, &s.ExpiresAt); err != nil {
				return err
			}
			sessions = append(sessions, s)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list active sessions: %w", err)
	}
	return sessions, nil
}

func (r *PostgresRepository) RevokeSessionByID(ctx context.Context, userID, sessionID uuid.UUID) error {
	return database.WithUserTx(ctx, r.pool, userID, func(tx pgx.Tx) error {
		var chainID uuid.UUID
		err := tx.QueryRow(ctx, `SELECT session_chain_id FROM public.user_sessions WHERE id = $1 AND user_id = $2`, sessionID, userID).Scan(&chainID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrSessionNotFound
			}
			return err
		}

		_, err = tx.Exec(ctx, `UPDATE public.user_sessions SET is_revoked = TRUE, revoked_at = NOW() WHERE session_chain_id = $1 AND user_id = $2`, chainID, userID)
		return err
	})
}

package personal

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/khaliullov/family-finance/internal/database"
)

var (
	ErrSessionStoreConflict = errors.New("session: concurrent modification conflict detected")
)

// SessionStore provides a clean, tenant-isolated persistent session storage contract.
type SessionStore interface {
	GetSession(ctx context.Context, familyID, connectionID uuid.UUID) (*Session, error)
	SaveSession(ctx context.Context, familyID, connectionID uuid.UUID, session *Session) error
	InvalidateSession(ctx context.Context, familyID, connectionID uuid.UUID) error
}

// DBSessionStore implements SessionStore against PostgreSQL bank_connections table under RLS.
type DBSessionStore struct {
	pool      *pgxpool.Pool
	cryptoKey []byte
}

// NewDBSessionStore creates a PostgreSQL-backed encrypted session store.
func NewDBSessionStore(pool *pgxpool.Pool, cryptoKey []byte) (*DBSessionStore, error) {
	if len(cryptoKey) != 32 {
		return nil, errors.New("session store: cryptoKey must be exactly 32 bytes")
	}
	return &DBSessionStore{
		pool:      pool,
		cryptoKey: cryptoKey,
	}, nil
}

// GetSession loads and decrypts the active session for the given connection.
func (s *DBSessionStore) GetSession(ctx context.Context, familyID, connectionID uuid.UUID) (*Session, error) {
	var encryptedCreds []byte
	var status string
	err := database.WithFamilyTx(ctx, s.pool, familyID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT encrypted_credentials, status
			FROM public.bank_connections
			WHERE family_id = $1 AND id = $2
		`, familyID, connectionID)

		return row.Scan(&encryptedCreds, &status)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrSessionNotFound
		}
		return nil, fmt.Errorf("load connection session: %w", err)
	}

	if len(encryptedCreds) == 0 {
		return nil, ErrSessionNotFound
	}

	sess, err := DecryptSession(s.cryptoKey, encryptedCreds)
	if err != nil {
		return nil, err
	}

	// Security: Verify that the decrypted session's family/conn IDs strictly match the requested tenant context
	if sess.FamilyID != familyID || sess.ConnectionID != connectionID {
		return nil, errors.New("session: tenant context mismatch detected in decrypted session envelope")
	}

	return sess, nil
}

// SaveSession encrypts and atomically commits the session to the database.
func (s *DBSessionStore) SaveSession(ctx context.Context, familyID, connectionID uuid.UUID, session *Session) error {
	if session == nil {
		return errors.New("cannot save nil session")
	}

	session.FamilyID = familyID
	session.ConnectionID = connectionID
	session.Version++

	ciphertext, err := session.Encrypt(s.cryptoKey)
	if err != nil {
		return fmt.Errorf("encrypt session: %w", err)
	}

	dbStatus := "active"
	if session.Status == SessionStatusExpired {
		dbStatus = "expired"
	} else if session.Status == SessionStatusDisconnected {
		dbStatus = "disconnected"
	}

	return database.WithFamilyTx(ctx, s.pool, familyID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE public.bank_connections
			SET encrypted_credentials = $1,
			    status = $2,
			    updated_at = NOW()
			WHERE family_id = $3 AND id = $4
		`, ciphertext, dbStatus, familyID, connectionID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrSessionNotFound
		}
		return nil
	})
}

// InvalidateSession marks the session as expired and clears credentials.
func (s *DBSessionStore) InvalidateSession(ctx context.Context, familyID, connectionID uuid.UUID) error {
	return database.WithFamilyTx(ctx, s.pool, familyID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE public.bank_connections
			SET status = 'expired',
			    updated_at = NOW()
			WHERE family_id = $1 AND id = $2
		`, familyID, connectionID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrSessionNotFound
		}
		return nil
	})
}

// MemorySessionStore is a thread-safe in-memory session store for testing.
type MemorySessionStore struct {
	mu        sync.RWMutex
	sessions  map[string]*Session
	cryptoKey []byte
}

// NewMemorySessionStore creates an in-memory session store.
func NewMemorySessionStore(cryptoKey []byte) *MemorySessionStore {
	return &MemorySessionStore{
		sessions:  make(map[string]*Session),
		cryptoKey: cryptoKey,
	}
}

func (m *MemorySessionStore) key(f, c uuid.UUID) string {
	return f.String() + ":" + c.String()
}

func (m *MemorySessionStore) GetSession(ctx context.Context, familyID, connectionID uuid.UUID) (*Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	sess, ok := m.sessions[m.key(familyID, connectionID)]
	if !ok || sess == nil {
		return nil, ErrSessionNotFound
	}

	// Return a copy to prevent concurrent modification
	copySess := *sess
	return &copySess, nil
}

func (m *MemorySessionStore) SaveSession(ctx context.Context, familyID, connectionID uuid.UUID, session *Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	session.FamilyID = familyID
	session.ConnectionID = connectionID
	session.Version++
	copySess := *session
	m.sessions[m.key(familyID, connectionID)] = &copySess
	return nil
}

func (m *MemorySessionStore) InvalidateSession(ctx context.Context, familyID, connectionID uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	k := m.key(familyID, connectionID)
	if sess, ok := m.sessions[k]; ok {
		sess.Status = SessionStatusExpired
	}
	return nil
}

package database

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNilFamilyID = errors.New("cannot execute WithFamilyTx with nil familyID")
	ErrNilUserID   = errors.New("cannot execute WithUserTx with nil userID")
)

// WithUserTx executes fn within a PostgreSQL transaction strictly scoped to the specified userID.
// It sets the session setting 'app.current_user_id' using transaction-local set_config(..., true).
func WithUserTx(ctx context.Context, pool *pgxpool.Pool, userID uuid.UUID, fn func(tx pgx.Tx) error) error {
	if userID == uuid.Nil {
		return ErrNilUserID
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}

	var committed bool
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(ctx)
			panic(p) // re-throw panic after guaranteeing rollback
		} else if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	// Inject transaction-scoped user context (is_local = true)
	_, err = tx.Exec(ctx, "SELECT set_config('app.current_user_id', $1, true)", userID.String())
	if err != nil {
		return fmt.Errorf("failed to inject user tenant context: %w", err)
	}

	if err := fn(tx); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}
	committed = true

	return nil
}

// WithFamilyTx executes fn within a PostgreSQL transaction strictly scoped to the specified familyID.
// It sets the session setting 'app.current_family_id' using transaction-local set_config(..., true).
// This guarantees:
// 1. PostgreSQL RLS policies evaluate against this family context for the duration of the transaction.
// 2. Upon COMMIT or ROLLBACK, the setting automatically reverts to empty, preventing GUC leaks across pooled connections.
// 3. fn is executed inside the transaction; any error or panic triggers an automatic rollback.
func WithFamilyTx(ctx context.Context, pool *pgxpool.Pool, familyID uuid.UUID, fn func(tx pgx.Tx) error) error {
	return WithFamilyUserTx(ctx, pool, familyID, uuid.Nil, fn)
}

// WithFamilyUserTx executes fn within a PostgreSQL transaction scoped to both familyID and optional userID.
func WithFamilyUserTx(ctx context.Context, pool *pgxpool.Pool, familyID uuid.UUID, userID uuid.UUID, fn func(tx pgx.Tx) error) error {
	if familyID == uuid.Nil {
		return ErrNilFamilyID
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}

	var committed bool
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(ctx)
			panic(p) // re-throw panic after guaranteeing rollback
		} else if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	// Inject transaction-scoped family context (is_local = true)
	_, err = tx.Exec(ctx, "SELECT set_config('app.current_family_id', $1, true)", familyID.String())
	if err != nil {
		return fmt.Errorf("failed to inject family tenant context: %w", err)
	}

	// Optionally inject transaction-scoped user context
	if userID != uuid.Nil {
		_, err = tx.Exec(ctx, "SELECT set_config('app.current_user_id', $1, true)", userID.String())
		if err != nil {
			return fmt.Errorf("failed to inject user tenant context: %w", err)
		}
	}

	if err := fn(tx); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}
	committed = true

	return nil
}

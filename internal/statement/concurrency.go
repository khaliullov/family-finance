package statement

import (
	"context"
	"fmt"
	"hash/crc32"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// AcquireFamilyImportSlot attempts to acquire one of two execution slots for statement imports
// within the specified family tenant using PostgreSQL transaction-scoped advisory locks.
//
// Mechanism:
// - key1 is derived from CRC32(family_id) as an int32.
// - key2 is slot 0 or slot 1.
// - pg_try_advisory_xact_lock is strictly transaction-scoped: it is automatically released
//   by PostgreSQL when tx commits or rolls back, preventing orphaned locks on process termination.
//
// Returns the acquired slot (0 or 1), or ErrTooManyConcurrentImports if both slots are held.
func AcquireFamilyImportSlot(ctx context.Context, tx pgx.Tx, familyID uuid.UUID) (int, error) {
	if familyID == uuid.Nil {
		return -1, fmt.Errorf("cannot acquire lock for nil familyID")
	}

	key1 := int32(crc32.ChecksumIEEE([]byte(familyID.String())))

	// Attempt slot 0
	var acquiredSlot0 bool
	err := tx.QueryRow(ctx, "SELECT pg_try_advisory_xact_lock($1, 0)", key1).Scan(&acquiredSlot0)
	if err != nil {
		return -1, fmt.Errorf("failed to query advisory lock for slot 0: %w", err)
	}
	if acquiredSlot0 {
		return 0, nil
	}

	// Attempt slot 1
	var acquiredSlot1 bool
	err = tx.QueryRow(ctx, "SELECT pg_try_advisory_xact_lock($1, 1)", key1).Scan(&acquiredSlot1)
	if err != nil {
		return -1, fmt.Errorf("failed to query advisory lock for slot 1: %w", err)
	}
	if acquiredSlot1 {
		return 1, nil
	}

	// Both slots occupied
	return -1, ErrTooManyConcurrentImports
}

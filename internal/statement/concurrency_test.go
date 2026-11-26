package statement

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// 20. Two-slot family concurrency limit:
// Verify AcquireFamilyImportSlot allows at most 2 concurrent imports per family tenant,
// isolates slot allocations across families, and releases slots upon transaction commit/rollback.
func TestTwoSlotFamilyConcurrencyLimit(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()

	ctx := context.Background()
	familyA := uuid.New()
	familyB := uuid.New()

	// Begin Tx1 for Family A
	tx1, err := appPool.Begin(ctx)
	if err != nil {
		t.Fatalf("failed to begin tx1: %v", err)
	}
	defer tx1.Rollback(ctx)

	slot1, err := AcquireFamilyImportSlot(ctx, tx1, familyA)
	if err != nil {
		t.Fatalf("expected tx1 to acquire slot, got error: %v", err)
	}
	if slot1 != 0 {
		t.Errorf("expected tx1 to acquire slot 0, got %d", slot1)
	}

	// Begin Tx2 for Family A (second concurrent slot)
	tx2, err := appPool.Begin(ctx)
	if err != nil {
		t.Fatalf("failed to begin tx2: %v", err)
	}
	defer tx2.Rollback(ctx)

	slot2, err := AcquireFamilyImportSlot(ctx, tx2, familyA)
	if err != nil {
		t.Fatalf("expected tx2 to acquire second slot, got error: %v", err)
	}
	if slot2 != 1 {
		t.Errorf("expected tx2 to acquire slot 1, got %d", slot2)
	}

	// Begin Tx3 for Family A (third concurrent slot - MUST FAIL)
	tx3, err := appPool.Begin(ctx)
	if err != nil {
		t.Fatalf("failed to begin tx3: %v", err)
	}
	defer tx3.Rollback(ctx)

	slot3, err := AcquireFamilyImportSlot(ctx, tx3, familyA)
	if err != ErrTooManyConcurrentImports {
		t.Fatalf("expected ErrTooManyConcurrentImports for tx3, got slot=%d, err=%v", slot3, err)
	}

	// Begin Tx4 for Family B (independent tenant MUST SUCCEED on slot 0)
	tx4, err := appPool.Begin(ctx)
	if err != nil {
		t.Fatalf("failed to begin tx4: %v", err)
	}
	defer tx4.Rollback(ctx)

	slot4, err := AcquireFamilyImportSlot(ctx, tx4, familyB)
	if err != nil {
		t.Fatalf("expected tx4 in Family B to acquire slot, got error: %v", err)
	}
	if slot4 != 0 {
		t.Errorf("expected Family B to acquire slot 0, got %d", slot4)
	}

	// Release Tx1 (slot 0 of Family A is freed)
	if err := tx1.Rollback(ctx); err != nil {
		t.Fatalf("failed to rollback tx1: %v", err)
	}

	// Tx5 for Family A can now acquire the freed slot 0
	tx5, err := appPool.Begin(ctx)
	if err != nil {
		t.Fatalf("failed to begin tx5: %v", err)
	}
	defer tx5.Rollback(ctx)

	slot5, err := AcquireFamilyImportSlot(ctx, tx5, familyA)
	if err != nil {
		t.Fatalf("expected tx5 to acquire freed slot in Family A, got error: %v", err)
	}
	if slot5 != 0 {
		t.Errorf("expected tx5 to acquire slot 0, got %d", slot5)
	}
}

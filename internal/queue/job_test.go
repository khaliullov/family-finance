package queue

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestSyncJobPayload_Validate(t *testing.T) {
	valid := SyncJobPayload{
		ConnectionID: uuid.New(),
		LookbackDays: 30,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("unexpected error for valid payload: %v", err)
	}

	missingConn := SyncJobPayload{
		ConnectionID: uuid.Nil,
		LookbackDays: 30,
	}
	if err := missingConn.Validate(); !errors.Is(err, ErrInvalidJobPayload) {
		t.Fatalf("expected ErrInvalidJobPayload for nil connection_id, got %v", err)
	}

	negativeLookback := SyncJobPayload{
		ConnectionID: uuid.New(),
		LookbackDays: -5,
	}
	if err := negativeLookback.Validate(); !errors.Is(err, ErrInvalidJobPayload) {
		t.Fatalf("expected ErrInvalidJobPayload for negative lookback, got %v", err)
	}
}

func TestCalculateBackoff(t *testing.T) {
	base := 10 * time.Second
	max := 1 * time.Hour

	// Attempt 1: base * 1 + jitter [0..2.5s] -> [10s .. 12.5s]
	b1 := CalculateBackoff(1, base, max)
	if b1 < 10*time.Second || b1 > 13*time.Second {
		t.Errorf("attempt 1 backoff out of expected range: %v", b1)
	}

	// Attempt 2: base * 2 + jitter [0..5s] -> [20s .. 25s]
	b2 := CalculateBackoff(2, base, max)
	if b2 < 20*time.Second || b2 > 26*time.Second {
		t.Errorf("attempt 2 backoff out of expected range: %v", b2)
	}

	// Attempt 3: base * 4 + jitter [0..10s] -> [40s .. 50s]
	b3 := CalculateBackoff(3, base, max)
	if b3 < 40*time.Second || b3 > 52*time.Second {
		t.Errorf("attempt 3 backoff out of expected range: %v", b3)
	}

	// Max backoff cap
	capped := CalculateBackoff(10, base, 30*time.Second)
	if capped > 30*time.Second {
		t.Errorf("expected backoff capped at 30s, got %v", capped)
	}
}

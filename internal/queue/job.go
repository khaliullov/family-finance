package queue

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"time"

	"github.com/google/uuid"
)

const (
	QueueBankSync   = "bank_sync"
	JobTypeBankSync = "bank_sync"

	StateAvailable = "available"
	StateRunning   = "running"
	StateCompleted = "completed"
	StateFailed    = "failed"
	StateDiscarded = "discarded"
)

var (
	ErrInvalidJobPayload = errors.New("invalid worker job payload")
	ErrLeaseLost         = errors.New("lease lost: job is no longer owned by this worker")
	ErrTransientDatabase = errors.New("transient database infrastructure error")
)

// SyncJobPayload is the JSON payload stored in worker_jobs.payload for bank_sync jobs.
type SyncJobPayload struct {
	ConnectionID  uuid.UUID `json:"connection_id"`
	LookbackDays  int       `json:"lookback_days"`
	Cursor        string    `json:"cursor,omitempty"`
	ForceFullSync bool      `json:"force_full_sync,omitempty"`
	ResetCursor   bool      `json:"reset_cursor,omitempty"`
}

// Validate checks payload invariants.
func (p SyncJobPayload) Validate() error {
	if p.ConnectionID == uuid.Nil {
		return fmt.Errorf("%w: missing connection_id", ErrInvalidJobPayload)
	}
	if p.LookbackDays < 0 {
		return fmt.Errorf("%w: lookback_days cannot be negative", ErrInvalidJobPayload)
	}
	return nil
}

// JobRecord represents a claimed worker job.
type JobRecord struct {
	ID        uuid.UUID
	FamilyID  uuid.UUID
	JobType   string
	Payload   json.RawMessage
	ClaimedBy string
}

// CalculateBackoff computes exponential backoff with jitter: base * 2^(attempt-1) + jitter.
func CalculateBackoff(attempt int, baseBackoff time.Duration, maxBackoff time.Duration) time.Duration {
	if attempt <= 0 {
		attempt = 1
	}
	// Cap attempt at 10 to prevent integer overflow
	if attempt > 10 {
		attempt = 10
	}

	multiplier := 1 << (attempt - 1) // 1, 2, 4, 8, 16...
	backoff := time.Duration(multiplier) * baseBackoff

	// Add jitter up to 25%
	jitterMax := int64(backoff / 4)
	if jitterMax > 0 {
		jitter := time.Duration(rand.Int63n(jitterMax))
		backoff += jitter
	}

	if maxBackoff > 0 && backoff > maxBackoff {
		backoff = maxBackoff
	}
	return backoff
}

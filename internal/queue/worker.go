package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/khaliullov/family-finance/internal/connector"
	"github.com/khaliullov/family-finance/internal/database"
)

// JobHandler defines the contract for processing a claimed worker job.
type JobHandler func(ctx context.Context, job *JobRecord) error

// WorkerConfig holds configuration for the queue worker.
type WorkerConfig struct {
	WorkerID            string
	QueueName           string
	Concurrency         int
	PollInterval        time.Duration
	LockDurationSeconds int
	RenewalInterval     time.Duration
	MaxAttempts         int
	BaseBackoff         time.Duration
	MaxBackoff          time.Duration
}

// DefaultWorkerConfig returns production defaults for WorkerConfig.
func DefaultWorkerConfig() WorkerConfig {
	return WorkerConfig{
		WorkerID:            fmt.Sprintf("worker-%d", time.Now().UnixNano()),
		QueueName:           QueueBankSync,
		Concurrency:         1,
		PollInterval:        500 * time.Millisecond,
		LockDurationSeconds: 600,
		RenewalInterval:     120 * time.Second,
		MaxAttempts:         5,
		BaseBackoff:         10 * time.Second,
		MaxBackoff:          1 * time.Hour,
	}
}

// Worker processes jobs from the PostgreSQL background worker queue.
type Worker struct {
	pool     *pgxpool.Pool
	cfg      WorkerConfig
	handlers map[string]JobHandler

	stopChan chan struct{}
	wg       sync.WaitGroup
	running  bool
	mu       sync.Mutex
}

// NewWorker initializes a new background queue worker.
func NewWorker(pool *pgxpool.Pool, cfg WorkerConfig) *Worker {
	if cfg.WorkerID == "" {
		cfg.WorkerID = fmt.Sprintf("worker-%d", time.Now().UnixNano())
	}
	if cfg.QueueName == "" {
		cfg.QueueName = QueueBankSync
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 1
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 500 * time.Millisecond
	}
	if cfg.LockDurationSeconds <= 0 {
		cfg.LockDurationSeconds = 600
	}
	if cfg.RenewalInterval <= 0 {
		cfg.RenewalInterval = time.Duration(cfg.LockDurationSeconds/3) * time.Second
		if cfg.RenewalInterval <= 0 {
			cfg.RenewalInterval = 100 * time.Millisecond
		}
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 5
	}
	if cfg.BaseBackoff <= 0 {
		cfg.BaseBackoff = 10 * time.Second
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = 1 * time.Hour
	}

	return &Worker{
		pool:     pool,
		cfg:      cfg,
		handlers: make(map[string]JobHandler),
		stopChan: make(chan struct{}),
	}
}

// RegisterHandler registers a processor for a specific job_type.
func (w *Worker) RegisterHandler(jobType string, handler JobHandler) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.handlers[jobType] = handler
}

// Start spawns the configured number of concurrent worker loops.
func (w *Worker) Start(ctx context.Context) {
	w.mu.Lock()
	if w.running {
		w.mu.Unlock()
		return
	}
	w.running = true
	w.mu.Unlock()

	for i := 0; i < w.cfg.Concurrency; i++ {
		w.wg.Add(1)
		workerSlot := i
		go func() {
			defer w.wg.Done()
			w.runWorkerLoop(ctx, workerSlot)
		}()
	}
}

// Stop signals graceful shutdown and waits for running jobs to finish.
func (w *Worker) Stop() {
	w.mu.Lock()
	if !w.running {
		w.mu.Unlock()
		return
	}
	w.running = false
	close(w.stopChan)
	w.mu.Unlock()

	w.wg.Wait()
}

func (w *Worker) runWorkerLoop(ctx context.Context, slot int) {
	workerID := fmt.Sprintf("%s-slot-%d", w.cfg.WorkerID, slot)

	for {
		select {
		case <-ctx.Done():
			return
		case <-w.stopChan:
			return
		default:
		}

		job, err := w.ClaimJob(ctx, workerID)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				log.Printf("[ERROR] [%s] Failed to claim next job: %v", workerID, err)
			}
			w.waitInterval(ctx, w.cfg.PollInterval)
			continue
		}

		if job == nil {
			// No job currently available
			w.waitInterval(ctx, w.cfg.PollInterval)
			continue
		}

		// Process claimed job
		w.processClaimedJob(ctx, job)
	}
}

func (w *Worker) waitInterval(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-w.stopChan:
	case <-time.After(d):
	}
}

// ClaimJob calls queue_sec.claim_next_worker_job to atomically claim a job.
func (w *Worker) ClaimJob(ctx context.Context, workerID string) (*JobRecord, error) {
	row := w.pool.QueryRow(ctx, `
		SELECT job_id, family_id, job_type, payload, claim_token
		FROM queue_sec.claim_next_worker_job($1, $2, $3)
	`, w.cfg.QueueName, workerID, w.cfg.LockDurationSeconds)

	var job JobRecord
	err := row.Scan(&job.ID, &job.FamilyID, &job.JobType, &job.Payload, &job.ClaimToken)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("claim_next_worker_job error: %w", err)
	}

	if job.ID == uuid.Nil {
		return nil, nil
	}

	job.ClaimedBy = workerID
	return &job, nil
}

// ProcessJobOnce processes a single job synchronously for testing or immediate execution.
func (w *Worker) ProcessJobOnce(ctx context.Context, job *JobRecord) error {
	return w.processClaimedJob(ctx, job)
}

func (w *Worker) processClaimedJob(parentCtx context.Context, job *JobRecord) error {
	w.mu.Lock()
	handler, ok := w.handlers[job.JobType]
	w.mu.Unlock()

	if !ok {
		updateCtx, cancelUpdate := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelUpdate()
		return w.markJobFailed(updateCtx, job, fmt.Errorf("unsupported job type %q: no registered handler", job.JobType))
	}

	// Create cancellable context for job execution
	jobCtx, cancelJob := context.WithCancel(parentCtx)
	defer cancelJob()

	var renewalMu sync.Mutex
	var renewalErr error

	// Launch periodic lease renewal if RenewalInterval > 0
	stopRenewal := make(chan struct{})
	renewalDone := make(chan struct{})

	if w.cfg.RenewalInterval > 0 {
		go func() {
			defer close(renewalDone)
			ticker := time.NewTicker(w.cfg.RenewalInterval)
			defer ticker.Stop()

			for {
				select {
				case <-jobCtx.Done():
					return
				case <-stopRenewal:
					return
				case <-ticker.C:
					// Use parentCtx bounded context for renewal attempt
					renewCtx, cancelRenew := context.WithTimeout(parentCtx, 5*time.Second)
					err := w.RenewLease(renewCtx, job)
					cancelRenew()
					if err != nil {
						log.Printf("[WARN] [%s] Lease renewal failed for job %s: %v; cancelling job context", job.ClaimedBy, job.ID, err)
						renewalMu.Lock()
						renewalErr = err
						renewalMu.Unlock()
						cancelJob()
						return
					}
				}
			}
		}()
	} else {
		close(renewalDone)
	}

	// Execute job handler with cancellable context
	processErr := handler(jobCtx, job)

	// Stop renewal and wait for renewal goroutine to exit (prevents goroutine leak)
	if w.cfg.RenewalInterval > 0 {
		close(stopRenewal)
		<-renewalDone
	}

	// Check whether renewal failed
	renewalMu.Lock()
	rErr := renewalErr
	renewalMu.Unlock()

	// Separate bounded context for updating queue state to avoid using cancelled jobCtx or parentCtx
	updateCtx, cancelUpdate := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelUpdate()

	if rErr != nil {
		// Renewal failure is authoritative! Never complete the job.
		if errors.Is(rErr, ErrLeaseLost) {
			// Lease expired or stolen: do not attempt to mutate job in DB
			return ErrLeaseLost
		}
		// Transient infrastructure failure during renewal: mark failed as retryable
		retryableErr := fmt.Errorf("%w: renewal failed: %v", ErrTransientDatabase, rErr)
		_ = w.markJobFailed(updateCtx, job, retryableErr)
		return retryableErr
	}

	// Check if parent context was cancelled
	if parentCtx.Err() != nil {
		_ = w.markJobFailed(updateCtx, job, parentCtx.Err())
		return parentCtx.Err()
	}

	if processErr == nil {
		// Commit-before-success guarantee: mark completed only after ingestion has committed
		return w.markJobCompleted(updateCtx, job)
	}

	return w.markJobFailed(updateCtx, job, processErr)
}

// RenewLease extends the lease duration for a running job if still owned by the worker with a valid claim token.
func (w *Worker) RenewLease(ctx context.Context, job *JobRecord) error {
	if job == nil || job.ClaimToken == uuid.Nil {
		return ErrLeaseLost
	}
	intervalStr := fmt.Sprintf("%d seconds", w.cfg.LockDurationSeconds)
	return database.WithFamilyTx(ctx, w.pool, job.FamilyID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE public.worker_jobs
			SET locked_until = NOW() + ($1)::INTERVAL,
			    updated_at = NOW()
			WHERE family_id = $2 AND id = $3 AND state = 'running' AND locked_by = $4 AND claim_token = $5 AND locked_until > NOW()
		`, intervalStr, job.FamilyID, job.ID, job.ClaimedBy, job.ClaimToken)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrLeaseLost
		}
		return nil
	})
}

// MarkJobCompleted marks a claimed running job as completed and clears the claim token.
func (w *Worker) MarkJobCompleted(ctx context.Context, job *JobRecord) error {
	if job == nil || job.ClaimToken == uuid.Nil {
		return ErrLeaseLost
	}
	return database.WithFamilyTx(ctx, w.pool, job.FamilyID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE public.worker_jobs
			SET state = 'completed',
			    claim_token = NULL,
			    updated_at = NOW()
			WHERE family_id = $1 AND id = $2 AND state = 'running' AND locked_by = $3 AND claim_token = $4 AND locked_until > NOW()
		`, job.FamilyID, job.ID, job.ClaimedBy, job.ClaimToken)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrLeaseLost
		}
		return nil
	})
}

func (w *Worker) markJobCompleted(ctx context.Context, job *JobRecord) error {
	return w.MarkJobCompleted(ctx, job)
}

// MarkJobFailed marks a claimed running job as failed/discarded or reschedules it with backoff.
func (w *Worker) MarkJobFailed(ctx context.Context, job *JobRecord, processErr error) error {
	if job == nil || job.ClaimToken == uuid.Nil {
		return ErrLeaseLost
	}
	isRetryable := isErrorRetryable(processErr)
	errMsg := processErr.Error()
	if len(errMsg) > 500 {
		errMsg = errMsg[:500]
	}

	return database.WithFamilyTx(ctx, w.pool, job.FamilyID, func(tx pgx.Tx) error {
		var attempts, maxAttempts int
		var currentState, currentLockedBy string
		var currentClaimToken *uuid.UUID
		var isUnexpired bool
		err := tx.QueryRow(ctx, `
			SELECT attempts, max_attempts, state, COALESCE(locked_by, ''), claim_token, (locked_until > NOW()) AS is_unexpired
			FROM public.worker_jobs
			WHERE family_id = $1 AND id = $2
			FOR UPDATE
		`, job.FamilyID, job.ID).Scan(&attempts, &maxAttempts, &currentState, &currentLockedBy, &currentClaimToken, &isUnexpired)
		if err != nil {
			return fmt.Errorf("failed to read job attempts: %w", err)
		}

		if currentState != StateRunning || currentLockedBy != job.ClaimedBy || currentClaimToken == nil || *currentClaimToken != job.ClaimToken || !isUnexpired {
			return ErrLeaseLost
		}

		if isRetryable && attempts < maxAttempts {
			// Reschedule with exponential backoff
			backoff := CalculateBackoff(attempts, w.cfg.BaseBackoff, w.cfg.MaxBackoff)
			intervalStr := fmt.Sprintf("%d milliseconds", backoff.Milliseconds())

			tag, err := tx.Exec(ctx, `
				UPDATE public.worker_jobs
				SET state = 'available',
				    scheduled_at = NOW() + ($1)::INTERVAL,
				    last_error = $2,
				    locked_by = NULL,
				    locked_until = NULL,
				    claim_token = NULL,
				    updated_at = NOW()
				WHERE family_id = $3 AND id = $4 AND state = 'running' AND locked_by = $5 AND claim_token = $6 AND locked_until > NOW()
			`, intervalStr, errMsg, job.FamilyID, job.ID, job.ClaimedBy, job.ClaimToken)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return ErrLeaseLost
			}
			return nil
		}

		// Permanent failure or retry limit exhausted
		finalState := StateFailed
		if attempts >= maxAttempts {
			finalState = StateDiscarded
		}

		tag, err := tx.Exec(ctx, `
			UPDATE public.worker_jobs
			SET state = $1,
			    last_error = $2,
			    claim_token = NULL,
			    updated_at = NOW()
			WHERE family_id = $3 AND id = $4 AND state = 'running' AND locked_by = $5 AND claim_token = $6 AND locked_until > NOW()
		`, finalState, errMsg, job.FamilyID, job.ID, job.ClaimedBy, job.ClaimToken)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrLeaseLost
		}
		return nil
	})
}

func (w *Worker) markJobFailed(ctx context.Context, job *JobRecord, processErr error) error {
	return w.MarkJobFailed(ctx, job, processErr)
}

func isErrorRetryable(err error) bool {
	if errors.Is(err, connector.ErrTemporary) ||
		errors.Is(err, connector.ErrRateLimited) ||
		errors.Is(err, ErrTransientDatabase) {
		return true
	}

	var syncErr *connector.SyncError
	if errors.As(err, &syncErr) {
		return syncErr.Retryable
	}

	return false
}

// EnqueueSyncJob enqueues a new bank synchronization job in the transactional queue under RLS.
func EnqueueSyncJob(ctx context.Context, pool *pgxpool.Pool, familyID uuid.UUID, payload SyncJobPayload) (uuid.UUID, error) {
	return EnqueueSyncJobWithQueue(ctx, pool, familyID, QueueBankSync, payload)
}

// EnqueueSyncJobWithQueue enqueues a sync job into a specific named queue under RLS.
func EnqueueSyncJobWithQueue(ctx context.Context, pool *pgxpool.Pool, familyID uuid.UUID, queueName string, payload SyncJobPayload) (uuid.UUID, error) {
	if err := payload.Validate(); err != nil {
		return uuid.Nil, err
	}

	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return uuid.Nil, fmt.Errorf("failed to marshal job payload: %w", err)
	}

	jobID := uuid.New()
	err = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO public.worker_jobs (
				id, family_id, queue_name, job_type, payload, state, scheduled_at
			) VALUES (
				$1, $2, $3, $4, $5, 'available', NOW()
			)
		`, jobID, familyID, queueName, JobTypeBankSync, payloadJSON)
		return err
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("failed to enqueue sync job: %w", err)
	}

	return jobID, nil
}

// EnqueueValidationJob enqueues a new bank validation job in the transactional queue under RLS.
func EnqueueValidationJob(ctx context.Context, pool *pgxpool.Pool, familyID uuid.UUID, payload ValidationJobPayload) (uuid.UUID, error) {
	return EnqueueValidationJobWithQueue(ctx, pool, familyID, QueueBankSync, payload)
}

// EnqueueValidationJobWithQueue enqueues a validation job into a specific named queue under RLS.
func EnqueueValidationJobWithQueue(ctx context.Context, pool *pgxpool.Pool, familyID uuid.UUID, queueName string, payload ValidationJobPayload) (uuid.UUID, error) {
	if err := payload.Validate(); err != nil {
		return uuid.Nil, err
	}

	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return uuid.Nil, fmt.Errorf("failed to marshal validation job payload: %w", err)
	}

	jobID := uuid.New()
	err = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO public.worker_jobs (
				id, family_id, queue_name, job_type, payload, state, scheduled_at
			) VALUES (
				$1, $2, $3, $4, $5, 'available', NOW()
			)
		`, jobID, familyID, queueName, JobTypeBankValidation, payloadJSON)
		return err
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("failed to enqueue validation job: %w", err)
	}

	return jobID, nil
}

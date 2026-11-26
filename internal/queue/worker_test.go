package queue_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/khaliullov/family-finance/internal/connector"
	"github.com/khaliullov/family-finance/internal/connector/fixture"
	"github.com/khaliullov/family-finance/internal/database"
	"github.com/khaliullov/family-finance/internal/queue"
	"github.com/khaliullov/family-finance/internal/transaction"
)

var migrateOnce sync.Once

func getWorkerConnString() string {
	host := os.Getenv("PGHOST")
	if host == "" {
		host = "localhost"
	}
	port := os.Getenv("PGPORT")
	if port == "" {
		port = "5432"
	}
	dbname := os.Getenv("PGDATABASE")
	if dbname == "" {
		dbname = "family_finance_migration_test"
	}
	return fmt.Sprintf("postgres://family_worker:worker_secret_placeholder@%s:%s/%s?sslmode=disable", host, port, dbname)
}

func getAppConnString() string {
	host := os.Getenv("PGHOST")
	if host == "" {
		host = "localhost"
	}
	port := os.Getenv("PGPORT")
	if port == "" {
		port = "5432"
	}
	dbname := os.Getenv("PGDATABASE")
	if dbname == "" {
		dbname = "family_finance_migration_test"
	}
	return fmt.Sprintf("postgres://family_app:app_secret_placeholder@%s:%s/%s?sslmode=disable", host, port, dbname)
}

func getPGConnString() string {
	host := os.Getenv("PGHOST")
	if host == "" {
		host = "localhost"
	}
	port := os.Getenv("PGPORT")
	if port == "" {
		port = "5432"
	}
	user := os.Getenv("PGUSER")
	if user == "" {
		user = "postgres"
	}
	password := os.Getenv("PGPASSWORD")
	if password == "" {
		password = "postgres"
	}
	dbname := os.Getenv("PGDATABASE")
	if dbname == "" {
		dbname = "family_finance_migration_test"
	}
	return fmt.Sprintf("pgx5://%s:%s@%s:%s/%s?sslmode=disable", user, password, host, port, dbname)
}

func ensureMigrations(t *testing.T) {
	migrateOnce.Do(func() {
		migrationsPath, err := filepath.Abs("../../migrations")
		if err != nil {
			t.Fatalf("failed to resolve migrations directory: %v", err)
		}
		m, err := migrate.New("file://"+migrationsPath, getPGConnString())
		if err != nil {
			t.Fatalf("failed to initialize migrations: %v", err)
		}
		defer m.Close()

		if v, d, err := m.Version(); err == nil && d {
			_ = m.Force(int(v))
		}
		if err := m.Up(); err != nil && err != migrate.ErrNoChange {
			t.Fatalf("failed to apply migrations: %v", err)
		}
	})
}

func getWorkerPool(t *testing.T) *pgxpool.Pool {
	ensureMigrations(t)
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(getWorkerConnString())
	if err != nil {
		t.Fatalf("failed to parse worker connection string: %v", err)
	}
	cfg.MaxConns = 15
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("failed to connect as family_worker: %v", err)
	}
	return pool
}

func getAppPool(t *testing.T) *pgxpool.Pool {
	ensureMigrations(t)
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(getAppConnString())
	if err != nil {
		t.Fatalf("failed to parse app connection string: %v", err)
	}
	cfg.MaxConns = 15
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("failed to connect as family_app: %v", err)
	}
	return pool
}

func getPGPool(t *testing.T) *pgxpool.Pool {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, "postgres://postgres:postgres@localhost:5432/family_finance_migration_test?sslmode=disable")
	if err != nil {
		t.Fatalf("failed to connect as postgres: %v", err)
	}
	return pool
}

func setupTenantFixture(t *testing.T, provider string) (uuid.UUID, uuid.UUID, uuid.UUID) {
	appPool := getAppPool(t)
	defer appPool.Close()

	ctx := context.Background()
	familyID := uuid.New()
	userID := uuid.New()
	connID := uuid.New()
	email := fmt.Sprintf("tenant-%s@example.com", uuid.New().String()[:8])

	err := database.WithFamilyUserTx(ctx, appPool, familyID, userID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO public.users (id, email, password_hash, first_name, last_name, is_active)
			VALUES ($1, $2, '$argon2id$v=19$m=65536,t=3,p=4$dummyhash', 'Test', 'Owner', TRUE)
		`, userID, email)
		if err != nil {
			return err
		}

		_, err = tx.Exec(ctx, `
			INSERT INTO public.families (id, name, default_currency)
			VALUES ($1, 'Test Family', 'RUB')
		`, familyID)
		if err != nil {
			return err
		}

		_, err = tx.Exec(ctx, `
			INSERT INTO public.family_members (id, family_id, user_id, role)
			VALUES ($1, $2, $3, 'owner')
		`, uuid.New(), familyID, userID)
		if err != nil {
			return err
		}

		_, err = tx.Exec(ctx, `
			INSERT INTO public.bank_connections (
				id, family_id, authorized_by_user_id, provider, name, status
			) VALUES (
				$1, $2, $3, $4, 'Test Connection', 'active'
			)
		`, connID, familyID, userID, provider)
		return err
	})
	if err != nil {
		t.Fatalf("setupTenantFixture failed: %v", err)
	}

	return familyID, userID, connID
}

func TestWorker_ClaimNextJob_Atomic(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()
	workerPool := getWorkerPool(t)
	defer workerPool.Close()
	ctx := context.Background()

	familyID, _, connID := setupTenantFixture(t, "fixture")

	uniqueQueue := fmt.Sprintf("queue_%s", uuid.New().String()[:8])

	// Enqueue 2 jobs into isolated queue
	jobID1, err := queue.EnqueueSyncJobWithQueue(ctx, appPool, familyID, uniqueQueue, queue.SyncJobPayload{ConnectionID: connID, LookbackDays: 7})
	if err != nil {
		t.Fatalf("enqueue 1 failed: %v", err)
	}
	jobID2, err := queue.EnqueueSyncJobWithQueue(ctx, appPool, familyID, uniqueQueue, queue.SyncJobPayload{ConnectionID: connID, LookbackDays: 14})
	if err != nil {
		t.Fatalf("enqueue 2 failed: %v", err)
	}

	workerCfg := queue.DefaultWorkerConfig()
	workerCfg.QueueName = uniqueQueue
	worker := queue.NewWorker(workerPool, workerCfg)

	// Worker A claims
	claimedA, err := worker.ClaimJob(ctx, "worker-node-A")
	if err != nil {
		t.Fatalf("worker A claim failed: %v", err)
	}
	if claimedA == nil {
		t.Fatalf("worker A expected job, got nil")
	}

	// Worker B claims concurrently
	claimedB, err := worker.ClaimJob(ctx, "worker-node-B")
	if err != nil {
		t.Fatalf("worker B claim failed: %v", err)
	}
	if claimedB == nil {
		t.Fatalf("worker B expected job, got nil")
	}

	// Invariant: two workers must claim distinct jobs without race collision
	if claimedA.ID == claimedB.ID {
		t.Fatalf("collision: both workers claimed same job %s", claimedA.ID)
	}

	expectedIDs := map[uuid.UUID]bool{jobID1: true, jobID2: true}
	if !expectedIDs[claimedA.ID] || !expectedIDs[claimedB.ID] {
		t.Errorf("unexpected claimed IDs: A=%s B=%s", claimedA.ID, claimedB.ID)
	}

	// Third claim should return nil (no available jobs)
	claimedC, err := worker.ClaimJob(ctx, "worker-node-C")
	if err != nil || claimedC != nil {
		t.Errorf("expected nil for 3rd claim, got %+v, err=%v", claimedC, err)
	}
}

func TestWorker_ExpiredLeaseRecovery(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()
	workerPool := getWorkerPool(t)
	defer workerPool.Close()
	ctx := context.Background()

	familyID, _, connID := setupTenantFixture(t, "fixture")

	uniqueQueue := fmt.Sprintf("queue_exp_%s", uuid.New().String()[:8])
	jobID, err := queue.EnqueueSyncJobWithQueue(ctx, appPool, familyID, uniqueQueue, queue.SyncJobPayload{ConnectionID: connID, LookbackDays: 7})
	if err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}

	// Simulate crashed worker: set state = 'running' with expired locked_until (10 minutes ago)
	_ = database.WithFamilyTx(ctx, workerPool, familyID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE public.worker_jobs
			SET state = 'running',
			    locked_by = 'crashed-worker-999',
			    locked_until = NOW() - INTERVAL '10 minutes',
			    updated_at = NOW()
			WHERE family_id = $1 AND id = $2
		`, familyID, jobID)
		return err
	})

	workerCfg := queue.DefaultWorkerConfig()
	workerCfg.QueueName = uniqueQueue
	worker := queue.NewWorker(workerPool, workerCfg)

	// Another worker should reclaim the expired lease
	reclaimed, err := worker.ClaimJob(ctx, "healthy-worker-1")
	if err != nil {
		t.Fatalf("claim failed: %v", err)
	}
	if reclaimed == nil || reclaimed.ID != jobID {
		t.Fatalf("expected crashed job %s to be reclaimed, got %+v", jobID, reclaimed)
	}
}

func TestWorker_RetryBackoff_TemporaryFailure(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()
	workerPool := getWorkerPool(t)
	defer workerPool.Close()
	ctx := context.Background()

	familyID, _, connID := setupTenantFixture(t, "fixture")

	uniqueQueue := fmt.Sprintf("queue_retry_%s", uuid.New().String()[:8])
	jobID, err := queue.EnqueueSyncJobWithQueue(ctx, appPool, familyID, uniqueQueue, queue.SyncJobPayload{ConnectionID: connID, LookbackDays: 7})
	if err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}

	workerCfg := queue.DefaultWorkerConfig()
	workerCfg.QueueName = uniqueQueue
	workerCfg.BaseBackoff = 50 * time.Millisecond
	worker := queue.NewWorker(workerPool, workerCfg)

	// Register a handler that returns a temporary retryable error
	worker.RegisterHandler(queue.JobTypeBankSync, func(ctx context.Context, job *queue.JobRecord) error {
		return connector.NewSyncError("fixture", "TEMPORARY_FAILURE", "network timeout", true, connector.ErrTemporary)
	})

	job, err := worker.ClaimJob(ctx, "test-worker")
	if err != nil || job == nil {
		t.Fatalf("claim failed: %v", err)
	}

	// Process job -> should trigger retry backoff
	err = worker.ProcessJobOnce(ctx, job)
	if err != nil {
		t.Fatalf("ProcessJobOnce failed to handle retry: %v", err)
	}

	// Verify job returned to 'available', attempts = 1, scheduled_at in future
	_ = database.WithFamilyTx(ctx, workerPool, familyID, func(tx pgx.Tx) error {
		var state, lastErr string
		var attempts int
		var isScheduledFuture bool
		err := tx.QueryRow(ctx, `
			SELECT state, attempts, last_error, scheduled_at > NOW()
			FROM public.worker_jobs
			WHERE family_id = $1 AND id = $2
		`, familyID, jobID).Scan(&state, &attempts, &lastErr, &isScheduledFuture)
		if err != nil {
			t.Fatalf("query job failed: %v", err)
		}
		if state != queue.StateAvailable {
			t.Errorf("expected state 'available' after retryable error, got %s", state)
		}
		if attempts != 1 {
			t.Errorf("expected attempts 1, got %d", attempts)
		}
		if lastErr == "" {
			t.Errorf("expected last_error populated")
		}
		if !isScheduledFuture {
			t.Errorf("expected scheduled_at to be in future with backoff")
		}
		return nil
	})
}

func TestWorker_PermanentFailure(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()
	workerPool := getWorkerPool(t)
	defer workerPool.Close()
	ctx := context.Background()

	familyID, _, connID := setupTenantFixture(t, "fixture")

	uniqueQueue := fmt.Sprintf("queue_perm_%s", uuid.New().String()[:8])
	jobID, err := queue.EnqueueSyncJobWithQueue(ctx, appPool, familyID, uniqueQueue, queue.SyncJobPayload{ConnectionID: connID, LookbackDays: 7})
	if err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}

	workerCfg := queue.DefaultWorkerConfig()
	workerCfg.QueueName = uniqueQueue
	worker := queue.NewWorker(workerPool, workerCfg)

	// Register a handler that returns a permanent non-retryable error
	worker.RegisterHandler(queue.JobTypeBankSync, func(ctx context.Context, job *queue.JobRecord) error {
		return errors.New("permanent schema mismatch")
	})

	job, err := worker.ClaimJob(ctx, "test-worker")
	if err != nil || job == nil {
		t.Fatalf("claim failed: %v", err)
	}

	_ = worker.ProcessJobOnce(ctx, job)

	// Verify job marked failed permanently
	_ = database.WithFamilyTx(ctx, workerPool, familyID, func(tx pgx.Tx) error {
		var state string
		_ = tx.QueryRow(ctx, "SELECT state FROM public.worker_jobs WHERE family_id = $1 AND id = $2", familyID, jobID).Scan(&state)
		if state != queue.StateFailed {
			t.Errorf("expected state 'failed' for permanent error, got %s", state)
		}
		return nil
	})
}

func TestWorker_RetryExhaustion_DeadLetter(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()
	workerPool := getWorkerPool(t)
	defer workerPool.Close()
	pgPool := getPGPool(t)
	defer pgPool.Close()
	ctx := context.Background()

	familyID, _, connID := setupTenantFixture(t, "fixture")

	uniqueQueue := fmt.Sprintf("queue_dead_%s", uuid.New().String()[:8])
	jobID, err := queue.EnqueueSyncJobWithQueue(ctx, appPool, familyID, uniqueQueue, queue.SyncJobPayload{ConnectionID: connID, LookbackDays: 7})
	if err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}

	// Set attempts to 4, max_attempts = 5 via admin pool
	_, err = pgPool.Exec(ctx, `
		UPDATE public.worker_jobs
		SET attempts = 4, max_attempts = 5
		WHERE family_id = $1 AND id = $2
	`, familyID, jobID)
	if err != nil {
		t.Fatalf("failed to seed attempts: %v", err)
	}

	workerCfg := queue.DefaultWorkerConfig()
	workerCfg.QueueName = uniqueQueue
	worker := queue.NewWorker(workerPool, workerCfg)
	worker.RegisterHandler(queue.JobTypeBankSync, func(ctx context.Context, job *queue.JobRecord) error {
		return connector.NewSyncError("fixture", "TEMPORARY_FAILURE", "timeout", true, connector.ErrTemporary)
	})

	job, err := worker.ClaimJob(ctx, "test-worker")
	if err != nil || job == nil {
		t.Fatalf("claim failed: %v", err)
	}
	// After claim, attempts becomes 5 (attempts = attempts + 1 in SQL)

	err = worker.ProcessJobOnce(ctx, job)
	if err != nil {
		t.Fatalf("ProcessJobOnce failed: %v", err)
	}

	// Verify job discarded (dead-lettered) when attempts reach max_attempts
	_ = database.WithFamilyTx(ctx, workerPool, familyID, func(tx pgx.Tx) error {
		var state string
		_ = tx.QueryRow(ctx, "SELECT state FROM public.worker_jobs WHERE family_id = $1 AND id = $2", familyID, jobID).Scan(&state)
		if state != queue.StateDiscarded {
			t.Errorf("expected state 'discarded' after retry exhaustion, got %s", state)
		}
		return nil
	})
}

func TestWorker_EndToEndExecution(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()
	workerPool := getWorkerPool(t)
	defer workerPool.Close()
	ctx := context.Background()

	familyID, _, connID := setupTenantFixture(t, "fixture")

	// Setup transaction ingestion service with fixture connector
	registry := connector.NewRegistry()
	registry.Register(fixture.NewFixtureConnector())
	transactionSvc := transaction.NewService(workerPool, registry)

	uniqueQueue := fmt.Sprintf("queue_e2e_%s", uuid.New().String()[:8])
	// Setup worker
	workerCfg := queue.DefaultWorkerConfig()
	workerCfg.QueueName = uniqueQueue
	worker := queue.NewWorker(workerPool, workerCfg)
	worker.RegisterHandler(queue.JobTypeBankSync, transactionSvc.ProcessBankSyncJob)

	// Enqueue sync job using family_app
	jobID, err := queue.EnqueueSyncJobWithQueue(ctx, appPool, familyID, uniqueQueue, queue.SyncJobPayload{
		ConnectionID: connID,
		LookbackDays: 30,
	})
	if err != nil {
		t.Fatalf("enqueue sync job failed: %v", err)
	}

	// Claim and execute job
	job, err := worker.ClaimJob(ctx, "e2e-worker")
	if err != nil || job == nil || job.ID != jobID {
		t.Fatalf("failed to claim enqueued job: %v, %+v", err, job)
	}

	err = worker.ProcessJobOnce(ctx, job)
	if err != nil {
		t.Fatalf("end-to-end job processing failed: %v", err)
	}

	// Verify:
	// 1. Worker job state = 'completed'
	_ = database.WithFamilyTx(ctx, workerPool, familyID, func(tx pgx.Tx) error {
		var state string
		err := tx.QueryRow(ctx, "SELECT state FROM public.worker_jobs WHERE family_id = $1 AND id = $2", familyID, jobID).Scan(&state)
		if err != nil || state != queue.StateCompleted {
			t.Errorf("expected job state 'completed', got %s, err=%v", state, err)
		}

		// 2. Bank transactions ingested
		var txCount int
		err = tx.QueryRow(ctx, "SELECT count(*) FROM public.bank_transactions WHERE family_id = $1", familyID).Scan(&txCount)
		if err != nil || txCount != 7 {
			t.Errorf("expected 7 bank_transactions, got %d, err=%v", txCount, err)
		}

		// 3. Accounting entries synchronized
		var aeCount int
		err = tx.QueryRow(ctx, "SELECT count(*) FROM public.accounting_entries WHERE family_id = $1", familyID).Scan(&aeCount)
		if err != nil || aeCount != 7 {
			t.Errorf("expected 7 accounting_entries, got %d, err=%v", aeCount, err)
		}

		// 4. Connection status active and last_synced_at set
		var status string
		var lastSynced bool
		err = tx.QueryRow(ctx, "SELECT status, last_synced_at IS NOT NULL FROM public.bank_connections WHERE family_id = $1 AND id = $2", familyID, connID).Scan(&status, &lastSynced)
		if err != nil || status != "active" || !lastSynced {
			t.Errorf("expected connection status active and synced, got status=%s synced=%v", status, lastSynced)
		}

		return nil
	})
}

func TestWorker_JobOwnership_StaleWorkerCompletionRejected(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()
	workerPool := getWorkerPool(t)
	defer workerPool.Close()
	ctx := context.Background()

	familyID, _, connID := setupTenantFixture(t, "fixture")
	uniqueQueue := fmt.Sprintf("queue_stale_%s", uuid.New().String()[:8])

	jobID, err := queue.EnqueueSyncJobWithQueue(ctx, appPool, familyID, uniqueQueue, queue.SyncJobPayload{
		ConnectionID: connID,
		LookbackDays: 7,
	})
	if err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}

	workerCfgA := queue.DefaultWorkerConfig()
	workerCfgA.QueueName = uniqueQueue
	workerA := queue.NewWorker(workerPool, workerCfgA)
	workerA.RegisterHandler(queue.JobTypeBankSync, func(ctx context.Context, job *queue.JobRecord) error {
		return nil
	})

	workerCfgB := queue.DefaultWorkerConfig()
	workerCfgB.QueueName = uniqueQueue
	workerB := queue.NewWorker(workerPool, workerCfgB)
	workerB.RegisterHandler(queue.JobTypeBankSync, func(ctx context.Context, job *queue.JobRecord) error {
		return nil
	})

	// 1. Worker A claims job
	jobA, err := workerA.ClaimJob(ctx, "worker-node-A")
	if err != nil || jobA == nil || jobA.ID != jobID {
		t.Fatalf("worker A claim failed: %v, job=%+v", err, jobA)
	}
	if jobA.ClaimedBy != "worker-node-A" {
		t.Fatalf("expected ClaimedBy to be worker-node-A, got %s", jobA.ClaimedBy)
	}

	// 2. Simulate Worker A's lease expiration
	_ = database.WithFamilyTx(ctx, workerPool, familyID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE public.worker_jobs
			SET locked_until = NOW() - INTERVAL '5 seconds',
			    updated_at = NOW()
			WHERE family_id = $1 AND id = $2
		`, familyID, jobID)
		return err
	})

	// 3. Worker B claims the expired job
	jobB, err := workerB.ClaimJob(ctx, "worker-node-B")
	if err != nil || jobB == nil || jobB.ID != jobID {
		t.Fatalf("worker B reclaim failed: %v, job=%+v", err, jobB)
	}
	if jobB.ClaimedBy != "worker-node-B" {
		t.Fatalf("expected ClaimedBy to be worker-node-B, got %s", jobB.ClaimedBy)
	}

	// 4. Stale Worker A attempts to complete the job
	err = workerA.ProcessJobOnce(ctx, jobA)
	if err == nil {
		t.Fatalf("expected stale Worker A to fail with lease-lost, but it succeeded")
	}
	if !errors.Is(err, queue.ErrLeaseLost) {
		t.Fatalf("expected ErrLeaseLost, got: %v", err)
	}

	// 5. Active Worker B completes the job successfully
	err = workerB.ProcessJobOnce(ctx, jobB)
	if err != nil {
		t.Fatalf("worker B failed to complete job: %v", err)
	}

	// 6. Verify job state is completed and owned by B
	_ = database.WithFamilyTx(ctx, workerPool, familyID, func(tx pgx.Tx) error {
		var state, lockedBy string
		err := tx.QueryRow(ctx, "SELECT state, locked_by FROM public.worker_jobs WHERE family_id = $1 AND id = $2", familyID, jobID).Scan(&state, &lockedBy)
		if err != nil || state != queue.StateCompleted || lockedBy != "worker-node-B" {
			t.Fatalf("unexpected job state: state=%s, lockedBy=%s, err=%v", state, lockedBy, err)
		}
		return nil
	})
}

func TestWorker_JobOwnership_StaleWorkerFailureRejected(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()
	workerPool := getWorkerPool(t)
	defer workerPool.Close()
	ctx := context.Background()

	familyID, _, connID := setupTenantFixture(t, "fixture")
	uniqueQueue := fmt.Sprintf("queue_stale_fail_%s", uuid.New().String()[:8])

	jobID, err := queue.EnqueueSyncJobWithQueue(ctx, appPool, familyID, uniqueQueue, queue.SyncJobPayload{
		ConnectionID: connID,
		LookbackDays: 7,
	})
	if err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}

	workerCfgA := queue.DefaultWorkerConfig()
	workerCfgA.QueueName = uniqueQueue
	workerA := queue.NewWorker(workerPool, workerCfgA)
	workerA.RegisterHandler(queue.JobTypeBankSync, func(ctx context.Context, job *queue.JobRecord) error {
		return errors.New("worker A failed")
	})

	workerCfgB := queue.DefaultWorkerConfig()
	workerCfgB.QueueName = uniqueQueue
	workerB := queue.NewWorker(workerPool, workerCfgB)

	// 1. Worker A claims job
	jobA, err := workerA.ClaimJob(ctx, "worker-node-A")
	if err != nil || jobA == nil {
		t.Fatalf("worker A claim failed: %v", err)
	}

	// 2. Lease expires and Worker B reclaims
	_ = database.WithFamilyTx(ctx, workerPool, familyID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE public.worker_jobs
			SET locked_until = NOW() - INTERVAL '5 seconds',
			    updated_at = NOW()
			WHERE family_id = $1 AND id = $2
		`, familyID, jobID)
		return err
	})

	jobB, err := workerB.ClaimJob(ctx, "worker-node-B")
	if err != nil || jobB == nil {
		t.Fatalf("worker B claim failed: %v", err)
	}

	// 3. Stale Worker A fails the job -> must be rejected with ErrLeaseLost
	err = workerA.ProcessJobOnce(ctx, jobA)
	if err == nil {
		t.Fatalf("expected stale failure handling to fail with lease-lost, but it succeeded")
	}
	if !errors.Is(err, queue.ErrLeaseLost) {
		t.Fatalf("expected ErrLeaseLost, got: %v", err)
	}

	// 4. Job state must remain running under worker B
	_ = database.WithFamilyTx(ctx, workerPool, familyID, func(tx pgx.Tx) error {
		var state, lockedBy string
		err := tx.QueryRow(ctx, "SELECT state, locked_by FROM public.worker_jobs WHERE family_id = $1 AND id = $2", familyID, jobID).Scan(&state, &lockedBy)
		if err != nil || state != queue.StateRunning || lockedBy != "worker-node-B" {
			t.Fatalf("job state unexpectedly altered by stale worker A: state=%s lockedBy=%s", state, lockedBy)
		}
		return nil
	})
}

func TestWorker_LeaseRenewal_SuccessAndExpiryExtension(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()
	workerPool := getWorkerPool(t)
	defer workerPool.Close()
	ctx := context.Background()

	familyID, _, connID := setupTenantFixture(t, "fixture")
	uniqueQueue := fmt.Sprintf("queue_renew_%s", uuid.New().String()[:8])

	jobID, err := queue.EnqueueSyncJobWithQueue(ctx, appPool, familyID, uniqueQueue, queue.SyncJobPayload{
		ConnectionID: connID,
		LookbackDays: 7,
	})
	if err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}

	workerCfg := queue.DefaultWorkerConfig()
	workerCfg.QueueName = uniqueQueue
	workerCfg.LockDurationSeconds = 300
	worker := queue.NewWorker(workerPool, workerCfg)

	job, err := worker.ClaimJob(ctx, "renew-worker-1")
	if err != nil || job == nil {
		t.Fatalf("claim failed: %v", err)
	}

	// Check initial locked_until
	var initialLockedUntil time.Time
	_ = database.WithFamilyTx(ctx, workerPool, familyID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT locked_until FROM public.worker_jobs WHERE family_id = $1 AND id = $2", familyID, jobID).Scan(&initialLockedUntil)
	})

	// Wait a tiny moment
	time.Sleep(10 * time.Millisecond)

	// Renew lease
	err = worker.RenewLease(ctx, job)
	if err != nil {
		t.Fatalf("RenewLease failed: %v", err)
	}

	var renewedLockedUntil time.Time
	_ = database.WithFamilyTx(ctx, workerPool, familyID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT locked_until FROM public.worker_jobs WHERE family_id = $1 AND id = $2", familyID, jobID).Scan(&renewedLockedUntil)
	})

	if !renewedLockedUntil.After(initialLockedUntil) {
		t.Errorf("expected renewed locked_until (%v) to be strictly after initial (%v)", renewedLockedUntil, initialLockedUntil)
	}
}

func TestWorker_LeaseRenewal_LostLeaseCancelsContext(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()
	workerPool := getWorkerPool(t)
	defer workerPool.Close()
	ctx := context.Background()

	familyID, _, connID := setupTenantFixture(t, "fixture")
	uniqueQueue := fmt.Sprintf("queue_renew_cancel_%s", uuid.New().String()[:8])

	jobID, err := queue.EnqueueSyncJobWithQueue(ctx, appPool, familyID, uniqueQueue, queue.SyncJobPayload{
		ConnectionID: connID,
		LookbackDays: 7,
	})
	if err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}

	workerCfg := queue.DefaultWorkerConfig()
	workerCfg.QueueName = uniqueQueue
	workerCfg.LockDurationSeconds = 10
	workerCfg.RenewalInterval = 20 * time.Millisecond
	worker := queue.NewWorker(workerPool, workerCfg)

	contextCancelled := make(chan bool, 1)

	worker.RegisterHandler(queue.JobTypeBankSync, func(jobCtx context.Context, job *queue.JobRecord) error {
		// Steal the job out from under this worker by changing locked_by in DB
		_ = database.WithFamilyTx(ctx, workerPool, familyID, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				UPDATE public.worker_jobs
				SET locked_by = 'thief-worker',
				    updated_at = NOW()
				WHERE family_id = $1 AND id = $2
			`, familyID, jobID)
			return err
		})

		// Wait for lease renewal ticker to trigger and cancel jobCtx
		select {
		case <-jobCtx.Done():
			contextCancelled <- true
			return jobCtx.Err()
		case <-time.After(500 * time.Millisecond):
			contextCancelled <- false
			return errors.New("timeout waiting for context cancellation")
		}
	})

	job, err := worker.ClaimJob(ctx, "victim-worker")
	if err != nil || job == nil {
		t.Fatalf("claim failed: %v", err)
	}

	_ = worker.ProcessJobOnce(ctx, job)

	cancelled := <-contextCancelled
	if !cancelled {
		t.Errorf("expected job context to be cancelled when lease was stolen, but context remained active")
	}
}

func TestWorker_LeaseRenewal_LostLeaseHandlerReturnsNil(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()
	workerPool := getWorkerPool(t)
	defer workerPool.Close()
	ctx := context.Background()

	familyID, _, connID := setupTenantFixture(t, "fixture")
	uniqueQueue := fmt.Sprintf("queue_renew_nil_%s", uuid.New().String()[:8])

	jobID, err := queue.EnqueueSyncJobWithQueue(ctx, appPool, familyID, uniqueQueue, queue.SyncJobPayload{
		ConnectionID: connID,
		LookbackDays: 7,
	})
	if err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}

	workerCfg := queue.DefaultWorkerConfig()
	workerCfg.QueueName = uniqueQueue
	workerCfg.LockDurationSeconds = 10
	workerCfg.RenewalInterval = 20 * time.Millisecond
	worker := queue.NewWorker(workerPool, workerCfg)

	worker.RegisterHandler(queue.JobTypeBankSync, func(jobCtx context.Context, job *queue.JobRecord) error {
		// Steal the job out from under this worker
		_ = database.WithFamilyTx(ctx, workerPool, familyID, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				UPDATE public.worker_jobs
				SET locked_by = 'thief-worker',
				    updated_at = NOW()
				WHERE family_id = $1 AND id = $2
			`, familyID, jobID)
			return err
		})

		// Wait for renewal goroutine to trigger and fail
		select {
		case <-jobCtx.Done():
		case <-time.After(500 * time.Millisecond):
		}

		// Handler rogue behavior: ignores context cancellation and returns nil
		return nil
	})

	job, err := worker.ClaimJob(ctx, "victim-worker")
	if err != nil || job == nil {
		t.Fatalf("claim failed: %v", err)
	}

	// Authoritative lease renewal check: ProcessJobOnce MUST return ErrLeaseLost and NOT mark completed
	err = worker.ProcessJobOnce(ctx, job)
	if err == nil || !errors.Is(err, queue.ErrLeaseLost) {
		t.Fatalf("expected ErrLeaseLost, got: %v", err)
	}

	// Job in DB must NOT be completed
	_ = database.WithFamilyTx(ctx, workerPool, familyID, func(tx pgx.Tx) error {
		var state string
		_ = tx.QueryRow(ctx, "SELECT state FROM public.worker_jobs WHERE family_id = $1 AND id = $2", familyID, jobID).Scan(&state)
		if state == queue.StateCompleted {
			t.Fatalf("CRITICAL DEFECT: job marked completed despite lost lease!")
		}
		return nil
	})
}

func TestWorker_ExpiredButNotYetReclaimed_CannotComplete(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()
	workerPool := getWorkerPool(t)
	defer workerPool.Close()
	ctx := context.Background()

	familyID, _, connID := setupTenantFixture(t, "fixture")
	uniqueQueue := fmt.Sprintf("queue_expired_unclaimed_%s", uuid.New().String()[:8])

	jobID, err := queue.EnqueueSyncJobWithQueue(ctx, appPool, familyID, uniqueQueue, queue.SyncJobPayload{
		ConnectionID: connID,
		LookbackDays: 7,
	})
	if err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}

	workerCfg := queue.DefaultWorkerConfig()
	workerCfg.QueueName = uniqueQueue
	workerCfg.LockDurationSeconds = 10
	workerCfg.RenewalInterval = 0 // Disable renewal
	worker := queue.NewWorker(workerPool, workerCfg)

	job, err := worker.ClaimJob(ctx, "expired-worker")
	if err != nil || job == nil {
		t.Fatalf("claim failed: %v", err)
	}

	// Expire the lease in DB while keeping state = running and locked_by = expired-worker (not yet reclaimed)
	_ = database.WithFamilyTx(ctx, workerPool, familyID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE public.worker_jobs
			SET locked_until = NOW() - INTERVAL '10 seconds',
			    updated_at = NOW()
			WHERE family_id = $1 AND id = $2
		`, familyID, jobID)
		return err
	})

	// 1. Attempting to renew lease must fail because locked_until > NOW() is not satisfied
	err = worker.RenewLease(ctx, job)
	if err == nil || !errors.Is(err, queue.ErrLeaseLost) {
		t.Fatalf("expected ErrLeaseLost on expired renew, got: %v", err)
	}

	// 2. Register handler that succeeds, process job
	worker.RegisterHandler(queue.JobTypeBankSync, func(ctx context.Context, job *queue.JobRecord) error {
		return nil
	})

	err = worker.ProcessJobOnce(ctx, job)
	if err == nil || !errors.Is(err, queue.ErrLeaseLost) {
		t.Fatalf("expected ErrLeaseLost on expired completion attempt, got: %v", err)
	}

	// 3. Job state must remain uncompleted
	_ = database.WithFamilyTx(ctx, workerPool, familyID, func(tx pgx.Tx) error {
		var state string
		_ = tx.QueryRow(ctx, "SELECT state FROM public.worker_jobs WHERE family_id = $1 AND id = $2", familyID, jobID).Scan(&state)
		if state == queue.StateCompleted {
			t.Fatalf("expired worker was able to mark job completed!")
		}
		return nil
	})
}

func TestWorker_10ConcurrentWorkers_AtomicClaim(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()

	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(getWorkerConnString())
	if err != nil {
		t.Fatalf("failed to parse worker connection string: %v", err)
	}
	cfg.MaxConns = 30
	workerPool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("failed to connect worker pool: %v", err)
	}
	defer workerPool.Close()

	familyID, _, connID := setupTenantFixture(t, "fixture")
	uniqueQueue := fmt.Sprintf("queue_10workers_%s", uuid.New().String()[:8])

	const numWorkers = 10
	const numJobs = 10

	var enqueuedJobIDs []uuid.UUID
	for i := 0; i < numJobs; i++ {
		jobID, err := queue.EnqueueSyncJobWithQueue(ctx, appPool, familyID, uniqueQueue, queue.SyncJobPayload{
			ConnectionID: connID,
			LookbackDays: i + 1,
		})
		if err != nil {
			t.Fatalf("failed to enqueue job %d: %v", i, err)
		}
		enqueuedJobIDs = append(enqueuedJobIDs, jobID)
	}

	// Prepare 10 workers
	type workerResult struct {
		workerID string
		job      *queue.JobRecord
		err      error
	}

	results := make([]workerResult, numWorkers)
	var wg sync.WaitGroup
	startBarrier := make(chan struct{})

	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		workerID := fmt.Sprintf("worker-concurrency-%02d", i)
		workerCfg := queue.DefaultWorkerConfig()
		workerCfg.QueueName = uniqueQueue
		workerCfg.LockDurationSeconds = 60
		w := queue.NewWorker(workerPool, workerCfg)

		go func(idx int, wID string, workerInst *queue.Worker) {
			defer wg.Done()
			<-startBarrier
			job, claimErr := workerInst.ClaimJob(ctx, wID)
			results[idx] = workerResult{
				workerID: wID,
				job:      job,
				err:      claimErr,
			}
		}(i, workerID, w)
	}

	// Release all workers simultaneously
	close(startBarrier)
	wg.Wait()

	// Verify all claims succeeded and claimed distinct jobs
	claimedJobs := make(map[uuid.UUID]string)
	for i, res := range results {
		if res.err != nil {
			t.Fatalf("worker %d (%s) encountered claim error: %v", i, res.workerID, res.err)
		}
		if res.job == nil {
			t.Fatalf("worker %d (%s) got nil job when %d jobs were available", i, res.workerID, numJobs)
		}

		prevWorker, exists := claimedJobs[res.job.ID]
		if exists {
			t.Fatalf("CRITICAL DEFECT: double claim! Job %s simultaneously claimed by %s and %s",
				res.job.ID, prevWorker, res.workerID)
		}
		claimedJobs[res.job.ID] = res.workerID
	}

	if len(claimedJobs) != numJobs {
		t.Fatalf("expected exactly %d claimed jobs, got %d", numJobs, len(claimedJobs))
	}

	// Verify database state: each job has state='running' and locked_by matching the claiming worker
	for jobID, claimingWorker := range claimedJobs {
		err := database.WithFamilyTx(ctx, workerPool, familyID, func(tx pgx.Tx) error {
			var state, lockedBy string
			var lockedUntil time.Time
			err := tx.QueryRow(ctx, `
				SELECT state, locked_by, locked_until
				FROM public.worker_jobs
				WHERE family_id = $1 AND id = $2
			`, familyID, jobID).Scan(&state, &lockedBy, &lockedUntil)
			if err != nil {
				return err
			}
			if state != queue.StateRunning {
				t.Errorf("job %s state = %q; want %q", jobID, state, queue.StateRunning)
			}
			if lockedBy != claimingWorker {
				t.Errorf("job %s locked_by = %q; want %q", jobID, lockedBy, claimingWorker)
			}
			if !lockedUntil.After(time.Now()) {
				t.Errorf("job %s locked_until %v is not in future", jobID, lockedUntil)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("database verification failed for job %s: %v", jobID, err)
		}
	}

	// Second round of claims: all 10 workers try to claim when 0 available jobs exist
	for i := 0; i < numWorkers; i++ {
		workerID := fmt.Sprintf("worker-concurrency-%02d", i)
		workerCfg := queue.DefaultWorkerConfig()
		workerCfg.QueueName = uniqueQueue
		w := queue.NewWorker(workerPool, workerCfg)

		job, err := w.ClaimJob(ctx, workerID)
		if err != nil {
			t.Fatalf("unexpected error claiming empty queue: %v", err)
		}
		if job != nil {
			t.Fatalf("expected nil job from empty queue, got job %s claimed by %s", job.ID, workerID)
		}
	}

	// Complete all jobs concurrently and verify all transition to completed
	var completeWg sync.WaitGroup
	for _, res := range results {
		completeWg.Add(1)
		go func(r workerResult) {
			defer completeWg.Done()
			workerCfg := queue.DefaultWorkerConfig()
			workerCfg.QueueName = uniqueQueue
			w := queue.NewWorker(workerPool, workerCfg)
			w.RegisterHandler(queue.JobTypeBankSync, func(c context.Context, j *queue.JobRecord) error {
				return nil
			})
			if err := w.ProcessJobOnce(ctx, r.job); err != nil {
				t.Errorf("worker %s failed to process/complete job %s: %v", r.workerID, r.job.ID, err)
			}
		}(res)
	}
	completeWg.Wait()

	// Verify all jobs in DB are completed
	for _, jobID := range enqueuedJobIDs {
		err := database.WithFamilyTx(ctx, workerPool, familyID, func(tx pgx.Tx) error {
			var state string
			err := tx.QueryRow(ctx, `
				SELECT state
				FROM public.worker_jobs
				WHERE family_id = $1 AND id = $2
			`, familyID, jobID).Scan(&state)
			if err != nil {
				return err
			}
			if state != queue.StateCompleted {
				t.Errorf("job %s state = %q; want %q", jobID, state, queue.StateCompleted)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("DB check failed: %v", err)
		}
	}
}

func TestWorker_ClaimFencing_SameWorkerID_StaleFencing(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()
	workerPool := getWorkerPool(t)
	defer workerPool.Close()
	ctx := context.Background()

	familyID, _, connID := setupTenantFixture(t, "fixture")
	uniqueQueue := fmt.Sprintf("queue_fencing_same_%s", uuid.New().String()[:8])

	jobID, err := queue.EnqueueSyncJobWithQueue(ctx, appPool, familyID, uniqueQueue, queue.SyncJobPayload{
		ConnectionID: connID,
		LookbackDays: 7,
	})
	if err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}

	workerCfgA := queue.DefaultWorkerConfig()
	workerCfgA.QueueName = uniqueQueue
	workerCfgA.WorkerID = "worker-stable"
	workerA := queue.NewWorker(workerPool, workerCfgA)

	workerCfgB := queue.DefaultWorkerConfig()
	workerCfgB.QueueName = uniqueQueue
	workerCfgB.WorkerID = "worker-stable" // SAME worker ID
	workerB := queue.NewWorker(workerPool, workerCfgB)

	// Step 1: Worker A claims the job using WorkerID worker-stable
	jobA, err := workerA.ClaimJob(ctx, "worker-stable")
	if err != nil || jobA == nil {
		t.Fatalf("worker A failed to claim job: %v", err)
	}
	if jobA.ID != jobID {
		t.Fatalf("expected job %s, got %s", jobID, jobA.ID)
	}
	if jobA.ClaimToken == uuid.Nil {
		t.Fatalf("worker A claim token must not be nil")
	}

	// Step 2: Deterministically allow Worker A's lease to expire
	err = database.WithFamilyTx(ctx, workerPool, familyID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE public.worker_jobs
			SET locked_until = NOW() - INTERVAL '1 second',
			    updated_at = NOW()
			WHERE family_id = $1 AND id = $2
		`, familyID, jobID)
		return err
	})
	if err != nil {
		t.Fatalf("failed to expire lease: %v", err)
	}

	// Step 3: Worker B claims the SAME job using the SAME WorkerID worker-stable
	jobB, err := workerB.ClaimJob(ctx, "worker-stable")
	if err != nil || jobB == nil {
		t.Fatalf("worker B failed to claim job: %v", err)
	}
	if jobB.ID != jobID {
		t.Fatalf("expected job %s, got %s", jobID, jobB.ID)
	}

	// Step 4: Worker B receives a new claim token
	if jobB.ClaimToken == uuid.Nil {
		t.Fatalf("worker B claim token must not be nil")
	}
	if jobB.ClaimToken == jobA.ClaimToken {
		t.Fatalf("worker B received identical claim token to worker A: %v", jobB.ClaimToken)
	}

	// Step 5: Worker A attempts RenewLease
	err = workerA.RenewLease(ctx, jobA)
	if err == nil || !errors.Is(err, queue.ErrLeaseLost) {
		t.Fatalf("expected ErrLeaseLost on stale Worker A RenewLease, got: %v", err)
	}

	// Step 6: Worker A attempts MarkJobCompleted
	err = workerA.MarkJobCompleted(ctx, jobA)
	if err == nil || !errors.Is(err, queue.ErrLeaseLost) {
		t.Fatalf("expected ErrLeaseLost on stale Worker A MarkJobCompleted, got: %v", err)
	}

	// Step 7: Worker A attempts MarkJobFailed
	err = workerA.MarkJobFailed(ctx, jobA, errors.New("stale failure attempt"))
	if err == nil || !errors.Is(err, queue.ErrLeaseLost) {
		t.Fatalf("expected ErrLeaseLost on stale Worker A MarkJobFailed, got: %v", err)
	}

	// Step 8: All stale operations returned ErrLeaseLost (verified above)

	// Step 9: Worker B must retain ownership and successfully complete the job
	err = database.WithFamilyTx(ctx, workerPool, familyID, func(tx pgx.Tx) error {
		var state, lockedBy string
		var claimToken uuid.UUID
		var lockedUntil time.Time
		err := tx.QueryRow(ctx, `
			SELECT state, locked_by, claim_token, locked_until
			FROM public.worker_jobs
			WHERE family_id = $1 AND id = $2
		`, familyID, jobID).Scan(&state, &lockedBy, &claimToken, &lockedUntil)
		if err != nil {
			return err
		}
		if state != queue.StateRunning {
			t.Errorf("expected state 'running', got %s", state)
		}
		if lockedBy != "worker-stable" {
			t.Errorf("expected locked_by 'worker-stable', got %s", lockedBy)
		}
		if claimToken != jobB.ClaimToken {
			t.Errorf("expected claim_token %v, got %v", jobB.ClaimToken, claimToken)
		}
		if !lockedUntil.After(time.Now()) {
			t.Errorf("expected locked_until in the future, got %v", lockedUntil)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("failed to verify Worker B ownership: %v", err)
	}

	// Worker B successfully completes the job
	err = workerB.MarkJobCompleted(ctx, jobB)
	if err != nil {
		t.Fatalf("worker B MarkJobCompleted failed: %v", err)
	}

	// Verify job in DB is completed and claim_token cleared
	err = database.WithFamilyTx(ctx, workerPool, familyID, func(tx pgx.Tx) error {
		var state string
		var claimToken *uuid.UUID
		err := tx.QueryRow(ctx, `
			SELECT state, claim_token
			FROM public.worker_jobs
			WHERE family_id = $1 AND id = $2
		`, familyID, jobID).Scan(&state, &claimToken)
		if err != nil {
			return err
		}
		if state != queue.StateCompleted {
			t.Errorf("expected state 'completed', got %s", state)
		}
		if claimToken != nil {
			t.Errorf("expected claim_token to be NULL, got %v", *claimToken)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("final DB check failed: %v", err)
	}
}

func TestWorker_ClaimFencing_DifferentWorkerID_StaleFencing(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()
	workerPool := getWorkerPool(t)
	defer workerPool.Close()
	ctx := context.Background()

	familyID, _, connID := setupTenantFixture(t, "fixture")
	uniqueQueue := fmt.Sprintf("queue_fencing_diff_%s", uuid.New().String()[:8])

	jobID, err := queue.EnqueueSyncJobWithQueue(ctx, appPool, familyID, uniqueQueue, queue.SyncJobPayload{
		ConnectionID: connID,
		LookbackDays: 7,
	})
	if err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}

	workerCfgA := queue.DefaultWorkerConfig()
	workerCfgA.QueueName = uniqueQueue
	workerA := queue.NewWorker(workerPool, workerCfgA)

	workerCfgB := queue.DefaultWorkerConfig()
	workerCfgB.QueueName = uniqueQueue
	workerB := queue.NewWorker(workerPool, workerCfgB)

	// Worker A claims with worker-node-1
	jobA, err := workerA.ClaimJob(ctx, "worker-node-1")
	if err != nil || jobA == nil {
		t.Fatalf("worker A failed to claim job: %v", err)
	}

	// Expire lease
	err = database.WithFamilyTx(ctx, workerPool, familyID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE public.worker_jobs
			SET locked_until = NOW() - INTERVAL '1 second',
			    updated_at = NOW()
			WHERE family_id = $1 AND id = $2
		`, familyID, jobID)
		return err
	})
	if err != nil {
		t.Fatalf("failed to expire lease: %v", err)
	}

	// Worker B claims with worker-node-2
	jobB, err := workerB.ClaimJob(ctx, "worker-node-2")
	if err != nil || jobB == nil {
		t.Fatalf("worker B failed to claim job: %v", err)
	}

	// Worker A stale operations must all fail
	if err := workerA.RenewLease(ctx, jobA); !errors.Is(err, queue.ErrLeaseLost) {
		t.Fatalf("expected ErrLeaseLost on RenewLease, got %v", err)
	}
	if err := workerA.MarkJobCompleted(ctx, jobA); !errors.Is(err, queue.ErrLeaseLost) {
		t.Fatalf("expected ErrLeaseLost on MarkJobCompleted, got %v", err)
	}
	if err := workerA.MarkJobFailed(ctx, jobA, errors.New("stale failure")); !errors.Is(err, queue.ErrLeaseLost) {
		t.Fatalf("expected ErrLeaseLost on MarkJobFailed, got %v", err)
	}

	// Worker B completes successfully
	if err := workerB.MarkJobCompleted(ctx, jobB); err != nil {
		t.Fatalf("worker B failed to complete: %v", err)
	}

	// Verify in DB
	err = database.WithFamilyTx(ctx, workerPool, familyID, func(tx pgx.Tx) error {
		var state, lockedBy string
		var claimToken *uuid.UUID
		err := tx.QueryRow(ctx, `
			SELECT state, locked_by, claim_token
			FROM public.worker_jobs
			WHERE family_id = $1 AND id = $2
		`, familyID, jobID).Scan(&state, &lockedBy, &claimToken)
		if err != nil {
			return err
		}
		if state != queue.StateCompleted {
			t.Errorf("expected completed, got %s", state)
		}
		if lockedBy != "worker-node-2" {
			t.Errorf("expected worker-node-2, got %s", lockedBy)
		}
		if claimToken != nil {
			t.Errorf("expected nil claimToken, got %v", *claimToken)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("db verification failed: %v", err)
	}
}

func TestWorker_SecurityDefinerPrivileges_ActualClaimFlow(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()
	workerPool := getWorkerPool(t)
	defer workerPool.Close()
	ctx := context.Background()

	familyID, _, connID := setupTenantFixture(t, "fixture")
	uniqueQueue := fmt.Sprintf("queue_secdef_%s", uuid.New().String()[:8])

	// 1. Create an available job using family_app
	jobID, err := queue.EnqueueSyncJobWithQueue(ctx, appPool, familyID, uniqueQueue, queue.SyncJobPayload{
		ConnectionID: connID,
		LookbackDays: 7,
	})
	if err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}

	// 2. Switch to family_worker pool and verify current database user
	var currentUser string
	err = workerPool.QueryRow(ctx, "SELECT CURRENT_USER").Scan(&currentUser)
	if err != nil {
		t.Fatalf("failed to query current user on worker pool: %v", err)
	}
	if currentUser != "family_worker" {
		t.Fatalf("workerPool user is %q; expected %q", currentUser, "family_worker")
	}

	// 3. Call queue_sec.claim_next_worker_job() directly as family_worker
	var claimedID, claimedFamilyID, claimedToken uuid.UUID
	var jobType string
	var payload []byte
	err = workerPool.QueryRow(ctx, `
		SELECT job_id, family_id, job_type, payload, claim_token
		FROM queue_sec.claim_next_worker_job($1, $2, $3)
	`, uniqueQueue, "worker-secdef-identity", 300).Scan(&claimedID, &claimedFamilyID, &jobType, &payload, &claimedToken)
	if err != nil {
		t.Fatalf("family_worker failed to execute queue_sec.claim_next_worker_job: %v", err)
	}

	// 4. Verify that a job is returned with a non-null claim_token
	if claimedID != jobID {
		t.Fatalf("claimed job ID mismatch: got %s, want %s", claimedID, jobID)
	}
	if claimedFamilyID != familyID {
		t.Fatalf("claimed family ID mismatch: got %s, want %s", claimedFamilyID, familyID)
	}
	if claimedToken == uuid.Nil {
		t.Fatalf("claimed claim_token must not be nil")
	}

	// 5. Verify that the job transitions to running, and
	// 6. Verify that the token stored in the database matches the returned token
	err = database.WithFamilyTx(ctx, workerPool, familyID, func(tx pgx.Tx) error {
		var state, lockedBy string
		var dbToken uuid.UUID
		var lockedUntil time.Time
		err := tx.QueryRow(ctx, `
			SELECT state, locked_by, claim_token, locked_until
			FROM public.worker_jobs
			WHERE family_id = $1 AND id = $2
		`, familyID, jobID).Scan(&state, &lockedBy, &dbToken, &lockedUntil)
		if err != nil {
			return err
		}
		if state != queue.StateRunning {
			t.Errorf("job state in DB = %q; want %q", state, queue.StateRunning)
		}
		if lockedBy != "worker-secdef-identity" {
			t.Errorf("job locked_by in DB = %q; want %q", lockedBy, "worker-secdef-identity")
		}
		if dbToken != claimedToken {
			t.Errorf("job claim_token in DB = %v; want %v", dbToken, claimedToken)
		}
		if !lockedUntil.After(time.Now()) {
			t.Errorf("job locked_until in DB is not in future: %v", lockedUntil)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("failed to verify job state in DB: %v", err)
	}

	// 7. Verify unauthorized direct operations by family_worker remain rejected
	// Direct UPDATE on job_type
	err = database.WithFamilyTx(ctx, workerPool, familyID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "UPDATE public.worker_jobs SET job_type = 'malicious' WHERE family_id = $1 AND id = $2", familyID, jobID)
		return err
	})
	if err == nil {
		t.Fatalf("expected permission denied when family_worker attempts direct UPDATE on job_type, got nil")
	}

	// Direct UPDATE on payload
	err = database.WithFamilyTx(ctx, workerPool, familyID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "UPDATE public.worker_jobs SET payload = '{}' WHERE family_id = $1 AND id = $2", familyID, jobID)
		return err
	})
	if err == nil {
		t.Fatalf("expected permission denied when family_worker attempts direct UPDATE on payload, got nil")
	}

	// Direct INSERT on worker_jobs
	err = database.WithFamilyTx(ctx, workerPool, familyID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO public.worker_jobs (family_id, queue_name, job_type, payload)
			VALUES ($1, $2, 'test', '{}')
		`, familyID, uniqueQueue)
		return err
	})
	if err == nil {
		t.Fatalf("expected permission denied when family_worker attempts direct INSERT on worker_jobs, got nil")
	}

	// Direct DELETE on worker_jobs
	err = database.WithFamilyTx(ctx, workerPool, familyID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "DELETE FROM public.worker_jobs WHERE family_id = $1 AND id = $2", familyID, jobID)
		return err
	})
	if err == nil {
		t.Fatalf("expected permission denied when family_worker attempts direct DELETE on worker_jobs, got nil")
	}

	// 8. Verify SECURITY DEFINER catalog assertions and role privileges
	pgPool := getPGPool(t)
	defer pgPool.Close()

	var owner string
	var isSecDefiner bool
	err = pgPool.QueryRow(ctx, `
		SELECT p.proowner::regrole::text, p.prosecdef
		FROM pg_proc p
		JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname = 'queue_sec' AND p.proname = 'claim_next_worker_job'
	`).Scan(&owner, &isSecDefiner)
	if err != nil {
		t.Fatalf("failed to query pg_proc for claim_next_worker_job: %v", err)
	}
	if owner != "family_queue_definer" {
		t.Errorf("function owner = %q; want %q", owner, "family_queue_definer")
	}
	if !isSecDefiner {
		t.Errorf("prosecdef = false; want true (SECURITY DEFINER)")
	}

	var definerHasUpdate bool
	err = pgPool.QueryRow(ctx, `
		SELECT has_column_privilege('family_queue_definer', 'public.worker_jobs', 'claim_token', 'UPDATE')
	`).Scan(&definerHasUpdate)
	if err != nil || !definerHasUpdate {
		t.Errorf("expected family_queue_definer to have UPDATE on claim_token, got %v, err=%v", definerHasUpdate, err)
	}

	var publicCanExec bool
	err = pgPool.QueryRow(ctx, `
		SELECT has_function_privilege('public', 'queue_sec.claim_next_worker_job(varchar, varchar, int)', 'EXECUTE')
	`).Scan(&publicCanExec)
	if err != nil || publicCanExec {
		t.Errorf("expected public to NOT have EXECUTE on claim_next_worker_job, got %v, err=%v", publicCanExec, err)
	}
}

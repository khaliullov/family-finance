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

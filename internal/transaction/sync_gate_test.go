package transaction

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/khaliullov/family-finance/internal/connector"
	"github.com/khaliullov/family-finance/internal/database"
	"github.com/khaliullov/family-finance/internal/queue"
)

func TestSyncGate_RejectionByLifecycleState(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()
	ctx := context.Background()

	gate := NewSyncGate(appPool)

	rejectedStates := []struct {
		status  string
		wantErr error
	}{
		{ConnectionStateCreated, ErrSyncGateInvalidState},
		{ConnectionStateAuthenticating, ErrSyncGateInvalidState},
		{ConnectionStateValidationPending, ErrSyncGateInvalidState},
		{ConnectionStateValidating, ErrSyncGateInvalidState},
		{ConnectionStateRequiresReauth, ErrSyncGateAuthExpired},
		{ConnectionStateExpired, ErrSyncGateAuthExpired},
		{ConnectionStateDisconnected, ErrSyncGateConnectionDisconnected},
		{ConnectionStateValidationFailed, ErrSyncGateValidationFailed},
	}

	for _, tt := range rejectedStates {
		t.Run(tt.status, func(t *testing.T) {
			familyID, _, connID := setupTenantFixture(t, "fixture")

			// Update connection to test state
			err := database.WithFamilyTx(ctx, appPool, familyID, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `
					UPDATE public.bank_connections
					SET status = $1, updated_at = NOW()
					WHERE family_id = $2 AND id = $3
				`, tt.status, familyID, connID)
				return err
			})
			if err != nil {
				t.Fatalf("failed to set connection status: %v", err)
			}

			_, _, err = gate.VerifyCanSync(ctx, familyID, connID)
			if err == nil {
				t.Fatalf("expected error for status %q, got nil", tt.status)
			}
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("status %q: got error %v, want %v", tt.status, err, tt.wantErr)
			}
		})
	}
}

func TestSyncGate_MissingCredentials_Rejected(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()
	ctx := context.Background()

	gate := NewSyncGate(appPool)
	familyID, _, connID := setupTenantFixture(t, "fixture")

	// Set credentials to empty
	err := database.WithFamilyTx(ctx, appPool, familyID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE public.bank_connections
			SET encrypted_credentials = NULL, updated_at = NOW()
			WHERE family_id = $1 AND id = $2
		`, familyID, connID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = gate.VerifyCanSync(ctx, familyID, connID)
	if !errors.Is(err, ErrSyncGateCredentialsMissing) {
		t.Fatalf("expected ErrSyncGateCredentialsMissing, got %v", err)
	}
}

func TestSyncGate_MissingValidationReport_Rejected(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()
	ctx := context.Background()

	gate := NewSyncGate(appPool)
	familyID, _, connID := setupTenantFixture(t, "fixture")

	// Clear validation report reference
	err := database.WithFamilyTx(ctx, appPool, familyID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE public.bank_connections
			SET last_validated_at = NULL,
			    last_validation_status = NULL,
			    last_validation_report_id = NULL,
			    updated_at = NOW()
			WHERE family_id = $1 AND id = $2
		`, familyID, connID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = gate.VerifyCanSync(ctx, familyID, connID)
	if !errors.Is(err, ErrSyncGateValidationRequired) {
		t.Fatalf("expected ErrSyncGateValidationRequired, got %v", err)
	}
}

func TestSyncGate_StaleValidationReport_SessionVersionMismatch(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()
	ctx := context.Background()

	gate := NewSyncGate(appPool)
	familyID, _, connID := setupTenantFixture(t, "fixture")

	// Simulate credential update: increment connection session_version without re-validation
	err := database.WithFamilyTx(ctx, appPool, familyID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE public.bank_connections
			SET session_version = session_version + 1, updated_at = NOW()
			WHERE family_id = $1 AND id = $2
		`, familyID, connID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = gate.VerifyCanSync(ctx, familyID, connID)
	if !errors.Is(err, ErrSyncGateValidationStale) {
		t.Fatalf("expected ErrSyncGateValidationStale for session_version mismatch, got %v", err)
	}
}

func TestSyncGate_StaleValidationReport_CredentialHashMismatch(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()
	ctx := context.Background()

	gate := NewSyncGate(appPool)
	familyID, _, connID := setupTenantFixture(t, "fixture")

	// Update credential_hash on connection to new hash
	err := database.WithFamilyTx(ctx, appPool, familyID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE public.bank_connections
			SET credential_hash = 'new_replaced_credential_hash_xyz', updated_at = NOW()
			WHERE family_id = $1 AND id = $2
		`, familyID, connID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = gate.VerifyCanSync(ctx, familyID, connID)
	if !errors.Is(err, ErrSyncGateValidationStale) {
		t.Fatalf("expected ErrSyncGateValidationStale for credential_hash mismatch, got %v", err)
	}
}

func TestSyncGate_ValidationSeverity_Critical_BlocksSync(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()
	ctx := context.Background()

	gate := NewSyncGate(appPool)
	familyID, _, connID := setupTenantFixture(t, "fixture")

	reportID := uuid.New()
	reportPayload := []byte(`{
		"id": "` + reportID.String() + `",
		"family_id": "` + familyID.String() + `",
		"connection_id": "` + connID.String() + `",
		"session_version": 1,
		"credential_hash": "dummy_hash_for_tests",
		"overall_result": "failed",
		"summary": {"connection_id": "` + connID.String() + `"},
		"authentication": {"authenticated": true, "session_valid": true},
		"accounts": {"discovered": 1, "valid": 1},
		"cards": {"discovered": 1, "valid": 0},
		"errors": [
			{
				"code": "CARD_UNMASKED_PAN",
				"severity": "critical",
				"message": "Security violation: card PAN is not masked",
				"stage": "cards"
			}
		]
	}`)

	err := database.WithFamilyTx(ctx, appPool, familyID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO public.bank_validation_reports (
				id, family_id, bank_connection_id, session_version, credential_hash,
				overall_result, started_at, finished_at, duration_ms, report_payload
			) VALUES (
				$1, $2, $3, 1, 'dummy_hash_for_tests', 'failed', NOW(), NOW(), 10, $4
			)
		`, reportID, familyID, connID, reportPayload)
		if err != nil {
			return err
		}

		_, err = tx.Exec(ctx, `
			UPDATE public.bank_connections
			SET last_validation_report_id = $1,
			    last_validation_status = 'failed',
			    status = 'connected',
			    updated_at = NOW()
			WHERE family_id = $2 AND id = $3
		`, reportID, familyID, connID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = gate.VerifyCanSync(ctx, familyID, connID)
	if !errors.Is(err, ErrSyncGateValidationFailed) {
		t.Fatalf("expected ErrSyncGateValidationFailed for critical error, got %v", err)
	}
}

func TestSyncGate_ValidationSeverity_Error_BlocksSync(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()
	ctx := context.Background()

	gate := NewSyncGate(appPool)
	familyID, _, connID := setupTenantFixture(t, "fixture")

	reportID := uuid.New()
	reportPayload := []byte(`{
		"id": "` + reportID.String() + `",
		"family_id": "` + familyID.String() + `",
		"connection_id": "` + connID.String() + `",
		"session_version": 1,
		"credential_hash": "dummy_hash_for_tests",
		"overall_result": "failed",
		"summary": {"connection_id": "` + connID.String() + `"},
		"authentication": {"authenticated": true, "session_valid": true},
		"accounts": {"discovered": 1, "valid": 1},
		"transactions": {"fetched": 1, "valid": 0, "unsupported_types": ["crypto_transfer"]},
		"errors": [
			{
				"code": "TRANSACTION_UNSUPPORTED_TYPE",
				"severity": "error",
				"message": "Unsupported transaction operation type: crypto_transfer",
				"stage": "transactions"
			}
		]
	}`)

	err := database.WithFamilyTx(ctx, appPool, familyID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO public.bank_validation_reports (
				id, family_id, bank_connection_id, session_version, credential_hash,
				overall_result, started_at, finished_at, duration_ms, report_payload
			) VALUES (
				$1, $2, $3, 1, 'dummy_hash_for_tests', 'failed', NOW(), NOW(), 10, $4
			)
		`, reportID, familyID, connID, reportPayload)
		if err != nil {
			return err
		}

		_, err = tx.Exec(ctx, `
			UPDATE public.bank_connections
			SET last_validation_report_id = $1,
			    last_validation_status = 'failed',
			    status = 'connected',
			    updated_at = NOW()
			WHERE family_id = $2 AND id = $3
		`, reportID, familyID, connID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = gate.VerifyCanSync(ctx, familyID, connID)
	if !errors.Is(err, ErrSyncGateValidationFailed) {
		t.Fatalf("expected ErrSyncGateValidationFailed for blocking error, got %v", err)
	}
}

func TestSyncGate_ValidationWarningOnly_PermitsSync(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()
	ctx := context.Background()

	gate := NewSyncGate(appPool)
	familyID, _, connID := setupTenantFixture(t, "fixture")

	reportID := uuid.New()
	reportPayload := []byte(`{
		"id": "` + reportID.String() + `",
		"family_id": "` + familyID.String() + `",
		"connection_id": "` + connID.String() + `",
		"session_version": 1,
		"credential_hash": "dummy_hash_for_tests",
		"overall_result": "partial_success",
		"summary": {"connection_id": "` + connID.String() + `"},
		"authentication": {"authenticated": true, "session_valid": true},
		"accounts": {"discovered": 1, "valid": 1},
		"transactions": {"fetched": 0, "valid": 0},
		"errors": [
			{
				"code": "ACCOUNTS_ZERO_DISCOVERED",
				"severity": "warning",
				"message": "Zero accounts discovered for connection",
				"stage": "accounts"
			}
		]
	}`)

	err := database.WithFamilyTx(ctx, appPool, familyID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO public.bank_validation_reports (
				id, family_id, bank_connection_id, session_version, credential_hash,
				overall_result, started_at, finished_at, duration_ms, report_payload
			) VALUES (
				$1, $2, $3, 1, 'dummy_hash_for_tests', 'partial_success', NOW(), NOW(), 10, $4
			)
		`, reportID, familyID, connID, reportPayload)
		if err != nil {
			return err
		}

		_, err = tx.Exec(ctx, `
			UPDATE public.bank_connections
			SET last_validation_report_id = $1,
			    last_validation_status = 'partial_success',
			    status = 'validation_passed',
			    updated_at = NOW()
			WHERE family_id = $2 AND id = $3
		`, reportID, familyID, connID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	conn, report, err := gate.VerifyCanSync(ctx, familyID, connID)
	if err != nil {
		t.Fatalf("expected sync permitted for warning-only report, got error: %v", err)
	}
	if conn == nil || report == nil {
		t.Fatal("expected non-nil connection and report")
	}
}

func TestSyncGate_ProductionFixture_Rejected(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()
	ctx := context.Background()

	gate := NewSyncGate(appPool)
	familyID, _, connID := setupTenantFixture(t, "fixture")

	// Set ENVIRONMENT=production
	t.Setenv("ENVIRONMENT", "production")

	_, _, err := gate.VerifyCanSync(ctx, familyID, connID)
	if !errors.Is(err, ErrSyncGateProductionUnsupported) {
		t.Fatalf("expected ErrSyncGateProductionUnsupported in production mode, got %v", err)
	}
}

func TestSyncGate_CrossFamily_Rejected(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()
	ctx := context.Background()

	gate := NewSyncGate(appPool)
	_, _, connA := setupTenantFixture(t, "fixture")
	familyB, _, _ := setupTenantFixture(t, "fixture")

	// Attempt to verify sync for Family B with Connection A
	_, _, err := gate.VerifyCanSync(ctx, familyB, connA)
	if err == nil {
		t.Fatal("expected cross-family tenant isolation rejection, got nil")
	}
	if !errors.Is(err, ErrConnectionNotFound) {
		t.Errorf("expected ErrConnectionNotFound for cross-family query, got %v", err)
	}
}

func TestSyncGate_DirectServiceInvocation_CannotBypassGate(t *testing.T) {
	workerPool := getWorkerPool(t)
	defer workerPool.Close()
	appPool := getAppPool(t)
	defer appPool.Close()
	ctx := context.Background()

	familyID, _, connID := setupTenantFixture(t, "fixture")

	// Disconnect connection
	err := database.WithFamilyTx(ctx, appPool, familyID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE public.bank_connections
			SET status = 'disconnected', updated_at = NOW()
			WHERE family_id = $1 AND id = $2
		`, familyID, connID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	reg := connector.NewRegistry()
	svc := NewService(workerPool, reg)

	_, err = svc.SyncConnection(ctx, familyID, connID, SyncOptions{})
	if err == nil {
		t.Fatal("expected direct Service.SyncConnection to fail closed for disconnected connection, got nil")
	}
	if !errors.Is(err, ErrConnectionNotActive) {
		t.Errorf("expected ErrConnectionNotActive, got %v", err)
	}
}

func TestSyncGate_WorkerJob_CannotBypassGate(t *testing.T) {
	workerPool := getWorkerPool(t)
	defer workerPool.Close()
	appPool := getAppPool(t)
	defer appPool.Close()
	ctx := context.Background()

	familyID, _, connID := setupTenantFixture(t, "fixture")

	// Invalidate validation report on connection
	err := database.WithFamilyTx(ctx, appPool, familyID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE public.bank_connections
			SET last_validated_at = NULL, last_validation_report_id = NULL, updated_at = NOW()
			WHERE family_id = $1 AND id = $2
		`, familyID, connID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	reg := connector.NewRegistry()
	svc := NewService(workerPool, reg)

	job := &queue.JobRecord{
		ID:       uuid.New(),
		FamilyID: familyID,
		JobType:  queue.JobTypeBankSync,
		Payload:  []byte(`{"connection_id": "` + connID.String() + `", "lookback_days": 30}`),
	}

	err = svc.ProcessBankSyncJob(ctx, job)
	if err == nil {
		t.Fatal("expected worker ProcessBankSyncJob to fail closed when validation report is missing, got nil")
	}
}

func TestSyncGate_APIEndpoint_CannotBypassGate(t *testing.T) {
	mux, _, tokenGen := setupTestRouter(t, nil)
	familyID, userID, connID := setupTenantFixture(t, "fixture")
	token := tokenGen(userID, familyID)

	appPool := getAppPool(t)
	defer appPool.Close()
	ctx := context.Background()

	// Set status to validating
	err := database.WithFamilyTx(ctx, appPool, familyID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE public.bank_connections
			SET status = 'validating', updated_at = NOW()
			WHERE family_id = $1 AND id = $2
		`, familyID, connID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/connections/"+connID.String()+"/sync", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected HTTP 409 Conflict for connection in validating state, got %d (body: %s)", rec.Code, rec.Body.String())
	}
}

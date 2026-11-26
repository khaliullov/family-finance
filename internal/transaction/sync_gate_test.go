package transaction

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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

	// Update credential_hash on connection to new hash (valid 64-hex SHA-256)
	newHash := "1111111111111111111111111111111111111111111111111111111111111111"
	err := database.WithFamilyTx(ctx, appPool, familyID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE public.bank_connections
			SET credential_hash = $1, updated_at = NOW()
			WHERE family_id = $2 AND id = $3
		`, newHash, familyID, connID)
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

	fixtureHash := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	reportID := uuid.New()
	reportPayload := []byte(`{
		"id": "` + reportID.String() + `",
		"family_id": "` + familyID.String() + `",
		"connection_id": "` + connID.String() + `",
		"session_version": 1,
		"credential_hash": "` + fixtureHash + `",
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
				$1, $2, $3, 1, $4, 'failed', NOW(), NOW(), 10, $5
			)
		`, reportID, familyID, connID, fixtureHash, reportPayload)
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

	fixtureHash := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	reportID := uuid.New()
	reportPayload := []byte(`{
		"id": "` + reportID.String() + `",
		"family_id": "` + familyID.String() + `",
		"connection_id": "` + connID.String() + `",
		"session_version": 1,
		"credential_hash": "` + fixtureHash + `",
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
				$1, $2, $3, 1, $4, 'failed', NOW(), NOW(), 10, $5
			)
		`, reportID, familyID, connID, fixtureHash, reportPayload)
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

	fixtureHash := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	reportID := uuid.New()
	reportPayload := []byte(`{
		"id": "` + reportID.String() + `",
		"family_id": "` + familyID.String() + `",
		"connection_id": "` + connID.String() + `",
		"session_version": 1,
		"credential_hash": "` + fixtureHash + `",
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
				$1, $2, $3, 1, $4, 'partial_success', NOW(), NOW(), 10, $5
			)
		`, reportID, familyID, connID, fixtureHash, reportPayload)
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

func TestSyncGate_StrictCredentialHash_Validation(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()
	ctx := context.Background()

	gate := NewSyncGate(appPool)

	t.Run("missing_connection_credential_hash", func(t *testing.T) {
		familyID, _, connID := setupTenantFixture(t, "fixture")
		err := database.WithFamilyTx(ctx, appPool, familyID, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				UPDATE public.bank_connections
				SET credential_hash = NULL, updated_at = NOW()
				WHERE family_id = $1 AND id = $2
			`, familyID, connID)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}

		_, _, err = gate.VerifyCanSync(ctx, familyID, connID)
		if !errors.Is(err, ErrSyncGateMissingCredentialHash) {
			t.Fatalf("expected ErrSyncGateMissingCredentialHash, got %v", err)
		}
	})

	t.Run("missing_report_credential_hash", func(t *testing.T) {
		familyID, _, connID := setupTenantFixture(t, "fixture")
		newReportID := uuid.New()
		err := database.WithFamilyTx(ctx, appPool, familyID, func(tx pgx.Tx) error {
			reportPayload := []byte(`{"overall_result":"passed","authentication":{"authenticated":true,"session_valid":true},"accounts":{"discovered":1,"valid":1},"transactions":{"fetched":1,"valid":1},"data_integrity":{"tenant_isolation_passed":true,"idempotency_passed":true}}`)
			_, err := tx.Exec(ctx, `
				INSERT INTO public.bank_validation_reports (
					id, family_id, bank_connection_id, session_version, credential_hash,
					overall_result, started_at, finished_at, duration_ms, report_payload
				) VALUES (
					$1, $2, $3, 1, '', 'passed', NOW(), NOW(), 10, $4
				)
			`, newReportID, familyID, connID, reportPayload)
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `
				UPDATE public.bank_connections
				SET last_validation_report_id = $1, updated_at = NOW()
				WHERE family_id = $2 AND id = $3
			`, newReportID, familyID, connID)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}

		_, _, err = gate.VerifyCanSync(ctx, familyID, connID)
		if !errors.Is(err, ErrSyncGateMissingReportHash) {
			t.Fatalf("expected ErrSyncGateMissingReportHash, got %v", err)
		}
	})

	t.Run("invalid_hex_format_on_connection", func(t *testing.T) {
		familyID, _, connID := setupTenantFixture(t, "fixture")
		err := database.WithFamilyTx(ctx, appPool, familyID, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				UPDATE public.bank_connections
				SET credential_hash = 'not_a_64_character_hex_hash_dummy_value_here', updated_at = NOW()
				WHERE family_id = $1 AND id = $2
			`, familyID, connID)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}

		_, _, err = gate.VerifyCanSync(ctx, familyID, connID)
		if !errors.Is(err, ErrSyncGateInvalidCredentialHash) {
			t.Fatalf("expected ErrSyncGateInvalidCredentialHash, got %v", err)
		}
	})

	t.Run("invalid_hex_format_on_report", func(t *testing.T) {
		familyID, _, connID := setupTenantFixture(t, "fixture")
		newReportID := uuid.New()
		err := database.WithFamilyTx(ctx, appPool, familyID, func(tx pgx.Tx) error {
			nonHexHash := strings.Repeat("z", 64)
			reportPayload := []byte(`{"overall_result":"passed","authentication":{"authenticated":true,"session_valid":true},"accounts":{"discovered":1,"valid":1},"transactions":{"fetched":1,"valid":1},"data_integrity":{"tenant_isolation_passed":true,"idempotency_passed":true}}`)
			_, err := tx.Exec(ctx, `
				INSERT INTO public.bank_validation_reports (
					id, family_id, bank_connection_id, session_version, credential_hash,
					overall_result, started_at, finished_at, duration_ms, report_payload
				) VALUES (
					$1, $2, $3, 1, $4, 'passed', NOW(), NOW(), 10, $5
				)
			`, newReportID, familyID, connID, nonHexHash, reportPayload)
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `
				UPDATE public.bank_connections
				SET last_validation_report_id = $1, updated_at = NOW()
				WHERE family_id = $2 AND id = $3
			`, newReportID, familyID, connID)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}

		_, _, err = gate.VerifyCanSync(ctx, familyID, connID)
		if !errors.Is(err, ErrSyncGateInvalidCredentialHash) {
			t.Fatalf("expected ErrSyncGateInvalidCredentialHash for report hash, got %v", err)
		}
	})
}

func TestSyncGate_InitialImportApproval_Lifecycle(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()
	ctx := context.Background()

	gate := NewSyncGate(appPool)
	fixtureHash := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	t.Run("unapproved_initial_import_rejected", func(t *testing.T) {
		familyID, _, connID := setupTenantFixture(t, "fixture")

		// Reset initial_import_completed_at and clear approvals
		err := database.WithFamilyTx(ctx, appPool, familyID, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				UPDATE public.bank_connections
				SET initial_import_completed_at = NULL, updated_at = NOW()
				WHERE family_id = $1 AND id = $2
			`, familyID, connID)
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `
				DELETE FROM public.bank_initial_import_approvals
				WHERE family_id = $1 AND bank_connection_id = $2
			`, familyID, connID)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}

		_, _, err = gate.VerifyCanSync(ctx, familyID, connID)
		if !errors.Is(err, ErrSyncGateInitialImportNotApproved) {
			t.Fatalf("expected ErrSyncGateInitialImportNotApproved, got %v", err)
		}
	})

	t.Run("invalidated_approval_rejected", func(t *testing.T) {
		familyID, userID, connID := setupTenantFixture(t, "fixture")

		err := database.WithFamilyTx(ctx, appPool, familyID, func(tx pgx.Tx) error {
			var reportID uuid.UUID
			_ = tx.QueryRow(ctx, "SELECT last_validation_report_id FROM public.bank_connections WHERE id = $1", connID).Scan(&reportID)
			_, err := tx.Exec(ctx, `
				UPDATE public.bank_connections
				SET initial_import_completed_at = NULL, updated_at = NOW()
				WHERE family_id = $1 AND id = $2
			`, familyID, connID)
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `
				DELETE FROM public.bank_initial_import_approvals
				WHERE family_id = $1 AND bank_connection_id = $2
			`, familyID, connID)
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `
				INSERT INTO public.bank_initial_import_approvals (
					id, family_id, bank_connection_id, approved_by_user_id,
					validation_report_id, session_version, credential_hash,
					approved_at, invalidated_at, invalidation_reason
				) VALUES (
					gen_random_uuid(), $1, $2, $3,
					$4, 1, $5,
					NOW(), NOW(), 'reauth'
				)
			`, familyID, connID, userID, reportID, fixtureHash)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}

		_, _, err = gate.VerifyCanSync(ctx, familyID, connID)
		if !errors.Is(err, ErrSyncGateInitialImportNotApproved) {
			t.Fatalf("expected ErrSyncGateInitialImportNotApproved for invalidated approval, got %v", err)
		}
	})

	t.Run("valid_approval_permits_sync", func(t *testing.T) {
		familyID, userID, connID := setupTenantFixture(t, "fixture")

		err := database.WithFamilyTx(ctx, appPool, familyID, func(tx pgx.Tx) error {
			var reportID uuid.UUID
			_ = tx.QueryRow(ctx, "SELECT last_validation_report_id FROM public.bank_connections WHERE id = $1", connID).Scan(&reportID)
			_, err := tx.Exec(ctx, `
				UPDATE public.bank_connections
				SET initial_import_completed_at = NULL, updated_at = NOW()
				WHERE family_id = $1 AND id = $2
			`, familyID, connID)
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `
				DELETE FROM public.bank_initial_import_approvals
				WHERE family_id = $1 AND bank_connection_id = $2
			`, familyID, connID)
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `
				INSERT INTO public.bank_initial_import_approvals (
					id, family_id, bank_connection_id, approved_by_user_id,
					validation_report_id, session_version, credential_hash,
					approved_at
				) VALUES (
					gen_random_uuid(), $1, $2, $3,
					$4, 1, $5,
					NOW()
				)
			`, familyID, connID, userID, reportID, fixtureHash)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}

		conn, report, err := gate.VerifyCanSync(ctx, familyID, connID)
		if err != nil {
			t.Fatalf("expected VerifyCanSync to succeed with valid approval, got: %v", err)
		}
		if conn == nil || report == nil {
			t.Fatal("expected non-nil connection and report")
		}
	})
}

func TestSyncGate_ActiveOperationConflict(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()
	ctx := context.Background()

	gate := NewSyncGate(appPool)

	t.Run("validation_operation_active_blocks_sync", func(t *testing.T) {
		familyID, _, connID := setupTenantFixture(t, "fixture")
		err := database.WithFamilyTx(ctx, appPool, familyID, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				UPDATE public.bank_connections
				SET active_operation_type = 'validation',
				    active_sync_token = gen_random_uuid(),
				    sync_locked_until = NOW() + INTERVAL '5 minutes',
				    updated_at = NOW()
				WHERE family_id = $1 AND id = $2
			`, familyID, connID)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}

		_, _, err = gate.VerifyCanSync(ctx, familyID, connID)
		if !errors.Is(err, ErrSyncGateActiveValidationConflict) {
			t.Fatalf("expected ErrSyncGateActiveValidationConflict, got %v", err)
		}
	})

	t.Run("sync_operation_active_blocks_sync", func(t *testing.T) {
		familyID, _, connID := setupTenantFixture(t, "fixture")
		err := database.WithFamilyTx(ctx, appPool, familyID, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				UPDATE public.bank_connections
				SET active_operation_type = 'sync',
				    active_sync_token = gen_random_uuid(),
				    sync_locked_until = NOW() + INTERVAL '5 minutes',
				    updated_at = NOW()
				WHERE family_id = $1 AND id = $2
			`, familyID, connID)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}

		_, _, err = gate.VerifyCanSync(ctx, familyID, connID)
		if !errors.Is(err, ErrSyncGateActiveSyncConflict) {
			t.Fatalf("expected ErrSyncGateActiveSyncConflict, got %v", err)
		}
	})

	t.Run("expired_operation_lease_does_not_block_sync", func(t *testing.T) {
		familyID, _, connID := setupTenantFixture(t, "fixture")
		err := database.WithFamilyTx(ctx, appPool, familyID, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				UPDATE public.bank_connections
				SET active_operation_type = 'validation',
				    active_sync_token = gen_random_uuid(),
				    sync_locked_until = NOW() - INTERVAL '1 minute',
				    updated_at = NOW()
				WHERE family_id = $1 AND id = $2
			`, familyID, connID)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}

		conn, report, err := gate.VerifyCanSync(ctx, familyID, connID)
		if err != nil {
			t.Fatalf("expected expired lease to not block sync, got error: %v", err)
		}
		if conn == nil || report == nil {
			t.Fatal("expected non-nil connection and report")
		}
	})
}

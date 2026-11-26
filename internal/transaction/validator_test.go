package transaction

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/khaliullov/family-finance/internal/connector"
	"github.com/khaliullov/family-finance/internal/connector/fixture"
	"github.com/khaliullov/family-finance/internal/database"
)

// mockExpiringConnector returns ErrAuthExpired on HealthCheck to verify reauthentication transitions.
type mockExpiringConnector struct {
	*fixture.FixtureConnector
}

func (m *mockExpiringConnector) ProviderCode() string {
	return "mock_expiring"
}

func (m *mockExpiringConnector) HealthCheck(ctx context.Context, credentials []byte) error {
	return connector.ErrAuthExpired
}

func TestValidator_ReadOnlyIntegrity_NoLedgerEntries(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()

	ctx := context.Background()
	familyID, _, connID := setupTenantFixture(t, "fixture")

	// Set connection to connected with valid non-empty credentials
	err := database.WithFamilyTx(ctx, appPool, familyID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE public.bank_connections
			SET status = 'connected',
			    encrypted_credentials = $1,
			    updated_at = NOW()
			WHERE family_id = $2 AND id = $3
		`, []byte("dummy_encrypted_valid_session"), familyID, connID)
		return err
	})
	if err != nil {
		t.Fatalf("failed to prepare connection: %v", err)
	}

	registry := connector.NewRegistry()
	registry.Register(fixture.NewFixtureConnector())

	validator := NewValidator(appPool, registry, []byte("01234567890123456789012345678901"))

	// 1. Run Validation
	report, err := validator.ValidateConnection(ctx, familyID, connID)
	if err != nil {
		t.Fatalf("ValidateConnection failed: %v", err)
	}

	if report == nil {
		t.Fatal("expected non-nil ValidationReport")
	}
	if report.OverallResult != ValidationResultPassed {
		t.Errorf("overall_result = %q, want passed (errors: %+v)", report.OverallResult, report.Errors)
	}
	if !report.Authentication.Authenticated || !report.Authentication.SessionValid {
		t.Errorf("authentication check failed: %+v", report.Authentication)
	}
	if report.Accounts.Discovered == 0 || report.Accounts.Valid == 0 {
		t.Errorf("expected discovered accounts, got discovered=%d valid=%d", report.Accounts.Discovered, report.Accounts.Valid)
	}
	if report.Balances.Checked == 0 {
		t.Errorf("expected balances checked, got %d", report.Balances.Checked)
	}
	if report.Transactions.Fetched == 0 || report.Transactions.Valid == 0 {
		t.Errorf("expected valid transactions, got fetched=%d valid=%d", report.Transactions.Fetched, report.Transactions.Valid)
	}

	// 2. Critical Safety Verification: Ensure ZERO rows written to public.bank_transactions or public.accounting_entries
	err = database.WithFamilyTx(ctx, appPool, familyID, func(tx pgx.Tx) error {
		var txCount int
		err := tx.QueryRow(ctx, "SELECT count(*) FROM public.bank_transactions WHERE family_id = $1", familyID).Scan(&txCount)
		if err != nil {
			return err
		}
		if txCount != 0 {
			t.Fatalf("SAFETY VIOLATION: validation wrote %d rows to public.bank_transactions; validation must be read-only!", txCount)
		}

		var entryCount int
		err = tx.QueryRow(ctx, "SELECT count(*) FROM public.accounting_entries WHERE family_id = $1", familyID).Scan(&entryCount)
		if err != nil {
			return err
		}
		if entryCount != 0 {
			t.Fatalf("SAFETY VIOLATION: validation wrote %d rows to public.accounting_entries; validation must be read-only!", entryCount)
		}

		// 3. Verify validation report was persisted under tenant RLS
		var reportCount int
		err = tx.QueryRow(ctx, "SELECT count(*) FROM public.bank_validation_reports WHERE family_id = $1 AND bank_connection_id = $2", familyID, connID).Scan(&reportCount)
		if err != nil || reportCount != 1 {
			t.Errorf("expected 1 persisted report, got %d, err=%v", reportCount, err)
		}

		// 4. Verify connection status updated to validation_passed
		var status, valStatus string
		var reportID *uuid.UUID
		err = tx.QueryRow(ctx, `
			SELECT status, COALESCE(last_validation_status, ''), last_validation_report_id
			FROM public.bank_connections
			WHERE family_id = $1 AND id = $2
		`, familyID, connID).Scan(&status, &valStatus, &reportID)
		if err != nil {
			return err
		}
		if status != ConnectionStateValidationPassed {
			t.Errorf("status = %q, want %q", status, ConnectionStateValidationPassed)
		}
		if valStatus != string(ValidationResultPassed) {
			t.Errorf("last_validation_status = %q, want %q", valStatus, ValidationResultPassed)
		}
		if reportID == nil || *reportID != report.ID {
			t.Errorf("last_validation_report_id mismatch: got %v, want %s", reportID, report.ID)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestValidator_ExpiredSession_RequiresReauth(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()

	ctx := context.Background()
	familyID, _, connID := setupTenantFixture(t, "mock_expiring")

	// Set connection to connected with credentials
	err := database.WithFamilyTx(ctx, appPool, familyID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE public.bank_connections
			SET status = 'connected',
			    encrypted_credentials = $1,
			    updated_at = NOW()
			WHERE family_id = $2 AND id = $3
		`, []byte("expired_session_creds"), familyID, connID)
		return err
	})
	if err != nil {
		t.Fatalf("failed to update connection: %v", err)
	}

	registry := connector.NewRegistry()
	registry.Register(&mockExpiringConnector{
		FixtureConnector: fixture.NewFixtureConnector(),
	})

	validator := NewValidator(appPool, registry, []byte("01234567890123456789012345678901"))

	report, err := validator.ValidateConnection(ctx, familyID, connID)
	if err != nil {
		t.Fatalf("ValidateConnection failed: %v", err)
	}

	if report.OverallResult != ValidationResultFailed {
		t.Errorf("overall_result = %q, want failed", report.OverallResult)
	}
	if !report.Authentication.ReauthRequired {
		t.Errorf("expected ReauthRequired = true on 401/403 expired session")
	}

	// Verify connection transitioned to requires_reauthentication
	err = database.WithFamilyTx(ctx, appPool, familyID, func(tx pgx.Tx) error {
		var status string
		err := tx.QueryRow(ctx, "SELECT status FROM public.bank_connections WHERE family_id = $1 AND id = $2", familyID, connID).Scan(&status)
		if err != nil {
			return err
		}
		if status != ConnectionStateRequiresReauth {
			t.Errorf("status = %q, want %q", status, ConnectionStateRequiresReauth)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestValidator_DisconnectedConnection_Rejected(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()

	ctx := context.Background()
	familyID, _, connID := setupTenantFixture(t, "fixture")

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

	registry := connector.NewRegistry()
	registry.Register(fixture.NewFixtureConnector())

	validator := NewValidator(appPool, registry, []byte("01234567890123456789012345678901"))

	_, err = validator.ValidateConnection(ctx, familyID, connID)
	if err == nil || !errors.Is(err, ErrConnectionDisconnected) {
		t.Fatalf("expected ErrConnectionDisconnected for disconnected connection, got %v", err)
	}
}

func TestValidator_TenantIsolation(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()

	ctx := context.Background()
	_, _, connID := setupTenantFixture(t, "fixture")

	// Other family
	otherFamilyID := uuid.New()
	registry := connector.NewRegistry()
	registry.Register(fixture.NewFixtureConnector())

	validator := NewValidator(appPool, registry, []byte("01234567890123456789012345678901"))

	// Other family attempting to validate connID must fail with ErrConnectionNotFound under RLS
	_, err := validator.ValidateConnection(ctx, otherFamilyID, connID)
	if err == nil || !errors.Is(err, ErrConnectionNotFound) {
		t.Fatalf("expected ErrConnectionNotFound for cross-tenant validation attempt, got %v", err)
	}
}

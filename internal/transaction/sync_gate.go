package transaction

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/khaliullov/family-finance/internal/database"
)

var (
	ErrSyncGateConnectionDisconnected = errors.New("bank connection is disconnected")
	ErrSyncGateCredentialsMissing     = errors.New("connection credentials missing: authentication required")
	ErrSyncGateAuthExpired            = errors.New("connection authentication has expired or requires reauthentication")
	ErrSyncGateValidationRequired     = errors.New("initial bank validation has not completed for this connection")
	ErrSyncGateValidationStale        = errors.New("validation report is stale for current session or credentials")
	ErrSyncGateValidationFailed       = errors.New("financial synchronization blocked: validation report contains blocking errors")
	ErrSyncGateProductionUnsupported  = errors.New("provider is strictly prohibited from production financial synchronization")
	ErrSyncGateInvalidState           = errors.New("connection lifecycle state does not permit synchronization")
)

// SyncGate coordinates fail-closed financial synchronization verification.
type SyncGate struct {
	pool *pgxpool.Pool
}

// NewSyncGate instantiates a Fail-Closed SyncGate.
func NewSyncGate(pool *pgxpool.Pool) *SyncGate {
	return &SyncGate{pool: pool}
}

// isProductionEnvironment checks if the application is executing in production mode.
func isProductionEnvironment() bool {
	env := strings.ToLower(strings.TrimSpace(os.Getenv("ENVIRONMENT")))
	if env == "production" || env == "prod" {
		return true
	}
	appEnv := strings.ToLower(strings.TrimSpace(os.Getenv("APP_ENV")))
	return appEnv == "production" || appEnv == "prod"
}

// VerifyCanSync executes the comprehensive fail-closed sync gate policy.
// It verifies connection status, credentials, session version matching, production provider eligibility,
// and confirms the validation report has zero Critical or Error severity failures.
func (g *SyncGate) VerifyCanSync(ctx context.Context, familyID, connectionID uuid.UUID) (*BankConnectionEntity, *ValidationReport, error) {
	if familyID == uuid.Nil || connectionID == uuid.Nil {
		return nil, nil, ErrMismatchedTenantContext
	}

	var conn BankConnectionEntity
	var report ValidationReport
	var reportPayload []byte

	err := database.WithFamilyTx(ctx, g.pool, familyID, func(tx pgx.Tx) error {
		// 1. Fetch connection row with row lock to prevent race with validation or reconnect
		var lastSync, lastAuth, lastVal *time.Time
		var lastValStatus, errMsg, credHash *string
		var lastValReportID *uuid.UUID

		err := tx.QueryRow(ctx, `
			SELECT id, family_id, authorized_by_user_id, provider, name, status,
			       encrypted_credentials, sync_cursor, last_synced_at, last_authenticated_at,
			       last_validated_at, last_validation_status, last_validation_report_id,
			       session_version, credential_hash, error_message, created_at, updated_at
			FROM public.bank_connections
			WHERE family_id = $1 AND id = $2
		`, familyID, connectionID).Scan(
			&conn.ID, &conn.FamilyID, &conn.AuthorizedByUserID, &conn.Provider, &conn.Name, &conn.Status,
			&conn.EncryptedCredentials, &conn.SyncCursor, &lastSync, &lastAuth,
			&lastVal, &lastValStatus, &lastValReportID,
			&conn.SessionVersion, &credHash, &errMsg, &conn.CreatedAt, &conn.UpdatedAt,
		)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrConnectionNotFound
			}
			return fmt.Errorf("failed to query bank connection: %w", err)
		}

		conn.LastSyncedAt = lastSync
		conn.LastAuthenticatedAt = lastAuth
		conn.LastValidatedAt = lastVal
		conn.LastValidationStatus = lastValStatus
		conn.LastValidationReportID = lastValReportID
		conn.CredentialHash = credHash
		conn.ErrorMessage = errMsg

		// 2. State verification
		if conn.Status == ConnectionStateDisconnected {
			return ErrSyncGateConnectionDisconnected
		}
		if conn.Status == ConnectionStateRequiresReauth || conn.Status == ConnectionStateExpired {
			return ErrSyncGateAuthExpired
		}
		if conn.Status == ConnectionStateCreated || conn.Status == ConnectionStateAuthenticating {
			return fmt.Errorf("%w: connection is in %q state", ErrSyncGateInvalidState, conn.Status)
		}
		if conn.Status == ConnectionStateValidating || conn.Status == ConnectionStateValidationPending {
			return fmt.Errorf("%w: validation is currently running (%q)", ErrSyncGateInvalidState, conn.Status)
		}
		if conn.Status == ConnectionStateValidationFailed {
			return fmt.Errorf("%w: connection status is validation_failed", ErrSyncGateValidationFailed)
		}

		// 3. Credentials presence
		if len(conn.EncryptedCredentials) == 0 {
			return ErrSyncGateCredentialsMissing
		}

		// 4. Production boundary enforcement
		if (conn.Provider == "fixture" || conn.Provider == "mock") && isProductionEnvironment() {
			return fmt.Errorf("%w: provider %q cannot be synchronized in production mode", ErrSyncGateProductionUnsupported, conn.Provider)
		}

		// 5. Validation completion prerequisites
		if conn.LastValidatedAt == nil || conn.LastValidationReportID == nil || conn.LastValidationStatus == nil {
			return ErrSyncGateValidationRequired
		}
		if *conn.LastValidationStatus == string(ValidationResultFailed) {
			return fmt.Errorf("%w: last validation status was failed", ErrSyncGateValidationFailed)
		}

		// 6. Fetch validation report from bank_validation_reports
		var repSessionVer int
		var repCredHash *string
		var overallResult string
		var repStartedAt, repFinishedAt time.Time
		var repDurationMs int64

		err = tx.QueryRow(ctx, `
			SELECT id, session_version, credential_hash, overall_result, started_at, finished_at, duration_ms, report_payload
			FROM public.bank_validation_reports
			WHERE family_id = $1 AND id = $2 AND bank_connection_id = $3
		`, familyID, *conn.LastValidationReportID, connectionID).Scan(
			&report.ID, &repSessionVer, &repCredHash, &overallResult,
			&repStartedAt, &repFinishedAt, &repDurationMs, &reportPayload,
		)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w: validation report record not found", ErrSyncGateValidationRequired)
			}
			return fmt.Errorf("failed to query validation report: %w", err)
		}

		report.SessionVersion = repSessionVer
		if repCredHash != nil {
			report.CredentialHash = *repCredHash
		}
		report.OverallResult = ValidationOverallResult(overallResult)
		report.StartedAt = repStartedAt
		report.FinishedAt = repFinishedAt
		report.DurationMs = repDurationMs

		// 7. Validate report freshness vs current session version and credential hash
		if report.SessionVersion != conn.SessionVersion {
			return fmt.Errorf("%w: report session version (%d) does not match connection current session version (%d)",
				ErrSyncGateValidationStale, report.SessionVersion, conn.SessionVersion)
		}
		if conn.CredentialHash != nil && *conn.CredentialHash != "" && report.CredentialHash != "" {
			if *conn.CredentialHash != report.CredentialHash {
				return fmt.Errorf("%w: report credential hash does not match current connection credentials",
					ErrSyncGateValidationStale)
			}
		}

		// 8. Deserialize full structured report payload for severity analysis
		if err := json.Unmarshal(reportPayload, &report); err != nil {
			return fmt.Errorf("%w: failed to parse validation report payload: %v", ErrSyncGateValidationFailed, err)
		}

		// 9. Failure severity policy
		if report.OverallResult == ValidationResultFailed {
			return fmt.Errorf("%w: validation overall result is failed", ErrSyncGateValidationFailed)
		}

		// Inspect all recorded errors
		for _, e := range report.Errors {
			if e.Severity == SeverityCritical {
				return fmt.Errorf("%w: critical failure [%s]: %s", ErrSyncGateValidationFailed, e.Code, e.Message)
			}
			if e.Severity == SeverityError {
				return fmt.Errorf("%w: blocking error [%s]: %s", ErrSyncGateValidationFailed, e.Code, e.Message)
			}
		}

		// Inspect data integrity violations
		if report.DataIntegrity.DuplicateExternalIDs > 0 {
			return fmt.Errorf("%w: duplicate external IDs (%d) discovered during validation",
				ErrSyncGateValidationFailed, report.DataIntegrity.DuplicateExternalIDs)
		}
		if report.DataIntegrity.OrphanTransactions > 0 {
			return fmt.Errorf("%w: orphan transactions (%d) discovered during validation",
				ErrSyncGateValidationFailed, report.DataIntegrity.OrphanTransactions)
		}
		if report.DataIntegrity.AccountMappingErrors > 0 {
			return fmt.Errorf("%w: account mapping errors (%d) discovered during validation",
				ErrSyncGateValidationFailed, report.DataIntegrity.AccountMappingErrors)
		}

		// Inspect transaction formatting violations
		if report.Transactions.InvalidAmounts > 0 {
			return fmt.Errorf("%w: transactions contain invalid or missing monetary amounts (%d)",
				ErrSyncGateValidationFailed, report.Transactions.InvalidAmounts)
		}
		if report.Transactions.MissingRequiredIdentifiers > 0 {
			return fmt.Errorf("%w: transactions missing required external identifiers (%d)",
				ErrSyncGateValidationFailed, report.Transactions.MissingRequiredIdentifiers)
		}
		if len(report.Transactions.UnsupportedStatuses) > 0 {
			return fmt.Errorf("%w: unsupported transaction booking statuses discovered (%v)",
				ErrSyncGateValidationFailed, report.Transactions.UnsupportedStatuses)
		}
		if len(report.Transactions.UnsupportedTypes) > 0 {
			return fmt.Errorf("%w: unsupported transaction operation types discovered (%v)",
				ErrSyncGateValidationFailed, report.Transactions.UnsupportedTypes)
		}

		return nil
	})

	if err != nil {
		return nil, nil, err
	}

	return &conn, &report, nil
}

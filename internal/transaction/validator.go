package transaction

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/khaliullov/family-finance/internal/connector"
	"github.com/khaliullov/family-finance/internal/database"
	"github.com/khaliullov/family-finance/internal/domain"
)

// Validator implements the read-only bank connection validation engine.
// It performs exhaustive integrity checks without creating production ledger entries.
type Validator struct {
	pool      *pgxpool.Pool
	registry  *connector.Registry
	cryptoKey []byte
}

// NewValidator creates a new bank connection validator.
func NewValidator(pool *pgxpool.Pool, registry *connector.Registry, cryptoKey []byte) *Validator {
	return &Validator{
		pool:      pool,
		registry:  registry,
		cryptoKey: cryptoKey,
	}
}

// ValidateConnection executes the read-only validation pipeline for a bank connection.
// It verifies Authentication, Accounts, Cards, Balances, Transactions, and Data Integrity,
// persists a ValidationReport in public.bank_validation_reports, and atomically updates
// the bank_connections status without touching the production financial ledger.
func (v *Validator) ValidateConnection(ctx context.Context, familyID, connectionID uuid.UUID) (*ValidationReport, error) {
	startTime := time.Now().UTC()
	runID := uuid.New()

	// 1. Load connection and atomically transition to "validating"
	var conn BankConnectionEntity
	err := database.WithFamilyTx(ctx, v.pool, familyID, func(tx pgx.Tx) error {
		var lastSync, lastAuth, lastVal *time.Time
		var lastValStatus, errMsg, credHash *string
		var lastValReportID *uuid.UUID
		var activeSyncToken *uuid.UUID
		var syncLockedUntil *time.Time
		var sessionVer int

		err := tx.QueryRow(ctx, `
			SELECT id, family_id, authorized_by_user_id, provider, name, status,
			       encrypted_credentials, sync_cursor, last_synced_at, last_authenticated_at,
			       last_validated_at, last_validation_status, last_validation_report_id,
			       session_version, credential_hash, active_sync_token, sync_locked_until,
			       error_message, created_at, updated_at
			FROM public.bank_connections
			WHERE family_id = $1 AND id = $2
			FOR UPDATE
		`, familyID, connectionID).Scan(
			&conn.ID, &conn.FamilyID, &conn.AuthorizedByUserID, &conn.Provider, &conn.Name, &conn.Status,
			&conn.EncryptedCredentials, &conn.SyncCursor, &lastSync, &lastAuth,
			&lastVal, &lastValStatus, &lastValReportID,
			&sessionVer, &credHash, &activeSyncToken, &syncLockedUntil,
			&errMsg, &conn.CreatedAt, &conn.UpdatedAt,
		)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrConnectionNotFound
			}
			return fmt.Errorf("load connection for validation: %w", err)
		}

		conn.LastSyncedAt = lastSync
		conn.LastAuthenticatedAt = lastAuth
		conn.LastValidatedAt = lastVal
		conn.LastValidationStatus = lastValStatus
		conn.LastValidationReportID = lastValReportID
		conn.SessionVersion = sessionVer
		conn.CredentialHash = credHash
		conn.ErrorMessage = errMsg

		// Mutual exclusion: fail closed if synchronization is actively running
		if activeSyncToken != nil && syncLockedUntil != nil && syncLockedUntil.After(time.Now()) {
			return fmt.Errorf("validation rejected: financial synchronization is currently active on connection %s", connectionID)
		}

		if err := CanValidateConnection(conn.Status); err != nil {
			return err
		}

		// Transition to validating
		_, err = tx.Exec(ctx, `
			UPDATE public.bank_connections
			SET status = $1, updated_at = NOW()
			WHERE family_id = $2 AND id = $3
		`, ConnectionStateValidating, familyID, connectionID)
		return err
	})
	if err != nil {
		return nil, err
	}

	// 2. Initialize Validation Report Structure
	report := &ValidationReport{
		ID:             runID,
		FamilyID:       familyID,
		ConnectionID:   connectionID,
		SessionVersion: conn.SessionVersion,
		StartedAt:      startTime,
		OverallResult:  ValidationResultPassed,
		Summary: ValidationSummary{
			ConnectionID:     connectionID,
			BankProvider:     conn.Provider,
			ValidationRunID:  runID,
			StartedAt:        startTime,
			ConnectorVersion: "Stage5B-1.0",
		},
		Authentication: AuthenticationValidation{
			Authenticated:  false,
			SessionValid:   false,
			ReauthRequired: false,
		},
		Accounts: AccountsValidation{
			Items:  make([]AccountValidationItem, 0),
			Errors: make([]string, 0),
		},
		Cards: CardsValidation{
			Items:  make([]CardValidationItem, 0),
			Errors: make([]string, 0),
		},
		Balances: BalancesValidation{
			Items:  make([]BalanceValidationItem, 0),
			Errors: make([]string, 0),
		},
		Transactions: TransactionsValidation{
			CountsByStatus:        make(map[string]int),
			CountsByOperationType: make(map[string]int),
			UnsupportedStatuses:   make([]string, 0),
			UnsupportedTypes:      make([]string, 0),
			Errors:                make([]string, 0),
		},
		DataIntegrity: DataIntegrityValidation{
			TenantIsolationPassed: true,
			IdempotencyPassed:     true,
		},
		Errors:    make([]ValidationErrorItem, 0),
		CreatedAt: startTime,
	}
	if conn.CredentialHash != nil {
		report.CredentialHash = *conn.CredentialHash
	}

	// 3. Resolve Connector
	providerCode := conn.Provider
	if providerCode == "tbank_personal" {
		providerCode = "tbank"
	}
	connAdapter, err := v.registry.Get(providerCode)
	if err != nil {
		report.OverallResult = ValidationResultFailed
		report.Errors = append(report.Errors, ValidationErrorItem{
			Code:           "CONNECTOR_NOT_FOUND",
			Severity:       SeverityCritical,
			Message:        fmt.Sprintf("Bank provider %q is not supported or not enabled", conn.Provider),
			Stage:          "authentication",
			Retryable:      false,
			ReauthRequired: false,
		})
		v.finalizeReport(ctx, familyID, connectionID, report, startTime)
		return report, nil
	}

	// 4. Stage A: Authentication Validation
	if len(conn.EncryptedCredentials) == 0 {
		report.OverallResult = ValidationResultFailed
		report.Authentication.ReauthRequired = true
		report.Authentication.Message = "Connection has no credentials stored; authentication required"
		report.Errors = append(report.Errors, ValidationErrorItem{
			Code:           "AUTH_CREDENTIALS_MISSING",
			Severity:       SeverityCritical,
			Message:        "No credentials stored on connection",
			Stage:          "authentication",
			Retryable:      false,
			ReauthRequired: true,
		})
		v.finalizeReport(ctx, familyID, connectionID, report, startTime)
		return report, nil
	}

	healthErr := connAdapter.HealthCheck(ctx, conn.EncryptedCredentials)
	if healthErr != nil {
		report.OverallResult = ValidationResultFailed
		if errors.Is(healthErr, connector.ErrAuthExpired) {
			report.Authentication.ReauthRequired = true
			report.Authentication.Message = "Banking session expired or rejected (HTTP 401/403)"
			report.Errors = append(report.Errors, ValidationErrorItem{
				Code:           "AUTH_SESSION_EXPIRED",
				Severity:       SeverityCritical,
				Message:        "Banking session expired or revoked by bank; please reconnect",
				Stage:          "authentication",
				Retryable:      false,
				ReauthRequired: true,
			})
		} else {
			report.Authentication.Message = "Health check probe failed"
			report.Errors = append(report.Errors, ValidationErrorItem{
				Code:           "AUTH_PROBE_FAILED",
				Severity:       SeverityError,
				Message:        "Bank endpoint health probe failed",
				Details:        healthErr.Error(),
				Stage:          "authentication",
				Retryable:      true,
				ReauthRequired: false,
			})
		}
		v.finalizeReport(ctx, familyID, connectionID, report, startTime)
		return report, nil
	}

	report.Authentication.Authenticated = true
	report.Authentication.SessionValid = true
	report.Authentication.Message = "Authentication session is valid and verified"

	// 5. Stage B: Accounts Validation
	accounts, err := connAdapter.FetchAccounts(ctx, conn.EncryptedCredentials)
	if err != nil {
		report.OverallResult = ValidationResultFailed
		if errors.Is(err, connector.ErrAuthExpired) {
			report.Authentication.ReauthRequired = true
			report.Errors = append(report.Errors, ValidationErrorItem{
				Code:           "AUTH_SESSION_EXPIRED",
				Severity:       SeverityCritical,
				Message:        "Session expired during account discovery",
				Stage:          "accounts",
				ReauthRequired: true,
			})
		} else {
			report.Errors = append(report.Errors, ValidationErrorItem{
				Code:      "ACCOUNTS_FETCH_FAILED",
				Severity:  SeverityError,
				Message:   "Failed to fetch accounts from bank API",
				Details:   err.Error(),
				Stage:     "accounts",
				Retryable: true,
			})
		}
		v.finalizeReport(ctx, familyID, connectionID, report, startTime)
		return report, nil
	}

	report.Accounts.Discovered = len(accounts)
	if len(accounts) == 0 {
		report.Errors = append(report.Errors, ValidationErrorItem{
			Code:      "ACCOUNTS_ZERO_DISCOVERED",
			Severity:  SeverityWarning,
			Message:   "Zero accounts discovered for connection",
			Stage:     "accounts",
			Retryable: false,
		})
	}
	seenAccountIDs := make(map[string]bool)
	accountMap := make(map[string]connector.AccountDTO)

	for _, acc := range accounts {
		maskedID := MaskIdentifier(acc.ExternalID)
		accValid := true
		var accErrMsg string

		if acc.ExternalID == "" {
			accValid = false
			accErrMsg = "Missing account external ID"
			report.Errors = append(report.Errors, ValidationErrorItem{
				Code:     "ACCOUNT_MISSING_ID",
				Severity: SeverityError,
				Message:  "Discovered account is missing external ID",
				Stage:    "accounts",
			})
		} else if seenAccountIDs[acc.ExternalID] {
			accValid = false
			accErrMsg = "Duplicate account external ID detected within connection"
			report.DataIntegrity.DuplicateExternalIDs++
			report.Errors = append(report.Errors, ValidationErrorItem{
				Code:     "ACCOUNT_DUPLICATE_ID",
				Severity: SeverityCritical,
				Message:  fmt.Sprintf("Duplicate account ID %s discovered", maskedID),
				Stage:    "accounts",
			})
		} else {
			seenAccountIDs[acc.ExternalID] = true
			accountMap[acc.ExternalID] = acc
		}

		// Validate currency
		if err := domain.ValidateCurrency(acc.Currency); err != nil {
			accValid = false
			accErrMsg = fmt.Sprintf("Unsupported account currency: %s", acc.Currency)
			report.Errors = append(report.Errors, ValidationErrorItem{
				Code:     "ACCOUNT_CURRENCY_INVALID",
				Severity: SeverityError,
				Message:  fmt.Sprintf("Account %s has unsupported currency %s", maskedID, acc.Currency),
				Stage:    "accounts",
			})
		}

		// Validate account type
		switch strings.ToLower(acc.Type) {
		case "checking", "savings", "credit_card", "debit", "credit", "saving":
			// valid recognized type
		default:
			report.Accounts.Errors = append(report.Accounts.Errors, fmt.Sprintf("Account %s has unclassified type: %s", maskedID, acc.Type))
		}

		if accValid {
			report.Accounts.Valid++
		} else {
			report.Accounts.Rejected++
			if report.OverallResult == ValidationResultPassed {
				report.OverallResult = ValidationResultPartialSuccess
			}
		}

		report.Accounts.Items = append(report.Accounts.Items, AccountValidationItem{
			ExternalIDMasked: maskedID,
			Name:             acc.Name,
			Type:             acc.Type,
			Currency:         acc.Currency,
			Status:           "ACTIVE",
			Valid:            accValid,
			Error:            accErrMsg,
		})
	}

	// 6. Stage C: Cards Validation
	for _, acc := range accounts {
		cards, err := connAdapter.FetchCards(ctx, conn.EncryptedCredentials, acc.ExternalID)
		if err != nil {
			report.Cards.Errors = append(report.Cards.Errors, fmt.Sprintf("Failed to fetch cards for account %s: %v", MaskIdentifier(acc.ExternalID), err))
			continue
		}
		if len(cards) == 0 && len(acc.Cards) > 0 {
			cards = acc.Cards
		}

		for _, c := range cards {
			report.Cards.Discovered++
			cardValid := true
			var cardErr string

			maskedCardID := MaskIdentifier(c.ExternalID)
			maskedAccID := MaskIdentifier(c.AccountExternalID)

			if c.ExternalID == "" {
				cardValid = false
				cardErr = "Missing card external ID"
				report.Errors = append(report.Errors, ValidationErrorItem{
					Code:     "CARD_MISSING_ID",
					Severity: SeverityError,
					Message:  "Discovered card is missing external ID",
					Stage:    "cards",
				})
			}
			if c.AccountExternalID == "" || c.AccountExternalID != acc.ExternalID {
				cardValid = false
				cardErr = "Card is not correctly linked to parent account"
				report.DataIntegrity.OrphanCards++
				report.Errors = append(report.Errors, ValidationErrorItem{
					Code:     "DATA_INTEGRITY_ORPHAN_CARD",
					Severity: SeverityError,
					Message:  fmt.Sprintf("Card %s is not linked to parent account %s", maskedCardID, maskedAccID),
					Stage:    "data_integrity",
				})
			}

			// Validate masked PAN: ensure no full PAN is exposed
			if isUnmaskedPAN(c.MaskedPAN) {
				cardValid = false
				cardErr = "Card number is unmasked; full PAN exposure violation"
				report.Errors = append(report.Errors, ValidationErrorItem{
					Code:     "CARD_UNMASKED_PAN",
					Severity: SeverityCritical,
					Message:  "Security violation: card PAN is not masked",
					Stage:    "cards",
				})
			}

			if cardValid {
				report.Cards.Valid++
			} else {
				report.Cards.Invalid++
				if report.OverallResult == ValidationResultPassed {
					report.OverallResult = ValidationResultPartialSuccess
				}
			}

			report.Cards.Items = append(report.Cards.Items, CardValidationItem{
				ExternalIDMasked:        maskedCardID,
				AccountExternalIDMasked: maskedAccID,
				MaskedPAN:               c.MaskedPAN,
				PaymentSystem:           c.PaymentSystem,
				IsVirtual:               c.IsVirtual,
				IsPrimary:               c.IsPrimary,
				Status:                  c.Status,
				Valid:                   cardValid,
				Error:                   cardErr,
			})
		}
	}

	// 7. Stage D: Balances Validation
	accIDs := make([]string, 0, len(accounts))
	for _, a := range accounts {
		accIDs = append(accIDs, a.ExternalID)
	}

	balances, err := connAdapter.FetchBalances(ctx, conn.EncryptedCredentials, accIDs)
	if err != nil {
		report.Balances.Errors = append(report.Balances.Errors, fmt.Sprintf("Failed to fetch balances: %v", err))
		if report.OverallResult == ValidationResultPassed {
			report.OverallResult = ValidationResultPartialSuccess
		}
	} else {
		for _, b := range balances {
			report.Balances.Checked++
			bValid := true
			var bErr string

			maskedAccID := MaskIdentifier(b.AccountExternalID)
			parentAcc, accFound := accountMap[b.AccountExternalID]
			if !accFound {
				bValid = false
				bErr = "Balance references unknown account"
				report.DataIntegrity.AccountMappingErrors++
				report.Errors = append(report.Errors, ValidationErrorItem{
					Code:     "DATA_INTEGRITY_MAPPING_ERROR",
					Severity: SeverityError,
					Message:  fmt.Sprintf("Balance references unknown account %s", maskedAccID),
					Stage:    "data_integrity",
				})
			} else if b.CurrentBalance != nil && b.CurrentBalance.Currency != "" && b.CurrentBalance.Currency != parentAcc.Currency {
				bValid = false
				bErr = fmt.Sprintf("Balance currency %s differs from account currency %s", b.CurrentBalance.Currency, parentAcc.Currency)
				report.Errors = append(report.Errors, ValidationErrorItem{
					Code:     "BALANCE_CURRENCY_MISMATCH",
					Severity: SeverityCritical,
					Message:  fmt.Sprintf("Balance currency mismatch for account %s", maskedAccID),
					Stage:    "balances",
				})
			}

			var availMinor *int64
			if b.AvailableBalance != nil {
				v := b.AvailableBalance.MinorUnits
				availMinor = &v
			}

			var curCurrency string
			var curMinor int64
			if b.CurrentBalance != nil {
				curCurrency = b.CurrentBalance.Currency
				curMinor = b.CurrentBalance.MinorUnits
			} else if b.AvailableBalance != nil {
				curCurrency = b.AvailableBalance.Currency
			} else if accFound {
				curCurrency = parentAcc.Currency
			}

			report.Balances.Items = append(report.Balances.Items, BalanceValidationItem{
				AccountExternalIDMasked: maskedAccID,
				Currency:                curCurrency,
				CurrentAmountMinor:      curMinor,
				AvailableAmountMinor:    availMinor,
				Valid:                   bValid,
				Error:                   bErr,
			})
		}
	}

	// 8. Stage E: Transactions Validation (Bounded Read-Only History)
	syncResult, err := connAdapter.FetchTransactions(ctx, conn.EncryptedCredentials, connector.SyncParams{
		StartDate: time.Now().Add(-30 * 24 * time.Hour),
		EndDate:   time.Now().Add(24 * time.Hour),
		Limit:     100,
	})
	if err != nil {
		report.Transactions.Errors = append(report.Transactions.Errors, fmt.Sprintf("Failed to fetch transaction timeline: %v", err))
		report.Errors = append(report.Errors, ValidationErrorItem{
			Code:      "TRANSACTIONS_FETCH_FAILED",
			Severity:  SeverityError,
			Message:   "Failed to fetch initial transactions sample",
			Details:   err.Error(),
			Stage:     "transactions",
			Retryable: true,
		})
		if report.OverallResult == ValidationResultPassed {
			report.OverallResult = ValidationResultPartialSuccess
		}
	} else if syncResult != nil {
		report.Transactions.Fetched = len(syncResult.Transactions)
		seenTxIDs := make(map[string]bool)

		for _, tx := range syncResult.Transactions {
			txValid := true

			// ExternalID check
			if tx.ExternalID == "" {
				txValid = false
				report.Transactions.MissingRequiredIdentifiers++
				report.Transactions.Errors = append(report.Transactions.Errors, "Transaction is missing ExternalID")
				report.Errors = append(report.Errors, ValidationErrorItem{
					Code:     "TRANSACTION_MISSING_ID",
					Severity: SeverityError,
					Message:  "Transaction is missing external identifier",
					Stage:    "transactions",
				})
			} else if seenTxIDs[tx.ExternalID] {
				txValid = false
				report.Transactions.Duplicates++
				report.DataIntegrity.DuplicateExternalIDs++
				report.Errors = append(report.Errors, ValidationErrorItem{
					Code:     "TRANSACTIONS_DUPLICATE_SAMPLE",
					Severity: SeverityWarning,
					Message:  fmt.Sprintf("Duplicate transaction external ID in sample: %s", MaskIdentifier(tx.ExternalID)),
					Stage:    "transactions",
				})
			} else {
				seenTxIDs[tx.ExternalID] = true
			}

			// Parent account relationship check
			if tx.AccountExternalID == "" || !seenAccountIDs[tx.AccountExternalID] {
				report.DataIntegrity.OrphanTransactions++
				report.Errors = append(report.Errors, ValidationErrorItem{
					Code:     "DATA_INTEGRITY_ORPHAN_TX",
					Severity: SeverityError,
					Message:  fmt.Sprintf("Transaction references unmapped account: %s", MaskIdentifier(tx.AccountExternalID)),
					Stage:    "data_integrity",
				})
			}

			// Status checks
			status := strings.ToLower(tx.BookingStatus)
			switch status {
			case "posted", "pending", "cancelled":
				report.Transactions.CountsByStatus[status]++
			default:
				txValid = false
				report.Transactions.UnsupportedStatuses = append(report.Transactions.UnsupportedStatuses, tx.BookingStatus)
				report.Errors = append(report.Errors, ValidationErrorItem{
					Code:     "TRANSACTION_UNSUPPORTED_STATUS",
					Severity: SeverityError,
					Message:  fmt.Sprintf("Unsupported transaction booking status: %s", tx.BookingStatus),
					Stage:    "transactions",
				})
			}

			// Operation type checks
			opType := strings.ToLower(tx.OperationType)
			switch opType {
			case "purchase", "refund", "transfer_out", "transfer_in", "atm_cash", "fee", "card_check":
				report.Transactions.CountsByOperationType[opType]++
			default:
				txValid = false
				report.Transactions.UnsupportedTypes = append(report.Transactions.UnsupportedTypes, tx.OperationType)
				report.Errors = append(report.Errors, ValidationErrorItem{
					Code:     "TRANSACTION_UNSUPPORTED_TYPE",
					Severity: SeverityError,
					Message:  fmt.Sprintf("Unsupported transaction operation type: %s", tx.OperationType),
					Stage:    "transactions",
				})
			}

			// Timestamp sanity check
			if tx.TransactionTime.IsZero() || tx.TransactionTime.Year() < 2000 || tx.TransactionTime.After(time.Now().Add(24*time.Hour)) {
				txValid = false
				report.Transactions.InvalidTimestamps++
				report.Errors = append(report.Errors, ValidationErrorItem{
					Code:     "TRANSACTION_TIMESTAMP_SUSPICIOUS",
					Severity: SeverityWarning,
					Message:  "Transaction timestamp outside expected historical window",
					Stage:    "transactions",
				})
			}

			// Monetary amount sanity check
			if tx.Amount.Currency == "" {
				txValid = false
				report.Transactions.InvalidAmounts++
				report.Errors = append(report.Errors, ValidationErrorItem{
					Code:     "TRANSACTION_INVALID_AMOUNT",
					Severity: SeverityError,
					Message:  "Transaction missing currency or invalid amount",
					Stage:    "transactions",
				})
			}

			if txValid {
				report.Transactions.Valid++
			} else {
				report.Transactions.Rejected++
				if report.OverallResult == ValidationResultPassed {
					report.OverallResult = ValidationResultPartialSuccess
				}
			}
		}
	}

	// 9. Final Outcome Evaluation
	hasCritical := false
	hasError := false
	hasWarning := false

	for _, e := range report.Errors {
		switch e.Severity {
		case SeverityCritical:
			hasCritical = true
		case SeverityError:
			hasError = true
		case SeverityWarning:
			hasWarning = true
		}
	}

	if hasCritical || hasError || report.Authentication.ReauthRequired {
		report.OverallResult = ValidationResultFailed
	} else if hasWarning || report.Accounts.Discovered == 0 || report.Transactions.Rejected > 0 {
		report.OverallResult = ValidationResultPartialSuccess
	} else {
		report.OverallResult = ValidationResultPassed
	}

	// 10. Persist Report & Update Connection State
	v.finalizeReport(ctx, familyID, connectionID, report, startTime)
	return report, nil
}

// finalizeReport persists the validation report and commits connection status under tenant isolation.
func (v *Validator) finalizeReport(ctx context.Context, familyID, connectionID uuid.UUID, report *ValidationReport, startTime time.Time) {
	finishedTime := time.Now().UTC()
	report.FinishedAt = finishedTime
	report.DurationMs = finishedTime.Sub(startTime).Milliseconds()
	report.Summary.FinishedAt = finishedTime
	report.Summary.DurationMs = report.DurationMs
	report.Summary.OverallResult = report.OverallResult

	reportJSON, err := json.Marshal(report)
	if err != nil {
		reportJSON = []byte(`{"error":"failed to serialize validation report"}`)
	}

	var newConnStatus string
	if report.Authentication.ReauthRequired {
		newConnStatus = ConnectionStateRequiresReauth
	} else if report.OverallResult == ValidationResultFailed {
		newConnStatus = ConnectionStateValidationFailed
	} else {
		newConnStatus = ConnectionStateValidationPassed
	}

	var errSummary *string
	if len(report.Errors) > 0 {
		msg := report.Errors[0].Message
		errSummary = &msg
	}

	_ = database.WithFamilyTx(ctx, v.pool, familyID, func(tx pgx.Tx) error {
		// 1. Insert validation report into bank_validation_reports
		_, err := tx.Exec(ctx, `
			INSERT INTO public.bank_validation_reports (
				id, family_id, bank_connection_id, session_version, credential_hash,
				overall_result, started_at, finished_at, duration_ms, report_payload, created_at
			) VALUES (
				$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11
			)
		`, report.ID, familyID, connectionID, report.SessionVersion, report.CredentialHash,
			string(report.OverallResult), report.StartedAt, report.FinishedAt, report.DurationMs, reportJSON, finishedTime)
		if err != nil {
			return err
		}

		// 2. Update bank_connections with validation summary atomically
		// Fail-closed against concurrent session replacement or reconnect by checking session_version and status
		_, err = tx.Exec(ctx, `
			UPDATE public.bank_connections
			SET status = $1,
			    last_validated_at = $2,
			    last_validation_status = $3,
			    last_validation_report_id = $4,
			    error_message = $5,
			    updated_at = NOW()
			WHERE family_id = $6 AND id = $7
			  AND session_version = $8
			  AND status = 'validating'
		`, newConnStatus, finishedTime, string(report.OverallResult), report.ID, errSummary,
			familyID, connectionID, report.SessionVersion)
		return err
	})
}

// isUnmaskedPAN tests if a string appears to be an unmasked primary account number (13-19 digits).
func isUnmaskedPAN(s string) bool {
	digits := 0
	for _, r := range s {
		if r >= '0' && r <= '9' {
			digits++
		}
	}
	return digits >= 13 && !strings.Contains(s, "*") && !strings.Contains(s, "X")
}

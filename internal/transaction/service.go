package transaction

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/khaliullov/family-finance/internal/connector"
	"github.com/khaliullov/family-finance/internal/database"
	"github.com/khaliullov/family-finance/internal/queue"
)

// Service coordinates bank synchronization and transaction ingestion.
type Service struct {
	pool     *pgxpool.Pool
	registry *connector.Registry
}

// NewService creates a new Ingestion Service.
func NewService(pool *pgxpool.Pool, registry *connector.Registry) *Service {
	return &Service{
		pool:     pool,
		registry: registry,
	}
}

// SyncConnection executes an end-to-end synchronization for a specific bank connection.
func (s *Service) SyncConnection(ctx context.Context, familyID, connectionID uuid.UUID, opts SyncOptions) (*SyncStats, error) {
	if familyID == uuid.Nil || connectionID == uuid.Nil {
		return nil, ErrMismatchedTenantContext
	}

	totalStats := &SyncStats{}

	// 1. Fetch connection details inside tenant context
	var conn BankConnectionEntity
	err := database.WithFamilyTx(ctx, s.pool, familyID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT id, family_id, authorized_by_user_id, provider, name,
			       status, encrypted_credentials, last_synced_at, error_message,
			       created_at, updated_at
			FROM public.bank_connections
			WHERE family_id = $1 AND id = $2
		`, familyID, connectionID)

		return row.Scan(
			&conn.ID, &conn.FamilyID, &conn.AuthorizedByUserID, &conn.Provider, &conn.Name,
			&conn.Status, &conn.EncryptedCredentials, &conn.LastSyncedAt, &conn.ErrorMessage,
			&conn.CreatedAt, &conn.UpdatedAt,
		)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrConnectionNotFound
		}
		return nil, fmt.Errorf("failed to load bank connection: %w", err)
	}

	if conn.Status == "disconnected" {
		return nil, fmt.Errorf("%w: connection is disconnected", ErrConnectionNotActive)
	}

	// 2. Resolve Connector
	bankConn, err := s.registry.Get(conn.Provider)
	if err != nil {
		s.recordConnectionFailure(ctx, familyID, connectionID, err)
		return nil, fmt.Errorf("unsupported provider %q: %w", conn.Provider, err)
	}

	// 3. Discover and upsert accounts and cards
	accountMap := make(map[string]uuid.UUID)
	cardMap := make(map[string]uuid.UUID)

	accountDTOs, err := bankConn.FetchAccounts(ctx, conn.EncryptedCredentials)
	if err != nil {
		s.recordConnectionFailure(ctx, familyID, connectionID, err)
		return nil, fmt.Errorf("failed to fetch accounts from provider: %w", err)
	}

	err = database.WithFamilyTx(ctx, s.pool, familyID, func(tx pgx.Tx) error {
		for _, accDTO := range accountDTOs {
			var currBal, debtBal, availBal, credLim, stmtBal, minPay *int64
			if accDTO.Balance != nil {
				currBal = &accDTO.Balance.MinorUnits
			}
			if accDTO.DebtAmount != nil {
				debtBal = &accDTO.DebtAmount.MinorUnits
			}
			if accDTO.AvailableBalance != nil {
				availBal = &accDTO.AvailableBalance.MinorUnits
			}
			if accDTO.CreditLimit != nil {
				credLim = &accDTO.CreditLimit.MinorUnits
			}
			if accDTO.StatementBalance != nil {
				stmtBal = &accDTO.StatementBalance.MinorUnits
			}
			if accDTO.MinimalPayment != nil {
				minPay = &accDTO.MinimalPayment.MinorUnits
			}

			accType := accDTO.Type
			if accType == "" {
				accType = "checking"
			}
			accCur := accDTO.Currency
			if accCur == "" {
				accCur = "RUB"
			}

			var accID uuid.UUID
			var isInserted bool
			newAccID := uuid.New()

			err := tx.QueryRow(ctx, `
				INSERT INTO public.accounts (
					id, family_id, bank_connection_id, owner_user_id,
					external_id, name, type, currency,
					current_balance_minor, debt_balance_minor, available_balance_minor,
					credit_limit_minor, statement_balance_minor, minimal_payment_minor,
					balance_updated_at, balance_source
				) VALUES (
					$1, $2, $3, $4,
					$5, $6, $7, $8,
					$9, $10, $11,
					$12, $13, $14,
					NOW(), 'bank_api'
				)
				ON CONFLICT (family_id, bank_connection_id, external_id)
				DO UPDATE SET
					current_balance_minor = COALESCE(EXCLUDED.current_balance_minor, accounts.current_balance_minor),
					debt_balance_minor = COALESCE(EXCLUDED.debt_balance_minor, accounts.debt_balance_minor),
					available_balance_minor = COALESCE(EXCLUDED.available_balance_minor, accounts.available_balance_minor),
					credit_limit_minor = COALESCE(EXCLUDED.credit_limit_minor, accounts.credit_limit_minor),
					statement_balance_minor = COALESCE(EXCLUDED.statement_balance_minor, accounts.statement_balance_minor),
					minimal_payment_minor = COALESCE(EXCLUDED.minimal_payment_minor, accounts.minimal_payment_minor),
					balance_updated_at = NOW(),
					balance_source = 'bank_api',
					updated_at = NOW()
				RETURNING id, (xmax = 0) AS is_inserted
			`,
				newAccID, familyID, connectionID, conn.AuthorizedByUserID,
				accDTO.ExternalID, accDTO.Name, accType, accCur,
				currBal, debtBal, availBal,
				credLim, stmtBal, minPay,
			).Scan(&accID, &isInserted)
			if err != nil {
				return fmt.Errorf("failed to upsert account %s: %w", accDTO.ExternalID, err)
			}

			if isInserted {
				totalStats.AccountsDiscovered++
			}
			accountMap[accDTO.ExternalID] = accID

			// Upsert Cards for this account
			for _, cardDTO := range accDTO.Cards {
				var cardID uuid.UUID
				var isCardInserted bool
				paySys := cardDTO.PaymentSystem
				if paySys == "" {
					paySys = "OTHER"
				}
				status := cardDTO.Status
				if status == "" {
					status = "ACTIVE"
				}
				pan := cardDTO.MaskedPAN
				if pan == "" {
					pan = "*0000"
				}

				newCardID := uuid.New()
				err := tx.QueryRow(ctx, `
					INSERT INTO public.cards (
						id, family_id, account_id, owner_user_id,
						external_id, masked_pan, display_name, payment_system,
						is_virtual, is_primary, status
					) VALUES (
						$1, $2, $3, $4,
						$5, $6, $7, $8,
						$9, $10, $11
					)
					ON CONFLICT (family_id, account_id, external_id)
					DO UPDATE SET
						display_name = EXCLUDED.display_name,
						status = EXCLUDED.status,
						is_primary = EXCLUDED.is_primary,
						updated_at = NOW()
					RETURNING id, (xmax = 0) AS is_inserted
				`,
					newCardID, familyID, accID, conn.AuthorizedByUserID,
					cardDTO.ExternalID, pan, cardDTO.DisplayName, paySys,
					cardDTO.IsVirtual, cardDTO.IsPrimary, status,
				).Scan(&cardID, &isCardInserted)
				if err != nil {
					return fmt.Errorf("failed to upsert card %s: %w", cardDTO.ExternalID, err)
				}

				if isCardInserted {
					totalStats.CardsDiscovered++
				}
				cardMap[cardDTO.ExternalID] = cardID
			}
		}
		return nil
	})
	if err != nil {
		s.recordConnectionFailure(ctx, familyID, connectionID, err)
		return nil, fmt.Errorf("failed to synchronize accounts and cards: %w", err)
	}

	// 4. Paginated Transaction Fetch & Ingestion
	cursor := opts.Cursor
	maxPages := opts.MaxPages
	if maxPages <= 0 {
		maxPages = 50
	}

	limit := opts.BatchLimit
	if limit <= 0 {
		limit = 100
	}

	var startDate, endDate time.Time
	if opts.LookbackDays > 0 {
		startDate = time.Now().UTC().AddDate(0, 0, -opts.LookbackDays)
		endDate = time.Now().UTC()
	}

	for page := 1; page <= maxPages; page++ {
		syncParams := connector.SyncParams{
			StartDate: startDate,
			EndDate:   endDate,
			Cursor:    cursor,
			Limit:     limit,
		}

		res, err := bankConn.FetchTransactions(ctx, conn.EncryptedCredentials, syncParams)
		if err != nil {
			s.recordConnectionFailure(ctx, familyID, connectionID, err)
			return nil, fmt.Errorf("page %d fetch failed: %w", page, err)
		}

		totalStats.PagesProcessed++

		// Atomically preserve raw payload and ingest transactions in a single transaction
		err = database.WithFamilyTx(ctx, s.pool, familyID, func(tx pgx.Tx) error {
			var rawPayloadID *uuid.UUID
			if len(res.RawPayload) > 0 {
				pid, err := PreserveRawPayload(ctx, tx, familyID, &connectionID, conn.Provider, res.RawPayload)
				if err != nil {
					return fmt.Errorf("failed to preserve raw payload on page %d: %w", page, err)
				}
				rawPayloadID = &pid
				totalStats.RawPayloadsPreserved++
			}

			if len(res.Transactions) > 0 {
				batchStats, err := IngestBatch(ctx, tx, familyID, rawPayloadID, res.Transactions, accountMap, cardMap, conn.AuthorizedByUserID)
				if err != nil {
					return fmt.Errorf("failed to ingest batch on page %d: %w", page, err)
				}
				totalStats.TransactionsIngested += batchStats.TransactionsIngested
				totalStats.TransactionsUpdated += batchStats.TransactionsUpdated
				totalStats.TransactionsSkipped += batchStats.TransactionsSkipped
			}

			return nil
		})
		if err != nil {
			s.recordConnectionFailure(ctx, familyID, connectionID, err)
			return nil, fmt.Errorf("page %d ingestion failed: %w", page, err)
		}

		if !res.Progress.HasMore || res.Progress.NextCursor == "" || res.Progress.NextCursor == cursor {
			break
		}
		cursor = res.Progress.NextCursor
	}

	// 5. Update BankConnection on successful sync
	_ = database.WithFamilyTx(ctx, s.pool, familyID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE public.bank_connections
			SET status = 'active',
			    last_synced_at = NOW(),
			    error_message = NULL,
			    updated_at = NOW()
			WHERE family_id = $1 AND id = $2
		`, familyID, connectionID)
		return err
	})

	return totalStats, nil
}

func (s *Service) recordConnectionFailure(ctx context.Context, familyID, connectionID uuid.UUID, syncErr error) {
	status := "error"
	if errors.Is(syncErr, connector.ErrAuthExpired) {
		status = "expired"
	}

	errMsg := syncErr.Error()
	if len(errMsg) > 500 {
		errMsg = errMsg[:500]
	}

	_ = database.WithFamilyTx(ctx, s.pool, familyID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE public.bank_connections
			SET status = $1,
			    error_message = $2,
			    updated_at = NOW()
			WHERE family_id = $3 AND id = $4
		`, status, errMsg, familyID, connectionID)
		return err
	})
}

// ProcessBankSyncJob processes a claimed bank_sync job from the worker queue.
func (s *Service) ProcessBankSyncJob(ctx context.Context, job *queue.JobRecord) error {
	var payload queue.SyncJobPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return fmt.Errorf("%w: failed to parse json: %v", queue.ErrInvalidJobPayload, err)
	}
	if err := payload.Validate(); err != nil {
		return err
	}
	opts := SyncOptions{
		LookbackDays:  payload.LookbackDays,
		Cursor:        payload.Cursor,
		ForceFullSync: payload.ForceFullSync,
	}
	_, err := s.SyncConnection(ctx, job.FamilyID, payload.ConnectionID, opts)
	return err
}

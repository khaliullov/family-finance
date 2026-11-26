package transaction

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
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
	pool              *pgxpool.Pool
	registry          *connector.Registry
	validator         *Validator
	cryptoKey         []byte
	SyncLeaseDuration time.Duration
	HeartbeatInterval time.Duration
}

// NewService creates a new Ingestion Service.
func NewService(pool *pgxpool.Pool, registry *connector.Registry) *Service {
	return &Service{
		pool:     pool,
		registry: registry,
	}
}

// SetValidator sets an explicit validator instance.
func (s *Service) SetValidator(v *Validator) {
	s.validator = v
}

// SetCryptoKey configures the master encryption key.
func (s *Service) SetCryptoKey(k []byte) {
	s.cryptoKey = k
}

// Validator returns the configured Validator, lazily initializing if necessary.
func (s *Service) Validator() *Validator {
	if s.validator != nil {
		return s.validator
	}
	s.validator = NewValidator(s.pool, s.registry, s.cryptoKey)
	return s.validator
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
			       status, encrypted_credentials, sync_cursor, last_synced_at, error_message,
			       created_at, updated_at
			FROM public.bank_connections
			WHERE family_id = $1 AND id = $2
		`, familyID, connectionID)

		return row.Scan(
			&conn.ID, &conn.FamilyID, &conn.AuthorizedByUserID, &conn.Provider, &conn.Name,
			&conn.Status, &conn.EncryptedCredentials, &conn.SyncCursor, &conn.LastSyncedAt, &conn.ErrorMessage,
			&conn.CreatedAt, &conn.UpdatedAt,
		)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrConnectionNotFound
		}
		return nil, fmt.Errorf("failed to load bank connection: %w", err)
	}

	if err := CanSyncConnection(conn.Status); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrConnectionNotActive, err)
	}

	// 2. Determine lease duration and heartbeat interval
	leaseDuration := s.SyncLeaseDuration
	if leaseDuration <= 0 {
		leaseDuration = 10 * time.Minute
	}
	heartbeatInterval := s.HeartbeatInterval
	if heartbeatInterval <= 0 {
		heartbeatInterval = leaseDuration / 3
		if heartbeatInterval < 10*time.Millisecond {
			heartbeatInterval = 10 * time.Millisecond
		}
	}
	leaseIntervalStr := fmt.Sprintf("%d milliseconds", leaseDuration.Milliseconds())

	// Acquire per-connection sync ownership atomically
	syncToken := uuid.New()
	var syncVersion int64
	err = database.WithFamilyTx(ctx, s.pool, familyID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE public.bank_connections
			SET active_sync_token = $1,
			    sync_version = sync_version + 1,
			    sync_locked_until = NOW() + ($4)::INTERVAL,
			    updated_at = NOW()
			WHERE family_id = $2 AND id = $3
			  AND (active_sync_token IS NULL OR sync_locked_until IS NULL OR sync_locked_until < NOW())
		`, syncToken, familyID, connectionID, leaseIntervalStr)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrSyncAlreadyRunning
		}

		return tx.QueryRow(ctx, `
			SELECT sync_version FROM public.bank_connections
			WHERE family_id = $1 AND id = $2
		`, familyID, connectionID).Scan(&syncVersion)
	})
	if err != nil {
		return nil, err
	}

	// Setup cancellable sync context and independent heartbeat goroutine
	syncCtx, cancelSync := context.WithCancel(ctx)
	defer cancelSync()

	var heartbeatErrMu sync.Mutex
	var heartbeatErr error

	setHeartbeatErr := func(err error) {
		heartbeatErrMu.Lock()
		if heartbeatErr == nil {
			heartbeatErr = err
		}
		heartbeatErrMu.Unlock()
		cancelSync()
	}

	getHeartbeatErr := func() error {
		heartbeatErrMu.Lock()
		defer heartbeatErrMu.Unlock()
		return heartbeatErr
	}

	checkAbort := func() error {
		if hbErr := getHeartbeatErr(); hbErr != nil {
			return hbErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := syncCtx.Err(); err != nil {
			if hbErr := getHeartbeatErr(); hbErr != nil {
				return hbErr
			}
			return err
		}
		return nil
	}

	stopHeartbeat := make(chan struct{})
	heartbeatDone := make(chan struct{})

	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(heartbeatInterval)
		defer ticker.Stop()

		for {
			select {
			case <-stopHeartbeat:
				return
			case <-syncCtx.Done():
				return
			case <-ticker.C:
				var renewed bool
				renewErr := database.WithFamilyTx(syncCtx, s.pool, familyID, func(tx pgx.Tx) error {
					tag, err := tx.Exec(syncCtx, `
						UPDATE public.bank_connections
						SET sync_locked_until = NOW() + ($4)::INTERVAL,
						    updated_at = NOW()
						WHERE family_id = $1 AND id = $2
						  AND active_sync_token = $3 AND sync_version = $5
						  AND sync_locked_until > NOW()
					`, familyID, connectionID, syncToken, leaseIntervalStr, syncVersion)
					if err != nil {
						return err
					}
					if tag.RowsAffected() > 0 {
						renewed = true
					}
					return nil
				})

				if renewErr != nil {
					if errors.Is(renewErr, context.Canceled) || errors.Is(renewErr, context.DeadlineExceeded) {
						return
					}
					slog.ErrorContext(syncCtx, "Heartbeat renewal failed with database error",
						"error", renewErr, "connection_id", connectionID)
					setHeartbeatErr(fmt.Errorf("%w: %v", ErrTransientDatabase, renewErr))
					return
				}

				if !renewed {
					slog.WarnContext(syncCtx, "Heartbeat renewal failed: sync lease expired or reclaimed",
						"connection_id", connectionID, "token", syncToken)
					setHeartbeatErr(ErrSyncOwnershipLost)
					return
				}
			}
		}
	}()

	var stopOnce sync.Once
	joinHeartbeat := func() {
		stopOnce.Do(func() {
			close(stopHeartbeat)
			<-heartbeatDone
		})
	}
	defer joinHeartbeat()

	if err := checkAbort(); err != nil {
		return totalStats, err
	}

	// 3. Resolve Connector
	bankConn, err := s.registry.Get(conn.Provider)
	if err != nil {
		s.recordConnectionFailure(ctx, familyID, connectionID, syncToken, syncVersion, err)
		return nil, fmt.Errorf("unsupported provider %q: %w", conn.Provider, err)
	}

	// 4. Discover and upsert accounts and cards
	accountMap := make(map[string]uuid.UUID)
	cardMap := make(map[string]uuid.UUID)

	accountDTOs, err := bankConn.FetchAccounts(syncCtx, conn.EncryptedCredentials)
	if err != nil {
		if hbErr := checkAbort(); hbErr != nil {
			return totalStats, hbErr
		}
		s.recordConnectionFailure(ctx, familyID, connectionID, syncToken, syncVersion, err)
		return nil, fmt.Errorf("failed to fetch accounts from provider: %w", err)
	}

	if err := checkAbort(); err != nil {
		return totalStats, err
	}

	err = database.WithFamilyTx(syncCtx, s.pool, familyID, func(tx pgx.Tx) error {
		// Verify sync ownership: strictly require unexpired lease
		var ownerToken *uuid.UUID
		var currentVersion int64
		err := tx.QueryRow(syncCtx, `
			SELECT active_sync_token, sync_version
			FROM public.bank_connections
			WHERE family_id = $1 AND id = $2 AND sync_locked_until > NOW()
			FOR UPDATE
		`, familyID, connectionID).Scan(&ownerToken, &currentVersion)
		if err != nil || ownerToken == nil || *ownerToken != syncToken || currentVersion != syncVersion {
			return ErrSyncOwnershipLost
		}

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

			// Primary Card Synchronization per account:
			// Follow Stage 1 account-first lock ordering to serialize concurrent card syncs
			var lockedAccID uuid.UUID
			err = tx.QueryRow(ctx, "SELECT id FROM public.accounts WHERE family_id = $1 AND id = $2 FOR UPDATE", familyID, accID).Scan(&lockedAccID)
			if err != nil {
				return fmt.Errorf("failed to acquire account lock for card synchronization: %w", err)
			}

			// Validate complete provider card set:
			// 1. Never leave a CLOSED card primary
			// 2. Disambiguate duplicate primary cards deterministically based on canonical external_id
			var primaryCandidates []string
			for i := range accDTO.Cards {
				if strings.EqualFold(accDTO.Cards[i].Status, "CLOSED") {
					accDTO.Cards[i].IsPrimary = false
				}
				if accDTO.Cards[i].IsPrimary {
					primaryCandidates = append(primaryCandidates, accDTO.Cards[i].ExternalID)
				}
			}

			var designatedPrimaryExtID *string
			if len(primaryCandidates) > 0 {
				sort.Strings(primaryCandidates)
				winner := primaryCandidates[0]
				designatedPrimaryExtID = &winner
				for i := range accDTO.Cards {
					accDTO.Cards[i].IsPrimary = (accDTO.Cards[i].ExternalID == winner)
				}
			}

			// If an incoming card is primary, clear is_primary on existing cards for this account first
			if designatedPrimaryExtID != nil {
				_, err := tx.Exec(ctx, `
					UPDATE public.cards
					SET is_primary = FALSE, updated_at = NOW()
					WHERE family_id = $1 AND account_id = $2 AND is_primary = TRUE AND external_id != $3
				`, familyID, accID, *designatedPrimaryExtID)
				if err != nil {
					return fmt.Errorf("failed to clear existing primary cards: %w", err)
				}
			}

			// Upsert cards for this account preserving existing cards not in response
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
		s.recordConnectionFailure(ctx, familyID, connectionID, syncToken, syncVersion, err)
		return nil, fmt.Errorf("failed to synchronize accounts and cards: %w", err)
	}

	// 5. Paginated Transaction Fetch & Ingestion with Durable Checkpointing
	//
	// Synchronization Mode and Cursor Resolution:
	// - Mode 1: Explicit restart from the beginning:
	//   opts.ResetCursor is true -> starts from opts.Cursor (default "") and ignores stored checkpoint.
	// - Mode 2: Explicit cursor provided:
	//   opts.Cursor != "" -> starts from opts.Cursor.
	// - Mode 3: Retry of an interrupted sync (both full and incremental):
	//   conn.SyncCursor != nil && *conn.SyncCursor != "" -> resumes from durable checkpoint (*conn.SyncCursor),
	//   preserving committed page progress even when opts.ForceFullSync is true!
	// - Mode 4: Initial full sync:
	//   conn.LastSyncedAt is nil AND conn.SyncCursor is nil -> starts from cursor "" with full lookback.
	// - Mode 5: Ordinary incremental sync:
	//   conn.LastSyncedAt != nil AND conn.SyncCursor is nil AND !opts.ForceFullSync -> incremental sync starting from LastSyncedAt.
	var cursor string
	if opts.ResetCursor {
		cursor = opts.Cursor
	} else if opts.Cursor != "" {
		cursor = opts.Cursor
	} else if conn.SyncCursor != nil && *conn.SyncCursor != "" {
		cursor = *conn.SyncCursor
	} else {
		cursor = ""
	}

	maxPages := opts.MaxPages
	if maxPages <= 0 {
		maxPages = 50
	}

	limit := opts.BatchLimit
	if limit <= 0 {
		limit = 100
	}

	var startDate, endDate time.Time
	if opts.ForceFullSync {
		if opts.LookbackDays > 0 {
			startDate = time.Now().UTC().AddDate(0, 0, -opts.LookbackDays)
			endDate = time.Now().UTC()
		}
	} else {
		if opts.LookbackDays > 0 {
			startDate = time.Now().UTC().AddDate(0, 0, -opts.LookbackDays)
			endDate = time.Now().UTC()
		} else if conn.LastSyncedAt != nil {
			startDate = *conn.LastSyncedAt
			endDate = time.Now().UTC()
		}
	}

	seenCursors := make(map[string]bool)

	for page := 1; page <= maxPages; page++ {
		if err := checkAbort(); err != nil {
			return totalStats, err
		}

		// Detect cursor loops and invalid repeated cursors
		if cursor != "" {
			if seenCursors[cursor] {
				loopErr := fmt.Errorf("%w: cursor %q already visited", ErrCursorLoop, cursor)
				s.recordConnectionFailure(ctx, familyID, connectionID, syncToken, syncVersion, loopErr)
				return totalStats, connector.NewSyncError(conn.Provider, "CURSOR_LOOP", loopErr.Error(), false, loopErr)
			}
			seenCursors[cursor] = true
		}

		syncParams := connector.SyncParams{
			StartDate: startDate,
			EndDate:   endDate,
			Cursor:    cursor,
			Limit:     limit,
		}

		res, err := bankConn.FetchTransactions(syncCtx, conn.EncryptedCredentials, syncParams)
		if err != nil {
			if hbErr := checkAbort(); hbErr != nil {
				return totalStats, hbErr
			}
			s.recordConnectionFailure(ctx, familyID, connectionID, syncToken, syncVersion, err)
			return nil, fmt.Errorf("page %d fetch failed: %w", page, err)
		}

		if err := checkAbort(); err != nil {
			return totalStats, err
		}

		totalStats.PagesProcessed++

		nextCursor := res.Progress.NextCursor
		hasMore := res.Progress.HasMore

		// 1. Critical: Reject invalid pagination progress before modifying state
		if hasMore && nextCursor == "" {
			paginationErr := fmt.Errorf("%w: page %d reported has_more=true with empty next cursor", ErrInvalidPagination, page)
			s.recordConnectionFailure(ctx, familyID, connectionID, syncToken, syncVersion, paginationErr)
			return totalStats, connector.NewSyncError(conn.Provider, "INVALID_PAGINATION", paginationErr.Error(), false, paginationErr)
		}

		if hasMore && nextCursor == cursor {
			loopErr := fmt.Errorf("%w: page %d reported next cursor identical to current cursor %q", ErrCursorLoop, page, cursor)
			s.recordConnectionFailure(ctx, familyID, connectionID, syncToken, syncVersion, loopErr)
			return totalStats, connector.NewSyncError(conn.Provider, "CURSOR_LOOP", loopErr.Error(), false, loopErr)
		}

		// Atomically preserve raw payload, ingest transactions, and checkpoint cursor in a single transaction
		err = database.WithFamilyTx(syncCtx, s.pool, familyID, func(tx pgx.Tx) error {
			// Verify sync ownership is still valid: strictly require unexpired lease
			var ownerToken *uuid.UUID
			var currentVersion int64
			err := tx.QueryRow(syncCtx, `
				SELECT active_sync_token, sync_version
				FROM public.bank_connections
				WHERE family_id = $1 AND id = $2 AND sync_locked_until > NOW()
				FOR UPDATE
			`, familyID, connectionID).Scan(&ownerToken, &currentVersion)
			if err != nil || ownerToken == nil || *ownerToken != syncToken || currentVersion != syncVersion {
				return ErrSyncOwnershipLost
			}

			var rawPayloadID *uuid.UUID
			if len(res.RawPayload) > 0 {
				pid, err := PreserveRawPayload(syncCtx, tx, familyID, &connectionID, conn.Provider, res.RawPayload)
				if err != nil {
					return fmt.Errorf("failed to preserve raw payload on page %d: %w", page, err)
				}
				rawPayloadID = &pid
				totalStats.RawPayloadsPreserved++
			}

			if len(res.Transactions) > 0 {
				batchStats, err := IngestBatch(syncCtx, tx, familyID, &connectionID, rawPayloadID, res.Transactions, accountMap, cardMap, conn.AuthorizedByUserID)
				if err != nil {
					return fmt.Errorf("failed to ingest batch on page %d: %w", page, err)
				}
				totalStats.TransactionsIngested += batchStats.TransactionsIngested
				totalStats.TransactionsUpdated += batchStats.TransactionsUpdated
				totalStats.TransactionsSkipped += batchStats.TransactionsSkipped
				totalStats.DiscrepanciesDetected += batchStats.DiscrepanciesDetected
				totalStats.Discrepancies = append(totalStats.Discrepancies, batchStats.Discrepancies...)
			}

			// Advance cursor atomically with the page ingestion
			if hasMore && nextCursor != "" {
				tag, err := tx.Exec(syncCtx, `
					UPDATE public.bank_connections
					SET sync_cursor = $1,
					    sync_locked_until = NOW() + ($6)::INTERVAL,
					    error_message = NULL,
					    updated_at = NOW()
					WHERE family_id = $2 AND id = $3 AND active_sync_token = $4 AND sync_version = $5
					  AND sync_locked_until > NOW()
				`, nextCursor, familyID, connectionID, syncToken, syncVersion, leaseIntervalStr)
				if err != nil {
					return fmt.Errorf("failed to checkpoint sync cursor on page %d: %w", page, err)
				}
				if tag.RowsAffected() == 0 {
					return ErrSyncOwnershipLost
				}
			} else if !hasMore {
				// Pagination complete; clear durable cursor
				tag, err := tx.Exec(syncCtx, `
					UPDATE public.bank_connections
					SET sync_cursor = NULL,
					    sync_locked_until = NOW() + ($5)::INTERVAL,
					    error_message = NULL,
					    updated_at = NOW()
					WHERE family_id = $1 AND id = $2 AND active_sync_token = $3 AND sync_version = $4
					  AND sync_locked_until > NOW()
				`, familyID, connectionID, syncToken, syncVersion, leaseIntervalStr)
				if err != nil {
					return fmt.Errorf("failed to clear sync cursor upon completion: %w", err)
				}
				if tag.RowsAffected() == 0 {
					return ErrSyncOwnershipLost
				}
			}

			return nil
		})
		if err != nil {
			if errors.Is(err, ErrSyncOwnershipLost) {
				return totalStats, ErrSyncOwnershipLost
			}
			s.recordConnectionFailure(ctx, familyID, connectionID, syncToken, syncVersion, err)
			return nil, fmt.Errorf("page %d ingestion failed: %w", page, err)
		}

		if !hasMore {
			break
		}

		if page == maxPages && hasMore {
			// Release sync lock so subsequent worker runs can resume from the durable checkpoint
			_ = database.WithFamilyTx(ctx, s.pool, familyID, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `
					UPDATE public.bank_connections
					SET active_sync_token = NULL,
					    sync_locked_until = NULL,
					    updated_at = NOW()
					WHERE family_id = $1 AND id = $2 AND active_sync_token = $3 AND sync_version = $4
					  AND sync_locked_until > NOW()
				`, familyID, connectionID, syncToken, syncVersion)
				return err
			})

			// Safety page limit reached while more pages remain
			incompleteErr := connector.NewSyncError(
				conn.Provider,
				"SYNC_INCOMPLETE",
				fmt.Sprintf("reached safety page limit (%d) with more pages pending", maxPages),
				true,
				ErrSyncIncomplete,
			)
			return totalStats, incompleteErr
		}

		cursor = nextCursor
	}

	if err := checkAbort(); err != nil {
		return totalStats, err
	}

	// 6. Update BankConnection on successful sync completion: strictly requires unexpired lease
	completionTime := time.Now().UTC()
	err = database.WithFamilyTx(ctx, s.pool, familyID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE public.bank_connections
			SET status = 'active',
			    last_synced_at = $1,
			    sync_cursor = NULL,
			    active_sync_token = NULL,
			    sync_locked_until = NULL,
			    error_message = NULL,
			    updated_at = NOW()
			WHERE family_id = $2 AND id = $3
			  AND active_sync_token = $4 AND sync_version = $5
			  AND sync_locked_until > NOW()
			  AND (last_synced_at IS NULL OR last_synced_at <= $1)
		`, completionTime, familyID, connectionID, syncToken, syncVersion)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrSyncOwnershipLost
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrSyncOwnershipLost) {
			return totalStats, ErrSyncOwnershipLost
		}
		slog.ErrorContext(ctx, "Failed to update connection completion status", "error", err, "connection_id", connectionID)
		return nil, fmt.Errorf("failed to update bank connection completion status: %w", err)
	}

	return totalStats, nil
}

func (s *Service) recordConnectionFailure(ctx context.Context, familyID, connectionID uuid.UUID, syncToken uuid.UUID, syncVersion int64, syncErr error) {
	if errors.Is(syncErr, ErrSyncOwnershipLost) {
		return
	}
	status := "error"
	if errors.Is(syncErr, connector.ErrAuthExpired) {
		status = "expired"
	}

	errMsg := syncErr.Error()
	if len(errMsg) > 500 {
		errMsg = errMsg[:500]
	}

	err := database.WithFamilyTx(ctx, s.pool, familyID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE public.bank_connections
			SET status = $1,
			    error_message = $2,
			    active_sync_token = NULL,
			    sync_locked_until = NULL,
			    updated_at = NOW()
			WHERE family_id = $3 AND id = $4
			  AND active_sync_token = $5 AND sync_version = $6
			  AND sync_locked_until > NOW()
		`, status, errMsg, familyID, connectionID, syncToken, syncVersion)
		return err
	})
	if err != nil {
		slog.ErrorContext(ctx, "Failed to persist connection error status in database",
			"error", err,
			"sync_error", syncErr,
			"family_id", familyID,
			"connection_id", connectionID,
		)
	}
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
		ResetCursor:   payload.ResetCursor,
	}
	_, err := s.SyncConnection(ctx, job.FamilyID, payload.ConnectionID, opts)
	if err != nil {
		if errors.Is(err, ErrSyncAlreadyRunning) {
			return connector.NewSyncError("queue", "SYNC_RUNNING", err.Error(), true, err)
		}
		return err
	}
	return nil
}

// ProcessBankValidationJob processes a claimed bank_validation job from the worker queue.
func (s *Service) ProcessBankValidationJob(ctx context.Context, job *queue.JobRecord) error {
	var payload queue.ValidationJobPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return fmt.Errorf("%w: failed to parse json: %v", queue.ErrInvalidJobPayload, err)
	}
	if err := payload.Validate(); err != nil {
		return err
	}

	validator := s.Validator()
	_, err := validator.ValidateConnection(ctx, job.FamilyID, payload.ConnectionID)
	return err
}

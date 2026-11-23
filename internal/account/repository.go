package account

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/khaliullov/family-finance/internal/database"
)

// Repository defines data access operations for accounts, cards, and balances.
type Repository interface {
	CreateAccount(ctx context.Context, familyID uuid.UUID, acc Account) (*Account, error)
	GetAccountByID(ctx context.Context, familyID, accountID uuid.UUID) (*Account, error)
	ListAccountsByFamily(ctx context.Context, familyID uuid.UUID, includeArchived bool) ([]Account, error)
	UpdateAccount(ctx context.Context, familyID, accountID uuid.UUID, params UpdateAccountParams) (*Account, error)
	ArchiveAccount(ctx context.Context, familyID, accountID uuid.UUID) error
	UpdateBalanceSnapshot(ctx context.Context, familyID, accountID uuid.UUID, snapshot BalanceSnapshotParams) (*Account, error)

	CreateCard(ctx context.Context, familyID uuid.UUID, card Card) (*Card, error)
	GetCardByID(ctx context.Context, familyID, cardID uuid.UUID) (*Card, error)
	ListCardsByAccount(ctx context.Context, familyID, accountID uuid.UUID) ([]Card, error)
	UpdateCard(ctx context.Context, familyID, cardID uuid.UUID, params UpdateCardParams) (*Card, error)
	CloseCard(ctx context.Context, familyID, cardID uuid.UUID) error
	SetPrimaryCard(ctx context.Context, familyID, accountID, cardID uuid.UUID) error

	GetPendingHolds(ctx context.Context, familyID, accountID uuid.UUID) ([]PendingHold, error)
	HasFinancialHistory(ctx context.Context, familyID, accountID uuid.UUID) (bool, error)
}

// PostgresRepository implements Repository using PostgreSQL with transaction-scoped tenant isolation.
type PostgresRepository struct {
	pool *pgxpool.Pool
}

// NewRepository creates a new PostgresRepository.
func NewRepository(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{pool: pool}
}

func (r *PostgresRepository) CreateAccount(ctx context.Context, familyID uuid.UUID, acc Account) (*Account, error) {
	if acc.ID == uuid.Nil {
		acc.ID = uuid.New()
	}
	acc.FamilyID = familyID

	var created Account
	err := database.WithFamilyTx(ctx, r.pool, familyID, func(tx pgx.Tx) error {
		query := `
			INSERT INTO public.accounts (
				id, family_id, bank_connection_id, owner_user_id, external_id,
				name, type, currency, current_balance_minor, available_balance_minor,
				debt_balance_minor, credit_limit_minor, statement_balance_minor,
				minimal_payment_minor, grace_period_end_date, balance_updated_at,
				balance_source, is_archived, created_at, updated_at
			) VALUES (
				$1, $2, $3, $4, $5,
				$6, $7, $8, $9, $10,
				$11, $12, $13, $14, $15,
				$16, $17, FALSE, NOW(), NOW()
			)
			RETURNING id, family_id, bank_connection_id, owner_user_id, external_id,
			          name, type, currency, current_balance_minor, available_balance_minor,
			          debt_balance_minor, credit_limit_minor, statement_balance_minor,
			          minimal_payment_minor, grace_period_end_date, balance_updated_at,
			          balance_source, is_archived, created_at, updated_at
		`
		return scanAccountRow(tx.QueryRow(ctx, query,
			acc.ID, acc.FamilyID, acc.BankConnectionID, acc.OwnerUserID, acc.ExternalID,
			acc.Name, string(acc.Type), acc.Currency, acc.CurrentBalanceMinor, acc.AvailableBalanceMinor,
			acc.DebtBalanceMinor, acc.CreditLimitMinor, acc.StatementBalanceMinor,
			acc.MinimalPaymentMinor, acc.GracePeriodEndDate, acc.BalanceUpdatedAt,
			acc.BalanceSource,
		), &created)
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			if pgErr.Code == pgerrcode.ForeignKeyViolation {
				return nil, fmt.Errorf("%w: owner or connection not found in family", ErrInvalidOwner)
			}
		}
		return nil, fmt.Errorf("failed to create account: %w", err)
	}
	return &created, nil
}

func (r *PostgresRepository) GetAccountByID(ctx context.Context, familyID, accountID uuid.UUID) (*Account, error) {
	var acc Account
	err := database.WithFamilyTx(ctx, r.pool, familyID, func(tx pgx.Tx) error {
		query := `
			SELECT id, family_id, bank_connection_id, owner_user_id, external_id,
			       name, type, currency, current_balance_minor, available_balance_minor,
			       debt_balance_minor, credit_limit_minor, statement_balance_minor,
			       minimal_payment_minor, grace_period_end_date, balance_updated_at,
			       balance_source, is_archived, created_at, updated_at
			FROM public.accounts
			WHERE family_id = $1 AND id = $2
		`
		return scanAccountRow(tx.QueryRow(ctx, query, familyID, accountID), &acc)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrAccountNotFound
		}
		return nil, fmt.Errorf("failed to get account by id: %w", err)
	}
	return &acc, nil
}

func (r *PostgresRepository) ListAccountsByFamily(ctx context.Context, familyID uuid.UUID, includeArchived bool) ([]Account, error) {
	var accounts []Account
	err := database.WithFamilyTx(ctx, r.pool, familyID, func(tx pgx.Tx) error {
		query := `
			SELECT id, family_id, bank_connection_id, owner_user_id, external_id,
			       name, type, currency, current_balance_minor, available_balance_minor,
			       debt_balance_minor, credit_limit_minor, statement_balance_minor,
			       minimal_payment_minor, grace_period_end_date, balance_updated_at,
			       balance_source, is_archived, created_at, updated_at
			FROM public.accounts
			WHERE family_id = $1 AND ($2 = TRUE OR is_archived = FALSE)
			ORDER BY created_at ASC
		`
		rows, err := tx.Query(ctx, query, familyID, includeArchived)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var acc Account
			if err := scanAccountRow(rows, &acc); err != nil {
				return err
			}
			accounts = append(accounts, acc)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list accounts: %w", err)
	}
	return accounts, nil
}

func (r *PostgresRepository) UpdateAccount(ctx context.Context, familyID, accountID uuid.UUID, params UpdateAccountParams) (*Account, error) {
	var updated Account
	err := database.WithFamilyTx(ctx, r.pool, familyID, func(tx pgx.Tx) error {
		query := `
			UPDATE public.accounts
			SET name = COALESCE($3, name),
			    updated_at = NOW()
			WHERE family_id = $1 AND id = $2 AND is_archived = FALSE
			RETURNING id, family_id, bank_connection_id, owner_user_id, external_id,
			          name, type, currency, current_balance_minor, available_balance_minor,
			          debt_balance_minor, credit_limit_minor, statement_balance_minor,
			          minimal_payment_minor, grace_period_end_date, balance_updated_at,
			          balance_source, is_archived, created_at, updated_at
		`
		return scanAccountRow(tx.QueryRow(ctx, query, familyID, accountID, params.Name), &updated)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrAccountNotFound
		}
		return nil, fmt.Errorf("failed to update account: %w", err)
	}
	return &updated, nil
}

func (r *PostgresRepository) ArchiveAccount(ctx context.Context, familyID, accountID uuid.UUID) error {
	return database.WithFamilyTx(ctx, r.pool, familyID, func(tx pgx.Tx) error {
		cmd, err := tx.Exec(ctx, `
			UPDATE public.accounts
			SET is_archived = TRUE, updated_at = NOW()
			WHERE family_id = $1 AND id = $2 AND is_archived = FALSE
		`, familyID, accountID)
		if err != nil {
			return err
		}
		if cmd.RowsAffected() == 0 {
			return ErrAccountNotFound
		}
		return nil
	})
}

func (r *PostgresRepository) UpdateBalanceSnapshot(ctx context.Context, familyID, accountID uuid.UUID, snapshot BalanceSnapshotParams) (*Account, error) {
	if err := snapshot.Validate(); err != nil {
		return nil, err
	}

	var updated Account
	err := database.WithFamilyTx(ctx, r.pool, familyID, func(tx pgx.Tx) error {
		query := `
			UPDATE public.accounts
			SET current_balance_minor = $3,
			    available_balance_minor = $4,
			    debt_balance_minor = $5,
			    credit_limit_minor = $6,
			    statement_balance_minor = $7,
			    minimal_payment_minor = $8,
			    grace_period_end_date = $9,
			    balance_source = $10,
			    balance_updated_at = $11,
			    updated_at = NOW()
			WHERE family_id = $1 AND id = $2 AND is_archived = FALSE
			RETURNING id, family_id, bank_connection_id, owner_user_id, external_id,
			          name, type, currency, current_balance_minor, available_balance_minor,
			          debt_balance_minor, credit_limit_minor, statement_balance_minor,
			          minimal_payment_minor, grace_period_end_date, balance_updated_at,
			          balance_source, is_archived, created_at, updated_at
		`
		return scanAccountRow(tx.QueryRow(ctx, query,
			familyID, accountID,
			snapshot.CurrentBalanceMinor, snapshot.AvailableBalanceMinor,
			snapshot.DebtBalanceMinor, snapshot.CreditLimitMinor,
			snapshot.StatementBalanceMinor, snapshot.MinimalPaymentMinor,
			snapshot.GracePeriodEndDate, string(snapshot.BalanceSource),
			snapshot.ObservedAt,
		), &updated)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrAccountNotFound
		}
		return nil, fmt.Errorf("failed to update balance snapshot: %w", err)
	}
	return &updated, nil
}

func (r *PostgresRepository) CreateCard(ctx context.Context, familyID uuid.UUID, card Card) (*Card, error) {
	if card.ID == uuid.Nil {
		card.ID = uuid.New()
	}
	card.FamilyID = familyID
	if card.Status == "" {
		card.Status = CardStatusActive
	}

	var created Card
	err := database.WithFamilyTx(ctx, r.pool, familyID, func(tx pgx.Tx) error {
		// If is_primary is set, reset other cards on the account first
		if card.IsPrimary {
			_, err := tx.Exec(ctx, `
				UPDATE public.cards
				SET is_primary = FALSE, updated_at = NOW()
				WHERE family_id = $1 AND account_id = $2 AND is_primary = TRUE
			`, familyID, card.AccountID)
			if err != nil {
				return err
			}
		}

		query := `
			INSERT INTO public.cards (
				id, family_id, account_id, owner_user_id, external_id,
				masked_pan, display_name, payment_system, is_virtual,
				is_primary, status, created_at, updated_at
			) VALUES (
				$1, $2, $3, $4, $5,
				$6, $7, $8, $9,
				$10, $11, NOW(), NOW()
			)
			RETURNING id, family_id, account_id, owner_user_id, external_id,
			          masked_pan, display_name, payment_system, is_virtual,
			          is_primary, status, created_at, updated_at
		`
		return scanCardRow(tx.QueryRow(ctx, query,
			card.ID, card.FamilyID, card.AccountID, card.OwnerUserID, card.ExternalID,
			card.MaskedPAN, card.DisplayName, string(card.PaymentSystem), card.IsVirtual,
			card.IsPrimary, string(card.Status),
		), &created)
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgerrcode.ForeignKeyViolation {
			return nil, fmt.Errorf("%w: account or owner does not belong to family", ErrCrossFamilyReference)
		}
		return nil, fmt.Errorf("failed to create card: %w", err)
	}
	return &created, nil
}

func (r *PostgresRepository) GetCardByID(ctx context.Context, familyID, cardID uuid.UUID) (*Card, error) {
	var card Card
	err := database.WithFamilyTx(ctx, r.pool, familyID, func(tx pgx.Tx) error {
		query := `
			SELECT id, family_id, account_id, owner_user_id, external_id,
			       masked_pan, display_name, payment_system, is_virtual,
			       is_primary, status, created_at, updated_at
			FROM public.cards
			WHERE family_id = $1 AND id = $2
		`
		return scanCardRow(tx.QueryRow(ctx, query, familyID, cardID), &card)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrCardNotFound
		}
		return nil, fmt.Errorf("failed to get card: %w", err)
	}
	return &card, nil
}

func (r *PostgresRepository) ListCardsByAccount(ctx context.Context, familyID, accountID uuid.UUID) ([]Card, error) {
	var cards []Card
	err := database.WithFamilyTx(ctx, r.pool, familyID, func(tx pgx.Tx) error {
		query := `
			SELECT id, family_id, account_id, owner_user_id, external_id,
			       masked_pan, display_name, payment_system, is_virtual,
			       is_primary, status, created_at, updated_at
			FROM public.cards
			WHERE family_id = $1 AND account_id = $2
			ORDER BY is_primary DESC, created_at ASC
		`
		rows, err := tx.Query(ctx, query, familyID, accountID)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var card Card
			if err := scanCardRow(rows, &card); err != nil {
				return err
			}
			cards = append(cards, card)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list cards: %w", err)
	}
	return cards, nil
}

func (r *PostgresRepository) UpdateCard(ctx context.Context, familyID, cardID uuid.UUID, params UpdateCardParams) (*Card, error) {
	var updated Card
	err := database.WithFamilyTx(ctx, r.pool, familyID, func(tx pgx.Tx) error {
		// If is_primary is set to true, reset other cards for this card's account
		if params.IsPrimary != nil && *params.IsPrimary {
			var accID uuid.UUID
			err := tx.QueryRow(ctx, "SELECT account_id FROM public.cards WHERE family_id = $1 AND id = $2", familyID, cardID).Scan(&accID)
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `
				UPDATE public.cards
				SET is_primary = FALSE, updated_at = NOW()
				WHERE family_id = $1 AND account_id = $2 AND id != $3
			`, familyID, accID, cardID)
			if err != nil {
				return err
			}
		}

		query := `
			UPDATE public.cards
			SET display_name = COALESCE($3, display_name),
			    status = COALESCE($4, status),
			    is_primary = COALESCE($5, is_primary),
			    updated_at = NOW()
			WHERE family_id = $1 AND id = $2
			RETURNING id, family_id, account_id, owner_user_id, external_id,
			          masked_pan, display_name, payment_system, is_virtual,
			          is_primary, status, created_at, updated_at
		`
		var statusStr *string
		if params.Status != nil {
			s := string(*params.Status)
			statusStr = &s
		}
		return scanCardRow(tx.QueryRow(ctx, query, familyID, cardID, params.DisplayName, statusStr, params.IsPrimary), &updated)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrCardNotFound
		}
		return nil, fmt.Errorf("failed to update card: %w", err)
	}
	return &updated, nil
}

func (r *PostgresRepository) CloseCard(ctx context.Context, familyID, cardID uuid.UUID) error {
	return database.WithFamilyTx(ctx, r.pool, familyID, func(tx pgx.Tx) error {
		cmd, err := tx.Exec(ctx, `
			UPDATE public.cards
			SET status = 'CLOSED', is_primary = FALSE, updated_at = NOW()
			WHERE family_id = $1 AND id = $2 AND status != 'CLOSED'
		`, familyID, cardID)
		if err != nil {
			return err
		}
		if cmd.RowsAffected() == 0 {
			return ErrCardNotFound
		}
		return nil
	})
}

func (r *PostgresRepository) SetPrimaryCard(ctx context.Context, familyID, accountID, cardID uuid.UUID) error {
	return database.WithFamilyTx(ctx, r.pool, familyID, func(tx pgx.Tx) error {
		// Ensure card belongs to this account and is active
		var status string
		err := tx.QueryRow(ctx, `
			SELECT status FROM public.cards
			WHERE family_id = $1 AND account_id = $2 AND id = $3
		`, familyID, accountID, cardID).Scan(&status)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrCardNotFound
			}
			return err
		}
		if status == string(CardStatusClosed) {
			return ErrCardClosed
		}

		// Reset all cards for account
		_, err = tx.Exec(ctx, `
			UPDATE public.cards
			SET is_primary = FALSE, updated_at = NOW()
			WHERE family_id = $1 AND account_id = $2
		`, familyID, accountID)
		if err != nil {
			return err
		}

		// Set designated card as primary
		_, err = tx.Exec(ctx, `
			UPDATE public.cards
			SET is_primary = TRUE, updated_at = NOW()
			WHERE family_id = $1 AND id = $2
		`, familyID, cardID)
		return err
	})
}

func (r *PostgresRepository) GetPendingHolds(ctx context.Context, familyID, accountID uuid.UUID) ([]PendingHold, error) {
	var holds []PendingHold
	err := database.WithFamilyTx(ctx, r.pool, familyID, func(tx pgx.Tx) error {
		query := `
			SELECT id, transaction_time, account_amount_minor, currency, raw_description, card_id
			FROM public.bank_transactions
			WHERE family_id = $1 AND account_id = $2 
			  AND booking_status = 'pending' 
			  AND account_amount_minor < 0
			ORDER BY transaction_time ASC
		`
		rows, err := tx.Query(ctx, query, familyID, accountID)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var h PendingHold
			if err := rows.Scan(&h.ID, &h.TransactionTime, &h.AmountMinor, &h.Currency, &h.Description, &h.CardID); err != nil {
				return err
			}
			h.HoldMinor = -h.AmountMinor // convert negative debit to positive hold magnitude
			holds = append(holds, h)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get pending holds: %w", err)
	}
	return holds, nil
}

func (r *PostgresRepository) HasFinancialHistory(ctx context.Context, familyID, accountID uuid.UUID) (bool, error) {
	var hasHistory bool
	err := database.WithFamilyTx(ctx, r.pool, familyID, func(tx pgx.Tx) error {
		query := `
			SELECT EXISTS (
				SELECT 1 FROM public.bank_transactions WHERE family_id = $1 AND account_id = $2
				UNION ALL
				SELECT 1 FROM public.accounting_entries WHERE family_id = $1 AND account_id = $2
			)
		`
		return tx.QueryRow(ctx, query, familyID, accountID).Scan(&hasHistory)
	})
	if err != nil {
		return false, fmt.Errorf("failed to check financial history: %w", err)
	}
	return hasHistory, nil
}

// scanner interface abstracts pgx.Row and pgx.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanAccountRow(s rowScanner, a *Account) error {
	var typeStr string
	err := s.Scan(
		&a.ID, &a.FamilyID, &a.BankConnectionID, &a.OwnerUserID, &a.ExternalID,
		&a.Name, &typeStr, &a.Currency, &a.CurrentBalanceMinor, &a.AvailableBalanceMinor,
		&a.DebtBalanceMinor, &a.CreditLimitMinor, &a.StatementBalanceMinor,
		&a.MinimalPaymentMinor, &a.GracePeriodEndDate, &a.BalanceUpdatedAt,
		&a.BalanceSource, &a.IsArchived, &a.CreatedAt, &a.UpdatedAt,
	)
	if err != nil {
		return err
	}
	a.Type = AccountType(typeStr)
	return nil
}

func scanCardRow(s rowScanner, c *Card) error {
	var paySys, status string
	err := s.Scan(
		&c.ID, &c.FamilyID, &c.AccountID, &c.OwnerUserID, &c.ExternalID,
		&c.MaskedPAN, &c.DisplayName, &paySys, &c.IsVirtual,
		&c.IsPrimary, &status, &c.CreatedAt, &c.UpdatedAt,
	)
	if err != nil {
		return err
	}
	c.PaymentSystem = PaymentSystem(paySys)
	c.Status = CardStatus(status)
	return nil
}

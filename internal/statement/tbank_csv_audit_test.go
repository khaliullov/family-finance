package statement

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/khaliullov/family-finance/internal/database"
)

// -----------------------------------------------------------------------------
// Finding 1 Verification Test:
// Legacy Tinkoff CSV header with 'Сумма платежа', 'Валюта платежа', 'Округление на инвесткопилку'
// -----------------------------------------------------------------------------
func TestAudit_LegacyTinkoffSchema_HeaderAliases(t *testing.T) {
	parser := NewTBankCSVParser()
	legacyCSV := []byte(
		"\"Дата операции\";\"Дата платежа\";\"Номер карты\";\"Статус\";\"Сумма операции\";\"Валюта операции\";\"Сумма платежа\";\"Валюта платежа\";\"Кэшбэк\";\"Категория\";\"MCC\";\"Описание\";\"Бонусы (включая кэшбэк)\";\"Округление на инвесткопилку\";\"Сумма операции с округлением\"\r\n" +
			"\"26.11.2026 22:54:30\";\"27.11.2026\";\"*1234\";\"Ок\";\"-900,00\";\"RUB\";\"-900,00\";\"RUB\";\"0,00\";\"Связь\";\"4816\";\"EVO.Интернет\";\"0,00\";\"0,00\";\"-900,00\"\r\n",
	)

	ctx := WithAccountSelector(context.Background(), AccountSelector{AccountName: "Black"})
	dto, err := parser.Parse(ctx, bytes.NewReader(legacyCSV), "RUB")
	if err != nil {
		t.Fatalf("expected legacy Tinkoff CSV to parse cleanly with aliases, got: %v", err)
	}
	if len(dto.Rows) != 1 {
		t.Fatalf("expected 1 row parsed, got %d", len(dto.Rows))
	}
	if dto.Rows[0].AccountAmountMinor != -90000 {
		t.Errorf("expected -90000 minor units, got %d", dto.Rows[0].AccountAmountMinor)
	}
}

// -----------------------------------------------------------------------------
// Finding 2 Verification Test:
// Analytics == 'Нет' alone must NOT cause a credit to be classified as a refund
// -----------------------------------------------------------------------------
func TestAudit_RefundClassification_AnalyticsFlagAloneCannotEstablishRefund(t *testing.T) {
	// A user receives money from a peer or sells an item, but turns off "Учёт в аналитике" in the mobile app.
	// This is NOT a merchant refund!
	category := "Другое"
	desc := "Пополнение от друга"
	amountMinor := int64(500000) // +5000.00 RUB
	analyticsFlag := "Нет"

	opType := ClassifyTBankOperation(category, desc, amountMinor, analyticsFlag)
	if opType == "refund" {
		t.Fatalf("CRITICAL: arbitrary credit with analytics='Нет' was wrongly classified as 'refund'!")
	}
	if opType != "transfer_in" {
		t.Errorf("expected 'transfer_in' for non-refund credit, got %q", opType)
	}

	// Confirmed refund with explicit refund indicator MUST still be classified as 'refund'
	refundDesc := "Возврат покупки в магазине"
	refundOpType := ClassifyTBankOperation("Супермаркеты", refundDesc, amountMinor, "Да")
	if refundOpType != "refund" {
		t.Errorf("expected 'refund' for explicit refund description, got %q", refundOpType)
	}
}

// -----------------------------------------------------------------------------
// Finding 3 Verification Test:
// Cancelled operations using ResolutionActionSkipCancelled and DB-level guards
// -----------------------------------------------------------------------------
func TestAudit_CancellationLifecycle_ResolutionActionAndDBGuard(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()

	ctx := context.Background()
	familyID, userID, accountID := setupStatementTenantFixture(t)
	batchID := uuid.New()

	err := database.WithFamilyUserTx(ctx, appPool, familyID, userID, func(tx pgx.Tx) error {
		docID := uuid.New()
		_, err := tx.Exec(ctx, `
			INSERT INTO public.statement_documents (id, family_id, uploaded_by_user_id, original_filename, file_size_bytes, content_hash, mime_type, encrypted_content)
			VALUES ($1, $2, $3, 'test.csv', 100, 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 'text/csv', $4)
		`, docID, familyID, userID, testCryptoKey)
		if err != nil {
			return err
		}

		_, err = tx.Exec(ctx, `
			INSERT INTO public.statement_import_batches (id, family_id, account_id, document_id, source_format, parser_version, status, reconciliation_status, total_rows, staged_count)
			VALUES ($1, $2, $3, $4, 'tbank_csv_v1', '1.0.0', 'staged', 'reconciled', 1, 1)
		`, batchID, familyID, accountID, docID)
		if err != nil {
			return err
		}

		stagedID := uuid.New()
		_, err = tx.Exec(ctx, `
			INSERT INTO public.staged_bank_transactions (
				id, family_id, batch_id, account_id, row_index,
				transaction_time, amount_minor, currency, account_amount_minor, account_currency,
				operation_type, booking_status, raw_description, import_fingerprint,
				duplicate_status, resolution_action
			) VALUES (
				$1, $2, $3, $4, 1,
				NOW(), -10000, 'RUB', -10000, 'RUB',
				'purchase', 'cancelled', 'Failed payment', 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',
				'new', 'skip_cancelled'
			)
		`, stagedID, familyID, batchID, accountID)
		if err != nil {
			return err
		}

		// 1. Database Guard Test: verify DB constraint rejects setting resolution_action='commit' on cancelled row
		_, updateErr := tx.Exec(ctx, `
			UPDATE public.staged_bank_transactions
			SET resolution_action = 'commit'
			WHERE id = $1
		`, stagedID)
		if updateErr == nil {
			t.Fatalf("CRITICAL SECURITY DEFECT: database allowed setting resolution_action='commit' on a cancelled staged row!")
		}
		t.Logf("Database correctly blocked update: %v", updateErr)

		return updateErr
	})
	if err == nil {
		t.Fatalf("expected error from aborted transaction, got nil")
	}

	// 2. Promotion Guard Test: verify cancelled rows always receive ResolutionActionSkipCancelled
	err = database.WithFamilyUserTx(ctx, appPool, familyID, userID, func(tx pgx.Tx) error {
		stagedRows, err := NewRepository().GetStagedTransactionsByBatch(ctx, tx, familyID, batchID)
		if err != nil {
			return err
		}
		if len(stagedRows) > 0 && stagedRows[0].ResolutionAction == ResolutionActionCommit {
			t.Fatalf("CRITICAL SECURITY DEFECT: cancelled staged row has resolution_action='commit'!")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("failed to query staged rows: %v", err)
	}
}

// -----------------------------------------------------------------------------
// Finding 4 Verification Test:
// Card mask must never override conflicting account name
// -----------------------------------------------------------------------------
func TestAudit_AccountSelector_CardMaskCannotOverrideAccountNameMismatch(t *testing.T) {
	parser := NewTBankCSVParser()

	// A file with a row for account "Платинум" and card "*1234"
	csvContent := []byte(
		"\"Имя счёта\";\"Номер карты\";\"Дата операции\";\"Сумма операции\";\"Валюта операции\";\"Сумма в валюте счёта\";\"Валюта счёта\";\"Статус\";\"Описание\"\r\n" +
			"\"Платинум\";\"*1234\";\"26.11.2026 22:54:30\";\"-900,00\";\"RUB\";\"-900,00\";\"RUB\";\"Ок\";\"Some Merchant\"\r\n",
	)

	// Target selector is for account "Black", but selector has CardMasks: ["*1234"]
	ctx := WithAccountSelector(context.Background(), AccountSelector{
		AccountName: "Black",
		CardMasks:   []string{"*1234"},
	})

	_, err := parser.Parse(ctx, bytes.NewReader(csvContent), "RUB")
	if err == nil {
		t.Fatalf("expected error because row for 'Платинум' must NOT match target account 'Black'")
	}
	if !errors.Is(err, ErrAccountNotFoundInStatement) {
		t.Errorf("expected ErrAccountNotFoundInStatement, got: %v", err)
	}
}

// -----------------------------------------------------------------------------
// Account Selector Verification Test:
// Accounts with same display name but different account currencies
// -----------------------------------------------------------------------------
func TestAudit_AccountSelector_SameNameDifferentCurrencies(t *testing.T) {
	parser := NewTBankCSVParser()

	// File with two accounts named "Black": one in RUB, one in USD
	csvContent := []byte(
		"\"Имя счёта\";\"Номер карты\";\"Дата операции\";\"Сумма операции\";\"Валюта операции\";\"Сумма в валюте счёта\";\"Валюта счёта\";\"Статус\";\"Описание\"\r\n" +
			"\"Black\";\"*1111\";\"26.11.2026 22:54:30\";\"-10,00\";\"USD\";\"-10,00\";\"USD\";\"Ок\";\"US Store\"\r\n" +
			"\"Black\";\"*2222\";\"26.11.2026 22:55:00\";\"-500,00\";\"RUB\";\"-500,00\";\"RUB\";\"Ок\";\"RU Store\"\r\n",
	)

	// Select the USD Black account
	ctxUSD := WithAccountSelector(context.Background(), AccountSelector{
		AccountName: "Black",
		Currency:    "USD",
	})
	dtoUSD, err := parser.Parse(ctxUSD, bytes.NewReader(csvContent), "USD")
	if err != nil {
		t.Fatalf("failed to parse USD account: %v", err)
	}
	if len(dtoUSD.Rows) != 1 {
		t.Fatalf("expected 1 row for USD Black, got %d", len(dtoUSD.Rows))
	}
	if dtoUSD.Rows[0].AccountAmountMinor != -1000 {
		t.Errorf("expected -1000 minor units (USD), got %d", dtoUSD.Rows[0].AccountAmountMinor)
	}
	if dtoUSD.UnmatchedRowsCount != 1 {
		t.Errorf("expected 1 unmatched row, got %d", dtoUSD.UnmatchedRowsCount)
	}
}

package statement

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/khaliullov/family-finance/internal/database"
	"github.com/khaliullov/family-finance/internal/domain"
)

const testdataTBankDir = "testdata/tbank"

func loadFixture(t *testing.T, filename string) []byte {
	t.Helper()
	path := filepath.Join(testdataTBankDir, filename)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read test fixture %s: %v", filename, err)
	}
	return data
}

// 1. Real 122-row CSV fixture & OFX cross-check:
// Verifies all 122 rows are parsed deterministically across accounts,
// and matching posted rows align with OFX reference entries.
func TestTBankCSV_ValidReal122Rows_AndOFXCrossCheck(t *testing.T) {
	parser := NewTBankCSVParser()
	csvBytes := loadFixture(t, "tbank_operations_valid.csv")
	ofxBytes := loadFixture(t, "tbank_operations_valid.ofx")

	// Parse with selector for "Платинум"
	ctxPlatinum := WithAccountSelector(context.Background(), AccountSelector{
		AccountName: "Платинум",
	})
	resPlatinum, err := parser.Parse(ctxPlatinum, bytes.NewReader(csvBytes), "RUB")
	if err != nil {
		t.Fatalf("failed to parse Platinum rows: %v", err)
	}
	if len(resPlatinum.Rows) != 88 {
		t.Errorf("expected 88 Platinum rows, got %d", len(resPlatinum.Rows))
	}

	// Parse with selector for "Black"
	ctxBlack := WithAccountSelector(context.Background(), AccountSelector{
		AccountName: "Black",
	})
	resBlack, err := parser.Parse(ctxBlack, bytes.NewReader(csvBytes), "RUB")
	if err != nil {
		t.Fatalf("failed to parse Black rows: %v", err)
	}
	if len(resBlack.Rows) != 32 {
		t.Errorf("expected 32 Black rows, got %d", len(resBlack.Rows))
	}

	// Parse with selector for "Накопительный счет"
	ctxSavings := WithAccountSelector(context.Background(), AccountSelector{
		AccountName: "Накопительный счет",
	})
	resSavings, err := parser.Parse(ctxSavings, bytes.NewReader(csvBytes), "RUB")
	if err != nil {
		t.Fatalf("failed to parse Savings rows: %v", err)
	}
	if len(resSavings.Rows) != 2 {
		t.Errorf("expected 2 Savings rows, got %d", len(resSavings.Rows))
	}

	// Total across all 3 accounts must equal 122 rows
	totalParsed := len(resPlatinum.Rows) + len(resBlack.Rows) + len(resSavings.Rows)
	if totalParsed != 122 {
		t.Errorf("expected 122 total operations across accounts, got %d", totalParsed)
	}

	// Cross-check against OFX reference
	type OFXSTMTTRN struct {
		TRNTYPE  string `xml:"TRNTYPE"`
		DTPOSTED string `xml:"DTPOSTED"`
		TRNAMT   string `xml:"TRNAMT"`
		FITID    string `xml:"FITID"`
		NAME     string `xml:"NAME"`
	}
	type OFXSTMTRS struct {
		CURDEF       string       `xml:"CURDEF"`
		BANKACCTID   string       `xml:"BANKACCTFROM>ACCTID"`
		TRANSACTIONS []OFXSTMTTRN `xml:"BANKTRANLIST>STMTTRN"`
	}
	type OFXRoot struct {
		Statements []OFXSTMTRS `xml:"BANKMSGSRSV1>STMTTRNRS>STMTRS"`
	}

	xmlStart := bytes.Index(ofxBytes, []byte("<OFX>"))
	if xmlStart < 0 {
		t.Fatalf("OFX opening tag not found")
	}

	var ofxRoot OFXRoot
	if err := xml.Unmarshal(ofxBytes[xmlStart:], &ofxRoot); err != nil {
		t.Fatalf("failed to unmarshal reference OFX: %v", err)
	}

	totalOFXTxs := 0
	for _, stmt := range ofxRoot.Statements {
		totalOFXTxs += len(stmt.TRANSACTIONS)
	}
	// OFX only includes posted transactions (117), omitting 5 cancelled operations
	if totalOFXTxs != 117 {
		t.Errorf("expected 117 transactions in reference OFX, got %d", totalOFXTxs)
	}
}

// 2. UTF-8 BOM handling:
func TestTBankCSV_UTF8BOM(t *testing.T) {
	parser := NewTBankCSVParser()
	bomBytes := loadFixture(t, "tbank_operations_utf8_bom.csv")

	ctx := WithAccountSelector(context.Background(), AccountSelector{AccountName: "Платинум"})
	res, err := parser.Parse(ctx, bytes.NewReader(bomBytes), "RUB")
	if err != nil {
		t.Fatalf("failed to parse CSV with UTF-8 BOM: %v", err)
	}
	if len(res.Rows) != 88 {
		t.Errorf("expected 88 rows from BOM fixture, got %d", len(res.Rows))
	}
}

// 3. Windows-1251 (CP1251) decoding:
func TestTBankCSV_Windows1251(t *testing.T) {
	parser := NewTBankCSVParser()
	cp1251Bytes := loadFixture(t, "tbank_operations_windows1251.csv")

	// Sniff must detect Windows-1251 header
	if !parser.Sniff(cp1251Bytes[:min(512, len(cp1251Bytes))]) {
		t.Errorf("expected Sniff to detect Windows-1251 T-Bank header")
	}

	ctx := WithAccountSelector(context.Background(), AccountSelector{AccountName: "Платинум"})
	res, err := parser.Parse(ctx, bytes.NewReader(cp1251Bytes), "RUB")
	if err != nil {
		t.Fatalf("failed to parse Windows-1251 CSV: %v", err)
	}
	if len(res.Rows) != 88 {
		t.Errorf("expected 88 rows from Windows-1251 fixture, got %d", len(res.Rows))
	}
}

// 4. CRLF and LF line endings:
func TestTBankCSV_CRLF_and_LF(t *testing.T) {
	parser := NewTBankCSVParser()
	rawBytes := loadFixture(t, "tbank_operations_valid.csv")

	// Replace all CRLF with isolated LF
	lfBytes := bytes.ReplaceAll(rawBytes, []byte("\r\n"), []byte("\n"))

	ctx := WithAccountSelector(context.Background(), AccountSelector{AccountName: "Платинум"})
	res, err := parser.Parse(ctx, bytes.NewReader(lfBytes), "RUB")
	if err != nil {
		t.Fatalf("failed to parse LF-only CSV: %v", err)
	}
	if len(res.Rows) != 88 {
		t.Errorf("expected 88 rows from LF-only CSV, got %d", len(res.Rows))
	}
}

// 5. Reordered columns:
func TestTBankCSV_ReorderedColumns(t *testing.T) {
	parser := NewTBankCSVParser()
	reorderedBytes := loadFixture(t, "tbank_operations_reordered_cols.csv")

	ctx := WithAccountSelector(context.Background(), AccountSelector{AccountName: "Платинум"})
	res, err := parser.Parse(ctx, bytes.NewReader(reorderedBytes), "RUB")
	if err != nil {
		t.Fatalf("failed to parse CSV with reordered columns: %v", err)
	}
	if len(res.Rows) != 88 {
		t.Errorf("expected 88 rows from reordered CSV, got %d", len(res.Rows))
	}
}

// 6. Missing required headers:
func TestTBankCSV_MissingRequiredHeaders(t *testing.T) {
	parser := NewTBankCSVParser()
	missingBytes := loadFixture(t, "tbank_operations_missing_required_col.csv")

	ctx := WithAccountSelector(context.Background(), AccountSelector{AccountName: "Платинум"})
	_, err := parser.Parse(ctx, bytes.NewReader(missingBytes), "RUB")
	if err == nil {
		t.Fatalf("expected error for missing required column, got nil")
	}
	if !errors.Is(err, ErrMissingRequiredColumn) {
		t.Errorf("expected ErrMissingRequiredColumn, got: %v", err)
	}
}

// 7. Malformed CSV quoting:
func TestTBankCSV_MalformedCSVQuoting(t *testing.T) {
	parser := NewTBankCSVParser()
	malformed := []byte("\"Имя счёта\";\"Дата операции\";\"Сумма операции\";\"Валюта операции\";\"Сумма в валюте счёта\";\"Валюта счёта\";\"Статус\";\"Описание\"\r\n" +
		"\"Платинум\";\"26.11.2026 22:54:30\";\"-900,00\";\"RUB\";\"-900,00\";\"RUB\";\"Ок\";\"Unclosed quote\r\n")

	ctx := WithAccountSelector(context.Background(), AccountSelector{AccountName: "Платинум"})
	_, err := parser.Parse(ctx, bytes.NewReader(malformed), "RUB")
	if err == nil {
		t.Fatalf("expected error for malformed quoting, got nil")
	}
	if !errors.Is(err, ErrMalformedCSVQuoting) && !strings.Contains(err.Error(), "quote") {
		t.Errorf("expected quoting error, got: %v", err)
	}
}

// 8. Invalid dates:
func TestTBankCSV_InvalidDates(t *testing.T) {
	parser := NewTBankCSVParser()
	invalidDateBytes := loadFixture(t, "tbank_operations_malformed_date.csv")

	ctx := WithAccountSelector(context.Background(), AccountSelector{AccountName: "Платинум"})
	_, err := parser.Parse(ctx, bytes.NewReader(invalidDateBytes), "RUB")
	if err == nil {
		t.Fatalf("expected error for invalid date, got nil")
	}
	if !errors.Is(err, ErrMalformedDate) {
		t.Errorf("expected ErrMalformedDate, got: %v", err)
	}
}

// 9. Invalid amounts:
func TestTBankCSV_InvalidAmounts(t *testing.T) {
	parser := NewTBankCSVParser()
	invalidAmtBytes := loadFixture(t, "tbank_operations_malformed_amount.csv")

	ctx := WithAccountSelector(context.Background(), AccountSelector{AccountName: "Платинум"})
	_, err := parser.Parse(ctx, bytes.NewReader(invalidAmtBytes), "RUB")
	if err == nil {
		t.Fatalf("expected error for invalid amount, got nil")
	}
	if !errors.Is(err, domain.ErrInvalidAmount) {
		t.Errorf("expected domain.ErrInvalidAmount, got: %v", err)
	}
}

// 10. Currency mismatch:
func TestTBankCSV_CurrencyMismatch(t *testing.T) {
	parser := NewTBankCSVParser()
	currMismatchBytes := loadFixture(t, "tbank_operations_currency_mismatch.csv")

	ctx := WithAccountSelector(context.Background(), AccountSelector{AccountName: "Платинум"})
	_, err := parser.Parse(ctx, bytes.NewReader(currMismatchBytes), "RUB")
	if err == nil {
		t.Fatalf("expected error for currency mismatch, got nil")
	}
	if !strings.Contains(err.Error(), "does not match target account currency") {
		t.Errorf("expected currency mismatch error, got: %v", err)
	}
}

// 11. Unknown status:
func TestTBankCSV_UnknownStatus(t *testing.T) {
	parser := NewTBankCSVParser()
	csvData := "\"Имя счёта\";\"Дата операции\";\"Сумма операции\";\"Валюта операции\";\"Сумма в валюте счёта\";\"Валюта счёта\";\"Статус\";\"Описание\"\r\n" +
		"\"Платинум\";\"26.11.2026 22:54:30\";\"-900,00\";\"RUB\";\"-900,00\";\"RUB\";\"UNKNOWN_STATUS\";\"Coffee\"\r\n"

	ctx := WithAccountSelector(context.Background(), AccountSelector{AccountName: "Платинум"})
	_, err := parser.Parse(ctx, strings.NewReader(csvData), "RUB")
	if err == nil {
		t.Fatalf("expected error for unknown status, got nil")
	}
	if !errors.Is(err, ErrInvalidBookingStatus) {
		t.Errorf("expected ErrInvalidBookingStatus, got: %v", err)
	}
}

// 12. Pending operations:
func TestTBankCSV_PendingOperations(t *testing.T) {
	parser := NewTBankCSVParser()
	pendingBytes := loadFixture(t, "tbank_operations_pending.csv")

	ctx := WithAccountSelector(context.Background(), AccountSelector{AccountName: "Платинум"})
	res, err := parser.Parse(ctx, bytes.NewReader(pendingBytes), "RUB")
	if err != nil {
		t.Fatalf("failed to parse pending operation: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(res.Rows))
	}
	if res.Rows[0].BookingStatus != "pending" {
		t.Errorf("expected booking_status = 'pending', got %q", res.Rows[0].BookingStatus)
	}
	if res.Rows[0].SettledTime != nil {
		t.Errorf("expected nil SettledTime for pending operation, got %v", res.Rows[0].SettledTime)
	}
}

// 13. Cancelled operations:
func TestTBankCSV_CancelledOperations(t *testing.T) {
	parser := NewTBankCSVParser()
	rawBytes := loadFixture(t, "tbank_operations_valid.csv")

	// Parse Black account, which contains 4 failed/cancelled operations in this export
	ctx := WithAccountSelector(context.Background(), AccountSelector{AccountName: "Black"})
	res, err := parser.Parse(ctx, bytes.NewReader(rawBytes), "RUB")
	if err != nil {
		t.Fatalf("failed to parse Black account: %v", err)
	}

	cancelledCount := 0
	for _, r := range res.Rows {
		if r.BookingStatus == "cancelled" {
			cancelledCount++
		}
	}
	if cancelledCount != 4 {
		t.Errorf("expected 4 cancelled operations in Black account, found %d", cancelledCount)
	}
}

// 14. Zero-amount card verification checks:
func TestTBankCSV_ZeroAmountCardChecks(t *testing.T) {
	parser := NewTBankCSVParser()
	zeroBytes := loadFixture(t, "tbank_operations_zero_card_check.csv")

	ctx := WithAccountSelector(context.Background(), AccountSelector{AccountName: "Платинум"})
	res, err := parser.Parse(ctx, bytes.NewReader(zeroBytes), "RUB")
	if err != nil {
		t.Fatalf("failed to parse zero card check: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(res.Rows))
	}
	if res.Rows[0].AmountMinor != 0 || res.Rows[0].AccountAmountMinor != 0 {
		t.Errorf("expected 0 minor units, got op=%d acc=%d", res.Rows[0].AmountMinor, res.Rows[0].AccountAmountMinor)
	}
	if res.Rows[0].OperationType != "card_check" {
		t.Errorf("expected operation_type = 'card_check', got %q", res.Rows[0].OperationType)
	}
}

// 15. Multi-account export filtering:
func TestTBankCSV_MultiAccountExportFiltering(t *testing.T) {
	parser := NewTBankCSVParser()
	rawBytes := loadFixture(t, "tbank_operations_valid.csv")

	// Filter Black
	ctxBlack := WithAccountSelector(context.Background(), AccountSelector{AccountName: "Black"})
	resBlack, err := parser.Parse(ctxBlack, bytes.NewReader(rawBytes), "RUB")
	if err != nil {
		t.Fatalf("failed to parse Black account: %v", err)
	}
	if len(resBlack.Rows) != 32 {
		t.Errorf("expected 32 rows, got %d", len(resBlack.Rows))
	}
	if resBlack.UnmatchedRowsCount != 90 {
		t.Errorf("expected 90 unmatched rows (88 Plat + 2 Sav), got %d", resBlack.UnmatchedRowsCount)
	}
	if resBlack.UnmatchedAccounts["Платинум"] != 88 || resBlack.UnmatchedAccounts["Накопительный счет"] != 2 {
		t.Errorf("unmatched accounts map mismatch: %v", resBlack.UnmatchedAccounts)
	}
}

// 16. Ambiguous account selection:
func TestTBankCSV_AmbiguousAccountSelection(t *testing.T) {
	parser := NewTBankCSVParser()
	rawBytes := loadFixture(t, "tbank_operations_valid.csv")

	// Multi-account export without selector MUST be rejected
	_, err := parser.Parse(context.Background(), bytes.NewReader(rawBytes), "RUB")
	if err == nil {
		t.Fatalf("expected error when parsing multi-account CSV without selector, got nil")
	}
	if !errors.Is(err, ErrMultiAccountAmbiguity) {
		t.Errorf("expected ErrMultiAccountAmbiguity, got: %v", err)
	}

	// Selector with non-existent account MUST be rejected
	ctxNonExistent := WithAccountSelector(context.Background(), AccountSelector{AccountName: "NonExistentAccount"})
	_, err = parser.Parse(ctxNonExistent, bytes.NewReader(rawBytes), "RUB")
	if err == nil {
		t.Fatalf("expected error when target account is not present in statement, got nil")
	}
	if !errors.Is(err, ErrAccountNotFoundInStatement) {
		t.Errorf("expected ErrAccountNotFoundInStatement, got: %v", err)
	}
}

// 17. Context cancellation:
func TestTBankCSV_ContextCancellation(t *testing.T) {
	parser := NewTBankCSVParser()
	rawBytes := loadFixture(t, "tbank_operations_valid.csv")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	ctxWithSel := WithAccountSelector(ctx, AccountSelector{AccountName: "Платинум"})
	_, err := parser.Parse(ctxWithSel, bytes.NewReader(rawBytes), "RUB")
	if err == nil {
		t.Fatalf("expected context cancellation error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got: %v", err)
	}
}

// 18. Maximum input size enforcement:
func TestTBankCSV_MaximumInputSize(t *testing.T) {
	parser := NewTBankCSVParser()
	parser.maxInputSize = 100 // Set artificial small limit

	rawBytes := loadFixture(t, "tbank_operations_valid.csv")
	ctx := WithAccountSelector(context.Background(), AccountSelector{AccountName: "Платинум"})
	_, err := parser.Parse(ctx, bytes.NewReader(rawBytes), "RUB")
	if err == nil {
		t.Fatalf("expected ErrFileTooLarge, got nil")
	}
	if !errors.Is(err, ErrFileTooLarge) {
		t.Errorf("expected ErrFileTooLarge, got: %v", err)
	}
}

// 19. Maximum record size enforcement (64 KiB):
func TestTBankCSV_MaximumRecordSize(t *testing.T) {
	parser := NewTBankCSVParser()

	// Construct CSV with a record exceeding 64 KiB
	hugeDesc := strings.Repeat("A", 65*1024)
	csvData := fmt.Sprintf("\"Имя счёта\";\"Дата операции\";\"Сумма операции\";\"Валюта операции\";\"Сумма в валюте счёта\";\"Валюта счёта\";\"Статус\";\"Описание\"\r\n"+
		"\"Платинум\";\"26.11.2026 22:54:30\";\"-900,00\";\"RUB\";\"-900,00\";\"RUB\";\"Ок\";\"%s\"\r\n", hugeDesc)

	ctx := WithAccountSelector(context.Background(), AccountSelector{AccountName: "Платинум"})
	_, err := parser.Parse(ctx, strings.NewReader(csvData), "RUB")
	if err == nil {
		t.Fatalf("expected ErrRecordTooLarge, got nil")
	}
	if !errors.Is(err, ErrRecordTooLarge) {
		t.Errorf("expected ErrRecordTooLarge, got: %v", err)
	}
}

// 20. Maximum row count enforcement (50,000):
func TestTBankCSV_MaximumRowCount(t *testing.T) {
	parser := NewTBankCSVParser()
	parser.maxRows = 5 // Set small limit

	rawBytes := loadFixture(t, "tbank_operations_valid.csv")
	ctx := WithAccountSelector(context.Background(), AccountSelector{AccountName: "Платинум"})
	_, err := parser.Parse(ctx, bytes.NewReader(rawBytes), "RUB")
	if err == nil {
		t.Fatalf("expected ErrTooManyRows, got nil")
	}
	if !errors.Is(err, ErrTooManyRows) {
		t.Errorf("expected ErrTooManyRows, got: %v", err)
	}
}

// 21. Cashback and round-up semantics:
func TestTBankCSV_CashbackAndRoundUpSemantics(t *testing.T) {
	parser := NewTBankCSVParser()
	rawBytes := loadFixture(t, "tbank_operations_valid.csv")

	ctx := WithAccountSelector(context.Background(), AccountSelector{AccountName: "Платинум"})
	res, err := parser.Parse(ctx, bytes.NewReader(rawBytes), "RUB")
	if err != nil {
		t.Fatalf("failed to parse: %v", err)
	}

	// Find transaction with cashback/bonus (Row 4: Яндекс Маркет, Amount: -3855.00, Bonus: 38.00)
	var ymRow *StagedRowDTO
	for i := range res.Rows {
		if strings.Contains(res.Rows[i].RawDescription, "Яндекс Маркет") {
			ymRow = &res.Rows[i]
			break
		}
	}

	if ymRow == nil {
		t.Fatalf("expected Яндекс Маркет row not found")
	}

	// Must be exactly -385500 minor units. Bonus (38.00) must NEVER alter transaction amount.
	if ymRow.AmountMinor != -385500 || ymRow.AccountAmountMinor != -385500 {
		t.Errorf("expected -385500 minor units, got amount=%d accAmount=%d", ymRow.AmountMinor, ymRow.AccountAmountMinor)
	}
}

// 22. Refund classification ambiguity & analytics flag:
func TestTBankCSV_RefundClassificationAmbiguity(t *testing.T) {
	parser := NewTBankCSVParser()
	rawBytes := loadFixture(t, "tbank_operations_valid.csv")

	ctx := WithAccountSelector(context.Background(), AccountSelector{AccountName: "Платинум"})
	res, err := parser.Parse(ctx, bytes.NewReader(rawBytes), "RUB")
	if err != nil {
		t.Fatalf("failed to parse: %v", err)
	}

	// Find the refund pair for 3050.00 RUB at Яндекс Еда (26.11.2026)
	var refundRow *StagedRowDTO
	for i := range res.Rows {
		if res.Rows[i].AccountAmountMinor == 305000 && strings.Contains(res.Rows[i].RawDescription, "Яндекс Еда") {
			refundRow = &res.Rows[i]
			break
		}
	}

	if refundRow == nil {
		t.Fatalf("expected refund row of 3050.00 not found")
	}

	// Confirmed refund must have operation_type = 'refund' and positive amount
	if refundRow.OperationType != "refund" {
		t.Errorf("expected operation_type = 'refund', got %q", refundRow.OperationType)
	}
	if refundRow.AccountAmountMinor <= 0 {
		t.Errorf("expected positive refund amount, got %d", refundRow.AccountAmountMinor)
	}
}

// 23. Transfer candidate vs confirmed internal transfer:
func TestTBankCSV_TransferCandidateVsConfirmed(t *testing.T) {
	parser := NewTBankCSVParser()
	rawBytes := loadFixture(t, "tbank_operations_valid.csv")

	ctx := WithAccountSelector(context.Background(), AccountSelector{AccountName: "Платинум"})
	res, err := parser.Parse(ctx, bytes.NewReader(rawBytes), "RUB")
	if err != nil {
		t.Fatalf("failed to parse: %v", err)
	}

	// Row 05.11.2026 19:28:14: "Между своими счетами", 158997.66 RUB
	var transferRow *StagedRowDTO
	for i := range res.Rows {
		if strings.Contains(res.Rows[i].RawDescription, "Между своими счетами") {
			transferRow = &res.Rows[i]
			break
		}
	}

	if transferRow == nil {
		t.Fatalf("transfer row 'Между своими счетами' not found")
	}

	// The parser classifies as candidate 'transfer_in' based on bank facts,
	// preserving original sign and leaving internal transfer pairing to the transfer matcher
	if transferRow.OperationType != "transfer_in" {
		t.Errorf("expected operation_type = 'transfer_in', got %q", transferRow.OperationType)
	}
	if transferRow.AccountAmountMinor != 15899766 {
		t.Errorf("expected 15899766 minor units, got %d", transferRow.AccountAmountMinor)
	}
}

// 24. Integration Test: Cancelled rows cannot reach ledger promotion:
func TestIntegration_CancelledRowsCannotReachLedgerPromotion(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()

	ctx := context.Background()
	familyID, userID, accountID := setupStatementTenantFixture(t)
	registry := NewDefaultParserRegistry()
	svc := NewService(appPool, testCryptoKey, registry)

	rawBytes := loadFixture(t, "tbank_operations_valid.csv")

	// Set account name to "Black" in DB so auto-selector matches Black (which has 4 cancelled ops)
	err := database.WithFamilyUserTx(ctx, appPool, familyID, userID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "UPDATE public.accounts SET name = 'Black' WHERE id = $1", accountID)
		return err
	})
	if err != nil {
		t.Fatalf("failed to update account name: %v", err)
	}

	batch, err := svc.UploadAndStage(
		ctx, familyID, userID, accountID,
		"Operations_Black.csv",
		bytes.NewReader(rawBytes),
		int64(len(rawBytes)),
		nil, // auto-sniff tbank_csv_v1
	)
	if err != nil {
		t.Fatalf("UploadAndStage failed: %v", err)
	}

	// Verify staged count is 32 (28 posted + 4 cancelled)
	if batch.StagedCount != 32 {
		t.Fatalf("expected 32 staged rows for Black, got %d", batch.StagedCount)
	}

	// Promote the batch
	res, err := svc.PromoteBatch(ctx, familyID, userID, batch.ID)
	if err != nil {
		t.Fatalf("PromoteBatch failed: %v", err)
	}

	// Verify that exactly 28 transactions were committed to ledger, and 4 cancelled rows were skipped!
	if res.TransactionsCommitted != 28 {
		t.Errorf("expected 28 committed transactions, got %d", res.TransactionsCommitted)
	}
	if res.TransactionsSkipped != 4 {
		t.Errorf("expected 4 skipped transactions (cancelled), got %d", res.TransactionsSkipped)
	}

	// Query DB to prove 0 cancelled transactions exist in bank_transactions or accounting_entries
	err = database.WithFamilyUserTx(ctx, appPool, familyID, userID, func(tx pgx.Tx) error {
		var btCancelledCount int
		err := tx.QueryRow(ctx, `
			SELECT count(*) FROM public.bank_transactions
			WHERE family_id = $1 AND account_id = $2 AND booking_status = 'cancelled'
		`, familyID, accountID).Scan(&btCancelledCount)
		if err != nil {
			return err
		}
		if btCancelledCount != 0 {
			t.Errorf("CRITICAL: found %d cancelled transactions in bank_transactions ledger table!", btCancelledCount)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verification failed: %v", err)
	}
}

// 25. Integration Test: Pending rows follow the existing pending lifecycle:
func TestIntegration_PendingRowsFollowLifecycle(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()

	ctx := context.Background()
	familyID, userID, accountID := setupStatementTenantFixture(t)
	registry := NewDefaultParserRegistry()
	svc := NewService(appPool, testCryptoKey, registry)

	pendingBytes := loadFixture(t, "tbank_operations_pending.csv")

	// Set account name to "Платинум" in DB
	err := database.WithFamilyUserTx(ctx, appPool, familyID, userID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "UPDATE public.accounts SET name = 'Платинум' WHERE id = $1", accountID)
		return err
	})
	if err != nil {
		t.Fatalf("failed to update account name: %v", err)
	}

	batch, err := svc.UploadAndStage(
		ctx, familyID, userID, accountID,
		"Operations_Pending.csv",
		bytes.NewReader(pendingBytes),
		int64(len(pendingBytes)),
		nil,
	)
	if err != nil {
		t.Fatalf("UploadAndStage failed: %v", err)
	}

	// Promote the pending batch
	res, err := svc.PromoteBatch(ctx, familyID, userID, batch.ID)
	if err != nil {
		t.Fatalf("PromoteBatch failed: %v", err)
	}
	if res.TransactionsCommitted != 1 {
		t.Fatalf("expected 1 committed transaction, got %d", res.TransactionsCommitted)
	}

	// Verify that bank_transactions contains booking_status = 'pending' and posted_time = nil
	var txID uuid.UUID
	err = database.WithFamilyUserTx(ctx, appPool, familyID, userID, func(tx pgx.Tx) error {
		var status string
		var postedTime *time.Time
		err := tx.QueryRow(ctx, `
			SELECT id, booking_status, posted_time
			FROM public.bank_transactions
			WHERE family_id = $1 AND account_id = $2
			LIMIT 1
		`, familyID, accountID).Scan(&txID, &status, &postedTime)
		if err != nil {
			return err
		}
		if status != "pending" {
			t.Errorf("expected booking_status = 'pending', got %q", status)
		}
		if postedTime != nil {
			t.Errorf("expected posted_time = nil for pending hold, got %v", postedTime)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("pending transaction verification failed: %v", err)
	}

	// Subsequent settlement: settle_pending_bank_transaction transitions it to posted!
	settledTime := time.Now().UTC()
	err = database.WithFamilyUserTx(ctx, appPool, familyID, userID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			SELECT public.settle_pending_bank_transaction($1, $2, $3, $4, -90000, 'settled_ext_01')
		`, familyID, accountID, txID, settledTime)
		return err
	})
	if err != nil {
		t.Fatalf("settle_pending_bank_transaction failed: %v", err)
	}

	// Verify status is now 'posted' with posted_time set
	err = database.WithFamilyUserTx(ctx, appPool, familyID, userID, func(tx pgx.Tx) error {
		var status string
		var pt time.Time
		err := tx.QueryRow(ctx, `
			SELECT booking_status, posted_time
			FROM public.bank_transactions
			WHERE family_id = $1 AND id = $2
		`, familyID, txID).Scan(&status, &pt)
		if err != nil {
			return err
		}
		if status != "posted" {
			t.Errorf("expected status = 'posted' after settlement, got %q", status)
		}
		if pt.IsZero() {
			t.Errorf("expected non-zero posted_time after settlement")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("settlement verification failed: %v", err)
	}
}

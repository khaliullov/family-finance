package statement

import (
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/khaliullov/family-finance/internal/domain"
	"golang.org/x/text/encoding/charmap"
)

const (
	TBankCSVFormatID      = "tbank_csv_v1"
	TBankCSVParserVersion = "v1.0.0"
	MaxTBankCSVInputSize  = 20 * 1024 * 1024 // 20 MiB
	MaxTBankCSVRecordSize = 64 * 1024        // 64 KiB
	MaxTBankCSVRowCount   = 50000
)

var (
	tbankCardMaskRegex = regexp.MustCompile(`^\*[0-9]{4}$`)
	mskTimezone        = time.FixedZone("MSK", 3*3600) // Europe/Moscow (UTC+3)
)

// TBankCSVParser parses official T-Bank (Tinkoff) Operations CSV exports.
type TBankCSVParser struct {
	maxInputSize  int64
	maxRecordSize int
	maxRows       int
}

// NewTBankCSVParser returns a production-configured T-Bank CSV statement parser.
func NewTBankCSVParser() *TBankCSVParser {
	return &TBankCSVParser{
		maxInputSize:  MaxTBankCSVInputSize,
		maxRecordSize: MaxTBankCSVRecordSize,
		maxRows:       MaxTBankCSVRowCount,
	}
}

// Profile returns format metadata for registration and discovery.
func (p *TBankCSVParser) Profile() StatementProfile {
	return StatementProfile{
		FormatID:       TBankCSVFormatID,
		DisplayName:    "T-Bank Operations CSV",
		FileExtensions: []string{".csv"},
		MIMETypes:      []string{"text/csv", "application/csv", "application/vnd.ms-excel"},
	}
}

// Sniff checks whether headerChunk matches a T-Bank Operations CSV export.
func (p *TBankCSVParser) Sniff(headerChunk []byte) bool {
	if len(headerChunk) == 0 {
		return false
	}

	raw := headerChunk
	// Strip UTF-8 BOM if present
	if bytes.HasPrefix(raw, []byte("\xef\xbb\xbf")) {
		raw = raw[3:]
	}

	trimmed := raw
	for i := 0; i < 4 && len(trimmed) > 0; i++ {
		if utf8.Valid(trimmed) {
			break
		}
		trimmed = trimmed[:len(trimmed)-1]
	}

	var text string
	if utf8.Valid(trimmed) {
		text = string(trimmed)
	} else {
		// Attempt Windows-1251 decoding
		decoded, err := charmap.Windows1251.NewDecoder().Bytes(raw)
		if err == nil {
			text = string(decoded)
		} else {
			return false
		}
	}

	// Must contain semicolon delimiter and key signature columns
	if !strings.Contains(text, ";") {
		return false
	}

	hasAccount := strings.Contains(text, "Имя счёта") || strings.Contains(text, "Имя счета")
	hasDate := strings.Contains(text, "Дата операции")
	hasAmount := strings.Contains(text, "Сумма операции")

	return hasAccount && hasDate && hasAmount
}

// Parse extracts normalized StagedRowDTO entries from a T-Bank Operations CSV reader.
func (p *TBankCSVParser) Parse(ctx context.Context, r io.Reader, targetCurrency string) (*ParsedStatementDTO, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// 1. Peek initial bytes to detect encoding (UTF-8 with BOM, UTF-8, or Windows-1251)
	bufR := bufio.NewReader(r)
	peekBytes, err := bufR.Peek(4096)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("failed to read header chunk: %w", err)
	}

	var decodedReader io.Reader
	if bytes.HasPrefix(peekBytes, []byte("\xef\xbb\xbf")) {
		// Discard 3-byte UTF-8 BOM
		if _, err := bufR.Discard(3); err != nil {
			return nil, fmt.Errorf("failed to discard UTF-8 BOM: %w", err)
		}
		decodedReader = bufR
	} else if utf8.Valid(peekBytes) {
		decodedReader = bufR
	} else {
		// Test Windows-1251
		decodedPeek, decErr := charmap.Windows1251.NewDecoder().Bytes(peekBytes)
		if decErr == nil && (strings.Contains(string(decodedPeek), "Имя счёта") || strings.Contains(string(decodedPeek), "Имя счета")) {
			decodedReader = charmap.Windows1251.NewDecoder().Reader(bufR)
		} else {
			return nil, fmt.Errorf("%w: input is neither valid UTF-8 nor Windows-1251", ErrUnsupportedEncoding)
		}
	}

	// 2. Enforce maximum CSV record size (64 KiB) and maximum input size
	recLimiter := newRecordSizeLimiter(decodedReader, p.maxRecordSize, p.maxInputSize)

	// 3. Configure CSV Reader: semicolon delimiter, strict quotes
	csvReader := csv.NewReader(recLimiter)
	csvReader.Comma = ';'
	csvReader.ReuseRecord = true
	csvReader.LazyQuotes = false

	// 4. Read and validate header row
	header, err := csvReader.Read()
	if err != nil {
		if errors.Is(err, ErrRecordTooLarge) {
			return nil, ErrRecordTooLarge
		}
		if errors.Is(err, ErrFileTooLarge) {
			return nil, ErrFileTooLarge
		}
		if errors.Is(err, csv.ErrQuote) || errors.Is(err, csv.ErrBareQuote) || strings.Contains(err.Error(), "quote") {
			return nil, fmt.Errorf("%w: %v", ErrMalformedCSVQuoting, err)
		}
		if errors.Is(err, io.EOF) {
			return &ParsedStatementDTO{
				FormatID:        TBankCSVFormatID,
				ParserVersion:   TBankCSVParserVersion,
				AccountCurrency: targetCurrency,
				Rows:            []StagedRowDTO{},
			}, nil
		}
		return nil, fmt.Errorf("failed to read CSV header: %w", err)
	}

	colIdxs, err := p.resolveColumns(header)
	if err != nil {
		return nil, err
	}

	// 6. Query account selector from context
	selector, hasSelector := AccountSelectorFromContext(ctx)

	discoveredAccountsMap := make(map[string]int)
	unmatchedAccountsMap := make(map[string]int)
	var rows []StagedRowDTO
	var periodStart, periodEnd *time.Time
	rowCount := 0
	unmatchedCount := 0

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		record, err := csvReader.Read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			if errors.Is(err, ErrRecordTooLarge) {
				return nil, ErrRecordTooLarge
			}
			if errors.Is(err, ErrFileTooLarge) {
				return nil, ErrFileTooLarge
			}
			if errors.Is(err, csv.ErrQuote) || errors.Is(err, csv.ErrBareQuote) || strings.Contains(err.Error(), "quote") {
				return nil, fmt.Errorf("%w: at row %d: %v", ErrMalformedCSVQuoting, rowCount+1, err)
			}
			return nil, fmt.Errorf("error reading row %d: %w", rowCount+1, err)
		}

		rowCount++
		if rowCount > p.maxRows {
			return nil, ErrTooManyRows
		}

		// Ensure record has sufficient fields
		if len(record) <= colIdxs.maxRequiredIdx {
			return nil, fmt.Errorf("row %d: incomplete record (got %d fields, required %d)", rowCount, len(record), colIdxs.maxRequiredIdx+1)
		}

		var rowAccountName string
		if colIdxs.accountName >= 0 && colIdxs.accountName < len(record) {
			rowAccountName = strings.TrimSpace(record[colIdxs.accountName])
		} else if hasSelector && selector.AccountName != "" {
			rowAccountName = selector.AccountName
		} else {
			rowAccountName = "Default"
		}
		discoveredAccountsMap[rowAccountName]++

		// Filter by AccountSelector if present
		if hasSelector && selector.AccountName != "" {
			var matches bool
			if colIdxs.accountName >= 0 && colIdxs.accountName < len(record) {
				// Authoritative match by account name
				matches = strings.EqualFold(rowAccountName, selector.AccountName)
			} else {
				// Legacy export without account name column: match by card mask if present
				if len(selector.CardMasks) > 0 && colIdxs.cardMask >= 0 && colIdxs.cardMask < len(record) {
					cardMaskVal := strings.TrimSpace(record[colIdxs.cardMask])
					if cardMaskVal != "" {
						for _, cm := range selector.CardMasks {
							if strings.EqualFold(cardMaskVal, cm) {
								matches = true
								break
							}
						}
					} else {
						matches = true
					}
				} else {
					matches = true
				}
			}

			// Cross-check Currency if selector specifies it
			if matches && selector.Currency != "" && colIdxs.accCurrency >= 0 && colIdxs.accCurrency < len(record) {
				rowCurrency := strings.ToUpper(strings.TrimSpace(record[colIdxs.accCurrency]))
				if rowCurrency != "" && !strings.EqualFold(rowCurrency, selector.Currency) {
					matches = false
				}
			}

			if !matches {
				unmatchedAccountsMap[rowAccountName]++
				unmatchedCount++
				continue
			}
		}

		dto, err := p.parseRow(record, colIdxs, rowCount, targetCurrency)
		if err != nil {
			return nil, err
		}

		if periodStart == nil || dto.TransactionTime.Before(*periodStart) {
			t := dto.TransactionTime
			periodStart = &t
		}
		if periodEnd == nil || dto.TransactionTime.After(*periodEnd) {
			t := dto.TransactionTime
			periodEnd = &t
		}

		rows = append(rows, *dto)
	}

	// 7. Check if stream exceeded input size limit
	if recLimiter.totalBytesRead > p.maxInputSize {
		return nil, ErrFileTooLarge
	}

	discoveredList := make([]string, 0, len(discoveredAccountsMap))
	for acc := range discoveredAccountsMap {
		discoveredList = append(discoveredList, acc)
	}

	// 8. Account ambiguity validation
	if !hasSelector || selector.AccountName == "" {
		if len(discoveredAccountsMap) > 1 {
			return nil, fmt.Errorf("%w: statement contains operations for multiple accounts (%s); explicit account selector required",
				ErrMultiAccountAmbiguity, strings.Join(discoveredList, ", "))
		}
	} else {
		if len(rows) == 0 && rowCount > 0 {
			return nil, fmt.Errorf("%w: target account %q not found in statement accounts (%s)",
				ErrAccountNotFoundInStatement, selector.AccountName, strings.Join(discoveredList, ", "))
		}
	}

	return &ParsedStatementDTO{
		FormatID:            TBankCSVFormatID,
		ParserVersion:       TBankCSVParserVersion,
		PeriodStartDate:     periodStart,
		PeriodEndDate:       periodEnd,
		OpeningBalanceMinor: nil, // Operations exports do not include period balance totals
		ClosingBalanceMinor: nil,
		AccountCurrency:     targetCurrency,
		Rows:                rows,
		IsReconcilable:      false,
		DiscoveredAccounts:  discoveredList,
		UnmatchedRowsCount:  unmatchedCount,
		UnmatchedAccounts:   unmatchedAccountsMap,
	}, nil
}

type tbankColIndices struct {
	accountName    int
	cardMask       int
	txDate         int
	opAmount       int
	opCurrency     int
	accAmount      int
	accCurrency    int
	status         int
	category       int
	userCategory   int
	mcc            int
	description    int
	message        int
	rounding       int
	roundedAmount  int
	bonuses        int
	analytics      int
	maxRequiredIdx int
}

func (p *TBankCSVParser) resolveColumns(header []string) (*tbankColIndices, error) {
	idxs := &tbankColIndices{
		accountName:    -1,
		cardMask:       -1,
		txDate:         -1,
		opAmount:       -1,
		opCurrency:     -1,
		accAmount:      -1,
		accCurrency:    -1,
		status:         -1,
		category:       -1,
		userCategory:   -1,
		mcc:            -1,
		description:    -1,
		message:        -1,
		rounding:       -1,
		roundedAmount:  -1,
		bonuses:        -1,
		analytics:      -1,
		maxRequiredIdx: -1,
	}

	for i, col := range header {
		norm := strings.ToLower(strings.TrimSpace(col))
		switch norm {
		case "имя счёта", "имя счета":
			idxs.accountName = i
		case "номер карты":
			idxs.cardMask = i
		case "дата операции":
			idxs.txDate = i
		case "сумма операции":
			idxs.opAmount = i
		case "валюта операции":
			idxs.opCurrency = i
		case "сумма в валюте счёта", "сумма в валюте счета", "сумма платежа":
			idxs.accAmount = i
		case "валюта счёта", "валюта счета", "валюта платежа":
			idxs.accCurrency = i
		case "статус":
			idxs.status = i
		case "категория по-умолчанию", "категория":
			idxs.category = i
		case "ваша категория":
			idxs.userCategory = i
		case "mcc":
			idxs.mcc = i
		case "описание":
			idxs.description = i
		case "сообщение":
			idxs.message = i
		case "округление", "округление на инвесткопилку":
			idxs.rounding = i
		case "сумма операции с округлением":
			idxs.roundedAmount = i
		case "бонусы (включая кэшбэк)", "бонусы", "кэшбэк", "кешбэк":
			idxs.bonuses = i
		case "учёт в аналитике", "учет в аналитике":
			idxs.analytics = i
		}
	}

	// Validate mandatory columns
	required := map[string]int{
		"Дата операции":        idxs.txDate,
		"Сумма операции":       idxs.opAmount,
		"Валюта операции":      idxs.opCurrency,
		"Сумма в валюте счёта": idxs.accAmount,
		"Валюта счёта":         idxs.accCurrency,
		"Статус":               idxs.status,
		"Описание":             idxs.description,
	}

	for name, idx := range required {
		if idx < 0 {
			return nil, fmt.Errorf("%w: %s", ErrMissingRequiredColumn, name)
		}
		if idx > idxs.maxRequiredIdx {
			idxs.maxRequiredIdx = idx
		}
	}
	if idxs.accountName > idxs.maxRequiredIdx {
		idxs.maxRequiredIdx = idxs.accountName
	}

	return idxs, nil
}

func (p *TBankCSVParser) parseRow(record []string, idxs *tbankColIndices, rowIndex int, targetCurrency string) (*StagedRowDTO, error) {
	// 1. Parse Timestamp: "DD.MM.YYYY HH:MM:SS" in Europe/Moscow (UTC+3)
	dateStr := strings.TrimSpace(record[idxs.txDate])
	t, err := time.ParseInLocation("02.01.2006 15:04:05", dateStr, mskTimezone)
	if err != nil {
		return nil, fmt.Errorf("row %d: %w: %q (%v)", rowIndex, ErrMalformedDate, dateStr, err)
	}
	txTimeUTC := t.UTC()

	// 2. Currencies
	opCur := strings.ToUpper(strings.TrimSpace(record[idxs.opCurrency]))
	if err := domain.ValidateCurrency(opCur); err != nil {
		return nil, fmt.Errorf("row %d: invalid operation currency %q: %w", rowIndex, opCur, err)
	}
	accCur := strings.ToUpper(strings.TrimSpace(record[idxs.accCurrency]))
	if err := domain.ValidateCurrency(accCur); err != nil {
		return nil, fmt.Errorf("row %d: invalid account currency %q: %w", rowIndex, accCur, err)
	}

	if targetCurrency != "" && accCur != strings.ToUpper(strings.TrimSpace(targetCurrency)) {
		return nil, fmt.Errorf("row %d: row account currency %q does not match target account currency %q",
			rowIndex, accCur, targetCurrency)
	}

	// 3. Monetary Amounts (integer minor units without floating-point arithmetic)
	cleanOpStr := cleanAmountString(record[idxs.opAmount])
	cleanAccStr := cleanAmountString(record[idxs.accAmount])

	opMoney, err := domain.ParseMoney(cleanOpStr, opCur)
	if err != nil {
		return nil, fmt.Errorf("row %d: %w: operation amount %q: %v", rowIndex, domain.ErrInvalidAmount, record[idxs.opAmount], err)
	}
	accMoney, err := domain.ParseMoney(cleanAccStr, accCur)
	if err != nil {
		return nil, fmt.Errorf("row %d: %w: account amount %q: %v", rowIndex, domain.ErrInvalidAmount, record[idxs.accAmount], err)
	}

	// 4. Booking Status
	statusRaw := strings.ToLower(strings.TrimSpace(record[idxs.status]))
	var bookingStatus string
	switch statusRaw {
	case "ок", "ok":
		bookingStatus = "posted"
	case "в обработке", "waiting", "pending":
		bookingStatus = "pending"
	case "ошибка", "failed", "cancelled", "отклонена":
		bookingStatus = "cancelled"
	default:
		return nil, fmt.Errorf("row %d: %w: %q", rowIndex, ErrInvalidBookingStatus, record[idxs.status])
	}

	// 5. Card Mask
	var cardMaskPtr *string
	if idxs.cardMask >= 0 && idxs.cardMask < len(record) {
		mask := strings.TrimSpace(record[idxs.cardMask])
		if mask != "" {
			if tbankCardMaskRegex.MatchString(mask) {
				cardMaskPtr = &mask
			} else {
				trimmed := mask
				cardMaskPtr = &trimmed
			}
		}
	}

	// 6. MCC
	var mccPtr *int
	if idxs.mcc >= 0 && idxs.mcc < len(record) {
		mccStr := strings.TrimSpace(record[idxs.mcc])
		if mccStr != "" {
			if val, err := strconv.Atoi(mccStr); err == nil && val > 0 {
				mccPtr = &val
			}
		}
	}

	// 7. Descriptions, Categories, Messages
	rawDesc := strings.TrimSpace(record[idxs.description])
	if rawDesc == "" {
		return nil, fmt.Errorf("row %d: description cannot be empty", rowIndex)
	}

	if idxs.message >= 0 && idxs.message < len(record) {
		msg := strings.TrimSpace(record[idxs.message])
		if msg != "" {
			rawDesc = rawDesc + " - " + msg
		}
	}

	var categoryPtr *string
	categoryVal := ""
	if idxs.category >= 0 && idxs.category < len(record) {
		categoryVal = strings.TrimSpace(record[idxs.category])
		if categoryVal != "" {
			categoryPtr = &categoryVal
		}
	}

	analyticsFlag := "Да"
	if idxs.analytics >= 0 && idxs.analytics < len(record) {
		flag := strings.TrimSpace(record[idxs.analytics])
		if flag != "" {
			analyticsFlag = flag
		}
	}

	// 8. Operation Classification
	opType := ClassifyTBankOperation(categoryVal, rawDesc, accMoney.AmountMinor(), analyticsFlag)

	var merchantNamePtr *string
	if opType == "purchase" || opType == "refund" {
		mName := strings.TrimSpace(record[idxs.description])
		merchantNamePtr = &mName
	}

	var settledTime *time.Time
	if bookingStatus == "posted" {
		settledTime = &txTimeUTC
	}

	return &StagedRowDTO{
		RowIndex:           rowIndex,
		SourceExternalID:   nil, // T-Bank CSV does not provide bank transaction IDs
		CardMask:           cardMaskPtr,
		TransactionTime:    txTimeUTC,
		SettledTime:        settledTime,
		AmountMinor:        opMoney.AmountMinor(),
		Currency:           opCur,
		AccountAmountMinor: accMoney.AmountMinor(),
		AccountCurrency:    accCur,
		OperationType:      opType,
		BookingStatus:      bookingStatus,
		RawDescription:     rawDesc,
		MerchantName:       merchantNamePtr,
		MCC:                mccPtr,
		BankCategory:       categoryPtr,
	}, nil
}

// ClassifyTBankOperation maps T-Bank category, narrative, and amount to the project's canonical operation_type.
func ClassifyTBankOperation(category, description string, amountMinor int64, analyticsFlag string) string {
	if amountMinor == 0 {
		return "card_check"
	}

	catLower := strings.ToLower(strings.TrimSpace(category))
	descLower := strings.ToLower(strings.TrimSpace(description))

	// Transfers
	if catLower == "переводы" {
		if amountMinor < 0 {
			return "transfer_out"
		}
		return "transfer_in"
	}

	// Cash withdrawals / ATM deposits
	if catLower == "наличные" || strings.Contains(descLower, "банкомат") || strings.Contains(descLower, "atm ") {
		return "atm_cash"
	}

	// Fees
	if catLower == "комиссии" || strings.Contains(descLower, "плата за обслуживание") || strings.Contains(descLower, "комиссия") {
		return "fee"
	}

	// Rewards and Interest
	if catLower == "бонусы" || catLower == "проценты" || strings.Contains(descLower, "проценты на остаток") || strings.Contains(descLower, "кэшбэк") {
		return "transfer_in"
	}

	// Positive amounts (credits/refunds)
	if amountMinor > 0 {
		// 1. Explicit refund narrative evidence
		if strings.Contains(descLower, "возврат") || strings.Contains(descLower, "отмена") || strings.Contains(descLower, "reversal") {
			return "refund"
		}
		if catLower == "пополнения" {
			return "transfer_in"
		}

		// 2. Retail merchant charge reversal: positive amount on a retail spending category with analytics excluded
		// Analytics flag alone CANNOT establish a refund if the category is generic/unknown/financial.
		isRetailCategory := catLower != "" && catLower != "другое" && catLower != "переводы" &&
			catLower != "пополнения" && catLower != "бонусы" && catLower != "проценты" &&
			catLower != "финансы" && catLower != "инвестиции"
		if isRetailCategory && analyticsFlag == "Нет" {
			return "refund"
		}

		return "transfer_in"
	}

	// Default debits: retail/merchant purchases
	return "purchase"
}

// cleanAmountString removes non-breaking spaces and spaces, and converts comma to dot.
func cleanAmountString(s string) string {
	cleaned := strings.TrimSpace(s)
	cleaned = strings.ReplaceAll(cleaned, " ", "")
	cleaned = strings.ReplaceAll(cleaned, "\u00a0", "")
	cleaned = strings.ReplaceAll(cleaned, ",", ".")
	return cleaned
}

// recordSizeLimiter wraps an io.Reader and returns ErrRecordTooLarge if any CSV record exceeds maxBytes,
// or ErrFileTooLarge if total bytes read exceeds maxTotalBytes.
type recordSizeLimiter struct {
	r              io.Reader
	maxBytes       int
	maxTotalBytes  int64
	currentBytes   int
	totalBytesRead int64
	inQuotes       bool
}

func newRecordSizeLimiter(r io.Reader, maxBytes int, maxTotalBytes int64) *recordSizeLimiter {
	return &recordSizeLimiter{
		r:             r,
		maxBytes:      maxBytes,
		maxTotalBytes: maxTotalBytes,
	}
}

func (l *recordSizeLimiter) Read(p []byte) (int, error) {
	n, err := l.r.Read(p)
	if n > 0 {
		l.totalBytesRead += int64(n)
		if l.maxTotalBytes > 0 && l.totalBytesRead > l.maxTotalBytes {
			return 0, ErrFileTooLarge
		}
		for i := 0; i < n; i++ {
			b := p[i]
			l.currentBytes++
			if l.currentBytes > l.maxBytes {
				return i, ErrRecordTooLarge
			}

			if b == '"' {
				l.inQuotes = !l.inQuotes
			} else if !l.inQuotes && b == '\n' {
				l.currentBytes = 0
			}
		}
	}
	return n, err
}

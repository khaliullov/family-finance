package personal

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/khaliullov/family-finance/internal/connector"
	"github.com/khaliullov/family-finance/internal/domain"
)

// Supported ISO-4217 currency mappings for verified T-Bank retail accounts.
var supportedCurrencies = map[string]string{
	"643": "RUB", "810": "RUB", "RUB": "RUB",
	"840": "USD", "USD": "USD",
	"978": "EUR", "EUR": "EUR",
	"156": "CNY", "CNY": "CNY",
	"398": "KZT", "KZT": "KZT",
	"933": "BYN", "BYN": "BYN",
	"764": "THB", "THB": "THB",
	"784": "AED", "AED": "AED",
	"949": "TRY", "TRY": "TRY",
}

// ParseMinorUnits parses a decimal json.Number into integer minor units without float arithmetic,
// determining the minor-unit exponent from the validated currency definition.
// Rejects unsupported precision rather than rounding or truncating silently.
// Detects int64 overflow, including the MinInt64 boundary, and handles negative values correctly.
func ParseMinorUnits(num json.Number, currency string) (int64, error) {
	str := strings.TrimSpace(num.String())
	if str == "" {
		return 0, errors.New("missing monetary amount value")
	}
	if strings.Contains(str, ",") {
		return 0, fmt.Errorf("invalid decimal format: comma separator not allowed in json.Number %q", num.String())
	}

	m, err := domain.ParseMoney(str, currency)
	if err != nil {
		return 0, err
	}
	return m.AmountMinor(), nil
}

// NormalizeCurrencyCode normalizes ISO-4217 numeric/string codes.
// Strictly fails closed if currency is missing, unknown, or unsupported (never defaults to RUB).
func NormalizeCurrencyCode(curr TBankCurrency) (string, error) {
	strCode := strings.ToUpper(strings.TrimSpace(curr.StrCode))
	if canon, ok := supportedCurrencies[strCode]; ok {
		return canon, nil
	}

	name := strings.ToUpper(strings.TrimSpace(curr.Name))
	if canon, ok := supportedCurrencies[name]; ok {
		return canon, nil
	}

	if curr.Code > 0 {
		codeStr := strconv.Itoa(curr.Code)
		if canon, ok := supportedCurrencies[codeStr]; ok {
			return canon, nil
		}
		return "", fmt.Errorf("unsupported ISO-4217 currency code: %d", curr.Code)
	}

	if strCode != "" {
		return "", fmt.Errorf("unsupported currency: %q", strCode)
	}
	if name != "" {
		return "", fmt.Errorf("unsupported currency: %q", name)
	}

	return "", errors.New("missing currency code")
}

// ClassifyOperation conservatively classifies a transaction into a canonical operation_type.
// Fees must never be inferred solely from positive feeAmount without validating bank sign semantics.
// Salary, cash-in, and unknown credits map to the existing safe equivalent "transfer_in"
// adhering to the database check constraint on bank_transactions.operation_type.
// Unknown debits map to the existing safe equivalent "purchase".
func ClassifyOperation(op TBankOperationItem, amountMinor int64) string {
	rawDesc := strings.ToLower(strings.TrimSpace(op.Description))
	catName := ""
	if op.SpendingCategory != nil {
		catName = strings.ToLower(strings.TrimSpace(op.SpendingCategory.Name))
	}
	subName := ""
	if op.Subgroup != nil {
		subName = strings.ToLower(strings.TrimSpace(op.Subgroup.Name))
	}

	// 1. Internal transfers between own accounts
	if op.IsInner {
		if amountMinor >= 0 {
			return "transfer_in"
		}
		return "transfer_out"
	}

	// 2. Inflow / Credit transactions
	if amountMinor > 0 {
		// Explicitly identified merchant refund
		if strings.Contains(rawDesc, "возврат") || strings.Contains(rawDesc, "refund") ||
			strings.Contains(catName, "возврат") || strings.Contains(subName, "возврат") {
			return "refund"
		}

		// Salary, cash-in, explicit transfers, and unclassified credits all map to
		// the existing safe schema equivalent "transfer_in".
		return "transfer_in"
	}

	// 3. Outflow / Debit transactions
	if amountMinor < 0 {
		// Outgoing transfer
		if strings.Contains(rawDesc, "перевод") || strings.Contains(rawDesc, "сбп") ||
			strings.Contains(rawDesc, "transfer") || strings.Contains(catName, "перевод") ||
			strings.Contains(subName, "перевод") {
			return "transfer_out"
		}

		// ATM cash withdrawal
		if strings.Contains(rawDesc, "снятие") || strings.Contains(rawDesc, "банкомат") ||
			strings.Contains(rawDesc, "выдача наличных") || strings.Contains(rawDesc, "atm") ||
			strings.Contains(catName, "снятие") || strings.Contains(catName, "наличные") {
			return "atm_cash"
		}

		// Bank fee: only if explicit in description/category; never solely from feeAmount
		if strings.Contains(rawDesc, "комиссия") || strings.Contains(rawDesc, "плата за") ||
			strings.Contains(catName, "комиссия") || strings.Contains(subName, "комиссия") {
			return "fee"
		}

		// Card purchases and unclassified debits map to the existing safe schema equivalent "purchase".
		return "purchase"
	}

	// Zero amount (e.g. card verification)
	return "card_check"
}

// MapOperationToCanonical converts a raw TBankOperationItem into a canonical transaction event.
// Fails closed on unknown operation statuses or malformed records.
func MapOperationToCanonical(op TBankOperationItem) (*connector.CanonicalBankTransaction, error) {
	if op.ID == "" {
		return nil, errors.New("missing operation external ID")
	}

	amountCurrency, err := NormalizeCurrencyCode(op.Amount.Currency)
	if err != nil {
		return nil, fmt.Errorf("invalid amount currency for op %s: %w", op.ID, err)
	}
	accountCurrency, err := NormalizeCurrencyCode(op.AccountAmount.Currency)
	if err != nil {
		return nil, fmt.Errorf("invalid accountAmount currency for op %s: %w", op.ID, err)
	}

	amountMinor, err := ParseMinorUnits(op.Amount.Value, amountCurrency)
	if err != nil {
		return nil, fmt.Errorf("parse amount for op %s: %w", op.ID, err)
	}
	accountAmountMinor, err := ParseMinorUnits(op.AccountAmount.Value, accountCurrency)
	if err != nil {
		return nil, fmt.Errorf("parse accountAmount for op %s: %w", op.ID, err)
	}

	if op.OperationTime.Milliseconds == 0 {
		return nil, fmt.Errorf("missing operation time for op %s", op.ID)
	}

	txTime := time.UnixMilli(op.OperationTime.Milliseconds).UTC()
	var settledTime *time.Time
	if op.DebitingTime != nil && op.DebitingTime.Milliseconds > 0 {
		t := time.UnixMilli(op.DebitingTime.Milliseconds).UTC()
		settledTime = &t
	}

	// Map status strictly; unknown status must NEVER become posted
	var bookingStatus string
	switch strings.ToUpper(strings.TrimSpace(op.Status)) {
	case "WAITING", "PENDING":
		bookingStatus = "pending"
	case "OK", "SUCCESS":
		bookingStatus = "posted"
	case "FAILED", "CANCELLED", "REJECTED":
		bookingStatus = "cancelled"
	default:
		return nil, fmt.Errorf("unknown operation status %q for op %s (fail-closed)", op.Status, op.ID)
	}

	// Conservative operation classification
	opType := ClassifyOperation(op, amountMinor)

	var authIDPtr *string
	if op.AuthorizationID != "" {
		a := op.AuthorizationID
		authIDPtr = &a
	}

	var cardIDPtr *string
	if op.Card != "" {
		c := op.Card
		cardIDPtr = &c
	}

	var merchantName string
	if op.Merchant != nil && op.Merchant.Name != "" {
		merchantName = op.Merchant.Name
	} else {
		merchantName = op.Description
	}

	var bankCategory string
	if op.SpendingCategory != nil && op.SpendingCategory.Name != "" {
		bankCategory = op.SpendingCategory.Name
	}

	return &connector.CanonicalBankTransaction{
		ExternalID:        op.ID,
		ExternalAuthID:    authIDPtr,
		AccountExternalID: op.Account,
		CardExternalID:    cardIDPtr,
		Amount: connector.CurrencyAmount{
			MinorUnits: amountMinor,
			Currency:   amountCurrency,
		},
		AccountAmount: connector.CurrencyAmount{
			MinorUnits: accountAmountMinor,
			Currency:   accountCurrency,
		},
		TransactionTime: txTime,
		SettledTime:     settledTime,
		BookingStatus:   bookingStatus,
		OperationType:   opType,
		MerchantName:    merchantName,
		RawDescription:  op.Description,
		MCC:             op.MCC,
		BankCategory:    bankCategory,
	}, nil
}

// MapAccountItemToDTO converts a TBankAccountItem into a canonical AccountDTO with nested CardDTOs.
// Fails closed on missing or invalid currencies or malformed monetary amounts.
func MapAccountItemToDTO(item TBankAccountItem) (*connector.AccountDTO, error) {
	if item.ID == "" {
		return nil, errors.New("missing account external ID")
	}

	accType := "checking"
	switch strings.ToLower(strings.TrimSpace(item.AccountType)) {
	case "credit":
		accType = "credit_card"
	case "deposit":
		accType = "deposit"
	case "saving", "investbox":
		accType = "savings"
	default:
		accType = "checking"
	}

	var currency string
	var balance *connector.CurrencyAmount
	if item.MoneyAmount != nil {
		curr, err := NormalizeCurrencyCode(item.MoneyAmount.Currency)
		if err != nil {
			return nil, fmt.Errorf("invalid account currency for acc %s: %w", item.ID, err)
		}
		currency = curr

		minor, err := ParseMinorUnits(item.MoneyAmount.Value, currency)
		if err != nil {
			return nil, fmt.Errorf("invalid balance for acc %s: %w", item.ID, err)
		}
		balance = &connector.CurrencyAmount{
			MinorUnits: minor,
			Currency:   currency,
		}
	} else {
		return nil, fmt.Errorf("missing money amount for acc %s", item.ID)
	}

	name := item.Name
	if name == "" {
		name = fmt.Sprintf("T-Bank %s", item.ID)
	}

	dto := &connector.AccountDTO{
		ExternalID:       item.ID,
		Name:             name,
		Type:             accType,
		Currency:         currency,
		Balance:          balance,
		AvailableBalance: nil, // Do not fabricate available balance without explicit provider field
	}

	if item.Card != nil && item.Card.ID != "" {
		cardStatus := "ACTIVE"
		if strings.ToUpper(item.Card.Status) == "CLOSED" {
			cardStatus = "CLOSED"
		} else if strings.ToUpper(item.Card.Status) == "BLOCKED" {
			cardStatus = "BLOCKED"
		}

		brandName := ""
		if item.Card.Brand != nil && item.Card.Brand.Name != "" {
			brandName = strings.ToUpper(strings.TrimSpace(item.Card.Brand.Name))
		}

		cardDTO := connector.CardDTO{
			ExternalID:        item.Card.ID,
			AccountExternalID: item.ID,
			MaskedPAN:         item.Card.Name,
			DisplayName:       item.Card.Name,
			PaymentSystem:     brandName,
			IsPrimary:         item.Card.Primary,
			Status:            cardStatus,
		}
		dto.Cards = append(dto.Cards, cardDTO)
	}

	return dto, nil
}

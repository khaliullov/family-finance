package personal

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/khaliullov/family-finance/internal/connector"
)

// Supported ISO-4217 currency mappings.
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

// ParseMinorUnits parses a decimal json.Number into integer minor units (cents/kopecks) without float64 arithmetic.
// Rejects precision > 2 decimal places rather than silently rounding it (fail closed).
func ParseMinorUnits(num json.Number) (int64, error) {
	str := strings.TrimSpace(num.String())
	if str == "" {
		return 0, errors.New("missing monetary amount value")
	}
	if str == "0" {
		return 0, nil
	}

	isNegative := false
	if strings.HasPrefix(str, "-") {
		isNegative = true
		str = str[1:]
	} else if strings.HasPrefix(str, "+") {
		str = str[1:]
	}

	var wholePart, fracPart string
	parts := strings.Split(str, ".")
	if len(parts) == 1 {
		wholePart = parts[0]
		fracPart = "00"
	} else if len(parts) == 2 {
		wholePart = parts[0]
		frac := parts[1]
		if len(frac) == 0 {
			fracPart = "00"
		} else if len(frac) == 1 {
			fracPart = frac + "0"
		} else if len(frac) == 2 {
			fracPart = frac
		} else {
			// Fail closed on unsupported precision > 2 decimals
			return 0, fmt.Errorf("unsupported decimal precision (%d decimal places): %q", len(frac), num.String())
		}
	} else {
		return 0, fmt.Errorf("invalid decimal format: %q", num.String())
	}

	whole, err := strconv.ParseInt(wholePart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid whole part %q: %w", wholePart, err)
	}

	frac, err := strconv.ParseInt(fracPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid fractional part %q: %w", fracPart, err)
	}

	total := whole*100 + frac
	if isNegative {
		total = -total
	}

	return total, nil
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

// MapOperationToCanonical converts a raw TBankOperationItem into a canonical transaction event.
// Fails closed on unknown operation statuses or malformed records.
func MapOperationToCanonical(op TBankOperationItem) (*connector.CanonicalBankTransaction, error) {
	if op.ID == "" {
		return nil, errors.New("missing operation external ID")
	}

	amountMinor, err := ParseMinorUnits(op.Amount.Value)
	if err != nil {
		return nil, fmt.Errorf("parse amount for op %s: %w", op.ID, err)
	}
	accountAmountMinor, err := ParseMinorUnits(op.AccountAmount.Value)
	if err != nil {
		return nil, fmt.Errorf("parse accountAmount for op %s: %w", op.ID, err)
	}

	amountCurrency, err := NormalizeCurrencyCode(op.Amount.Currency)
	if err != nil {
		return nil, fmt.Errorf("invalid amount currency for op %s: %w", op.ID, err)
	}
	accountCurrency, err := NormalizeCurrencyCode(op.AccountAmount.Currency)
	if err != nil {
		return nil, fmt.Errorf("invalid accountAmount currency for op %s: %w", op.ID, err)
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

	// Map operation type
	opType := "purchase"
	rawType := strings.ToLower(strings.TrimSpace(op.Type))
	rawDesc := strings.ToLower(strings.TrimSpace(op.Description))

	if op.IsInner {
		if amountMinor >= 0 {
			opType = "transfer_in"
		} else {
			opType = "transfer_out"
		}
	} else if rawType == "credit" || rawType == "income" || strings.Contains(rawDesc, "возврат") || strings.Contains(rawDesc, "пополнение") {
		if strings.Contains(rawDesc, "возврат") {
			opType = "refund"
		} else {
			opType = "transfer_in"
		}
	} else if strings.Contains(rawDesc, "снятие") || strings.Contains(rawDesc, "банкомат") {
		opType = "atm_cash"
	} else if strings.Contains(rawDesc, "комиссия") || strings.Contains(rawDesc, "плата за") {
		opType = "fee"
	}

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

		minor, err := ParseMinorUnits(item.MoneyAmount.Value)
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

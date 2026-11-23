package personal

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/khaliullov/family-finance/internal/connector"
)

// ParseMinorUnits parses a decimal json.Number into integer minor units (cents/kopecks) without float64 arithmetic.
// Supports up to 2 decimal places (standard for RUB, USD, EUR).
func ParseMinorUnits(num json.Number) (int64, error) {
	str := strings.TrimSpace(num.String())
	if str == "" || str == "0" {
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
			// Round to 2 places based on 3rd digit
			digit3 := frac[2] - '0'
			frac2, _ := strconv.ParseInt(frac[:2], 10, 64)
			if digit3 >= 5 {
				frac2++
			}
			fracPart = fmt.Sprintf("%02d", frac2)
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

// NormalizeCurrencyCode normalizes ISO-4217 numeric/string codes (e.g. 643 or "RUB" -> "RUB").
func NormalizeCurrencyCode(curr TBankCurrency) string {
	strCode := strings.ToUpper(strings.TrimSpace(curr.StrCode))
	if strCode != "" {
		return strCode
	}
	name := strings.ToUpper(strings.TrimSpace(curr.Name))
	if name != "" {
		return name
	}
	switch curr.Code {
	case 643, 810:
		return "RUB"
	case 840:
		return "USD"
	case 978:
		return "EUR"
	default:
		if curr.Code > 0 {
			return strconv.Itoa(curr.Code)
		}
		return "RUB"
	}
}

// MapOperationToCanonical converts a raw TBankOperationItem into a canonical transaction event.
func MapOperationToCanonical(op TBankOperationItem) (*connector.CanonicalBankTransaction, error) {
	amountMinor, err := ParseMinorUnits(op.Amount.Value)
	if err != nil {
		return nil, fmt.Errorf("parse amount for op %s: %w", op.ID, err)
	}
	accountAmountMinor, err := ParseMinorUnits(op.AccountAmount.Value)
	if err != nil {
		return nil, fmt.Errorf("parse accountAmount for op %s: %w", op.ID, err)
	}

	amountCurrency := NormalizeCurrencyCode(op.Amount.Currency)
	accountCurrency := NormalizeCurrencyCode(op.AccountAmount.Currency)

	txTime := time.UnixMilli(op.OperationTime.Milliseconds).UTC()
	var settledTime *time.Time
	if op.DebitingTime != nil && op.DebitingTime.Milliseconds > 0 {
		t := time.UnixMilli(op.DebitingTime.Milliseconds).UTC()
		settledTime = &t
	}

	// Map status
	var bookingStatus string
	switch strings.ToUpper(strings.TrimSpace(op.Status)) {
	case "WAITING", "PENDING":
		bookingStatus = "pending"
	case "OK", "SUCCESS":
		bookingStatus = "posted"
	case "FAILED", "CANCELLED", "REJECTED":
		bookingStatus = "cancelled"
	default:
		bookingStatus = "posted"
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
func MapAccountItemToDTO(item TBankAccountItem) (*connector.AccountDTO, error) {
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
		currency = NormalizeCurrencyCode(item.MoneyAmount.Currency)
		minor, err := ParseMinorUnits(item.MoneyAmount.Value)
		if err == nil {
			balance = &connector.CurrencyAmount{
				MinorUnits: minor,
				Currency:   currency,
			}
		}
	}
	if currency == "" {
		currency = "RUB"
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
		AvailableBalance: balance,
	}

	if item.Card != nil && item.Card.ID != "" {
		cardStatus := "ACTIVE"
		if strings.ToUpper(item.Card.Status) == "CLOSED" {
			cardStatus = "CLOSED"
		} else if strings.ToUpper(item.Card.Status) == "BLOCKED" {
			cardStatus = "BLOCKED"
		}

		brandName := "MIR"
		if item.Card.Brand != nil && item.Card.Brand.Name != "" {
			brandName = strings.ToUpper(item.Card.Brand.Name)
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

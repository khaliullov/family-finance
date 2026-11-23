package domain

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

var (
	ErrCurrencyMismatch    = errors.New("currency mismatch between money values")
	ErrInvalidCurrency     = errors.New("invalid ISO 4217 currency code")
	ErrUnsupportedCurrency = fmt.Errorf("%w: unsupported ISO 4217 currency code", ErrInvalidCurrency)
	ErrInvalidAmount       = errors.New("invalid decimal amount string")
	ErrExcessPrecision     = errors.New("amount has more fractional digits than allowed by currency exponent")
	ErrOverflow            = errors.New("monetary arithmetic overflow or underflow")
	ErrInvalidDivisor      = errors.New("division by zero or invalid divisor")
)

// Currency exponents according to ISO 4217 standard.
var currencyExponents = map[string]int{
	// 0 decimals
	"JPY": 0, "KRW": 0, "VND": 0, "CLP": 0, "HUF": 0, "ISK": 0, "PYG": 0, "UGX": 0,
	// 2 decimals (standard)
	"RUB": 2, "USD": 2, "EUR": 2, "GBP": 2, "CHF": 2, "CNY": 2, "TRY": 2, "KZT": 2,
	"GEL": 2, "AMD": 2, "BYN": 2, "CAD": 2, "AUD": 2, "NZD": 2, "SEK": 2, "NOK": 2,
	"PLN": 2, "CZK": 2, "ILS": 2, "AED": 2, "SAR": 2, "SGD": 2, "HKD": 2, "INR": 2,
	"BRL": 2, "MXN": 2, "ZAR": 2, "THB": 2, "IDR": 2, "PHP": 2, "MYR": 2,
	// 3 decimals
	"BHD": 3, "KWD": 3, "OMR": 3, "JOD": 3, "TND": 3, "IQD": 3, "LYD": 3,
}

// Money represents a monetary value as an exact integer of minor units (cents, kopeks)
// with a 3-letter ISO 4217 currency code.
type Money struct {
	amountMinor int64
	currency    string
}

// ValidateCurrency validates that the currency is a valid, supported 3-letter uppercase ASCII ISO 4217 code.
func ValidateCurrency(code string) error {
	trimmed := strings.TrimSpace(code)
	if len(trimmed) != 3 {
		return fmt.Errorf("%w: must be exactly 3 uppercase letters, got %q", ErrInvalidCurrency, code)
	}
	for i := 0; i < 3; i++ {
		if trimmed[i] < 'A' || trimmed[i] > 'Z' {
			return fmt.Errorf("%w: must contain only uppercase ASCII letters, got %q", ErrInvalidCurrency, code)
		}
	}
	if _, ok := currencyExponents[trimmed]; !ok {
		return fmt.Errorf("%w: %q", ErrUnsupportedCurrency, code)
	}
	return nil
}

// CurrencyExponent returns the ISO 4217 decimal exponent for a currency (default 2).
func CurrencyExponent(currency string) int {
	if exp, ok := currencyExponents[currency]; ok {
		return exp
	}
	return 2
}

// GetCurrencyExponent returns the ISO 4217 decimal exponent or ErrUnsupportedCurrency if unsupported.
func GetCurrencyExponent(currency string) (int, error) {
	curr := strings.TrimSpace(currency)
	if err := ValidateCurrency(curr); err != nil {
		return 0, err
	}
	return currencyExponents[curr], nil
}

// NewMoney creates a Money value from minor units and currency code.
func NewMoney(amountMinor int64, currency string) (Money, error) {
	curr := strings.TrimSpace(currency)
	if err := ValidateCurrency(curr); err != nil {
		return Money{}, err
	}
	return Money{amountMinor: amountMinor, currency: curr}, nil
}

// MustNewMoney creates a Money value or panics if the currency is invalid.
func MustNewMoney(amountMinor int64, currency string) Money {
	m, err := NewMoney(amountMinor, currency)
	if err != nil {
		panic(err)
	}
	return m
}

// ParseMoney parses a decimal string into Money without using floating point arithmetic.
// Rejects amounts with excess fractional digits without silently rounding.
func ParseMoney(amountStr string, currency string) (Money, error) {
	curr := strings.TrimSpace(currency)
	if err := ValidateCurrency(curr); err != nil {
		return Money{}, err
	}

	raw := strings.TrimSpace(amountStr)
	if raw == "" {
		return Money{}, fmt.Errorf("%w: empty string", ErrInvalidAmount)
	}

	isNegative := false
	if raw[0] == '-' {
		isNegative = true
		raw = raw[1:]
	} else if raw[0] == '+' {
		raw = raw[1:]
	}

	if raw == "" {
		return Money{}, fmt.Errorf("%w: missing digits after sign", ErrInvalidAmount)
	}

	// Split integer and fractional parts on '.' or ','
	var intPart, fracPart string
	sepIdx := strings.IndexAny(raw, ".,")
	if sepIdx == -1 {
		intPart = raw
		fracPart = ""
	} else {
		// Check for multiple separators
		rest := raw[sepIdx+1:]
		if strings.ContainsAny(rest, ".,") {
			return Money{}, fmt.Errorf("%w: multiple decimal separators in %q", ErrInvalidAmount, amountStr)
		}
		intPart = raw[:sepIdx]
		fracPart = rest
	}

	// Validate integer part
	if intPart == "" {
		intPart = "0"
	}
	for i := 0; i < len(intPart); i++ {
		if intPart[i] < '0' || intPart[i] > '9' {
			return Money{}, fmt.Errorf("%w: invalid character in integer portion %q", ErrInvalidAmount, amountStr)
		}
	}

	// Validate fractional part
	for i := 0; i < len(fracPart); i++ {
		if fracPart[i] < '0' || fracPart[i] > '9' {
			return Money{}, fmt.Errorf("%w: invalid character in fractional portion %q", ErrInvalidAmount, amountStr)
		}
	}

	exp := CurrencyExponent(curr)
	if len(fracPart) > exp {
		return Money{}, fmt.Errorf("%w: currency %s permits at most %d decimal places, got %d in %q",
			ErrExcessPrecision, curr, exp, len(fracPart), amountStr)
	}

	// Pad fractional part with trailing zeros up to the currency exponent
	paddedFrac := fracPart + strings.Repeat("0", exp-len(fracPart))

	combined := intPart + paddedFrac
	// Strip leading zeros unless combined is just "0"
	combined = strings.TrimLeft(combined, "0")
	if combined == "" {
		combined = "0"
	}

	parsed, err := strconv.ParseInt(combined, 10, 64)
	if err != nil {
		return Money{}, fmt.Errorf("%w: %v", ErrOverflow, err)
	}

	if isNegative {
		if parsed > math.MaxInt64 {
			return Money{}, ErrOverflow
		}
		parsed = -parsed
	}

	return Money{amountMinor: parsed, currency: curr}, nil
}

// AmountMinor returns the amount in minor units (e.g. kopeks, cents).
func (m Money) AmountMinor() int64 {
	return m.amountMinor
}

// Currency returns the 3-letter ISO 4217 currency code.
func (m Money) Currency() string {
	return m.currency
}

// Exponent returns the decimal exponent for this money's currency.
func (m Money) Exponent() int {
	return CurrencyExponent(m.currency)
}

// Format returns the decimal string representation of the money amount (e.g. "149.99", "-150.00").
func (m Money) Format() string {
	exp := m.Exponent()
	isNeg := m.amountMinor < 0
	val := m.amountMinor
	if isNeg {
		// Handle math.MinInt64 edge case safely
		if val == math.MinInt64 {
			// -9223372036854775808
			str := "9223372036854775808"
			if exp == 0 {
				return "-" + str
			}
			intP := str[:len(str)-exp]
			fracP := str[len(str)-exp:]
			return "-" + intP + "." + fracP
		}
		val = -val
	}

	if exp == 0 {
		if isNeg {
			return fmt.Sprintf("-%d", val)
		}
		return fmt.Sprintf("%d", val)
	}

	divisor := int64(1)
	for i := 0; i < exp; i++ {
		divisor *= 10
	}

	intPart := val / divisor
	fracPart := val % divisor

	formatPattern := fmt.Sprintf("%%d.%%0%dd", exp)
	res := fmt.Sprintf(formatPattern, intPart, fracPart)
	if isNeg {
		return "-" + res
	}
	return res
}

// String returns formatted amount with currency (e.g. "149.99 RUB").
func (m Money) String() string {
	return fmt.Sprintf("%s %s", m.Format(), m.currency)
}

// Add returns m + other. Returns error if currencies do not match or on integer overflow.
func (m Money) Add(other Money) (Money, error) {
	if m.currency != other.currency {
		return Money{}, fmt.Errorf("%w: %s vs %s", ErrCurrencyMismatch, m.currency, other.currency)
	}

	a := m.amountMinor
	b := other.amountMinor
	if (b > 0 && a > math.MaxInt64-b) || (b < 0 && a < math.MinInt64-b) {
		return Money{}, ErrOverflow
	}

	return Money{amountMinor: a + b, currency: m.currency}, nil
}

// Sub returns m - other. Returns error if currencies do not match or on integer overflow.
func (m Money) Sub(other Money) (Money, error) {
	if m.currency != other.currency {
		return Money{}, fmt.Errorf("%w: %s vs %s", ErrCurrencyMismatch, m.currency, other.currency)
	}

	a := m.amountMinor
	b := other.amountMinor
	if (b < 0 && a > math.MaxInt64+b) || (b > 0 && a < math.MinInt64+b) {
		return Money{}, ErrOverflow
	}

	return Money{amountMinor: a - b, currency: m.currency}, nil
}

// Neg returns -m. Returns error on overflow (math.MinInt64).
func (m Money) Neg() (Money, error) {
	if m.amountMinor == math.MinInt64 {
		return Money{}, ErrOverflow
	}
	return Money{amountMinor: -m.amountMinor, currency: m.currency}, nil
}

// Abs returns absolute value of m. Returns error on overflow (math.MinInt64).
func (m Money) Abs() (Money, error) {
	if m.amountMinor < 0 {
		return m.Neg()
	}
	return m, nil
}

// MulFraction multiplies Money by (numerator / denominator) using Half-Even (Banker's) Rounding.
// Prevents systematic upward/downward drift in financial aggregation.
func (m Money) MulFraction(numerator, denominator int64) (Money, error) {
	if denominator == 0 {
		return Money{}, ErrInvalidDivisor
	}

	if m.amountMinor == 0 || numerator == 0 {
		return Money{amountMinor: 0, currency: m.currency}, nil
	}

	// Normalize signs so denominator > 0
	num := numerator
	den := denominator
	if den < 0 {
		if den == math.MinInt64 {
			return Money{}, ErrOverflow
		}
		den = -den
		if num == math.MinInt64 {
			return Money{}, ErrOverflow
		}
		num = -num
	}

	isNegative := false
	amt := m.amountMinor
	if amt < 0 {
		isNegative = !isNegative
		if amt == math.MinInt64 {
			return Money{}, ErrOverflow
		}
		amt = -amt
	}
	if num < 0 {
		isNegative = !isNegative
		if num == math.MinInt64 {
			return Money{}, ErrOverflow
		}
		num = -num
	}

	// Check multiplication overflow before multiplying: amt * num
	if amt > 0 && num > math.MaxInt64/amt {
		return Money{}, ErrOverflow
	}

	prod := amt * num
	q := prod / den
	r := prod % den

	// Half-Even Rounding
	// 2*r compared to den
	doubleR := r * 2
	if doubleR > den {
		// Strictly > half -> round up
		if q == math.MaxInt64 {
			return Money{}, ErrOverflow
		}
		q++
	} else if doubleR == den {
		// Exactly halfway -> round to nearest EVEN integer
		if q%2 != 0 {
			if q == math.MaxInt64 {
				return Money{}, ErrOverflow
			}
			q++
		}
	}

	if isNegative {
		q = -q
	}

	return Money{amountMinor: q, currency: m.currency}, nil
}

// Split divides Money into equal parts, distributing remainder minor units
// deterministically to the first remainder shares so that sum(shares) == m.amountMinor.
func (m Money) Split(parts int) ([]Money, error) {
	if parts <= 0 {
		return nil, fmt.Errorf("%w: parts must be > 0, got %d", ErrInvalidDivisor, parts)
	}

	p := int64(parts)
	base := m.amountMinor / p
	remainder := m.amountMinor % p

	// If negative remainder, handle symmetrically
	result := make([]Money, parts)
	if remainder >= 0 {
		for i := 0; i < parts; i++ {
			share := base
			if int64(i) < remainder {
				share++
			}
			result[i] = Money{amountMinor: share, currency: m.currency}
		}
	} else {
		remMagnitude := -remainder
		for i := 0; i < parts; i++ {
			share := base
			if int64(i) < remMagnitude {
				share--
			}
			result[i] = Money{amountMinor: share, currency: m.currency}
		}
	}

	return result, nil
}

// Allocate divides Money across proportional integer ratios (e.g. [50, 50], [1, 2, 3]).
// Remainder minor units are distributed to the shares with the largest fractional remainders.
func (m Money) Allocate(ratios ...int64) ([]Money, error) {
	if len(ratios) == 0 {
		return nil, fmt.Errorf("%w: at least one ratio required", ErrInvalidDivisor)
	}

	var totalWeight int64
	for _, r := range ratios {
		if r < 0 {
			return nil, fmt.Errorf("%w: ratio weight cannot be negative: %d", ErrInvalidDivisor, r)
		}
		if r > math.MaxInt64-totalWeight {
			return nil, ErrOverflow
		}
		totalWeight += r
	}

	if totalWeight == 0 {
		return nil, fmt.Errorf("%w: sum of ratio weights must be > 0", ErrInvalidDivisor)
	}

	result := make([]Money, len(ratios))
	type remainderTracker struct {
		index     int
		remainder int64
	}
	remainders := make([]remainderTracker, len(ratios))

	isNegative := m.amountMinor < 0
	amt := m.amountMinor
	if isNegative {
		if amt == math.MinInt64 {
			return nil, ErrOverflow
		}
		amt = -amt
	}

	var sumAllocated int64
	for i, r := range ratios {
		if amt > 0 && r > math.MaxInt64/amt {
			return nil, ErrOverflow
		}
		prod := amt * r
		share := prod / totalWeight
		rem := prod % totalWeight
		sumAllocated += share
		result[i] = Money{amountMinor: share, currency: m.currency}
		remainders[i] = remainderTracker{index: i, remainder: rem}
	}

	leftover := amt - sumAllocated
	// Distribute leftover minor units to largest remainders first
	// Simple stable insertion sort for remainders
	for i := 1; i < len(remainders); i++ {
		cur := remainders[i]
		j := i - 1
		for j >= 0 && (remainders[j].remainder < cur.remainder || (remainders[j].remainder == cur.remainder && remainders[j].index > cur.index)) {
			remainders[j+1] = remainders[j]
			j--
		}
		remainders[j+1] = cur
	}

	for k := int64(0); k < leftover; k++ {
		idx := remainders[k].index
		result[idx].amountMinor++
	}

	if isNegative {
		for i := range result {
			result[i].amountMinor = -result[i].amountMinor
		}
	}

	return result, nil
}

// Cmp compares m to other. Returns -1 if m < other, 0 if m == other, 1 if m > other.
func (m Money) Cmp(other Money) (int, error) {
	if m.currency != other.currency {
		return 0, fmt.Errorf("%w: %s vs %s", ErrCurrencyMismatch, m.currency, other.currency)
	}
	if m.amountMinor < other.amountMinor {
		return -1, nil
	} else if m.amountMinor > other.amountMinor {
		return 1, nil
	}
	return 0, nil
}

// Equal returns true if both money values have the same currency and amount.
func (m Money) Equal(other Money) bool {
	return m.currency == other.currency && m.amountMinor == other.amountMinor
}

// IsZero returns true if amount is 0.
func (m Money) IsZero() bool {
	return m.amountMinor == 0
}

// IsPositive returns true if amount > 0.
func (m Money) IsPositive() bool {
	return m.amountMinor > 0
}

// IsNegative returns true if amount < 0.
func (m Money) IsNegative() bool {
	return m.amountMinor < 0
}

// =============================================================================
// SQL & JSON Semantics
// =============================================================================

// Value implements driver.Valuer, returning the amount as an int64 minor unit.
func (m Money) Value() (driver.Value, error) {
	return m.amountMinor, nil
}

// Scan implements sql.Scanner for Money.
// Supports scanning from int64 (minor units), string (decimal string), or byte slice.
func (m *Money) Scan(src any) error {
	if src == nil {
		m.amountMinor = 0
		return nil
	}

	switch v := src.(type) {
	case int64:
		m.amountMinor = v
		return nil
	case int32:
		m.amountMinor = int64(v)
		return nil
	case int:
		m.amountMinor = int64(v)
		return nil
	case string:
		// Format: "149.99 RUB" or "14999"
		parts := strings.Fields(v)
		if len(parts) == 2 {
			parsed, err := ParseMoney(parts[0], parts[1])
			if err != nil {
				return err
			}
			*m = parsed
			return nil
		}
		// Integer string
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fmt.Errorf("cannot scan %q into Money: %w", v, err)
		}
		m.amountMinor = n
		return nil
	case []byte:
		return m.Scan(string(v))
	default:
		return fmt.Errorf("unsupported Scan source type for Money: %T", src)
	}
}

// =============================================================================
// JSON Serialization Contract
// =============================================================================
//
// In public API JSON payloads, Money serializes as a lossless structured object:
//
//	{
//	  "amount_minor": 14999,
//	  "currency": "RUB",
//	  "formatted": "149.99"
//	}
//
// Fields:
//   - "amount_minor" (int64): The canonical signed minor currency unit (e.g. kopeks, cents).
//     Guarantees zero IEEE 754 floating-point drift across API consumers and frontends.
//   - "currency" (string): 3-letter uppercase ISO 4217 currency code.
//   - "formatted" (string): Exact lossless decimal string formatted to currency exponent
//     (e.g. 2 for RUB/USD, 0 for JPY, 3 for BHD).
//
// In JSON deserialization (UnmarshalJSON), the parser accepts either:
// 1. Exact minor units: {"amount_minor": 14999, "currency": "RUB"}
// 2. Exact decimal string: {"amount": "149.99", "currency": "RUB"}
// Note: Bare float numbers (e.g. "amount": 149.99) are strictly rejected to prevent float imprecision.
type moneyJSON struct {
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
	Formatted   string `json:"formatted"`
}

// MarshalJSON serializes Money into JSON.
func (m Money) MarshalJSON() ([]byte, error) {
	return json.Marshal(moneyJSON{
		AmountMinor: m.amountMinor,
		Currency:    m.currency,
		Formatted:   m.Format(),
	})
}

// UnmarshalJSON deserializes Money from JSON.
func (m *Money) UnmarshalJSON(data []byte) error {
	var aux struct {
		AmountMinor *int64  `json:"amount_minor"`
		Amount      *string `json:"amount"`
		Currency    string  `json:"currency"`
	}

	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}

	if err := ValidateCurrency(aux.Currency); err != nil {
		return err
	}

	if aux.AmountMinor != nil && aux.Amount != nil {
		parsed, err := ParseMoney(*aux.Amount, aux.Currency)
		if err != nil {
			return err
		}
		if parsed.amountMinor != *aux.AmountMinor {
			return fmt.Errorf("%w: conflicting amount_minor (%d) and amount (%s)",
				ErrInvalidAmount, *aux.AmountMinor, *aux.Amount)
		}
		m.amountMinor = *aux.AmountMinor
		m.currency = strings.ToUpper(aux.Currency)
		return nil
	}

	if aux.AmountMinor != nil {
		m.amountMinor = *aux.AmountMinor
		m.currency = strings.ToUpper(aux.Currency)
		return nil
	}

	if aux.Amount != nil {
		parsed, err := ParseMoney(*aux.Amount, aux.Currency)
		if err != nil {
			return err
		}
		*m = parsed
		return nil
	}

	return fmt.Errorf("%w: missing amount_minor or amount in JSON", ErrInvalidAmount)
}

package domain

import (
	"encoding/json"
	"errors"
	"math"
	"testing"
)

func TestParseMoney_CanonicalVectors(t *testing.T) {
	tests := []struct {
		name          string
		input         string
		currency      string
		expectedMinor int64
		expectedFmt   string
	}{
		{"RUB 149.99", "149.99", "RUB", 14999, "149.99"},
		{"RUB comma 149,99", "149,99", "RUB", 14999, "149.99"},
		{"RUB negative -149.99", "-149.99", "RUB", -14999, "-149.99"},
		{"RUB single frac 149.9", "149.9", "RUB", 14990, "149.90"},
		{"RUB integer 150", "150", "RUB", 15000, "150.00"},
		{"RUB zero 0", "0", "RUB", 0, "0.00"},
		{"RUB dot zero .0", ".0", "RUB", 0, "0.00"},
		{"RUB dot 50 .50", ".50", "RUB", 50, "0.50"},

		{"JPY 150", "150", "JPY", 150, "150"},
		{"JPY negative -150", "-150", "JPY", -150, "-150"},
		{"JPY zero 0", "0", "JPY", 0, "0"},

		{"BHD 12.345", "12.345", "BHD", 12345, "12.345"},
		{"BHD negative -12.345", "-12.345", "BHD", -12345, "-12.345"},
		{"BHD single frac 12.3", "12.3", "BHD", 12300, "12.300"},
		{"BHD two frac 12.34", "12.34", "BHD", 12340, "12.340"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, err := ParseMoney(tc.input, tc.currency)
			if err != nil {
				t.Fatalf("unexpected error parsing %q %s: %v", tc.input, tc.currency, err)
			}
			if m.AmountMinor() != tc.expectedMinor {
				t.Errorf("expected minor units %d, got %d", tc.expectedMinor, m.AmountMinor())
			}
			if m.Format() != tc.expectedFmt {
				t.Errorf("expected formatted %q, got %q", tc.expectedFmt, m.Format())
			}
			if m.Currency() != tc.currency {
				t.Errorf("expected currency %s, got %s", tc.currency, m.Currency())
			}
		})
	}
}

func TestParseMoney_ExcessFractionalDigits(t *testing.T) {
	// Must not silently round excess fractional digits
	tests := []struct {
		name     string
		input    string
		currency string
	}{
		{"RUB 3 decimals", "149.999", "RUB"},
		{"RUB 4 decimals", "149.9999", "RUB"},
		{"JPY 1 decimal", "150.1", "JPY"},
		{"JPY 2 decimals", "150.05", "JPY"},
		{"BHD 4 decimals", "12.3456", "BHD"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseMoney(tc.input, tc.currency)
			if err == nil {
				t.Fatalf("expected error for excess precision in %q %s, got nil", tc.input, tc.currency)
			}
			if !errors.Is(err, ErrExcessPrecision) {
				t.Errorf("expected ErrExcessPrecision, got %v", err)
			}
		})
	}
}

func TestParseMoney_InvalidStrings(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		currency string
	}{
		{"Empty string", "", "RUB"},
		{"Whitespace only", "   ", "RUB"},
		{"Only sign +", "+", "RUB"},
		{"Only sign -", "-", "RUB"},
		{"Letters in integer", "12a.50", "RUB"},
		{"Letters in fraction", "12.5a", "RUB"},
		{"Multiple dots", "12.34.56", "RUB"},
		{"Multiple commas", "12,34,56", "RUB"},
		{"Both dot and comma", "12.34,56", "RUB"},
		{"Trailing minus", "12.50-", "RUB"},
		{"Double minus", "--12.50", "RUB"},
		{"Invalid currency lower", "100", "rub"}, // ParseMoney trims/uppercases valid codes, but invalid chars fail
		{"Invalid currency length", "100", "USDD"},
		{"Invalid currency digits", "100", "RU1"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseMoney(tc.input, tc.currency)
			if err == nil {
				t.Errorf("expected error for invalid input %q %s, got nil", tc.input, tc.currency)
			}
		})
	}
}

func TestMoney_ArithmeticAndOverflow(t *testing.T) {
	m100 := MustNewMoney(10000, "RUB") // 100.00 RUB
	m50 := MustNewMoney(5000, "RUB")   // 50.00 RUB
	usd100 := MustNewMoney(10000, "USD")

	// Addition
	sum, err := m100.Add(m50)
	if err != nil || sum.AmountMinor() != 15000 {
		t.Fatalf("expected 15000, got %d, err: %v", sum.AmountMinor(), err)
	}

	// Subtraction
	diff, err := m100.Sub(m50)
	if err != nil || diff.AmountMinor() != 5000 {
		t.Fatalf("expected 5000, got %d, err: %v", diff.AmountMinor(), err)
	}

	// Currency Mismatch on Add / Sub / Cmp
	if _, err := m100.Add(usd100); !errors.Is(err, ErrCurrencyMismatch) {
		t.Errorf("expected ErrCurrencyMismatch on Add, got %v", err)
	}
	if _, err := m100.Sub(usd100); !errors.Is(err, ErrCurrencyMismatch) {
		t.Errorf("expected ErrCurrencyMismatch on Sub, got %v", err)
	}
	if _, err := m100.Cmp(usd100); !errors.Is(err, ErrCurrencyMismatch) {
		t.Errorf("expected ErrCurrencyMismatch on Cmp, got %v", err)
	}

	// Overflow bounds
	maxM := MustNewMoney(math.MaxInt64, "RUB")
	minM := MustNewMoney(math.MinInt64, "RUB")
	oneM := MustNewMoney(1, "RUB")

	if _, err := maxM.Add(oneM); !errors.Is(err, ErrOverflow) {
		t.Errorf("expected overflow adding to MaxInt64, got %v", err)
	}
	if _, err := minM.Sub(oneM); !errors.Is(err, ErrOverflow) {
		t.Errorf("expected overflow subtracting from MinInt64, got %v", err)
	}
	if _, err := minM.Neg(); !errors.Is(err, ErrOverflow) {
		t.Errorf("expected overflow negating MinInt64, got %v", err)
	}
}

func TestMoney_HalfEvenRounding(t *testing.T) {
	// Banker's Rounding (Round Half to Even)
	// Testing positive and negative half-point rounding
	tests := []struct {
		name          string
		initialMinor  int64
		num           int64
		den           int64
		expectedMinor int64 // Half-even rounded result
	}{
		// 5 / 2 = 2.5 -> rounds to 2 (even)
		{"2.5 rounds to 2", 5, 1, 2, 2},
		// -5 / 2 = -2.5 -> rounds to -2 (even)
		{"-2.5 rounds to -2", -5, 1, 2, -2},
		// 7 / 2 = 3.5 -> rounds to 4 (even)
		{"3.5 rounds to 4", 7, 1, 2, 4},
		// -7 / 2 = -3.5 -> rounds to -4 (even)
		{"-3.5 rounds to -4", -7, 1, 2, -4},
		// 1 / 2 = 0.5 -> rounds to 0 (even)
		{"0.5 rounds to 0", 1, 1, 2, 0},
		// -1 / 2 = -0.5 -> rounds to 0 (even)
		{"-0.5 rounds to 0", -1, 1, 2, 0},
		// 3 / 2 = 1.5 -> rounds to 2 (even)
		{"1.5 rounds to 2", 3, 1, 2, 2},
		// -3 / 2 = -1.5 -> rounds to -2 (even)
		{"-1.5 rounds to -2", -3, 1, 2, -2},

		// Proportional tax / percentage: 100 kopeks * 15% (15/100) = 15
		{"100 * 15/100 = 15", 100, 15, 100, 15},
		// 105 kopeks * 15% = 15.75 -> 16
		{"105 * 15/100 = 15.75 -> 16", 105, 15, 100, 16},
		// 110 kopeks * 15% = 16.5 -> 16 (even)
		{"110 * 15/100 = 16.5 -> 16", 110, 15, 100, 16},
		// 130 kopeks * 15% = 19.5 -> 20 (even)
		{"130 * 15/100 = 19.5 -> 20", 130, 15, 100, 20},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := MustNewMoney(tc.initialMinor, "RUB")
			res, err := m.MulFraction(tc.num, tc.den)
			if err != nil {
				t.Fatalf("unexpected MulFraction error: %v", err)
			}
			if res.AmountMinor() != tc.expectedMinor {
				t.Errorf("expected %d, got %d", tc.expectedMinor, res.AmountMinor())
			}
		})
	}
}

func TestMoney_SplitRemainderDistribution(t *testing.T) {
	// 100 kopeks split 3 ways: 100 / 3 = 33 r 1
	// Deterministic rule: remainder +1 assigned to first share
	// Shares: [34, 33, 33], sum = 100
	m100 := MustNewMoney(100, "RUB")
	shares, err := m100.Split(3)
	if err != nil {
		t.Fatalf("unexpected Split error: %v", err)
	}
	if len(shares) != 3 {
		t.Fatalf("expected 3 shares, got %d", len(shares))
	}
	expectedShares := []int64{34, 33, 33}
	var total int64
	for i, s := range shares {
		if s.AmountMinor() != expectedShares[i] {
			t.Errorf("share %d expected %d, got %d", i, expectedShares[i], s.AmountMinor())
		}
		total += s.AmountMinor()
	}
	if total != 100 {
		t.Errorf("sum of shares %d != total 100", total)
	}

	// Negative 100 kopeks split 3 ways:
	// Shares: [-34, -33, -33], sum = -100
	mNeg100 := MustNewMoney(-100, "RUB")
	negShares, err := mNeg100.Split(3)
	if err != nil {
		t.Fatalf("unexpected Split error on negative: %v", err)
	}
	expectedNeg := []int64{-34, -33, -33}
	var negTotal int64
	for i, s := range negShares {
		if s.AmountMinor() != expectedNeg[i] {
			t.Errorf("neg share %d expected %d, got %d", i, expectedNeg[i], s.AmountMinor())
		}
		negTotal += s.AmountMinor()
	}
	if negTotal != -100 {
		t.Errorf("sum of negative shares %d != total -100", negTotal)
	}

	// 1000 kopeks split 3 ways: 334, 333, 333
	m1000 := MustNewMoney(1000, "RUB")
	shares1000, err := m1000.Split(3)
	if err != nil {
		t.Fatalf("unexpected Split error: %v", err)
	}
	if shares1000[0].AmountMinor() != 334 || shares1000[1].AmountMinor() != 333 || shares1000[2].AmountMinor() != 333 {
		t.Errorf("unexpected split of 1000: %v", shares1000)
	}

	// Split by 0 or negative
	if _, err := m100.Split(0); !errors.Is(err, ErrInvalidDivisor) {
		t.Errorf("expected ErrInvalidDivisor for split 0, got %v", err)
	}
	if _, err := m100.Split(-2); !errors.Is(err, ErrInvalidDivisor) {
		t.Errorf("expected ErrInvalidDivisor for split negative, got %v", err)
	}
}

func TestMoney_Allocate(t *testing.T) {
	// Allocate 100 across [1, 2] -> 1/3 and 2/3
	// 100 * 1 / 3 = 33 r 1
	// 100 * 2 / 3 = 66 r 2
	// Largest remainder is 2 (index 1), so index 1 gets +1 -> 33, 67
	m100 := MustNewMoney(100, "RUB")
	allocated, err := m100.Allocate(1, 2)
	if err != nil {
		t.Fatalf("unexpected Allocate error: %v", err)
	}
	if allocated[0].AmountMinor() != 33 || allocated[1].AmountMinor() != 67 {
		t.Errorf("expected [33, 67], got [%d, %d]", allocated[0].AmountMinor(), allocated[1].AmountMinor())
	}
	if allocated[0].AmountMinor()+allocated[1].AmountMinor() != 100 {
		t.Errorf("allocated sum must equal 100")
	}

	// Negative amount allocate
	mNeg100 := MustNewMoney(-100, "RUB")
	negAlloc, err := mNeg100.Allocate(1, 2)
	if err != nil {
		t.Fatalf("unexpected Allocate error on negative: %v", err)
	}
	if negAlloc[0].AmountMinor() != -33 || negAlloc[1].AmountMinor() != -67 {
		t.Errorf("expected [-33, -67], got [%d, %d]", negAlloc[0].AmountMinor(), negAlloc[1].AmountMinor())
	}
	if negAlloc[0].AmountMinor()+negAlloc[1].AmountMinor() != -100 {
		t.Errorf("allocated sum must equal -100")
	}
}

func TestMoney_SQLDriverAndJSON(t *testing.T) {
	m := MustNewMoney(14999, "RUB")

	// driver.Valuer
	val, err := m.Value()
	if err != nil {
		t.Fatalf("Value() error: %v", err)
	}
	if val != int64(14999) {
		t.Errorf("expected valuer int64(14999), got %v", val)
	}

	// sql.Scanner from int64
	var scanned Money
	if err := scanned.Scan(int64(14999)); err != nil {
		t.Fatalf("Scan(int64) error: %v", err)
	}
	if scanned.AmountMinor() != 14999 {
		t.Errorf("scanned amount expected 14999, got %d", scanned.AmountMinor())
	}

	// sql.Scanner from string
	if err := scanned.Scan("149.99 RUB"); err != nil {
		t.Fatalf("Scan(string) error: %v", err)
	}
	if scanned.AmountMinor() != 14999 || scanned.Currency() != "RUB" {
		t.Errorf("scanned from formatted string failed: %v", scanned)
	}

	// JSON Marshal
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}

	// JSON Unmarshal with amount_minor
	var unmarshaled Money
	if err := json.Unmarshal(data, &unmarshaled); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}
	if !m.Equal(unmarshaled) {
		t.Errorf("expected unmarshaled %v == %v", unmarshaled, m)
	}

	// JSON Unmarshal with decimal string
	decJSON := []byte(`{"amount": "149.99", "currency": "RUB"}`)
	var fromDec Money
	if err := json.Unmarshal(decJSON, &fromDec); err != nil {
		t.Fatalf("unmarshal from decimal json failed: %v", err)
	}
	if fromDec.AmountMinor() != 14999 || fromDec.Currency() != "RUB" {
		t.Errorf("unmarshal from decJSON failed: %v", fromDec)
	}

	// Test Zero-decimal currency (JPY) JSON round-trip
	jpy := MustNewMoney(150, "JPY")
	jpyData, err := json.Marshal(jpy)
	if err != nil {
		t.Fatalf("marshal JPY failed: %v", err)
	}
	var unmarshaledJPY Money
	if err := json.Unmarshal(jpyData, &unmarshaledJPY); err != nil {
		t.Fatalf("unmarshal JPY failed: %v", err)
	}
	if !jpy.Equal(unmarshaledJPY) || unmarshaledJPY.Format() != "150" {
		t.Errorf("JPY round-trip mismatch: got %v", unmarshaledJPY)
	}

	// Test 3-decimal currency (BHD) JSON round-trip
	bhd := MustNewMoney(12345, "BHD")
	bhdData, err := json.Marshal(bhd)
	if err != nil {
		t.Fatalf("marshal BHD failed: %v", err)
	}
	var unmarshaledBHD Money
	if err := json.Unmarshal(bhdData, &unmarshaledBHD); err != nil {
		t.Fatalf("unmarshal BHD failed: %v", err)
	}
	if !bhd.Equal(unmarshaledBHD) || unmarshaledBHD.Format() != "12.345" {
		t.Errorf("BHD round-trip mismatch: got %v", unmarshaledBHD)
	}

	// Test negative Money JSON
	negMoney := MustNewMoney(-5000, "RUB")
	negData, err := json.Marshal(negMoney)
	if err != nil {
		t.Fatalf("marshal negative Money failed: %v", err)
	}
	var unmarshaledNeg Money
	if err := json.Unmarshal(negData, &unmarshaledNeg); err != nil {
		t.Fatalf("unmarshal negative Money failed: %v", err)
	}
	if !negMoney.Equal(unmarshaledNeg) || unmarshaledNeg.Format() != "-50.00" {
		t.Errorf("negative Money roundtrip mismatch: got %v", unmarshaledNeg)
	}

	// Test rejection of raw float in amount field
	rawFloatJSON := []byte(`{"amount": 149.99, "currency": "RUB"}`)
	var rawFloat Money
	if err := json.Unmarshal(rawFloatJSON, &rawFloat); err == nil {
		t.Errorf("expected error when unmarshaling bare float in amount, but got nil")
	}

	// Test rejection of raw float in amount_minor field
	rawFloatMinorJSON := []byte(`{"amount_minor": 149.99, "currency": "RUB"}`)
	var rawFloatMinor Money
	if err := json.Unmarshal(rawFloatMinorJSON, &rawFloatMinor); err == nil {
		t.Errorf("expected error when unmarshaling bare float in amount_minor, but got nil")
	}

	// Test client-supplied "formatted" is never trusted and completely ignored
	untrustedJSON := []byte(`{"amount_minor": 100, "currency": "RUB", "formatted": "999999.99"}`)
	var untrusted Money
	if err := json.Unmarshal(untrustedJSON, &untrusted); err != nil {
		t.Fatalf("unexpected unmarshal error: %v", err)
	}
	if untrusted.AmountMinor() != 100 || untrusted.Format() != "1.00" {
		t.Errorf("expected untrusted formatted to be ignored (got minor=%d, formatted=%s)",
			untrusted.AmountMinor(), untrusted.Format())
	}

	// Test conflicting amount_minor and amount in JSON is rejected
	conflictingJSON := []byte(`{"amount_minor": 100, "amount": "2.00", "currency": "RUB"}`)
	var conflicting Money
	if err := json.Unmarshal(conflictingJSON, &conflicting); err == nil {
		t.Errorf("expected error for conflicting amount_minor and amount, but got nil")
	}

	// Test consistent amount_minor and amount in JSON succeeds
	consistentJSON := []byte(`{"amount_minor": 200, "amount": "2.00", "currency": "RUB"}`)
	var consistent Money
	if err := json.Unmarshal(consistentJSON, &consistent); err != nil {
		t.Fatalf("unexpected error for consistent amount_minor and amount: %v", err)
	}
	if consistent.AmountMinor() != 200 {
		t.Errorf("expected minor 200, got %d", consistent.AmountMinor())
	}

	// Test invalid currency in JSON
	badCurrJSON := []byte(`{"amount_minor": 100, "currency": "rub"}`)
	var badCurr Money
	if err := json.Unmarshal(badCurrJSON, &badCurr); err == nil {
		t.Errorf("expected error for lowercase currency in JSON, got nil")
	}

	// Test excess precision in JSON for zero-decimal JPY
	jpyExcessJSON := []byte(`{"amount": "150.5", "currency": "JPY"}`)
	var jpyExcess Money
	if err := json.Unmarshal(jpyExcessJSON, &jpyExcess); err == nil {
		t.Errorf("expected excess precision error for JPY with decimal, got nil")
	}

	// Test excess precision in JSON for 3-decimal BHD
	bhdExcessJSON := []byte(`{"amount": "12.3456", "currency": "BHD"}`)
	var bhdExcess Money
	if err := json.Unmarshal(bhdExcessJSON, &bhdExcess); err == nil {
		t.Errorf("expected excess precision error for BHD with 4 decimals, got nil")
	}
}

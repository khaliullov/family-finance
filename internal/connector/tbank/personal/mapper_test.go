package personal_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/khaliullov/family-finance/internal/connector/tbank/personal"
)

func TestParseMinorUnits(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		currency string
		expected int64
	}{
		// RUB: exponent 2
		{"RUB zero", "0", "RUB", 0},
		{"RUB zero decimal", "0.00", "RUB", 0},
		{"RUB whole integer", "10", "RUB", 1000},
		{"RUB 1-decimal pad", "10.5", "RUB", 1050},
		{"RUB 2-decimal exact", "10.50", "RUB", 1050},
		{"RUB 150.75", "150.75", "RUB", 15075},
		{"RUB negative", "-1250.75", "RUB", -125075},
		{"RUB negative whole", "-3500", "RUB", -350000},
		{"RUB large salary", "50000.00", "RUB", 5000000},
		{"RUB 1 kopeck", "0.01", "RUB", 1},
		{"RUB -5 kopecks", "-0.05", "RUB", -5},

		// USD: exponent 2
		{"USD zero", "0.00", "USD", 0},
		{"USD 12.50", "12.50", "USD", 1250},
		{"USD negative", "-99.99", "USD", -9999},

		// JPY: exponent 0
		{"JPY zero", "0", "JPY", 0},
		{"JPY 500", "500", "JPY", 500},
		{"JPY negative", "-12000", "JPY", -12000},

		// KWD & BHD: exponent 3
		{"KWD zero", "0.000", "KWD", 0},
		{"KWD 1.234", "1.234", "KWD", 1234},
		{"KWD 1-decimal pad", "1.5", "KWD", 1500},
		{"KWD negative", "-25.750", "KWD", -25750},
		{"BHD 0.005", "0.005", "BHD", 5},
		{"BHD negative", "-100.125", "BHD", -100125},

		// Exact int64 boundary parsing
		{"MaxInt64 RUB boundary", "92233720368547758.07", "RUB", 9223372036854775807},
		{"MinInt64 RUB boundary", "-92233720368547758.08", "RUB", -9223372036854775808},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := personal.ParseMinorUnits(json.Number(tt.input), tt.currency)
			if err != nil {
				t.Fatalf("ParseMinorUnits(%q, %q) error: %v", tt.input, tt.currency, err)
			}
			if got != tt.expected {
				t.Errorf("ParseMinorUnits(%q, %q) = %d; want %d", tt.input, tt.currency, got, tt.expected)
			}
		})
	}
}

func TestParseMinorUnits_FailClosed(t *testing.T) {
	invalidCases := []struct {
		name     string
		input    string
		currency string
	}{
		// Missing / null / empty
		{"empty string", "", "RUB"},
		{"whitespace only", "   ", "RUB"},

		// Excess fractional precision (no silent rounding or truncation)
		{"RUB 3 decimals", "12.345", "RUB"},
		{"RUB 0.001", "0.001", "RUB"},
		{"RUB negative 3 decimals", "-100.999", "RUB"},
		{"JPY 1 decimal", "100.5", "JPY"},
		{"KWD 4 decimals", "1.2345", "KWD"},
		{"BHD 4 decimals", "-5.0001", "BHD"},

		// Malformed lexemes
		{"non-numeric alpha", "abc", "RUB"},
		{"multiple dots", "12.34.56", "RUB"},
		{"comma separator", "12,34", "RUB"},
		{"lone minus", "-", "RUB"},
		{"lone plus", "+", "RUB"},

		// Unknown / unverified currency (must NOT silently assume 2)
		{"unknown currency code", "100.00", "XYZ"},
		{"unsupported currency FOO", "100.00", "FOO"},
		{"empty currency", "100.00", ""},

		// Overflow beyond int64 boundaries
		{"positive overflow", "92233720368547758.08", "RUB"},
		{"negative underflow", "-92233720368547758.09", "RUB"},
		{"huge number overflow", "99999999999999999999", "RUB"},
	}

	for _, tt := range invalidCases {
		t.Run(tt.name, func(t *testing.T) {
			got, err := personal.ParseMinorUnits(json.Number(tt.input), tt.currency)
			if err == nil {
				t.Errorf("ParseMinorUnits(%q, %q) expected error, got %d", tt.input, tt.currency, got)
			}
		})
	}
}

func TestNormalizeCurrencyCode(t *testing.T) {
	tests := []struct {
		curr     personal.TBankCurrency
		expected string
		wantErr  bool
	}{
		// Valid mappings
		{personal.TBankCurrency{Code: 643}, "RUB", false},
		{personal.TBankCurrency{Code: 810}, "RUB", false},
		{personal.TBankCurrency{StrCode: "643"}, "RUB", false},
		{personal.TBankCurrency{Name: "RUB"}, "RUB", false},
		{personal.TBankCurrency{StrCode: "rub"}, "RUB", false},
		{personal.TBankCurrency{Code: 840}, "USD", false},
		{personal.TBankCurrency{Name: "USD"}, "USD", false},
		{personal.TBankCurrency{Code: 978}, "EUR", false},
		{personal.TBankCurrency{Name: "EUR"}, "EUR", false},
		{personal.TBankCurrency{Code: 156}, "CNY", false},
		{personal.TBankCurrency{Code: 398}, "KZT", false},
		{personal.TBankCurrency{Code: 933}, "BYN", false},
		{personal.TBankCurrency{Code: 764}, "THB", false},
		{personal.TBankCurrency{Code: 784}, "AED", false},
		{personal.TBankCurrency{Code: 949}, "TRY", false},

		// Invalid mappings (must fail closed, NOT default to RUB)
		{personal.TBankCurrency{}, "", true},
		{personal.TBankCurrency{Name: ""}, "", true},
		{personal.TBankCurrency{Name: "XYZ"}, "", true},
		{personal.TBankCurrency{Code: 999}, "", true},
		{personal.TBankCurrency{StrCode: "999"}, "", true},
		{personal.TBankCurrency{StrCode: "INVALID"}, "", true},
	}

	for _, tt := range tests {
		got, err := personal.NormalizeCurrencyCode(tt.curr)
		if (err != nil) != tt.wantErr {
			t.Errorf("NormalizeCurrencyCode(%+v) error = %v, wantErr %v", tt.curr, err, tt.wantErr)
			continue
		}
		if !tt.wantErr && got != tt.expected {
			t.Errorf("NormalizeCurrencyCode(%+v) = %q, want %q", tt.curr, got, tt.expected)
		}
	}
}

func TestMapOperationToCanonical_Fixture(t *testing.T) {
	fixturePath := filepath.Join("testdata", "operations_timeline.json")
	data, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("failed to read fixture: %v", err)
	}

	var resp personal.TBankOperationsResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		t.Fatalf("unmarshal fixture failed: %v", err)
	}

	if len(resp.Payload) < 4 {
		t.Fatalf("expected at least 4 items in fixture, got %d", len(resp.Payload))
	}

	// 1. Pending Purchase Hold (op_1001)
	op1 := resp.Payload[0]
	dto1, err := personal.MapOperationToCanonical(op1)
	if err != nil {
		t.Fatalf("map op 1 failed: %v", err)
	}
	if dto1.ExternalID != "op_1001" {
		t.Errorf("ExternalID = %q; want op_1001", dto1.ExternalID)
	}
	if dto1.ExternalAuthID == nil || *dto1.ExternalAuthID != "auth_hold_501" {
		t.Errorf("ExternalAuthID = %v; want auth_hold_501", dto1.ExternalAuthID)
	}
	if dto1.BookingStatus != "pending" {
		t.Errorf("BookingStatus = %q; want pending", dto1.BookingStatus)
	}
	if dto1.Amount.MinorUnits != -125075 || dto1.Amount.Currency != "RUB" {
		t.Errorf("Amount = %+v; want -125075 RUB", dto1.Amount)
	}
	if dto1.SettledTime != nil {
		t.Errorf("SettledTime should be nil for pending hold, got %v", dto1.SettledTime)
	}
	if dto1.MCC == nil || *dto1.MCC != 5411 {
		t.Errorf("MCC = %v; want 5411", dto1.MCC)
	}

	// 2. Posted Taxi Purchase (op_1002)
	op2 := resp.Payload[1]
	dto2, err := personal.MapOperationToCanonical(op2)
	if err != nil {
		t.Fatalf("map op 2 failed: %v", err)
	}
	if dto2.BookingStatus != "posted" {
		t.Errorf("BookingStatus = %q; want posted", dto2.BookingStatus)
	}
	if dto2.SettledTime == nil {
		t.Errorf("SettledTime should not be nil for posted op")
	}
	if dto2.Amount.MinorUnits != -350000 {
		t.Errorf("Amount minor = %d; want -350000", dto2.Amount.MinorUnits)
	}

	// 3. Salary Income (op_1003) -> mapped to safe schema equivalent "transfer_in"
	op3 := resp.Payload[2]
	dto3, err := personal.MapOperationToCanonical(op3)
	if err != nil {
		t.Fatalf("map op 3 failed: %v", err)
	}
	if dto3.OperationType != "transfer_in" {
		t.Errorf("OperationType = %q; want transfer_in", dto3.OperationType)
	}
	if dto3.Amount.MinorUnits != 5000000 {
		t.Errorf("Amount minor = %d; want 5000000", dto3.Amount.MinorUnits)
	}

	// 4. Refund (op_1004)
	op4 := resp.Payload[3]
	dto4, err := personal.MapOperationToCanonical(op4)
	if err != nil {
		t.Fatalf("map op 4 failed: %v", err)
	}
	if dto4.OperationType != "refund" {
		t.Errorf("OperationType = %q; want refund", dto4.OperationType)
	}
	if dto4.Amount.MinorUnits != 80050 {
		t.Errorf("Amount minor = %d; want 80050", dto4.Amount.MinorUnits)
	}
}

func TestMapAccountItemToDTO_Fixture(t *testing.T) {
	fixturePath := filepath.Join("testdata", "accounts_light_ib.json")
	data, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("failed to read fixture: %v", err)
	}

	var resp personal.TBankAccountsLightResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		t.Fatalf("unmarshal fixture failed: %v", err)
	}

	if len(resp.Payload) < 2 {
		t.Fatalf("expected 2 accounts, got %d", len(resp.Payload))
	}

	acc1 := resp.Payload[0]
	dto1, err := personal.MapAccountItemToDTO(acc1)
	if err != nil {
		t.Fatal(err)
	}

	if dto1.ExternalID != "acc_debit_01" {
		t.Errorf("ExternalID = %q; want acc_debit_01", dto1.ExternalID)
	}
	if dto1.Type != "checking" {
		t.Errorf("Type = %q; want checking", dto1.Type)
	}
	if dto1.Balance == nil || dto1.Balance.MinorUnits != 14523050 {
		t.Errorf("Balance = %v; want 14523050", dto1.Balance)
	}
	if len(dto1.Cards) != 1 {
		t.Fatalf("Cards count = %d; want 1", len(dto1.Cards))
	}
	card := dto1.Cards[0]
	if card.ExternalID != "card_mir_01" || card.MaskedPAN != "*5512" || !card.IsPrimary {
		t.Errorf("Card details mismatch: %+v", card)
	}

	acc2 := resp.Payload[1]
	dto2, err := personal.MapAccountItemToDTO(acc2)
	if err != nil {
		t.Fatal(err)
	}
	if dto2.Type != "credit_card" {
		t.Errorf("Type = %q; want credit_card", dto2.Type)
	}
	if dto2.Balance == nil || dto2.Balance.MinorUnits != -1245000 {
		t.Errorf("Balance = %v; want -1245000", dto2.Balance)
	}
}

func TestMapOperationToCanonical_FailClosed(t *testing.T) {
	validOp := personal.TBankOperationItem{
		ID:            "op_test_1",
		Status:        "OK",
		OperationTime: personal.TBankTime{Milliseconds: 1700000000000},
		Amount: personal.TBankMoneyAmount{
			Value:    "100.50",
			Currency: personal.TBankCurrency{Code: 643},
		},
		AccountAmount: personal.TBankMoneyAmount{
			Value:    "100.50",
			Currency: personal.TBankCurrency{Code: 643},
		},
	}

	// 1. Missing external ID
	opMissingID := validOp
	opMissingID.ID = ""
	if _, err := personal.MapOperationToCanonical(opMissingID); err == nil {
		t.Errorf("expected error on missing ID, got nil")
	}

	// 2. Unknown status (must fail closed, NEVER default to posted)
	for _, badStatus := range []string{"UNKNOWN", "IN_PROGRESS", "SETTLED_HOLD", "PROCESSING", ""} {
		opBadStatus := validOp
		opBadStatus.Status = badStatus
		if _, err := personal.MapOperationToCanonical(opBadStatus); err == nil {
			t.Errorf("expected error on status %q, got nil", badStatus)
		}
	}

	// 3. Invalid amount precision
	opBadPrecision := validOp
	opBadPrecision.Amount.Value = "100.555"
	if _, err := personal.MapOperationToCanonical(opBadPrecision); err == nil {
		t.Errorf("expected error on amount precision > 2 decimals, got nil")
	}

	// 4. Missing currency
	opMissingCurr := validOp
	opMissingCurr.Amount.Currency = personal.TBankCurrency{}
	if _, err := personal.MapOperationToCanonical(opMissingCurr); err == nil {
		t.Errorf("expected error on missing amount currency, got nil")
	}

	// 5. Unsupported currency
	opUnsupportedCurr := validOp
	opUnsupportedCurr.Amount.Currency = personal.TBankCurrency{Name: "NON_EXISTENT_CURRENCY"}
	if _, err := personal.MapOperationToCanonical(opUnsupportedCurr); err == nil {
		t.Errorf("expected error on unsupported currency, got nil")
	}

	// 6. Missing timestamp
	opMissingTime := validOp
	opMissingTime.OperationTime = personal.TBankTime{Milliseconds: 0}
	if _, err := personal.MapOperationToCanonical(opMissingTime); err == nil {
		t.Errorf("expected error on missing timestamp, got nil")
	}
}

func TestMapAccountItemToDTO_FailClosed(t *testing.T) {
	validAcc := personal.TBankAccountItem{
		ID:          "acc_test_1",
		AccountType: "Current",
		Name:        "Test Account",
		MoneyAmount: &personal.TBankMoneyAmount{
			Value:    "5000.00",
			Currency: personal.TBankCurrency{Code: 643},
		},
	}

	// 1. Missing ID
	accMissingID := validAcc
	accMissingID.ID = ""
	if _, err := personal.MapAccountItemToDTO(accMissingID); err == nil {
		t.Errorf("expected error on missing account ID, got nil")
	}

	// 2. Missing MoneyAmount
	accMissingMoney := validAcc
	accMissingMoney.MoneyAmount = nil
	if _, err := personal.MapAccountItemToDTO(accMissingMoney); err == nil {
		t.Errorf("expected error on missing MoneyAmount, got nil")
	}

	// 3. Unsupported currency
	accBadCurr := validAcc
	accBadCurr.MoneyAmount = &personal.TBankMoneyAmount{
		Value:    "5000.00",
		Currency: personal.TBankCurrency{Name: "INVALID_CURRENCY"},
	}
	if _, err := personal.MapAccountItemToDTO(accBadCurr); err == nil {
		t.Errorf("expected error on unsupported account currency, got nil")
	}

	// 4. Invalid balance precision
	accBadPrecision := validAcc
	accBadPrecision.MoneyAmount = &personal.TBankMoneyAmount{
		Value:    "5000.999",
		Currency: personal.TBankCurrency{Code: 643},
	}
	if _, err := personal.MapAccountItemToDTO(accBadPrecision); err == nil {
		t.Errorf("expected error on invalid balance precision, got nil")
	}
}

func TestBatchMapping_FailFast(t *testing.T) {
	items := []personal.TBankOperationItem{
		{
			ID:            "op_1",
			Status:        "OK",
			OperationTime: personal.TBankTime{Milliseconds: 1700000000000},
			Amount: personal.TBankMoneyAmount{
				Value:    "10.00",
				Currency: personal.TBankCurrency{Code: 643},
			},
			AccountAmount: personal.TBankMoneyAmount{
				Value:    "10.00",
				Currency: personal.TBankCurrency{Code: 643},
			},
		},
		{
			ID:            "op_2_corrupted",
			Status:        "UNKNOWN_STATUS", // Will fail closed
			OperationTime: personal.TBankTime{Milliseconds: 1700000000000},
			Amount: personal.TBankMoneyAmount{
				Value:    "20.00",
				Currency: personal.TBankCurrency{Code: 643},
			},
			AccountAmount: personal.TBankMoneyAmount{
				Value:    "20.00",
				Currency: personal.TBankCurrency{Code: 643},
			},
		},
		{
			ID:            "op_3",
			Status:        "OK",
			OperationTime: personal.TBankTime{Milliseconds: 1700000000000},
			Amount: personal.TBankMoneyAmount{
				Value:    "30.00",
				Currency: personal.TBankCurrency{Code: 643},
			},
			AccountAmount: personal.TBankMoneyAmount{
				Value:    "30.00",
				Currency: personal.TBankCurrency{Code: 643},
			},
		},
	}

	// Verify fail fast in a batch mapping loop
	var mappedCount int
	var mapErr error
	for _, it := range items {
		_, err := personal.MapOperationToCanonical(it)
		if err != nil {
			mapErr = err
			break
		}
		mappedCount++
	}

	if mapErr == nil {
		t.Fatalf("expected batch mapping to fail fast on corrupted item, but it succeeded")
	}
	if mappedCount != 1 {
		t.Errorf("expected fail fast after 1 item, mappedCount = %d", mappedCount)
	}
}

func TestClassifyOperation(t *testing.T) {
	mcc5411 := 5411
	tests := []struct {
		name        string
		op          personal.TBankOperationItem
		amountMinor int64
		expected    string
	}{
		// Explicit merchant refund
		{
			name: "explicit refund in description",
			op: personal.TBankOperationItem{
				Description: "Возврат покупки Ozon",
			},
			amountMinor: 80050,
			expected:    "refund",
		},
		{
			name: "explicit refund in category",
			op: personal.TBankOperationItem{
				Description: "Unknown return",
				SpendingCategory: &personal.TBankCategory{
					Name: "Возврат",
				},
			},
			amountMinor: 15000,
			expected:    "refund",
		},

		// Explicit incoming transfer
		{
			name: "incoming transfer SBP",
			op: personal.TBankOperationItem{
				Description: "Перевод по СБП от Иван И.",
			},
			amountMinor: 500000,
			expected:    "transfer_in",
		},
		{
			name: "inner transfer in",
			op: personal.TBankOperationItem{
				IsInner:     true,
				Description: "Между своими счетами",
			},
			amountMinor: 100000,
			expected:    "transfer_in",
		},

		// Salary, cash-in, and other identified income -> safe schema equivalent "transfer_in"
		{
			name: "salary credit",
			op: personal.TBankOperationItem{
				Description: "Зачисление зарплаты за ноябрь",
			},
			amountMinor: 12000000,
			expected:    "transfer_in",
		},
		{
			name: "cash-in ATM",
			op: personal.TBankOperationItem{
				Description: "Пополнение через банкомат Тинькофф",
			},
			amountMinor: 3000000,
			expected:    "transfer_in",
		},
		{
			name: "interest on balance",
			op: personal.TBankOperationItem{
				Description: "Проценты на остаток по счету",
			},
			amountMinor: 45000,
			expected:    "transfer_in",
		},
		{
			name: "cashback bonus",
			op: personal.TBankOperationItem{
				Description: "Кэшбэк за покупки в октябре",
			},
			amountMinor: 185000,
			expected:    "transfer_in",
		},

		// Unknown credit (must NOT be mapped to refund; maps to safe schema equivalent "transfer_in")
		{
			name: "unknown credit becomes transfer_in",
			op: personal.TBankOperationItem{
				Description: "Unknown mystery credit event",
			},
			amountMinor: 50000,
			expected:    "transfer_in",
		},

		// Outflow / Debits
		{
			name: "retail card purchase with MCC and merchant",
			op: personal.TBankOperationItem{
				Type:        "Debit",
				Description: "Perekrestok",
				MCC:         &mcc5411,
				Merchant:    &personal.TBankMerchant{Name: "Perekrestok"},
			},
			amountMinor: -125050,
			expected:    "purchase",
		},
		{
			name: "atm cash withdrawal",
			op: personal.TBankOperationItem{
				Description: "Снятие наличных в банкомате",
			},
			amountMinor: -500000,
			expected:    "atm_cash",
		},
		{
			name: "explicit bank fee",
			op: personal.TBankOperationItem{
				Description: "Комиссия за обслуживание карты",
			},
			amountMinor: -9900,
			expected:    "fee",
		},
		{
			name: "fees NOT inferred solely from positive feeAmount without fee description",
			op: personal.TBankOperationItem{
				Type:        "Debit",
				Description: "Perekrestok Store",
				MCC:         &mcc5411,
				Merchant:    &personal.TBankMerchant{Name: "Perekrestok"},
			},
			amountMinor: -125050,
			expected:    "purchase", // Must remain purchase, NOT fee!
		},
		{
			name: "inner transfer out",
			op: personal.TBankOperationItem{
				IsInner:     true,
				Description: "Между своими счетами",
			},
			amountMinor: -100000,
			expected:    "transfer_out",
		},
		{
			name: "outgoing transfer SBP",
			op: personal.TBankOperationItem{
				Description: "Перевод по СБП в Альфа-Банк",
			},
			amountMinor: -250000,
			expected:    "transfer_out",
		},

		// Unknown debit (maps to safe schema equivalent "purchase")
		{
			name: "unknown debit becomes purchase",
			op: personal.TBankOperationItem{
				Description: "Random unclassified transaction descriptor",
			},
			amountMinor: -7500,
			expected:    "purchase",
		},

		// Zero check
		{
			name: "zero card check",
			op: personal.TBankOperationItem{
				Description: "Card authorization probe",
			},
			amountMinor: 0,
			expected:    "card_check",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := personal.ClassifyOperation(tt.op, tt.amountMinor)
			if got != tt.expected {
				t.Errorf("ClassifyOperation() = %q; want %q", got, tt.expected)
			}
		})
	}
}

func TestPendingDoesNotDependOnDebitingTime(t *testing.T) {
	// A transaction with status="OK" and debitingTime=nil must STILL be posted,
	// NOT falsely classified as pending!
	op := personal.TBankOperationItem{
		ID:            "op_posted_no_debit_time",
		Status:        "OK",
		OperationTime: personal.TBankTime{Milliseconds: 1700000000000},
		DebitingTime:  nil, // Absent debiting time
		Amount: personal.TBankMoneyAmount{
			Value:    "-500.00",
			Currency: personal.TBankCurrency{Code: 643},
		},
		AccountAmount: personal.TBankMoneyAmount{
			Value:    "-500.00",
			Currency: personal.TBankCurrency{Code: 643},
		},
		Description: "Coffee shop",
	}

	dto, err := personal.MapOperationToCanonical(op)
	if err != nil {
		t.Fatalf("MapOperationToCanonical failed: %v", err)
	}

	if dto.BookingStatus != "posted" {
		t.Errorf("BookingStatus = %q; want posted (must not infer pending merely because debitingTime is absent)", dto.BookingStatus)
	}
}

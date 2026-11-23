package transaction

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/khaliullov/family-finance/internal/domain"
)

func TestComputeFingerprint(t *testing.T) {
	accID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	txTime := time.Date(2026, 11, 20, 15, 30, 0, 0, time.UTC)
	amountMinor := int64(-150000)
	currency := "rub"
	desc := "  Yandex Go Taxi  "

	fp1 := ComputeFingerprint(accID, txTime, amountMinor, currency, desc)
	fp2 := ComputeFingerprint(accID, txTime, amountMinor, "RUB", "Yandex Go Taxi")

	if fp1 == "" {
		t.Fatalf("expected non-empty fingerprint")
	}
	if fp1 != fp2 {
		t.Fatalf("fingerprints must be deterministic and normalize currency/whitespace: %s != %s", fp1, fp2)
	}

	// Changing timestamp should alter fingerprint
	fpDifferentTime := ComputeFingerprint(accID, txTime.Add(time.Second), amountMinor, currency, desc)
	if fp1 == fpDifferentTime {
		t.Fatalf("different timestamp must produce different fingerprint")
	}

	// Changing amount should alter fingerprint
	fpDifferentAmount := ComputeFingerprint(accID, txTime, -160000, currency, desc)
	if fp1 == fpDifferentAmount {
		t.Fatalf("different amount must produce different fingerprint")
	}
}

func TestComputePayloadHash(t *testing.T) {
	payload1 := []byte(`{"transactions": [{"id": "1"}]}`)
	payload2 := []byte(`{"transactions": [{"id": "1"}]}`)
	payload3 := []byte(`{"transactions": [{"id": "2"}]}`)

	h1 := ComputePayloadHash(payload1)
	h2 := ComputePayloadHash(payload2)
	h3 := ComputePayloadHash(payload3)

	if h1 != h2 {
		t.Errorf("expected identical hashes for identical payloads")
	}
	if h1 == h3 {
		t.Errorf("expected different hashes for different payloads")
	}
	if len(h1) != 64 {
		t.Errorf("expected 64 hex characters for SHA-256, got %d", len(h1))
	}
}

func TestNormalizeSignedAmount(t *testing.T) {
	tests := []struct {
		name        string
		opType      string
		input       int64
		expected    int64
		expectError bool
	}{
		// Outflows must be negative
		{"purchase positive", "purchase", 150000, -150000, false},
		{"purchase negative", "purchase", -150000, -150000, false},
		{"fee", "fee", 9900, -9900, false},
		{"atm_cash", "atm_cash", 500000, -500000, false},
		{"transfer_out", "transfer_out", 1000000, -1000000, false},
		{"withdrawal", "withdrawal", 2000, -2000, false},

		// Inflows must be positive
		{"transfer_in positive", "transfer_in", 25000000, 25000000, false},
		{"transfer_in negative", "transfer_in", -25000000, 25000000, false},
		{"refund positive", "refund", 150000, 150000, false},
		{"refund negative", "refund", -150000, 150000, false},
		{"reversal", "reversal", 50000, 50000, false},
		{"salary", "salary", 10000000, 10000000, false},
		{"deposit", "deposit", 500000, 500000, false},
		{"income", "income", 300000, 300000, false},

		// Card verification checks must be 0
		{"card_check zero", "card_check", 0, 0, false},
		{"card_check non-zero", "card_check", 100, 0, false},
		{"card_check negative", "card_check", -50, 0, false},

		// Zero amounts
		{"zero purchase", "purchase", 0, 0, false},
		{"zero refund", "refund", 0, 0, false},

		// Default unknown opType
		{"unknown positive defaults negative", "custom_debit", 500, -500, false},
		{"unknown negative preserved negative", "custom_debit", -500, -500, false},

		// Boundary values
		{"max int64 purchase", "purchase", math.MaxInt64, -math.MaxInt64, false},
		{"max int64 refund", "refund", math.MaxInt64, math.MaxInt64, false},
		{"min int64 + 1 purchase", "purchase", math.MinInt64 + 1, -math.MaxInt64, false},
		{"min int64 + 1 refund", "refund", math.MinInt64 + 1, math.MaxInt64, false},

		// Overflow path: math.MinInt64 cannot be negated without overflow
		{"min int64 overflow purchase", "purchase", math.MinInt64, 0, true},
		{"min int64 overflow refund", "refund", math.MinInt64, 0, true},
		{"min int64 overflow unknown", "unknown", math.MinInt64, 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeSignedAmount(tt.opType, tt.input)
			if tt.expectError {
				if err == nil {
					t.Fatalf("NormalizeSignedAmount(%q, %d) expected error, got nil", tt.opType, tt.input)
				}
				if !errors.Is(err, domain.ErrOverflow) {
					t.Fatalf("expected ErrOverflow, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeSignedAmount(%q, %d) unexpected error: %v", tt.opType, tt.input, err)
			}
			if got != tt.expected {
				t.Errorf("NormalizeSignedAmount(%q, %d) = %d; want %d", tt.opType, tt.input, got, tt.expected)
			}
		})
	}

	// Test MustNormalizeSignedAmount panic behavior
	defer func() {
		r := recover()
		if r == nil {
			t.Errorf("expected MustNormalizeSignedAmount to panic on math.MinInt64")
		}
	}()
	MustNormalizeSignedAmount("purchase", math.MinInt64)
}

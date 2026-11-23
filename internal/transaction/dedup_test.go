package transaction

import (
	"testing"
	"time"

	"github.com/google/uuid"
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
		opType   string
		input    int64
		expected int64
	}{
		// Outflows must be negative
		{"purchase", 150000, -150000},
		{"purchase", -150000, -150000},
		{"fee", 9900, -9900},
		{"atm_cash", 500000, -500000},
		{"transfer_out", 1000000, -1000000},

		// Inflows must be positive
		{"transfer_in", 25000000, 25000000},
		{"transfer_in", -25000000, 25000000},
		{"refund", 150000, 150000},
		{"refund", -150000, 150000},

		// Card verification checks must be 0
		{"card_check", 0, 0},
		{"card_check", 100, 0},
	}

	for _, tt := range tests {
		got := NormalizeSignedAmount(tt.opType, tt.input)
		if got != tt.expected {
			t.Errorf("NormalizeSignedAmount(%q, %d) = %d; want %d", tt.opType, tt.input, got, tt.expected)
		}
	}
}

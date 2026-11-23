package transaction

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/khaliullov/family-finance/internal/domain"
)

// ComputeFingerprint calculates a deterministic SHA-256 fingerprint for a transaction.
// Combines account ID, ISO 8601 UTC timestamp, cleared minor amount, currency, and raw description.
func ComputeFingerprint(accountID uuid.UUID, txTime time.Time, amountMinor int64, currency string, rawDesc string) string {
	payload := fmt.Sprintf("%s|%s|%d|%s|%s",
		accountID.String(),
		txTime.UTC().Format(time.RFC3339Nano),
		amountMinor,
		strings.ToUpper(strings.TrimSpace(currency)),
		strings.TrimSpace(rawDesc),
	)
	hash := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(hash[:])
}

// ComputePayloadHash calculates a deterministic SHA-256 hash of a raw bank JSON payload.
func ComputePayloadHash(rawPayload []byte) string {
	hash := sha256.Sum256(rawPayload)
	return hex.EncodeToString(hash[:])
}

// NormalizeSignedAmount applies the project's accounting sign convention:
// Outflows (purchases, fees, cash withdrawals, outgoing transfers) are stored as negative minor units.
// Inflows (salary, incoming transfers, refunds, reversals) are stored as positive minor units.
// Zero-amount card verification checks are strictly zero.
// Returns domain.ErrOverflow if the amount cannot be represented safely (e.g. math.MinInt64).
func NormalizeSignedAmount(opType string, minorUnits int64) (int64, error) {
	if minorUnits == math.MinInt64 {
		return 0, fmt.Errorf("%w: amount %d cannot be normalized without signed integer overflow", domain.ErrOverflow, minorUnits)
	}

	normOp := strings.ToLower(strings.TrimSpace(opType))
	if normOp == "card_check" {
		return 0, nil
	}

	if minorUnits == 0 {
		return 0, nil
	}

	abs := minorUnits
	if abs < 0 {
		abs = -abs
	}

	switch normOp {
	case "refund", "reversal", "transfer_in", "salary", "deposit", "income":
		return abs, nil
	case "purchase", "fee", "transfer_out", "atm_cash", "withdrawal":
		return -abs, nil
	default:
		// If opType is unknown, preserve sign if negative, otherwise default to negative (debit)
		if minorUnits < 0 {
			return minorUnits, nil
		}
		return -abs, nil
	}
}

// MustNormalizeSignedAmount calls NormalizeSignedAmount or panics on error.
func MustNormalizeSignedAmount(opType string, minorUnits int64) int64 {
	res, err := NormalizeSignedAmount(opType, minorUnits)
	if err != nil {
		panic(err)
	}
	return res
}

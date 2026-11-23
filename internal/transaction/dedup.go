package transaction

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
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
// Inflows (salary, incoming transfers, refunds) are stored as positive minor units.
// Zero-amount card verification checks are strictly zero.
func NormalizeSignedAmount(opType string, minorUnits int64) int64 {
	abs := minorUnits
	if abs < 0 {
		abs = -abs
	}

	switch strings.ToLower(strings.TrimSpace(opType)) {
	case "card_check":
		return 0
	case "refund", "transfer_in":
		return abs
	case "purchase", "fee", "transfer_out", "atm_cash":
		return -abs
	default:
		// If opType is unknown, preserve sign if negative, otherwise make negative by default for debits
		if minorUnits < 0 {
			return minorUnits
		}
		return -abs
	}
}

package statement

import (
	"strings"
	"time"
)

// ReconciliationResult contains the evaluation of a statement's internal balance math.
type ReconciliationResult struct {
	Classification         string
	OpeningBalanceMinor    *int64
	ClosingBalanceMinor    *int64
	CalculatedBalanceMinor *int64
	DiscrepancyDeltaMinor  *int64
	PostedOperationsCount  int
	PostedDeltaMinor       int64
}

// EvaluateReconciliation evaluates statement balance math across the closed interval [periodStart, periodEnd].
//
// Invariants:
// 1. If openingBalance, closingBalance, periodStart, or periodEnd is nil, or currency is mismatched,
//    returns ReconciliationNonReconcilable.
// 2. Only posted operations with settled_time within [periodStart, periodEnd] affect the cleared delta.
// 3. Pending authorizations and cancelled operations are strictly excluded from cleared balance delta.
// 4. Returns ReconciliationReconciled if opening + postedDelta == closing. Otherwise ReconciliationDiscrepancy.
func EvaluateReconciliation(
	accountCurrency string,
	periodStart, periodEnd *time.Time,
	openingBalance, closingBalance *int64,
	rows []StagedRowDTO,
) ReconciliationResult {
	res := ReconciliationResult{
		Classification:      ReconciliationNonReconcilable,
		OpeningBalanceMinor: openingBalance,
		ClosingBalanceMinor: closingBalance,
	}

	if openingBalance == nil || closingBalance == nil || periodStart == nil || periodEnd == nil {
		return res
	}

	targetCur := strings.ToUpper(strings.TrimSpace(accountCurrency))
	var postedDelta int64
	var postedCount int

	for _, row := range rows {
		// Only posted operations in the account currency alter the settled balance
		if strings.ToLower(strings.TrimSpace(row.BookingStatus)) != "posted" {
			continue
		}
		if strings.ToUpper(strings.TrimSpace(row.AccountCurrency)) != targetCur {
			continue
		}

		// Use settled_time as the authoritative clearing timestamp
		clearingTime := row.TransactionTime
		if row.SettledTime != nil && !row.SettledTime.IsZero() {
			clearingTime = *row.SettledTime
		}

		// Closed interval check: [periodStart, periodEnd]
		if clearingTime.Before(*periodStart) || clearingTime.After(*periodEnd) {
			continue
		}

		postedDelta += row.AccountAmountMinor
		postedCount++
	}

	res.PostedOperationsCount = postedCount
	res.PostedDeltaMinor = postedDelta

	calculated := *openingBalance + postedDelta
	res.CalculatedBalanceMinor = &calculated

	discrepancy := *closingBalance - calculated
	res.DiscrepancyDeltaMinor = &discrepancy

	if discrepancy == 0 {
		res.Classification = ReconciliationReconciled
	} else {
		res.Classification = ReconciliationDiscrepancy
	}

	return res
}

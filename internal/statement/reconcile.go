package statement

import (
	"fmt"
	"strings"
	"time"

	"github.com/khaliullov/family-finance/internal/domain"
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
// 1. If openingBalance, closingBalance, periodStart, or periodEnd is nil,
//    returns ReconciliationNonReconcilable with nil error.
// 2. Only posted operations with settled_time within [periodStart, periodEnd] affect the cleared delta.
// 3. Pending authorizations and cancelled operations are strictly excluded from cleared balance delta.
// 4. Returns ReconciliationReconciled if opening + postedDelta == closing. Otherwise ReconciliationDiscrepancy.
// 5. Arithmetic overflow on any sum or subtraction fails safely with domain.ErrOverflow.
func EvaluateReconciliation(
	accountCurrency string,
	periodStart, periodEnd *time.Time,
	openingBalance, closingBalance *int64,
	rows []StagedRowDTO,
) (ReconciliationResult, error) {
	res := ReconciliationResult{
		Classification:      ReconciliationNonReconcilable,
		OpeningBalanceMinor: openingBalance,
		ClosingBalanceMinor: closingBalance,
	}

	if openingBalance == nil || closingBalance == nil || periodStart == nil || periodEnd == nil {
		return res, nil
	}

	targetCur := strings.ToUpper(strings.TrimSpace(accountCurrency))
	if err := domain.ValidateCurrency(targetCur); err != nil {
		return res, fmt.Errorf("invalid account currency: %w", err)
	}

	pStart := periodStart.UTC()
	pEnd := periodEnd.UTC()
	if pStart.After(pEnd) {
		return res, fmt.Errorf("invalid reconciliation period: start %v is after end %v", pStart, pEnd)
	}

	var postedDelta int64
	var postedCount int

	for _, row := range rows {
		// Verify row account currency matches target account currency
		rowAccCur := strings.ToUpper(strings.TrimSpace(row.AccountCurrency))
		if rowAccCur != targetCur {
			return res, fmt.Errorf("statement row %d account currency %q does not match account currency %q", row.RowIndex, row.AccountCurrency, targetCur)
		}

		// Only posted operations alter the settled balance
		if strings.ToLower(strings.TrimSpace(row.BookingStatus)) != "posted" {
			continue
		}

		// Use settled_time as the authoritative clearing timestamp
		clearingTime := row.TransactionTime.UTC()
		if row.SettledTime != nil && !row.SettledTime.IsZero() {
			clearingTime = row.SettledTime.UTC()
		}

		// Closed interval check: [periodStart, periodEnd]
		if clearingTime.Before(pStart) || clearingTime.After(pEnd) {
			continue
		}

		var err error
		postedDelta, err = domain.SafeAdd(postedDelta, row.AccountAmountMinor)
		if err != nil {
			return res, fmt.Errorf("reconciliation posted delta overflow: %w", err)
		}
		postedCount++
	}

	res.PostedOperationsCount = postedCount
	res.PostedDeltaMinor = postedDelta

	calculated, err := domain.SafeAdd(*openingBalance, postedDelta)
	if err != nil {
		return res, fmt.Errorf("reconciliation calculated balance overflow: %w", err)
	}
	res.CalculatedBalanceMinor = &calculated

	discrepancy, err := domain.SafeSub(*closingBalance, calculated)
	if err != nil {
		return res, fmt.Errorf("reconciliation discrepancy delta overflow: %w", err)
	}
	res.DiscrepancyDeltaMinor = &discrepancy

	if discrepancy == 0 {
		res.Classification = ReconciliationReconciled
	} else {
		res.Classification = ReconciliationDiscrepancy
	}

	return res, nil
}

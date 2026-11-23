package personal

import "encoding/json"

// TBankTime represents T-Bank's timestamp object containing millisecond unix epoch.
type TBankTime struct {
	Milliseconds int64 `json:"milliseconds"`
}

// TBankCurrency represents currency details within money amounts.
type TBankCurrency struct {
	Code    int    `json:"code"`    // e.g. 643 for RUB, 840 for USD
	Name    string `json:"name"`    // "RUB"
	StrCode string `json:"strCode"` // "RUB"
}

// TBankMoneyAmount represents monetary amounts returned by T-Bank Web API.
type TBankMoneyAmount struct {
	Value    json.Number   `json:"value"` // Stored as json.Number to preserve exact decimal precision
	Currency TBankCurrency `json:"currency"`
}

// TBankMerchant represents merchant details.
type TBankMerchant struct {
	Name string `json:"name"`
}

// TBankCategory represents spending category.
type TBankCategory struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// TBankSubgroup represents operation classification subgroup.
type TBankSubgroup struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// TBankOperationItem represents a single transaction item observed in
// GET /mybank/api/operations/timeline/public/legacy/v1/operations.
type TBankOperationItem struct {
	ID               string           `json:"id"`
	Account          string           `json:"account"`
	AuthorizationID  string           `json:"authorizationId,omitempty"`
	Card             string           `json:"card,omitempty"`
	CardNumber       string           `json:"cardNumber,omitempty"` // e.g. "*1234"
	Amount           TBankMoneyAmount `json:"amount"`
	AccountAmount    TBankMoneyAmount `json:"accountAmount"`
	OperationTime    TBankTime        `json:"operationTime"`
	DebitingTime     *TBankTime       `json:"debitingTime,omitempty"`
	Status           string           `json:"status"` // "OK", "WAITING", "FAILED"
	Type             string           `json:"type"`   // "Debit", "Credit", "Income", "Transfer"
	Description      string           `json:"description"`
	MCC              *int             `json:"mcc,omitempty"`
	MCCString        string           `json:"mccString,omitempty"`
	Merchant         *TBankMerchant   `json:"merchant,omitempty"`
	SpendingCategory *TBankCategory   `json:"spendingCategory,omitempty"`
	Subgroup         *TBankSubgroup   `json:"subgroup,omitempty"`
	IsInner          bool             `json:"isInner,omitempty"`
	IsExternalCard   bool             `json:"isExternalCard,omitempty"`
	Cashback         *json.Number     `json:"cashback,omitempty"`
	Group            string           `json:"group,omitempty"`
}

// TBankOperationsResponse represents the envelope for the operations timeline.
type TBankOperationsResponse struct {
	ResultCode string               `json:"resultCode"`
	TrackingID string               `json:"trackingId"`
	Payload    []TBankOperationItem `json:"payload"`
}

// TBankCardBrand represents card brand visual and naming metadata.
type TBankCardBrand struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// TBankCardItem represents a payment card associated with an account in accounts_light_ib.
type TBankCardItem struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	Brand      *TBankCardBrand `json:"brand,omitempty"`
	Expiration *TBankTime      `json:"expiration,omitempty"`
	Payable    bool            `json:"payable"`
	Primary    bool            `json:"primary"`
	Status     string          `json:"status"` // "ACTIVE", "CLOSED", "BLOCKED"
}

// TBankAccountItem represents an account observed in GET /api/common/v1/accounts_light_ib.
type TBankAccountItem struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	AccountType string            `json:"accountType"` // "Current", "Credit", "Deposit", "Saving"
	MoneyAmount *TBankMoneyAmount `json:"moneyAmount,omitempty"`
	Card        *TBankCardItem    `json:"card,omitempty"`
	Status      string            `json:"status"`
}

// TBankAccountsLightResponse represents the envelope for accounts_light_ib.
type TBankAccountsLightResponse struct {
	ResultCode string             `json:"resultCode"`
	TrackingID string             `json:"trackingId"`
	Payload    []TBankAccountItem `json:"payload"`
}

// TBankAccountDetails represents details observed in GET /api/common/v1/account_details_ib.
type TBankAccountDetails struct {
	AccountGroup           string            `json:"accountGroup"`
	AccountType            string            `json:"accountType"`
	AuthorizationsAmount   *TBankMoneyAmount `json:"authorizationsAmount,omitempty"` // Pending holds!
	CreditLimit            *TBankMoneyAmount `json:"creditLimit,omitempty"`
	CurrentMinimalPayment  *TBankMoneyAmount `json:"currentMinimalPayment,omitempty"`
	AfterNextStatementDate *TBankTime        `json:"afterNextStatementDate,omitempty"`
}

// TBankAccountDetailsResponse represents the envelope for account_details_ib.
type TBankAccountDetailsResponse struct {
	ResultCode string              `json:"resultCode"`
	Payload    TBankAccountDetails `json:"payload"`
}

// TBankStatementItem represents a periodic statement observed in GET /api/common/v1/statements.
type TBankStatementItem struct {
	StatementDate  TBankTime         `json:"statementDate"`
	Balance        *TBankMoneyAmount `json:"balance,omitempty"`
	BilledDebt     *TBankMoneyAmount `json:"billedDebt,omitempty"`
	CreditLimit    *TBankMoneyAmount `json:"creditLimit,omitempty"`
	AvailableLimit *TBankMoneyAmount `json:"availableLimit,omitempty"`
	PastDueDebt    *TBankMoneyAmount `json:"pastDueDebt,omitempty"`
	ArrestAmount   *TBankMoneyAmount `json:"arrestAmount,omitempty"`
	Cashback       *TBankMoneyAmount `json:"cashback,omitempty"`
	Period         string            `json:"period,omitempty"`
}

// TBankStatementsResponse represents the envelope for statements.
type TBankStatementsResponse struct {
	ResultCode string               `json:"resultCode"`
	Payload    []TBankStatementItem `json:"payload"`
}

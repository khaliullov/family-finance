package account

import "errors"

var (
	ErrAccountNotFound      = errors.New("account not found")
	ErrCardNotFound         = errors.New("card not found")
	ErrAccountArchived      = errors.New("account is archived")
	ErrCardClosed           = errors.New("card is closed")
	ErrInvalidAccountType   = errors.New("invalid account type")
	ErrInvalidPaymentSystem = errors.New("invalid payment system")
	ErrInvalidCardStatus    = errors.New("invalid card status")
	ErrInvalidMaskedPAN     = errors.New("invalid masked PAN format")
	ErrCrossFamilyReference = errors.New("cross-family reference prohibited")
	ErrInvalidOwner         = errors.New("owner must be an active family member")
	ErrHasFinancialHistory  = errors.New("cannot perform operation on entity with existing financial history")
	ErrCurrencyMismatch     = errors.New("currency mismatch between account and transaction")
	ErrInvalidAccountName   = errors.New("account name cannot be empty")
	ErrInvalidCardName      = errors.New("card display name cannot be empty")
	ErrUnauthorized         = errors.New("unauthorized family operation")
)

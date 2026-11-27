package statement

import (
	"context"

	"github.com/google/uuid"
)

type accountSelectorKey struct{}

// AccountSelector defines criteria for deterministic account matching in multi-account exports.
type AccountSelector struct {
	AccountID   uuid.UUID
	AccountName string
	AccountType string
	Currency    string
	ExternalID  string
	CardMasks   []string
}

// WithAccountSelector embeds an AccountSelector into the context.
func WithAccountSelector(ctx context.Context, sel AccountSelector) context.Context {
	return context.WithValue(ctx, accountSelectorKey{}, sel)
}

// AccountSelectorFromContext retrieves an AccountSelector from the context, if present.
func AccountSelectorFromContext(ctx context.Context) (AccountSelector, bool) {
	val := ctx.Value(accountSelectorKey{})
	if val == nil {
		return AccountSelector{}, false
	}
	sel, ok := val.(AccountSelector)
	return sel, ok
}

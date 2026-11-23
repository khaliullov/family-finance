package connector_test

import (
	"errors"
	"testing"

	"github.com/khaliullov/family-finance/internal/connector"
)

func TestSyncError(t *testing.T) {
	underlying := errors.New("network dial tcp timeout")
	err := connector.NewSyncError("test_prov", "TEMPORARY_FAILURE", "connection dropped", true, underlying)

	if err.Error() != "[test_prov] TEMPORARY_FAILURE: connection dropped (underlying: network dial tcp timeout)" {
		t.Errorf("unexpected error format: %s", err.Error())
	}

	if !errors.Is(err, underlying) {
		t.Errorf("expected errors.Is to match underlying error")
	}

	if !errors.Is(err, connector.ErrTemporary) {
		t.Errorf("expected errors.Is to match ErrTemporary")
	}

	authErr := connector.NewSyncError("test_prov", "AUTH_EXPIRED", "token expired", false, nil)
	if !errors.Is(authErr, connector.ErrAuthExpired) {
		t.Errorf("expected errors.Is to match ErrAuthExpired")
	}

	rateLimitErr := connector.NewSyncError("test_prov", "RATE_LIMITED", "429 too many requests", true, nil)
	if !errors.Is(rateLimitErr, connector.ErrRateLimited) {
		t.Errorf("expected errors.Is to match ErrRateLimited")
	}
}

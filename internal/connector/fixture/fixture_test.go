package fixture

import (
	"context"
	"errors"
	"testing"

	"github.com/khaliullov/family-finance/internal/connector"
)

func TestFixtureConnector_FetchAccounts(t *testing.T) {
	fc := NewFixtureConnector()
	ctx := context.Background()

	accounts, err := fc.FetchAccounts(ctx, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(accounts) != 3 {
		t.Fatalf("expected 3 accounts, got %d", len(accounts))
	}

	// Verify checking account
	acc1 := accounts[0]
	if acc1.ExternalID != "acc_rub_main" || acc1.Type != "checking" || acc1.Currency != "RUB" {
		t.Errorf("unexpected acc1: %+v", acc1)
	}
	if acc1.Balance == nil || acc1.Balance.MinorUnits != 25000000 {
		t.Errorf("unexpected acc1 balance: %+v", acc1.Balance)
	}
	if len(acc1.Cards) != 2 {
		t.Fatalf("expected 2 cards for acc1, got %d", len(acc1.Cards))
	}
	if !acc1.Cards[0].IsPrimary || acc1.Cards[0].MaskedPAN != "*1234" {
		t.Errorf("unexpected card 0: %+v", acc1.Cards[0])
	}
	if acc1.Cards[1].IsPrimary || acc1.Cards[1].MaskedPAN != "*5678" {
		t.Errorf("unexpected card 1: %+v", acc1.Cards[1])
	}

	// Verify credit card account
	acc2 := accounts[1]
	if acc2.ExternalID != "acc_rub_credit" || acc2.Type != "credit_card" {
		t.Errorf("unexpected acc2: %+v", acc2)
	}
	if acc2.CreditLimit == nil || acc2.CreditLimit.MinorUnits != 50000000 {
		t.Errorf("unexpected credit limit: %+v", acc2.CreditLimit)
	}
	if acc2.DebtAmount == nil || acc2.DebtAmount.MinorUnits != 3500000 {
		t.Errorf("unexpected debt amount: %+v", acc2.DebtAmount)
	}

	// Verify foreign currency account
	acc3 := accounts[2]
	if acc3.ExternalID != "acc_usd_travel" || acc3.Currency != "USD" {
		t.Errorf("unexpected acc3: %+v", acc3)
	}
}

func TestFixtureConnector_FetchCards(t *testing.T) {
	fc := NewFixtureConnector()
	ctx := context.Background()

	cards, err := fc.FetchCards(ctx, nil, "acc_rub_main")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cards) != 2 {
		t.Fatalf("expected 2 cards for acc_rub_main, got %d", len(cards))
	}

	cardsCredit, err := fc.FetchCards(ctx, nil, "acc_rub_credit")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cardsCredit) != 1 {
		t.Fatalf("expected 1 card for acc_rub_credit, got %d", len(cardsCredit))
	}

	cardsUnknown, err := fc.FetchCards(ctx, nil, "acc_unknown")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cardsUnknown) != 0 {
		t.Fatalf("expected 0 cards for acc_unknown, got %d", len(cardsUnknown))
	}
}

func TestFixtureConnector_FetchBalances(t *testing.T) {
	fc := NewFixtureConnector()
	ctx := context.Background()

	balances, err := fc.FetchBalances(ctx, nil, []string{"acc_rub_main", "acc_rub_credit"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(balances) != 2 {
		t.Fatalf("expected 2 balances, got %d", len(balances))
	}

	for _, b := range balances {
		if b.Source != "bank_api" {
			t.Errorf("expected source bank_api, got %s", b.Source)
		}
		if b.ObservedAt.IsZero() {
			t.Errorf("expected non-zero observed_at")
		}
	}
}

func TestFixtureConnector_FetchTransactions_Pagination(t *testing.T) {
	fc := NewFixtureConnector()
	ctx := context.Background()

	// Page 1
	res1, err := fc.FetchTransactions(ctx, nil, connector.SyncParams{})
	if err != nil {
		t.Fatalf("page 1 failed: %v", err)
	}
	if len(res1.Transactions) != 5 {
		t.Fatalf("expected 5 transactions in page 1, got %d", len(res1.Transactions))
	}
	if !res1.Progress.HasMore || res1.Progress.NextCursor != "cursor_page_2" {
		t.Fatalf("expected HasMore=true and next_cursor=cursor_page_2, got %+v", res1.Progress)
	}
	if len(res1.RawPayload) == 0 {
		t.Fatalf("expected non-empty raw payload")
	}

	// Verify Pending Hold in page 1
	tx0 := res1.Transactions[0]
	if tx0.ExternalID != "tx_hold_1001" || tx0.BookingStatus != "pending" {
		t.Errorf("unexpected tx0: %+v", tx0)
	}
	if tx0.ExternalAuthID == nil || *tx0.ExternalAuthID != "auth_hold_1001" {
		t.Errorf("expected external_auth_id auth_hold_1001, got %v", tx0.ExternalAuthID)
	}

	// Verify Foreign Currency in page 1
	tx1 := res1.Transactions[1]
	if tx1.Amount.Currency != "USD" || tx1.AccountAmount.Currency != "RUB" {
		t.Errorf("expected currency mismatch (USD vs RUB), got %+v", tx1)
	}

	// Verify card check (0.00) in page 1
	tx2 := res1.Transactions[2]
	if tx2.OperationType != "card_check" || tx2.AccountAmount.MinorUnits != 0 {
		t.Errorf("expected card_check with 0 amount, got %+v", tx2)
	}

	// Page 2 using cursor
	res2, err := fc.FetchTransactions(ctx, nil, connector.SyncParams{
		Cursor: res1.Progress.NextCursor,
	})
	if err != nil {
		t.Fatalf("page 2 failed: %v", err)
	}
	if len(res2.Transactions) != 2 {
		t.Fatalf("expected 2 transactions in page 2, got %d", len(res2.Transactions))
	}
	if res2.Progress.HasMore {
		t.Fatalf("expected HasMore=false on page 2")
	}
}

func TestFixtureConnector_FetchTransactions_SettledAndDuplicates(t *testing.T) {
	fc := NewFixtureConnector()
	ctx := context.Background()

	// Settled fixture
	resSettled, err := fc.FetchTransactions(ctx, nil, connector.SyncParams{Cursor: "cursor_settled"})
	if err != nil {
		t.Fatalf("settled fixture failed: %v", err)
	}
	if len(resSettled.Transactions) != 1 {
		t.Fatalf("expected 1 settled tx, got %d", len(resSettled.Transactions))
	}
	if resSettled.Transactions[0].BookingStatus != "posted" {
		t.Errorf("expected booking_status=posted, got %s", resSettled.Transactions[0].BookingStatus)
	}

	// Duplicates fixture
	resDup, err := fc.FetchTransactions(ctx, nil, connector.SyncParams{Cursor: "cursor_duplicates"})
	if err != nil {
		t.Fatalf("duplicates fixture failed: %v", err)
	}
	if len(resDup.Transactions) != 3 {
		t.Fatalf("expected 3 txs in duplicates fixture, got %d", len(resDup.Transactions))
	}

	// Empty fixture
	resEmpty, err := fc.FetchTransactions(ctx, nil, connector.SyncParams{Cursor: "cursor_empty"})
	if err != nil {
		t.Fatalf("empty fixture failed: %v", err)
	}
	if len(resEmpty.Transactions) != 0 || resEmpty.Progress.HasMore {
		t.Fatalf("expected 0 transactions in empty fixture")
	}
}

func TestFixtureConnector_ErrorSimulation(t *testing.T) {
	fc := NewFixtureConnector()
	ctx := context.Background()

	// 1. Auth expired
	err := fc.HealthCheck(ctx, []byte("fail:auth"))
	if err == nil || !errors.Is(err, connector.ErrAuthExpired) {
		t.Fatalf("expected ErrAuthExpired, got %v", err)
	}

	// 2. Rate limited
	_, err = fc.FetchAccounts(ctx, []byte("fail:rate_limit"))
	if err == nil || !errors.Is(err, connector.ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
	var syncErr *connector.SyncError
	if errors.As(err, &syncErr) && !syncErr.Retryable {
		t.Errorf("expected rate limit to be retryable")
	}

	// 3. Temporary failure
	_, err = fc.FetchTransactions(ctx, []byte("fail:temporary"), connector.SyncParams{})
	if err == nil || !errors.Is(err, connector.ErrTemporary) {
		t.Fatalf("expected ErrTemporary, got %v", err)
	}

	// 4. Invalid response
	_, err = fc.FetchTransactions(ctx, []byte("fail:invalid"), connector.SyncParams{})
	if err == nil || !errors.Is(err, connector.ErrInvalidResponse) {
		t.Fatalf("expected ErrInvalidResponse, got %v", err)
	}

	// 5. Unsupported op
	_, err = fc.FetchCards(ctx, []byte("fail:unsupported"), "acc1")
	if err == nil || !errors.Is(err, connector.ErrUnsupportedOperation) {
		t.Fatalf("expected ErrUnsupportedOperation, got %v", err)
	}
}

func TestFixtureConnector_ContextCancellation(t *testing.T) {
	fc := NewFixtureConnector()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	if err := fc.HealthCheck(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if _, err := fc.FetchAccounts(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if _, err := fc.FetchTransactions(ctx, nil, connector.SyncParams{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestConnectorRegistry(t *testing.T) {
	reg := connector.NewRegistry()
	fc := NewFixtureConnector()

	reg.Register(fc)

	got, err := reg.Get("fixture")
	if err != nil || got != fc {
		t.Fatalf("expected to get registered fixture connector, got %v, err=%v", got, err)
	}

	_, err = reg.Get("nonexistent")
	if !errors.Is(err, connector.ErrConnectorNotFound) {
		t.Fatalf("expected ErrConnectorNotFound, got %v", err)
	}
}

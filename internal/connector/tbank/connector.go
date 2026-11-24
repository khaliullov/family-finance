package tbank

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/khaliullov/family-finance/internal/connector"
	"github.com/khaliullov/family-finance/internal/connector/tbank/personal"
)

const (
	ProviderCodePersonal = "tbank_personal"
	ProviderCodeDefault  = "tbank"
)

// PersonalConnector implements connector.BankConnector for T-Bank personal retail accounts.
type PersonalConnector struct {
	client    *personal.PersonalClient
	cryptoKey []byte
}

// Option configures the PersonalConnector.
type Option func(*PersonalConnector)

// WithCryptoKey configures the AES-256 key used for decrypting stored credentials.
func WithCryptoKey(key []byte) Option {
	return func(c *PersonalConnector) {
		c.cryptoKey = key
	}
}

// WithBaseURL configures the target URL (e.g. for mock testing).
func WithBaseURL(urlStr string) Option {
	return func(c *PersonalConnector) {
		c.client.SetBaseURL(urlStr)
	}
}

// NewPersonalConnector initializes the T-Bank Personal connector.
// A valid 32-byte AES-256 encryption key is strictly required at construction time (fail closed).
func NewPersonalConnector(opts ...Option) (*PersonalConnector, error) {
	cfg := personal.DefaultTransportConfig()
	client, err := personal.NewPersonalClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("create personal client: %w", err)
	}

	c := &PersonalConnector{
		client: client,
	}

	for _, opt := range opts {
		opt(c)
	}

	if len(c.cryptoKey) != 32 {
		return nil, errors.New("tbank personal: a valid 32-byte AES-256 crypto key is strictly required")
	}

	return c, nil
}

func (c *PersonalConnector) ProviderCode() string {
	return ProviderCodeDefault
}

// resolveSession strictly decrypts and authenticates the encrypted session envelope using AES-256-GCM.
// Plaintext JSON, unencrypted session cookies, or raw tokens are strictly rejected (fail closed).
func (c *PersonalConnector) resolveSession(credentials []byte) (*personal.Session, error) {
	if len(credentials) == 0 {
		return nil, connector.ErrAuthExpired
	}

	sess, err := personal.DecryptSession(c.cryptoKey, credentials)
	if err != nil {
		return nil, connector.NewSyncError(ProviderCodePersonal, "INVALID_CREDENTIALS", "failed to decrypt banking session envelope", false, connector.ErrAuthExpired)
	}

	if sess == nil || sess.IsExpired() {
		return nil, connector.ErrAuthExpired
	}

	return sess, nil
}

func (c *PersonalConnector) HealthCheck(ctx context.Context, credentials []byte) error {
	session, err := c.resolveSession(credentials)
	if err != nil {
		return connector.ErrAuthExpired
	}

	// Verify session reachability by fetching accounts
	_, err = c.client.FetchAccounts(ctx, session)
	if err != nil {
		if errors.Is(err, connector.ErrAuthExpired) {
			return connector.ErrAuthExpired
		}
		return err
	}
	return nil
}

func (c *PersonalConnector) FetchAccounts(ctx context.Context, credentials []byte) ([]connector.AccountDTO, error) {
	session, err := c.resolveSession(credentials)
	if err != nil {
		return nil, err
	}
	return c.client.FetchAccounts(ctx, session)
}

func (c *PersonalConnector) FetchCards(ctx context.Context, credentials []byte, accountExternalID string) ([]connector.CardDTO, error) {
	accounts, err := c.FetchAccounts(ctx, credentials)
	if err != nil {
		return nil, err
	}

	for _, acc := range accounts {
		if acc.ExternalID == accountExternalID {
			return acc.Cards, nil
		}
	}
	return []connector.CardDTO{}, nil
}

func (c *PersonalConnector) FetchBalances(ctx context.Context, credentials []byte, accountExternalIDs []string) ([]connector.BalanceDTO, error) {
	accounts, err := c.FetchAccounts(ctx, credentials)
	if err != nil {
		return nil, err
	}

	reqMap := make(map[string]bool)
	for _, id := range accountExternalIDs {
		reqMap[id] = true
	}

	balances := make([]connector.BalanceDTO, 0, len(accounts))
	now := time.Now().UTC()
	for _, acc := range accounts {
		if len(reqMap) == 0 || reqMap[acc.ExternalID] {
			balances = append(balances, connector.BalanceDTO{
				AccountExternalID: acc.ExternalID,
				CurrentBalance:    acc.Balance,
				AvailableBalance:  acc.AvailableBalance,
				ObservedAt:        now,
				Source:            "tbank_personal",
			})
		}
	}

	return balances, nil
}

func (c *PersonalConnector) FetchTransactions(ctx context.Context, credentials []byte, params connector.SyncParams) (*connector.SyncResult, error) {
	session, err := c.resolveSession(credentials)
	if err != nil {
		return nil, err
	}
	return c.client.FetchTransactions(ctx, session, params)
}

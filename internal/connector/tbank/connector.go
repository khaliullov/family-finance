package tbank

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
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

	return c, nil
}

func (c *PersonalConnector) ProviderCode() string {
	return ProviderCodeDefault
}

// resolveSession resolves an encrypted or raw session envelope.
func (c *PersonalConnector) resolveSession(credentials []byte) (*personal.Session, error) {
	if len(credentials) == 0 {
		return nil, connector.ErrAuthExpired
	}

	// 1. Try decrypting using cryptoKey if configured
	if len(c.cryptoKey) == 32 {
		sess, err := personal.DecryptSession(c.cryptoKey, credentials)
		if err == nil && sess != nil {
			if sess.IsExpired() {
				return nil, connector.ErrAuthExpired
			}
			return sess, nil
		}
	}

	// 2. Try unmarshaling directly as unencrypted JSON Session (used in tests or fixture payloads)
	var directSess personal.Session
	if err := json.Unmarshal(credentials, &directSess); err == nil && len(directSess.Cookies) > 0 {
		if directSess.IsExpired() {
			return nil, connector.ErrAuthExpired
		}
		return &directSess, nil
	}

	// 3. Try parsing simple key-value credentials: {"session_id": "...", "cookies": [...]}
	var simpleCreds struct {
		SessionID string `json:"session_id"`
		Token     string `json:"token"`
	}
	if err := json.Unmarshal(credentials, &simpleCreds); err == nil {
		if simpleCreds.SessionID != "" {
			sess := personal.NewSession(uuid.Nil, uuid.Nil, []*http.Cookie{
				{Name: "sessionid", Value: simpleCreds.SessionID},
			}, 24*time.Hour)
			return sess, nil
		}
	}

	return nil, connector.ErrAuthExpired
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

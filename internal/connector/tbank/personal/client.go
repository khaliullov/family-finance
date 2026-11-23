package personal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/khaliullov/family-finance/internal/connector"
)

// PersonalClient provides high-level typed access to T-Bank personal retail accounts.
type PersonalClient struct {
	http   *PersonalHTTPClient
	config TransportConfig
}

// NewPersonalClient creates a new T-Bank personal client with the given transport configuration.
func NewPersonalClient(cfg TransportConfig) (*PersonalClient, error) {
	httpClient, err := NewPersonalHTTPClient(cfg)
	if err != nil {
		return nil, err
	}
	return &PersonalClient{
		http:   httpClient,
		config: cfg,
	}, nil
}

// HTTPClient returns the underlying PersonalHTTPClient.
func (c *PersonalClient) HTTPClient() *PersonalHTTPClient {
	return c.http
}

// SetBaseURL configures the root API URL (used for mock tests).
func (c *PersonalClient) SetBaseURL(u string) {
	c.http.SetBaseURL(u)
}

func (c *PersonalClient) prepareRequest(ctx context.Context, method, path string, session *Session) (*http.Request, error) {
	if session == nil || session.IsExpired() {
		return nil, connector.ErrAuthExpired
	}

	fullURL := c.http.BaseURL() + path
	req, err := http.NewRequestWithContext(ctx, method, fullURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	// Apply session cookies
	cookies := session.ToHTTPCookies()
	if len(cookies) > 0 {
		_ = c.http.SetCookiesForURL(c.http.BaseURL(), cookies)
	}

	// Set realistic modern browser client headers
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Referer", c.http.BaseURL()+"/mybank/")
	req.Header.Set("sec-ch-ua", `"Not_A Brand";v="8", "Chromium";v="120", "Google Chrome";v="120"`)
	req.Header.Set("sec-ch-ua-mobile", "?0")
	req.Header.Set("sec-ch-ua-platform", `"macOS"`)

	return req, nil
}

func (c *PersonalClient) handleHTTPError(statusCode int, body []byte) error {
	switch statusCode {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return connector.NewSyncError("tbank_personal", "AUTH_EXPIRED", "session expired or unauthorized", false, connector.ErrAuthExpired)
	case http.StatusTooManyRequests:
		return connector.NewSyncError("tbank_personal", "RATE_LIMITED", "t-bank web rate limit exceeded", true, connector.ErrRateLimited)
	case http.StatusServiceUnavailable, http.StatusBadGateway, http.StatusGatewayTimeout:
		return connector.NewSyncError("tbank_personal", "TEMPORARY_FAILURE", fmt.Sprintf("bank server error: %d", statusCode), true, connector.ErrTemporary)
	default:
		if statusCode >= 500 {
			return connector.NewSyncError("tbank_personal", "TEMPORARY_FAILURE", fmt.Sprintf("upstream error: %d", statusCode), true, connector.ErrTemporary)
		}
		return connector.NewSyncError("tbank_personal", "INVALID_RESPONSE", fmt.Sprintf("unexpected status code: %d", statusCode), false, connector.ErrInvalidResponse)
	}
}

// FetchAccounts retrieves personal accounts and cards via /api/common/v1/accounts_light_ib.
func (c *PersonalClient) FetchAccounts(ctx context.Context, session *Session) ([]connector.AccountDTO, error) {
	req, err := c.prepareRequest(ctx, http.MethodGet, "/api/common/v1/accounts_light_ib", session)
	if err != nil {
		return nil, err
	}

	body, statusCode, _, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if err := c.handleHTTPError(statusCode, body); err != nil {
		return nil, err
	}

	var resp TBankAccountsLightResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, connector.NewSyncError("tbank_personal", "INVALID_RESPONSE", "malformed accounts_light_ib response", false, err)
	}

	if resp.ResultCode != "OK" && resp.ResultCode != "" {
		if strings.Contains(strings.ToUpper(resp.ResultCode), "AUTH") || strings.Contains(strings.ToUpper(resp.ResultCode), "SESSION") {
			return nil, connector.ErrAuthExpired
		}
		return nil, connector.NewSyncError("tbank_personal", "INVALID_RESPONSE", fmt.Sprintf("upstream resultCode: %s", resp.ResultCode), false, errors.New(resp.ResultCode))
	}

	dtos := make([]connector.AccountDTO, 0, len(resp.Payload))
	for _, item := range resp.Payload {
		dto, err := MapAccountItemToDTO(item)
		if err != nil {
			continue
		}
		dtos = append(dtos, *dto)
	}

	return dtos, nil
}

// FetchAccountDetails retrieves details including authorizationsAmount holds via /api/common/v1/account_details_ib.
func (c *PersonalClient) FetchAccountDetails(ctx context.Context, session *Session, accountID string) (*TBankAccountDetails, error) {
	path := "/api/common/v1/account_details_ib"
	if accountID != "" {
		path += "?account=" + url.QueryEscape(accountID)
	}

	req, err := c.prepareRequest(ctx, http.MethodGet, path, session)
	if err != nil {
		return nil, err
	}

	body, statusCode, _, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if err := c.handleHTTPError(statusCode, body); err != nil {
		return nil, err
	}

	var resp TBankAccountDetailsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, connector.NewSyncError("tbank_personal", "INVALID_RESPONSE", "malformed account_details_ib response", false, err)
	}

	return &resp.Payload, nil
}

// FetchTransactions retrieves transactions via /mybank/api/operations/timeline/public/legacy/v1/operations.
func (c *PersonalClient) FetchTransactions(ctx context.Context, session *Session, params connector.SyncParams) (*connector.SyncResult, error) {
	path := "/mybank/api/operations/timeline/public/legacy/v1/operations"

	queryParams := url.Values{}
	if !params.StartDate.IsZero() {
		queryParams.Set("from", params.StartDate.UTC().Format(time.RFC3339))
	}
	if !params.EndDate.IsZero() {
		queryParams.Set("till", params.EndDate.UTC().Format(time.RFC3339))
	}
	if params.Cursor != "" {
		queryParams.Set("cursor", params.Cursor)
	}
	if len(params.Accounts) > 0 {
		queryParams.Set("account", params.Accounts[0])
	}
	if len(queryParams) > 0 {
		path += "?" + queryParams.Encode()
	}

	req, err := c.prepareRequest(ctx, http.MethodGet, path, session)
	if err != nil {
		return nil, err
	}

	body, statusCode, _, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if err := c.handleHTTPError(statusCode, body); err != nil {
		return nil, err
	}

	var resp TBankOperationsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, connector.NewSyncError("tbank_personal", "INVALID_RESPONSE", "malformed operations timeline response", false, err)
	}

	if resp.ResultCode != "OK" && resp.ResultCode != "" {
		if strings.Contains(strings.ToUpper(resp.ResultCode), "AUTH") || strings.Contains(strings.ToUpper(resp.ResultCode), "SESSION") {
			return nil, connector.ErrAuthExpired
		}
		return nil, connector.NewSyncError("tbank_personal", "INVALID_RESPONSE", fmt.Sprintf("upstream resultCode: %s", resp.ResultCode), false, errors.New(resp.ResultCode))
	}

	transactions := make([]connector.CanonicalBankTransaction, 0, len(resp.Payload))
	for _, item := range resp.Payload {
		canonical, err := MapOperationToCanonical(item)
		if err != nil {
			continue
		}
		transactions = append(transactions, *canonical)
	}

	result := &connector.SyncResult{
		Transactions: transactions,
		Progress: connector.SyncProgress{
			CurrentPage:    1,
			RecordsFetched: len(transactions),
			HasMore:        false, // Until continuation token capture confirms pagination
		},
		RawPayload: body,
	}

	return result, nil
}

package personal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"
)

var (
	ErrResponseBodyTooLarge = errors.New("http: response body exceeded maximum allowed size limit")
	ErrCrossDomainRedirect  = errors.New("http: redirect to untrusted external origin rejected")
)

const (
	DefaultMaxBodyBytes = 10 * 1024 * 1024 // 10 MB bounded response
	DefaultHTTPTimeout  = 30 * time.Second
)

// TransportConfig controls HTTP client parameters and safety boundaries.
type TransportConfig struct {
	TLSConfig          TLSConfig
	MaxResponseBody    int64
	RequestTimeout     time.Duration
	AllowedOriginHosts []string
}

// DefaultTransportConfig returns secure defaults for the T-Bank Personal client.
func DefaultTransportConfig() TransportConfig {
	return TransportConfig{
		TLSConfig:       DefaultTLSConfig(),
		MaxResponseBody: DefaultMaxBodyBytes,
		RequestTimeout:  DefaultHTTPTimeout,
		AllowedOriginHosts: []string{
			"tbank.ru",
			"www.tbank.ru",
			"id.tbank.ru",
			"tinkoff.ru",
			"www.tinkoff.ru",
		},
	}
}

// PersonalHTTPClient encapsulates an http.Client configured with uTLS and cookie isolation.
type PersonalHTTPClient struct {
	Client  *http.Client
	Jar     http.CookieJar
	Config  TransportConfig
	baseURL string
}

// NewPersonalHTTPClient creates a production-grade personal banking HTTP client.
func NewPersonalHTTPClient(cfg TransportConfig) (*PersonalHTTPClient, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("create cookiejar: %w", err)
	}

	maxBody := cfg.MaxResponseBody
	if maxBody <= 0 {
		maxBody = DefaultMaxBodyBytes
	}

	reqTimeout := cfg.RequestTimeout
	if reqTimeout <= 0 {
		reqTimeout = DefaultHTTPTimeout
	}

	// Base transport with custom uTLS dialer
	baseTransport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return DialTLSContext(ctx, network, addr, cfg.TLSConfig)
		},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	client := &http.Client{
		Transport: baseTransport,
		Jar:       jar,
		Timeout:   reqTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			// Security: Validate target host against allowed origins
			targetHost := strings.ToLower(req.URL.Hostname())
			if len(cfg.AllowedOriginHosts) > 0 && !isHostAllowed(targetHost, cfg.AllowedOriginHosts) {
				return fmt.Errorf("%w: %q", ErrCrossDomainRedirect, targetHost)
			}
			// Security: Never leak Authorization header or session cookies to cross-domain redirects
			if len(via) > 0 {
				firstHost := strings.ToLower(via[0].URL.Hostname())
				if targetHost != firstHost {
					req.Header.Del("Authorization")
					req.Header.Del("Cookie")
					req.Header.Del("X-CSRF-Token")
					req.Header.Del("w-csrf-token")
				}
			}
			return nil
		},
	}

	return &PersonalHTTPClient{
		Client:  client,
		Jar:     jar,
		Config:  cfg,
		baseURL: "https://www.tbank.ru",
	}, nil
}

// SetBaseURL configures the root URL (e.g. for httptest.Server in unit tests).
func (c *PersonalHTTPClient) SetBaseURL(urlStr string) {
	c.baseURL = strings.TrimRight(urlStr, "/")
}

// BaseURL returns the configured base URL.
func (c *PersonalHTTPClient) BaseURL() string {
	return c.baseURL
}

// Do executes a bounded, sanitized HTTP request with context cancellation.
func (c *PersonalHTTPClient) Do(req *http.Request) ([]byte, int, http.Header, error) {
	resp, err := c.Client.Do(req)
	if err != nil {
		return nil, 0, nil, sanitizeHTTPError(err)
	}
	defer resp.Body.Close()

	// Enforce bounded response body size
	maxBytes := c.Config.MaxResponseBody
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBodyBytes
	}

	limitReader := io.LimitReader(resp.Body, maxBytes+1)
	bodyBytes, err := io.ReadAll(limitReader)
	if err != nil {
		return nil, resp.StatusCode, resp.Header, fmt.Errorf("read response body: %w", sanitizeHTTPError(err))
	}

	if int64(len(bodyBytes)) > maxBytes {
		return nil, resp.StatusCode, resp.Header, ErrResponseBodyTooLarge
	}

	return bodyBytes, resp.StatusCode, resp.Header, nil
}

// isHostAllowed returns true if host matches any allowed host or subdomain.
func isHostAllowed(host string, allowed []string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	for _, a := range allowed {
		a = strings.ToLower(strings.TrimSpace(a))
		if host == a || strings.HasSuffix(host, "."+a) {
			return true
		}
	}
	return false
}

// sanitizeHTTPError strips sensitive credential fragments, tokens, or URL query parameters from network errors.
func sanitizeHTTPError(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()

	// Redact URL queries if present
	if idx := strings.Index(msg, "?"); idx != -1 {
		endIdx := strings.IndexAny(msg[idx:], " \t\r\n\"'")
		if endIdx != -1 {
			msg = msg[:idx] + "?[REDACTED]" + msg[idx+endIdx:]
		} else {
			msg = msg[:idx] + "?[REDACTED]"
		}
	}

	return errors.New(msg)
}

// GetCookiesForURL returns the cookies stored in the jar for a specific URL.
func (c *PersonalHTTPClient) GetCookiesForURL(rawURL string) ([]*http.Cookie, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	return c.Jar.Cookies(u), nil
}

// SetCookiesForURL sets cookies in the jar for a specific URL.
func (c *PersonalHTTPClient) SetCookiesForURL(rawURL string, cookies []*http.Cookie) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	c.Jar.SetCookies(u, cookies)
	return nil
}

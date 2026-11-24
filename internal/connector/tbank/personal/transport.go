package personal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
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

// PersonalHTTPClient encapsulates an http.Transport configured with uTLS connection pooling.
// Cookie state is never stored on this client; instead, each request uses the isolated
// CookieJar of the calling session, guaranteeing tenant and connection isolation.
type PersonalHTTPClient struct {
	Transport     *http.Transport
	Config        TransportConfig
	checkRedirect func(req *http.Request, via []*http.Request) error
	baseURL       string
}

// NewPersonalHTTPClient creates a production-grade personal banking HTTP client.
func NewPersonalHTTPClient(cfg TransportConfig) (*PersonalHTTPClient, error) {
	maxBody := cfg.MaxResponseBody
	if maxBody <= 0 {
		maxBody = DefaultMaxBodyBytes
	}

	reqTimeout := cfg.RequestTimeout
	if reqTimeout <= 0 {
		reqTimeout = DefaultHTTPTimeout
	}

	// Base transport with custom uTLS dialer and pooled TCP connections
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

	checkRedirect := func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		// Security: Validate target host against allowed origins
		targetHost := strings.ToLower(req.URL.Hostname())
		if len(cfg.AllowedOriginHosts) > 0 && !isHostAllowed(targetHost, cfg.AllowedOriginHosts) {
			return fmt.Errorf("%w: %q", ErrCrossDomainRedirect, targetHost)
		}
		if len(via) > 0 {
			initialReq := via[0]
			// Security: Block insecure redirect downgrade from HTTPS to HTTP
			if initialReq.URL.Scheme == "https" && req.URL.Scheme == "http" {
				return errors.New("http: insecure redirect downgrade from HTTPS to HTTP rejected")
			}
			// Security: Never leak Authorization header or session cookies across origin boundaries (scheme, host, port)
			initialHost := strings.ToLower(initialReq.URL.Hostname())
			sameOrigin := req.URL.Scheme == initialReq.URL.Scheme &&
				targetHost == initialHost &&
				req.URL.Port() == initialReq.URL.Port()
			if !sameOrigin {
				req.Header.Del("Authorization")
				req.Header.Del("Cookie")
				req.Header.Del("X-CSRF-Token")
				req.Header.Del("w-csrf-token")
			}
		}
		return nil
	}

	return &PersonalHTTPClient{
		Transport:     baseTransport,
		Config:        cfg,
		checkRedirect: checkRedirect,
		baseURL:       "https://www.tbank.ru",
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

// Do executes a bounded, sanitized HTTP request with context cancellation and isolated session cookies.
// The session parameter must be non-nil for authenticated requests. Each request uses the session's
// own isolated CookieJar, preventing cross-connection or cross-tenant cookie contamination.
func (c *PersonalHTTPClient) Do(req *http.Request, session *Session) ([]byte, int, http.Header, error) {
	var jar http.CookieJar
	if session != nil {
		sessJar, err := session.CookieJar(c.baseURL)
		if err != nil {
			return nil, 0, nil, fmt.Errorf("prepare session cookiejar: %w", err)
		}
		jar = sessJar
	}

	reqTimeout := c.Config.RequestTimeout
	if reqTimeout <= 0 {
		reqTimeout = DefaultHTTPTimeout
	}

	// Request-scoped http.Client sharing the underlying persistent Transport connection pool
	client := &http.Client{
		Transport:     c.Transport,
		Jar:           jar,
		Timeout:       reqTimeout,
		CheckRedirect: c.checkRedirect,
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, nil, sanitizeHTTPError(err)
	}
	defer resp.Body.Close()

	// Update session cookies if server provided Set-Cookie headers
	if session != nil {
		_ = session.UpdateCookiesFromJar(c.baseURL)
	}

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

// isHostAllowed returns true if host matches any allowed host or subdomain (handling IPv6 and ports).
func isHostAllowed(host string, allowed []string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	host = strings.Trim(host, "[]")
	for _, a := range allowed {
		a = strings.ToLower(strings.TrimSpace(a))
		a = strings.Trim(a, "[]")
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

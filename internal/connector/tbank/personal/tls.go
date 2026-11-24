package personal

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"time"

	utls "github.com/refraction-networking/utls"
)

// TLSClientProfile represents a configurable TLS ClientHello profile.
type TLSClientProfile string

const (
	// TLSProfileStandard uses standard Go crypto/tls (RFC compliant).
	TLSProfileStandard TLSClientProfile = "standard"
	// TLSProfileChrome uses Google Chrome's ClientHello fingerprint via uTLS.
	TLSProfileChrome TLSClientProfile = "chrome"
	// TLSProfileFirefox uses Mozilla Firefox's ClientHello fingerprint via uTLS.
	TLSProfileFirefox TLSClientProfile = "firefox"
	// TLSProfileEdge uses Microsoft Edge's ClientHello fingerprint via uTLS.
	TLSProfileEdge TLSClientProfile = "edge"
)

var (
	ErrTLSCertVerificationFailed = errors.New("tls: certificate verification failed")
	ErrUnsupportedTLSProfile     = errors.New("tls: unsupported client profile")
)

// TLSConfig defines the client TLS handshake parameters.
type TLSConfig struct {
	Profile    TLSClientProfile
	ServerName string
	RootCAs    *x509.CertPool
	Timeout    time.Duration
}

// DefaultTLSConfig returns the default production TLS configuration (Chrome uTLS profile).
func DefaultTLSConfig() TLSConfig {
	return TLSConfig{
		Profile: TLSProfileChrome,
		Timeout: 10 * time.Second,
	}
}

// DialTLSContext dials a TLS connection to addr with context cancellation and TLS ClientHello compatibility.
// Always enforces strict certificate verification (InsecureSkipVerify is never enabled).
// Correctly parses IPv4 and IPv6 addresses using net.SplitHostPort and verifies SNI.
func DialTLSContext(ctx context.Context, network, addr string, cfg TLSConfig) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("invalid tls addr %q: %w", addr, err)
	}

	serverName := cfg.ServerName
	if serverName == "" {
		serverName = host
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	dialer := &net.Dialer{
		Timeout: timeout,
	}

	rawConn, err := dialer.DialContext(ctx, network, addr)
	if err != nil {
		return nil, fmt.Errorf("tls dial tcp failed: %w", err)
	}

	// 1. Standard crypto/tls path
	if cfg.Profile == TLSProfileStandard || cfg.Profile == "" {
		stdTLSConf := &tls.Config{
			ServerName: serverName,
			RootCAs:    cfg.RootCAs,
			MinVersion: tls.VersionTLS12,
		}
		stdConn := tls.Client(rawConn, stdTLSConf)
		if err := stdConn.HandshakeContext(ctx); err != nil {
			rawConn.Close()
			return nil, fmt.Errorf("standard tls handshake failed: %w", err)
		}
		return stdConn, nil
	}

	// 2. uTLS path for protocol ClientHello compatibility
	var clientHelloID utls.ClientHelloID
	switch cfg.Profile {
	case TLSProfileChrome:
		clientHelloID = utls.HelloChrome_120
	case TLSProfileFirefox:
		clientHelloID = utls.HelloFirefox_120
	case TLSProfileEdge:
		clientHelloID = utls.HelloEdge_106
	default:
		rawConn.Close()
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedTLSProfile, cfg.Profile)
	}

	uTLSConf := &utls.Config{
		ServerName: serverName,
		RootCAs:    cfg.RootCAs,
		MinVersion: utls.VersionTLS12,
		// Enforce certificate verification unconditionally
		InsecureSkipVerify: false,
	}

	uConn := utls.UClient(rawConn, uTLSConf, clientHelloID)
	if err := uConn.HandshakeContext(ctx); err != nil {
		rawConn.Close()
		return nil, fmt.Errorf("utls handshake (%s) failed: %w", cfg.Profile, err)
	}

	return uConn, nil
}

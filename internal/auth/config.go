package auth

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

const (
	// InsecureDevSecret is the development placeholder secret key.
	// Production configuration MUST NOT use this key.
	InsecureDevSecret = "super-secret-key-that-is-at-least-32-bytes-long!"
)

var (
	ErrSecretMissing      = errors.New("jwt secret key is required")
	ErrInsecureProdSecret = errors.New("production environment cannot use insecure development secret placeholder")
)

// Config encapsulates configuration for the authentication subsystem.
type Config struct {
	JWTSecret       string          `json:"-"`
	Issuer          string          `json:"issuer"`
	Audience        string          `json:"audience"`
	AccessTokenTTL  time.Duration   `json:"access_token_ttl"`
	RefreshTokenTTL time.Duration   `json:"refresh_token_ttl"`
	ClockSkew       time.Duration   `json:"clock_skew"`
	Environment     string          `json:"environment"` // "production", "development", "test"
	RateLimits      RateLimitConfig `json:"rate_limits"`
}

// DefaultConfig provides development/testing defaults.
func DefaultConfig() Config {
	return Config{
		JWTSecret:       InsecureDevSecret,
		Issuer:          DefaultIssuer,
		Audience:        DefaultAudience,
		AccessTokenTTL:  DefaultAccessTokenTTL,
		RefreshTokenTTL: DefaultRefreshTokenTTL,
		ClockSkew:       1 * time.Minute,
		Environment:     "development",
		RateLimits:      DefaultRateLimitConfig(),
	}
}

// Validate checks the configuration against production security requirements.
func (c *Config) Validate() error {
	if strings.TrimSpace(c.JWTSecret) == "" {
		return ErrSecretMissing
	}
	if len(c.JWTSecret) < 32 {
		return ErrSecretTooWeak
	}

	env := strings.ToLower(strings.TrimSpace(c.Environment))
	if env == "production" || env == "prod" {
		if c.JWTSecret == InsecureDevSecret {
			return ErrInsecureProdSecret
		}
	}

	if c.Issuer == "" {
		c.Issuer = DefaultIssuer
	}
	if c.Audience == "" {
		c.Audience = DefaultAudience
	}
	if c.AccessTokenTTL <= 0 {
		c.AccessTokenTTL = DefaultAccessTokenTTL
	}
	if c.RefreshTokenTTL <= 0 {
		c.RefreshTokenTTL = DefaultRefreshTokenTTL
	}
	if c.ClockSkew <= 0 {
		c.ClockSkew = 1 * time.Minute
	}
	if c.RateLimits.MaxEntries <= 0 {
		c.RateLimits = DefaultRateLimitConfig()
	}

	return nil
}

// LoadConfigFromEnv loads and validates authentication configuration from environment variables.
func LoadConfigFromEnv() (*Config, error) {
	cfg := DefaultConfig()

	if sec := os.Getenv("JWT_SECRET"); sec != "" {
		cfg.JWTSecret = sec
	}
	if env := os.Getenv("APP_ENV"); env != "" {
		cfg.Environment = env
	}
	if iss := os.Getenv("JWT_ISSUER"); iss != "" {
		cfg.Issuer = iss
	}
	if aud := os.Getenv("JWT_AUDIENCE"); aud != "" {
		cfg.Audience = aud
	}
	if ttlStr := os.Getenv("JWT_ACCESS_TTL"); ttlStr != "" {
		if ttl, err := time.ParseDuration(ttlStr); err == nil {
			cfg.AccessTokenTTL = ttl
		}
	}
	if ttlStr := os.Getenv("JWT_REFRESH_TTL"); ttlStr != "" {
		if ttl, err := time.ParseDuration(ttlStr); err == nil {
			cfg.RefreshTokenTTL = ttl
		}
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid auth configuration: %w", err)
	}

	return &cfg, nil
}

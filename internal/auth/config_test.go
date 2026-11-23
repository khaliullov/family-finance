package auth

import (
	"errors"
	"testing"
)

func TestConfig_Validation(t *testing.T) {
	// 1. Default config in dev passes
	devCfg := DefaultConfig()
	if err := devCfg.Validate(); err != nil {
		t.Fatalf("DefaultConfig() should be valid, got: %v", err)
	}

	// 2. Missing secret fails
	emptyCfg := DefaultConfig()
	emptyCfg.JWTSecret = ""
	if err := emptyCfg.Validate(); !errors.Is(err, ErrSecretMissing) {
		t.Fatalf("expected ErrSecretMissing, got %v", err)
	}

	// 3. Secret too weak (< 32 bytes)
	weakCfg := DefaultConfig()
	weakCfg.JWTSecret = "short-secret-key"
	if err := weakCfg.Validate(); !errors.Is(err, ErrSecretTooWeak) {
		t.Fatalf("expected ErrSecretTooWeak, got %v", err)
	}

	// 4. Insecure dev placeholder prohibited in production
	prodInsecureCfg := DefaultConfig()
	prodInsecureCfg.Environment = "production"
	prodInsecureCfg.JWTSecret = InsecureDevSecret
	if err := prodInsecureCfg.Validate(); !errors.Is(err, ErrInsecureProdSecret) {
		t.Fatalf("expected ErrInsecureProdSecret for prod placeholder, got %v", err)
	}

	// 5. Valid strong production secret passes
	prodValidCfg := DefaultConfig()
	prodValidCfg.Environment = "production"
	prodValidCfg.JWTSecret = "production-cryptographically-random-secure-secret-key-2026!"
	if err := prodValidCfg.Validate(); err != nil {
		t.Fatalf("valid prod config should pass validation, got: %v", err)
	}
}

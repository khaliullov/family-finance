package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestGenerateRefreshToken(t *testing.T) {
	raw, hash, err := GenerateRefreshToken()
	if err != nil {
		t.Fatalf("GenerateRefreshToken failed: %v", err)
	}

	if len(raw) < 40 {
		t.Fatalf("expected raw token to be at least 40 chars base64url, got %d", len(raw))
	}
	if len(hash) != 64 {
		t.Fatalf("expected sha256 hash to be 64 hex chars, got %d", len(hash))
	}

	expectedHash := HashSessionToken(raw)
	if hash != expectedHash {
		t.Fatalf("hash mismatch: got %s, expected %s", hash, expectedHash)
	}

	// Test uniqueness
	raw2, hash2, err := GenerateRefreshToken()
	if err != nil {
		t.Fatalf("second GenerateRefreshToken failed: %v", err)
	}
	if raw == raw2 || hash == hash2 {
		t.Fatalf("expected distinct refresh tokens")
	}
}

func TestHashSessionToken(t *testing.T) {
	token := "test-token-12345"
	expectedSum := sha256.Sum256([]byte(token))
	expectedHex := hex.EncodeToString(expectedSum[:])

	computed := HashSessionToken(token)
	if computed != expectedHex {
		t.Fatalf("HashSessionToken(%q) = %q, want %q", token, computed, expectedHex)
	}
}

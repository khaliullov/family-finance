package auth

import (
	"strings"
	"testing"
)

func TestValidatePassword(t *testing.T) {
	tests := []struct {
		name      string
		password  string
		wantError error
	}{
		{"Too short 9 chars", "123456789", ErrPasswordTooShort},
		{"Empty string", "", ErrPasswordTooShort},
		{"Exact minimum 10 chars", "1234567890", nil},
		{"Normal 16 chars", "correct-horse-battery", nil},
		{"Exact maximum 128 chars", strings.Repeat("a", 128), nil},
		{"Too long 129 chars", strings.Repeat("a", 129), ErrPasswordTooLong},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidatePassword(tt.password)
			if err != tt.wantError {
				t.Fatalf("ValidatePassword(%q) = %v, want %v", tt.password, err, tt.wantError)
			}
		})
	}
}

func TestHashAndVerifyPassword(t *testing.T) {
	// Use slightly smaller config for quick unit testing if needed, or DefaultPasswordConfig
	// To verify DefaultPasswordConfig directly:
	pw := "secure_P@ssw0rd!2026"

	encoded, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("HashPassword failed: %v", err)
	}

	if !strings.HasPrefix(encoded, "$argon2id$v=19$m=65536,t=3,p=4$") {
		t.Fatalf("Hash format unexpected: %s", encoded)
	}

	// Correct password
	match, err := VerifyPassword(pw, encoded)
	if err != nil {
		t.Fatalf("VerifyPassword failed: %v", err)
	}
	if !match {
		t.Fatalf("expected password to match")
	}

	// Wrong password
	match, err = VerifyPassword("wrong_password_here", encoded)
	if err != nil {
		t.Fatalf("VerifyPassword failed on wrong password: %v", err)
	}
	if match {
		t.Fatalf("expected wrong password not to match")
	}

	// Salt uniqueness
	encoded2, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("HashPassword second call failed: %v", err)
	}
	if encoded == encoded2 {
		t.Fatalf("expected different salts to generate different hashes")
	}
}

func TestVerifyPassword_InvalidHashes(t *testing.T) {
	invalidHashes := []string{
		"",
		"not-a-hash",
		"$argon2i$v=19$m=65536,t=3,p=4$c2FsdA$aGFzaA", // argon2i instead of argon2id
		"$argon2id$v=18$m=65536,t=3,p=4$c2FsdA$aGFzaA", // invalid version
		"$argon2id$v=19$bad_params$c2FsdA$aGFzaA",       // malformed params
		"$argon2id$v=19$m=65536,t=3,p=4$!invalidb64$aGFzaA",
	}

	for _, h := range invalidHashes {
		match, err := VerifyPassword("any-pass", h)
		if match || err == nil {
			t.Errorf("expected error and false for invalid hash %q, got match=%v, err=%v", h, match, err)
		}
	}
}

func TestDummyVerify(t *testing.T) {
	// Should execute without panic
	DummyVerify("some-nonexistent-user-password")
}

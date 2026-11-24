package crypto_test

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"testing"

	"github.com/khaliullov/family-finance/internal/crypto"
)

func TestEncryptDecrypt_RoundTrip(t *testing.T) {
	key, err := crypto.GenerateRandomKey()
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}

	plaintext := []byte("confidential banking session cookie: sessionid=secret_12345; Path=/")
	ciphertext, err := crypto.Encrypt(key, plaintext)
	if err != nil {
		t.Fatalf("encryption failed: %v", err)
	}

	if bytes.Equal(plaintext, ciphertext) {
		t.Fatalf("ciphertext must not match plaintext")
	}

	decrypted, err := crypto.Decrypt(key, ciphertext)
	if err != nil {
		t.Fatalf("decryption failed: %v", err)
	}

	if !bytes.Equal(plaintext, decrypted) {
		t.Fatalf("decrypted %q != plaintext %q", string(decrypted), string(plaintext))
	}
}

func TestEncrypt_UniqueNonces(t *testing.T) {
	key, err := crypto.GenerateRandomKey()
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}

	plaintext := []byte("static payload")
	c1, err := crypto.Encrypt(key, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := crypto.Encrypt(key, plaintext)
	if err != nil {
		t.Fatal(err)
	}

	if bytes.Equal(c1, c2) {
		t.Fatalf("two encryptions of the same plaintext must produce different ciphertexts due to fresh nonces")
	}
}

func TestDecrypt_TamperedCiphertextFails(t *testing.T) {
	key, err := crypto.GenerateRandomKey()
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}

	plaintext := []byte("tamper test payload")
	ciphertext, err := crypto.Encrypt(key, plaintext)
	if err != nil {
		t.Fatal(err)
	}

	// Flip one bit in the ciphertext
	ciphertext[len(ciphertext)-1] ^= 0x01

	_, err = crypto.Decrypt(key, ciphertext)
	if err == nil {
		t.Fatalf("expected decryption error on tampered ciphertext, got nil")
	}
}

func TestDecrypt_WrongKeyFails(t *testing.T) {
	key1, _ := crypto.GenerateRandomKey()
	key2, _ := crypto.GenerateRandomKey()

	plaintext := []byte("wrong key test")
	ciphertext, err := crypto.Encrypt(key1, plaintext)
	if err != nil {
		t.Fatal(err)
	}

	_, err = crypto.Decrypt(key2, ciphertext)
	if err == nil {
		t.Fatalf("expected decryption error with wrong key, got nil")
	}
}

func TestDeriveKey_Deterministic(t *testing.T) {
	secret := "master-secret-password-123!"
	salt := []byte("connection-salt")

	k1 := crypto.DeriveKey(secret, salt)
	k2 := crypto.DeriveKey(secret, salt)

	if len(k1) != crypto.KeySize {
		t.Fatalf("expected derived key length %d, got %d", crypto.KeySize, len(k1))
	}

	if !bytes.Equal(k1, k2) {
		t.Fatalf("derived keys must be identical for identical secret and salt")
	}
}

func TestDeriveKeyArgon2id(t *testing.T) {
	password := "correct horse battery staple"
	salt := []byte("0123456789abcdef") // 16 bytes

	k1, err := crypto.DeriveKeyArgon2id(password, salt)
	if err != nil {
		t.Fatalf("DeriveKeyArgon2id failed: %v", err)
	}
	if len(k1) != crypto.KeySize {
		t.Fatalf("expected key size %d, got %d", crypto.KeySize, len(k1))
	}

	// Deterministic
	k2, err := crypto.DeriveKeyArgon2id(password, salt)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(k1, k2) {
		t.Fatalf("Argon2id keys must be identical for same password and salt")
	}

	// Distinct for different password
	k3, err := crypto.DeriveKeyArgon2id(password+"!", salt)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(k1, k3) {
		t.Fatalf("Argon2id keys must differ for different passwords")
	}

	// Distinct for different salt
	salt2 := []byte("fedcba9876543210")
	k4, err := crypto.DeriveKeyArgon2id(password, salt2)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(k1, k4) {
		t.Fatalf("Argon2id keys must differ for different salts")
	}

	// Reject empty password
	if _, err := crypto.DeriveKeyArgon2id("", salt); err == nil {
		t.Errorf("expected error on empty password, got nil")
	}

	// Reject short salt (< 16 bytes)
	if _, err := crypto.DeriveKeyArgon2id(password, []byte("short-salt")); err == nil {
		t.Errorf("expected error on salt < 16 bytes, got nil")
	}
}

func TestParseMasterKey(t *testing.T) {
	// 1. Raw 32-byte key
	rawKey := "01234567890123456789012345678901" // exactly 32 chars
	kRaw, err := crypto.ParseMasterKey(rawKey)
	if err != nil {
		t.Fatalf("failed to parse raw 32-byte key: %v", err)
	}
	if len(kRaw) != 32 || string(kRaw) != rawKey {
		t.Fatalf("parsed raw key mismatch: %v", kRaw)
	}

	// 2. 64-char Hex key
	hexKey := hex.EncodeToString([]byte("01234567890123456789012345678901"))
	kHex, err := crypto.ParseMasterKey(hexKey)
	if err != nil {
		t.Fatalf("failed to parse hex key: %v", err)
	}
	if !bytes.Equal(kHex, []byte(rawKey)) {
		t.Fatalf("parsed hex key mismatch")
	}

	// 3. 44-char Base64 key
	b64Key := base64.StdEncoding.EncodeToString([]byte("01234567890123456789012345678901"))
	kB64, err := crypto.ParseMasterKey(b64Key)
	if err != nil {
		t.Fatalf("failed to parse base64 key: %v", err)
	}
	if !bytes.Equal(kB64, []byte(rawKey)) {
		t.Fatalf("parsed base64 key mismatch")
	}

	// 4. Invalid formats and lengths must be rejected
	invalidCases := []struct {
		name string
		key  string
	}{
		{"empty", ""},
		{"short password", "short-key"},
		{"16 bytes", "1234567890123456"},
		{"31 bytes", "0123456789012345678901234567890"},
		{"33 bytes", "012345678901234567890123456789012"},
		{"64 chars invalid hex", "ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ"},
		{"44 chars invalid base64", "!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!"},
		{"arbitrary sentence", "this is a long passphrase that is not a master key format!"},
	}

	for _, tc := range invalidCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := crypto.ParseMasterKey(tc.key)
			if err == nil {
				t.Errorf("expected error for %s (%q), got nil", tc.name, tc.key)
			}
		})
	}
}

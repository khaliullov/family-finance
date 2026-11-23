package crypto_test

import (
	"bytes"
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

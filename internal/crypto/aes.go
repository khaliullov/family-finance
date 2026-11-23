package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
)

var (
	ErrInvalidKeyLength = errors.New("crypto: key must be 32 bytes for AES-256")
	ErrCiphertextShort  = errors.New("crypto: ciphertext too short")
	ErrDecryptionFailed = errors.New("crypto: message authentication or decryption failed")
)

const (
	KeySize   = 32 // AES-256
	NonceSize = 12 // Standard GCM nonce size
)

// Encrypt encrypts plaintext using AES-256-GCM with a freshly generated random 12-byte nonce.
// The nonce is prepended to the returned ciphertext: [nonce (12B)][ciphertext + tag (16B)].
func Encrypt(key []byte, plaintext []byte) ([]byte, error) {
	if len(key) != KeySize {
		return nil, ErrInvalidKeyLength
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("crypto: create cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: create gcm: %w", err)
	}

	nonce := make([]byte, NonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("crypto: generate nonce: %w", err)
	}

	// Seal appends the ciphertext and tag to nonce
	ciphertext := gcm.Seal(nonce, nonce, plaintext, nil)
	return ciphertext, nil
}

// Decrypt authenticates and decrypts ciphertext using AES-256-GCM.
// Expects ciphertext in the format: [nonce (12B)][ciphertext + tag (16B)].
func Decrypt(key []byte, ciphertext []byte) ([]byte, error) {
	if len(key) != KeySize {
		return nil, ErrInvalidKeyLength
	}
	if len(ciphertext) < NonceSize {
		return nil, ErrCiphertextShort
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("crypto: create cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: create gcm: %w", err)
	}

	nonce := ciphertext[:NonceSize]
	actualCiphertext := ciphertext[NonceSize:]

	plaintext, err := gcm.Open(nil, nonce, actualCiphertext, nil)
	if err != nil {
		return nil, ErrDecryptionFailed
	}

	return plaintext, nil
}

// DeriveKey derives a deterministic 32-byte AES-256 key from a master secret and salt using SHA-256.
func DeriveKey(secret string, salt []byte) []byte {
	h := sha256.New()
	h.Write(salt)
	h.Write([]byte(secret))
	return h.Sum(nil)
}

// GenerateRandomKey creates a cryptographically secure 32-byte key.
func GenerateRandomKey() ([]byte, error) {
	k := make([]byte, KeySize)
	if _, err := io.ReadFull(rand.Reader, k); err != nil {
		return nil, fmt.Errorf("crypto: generate key: %w", err)
	}
	return k, nil
}

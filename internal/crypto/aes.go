package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"golang.org/x/crypto/argon2"
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

// Default Argon2id parameters recommended for password hashing and key derivation.
const (
	Argon2Time    uint32 = 1
	Argon2Memory  uint32 = 64 * 1024 // 64 MB
	Argon2Threads uint8  = 4
	Argon2KeyLen  uint32 = 32
)

// DeriveKeyArgon2id derives a deterministic 32-byte AES-256 key from a password and salt using Argon2id.
// Salt must be at least 16 bytes to resist precomputation attacks.
func DeriveKeyArgon2id(password string, salt []byte) ([]byte, error) {
	if len(salt) < 16 {
		return nil, errors.New("crypto: salt must be at least 16 bytes for Argon2id")
	}
	if password == "" {
		return nil, errors.New("crypto: password cannot be empty")
	}
	key := argon2.IDKey([]byte(password), salt, Argon2Time, Argon2Memory, Argon2Threads, Argon2KeyLen)
	return key, nil
}

// ParseMasterKey parses a 32-byte AES-256 master key from an environment variable or secret string.
// Supports:
// - 64-character hex-encoded string
// - 44-character (or 43-character unpadded) base64-encoded string
// - Exact 32-byte raw string
// Rejects invalid formats and lengths.
func ParseMasterKey(keyStr string) ([]byte, error) {
	keyStr = strings.TrimSpace(keyStr)
	switch len(keyStr) {
	case 64:
		b, err := hex.DecodeString(keyStr)
		if err != nil {
			return nil, fmt.Errorf("crypto: invalid hex master key: %w", err)
		}
		if len(b) != KeySize {
			return nil, ErrInvalidKeyLength
		}
		return b, nil
	case 43, 44:
		b, err := base64.StdEncoding.DecodeString(keyStr)
		if err != nil {
			b, err = base64.URLEncoding.DecodeString(keyStr)
		}
		if err != nil {
			b, err = base64.RawStdEncoding.DecodeString(keyStr)
		}
		if err != nil {
			b, err = base64.RawURLEncoding.DecodeString(keyStr)
		}
		if err != nil {
			return nil, fmt.Errorf("crypto: invalid base64 master key: %w", err)
		}
		if len(b) != KeySize {
			return nil, ErrInvalidKeyLength
		}
		return b, nil
	case 32:
		return []byte(keyStr), nil
	default:
		return nil, fmt.Errorf("crypto: invalid master key: must be 32-byte raw, 64-char hex, or 44-char base64 (got %d chars)", len(keyStr))
	}
}

// DeriveKey derives a deterministic 32-byte AES-256 key from a master secret and salt using SHA-256.
// Deprecated: DeriveKey uses SHA-256 and is not a secure password-based KDF. Use DeriveKeyArgon2id for passwords or ParseMasterKey for pre-shared keys.
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

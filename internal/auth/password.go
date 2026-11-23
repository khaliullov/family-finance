package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

var (
	ErrPasswordTooShort    = errors.New("password must be at least 10 characters long")
	ErrPasswordTooLong     = errors.New("password cannot exceed 128 characters")
	ErrInvalidHash         = errors.New("the encoded hash is not in the correct format")
	ErrIncompatibleVersion = errors.New("incompatible version of argon2")
)

type PasswordConfig struct {
	Memory      uint32
	Iterations  uint32
	Parallelism uint8
	SaltLength  uint32
	KeyLength   uint32
}

// DefaultPasswordConfig implements OWASP / RFC 9106 recommended parameters for Argon2id.
var DefaultPasswordConfig = PasswordConfig{
	Memory:      64 * 1024, // 64 MB
	Iterations:  3,
	Parallelism: 4,
	SaltLength:  16,
	KeyLength:   32,
}

// dummyHash is pre-computed at package init to allow constant-time verification for non-existent users.
var dummyHash string

func init() {
	var err error
	dummyHash, err = HashPassword("dummy-password-for-anti-enumeration")
	if err != nil {
		panic(fmt.Sprintf("failed to generate dummy hash: %v", err))
	}
}

// ValidatePassword enforces password length constraints (10 to 128 chars).
func ValidatePassword(password string) error {
	l := len(password)
	if l < 10 {
		return ErrPasswordTooShort
	}
	if l > 128 {
		return ErrPasswordTooLong
	}
	return nil
}

// HashPassword hashes a password using Argon2id with DefaultPasswordConfig.
func HashPassword(password string) (string, error) {
	return HashPasswordWithConfig(password, DefaultPasswordConfig)
}

// HashPasswordWithConfig hashes a password using Argon2id with the supplied configuration.
func HashPasswordWithConfig(password string, config PasswordConfig) (string, error) {
	salt := make([]byte, config.SaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("failed to generate random salt: %w", err)
	}

	hash := argon2.IDKey([]byte(password), salt, config.Iterations, config.Memory, config.Parallelism, config.KeyLength)

	b64Salt := base64.RawStdEncoding.EncodeToString(salt)
	b64Hash := base64.RawStdEncoding.EncodeToString(hash)

	encoded := fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, config.Memory, config.Iterations, config.Parallelism, b64Salt, b64Hash)

	return encoded, nil
}

// VerifyPassword verifies a password against an Argon2id PHC encoded hash in constant time.
func VerifyPassword(password, encodedHash string) (bool, error) {
	config, salt, hash, err := decodeHash(encodedHash)
	if err != nil {
		return false, err
	}

	computedHash := argon2.IDKey([]byte(password), salt, config.Iterations, config.Memory, config.Parallelism, config.KeyLength)

	if subtle.ConstantTimeCompare(hash, computedHash) == 1 {
		return true, nil
	}
	return false, nil
}

// DummyVerify performs an Argon2id comparison against a precomputed dummy hash.
// This ensures that login requests for non-existent users consume the same computation time,
// preventing timing-based username enumeration.
func DummyVerify(password string) {
	_, _ = VerifyPassword(password, dummyHash)
}

func decodeHash(encodedHash string) (config PasswordConfig, salt, hash []byte, err error) {
	parts := strings.Split(encodedHash, "$")
	if len(parts) != 6 {
		return config, nil, nil, ErrInvalidHash
	}

	if parts[1] != "argon2id" {
		return config, nil, nil, ErrInvalidHash
	}

	var version int
	_, err = fmt.Sscanf(parts[2], "v=%d", &version)
	if err != nil {
		return config, nil, nil, ErrInvalidHash
	}
	if version != argon2.Version {
		return config, nil, nil, ErrIncompatibleVersion
	}

	var memory, iterations, parallelism uint32
	_, err = fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &iterations, &parallelism)
	if err != nil {
		return config, nil, nil, ErrInvalidHash
	}

	config.Memory = memory
	config.Iterations = iterations
	config.Parallelism = uint8(parallelism)

	salt, err = base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return config, nil, nil, ErrInvalidHash
	}
	config.SaltLength = uint32(len(salt))

	hash, err = base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return config, nil, nil, ErrInvalidHash
	}
	config.KeyLength = uint32(len(hash))

	return config, salt, hash, nil
}

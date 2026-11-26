package crypto

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"

	"github.com/google/uuid"
)

var (
	ErrEnvelopeTooShort           = errors.New("crypto: envelope too short to contain valid STMT header")
	ErrInvalidEnvelopeMagic       = errors.New("crypto: invalid envelope magic header (expected STMT)")
	ErrUnsupportedEnvelopeVersion = errors.New("crypto: unsupported envelope version")
	ErrInvalidEnvelopeTenant      = errors.New("crypto: invalid family or document identifier for envelope binding")
)

var StmtMagic = [4]byte{'S', 'T', 'M', 'T'}

const (
	StmtCurrentVersion byte = 0x01
	StmtHeaderSize          = 4 + 1 + NonceSize // 4 (magic) + 1 (version) + 12 (nonce) = 17 bytes
	StmtMinEnvelopeSize    = StmtHeaderSize + 16 // 17 + 16 (GCM tag) = 33 bytes
)

// buildStatementAAD constructs deterministic Additional Authenticated Data (AAD)
// binding the encrypted ciphertext strictly to the specified family and document tenant scope.
func buildStatementAAD(familyID, documentID uuid.UUID) []byte {
	return []byte(fmt.Sprintf("family:%s:doc:%s", familyID.String(), documentID.String()))
}

// EncryptStatementDocument encrypts document plaintext using AES-256-GCM under a versioned STMT envelope
// with cryptographic AEAD binding to familyID and documentID.
//
// Binary Layout:
// [4 Bytes Magic: "STMT"]
// [1 Byte Version: 0x01]
// [12 Bytes Nonce]
// [Ciphertext + 16 Bytes GCM Auth Tag]
func EncryptStatementDocument(key []byte, familyID, documentID uuid.UUID, plaintext []byte) ([]byte, error) {
	if len(key) != KeySize {
		return nil, ErrInvalidKeyLength
	}
	if familyID == uuid.Nil || documentID == uuid.Nil {
		return nil, ErrInvalidEnvelopeTenant
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

	aad := buildStatementAAD(familyID, documentID)

	// Pre-allocate envelope buffer: magic (4) + version (1) + nonce (12) + ciphertext + tag (16)
	envelope := make([]byte, 0, StmtHeaderSize+len(plaintext)+gcm.Overhead())
	envelope = append(envelope, StmtMagic[:]...)
	envelope = append(envelope, StmtCurrentVersion)
	envelope = append(envelope, nonce...)

	// Seal appends ciphertext and authentication tag
	envelope = gcm.Seal(envelope, nonce, plaintext, aad)

	return envelope, nil
}

// DecryptStatementDocument authenticates and decrypts an encrypted STMT document envelope.
// Validates magic bytes, envelope version, and verifies that the ciphertext belongs strictly
// to the specified familyID and documentID via AEAD AAD verification.
// Returns ErrDecryptionFailed if the ciphertext is tampered with or transplanted across tenants.
func DecryptStatementDocument(key []byte, familyID, documentID uuid.UUID, envelope []byte) ([]byte, error) {
	if len(key) != KeySize {
		return nil, ErrInvalidKeyLength
	}
	if familyID == uuid.Nil || documentID == uuid.Nil {
		return nil, ErrInvalidEnvelopeTenant
	}
	if len(envelope) < StmtMinEnvelopeSize {
		return nil, ErrEnvelopeTooShort
	}

	// 1. Verify Magic Header
	if !bytes.Equal(envelope[:4], StmtMagic[:]) {
		return nil, ErrInvalidEnvelopeMagic
	}

	// 2. Verify Envelope Version
	version := envelope[4]
	if version != StmtCurrentVersion {
		return nil, fmt.Errorf("%w: version %d", ErrUnsupportedEnvelopeVersion, version)
	}

	// 3. Extract Nonce and Ciphertext
	nonce := envelope[5 : 5+NonceSize]
	ciphertextWithTag := envelope[StmtHeaderSize:]

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("crypto: create cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: create gcm: %w", err)
	}

	aad := buildStatementAAD(familyID, documentID)

	plaintext, err := gcm.Open(nil, nonce, ciphertextWithTag, aad)
	if err != nil {
		return nil, ErrDecryptionFailed
	}

	return plaintext, nil
}

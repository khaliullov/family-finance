package crypto

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"testing"

	"github.com/google/uuid"
)

func TestStatementEnvelope_RoundTrip(t *testing.T) {
	key, err := GenerateRandomKey()
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}

	familyID := uuid.New()
	documentID := uuid.New()
	plaintext := []byte("Date,Amount,Description\n2026-01-01,150.00,Grocery Store\n")

	envelope, err := EncryptStatementDocument(key, familyID, documentID, plaintext)
	if err != nil {
		t.Fatalf("EncryptStatementDocument failed: %v", err)
	}

	decrypted, err := DecryptStatementDocument(key, familyID, documentID, envelope)
	if err != nil {
		t.Fatalf("DecryptStatementDocument failed: %v", err)
	}

	if !bytes.Equal(decrypted, plaintext) {
		t.Fatalf("decrypted plaintext does not match original: got %q, want %q", decrypted, plaintext)
	}
}

func TestStatementEnvelope_AADTamperingRejection(t *testing.T) {
	key, err := GenerateRandomKey()
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}

	familyID1 := uuid.New()
	familyID2 := uuid.New()
	documentID := uuid.New()
	plaintext := []byte("Sensitive statement data")

	envelope, err := EncryptStatementDocument(key, familyID1, documentID, plaintext)
	if err != nil {
		t.Fatalf("EncryptStatementDocument failed: %v", err)
	}

	// 1. Cross-tenant decryption attempt: different familyID
	_, err = DecryptStatementDocument(key, familyID2, documentID, envelope)
	if err == nil {
		t.Fatalf("expected ErrDecryptionFailed when decrypting with mismatched familyID, but got nil")
	}
	if !errors.Is(err, ErrDecryptionFailed) {
		t.Fatalf("expected ErrDecryptionFailed, got: %v", err)
	}

	// 2. Cross-document decryption attempt: different documentID
	otherDocID := uuid.New()
	_, err = DecryptStatementDocument(key, familyID1, otherDocID, envelope)
	if err == nil {
		t.Fatalf("expected ErrDecryptionFailed when decrypting with mismatched documentID, but got nil")
	}
	if !errors.Is(err, ErrDecryptionFailed) {
		t.Fatalf("expected ErrDecryptionFailed, got: %v", err)
	}
}

func TestStatementEnvelope_EnvelopeFormatValidation(t *testing.T) {
	key, err := GenerateRandomKey()
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}
	familyID := uuid.New()
	documentID := uuid.New()

	// 1. Truncated envelope
	shortEnvelope := []byte("STMT\x01123456789012")
	_, err = DecryptStatementDocument(key, familyID, documentID, shortEnvelope)
	if !errors.Is(err, ErrEnvelopeTooShort) {
		t.Fatalf("expected ErrEnvelopeTooShort, got: %v", err)
	}

	// 2. Corrupted magic bytes
	envelope, err := EncryptStatementDocument(key, familyID, documentID, []byte("test"))
	if err != nil {
		t.Fatalf("encrypt failed: %v", err)
	}
	corruptedMagic := append([]byte(nil), envelope...)
	corruptedMagic[0] = 'X'
	_, err = DecryptStatementDocument(key, familyID, documentID, corruptedMagic)
	if !errors.Is(err, ErrInvalidEnvelopeMagic) {
		t.Fatalf("expected ErrInvalidEnvelopeMagic, got: %v", err)
	}

	// 3. Unsupported version
	corruptedVersion := append([]byte(nil), envelope...)
	corruptedVersion[4] = 0x99
	_, err = DecryptStatementDocument(key, familyID, documentID, corruptedVersion)
	if !errors.Is(err, ErrUnsupportedEnvelopeVersion) {
		t.Fatalf("expected ErrUnsupportedEnvelopeVersion, got: %v", err)
	}

	// 4. Tampered ciphertext payload
	tamperedPayload := append([]byte(nil), envelope...)
	tamperedPayload[len(tamperedPayload)-1] ^= 0xFF
	_, err = DecryptStatementDocument(key, familyID, documentID, tamperedPayload)
	if !errors.Is(err, ErrDecryptionFailed) {
		t.Fatalf("expected ErrDecryptionFailed on tampered ciphertext, got: %v", err)
	}
}

func TestStatementEnvelope_LargePayload(t *testing.T) {
	key, err := GenerateRandomKey()
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}
	familyID := uuid.New()
	documentID := uuid.New()

	// 2 MB statement payload
	largePlaintext := make([]byte, 2*1024*1024)
	if _, err := io.ReadFull(rand.Reader, largePlaintext); err != nil {
		t.Fatalf("failed to generate random plaintext: %v", err)
	}

	envelope, err := EncryptStatementDocument(key, familyID, documentID, largePlaintext)
	if err != nil {
		t.Fatalf("EncryptStatementDocument failed for large payload: %v", err)
	}

	decrypted, err := DecryptStatementDocument(key, familyID, documentID, envelope)
	if err != nil {
		t.Fatalf("DecryptStatementDocument failed for large payload: %v", err)
	}

	if !bytes.Equal(decrypted, largePlaintext) {
		t.Fatalf("decrypted payload mismatch for large payload")
	}
}

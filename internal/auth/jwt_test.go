package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const testSecret = "super-secret-key-that-is-at-least-32-bytes-long!"

func TestNewJWTManager(t *testing.T) {
	_, err := NewJWTManager("too-short", time.Minute)
	if !errors.Is(err, ErrSecretTooWeak) {
		t.Fatalf("expected ErrSecretTooWeak, got %v", err)
	}

	mgr, err := NewJWTManager(testSecret, 0)
	if err != nil {
		t.Fatalf("expected NewJWTManager to succeed, got %v", err)
	}
	if mgr.ttl != DefaultAccessTokenTTL {
		t.Fatalf("expected default ttl %v, got %v", DefaultAccessTokenTTL, mgr.ttl)
	}
}

func TestGenerateAndValidateToken(t *testing.T) {
	mgr, err := NewJWTManager(testSecret, 15*time.Minute)
	if err != nil {
		t.Fatalf("NewJWTManager failed: %v", err)
	}

	userID := uuid.New()
	familyID := uuid.New()
	email := "alex@example.com"
	role := "owner"

	tokenStr, exp, err := mgr.GenerateToken(userID, familyID, email, role)
	if err != nil {
		t.Fatalf("GenerateToken failed: %v", err)
	}
	if tokenStr == "" {
		t.Fatalf("expected non-empty token string")
	}
	if exp.Before(time.Now()) {
		t.Fatalf("expected token expiration in the future")
	}

	claims, err := mgr.ValidateToken(tokenStr)
	if err != nil {
		t.Fatalf("ValidateToken failed: %v", err)
	}

	if claims.UserID != userID {
		t.Errorf("expected UserID %v, got %v", userID, claims.UserID)
	}
	if claims.FamilyID != familyID {
		t.Errorf("expected FamilyID %v, got %v", familyID, claims.FamilyID)
	}
	if claims.Email != email {
		t.Errorf("expected Email %v, got %v", email, claims.Email)
	}
	if claims.Role != role {
		t.Errorf("expected Role %v, got %v", role, claims.Role)
	}
	if claims.Issuer != DefaultIssuer {
		t.Errorf("expected Issuer %v, got %v", DefaultIssuer, claims.Issuer)
	}
}

func TestValidateToken_Expired(t *testing.T) {
	mgr, err := NewJWTManager(testSecret, -5*time.Minute)
	if err != nil {
		t.Fatalf("NewJWTManager failed: %v", err)
	}

	tokenStr, _, err := mgr.GenerateToken(uuid.New(), uuid.New(), "test@example.com", "owner")
	if err != nil {
		t.Fatalf("GenerateToken failed: %v", err)
	}

	_, err = mgr.ValidateToken(tokenStr)
	if !errors.Is(err, ErrExpiredToken) {
		t.Fatalf("expected ErrExpiredToken, got %v", err)
	}
}

func TestValidateToken_WrongSecret(t *testing.T) {
	mgr1, _ := NewJWTManager(testSecret, 15*time.Minute)
	mgr2, _ := NewJWTManager("another-super-secret-key-that-is-32-bytes-long!", 15*time.Minute)

	tokenStr, _, err := mgr1.GenerateToken(uuid.New(), uuid.New(), "test@example.com", "owner")
	if err != nil {
		t.Fatalf("GenerateToken failed: %v", err)
	}

	_, err = mgr2.ValidateToken(tokenStr)
	if !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expected ErrInvalidToken for wrong secret, got %v", err)
	}
}

func TestValidateToken_RejectsNoneAlgorithm(t *testing.T) {
	claims := Claims{
		UserID:   uuid.New(),
		FamilyID: uuid.New(),
		Role:     "owner",
		Email:    "test@example.com",
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:  uuid.New().String(),
			Issuer:   DefaultIssuer,
			Audience: jwt.ClaimStrings{DefaultAudience},
		},
	}

	// Sign with None
	token := jwt.NewWithClaims(jwt.SigningMethodNone, claims)
	tokenStr, err := token.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("failed to sign with none: %v", err)
	}

	mgr, _ := NewJWTManager(testSecret, 15*time.Minute)
	_, err = mgr.ValidateToken(tokenStr)
	if err == nil {
		t.Fatalf("expected error rejecting 'none' algorithm, got nil")
	}
}

func TestValidateToken_RejectsRSAAlgorithm(t *testing.T) {
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey failed: %v", err)
	}

	claims := Claims{
		UserID:   uuid.New(),
		FamilyID: uuid.New(),
		Role:     "owner",
		Email:    "test@example.com",
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:  uuid.New().String(),
			Issuer:   DefaultIssuer,
			Audience: jwt.ClaimStrings{DefaultAudience},
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tokenStr, err := token.SignedString(privKey)
	if err != nil {
		t.Fatalf("failed to sign with RSA: %v", err)
	}

	mgr, _ := NewJWTManager(testSecret, 15*time.Minute)
	_, err = mgr.ValidateToken(tokenStr)
	if err == nil {
		t.Fatalf("expected error rejecting 'RS256' algorithm, got nil")
	}
}

func TestValidateToken_RejectsInvalidIssuerAudience(t *testing.T) {
	mgr, _ := NewJWTManager(testSecret, 15*time.Minute)

	// Invalid issuer
	claimsBadIssuer := Claims{
		UserID:   uuid.New(),
		FamilyID: uuid.New(),
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:  uuid.New().String(),
			Issuer:   "malicious-issuer",
			Audience: jwt.ClaimStrings{DefaultAudience},
		},
	}
	tok1 := jwt.NewWithClaims(jwt.SigningMethodHS256, claimsBadIssuer)
	str1, _ := tok1.SignedString([]byte(testSecret))
	if _, err := mgr.ValidateToken(str1); err == nil {
		t.Fatalf("expected validation failure for wrong issuer")
	}

	// Invalid audience
	claimsBadAud := Claims{
		UserID:   uuid.New(),
		FamilyID: uuid.New(),
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:  uuid.New().String(),
			Issuer:   DefaultIssuer,
			Audience: jwt.ClaimStrings{"wrong-aud"},
		},
	}
	tok2 := jwt.NewWithClaims(jwt.SigningMethodHS256, claimsBadAud)
	str2, _ := tok2.SignedString([]byte(testSecret))
	if _, err := mgr.ValidateToken(str2); err == nil {
		t.Fatalf("expected validation failure for wrong audience")
	}
}

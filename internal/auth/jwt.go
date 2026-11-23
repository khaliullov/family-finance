package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

var (
	ErrInvalidToken    = errors.New("invalid token")
	ErrExpiredToken    = errors.New("token has expired")
	ErrUnexpectedAlg   = errors.New("unexpected signing algorithm")
	ErrSecretTooWeak   = errors.New("jwt secret must be at least 32 bytes")
)

const (
	DefaultIssuer         = "family-finance"
	DefaultAudience       = "family-finance-api"
	DefaultAccessTokenTTL = 15 * time.Minute
)

// Claims represents the JWT payload claims for an authenticated user session.
type Claims struct {
	UserID   uuid.UUID `json:"user_id"`
	FamilyID uuid.UUID `json:"family_id"`
	Role     string    `json:"role"`
	Email    string    `json:"email"`
	jwt.RegisteredClaims
}

// JWTManager handles signing and verifying access tokens with strict HS256 validation.
type JWTManager struct {
	secretKey []byte
	issuer    string
	audience  string
	ttl       time.Duration
}

// NewJWTManager creates a new JWTManager verifying that the secret is at least 32 bytes long.
func NewJWTManager(secretKey string, ttl time.Duration) (*JWTManager, error) {
	if len(secretKey) < 32 {
		return nil, ErrSecretTooWeak
	}
	if ttl == 0 {
		ttl = DefaultAccessTokenTTL
	}
	return &JWTManager{
		secretKey: []byte(secretKey),
		issuer:    DefaultIssuer,
		audience:  DefaultAudience,
		ttl:       ttl,
	}, nil
}

// GenerateToken creates and signs a new JWT access token.
func (m *JWTManager) GenerateToken(userID, familyID uuid.UUID, email, role string) (string, time.Time, error) {
	now := time.Now().UTC()
	expiresAt := now.Add(m.ttl)

	claims := Claims{
		UserID:   userID,
		FamilyID: familyID,
		Role:     role,
		Email:    email,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID.String(),
			Issuer:    m.issuer,
			Audience:  jwt.ClaimStrings{m.audience},
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			ID:        uuid.New().String(),
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signedToken, err := token.SignedString(m.secretKey)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("failed to sign token: %w", err)
	}

	return signedToken, expiresAt, nil
}

// ValidateToken parses, validates signature and claims of the access token.
func (m *JWTManager) ValidateToken(tokenStr string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &Claims{}, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok || token.Method.Alg() != jwt.SigningMethodHS256.Alg() {
			return nil, fmt.Errorf("%w: %v", ErrUnexpectedAlg, token.Header["alg"])
		}
		return m.secretKey, nil
	},
		jwt.WithIssuer(m.issuer),
		jwt.WithAudience(m.audience),
		jwt.WithValidMethods([]string{"HS256"}),
	)

	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, ErrExpiredToken
		}
		return nil, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}

	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		return nil, ErrInvalidToken
	}

	if claims.Subject != claims.UserID.String() {
		return nil, fmt.Errorf("%w: subject mismatch", ErrInvalidToken)
	}

	return claims, nil
}

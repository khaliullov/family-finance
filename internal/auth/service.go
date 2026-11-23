package auth

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	ErrInvalidEmail         = errors.New("invalid email address")
	ErrInvalidCredentials   = errors.New("invalid email or password")
	ErrMissingRequiredField = errors.New("missing required field")
)

type RegisterRequest struct {
	Email           string `json:"email"`
	Password        string `json:"password"`
	FirstName       string `json:"first_name"`
	LastName        string `json:"last_name,omitempty"`
	FamilyName      string `json:"family_name"`
	DefaultCurrency string `json:"default_currency,omitempty"`
}

type LoginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type RefreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

type LogoutRequest struct {
	RefreshToken string `json:"refresh_token,omitempty"`
}

type AuthResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"` // in seconds, e.g. 900
	User         User   `json:"user"`
	Family       Family `json:"family"`
}

type UserProfileResponse struct {
	User    User           `json:"user"`
	Family  Family         `json:"family"`
	Members []FamilyMember `json:"members"`
}

type Service struct {
	repo       Repository
	jwtManager *JWTManager
}

func NewService(repo Repository, jwtManager *JWTManager) *Service {
	return &Service{
		repo:       repo,
		jwtManager: jwtManager,
	}
}

func (s *Service) Register(ctx context.Context, req RegisterRequest, userAgent, ipAddress string) (*AuthResponse, error) {
	req.Email = strings.TrimSpace(req.Email)
	if _, err := mail.ParseAddress(req.Email); err != nil {
		return nil, ErrInvalidEmail
	}

	if err := ValidatePassword(req.Password); err != nil {
		return nil, err
	}

	req.FirstName = strings.TrimSpace(req.FirstName)
	if req.FirstName == "" {
		return nil, fmt.Errorf("%w: first_name", ErrMissingRequiredField)
	}

	req.FamilyName = strings.TrimSpace(req.FamilyName)
	if req.FamilyName == "" {
		return nil, fmt.Errorf("%w: family_name", ErrMissingRequiredField)
	}

	hash, err := HashPassword(req.Password)
	if err != nil {
		return nil, fmt.Errorf("failed to hash password: %w", err)
	}

	rawRefreshToken, refreshHash, err := GenerateRefreshToken()
	if err != nil {
		return nil, fmt.Errorf("failed to generate refresh token: %w", err)
	}

	sessionExp := time.Now().Add(DefaultRefreshTokenTTL)

	params := RegisterBootstrapParams{
		Email:            req.Email,
		PasswordHash:     hash,
		FirstName:        req.FirstName,
		LastName:         strings.TrimSpace(req.LastName),
		FamilyName:       req.FamilyName,
		DefaultCurrency:  req.DefaultCurrency,
		UserAgent:        userAgent,
		IPAddress:        ipAddress,
		RefreshTokenHash: refreshHash,
		SessionExpiresAt: sessionExp,
	}

	res, err := s.repo.RegisterBootstrap(ctx, params)
	if err != nil {
		return nil, err
	}

	accessToken, exp, err := s.jwtManager.GenerateToken(res.User.ID, res.Family.ID, res.User.Email, res.Role)
	if err != nil {
		return nil, fmt.Errorf("failed to generate access token: %w", err)
	}

	return &AuthResponse{
		AccessToken:  accessToken,
		RefreshToken: rawRefreshToken,
		TokenType:    "Bearer",
		ExpiresIn:    int(time.Until(exp).Seconds()),
		User:         res.User,
		Family:       res.Family,
	}, nil
}

func (s *Service) Login(ctx context.Context, req LoginRequest, userAgent, ipAddress string) (*AuthResponse, error) {
	req.Email = strings.TrimSpace(req.Email)
	if req.Email == "" || req.Password == "" {
		return nil, ErrInvalidCredentials
	}

	cred, err := s.repo.GetCredential(ctx, req.Email)
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve credentials: %w", err)
	}

	if cred == nil {
		// Anti-enumeration: execute dummy verify to burn matching Argon2id computation time
		DummyVerify(req.Password)
		return nil, ErrInvalidCredentials
	}

	if !cred.IsActive {
		DummyVerify(req.Password)
		return nil, ErrInvalidCredentials
	}

	match, err := VerifyPassword(req.Password, cred.PasswordHash)
	if err != nil || !match {
		return nil, ErrInvalidCredentials
	}

	user, err := s.repo.GetUser(ctx, cred.UserID)
	if err != nil {
		return nil, fmt.Errorf("failed to get user: %w", err)
	}

	memberships, err := s.repo.GetFamilyMemberships(ctx, cred.UserID)
	if err != nil || len(memberships) == 0 {
		return nil, fmt.Errorf("user does not belong to any family")
	}

	primaryFamily := memberships[0]

	rawRefreshToken, refreshHash, err := GenerateRefreshToken()
	if err != nil {
		return nil, fmt.Errorf("failed to generate refresh token: %w", err)
	}

	sessionID := uuid.New()
	sessionChainID := uuid.New()
	sessionExp := time.Now().Add(DefaultRefreshTokenTTL)

	err = s.repo.CreateSession(ctx, Session{
		ID:             sessionID,
		UserID:         user.ID,
		FamilyID:       primaryFamily.FamilyID,
		TokenHash:      refreshHash,
		SessionChainID: sessionChainID,
		UserAgent:      userAgent,
		IPAddress:      ipAddress,
		ExpiresAt:      sessionExp,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create session: %w", err)
	}

	accessToken, exp, err := s.jwtManager.GenerateToken(user.ID, primaryFamily.FamilyID, user.Email, primaryFamily.Role)
	if err != nil {
		return nil, fmt.Errorf("failed to generate access token: %w", err)
	}

	return &AuthResponse{
		AccessToken:  accessToken,
		RefreshToken: rawRefreshToken,
		TokenType:    "Bearer",
		ExpiresIn:    int(time.Until(exp).Seconds()),
		User:         *user,
		Family: Family{
			ID:              primaryFamily.FamilyID,
			Name:            primaryFamily.FamilyName,
			DefaultCurrency: primaryFamily.DefaultCurrency,
		},
	}, nil
}

func (s *Service) RefreshToken(ctx context.Context, rawRefreshToken string, userAgent, ipAddress string) (*AuthResponse, error) {
	rawRefreshToken = strings.TrimSpace(rawRefreshToken)
	if rawRefreshToken == "" {
		return nil, ErrSessionNotFound
	}

	oldHash := HashSessionToken(rawRefreshToken)
	newRawToken, newHash, err := GenerateRefreshToken()
	if err != nil {
		return nil, fmt.Errorf("failed to generate new refresh token: %w", err)
	}

	newExp := time.Now().Add(DefaultRefreshTokenTTL)
	rotRes, err := s.repo.RotateSession(ctx, oldHash, newHash, newExp, userAgent, ipAddress)
	if err != nil {
		return nil, err
	}

	user, err := s.repo.GetUser(ctx, rotRes.UserID)
	if err != nil {
		return nil, fmt.Errorf("failed to load user: %w", err)
	}

	family, err := s.repo.GetFamily(ctx, rotRes.FamilyID, rotRes.UserID)
	if err != nil {
		return nil, fmt.Errorf("failed to load family: %w", err)
	}

	memberships, err := s.repo.GetFamilyMemberships(ctx, rotRes.UserID)
	if err != nil {
		return nil, fmt.Errorf("failed to load memberships: %w", err)
	}
	role := "member"
	for _, m := range memberships {
		if m.FamilyID == rotRes.FamilyID {
			role = m.Role
			break
		}
	}

	accessToken, exp, err := s.jwtManager.GenerateToken(user.ID, family.ID, user.Email, role)
	if err != nil {
		return nil, fmt.Errorf("failed to sign access token: %w", err)
	}

	return &AuthResponse{
		AccessToken:  accessToken,
		RefreshToken: newRawToken,
		TokenType:    "Bearer",
		ExpiresIn:    int(time.Until(exp).Seconds()),
		User:         *user,
		Family:       *family,
	}, nil
}

func (s *Service) Logout(ctx context.Context, rawRefreshToken string) error {
	rawRefreshToken = strings.TrimSpace(rawRefreshToken)
	if rawRefreshToken == "" {
		return nil
	}
	hash := HashSessionToken(rawRefreshToken)
	err := s.repo.RevokeSessionByToken(ctx, hash)
	if err != nil && !errors.Is(err, ErrSessionNotFound) {
		return err
	}
	return nil
}

func (s *Service) GetMe(ctx context.Context, userID, familyID uuid.UUID) (*UserProfileResponse, error) {
	user, err := s.repo.GetUser(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to load user: %w", err)
	}

	family, err := s.repo.GetFamily(ctx, familyID, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to load family: %w", err)
	}

	members, err := s.repo.GetFamilyMembers(ctx, familyID, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to load family members: %w", err)
	}

	return &UserProfileResponse{
		User:    *user,
		Family:  *family,
		Members: members,
	}, nil
}

func (s *Service) ListSessions(ctx context.Context, userID uuid.UUID) ([]SessionDTO, error) {
	return s.repo.ListActiveSessions(ctx, userID)
}

func (s *Service) RevokeSession(ctx context.Context, userID, sessionID uuid.UUID) error {
	return s.repo.RevokeSessionByID(ctx, userID, sessionID)
}

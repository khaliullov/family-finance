package auth

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
)

func setupTestServer(t *testing.T) (*httptest.Server, *Service, *JWTManager) {
	pool := getAppPool(t)
	repo := NewRepository(pool)
	jwtMgr, err := NewJWTManager(testSecret, 15*time.Minute)
	if err != nil {
		t.Fatalf("NewJWTManager failed: %v", err)
	}

	service := NewService(repo, jwtMgr)
	handler := NewHandler(service)
	authMw := AuthMiddleware(jwtMgr, repo)

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux, authMw)

	server := httptest.NewServer(mux)
	t.Cleanup(func() {
		server.Close()
		pool.Close()
	})

	return server, service, jwtMgr
}

func TestHandler_RegisterAndLogin_Flow(t *testing.T) {
	server, _, _ := setupTestServer(t)

	uniqueID := uuid.New().String()[:8]
	email := fmt.Sprintf("e2e-%s@example.com", uniqueID)
	password := "SecurePassword123!"

	// 1. Validation error: password too short
	badReg := RegisterRequest{
		Email:      email,
		Password:   "short",
		FirstName:  "E2E",
		FamilyName: "Test Family",
	}
	body, _ := json.Marshal(badReg)
	resp, err := http.Post(server.URL+"/api/v1/auth/register", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST register failed: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for short password, got %d", resp.StatusCode)
	}

	// 2. Successful Registration
	validReg := RegisterRequest{
		Email:           email,
		Password:        password,
		FirstName:       "E2E",
		LastName:        "Tester",
		FamilyName:      "E2E Family Space",
		DefaultCurrency: "RUB",
	}
	body, _ = json.Marshal(validReg)
	resp, err = http.Post(server.URL+"/api/v1/auth/register", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST register failed: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d", resp.StatusCode)
	}

	var regAuth AuthResponse
	if err := json.NewDecoder(resp.Body).Decode(&regAuth); err != nil {
		t.Fatalf("failed to decode register response: %v", err)
	}

	if regAuth.AccessToken == "" || regAuth.RefreshToken == "" {
		t.Fatalf("expected access and refresh tokens")
	}
	if regAuth.User.Email != email {
		t.Errorf("expected email %q, got %q", email, regAuth.User.Email)
	}
	if regAuth.Family.Name != "E2E Family Space" {
		t.Errorf("expected family name 'E2E Family Space', got %q", regAuth.Family.Name)
	}

	// 3. Duplicate email returns 409 Conflict
	resp, err = http.Post(server.URL+"/api/v1/auth/register", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST register duplicate failed: %v", err)
	}
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 Conflict for duplicate email, got %d", resp.StatusCode)
	}

	// 4. Login with wrong password returns 401
	badLogin := LoginRequest{
		Email:    email,
		Password: "WrongPassword!",
	}
	body, _ = json.Marshal(badLogin)
	resp, err = http.Post(server.URL+"/api/v1/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST login failed: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized for wrong password, got %d", resp.StatusCode)
	}

	// 5. Login with non-existent email returns 401 (anti-enumeration consistency)
	nonLogin := LoginRequest{
		Email:    "nonexistent@domain.com",
		Password: "SomePassword123!",
	}
	body, _ = json.Marshal(nonLogin)
	resp, err = http.Post(server.URL+"/api/v1/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST login failed: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized for nonexistent user, got %d", resp.StatusCode)
	}

	// 6. Login with correct credentials returns 200 OK
	correctLogin := LoginRequest{
		Email:    email,
		Password: password,
	}
	body, _ = json.Marshal(correctLogin)
	resp, err = http.Post(server.URL+"/api/v1/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST login failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}

	var loginAuth AuthResponse
	if err := json.NewDecoder(resp.Body).Decode(&loginAuth); err != nil {
		t.Fatalf("failed to decode login response: %v", err)
	}
	if loginAuth.AccessToken == "" || loginAuth.RefreshToken == "" {
		t.Fatalf("expected access and refresh tokens from login")
	}
}

func TestHandler_RefreshTokenRotationAndReplay(t *testing.T) {
	server, _, _ := setupTestServer(t)

	uniqueID := uuid.New().String()[:8]
	email := fmt.Sprintf("rot-%s@example.com", uniqueID)
	password := "SecurePassword123!"

	// Register user
	body, _ := json.Marshal(RegisterRequest{
		Email:      email,
		Password:   password,
		FirstName:  "Rotation",
		FamilyName: "Rotation Fam",
	})
	resp, err := http.Post(server.URL+"/api/v1/auth/register", "application/json", bytes.NewReader(body))
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("register failed: %v", err)
	}

	var regAuth AuthResponse
	_ = json.NewDecoder(resp.Body).Decode(&regAuth)
	token1 := regAuth.RefreshToken

	// 1. Valid Refresh (token1 -> token2)
	refreshReq, _ := json.Marshal(RefreshRequest{RefreshToken: token1})
	resp, err = http.Post(server.URL+"/api/v1/auth/refresh", "application/json", bytes.NewReader(refreshReq))
	if err != nil {
		t.Fatalf("POST refresh failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for valid refresh, got %d", resp.StatusCode)
	}

	var refAuth AuthResponse
	_ = json.NewDecoder(resp.Body).Decode(&refAuth)
	token2 := refAuth.RefreshToken
	if token2 == "" || token2 == token1 {
		t.Fatalf("expected rotated distinct refresh token")
	}

	// 2. Replay Detection: Reusing token1 must fail with 401 and trigger chain revocation
	resp, err = http.Post(server.URL+"/api/v1/auth/refresh", "application/json", bytes.NewReader(refreshReq))
	if err != nil {
		t.Fatalf("POST refresh replay failed: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized for replayed token, got %d", resp.StatusCode)
	}

	// 3. Attempting to use token2 must now ALSO be rejected (chain was revoked!)
	refreshReq2, _ := json.Marshal(RefreshRequest{RefreshToken: token2})
	resp, err = http.Post(server.URL+"/api/v1/auth/refresh", "application/json", bytes.NewReader(refreshReq2))
	if err != nil {
		t.Fatalf("POST refresh token2 failed: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized for token2 after chain revocation, got %d", resp.StatusCode)
	}
}

func TestHandler_GetMe_And_TenantBoundaries(t *testing.T) {
	server, _, _ := setupTestServer(t)

	uniqueID := uuid.New().String()[:8]
	email := fmt.Sprintf("me-%s@example.com", uniqueID)
	password := "SecurePassword123!"

	// Register user
	body, _ := json.Marshal(RegisterRequest{
		Email:      email,
		Password:   password,
		FirstName:  "Alice",
		LastName:   "Wonderland",
		FamilyName: "Alice Household",
	})
	resp, _ := http.Post(server.URL+"/api/v1/auth/register", "application/json", bytes.NewReader(body))
	var authRes AuthResponse
	_ = json.NewDecoder(resp.Body).Decode(&authRes)

	// 1. Unauthenticated request to GET /me returns 401
	req, _ := http.NewRequest(http.MethodGet, server.URL+"/api/v1/auth/me", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /me unauthenticated failed: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized, got %d", resp.StatusCode)
	}

	// 2. Authenticated request to GET /me returns 200 with profile
	req, _ = http.NewRequest(http.MethodGet, server.URL+"/api/v1/auth/me", nil)
	req.Header.Set("Authorization", "Bearer "+authRes.AccessToken)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /me authenticated failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}

	var profile UserProfileResponse
	if err := json.NewDecoder(resp.Body).Decode(&profile); err != nil {
		t.Fatalf("failed to decode profile: %v", err)
	}
	if profile.User.Email != email {
		t.Errorf("expected email %q, got %q", email, profile.User.Email)
	}
	if profile.Family.Name != "Alice Household" {
		t.Errorf("expected family 'Alice Household', got %q", profile.Family.Name)
	}
	if len(profile.Members) != 1 || profile.Members[0].Role != "owner" {
		t.Errorf("expected 1 owner member, got %+v", profile.Members)
	}

	// 3. Client attempts to switch to an unauthorized arbitrary family via X-Family-ID header
	randomFamilyID := uuid.New()
	req, _ = http.NewRequest(http.MethodGet, server.URL+"/api/v1/auth/me", nil)
	req.Header.Set("Authorization", "Bearer "+authRes.AccessToken)
	req.Header.Set("X-Family-ID", randomFamilyID.String())
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /me with unauthorized X-Family-ID failed: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for unauthorized family switch, got %d", resp.StatusCode)
	}
}

func TestHandler_SessionsManagement_And_Logout(t *testing.T) {
	server, _, _ := setupTestServer(t)

	uniqueID := uuid.New().String()[:8]
	email := fmt.Sprintf("sess-%s@example.com", uniqueID)
	password := "SecurePassword123!"

	// Register user
	body, _ := json.Marshal(RegisterRequest{
		Email:      email,
		Password:   password,
		FirstName:  "Session",
		FamilyName: "Session Space",
	})
	resp, _ := http.Post(server.URL+"/api/v1/auth/register", "application/json", bytes.NewReader(body))
	var authRes AuthResponse
	_ = json.NewDecoder(resp.Body).Decode(&authRes)

	// 1. List active sessions: 1 session from registration
	req, _ := http.NewRequest(http.MethodGet, server.URL+"/api/v1/auth/sessions", nil)
	req.Header.Set("Authorization", "Bearer "+authRes.AccessToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /sessions failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}

	var sessions []SessionDTO
	_ = json.NewDecoder(resp.Body).Decode(&sessions)
	if len(sessions) != 1 {
		t.Fatalf("expected 1 active session, got %d", len(sessions))
	}
	sessionID := sessions[0].ID

	// 2. Revoke session via DELETE /sessions/{id}
	delReq, _ := http.NewRequest(http.MethodDelete, fmt.Sprintf("%s/api/v1/auth/sessions/%s", server.URL, sessionID), nil)
	delReq.Header.Set("Authorization", "Bearer "+authRes.AccessToken)
	resp, err = http.DefaultClient.Do(delReq)
	if err != nil {
		t.Fatalf("DELETE /sessions/{id} failed: %v", err)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 No Content on revocation, got %d", resp.StatusCode)
	}

	// 3. Verify session is no longer in active list
	req, _ = http.NewRequest(http.MethodGet, server.URL+"/api/v1/auth/sessions", nil)
	req.Header.Set("Authorization", "Bearer "+authRes.AccessToken)
	resp, _ = http.DefaultClient.Do(req)
	var remaining []SessionDTO
	_ = json.NewDecoder(resp.Body).Decode(&remaining)
	if len(remaining) != 0 {
		t.Fatalf("expected 0 active sessions after revocation, got %d", len(remaining))
	}

	// 4. Test Logout with refresh token
	// First login again to get a fresh session
	loginBody, _ := json.Marshal(LoginRequest{Email: email, Password: password})
	resp, _ = http.Post(server.URL+"/api/v1/auth/login", "application/json", bytes.NewReader(loginBody))
	var loginAuth AuthResponse
	_ = json.NewDecoder(resp.Body).Decode(&loginAuth)

	logoutReq, _ := json.Marshal(LogoutRequest{RefreshToken: loginAuth.RefreshToken})
	resp, err = http.Post(server.URL+"/api/v1/auth/logout", "application/json", bytes.NewReader(logoutReq))
	if err != nil {
		t.Fatalf("POST /logout failed: %v", err)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 No Content for logout, got %d", resp.StatusCode)
	}

	// Refreshing with the logged-out token must fail
	refReq, _ := json.Marshal(RefreshRequest{RefreshToken: loginAuth.RefreshToken})
	resp, _ = http.Post(server.URL+"/api/v1/auth/refresh", "application/json", bytes.NewReader(refReq))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized after logout, got %d", resp.StatusCode)
	}
}

package transaction

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/khaliullov/family-finance/internal/auth"
	"github.com/khaliullov/family-finance/internal/connector"
	"github.com/khaliullov/family-finance/internal/connector/fixture"
)

func setupTestRouter(t *testing.T, cryptoKey []byte) (*http.ServeMux, *auth.JWTManager, func(userID, familyID uuid.UUID) string) {
	appPool := getAppPool(t)

	registry := connector.NewRegistry()
	registry.Register(fixture.NewFixtureConnector())

	svc := NewService(appPool, registry)
	if len(cryptoKey) == 32 {
		svc.SetCryptoKey(cryptoKey)
	}

	h := NewHandler(appPool, svc)
	h.SetRegistry(registry)
	if len(cryptoKey) == 32 {
		h.SetCryptoKey(cryptoKey)
	}

	jwtMgr, err := auth.NewJWTManagerWithLeeway("test_secret_32_bytes_super_secure!", 15*time.Minute, 10*time.Second)
	if err != nil {
		t.Fatalf("jwtManager init failed: %v", err)
	}

	authRepo := auth.NewRepository(appPool)
	authMw := auth.AuthMiddleware(jwtMgr, authRepo)

	mux := http.NewServeMux()
	h.RegisterRoutes(mux, authMw)

	tokenGen := func(userID, familyID uuid.UUID) string {
		tok, _, err := jwtMgr.GenerateToken(userID, familyID, "user@test.org", "owner")
		if err != nil {
			t.Fatalf("token generation failed: %v", err)
		}
		return tok
	}

	return mux, jwtMgr, tokenGen
}

func TestHandler_ProvidersList(t *testing.T) {
	mux, _, tokenGen := setupTestRouter(t, nil)
	familyID, userID, _ := setupTenantFixture(t, "fixture")
	token := tokenGen(userID, familyID)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/connections/providers", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}

	var providers []ProviderInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &providers); err != nil {
		t.Fatalf("failed to unmarshal providers: %v", err)
	}

	foundTBank := false
	foundFixture := false
	for _, p := range providers {
		if p.Code == "tbank_personal" {
			foundTBank = true
		}
		if p.Code == "fixture" {
			foundFixture = true
		}
	}

	if !foundTBank || !foundFixture {
		t.Errorf("expected tbank_personal and fixture providers, got: %+v", providers)
	}
}

func TestHandler_ConnectionLifecycle_FullWorkflow(t *testing.T) {
	cryptoKey := []byte("01234567890123456789012345678901")
	mux, _, tokenGen := setupTestRouter(t, cryptoKey)
	familyID, userID, _ := setupTenantFixture(t, "fixture")
	token := tokenGen(userID, familyID)

	// 1. Create a new connection
	createPayload := `{"provider": "fixture", "name": "Primary Fixture Account"}`
	reqCreate := httptest.NewRequest(http.MethodPost, "/api/v1/connections", bytes.NewBufferString(createPayload))
	reqCreate.Header.Set("Authorization", "Bearer "+token)
	reqCreate.Header.Set("Content-Type", "application/json")
	recCreate := httptest.NewRecorder()
	mux.ServeHTTP(recCreate, reqCreate)

	if recCreate.Code != http.StatusCreated {
		t.Fatalf("create connection failed with %d: %s", recCreate.Code, recCreate.Body.String())
	}

	var conn ConnectionResponse
	if err := json.Unmarshal(recCreate.Body.Bytes(), &conn); err != nil {
		t.Fatalf("unmarshal created connection failed: %v", err)
	}
	if conn.Status != ConnectionStateCreated {
		t.Errorf("expected initial status %q, got %q", ConnectionStateCreated, conn.Status)
	}

	connID := conn.ID

	// 2. Cannot sync while status is 'created' (fail closed)
	reqSyncPre := httptest.NewRequest(http.MethodPost, "/api/v1/connections/"+connID.String()+"/sync", nil)
	reqSyncPre.Header.Set("Authorization", "Bearer "+token)
	recSyncPre := httptest.NewRecorder()
	mux.ServeHTTP(recSyncPre, reqSyncPre)
	if recSyncPre.Code != http.StatusConflict {
		t.Errorf("expected 409 Conflict when syncing unauthenticated connection, got %d", recSyncPre.Code)
	}

	// 3. Cannot validate while status is 'created' (fail closed)
	reqValPre := httptest.NewRequest(http.MethodPost, "/api/v1/connections/"+connID.String()+"/validate", nil)
	reqValPre.Header.Set("Authorization", "Bearer "+token)
	recValPre := httptest.NewRecorder()
	mux.ServeHTTP(recValPre, reqValPre)
	if recValPre.Code != http.StatusConflict {
		t.Errorf("expected 409 Conflict when validating unauthenticated connection, got %d", recValPre.Code)
	}

	// 4. Start authentication attempt
	reqStart := httptest.NewRequest(http.MethodPost, "/api/v1/connections/"+connID.String()+"/auth/start", nil)
	reqStart.Header.Set("Authorization", "Bearer "+token)
	recStart := httptest.NewRecorder()
	mux.ServeHTTP(recStart, reqStart)
	if recStart.Code != http.StatusOK {
		t.Fatalf("auth start failed with %d: %s", recStart.Code, recStart.Body.String())
	}

	// 5. Check auth status -> authenticating
	reqStatus := httptest.NewRequest(http.MethodGet, "/api/v1/connections/"+connID.String()+"/auth/status", nil)
	reqStatus.Header.Set("Authorization", "Bearer "+token)
	recStatus := httptest.NewRecorder()
	mux.ServeHTTP(recStatus, reqStatus)
	if recStatus.Code != http.StatusOK {
		t.Fatalf("auth status failed with %d: %s", recStatus.Code, recStatus.Body.String())
	}

	var statusResp map[string]any
	_ = json.Unmarshal(recStatus.Body.Bytes(), &statusResp)
	if statusResp["status"] != ConnectionStateAuthenticating || statusResp["is_authenticated"] != false {
		t.Errorf("unexpected auth status: %+v", statusResp)
	}

	// 6. Seed valid session
	sessionPayload := `{"credentials": "valid_session"}`
	reqSession := httptest.NewRequest(http.MethodPost, "/api/v1/connections/"+connID.String()+"/auth/session", bytes.NewBufferString(sessionPayload))
	reqSession.Header.Set("Authorization", "Bearer "+token)
	reqSession.Header.Set("Content-Type", "application/json")
	recSession := httptest.NewRecorder()
	mux.ServeHTTP(recSession, reqSession)
	if recSession.Code != http.StatusOK {
		t.Fatalf("auth session failed with %d: %s", recSession.Code, recSession.Body.String())
	}

	// 7. Check auth status -> connected
	reqStatus2 := httptest.NewRequest(http.MethodGet, "/api/v1/connections/"+connID.String()+"/auth/status", nil)
	reqStatus2.Header.Set("Authorization", "Bearer "+token)
	recStatus2 := httptest.NewRecorder()
	mux.ServeHTTP(recStatus2, reqStatus2)
	_ = json.Unmarshal(recStatus2.Body.Bytes(), &statusResp)
	if statusResp["status"] != ConnectionStateConnected || statusResp["is_authenticated"] != true {
		t.Errorf("expected connected and authenticated, got %+v", statusResp)
	}

	// 8. Trigger synchronous validation
	reqVal := httptest.NewRequest(http.MethodPost, "/api/v1/connections/"+connID.String()+"/validate?sync=true", nil)
	reqVal.Header.Set("Authorization", "Bearer "+token)
	recVal := httptest.NewRecorder()
	mux.ServeHTTP(recVal, reqVal)
	if recVal.Code != http.StatusOK {
		t.Fatalf("validation failed with %d: %s", recVal.Code, recVal.Body.String())
	}

	var report ValidationReport
	if err := json.Unmarshal(recVal.Body.Bytes(), &report); err != nil {
		t.Fatalf("unmarshal validation report failed: %v", err)
	}
	if report.OverallResult != ValidationResultPassed {
		t.Errorf("overall result = %s, want passed", report.OverallResult)
	}
	if report.Accounts.Valid == 0 || report.Balances.Checked == 0 || report.Transactions.Valid == 0 {
		t.Errorf("unexpected validation report counts: %+v", report)
	}

	// 9. Query validation status
	reqValStatus := httptest.NewRequest(http.MethodGet, "/api/v1/connections/"+connID.String()+"/validation/status", nil)
	reqValStatus.Header.Set("Authorization", "Bearer "+token)
	recValStatus := httptest.NewRecorder()
	mux.ServeHTTP(recValStatus, reqValStatus)
	if recValStatus.Code != http.StatusOK {
		t.Fatalf("query validation status failed: %d", recValStatus.Code)
	}

	var valStatus map[string]any
	_ = json.Unmarshal(recValStatus.Body.Bytes(), &valStatus)
	if valStatus["status"] != ConnectionStateValidationPassed || valStatus["last_validation_status"] != "passed" {
		t.Errorf("unexpected validation status response: %+v", valStatus)
	}

	// 10. List validation reports
	reqListReports := httptest.NewRequest(http.MethodGet, "/api/v1/connections/"+connID.String()+"/validation/reports", nil)
	reqListReports.Header.Set("Authorization", "Bearer "+token)
	recListReports := httptest.NewRecorder()
	mux.ServeHTTP(recListReports, reqListReports)
	if recListReports.Code != http.StatusOK {
		t.Fatalf("list validation reports failed: %d", recListReports.Code)
	}

	var reports []ValidationReportSummary
	_ = json.Unmarshal(recListReports.Body.Bytes(), &reports)
	if len(reports) == 0 || reports[0].OverallResult != "passed" {
		t.Errorf("expected at least one passed report summary, got %+v", reports)
	}

	// 11. Retrieve single validation report by ID
	reportID := reports[0].ID.String()
	reqGetReport := httptest.NewRequest(http.MethodGet, "/api/v1/connections/"+connID.String()+"/validation/reports/"+reportID, nil)
	reqGetReport.Header.Set("Authorization", "Bearer "+token)
	recGetReport := httptest.NewRecorder()
	mux.ServeHTTP(recGetReport, reqGetReport)
	if recGetReport.Code != http.StatusOK {
		t.Fatalf("get single validation report failed: %d", recGetReport.Code)
	}

	var retrievedReport ValidationReport
	_ = json.Unmarshal(recGetReport.Body.Bytes(), &retrievedReport)
	if retrievedReport.ID != reports[0].ID {
		t.Errorf("retrieved report ID %s != expected %s", retrievedReport.ID, reports[0].ID)
	}

	// 12. Trigger sync is now valid and enqueued
	reqSync := httptest.NewRequest(http.MethodPost, "/api/v1/connections/"+connID.String()+"/sync", bytes.NewBufferString(`{"lookback_days": 10}`))
	reqSync.Header.Set("Authorization", "Bearer "+token)
	recSync := httptest.NewRecorder()
	mux.ServeHTTP(recSync, reqSync)
	if recSync.Code != http.StatusAccepted {
		t.Fatalf("expected 202 Accepted, got %d: %s", recSync.Code, recSync.Body.String())
	}

	// 13. Disconnect connection
	reqDisc := httptest.NewRequest(http.MethodDelete, "/api/v1/connections/"+connID.String(), nil)
	reqDisc.Header.Set("Authorization", "Bearer "+token)
	recDisc := httptest.NewRecorder()
	mux.ServeHTTP(recDisc, reqDisc)
	if recDisc.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", recDisc.Code, recDisc.Body.String())
	}

	// 14. Sync is rejected after disconnection
	reqSyncPost := httptest.NewRequest(http.MethodPost, "/api/v1/connections/"+connID.String()+"/sync", nil)
	reqSyncPost.Header.Set("Authorization", "Bearer "+token)
	recSyncPost := httptest.NewRecorder()
	mux.ServeHTTP(recSyncPost, reqSyncPost)
	if recSyncPost.Code != http.StatusConflict {
		t.Errorf("expected 409 Conflict after disconnection, got %d", recSyncPost.Code)
	}
}

func TestHandler_TenantIsolation_Security(t *testing.T) {
	mux, _, tokenGen := setupTestRouter(t, nil)

	familyA, userA, connA := setupTenantFixture(t, "fixture")
	familyB, userB, _ := setupTenantFixture(t, "fixture")

	tokenB := tokenGen(userB, familyB) // Tenant B attempts accessing Tenant A's connection

	// 1. Tenant B cannot get Tenant A's connection -> 404
	reqGet := httptest.NewRequest(http.MethodGet, "/api/v1/connections/"+connA.String(), nil)
	reqGet.Header.Set("Authorization", "Bearer "+tokenB)
	recGet := httptest.NewRecorder()
	mux.ServeHTTP(recGet, reqGet)
	if recGet.Code != http.StatusNotFound {
		t.Errorf("security breach: expected 404 for cross-tenant GET, got %d", recGet.Code)
	}

	// 2. Tenant B cannot start auth on Tenant A's connection -> 404
	reqAuth := httptest.NewRequest(http.MethodPost, "/api/v1/connections/"+connA.String()+"/auth/start", nil)
	reqAuth.Header.Set("Authorization", "Bearer "+tokenB)
	recAuth := httptest.NewRecorder()
	mux.ServeHTTP(recAuth, reqAuth)
	if recAuth.Code != http.StatusNotFound {
		t.Errorf("security breach: expected 404 for cross-tenant auth start, got %d", recAuth.Code)
	}

	// 3. Tenant B cannot trigger validation on Tenant A's connection -> 404
	reqVal := httptest.NewRequest(http.MethodPost, "/api/v1/connections/"+connA.String()+"/validate", nil)
	reqVal.Header.Set("Authorization", "Bearer "+tokenB)
	recVal := httptest.NewRecorder()
	mux.ServeHTTP(recVal, reqVal)
	if recVal.Code != http.StatusNotFound {
		t.Errorf("security breach: expected 404 for cross-tenant validate, got %d", recVal.Code)
	}

	// 4. Tenant B cannot disconnect Tenant A's connection -> 404
	reqDisc := httptest.NewRequest(http.MethodDelete, "/api/v1/connections/"+connA.String(), nil)
	reqDisc.Header.Set("Authorization", "Bearer "+tokenB)
	recDisc := httptest.NewRecorder()
	mux.ServeHTTP(recDisc, reqDisc)
	if recDisc.Code != http.StatusNotFound {
		t.Errorf("security breach: expected 404 for cross-tenant disconnect, got %d", recDisc.Code)
	}

	_ = familyA
	_ = userA
}

func TestHandler_UI_Serving(t *testing.T) {
	mux, _, _ := setupTestRouter(t, nil)

	req1 := httptest.NewRequest(http.MethodGet, "/web/connections", nil)
	rec1 := httptest.NewRecorder()
	mux.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Errorf("expected 200 for /web/connections, got %d", rec1.Code)
	}
	if rec1.Header().Get("Content-Type") != "text/html; charset=utf-8" {
		t.Errorf("unexpected content type: %s", rec1.Header().Get("Content-Type"))
	}

	htmlContent := rec1.Body.String()
	// Security audit assertions for embedded UI:
	// 1. Must implement HTML entity escaping function
	if !strings.Contains(htmlContent, "function escapeHtml") {
		t.Error("expected UI to include escapeHtml function for XSS protection")
	}
	// 2. Must clearly label fixture/test mode and experimental status
	if !strings.Contains(htmlContent, "TEST DATA / FIXTURE") {
		t.Error("expected UI to explicitly label test fixtures")
	}
	if !strings.Contains(htmlContent, "EXPERIMENTAL / DIAGNOSTIC") {
		t.Error("expected UI to explicitly label experimental diagnostic handoff")
	}
	// 3. Must enforce that Sync is disabled unless validation passed
	if !strings.Contains(htmlContent, "Financial Sync is blocked by fail-closed gate") {
		t.Error("expected UI to enforce disabled sync state when gate is not satisfied")
	}
	// 4. Must ensure credentials are not stored in browser localStorage or sessionStorage
	if strings.Contains(htmlContent, "localStorage.setItem('session") || strings.Contains(htmlContent, "sessionStorage.setItem") {
		t.Error("CRITICAL SECURITY VIOLATION: browser-side credential persistence detected in UI")
	}

	req2 := httptest.NewRequest(http.MethodGet, "/connections/ui", nil)
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Errorf("expected 200 for /connections/ui, got %d", rec2.Code)
	}
}

func TestHandler_SessionProbeFailure_ReturnsUnprocessable(t *testing.T) {
	mux, _, tokenGen := setupTestRouter(t, nil)
	familyID, userID, connID := setupTenantFixture(t, "fixture")
	token := tokenGen(userID, familyID)

	// Transition to authenticating
	reqStart := httptest.NewRequest(http.MethodPost, "/api/v1/connections/"+connID.String()+"/auth/start", nil)
	reqStart.Header.Set("Authorization", "Bearer "+token)
	recStart := httptest.NewRecorder()
	mux.ServeHTTP(recStart, reqStart)

	// Fixture connector simulates auth expiration when string contains fail:auth
	badSession := `{"credentials": "fail:auth"}`
	reqBad := httptest.NewRequest(http.MethodPost, "/api/v1/connections/"+connID.String()+"/auth/session", bytes.NewBufferString(badSession))
	reqBad.Header.Set("Authorization", "Bearer "+token)
	reqBad.Header.Set("Content-Type", "application/json")
	recBad := httptest.NewRecorder()
	mux.ServeHTTP(recBad, reqBad)

	if recBad.Code != http.StatusUnprocessableEntity {
		t.Errorf("expected 422 Unprocessable Entity for invalid probe, got %d: %s", recBad.Code, recBad.Body.String())
	}
}

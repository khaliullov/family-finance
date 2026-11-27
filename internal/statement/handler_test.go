package statement

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/khaliullov/family-finance/internal/auth"
	"github.com/khaliullov/family-finance/internal/database"
)

func setupTestRouter(t *testing.T) (*http.ServeMux, *Service, *auth.JWTManager) {
	pool := getAppPool(t)
	jwtMgr, err := auth.NewJWTManager("test-jwt-secret-at-least-32-bytes-long", time.Hour)
	if err != nil {
		t.Fatalf("failed to create jwt manager: %v", err)
	}

	authRepo := auth.NewRepository(pool)
	authMw := auth.AuthMiddleware(jwtMgr, authRepo)

	parsers := NewDefaultParserRegistry()
	svc := NewService(pool, testCryptoKey, parsers)
	h := NewHandler(svc)

	mux := http.NewServeMux()
	h.RegisterRoutes(mux, authMw)

	return mux, svc, jwtMgr
}

func createMultipartRequest(url string, fields map[string]string, filename string, fileContent []byte) (*http.Request, error) {
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	for k, v := range fields {
		if err := writer.WriteField(k, v); err != nil {
			return nil, err
		}
	}

	if filename != "" {
		part, err := writer.CreateFormFile("file", filename)
		if err != nil {
			return nil, err
		}
		if _, err := part.Write(fileContent); err != nil {
			return nil, err
		}
	}

	if err := writer.Close(); err != nil {
		return nil, err
	}

	req := httptest.NewRequest("POST", url, body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	return req, nil
}

// 1. Authorization Tests: Verify all endpoints reject unauthenticated requests
func TestHandler_AuthValidation(t *testing.T) {
	mux, _, _ := setupTestRouter(t)

	endpoints := []struct {
		method string
		path   string
	}{
		{"POST", "/api/v1/imports"},
		{"GET", "/api/v1/imports"},
		{"GET", "/api/v1/imports/" + uuid.NewString()},
		{"GET", "/api/v1/imports/" + uuid.NewString() + "/preview"},
		{"PATCH", "/api/v1/imports/" + uuid.NewString() + "/rows/" + uuid.NewString()},
		{"POST", "/api/v1/imports/" + uuid.NewString() + "/approve"},
		{"POST", "/api/v1/imports/" + uuid.NewString() + "/promote"},
		{"POST", "/api/v1/imports/" + uuid.NewString() + "/cancel"},
	}

	for _, ep := range endpoints {
		t.Run(fmt.Sprintf("%s %s", ep.method, ep.path), func(t *testing.T) {
			req := httptest.NewRequest(ep.method, ep.path, nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401 Unauthorized, got %d for %s %s", rec.Code, ep.method, ep.path)
			}
		})
	}
}

// 2. Upload Security & Validation
func TestHandler_UploadValidationAndSafety(t *testing.T) {
	mux, _, jwtMgr := setupTestRouter(t)
	familyID, userID, accountID := setupStatementTenantFixture(t)

	token, _, err := jwtMgr.GenerateToken(userID, familyID, "test@example.com", "owner")
	if err != nil {
		t.Fatalf("failed to generate token: %v", err)
	}

	validCSV := `"Имя счёта";"Дата операции";"Дата платежа";"Номер карты";"Статус";"Сумма операции";"Валюта операции";"Сумма в валюте счёта";"Валюта счёта";"Кэшбэк";"Категория";"MCC";"Описание";"Бонусы (включая кэшбэк)";"Округление на инвесткопилку";"Сумма операции с округлением"
"Primary Checking";"20.11.2026 14:30:00";"20.11.2026 14:30:00";"*1234";"OK";"-500.00";"RUB";"-500.00";"RUB";"0";"Супермаркеты";"5411";"Пятерочка";"0";"0";"500.00"`

	// Test 2a: Missing account_id
	t.Run("Missing account_id", func(t *testing.T) {
		req, _ := createMultipartRequest("/api/v1/imports", map[string]string{}, "stmt.csv", []byte(validCSV))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 Bad Request, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	// Test 2b: Invalid account_id format
	t.Run("Invalid account_id format", func(t *testing.T) {
		req, _ := createMultipartRequest("/api/v1/imports", map[string]string{"account_id": "not-a-uuid"}, "stmt.csv", []byte(validCSV))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 Bad Request, got %d", rec.Code)
		}
	})

	// Test 2c: Missing file
	t.Run("Missing file field", func(t *testing.T) {
		req, _ := createMultipartRequest("/api/v1/imports", map[string]string{"account_id": accountID.String()}, "", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 Bad Request, got %d", rec.Code)
		}
	})

	// Test 2d: Empty file
	t.Run("Empty file", func(t *testing.T) {
		req, _ := createMultipartRequest("/api/v1/imports", map[string]string{"account_id": accountID.String()}, "empty.csv", []byte(""))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 Bad Request for empty file, got %d", rec.Code)
		}
	})

	// Test 2e: Path traversal filename is sanitized safely
	t.Run("Path traversal filename", func(t *testing.T) {
		req, _ := createMultipartRequest("/api/v1/imports", map[string]string{"account_id": accountID.String()}, "../../../../../etc/passwd", []byte(validCSV))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusCreated {
			t.Fatalf("expected 201 Created for sanitized filename, got %d: %s", rec.Code, rec.Body.String())
		}

		var summary ImportSummary
		if err := json.NewDecoder(rec.Body).Decode(&summary); err != nil {
			t.Fatalf("failed to decode summary: %v", err)
		}
		if summary.OriginalFilename != "passwd" {
			t.Fatalf("expected sanitized filename 'passwd', got %q", summary.OriginalFilename)
		}
	})

	// Test 2f: Cross-family account access rejected
	t.Run("Cross-family account access rejected", func(t *testing.T) {
		_, _, otherAccountID := setupStatementTenantFixture(t)

		req, _ := createMultipartRequest("/api/v1/imports", map[string]string{"account_id": otherAccountID.String()}, "stmt.csv", []byte(validCSV))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest && rec.Code != http.StatusForbidden {
			t.Fatalf("expected 400 or 403 for cross-family account, got %d", rec.Code)
		}
	})

	// Test 2g: Unsupported format parameter
	t.Run("Unsupported format parameter", func(t *testing.T) {
		req, _ := createMultipartRequest("/api/v1/imports", map[string]string{
			"account_id":  accountID.String(),
			"bank_format": "unsupported_xyz_format",
		}, "stmt.csv", []byte(validCSV))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 Bad Request for unsupported format, got %d", rec.Code)
		}
	})
}

// 3. End-to-End Workflow: Upload -> Preview -> Edit Row -> Approve -> Promote -> Idempotency
func TestHandler_EndToEndReviewAndPromotionWorkflow(t *testing.T) {
	mux, svc, jwtMgr := setupTestRouter(t)
	familyID, userID, accountID := setupStatementTenantFixture(t)

	token, _, err := jwtMgr.GenerateToken(userID, familyID, "test@example.com", "owner")
	if err != nil {
		t.Fatalf("failed to generate token: %v", err)
	}

	// Create a test category in the family
	categoryID := uuid.New()
	pool := svc.pool
	err = database.WithFamilyUserTx(context.Background(), pool, familyID, userID, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `
			INSERT INTO public.categories (id, family_id, name, type)
			VALUES ($1, $2, 'Groceries Test', 'expense')
		`, categoryID, familyID)
		return err
	})
	if err != nil {
		t.Fatalf("failed to create test category: %v", err)
	}

	// Statement with 1 normal row, 1 cancelled row
	csvData := `"Имя счёта";"Дата операции";"Дата платежа";"Номер карты";"Статус";"Сумма операции";"Валюта операции";"Сумма в валюте счёта";"Валюта счёта";"Кэшбэк";"Категория";"MCC";"Описание";"Бонусы (включая кэшбэк)";"Округление на инвесткопилку";"Сумма операции с округлением"
"Primary Checking";"20.11.2026 12:00:00";"20.11.2026 12:00:00";"*1234";"OK";"-800.00";"RUB";"-800.00";"RUB";"0";"Супермаркеты";"5411";"Магнит";"0";"0";"800.00"
"Primary Checking";"20.11.2026 13:00:00";"";"*1234";"Ошибка";"-500.00";"RUB";"-500.00";"RUB";"0";"Фастфуд";"5814";"Бургер Кинг";"0";"0";"500.00"`

	// Step A: Upload
	uploadReq, _ := createMultipartRequest("/api/v1/imports", map[string]string{"account_id": accountID.String()}, "tbank_nov20.csv", []byte(csvData))
	uploadReq.Header.Set("Authorization", "Bearer "+token)
	uploadRec := httptest.NewRecorder()
	mux.ServeHTTP(uploadRec, uploadReq)

	if uploadRec.Code != http.StatusCreated {
		t.Fatalf("Upload failed with %d: %s", uploadRec.Code, uploadRec.Body.String())
	}

	var uploadSummary ImportSummary
	if err := json.NewDecoder(uploadRec.Body).Decode(&uploadSummary); err != nil {
		t.Fatalf("failed to decode upload summary: %v", err)
	}
	batchID := uploadSummary.ImportID
	if uploadSummary.Status != BatchStatusStaged {
		t.Fatalf("expected status 'staged', got %s", uploadSummary.Status)
	}
	if uploadSummary.TotalRows != 2 {
		t.Fatalf("expected total_rows=2, got %d", uploadSummary.TotalRows)
	}

	// Step B: Preview
	prevReq := httptest.NewRequest("GET", fmt.Sprintf("/api/v1/imports/%s/preview", batchID), nil)
	prevReq.Header.Set("Authorization", "Bearer "+token)
	prevRec := httptest.NewRecorder()
	mux.ServeHTTP(prevRec, prevReq)

	if prevRec.Code != http.StatusOK {
		t.Fatalf("Preview failed with %d: %s", prevRec.Code, prevRec.Body.String())
	}

	var preview ImportPreviewResponse
	if err := json.NewDecoder(prevRec.Body).Decode(&preview); err != nil {
		t.Fatalf("failed to decode preview: %v", err)
	}
	if len(preview.Rows) != 2 {
		t.Fatalf("expected 2 preview rows, got %d", len(preview.Rows))
	}

	var normalRow, cancelledRow StagedRowPreview
	for _, r := range preview.Rows {
		if r.BookingStatus == "cancelled" {
			cancelledRow = r
		} else {
			normalRow = r
		}
	}

	// Verify cancelled row cannot be promoted
	if cancelledRow.IsEligibleForPromotion {
		t.Fatalf("cancelled row must NOT be marked eligible for promotion")
	}

	// Step C: Attempt to force resolution_action='commit' on cancelled row -> Must fail with 400
	forceCommitBody := `{"resolution_action": "commit"}`
	forceReq := httptest.NewRequest("PATCH", fmt.Sprintf("/api/v1/imports/%s/rows/%s", batchID, cancelledRow.ID), strings.NewReader(forceCommitBody))
	forceReq.Header.Set("Authorization", "Bearer "+token)
	forceReq.Header.Set("Content-Type", "application/json")
	forceRec := httptest.NewRecorder()
	mux.ServeHTTP(forceRec, forceReq)

	if forceRec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request when setting commit on cancelled row, got %d: %s", forceRec.Code, forceRec.Body.String())
	}

	// Step D: Update normal row category
	catUpdateBody := fmt.Sprintf(`{"category_id": "%s"}`, categoryID)
	catReq := httptest.NewRequest("PATCH", fmt.Sprintf("/api/v1/imports/%s/rows/%s", batchID, normalRow.ID), strings.NewReader(catUpdateBody))
	catReq.Header.Set("Authorization", "Bearer "+token)
	catReq.Header.Set("Content-Type", "application/json")
	catRec := httptest.NewRecorder()
	mux.ServeHTTP(catRec, catReq)

	if catRec.Code != http.StatusOK {
		t.Fatalf("Update category failed with %d: %s", catRec.Code, catRec.Body.String())
	}

	// Step E: Attempt to promote BEFORE approval -> Must fail with 400
	promoteEarlyReq := httptest.NewRequest("POST", fmt.Sprintf("/api/v1/imports/%s/promote", batchID), nil)
	promoteEarlyReq.Header.Set("Authorization", "Bearer "+token)
	promoteEarlyRec := httptest.NewRecorder()
	mux.ServeHTTP(promoteEarlyRec, promoteEarlyReq)

	if promoteEarlyRec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request when promoting unapproved batch, got %d: %s", promoteEarlyRec.Code, promoteEarlyRec.Body.String())
	}

	// Step F: Explicit Approval
	approveReq := httptest.NewRequest("POST", fmt.Sprintf("/api/v1/imports/%s/approve", batchID), strings.NewReader(`{"notes": "Approved by family owner"}`))
	approveReq.Header.Set("Authorization", "Bearer "+token)
	approveReq.Header.Set("Content-Type", "application/json")
	approveRec := httptest.NewRecorder()
	mux.ServeHTTP(approveRec, approveReq)

	if approveRec.Code != http.StatusOK {
		t.Fatalf("Approve failed with %d: %s", approveRec.Code, approveRec.Body.String())
	}
	var approvedSummary ImportSummary
	_ = json.NewDecoder(approveRec.Body).Decode(&approvedSummary)
	if approvedSummary.Status != BatchStatusApproved {
		t.Fatalf("expected status 'approved', got %s", approvedSummary.Status)
	}

	// Step G: Modify row after approval -> revokes approval
	modReq := httptest.NewRequest("PATCH", fmt.Sprintf("/api/v1/imports/%s/rows/%s", batchID, normalRow.ID), strings.NewReader(`{"operation_type": "purchase"}`))
	modReq.Header.Set("Authorization", "Bearer "+token)
	modReq.Header.Set("Content-Type", "application/json")
	modRec := httptest.NewRecorder()
	mux.ServeHTTP(modRec, modReq)
	if modRec.Code != http.StatusOK {
		t.Fatalf("Patch after approve failed: %d", modRec.Code)
	}

	// Promote must fail because approval was revoked by mutation
	promoteAfterModReq := httptest.NewRequest("POST", fmt.Sprintf("/api/v1/imports/%s/promote", batchID), nil)
	promoteAfterModReq.Header.Set("Authorization", "Bearer "+token)
	promoteAfterModRec := httptest.NewRecorder()
	mux.ServeHTTP(promoteAfterModRec, promoteAfterModReq)
	if promoteAfterModRec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request when promoting mutated batch, got %d", promoteAfterModRec.Code)
	}

	// Re-approve
	reApproveReq := httptest.NewRequest("POST", fmt.Sprintf("/api/v1/imports/%s/approve", batchID), nil)
	reApproveReq.Header.Set("Authorization", "Bearer "+token)
	reApproveRec := httptest.NewRecorder()
	mux.ServeHTTP(reApproveRec, reApproveReq)
	if reApproveRec.Code != http.StatusOK {
		t.Fatalf("Re-approve failed: %d", reApproveRec.Code)
	}

	// Step H: Promote Approved Batch
	promoteReq := httptest.NewRequest("POST", fmt.Sprintf("/api/v1/imports/%s/promote", batchID), nil)
	promoteReq.Header.Set("Authorization", "Bearer "+token)
	promoteRec := httptest.NewRecorder()
	mux.ServeHTTP(promoteRec, promoteReq)

	if promoteRec.Code != http.StatusOK {
		t.Fatalf("Promote failed with %d: %s", promoteRec.Code, promoteRec.Body.String())
	}

	var pResult PromotionResult
	if err := json.NewDecoder(promoteRec.Body).Decode(&pResult); err != nil {
		t.Fatalf("failed to decode promotion result: %v", err)
	}
	if pResult.TransactionsCommitted != 1 {
		t.Fatalf("expected 1 committed transaction, got %d", pResult.TransactionsCommitted)
	}
	if pResult.TransactionsSkipped != 1 {
		t.Fatalf("expected 1 skipped (cancelled) transaction, got %d", pResult.TransactionsSkipped)
	}

	// Verify that the assigned category_id was stored in accounting_entries
	var entryCatID *uuid.UUID
	err = database.WithFamilyTx(context.Background(), pool, familyID, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `
			SELECT category_id FROM public.accounting_entries
			WHERE family_id = $1 AND account_id = $2
		`, familyID, accountID).Scan(&entryCatID)
	})
	if err != nil || entryCatID == nil || *entryCatID != categoryID {
		t.Fatalf("expected accounting_entries.category_id to be %s, got %v", categoryID, entryCatID)
	}

	// Step I: Re-promoting committed batch must be idempotent (200 OK, 0 new rows)
	promoteRetryReq := httptest.NewRequest("POST", fmt.Sprintf("/api/v1/imports/%s/promote", batchID), nil)
	promoteRetryReq.Header.Set("Authorization", "Bearer "+token)
	promoteRetryRec := httptest.NewRecorder()
	mux.ServeHTTP(promoteRetryRec, promoteRetryReq)
	if promoteRetryRec.Code != http.StatusOK {
		t.Fatalf("idempotent promote retry failed with %d: %s", promoteRetryRec.Code, promoteRetryRec.Body.String())
	}

	// Step J: Mutating or cancelling committed batch must return 409 Conflict
	patchCommittedReq := httptest.NewRequest("PATCH", fmt.Sprintf("/api/v1/imports/%s/rows/%s", batchID, normalRow.ID), strings.NewReader(`{"operation_type": "expense"}`))
	patchCommittedReq.Header.Set("Authorization", "Bearer "+token)
	patchCommittedRec := httptest.NewRecorder()
	mux.ServeHTTP(patchCommittedRec, patchCommittedReq)
	if patchCommittedRec.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict on mutating committed batch, got %d", patchCommittedRec.Code)
	}

	cancelCommittedReq := httptest.NewRequest("POST", fmt.Sprintf("/api/v1/imports/%s/cancel", batchID), nil)
	cancelCommittedReq.Header.Set("Authorization", "Bearer "+token)
	cancelCommittedRec := httptest.NewRecorder()
	mux.ServeHTTP(cancelCommittedRec, cancelCommittedReq)
	if cancelCommittedRec.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict on cancelling committed batch, got %d", cancelCommittedRec.Code)
	}
}

// 4. Batch Cancellation Workflow
func TestHandler_CancellationWorkflow(t *testing.T) {
	mux, _, jwtMgr := setupTestRouter(t)
	familyID, userID, accountID := setupStatementTenantFixture(t)

	token, _, _ := jwtMgr.GenerateToken(userID, familyID, "test@example.com", "owner")

	csvData := `"Имя счёта";"Дата операции";"Дата платежа";"Номер карты";"Статус";"Сумма операции";"Валюта операции";"Сумма в валюте счёта";"Валюта счёта";"Кэшбэк";"Категория";"MCC";"Описание";"Бонусы (включая кэшбэк)";"Округление на инвесткопилку";"Сумма операции с округлением"
"Primary Checking";"20.11.2026 12:00:00";"20.11.2026 12:00:00";"*1234";"OK";"-300.00";"RUB";"-300.00";"RUB";"0";"Супермаркеты";"5411";"Магнит";"0";"0";"300.00"`

	uploadReq, _ := createMultipartRequest("/api/v1/imports", map[string]string{"account_id": accountID.String()}, "stmt.csv", []byte(csvData))
	uploadReq.Header.Set("Authorization", "Bearer "+token)
	uploadRec := httptest.NewRecorder()
	mux.ServeHTTP(uploadRec, uploadReq)

	var summary ImportSummary
	_ = json.NewDecoder(uploadRec.Body).Decode(&summary)
	batchID := summary.ImportID

	// Cancel batch
	cancelReq := httptest.NewRequest("POST", fmt.Sprintf("/api/v1/imports/%s/cancel", batchID), nil)
	cancelReq.Header.Set("Authorization", "Bearer "+token)
	cancelRec := httptest.NewRecorder()
	mux.ServeHTTP(cancelRec, cancelReq)

	if cancelRec.Code != http.StatusOK {
		t.Fatalf("Cancel failed with %d: %s", cancelRec.Code, cancelRec.Body.String())
	}
	var cancelledSummary ImportSummary
	_ = json.NewDecoder(cancelRec.Body).Decode(&cancelledSummary)
	if cancelledSummary.Status != BatchStatusCancelled {
		t.Fatalf("expected status 'cancelled', got %s", cancelledSummary.Status)
	}

	// Attempting to approve cancelled batch must fail
	appReq := httptest.NewRequest("POST", fmt.Sprintf("/api/v1/imports/%s/approve", batchID), nil)
	appReq.Header.Set("Authorization", "Bearer "+token)
	appRec := httptest.NewRecorder()
	mux.ServeHTTP(appRec, appReq)

	if appRec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request when approving cancelled batch, got %d", appRec.Code)
	}
}

// 5. History and Cross-Tenant Isolation
func TestHandler_HistoryAndCrossTenantIsolation(t *testing.T) {
	mux, _, jwtMgr := setupTestRouter(t)

	familyA, userA, accA := setupStatementTenantFixture(t)
	familyB, userB, _ := setupStatementTenantFixture(t)

	tokenA, _, _ := jwtMgr.GenerateToken(userA, familyA, "userA@example.com", "owner")
	tokenB, _, _ := jwtMgr.GenerateToken(userB, familyB, "userB@example.com", "owner")

	csvData := `"Имя счёта";"Дата операции";"Дата платежа";"Номер карты";"Статус";"Сумма операции";"Валюта операции";"Сумма в валюте счёта";"Валюта счёта";"Кэшбэк";"Категория";"MCC";"Описание";"Бонусы (включая кэшбэк)";"Округление на инвесткопилку";"Сумма операции с округлением"
"Primary Checking";"20.11.2026 12:00:00";"20.11.2026 12:00:00";"*1234";"OK";"-400.00";"RUB";"-400.00";"RUB";"0";"Супермаркеты";"5411";"Магнит";"0";"0";"400.00"`

	uploadReq, _ := createMultipartRequest("/api/v1/imports", map[string]string{"account_id": accA.String()}, "family_a_stmt.csv", []byte(csvData))
	uploadReq.Header.Set("Authorization", "Bearer "+tokenA)
	uploadRec := httptest.NewRecorder()
	mux.ServeHTTP(uploadRec, uploadReq)

	var summaryA ImportSummary
	_ = json.NewDecoder(uploadRec.Body).Decode(&summaryA)
	batchA := summaryA.ImportID

	// Test 5a: Family A lists history -> sees 1 item
	histReqA := httptest.NewRequest("GET", "/api/v1/imports", nil)
	histReqA.Header.Set("Authorization", "Bearer "+tokenA)
	histRecA := httptest.NewRecorder()
	mux.ServeHTTP(histRecA, histReqA)

	if histRecA.Code != http.StatusOK {
		t.Fatalf("History for A failed: %d", histRecA.Code)
	}
	var histRespA ImportHistoryResponse
	_ = json.NewDecoder(histRecA.Body).Decode(&histRespA)
	if histRespA.Total < 1 {
		t.Fatalf("expected at least 1 history item for family A, got %d", histRespA.Total)
	}

	// Test 5b: Family B lists history -> sees 0 items (no cross-tenant leakage!)
	histReqB := httptest.NewRequest("GET", "/api/v1/imports", nil)
	histReqB.Header.Set("Authorization", "Bearer "+tokenB)
	histRecB := httptest.NewRecorder()
	mux.ServeHTTP(histRecB, histReqB)

	if histRecB.Code != http.StatusOK {
		t.Fatalf("History for B failed: %d", histRecB.Code)
	}
	var histRespB ImportHistoryResponse
	_ = json.NewDecoder(histRecB.Body).Decode(&histRespB)
	if histRespB.Total != 0 {
		t.Fatalf("cross-tenant leakage: family B sees %d items from family A", histRespB.Total)
	}

	// Test 5c: Family B attempts to access Family A batch directly -> 404 Not Found
	getReqB := httptest.NewRequest("GET", fmt.Sprintf("/api/v1/imports/%s", batchA), nil)
	getReqB.Header.Set("Authorization", "Bearer "+tokenB)
	getRecB := httptest.NewRecorder()
	mux.ServeHTTP(getRecB, getReqB)
	if getRecB.Code != http.StatusNotFound {
		t.Fatalf("expected 404 Not Found for cross-family GET batch, got %d", getRecB.Code)
	}

	prevReqB := httptest.NewRequest("GET", fmt.Sprintf("/api/v1/imports/%s/preview", batchA), nil)
	prevReqB.Header.Set("Authorization", "Bearer "+tokenB)
	prevRecB := httptest.NewRecorder()
	mux.ServeHTTP(prevRecB, prevReqB)
	if prevRecB.Code != http.StatusNotFound {
		t.Fatalf("expected 404 Not Found for cross-family GET preview, got %d", prevRecB.Code)
	}

	appReqB := httptest.NewRequest("POST", fmt.Sprintf("/api/v1/imports/%s/approve", batchA), nil)
	appReqB.Header.Set("Authorization", "Bearer "+tokenB)
	appRecB := httptest.NewRecorder()
	mux.ServeHTTP(appRecB, appReqB)
	if appRecB.Code != http.StatusNotFound {
		t.Fatalf("expected 404 Not Found for cross-family approve, got %d", appRecB.Code)
	}

	promoteReqB := httptest.NewRequest("POST", fmt.Sprintf("/api/v1/imports/%s/promote", batchA), nil)
	promoteReqB.Header.Set("Authorization", "Bearer "+tokenB)
	promoteRecB := httptest.NewRecorder()
	mux.ServeHTTP(promoteRecB, promoteReqB)
	if promoteRecB.Code != http.StatusNotFound {
		t.Fatalf("expected 404 Not Found for cross-family promote, got %d", promoteRecB.Code)
	}
}

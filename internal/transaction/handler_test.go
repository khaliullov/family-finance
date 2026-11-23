package transaction

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/khaliullov/family-finance/internal/auth"
	"github.com/khaliullov/family-finance/internal/connector"
	"github.com/khaliullov/family-finance/internal/database"
)

func TestHandler_TriggerSync(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()

	familyID, userID, connID := setupTenantFixture(t, "fixture")

	registry := connector.NewRegistry()
	svc := NewService(appPool, registry)
	h := NewHandler(appPool, svc)

	jwtMgr, err := auth.NewJWTManagerWithLeeway("test_secret_32_bytes_super_secure!", 15*time.Minute, 10*time.Second)
	if err != nil {
		t.Fatalf("jwtManager init failed: %v", err)
	}
	token, _, err := jwtMgr.GenerateToken(userID, familyID, "test@example.com", "owner")
	if err != nil {
		t.Fatalf("token generation failed: %v", err)
	}

	authRepo := auth.NewRepository(appPool)
	authMw := auth.AuthMiddleware(jwtMgr, authRepo)

	mux := http.NewServeMux()
	h.RegisterRoutes(mux, authMw)

	// 1. Valid trigger sync request
	body := bytes.NewBufferString(`{"lookback_days": 15}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/connections/"+connID.String()+"/sync", body)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202 Accepted, got %d: %s", rec.Code, rec.Body.String())
	}

	var res SyncJobResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal response failed: %v", err)
	}
	if res.JobID == uuid.Nil || res.Status != "enqueued" {
		t.Errorf("unexpected sync response: %+v", res)
	}

	// Verify job was enqueued in worker_jobs under tenant RLS
	_ = database.WithFamilyTx(req.Context(), appPool, familyID, func(tx pgx.Tx) error {
		var state, queueName string
		err := tx.QueryRow(req.Context(), `
			SELECT state, queue_name
			FROM public.worker_jobs
			WHERE family_id = $1 AND id = $2
		`, familyID, res.JobID).Scan(&state, &queueName)
		if err != nil {
			t.Fatalf("failed to query enqueued worker job: %v", err)
		}
		if state != "available" || queueName != "bank_sync" {
			t.Errorf("expected available/bank_sync, got %s/%s", state, queueName)
		}
		return nil
	})

	// 2. Non-existent connection returns 404
	randomConnID := uuid.New()
	req404 := httptest.NewRequest(http.MethodPost, "/api/v1/connections/"+randomConnID.String()+"/sync", nil)
	req404.Header.Set("Authorization", "Bearer "+token)
	rec404 := httptest.NewRecorder()

	mux.ServeHTTP(rec404, req404)
	if rec404.Code != http.StatusNotFound {
		t.Errorf("expected 404 Not Found, got %d", rec404.Code)
	}

	// 3. Unauthenticated request returns 401
	req401 := httptest.NewRequest(http.MethodPost, "/api/v1/connections/"+connID.String()+"/sync", nil)
	rec401 := httptest.NewRecorder()

	mux.ServeHTTP(rec401, req401)
	if rec401.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 Unauthorized, got %d", rec401.Code)
	}
}

func TestHandler_ListAndCreateConnections(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()

	familyID, userID, _ := setupTenantFixture(t, "fixture")

	registry := connector.NewRegistry()
	svc := NewService(appPool, registry)
	h := NewHandler(appPool, svc)

	jwtMgr, _ := auth.NewJWTManagerWithLeeway("test_secret_32_bytes_super_secure!", 15*time.Minute, 10*time.Second)
	token, _, _ := jwtMgr.GenerateToken(userID, familyID, "test@example.com", "owner")

	authRepo := auth.NewRepository(appPool)
	authMw := auth.AuthMiddleware(jwtMgr, authRepo)

	mux := http.NewServeMux()
	h.RegisterRoutes(mux, authMw)

	// 1. List connections
	reqList := httptest.NewRequest(http.MethodGet, "/api/v1/connections", nil)
	reqList.Header.Set("Authorization", "Bearer "+token)
	recList := httptest.NewRecorder()

	mux.ServeHTTP(recList, reqList)
	if recList.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", recList.Code, recList.Body.String())
	}

	var conns []ConnectionResponse
	if err := json.Unmarshal(recList.Body.Bytes(), &conns); err != nil {
		t.Fatalf("unmarshal connections failed: %v", err)
	}
	if len(conns) != 1 || conns[0].Provider != "fixture" {
		t.Errorf("unexpected connections list: %+v", conns)
	}

	// 2. Create new connection
	createBody := bytes.NewBufferString(`{"provider": "fixture", "name": "Second Connection"}`)
	reqCreate := httptest.NewRequest(http.MethodPost, "/api/v1/connections", createBody)
	reqCreate.Header.Set("Authorization", "Bearer "+token)
	reqCreate.Header.Set("Content-Type", "application/json")
	recCreate := httptest.NewRecorder()

	mux.ServeHTTP(recCreate, reqCreate)
	if recCreate.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d: %s", recCreate.Code, recCreate.Body.String())
	}

	var created ConnectionResponse
	if err := json.Unmarshal(recCreate.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal created connection failed: %v", err)
	}
	if created.ID == uuid.Nil || created.Name != "Second Connection" {
		t.Errorf("unexpected created connection: %+v", created)
	}
}

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/khaliullov/family-finance/internal/auth"
)

func TestHealthCheck(t *testing.T) {
	authCfg := auth.DefaultConfig()
	router, err := setupRouter(nil, &authCfg)
	if err != nil {
		t.Fatalf("setupRouter failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200 OK, got %d", rec.Code)
	}

	var res map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if res["status"] != "UP" {
		t.Errorf("expected status 'UP', got %q", res["status"])
	}
}

func TestReadyCheck_WhenPoolNil(t *testing.T) {
	authCfg := auth.DefaultConfig()
	router, err := setupRouter(nil, &authCfg)
	if err != nil {
		t.Fatalf("setupRouter failed: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/ready", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected status 503 Service Unavailable when DB is nil, got %d", rec.Code)
	}

	var res map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if res["status"] != "UNAVAILABLE" || res["database"] != "DOWN" {
		t.Errorf("unexpected readiness payload: %+v", res)
	}
}

func TestGetDatabaseConnString(t *testing.T) {
	// 1. Default fallback
	os.Unsetenv("DATABASE_URL")
	os.Unsetenv("PGHOST")
	os.Unsetenv("PGPORT")
	os.Unsetenv("PGUSER")
	os.Unsetenv("PGPASSWORD")
	os.Unsetenv("PGDATABASE")
	os.Unsetenv("PGSSLMODE")

	connStr := getDatabaseConnString()
	expectedDefault := "postgres://family_app:app_secret_placeholder@localhost:5432/family_finance?sslmode=disable"
	if connStr != expectedDefault {
		t.Errorf("expected %q, got %q", expectedDefault, connStr)
	}

	// 2. Explicit DATABASE_URL override
	customURL := "postgres://custom_user:custom_pass@db.internal:5433/custom_db?sslmode=require"
	os.Setenv("DATABASE_URL", customURL)
	defer os.Unsetenv("DATABASE_URL")

	if got := getDatabaseConnString(); got != customURL {
		t.Errorf("expected %q, got %q", customURL, got)
	}
}

func TestGetWorkerDatabaseConnString(t *testing.T) {
	os.Unsetenv("WORKER_DATABASE_URL")
	os.Unsetenv("WORKER_PGHOST")
	os.Unsetenv("WORKER_PGPORT")
	os.Unsetenv("WORKER_PGUSER")
	os.Unsetenv("WORKER_PGPASSWORD")
	os.Unsetenv("WORKER_PGDATABASE")
	os.Unsetenv("WORKER_PGSSLMODE")

	// 1. Production fails when WORKER_DATABASE_URL is missing
	_, err := getWorkerDatabaseConnString("production")
	if err == nil {
		t.Errorf("expected getWorkerDatabaseConnString to fail in production when WORKER_DATABASE_URL missing")
	}

	// 2. Production succeeds when WORKER_DATABASE_URL is set
	workerProdURL := "postgres://family_worker:prod_secret@db.prod:5432/family_finance?sslmode=verify-full"
	os.Setenv("WORKER_DATABASE_URL", workerProdURL)
	gotProd, err := getWorkerDatabaseConnString("production")
	if err != nil || gotProd != workerProdURL {
		t.Errorf("expected %q in production, got %q, err=%v", workerProdURL, gotProd, err)
	}
	os.Unsetenv("WORKER_DATABASE_URL")

	// 3. Development defaults to family_worker, never family_app
	gotDev, err := getWorkerDatabaseConnString("development")
	if err != nil {
		t.Errorf("unexpected error in development: %v", err)
	}
	expectedDev := "postgres://family_worker:worker_secret_placeholder@localhost:5432/family_finance?sslmode=disable"
	if gotDev != expectedDev {
		t.Errorf("expected %q, got %q", expectedDev, gotDev)
	}
}

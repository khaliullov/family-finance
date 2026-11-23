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

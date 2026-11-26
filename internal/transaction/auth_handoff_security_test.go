package transaction

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/khaliullov/family-finance/internal/connector/tbank/personal"
	"github.com/khaliullov/family-finance/internal/database"
)

func TestAuthHandoff_MalformedOrMissingTokens_Rejected(t *testing.T) {
	mux, _, tokenGen := setupTestRouter(t, nil)
	familyID, userID, connID := setupTenantFixture(t, "tbank_personal")
	token := tokenGen(userID, familyID)

	appPool := getAppPool(t)
	defer appPool.Close()
	ctx := context.Background()

	// Set connection to authenticating
	err := database.WithFamilyTx(ctx, appPool, familyID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE public.bank_connections
			SET status = 'authenticating', updated_at = NOW()
			WHERE family_id = $1 AND id = $2
		`, familyID, connID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("missing_required_tokens", func(t *testing.T) {
		body := map[string]any{
			"cookies": []map[string]string{
				{"name": "_ga", "value": "GA1.2.34567"},
			},
		}
		bodyBytes, _ := json.Marshal(body)

		req := httptest.NewRequest(http.MethodPost, "/api/v1/connections/"+connID.String()+"/auth/session", bytes.NewReader(bodyBytes))
		req.Header.Set("Authorization", "Bearer "+token)

		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("expected HTTP 422 for missing required tokens, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("empty_cookies", func(t *testing.T) {
		body := map[string]any{
			"cookies": []map[string]string{},
		}
		bodyBytes, _ := json.Marshal(body)

		req := httptest.NewRequest(http.MethodPost, "/api/v1/connections/"+connID.String()+"/auth/session", bytes.NewReader(bodyBytes))
		req.Header.Set("Authorization", "Bearer "+token)

		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("expected HTTP 422 for empty cookies, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("unsupported_provider", func(t *testing.T) {
		unsuppConnID := uuid.New()
		_ = database.WithFamilyTx(ctx, appPool, familyID, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				INSERT INTO public.bank_connections (id, family_id, authorized_by_user_id, provider, name, status)
				VALUES ($1, $2, $3, 'unsupported_bank_abc', 'Unsupp', 'authenticating')
			`, unsuppConnID, familyID, userID)
			return err
		})

		body := map[string]any{
			"cookies": []map[string]string{
				{"name": "psid", "value": "abc"},
			},
		}
		bodyBytes, _ := json.Marshal(body)

		req := httptest.NewRequest(http.MethodPost, "/api/v1/connections/"+unsuppConnID.String()+"/auth/session", bytes.NewReader(bodyBytes))
		req.Header.Set("Authorization", "Bearer "+token)

		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected HTTP 400 for unsupported provider, got %d: %s", rec.Code, rec.Body.String())
		}
	})
}

func TestAuthHandoff_PayloadBoundary_MaxBytes(t *testing.T) {
	mux, _, tokenGen := setupTestRouter(t, nil)
	familyID, userID, connID := setupTenantFixture(t, "tbank_personal")
	token := tokenGen(userID, familyID)

	appPool := getAppPool(t)
	defer appPool.Close()
	ctx := context.Background()

	_ = database.WithFamilyTx(ctx, appPool, familyID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE public.bank_connections
			SET status = 'authenticating', updated_at = NOW()
			WHERE family_id = $1 AND id = $2
		`, familyID, connID)
		return err
	})

	// Oversized payload: 128KB > 64KB limit
	hugePayload := strings.Repeat("A", 128*1024)
	body := map[string]any{
		"session_data": hugePayload,
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/connections/"+connID.String()+"/auth/session", bytes.NewReader(bodyBytes))
	req.Header.Set("Authorization", "Bearer "+token)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected HTTP 400 for oversized payload, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAuthHandoff_ValidSubmission_EncryptedAndNeverExposed(t *testing.T) {
	cryptoKey := []byte("01234567890123456789012345678901")
	mux, _, tokenGen := setupTestRouter(t, cryptoKey)
	familyID, userID, connID := setupTenantFixture(t, "tbank_personal")
	token := tokenGen(userID, familyID)

	appPool := getAppPool(t)
	defer appPool.Close()
	ctx := context.Background()

	_ = database.WithFamilyTx(ctx, appPool, familyID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE public.bank_connections
			SET status = 'authenticating', session_version = 1, updated_at = NOW()
			WHERE family_id = $1 AND id = $2
		`, familyID, connID)
		return err
	})

	secretCookieVal := "secret_psid_token_value_987654321"
	body := map[string]any{
		"cookies": []map[string]string{
			{"name": "psid", "value": secretCookieVal, "domain": ".tbank.ru", "path": "/"},
			{"name": "wses", "value": "secret_wses_token_12345", "domain": ".tbank.ru", "path": "/"},
		},
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/connections/"+connID.String()+"/auth/session", bytes.NewReader(bodyBytes))
	req.Header.Set("Authorization", "Bearer "+token)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d: %s", rec.Code, rec.Body.String())
	}

	respBody := rec.Body.String()
	// 1. Confirm secret credentials NEVER appear in response body
	if strings.Contains(respBody, secretCookieVal) || strings.Contains(respBody, "secret_wses_token") {
		t.Fatalf("CRITICAL SECURITY VIOLATION: credentials exposed in API response body: %s", respBody)
	}

	// 2. Confirm database stored ciphertext, not plaintext
	var storedCreds []byte
	var sessionVer int
	var credHash *string
	err := database.WithFamilyTx(ctx, appPool, familyID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT encrypted_credentials, session_version, credential_hash
			FROM public.bank_connections
			WHERE family_id = $1 AND id = $2
		`, familyID, connID).Scan(&storedCreds, &sessionVer, &credHash)
	})
	if err != nil {
		t.Fatal(err)
	}

	if bytes.Contains(storedCreds, []byte(secretCookieVal)) {
		t.Fatal("CRITICAL SECURITY VIOLATION: plaintext credentials persisted in database")
	}

	// Confirm session_version incremented to 2
	if sessionVer != 2 {
		t.Errorf("expected session_version to increment to 2, got %d", sessionVer)
	}
	if credHash == nil || *credHash == "" {
		t.Error("expected non-empty credential_hash stored in database")
	}

	// Decrypt with key to verify integrity
	decryptedSess, err := personal.DecryptSession(cryptoKey, storedCreds)
	if err != nil {
		t.Fatalf("failed to decrypt persisted session with master key: %v", err)
	}
	if len(decryptedSess.Cookies) != 2 {
		t.Fatalf("expected 2 decrypted cookies, got %d", len(decryptedSess.Cookies))
	}
}

func TestAuthHandoff_CancelAndDisconnect_WipesCredentials(t *testing.T) {
	mux, _, tokenGen := setupTestRouter(t, nil)
	familyID, userID, connID := setupTenantFixture(t, "fixture")
	token := tokenGen(userID, familyID)

	appPool := getAppPool(t)
	defer appPool.Close()
	ctx := context.Background()

	// Set credentials on connection
	_ = database.WithFamilyTx(ctx, appPool, familyID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE public.bank_connections
			SET status = 'authenticating', encrypted_credentials = 'some_credentials', updated_at = NOW()
			WHERE family_id = $1 AND id = $2
		`, familyID, connID)
		return err
	})

	// 1. Cancel authentication
	req := httptest.NewRequest(http.MethodPost, "/api/v1/connections/"+connID.String()+"/auth/cancel", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200 for cancel, got %d", rec.Code)
	}

	// Confirm credentials wiped
	var creds []byte
	var status string
	_ = database.WithFamilyTx(ctx, appPool, familyID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT status, encrypted_credentials FROM public.bank_connections WHERE family_id = $1 AND id = $2", familyID, connID).Scan(&status, &creds)
	})
	if status != ConnectionStateCreated {
		t.Errorf("status = %q, want %q", status, ConnectionStateCreated)
	}
	if len(creds) != 0 {
		t.Errorf("expected encrypted_credentials wiped on cancel, got %s", string(creds))
	}

	// 2. Disconnect connection
	reqDisc := httptest.NewRequest(http.MethodDelete, "/api/v1/connections/"+connID.String(), nil)
	reqDisc.Header.Set("Authorization", "Bearer "+token)

	recDisc := httptest.NewRecorder()
	mux.ServeHTTP(recDisc, reqDisc)

	if recDisc.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200 for disconnect, got %d", recDisc.Code)
	}

	_ = database.WithFamilyTx(ctx, appPool, familyID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT status, encrypted_credentials FROM public.bank_connections WHERE family_id = $1 AND id = $2", familyID, connID).Scan(&status, &creds)
	})
	if status != ConnectionStateDisconnected {
		t.Errorf("status = %q, want %q", status, ConnectionStateDisconnected)
	}
	if len(creds) != 0 {
		t.Errorf("expected encrypted_credentials wiped on disconnect, got %s", string(creds))
	}

	// 3. Attempt reconnect on disconnected connection -> MUST FAIL (terminal state)
	reqReconn := httptest.NewRequest(http.MethodPost, "/api/v1/connections/"+connID.String()+"/reconnect", nil)
	reqReconn.Header.Set("Authorization", "Bearer "+token)

	recReconn := httptest.NewRecorder()
	mux.ServeHTTP(recReconn, reqReconn)

	if recReconn.Code != http.StatusConflict {
		t.Fatalf("expected HTTP 409 Conflict attempting reconnect on terminal disconnected connection, got %d", recReconn.Code)
	}
}

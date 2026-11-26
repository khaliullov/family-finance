package transaction

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/khaliullov/family-finance/internal/crypto"
	"github.com/khaliullov/family-finance/internal/database"
)

func TestAuthHandoff_InsecureTokens_RejectedWith400(t *testing.T) {
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

	testCases := []struct {
		name    string
		payload any
	}{
		{
			name: "cookies_array",
			payload: map[string]any{
				"cookies": []map[string]string{
					{"name": "psid", "value": "secret_cookie_val"},
				},
			},
		},
		{
			name: "session_data",
			payload: map[string]any{
				"session_data": "raw_session_token_12345",
			},
		},
		{
			name: "credentials_field",
			payload: map[string]any{
				"credentials": "plaintext_credentials",
			},
		},
		{
			name: "psid_key",
			payload: map[string]any{
				"psid": "tbank_psid_token_xyz",
			},
		},
		{
			name: "wses_key",
			payload: map[string]any{
				"wses": "tbank_wses_token_xyz",
			},
		},
		{
			name: "password_field",
			payload: map[string]any{
				"password": "secretpassword",
			},
		},
		{
			name: "otp_field",
			payload: map[string]any{
				"otp": "123456",
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			bodyBytes, _ := json.Marshal(tc.payload)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/connections/"+connID.String()+"/auth/session", bytes.NewReader(bodyBytes))
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", "application/json")

			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected HTTP 400 Bad Request for %s, got %d: %s", tc.name, rec.Code, rec.Body.String())
			}

			if !strings.Contains(rec.Body.String(), "Insecure credential handoff rejected") {
				t.Errorf("expected error message to mention insecure credential handoff rejection, got: %s", rec.Body.String())
			}
		})
	}
}

func TestAuthHandoff_RetailBanks_Return501NotImplemented(t *testing.T) {
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
			SET status = 'authenticating', encrypted_credentials = NULL, updated_at = NOW()
			WHERE family_id = $1 AND id = $2
		`, familyID, connID)
		return err
	})

	body := map[string]any{
		"simulated": true,
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/connections/"+connID.String()+"/auth/session", bytes.NewReader(bodyBytes))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("expected HTTP 501 Not Implemented for tbank_personal auth session, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify connection did NOT transition to connected
	var status string
	_ = database.WithFamilyTx(ctx, appPool, familyID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT status FROM public.bank_connections WHERE family_id = $1 AND id = $2", familyID, connID).Scan(&status)
	})
	if status != ConnectionStateAuthenticating {
		t.Errorf("connection status improperly updated: expected %q, got %q", ConnectionStateAuthenticating, status)
	}
}

func TestAuthHandoff_MissingOrInvalidEncryptionKey_FailsClosed500(t *testing.T) {
	// Router initialized without 32-byte crypto key
	mux, _, tokenGen := setupTestRouter(t, nil)
	familyID, userID, connID := setupTenantFixture(t, "fixture")
	token := tokenGen(userID, familyID)

	appPool := getAppPool(t)
	defer appPool.Close()
	ctx := context.Background()

	_ = database.WithFamilyTx(ctx, appPool, familyID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE public.bank_connections
			SET status = 'authenticating', encrypted_credentials = NULL, updated_at = NOW()
			WHERE family_id = $1 AND id = $2
		`, familyID, connID)
		return err
	})

	body := map[string]any{
		"simulated": true,
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/connections/"+connID.String()+"/auth/session", bytes.NewReader(bodyBytes))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected HTTP 500 when crypto key is missing, got %d: %s", rec.Code, rec.Body.String())
	}

	// Ensure zero plaintext persisted
	var creds []byte
	var status string
	_ = database.WithFamilyTx(ctx, appPool, familyID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT status, encrypted_credentials FROM public.bank_connections WHERE family_id = $1 AND id = $2", familyID, connID).Scan(&status, &creds)
	})

	if status != ConnectionStateAuthenticating {
		t.Errorf("status modified despite crypto failure: got %s, want authenticating", status)
	}
	if len(creds) > 0 {
		t.Fatalf("credentials persisted despite crypto failure: len=%d", len(creds))
	}
}

func TestAuthHandoff_ValidSimulatedFixture_EncryptedAndNeverExposed(t *testing.T) {
	cryptoKey := []byte("01234567890123456789012345678901")
	mux, _, tokenGen := setupTestRouter(t, cryptoKey)
	familyID, userID, connID := setupTenantFixture(t, "fixture")
	token := tokenGen(userID, familyID)

	appPool := getAppPool(t)
	defer appPool.Close()
	ctx := context.Background()

	var sessionVerBefore int64
	var fencingBefore int64
	_ = database.WithFamilyTx(ctx, appPool, familyID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE public.bank_connections
			SET status = 'authenticating', updated_at = NOW()
			WHERE family_id = $1 AND id = $2
		`, familyID, connID)
		if err != nil {
			return err
		}
		return tx.QueryRow(ctx, "SELECT session_version, fencing_token FROM public.bank_connections WHERE id = $1", connID).Scan(&sessionVerBefore, &fencingBefore)
	})

	body := map[string]any{
		"simulated": true,
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/connections/"+connID.String()+"/auth/session", bytes.NewReader(bodyBytes))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d: %s", rec.Code, rec.Body.String())
	}

	respBody := rec.Body.String()
	if strings.Contains(respBody, "secret") || strings.Contains(respBody, "token") {
		t.Fatalf("credentials or tokens exposed in API response body: %s", respBody)
	}

	// Verify database persistence
	var storedCreds []byte
	var status string
	var sessionVerAfter int64
	var fencingAfter int64
	var credHash *string
	err := database.WithFamilyTx(ctx, appPool, familyID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT status, encrypted_credentials, session_version, fencing_token, credential_hash
			FROM public.bank_connections
			WHERE family_id = $1 AND id = $2
		`, familyID, connID).Scan(&status, &storedCreds, &sessionVerAfter, &fencingAfter, &credHash)
	})
	if err != nil {
		t.Fatal(err)
	}

	if status != ConnectionStateConnected {
		t.Errorf("status = %q, want %q", status, ConnectionStateConnected)
	}
	if sessionVerAfter != sessionVerBefore+1 {
		t.Errorf("session_version = %d, want %d", sessionVerAfter, sessionVerBefore+1)
	}
	if fencingAfter != fencingBefore+1 {
		t.Errorf("fencing_token = %d, want %d", fencingAfter, fencingBefore+1)
	}
	if credHash == nil || len(*credHash) != 64 {
		t.Fatalf("expected 64-hex SHA-256 credential_hash, got %v", credHash)
	}
	if _, err := hex.DecodeString(*credHash); err != nil {
		t.Fatalf("credential_hash is not valid hex: %v", err)
	}

	// Decrypt with key to verify it is encrypted ciphertext
	decrypted, err := crypto.Decrypt(cryptoKey, storedCreds)
	if err != nil {
		t.Fatalf("failed to decrypt persisted credentials: %v", err)
	}
	if !strings.Contains(string(decrypted), `"simulated":true`) {
		t.Errorf("decrypted payload missing simulated marker: %s", string(decrypted))
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

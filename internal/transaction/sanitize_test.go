package transaction

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/khaliullov/family-finance/internal/database"
)

func TestSanitizeRawPayload_RedactsSensitiveKeys(t *testing.T) {
	raw := []byte(`{
		"token": "secret_token_123",
		"AccessToken": "access_xyz",
		"refresh_token": "refresh_abc",
		"Password": "super_secret_pw",
		"apiKey": "key_9999",
		"client_secret": "cs_5555",
		"session_token": "st_7777",
		"cvv": "123",
		"pin": "0000",
		"amount": 15000,
		"currency": "RUB",
		"description": "Coffee Shop"
	}`)

	sanitized, err := SanitizeRawPayload(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(sanitized, &parsed); err != nil {
		t.Fatalf("failed to unmarshal sanitized payload: %v", err)
	}

	// Verify sensitive keys are redacted
	for _, key := range []string{"token", "AccessToken", "refresh_token", "Password", "apiKey", "client_secret", "session_token", "cvv", "pin"} {
		val, ok := parsed[key]
		if !ok {
			t.Errorf("expected key %s to be preserved with redacted value", key)
			continue
		}
		if val != "[REDACTED]" {
			t.Errorf("expected key %s to be [REDACTED], got %v", key, val)
		}
	}

	// Verify financial fields are preserved
	if parsed["amount"] != float64(15000) {
		t.Errorf("expected amount 15000, got %v", parsed["amount"])
	}
	if parsed["currency"] != "RUB" {
		t.Errorf("expected currency RUB, got %v", parsed["currency"])
	}
	if parsed["description"] != "Coffee Shop" {
		t.Errorf("expected description 'Coffee Shop', got %v", parsed["description"])
	}
}

func TestSanitizeRawPayload_MasksPANs(t *testing.T) {
	raw := []byte(`{
		"pan": "4111222233334444",
		"card_number": "5555 6666 7777 8888",
		"account_pan": "2200-1111-2222-3333",
		"description": "Payment using card 4111222233334444 at grocery store",
		"safe_info": "Order #2026-11-23-001 with total 500 RUB"
	}`)

	sanitized, err := SanitizeRawPayload(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(sanitized, &parsed); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if parsed["pan"] != "****4444" {
		t.Errorf("expected pan to be ****4444, got %v", parsed["pan"])
	}
	if parsed["card_number"] != "****8888" {
		t.Errorf("expected card_number to be ****8888, got %v", parsed["card_number"])
	}
	if parsed["account_pan"] != "****3333" {
		t.Errorf("expected account_pan to be ****3333, got %v", parsed["account_pan"])
	}

	// String value with embedded PAN
	desc, ok := parsed["description"].(string)
	if !ok || !strings.Contains(desc, "****4444") {
		t.Errorf("expected embedded PAN in description to be masked, got %s", desc)
	}

	// Date-like format in safe_info should NOT be masked
	safeInfo, ok := parsed["safe_info"].(string)
	if !ok || !strings.Contains(safeInfo, "Order #2026-11-23-001") {
		t.Errorf("expected safe_info date not to be destroyed, got %s", safeInfo)
	}
}

func TestSanitizeRawPayload_HeadersAndNested(t *testing.T) {
	raw := []byte(`{
		"headers": {
			"Authorization": "Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9...",
			"Cookie": "session_id=abcdef123456",
			"User-Agent": "FamilyFinanceSync/1.0"
		},
		"data": {
			"sub_account": {
				"auth_token": "deep_token_abc",
				"balance": 250000
			}
		}
	}`)

	sanitized, err := SanitizeRawPayload(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(sanitized, &parsed); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	headers, ok := parsed["headers"].(map[string]any)
	if !ok {
		t.Fatalf("headers not a map: %v", parsed["headers"])
	}
	if headers["Authorization"] != "[REDACTED]" {
		t.Errorf("expected Authorization header to be [REDACTED], got %v", headers["Authorization"])
	}
	if headers["Cookie"] != "[REDACTED]" {
		t.Errorf("expected Cookie header to be [REDACTED], got %v", headers["Cookie"])
	}
	if headers["User-Agent"] != "FamilyFinanceSync/1.0" {
		t.Errorf("expected User-Agent preserved, got %v", headers["User-Agent"])
	}

	data := parsed["data"].(map[string]any)
	sub := data["sub_account"].(map[string]any)
	if sub["auth_token"] != "[REDACTED]" {
		t.Errorf("expected nested auth_token to be [REDACTED], got %v", sub["auth_token"])
	}
	if sub["balance"] != float64(250000) {
		t.Errorf("expected balance 250000, got %v", sub["balance"])
	}
}

func TestSanitizeRawPayload_FailsOnInvalidJSON(t *testing.T) {
	invalidJSON := []byte(`not json at all <xml>bad</xml>`)
	_, err := SanitizeRawPayload(invalidJSON)
	if err == nil {
		t.Fatal("expected error on invalid JSON, got nil")
	}
	if !errors.Is(err, ErrInvalidRawPayload) {
		t.Errorf("expected ErrInvalidRawPayload, got: %v", err)
	}

	emptyJSON := []byte(``)
	_, err = SanitizeRawPayload(emptyJSON)
	if err == nil {
		t.Fatal("expected error on empty payload, got nil")
	}
	if !errors.Is(err, ErrEmptyPayload) {
		t.Errorf("expected ErrEmptyPayload, got: %v", err)
	}
}

func TestPreserveRawPayload_DeduplicationScope(t *testing.T) {
	pool := getWorkerPool(t)
	defer pool.Close()
	ctx := context.Background()

	familyID, _, connID1 := setupTenantFixture(t, "fixture")

	// Create second bank connection for same family
	appPool := getAppPool(t)
	defer appPool.Close()

	connID2 := uuid.New()
	err := database.WithFamilyTx(ctx, appPool, familyID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO public.bank_connections (id, family_id, authorized_by_user_id, provider, name, status)
			SELECT $1, family_id, authorized_by_user_id, 'fixture', 'Second Connection', 'active'
			FROM public.bank_connections WHERE id = $2
		`, connID2, connID1)
		return err
	})
	if err != nil {
		t.Fatalf("failed to create second connection: %v", err)
	}

	payload := []byte(`{"transactions": [], "status": "ok"}`)

	var id1A, id1B, id2 uuid.UUID
	err = database.WithFamilyTx(ctx, pool, familyID, func(tx pgx.Tx) error {
		var err error
		id1A, err = PreserveRawPayload(ctx, tx, familyID, &connID1, "fixture", payload)
		if err != nil {
			return err
		}
		// Same connection + same payload -> must deduplicate
		id1B, err = PreserveRawPayload(ctx, tx, familyID, &connID1, "fixture", payload)
		if err != nil {
			return err
		}
		// Different connection + same payload -> must NOT reuse conn1's payload record
		id2, err = PreserveRawPayload(ctx, tx, familyID, &connID2, "fixture", payload)
		return err
	})
	if err != nil {
		t.Fatalf("PreserveRawPayload failed: %v", err)
	}

	if id1A != id1B {
		t.Errorf("expected identical payloads on same connection to deduplicate: id1A=%s, id1B=%s", id1A, id1B)
	}

	if id2 == id1A {
		t.Errorf("expected payload on different connection to have distinct raw payload record, but got same ID: %s", id2)
	}
}

func TestSanitize_NumericPANPrecision(t *testing.T) {
	// 19-digit card PAN as JSON number: float64 would corrupt the lowest digits (IEEE 754 precision limit is ~15-17 digits)
	raw := []byte(`{
		"card_number": 4111222233334444555,
		"pan": 5555666677778888,
		"account_balance": 999999999999999999
	}`)

	sanitized, err := SanitizeRawPayload(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(sanitized, &parsed); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if parsed["card_number"] != "****4555" {
		t.Errorf("expected card_number to be ****4555, got %v", parsed["card_number"])
	}
	if parsed["pan"] != "****8888" {
		t.Errorf("expected pan to be ****8888, got %v", parsed["pan"])
	}
	// Verify that non-PAN large numbers (like account balance) are preserved without float conversion
	if !strings.Contains(string(sanitized), "999999999999999999") {
		t.Errorf("expected large account_balance 999999999999999999 to be preserved verbatim, got: %s", string(sanitized))
	}
}

func TestSanitize_LegitimateNumericIdentifiersPreserved(t *testing.T) {
	raw := []byte(`{
		"uuid_field": "c748c9bb-3687-4ee2-9659-dc6d97cbe60b",
		"numeric_uuid": "12345678-1234-1234-1234-123456789012",
		"timestamp_iso": "2026-11-23T23:05:44Z",
		"timestamp_ms": 1700000000000,
		"decimal_amount": 1234567890123.45,
		"tx_id": "tx_9876543210123456",
		"ext_id": "EXT-1234567890123456",
		"description": "Invoice for order EXT-1234567890123456 total 1234567890123.45 USD at 2026-11-23"
	}`)

	sanitized, err := SanitizeRawPayload(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(sanitized, &parsed); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	if parsed["uuid_field"] != "c748c9bb-3687-4ee2-9659-dc6d97cbe60b" {
		t.Errorf("uuid corrupted: %v", parsed["uuid_field"])
	}
	if parsed["numeric_uuid"] != "12345678-1234-1234-1234-123456789012" {
		t.Errorf("numeric uuid corrupted: %v", parsed["numeric_uuid"])
	}
	if parsed["timestamp_iso"] != "2026-11-23T23:05:44Z" {
		t.Errorf("timestamp corrupted: %v", parsed["timestamp_iso"])
	}
	if parsed["tx_id"] != "tx_9876543210123456" {
		t.Errorf("tx_id corrupted: %v", parsed["tx_id"])
	}
	if parsed["ext_id"] != "EXT-1234567890123456" {
		t.Errorf("ext_id corrupted: %v", parsed["ext_id"])
	}

	desc := parsed["description"].(string)
	if strings.Contains(desc, "****") {
		t.Errorf("description legitimate IDs were falsely masked: %s", desc)
	}
	if !strings.Contains(desc, "1234567890123.45") || !strings.Contains(desc, "EXT-1234567890123456") {
		t.Errorf("expected original values in description, got: %s", desc)
	}
}

func TestSanitize_PreciseKeyMatching_NoOverRedaction(t *testing.T) {
	raw := []byte(`{
		"shipping_address": "742 Evergreen Terrace",
		"pinpoint_location": "Store 42",
		"alpine_ski_pass": "PASS-999",
		"spinning_wheel": true,
		"pin": "1234",
		"card_pin": "5678",
		"Pin-Code": "9999",
		"X-Api-Key": "my-secret-key",
		"Client_Secret": "top-secret",
		"Auth-Token": "bearer-val"
	}`)

	sanitized, err := SanitizeRawPayload(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(sanitized, &parsed); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	// Legitimate business fields containing 'pin' must NOT be redacted
	if parsed["shipping_address"] != "742 Evergreen Terrace" {
		t.Errorf("shipping_address was falsely redacted: %v", parsed["shipping_address"])
	}
	if parsed["pinpoint_location"] != "Store 42" {
		t.Errorf("pinpoint_location was falsely redacted: %v", parsed["pinpoint_location"])
	}
	if parsed["alpine_ski_pass"] != "PASS-999" {
		t.Errorf("alpine_ski_pass was falsely redacted: %v", parsed["alpine_ski_pass"])
	}
	if parsed["spinning_wheel"] != true {
		t.Errorf("spinning_wheel was falsely redacted: %v", parsed["spinning_wheel"])
	}

	// Sensitive keys with various cases and separators must be redacted
	for _, key := range []string{"pin", "card_pin", "Pin-Code", "X-Api-Key", "Client_Secret", "Auth-Token"} {
		if parsed[key] != "[REDACTED]" {
			t.Errorf("expected sensitive key %q to be [REDACTED], got %v", key, parsed[key])
		}
	}
}

func TestSanitize_FailClosed_MalformedAndTrailingJSON(t *testing.T) {
	testCases := []struct {
		name string
		raw  []byte
	}{
		{"truncated json", []byte(`{"status": "ok", "items": [1, 2`)},
		{"trailing tokens", []byte(`{"status": "ok"} {"extra": 1}`)},
		{"garbage prefix", []byte(`HTTP/1.1 200 OK\r\n{"status": "ok"}`)},
		{"empty payload", []byte(``)},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := SanitizeRawPayload(tc.raw)
			if err == nil {
				t.Fatalf("expected error for %s, got nil", tc.name)
			}
		})
	}
}

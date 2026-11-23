package transaction

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var (
	// ErrInvalidRawPayload is returned when provider payload fails JSON structural validation.
	ErrInvalidRawPayload = errors.New("invalid raw payload: payload must be valid JSON to enter sanitization boundary")
	// ErrSanitizationFailed is returned when sanitization cannot safely process the payload.
	ErrSanitizationFailed = errors.New("sanitization failed: cannot safely sanitize payload without destroying required evidence")
	// ErrEmptyPayload is returned when the payload is empty.
	ErrEmptyPayload = errors.New("cannot sanitize empty payload")
)

// Regex to detect 13-19 digit card numbers (PANs) with optional spaces or hyphens.
// e.g. 4111 2222 3333 4444, 4111-2222-3333-4444, 4111222233334444
var panRegex = regexp.MustCompile(`\b(?:\d[ -]?){12,18}\d\b`)

// Regex to detect Bearer or Basic authorization tokens in strings.
var authHeaderRegex = regexp.MustCompile(`(?i)\b(Bearer|Basic)\s+[A-Za-z0-9\-_.~+/]+=*`)

// Prohibited key patterns (case-insensitive, matched with punctuation stripped).
var prohibitedKeySubstrings = []string{
	"token",
	"authorization",
	"auth_token",
	"authtoken",
	"access_token",
	"accesstoken",
	"refresh_token",
	"refreshtoken",
	"password",
	"passwd",
	"secret",
	"client_secret",
	"clientsecret",
	"api_key",
	"apikey",
	"private_key",
	"privatekey",
	"cookie",
	"set_cookie",
	"setcookie",
	"credential",
	"credentials",
	"session_token",
	"sessiontoken",
	"cvv",
	"cvc",
	"pin",
}

// Card PAN key patterns.
var panKeySubstrings = []string{
	"pan",
	"card_number",
	"cardnumber",
	"card_num",
	"cardnum",
	"account_pan",
	"accountpan",
	"primary_account_number",
}

// Header container key patterns.
var headerKeySubstrings = []string{
	"header",
	"headers",
	"http_header",
	"http_headers",
	"request_headers",
	"response_headers",
}

func normalizeKey(k string) string {
	s := strings.ToLower(k)
	s = strings.ReplaceAll(s, "-", "")
	s = strings.ReplaceAll(s, "_", "")
	return s
}

func isProhibitedKey(k string) bool {
	norm := normalizeKey(k)
	for _, sub := range prohibitedKeySubstrings {
		if strings.Contains(norm, strings.ReplaceAll(sub, "_", "")) {
			return true
		}
	}
	return false
}

func isPANKey(k string) bool {
	norm := normalizeKey(k)
	for _, sub := range panKeySubstrings {
		cleanSub := strings.ReplaceAll(sub, "_", "")
		if norm == cleanSub || strings.HasSuffix(norm, cleanSub) {
			return true
		}
	}
	return false
}

func isHeaderKey(k string) bool {
	norm := normalizeKey(k)
	for _, sub := range headerKeySubstrings {
		if norm == strings.ReplaceAll(sub, "_", "") {
			return true
		}
	}
	return false
}

// maskPAN replaces all but the last 4 digits of a card number with asterisks.
func maskPAN(s string) string {
	// Extract digits
	var digits []rune
	for _, r := range s {
		if r >= '0' && r <= '9' {
			digits = append(digits, r)
		}
	}
	if len(digits) < 13 || len(digits) > 19 {
		return s
	}
	last4 := string(digits[len(digits)-4:])
	return "****" + last4
}

// sanitizeString sanitizes string values by masking embedded PANs and auth header patterns.
func sanitizeString(val string) string {
	// Mask auth tokens like "Bearer xxx"
	val = authHeaderRegex.ReplaceAllString(val, "[REDACTED]")

	// Mask full card numbers
	val = panRegex.ReplaceAllStringFunc(val, func(match string) string {
		var digits []rune
		for _, r := range match {
			if r >= '0' && r <= '9' {
				digits = append(digits, r)
			}
		}
		if len(digits) >= 13 && len(digits) <= 19 {
			return "****" + string(digits[len(digits)-4:])
		}
		return match
	})

	return val
}

// sanitizeHeaders removes or redacts authentication headers from header maps.
func sanitizeHeaders(m map[string]any) map[string]any {
	cleaned := make(map[string]any, len(m))
	for k, v := range m {
		norm := strings.ToLower(strings.TrimSpace(k))
		if norm == "authorization" || norm == "proxy-authorization" ||
			norm == "cookie" || norm == "set-cookie" ||
			norm == "x-api-key" || norm == "api-key" ||
			strings.Contains(norm, "token") || strings.Contains(norm, "secret") {
			cleaned[k] = "[REDACTED]"
			continue
		}
		cleaned[k] = sanitizeValue(v)
	}
	return cleaned
}

// sanitizeValue recursively inspects and sanitizes arbitrary JSON values.
func sanitizeValue(v any) any {
	if v == nil {
		return nil
	}

	switch val := v.(type) {
	case string:
		return sanitizeString(val)
	case map[string]any:
		return sanitizeMap(val)
	case []any:
		cleanedSlice := make([]any, len(val))
		for i, item := range val {
			cleanedSlice[i] = sanitizeValue(item)
		}
		return cleanedSlice
	default:
		return v
	}
}

// sanitizeMap inspects each key-value pair in a map and applies sanitization rules.
func sanitizeMap(m map[string]any) map[string]any {
	cleaned := make(map[string]any, len(m))
	for k, v := range m {
		// 1. Prohibited sensitive keys -> redact value
		if isProhibitedKey(k) {
			cleaned[k] = "[REDACTED]"
			continue
		}

		// 2. HTTP header sections -> sanitize headers
		if isHeaderKey(k) {
			if headerMap, ok := v.(map[string]any); ok {
				cleaned[k] = sanitizeHeaders(headerMap)
				continue
			}
		}

		// 3. Card PAN keys -> mask to last 4 digits
		if isPANKey(k) {
			switch pv := v.(type) {
			case string:
				cleaned[k] = maskPAN(pv)
				continue
			case float64:
				// Sometimes PANs are sent as numbers
				panStr := fmt.Sprintf("%.0f", pv)
				if len(panStr) >= 13 && len(panStr) <= 19 {
					cleaned[k] = maskPAN(panStr)
					continue
				}
			}
		}

		// 4. General recursive traversal
		cleaned[k] = sanitizeValue(v)
	}
	return cleaned
}

// SanitizeRawPayload enforces an explicit sanitization contract on provider responses.
//
// Guarantees:
// 1. Fails closed: non-JSON or malformed payloads return ErrInvalidRawPayload.
// 2. Credentials, tokens, cookies, auth headers, and full PANs are redacted or masked.
// 3. Preserves financial data, transaction IDs, amounts, currencies, and descriptions.
// 4. Returns canonical, deterministic JSON bytes suitable for hashing and byte-for-byte storage.
//
// Limitations:
// - Payloads must be valid JSON; unstructured binary payloads must be handled prior to ingestion.
func SanitizeRawPayload(rawBytes []byte) ([]byte, error) {
	if len(rawBytes) == 0 {
		return nil, ErrEmptyPayload
	}

	var root any
	if err := json.Unmarshal(rawBytes, &root); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRawPayload, err)
	}

	sanitizedRoot := sanitizeValue(root)

	sanitizedBytes, err := json.Marshal(sanitizedRoot)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSanitizationFailed, err)
	}

	return sanitizedBytes, nil
}

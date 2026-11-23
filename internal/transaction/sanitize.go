package transaction

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// Regex to detect Bearer or Basic authorization tokens in strings.
var authHeaderRegex = regexp.MustCompile(`(?i)\b(Bearer|Basic)\s+[A-Za-z0-9\-_.~+/]+=*`)

// Regex to detect standard UUIDs (8-4-4-4-12 hex format) to ensure they are never mistaken for PANs.
var uuidRegex = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)

// Regex to detect standard ISO-8601 dates and timestamps so they are never masked.
var dateRegex = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}(?:[T\s]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})?)?\b`)

// Regex to detect standard grouped card numbers:
// 4-4-4-4 (16 digits), 4-6-5 (15 digits, Amex), or 4-4-4-4-3 (19 digits)
var formattedCardRegex = regexp.MustCompile(`\b(?:\d{4}[ -]\d{4}[ -]\d{4}[ -]\d{1,7}|\d{4}[ -]\d{6}[ -]\d{4,5})\b`)

// Regex to find candidate continuous digit sequences of 13-19 digits.
var candidatePanRegex = regexp.MustCompile(`\d{13,19}`)

// Exact normalized prohibited keys (lowercased, stripped of _ and -).
var prohibitedExactKeys = map[string]struct{}{
	"token":              {},
	"authtoken":          {},
	"accesstoken":        {},
	"refreshtoken":       {},
	"sessiontoken":       {},
	"bearertoken":        {},
	"oauthtoken":         {},
	"securitytoken":      {},
	"idtoken":            {},
	"devicetoken":        {},
	"pushtoken":          {},
	"jwt":                {},
	"jwttoken":           {},
	"password":           {},
	"passwd":             {},
	"passcode":           {},
	"secret":             {},
	"clientsecret":       {},
	"sharedsecret":       {},
	"apisecret":          {},
	"appsecret":          {},
	"privatekey":         {},
	"secretkey":          {},
	"apikey":             {},
	"xapikey":            {},
	"cookie":             {},
	"setcookie":          {},
	"sessioncookie":      {},
	"authorization":      {},
	"proxyauthorization": {},
	"credential":         {},
	"credentials":        {},
	"cvv":                {},
	"cvc":                {},
	"cvv2":               {},
	"cvc2":               {},
	"cid":                {},
	"pin":                {},
	"pincode":            {},
	"cardpin":            {},
	"userpin":            {},
	"atmpin":             {},
	"pinentry":           {},
}

// Exact normalized PAN keys.
var panExactKeys = map[string]struct{}{
	"pan":                  {},
	"cardnumber":           {},
	"cardnum":              {},
	"accountpan":           {},
	"primaryaccountnumber": {},
	"cardpan":              {},
	"maskedpan":            {},
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
	var b strings.Builder
	for _, r := range strings.ToLower(k) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func splitKeyTokens(k string) []string {
	return strings.FieldsFunc(k, func(r rune) bool {
		return r == '_' || r == '-' || r == '.' || r == ' ' || r == '/' || r == ':'
	})
}

func isProhibitedKey(k string) bool {
	norm := normalizeKey(k)
	if _, ok := prohibitedExactKeys[norm]; ok {
		return true
	}
	tokens := splitKeyTokens(k)
	for _, tok := range tokens {
		tokNorm := normalizeKey(tok)
		if tokNorm == "pin" || tokNorm == "pincode" || tokNorm == "cardpin" || tokNorm == "cvv" || tokNorm == "cvc" || tokNorm == "password" || tokNorm == "passwd" {
			return true
		}
		if tokNorm == "token" && len(tokens) == 1 {
			return true
		}
		if tokNorm == "authtoken" || tokNorm == "accesstoken" || tokNorm == "refreshtoken" || tokNorm == "clientsecret" {
			return true
		}
	}
	if strings.HasSuffix(norm, "password") || strings.HasSuffix(norm, "passwd") || strings.HasSuffix(norm, "clientsecret") || strings.HasSuffix(norm, "apisecret") {
		return true
	}
	return false
}

func isPANKey(k string) bool {
	norm := normalizeKey(k)
	if _, ok := panExactKeys[norm]; ok {
		return true
	}
	if strings.HasSuffix(norm, "cardnumber") || strings.HasSuffix(norm, "cardnum") || strings.HasSuffix(norm, "primaryaccountnumber") {
		return true
	}
	tokens := splitKeyTokens(k)
	for _, tok := range tokens {
		if normalizeKey(tok) == "pan" {
			return true
		}
	}
	return false
}

func isHeaderKey(k string) bool {
	norm := normalizeKey(k)
	for _, sub := range headerKeySubstrings {
		if norm == normalizeKey(sub) {
			return true
		}
	}
	return false
}

// maskPAN replaces all but the last 4 digits of a card number with asterisks.
func maskPAN(s string) string {
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

func isDigit(b byte) bool {
	return b >= '0' && b <= '9'
}

func isAlpha(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// sanitizeString sanitizes string values by masking embedded PANs and auth header patterns.
//
// Unavoidable Limitations of PAN detection in unstructured descriptions:
//  1. Unstructured text without explicit delimiters may contain legitimate 13-19 digit numeric IDs
//     (such as invoice references without prefix) that match PAN patterns. To minimize false positives,
//     numbers adjacent to decimal points (currency amounts), UUIDs, ISO dates, and alphanumeric IDs
//     are preserved.
//  2. Non-standard obfuscations (e.g. interspersed non-alphanumeric punctuation) in unstructured
//     descriptions may not be detected by standard patterns to avoid over-redacting business descriptions.
func sanitizeString(val string) string {
	// 1. Mask authorization tokens like "Bearer xxx" or "Basic yyy"
	val = authHeaderRegex.ReplaceAllString(val, "[REDACTED]")

	// 2. Mask formatted card numbers (e.g. "4111 2222 3333 4444" or "4111-2222-3333-4444")
	val = formattedCardRegex.ReplaceAllStringFunc(val, func(match string) string {
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

	// 3. Mask continuous 13-19 digit PANs in free text, distinguishing them from:
	//    - Decimal monetary values (e.g. 1234567890123.45)
	//    - Prefixed transaction identifiers (e.g. tx_9876543210123456, EXT-1234567890123456)
	//    - UUIDs and ISO timestamps
	uuidMatches := uuidRegex.FindAllStringIndex(val, -1)
	dateMatches := dateRegex.FindAllStringIndex(val, -1)

	isOverlapping := func(start, end int, matches [][]int) bool {
		for _, m := range matches {
			if start >= m[0] && end <= m[1] {
				return true
			}
		}
		return false
	}

	locs := candidatePanRegex.FindAllStringIndex(val, -1)
	if len(locs) == 0 {
		return val
	}

	var buf bytes.Buffer
	lastIdx := 0
	for _, loc := range locs {
		start, end := loc[0], loc[1]
		if start < lastIdx {
			continue
		}

		// Check decimal point immediately after or before
		if end < len(val) && val[end] == '.' && end+1 < len(val) && isDigit(val[end+1]) {
			continue
		}
		if start > 0 && val[start-1] == '.' && start > 1 && isDigit(val[start-2]) {
			continue
		}

		// Check alphanumeric prefix (e.g. tx_... or ORDER...)
		if start > 0 {
			prev := val[start-1]
			if isAlpha(prev) || prev == '_' {
				continue
			}
			if prev == '-' && start > 1 && isAlpha(val[start-2]) {
				continue
			}
		}

		// Check alphanumeric suffix
		if end < len(val) {
			next := val[end]
			if isAlpha(next) || next == '_' {
				continue
			}
			if next == '-' && end+1 < len(val) && isAlpha(val[end+1]) {
				continue
			}
		}

		// Check UUID or Date overlap
		if isOverlapping(start, end, uuidMatches) || isOverlapping(start, end, dateMatches) {
			continue
		}

		// Legitimate free-text PAN: mask while preserving last 4 digits
		buf.WriteString(val[lastIdx:start])
		buf.WriteString("****" + val[end-4:end])
		lastIdx = end
	}

	if lastIdx < len(val) {
		buf.WriteString(val[lastIdx:])
	}

	return buf.String()
}

// SanitizeDiscrepancyValue sanitizes a text value before persistence in reconciliation_discrepancies.
func SanitizeDiscrepancyValue(val string) string {
	return sanitizeString(val)
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
	case json.Number:
		// Standalone JSON number: preserve verbatim precision for financial amounts/timestamps
		return val
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
		// 1. Prohibited sensitive keys -> redact value completely
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

		// 3. Card PAN keys -> mask to last 4 digits without float64 precision loss
		if isPANKey(k) {
			switch pv := v.(type) {
			case string:
				cleaned[k] = maskPAN(pv)
				continue
			case json.Number:
				cleaned[k] = maskPAN(pv.String())
				continue
			case []any:
				panList := make([]any, len(pv))
				for i, item := range pv {
					switch iv := item.(type) {
					case string:
						panList[i] = maskPAN(iv)
					case json.Number:
						panList[i] = maskPAN(iv.String())
					default:
						panList[i] = sanitizeValue(item)
					}
				}
				cleaned[k] = panList
				continue
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
// 1. Fails closed: non-JSON, empty, or malformed payloads return ErrInvalidRawPayload.
// 2. Preserves JSON numeric precision using json.Decoder.UseNumber (no float64 conversion).
// 3. Credentials, tokens, cookies, auth headers, and full PANs are redacted or masked.
// 4. Preserves financial amounts, transaction IDs, currencies, and legitimate descriptions.
// 5. Returns canonical, deterministic JSON bytes suitable for hashing and byte-for-byte storage.
func SanitizeRawPayload(rawBytes []byte) ([]byte, error) {
	if len(rawBytes) == 0 {
		return nil, ErrEmptyPayload
	}

	d := json.NewDecoder(bytes.NewReader(rawBytes))
	d.UseNumber()

	var root any
	if err := d.Decode(&root); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRawPayload, err)
	}

	// Fail closed if there are trailing tokens after the first root JSON entity
	var trailing any
	if err := d.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("%w: extra trailing tokens after top-level JSON", ErrInvalidRawPayload)
	}

	sanitizedRoot := sanitizeValue(root)

	sanitizedBytes, err := json.Marshal(sanitizedRoot)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSanitizationFailed, err)
	}

	return sanitizedBytes, nil
}

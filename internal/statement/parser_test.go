package statement

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"
)

// 19. Upload and parser resource limits:
// Verify file size bounds (10 MB maximum), row count bounds (50,000 rows maximum),
// and parser plugin registration and sniffing.
func TestResourceLimits_FileTooLarge(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()

	ctx := context.Background()
	familyID, userID, accountID := setupStatementTenantFixture(t)

	registry := NewParserRegistry()
	svc := NewService(appPool, testCryptoKey, registry)

	parser := &StubParser{}

	// File exceeds 10 MB limit by 1 byte
	largeSize := int64(10*1024*1024 + 1)
	r := io.LimitReader(bytes.NewReader(bytes.Repeat([]byte("a"), 1024)), largeSize)

	_, err := svc.UploadAndStage(
		ctx,
		familyID, userID, accountID,
		"oversized.csv",
		r,
		largeSize,
		parser,
	)
	if err != ErrFileTooLarge {
		t.Fatalf("expected ErrFileTooLarge for size %d, got: %v", largeSize, err)
	}

	// Zero size file
	_, err = svc.UploadAndStage(
		ctx,
		familyID, userID, accountID,
		"empty.csv",
		bytes.NewReader([]byte{}),
		0,
		parser,
	)
	if err != ErrFileTooLarge {
		t.Fatalf("expected ErrFileTooLarge for size 0, got: %v", err)
	}

	// Missing parser
	_, err = svc.UploadAndStage(
		ctx,
		familyID, userID, accountID,
		"valid.csv",
		bytes.NewReader([]byte("valid content")),
		13,
		nil,
	)
	if err != ErrParserNotFound {
		t.Fatalf("expected ErrParserNotFound for nil parser, got: %v", err)
	}
}

func TestResourceLimits_TooManyRows(t *testing.T) {
	appPool := getAppPool(t)
	defer appPool.Close()

	ctx := context.Background()
	familyID, userID, accountID := setupStatementTenantFixture(t)

	registry := NewParserRegistry()
	svc := NewService(appPool, testCryptoKey, registry)

	// Stub parser producing 50,001 rows
	now := time.Now().UTC()
	rows := make([]StagedRowDTO, 50001)
	for i := range rows {
		rows[i] = StagedRowDTO{
			RowIndex:           i + 1,
			TransactionTime:    now,
			AmountMinor:        -100,
			Currency:           "RUB",
			AccountAmountMinor: -100,
			AccountCurrency:    "RUB",
			BookingStatus:      "posted",
			OperationType:      "purchase",
			RawDescription:     "Item",
		}
	}

	parser := &StubParser{
		ParsedData: &ParsedStatementDTO{
			FormatID:      "test_format",
			ParserVersion: "1.0.0",
			Rows:          rows,
		},
	}

	_, err := svc.UploadAndStage(
		ctx,
		familyID, userID, accountID,
		"many_rows.csv",
		bytes.NewReader([]byte("dummy content")),
		13,
		parser,
	)
	if err != ErrTooManyRows {
		t.Fatalf("expected ErrTooManyRows for 50,001 rows, got: %v", err)
	}
}

func TestParserRegistry_RegistrationAndSniffing(t *testing.T) {
	registry := NewParserRegistry()

	csvParser := &StubParser{
		ProfileData: StatementProfile{
			FormatID:    "generic_csv",
			DisplayName: "Generic CSV Parser",
		},
	}

	registry.Register(csvParser)

	// Lookup by format ID
	p, found := registry.Get("generic_csv")
	if !found {
		t.Fatalf("expected to find generic_csv parser in registry")
	}
	if p.Profile().DisplayName != "Generic CSV Parser" {
		t.Errorf("expected displayName 'Generic CSV Parser', got: %s", p.Profile().DisplayName)
	}

	// Lookup non-existent format
	_, found = registry.Get("non_existent")
	if found {
		t.Errorf("expected not to find non_existent parser")
	}

	// Sniffing
	sniffed, ok := registry.Sniff([]byte("Date,Amount,Description\n"))
	if !ok || sniffed == nil {
		t.Fatalf("expected successful sniffing for registered parser")
	}
}

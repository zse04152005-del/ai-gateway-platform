//go:build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"sort"
	"testing"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/execution"
	"github.com/zse04152005-del/ai-gateway-platform/internal/metering"
)

const usageTaxonomyRequestID = "integration-usage-taxonomy"

var usageTaxonomyLiteralPattern = regexp.MustCompile(`'([a-z][a-z0-9_]*)'::text`)

func TestUsageTaxonomySchemaParity(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL is not set")
	}
	database, err := sql.Open("postgres", databaseURL)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	database.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := database.PingContext(ctx); err != nil {
		t.Fatalf("database.PingContext() error = %v", err)
	}

	cleanupUsageLedgerFixtures(t, database)
	cleanupModelListFixtures(t, database)
	t.Cleanup(func() {
		cleanupUsageLedgerFixtures(t, database)
		cleanupModelListFixtures(t, database)
	})
	seedModelListCatalog(ctx, t, database)
	seedExecutionVirtualKey(ctx, t, database)

	now := time.Now().UTC().Truncate(time.Microsecond)
	recorder, err := execution.NewPostgresRecorder(database, func() time.Time {
		now = now.Add(time.Microsecond)
		return now
	}, rand.Reader)
	if err != nil {
		t.Fatalf("execution.NewPostgresRecorder() error = %v", err)
	}
	_ = startExecutionRequest(ctx, t, recorder, usageTaxonomyRequestID)

	sequence := 0
	for _, tokenType := range metering.TokenTypes() {
		for _, source := range metering.Sources() {
			sequence++
			insertUsageLedgerEntry(ctx, t, database, usageTaxonomyEventID(sequence), modelListTenantOneID,
				usageTaxonomyRequestID, nil, string(tokenType), int64(sequence), string(source),
				now.Add(time.Duration(sequence)*time.Microsecond))
		}
	}
	wantCount := len(metering.TokenTypes()) * len(metering.Sources())
	var entryCount, tokenTypeCount, sourceCount int
	err = database.QueryRowContext(ctx, `
		SELECT count(*), count(DISTINCT token_type), count(DISTINCT source)
		FROM app.usage_ledger_entries
		WHERE request_id = $1`, usageTaxonomyRequestID,
	).Scan(&entryCount, &tokenTypeCount, &sourceCount)
	if err != nil || entryCount != wantCount || tokenTypeCount != len(metering.TokenTypes()) ||
		sourceCount != len(metering.Sources()) {
		t.Fatalf("taxonomy rows = entries:%d types:%d sources:%d, want %d/%d/%d; error:%v",
			entryCount, tokenTypeCount, sourceCount, wantCount, len(metering.TokenTypes()), len(metering.Sources()), err)
	}
	wantTokenTypes := make([]string, 0, len(metering.TokenTypes()))
	for _, tokenType := range metering.TokenTypes() {
		wantTokenTypes = append(wantTokenTypes, string(tokenType))
	}
	wantSources := make([]string, 0, len(metering.Sources()))
	for _, source := range metering.Sources() {
		wantSources = append(wantSources, string(source))
	}
	assertUsageTaxonomyConstraintValues(ctx, t, database,
		"usage_ledger_entries_token_type_valid", wantTokenTypes)
	assertUsageTaxonomyConstraintValues(ctx, t, database,
		"usage_ledger_entries_source_valid", wantSources)

	invalidTokenTypes := []string{"", "Input", "input ", "total", "audio", "image", "vendor_meter"}
	for index, tokenType := range invalidTokenTypes {
		_, insertErr := database.ExecContext(ctx, insertUsageLedgerSQL,
			usageTaxonomyEventID(100+index), modelListTenantOneID, usageTaxonomyRequestID, nil,
			tokenType, int64(1), string(metering.SourceProvider), now, "integration:usage-taxonomy")
		expectConstraint(t, insertErr, "usage_ledger_entries_token_type_valid")
	}
	invalidSources := []string{"", "Provider", "provider ", "vendor", "inferred", "billing"}
	for index, source := range invalidSources {
		_, insertErr := database.ExecContext(ctx, insertUsageLedgerSQL,
			usageTaxonomyEventID(200+index), modelListTenantOneID, usageTaxonomyRequestID, nil,
			string(metering.TokenTypeInput), int64(1), source, now, "integration:usage-taxonomy")
		expectConstraint(t, insertErr, "usage_ledger_entries_source_valid")
	}
}

func usageTaxonomyEventID(sequence int) string {
	return fmt.Sprintf("7a000000-0000-4000-8000-%012d", sequence)
}

func assertUsageTaxonomyConstraintValues(
	ctx context.Context,
	t *testing.T,
	database *sql.DB,
	constraint string,
	want []string,
) {
	t.Helper()
	var definition string
	err := database.QueryRowContext(ctx, `
		SELECT pg_get_constraintdef(oid)
		FROM pg_constraint
		WHERE conrelid = 'app.usage_ledger_entries'::regclass AND conname = $1`,
		constraint,
	).Scan(&definition)
	if err != nil {
		t.Fatalf("query constraint %s: %v", constraint, err)
	}
	matches := usageTaxonomyLiteralPattern.FindAllStringSubmatch(definition, -1)
	got := make([]string, 0, len(matches))
	for _, match := range matches {
		got = append(got, match[1])
	}
	wantCopy := append([]string(nil), want...)
	sort.Strings(got)
	sort.Strings(wantCopy)
	if !reflect.DeepEqual(got, wantCopy) {
		t.Fatalf("constraint %s values = %v, want %v; definition = %s",
			constraint, got, wantCopy, definition)
	}
}

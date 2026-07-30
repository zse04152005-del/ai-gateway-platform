package execution

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
)

func TestMarshalUsageSummaryPreservesPresenceAndDropsRawEvidence(t *testing.T) {
	usage := estimatedUsage()
	evidence, err := adapter.NewUsageEvidence([]byte(`{"private_provider_field":"must-not-persist"}`))
	if err != nil {
		t.Fatalf("adapter.NewUsageEvidence() error = %v", err)
	}
	usage.RawEvidence = evidence
	encoded, err := marshalUsageSummary(&usage)
	if err != nil {
		t.Fatalf("marshalUsageSummary() error = %v", err)
	}
	var summary map[string]any
	if err := json.Unmarshal(encoded, &summary); err != nil {
		t.Fatalf("decode usage summary: %v", err)
	}
	if value, exists := summary["input_tokens"]; !exists || value != float64(0) {
		t.Fatalf("input token presence/value = %#v/%v", value, exists)
	}
	if _, exists := summary["cache_read_tokens"]; exists {
		t.Fatalf("missing cache count was serialized: %s", encoded)
	}
	for _, forbidden := range []string{"raw", "evidence", "sha256", "private_provider_field", "must-not-persist"} {
		if strings.Contains(strings.ToLower(string(encoded)), forbidden) {
			t.Fatalf("usage summary leaked %q: %s", forbidden, encoded)
		}
	}
	if summary["source"] != string(adapter.UsageSourceEstimated) || summary["complete"] != false {
		t.Fatalf("usage metadata = %#v", summary)
	}
	if encoded, err := marshalUsageSummary(nil); err != nil || encoded != nil || nullableJSON(nil) != nil {
		t.Fatalf("nil usage = %q/%v; nullable = %#v", encoded, err, nullableJSON(nil))
	}
	if got := nullableJSON([]byte(`{"complete":false}`)); got != `{"complete":false}` {
		t.Fatalf("nullableJSON() = %#v", got)
	}
}

func TestRecorderConstructorAndPrivateErrors(t *testing.T) {
	now := time.Date(2026, 7, 30, 1, 2, 3, 0, time.UTC)
	if _, err := NewPostgresRecorder(nil, func() time.Time { return now }, bytes.NewReader(make([]byte, 16))); err == nil {
		t.Fatal("NewPostgresRecorder(nil database) error = nil")
	}
	database := &sql.DB{}
	if _, err := NewPostgresRecorder(database, nil, bytes.NewReader(make([]byte, 16))); err == nil {
		t.Fatal("NewPostgresRecorder(nil clock) error = nil")
	}
	if _, err := NewPostgresRecorder(database, func() time.Time { return time.Time{} }, bytes.NewReader(make([]byte, 16))); err == nil {
		t.Fatal("NewPostgresRecorder(zero clock) error = nil")
	}
	if _, err := NewPostgresRecorder(database, func() time.Time { return now }, nil); err == nil {
		t.Fatal("NewPostgresRecorder(nil random) error = nil")
	}
	if _, err := NewPostgresRecorder(database, func() time.Time { return now }, bytes.NewReader(make([]byte, 16))); err != nil {
		t.Fatalf("NewPostgresRecorder(valid) error = %v", err)
	}

	private := errors.New("postgres://private-user:private-password@private-host/internal")
	failure := newRecordError(ErrUnavailable, private)
	if failure.Error() != ErrUnavailable.Error() || strings.Contains(failure.Error(), "private") ||
		!errors.Is(failure, ErrUnavailable) || !errors.Is(failure, private) {
		t.Fatalf("record error safety/unwrap = %q/%v", failure, failure)
	}
	var nilFailure *recordError
	if nilFailure.Error() != "execution record failed" || nilFailure.Unwrap() != nil {
		t.Fatalf("nil recordError methods = %q/%#v", nilFailure.Error(), nilFailure.Unwrap())
	}
}

func TestDatabaseErrorsMapToStableKindsWithoutLeaking(t *testing.T) {
	conflicts := []error{
		sql.ErrNoRows,
		&pq.Error{Code: "23503", Message: "private foreign key detail"},
		&pq.Error{Code: "23505", Message: "private duplicate detail"},
		&pq.Error{Code: "23514", Message: "private constraint detail"},
	}
	for _, source := range conflicts {
		mapped := mapDatabaseError(source)
		if !errors.Is(mapped, ErrConflict) || strings.Contains(mapped.Error(), "private") {
			t.Errorf("mapDatabaseError(%T) = %q", source, mapped)
		}
	}
	private := errors.New("dial private.database.internal failed")
	mapped := mapDatabaseError(private)
	if !errors.Is(mapped, ErrUnavailable) || !errors.Is(mapped, private) || strings.Contains(mapped.Error(), "private") {
		t.Fatalf("unavailable mapping = %q", mapped)
	}
}

func TestAttemptCompletionStateProtectsClientVisibleOutput(t *testing.T) {
	observedAt := time.Date(2026, 7, 30, 4, 5, 6, 0, time.UTC)
	usage := estimatedUsage()
	connecting := RouteAttempt{
		ID: "60000000-0000-4000-8000-000000000001", RequestID: testRequestID,
		AttemptNo: 1, DeploymentID: testDeploymentID, Status: AttemptConnecting, Version: 2,
	}
	streaming := connecting
	streaming.Status = AttemptStreaming
	streaming.Version = 4
	streaming.HeadersReceivedAt = &observedAt
	streaming.FirstByteAt = &observedAt
	streaming.ProviderRequestID = "provider/stream-1"
	partial := AttemptOutcome{
		AttemptStatus: AttemptPartialFailed, RequestStatus: RequestPartialFailed, HeadersReceived: true,
		EndReason: "stream_interrupted", ErrorCategory: "transport", ErrorCode: "STREAM_INTERRUPTED",
		Usage: &usage,
	}
	if err := validateAttemptCompletion(streaming, partial); err != nil {
		t.Fatalf("valid streaming partial completion error = %v", err)
	}
	if err := validateAttemptCompletion(connecting, partial); !errors.Is(err, ErrInvalid) {
		t.Fatalf("connecting partial completion error = %v, want ErrInvalid", err)
	}
	for _, status := range []AttemptStatus{AttemptRetryableFailed, AttemptFailed} {
		outcome := AttemptOutcome{
			AttemptStatus: status, RequestStatus: RequestFailed, HeadersReceived: true,
			EndReason: "stream_interrupted", ErrorCategory: "transport", ErrorCode: "STREAM_INTERRUPTED",
		}
		if err := validateAttemptCompletion(streaming, outcome); !errors.Is(err, ErrInvalid) {
			t.Errorf("streaming completion as %s error = %v, want ErrInvalid", status, err)
		}
	}
	withoutFirstByte := streaming
	withoutFirstByte.FirstByteAt = nil
	if err := validateAttemptCompletion(withoutFirstByte, partial); !errors.Is(err, ErrInvalid) {
		t.Fatalf("streaming without first byte error = %v, want ErrInvalid", err)
	}
	mismatchedProvider := partial
	mismatchedProvider.ProviderRequestID = "provider/stream-2"
	if err := validateAttemptCompletion(streaming, mismatchedProvider); !errors.Is(err, ErrInvalid) {
		t.Fatalf("provider identity replacement error = %v, want ErrInvalid", err)
	}
}

package execution

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
)

const (
	testRequestID    = "request-execution-0001"
	testTenantID     = "10000000-0000-4000-8000-000000000001"
	testProjectID    = "20000000-0000-4000-8000-000000000001"
	testVirtualKeyID = "30000000-0000-4000-8000-000000000001"
	testDeploymentID = "60000000-0000-4000-8000-000000000001"
	testTraceID      = "11111111111111111111111111111111"
	testSpanID       = "2222222222222222"
)

func TestStartRequestValidation(t *testing.T) {
	valid := validStartRequest()
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid StartRequest.Validate() error = %v", err)
	}
	invalid := []StartRequest{
		{},
		withStart(valid, func(value *StartRequest) { value.ID = "short" }),
		withStart(valid, func(value *StartRequest) { value.TenantID = "not-a-uuid" }),
		withStart(valid, func(value *StartRequest) { value.ProjectID = "not-a-uuid" }),
		withStart(valid, func(value *StartRequest) { value.VirtualKeyID = "A0000000-0000-4000-8000-000000000001" }),
		withStart(valid, func(value *StartRequest) { value.LogicalModel = "model with space" }),
		withStart(valid, func(value *StartRequest) { value.TraceID = strings.Repeat("0", 31) }),
		withStart(valid, func(value *StartRequest) { value.SpanID = strings.Repeat("g", 16) }),
	}
	for index, value := range invalid {
		if !errors.Is(value.Validate(), ErrInvalid) {
			t.Errorf("invalid[%d].Validate() = %v, want ErrInvalid", index, value.Validate())
		}
	}
}

func TestAttemptOutcomeValidationMatrix(t *testing.T) {
	usage := estimatedUsage()
	valid := []AttemptOutcome{
		{AttemptStatus: AttemptSucceeded, RequestStatus: RequestSucceeded, HeadersReceived: true, EndReason: "completed", ProviderRequestID: "req/provider-1", Usage: &usage},
		{AttemptStatus: AttemptRetryableFailed, RequestStatus: RequestFailed, EndReason: "provider_capacity", ErrorCategory: string(adapter.ErrorCapacity), ErrorCode: "PROVIDER_CAPACITY"},
		{AttemptStatus: AttemptFailed, RequestStatus: RequestFailed, HeadersReceived: true, EndReason: "provider_protocol", ProviderRequestID: "provider:request_1", ErrorCategory: "protocol", ErrorCode: "PROVIDER_PROTOCOL"},
		{AttemptStatus: AttemptPartialFailed, RequestStatus: RequestPartialFailed, HeadersReceived: true, EndReason: "stream_interrupted", ErrorCategory: "transport", ErrorCode: "STREAM_INTERRUPTED", Usage: &usage},
		{AttemptStatus: AttemptCancelled, RequestStatus: RequestCancelled, EndReason: "client_cancelled", ErrorCategory: string(adapter.ErrorCancelled), ErrorCode: "CLIENT_CANCELLED"},
	}
	for index, outcome := range valid {
		if err := outcome.Validate(); err != nil {
			t.Errorf("valid[%d].Validate() error = %v", index, err)
		}
	}

	invalid := []AttemptOutcome{
		{},
		{AttemptStatus: AttemptSucceeded, RequestStatus: RequestSucceeded, EndReason: "completed"},
		{AttemptStatus: AttemptSucceeded, RequestStatus: RequestFailed, HeadersReceived: true, EndReason: "completed"},
		{AttemptStatus: AttemptFailed, RequestStatus: RequestFailed, EndReason: "bad reason", ErrorCategory: "transport", ErrorCode: "TRANSPORT_FAILED"},
		{AttemptStatus: AttemptFailed, RequestStatus: RequestFailed, EndReason: "failed", ErrorCategory: "transport", ErrorCode: "lowercase"},
		{AttemptStatus: AttemptFailed, RequestStatus: RequestFailed, EndReason: "failed", ProviderRequestID: "unsafe provider id", ErrorCategory: "transport", ErrorCode: "TRANSPORT_FAILED"},
		{AttemptStatus: AttemptFailed, RequestStatus: RequestFailed, EndReason: "failed", ProviderRequestID: "provider-request-without-headers", ErrorCategory: "transport", ErrorCode: "TRANSPORT_FAILED"},
		{AttemptStatus: AttemptRetryableFailed, RequestStatus: RequestFailed, EndReason: "failed", ErrorCategory: "transport", ErrorCode: "TRANSPORT_FAILED", Usage: &usage},
		{AttemptStatus: AttemptPartialFailed, RequestStatus: RequestPartialFailed, EndReason: "stream_interrupted", ErrorCategory: "transport", ErrorCode: "STREAM_INTERRUPTED", Usage: &usage},
		{AttemptStatus: AttemptCancelled, RequestStatus: RequestCancelled, EndReason: "client_cancelled", ErrorCategory: "transport", ErrorCode: "CLIENT_CANCELLED"},
	}
	for index, outcome := range invalid {
		if !errors.Is(outcome.Validate(), ErrInvalid) {
			t.Errorf("invalid[%d].Validate() = %v, want ErrInvalid", index, outcome.Validate())
		}
	}
}

func TestUUIDGenerationUsesRFC4122VersionAndVariant(t *testing.T) {
	identifier, err := newUUID(bytes.NewReader(make([]byte, 16)))
	if err != nil {
		t.Fatalf("newUUID() error = %v", err)
	}
	if identifier != "00000000-0000-4000-8000-000000000000" || !uuidPattern.MatchString(identifier) {
		t.Fatalf("newUUID() = %q", identifier)
	}
	if _, err := newUUID(bytes.NewReader(make([]byte, 15))); err == nil {
		t.Fatal("newUUID(short entropy) error = nil")
	}
}

func validStartRequest() StartRequest {
	return StartRequest{
		ID: testRequestID, TenantID: testTenantID, ProjectID: testProjectID,
		VirtualKeyID: testVirtualKeyID, LogicalModel: "model-a", TraceID: testTraceID, SpanID: testSpanID,
	}
}

func withStart(value StartRequest, mutate func(*StartRequest)) StartRequest {
	mutate(&value)
	return value
}

func estimatedUsage() adapter.NormalizedUsage {
	return adapter.NormalizedUsage{
		InputTokens: adapter.Tokens(0), OutputTokens: adapter.Tokens(7),
		Source: adapter.UsageSourceEstimated, Complete: false,
	}
}

package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/apierror"
	"github.com/zse04152005-del/ai-gateway-platform/internal/execution"
	"github.com/zse04152005-del/ai-gateway-platform/internal/metering"
	"github.com/zse04152005-del/ai-gateway-platform/internal/meteringcost"
)

const (
	costTenantID  = "83000000-0000-4000-8000-000000000001"
	costProjectID = "83000000-0000-4000-8000-000000000002"
	costRequestID = "request-cost-http-0001"
	costPath      = "/admin/v1/tenants/" + costTenantID + "/projects/" + costProjectID +
		"/requests/" + costRequestID + "/cost"
)

func TestRequestCostRouteReturnsScopedImmutableDetails(t *testing.T) {
	want := requestCostFixture()
	reader := stubRequestCostReader(func(
		_ context.Context,
		scope meteringcost.Scope,
		requestID string,
	) (meteringcost.RequestCost, error) {
		if scope.TenantID != costTenantID || scope.ProjectID != costProjectID || requestID != costRequestID {
			t.Fatalf("Aggregate() = %+v/%q", scope, requestID)
		}
		return want, nil
	})
	handler := NewHandlerWithServices("test", nil, reader)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, costPath, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", response.Code, response.Body)
	}
	if response.Header().Get("Cache-Control") != "no-store" ||
		response.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("safe headers = %v", response.Header())
	}
	var got meteringcost.RequestCost
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.RequestID != want.RequestID || got.AttemptCount != 1 || len(got.Attempts) != 1 ||
		len(got.Attempts[0].Entries) != 1 ||
		got.Attempts[0].Entries[0].TokenType != metering.TokenTypeInput ||
		got.Attempts[0].Entries[0].PriceVersionID == "" ||
		got.Attempts[0].Entries[0].AmountMicros != 25 {
		t.Fatalf("response details = %+v", got)
	}
	for _, forbidden := range []string{"prompt", "response_body", "credential", "raw_evidence"} {
		if strings.Contains(response.Body.String(), forbidden) {
			t.Fatalf("response contains forbidden field %q: %s", forbidden, response.Body)
		}
	}
}

func TestRequestCostRouteMapsStableSafeErrorsAndMethods(t *testing.T) {
	tests := []struct {
		name       string
		failure    error
		wantStatus int
		wantCode   string
		wantRetry  bool
	}{
		{name: "invalid", failure: meteringcost.ErrInvalid, wantStatus: 400, wantCode: "INVALID_COST_QUERY"},
		{name: "not found", failure: meteringcost.ErrNotFound, wantStatus: 404, wantCode: "REQUEST_COST_NOT_FOUND"},
		{name: "active", failure: meteringcost.ErrNotTerminal, wantStatus: 409, wantCode: "REQUEST_NOT_TERMINAL"},
		{name: "pending", failure: meteringcost.ErrPending, wantStatus: 409, wantCode: "REQUEST_COST_PENDING", wantRetry: true},
		{
			name: "unavailable", failure: errors.Join(meteringcost.ErrUnavailable, errors.New("postgres://private-host/secret")),
			wantStatus: 503, wantCode: "REQUEST_COST_UNAVAILABLE",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := NewHandlerWithServices("test", nil, stubRequestCostReader(func(
				context.Context,
				meteringcost.Scope,
				string,
			) (meteringcost.RequestCost, error) {
				return meteringcost.RequestCost{}, test.failure
			}))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, costPath, nil))
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", response.Code, test.wantStatus, response.Body)
			}
			var envelope apierror.Envelope
			if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
				t.Fatalf("decode error: %v", err)
			}
			if envelope.Error.Code != test.wantCode || strings.Contains(response.Body.String(), "private-host") {
				t.Fatalf("error response = %+v/%s", envelope, response.Body)
			}
			if (response.Header().Get("Retry-After") != "") != test.wantRetry {
				t.Fatalf("Retry-After = %q", response.Header().Get("Retry-After"))
			}
		})
	}

	methodResponse := httptest.NewRecorder()
	NewHandlerWithServices("test", nil, stubRequestCostReader(nil)).ServeHTTP(
		methodResponse, httptest.NewRequest(http.MethodPost, costPath, nil),
	)
	if methodResponse.Code != http.StatusMethodNotAllowed ||
		methodResponse.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("wrong method response = %d/%v", methodResponse.Code, methodResponse.Header())
	}

	unknownResponse := httptest.NewRecorder()
	NewHandlerWithServices("test", nil, stubRequestCostReader(nil)).ServeHTTP(
		unknownResponse, httptest.NewRequest(http.MethodGet, costPath+"/extra", nil),
	)
	if unknownResponse.Code != http.StatusNotFound {
		t.Fatalf("unknown route status = %d", unknownResponse.Code)
	}
}

type stubRequestCostReader func(
	context.Context,
	meteringcost.Scope,
	string,
) (meteringcost.RequestCost, error)

func (reader stubRequestCostReader) Aggregate(
	ctx context.Context,
	scope meteringcost.Scope,
	requestID string,
) (meteringcost.RequestCost, error) {
	if reader == nil {
		return meteringcost.RequestCost{}, errors.New("unexpected Aggregate call")
	}
	return reader(ctx, scope, requestID)
}

func requestCostFixture() meteringcost.RequestCost {
	observedAt := time.Date(2026, time.August, 6, 2, 0, 0, 0, time.UTC)
	entry := meteringcost.LedgerEntry{
		EventID:   "83000000-0000-4000-8000-000000000003",
		AttemptID: "83000000-0000-4000-8000-000000000004",
		TokenType: metering.TokenTypeInput, Quantity: 10, Source: metering.SourceProvider,
		ObservedAt: observedAt, CreatedAt: observedAt.Add(time.Second),
		PriceVersionID: "83000000-0000-4000-8000-000000000005", Currency: "USD",
		BillingUnit: metering.BillingUnitToken, UnitQuantity: 1_000_000,
		UnitPriceMicros: 2_500_000, AmountMicros: 25,
	}
	return meteringcost.RequestCost{
		TenantID: costTenantID, ProjectID: costProjectID, RequestID: costRequestID,
		Status: execution.RequestSucceeded, AttemptCount: 1, LedgerEntryCount: 1,
		Attempts: []meteringcost.AttemptCost{{
			AttemptID: entry.AttemptID, AttemptNo: 1,
			DeploymentID: "83000000-0000-4000-8000-000000000006",
			Status:       execution.AttemptSucceeded,
			CostBucket: meteringcost.CostBucket{
				LedgerEntryCount: 1, Entries: []meteringcost.LedgerEntry{entry},
				Totals: []meteringcost.CurrencyTotal{{Currency: "USD", AmountMicros: 25}},
			},
		}},
		RequestLevel: meteringcost.CostBucket{
			Entries: make([]meteringcost.LedgerEntry, 0), Totals: make([]meteringcost.CurrencyTotal, 0),
		},
		Totals: []meteringcost.CurrencyTotal{{Currency: "USD", AmountMicros: 25}},
	}
}

package meteringcost

import (
	"errors"
	"reflect"
	"testing"

	"github.com/zse04152005-del/ai-gateway-platform/internal/execution"
	"github.com/zse04152005-del/ai-gateway-platform/internal/metering"
)

const (
	testTenantID  = "71000000-0000-4000-8000-000000000001"
	testProjectID = "71000000-0000-4000-8000-000000000002"
	testRequestID = "request-cost-aggregate-1"
)

func TestBuildRequestCostIncludesAllAttemptsAndSeparatesCurrencies(t *testing.T) {
	scope := Scope{TenantID: testTenantID, ProjectID: testProjectID}
	attempts := []attemptFact{
		{
			id: "71000000-0000-4000-8000-000000000012", number: 2,
			deploymentID: "71000000-0000-4000-8000-000000000022", status: execution.AttemptSucceeded,
		},
		{
			id: "71000000-0000-4000-8000-000000000011", number: 1,
			deploymentID: "71000000-0000-4000-8000-000000000021", status: execution.AttemptRetryableFailed,
		},
		{
			id: "71000000-0000-4000-8000-000000000013", number: 3,
			deploymentID: "71000000-0000-4000-8000-000000000023", status: execution.AttemptCancelled,
		},
	}
	entries := []ledgerFact{
		{
			eventID:   "71000000-0000-4000-8000-000000000031",
			attemptID: attempts[1].id, currency: "USD", amountMicros: 3,
		},
		{
			eventID:   "71000000-0000-4000-8000-000000000032",
			attemptID: attempts[1].id, currency: "USD", amountMicros: 2,
		},
		{
			eventID:   "71000000-0000-4000-8000-000000000033",
			attemptID: attempts[0].id, currency: "EUR", amountMicros: 4,
		},
		{
			eventID:  "71000000-0000-4000-8000-000000000034",
			currency: "USD", amountMicros: 1,
		},
	}

	result, err := buildRequestCost(
		scope, testRequestID, execution.RequestSucceeded, len(attempts), attempts, entries,
	)
	if err != nil {
		t.Fatalf("buildRequestCost() error = %v", err)
	}
	if result.AttemptCount != 3 || result.LedgerEntryCount != 4 || len(result.Attempts) != 3 ||
		result.Attempts[0].Status != execution.AttemptRetryableFailed ||
		result.Attempts[1].Status != execution.AttemptSucceeded ||
		result.Attempts[2].Status != execution.AttemptCancelled ||
		result.Attempts[2].LedgerEntryCount != 0 || len(result.Attempts[2].Totals) != 0 {
		t.Fatalf("request/attempt aggregate = %+v", result)
	}
	if !reflect.DeepEqual(result.Attempts[0].Totals, []CurrencyTotal{{Currency: "USD", AmountMicros: 5}}) ||
		!reflect.DeepEqual(result.Attempts[1].Totals, []CurrencyTotal{{Currency: "EUR", AmountMicros: 4}}) ||
		!reflect.DeepEqual(result.RequestLevel, CostBucket{
			LedgerEntryCount: 1,
			Totals:           []CurrencyTotal{{Currency: "USD", AmountMicros: 1}},
		}) || !reflect.DeepEqual(result.Totals, []CurrencyTotal{
		{Currency: "EUR", AmountMicros: 4},
		{Currency: "USD", AmountMicros: 6},
	}) {
		t.Fatalf("currency totals = attempts:%+v request:%+v totals:%+v",
			result.Attempts, result.RequestLevel, result.Totals)
	}
}

func TestBuildRequestCostRejectsUnsafeStoredFacts(t *testing.T) {
	scope := Scope{TenantID: testTenantID, ProjectID: testProjectID}
	attempt := attemptFact{
		id: "71000000-0000-4000-8000-000000000011", number: 1,
		deploymentID: "71000000-0000-4000-8000-000000000021", status: execution.AttemptFailed,
	}
	validEntry := ledgerFact{
		eventID:   "71000000-0000-4000-8000-000000000031",
		attemptID: attempt.id, currency: "USD", amountMicros: 1,
	}
	tests := map[string]struct {
		status       execution.RequestStatus
		attemptCount int
		attempts     []attemptFact
		entries      []ledgerFact
	}{
		"active request": {
			status: execution.RequestRunning, attemptCount: 1,
			attempts: []attemptFact{attempt}, entries: []ledgerFact{validEntry},
		},
		"attempt count drift": {
			status: execution.RequestFailed, attemptCount: 2,
			attempts: []attemptFact{attempt}, entries: []ledgerFact{validEntry},
		},
		"active attempt": {
			status: execution.RequestFailed, attemptCount: 1,
			attempts: []attemptFact{{
				id: attempt.id, number: 1, deploymentID: attempt.deploymentID,
				status: execution.AttemptStreaming,
			}},
			entries: []ledgerFact{validEntry},
		},
		"unknown attempt": {
			status: execution.RequestFailed, attemptCount: 1,
			attempts: []attemptFact{attempt}, entries: []ledgerFact{{
				eventID: validEntry.eventID, attemptID: "71000000-0000-4000-8000-000000000099",
				currency: "USD", amountMicros: 1,
			}},
		},
		"duplicate event": {
			status: execution.RequestFailed, attemptCount: 1,
			attempts: []attemptFact{attempt}, entries: []ledgerFact{validEntry, validEntry},
		},
		"currency sum overflow": {
			status: execution.RequestFailed, attemptCount: 1,
			attempts: []attemptFact{attempt}, entries: []ledgerFact{
				{
					eventID: validEntry.eventID, attemptID: attempt.id,
					currency: "USD", amountMicros: metering.MaximumExactInteger,
				},
				{
					eventID:   "71000000-0000-4000-8000-000000000032",
					attemptID: attempt.id, currency: "USD", amountMicros: 1,
				},
			},
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := buildRequestCost(
				scope, testRequestID, test.status, test.attemptCount, test.attempts, test.entries,
			)
			if !errors.Is(err, ErrUnavailable) {
				t.Fatalf("buildRequestCost() error = %v, want ErrUnavailable", err)
			}
		})
	}
}

func TestScopeValidationAndTerminalStatusMatrix(t *testing.T) {
	if (Scope{TenantID: testTenantID, ProjectID: testProjectID}).Validate() != nil {
		t.Fatal("valid Scope was rejected")
	}
	for _, invalid := range []Scope{
		{},
		{TenantID: testTenantID, ProjectID: "bad"},
		{TenantID: "bad", ProjectID: testProjectID},
	} {
		if !errors.Is(invalid.Validate(), ErrInvalid) {
			t.Errorf("Scope.Validate(%+v) error = %v", invalid, invalid.Validate())
		}
	}
	for _, status := range []execution.RequestStatus{
		execution.RequestSucceeded, execution.RequestPartialFailed,
		execution.RequestFailed, execution.RequestCancelled,
	} {
		if !terminalRequestStatus(status) {
			t.Errorf("terminalRequestStatus(%q) = false", status)
		}
	}
	for _, status := range []execution.RequestStatus{
		execution.RequestAuthorized, execution.RequestRouting, execution.RequestRunning, "unknown",
	} {
		if terminalRequestStatus(status) {
			t.Errorf("terminalRequestStatus(%q) = true", status)
		}
	}
}

package meteringcost

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/execution"
	"github.com/zse04152005-del/ai-gateway-platform/internal/metering"
	"github.com/zse04152005-del/ai-gateway-platform/internal/meteringadjustment"
)

const (
	testTenantID  = "71000000-0000-4000-8000-000000000001"
	testProjectID = "71000000-0000-4000-8000-000000000002"
	testRequestID = "request-cost-aggregate-1"
	testPriceID   = "71000000-0000-4000-8000-000000000041"
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
		providerLedgerFact("71000000-0000-4000-8000-000000000031", attempts[1].id, "USD", 3),
		providerLedgerFact("71000000-0000-4000-8000-000000000032", attempts[1].id, "USD", 2),
		providerLedgerFact("71000000-0000-4000-8000-000000000033", attempts[0].id, "EUR", 4),
		providerLedgerFact("71000000-0000-4000-8000-000000000034", "", "USD", 1),
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
		result.Attempts[2].LedgerEntryCount != 0 || len(result.Attempts[2].Totals) != 0 ||
		len(result.Attempts[2].Entries) != 0 {
		t.Fatalf("request/attempt aggregate = %+v", result)
	}
	if !reflect.DeepEqual(result.Attempts[0].Totals, []CurrencyTotal{{Currency: "USD", AmountMicros: 5}}) ||
		!reflect.DeepEqual(result.Attempts[1].Totals, []CurrencyTotal{{Currency: "EUR", AmountMicros: 4}}) ||
		!reflect.DeepEqual(result.RequestLevel.Totals, []CurrencyTotal{{Currency: "USD", AmountMicros: 1}}) ||
		!reflect.DeepEqual(result.Totals, []CurrencyTotal{
			{Currency: "EUR", AmountMicros: 4},
			{Currency: "USD", AmountMicros: 6},
		}) {
		t.Fatalf("currency totals = attempts:%+v request:%+v totals:%+v",
			result.Attempts, result.RequestLevel, result.Totals)
	}
	if len(result.Attempts[0].Entries) != 2 ||
		result.Attempts[0].Entries[0].PriceVersionID != testPriceID ||
		result.Attempts[0].Entries[0].TokenType != metering.TokenTypeInput ||
		len(result.RequestLevel.Entries) != 1 || result.RequestLevel.LedgerEntryCount != 1 {
		t.Fatalf("ledger details = attempts:%+v request:%+v", result.Attempts, result.RequestLevel)
	}
}

func TestBuildRequestCostRejectsUnsafeStoredFacts(t *testing.T) {
	scope := Scope{TenantID: testTenantID, ProjectID: testProjectID}
	attempt := attemptFact{
		id: "71000000-0000-4000-8000-000000000011", number: 1,
		deploymentID: "71000000-0000-4000-8000-000000000021", status: execution.AttemptFailed,
	}
	validEntry := providerLedgerFact(
		"71000000-0000-4000-8000-000000000031", attempt.id, "USD", 1,
	)
	unknownAttempt := validEntry
	unknownAttempt.attemptID = "71000000-0000-4000-8000-000000000099"
	invalidRate := validEntry
	invalidRate.billingUnit = metering.BillingUnitImage
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
			attempts: []attemptFact{afterStatus(attempt, execution.AttemptStreaming)},
			entries:  []ledgerFact{validEntry},
		},
		"unknown attempt": {
			status: execution.RequestFailed, attemptCount: 1,
			attempts: []attemptFact{attempt}, entries: []ledgerFact{unknownAttempt},
		},
		"duplicate event": {
			status: execution.RequestFailed, attemptCount: 1,
			attempts: []attemptFact{attempt}, entries: []ledgerFact{validEntry, validEntry},
		},
		"invalid locked rate": {
			status: execution.RequestFailed, attemptCount: 1,
			attempts: []attemptFact{attempt}, entries: []ledgerFact{invalidRate},
		},
		"currency sum overflow": {
			status: execution.RequestFailed, attemptCount: 1,
			attempts: []attemptFact{attempt}, entries: []ledgerFact{
				providerLedgerFact(validEntry.eventID, attempt.id, "USD", metering.MaximumExactInteger),
				providerLedgerFact("71000000-0000-4000-8000-000000000032", attempt.id, "USD", 1),
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

func TestBuildRequestCostIncludesSignedAdjustmentsButRejectsNegativeEffectiveCost(t *testing.T) {
	scope := Scope{TenantID: testTenantID, ProjectID: testProjectID}
	attempt := attemptFact{
		id: "71000000-0000-4000-8000-000000000011", number: 1,
		deploymentID: "71000000-0000-4000-8000-000000000021", status: execution.AttemptSucceeded,
	}
	entries := []ledgerFact{
		providerLedgerFact("71000000-0000-4000-8000-000000000031", attempt.id, "USD", 10),
		adjustmentLedgerFact(
			"71000000-0000-4000-8000-000000000032",
			"71000000-0000-4000-8000-000000000031", attempt.id, -4,
		),
	}
	result, err := buildRequestCost(
		scope, testRequestID, execution.RequestSucceeded, 1, []attemptFact{attempt}, entries,
	)
	if err != nil || len(result.Totals) != 1 || result.Totals[0].AmountMicros != 6 ||
		len(result.Attempts[0].Totals) != 1 || result.Attempts[0].Totals[0].AmountMicros != 6 ||
		result.Attempts[0].Entries[1].Adjustment == nil ||
		result.Attempts[0].Entries[1].Adjustment.Actor != "admin:user-1" {
		t.Fatalf("signed adjustment aggregate = %+v, error = %v", result, err)
	}
	entries[1].amountMicros = -11
	if _, err := buildRequestCost(
		scope, testRequestID, execution.RequestSucceeded, 1, []attemptFact{attempt}, entries,
	); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("negative effective total error = %v, want ErrUnavailable", err)
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

func providerLedgerFact(eventID, attemptID, currency string, amountMicros int64) ledgerFact {
	observedAt := time.Date(2026, time.August, 6, 1, 0, 0, 0, time.UTC)
	return ledgerFact{
		eventID: eventID, attemptID: attemptID, tokenType: metering.TokenTypeInput,
		quantity: 1, source: metering.SourceProvider, observedAt: observedAt,
		createdAt: observedAt.Add(time.Second), priceVersionID: testPriceID,
		currency: currency, billingUnit: metering.BillingUnitToken,
		unitQuantity: 1, unitPriceMicros: 1, amountMicros: amountMicros,
	}
}

func adjustmentLedgerFact(eventID, targetEventID, attemptID string, amountMicros int64) ledgerFact {
	fact := providerLedgerFact(eventID, attemptID, "USD", amountMicros)
	fact.quantity = -1
	fact.source = metering.SourceAdjustment
	fact.adjustment = &LedgerAdjustment{
		TargetEventID: targetEventID, Origin: meteringadjustment.OriginManual,
		Reason: "invoice_correction", Reference: "ticket:BILL-1", Actor: "admin:user-1",
		CorrectedQuantity: 0, CorrectedAmountMicros: 6,
	}
	return fact
}

func afterStatus(fact attemptFact, status execution.AttemptStatus) attemptFact {
	fact.status = status
	return fact
}

// Package meteringcost rebuilds request cost from immutable priced ledger facts.
package meteringcost

import (
	"errors"
	"regexp"
	"sort"

	"github.com/zse04152005-del/ai-gateway-platform/internal/execution"
	"github.com/zse04152005-del/ai-gateway-platform/internal/metering"
)

var (
	// ErrInvalid means an aggregation scope or request identity is malformed.
	ErrInvalid = errors.New("metering cost aggregation input is invalid")
	// ErrNotFound means no request exists inside the trusted tenant/project scope.
	ErrNotFound = errors.New("metering cost request not found")
	// ErrNotTerminal means the request can still create additional Attempts or usage.
	ErrNotTerminal = errors.New("metering cost request is not terminal")
	// ErrPending means at least one durable Outbox fact has not reached the Ledger.
	ErrPending = errors.New("metering cost aggregation is pending")
	// ErrUnavailable means stored facts cannot be read or do not form a safe aggregate.
	ErrUnavailable = errors.New("metering cost aggregation is unavailable")

	requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{7,127}$`)
	uuidPattern      = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	currencyPattern  = regexp.MustCompile(`^[A-Z]{3}$`)
)

// Scope is the trusted tenant/project boundary for one request aggregation.
type Scope struct {
	TenantID  string
	ProjectID string
}

// Validate rejects a malformed or ambiguous query scope.
func (scope Scope) Validate() error {
	if !uuidPattern.MatchString(scope.TenantID) || !uuidPattern.MatchString(scope.ProjectID) {
		return ErrInvalid
	}
	return nil
}

// CurrencyTotal is one exact request or Attempt amount. Different currencies
// remain separate and must never be mechanically added together.
type CurrencyTotal struct {
	Currency     string `json:"currency"`
	AmountMicros int64  `json:"amount_micros"`
}

// CostBucket groups immutable Ledger entries without losing currency identity.
type CostBucket struct {
	LedgerEntryCount int             `json:"ledger_entry_count"`
	Totals           []CurrencyTotal `json:"totals"`
}

// AttemptCost is one physical provider call, including zero-cost Attempts.
type AttemptCost struct {
	AttemptID    string                  `json:"attempt_id"`
	AttemptNo    int                     `json:"attempt_no"`
	DeploymentID string                  `json:"deployment_id"`
	Status       execution.AttemptStatus `json:"status"`
	CostBucket
}

// RequestCost is a complete query-time projection over immutable Ledger facts.
// RequestLevel contains future cache-only or other non-Attempt facts.
type RequestCost struct {
	TenantID         string                  `json:"tenant_id"`
	ProjectID        string                  `json:"project_id"`
	RequestID        string                  `json:"request_id"`
	Status           execution.RequestStatus `json:"status"`
	AttemptCount     int                     `json:"attempt_count"`
	LedgerEntryCount int                     `json:"ledger_entry_count"`
	Attempts         []AttemptCost           `json:"attempts"`
	RequestLevel     CostBucket              `json:"request_level"`
	Totals           []CurrencyTotal         `json:"totals"`
}

type attemptFact struct {
	id           string
	number       int
	deploymentID string
	status       execution.AttemptStatus
}

type ledgerFact struct {
	eventID      string
	attemptID    string
	currency     string
	amountMicros int64
}

type currencyAccumulator map[string]int64

func buildRequestCost(
	scope Scope,
	requestID string,
	status execution.RequestStatus,
	declaredAttemptCount int,
	attemptFacts []attemptFact,
	ledgerFacts []ledgerFact,
) (RequestCost, error) {
	if scope.Validate() != nil || !requestIDPattern.MatchString(requestID) ||
		!terminalRequestStatus(status) || declaredAttemptCount < 0 ||
		declaredAttemptCount != len(attemptFacts) {
		return RequestCost{}, ErrUnavailable
	}
	sort.Slice(attemptFacts, func(left, right int) bool {
		return attemptFacts[left].number < attemptFacts[right].number
	})
	attempts := make([]AttemptCost, len(attemptFacts))
	attemptIndex := make(map[string]int, len(attemptFacts))
	attemptTotals := make([]currencyAccumulator, len(attemptFacts))
	for index, fact := range attemptFacts {
		if !uuidPattern.MatchString(fact.id) || fact.number != index+1 ||
			!uuidPattern.MatchString(fact.deploymentID) || !terminalAttemptStatus(fact.status) {
			return RequestCost{}, ErrUnavailable
		}
		if _, duplicate := attemptIndex[fact.id]; duplicate {
			return RequestCost{}, ErrUnavailable
		}
		attemptIndex[fact.id] = index
		attemptTotals[index] = make(currencyAccumulator)
		attempts[index] = AttemptCost{
			AttemptID: fact.id, AttemptNo: fact.number,
			DeploymentID: fact.deploymentID, Status: fact.status,
			CostBucket: CostBucket{Totals: make([]CurrencyTotal, 0)},
		}
	}
	requestLevelTotals := make(currencyAccumulator)
	requestTotals := make(currencyAccumulator)
	seenEvents := make(map[string]struct{}, len(ledgerFacts))
	requestLevelEntryCount := 0
	for _, fact := range ledgerFacts {
		if !uuidPattern.MatchString(fact.eventID) || !currencyPattern.MatchString(fact.currency) ||
			fact.amountMicros < 0 || fact.amountMicros > metering.MaximumExactInteger {
			return RequestCost{}, ErrUnavailable
		}
		if _, duplicate := seenEvents[fact.eventID]; duplicate {
			return RequestCost{}, ErrUnavailable
		}
		seenEvents[fact.eventID] = struct{}{}
		if err := requestTotals.add(fact.currency, fact.amountMicros); err != nil {
			return RequestCost{}, err
		}
		if fact.attemptID == "" {
			requestLevelEntryCount++
			if err := requestLevelTotals.add(fact.currency, fact.amountMicros); err != nil {
				return RequestCost{}, err
			}
			continue
		}
		index, exists := attemptIndex[fact.attemptID]
		if !exists {
			return RequestCost{}, ErrUnavailable
		}
		attempts[index].LedgerEntryCount++
		if err := attemptTotals[index].add(fact.currency, fact.amountMicros); err != nil {
			return RequestCost{}, err
		}
	}
	for index := range attempts {
		attempts[index].Totals = attemptTotals[index].ordered()
	}
	return RequestCost{
		TenantID: scope.TenantID, ProjectID: scope.ProjectID, RequestID: requestID,
		Status: status, AttemptCount: declaredAttemptCount, LedgerEntryCount: len(ledgerFacts),
		Attempts: attempts,
		RequestLevel: CostBucket{
			LedgerEntryCount: requestLevelEntryCount,
			Totals:           requestLevelTotals.ordered(),
		},
		Totals: requestTotals.ordered(),
	}, nil
}

func (totals currencyAccumulator) add(currency string, amount int64) error {
	current := totals[currency]
	if current > metering.MaximumExactInteger-amount {
		return ErrUnavailable
	}
	totals[currency] = current + amount
	return nil
}

func (totals currencyAccumulator) ordered() []CurrencyTotal {
	currencies := make([]string, 0, len(totals))
	for currency := range totals {
		currencies = append(currencies, currency)
	}
	sort.Strings(currencies)
	result := make([]CurrencyTotal, 0, len(currencies))
	for _, currency := range currencies {
		result = append(result, CurrencyTotal{Currency: currency, AmountMicros: totals[currency]})
	}
	return result
}

func terminalRequestStatus(status execution.RequestStatus) bool {
	switch status {
	case execution.RequestSucceeded, execution.RequestPartialFailed,
		execution.RequestFailed, execution.RequestCancelled:
		return true
	case execution.RequestAuthorized, execution.RequestRouting, execution.RequestRunning:
		return false
	default:
		return false
	}
}

func terminalAttemptStatus(status execution.AttemptStatus) bool {
	switch status {
	case execution.AttemptSucceeded, execution.AttemptRetryableFailed, execution.AttemptFailed,
		execution.AttemptPartialFailed, execution.AttemptCancelled:
		return true
	case execution.AttemptCreated, execution.AttemptConnecting,
		execution.AttemptHeadersReceived, execution.AttemptStreaming:
		return false
	default:
		return false
	}
}

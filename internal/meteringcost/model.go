// Package meteringcost rebuilds request cost from immutable priced ledger facts.
package meteringcost

import (
	"errors"
	"math/big"
	"regexp"
	"sort"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/execution"
	"github.com/zse04152005-del/ai-gateway-platform/internal/metering"
	"github.com/zse04152005-del/ai-gateway-platform/internal/meteringadjustment"
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
	auditPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@/-]{0,199}$`)
	reasonPattern    = regexp.MustCompile(`^[a-z][a-z0-9._:-]{0,127}$`)
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

// LedgerAdjustment explains one signed correction without exposing external evidence content.
type LedgerAdjustment struct {
	TargetEventID         string                    `json:"target_event_id"`
	Origin                meteringadjustment.Origin `json:"origin"`
	Reason                string                    `json:"reason"`
	Reference             string                    `json:"reference"`
	Actor                 string                    `json:"actor"`
	CorrectedQuantity     int64                     `json:"corrected_quantity"`
	CorrectedAmountMicros int64                     `json:"corrected_amount_micros"`
}

// LedgerEntry is one content-free immutable usage or correction fact with its locked rate.
type LedgerEntry struct {
	EventID         string               `json:"event_id"`
	AttemptID       string               `json:"attempt_id,omitempty"`
	TokenType       metering.TokenType   `json:"token_type"`
	Quantity        int64                `json:"quantity"`
	Source          metering.Source      `json:"source"`
	ObservedAt      time.Time            `json:"observed_at"`
	CreatedAt       time.Time            `json:"created_at"`
	PriceVersionID  string               `json:"price_version_id"`
	Currency        string               `json:"currency"`
	BillingUnit     metering.BillingUnit `json:"billing_unit"`
	UnitQuantity    int64                `json:"unit_quantity"`
	UnitPriceMicros int64                `json:"unit_price_micros"`
	AmountMicros    int64                `json:"amount_micros"`
	Adjustment      *LedgerAdjustment    `json:"adjustment,omitempty"`
}

// CostBucket groups immutable Ledger entries without losing currency identity.
type CostBucket struct {
	LedgerEntryCount int             `json:"ledger_entry_count"`
	Entries          []LedgerEntry   `json:"entries"`
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
	eventID         string
	attemptID       string
	tokenType       metering.TokenType
	quantity        int64
	source          metering.Source
	observedAt      time.Time
	createdAt       time.Time
	priceVersionID  string
	currency        string
	billingUnit     metering.BillingUnit
	unitQuantity    int64
	unitPriceMicros int64
	amountMicros    int64
	adjustment      *LedgerAdjustment
}

type currencyAccumulator map[string]*big.Int

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
			CostBucket: CostBucket{
				Entries: make([]LedgerEntry, 0), Totals: make([]CurrencyTotal, 0),
			},
		}
	}
	requestLevelTotals := make(currencyAccumulator)
	requestTotals := make(currencyAccumulator)
	requestLevelEntries := make([]LedgerEntry, 0)
	seenEvents := make(map[string]struct{}, len(ledgerFacts))
	requestLevelEntryCount := 0
	for _, fact := range ledgerFacts {
		entry, entryErr := fact.entry()
		if entryErr != nil {
			return RequestCost{}, ErrUnavailable
		}
		if _, duplicate := seenEvents[fact.eventID]; duplicate {
			return RequestCost{}, ErrUnavailable
		}
		seenEvents[fact.eventID] = struct{}{}
		requestTotals.add(fact.currency, fact.amountMicros)
		if fact.attemptID == "" {
			requestLevelEntryCount++
			requestLevelEntries = append(requestLevelEntries, entry)
			requestLevelTotals.add(fact.currency, fact.amountMicros)
			continue
		}
		index, exists := attemptIndex[fact.attemptID]
		if !exists {
			return RequestCost{}, ErrUnavailable
		}
		attempts[index].LedgerEntryCount++
		attempts[index].Entries = append(attempts[index].Entries, entry)
		attemptTotals[index].add(fact.currency, fact.amountMicros)
	}
	for index := range attempts {
		orderedTotals, orderedErr := attemptTotals[index].ordered()
		if orderedErr != nil {
			return RequestCost{}, orderedErr
		}
		attempts[index].Totals = orderedTotals
	}
	requestLevelOrdered, orderedErr := requestLevelTotals.ordered()
	if orderedErr != nil {
		return RequestCost{}, orderedErr
	}
	requestOrdered, orderedErr := requestTotals.ordered()
	if orderedErr != nil {
		return RequestCost{}, orderedErr
	}
	return RequestCost{
		TenantID: scope.TenantID, ProjectID: scope.ProjectID, RequestID: requestID,
		Status: status, AttemptCount: declaredAttemptCount, LedgerEntryCount: len(ledgerFacts),
		Attempts: attempts,
		RequestLevel: CostBucket{
			LedgerEntryCount: requestLevelEntryCount,
			Entries:          requestLevelEntries,
			Totals:           requestLevelOrdered,
		},
		Totals: requestOrdered,
	}, nil
}

func (fact ledgerFact) entry() (LedgerEntry, error) {
	rate := metering.PriceRate{
		TokenType: fact.tokenType, BillingUnit: fact.billingUnit,
		UnitQuantity: fact.unitQuantity, UnitPriceMicros: fact.unitPriceMicros,
	}
	if !uuidPattern.MatchString(fact.eventID) ||
		(fact.attemptID != "" && !uuidPattern.MatchString(fact.attemptID)) ||
		!uuidPattern.MatchString(fact.priceVersionID) || !currencyPattern.MatchString(fact.currency) ||
		fact.observedAt.IsZero() || fact.createdAt.IsZero() || rate.Validate() != nil ||
		!validSource(fact.source) {
		return LedgerEntry{}, ErrUnavailable
	}
	if fact.source == metering.SourceAdjustment {
		if fact.quantity < -metering.MaximumExactInteger || fact.quantity > metering.MaximumExactInteger ||
			fact.amountMicros < -metering.MaximumExactInteger || fact.amountMicros > metering.MaximumExactInteger ||
			(fact.quantity == 0 && fact.amountMicros == 0) || !validAdjustment(fact.adjustment) {
			return LedgerEntry{}, ErrUnavailable
		}
	} else if fact.quantity < 1 || fact.quantity > metering.MaximumExactInteger ||
		fact.amountMicros < 0 || fact.amountMicros > metering.MaximumExactInteger || fact.adjustment != nil {
		return LedgerEntry{}, ErrUnavailable
	}
	return LedgerEntry{
		EventID: fact.eventID, AttemptID: fact.attemptID, TokenType: fact.tokenType,
		Quantity: fact.quantity, Source: fact.source, ObservedAt: fact.observedAt,
		CreatedAt: fact.createdAt, PriceVersionID: fact.priceVersionID, Currency: fact.currency,
		BillingUnit: fact.billingUnit, UnitQuantity: fact.unitQuantity,
		UnitPriceMicros: fact.unitPriceMicros, AmountMicros: fact.amountMicros,
		Adjustment: cloneAdjustment(fact.adjustment),
	}, nil
}

func validAdjustment(adjustment *LedgerAdjustment) bool {
	return adjustment != nil && uuidPattern.MatchString(adjustment.TargetEventID) &&
		adjustment.Origin.Valid() && reasonPattern.MatchString(adjustment.Reason) &&
		auditPattern.MatchString(adjustment.Reference) && auditPattern.MatchString(adjustment.Actor) &&
		adjustment.CorrectedQuantity >= 0 && adjustment.CorrectedQuantity <= metering.MaximumExactInteger &&
		adjustment.CorrectedAmountMicros >= 0 &&
		adjustment.CorrectedAmountMicros <= metering.MaximumExactInteger
}

func cloneAdjustment(adjustment *LedgerAdjustment) *LedgerAdjustment {
	if adjustment == nil {
		return nil
	}
	cloned := *adjustment
	return &cloned
}

func validSource(source metering.Source) bool {
	for _, supported := range metering.Sources() {
		if source == supported {
			return true
		}
	}
	return false
}

func (totals currencyAccumulator) add(currency string, amount int64) {
	current, exists := totals[currency]
	if !exists {
		current = new(big.Int)
		totals[currency] = current
	}
	current.Add(current, big.NewInt(amount))
}

func (totals currencyAccumulator) ordered() ([]CurrencyTotal, error) {
	currencies := make([]string, 0, len(totals))
	for currency := range totals {
		currencies = append(currencies, currency)
	}
	sort.Strings(currencies)
	result := make([]CurrencyTotal, 0, len(currencies))
	for _, currency := range currencies {
		amount := totals[currency]
		if amount.Sign() < 0 || amount.Cmp(big.NewInt(metering.MaximumExactInteger)) > 0 {
			return nil, ErrUnavailable
		}
		result = append(result, CurrencyTotal{Currency: currency, AmountMicros: amount.Int64()})
	}
	return result, nil
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

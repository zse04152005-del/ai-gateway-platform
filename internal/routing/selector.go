// Package routing selects a physical deployment without performing provider I/O.
package routing

import (
	"context"
	"errors"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/catalog"
)

const maximumCandidates = 256

var (
	// ErrNoCandidate means no authorized, compatible, healthy deployment exists.
	ErrNoCandidate = errors.New("no route candidate")
	// ErrCandidateSource means the authoritative catalog could not be queried safely.
	ErrCandidateSource = errors.New("route candidate source unavailable")
	// ErrHealthUnavailable means health could not be evaluated safely.
	ErrHealthUnavailable = errors.New("route health unavailable")
)

// CandidateSource returns authorized active catalog candidates.
type CandidateSource interface {
	ListRouteCandidates(context.Context, catalog.RouteQuery) ([]catalog.RouteCandidate, error)
}

// HealthReader reports current eligibility without mutating a candidate.
type HealthReader interface {
	Healthy(context.Context, string) (bool, error)
}

// ActiveCatalogHealth is the P06 bootstrap health policy: catalog-active
// candidates are eligible. P08 replaces this implementation with measured
// active/passive health while keeping the selector contract.
type ActiveCatalogHealth struct{}

// Healthy accepts the already-validated active candidate unless cancelled.
func (ActiveCatalogHealth) Healthy(ctx context.Context, _ string) (bool, error) {
	if ctx == nil {
		return false, errors.New("route health context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return true, nil
}

// SelectionRequest contains trusted scope plus one validated normalized request.
type SelectionRequest struct {
	Access  catalog.Access
	Request adapter.NormalizedRequest
}

// Selection is the immutable catalog fact chosen for one first attempt.
type Selection struct {
	Candidate catalog.RouteCandidate
}

// Selector performs deterministic minimum viable priority routing.
type Selector struct {
	filter *CandidateFilter
}

// NewSelector validates dependencies.
func NewSelector(source CandidateSource, health HealthReader) (*Selector, error) {
	return NewSelectorWithEligibility(source, health, allowAllEligibility{}, allowAllEligibility{})
}

// NewSelectorWithEligibility installs explicit budget and capacity readers.
func NewSelectorWithEligibility(source CandidateSource, health HealthReader, budget BudgetReader, capacity CapacityReader) (*Selector, error) {
	filter, err := NewCandidateFilter(source, health, budget, capacity)
	if err != nil {
		return nil, err
	}
	return &Selector{filter: filter}, nil
}

// Select filters request capabilities, then returns the first healthy candidate
// by ascending binding priority and stable provider/deployment tie breakers.
func (selector *Selector) Select(ctx context.Context, request SelectionRequest) (Selection, error) {
	if selector == nil || selector.filter == nil {
		return Selection{}, errors.New("route selector is not initialized")
	}
	result, err := selector.filter.Filter(ctx, request)
	if err != nil {
		return Selection{}, err
	}
	if len(result.eligible) > 0 {
		return Selection{Candidate: result.eligible[0].Clone()}, nil
	}
	return Selection{}, ErrNoCandidate
}

func requiredRequestCapabilities(request adapter.NormalizedRequest) catalog.CapabilityRequirements {
	requirements := catalog.CapabilityRequirements{
		Chat:             true,
		Stream:           request.Stream,
		Tools:            len(request.Tools) > 0,
		StructuredOutput: request.ResponseFormat != nil && request.ResponseFormat.Type != adapter.ResponseFormatText,
		MinOutputTokens:  cloneInt64(request.MaxOutputTokens),
	}
	for _, message := range request.Messages {
		requirements.Tools = requirements.Tools || len(message.ToolCalls) > 0 || message.Role == adapter.RoleTool
		for _, part := range message.Parts {
			requirements.Vision = requirements.Vision || part.Kind == adapter.ContentImageReference
			requirements.AudioInput = requirements.AudioInput || part.Kind == adapter.ContentAudioReference
		}
	}
	return requirements
}

func candidateLess(left, right catalog.RouteCandidate) bool {
	if left.Binding.Priority != right.Binding.Priority {
		return left.Binding.Priority < right.Binding.Priority
	}
	if left.Provider.Code != right.Provider.Code {
		return left.Provider.Code < right.Provider.Code
	}
	if left.Deployment.Code != right.Deployment.Code {
		return left.Deployment.Code < right.Deployment.Code
	}
	return left.Deployment.ID < right.Deployment.ID
}

func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

// Package routing selects a physical deployment without performing provider I/O.
package routing

import (
	"context"
	"errors"
	"fmt"
	"sort"

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
	source CandidateSource
	health HealthReader
}

// NewSelector validates dependencies.
func NewSelector(source CandidateSource, health HealthReader) (*Selector, error) {
	if source == nil {
		return nil, errors.New("route candidate source must not be nil")
	}
	if health == nil {
		return nil, errors.New("route health reader must not be nil")
	}
	return &Selector{source: source, health: health}, nil
}

// Select filters request capabilities, then returns the first healthy candidate
// by ascending binding priority and stable provider/deployment tie breakers.
func (selector *Selector) Select(ctx context.Context, request SelectionRequest) (Selection, error) {
	if selector == nil || selector.source == nil || selector.health == nil {
		return Selection{}, errors.New("route selector is not initialized")
	}
	if ctx == nil {
		return Selection{}, errors.New("route selection context must not be nil")
	}
	if err := request.Request.Validate(); err != nil {
		return Selection{}, fmt.Errorf("validate route request: %w", err)
	}
	candidates, err := selector.source.ListRouteCandidates(ctx, catalog.RouteQuery{
		Access: request.Access, LogicalModel: request.Request.LogicalModel,
	})
	if err != nil {
		return Selection{}, fmt.Errorf("%w: %w", ErrCandidateSource, err)
	}
	if len(candidates) > maximumCandidates {
		return Selection{}, fmt.Errorf("%w: candidate limit exceeded", ErrCandidateSource)
	}
	capabilities := requiredRequestCapabilities(request.Request)
	sort.Slice(candidates, func(left, right int) bool {
		return candidateLess(candidates[left], candidates[right])
	})
	for index := range candidates {
		candidate := candidates[index]
		if err := validateCandidate(request, candidate); err != nil {
			return Selection{}, fmt.Errorf("%w: invalid catalog candidate", ErrCandidateSource)
		}
		if !candidate.Deployment.Capabilities.Satisfies(capabilities) {
			continue
		}
		healthy, err := selector.health.Healthy(ctx, candidate.Deployment.ID)
		if err != nil {
			return Selection{}, fmt.Errorf("%w: %w", ErrHealthUnavailable, err)
		}
		if healthy {
			return Selection{Candidate: candidate.Clone()}, nil
		}
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

func validateCandidate(request SelectionRequest, candidate catalog.RouteCandidate) error {
	if err := candidate.Validate(); err != nil {
		return err
	}
	if candidate.LogicalModel.TenantID != request.Access.TenantID {
		return errors.New("route candidate tenant mismatch")
	}
	if candidate.LogicalModel.Name != request.Request.LogicalModel {
		return errors.New("route candidate logical model mismatch")
	}
	return nil
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

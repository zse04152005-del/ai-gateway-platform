package gateway

import (
	"context"
	"errors"
	"fmt"

	"github.com/zse04152005-del/ai-gateway-platform/internal/retry"
	"github.com/zse04152005-del/ai-gateway-platform/internal/routedecision"
	"github.com/zse04152005-del/ai-gateway-platform/internal/routing"
)

var errRouteDecisionRecord = errors.New("route decision recording failed")

func selectAndRecordRoute(
	ctx context.Context,
	selector RouteSelector,
	recorder routedecision.Recorder,
	requestID string,
	nextAttemptNo int,
	request routing.SelectionRequest,
	retryDecision *retry.Decision,
) (routing.Selection, error) {
	if ctx == nil || selector == nil || recorder == nil || requestID == "" || nextAttemptNo < 1 {
		return routing.Selection{}, ErrFailoverInvalid
	}
	selection, selectionErr := selector.Select(ctx, cloneSelectionRequest(request))
	input := routedecision.Input{
		RequestID: requestID, NextAttemptNo: nextAttemptNo,
		Outcome: routedecision.OutcomeSelectionFailed, Filter: selection.Filter.Clone(),
	}
	if retryDecision != nil {
		cloned := *retryDecision
		input.Retry = &cloned
	}
	switch {
	case selectionErr == nil:
		input.Outcome = routedecision.OutcomeSelected
		policy := selection.Decision.Clone()
		input.Policy = &policy
	case errors.Is(selectionErr, routing.ErrNoCandidate):
		input.Outcome = routedecision.OutcomeNoCandidate
	}
	recordContext, cancel := terminalRecordContext(ctx)
	defer cancel()
	if _, recordErr := recorder.Record(recordContext, input); recordErr != nil {
		return selection.Clone(), fmt.Errorf("%w: %w", errRouteDecisionRecord, recordErr)
	}
	return selection.Clone(), selectionErr
}

func recordRetryDecision(
	ctx context.Context,
	recorder routedecision.Recorder,
	requestID string,
	attemptNo int,
	decision retry.Decision,
) error {
	if ctx == nil || recorder == nil || requestID == "" || attemptNo < 1 {
		return ErrFailoverInvalid
	}
	recordContext, cancel := terminalRecordContext(ctx)
	defer cancel()
	if _, err := recorder.RecordRetry(recordContext, routedecision.RetryInput{
		RequestID: requestID, AttemptNo: attemptNo, Decision: decision,
	}); err != nil {
		return fmt.Errorf("%w: %w", errRouteDecisionRecord, err)
	}
	return nil
}

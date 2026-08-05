// Package gateway assembles data-plane application routes.
package gateway

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/apierror"
	"github.com/zse04152005-del/ai-gateway-platform/internal/catalog"
	"github.com/zse04152005-del/ai-gateway-platform/internal/correlation"
	"github.com/zse04152005-del/ai-gateway-platform/internal/execution"
	"github.com/zse04152005-del/ai-gateway-platform/internal/routedecision"
	"github.com/zse04152005-del/ai-gateway-platform/internal/routing"
)

// Authenticator is implemented by keyauth.Authenticator.
type Authenticator interface {
	Middleware(http.Handler) http.Handler
}

// ModelCatalog resolves only logical models available in one trusted access scope.
type ModelCatalog interface {
	ListAvailable(context.Context, catalog.Access) ([]catalog.AvailableModel, error)
}

// RouteSelector chooses one authorized physical candidate for a normalized request.
type RouteSelector interface {
	Select(context.Context, routing.SelectionRequest) (routing.Selection, error)
}

// ChatExecutor performs one already-selected provider attempt.
type ChatExecutor interface {
	Execute(context.Context, routing.Selection, adapter.NormalizedRequest) (adapter.NormalizedResponse, error)
}

// LocalUsageEstimator produces gateway-owned fallback usage for one selected
// physical model. Implementations must return UsageSourceEstimated and must not
// reinterpret provider-reported usage.
type LocalUsageEstimator interface {
	EstimateUsage(
		context.Context,
		catalog.Deployment,
		adapter.NormalizedRequest,
		*adapter.NormalizedResponse,
	) (adapter.NormalizedUsage, error)
}

// NewHandler protects every /v1 route while leaving unknown non-data-plane paths as safe 404s.
func NewHandler(authenticator Authenticator, modelCatalog ModelCatalog, selectors ...RouteSelector) (http.Handler, error) {
	if authenticator == nil {
		return nil, errors.New("gateway authenticator must not be nil")
	}
	if modelCatalog == nil {
		return nil, errors.New("gateway model catalog must not be nil")
	}
	if len(selectors) > 1 {
		return nil, errors.New("gateway accepts at most one route selector")
	}
	var routeSelector RouteSelector
	if len(selectors) == 1 {
		if selectors[0] == nil {
			return nil, errors.New("gateway route selector must not be nil")
		}
		routeSelector = selectors[0]
	}
	return newHandler(authenticator, modelCatalog, routeSelector, nil, nil, nil, nil), nil
}

// NewExecutableHandler requires the complete non-streaming execution chain.
func NewExecutableHandler(
	authenticator Authenticator,
	modelCatalog ModelCatalog,
	routeSelector RouteSelector,
	executor ChatExecutor,
	recorder execution.Recorder,
	decisionRecorder routedecision.Recorder,
) (http.Handler, error) {
	return NewExecutableHandlerWithFailover(
		authenticator, modelCatalog, routeSelector, executor, recorder, decisionRecorder, DefaultFailoverOptions(),
	)
}

// NewExecutableHandlerWithUsageEstimator installs the production local usage
// fallback while preserving provider usage as the authoritative first choice.
func NewExecutableHandlerWithUsageEstimator(
	authenticator Authenticator,
	modelCatalog ModelCatalog,
	routeSelector RouteSelector,
	executor ChatExecutor,
	recorder execution.Recorder,
	decisionRecorder routedecision.Recorder,
	estimator LocalUsageEstimator,
) (http.Handler, error) {
	if estimator == nil {
		return nil, errors.New("gateway local usage estimator must not be nil")
	}
	return newExecutableHandlerWithFailover(
		authenticator, modelCatalog, routeSelector, executor, recorder, decisionRecorder,
		DefaultFailoverOptions(), estimator,
	)
}

// NewExecutableHandlerWithFailover installs an explicit request-wide failover envelope.
func NewExecutableHandlerWithFailover(
	authenticator Authenticator,
	modelCatalog ModelCatalog,
	routeSelector RouteSelector,
	executor ChatExecutor,
	recorder execution.Recorder,
	decisionRecorder routedecision.Recorder,
	options FailoverOptions,
) (http.Handler, error) {
	return newExecutableHandlerWithFailover(
		authenticator, modelCatalog, routeSelector, executor, recorder, decisionRecorder, options, nil,
	)
}

func newExecutableHandlerWithFailover(
	authenticator Authenticator,
	modelCatalog ModelCatalog,
	routeSelector RouteSelector,
	executor ChatExecutor,
	recorder execution.Recorder,
	decisionRecorder routedecision.Recorder,
	options FailoverOptions,
	estimator LocalUsageEstimator,
) (http.Handler, error) {
	if authenticator == nil {
		return nil, errors.New("gateway authenticator must not be nil")
	}
	if modelCatalog == nil {
		return nil, errors.New("gateway model catalog must not be nil")
	}
	if routeSelector == nil {
		return nil, errors.New("gateway route selector must not be nil")
	}
	if executor == nil {
		return nil, errors.New("gateway chat executor must not be nil")
	}
	if recorder == nil {
		return nil, errors.New("gateway execution recorder must not be nil")
	}
	if decisionRecorder == nil {
		return nil, errors.New("gateway route decision recorder must not be nil")
	}
	failover, err := newNonStreamFailover(
		routeSelector, executor, recorder, decisionRecorder, options, time.Now, defaultRetryWaiter, estimator,
	)
	if err != nil {
		return nil, err
	}
	return newHandler(authenticator, modelCatalog, routeSelector, executor, recorder, decisionRecorder, failover), nil
}

func newHandler(
	authenticator Authenticator,
	modelCatalog ModelCatalog,
	routeSelector RouteSelector,
	executor ChatExecutor,
	recorder execution.Recorder,
	decisionRecorder routedecision.Recorder,
	failover *nonStreamFailover,
) http.Handler {
	notFound := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		apierror.WriteHTTP(writer, apierror.MustNew(apierror.Definition{
			Status: http.StatusNotFound, Code: "NOT_FOUND",
			Message: "The requested resource was not found", Type: "invalid_request_error",
		}, nil), correlation.RequestID(request.Context()), "gateway_error")
	})
	mux := http.NewServeMux()
	mux.Handle("/v1/models", authenticator.Middleware(newModelsHandler(modelCatalog)))
	mux.Handle("/v1/chat/completions", authenticator.Middleware(newChatCompletionsHandler(routeSelector, executor, recorder, decisionRecorder, failover)))
	mux.Handle("/v1/", authenticator.Middleware(notFound))
	mux.Handle("/", notFound)
	return mux
}

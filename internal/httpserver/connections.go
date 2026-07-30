package httpserver

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/apierror"
	"github.com/zse04152005-del/ai-gateway-platform/internal/correlation"
)

var (
	// ErrStreamingShutdown is the cancellation cause for streams during graceful drain.
	ErrStreamingShutdown = errors.New("stream canceled for graceful server shutdown")
	// ErrForcedShutdown is the cancellation cause after the graceful deadline expires.
	ErrForcedShutdown = errors.New("request canceled after server shutdown deadline")
	// ErrServerStopped is the cancellation cause when the listener stops unexpectedly.
	ErrServerStopped = errors.New("request canceled because the HTTP server stopped")
)

type lifecycleContextKey struct{}

type trackedRequest struct {
	manager   *connectionManager
	cancel    context.CancelCauseFunc
	streaming atomic.Bool
}

type connectionManager struct {
	mu        sync.Mutex
	requests  map[*trackedRequest]struct{}
	draining  bool
	errorType string
	busyError *apierror.Error
}

func newConnectionManager(errorType string) *connectionManager {
	return &connectionManager{
		requests:  make(map[*trackedRequest]struct{}),
		errorType: errorType,
		busyError: apierror.MustNew(apierror.Definition{
			Status:     http.StatusServiceUnavailable,
			Code:       "SERVER_DRAINING",
			Message:    "The service is shutting down",
			Type:       errorType,
			Retryable:  true,
			RetryAfter: time.Second,
		}, nil),
	}
}

func (m *connectionManager) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestCtx, state, accepted := m.register(request.Context())
		if !accepted {
			apierror.WriteHTTP(writer, m.busyError, correlation.RequestID(request.Context()), m.errorType)
			return
		}
		defer func() {
			m.unregister(state)
			state.cancel(nil)
		}()
		next.ServeHTTP(writer, request.WithContext(requestCtx))
	})
}

func (m *connectionManager) register(parent context.Context) (context.Context, *trackedRequest, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.draining {
		return nil, nil, false
	}
	requestCtx, cancel := context.WithCancelCause(parent)
	state := &trackedRequest{manager: m, cancel: cancel}
	m.requests[state] = struct{}{}
	return context.WithValue(requestCtx, lifecycleContextKey{}, state), state, true
}

func (m *connectionManager) unregister(state *trackedRequest) {
	m.mu.Lock()
	delete(m.requests, state)
	m.mu.Unlock()
}

func (m *connectionManager) markStreaming(state *trackedRequest) {
	m.mu.Lock()
	state.streaming.Store(true)
	draining := m.draining
	m.mu.Unlock()
	if draining {
		state.cancel(ErrStreamingShutdown)
	}
}

func (m *connectionManager) beginDrain() {
	m.mu.Lock()
	m.draining = true
	streams := m.collectLocked(true)
	m.mu.Unlock()
	cancelRequests(streams, ErrStreamingShutdown)
}

func (m *connectionManager) stop(cause error) {
	m.mu.Lock()
	m.draining = true
	requests := m.collectLocked(false)
	m.mu.Unlock()
	cancelRequests(requests, cause)
}

func (m *connectionManager) cancelAll(cause error) {
	m.mu.Lock()
	requests := m.collectLocked(false)
	m.mu.Unlock()
	cancelRequests(requests, cause)
}

func (m *connectionManager) collectLocked(streamsOnly bool) []*trackedRequest {
	requests := make([]*trackedRequest, 0, len(m.requests))
	for request := range m.requests {
		if !streamsOnly || request.streaming.Load() {
			requests = append(requests, request)
		}
	}
	return requests
}

func (m *connectionManager) counts() (active, streams int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for request := range m.requests {
		active++
		if request.streaming.Load() {
			streams++
		}
	}
	return active, streams
}

func cancelRequests(requests []*trackedRequest, cause error) {
	for _, request := range requests {
		request.cancel(cause)
	}
}

// MarkStreaming marks the current managed HTTP request as long-lived. During
// shutdown its context is canceled immediately while ordinary requests drain.
func MarkStreaming(ctx context.Context) error {
	if ctx == nil {
		return errors.New("stream context must not be nil")
	}
	state, ok := ctx.Value(lifecycleContextKey{}).(*trackedRequest)
	if !ok || state == nil || state.manager == nil {
		return errors.New("stream context is not managed by the HTTP server")
	}
	state.manager.markStreaming(state)
	return nil
}

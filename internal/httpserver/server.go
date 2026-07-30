// Package httpserver provides the shared lifecycle and health surface for HTTP processes.
package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/apierror"
	"github.com/zse04152005-del/ai-gateway-platform/internal/correlation"
)

const (
	defaultVersion = "dev"
	maxHeaderBytes = 1 << 20
	idleTimeout    = 60 * time.Second
)

// Options defines a service HTTP server's identity and process-level behavior.
type Options struct {
	ServiceName        string
	Version            string
	NotReadyCode       string
	NotReadyMessage    string
	ErrorType          string
	ReadHeaderTimeout  time.Duration
	ShutdownTimeout    time.Duration
	ApplicationHandler http.Handler
}

// Server owns one HTTP listener lifecycle and its readiness state.
// A Server is single-use because net/http.Server cannot be restarted after shutdown.
type Server struct {
	serviceName     string
	httpServer      *http.Server
	shutdownTimeout time.Duration
	ready           atomic.Bool
	started         atomic.Bool
}

// NewServer creates a service server. ApplicationHandler receives every route
// not owned by the shared health surface.
func NewServer(options Options) (*Server, error) {
	for name, value := range map[string]string{
		"service name":      options.ServiceName,
		"not-ready code":    options.NotReadyCode,
		"not-ready message": options.NotReadyMessage,
		"error type":        options.ErrorType,
	} {
		if strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("%s must not be empty", name)
		}
	}
	if options.ReadHeaderTimeout <= 0 {
		return nil, errors.New("read header timeout must be greater than zero")
	}
	if options.ShutdownTimeout <= 0 {
		return nil, errors.New("shutdown timeout must be greater than zero")
	}

	version := strings.TrimSpace(options.Version)
	if version == "" {
		version = defaultVersion
	}
	identity := healthIdentity{
		version:         version,
		notReadyCode:    strings.TrimSpace(options.NotReadyCode),
		notReadyMessage: strings.TrimSpace(options.NotReadyMessage),
		errorType:       strings.TrimSpace(options.ErrorType),
	}
	if _, err := apierror.New(apierror.Definition{
		Status:     http.StatusServiceUnavailable,
		Code:       identity.notReadyCode,
		Message:    identity.notReadyMessage,
		Type:       identity.errorType,
		Retryable:  true,
		RetryAfter: time.Second,
	}, nil); err != nil {
		return nil, fmt.Errorf("invalid not-ready public error: %w", err)
	}
	correlationManager, err := correlation.New(correlation.Options{ErrorType: identity.errorType})
	if err != nil {
		return nil, fmt.Errorf("create request correlation manager: %w", err)
	}

	server := &Server{
		serviceName:     strings.TrimSpace(options.ServiceName),
		shutdownTimeout: options.ShutdownTimeout,
	}
	server.httpServer = &http.Server{
		Handler:           correlationManager.Middleware(server.routes(identity, options.ApplicationHandler)),
		ReadHeaderTimeout: options.ReadHeaderTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
	}
	return server, nil
}

// Handler exposes the complete HTTP surface for transport-level tests.
func (s *Server) Handler() http.Handler {
	return s.httpServer.Handler
}

// Ready reports whether the listener is accepting traffic and shutdown has not begun.
func (s *Server) Ready() bool {
	return s.ready.Load()
}

// Serve runs until the listener fails or ctx is canceled. Cancellation first
// makes readiness fail, then drains ordinary in-flight requests within the configured timeout.
func (s *Server) Serve(ctx context.Context, listener net.Listener) error {
	if ctx == nil {
		return errors.New("serve context must not be nil")
	}
	if listener == nil {
		return errors.New("listener must not be nil")
	}
	if !s.started.CompareAndSwap(false, true) {
		return fmt.Errorf("%s HTTP server can only be served once", s.serviceName)
	}

	serveErr := make(chan error, 1)
	s.ready.Store(true)
	go func() {
		serveErr <- s.httpServer.Serve(listener)
	}()

	select {
	case err := <-serveErr:
		s.ready.Store(false)
		return normalizeServeError(s.serviceName, err)
	case <-ctx.Done():
		s.ready.Store(false)
		return s.shutdown(serveErr)
	}
}

func (s *Server) shutdown(serveErr <-chan error) error {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
	defer cancel()

	if err := s.httpServer.Shutdown(shutdownCtx); err != nil {
		closeErr := s.httpServer.Close()
		return errors.Join(
			fmt.Errorf("shutdown %s HTTP server: %w", s.serviceName, err),
			normalizeCloseError(s.serviceName, closeErr),
			normalizeServeError(s.serviceName, <-serveErr),
		)
	}
	return normalizeServeError(s.serviceName, <-serveErr)
}

type healthIdentity struct {
	version         string
	notReadyCode    string
	notReadyMessage string
	errorType       string
}

func (s *Server) routes(identity healthIdentity, applicationHandler http.Handler) http.Handler {
	notReadyError := apierror.MustNew(apierror.Definition{
		Status:     http.StatusServiceUnavailable,
		Code:       identity.notReadyCode,
		Message:    identity.notReadyMessage,
		Type:       identity.errorType,
		Retryable:  true,
		RetryAfter: time.Second,
	}, nil)
	methodError := apierror.MustNew(apierror.Definition{
		Status:  http.StatusMethodNotAllowed,
		Code:    "METHOD_NOT_ALLOWED",
		Message: "Only GET is allowed for health endpoints",
		Type:    "invalid_request_error",
	}, nil)
	notFoundError := apierror.MustNew(apierror.Definition{
		Status:  http.StatusNotFound,
		Code:    "NOT_FOUND",
		Message: "The requested resource was not found",
		Type:    "invalid_request_error",
	}, nil)
	if applicationHandler == nil {
		applicationHandler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			apierror.WriteHTTP(writer, notFoundError, correlation.RequestID(request.Context()), identity.errorType)
		})
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, healthResponse{Status: "ok", Version: identity.version})
	})
	mux.HandleFunc("GET /health/ready", func(writer http.ResponseWriter, request *http.Request) {
		if !s.Ready() {
			apierror.WriteHTTP(writer, notReadyError, correlation.RequestID(request.Context()), identity.errorType)
			return
		}
		writeJSON(writer, http.StatusOK, healthResponse{Status: "ok", Version: identity.version})
	})
	mux.HandleFunc("/health/live", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Allow", http.MethodGet)
		apierror.WriteHTTP(writer, methodError, correlation.RequestID(request.Context()), identity.errorType)
	})
	mux.HandleFunc("/health/ready", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Allow", http.MethodGet)
		apierror.WriteHTTP(writer, methodError, correlation.RequestID(request.Context()), identity.errorType)
	})
	mux.Handle("/", applicationHandler)
	return mux
}

type healthResponse struct {
	Status  string `json:"status"`
	Version string `json:"version,omitempty"`
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		http.Error(writer, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	body = append(body, '\n')

	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.WriteHeader(status)
	if _, err := writer.Write(body); err != nil {
		return
	}
}

func normalizeServeError(serviceName string, err error) error {
	if err == nil || errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return fmt.Errorf("serve %s HTTP: %w", serviceName, err)
}

func normalizeCloseError(serviceName string, err error) error {
	if err == nil || errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return fmt.Errorf("force-close %s HTTP server: %w", serviceName, err)
}

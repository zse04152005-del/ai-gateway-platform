// Package gateway provides the data-plane HTTP process lifecycle.
package gateway

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
)

const (
	defaultVersion = "dev"
	maxHeaderBytes = 1 << 20
	idleTimeout    = 60 * time.Second
)

// Options defines the gateway HTTP server's process-level behavior.
type Options struct {
	Version            string
	ReadHeaderTimeout  time.Duration
	ShutdownTimeout    time.Duration
	ApplicationHandler http.Handler
}

// Server owns the gateway HTTP listener lifecycle and readiness state.
// A Server is single-use because net/http.Server cannot be restarted after shutdown.
type Server struct {
	httpServer      *http.Server
	shutdownTimeout time.Duration
	ready           atomic.Bool
	started         atomic.Bool
}

// NewServer creates a gateway server. ApplicationHandler receives all routes
// not owned by the process health surface.
func NewServer(options Options) (*Server, error) {
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

	server := &Server{shutdownTimeout: options.ShutdownTimeout}
	server.httpServer = &http.Server{
		Handler:           server.routes(version, options.ApplicationHandler),
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
		return errors.New("gateway server can only be served once")
	}

	serveErr := make(chan error, 1)
	s.ready.Store(true)
	go func() {
		serveErr <- s.httpServer.Serve(listener)
	}()

	select {
	case err := <-serveErr:
		s.ready.Store(false)
		return normalizeServeError(err)
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
			fmt.Errorf("shutdown gateway HTTP server: %w", err),
			normalizeCloseError(closeErr),
			normalizeServeError(<-serveErr),
		)
	}
	return normalizeServeError(<-serveErr)
}

func (s *Server) routes(version string, applicationHandler http.Handler) http.Handler {
	if applicationHandler == nil {
		applicationHandler = http.NotFoundHandler()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, healthResponse{Status: "ok", Version: version})
	})
	mux.HandleFunc("GET /health/ready", func(writer http.ResponseWriter, _ *http.Request) {
		if !s.Ready() {
			writer.Header().Set("Retry-After", "1")
			writeJSON(writer, http.StatusServiceUnavailable, errorEnvelope{
				Error: errorDetail{
					Code:       "GATEWAY_NOT_READY",
					Message:    "Gateway is not ready",
					Type:       "gateway_error",
					RequestID:  "",
					Retryable:  true,
					RetryAfter: int64Pointer(1000),
				},
			})
			return
		}
		writeJSON(writer, http.StatusOK, healthResponse{Status: "ok", Version: version})
	})
	mux.HandleFunc("/health/live", healthMethodNotAllowed)
	mux.HandleFunc("/health/ready", healthMethodNotAllowed)
	mux.Handle("/", applicationHandler)
	return mux
}

func healthMethodNotAllowed(writer http.ResponseWriter, _ *http.Request) {
	writer.Header().Set("Allow", http.MethodGet)
	writeJSON(writer, http.StatusMethodNotAllowed, errorEnvelope{
		Error: errorDetail{
			Code:      "METHOD_NOT_ALLOWED",
			Message:   "Only GET is allowed for health endpoints",
			Type:      "invalid_request_error",
			RequestID: "",
			Retryable: false,
		},
	})
}

type healthResponse struct {
	Status  string `json:"status"`
	Version string `json:"version,omitempty"`
}

type errorEnvelope struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code       string  `json:"code"`
	Message    string  `json:"message"`
	Type       string  `json:"type"`
	Param      *string `json:"param"`
	RequestID  string  `json:"request_id"`
	Retryable  bool    `json:"retryable"`
	RetryAfter *int64  `json:"retry_after_ms"`
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

func normalizeServeError(err error) error {
	if err == nil || errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return fmt.Errorf("serve gateway HTTP: %w", err)
}

func normalizeCloseError(err error) error {
	if err == nil || errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return fmt.Errorf("force-close gateway HTTP server: %w", err)
}

func int64Pointer(value int64) *int64 {
	return &value
}

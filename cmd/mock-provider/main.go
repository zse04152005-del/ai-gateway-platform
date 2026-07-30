// Command mock-provider runs the deterministic local-only provider simulator.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/zse04152005-del/ai-gateway-platform/internal/config"
	"github.com/zse04152005-del/ai-gateway-platform/internal/httpserver"
	"github.com/zse04152005-del/ai-gateway-platform/internal/mockprovider"
	"github.com/zse04152005-del/ai-gateway-platform/internal/observability"
)

var version = "dev"

type listenFunc func(network, address string) (net.Listener, error)

func main() {
	os.Exit(realMain())
}

func realMain() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := runWithLogs(ctx, os.LookupEnv, net.Listen, os.Stderr); err != nil {
		bootstrapLogger().Error(ctx, "process stopped with error", observability.Fields{},
			slog.String("errorCode", "MOCK_PROVIDER_PROCESS_FAILED"))
		return 1
	}
	return 0
}

func run(ctx context.Context, lookup config.LookupEnv, listen listenFunc) error {
	return runWithLogs(ctx, lookup, listen, io.Discard)
}

func runWithLogs(ctx context.Context, lookup config.LookupEnv, listen listenFunc, logWriter io.Writer) (runErr error) {
	if ctx == nil {
		return errors.New("mock-provider context must not be nil")
	}
	if listen == nil {
		return errors.New("mock-provider listen function must not be nil")
	}
	cfg, err := config.LoadMockProvider(lookup)
	if err != nil {
		return fmt.Errorf("load mock-provider configuration: %w", err)
	}
	logger, err := observability.NewJSON(logWriter, "mock-provider", version, cfg.Environment.LogLevel)
	if err != nil {
		return fmt.Errorf("create mock-provider logger: %w", err)
	}
	server, err := httpserver.NewServer(httpserver.Options{
		ServiceName:        "mock-provider",
		Version:            version,
		NotReadyCode:       "MOCK_PROVIDER_NOT_READY",
		NotReadyMessage:    "Mock provider is not ready",
		ErrorType:          "mock_provider_error",
		ReadHeaderTimeout:  cfg.HTTP.ReadHeaderTimeout,
		ShutdownTimeout:    cfg.HTTP.ShutdownTimeout,
		ApplicationHandler: mockprovider.NewHandler(),
	})
	if err != nil {
		return fmt.Errorf("create mock-provider server: %w", err)
	}
	listener, err := listen("tcp", cfg.HTTP.Addr)
	if err != nil {
		return fmt.Errorf("listen for mock-provider HTTP on %q: %w", cfg.HTTP.Addr, err)
	}
	if listener == nil {
		return errors.New("listen for mock-provider HTTP returned a nil listener")
	}
	defer func() {
		if closeErr := listener.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			runErr = errors.Join(runErr, fmt.Errorf("close mock-provider listener: %w", closeErr))
		}
	}()

	logger.Info(ctx, "HTTP server started", observability.Fields{}, slog.String("listenAddress", cfg.HTTP.Addr))
	runErr = server.Serve(ctx, listener)
	if runErr == nil {
		logger.Info(ctx, "HTTP server stopped", observability.Fields{})
	}
	return runErr
}

func bootstrapLogger() *observability.Logger {
	return observability.MustNewJSON(os.Stderr, "mock-provider", version, "info")
}

// Command gateway runs the AI gateway data-plane HTTP process.
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
		bootstrapLogger("gateway").Error(ctx, "process stopped with error", observability.Fields{},
			slog.String("errorCode", "GATEWAY_PROCESS_FAILED"))
		return 1
	}
	return 0
}

func run(ctx context.Context, lookup config.LookupEnv, listen listenFunc) (runErr error) {
	return runWithLogs(ctx, lookup, listen, io.Discard)
}

func runWithLogs(ctx context.Context, lookup config.LookupEnv, listen listenFunc, logWriter io.Writer) (runErr error) {
	if ctx == nil {
		return errors.New("gateway context must not be nil")
	}
	if listen == nil {
		return errors.New("gateway listen function must not be nil")
	}

	cfg, err := config.Load(lookup)
	if err != nil {
		return fmt.Errorf("load gateway configuration: %w", err)
	}
	logger, err := observability.NewJSON(logWriter, "gateway", version, cfg.Environment.LogLevel)
	if err != nil {
		return fmt.Errorf("create gateway logger: %w", err)
	}
	server, err := httpserver.NewServer(httpserver.Options{
		ServiceName:       "gateway",
		Version:           version,
		NotReadyCode:      "GATEWAY_NOT_READY",
		NotReadyMessage:   "Gateway is not ready",
		ErrorType:         "gateway_error",
		ReadHeaderTimeout: cfg.HTTP.ReadHeaderTimeout,
		ShutdownTimeout:   cfg.HTTP.ShutdownTimeout,
	})
	if err != nil {
		return fmt.Errorf("create gateway server: %w", err)
	}

	listener, err := listen("tcp", cfg.HTTP.GatewayAddr)
	if err != nil {
		return fmt.Errorf("listen for gateway HTTP on %q: %w", cfg.HTTP.GatewayAddr, err)
	}
	if listener == nil {
		return errors.New("listen for gateway HTTP returned a nil listener")
	}
	defer func() {
		if closeErr := listener.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			runErr = errors.Join(runErr, fmt.Errorf("close gateway listener: %w", closeErr))
		}
	}()

	logger.Info(ctx, "HTTP server started", observability.Fields{},
		slog.String("listenAddress", cfg.HTTP.GatewayAddr))
	runErr = server.Serve(ctx, listener)
	if runErr == nil {
		logger.Info(ctx, "HTTP server stopped", observability.Fields{})
	}
	return runErr
}

func bootstrapLogger(service string) *observability.Logger {
	return observability.MustNewJSON(os.Stderr, service, version, "info")
}

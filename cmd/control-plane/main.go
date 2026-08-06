// Command control-plane runs the AI gateway management-plane HTTP process.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/lib/pq"

	"github.com/zse04152005-del/ai-gateway-platform/internal/config"
	"github.com/zse04152005-del/ai-gateway-platform/internal/controlplane"
	"github.com/zse04152005-del/ai-gateway-platform/internal/httpserver"
	"github.com/zse04152005-del/ai-gateway-platform/internal/meteringcost"
	"github.com/zse04152005-del/ai-gateway-platform/internal/observability"
	"github.com/zse04152005-del/ai-gateway-platform/internal/virtualkey"
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
		bootstrapLogger("control-plane").Error(ctx, "process stopped with error", observability.Fields{},
			slog.String("errorCode", "CONTROL_PLANE_PROCESS_FAILED"))
		return 1
	}
	return 0
}

func run(ctx context.Context, lookup config.LookupEnv, listen listenFunc) (runErr error) {
	return runWithLogs(ctx, lookup, listen, io.Discard)
}

func runWithLogs(ctx context.Context, lookup config.LookupEnv, listen listenFunc, logWriter io.Writer) (runErr error) {
	if ctx == nil {
		return errors.New("control-plane context must not be nil")
	}
	if listen == nil {
		return errors.New("control-plane listen function must not be nil")
	}

	cfg, err := config.Load(lookup)
	if err != nil {
		return fmt.Errorf("load control-plane configuration: %w", err)
	}
	logger, err := observability.NewJSON(logWriter, "control-plane", version, cfg.Environment.LogLevel)
	if err != nil {
		return fmt.Errorf("create control-plane logger: %w", err)
	}
	digestKey, err := cfg.ResolveVirtualKeyHashKey()
	if err != nil {
		return fmt.Errorf("resolve virtual credential digest key: %w", err)
	}
	digester, err := virtualkey.NewHMACDigester(cfg.Security.VirtualKeyHashVersion, digestKey)
	clear(digestKey)
	if err != nil {
		return fmt.Errorf("create virtual credential digester: %w", err)
	}
	database, err := sql.Open("postgres", cfg.Postgres.URL)
	if err != nil {
		return fmt.Errorf("open control-plane database: %w", err)
	}
	database.SetMaxOpenConns(10)
	database.SetMaxIdleConns(5)
	database.SetConnMaxLifetime(30 * time.Minute)
	defer func() {
		if closeErr := database.Close(); closeErr != nil {
			runErr = errors.Join(runErr, fmt.Errorf("close control-plane database: %w", closeErr))
		}
	}()
	virtualKeyStore, err := virtualkey.NewPostgresStore(database)
	if err != nil {
		return fmt.Errorf("create virtual credential store: %w", err)
	}
	virtualKeyManager, err := virtualkey.NewProductionManager(virtualKeyStore, digester)
	if err != nil {
		return fmt.Errorf("create virtual credential manager: %w", err)
	}
	costAggregator, err := meteringcost.NewPostgresAggregator(database)
	if err != nil {
		return fmt.Errorf("create request cost aggregator: %w", err)
	}
	server, err := httpserver.NewServer(httpserver.Options{
		ServiceName:        "control-plane",
		Version:            version,
		NotReadyCode:       "CONTROL_PLANE_NOT_READY",
		NotReadyMessage:    "Control plane is not ready",
		ErrorType:          "control_plane_error",
		ReadHeaderTimeout:  cfg.HTTP.ReadHeaderTimeout,
		ShutdownTimeout:    cfg.HTTP.ShutdownTimeout,
		ApplicationHandler: controlplane.NewHandlerWithServices(version, virtualKeyManager, costAggregator),
	})
	if err != nil {
		return fmt.Errorf("create control-plane server: %w", err)
	}

	listener, err := listen("tcp", cfg.HTTP.ControlPlaneAddr)
	if err != nil {
		return fmt.Errorf("listen for control-plane HTTP on %q: %w", cfg.HTTP.ControlPlaneAddr, err)
	}
	if listener == nil {
		return errors.New("listen for control-plane HTTP returned a nil listener")
	}
	defer func() {
		if closeErr := listener.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			runErr = errors.Join(runErr, fmt.Errorf("close control-plane listener: %w", closeErr))
		}
	}()

	logger.Info(ctx, "HTTP server started", observability.Fields{},
		slog.String("listenAddress", cfg.HTTP.ControlPlaneAddr))
	runErr = server.Serve(ctx, listener)
	if runErr == nil {
		logger.Info(ctx, "HTTP server stopped", observability.Fields{})
	}
	return runErr
}

func bootstrapLogger(service string) *observability.Logger {
	return observability.MustNewJSON(os.Stderr, service, version, "info")
}

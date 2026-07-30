// Command gateway runs the AI gateway data-plane HTTP process.
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

	"github.com/zse04152005-del/ai-gateway-platform/internal/catalog"
	"github.com/zse04152005-del/ai-gateway-platform/internal/config"
	"github.com/zse04152005-del/ai-gateway-platform/internal/gateway"
	"github.com/zse04152005-del/ai-gateway-platform/internal/httpserver"
	"github.com/zse04152005-del/ai-gateway-platform/internal/keyauth"
	"github.com/zse04152005-del/ai-gateway-platform/internal/observability"
	"github.com/zse04152005-del/ai-gateway-platform/internal/routing"
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
	digestKey, err := cfg.ResolveVirtualKeyHashKey()
	if err != nil {
		return fmt.Errorf("resolve virtual credential digest key: %w", err)
	}
	keyring, err := keyauth.NewKeyring(cfg.Security.VirtualKeyHashVersion, digestKey, nil)
	clear(digestKey)
	if err != nil {
		return fmt.Errorf("create virtual credential authentication keyring: %w", err)
	}
	database, err := sql.Open("postgres", cfg.Postgres.URL)
	if err != nil {
		return fmt.Errorf("open gateway database: %w", err)
	}
	database.SetMaxOpenConns(20)
	database.SetMaxIdleConns(10)
	database.SetConnMaxLifetime(30 * time.Minute)
	defer func() {
		if closeErr := database.Close(); closeErr != nil {
			runErr = errors.Join(runErr, fmt.Errorf("close gateway database: %w", closeErr))
		}
	}()
	authenticationStore, err := keyauth.NewPostgresStore(database)
	if err != nil {
		return fmt.Errorf("create virtual credential authentication store: %w", err)
	}
	authenticationCache, err := keyauth.NewMemoryCache(
		cfg.Security.VirtualKeyAuthCacheTTL,
		10_000,
		time.Now,
	)
	if err != nil {
		return fmt.Errorf("create virtual credential authentication cache: %w", err)
	}
	authenticator, err := keyauth.NewAuthenticator(authenticationStore, keyring, authenticationCache, time.Now)
	if err != nil {
		return fmt.Errorf("create virtual credential authenticator: %w", err)
	}
	modelCatalog, err := catalog.NewPostgresStore(database)
	if err != nil {
		return fmt.Errorf("create model catalog store: %w", err)
	}
	routeSelector, err := routing.NewSelector(modelCatalog, routing.ActiveCatalogHealth{})
	if err != nil {
		return fmt.Errorf("create route selector: %w", err)
	}
	applicationHandler, err := gateway.NewHandler(authenticator, modelCatalog, routeSelector)
	if err != nil {
		return fmt.Errorf("create gateway application handler: %w", err)
	}
	server, err := httpserver.NewServer(httpserver.Options{
		ServiceName:        "gateway",
		Version:            version,
		NotReadyCode:       "GATEWAY_NOT_READY",
		NotReadyMessage:    "Gateway is not ready",
		ErrorType:          "gateway_error",
		ReadHeaderTimeout:  cfg.HTTP.ReadHeaderTimeout,
		ShutdownTimeout:    cfg.HTTP.ShutdownTimeout,
		ApplicationHandler: applicationHandler,
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

// Command gateway runs the AI gateway data-plane HTTP process.
package main

import (
	"context"
	"crypto/rand"
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

	"github.com/zse04152005-del/ai-gateway-platform/internal/activehealth"
	"github.com/zse04152005-del/ai-gateway-platform/internal/catalog"
	"github.com/zse04152005-del/ai-gateway-platform/internal/config"
	"github.com/zse04152005-del/ai-gateway-platform/internal/execution"
	"github.com/zse04152005-del/ai-gateway-platform/internal/gateway"
	"github.com/zse04152005-del/ai-gateway-platform/internal/httpserver"
	"github.com/zse04152005-del/ai-gateway-platform/internal/keyauth"
	"github.com/zse04152005-del/ai-gateway-platform/internal/mockadapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/observability"
	"github.com/zse04152005-del/ai-gateway-platform/internal/openaiadapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/provideradapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/providersecret"
	"github.com/zse04152005-del/ai-gateway-platform/internal/proxy"
	"github.com/zse04152005-del/ai-gateway-platform/internal/routing"
	"github.com/zse04152005-del/ai-gateway-platform/internal/upstreamhttp"
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
	upstreamClient, err := upstreamhttp.NewClient(upstreamhttp.Options{
		ConnectTimeout:         cfg.UpstreamHTTP.ConnectTimeout,
		KeepAlive:              cfg.UpstreamHTTP.KeepAlive,
		TLSHandshakeTimeout:    cfg.UpstreamHTTP.TLSHandshakeTimeout,
		ResponseHeaderTimeout:  cfg.UpstreamHTTP.ResponseHeaderTimeout,
		TotalTimeout:           cfg.UpstreamHTTP.TotalTimeout,
		IdleConnTimeout:        cfg.UpstreamHTTP.IdleConnTimeout,
		ExpectContinueTimeout:  cfg.UpstreamHTTP.ExpectContinueTimeout,
		MaxIdleConns:           cfg.UpstreamHTTP.MaxIdleConns,
		MaxIdleConnsPerHost:    cfg.UpstreamHTTP.MaxIdleConnsPerHost,
		MaxConnsPerHost:        cfg.UpstreamHTTP.MaxConnsPerHost,
		MaxResponseHeaderBytes: cfg.UpstreamHTTP.MaxResponseHeaderBytes,
	})
	if err != nil {
		return fmt.Errorf("create upstream HTTP client: %w", err)
	}
	defer upstreamClient.CloseIdleConnections()
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
	passiveHealth, err := routing.NewPassiveHealth(routing.DefaultPassiveHealthOptions(), time.Now)
	if err != nil {
		return fmt.Errorf("create passive health tracker: %w", err)
	}
	activeHealthOptions := activehealth.DefaultOptions()
	activeHealthTracker, err := activehealth.NewTracker(activeHealthOptions, time.Now)
	if err != nil {
		return fmt.Errorf("create active health tracker: %w", err)
	}
	circuitBreaker, err := routing.NewCircuitBreaker(routing.DefaultCircuitOptions(), time.Now)
	if err != nil {
		return fmt.Errorf("create deployment circuit breaker: %w", err)
	}
	combinedHealth, err := routing.NewCompositeHealth(passiveHealth, activeHealthTracker, circuitBreaker)
	if err != nil {
		return fmt.Errorf("create composite route health: %w", err)
	}
	routeSelector, err := routing.NewSelector(modelCatalog, combinedHealth)
	if err != nil {
		return fmt.Errorf("create route selector: %w", err)
	}
	adapterRegistry, err := newAdapterRegistry(database, cfg)
	if err != nil {
		return fmt.Errorf("create provider adapter registry: %w", err)
	}
	chatExecutor, err := proxy.NewNonStreamExecutor(adapterRegistry, upstreamClient)
	if err != nil {
		return fmt.Errorf("create chat executor: %w", err)
	}
	observedChatExecutor, err := gateway.NewObservedChatExecutor(chatExecutor, passiveHealth, time.Now)
	if err != nil {
		return fmt.Errorf("create observed chat executor: %w", err)
	}
	circuitChatExecutor, err := gateway.NewCircuitChatExecutor(observedChatExecutor, circuitBreaker)
	if err != nil {
		return fmt.Errorf("create circuit chat executor: %w", err)
	}
	executionRecorder, err := execution.NewPostgresRecorder(database, time.Now, rand.Reader)
	if err != nil {
		return fmt.Errorf("create execution recorder: %w", err)
	}
	probeClient, err := newActiveHealthClient(cfg.UpstreamHTTP)
	if err != nil {
		return fmt.Errorf("create isolated active health client: %w", err)
	}
	defer probeClient.CloseIdleConnections()
	activeProber, err := activehealth.NewAdapterProber(adapterRegistry, probeClient, time.Now)
	if err != nil {
		return fmt.Errorf("create active health prober: %w", err)
	}
	activeScheduler, err := activehealth.NewScheduler(
		activeHealthOptions, modelCatalog, activeProber, activeHealthTracker, passiveHealth, time.Now,
	)
	if err != nil {
		return fmt.Errorf("create active health scheduler: %w", err)
	}
	applicationHandler, err := gateway.NewExecutableHandler(
		authenticator, modelCatalog, routeSelector, circuitChatExecutor, executionRecorder,
	)
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
	healthContext, stopHealth := context.WithCancel(ctx)
	healthDone := make(chan struct{})
	go func() {
		defer close(healthDone)
		if schedulerErr := activeScheduler.Run(healthContext); schedulerErr != nil {
			logger.Error(healthContext, "active health scheduler stopped with error", observability.Fields{},
				slog.String("errorCode", "ACTIVE_HEALTH_SCHEDULER_FAILED"))
		}
	}()
	defer func() {
		stopHealth()
		<-healthDone
	}()

	logger.Info(ctx, "HTTP server started", observability.Fields{},
		slog.String("listenAddress", cfg.HTTP.GatewayAddr))
	runErr = server.Serve(ctx, listener)
	if runErr == nil {
		logger.Info(ctx, "HTTP server stopped", observability.Fields{})
	}
	return runErr
}

type optionalProviderSecretResolver struct {
	manager *providersecret.Manager
}

func (resolver optionalProviderSecretResolver) Resolve(
	ctx context.Context,
	locator providersecret.Locator,
) ([]byte, error) {
	if resolver.manager == nil {
		return nil, providersecret.ErrBackendUnavailable
	}
	return resolver.manager.Resolve(ctx, locator)
}

func newChatExecutor(
	database *sql.DB,
	cfg config.Config,
	client *upstreamhttp.Client,
) (*proxy.NonStreamExecutor, error) {
	if database == nil {
		return nil, errors.New("chat executor database must not be nil")
	}
	if client == nil {
		return nil, errors.New("chat executor HTTP client must not be nil")
	}
	registry, err := newAdapterRegistry(database, cfg)
	if err != nil {
		return nil, err
	}
	executor, err := proxy.NewNonStreamExecutor(registry, client)
	if err != nil {
		return nil, fmt.Errorf("create non-stream executor: %w", err)
	}
	return executor, nil
}

func newAdapterRegistry(database *sql.DB, cfg config.Config) (*provideradapter.Registry, error) {
	if database == nil {
		return nil, errors.New("provider adapter registry database must not be nil")
	}
	secretResolver := optionalProviderSecretResolver{}
	if len(cfg.Security.LocalEnvelopeKey) > 0 {
		cipher, err := providersecret.NewLocalCipher(
			cfg.Security.LocalEnvelopeKeyVersion,
			cfg.Security.LocalEnvelopeKey,
			nil,
			rand.Reader,
		)
		clear(cfg.Security.LocalEnvelopeKey)
		if err != nil {
			return nil, fmt.Errorf("create local provider secret cipher: %w", err)
		}
		store, err := providersecret.NewPostgresStore(database)
		if err != nil {
			return nil, fmt.Errorf("create provider secret store: %w", err)
		}
		manager, err := providersecret.NewManager(store, cipher, nil, rand.Reader, time.Now)
		if err != nil {
			return nil, fmt.Errorf("create provider secret manager: %w", err)
		}
		secretResolver.manager = manager
	}
	openAIFactory, err := openaiadapter.NewFactory(openaiadapter.FactoryOptions{Secrets: secretResolver})
	if err != nil {
		return nil, fmt.Errorf("create OpenAI adapter factory: %w", err)
	}
	mockFactory, err := mockadapter.NewFactory(mockadapter.FactoryOptions{})
	if err != nil {
		return nil, fmt.Errorf("create mock adapter factory: %w", err)
	}
	registry, err := provideradapter.NewRegistry(openAIFactory, mockFactory)
	if err != nil {
		return nil, fmt.Errorf("create provider adapter registry: %w", err)
	}
	return registry, nil
}

func newActiveHealthClient(cfg config.UpstreamHTTPConfig) (*upstreamhttp.Client, error) {
	return upstreamhttp.NewClient(upstreamhttp.Options{
		ConnectTimeout:         minimumDuration(cfg.ConnectTimeout, 2*time.Second),
		KeepAlive:              cfg.KeepAlive,
		TLSHandshakeTimeout:    minimumDuration(cfg.TLSHandshakeTimeout, 2*time.Second),
		ResponseHeaderTimeout:  minimumDuration(cfg.ResponseHeaderTimeout, 5*time.Second),
		TotalTimeout:           minimumDuration(cfg.TotalTimeout, 5*time.Second),
		IdleConnTimeout:        minimumDuration(cfg.IdleConnTimeout, 30*time.Second),
		ExpectContinueTimeout:  cfg.ExpectContinueTimeout,
		MaxIdleConns:           16,
		MaxIdleConnsPerHost:    2,
		MaxConnsPerHost:        2,
		MaxResponseHeaderBytes: cfg.MaxResponseHeaderBytes,
	})
}

func minimumDuration(value, maximum time.Duration) time.Duration {
	if value < maximum {
		return value
	}
	return maximum
}

func bootstrapLogger(service string) *observability.Logger {
	return observability.MustNewJSON(os.Stderr, service, version, "info")
}

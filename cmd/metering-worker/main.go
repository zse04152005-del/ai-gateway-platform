// Command metering-worker runs the AI gateway usage metering process.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/config"
	"github.com/zse04152005-del/ai-gateway-platform/internal/meteringworker"
	"github.com/zse04152005-del/ai-gateway-platform/internal/observability"
)

const eventBusConnectTimeout = 5 * time.Second

var version = "dev"

func main() {
	os.Exit(realMain())
}

func realMain() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := runWithLogs(ctx, os.LookupEnv, meteringworker.NewTCPConnector(), os.Stderr); err != nil {
		bootstrapLogger("metering-worker").Error(ctx, "process stopped with error", observability.Fields{},
			slog.String("errorCode", "METERING_WORKER_PROCESS_FAILED"))
		return 1
	}
	return 0
}

func run(ctx context.Context, lookup config.LookupEnv, connector meteringworker.Connector) error {
	return runWithLogs(ctx, lookup, connector, io.Discard)
}

func runWithLogs(ctx context.Context, lookup config.LookupEnv, connector meteringworker.Connector, logWriter io.Writer) error {
	if ctx == nil {
		return errors.New("metering-worker context must not be nil")
	}
	if connector == nil {
		return errors.New("metering-worker connector must not be nil")
	}

	cfg, err := config.Load(lookup)
	if err != nil {
		return fmt.Errorf("load metering-worker configuration: %w", err)
	}
	logger, err := observability.NewJSON(logWriter, "metering-worker", version, cfg.Environment.LogLevel)
	if err != nil {
		return fmt.Errorf("create metering-worker logger: %w", err)
	}
	worker, err := meteringworker.New(meteringworker.Options{
		Brokers:         cfg.Kafka.Brokers,
		ConnectTimeout:  eventBusConnectTimeout,
		ShutdownTimeout: cfg.HTTP.ShutdownTimeout,
		Connector:       connector,
	})
	if err != nil {
		return fmt.Errorf("create metering worker: %w", err)
	}
	logger.Info(ctx, "metering worker started", observability.Fields{},
		slog.Int("brokerCount", len(cfg.Kafka.Brokers)))
	if err := worker.Run(ctx); err != nil {
		return err
	}
	logger.Info(ctx, "metering worker stopped", observability.Fields{})
	return nil
}

func bootstrapLogger(service string) *observability.Logger {
	return observability.MustNewJSON(os.Stderr, service, version, "info")
}

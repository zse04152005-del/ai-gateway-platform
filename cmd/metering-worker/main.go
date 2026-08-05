// Command metering-worker runs the AI gateway usage metering process.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/lib/pq"

	"github.com/zse04152005-del/ai-gateway-platform/internal/config"
	"github.com/zse04152005-del/ai-gateway-platform/internal/meteringconsumer"
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

	if err := runProductionWithLogs(ctx, os.LookupEnv, os.Stderr); err != nil {
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
	return runConfigured(ctx, cfg, connector, logger)
}

func runProductionWithLogs(
	ctx context.Context,
	lookup config.LookupEnv,
	logWriter io.Writer,
) error {
	if ctx == nil {
		return errors.New("metering-worker context must not be nil")
	}
	cfg, err := config.Load(lookup)
	if err != nil {
		return fmt.Errorf("load metering-worker configuration: %w", err)
	}
	logger, err := observability.NewJSON(logWriter, "metering-worker", version, cfg.Environment.LogLevel)
	if err != nil {
		return fmt.Errorf("create metering-worker logger: %w", err)
	}
	database, err := sql.Open("postgres", cfg.Postgres.URL)
	if err != nil {
		return fmt.Errorf("open metering database: %w", err)
	}
	defer func() { _ = database.Close() }()
	database.SetMaxOpenConns(8)
	database.SetMaxIdleConns(4)
	database.SetConnMaxLifetime(30 * time.Minute)
	processor, err := meteringconsumer.NewProcessor(
		database, meteringconsumer.DefaultConsumerGroup, time.Now,
	)
	if err != nil {
		return fmt.Errorf("create usage event processor: %w", err)
	}
	connector, err := meteringconsumer.NewKafkaConnector(processor, meteringconsumer.KafkaOptions{
		ConsumerGroup: meteringconsumer.DefaultConsumerGroup,
	})
	if err != nil {
		return fmt.Errorf("create usage event Kafka consumer: %w", err)
	}
	return runConfigured(ctx, cfg, connector, logger)
}

func runConfigured(
	ctx context.Context,
	cfg config.Config,
	connector meteringworker.Connector,
	logger *observability.Logger,
) error {
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

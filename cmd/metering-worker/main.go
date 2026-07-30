// Command metering-worker runs the AI gateway usage metering process.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/config"
	"github.com/zse04152005-del/ai-gateway-platform/internal/meteringworker"
)

const eventBusConnectTimeout = 5 * time.Second

func main() {
	os.Exit(realMain())
}

func realMain() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.LookupEnv, meteringworker.NewTCPConnector()); err != nil {
		log.Printf("metering-worker stopped with error: %v", err)
		return 1
	}
	return 0
}

func run(ctx context.Context, lookup config.LookupEnv, connector meteringworker.Connector) error {
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
	worker, err := meteringworker.New(meteringworker.Options{
		Brokers:         cfg.Kafka.Brokers,
		ConnectTimeout:  eventBusConnectTimeout,
		ShutdownTimeout: cfg.HTTP.ShutdownTimeout,
		Connector:       connector,
	})
	if err != nil {
		return fmt.Errorf("create metering worker: %w", err)
	}
	return worker.Run(ctx)
}

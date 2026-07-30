// Command gateway runs the AI gateway data-plane HTTP process.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/zse04152005-del/ai-gateway-platform/internal/config"
	"github.com/zse04152005-del/ai-gateway-platform/internal/gateway"
)

var version = "dev"

type listenFunc func(network, address string) (net.Listener, error)

func main() {
	os.Exit(realMain())
}

func realMain() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.LookupEnv, net.Listen); err != nil {
		log.Printf("gateway stopped with error: %v", err)
		return 1
	}
	return 0
}

func run(ctx context.Context, lookup config.LookupEnv, listen listenFunc) (runErr error) {
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
	server, err := gateway.NewServer(gateway.Options{
		Version:           version,
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

	return server.Serve(ctx, listener)
}

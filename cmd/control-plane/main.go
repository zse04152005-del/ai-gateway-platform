// Command control-plane runs the AI gateway management-plane HTTP process.
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
	"github.com/zse04152005-del/ai-gateway-platform/internal/controlplane"
	"github.com/zse04152005-del/ai-gateway-platform/internal/httpserver"
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
		log.Printf("control-plane stopped with error: %v", err)
		return 1
	}
	return 0
}

func run(ctx context.Context, lookup config.LookupEnv, listen listenFunc) (runErr error) {
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
	server, err := httpserver.NewServer(httpserver.Options{
		ServiceName:        "control-plane",
		Version:            version,
		NotReadyCode:       "CONTROL_PLANE_NOT_READY",
		NotReadyMessage:    "Control plane is not ready",
		ErrorType:          "control_plane_error",
		ReadHeaderTimeout:  cfg.HTTP.ReadHeaderTimeout,
		ShutdownTimeout:    cfg.HTTP.ShutdownTimeout,
		ApplicationHandler: controlplane.NewHandler(version),
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

	return server.Serve(ctx, listener)
}

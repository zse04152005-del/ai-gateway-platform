//go:build integration

package integration_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/meteringworker"
)

func TestMeteringWorkerConnectsToEventBus(t *testing.T) {
	rawBrokers := strings.TrimSpace(os.Getenv("KAFKA_BROKERS"))
	if rawBrokers == "" {
		t.Skip("KAFKA_BROKERS is not set")
	}
	brokers := strings.Split(rawBrokers, ",")
	for index := range brokers {
		brokers[index] = strings.TrimSpace(brokers[index])
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, err := meteringworker.NewTCPConnector().Connect(ctx, brokers)
	if err != nil {
		t.Fatalf("event-bus Connect() error = %v", err)
	}
	if err := session.Close(context.Background()); err != nil {
		t.Fatalf("event-bus session Close() error = %v", err)
	}
}

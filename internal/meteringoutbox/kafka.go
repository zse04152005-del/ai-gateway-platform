package meteringoutbox

import (
	"context"
	"errors"
	"strings"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/zse04152005-del/ai-gateway-platform/internal/metering"
)

var (
	// ErrKafkaSinkInvalid means the producer or record contract is incomplete.
	ErrKafkaSinkInvalid = errors.New("usage event Kafka sink is invalid")
	// ErrKafkaUnavailable means the broker did not acknowledge the record within its context.
	ErrKafkaUnavailable = errors.New("usage event Kafka sink is unavailable")
)

// KafkaSink synchronously publishes one bounded UsageEvent record to Redpanda/Kafka.
type KafkaSink struct {
	client *kgo.Client
}

// NewKafkaSink creates an idempotent producer with all in-sync replica acknowledgements.
func NewKafkaSink(brokers []string) (*KafkaSink, error) {
	if len(brokers) == 0 {
		return nil, ErrKafkaSinkInvalid
	}
	seedBrokers := make([]string, len(brokers))
	for index, broker := range brokers {
		if strings.TrimSpace(broker) == "" || broker != strings.TrimSpace(broker) {
			return nil, ErrKafkaSinkInvalid
		}
		seedBrokers[index] = broker
	}
	client, err := kgo.NewClient(
		kgo.SeedBrokers(seedBrokers...),
		kgo.RequiredAcks(kgo.AllISRAcks()),
		kgo.MaxBufferedRecords(1000),
	)
	if err != nil {
		return nil, newRelayError(ErrKafkaSinkInvalid, err)
	}
	return &KafkaSink{client: client}, nil
}

// Publish waits only in the background Relay and preserves eventId as the Kafka key.
func (sink *KafkaSink) Publish(ctx context.Context, eventID string, payload []byte) error {
	if sink == nil || sink.client == nil || ctx == nil || eventID == "" || len(payload) == 0 {
		return ErrKafkaSinkInvalid
	}
	result := sink.client.ProduceSync(ctx, &kgo.Record{
		Topic: metering.UsageEventTopic,
		Key:   append([]byte(nil), eventID...),
		Value: append([]byte(nil), payload...),
	})
	if err := result.FirstErr(); err != nil {
		return newRelayError(ErrKafkaUnavailable, err)
	}
	return nil
}

// Close releases producer resources after the Relay has stopped.
func (sink *KafkaSink) Close() {
	if sink != nil && sink.client != nil {
		sink.client.Close()
	}
}

var _ Sink = (*KafkaSink)(nil)

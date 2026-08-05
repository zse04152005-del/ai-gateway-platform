package meteringconsumer

import (
	"context"
	"errors"
	"net"
	"regexp"
	"strings"
	"sync"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/zse04152005-del/ai-gateway-platform/internal/metering"
	"github.com/zse04152005-del/ai-gateway-platform/internal/meteringworker"
)

const (
	// DefaultConsumerGroup is the stable production offset and receipt identity.
	DefaultConsumerGroup = "ai-gateway-metering-v1"
	maximumPollRecords   = 100
	maximumFetchBytes    = 1024 * 1024
)

var (
	// ErrKafkaConsumerInvalid means the group, broker, or handler contract is unsafe.
	ErrKafkaConsumerInvalid = errors.New("metering Kafka consumer is invalid")
	// ErrKafkaConsumerUnavailable means fetch or offset acknowledgement failed.
	ErrKafkaConsumerUnavailable = errors.New("metering Kafka consumer is unavailable")
	// ErrKafkaRecordFailed means a record could not be durably processed.
	ErrKafkaRecordFailed = errors.New("metering Kafka record processing failed")

	kafkaGroupPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{2,127}$`)
)

// Handler durably handles one Kafka key and value before its offset is committed.
type Handler interface {
	Process(context.Context, string, []byte) (Result, error)
}

// KafkaOptions configures a stable consumer group and test-only assignment signal.
type KafkaOptions struct {
	ConsumerGroup        string
	StartAtEnd           bool
	OnPartitionsAssigned func()
	OnRecordCommitted    func()
}

// KafkaConnector builds bounded manual-commit franz-go consumer sessions.
type KafkaConnector struct {
	handler              Handler
	consumerGroup        string
	startAtEnd           bool
	onPartitionsAssigned func()
	onRecordCommitted    func()
}

// NewKafkaConnector validates the durable handler and consumer group.
func NewKafkaConnector(handler Handler, options KafkaOptions) (*KafkaConnector, error) {
	if handler == nil || !kafkaGroupPattern.MatchString(options.ConsumerGroup) {
		return nil, ErrKafkaConsumerInvalid
	}
	return &KafkaConnector{
		handler: handler, consumerGroup: options.ConsumerGroup,
		startAtEnd: options.StartAtEnd, onPartitionsAssigned: options.OnPartitionsAssigned,
		onRecordCommitted: options.OnRecordCommitted,
	}, nil
}

// Connect creates a Kafka protocol consumer. Broker reachability is retried by
// franz-go during Run so a temporary broker outage does not discard offsets.
func (connector *KafkaConnector) Connect(
	ctx context.Context,
	brokers []string,
) (meteringworker.Session, error) {
	if connector == nil || connector.handler == nil || ctx == nil || len(brokers) == 0 {
		return nil, ErrKafkaConsumerInvalid
	}
	seedBrokers := make([]string, len(brokers))
	for index, broker := range brokers {
		if broker != strings.TrimSpace(broker) {
			return nil, ErrKafkaConsumerInvalid
		}
		if _, _, err := net.SplitHostPort(broker); err != nil {
			return nil, newConsumerError(ErrKafkaConsumerInvalid, err)
		}
		seedBrokers[index] = broker
	}
	initialOffset := kgo.NewOffset().AtStart()
	if connector.startAtEnd {
		initialOffset = kgo.NewOffset().AtEnd()
	}
	var assignedOnce sync.Once
	client, err := kgo.NewClient(
		kgo.SeedBrokers(seedBrokers...),
		kgo.ConsumeTopics(metering.UsageEventTopic),
		kgo.ConsumerGroup(connector.consumerGroup),
		kgo.ConsumeResetOffset(initialOffset),
		kgo.DisableAutoCommit(),
		kgo.BlockRebalanceOnPoll(),
		kgo.FetchMaxBytes(maximumFetchBytes),
		kgo.FetchMaxPartitionBytes(maximumFetchBytes),
		kgo.OnPartitionsAssigned(func(context.Context, *kgo.Client, map[string][]int32) {
			if connector.onPartitionsAssigned != nil {
				assignedOnce.Do(connector.onPartitionsAssigned)
			}
		}),
	)
	if err != nil {
		return nil, newConsumerError(ErrKafkaConsumerInvalid, err)
	}
	return &kafkaSession{
		client: client, handler: connector.handler,
		onRecordCommitted: connector.onRecordCommitted,
	}, nil
}

type kafkaSession struct {
	client            *kgo.Client
	handler           Handler
	onRecordCommitted func()
}

func (session *kafkaSession) Run(ctx context.Context) error {
	if session == nil || session.client == nil || session.handler == nil || ctx == nil {
		return ErrKafkaConsumerInvalid
	}
	for ctx.Err() == nil {
		fetches := session.client.PollRecords(ctx, maximumPollRecords)
		if ctx.Err() != nil {
			session.client.AllowRebalance()
			continue
		}
		if fetchErrors := fetches.Errors(); len(fetchErrors) != 0 {
			session.client.AllowRebalance()
			causes := make([]error, 0, len(fetchErrors))
			for _, fetchError := range fetchErrors {
				causes = append(causes, fetchError.Err)
			}
			return newConsumerError(ErrKafkaConsumerUnavailable, errors.Join(causes...))
		}
		for _, record := range fetches.Records() {
			if record.Topic != metering.UsageEventTopic {
				session.client.AllowRebalance()
				return ErrKafkaRecordFailed
			}
			if _, err := session.handler.Process(ctx, string(record.Key), record.Value); err != nil {
				session.client.AllowRebalance()
				return newConsumerError(ErrKafkaRecordFailed, err)
			}
			if err := session.client.CommitRecords(ctx, record); err != nil {
				session.client.AllowRebalance()
				return newConsumerError(ErrKafkaConsumerUnavailable, err)
			}
			if session.onRecordCommitted != nil {
				session.onRecordCommitted()
			}
		}
		session.client.AllowRebalance()
	}
	return nil
}

func (session *kafkaSession) Close(context.Context) error {
	if session == nil || session.client == nil {
		return nil
	}
	session.client.Close()
	return nil
}

var (
	_ meteringworker.Connector = (*KafkaConnector)(nil)
	_ meteringworker.Session   = (*kafkaSession)(nil)
)

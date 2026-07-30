package openaiadapter

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/provideradapter"
)

const (
	maximumSSELineBytes  = 64 * 1024
	maximumSSEEventBytes = 256 * 1024
)

type streamEnvelope struct {
	ID                string            `json:"id"`
	Object            string            `json:"object"`
	Created           int64             `json:"created"`
	Model             string            `json:"model"`
	Choices           []json.RawMessage `json:"choices"`
	Usage             json.RawMessage   `json:"usage"`
	SystemFingerprint string            `json:"system_fingerprint"`
	ServiceTier       string            `json:"service_tier"`
	Moderation        json.RawMessage   `json:"moderation"`
	Obfuscation       string            `json:"obfuscation"`
}

type streamChoice struct {
	Index        int             `json:"index"`
	Delta        json.RawMessage `json:"delta"`
	FinishReason *string         `json:"finish_reason"`
	Logprobs     json.RawMessage `json:"logprobs"`
}

type streamDelta struct {
	Role         string            `json:"role"`
	Content      string            `json:"content"`
	Refusal      json.RawMessage   `json:"refusal"`
	FunctionCall json.RawMessage   `json:"function_call"`
	ToolCalls    []json.RawMessage `json:"tool_calls"`
}

type streamToolCall struct {
	Index    int             `json:"index"`
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Function json.RawMessage `json:"function"`
}

type streamToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type sseEvent struct {
	eventType string
	data      []byte
	heartbeat bool
}

type pendingFinish struct {
	choiceIndex    int
	reason         adapter.FinishReason
	providerReason string
}

type openAIStream struct {
	body   io.ReadCloser
	reader *bufio.Reader
	now    func() time.Time

	nextMu    sync.Mutex
	closeOnce sync.Once
	closed    atomic.Bool
	closeErr  error

	queue           []adapter.NormalizedChunk
	nextSequence    uint64
	pendingFinish   *pendingFinish
	pendingUsage    *adapter.NormalizedUsage
	terminalEmitted bool
	sawDone         bool
}

// OpenStream validates status and content type, then transfers body ownership
// to the bounded SSE parser.
func (openAI *openAIAdapter) OpenStream(
	ctx context.Context,
	response *http.Response,
) (provideradapter.ChunkStream, error) {
	if openAI == nil || openAI.now == nil {
		return nil, errors.New("openai adapter is not initialized")
	}
	if ctx == nil {
		closeResponse(response)
		return nil, errors.New("openai adapter stream context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		closeResponse(response)
		return nil, fmt.Errorf("openai adapter stream cancelled: %w", err)
	}
	if response == nil || response.Body == nil {
		return nil, protocolError("open_stream", "missing_response", nil)
	}
	if response.StatusCode != http.StatusOK {
		body, err := readBoundedBody(response.Body, maximumErrorBodyBytes)
		if err != nil {
			return nil, err
		}
		return nil, openAI.NormalizeError(ctx, response, body)
	}
	if err := requireMediaType(response.Header.Get("Content-Type"), "text/event-stream"); err != nil {
		_ = response.Body.Close()
		return nil, protocolError("open_stream", "invalid_content_type", err)
	}
	return &openAIStream{
		body: response.Body, reader: bufio.NewReaderSize(response.Body, maximumSSELineBytes), now: openAI.now,
	}, nil
}

// Next returns one validated normalized fact. Calls are serialized and context
// cancellation closes the response body to unblock a pending network read.
func (stream *openAIStream) Next(ctx context.Context) (adapter.NormalizedChunk, error) {
	if stream == nil {
		return adapter.NormalizedChunk{}, io.ErrClosedPipe
	}
	stream.nextMu.Lock()
	defer stream.nextMu.Unlock()
	if ctx == nil {
		return adapter.NormalizedChunk{}, errors.New("openai stream context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		_ = stream.Close()
		return adapter.NormalizedChunk{}, err
	}
	if len(stream.queue) > 0 {
		return stream.dequeue(), nil
	}
	if stream.sawDone {
		return adapter.NormalizedChunk{}, io.EOF
	}
	if stream.closed.Load() {
		return adapter.NormalizedChunk{}, io.ErrClosedPipe
	}

	for {
		event, err := stream.readEvent(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return adapter.NormalizedChunk{}, ctx.Err()
			}
			if errors.Is(err, io.EOF) {
				return adapter.NormalizedChunk{}, protocolError("read_stream", "unexpected_eof", err)
			}
			return adapter.NormalizedChunk{}, err
		}
		if event.heartbeat {
			if err := stream.enqueue(adapter.NormalizedChunk{
				Kind: adapter.ChunkHeartbeat, ChoiceIndex: 0, ProviderEventType: "comment",
			}); err != nil {
				return adapter.NormalizedChunk{}, err
			}
			return stream.dequeue(), nil
		}
		if bytes.Equal(bytes.TrimSpace(event.data), []byte("[DONE]")) {
			stream.sawDone = true
			if !stream.terminalEmitted {
				if stream.pendingFinish == nil {
					return adapter.NormalizedChunk{}, protocolError("read_stream", "done_without_finish", nil)
				}
				if err := stream.enqueueTerminal(); err != nil {
					return adapter.NormalizedChunk{}, err
				}
				return stream.dequeue(), nil
			}
			return adapter.NormalizedChunk{}, io.EOF
		}
		if err := stream.parseDataEvent(event); err != nil {
			return adapter.NormalizedChunk{}, err
		}
		if len(stream.queue) > 0 {
			return stream.dequeue(), nil
		}
	}
}

// Close is idempotent and releases the upstream connection.
func (stream *openAIStream) Close() error {
	if stream == nil {
		return nil
	}
	stream.closeOnce.Do(func() {
		stream.closed.Store(true)
		stream.closeErr = stream.body.Close()
	})
	return stream.closeErr
}

func (stream *openAIStream) readEvent(ctx context.Context) (sseEvent, error) {
	stopClose := context.AfterFunc(ctx, func() { _ = stream.Close() })
	defer stopClose()
	var data bytes.Buffer
	eventType := "message"
	heartbeat := false
	started := false
	for {
		line, isPrefix, err := stream.reader.ReadLine()
		if isPrefix {
			return sseEvent{}, protocolError("read_stream", "line_too_large", ErrResponseTooLarge)
		}
		if err != nil {
			if errors.Is(err, io.EOF) && started {
				return sseEvent{eventType: eventType, data: data.Bytes(), heartbeat: heartbeat && data.Len() == 0}, nil
			}
			return sseEvent{}, err
		}
		started = true
		if len(line) == 0 {
			if data.Len() == 0 && !heartbeat {
				started = false
				continue
			}
			return sseEvent{eventType: eventType, data: data.Bytes(), heartbeat: heartbeat && data.Len() == 0}, nil
		}
		if line[0] == ':' {
			heartbeat = true
			continue
		}
		field, value, found := bytes.Cut(line, []byte{':'})
		if !found {
			field, value = line, nil
		}
		value = bytes.TrimPrefix(value, []byte{' '})
		switch string(field) {
		case "data":
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			if data.Len()+len(value) > maximumSSEEventBytes {
				return sseEvent{}, protocolError("read_stream", "event_too_large", ErrResponseTooLarge)
			}
			data.Write(value)
		case "event":
			eventType = string(value)
		case "id", "retry":
		default:
			return sseEvent{}, protocolError("read_stream", "unknown_sse_field", nil)
		}
	}
}

func (stream *openAIStream) parseDataEvent(event sseEvent) error {
	if len(event.data) == 0 {
		return protocolError("parse_stream", "empty_data_event", nil)
	}
	if stream.terminalEmitted {
		return protocolError("parse_stream", "event_after_terminal", nil)
	}
	var envelope streamEnvelope
	if err := decodeOneJSON(event.data, &envelope); err != nil {
		return protocolError("parse_stream", "invalid_chunk_json", err)
	}
	if envelope.Object != "chat.completion.chunk" || envelope.Model == "" {
		return protocolError("parse_stream", "unexpected_chunk_identity", nil)
	}
	if hasValue(envelope.Moderation) {
		return protocolError("parse_stream", "moderation_chunk_unsupported", nil)
	}
	if len(envelope.Choices) > 1 {
		return protocolError("parse_stream", "multiple_choices_unsupported", nil)
	}
	unknown := hasUnknownStreamFields(event.data, envelope.Choices)
	for index := range envelope.Choices {
		finish, err := stream.parseStreamChoice(envelope.Choices[index], event.eventType)
		if err != nil {
			return err
		}
		if finish != nil {
			if stream.pendingFinish != nil {
				return protocolError("parse_stream", "duplicate_finish", nil)
			}
			stream.pendingFinish = finish
		}
	}
	if hasNonNullValue(envelope.Usage) {
		if stream.pendingUsage != nil {
			return protocolError("parse_stream", "duplicate_usage", nil)
		}
		usage, err := parseUsage(envelope.Usage, true)
		if err != nil {
			return err
		}
		stream.pendingUsage = &usage
	}
	if unknown {
		if err := stream.enqueue(adapter.NormalizedChunk{
			Kind: adapter.ChunkProviderExtension, ChoiceIndex: 0,
			ProviderEventType: "provider.unknown_fields",
			ProviderExtension: append(json.RawMessage(nil), event.data...),
		}); err != nil {
			return err
		}
	}
	if stream.pendingFinish != nil && stream.pendingUsage != nil && !stream.terminalEmitted {
		return stream.enqueueTerminal()
	}
	return nil
}

func (stream *openAIStream) parseStreamChoice(raw json.RawMessage, eventType string) (*pendingFinish, error) {
	var choice streamChoice
	if err := decodeOneJSON(raw, &choice); err != nil {
		return nil, protocolError("parse_stream", "invalid_stream_choice", err)
	}
	if hasValue(choice.Logprobs) {
		return nil, protocolError("parse_stream", "logprobs_unsupported", nil)
	}
	var delta streamDelta
	if err := decodeOneJSON(choice.Delta, &delta); err != nil {
		return nil, protocolError("parse_stream", "invalid_stream_delta", err)
	}
	if hasValue(delta.Refusal) || hasValue(delta.FunctionCall) {
		return nil, protocolError("parse_stream", "unsupported_stream_payload", nil)
	}
	if delta.Role != "" {
		if delta.Role != string(adapter.RoleAssistant) {
			return nil, protocolError("parse_stream", "unexpected_stream_role", nil)
		}
		if err := stream.enqueue(adapter.NormalizedChunk{
			Kind: adapter.ChunkMessageStart, ChoiceIndex: choice.Index,
			Role: adapter.RoleAssistant, ProviderEventType: eventType,
		}); err != nil {
			return nil, err
		}
	}
	if delta.Content != "" {
		if err := stream.enqueue(adapter.NormalizedChunk{
			Kind: adapter.ChunkContentDelta, ChoiceIndex: choice.Index,
			ContentDelta: delta.Content, ProviderEventType: eventType,
		}); err != nil {
			return nil, err
		}
	}
	for index := range delta.ToolCalls {
		toolDelta, err := parseStreamToolCall(delta.ToolCalls[index])
		if err != nil {
			return nil, err
		}
		if err := stream.enqueue(adapter.NormalizedChunk{
			Kind: adapter.ChunkToolDelta, ChoiceIndex: choice.Index,
			ToolDelta: &toolDelta, ProviderEventType: eventType,
		}); err != nil {
			return nil, err
		}
	}
	if choice.FinishReason == nil {
		return nil, nil
	}
	reason, providerReason := normalizeFinishReason(*choice.FinishReason)
	return &pendingFinish{choiceIndex: choice.Index, reason: reason, providerReason: providerReason}, nil
}

func parseStreamToolCall(raw json.RawMessage) (adapter.ToolCallDelta, error) {
	var call streamToolCall
	if err := decodeOneJSON(raw, &call); err != nil {
		return adapter.ToolCallDelta{}, protocolError("parse_stream", "invalid_stream_tool_call", err)
	}
	if call.Type != "" && call.Type != "function" {
		return adapter.ToolCallDelta{}, protocolError("parse_stream", "invalid_stream_tool_type", nil)
	}
	var function streamToolFunction
	if len(call.Function) > 0 {
		if err := decodeOneJSON(call.Function, &function); err != nil {
			return adapter.ToolCallDelta{}, protocolError("parse_stream", "invalid_stream_tool_function", err)
		}
	}
	return adapter.ToolCallDelta{
		Index: call.Index, ID: call.ID, Name: function.Name, ArgumentsFragment: function.Arguments,
	}, nil
}

func (stream *openAIStream) enqueueTerminal() error {
	finish := stream.pendingFinish
	status := adapter.UsageStatusMissing
	var usage *adapter.NormalizedUsage
	if stream.pendingUsage != nil {
		cloned := stream.pendingUsage.Clone()
		usage = &cloned
		if cloned.Complete {
			status = adapter.UsageStatusPresent
		} else {
			status = adapter.UsageStatusPartial
		}
	}
	if err := stream.enqueue(adapter.NormalizedChunk{
		Kind: adapter.ChunkMessageEnd, ChoiceIndex: finish.choiceIndex,
		FinishReason: finish.reason, ProviderFinishReason: finish.providerReason,
		Usage: usage, UsageStatus: status, ProviderEventType: "message",
	}); err != nil {
		return err
	}
	stream.terminalEmitted = true
	stream.pendingFinish = nil
	stream.pendingUsage = nil
	return nil
}

func (stream *openAIStream) enqueue(chunk adapter.NormalizedChunk) error {
	chunk.Sequence = stream.nextSequence
	chunk.ObservedAt = stream.now().UTC()
	if err := chunk.Validate(); err != nil {
		return protocolError("normalize_stream", "invalid_normalized_chunk", err)
	}
	stream.nextSequence++
	stream.queue = append(stream.queue, chunk)
	return nil
}

func (stream *openAIStream) dequeue() adapter.NormalizedChunk {
	chunk := stream.queue[0]
	stream.queue = stream.queue[1:]
	return chunk
}

func hasUnknownStreamFields(event []byte, choices []json.RawMessage) bool {
	if !onlyObjectFields(event, "id", "object", "created", "model", "choices", "usage", "system_fingerprint", "service_tier", "moderation", "obfuscation") {
		return true
	}
	for _, rawChoice := range choices {
		if !onlyObjectFields(rawChoice, "index", "delta", "finish_reason", "logprobs") {
			return true
		}
		var choice streamChoice
		if decodeOneJSON(rawChoice, &choice) != nil || !onlyObjectFields(choice.Delta, "role", "content", "refusal", "function_call", "tool_calls") {
			return true
		}
		var delta streamDelta
		if decodeOneJSON(choice.Delta, &delta) != nil {
			return true
		}
		for _, rawCall := range delta.ToolCalls {
			if !onlyObjectFields(rawCall, "index", "id", "type", "function") {
				return true
			}
			var call streamToolCall
			if decodeOneJSON(rawCall, &call) != nil || (len(call.Function) > 0 && !onlyObjectFields(call.Function, "name", "arguments")) {
				return true
			}
		}
	}
	return false
}

func closeResponse(response *http.Response) {
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
}

var _ provideradapter.ChunkStream = (*openAIStream)(nil)

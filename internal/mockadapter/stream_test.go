package mockadapter_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/mockadapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/mockprovider"
)

func TestOpenStreamParsesCompleteSSEFixture(t *testing.T) {
	t.Parallel()

	built, client, _ := newAdapterRuntime(t, mockprovider.NewHandler())
	response, ctx := execute(t, client, built, normalizedRequest(true, mockadapter.ScenarioSSE))
	defer func() { _ = response.Body.Close() }()
	stream, err := built.OpenStream(ctx, response)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer func() {
		if closeErr := stream.Close(); closeErr != nil {
			t.Errorf("close stream: %v", closeErr)
		}
	}()

	var chunks []adapter.NormalizedChunk
	for {
		chunk, nextErr := stream.Next(ctx)
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			t.Fatalf("next: %v", nextErr)
		}
		chunks = append(chunks, chunk)
	}
	wantKinds := []adapter.ChunkKind{
		adapter.ChunkMessageStart, adapter.ChunkContentDelta, adapter.ChunkContentDelta, adapter.ChunkMessageEnd,
	}
	if len(chunks) != len(wantKinds) {
		t.Fatalf("chunks = %d: %#v", len(chunks), chunks)
	}
	var content strings.Builder
	for index, chunk := range chunks {
		if chunk.Sequence != uint64(index) || chunk.Kind != wantKinds[index] || chunk.ObservedAt != fixedClock() {
			t.Fatalf("chunk[%d] = %#v", index, chunk)
		}
		content.WriteString(chunk.ContentDelta)
	}
	if content.String() != "deterministic mock response" {
		t.Fatalf("content = %q", content.String())
	}
	terminal := chunks[len(chunks)-1]
	if terminal.FinishReason != adapter.FinishStop || terminal.UsageStatus != adapter.UsageStatusPresent ||
		terminal.Usage == nil || terminal.Usage.InputTokens != adapter.Tokens(6) || terminal.Usage.OutputTokens != adapter.Tokens(4) {
		t.Fatalf("terminal = %#v", terminal)
	}
	if _, err := stream.Next(ctx); !errors.Is(err, io.EOF) {
		t.Fatalf("next after done = %v", err)
	}
}

func TestOpenStreamSurfacesMalformedChunkAfterPartialSuccess(t *testing.T) {
	t.Parallel()

	built, client, _ := newAdapterRuntime(t, mockprovider.NewHandler())
	response, ctx := execute(t, client, built, normalizedRequest(true, mockadapter.ScenarioMalformedChunk))
	defer func() { _ = response.Body.Close() }()
	stream, err := built.OpenStream(ctx, response)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer func() { _ = stream.Close() }()
	first, err := stream.Next(ctx)
	if err != nil || first.Kind != adapter.ChunkMessageStart {
		t.Fatalf("first = %#v err=%v", first, err)
	}
	_, err = stream.Next(ctx)
	if !errors.Is(err, mockadapter.ErrProtocol) {
		t.Fatalf("malformed error = %v", err)
	}
	if strings.Contains(err.Error(), "chatcmpl") || strings.Contains(err.Error(), "choices") {
		t.Fatalf("protocol error leaked raw chunk: %s", err)
	}
}

func TestOpenStreamNormalizesHTTPErrorBeforeParser(t *testing.T) {
	t.Parallel()

	built, client, _ := newAdapterRuntime(t, mockprovider.NewHandler())
	response, ctx := execute(t, client, built, normalizedRequest(true, mockadapter.ScenarioRateLimit))
	defer func() { _ = response.Body.Close() }()
	_, err := built.OpenStream(ctx, response)
	var normalized adapter.NormalizedError
	if !errors.As(err, &normalized) || normalized.Category != adapter.ErrorRateLimit || normalized.RetryAfter == nil {
		t.Fatalf("open stream error = %#v (%v)", normalized, err)
	}
}

func TestOpenStreamIsolatesUnknownProviderEvent(t *testing.T) {
	t.Parallel()

	handler := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"model\":\"mock-chat-v1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"future_delta\":7},\"finish_reason\":null}],\"future_top\":true}\n\n")
		_, _ = io.WriteString(writer, "data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"model\":\"mock-chat-v1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n")
		_, _ = io.WriteString(writer, "data: [DONE]\n\n")
	})
	built, client, _ := newAdapterRuntime(t, handler)
	response, ctx := execute(t, client, built, normalizedRequest(true, ""))
	defer func() { _ = response.Body.Close() }()
	stream, err := built.OpenStream(ctx, response)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = stream.Close() }()
	start, err := stream.Next(ctx)
	if err != nil || start.Kind != adapter.ChunkMessageStart {
		t.Fatalf("start = %#v err=%v", start, err)
	}
	extension, err := stream.Next(ctx)
	if err != nil || extension.Kind != adapter.ChunkProviderExtension ||
		!strings.Contains(string(extension.ProviderExtension), "future_delta") ||
		!strings.Contains(string(extension.ProviderExtension), "future_top") {
		t.Fatalf("extension = %#v err=%v", extension, err)
	}
	terminal, err := stream.Next(ctx)
	if err != nil || terminal.Kind != adapter.ChunkMessageEnd || terminal.UsageStatus != adapter.UsageStatusPresent {
		t.Fatalf("terminal = %#v err=%v", terminal, err)
	}
	if _, err := stream.Next(ctx); !errors.Is(err, io.EOF) {
		t.Fatalf("done = %v", err)
	}
}

func TestOpenStreamCancellationClosesBlockedBody(t *testing.T) {
	t.Parallel()

	requestCancelled := make(chan struct{})
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		if flusher, ok := writer.(http.Flusher); ok {
			flusher.Flush()
		}
		<-request.Context().Done()
		close(requestCancelled)
	})
	built, client, _ := newAdapterRuntime(t, handler)
	response, requestCtx := execute(t, client, built, normalizedRequest(true, ""))
	defer func() { _ = response.Body.Close() }()
	stream, err := built.OpenStream(requestCtx, response)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	nextCtx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, nextErr := stream.Next(nextCtx)
		result <- nextErr
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case nextErr := <-result:
		if !errors.Is(nextErr, context.Canceled) {
			t.Fatalf("next error = %v", nextErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Next did not unblock after cancellation")
	}
	select {
	case <-requestCancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream request context was not cancelled by Body close")
	}
	if _, err := stream.Next(context.Background()); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("next after close = %v", err)
	}
}

func TestOpenStreamRejectsMissingDoneAndOversizeLine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		handler http.Handler
		want    error
	}{
		{
			"missing done",
			http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(writer, "data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"model\":\"mock-chat-v1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n")
			}),
			mockadapter.ErrProtocol,
		},
		{
			"oversize line",
			http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(writer, "data: %s\n\n", strings.Repeat("x", 70*1024))
			}),
			mockadapter.ErrResponseTooLarge,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			built, client, _ := newAdapterRuntime(t, test.handler)
			response, ctx := execute(t, client, built, normalizedRequest(true, ""))
			defer func() { _ = response.Body.Close() }()
			stream, err := built.OpenStream(ctx, response)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer func() { _ = stream.Close() }()
			for {
				_, err = stream.Next(ctx)
				if err != nil {
					break
				}
			}
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestOpenStreamParsesHeartbeatReasoningAndToolDelta(t *testing.T) {
	t.Parallel()

	handler := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, ": keepalive\n\n")
		_, _ = io.WriteString(writer, "data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"model\":\"mock-chat-v1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"reasoning_content\":\"classified\",\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\\\"q\\\":\"}}]},\"finish_reason\":null}]}\n\n")
		_, _ = io.WriteString(writer, "data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"model\":\"mock-chat-v1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\n")
		_, _ = io.WriteString(writer, "data: [DONE]\n\n")
	})
	built, client, _ := newAdapterRuntime(t, handler)
	response, ctx := execute(t, client, built, normalizedRequest(true, ""))
	defer func() { _ = response.Body.Close() }()
	stream, err := built.OpenStream(ctx, response)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = stream.Close() }()
	want := []adapter.ChunkKind{
		adapter.ChunkHeartbeat, adapter.ChunkMessageStart, adapter.ChunkReasoningDelta,
		adapter.ChunkToolDelta, adapter.ChunkMessageEnd,
	}
	chunks := make([]adapter.NormalizedChunk, 0, len(want))
	for {
		chunk, nextErr := stream.Next(ctx)
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			t.Fatalf("next: %v", nextErr)
		}
		chunks = append(chunks, chunk)
	}
	if len(chunks) != len(want) {
		t.Fatalf("chunks = %#v", chunks)
	}
	for index := range want {
		if chunks[index].Kind != want[index] {
			t.Fatalf("chunk[%d] kind = %q, want %q", index, chunks[index].Kind, want[index])
		}
	}
	if chunks[2].ReasoningDelta != "classified" || chunks[3].ToolDelta == nil ||
		chunks[3].ToolDelta.Name != "lookup" || chunks[3].ToolDelta.ArgumentsFragment != `{"q":` {
		t.Fatalf("reasoning/tool chunks = %#v / %#v", chunks[2], chunks[3])
	}
	if chunks[4].FinishReason != adapter.FinishToolCalls || chunks[4].Usage.InputTokens != adapter.Tokens(3) {
		t.Fatalf("terminal = %#v", chunks[4])
	}
}

func TestOpenStreamRejectsInvalidContentTypeUnknownFieldAndCancelledContext(t *testing.T) {
	t.Parallel()

	built, _, _ := newAdapterRuntime(t, mockprovider.NewHandler())
	body := &trackedBody{Reader: strings.NewReader("ignored")}
	response := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/plain"}}, Body: body}
	if _, err := built.OpenStream(context.Background(), response); !errors.Is(err, mockadapter.ErrProtocol) {
		t.Fatalf("content type error = %v", err)
	}
	if !body.closed {
		t.Fatal("invalid content type did not close Body")
	}

	cancelledBody := &trackedBody{Reader: strings.NewReader("")}
	cancelledResponse := &http.Response{StatusCode: 200, Header: make(http.Header), Body: cancelledBody}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := built.OpenStream(cancelled, cancelledResponse); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled open error = %v", err)
	}
	if !cancelledBody.closed {
		t.Fatal("cancelled OpenStream did not close Body")
	}

	handler := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "vendor: value\n\n")
	})
	streamAdapter, client, _ := newAdapterRuntime(t, handler)
	httpResponse, ctx := execute(t, client, streamAdapter, normalizedRequest(true, ""))
	defer func() { _ = httpResponse.Body.Close() }()
	stream, err := streamAdapter.OpenStream(ctx, httpResponse)
	if err != nil {
		t.Fatalf("open unknown field stream: %v", err)
	}
	defer func() { _ = stream.Close() }()
	if _, err := stream.Next(ctx); !errors.Is(err, mockadapter.ErrProtocol) {
		t.Fatalf("unknown SSE field error = %v", err)
	}
}

func TestDelayScenarioWorksForOrdinaryAndStreamRequests(t *testing.T) {
	t.Parallel()

	built, client, _ := newAdapterRuntime(t, mockprovider.NewHandler())
	ordinary := normalizedRequest(false, "")
	ordinary.ProviderOptions = []byte(`{"mock_scenario":"delay","mock_delay_ms":1}`)
	response, ctx := execute(t, client, built, ordinary)
	defer func() { _ = response.Body.Close() }()
	if _, err := built.ParseResponse(ctx, response); err != nil {
		t.Fatalf("parse delayed ordinary response: %v", err)
	}

	streamRequest := normalizedRequest(true, "")
	streamRequest.ProviderOptions = []byte(`{"mock_scenario":"delay","mock_delay_ms":1}`)
	streamResponse, streamCtx := execute(t, client, built, streamRequest)
	defer func() { _ = streamResponse.Body.Close() }()
	stream, err := built.OpenStream(streamCtx, streamResponse)
	if err != nil {
		t.Fatalf("open delayed stream: %v", err)
	}
	defer func() { _ = stream.Close() }()
	for {
		_, err = stream.Next(streamCtx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read delayed stream: %v", err)
		}
	}
}

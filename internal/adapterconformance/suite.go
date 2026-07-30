package adapterconformance

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
)

// Run validates registration completeness and executes the same real-HTTP
// behavior matrix for one adapter. Every subtest owns a fresh server and adapter.
func Run(t *testing.T, registration Registration) {
	t.Helper()
	if err := registration.Validate(); err != nil {
		t.Fatalf("validate adapter conformance registration: %v", err)
	}
	t.Run(registration.Name, func(t *testing.T) {
		t.Run("ordinary", func(t *testing.T) {
			runResponseFixture(t, registration.NewAdapter, registration.Fixtures.Ordinary)
		})
		t.Run("stream", func(t *testing.T) {
			runStreamFixture(t, registration.NewAdapter, registration.Fixtures.Stream)
		})
		t.Run("cancellation", func(t *testing.T) {
			runCancellationFixture(t, registration.NewAdapter, registration.Fixtures.Cancellation)
		})
		t.Run("errors", func(t *testing.T) {
			for _, fixture := range []ErrorFixture{
				registration.Fixtures.RateLimit,
				registration.Fixtures.ProviderFailure,
			} {
				fixture := fixture
				t.Run(fixture.Name, func(t *testing.T) {
					runErrorFixture(t, registration.NewAdapter, fixture)
				})
			}
		})
		t.Run("cached_usage", func(t *testing.T) {
			runResponseFixture(t, registration.NewAdapter, registration.Fixtures.CachedUsage)
		})
		t.Run("tool_call", func(t *testing.T) {
			runResponseFixture(t, registration.NewAdapter, registration.Fixtures.ToolCall)
		})
		t.Run("finish_reasons", func(t *testing.T) {
			for _, fixture := range registration.Fixtures.FinishReasons {
				fixture := fixture
				t.Run(fixture.Name, func(t *testing.T) {
					runResponseFixture(t, registration.NewAdapter, fixture)
				})
			}
		})
		t.Run("unknown_fields", func(t *testing.T) {
			t.Run("ordinary_rejected", func(t *testing.T) {
				runProtocolErrorFixture(t, registration.NewAdapter, registration.Fixtures.UnknownOrdinary)
			})
			t.Run("stream_isolated", func(t *testing.T) {
				runStreamFixture(t, registration.NewAdapter, registration.Fixtures.UnknownStream)
			})
		})
	})
}

func runResponseFixture(t *testing.T, build AdapterBuilder, fixture ResponseFixture) {
	t.Helper()
	rt := newRuntime(t, build, fixture.NewHandler())
	response, ctx := rt.execute(t, fixture.Request.Clone())
	defer func() { _ = response.Body.Close() }()
	got, err := rt.adapter.ParseResponse(ctx, response)
	if err != nil {
		t.Fatalf("parse ordinary fixture response: %v", err)
	}
	assertResponse(t, got, fixture.Want)
}

func runStreamFixture(t *testing.T, build AdapterBuilder, fixture StreamFixture) {
	t.Helper()
	rt := newRuntime(t, build, fixture.NewHandler())
	response, ctx := rt.execute(t, fixture.Request.Clone())
	defer func() { _ = response.Body.Close() }()
	stream, err := rt.adapter.OpenStream(ctx, response)
	if err != nil {
		t.Fatalf("open fixture stream: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := stream.Close(); closeErr != nil {
			t.Errorf("close fixture stream: %v", closeErr)
		}
	})
	chunks := make([]adapter.NormalizedChunk, 0, len(fixture.Want))
	for {
		chunk, nextErr := stream.Next(ctx)
		switch {
		case nextErr == nil:
			chunks = append(chunks, chunk)
		case errors.Is(nextErr, io.EOF):
			assertChunks(t, chunks, fixture.Want)
			if _, afterErr := stream.Next(ctx); !errors.Is(afterErr, io.EOF) {
				t.Fatalf("stream Next after terminal EOF = %v", afterErr)
			}
			return
		default:
			t.Fatal(unexpectedStreamTermination(nextErr))
		}
	}
}

func runErrorFixture(t *testing.T, build AdapterBuilder, fixture ErrorFixture) {
	t.Helper()
	rt := newRuntime(t, build, fixture.NewHandler())
	response, ctx := rt.execute(t, fixture.Request.Clone())
	defer func() { _ = response.Body.Close() }()
	_, err := rt.adapter.ParseResponse(ctx, response)
	if err == nil {
		t.Fatal("provider error fixture was accepted as a successful response")
	}
	assertNormalizedError(t, err, fixture.Want, fixture.ForbiddenText)
}

func runProtocolErrorFixture(t *testing.T, build AdapterBuilder, fixture ProtocolErrorFixture) {
	t.Helper()
	rt := newRuntime(t, build, fixture.NewHandler())
	response, ctx := rt.execute(t, fixture.Request.Clone())
	defer func() { _ = response.Body.Close() }()
	_, err := rt.adapter.ParseResponse(ctx, response)
	if !errors.Is(err, fixture.Want) {
		t.Fatalf("protocol error = %v, want errors.Is(_, %v)", err, fixture.Want)
	}
	assertForbidden(t, err.Error(), fixture.ForbiddenText)
}

func runCancellationFixture(t *testing.T, build AdapterBuilder, fixture CancellationFixture) {
	t.Helper()
	upstreamCancelled := make(chan struct{})
	rt := newRuntime(t, build, fixture.NewHandler(upstreamCancelled))
	response, requestCtx := rt.execute(t, fixture.Request.Clone())
	defer func() { _ = response.Body.Close() }()
	stream, err := rt.adapter.OpenStream(requestCtx, response)
	if err != nil {
		t.Fatalf("open cancellation fixture stream: %v", err)
	}
	t.Cleanup(func() { _ = stream.Close() })

	nextCtx, cancelNext := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, nextErr := stream.Next(nextCtx)
		result <- nextErr
	}()
	cancelNext()
	select {
	case nextErr := <-result:
		if !errors.Is(nextErr, context.Canceled) {
			t.Fatalf("blocked Next cancellation error = %v", nextErr)
		}
	case <-time.After(fixtureTimeout):
		t.Fatal("blocked Next did not return after context cancellation")
	}
	select {
	case <-upstreamCancelled:
	case <-time.After(fixtureTimeout):
		t.Fatal("stream cancellation did not reach the upstream HTTP request context")
	}

	afterCtx, cancelAfter := context.WithTimeout(context.Background(), time.Second)
	defer cancelAfter()
	if _, afterErr := stream.Next(afterCtx); afterErr == nil {
		t.Fatal("closed stream accepted another Next call")
	}
}

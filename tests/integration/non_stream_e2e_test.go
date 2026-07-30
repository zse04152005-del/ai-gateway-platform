//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/apierror"
	"github.com/zse04152005-del/ai-gateway-platform/internal/catalog"
	"github.com/zse04152005-del/ai-gateway-platform/internal/execution"
	"github.com/zse04152005-del/ai-gateway-platform/internal/gateway"
	"github.com/zse04152005-del/ai-gateway-platform/internal/httpserver"
	"github.com/zse04152005-del/ai-gateway-platform/internal/keyauth"
	"github.com/zse04152005-del/ai-gateway-platform/internal/mockadapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/mockprovider"
	"github.com/zse04152005-del/ai-gateway-platform/internal/provideradapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/proxy"
	"github.com/zse04152005-del/ai-gateway-platform/internal/routing"
	"github.com/zse04152005-del/ai-gateway-platform/internal/upstreamhttp"
	"github.com/zse04152005-del/ai-gateway-platform/internal/virtualkey"
)

const (
	nonStreamTenantID     = "15000000-0000-4000-8000-000000000001"
	nonStreamProjectID    = "25000000-0000-4000-8000-000000000001"
	nonStreamProviderID   = "45000000-0000-4000-8000-000000000001"
	nonStreamModelID      = "55000000-0000-4000-8000-000000000001"
	nonStreamDeploymentID = "65000000-0000-4000-8000-000000000001"
	nonStreamModel        = "e2e-chat"
	nonStreamUnknownModel = "e2e-unknown"
	nonStreamRequestRoot  = "e2e-nonstream-"
)

func TestNonStreamEndToEnd(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL is not set")
	}
	database, err := sql.Open("postgres", databaseURL)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	database.SetMaxOpenConns(8)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := database.PingContext(ctx); err != nil {
		t.Fatalf("database.PingContext() error = %v", err)
	}

	cleanupNonStreamFixtures(t, database)
	t.Cleanup(func() { cleanupNonStreamFixtures(t, database) })

	providerControl := newControlledMockProvider(mockprovider.NewHandler())
	providerServer := httptest.NewServer(providerControl)
	t.Cleanup(providerServer.Close)
	seedNonStreamFixtures(ctx, t, database, providerServer.URL)

	credential := buildNonStreamGateway(t, database)

	t.Run("success traverses the complete HTTP and persistence chain", func(t *testing.T) {
		providerControl.selectScenario(mockadapter.ScenarioNormal, 0)
		requestID := nonStreamRequestRoot + "success"
		status, body := performNonStreamRequest(t, credential.gatewayURL, credential.value, requestID, nonStreamModel)
		assertE2EStatusAndCode(t, status, body, http.StatusOK, "")
		var response struct {
			Model   string `json:"model"`
			Gateway struct {
				RequestID     string `json:"request_id"`
				AttemptCount  int    `json:"attempt_count"`
				UsageComplete bool   `json:"usage_complete"`
			} `json:"gateway"`
		}
		if err := json.Unmarshal(body, &response); err != nil {
			t.Fatalf("decode success response: %v", err)
		}
		if response.Model != nonStreamModel || response.Gateway.RequestID != requestID ||
			response.Gateway.AttemptCount != 1 || !response.Gateway.UsageComplete {
			t.Fatalf("success response = %+v", response)
		}
		assertNonStreamExecution(t, database, requestID, execution.RequestSucceeded, "completed", 1,
			execution.AttemptSucceeded, "completed", "", "", true)
	})

	t.Run("authentication failure never creates an execution record", func(t *testing.T) {
		requestID := nonStreamRequestRoot + "authentication"
		status, body := performNonStreamRequest(t, credential.gatewayURL, "invalid-credential", requestID, nonStreamModel)
		assertE2EStatusAndCode(t, status, body, http.StatusUnauthorized, "INVALID_API_KEY")
		assertNoGatewayRequest(t, database, requestID)
	})

	t.Run("unknown model is an auditable routing failure", func(t *testing.T) {
		requestID := nonStreamRequestRoot + "unknown-model"
		status, body := performNonStreamRequest(t, credential.gatewayURL, credential.value, requestID, nonStreamUnknownModel)
		assertE2EStatusAndCode(t, status, body, http.StatusServiceUnavailable, "MODEL_UNAVAILABLE")
		assertNonStreamExecution(t, database, requestID, execution.RequestFailed, "model_unavailable", 0,
			"", "", "", "", false)
	})

	t.Run("upstream timeout is retryable and has no received headers", func(t *testing.T) {
		providerControl.selectScenario(mockadapter.ScenarioDelay, 1000)
		requestID := nonStreamRequestRoot + "timeout"
		status, body := performNonStreamRequest(t, credential.gatewayURL, credential.value, requestID, nonStreamModel)
		assertE2EStatusAndCode(t, status, body, http.StatusGatewayTimeout, "PROVIDER_TIMEOUT")
		assertNonStreamExecution(t, database, requestID, execution.RequestFailed, "provider_timeout", 1,
			execution.AttemptRetryableFailed, "provider_timeout", "timeout", "PROVIDER_TIMEOUT", false)
		providerControl.waitReleased(t)
	})

	t.Run("provider 429 remains a safe rate limit failure", func(t *testing.T) {
		providerControl.selectScenario(mockadapter.ScenarioRateLimit, 0)
		requestID := nonStreamRequestRoot + "rate-limit"
		status, body := performNonStreamRequest(t, credential.gatewayURL, credential.value, requestID, nonStreamModel)
		assertE2EStatusAndCode(t, status, body, http.StatusTooManyRequests, "PROVIDER_RATE_LIMITED")
		assertNonStreamExecution(t, database, requestID, execution.RequestFailed, "provider_rate_limit", 1,
			execution.AttemptRetryableFailed, "provider_rate_limit", "rate_limit", "MOCK_RATE_LIMITED", true)
	})

	t.Run("provider 5xx remains a retryable availability failure", func(t *testing.T) {
		providerControl.selectScenario(mockadapter.ScenarioServerError, 0)
		requestID := nonStreamRequestRoot + "server-error"
		status, body := performNonStreamRequest(t, credential.gatewayURL, credential.value, requestID, nonStreamModel)
		assertE2EStatusAndCode(t, status, body, http.StatusServiceUnavailable, "PROVIDER_UNAVAILABLE")
		assertNonStreamExecution(t, database, requestID, execution.RequestFailed, "provider_capacity", 1,
			execution.AttemptRetryableFailed, "provider_capacity", "capacity", "MOCK_PROVIDER_UNAVAILABLE", true)
	})

	t.Run("client cancellation releases the real provider and persists cancellation", func(t *testing.T) {
		providerControl.selectScenario(mockadapter.ScenarioDelay, 1000)
		requestID := nonStreamRequestRoot + "client-cancel"
		requestContext, requestCancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() {
			request, requestErr := newNonStreamHTTPRequest(
				requestContext, credential.gatewayURL, credential.value, requestID, nonStreamModel,
			)
			if requestErr != nil {
				result <- requestErr
				return
			}
			// #nosec G704 -- the URL is owned by the in-process httptest Gateway.
			response, requestErr := http.DefaultClient.Do(request)
			if response != nil {
				_ = response.Body.Close()
			}
			result <- requestErr
		}()
		providerControl.waitStarted(t)
		requestCancel()
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled client error = %v, want context.Canceled", err)
		}
		providerControl.waitReleased(t)
		waitForNonStreamExecution(t, database, requestID, execution.RequestCancelled, execution.AttemptCancelled)
		assertNonStreamExecution(t, database, requestID, execution.RequestCancelled, "client_cancelled", 1,
			execution.AttemptCancelled, "client_cancelled", "cancelled", "CLIENT_CANCELLED", false)
	})
}

type nonStreamCredential struct {
	value      string
	gatewayURL string
}

func buildNonStreamGateway(t *testing.T, database *sql.DB) nonStreamCredential {
	t.Helper()
	digestKey := bytes.Repeat([]byte{0x4d}, 32)
	digester, err := virtualkey.NewHMACDigester("non-stream-e2e-v1", digestKey)
	if err != nil {
		t.Fatalf("virtualkey.NewHMACDigester() error = %v", err)
	}
	keyStore, err := virtualkey.NewPostgresStore(database)
	if err != nil {
		t.Fatalf("virtualkey.NewPostgresStore() error = %v", err)
	}
	manager, err := virtualkey.NewManager(keyStore, digester, rand.Reader, time.Now)
	if err != nil {
		t.Fatalf("virtualkey.NewManager() error = %v", err)
	}
	issued, err := manager.Create(context.Background(), virtualkey.CreateCommand{
		TenantID: nonStreamTenantID, ProjectID: nonStreamProjectID, Mode: "test", Actor: "integration:non-stream-e2e",
	})
	if err != nil {
		t.Fatalf("create non-stream credential: %v", err)
	}

	authStore, err := keyauth.NewPostgresStore(database)
	if err != nil {
		t.Fatalf("keyauth.NewPostgresStore() error = %v", err)
	}
	keyring, err := keyauth.NewKeyring("non-stream-e2e-v1", digestKey, nil)
	if err != nil {
		t.Fatalf("keyauth.NewKeyring() error = %v", err)
	}
	cache, err := keyauth.NewMemoryCache(time.Minute, 32, time.Now)
	if err != nil {
		t.Fatalf("keyauth.NewMemoryCache() error = %v", err)
	}
	authenticator, err := keyauth.NewAuthenticator(authStore, keyring, cache, time.Now)
	if err != nil {
		t.Fatalf("keyauth.NewAuthenticator() error = %v", err)
	}
	catalogStore, err := catalog.NewPostgresStore(database)
	if err != nil {
		t.Fatalf("catalog.NewPostgresStore() error = %v", err)
	}
	selector, err := routing.NewSelector(catalogStore, routing.ActiveCatalogHealth{})
	if err != nil {
		t.Fatalf("routing.NewSelector() error = %v", err)
	}
	mockFactory, err := mockadapter.NewFactory(mockadapter.FactoryOptions{Now: time.Now})
	if err != nil {
		t.Fatalf("mockadapter.NewFactory() error = %v", err)
	}
	registry, err := provideradapter.NewRegistry(mockFactory)
	if err != nil {
		t.Fatalf("provideradapter.NewRegistry() error = %v", err)
	}
	upstreamClient, err := upstreamhttp.NewClient(upstreamhttp.Options{
		ConnectTimeout: 2 * time.Second, KeepAlive: 30 * time.Second,
		TLSHandshakeTimeout: 2 * time.Second, ResponseHeaderTimeout: 250 * time.Millisecond,
		TotalTimeout: 250 * time.Millisecond, IdleConnTimeout: time.Minute,
		ExpectContinueTimeout: time.Second, MaxIdleConns: 16, MaxIdleConnsPerHost: 8,
		MaxConnsPerHost: 16, MaxResponseHeaderBytes: 64 * 1024,
	})
	if err != nil {
		t.Fatalf("upstreamhttp.NewClient() error = %v", err)
	}
	t.Cleanup(upstreamClient.CloseIdleConnections)
	executor, err := proxy.NewNonStreamExecutor(registry, upstreamClient)
	if err != nil {
		t.Fatalf("proxy.NewNonStreamExecutor() error = %v", err)
	}
	recorder, err := execution.NewPostgresRecorder(database, time.Now, rand.Reader)
	if err != nil {
		t.Fatalf("execution.NewPostgresRecorder() error = %v", err)
	}
	application, err := gateway.NewExecutableHandler(authenticator, catalogStore, selector, executor, recorder)
	if err != nil {
		t.Fatalf("gateway.NewExecutableHandler() error = %v", err)
	}
	server, err := httpserver.NewServer(httpserver.Options{
		ServiceName: "gateway-e2e", Version: "test", NotReadyCode: "GATEWAY_NOT_READY",
		NotReadyMessage: "Gateway is not ready", ErrorType: "gateway_error",
		ReadHeaderTimeout: 2 * time.Second, ShutdownTimeout: 2 * time.Second,
		ApplicationHandler: application,
	})
	if err != nil {
		t.Fatalf("httpserver.NewServer() error = %v", err)
	}
	gatewayServer := httptest.NewServer(server.Handler())
	t.Cleanup(gatewayServer.Close)
	return nonStreamCredential{value: issued.Credential, gatewayURL: gatewayServer.URL}
}

type controlledMockProvider struct {
	downstream http.Handler
	mu         sync.RWMutex
	scenario   string
	delayMS    int
	started    chan struct{}
	released   chan struct{}
}

func newControlledMockProvider(downstream http.Handler) *controlledMockProvider {
	return &controlledMockProvider{downstream: downstream}
}

func (provider *controlledMockProvider) selectScenario(scenario string, delayMS int) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	provider.scenario = scenario
	provider.delayMS = delayMS
	provider.started = make(chan struct{})
	provider.released = make(chan struct{})
}

func (provider *controlledMockProvider) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	provider.mu.RLock()
	scenario, delayMS := provider.scenario, provider.delayMS
	started, released := provider.started, provider.released
	provider.mu.RUnlock()
	signal(started)
	defer signal(released)

	encoded, err := io.ReadAll(io.LimitReader(request.Body, 1<<20))
	if err != nil {
		http.Error(writer, "mock request read failed", http.StatusBadRequest)
		return
	}
	_ = request.Body.Close()
	var body map[string]any
	if err := json.Unmarshal(encoded, &body); err != nil {
		http.Error(writer, "mock request decode failed", http.StatusBadRequest)
		return
	}
	body["mock_scenario"] = scenario
	if scenario == mockadapter.ScenarioDelay {
		body["mock_delay_ms"] = delayMS
	}
	encoded, err = json.Marshal(body)
	if err != nil {
		http.Error(writer, "mock request encode failed", http.StatusInternalServerError)
		return
	}
	request.Body = io.NopCloser(bytes.NewReader(encoded))
	request.ContentLength = int64(len(encoded))
	provider.downstream.ServeHTTP(writer, request)
}

func (provider *controlledMockProvider) waitStarted(t *testing.T) {
	t.Helper()
	provider.mu.RLock()
	started := provider.started
	provider.mu.RUnlock()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("mock provider did not receive the request")
	}
}

func (provider *controlledMockProvider) waitReleased(t *testing.T) {
	t.Helper()
	provider.mu.RLock()
	released := provider.released
	provider.mu.RUnlock()
	select {
	case <-released:
	case <-time.After(2 * time.Second):
		t.Fatal("mock provider handler was not released")
	}
}

func signal(channel chan struct{}) {
	if channel == nil {
		return
	}
	select {
	case <-channel:
	default:
		close(channel)
	}
}

func seedNonStreamFixtures(ctx context.Context, t *testing.T, database *sql.DB, endpointURL string) {
	t.Helper()
	insertTenant(ctx, t, database, nonStreamTenantID, "non-stream-e2e", "Non-stream E2E", "")
	insertProject(ctx, t, database, nonStreamProjectID, nonStreamTenantID, "non-stream-e2e", "Non-stream E2E", "")
	statements := []struct {
		name  string
		query string
		args  []any
	}{
		{name: "provider", query: `INSERT INTO app.providers
			(id, code, name, adapter_type, status, created_by, updated_by)
			VALUES ($1, 'non-stream-mock', 'Non-stream Mock', 'mock', 'active', 'integration:e2e', 'integration:e2e')`,
			args: []any{nonStreamProviderID}},
		{name: "logical model", query: `INSERT INTO app.logical_models
			(id, tenant_id, name, display_name, required_capabilities, status, created_by, updated_by)
			VALUES ($1, $2, $3, 'E2E Chat', '{"chat":true}', 'active', 'integration:e2e', 'integration:e2e')`,
			args: []any{nonStreamModelID, nonStreamTenantID, nonStreamModel}},
		{name: "deployment", query: `INSERT INTO app.deployments
			(id, provider_id, code, physical_model, endpoint_url, region, capabilities, status, created_by, updated_by)
			VALUES ($1, $2, 'non-stream-mock', 'mock-physical', $3, 'local',
			'{"chat":true,"stream":true,"tools":true,"max_context_tokens":128000,"max_output_tokens":8192,"data_retention_mode":"zero_retention","provider_protocol_version":"v1"}',
			'active', 'integration:e2e', 'integration:e2e')`,
			args: []any{nonStreamDeploymentID, nonStreamProviderID, endpointURL}},
		{name: "binding", query: `INSERT INTO app.logical_model_deployments
			(logical_model_id, deployment_id, priority, weight, status, created_by, updated_by)
			VALUES ($1, $2, 10, 100, 'active', 'integration:e2e', 'integration:e2e')`,
			args: []any{nonStreamModelID, nonStreamDeploymentID}},
		{name: "project grant", query: `INSERT INTO app.project_logical_models
			(tenant_id, project_id, logical_model_id, created_by, updated_by)
			VALUES ($1, $2, $3, 'integration:e2e', 'integration:e2e')`,
			args: []any{nonStreamTenantID, nonStreamProjectID, nonStreamModelID}},
	}
	for _, statement := range statements {
		if _, err := database.ExecContext(ctx, statement.query, statement.args...); err != nil {
			t.Fatalf("seed non-stream %s: %v", statement.name, err)
		}
	}
}

func performNonStreamRequest(t *testing.T, gatewayURL, credential, requestID, model string) (int, []byte) {
	t.Helper()
	request, err := newNonStreamHTTPRequest(context.Background(), gatewayURL, credential, requestID, model)
	if err != nil {
		t.Fatalf("new non-stream request: %v", err)
	}
	// #nosec G704 -- the URL is owned by the in-process httptest Gateway.
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("perform non-stream request: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read non-stream response: %v", err)
	}
	return response.StatusCode, body
}

func newNonStreamHTTPRequest(
	ctx context.Context,
	gatewayURL, credential, requestID, model string,
) (*http.Request, error) {
	body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hello"}]}`, model)
	// #nosec G704 -- gatewayURL is issued by the in-process httptest Server.
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, gatewayURL+"/v1/chat/completions", bytes.NewBufferString(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+credential)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Request-Id", requestID)
	return request, nil
}

func assertE2EStatusAndCode(t *testing.T, status int, body []byte, wantStatus int, wantCode string) {
	t.Helper()
	if status != wantStatus {
		t.Fatalf("HTTP status = %d, want %d; body = %s", status, wantStatus, body)
	}
	if wantCode == "" {
		return
	}
	var envelope apierror.Envelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode error envelope: %v; body = %s", err, body)
	}
	if envelope.Error.Code != wantCode {
		t.Fatalf("error code = %q, want %q; body = %s", envelope.Error.Code, wantCode, body)
	}
}

func assertNonStreamExecution(
	t *testing.T,
	database *sql.DB,
	requestID string,
	requestStatus execution.RequestStatus,
	requestReason string,
	attemptCount int,
	attemptStatus execution.AttemptStatus,
	attemptReason, errorCategory, errorCode string,
	headersReceived bool,
) {
	t.Helper()
	var gotRequestStatus, gotRequestReason string
	var gotAttemptCount int
	if err := database.QueryRow(`SELECT status, end_reason, attempt_count FROM app.gateway_requests WHERE id = $1`, requestID).
		Scan(&gotRequestStatus, &gotRequestReason, &gotAttemptCount); err != nil {
		t.Fatalf("query GatewayRequest %s: %v", requestID, err)
	}
	if gotRequestStatus != string(requestStatus) || gotRequestReason != requestReason || gotAttemptCount != attemptCount {
		t.Fatalf("GatewayRequest %s = %s/%s/%d, want %s/%s/%d",
			requestID, gotRequestStatus, gotRequestReason, gotAttemptCount, requestStatus, requestReason, attemptCount)
	}

	var attempts int
	if err := database.QueryRow(`SELECT count(*) FROM app.route_attempts WHERE request_id = $1`, requestID).Scan(&attempts); err != nil {
		t.Fatalf("count RouteAttempts for %s: %v", requestID, err)
	}
	if attempts != attemptCount {
		t.Fatalf("RouteAttempt count for %s = %d, want %d", requestID, attempts, attemptCount)
	}
	requestEvents := queryStatusEvents(context.Background(), t, database, `
		SELECT to_status FROM app.gateway_request_status_events
		WHERE request_id = $1 ORDER BY request_version`, requestID)
	wantRequestEvents := []string{"authorized", "routing"}
	if attemptCount > 0 {
		wantRequestEvents = append(wantRequestEvents, "running")
	}
	wantRequestEvents = append(wantRequestEvents, string(requestStatus))
	if !reflect.DeepEqual(requestEvents, wantRequestEvents) {
		t.Fatalf("GatewayRequest events for %s = %v, want %v", requestID, requestEvents, wantRequestEvents)
	}
	if attemptCount == 0 {
		return
	}

	var attemptID, gotAttemptStatus, gotAttemptReason string
	var gotErrorCategory, gotErrorCode sql.NullString
	var gotHeaders bool
	if err := database.QueryRow(`SELECT id::text, status, end_reason, error_category, error_code,
			headers_received_at IS NOT NULL FROM app.route_attempts WHERE request_id = $1`, requestID).
		Scan(&attemptID, &gotAttemptStatus, &gotAttemptReason, &gotErrorCategory, &gotErrorCode, &gotHeaders); err != nil {
		t.Fatalf("query RouteAttempt for %s: %v", requestID, err)
	}
	if gotAttemptStatus != string(attemptStatus) || gotAttemptReason != attemptReason ||
		gotErrorCategory.String != errorCategory || gotErrorCategory.Valid != (errorCategory != "") ||
		gotErrorCode.String != errorCode || gotErrorCode.Valid != (errorCode != "") || gotHeaders != headersReceived {
		t.Fatalf("RouteAttempt for %s = %s/%s/%v/%v/headers=%t, want %s/%s/%q/%q/headers=%t",
			requestID, gotAttemptStatus, gotAttemptReason, gotErrorCategory, gotErrorCode, gotHeaders,
			attemptStatus, attemptReason, errorCategory, errorCode, headersReceived)
	}
	attemptEvents := queryStatusEvents(context.Background(), t, database, `
		SELECT to_status FROM app.route_attempt_status_events
		WHERE attempt_id = $1::uuid ORDER BY attempt_version`, attemptID)
	wantAttemptEvents := []string{"created", "connecting"}
	if headersReceived {
		wantAttemptEvents = append(wantAttemptEvents, "headers_received")
	}
	wantAttemptEvents = append(wantAttemptEvents, string(attemptStatus))
	if !reflect.DeepEqual(attemptEvents, wantAttemptEvents) {
		t.Fatalf("RouteAttempt events for %s = %v, want %v", requestID, attemptEvents, wantAttemptEvents)
	}
}

func assertNoGatewayRequest(t *testing.T, database *sql.DB, requestID string) {
	t.Helper()
	var count int
	if err := database.QueryRow(`SELECT count(*) FROM app.gateway_requests WHERE id = $1`, requestID).Scan(&count); err != nil {
		t.Fatalf("count GatewayRequest %s: %v", requestID, err)
	}
	if count != 0 {
		t.Fatalf("GatewayRequest %s exists after authentication failure", requestID)
	}
}

func waitForNonStreamExecution(
	t *testing.T,
	database *sql.DB,
	requestID string,
	requestStatus execution.RequestStatus,
	attemptStatus execution.AttemptStatus,
) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		var gotRequest, gotAttempt string
		err := database.QueryRow(`SELECT request.status, attempt.status
			FROM app.gateway_requests AS request
			JOIN app.route_attempts AS attempt ON attempt.request_id = request.id
			WHERE request.id = $1`, requestID).Scan(&gotRequest, &gotAttempt)
		if err == nil && gotRequest == string(requestStatus) && gotAttempt == string(attemptStatus) {
			return
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("poll cancelled execution: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("execution %s did not reach %s/%s", requestID, requestStatus, attemptStatus)
}

func cleanupNonStreamFixtures(t *testing.T, database *sql.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	statements := []struct {
		name  string
		query string
		args  []any
	}{
		{name: "attempt events", query: `DELETE FROM app.route_attempt_status_events WHERE attempt_id IN
			(SELECT id FROM app.route_attempts WHERE request_id LIKE 'e2e-nonstream-%')`},
		{name: "request events", query: `DELETE FROM app.gateway_request_status_events WHERE request_id LIKE 'e2e-nonstream-%'`},
		{name: "attempts", query: `DELETE FROM app.route_attempts WHERE request_id LIKE 'e2e-nonstream-%'`},
		{name: "requests", query: `DELETE FROM app.gateway_requests WHERE id LIKE 'e2e-nonstream-%'`},
		{name: "credentials", query: `DELETE FROM app.virtual_api_keys WHERE tenant_id = $1`, args: []any{nonStreamTenantID}},
		{name: "project grants", query: `DELETE FROM app.project_logical_models WHERE tenant_id = $1`, args: []any{nonStreamTenantID}},
		{name: "bindings", query: `DELETE FROM app.logical_model_deployments WHERE logical_model_id = $1`, args: []any{nonStreamModelID}},
		{name: "deployments", query: `DELETE FROM app.deployments WHERE id = $1`, args: []any{nonStreamDeploymentID}},
		{name: "logical models", query: `DELETE FROM app.logical_models WHERE tenant_id = $1`, args: []any{nonStreamTenantID}},
		{name: "providers", query: `DELETE FROM app.providers WHERE id = $1`, args: []any{nonStreamProviderID}},
		{name: "projects", query: `DELETE FROM app.projects WHERE tenant_id = $1`, args: []any{nonStreamTenantID}},
		{name: "tenants", query: `DELETE FROM app.tenants WHERE id = $1`, args: []any{nonStreamTenantID}},
	}
	for _, statement := range statements {
		if _, err := database.ExecContext(ctx, statement.query, statement.args...); err != nil {
			t.Errorf("cleanup non-stream %s: %v", statement.name, err)
		}
	}
}

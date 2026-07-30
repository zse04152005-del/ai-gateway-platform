//go:build integration

package integration_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

const processTestTimeout = 15 * time.Second

type runningProcess struct {
	command  *exec.Cmd
	done     chan error
	stderr   bytes.Buffer
	stdout   bytes.Buffer
	finished atomic.Bool
}

type acceptResult struct {
	connection net.Conn
	err        error
}

func TestProcessLifecycle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("actual SIGTERM lifecycle validation runs on Linux CI; command unit tests cover Windows cancellation")
	}
	root := repositoryRoot(t)
	binaries := buildProcessBinaries(t, root)

	t.Run("gateway", func(t *testing.T) {
		address := freeAddress(t)
		testHTTPProcess(t, root, binaries["gateway"], "gateway", map[string]string{
			"GATEWAY_HTTP_ADDR": address,
		}, "http://"+address+"/health/ready", `"status":"ok"`)
	})

	t.Run("control-plane", func(t *testing.T) {
		address := freeAddress(t)
		testHTTPProcess(t, root, binaries["control-plane"], "control-plane", map[string]string{
			"CONTROL_PLANE_HTTP_ADDR": address,
		}, "http://"+address+"/admin/v1/status", `"service":"control-plane"`)
	})

	t.Run("metering-worker", func(t *testing.T) {
		testMeteringWorkerProcess(t, root, binaries["metering-worker"])
	})

	t.Run("configuration-errors", func(t *testing.T) {
		tests := map[string]string{
			"gateway":         "GATEWAY_PROCESS_FAILED",
			"control-plane":   "CONTROL_PLANE_PROCESS_FAILED",
			"metering-worker": "METERING_WORKER_PROCESS_FAILED",
		}
		for service, errorCode := range tests {
			t.Run(service, func(t *testing.T) {
				testConfigurationError(t, root, binaries[service], service, errorCode)
			})
		}
	})
}

func testHTTPProcess(
	t *testing.T,
	root, binary, service string,
	overrides map[string]string,
	target, wantBody string,
) {
	t.Helper()
	process := startProcess(t, root, binary, overrides)
	waitForHTTP(t, process, target, http.StatusOK, wantBody)
	stopProcess(t, process)
	assertJSONLogs(t, process.stderr.String(), service, "postgres://")
}

func testMeteringWorkerProcess(t *testing.T, root, binary string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	accepted := make(chan acceptResult, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		accepted <- acceptResult{connection: connection, err: acceptErr}
	}()

	process := startProcess(t, root, binary, map[string]string{
		"KAFKA_BROKERS": listener.Addr().String(),
	})
	var connection net.Conn
	select {
	case result := <-accepted:
		if result.err != nil {
			t.Fatalf("event-bus Accept() error = %v", result.err)
		}
		connection = result.connection
	case err := <-process.done:
		t.Fatalf("metering-worker exited before healthy connection: %v; stderr=%s", err, process.stderr.String())
	case <-time.After(processTestTimeout):
		t.Fatal("timed out waiting for metering-worker event-bus connection")
	}
	t.Cleanup(func() { _ = connection.Close() })

	stopProcess(t, process)
	if err := connection.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("SetReadDeadline() error = %v", err)
	}
	buffer := make([]byte, 1)
	if _, err := connection.Read(buffer); err == nil {
		t.Fatal("event-bus connection remained open after worker shutdown")
	}
	assertJSONLogs(t, process.stderr.String(), "metering-worker", listener.Addr().String(), "postgres://")
}

func testConfigurationError(t *testing.T, root, binary, service, errorCode string) {
	t.Helper()
	command := exec.Command(binary)
	command.Dir = root
	command.Env = processEnvironment(map[string]string{
		"DATABASE_URL": "http://10.9.8.7/private-config-value",
	})
	var stderr bytes.Buffer
	command.Stderr = &stderr
	command.Stdout = &bytes.Buffer{}
	err := command.Run()
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() == 0 {
		t.Fatalf("invalid configuration exit error = %v", err)
	}
	assertJSONLogs(t, stderr.String(), service, "private-config-value", "10.9.8.7")
	if !strings.Contains(stderr.String(), errorCode) {
		t.Fatalf("configuration log does not contain %q: %s", errorCode, stderr.String())
	}
}

func buildProcessBinaries(t *testing.T, root string) map[string]string {
	t.Helper()
	directory := t.TempDir()
	binaries := make(map[string]string, 3)
	for _, name := range []string{"gateway", "control-plane", "metering-worker"} {
		path := filepath.Join(directory, name)
		// The executable path is under t.TempDir and name comes only from the fixed literal slice above.
		command := exec.Command("go", "build", "-buildvcs=false", "-o", path, "./cmd/"+name) //nolint:gosec
		command.Dir = root
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("build %s: %v\n%s", name, err, output)
		}
		binaries[name] = path
	}
	return binaries
}

func startProcess(t *testing.T, root, binary string, overrides map[string]string) *runningProcess {
	t.Helper()
	process := &runningProcess{
		command: exec.Command(binary),
		done:    make(chan error, 1),
	}
	process.command.Dir = root
	process.command.Env = processEnvironment(overrides)
	process.command.Stderr = &process.stderr
	process.command.Stdout = &process.stdout
	if err := process.command.Start(); err != nil {
		t.Fatalf("start %s: %v", filepath.Base(binary), err)
	}
	go func() {
		err := process.command.Wait()
		process.finished.Store(true)
		process.done <- err
	}()
	t.Cleanup(func() {
		if process.finished.Load() {
			return
		}
		_ = process.command.Process.Kill()
		select {
		case <-process.done:
		case <-time.After(processTestTimeout):
		}
	})
	return process
}

func stopProcess(t *testing.T, process *runningProcess) {
	t.Helper()
	if err := process.command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM: %v", err)
	}
	select {
	case err := <-process.done:
		if err != nil {
			t.Fatalf("process shutdown error = %v; stderr=%s", err, process.stderr.String())
		}
	case <-time.After(processTestTimeout):
		t.Fatal("process did not stop after SIGTERM")
	}
}

func waitForHTTP(t *testing.T, process *runningProcess, target string, status int, wantBody string) {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	t.Cleanup(client.CloseIdleConnections)
	deadline := time.Now().Add(processTestTimeout)
	for time.Now().Before(deadline) {
		select {
		case err := <-process.done:
			t.Fatalf("process exited before HTTP health: %v; stderr=%s", err, process.stderr.String())
		default:
		}
		response, err := client.Get(target)
		if err == nil {
			body := new(bytes.Buffer)
			_, copyErr := body.ReadFrom(response.Body)
			closeErr := response.Body.Close()
			if copyErr == nil && closeErr == nil && response.StatusCode == status && strings.Contains(body.String(), wantBody) {
				if response.Header.Get("X-Request-Id") == "" || response.Header.Get("traceparent") == "" {
					t.Fatalf("health response is missing correlation headers: %v", response.Header)
				}
				return
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", target)
}

func assertJSONLogs(t *testing.T, raw, service string, forbidden ...string) {
	t.Helper()
	for _, value := range forbidden {
		if strings.Contains(raw, value) {
			t.Fatalf("%s logs contain forbidden value %q: %s", service, value, raw)
		}
	}
	found := false
	scanner := bufio.NewScanner(strings.NewReader(raw))
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatalf("%s emitted non-JSON log %q: %v", service, scanner.Text(), err)
		}
		if record["service"] == service && record["level"] != nil {
			found = true
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan %s logs: %v", service, err)
	}
	if !found {
		t.Fatalf("no structured %s log found: %s", service, raw)
	}
}

func freeAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve address: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release reserved address: %v", err)
	}
	return address
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller() failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func processEnvironment(overrides map[string]string) []string {
	values := map[string]string{
		"APP_ENV":                      "test",
		"LOG_LEVEL":                    "info",
		"DATABASE_URL":                 "postgres://127.0.0.1:5432/process_test?sslmode=disable",
		"GATEWAY_HTTP_ADDR":            "127.0.0.1:18080",
		"CONTROL_PLANE_HTTP_ADDR":      "127.0.0.1:18081",
		"METRICS_ADDR":                 "127.0.0.1:19091",
		"SHUTDOWN_TIMEOUT":             "2s",
		"HTTP_READ_HEADER_TIMEOUT":     "2s",
		"REDIS_ADDR":                   "127.0.0.1:6379",
		"REDIS_PASSWORD":               "",
		"REDIS_DB":                     "0",
		"KAFKA_BROKERS":                "127.0.0.1:19092",
		"CLICKHOUSE_HTTP_URL":          "http://127.0.0.1:8123",
		"OTEL_EXPORTER_OTLP_ENDPOINT":  "http://127.0.0.1:4318",
		"LOCAL_ENVELOPE_KEY":           "",
		"VIRTUAL_KEY_HASH_KEY":         strings.Repeat("11", 32),
		"VIRTUAL_KEY_HASH_KEY_VERSION": "process-v1",
	}
	for key, value := range overrides {
		values[key] = value
	}
	blocked := make(map[string]struct{}, len(values))
	for key := range values {
		blocked[key] = struct{}{}
	}
	environment := make([]string, 0, len(os.Environ())+len(values))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if _, replace := blocked[key]; !replace {
			environment = append(environment, entry)
		}
	}
	for key, value := range values {
		environment = append(environment, fmt.Sprintf("%s=%s", key, value))
	}
	return environment
}

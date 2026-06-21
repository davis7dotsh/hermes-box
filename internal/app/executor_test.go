package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/davis7dotsh/hermes-box/internal/config"
	"github.com/davis7dotsh/hermes-box/internal/process"
)

type executorAutoStartRunner struct {
	startDeadline  time.Time
	sawDeadline    bool
	probeCalls     int
	unboundedProbe bool
}

func (r *executorAutoStartRunner) Run(ctx context.Context, spec process.Spec) error {
	if spec.Name == "smolvm" && containsArgument(spec.Args, "start") {
		r.startDeadline, r.sawDeadline = ctx.Deadline()
		return errors.New("stop after capturing Executor auto-start deadline")
	}
	return nil
}

func (r *executorAutoStartRunner) Output(ctx context.Context, spec process.Spec) ([]byte, error) {
	if containsArgument(spec.Args, "list") {
		r.recordProbeContext(ctx)
		return []byte(`[{"name":"test-box"}]`), nil
	}
	if containsArgument(spec.Args, "status") {
		r.recordProbeContext(ctx)
		return []byte(`{"state":"stopped"}`), nil
	}
	return nil, nil
}

func (r *executorAutoStartRunner) recordProbeContext(ctx context.Context) {
	r.probeCalls++
	if _, ok := ctx.Deadline(); !ok {
		r.unboundedProbe = true
	}
}

func TestExecutorMCPClientInitializesListsAndExecutes(t *testing.T) {
	t.Helper()
	const apiKey = "executor-test-key"
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		if request.Header.Get("Authorization") != "Bearer "+apiKey {
			t.Errorf("Authorization = %q", request.Header.Get("Authorization"))
		}
		var message struct {
			ID     int             `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(request.Body).Decode(&message); err != nil {
			t.Errorf("decode request: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		switch message.Method {
		case "initialize":
			writer.Header().Set("Mcp-Session-Id", "test-session")
			writeRPCResult(t, writer, message.ID, map[string]any{
				"protocolVersion": "2025-03-26",
				"capabilities":    map[string]any{},
				"serverInfo":      map[string]string{"name": "executor", "version": "test"},
			})
		case "notifications/initialized":
			if request.Header.Get("Mcp-Session-Id") != "test-session" {
				t.Errorf("notification session = %q", request.Header.Get("Mcp-Session-Id"))
			}
			writer.WriteHeader(http.StatusAccepted)
		case "tools/list":
			writeRPCResult(t, writer, message.ID, map[string]any{
				"tools": []map[string]string{{"name": "execute"}, {"name": "resume"}},
			})
		case "tools/call":
			var params struct {
				Name      string            `json:"name"`
				Arguments map[string]string `json:"arguments"`
			}
			if err := json.Unmarshal(message.Params, &params); err != nil {
				t.Errorf("unmarshal tools/call params: %v", err)
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			if params.Name != "execute" || params.Arguments["code"] != "return 42;" {
				t.Errorf("tools/call params = %#v", params)
			}
			writeRPCResult(t, writer, message.ID, map[string]any{
				"content": []map[string]string{{"type": "text", "text": `{"answer":42}`}},
			})
		default:
			t.Errorf("unexpected method %q", message.Method)
			writer.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()

	client := newExecutorMCPClient(server.URL, apiKey, server.Client())
	ctx := context.Background()
	if err := client.initialize(ctx); err != nil {
		t.Fatal(err)
	}
	tools, err := client.listTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(tools, []string{"execute", "resume"}) {
		t.Fatalf("tools = %v", tools)
	}
	result, err := client.execute(ctx, "return 42;")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result, map[string]any{"answer": float64(42)}) {
		t.Fatalf("result = %#v", result)
	}
	if requests != 4 {
		t.Fatalf("requests = %d, want 4", requests)
	}
}

func TestExecutorMCPMessageParsesSSE(t *testing.T) {
	body := []byte("event: message\r\ndata: {\"jsonrpc\":\"2.0\",\r\ndata: \"result\":{}}\r\n\r\n")
	message, err := executorMCPMessage("text/event-stream; charset=utf-8", body)
	if err != nil {
		t.Fatal(err)
	}
	if string(message) != "{\"jsonrpc\":\"2.0\",\n\"result\":{}}" {
		t.Fatalf("message = %q", message)
	}
}

func TestParseExecutorToolOptions(t *testing.T) {
	query, namespace, limit, jsonOutput, err := parseExecutorToolOptions([]string{
		"calendar events", "--namespace", "google", "--limit", "7", "--json",
	})
	if err != nil {
		t.Fatal(err)
	}
	if query != "calendar events" || namespace != "google" || limit != 7 || !jsonOutput {
		t.Fatalf("options = %q %q %d %t", query, namespace, limit, jsonOutput)
	}
}

func TestParseExecutorToolOptionsRejectsInvalidLimit(t *testing.T) {
	if _, _, _, _, err := parseExecutorToolOptions([]string{"--limit", "101"}); err == nil {
		t.Fatal("accepted an excessive tool limit")
	}
}

func TestParseExecutorLogOptions(t *testing.T) {
	follow, lines, err := parseLogOptions("executor logs", []string{"-f", "-n", "25"})
	if err != nil {
		t.Fatal(err)
	}
	if !follow || lines != 25 {
		t.Fatalf("follow = %t, lines = %d", follow, lines)
	}
}

func TestParseExecutorStatusOptions(t *testing.T) {
	jsonOutput, sizes, err := parseExecutorStatusOptions([]string{"--sizes", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	if !jsonOutput || !sizes {
		t.Fatalf("json = %t, sizes = %t", jsonOutput, sizes)
	}
}

func TestParseExecutorStatusOptionsRejectsDuplicatesAndUnknowns(t *testing.T) {
	for _, args := range [][]string{{"--sizes", "--sizes"}, {"--json", "--json"}, {"--fast"}} {
		if _, _, err := parseExecutorStatusOptions(args); err == nil {
			t.Fatalf("accepted status options %v", args)
		}
	}
}

func TestExecutorRuntimeMetadataSkipsRecursiveSizesByDefault(t *testing.T) {
	runner := &recordingRunner{}
	application := New(t.TempDir(), config.Config{MachineName: "test-box"}, runner, io.Discard, io.Discard)
	if _, err := application.executorRuntimeMetadata(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	script := runner.last.Args[len(runner.last.Args)-1]
	if strings.Contains(script, "du -sh") || strings.Contains(script, "runtime_size=") {
		t.Fatalf("default metadata performs a recursive size scan: %s", script)
	}

	if _, err := application.executorRuntimeMetadata(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	script = runner.last.Args[len(runner.last.Args)-1]
	for _, expected := range []string{"timeout --signal=TERM", "runtime_size=", "data_size=", "du -sh"} {
		if !strings.Contains(script, expected) {
			t.Fatalf("sized metadata does not contain %q: %s", expected, script)
		}
	}
}

func TestEnsureExecutorReadyUsesCentralExecutorStartupDeadline(t *testing.T) {
	runner := &executorAutoStartRunner{}
	application := New(t.TempDir(), config.Config{
		MachineName:     "test-box",
		SSHPort:         2223,
		ExecutorEnabled: true,
		ExecutorPort:    4788,
	}, runner, io.Discard, io.Discard)

	if err := application.ensureExecutorReady(context.Background()); err == nil {
		t.Fatal("ensureExecutorReady succeeded despite injected start failure")
	}
	if !runner.sawDeadline {
		t.Fatal("Executor auto-start did not receive a startup deadline")
	}
	if runner.probeCalls != 4 || runner.unboundedProbe {
		t.Fatalf(
			"Executor auto-start probe calls = %d, unbounded = %t; want four bounded probes",
			runner.probeCalls,
			runner.unboundedProbe,
		)
	}
	remaining := time.Until(runner.startDeadline)
	if remaining < 2*time.Hour-5*time.Second || remaining > 2*time.Hour {
		t.Fatalf("Executor auto-start deadline remaining = %s, want approximately 2h", remaining)
	}
}

func TestWriteExecutorStatusDoesNotExposeSecrets(t *testing.T) {
	var output strings.Builder
	status := executorStatus{
		Enabled: true, Machine: "test-box", State: "running", URL: "http://localhost:4788",
		Healthy: true, AuthStored: true, Supervisor: "executor RUNNING", Image: "executor@test",
	}
	if err := writeExecutorStatus(&output, status, false); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"test-box (running)", "healthy", "stored", "executor RUNNING"} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("status output does not contain %q: %s", expected, output.String())
		}
	}
}

func TestWriteExecutorStatusWhenDisabledSkipsHealth(t *testing.T) {
	var output strings.Builder
	status := executorStatus{Machine: "test-box", State: "running", URL: "http://localhost:4788"}
	if err := writeExecutorStatus(&output, status, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Enabled:    no") || strings.Contains(output.String(), "Health:") {
		t.Fatalf("disabled status output = %q", output.String())
	}
}

func TestWriteExecutorStatusReportsUnavailableListener(t *testing.T) {
	var output strings.Builder
	status := executorStatus{
		Enabled: true, Machine: "box", State: "running", URL: "http://localhost:4788",
		HealthError: "Executor listener is unavailable or unsafe: no listener found",
	}
	if err := writeExecutorStatus(&output, status, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Health:     unhealthy") ||
		!strings.Contains(output.String(), status.HealthError) {
		t.Fatalf("unavailable status output = %q", output.String())
	}
}

func TestWriteExecutorStatusSeparatesMetadataFailureFromHealth(t *testing.T) {
	var output strings.Builder
	status := executorStatus{
		Enabled: true, Machine: "box", State: "running", URL: "http://localhost:4788",
		Healthy: true, MetadataError: "size scan timed out",
	}
	if err := writeExecutorStatus(&output, status, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Health:     healthy") ||
		!strings.Contains(output.String(), "Metadata err: size scan timed out") ||
		strings.Contains(output.String(), "Health err:") {
		t.Fatalf("metadata failure status output = %q", output.String())
	}
}

func TestWriteExecutorStatusShowsSizesOnlyWhenRequestedMetadataExists(t *testing.T) {
	status := executorStatus{
		Enabled: true, Machine: "box", State: "running", URL: "http://localhost:4788",
		Healthy: true, RuntimeSize: "2.5G", DataSize: "12M",
	}
	var output strings.Builder
	if err := writeExecutorStatus(&output, status, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Disk:       runtime 2.5G, data 12M") {
		t.Fatalf("sized status output = %q", output.String())
	}

	output.Reset()
	status.RuntimeSize = ""
	status.DataSize = ""
	if err := writeExecutorStatus(&output, status, true); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "runtimeSize") || strings.Contains(output.String(), "dataSize") {
		t.Fatalf("default JSON status exposed empty sizes: %q", output.String())
	}
}

func TestWriteExecutorResultJSON(t *testing.T) {
	var output strings.Builder
	if err := writeExecutorResult(&output, map[string]any{"connections": []any{}}, true); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(output.String()) != `{"connections":[]}` {
		t.Fatalf("output = %q", output.String())
	}
}

func TestExecutorAuthSetPromptsWithoutPuttingKeyInArguments(t *testing.T) {
	application := New(t.TempDir(), config.Config{MachineName: "test-box"}, nil, io.Discard, io.Discard)
	spec := application.executorAuthSetSpec()
	if spec.Name != "security" || spec.Args[len(spec.Args)-1] != "-w" {
		t.Fatalf("security command = %s %v", spec.Name, spec.Args)
	}
	if strings.Join(spec.Args, " ") != "add-generic-password -U -a api-key -s com.highmatter.hermes-box.executor.test-box -l Hermes Box Executor API key (test-box) -w" {
		t.Fatalf("security arguments = %v", spec.Args)
	}
}

func TestNormalizeExecutorAPIKey(t *testing.T) {
	for input, expected := range map[string]string{
		" token ":          "token",
		"Bearer token":     "token",
		"bearer   token  ": "token",
	} {
		if actual := normalizeExecutorAPIKey(input); actual != expected {
			t.Errorf("normalizeExecutorAPIKey(%q) = %q, want %q", input, actual, expected)
		}
	}
}

func TestExecutorTokenSSHSpecKeepsKeyOutOfArguments(t *testing.T) {
	const apiKey = "executor-secret-token"
	application := New(t.TempDir(), config.Config{MachineName: "test-box", SSHPort: 2299}, nil, io.Discard, io.Discard)
	spec := application.executorTokenSSHSpec("remote command", apiKey)
	if strings.Contains(strings.Join(spec.Args, " "), apiKey) {
		t.Fatalf("API key leaked into SSH arguments: %v", spec.Args)
	}
	value, err := io.ReadAll(spec.Stdin)
	if err != nil {
		t.Fatal(err)
	}
	if string(value) != apiKey {
		t.Fatalf("SSH stdin = %q", value)
	}
}

func writeRPCResult(t *testing.T, writer io.Writer, id int, result any) {
	t.Helper()
	if err := json.NewEncoder(writer).Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	}); err != nil {
		t.Fatal(err)
	}
}

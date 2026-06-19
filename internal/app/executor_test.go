package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/davis7dotsh/hermes-box/internal/config"
)

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

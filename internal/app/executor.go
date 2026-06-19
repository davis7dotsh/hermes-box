package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/davis7dotsh/hermes-box/internal/process"
)

const executorGuestURL = "http://127.0.0.1:4788"

type executorStatus struct {
	Enabled     bool   `json:"enabled"`
	Machine     string `json:"machine"`
	State       string `json:"state"`
	URL         string `json:"url"`
	Healthy     bool   `json:"healthy"`
	HealthError string `json:"healthError,omitempty"`
	AuthStored  bool   `json:"authStored"`
	Supervisor  string `json:"supervisor,omitempty"`
	Image       string `json:"image,omitempty"`
	ImageDigest string `json:"imageDigest,omitempty"`
	RuntimeSize string `json:"runtimeSize,omitempty"`
	DataSize    string `json:"dataSize,omitempty"`
}

type executorMCPClient struct {
	endpoint  string
	apiKey    string
	client    *http.Client
	sessionID string
	nextID    int
}

type executorRPCResponse struct {
	Result json.RawMessage   `json:"result"`
	Error  *executorRPCError `json:"error"`
}

type executorRPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

func (e *executorRPCError) Error() string {
	if len(e.Data) == 0 || string(e.Data) == "null" {
		return fmt.Sprintf("Executor MCP error %d: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("Executor MCP error %d: %s (%s)", e.Code, e.Message, e.Data)
}

func (a *App) cmdExecutor(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		if len(args) > 1 {
			return errors.New("executor help takes no arguments")
		}
		a.executorUsage()
		return nil
	}
	if args[0] == "status" {
		return a.cmdExecutorStatus(ctx, args[1:])
	}
	if !a.config.ExecutorEnabled {
		return errors.New("executor is disabled; set HERMES_BOX_EXECUTOR_ENABLED=true before using executor commands")
	}

	subcommand := args[0]
	args = args[1:]
	switch subcommand {
	case "open":
		return a.withLock(func() error { return a.cmdExecutorOpen(ctx, args) })
	case "logs":
		return a.cmdExecutorLogs(ctx, args)
	case "auth":
		return a.cmdExecutorAuth(ctx, args)
	case "connections":
		return a.withLock(func() error { return a.cmdExecutorConnections(ctx, args) })
	case "tools":
		return a.withLock(func() error { return a.cmdExecutorTools(ctx, args) })
	case "mcp-test":
		return a.withLock(func() error { return a.cmdExecutorMCPTest(ctx, args) })
	case "connect-hermes":
		return a.withLock(func() error { return a.cmdExecutorConnectHermes(ctx, args) })
	default:
		return fmt.Errorf("unknown executor command: %s", subcommand)
	}
}

func (a *App) executorUsage() {
	fmt.Fprintf(a.stdout, `Usage: hermes-box executor COMMAND [ARGS]

Commands:
  open                         Start Executor if needed and open its portal
  status [--json]              Show runtime, health, image, and auth status
  logs [-f] [-n LINES]         Show or follow Executor logs
  auth set|status|clear        Manage this box's API key in macOS Keychain
  connections [--json]         List configured Executor connections
  tools [QUERY] [OPTIONS]      Search available tools
  mcp-test                     Verify the authenticated MCP surface
  connect-hermes               Register Executor with Hermes and test it

Tool options:
  --json                       Emit machine-readable JSON
  --limit COUNT                Return at most COUNT tools (default 20)
  --namespace NAME             Limit search to one integration namespace
`)
}

func (a *App) executorURL() string {
	return fmt.Sprintf("http://localhost:%d", a.config.ExecutorPort)
}

func (a *App) ensureExecutorReady(ctx context.Context) error {
	if !a.machineExists(ctx, a.config.MachineName) {
		return errors.New("run init first")
	}
	running, err := a.isRunning(ctx, a.config.MachineName)
	if err != nil {
		return err
	}
	if !running {
		if err := a.cmdStart(ctx, nil); err != nil {
			return err
		}
	}
	if err := a.verifyLoopbackListener(ctx, a.config.ExecutorPort); err != nil {
		return fmt.Errorf("executor listener is not loopback-only: %w", err)
	}
	return a.verifyExecutorHTTP(ctx)
}

func (a *App) cmdExecutorOpen(ctx context.Context, args []string) error {
	if err := requireNoArgs("executor open", args); err != nil {
		return err
	}
	if runtime.GOOS != "darwin" {
		return errors.New("executor open currently requires macOS")
	}
	if err := a.ensureExecutorReady(ctx); err != nil {
		return err
	}
	return a.run(ctx, "open", a.executorURL())
}

func (a *App) cmdExecutorStatus(ctx context.Context, args []string) error {
	jsonOutput, err := parseJSONOnlyOption("executor status", args)
	if err != nil {
		return err
	}
	status := executorStatus{
		Enabled:    a.config.ExecutorEnabled,
		Machine:    a.config.MachineName,
		State:      "missing",
		URL:        a.executorURL(),
		AuthStored: a.executorAuthStored(ctx),
	}
	if a.machineExists(ctx, a.config.MachineName) {
		status.State, err = a.machineState(ctx, a.config.MachineName)
		if err != nil {
			return err
		}
	}
	if status.Enabled && status.State == "running" {
		if err := a.verifyLoopbackListener(ctx, a.config.ExecutorPort); err != nil {
			status.HealthError = fmt.Sprintf("Executor listener is unavailable or unsafe: %v", err)
		} else if err := a.verifyExecutorHTTP(ctx); err != nil {
			status.HealthError = err.Error()
		} else {
			status.Healthy = true
		}
		metadata, metadataErr := a.executorRuntimeMetadata(ctx)
		if metadataErr != nil {
			if status.HealthError == "" {
				status.HealthError = metadataErr.Error()
			}
		} else {
			status.Supervisor = metadata["supervisor"]
			status.Image = metadata["image"]
			status.ImageDigest = metadata["digest"]
			status.RuntimeSize = metadata["runtime_size"]
			status.DataSize = metadata["data_size"]
		}
	}
	return writeExecutorStatus(a.stdout, status, jsonOutput)
}

func (a *App) executorRuntimeMetadata(ctx context.Context) (map[string]string, error) {
	script := `
set -e
printf 'supervisor=%s\n' "$(supervisorctl status executor | tr -s ' ')"
printf 'image=%s\n' "$(cat /workspace/.hermes-box-runtime/executor/current/.image-reference)"
printf 'digest=%s\n' "$(cat /workspace/.hermes-box-runtime/executor/current/.manifest-digest)"
printf 'runtime_size=%s\n' "$(du -sh /workspace/.hermes-box-runtime/executor | cut -f1)"
printf 'data_size=%s\n' "$(du -sh /workspace/executor/data | cut -f1)"
`
	output, err := a.output(
		ctx,
		"smolvm",
		"machine", "exec",
		"--name", a.config.MachineName,
		"--",
		"bash", "-lc", script,
	)
	if err != nil {
		return nil, fmt.Errorf("read Executor runtime metadata: %w", err)
	}
	metadata := map[string]string{}
	for line := range strings.SplitSeq(strings.TrimSpace(string(output)), "\n") {
		key, value, found := strings.Cut(line, "=")
		if found {
			metadata[key] = value
		}
	}
	return metadata, nil
}

func writeExecutorStatus(writer io.Writer, status executorStatus, jsonOutput bool) error {
	if jsonOutput {
		return json.NewEncoder(writer).Encode(status)
	}
	fmt.Fprintf(writer, "Enabled:    %s\n", yesNo(status.Enabled, "yes", "no"))
	fmt.Fprintf(writer, "Machine:    %s (%s)\n", status.Machine, status.State)
	fmt.Fprintf(writer, "Portal:     %s\n", status.URL)
	fmt.Fprintf(writer, "API key:    %s\n", yesNo(status.AuthStored, "stored", "not stored"))
	if !status.Enabled || status.State != "running" {
		return nil
	}
	fmt.Fprintf(writer, "Health:     %s\n", yesNo(status.Healthy, "healthy", "unhealthy"))
	if status.HealthError != "" {
		fmt.Fprintf(writer, "Health err: %s\n", status.HealthError)
	}
	if status.Supervisor != "" {
		fmt.Fprintf(writer, "Supervisor: %s\n", status.Supervisor)
	}
	if status.Image != "" {
		fmt.Fprintf(writer, "Image:      %s\n", status.Image)
	}
	if status.ImageDigest != "" {
		fmt.Fprintf(writer, "Digest:     %s\n", status.ImageDigest)
	}
	if status.RuntimeSize != "" || status.DataSize != "" {
		fmt.Fprintf(writer, "Disk:       runtime %s, data %s\n", status.RuntimeSize, status.DataSize)
	}
	return nil
}

func yesNo(value bool, yes, no string) string {
	if value {
		return yes
	}
	return no
}

func (a *App) cmdExecutorLogs(ctx context.Context, args []string) error {
	follow, lines, err := parseLogOptions("executor logs", args)
	if err != nil {
		return err
	}
	if !a.machineExists(ctx, a.config.MachineName) {
		return errors.New("machine does not exist")
	}
	running, err := a.isRunning(ctx, a.config.MachineName)
	if err != nil {
		return err
	}
	if !running {
		return errors.New("machine is stopped")
	}
	commandArgs := []string{"machine", "exec"}
	if follow {
		commandArgs = append(commandArgs, "--stream")
	}
	commandArgs = append(commandArgs, "--name", a.config.MachineName, "--", "tail", "-n", strconv.Itoa(lines))
	if follow {
		commandArgs = append(commandArgs, "-F")
	}
	commandArgs = append(commandArgs, "/workspace/executor/executor.log")
	return a.runner.Run(ctx, process.Spec{
		Name: "smolvm", Args: commandArgs, Stdout: a.stdout, Stderr: a.stderr,
	})
}

func parseLogOptions(command string, args []string) (bool, int, error) {
	follow := false
	lines := 200
	for len(args) > 0 {
		switch args[0] {
		case "-f", "--follow":
			follow = true
			args = args[1:]
		case "-n", "--lines":
			if len(args) < 2 {
				return false, 0, errors.New("--lines requires a line count")
			}
			value, err := strconv.Atoi(args[1])
			if err != nil || value < 1 {
				return false, 0, errors.New("log line count must be a positive integer")
			}
			lines = value
			args = args[2:]
		default:
			return false, 0, fmt.Errorf("unknown %s option: %s", command, args[0])
		}
	}
	return follow, lines, nil
}

func (a *App) executorKeychainService() string {
	return "com.highmatter.hermes-box.executor." + a.config.MachineName
}

func (a *App) executorAuthStored(ctx context.Context) bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	return a.runner.Run(ctx, process.Spec{
		Name:   "security",
		Args:   []string{"find-generic-password", "-a", "api-key", "-s", a.executorKeychainService()},
		Stdout: io.Discard,
		Stderr: io.Discard,
	}) == nil
}

func (a *App) executorAPIKey(ctx context.Context) (string, error) {
	if runtime.GOOS != "darwin" {
		return "", errors.New("executor Keychain auth currently requires macOS")
	}
	output, err := a.runner.Output(ctx, process.Spec{
		Name:   "security",
		Args:   []string{"find-generic-password", "-a", "api-key", "-s", a.executorKeychainService(), "-w"},
		Stderr: io.Discard,
	})
	if err != nil {
		return "", errors.New("Executor API key is not stored; run `hermes-box executor auth set`")
	}
	apiKey := normalizeExecutorAPIKey(string(output))
	if apiKey == "" {
		return "", errors.New("stored Executor API key is empty; run `hermes-box executor auth set`")
	}
	return apiKey, nil
}

func normalizeExecutorAPIKey(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= len("Bearer ") && strings.EqualFold(value[:len("Bearer ")], "Bearer ") {
		value = strings.TrimSpace(value[len("Bearer "):])
	}
	return value
}

func (a *App) cmdExecutorAuth(ctx context.Context, args []string) error {
	if runtime.GOOS != "darwin" {
		return errors.New("Executor Keychain auth currently requires macOS")
	}
	if len(args) != 1 {
		return errors.New("usage: hermes-box executor auth set|status|clear")
	}
	switch args[0] {
	case "set":
		fmt.Fprintln(a.stderr, "Enter the Executor API key when prompted by macOS Keychain:")
		return a.runner.Run(ctx, a.executorAuthSetSpec())
	case "status":
		fmt.Fprintf(a.stdout, "Executor API key: %s\n", yesNo(a.executorAuthStored(ctx), "stored in Keychain", "not stored"))
		return nil
	case "clear":
		if !a.executorAuthStored(ctx) {
			fmt.Fprintln(a.stdout, "Executor API key is already clear")
			return nil
		}
		if err := a.run(ctx, "security", "delete-generic-password", "-a", "api-key", "-s", a.executorKeychainService()); err != nil {
			return err
		}
		fmt.Fprintln(a.stdout, "Executor API key removed from Keychain")
		return nil
	default:
		return fmt.Errorf("unknown executor auth command: %s", args[0])
	}
}

func (a *App) executorAuthSetSpec() process.Spec {
	return process.Spec{
		Name: "security",
		Args: []string{
			"add-generic-password", "-U",
			"-a", "api-key",
			"-s", a.executorKeychainService(),
			"-l", "Hermes Box Executor API key (" + a.config.MachineName + ")",
			"-w",
		},
		Stdin: os.Stdin, Stdout: a.stdout, Stderr: a.stderr,
	}
}

func (a *App) cmdExecutorConnections(ctx context.Context, args []string) error {
	jsonOutput, err := parseJSONOnlyOption("executor connections", args)
	if err != nil {
		return err
	}
	client, err := a.executorClient(ctx)
	if err != nil {
		return err
	}
	result, err := client.execute(ctx, "return await tools.executor.coreTools.connections.list({});")
	if err != nil {
		return err
	}
	return writeExecutorResult(a.stdout, result, jsonOutput)
}

func (a *App) cmdExecutorTools(ctx context.Context, args []string) error {
	query, namespace, limit, jsonOutput, err := parseExecutorToolOptions(args)
	if err != nil {
		return err
	}
	search := map[string]any{"limit": limit}
	if query != "" {
		search["query"] = query
	}
	if namespace != "" {
		search["namespace"] = namespace
	}
	encoded, err := json.Marshal(search)
	if err != nil {
		return err
	}
	client, err := a.executorClient(ctx)
	if err != nil {
		return err
	}
	result, err := client.execute(ctx, "return await tools.search("+string(encoded)+");")
	if err != nil {
		return err
	}
	return writeExecutorResult(a.stdout, result, jsonOutput)
}

func parseExecutorToolOptions(args []string) (string, string, int, bool, error) {
	query := ""
	namespace := ""
	limit := 20
	jsonOutput := false
	for len(args) > 0 {
		switch args[0] {
		case "--json":
			jsonOutput = true
			args = args[1:]
		case "--limit":
			if len(args) < 2 {
				return "", "", 0, false, errors.New("--limit requires a count")
			}
			value, err := strconv.Atoi(args[1])
			if err != nil || value < 1 || value > 100 {
				return "", "", 0, false, errors.New("tool limit must be between 1 and 100")
			}
			limit = value
			args = args[2:]
		case "--namespace":
			if len(args) < 2 || strings.TrimSpace(args[1]) == "" {
				return "", "", 0, false, errors.New("--namespace requires a name")
			}
			namespace = args[1]
			args = args[2:]
		default:
			if strings.HasPrefix(args[0], "-") {
				return "", "", 0, false, fmt.Errorf("unknown executor tools option: %s", args[0])
			}
			if query != "" {
				return "", "", 0, false, errors.New("executor tools accepts at most one query")
			}
			query = args[0]
			args = args[1:]
		}
	}
	return query, namespace, limit, jsonOutput, nil
}

func parseJSONOnlyOption(command string, args []string) (bool, error) {
	if len(args) == 0 {
		return false, nil
	}
	if len(args) == 1 && args[0] == "--json" {
		return true, nil
	}
	return false, fmt.Errorf("usage: hermes-box %s [--json]", command)
}

func (a *App) executorClient(ctx context.Context) (*executorMCPClient, error) {
	if err := a.ensureExecutorReady(ctx); err != nil {
		return nil, err
	}
	apiKey, err := a.executorAPIKey(ctx)
	if err != nil {
		return nil, err
	}
	client := &executorMCPClient{
		endpoint: fmt.Sprintf("http://127.0.0.1:%d/mcp", a.config.ExecutorPort),
		apiKey:   apiKey,
		client:   &http.Client{Timeout: 30 * time.Second},
		nextID:   1,
	}
	if err := client.initialize(ctx); err != nil {
		return nil, err
	}
	return client, nil
}

func (a *App) cmdExecutorMCPTest(ctx context.Context, args []string) error {
	if err := requireNoArgs("executor mcp-test", args); err != nil {
		return err
	}
	client, err := a.executorClient(ctx)
	if err != nil {
		return err
	}
	tools, err := client.listTools(ctx)
	if err != nil {
		return err
	}
	sort.Strings(tools)
	expected := []string{"execute", "resume"}
	if strings.Join(tools, "\x00") != strings.Join(expected, "\x00") {
		return fmt.Errorf("unexpected Executor MCP tools: got %v, want %v", tools, expected)
	}
	fmt.Fprintf(a.stdout, "Executor MCP OK: %s/mcp (%s)\n", a.executorURL(), strings.Join(tools, ", "))
	return nil
}

func (a *App) cmdExecutorConnectHermes(ctx context.Context, args []string) error {
	if err := requireNoArgs("executor connect-hermes", args); err != nil {
		return err
	}
	client, err := a.executorClient(ctx)
	if err != nil {
		return err
	}
	tools, err := client.listTools(ctx)
	if err != nil {
		return err
	}
	sort.Strings(tools)
	if strings.Join(tools, "\x00") != "execute\x00resume" {
		return fmt.Errorf("refusing to connect unexpected Executor MCP surface: %v", tools)
	}
	apiKey, err := a.executorAPIKey(ctx)
	if err != nil {
		return err
	}
	preflightPython := `import sys; from hermes_cli.mcp_config import _probe_single_server; token=sys.stdin.read().strip(); assert token; config={"url": "` + executorGuestURL + `/mcp", "headers": {"Authorization": "Bearer " + token}, "enabled": True}; tools=_probe_single_server("executor-preflight", config); assert sorted(tool[0] for tool in tools) == ["execute", "resume"], tools`
	preflightRemote := "sudo -iu hermes env HERMES_HOME=/workspace/hermes-home /usr/local/lib/hermes-agent/venv/bin/python -c '" + preflightPython + "'"
	if err := a.runner.Run(ctx, a.executorTokenSSHSpec(preflightRemote, apiKey)); err != nil {
		return fmt.Errorf("preflight Hermes MCP connection: %w", err)
	}
	python := `import sys; from hermes_cli.config import load_config, save_config, save_env_value; token=sys.stdin.read().strip(); assert token; save_env_value("MCP_EXECUTOR_API_KEY", token); config=load_config(); config.setdefault("mcp_servers", {})["executor"]={"url": "` + executorGuestURL + `/mcp", "headers": {"Authorization": "Bearer ${MCP_EXECUTOR_API_KEY}"}, "enabled": True}; save_config(config)`
	remote := "sudo -iu hermes env HERMES_HOME=/workspace/hermes-home /usr/local/lib/hermes-agent/venv/bin/python -c '" + python + "'"
	if err := a.runner.Run(ctx, a.executorTokenSSHSpec(remote, apiKey)); err != nil {
		return fmt.Errorf("configure Hermes MCP: %w", err)
	}
	testRemote := "sudo -iu hermes env HERMES_HOME=/workspace/hermes-home hermes mcp test executor"
	if err := a.run(ctx, "ssh", a.sshArgs(a.config.SSHPort, testRemote)...); err != nil {
		return fmt.Errorf("test Hermes MCP connection: %w", err)
	}
	restartRemote := "sudo supervisorctl restart hermes && sudo supervisorctl status hermes"
	if err := a.run(ctx, "ssh", a.sshArgs(a.config.SSHPort, restartRemote)...); err != nil {
		return fmt.Errorf("restart Hermes gateway: %w", err)
	}
	fmt.Fprintln(a.stdout, "Hermes is connected to Executor as MCP server `executor`; start a new Hermes CLI session to load it")
	return nil
}

func (a *App) executorTokenSSHSpec(remote, apiKey string) process.Spec {
	return process.Spec{
		Name:   "ssh",
		Args:   a.sshArgs(a.config.SSHPort, remote),
		Stdin:  strings.NewReader(apiKey),
		Stdout: io.Discard,
		Stderr: a.stderr,
	}
}

func newExecutorMCPClient(endpoint, apiKey string, client *http.Client) *executorMCPClient {
	return &executorMCPClient{endpoint: endpoint, apiKey: apiKey, client: client, nextID: 1}
}

func (c *executorMCPClient) initialize(ctx context.Context) error {
	var result json.RawMessage
	if err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": "2025-03-26",
		"capabilities":    map[string]any{},
		"clientInfo": map[string]string{
			"name":    "hermes-box",
			"version": "1",
		},
	}, &result); err != nil {
		return fmt.Errorf("initialize Executor MCP: %w", err)
	}
	if err := c.notify(ctx, "notifications/initialized", map[string]any{}); err != nil {
		return fmt.Errorf("finish Executor MCP initialization: %w", err)
	}
	return nil
}

func (c *executorMCPClient) listTools(ctx context.Context) ([]string, error) {
	var result struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := c.call(ctx, "tools/list", map[string]any{}, &result); err != nil {
		return nil, fmt.Errorf("list Executor MCP tools: %w", err)
	}
	names := make([]string, 0, len(result.Tools))
	for _, tool := range result.Tools {
		names = append(names, tool.Name)
	}
	return names, nil
}

func (c *executorMCPClient) execute(ctx context.Context, code string) (any, error) {
	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StructuredContent json.RawMessage `json:"structuredContent"`
		IsError           bool            `json:"isError"`
	}
	if err := c.call(ctx, "tools/call", map[string]any{
		"name": "execute",
		"arguments": map[string]string{
			"code": code,
		},
	}, &result); err != nil {
		return nil, fmt.Errorf("call Executor execute tool: %w", err)
	}
	if result.IsError {
		return nil, fmt.Errorf("Executor execute failed: %s", executorContentText(result.Content))
	}
	if len(result.StructuredContent) > 0 && string(result.StructuredContent) != "null" {
		var value any
		if err := json.Unmarshal(result.StructuredContent, &value); err == nil {
			return value, nil
		}
	}
	text := executorContentText(result.Content)
	var value any
	if json.Unmarshal([]byte(text), &value) == nil {
		return value, nil
	}
	return text, nil
}

func executorContentText(content []struct {
	Type string `json:"type"`
	Text string `json:"text"`
}) string {
	parts := make([]string, 0, len(content))
	for _, item := range content {
		if item.Type == "text" && item.Text != "" {
			parts = append(parts, item.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func (c *executorMCPClient) call(ctx context.Context, method string, params any, result any) error {
	id := c.nextID
	c.nextID++
	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	}
	response, err := c.send(ctx, payload, true)
	if err != nil {
		return err
	}
	if response.Error != nil {
		return response.Error
	}
	if len(response.Result) == 0 {
		return errors.New("Executor MCP returned no result")
	}
	if err := json.Unmarshal(response.Result, result); err != nil {
		return fmt.Errorf("decode Executor MCP %s result: %w", method, err)
	}
	return nil
}

func (c *executorMCPClient) notify(ctx context.Context, method string, params any) error {
	_, err := c.send(ctx, map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	}, false)
	return err
}

func (c *executorMCPClient) send(ctx context.Context, payload any, expectResponse bool) (executorRPCResponse, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return executorRPCResponse{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(encoded))
	if err != nil {
		return executorRPCResponse{}, err
	}
	request.Header.Set("Authorization", "Bearer "+c.apiKey)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	if c.sessionID != "" {
		request.Header.Set("Mcp-Session-Id", c.sessionID)
	}
	response, err := c.client.Do(request)
	if err != nil {
		return executorRPCResponse{}, err
	}
	defer response.Body.Close()
	if sessionID := response.Header.Get("Mcp-Session-Id"); sessionID != "" {
		c.sessionID = sessionID
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return executorRPCResponse{}, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return executorRPCResponse{}, fmt.Errorf("Executor MCP returned %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	if !expectResponse {
		return executorRPCResponse{}, nil
	}
	message, err := executorMCPMessage(response.Header.Get("Content-Type"), body)
	if err != nil {
		return executorRPCResponse{}, err
	}
	var rpcResponse executorRPCResponse
	if err := json.Unmarshal(message, &rpcResponse); err != nil {
		return executorRPCResponse{}, fmt.Errorf("decode Executor MCP response: %w", err)
	}
	return rpcResponse, nil
}

func executorMCPMessage(contentType string, body []byte) ([]byte, error) {
	if !strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		if len(bytes.TrimSpace(body)) == 0 {
			return nil, errors.New("Executor MCP returned an empty response")
		}
		return body, nil
	}
	var data []string
	for line := range strings.SplitSeq(string(body), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			if len(data) > 0 {
				return []byte(strings.Join(data, "\n")), nil
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if len(data) > 0 {
		return []byte(strings.Join(data, "\n")), nil
	}
	return nil, errors.New("Executor MCP event stream contained no data")
}

func writeExecutorResult(writer io.Writer, result any, jsonOutput bool) error {
	if text, ok := result.(string); ok && !jsonOutput {
		_, err := fmt.Fprintln(writer, text)
		return err
	}
	encoder := json.NewEncoder(writer)
	if !jsonOutput {
		encoder.SetIndent("", "  ")
	}
	return encoder.Encode(result)
}

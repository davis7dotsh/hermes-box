package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type CLI struct {
	deps    Dependencies
	stdin   io.Reader
	stdout  io.Writer
	stderr  io.Writer
	environ []string
}

type globalOptions struct {
	config string
	home   string
	json   bool
	quiet  bool
	// v2 currently emits no ANSI, but the compatibility flag remains stable so
	// callers do not need to detect when styled output is introduced.
	noColor bool
	help    bool
}

type invocation struct {
	global  globalOptions
	command string
	args    []string
}

func New(deps Dependencies, stdin io.Reader, stdout, stderr io.Writer, environ []string) *CLI {
	if deps.Clock == nil {
		deps.Clock = systemClock{}
	}
	return &CLI{deps: deps, stdin: stdin, stdout: stdout, stderr: stderr, environ: environ}
}

func (c *CLI) Run(ctx context.Context, args []string) int {
	inv, err := parseInvocation(args, c.environ)
	if err != nil {
		return c.writeFailure(inv, err)
	}
	if inv.command == "help" || inv.global.help || inv.command == "completion" {
		if err := c.runTextCommand(inv); err != nil {
			return c.writeFailure(inv, err)
		}
		return 0
	}
	// Version is a host capability query. It must remain available before a
	// repository lock has been promoted and from directories that contain no
	// Hermes Box configuration at all.
	if inv.command == "version" {
		if len(inv.args) != 0 {
			return c.writeFailure(inv, apiError("invalid_input", "version received an invalid number of arguments", 2, nil))
		}
		if c.deps.Operations == nil {
			return c.writeFailure(inv, apiError("preflight_failed", "Hermes Box runtime adapter is not configured", 1, nil))
		}
		result, err := c.deps.Operations.Version(ctx, Definition{Home: inv.global.home})
		if err != nil {
			return c.writeFailure(inv, err)
		}
		if err := c.writeSuccess(inv, "", result); err != nil {
			fmt.Fprintf(c.stderr, "[hermes-box] ERROR: %v\n", err)
			return 1
		}
		return 0
	}
	if c.deps.Loader == nil || c.deps.Operations == nil || c.deps.Backups == nil || c.deps.Locker == nil {
		return c.writeFailure(inv, apiError("preflight_failed", "Hermes Box runtime adapters are not configured", 1, nil))
	}
	definition, err := c.deps.Loader.Load(ctx, LoadRequest{
		ConfigPath: inv.global.config,
		Home:       inv.global.home,
		Environ:    c.environ,
		Command:    inv.command,
	})
	if err != nil {
		return c.writeFailure(inv, apiError("invalid_input", err.Error(), 2, err))
	}
	result, status, err := c.dispatch(ctx, definition, inv)
	if status >= 0 {
		return status
	}
	if err != nil {
		return c.writeFailureWithBox(inv, definition.Name, err)
	}
	if err := c.writeSuccess(inv, definition.Name, result); err != nil {
		fmt.Fprintf(c.stderr, "[hermes-box] ERROR: %v\n", err)
		return 1
	}
	return 0
}

func parseInvocation(args, environ []string) (invocation, error) {
	inv := invocation{command: "help"}
	for _, item := range environ {
		key, value, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		switch key {
		case "HERMES_BOX_CONFIG":
			inv.global.config = value
		case "HERMES_BOX_HOME":
			inv.global.home = value
		case "NO_COLOR":
			inv.global.noColor = true
		}
	}
	if inv.global.config == "" {
		inv.global.config = "hermes-box.yaml"
	}
	if inv.global.home == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return inv, apiError("invalid_input", "resolve home directory: "+err.Error(), 2, err)
		}
		inv.global.home = filepath.Join(home, ".hermes-box")
	}
	index := 0
	for index < len(args) {
		arg := args[index]
		if arg == "--" || !strings.HasPrefix(arg, "-") {
			break
		}
		switch arg {
		case "--config":
			index++
			if index >= len(args) || args[index] == "" {
				return inv, apiError("invalid_input", "--config requires a path", 2, nil)
			}
			inv.global.config = args[index]
		case "--json":
			inv.global.json = true
		case "--quiet":
			inv.global.quiet = true
		case "--no-color":
			inv.global.noColor = true
		case "-h", "--help":
			inv.global.help = true
		default:
			return inv, apiError("invalid_input", "unknown global flag: "+arg, 2, nil)
		}
		index++
	}
	if index < len(args) && args[index] == "--" {
		index++
	}
	if index < len(args) {
		inv.command = args[index]
		index++
	}
	inv.args = append([]string(nil), args[index:]...)
	if inv.global.help && inv.command != "help" {
		inv.args = []string{inv.command}
		inv.command = "help"
	}
	if inv.global.json && jsonUnsupported(inv) {
		return inv, apiError("invalid_input", "--json is not supported for "+inv.command, 2, nil)
	}
	return inv, nil
}

func jsonUnsupported(inv invocation) bool {
	if inv.command == "ssh" || inv.command == "exec" || inv.command == "completion" || inv.command == "help" {
		return true
	}
	return inv.command == "logs" && contains(inv.args, "-f")
}

func (c *CLI) writeSuccess(inv invocation, box string, result any) error {
	if inv.global.json {
		return json.NewEncoder(c.stdout).Encode(map[string]any{
			"schema": Schema, "ok": true, "command": inv.command, "box": box, "result": result,
		})
	}
	if inv.global.quiet {
		return nil
	}
	if result == nil {
		return nil
	}
	return c.writeHumanSuccess(inv, box, result)
}

func (c *CLI) writeHumanSuccess(inv invocation, box string, result any) error {
	value, ok := result.(map[string]any)
	if !ok {
		encoded, err := json.Marshal(result)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(encoded, &value); err != nil {
			return err
		}
	}
	line := func(format string, args ...any) error {
		_, err := fmt.Fprintf(c.stdout, format+"\n", args...)
		return err
	}
	switch inv.command {
	case "create":
		return line("Box %s created and healthy.", box)
	case "start", "restore", "rebuild":
		return line("Box %s is running and healthy.", box)
	case "stop":
		return line("Box %s is stopped.", box)
	case "status":
		state, _ := value["state"].(string)
		if err := line("Box %s: %s", box, state); err != nil {
			return err
		}
		if setup := stringSlice(value["setup_required"]); len(setup) > 0 {
			if err := line("Setup required: %s", strings.Join(setup, ", ")); err != nil {
				return err
			}
		}
		components, _ := value["components"].(map[string]any)
		drift := make([]string, 0, len(components))
		for name, raw := range components {
			component, _ := raw.(map[string]any)
			desired, _ := component["desired"].(string)
			applied, _ := component["applied"].(string)
			componentState, _ := component["state"].(string)
			if componentState == "drifted" || desired != "" && applied != "" && desired != applied {
				drift = append(drift, fmt.Sprintf("%s %s -> %s", name, applied, desired))
			}
		}
		sort.Strings(drift)
		if len(drift) > 0 {
			if err := line("Reviewed lock drift: %s", strings.Join(drift, ", ")); err != nil {
				return err
			}
		}
		candidates, failures := humanUpdateCandidates(value["updates"])
		if len(candidates) > 0 {
			if err := line("Upstream candidates (review and qualification required): %s", strings.Join(candidates, ", ")); err != nil {
				return err
			}
		}
		if len(failures) > 0 {
			return line("Upstream checks unavailable: %s", strings.Join(failures, ", "))
		}
		return nil
	case "open":
		return line("Opened %v", value["url"])
	case "setup":
		return line("Executor connected to Hermes.")
	case "update":
		changed := stringSlice(value["changed"])
		if len(changed) == 0 {
			return line("All selected components already match the reviewed lock.")
		}
		return line("Updated: %s", strings.Join(changed, ", "))
	case "rollback":
		return line("Rolled back %v to %v.", value["component"], value["current"])
	case "backup":
		return line("Backup verified: %v", value["archive"])
	case "doctor":
		if healthy, _ := value["healthy"].(bool); healthy {
			return line("Box %s passed all diagnostics.", box)
		}
		return line("Box %s has unhealthy diagnostics.", box)
	case "key":
		return line("Backup identity exported to %v", value["path"])
	case "destroy":
		return line("Box %s removed; backups and its Keychain identity were preserved.", box)
	case "version":
		return line("Hermes Box %v (Lima %v, config schema %v, lock schema %v)", value["cli"], value["lima"], value["config_schema"], value["lock_schema"])
	}
	return nil
}

func humanUpdateCandidates(value any) ([]string, []string) {
	updates, ok := value.([]any)
	if !ok {
		return nil, nil
	}
	candidates := make([]string, 0, len(updates))
	failures := make([]string, 0, len(updates))
	for _, raw := range updates {
		update, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name, _ := update["component"].(string)
		candidate, _ := update["candidate"].(string)
		if name == "" {
			continue
		}
		if message, _ := update["error"].(string); message != "" {
			failures = append(failures, name+": "+message)
			continue
		}
		if candidate != "" {
			candidates = append(candidates, name+" "+candidate)
		}
	}
	sort.Strings(candidates)
	sort.Strings(failures)
	return candidates, failures
}

func stringSlice(value any) []string {
	switch values := value.(type) {
	case []string:
		return values
	case []any:
		result := make([]string, 0, len(values))
		for _, item := range values {
			if text, ok := item.(string); ok {
				result = append(result, text)
			}
		}
		return result
	default:
		return nil
	}
}

func (c *CLI) writeFailure(inv invocation, err error) int {
	return c.writeFailureWithBox(inv, "", err)
}

func (c *CLI) writeFailureWithBox(inv invocation, box string, err error) int {
	api := classify(err)
	if inv.global.json {
		_ = json.NewEncoder(c.stdout).Encode(map[string]any{
			"schema": Schema, "ok": false, "command": inv.command, "box": box, "error": api,
		})
	}
	fmt.Fprintf(c.stderr, "[hermes-box] ERROR: %s\n", api.Message)
	if api.Recovery != "" {
		fmt.Fprintf(c.stderr, "[hermes-box] RECOVERY: %s\n", api.Recovery)
	}
	if api.Status == 0 {
		return 1
	}
	return api.Status
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

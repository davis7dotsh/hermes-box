package app

import (
	"fmt"
	"strings"
)

func (c *CLI) runTextCommand(inv invocation) error {
	switch inv.command {
	case "help":
		if len(inv.args) > 1 {
			return apiError("invalid_input", "help accepts at most one command", 2, nil)
		}
		text := usage
		if len(inv.args) == 1 {
			var ok bool
			text, ok = commandHelp[inv.args[0]]
			if !ok {
				return apiError("invalid_input", "unknown command: "+inv.args[0], 2, nil)
			}
		}
		_, err := fmt.Fprint(c.stdout, text)
		return err
	case "completion":
		if len(inv.args) != 1 {
			return apiError("invalid_input", "usage: hermes-box completion [bash|zsh|fish]", 2, nil)
		}
		script, ok := completions[inv.args[0]]
		if !ok {
			return apiError("invalid_input", "completion shell must be bash, zsh, or fish", 2, nil)
		}
		_, err := fmt.Fprint(c.stdout, script)
		return err
	default:
		return apiError("invalid_input", "unknown text command: "+inv.command, 2, nil)
	}
}

const usage = `Usage: hermes-box [GLOBAL FLAGS] COMMAND [ARGS]

Global flags:
  --config PATH    Select hermes-box.yaml
  --json           Emit one JSON result object
  --quiet          Suppress progress and human result output
  --no-color       Disable ANSI styling
  -h, --help       Show contextual help

Commands:
  create           Create, provision, verify, and back up a fresh box
  start            Start applied releases without updating them
  stop             Stop services, sync data, and stop the VM
  ssh              Attach the persistent main tmux session
  exec -- CMD      Run a noninteractive command as agent
  status [--check] Show health, versions, drift, and setup requirements
  logs             Show Hermes, Executor, or recovery-service logs
  open executor    Open the loopback-only Executor portal
  setup executor   Connect Executor to Hermes using a protected token
  update TARGET    Apply reviewed lock drift transactionally
  rollback TARGET  Activate the one retained previous release
  backup [LABEL]   Create a verified encrypted recovery bundle
  restore          Restore an absent box from a recovery bundle
  rebuild          Replace the disposable root and preserve data
  doctor           Run bounded read-only diagnostics
  key export PATH  Export the backup decryption identity
  destroy          Final-backup-gated removal of VM and data disk
  completion       Print shell completion
  version          Print CLI and schema versions
  help [COMMAND]   Show help
`

var commandHelp = map[string]string{
	"create":     "Usage: hermes-box create\n",
	"start":      "Usage: hermes-box start\nStarts applied releases. It never installs updates.\n",
	"stop":       "Usage: hermes-box stop\n",
	"ssh":        "Usage: hermes-box ssh\n",
	"exec":       "Usage: hermes-box exec -- COMMAND [ARGS...]\n",
	"status":     "Usage: hermes-box status [--check]\n",
	"logs":       "Usage: hermes-box logs [hermes|executor|recovery] [-f] [-n LINES]\n",
	"open":       "Usage: hermes-box open executor\n",
	"setup":      "Usage: hermes-box setup executor [--token-stdin]\n",
	"update":     "Usage: hermes-box update [claude|codex|hermes|executor|node|uv|all]\n",
	"rollback":   "Usage: hermes-box rollback [claude|codex|hermes|executor|node|uv]\n",
	"backup":     "Usage: hermes-box backup [LABEL]\n",
	"restore":    "Usage: hermes-box restore BACKUP --identity PATH [--lock PATH]\n",
	"rebuild":    "Usage: hermes-box rebuild\n",
	"doctor":     "Usage: hermes-box doctor\n",
	"key":        "Usage: hermes-box key export PATH\n",
	"destroy":    "Usage: hermes-box destroy [--force]\n",
	"completion": "Usage: hermes-box completion [bash|zsh|fish]\n",
	"version":    "Usage: hermes-box version\n",
}

var completions = map[string]string{
	"bash": "complete -W '" + strings.Join(commandNames(), " ") + "' hermes-box\n",
	"zsh":  "compctl -k '(" + strings.Join(commandNames(), " ") + ")' hermes-box\n",
	"fish": "complete -c hermes-box -f -a '" + strings.Join(commandNames(), " ") + "'\n",
}

func commandNames() []string {
	return []string{
		"create", "start", "stop", "ssh", "exec", "status", "logs", "open", "setup",
		"update", "rollback", "backup", "restore", "rebuild", "doctor", "key", "destroy",
		"completion", "version", "help",
	}
}

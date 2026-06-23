package guestupdate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/davis7dotsh/hermes-box/internal/component"
)

const ProtocolSchema = 1

type Request struct {
	Schema          int               `json:"schema"`
	Operation       string            `json:"operation"`
	Root            string            `json:"root,omitempty"`
	Components      []component.Spec  `json:"components,omitempty"`
	Component       component.Name    `json:"component,omitempty"`
	Initial         bool              `json:"initial,omitempty"`
	SnapshotReady   bool              `json:"snapshot_ready,omitempty"`
	ReviewedLock    string            `json:"reviewed_lock,omitempty"`
	Argv            []string          `json:"argv,omitempty"`
	Directory       string            `json:"directory,omitempty"`
	Environment     map[string]string `json:"environment,omitempty"`
	BackupPaths     []string          `json:"backup_paths,omitempty"`
	ReplaceExisting bool              `json:"replace_existing,omitempty"`
}

type Response struct {
	Schema int            `json:"schema"`
	OK     bool           `json:"ok"`
	Result any            `json:"result,omitempty"`
	Error  *ProtocolError `json:"error,omitempty"`
}

type ProtocolError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type AppliedState struct {
	Schema     int                       `json:"schema"`
	Components map[component.Name]string `json:"components"`
	UpdatedAt  time.Time                 `json:"updated_at"`
}

type ReleasesState struct {
	Schema     int                                `json:"schema"`
	Components map[component.Name]ReleaseMetadata `json:"components"`
}

// persistedState is the single crash-consistent publication unit for all
// component state. Applied and release metadata must never be written as
// separate files: either readers see the complete old state or the complete
// new state after atomicFile's rename and directory sync.
type persistedState struct {
	Schema   int           `json:"schema"`
	Applied  AppliedState  `json:"applied"`
	Releases ReleasesState `json:"releases"`
}

type ReleaseMetadata struct {
	Current  string   `json:"current,omitempty"`
	Previous string   `json:"previous,omitempty"`
	Retained []string `json:"retained,omitempty"`
}

type Journal struct {
	Schema       int            `json:"schema"`
	Component    component.Name `json:"component"`
	Previous     string         `json:"previous,omitempty"`
	PreviousLock string         `json:"previous_lock,omitempty"`
	Candidate    string         `json:"candidate"`
	Phase        string         `json:"phase"`
	Services     []string       `json:"services,omitempty"`
	CreatedAt    time.Time      `json:"created_at"`
}

type releaseContract struct {
	Fingerprint string            `json:"fingerprint"`
	Health      [][]string        `json:"health,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
}

type pathRestoreJournal struct {
	Schema    int                `json:"schema"`
	Component component.Name     `json:"component"`
	Phase     string             `json:"phase"`
	Staging   string             `json:"staging"`
	Committed bool               `json:"committed"`
	DataRoot  *pathRestoreRoot   `json:"data_root,omitempty"`
	Entries   []pathRestoreEntry `json:"entries"`
}

type pathRestoreRoot struct {
	Before archiveMetadata `json:"before"`
	After  archiveMetadata `json:"after"`
}

type archiveMetadata struct {
	Mode int64 `json:"mode"`
	UID  int   `json:"uid"`
	GID  int   `json:"gid"`
}

type pathRestoreEntry struct {
	Scope       string `json:"scope"`
	Destination string `json:"destination"`
	Old         string `json:"old"`
	HadOriginal bool   `json:"had_original"`
	Installed   bool   `json:"installed"`
	Absent      bool   `json:"absent"`
}

type Status struct {
	Applied        AppliedState    `json:"applied"`
	AppliedLock    string          `json:"applied_lock,omitempty"`
	Releases       ReleasesState   `json:"releases"`
	Pending        *Journal        `json:"pending,omitempty"`
	RestorePending *RestorePending `json:"restore_pending,omitempty"`
}

type RestorePending struct {
	Component component.Name `json:"component,omitempty"`
	Committed bool           `json:"committed"`
}

type StreamResult struct {
	Stream  string `json:"stream"`
	Framing string `json:"framing"`
	Owner   string `json:"owner"`
}

func (result StreamResult) Validate() error {
	if result.Stream != "tar" || result.Framing != "direct" || result.Owner != "guest" {
		return errors.New("backup stream header is invalid")
	}
	return nil
}

type ExecResult struct {
	ExitCode int `json:"exit_code"`
}

func decodeRequest(line []byte) (Request, error) {
	var request Request
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return request, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return request, errors.New("request contains multiple JSON values")
		}
		return request, err
	}
	return request, nil
}

var environmentNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func (request Request) Validate() error {
	if request.Schema != ProtocolSchema {
		return fmt.Errorf("unsupported schema %d", request.Schema)
	}
	if len(request.Root) > 4096 || strings.ContainsRune(request.Root, '\x00') {
		return errors.New("root is invalid")
	}
	if len(request.Components) > len(component.Names()) || len(request.Argv) > 4096 || len(request.BackupPaths) > 32 {
		return errors.New("request collection exceeds schema limits")
	}
	for _, value := range request.Argv {
		if len(value) > 64<<10 || strings.ContainsRune(value, '\x00') {
			return errors.New("exec argument is invalid")
		}
	}
	if len(request.Directory) > 4096 || strings.ContainsRune(request.Directory, '\x00') {
		return errors.New("exec directory is invalid")
	}
	if len(request.Environment) > 128 {
		return errors.New("exec environment exceeds 128 entries")
	}
	for key, value := range request.Environment {
		if !environmentNamePattern.MatchString(key) || len(value) > 64<<10 || strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("exec environment entry %q is invalid", key)
		}
		if key == "PATH" || key == "HOME" || key == "CODEX_HOME" || key == "HERMES_HOME" {
			return fmt.Errorf("exec environment may not override managed variable %q", key)
		}
	}
	for _, value := range request.BackupPaths {
		if len(value) > 4096 || strings.ContainsRune(value, '\x00') {
			return errors.New("backup path is invalid")
		}
	}

	hasApplyFields := len(request.Components) != 0 || request.Initial || request.SnapshotReady || request.ReviewedLock != ""
	hasExecFields := len(request.Argv) != 0 || request.Directory != "" || len(request.Environment) != 0
	hasBackupFields := len(request.BackupPaths) != 0
	hasRestoreFields := request.ReplaceExisting
	switch request.Operation {
	case "apply":
		if len(request.Components) == 0 || request.Component != "" || hasExecFields || hasBackupFields || hasRestoreFields {
			return errors.New("apply request fields do not match the schema")
		}
	case "rollback":
		if !component.Known(request.Component) || len(request.Components) != 0 || request.Initial || hasExecFields || hasBackupFields || hasRestoreFields {
			return errors.New("rollback request fields do not match the schema")
		}
	case "recover", "status":
		if request.Component != "" || hasApplyFields || hasExecFields || hasBackupFields || hasRestoreFields {
			return fmt.Errorf("%s request must not contain operation data", request.Operation)
		}
	case "exec":
		if len(request.Argv) == 0 || request.Component != "" || hasApplyFields || hasBackupFields || hasRestoreFields {
			return errors.New("exec request fields do not match the schema")
		}
	case "backup-stream":
		if request.Component != "" && !component.Known(request.Component) {
			return errors.New("backup-stream component is invalid")
		}
		if hasApplyFields || hasExecFields || hasRestoreFields || (request.Component != "" && hasBackupFields) {
			return errors.New("backup-stream request fields do not match the schema")
		}
	case "restore-stream":
		if request.Component != "" || hasApplyFields || hasExecFields || hasBackupFields {
			return errors.New("restore-stream request must not contain operation data")
		}
	case "restore-paths":
		if !component.Known(request.Component) || hasApplyFields || hasExecFields || hasBackupFields || hasRestoreFields {
			return errors.New("restore-paths request fields do not match the schema")
		}
	default:
		return fmt.Errorf("unknown operation %q", request.Operation)
	}
	return nil
}

func (response Response) Validate() error {
	if response.Schema != ProtocolSchema {
		return errors.New("response schema is invalid")
	}
	if response.OK == (response.Error != nil) {
		return errors.New("response must contain exactly one success or error outcome")
	}
	if response.Error != nil && (response.Error.Code == "" || response.Error.Message == "") {
		return errors.New("response error is incomplete")
	}
	if response.Error != nil {
		switch response.Error.Code {
		case "busy", "integrity_failed", "health_failed", "invalid_input", "external_failed":
		default:
			return fmt.Errorf("response error code %q is invalid", response.Error.Code)
		}
	}
	if stream, ok := response.Result.(StreamResult); ok {
		return stream.Validate()
	}
	return nil
}

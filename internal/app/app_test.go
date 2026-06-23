package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/davis7dotsh/hermes-box/internal/box"
	"github.com/davis7dotsh/hermes-box/internal/component"
	"github.com/davis7dotsh/hermes-box/internal/config"
	"github.com/davis7dotsh/hermes-box/internal/guestupdate"
	"github.com/davis7dotsh/hermes-box/internal/keychain"
	"github.com/davis7dotsh/hermes-box/internal/lima"
	"github.com/davis7dotsh/hermes-box/internal/process"
)

type fakeLoader struct {
	request LoadRequest
	def     Definition
	err     error
}

type captureRunner struct {
	spec process.Spec
}

type secretRunner struct {
	runs       []process.Spec
	outputs    []process.Spec
	disableErr error
	restoreErr error
}

type processResult struct {
	output []byte
	err    error
}

type scriptedProcessRunner struct {
	runs          []process.Spec
	runContextErr []error
	outputs       []process.Spec
	runErrors     []error
	outputResults []processResult
}

func (r *scriptedProcessRunner) Run(ctx context.Context, spec process.Spec) error {
	r.runs = append(r.runs, spec)
	r.runContextErr = append(r.runContextErr, ctx.Err())
	if len(r.outputResults) > 0 && spec.Stdout != nil {
		result := r.outputResults[0]
		r.outputResults = r.outputResults[1:]
		if len(result.output) > 0 {
			if _, err := spec.Stdout.Write(result.output); err != nil {
				return errors.Join(result.err, err)
			}
		}
		return result.err
	}
	if len(r.runErrors) == 0 {
		return nil
	}
	err := r.runErrors[0]
	r.runErrors = r.runErrors[1:]
	return err
}

func (r *scriptedProcessRunner) Output(_ context.Context, spec process.Spec) ([]byte, error) {
	r.outputs = append(r.outputs, spec)
	if len(r.outputResults) == 0 {
		return nil, errors.New("unexpected process output invocation")
	}
	result := r.outputResults[0]
	r.outputResults = r.outputResults[1:]
	return result.output, result.err
}

func (r *secretRunner) Run(_ context.Context, spec process.Spec) error {
	r.runs = append(r.runs, spec)
	if spec.Name == "stty" && reflect.DeepEqual(spec.Args, []string{"-echo"}) {
		return r.disableErr
	}
	if spec.Name == "stty" && len(spec.Args) == 1 && spec.Args[0] != "-echo" {
		return r.restoreErr
	}
	return nil
}

func (r *secretRunner) Output(_ context.Context, spec process.Spec) ([]byte, error) {
	r.outputs = append(r.outputs, spec)
	if spec.Name == "stty" {
		return []byte("deadbeef:1\n"), nil
	}
	return nil, errors.New("unexpected output command")
}

type scriptedLimaResult struct {
	result lima.Result
	err    error
}

type scriptedLimaRunner struct {
	invocations []lima.Invocation
	results     []scriptedLimaResult
}

func (r *scriptedLimaRunner) Run(_ context.Context, invocation lima.Invocation) (lima.Result, error) {
	r.invocations = append(r.invocations, invocation)
	if len(r.results) == 0 {
		return lima.Result{}, errors.New("unexpected Lima invocation")
	}
	result := r.results[0]
	r.results = r.results[1:]
	return result.result, result.err
}

func (r *captureRunner) Run(_ context.Context, spec process.Spec) error {
	r.spec = spec
	_, err := io.WriteString(spec.Stdout, `{"schema":1,"ok":true,"result":{"exit_code":0,"stdout":"ok","stderr":""}}`)
	return err
}
func (r *captureRunner) Output(_ context.Context, spec process.Spec) ([]byte, error) {
	r.spec = spec
	return []byte(`{"schema":1,"ok":true,"result":{"exit_code":0,"stdout":"ok","stderr":""}}`), nil
}

func (f *fakeLoader) Load(_ context.Context, request LoadRequest) (Definition, error) {
	f.request = request
	return f.def, f.err
}

type fakeLocker struct {
	commands []string
	err      error
}

func (f *fakeLocker) Acquire(_ context.Context, _ Definition, command string) (func() error, error) {
	f.commands = append(f.commands, command)
	if f.err != nil {
		return nil, f.err
	}
	return func() error { return nil }, nil
}

type fakeBackups struct {
	calls     []string
	result    BackupResult
	err       error
	latest    *BackupResult
	latestErr error
}

func (f *fakeBackups) Create(_ context.Context, _ Definition, label string) (BackupResult, error) {
	f.calls = append(f.calls, label)
	return f.result, f.err
}

func (f *fakeBackups) LatestVerified(context.Context, Definition) (*BackupResult, error) {
	return f.latest, f.latestErr
}

type fakeOperations struct {
	calls          []string
	ownership      Ownership
	health         Health
	status         Status
	commandStatus  int
	applyResult    map[string]any
	rollbackResult map[string]any
	errAt          map[string]error
	recoveryCtxErr error
	logsOutput     *string
}

func (f *fakeOperations) call(name string) error {
	f.calls = append(f.calls, name)
	return f.errAt[name]
}

func (f *fakeOperations) Preflight(_ context.Context, _ Definition, action string) error {
	return f.call("preflight:" + action)
}
func (f *fakeOperations) ResumeInterruptedMutation(context.Context, Definition) error {
	return f.call("resume-host-operation")
}
func (f *fakeOperations) Ownership(context.Context, Definition) (Ownership, error) {
	return f.ownership, f.call("ownership")
}
func (f *fakeOperations) CreateInfrastructure(context.Context, Definition) error {
	return f.call("create-infrastructure")
}
func (f *fakeOperations) CompleteCreate(context.Context, Definition) error {
	return f.call("complete-create")
}
func (f *fakeOperations) RecreateVM(context.Context, Definition) error { return f.call("recreate-vm") }
func (f *fakeOperations) CleanupCreate(context.Context, Definition) error {
	return f.call("cleanup-create")
}
func (f *fakeOperations) StartVM(context.Context, Definition) error { return f.call("start-vm") }
func (f *fakeOperations) StopVM(context.Context, Definition) error  { return f.call("stop-vm") }
func (f *fakeOperations) RemoveVM(_ context.Context, _ Definition, preserve bool) error {
	return f.call("remove-vm:" + boolString(preserve))
}
func (f *fakeOperations) RemoveAll(context.Context, Definition) error { return f.call("remove-all") }
func (f *fakeOperations) Recover(context.Context, Definition) error   { return f.call("recover") }
func (f *fakeOperations) Apply(_ context.Context, _ Definition, component string) (map[string]any, error) {
	result := f.applyResult
	if result == nil {
		result = map[string]any{"changed": []string{component}, "components": map[string]any{}}
	}
	return result, f.call("apply:" + component)
}
func (f *fakeOperations) Rollback(_ context.Context, _ Definition, component string) (map[string]any, error) {
	result := f.rollbackResult
	if result == nil {
		result = map[string]any{"component": component, "previous": "new", "current": "old", "desired": "new"}
	}
	return result, f.call("rollback:" + component)
}
func (f *fakeOperations) StartServices(context.Context, Definition) error {
	return f.call("start-services")
}
func (f *fakeOperations) StopServices(context.Context, Definition) error {
	return f.call("stop-services")
}
func (f *fakeOperations) SyncData(context.Context, Definition) error { return f.call("sync-data") }
func (f *fakeOperations) Health(context.Context, Definition) (Health, error) {
	return f.health, f.call("health")
}
func (f *fakeOperations) Status(_ context.Context, _ Definition, check bool) (Status, error) {
	return f.status, f.call("status:" + boolString(check))
}
func (f *fakeOperations) SSH(context.Context, Definition, io.Reader, io.Writer, io.Writer) (int, error) {
	return f.commandStatus, f.call("ssh")
}
func (f *fakeOperations) Exec(_ context.Context, _ Definition, args []string, _ io.Reader, _ io.Writer, _ io.Writer) (int, error) {
	return f.commandStatus, f.call("exec:" + strings.Join(args, ","))
}
func (f *fakeOperations) Logs(_ context.Context, _ Definition, target string, lines int, follow bool, stdout, _ io.Writer) error {
	output := "one\ntwo\n"
	if f.logsOutput != nil {
		output = *f.logsOutput
	}
	_, _ = io.WriteString(stdout, output)
	return f.call("logs:" + target + ":" + boolString(follow))
}
func (f *fakeOperations) OpenExecutor(context.Context, Definition) (string, error) {
	return "http://127.0.0.1:4788", f.call("open")
}
func (f *fakeOperations) SetupExecutor(context.Context, Definition, io.Reader, bool) ([]string, error) {
	return []string{"execute", "resume"}, f.call("setup")
}
func (f *fakeOperations) Doctor(context.Context, Definition) ([]map[string]any, error) {
	return []map[string]any{{"name": "host", "ok": true}}, f.call("doctor")
}
func (f *fakeOperations) ExportKey(context.Context, Definition, string) (string, error) {
	return "fingerprint", f.call("export-key")
}
func (f *fakeOperations) CaptureRecoveryState(context.Context, Definition) (RecoveryState, error) {
	return RecoveryState{AppliedLock: "applied"}, f.call("capture-recovery")
}
func (f *fakeOperations) PrepareRebuildRecovery(_ context.Context, _ Definition, recovery RecoveryState, _ BackupResult) (RecoveryState, error) {
	return recovery, f.call("prepare-rebuild-recovery")
}
func (f *fakeOperations) CompleteRebuild(context.Context, Definition) error {
	return f.call("complete-rebuild")
}
func (f *fakeOperations) Restore(context.Context, Definition, string, string, string) error {
	return f.call("restore")
}
func (f *fakeOperations) RestoreRebuildData(context.Context, Definition, BackupResult) error {
	return f.call("restore-rebuild-data")
}
func (f *fakeOperations) RecoverRebuild(ctx context.Context, _ Definition, _ RecoveryState, _ BackupResult) error {
	f.recoveryCtxErr = ctx.Err()
	return f.call("recover-rebuild")
}
func (f *fakeOperations) Version(context.Context, Definition) (VersionResult, error) {
	return VersionResult{CLI: "test", ConfigSchema: 1, LockSchema: 1}, f.call("version")
}

func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func newTestCLI() (*CLI, *fakeLoader, *fakeOperations, *fakeBackups, *fakeLocker, *bytes.Buffer, *bytes.Buffer) {
	loader := &fakeLoader{def: Definition{Name: "main"}}
	operations := &fakeOperations{
		ownership: Ownership{Exists: true, Owned: true, Running: true},
		health:    Health{Healthy: true, Components: map[string]any{}, Ports: map[string]any{}},
		status: Status{
			State: "running", Healthy: true, SetupRequired: []string{}, Components: map[string]any{},
			Storage: map[string]any{}, Ports: map[string]any{}, Updates: []any{},
		},
		errAt: make(map[string]error),
	}
	backups := &fakeBackups{result: BackupResult{Archive: "backup.age", Envelope: "backup.json", ArchiveSHA256: "abc"}}
	locker := &fakeLocker{}
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	cli := New(Dependencies{Loader: loader, Operations: operations, Backups: backups, Locker: locker}, strings.NewReader("token"), stdout, stderr, []string{"HOME=/tmp/home"})
	return cli, loader, operations, backups, locker, stdout, stderr
}

func TestGlobalsAreParsedBeforeLoadingConfiguration(t *testing.T) {
	cli, loader, _, _, _, _, _ := newTestCLI()
	if status := cli.Run(context.Background(), []string{"--config", "/tmp/box.yaml", "--quiet", "status"}); status != 0 {
		t.Fatalf("status = %d", status)
	}
	if loader.request.ConfigPath != "/tmp/box.yaml" {
		t.Fatalf("config path = %q", loader.request.ConfigPath)
	}
}

func TestNoColorCompatibilityFlagIsAcceptedWithoutANSIOutput(t *testing.T) {
	cli, _, _, _, _, stdout, _ := newTestCLI()
	if status := cli.Run(context.Background(), []string{"--no-color", "status"}); status != 0 {
		t.Fatalf("status = %d", status)
	}
	if strings.Contains(stdout.String(), "\x1b[") {
		t.Fatalf("no-color output contained ANSI: %q", stdout.String())
	}
}

func TestInvalidGlobalFlagDoesNotLoadConfiguration(t *testing.T) {
	cli, loader, _, _, _, _, _ := newTestCLI()
	if status := cli.Run(context.Background(), []string{"--wat", "status"}); status != 2 {
		t.Fatalf("status = %d", status)
	}
	if loader.request.ConfigPath != "" {
		t.Fatal("configuration loaded before global parsing completed")
	}
}

func TestVersionDoesNotLoadConfigurationOrRequireRepositoryLock(t *testing.T) {
	cli, loader, operations, _, _, stdout, _ := newTestCLI()
	loader.err = errors.New("missing hermes-box.lock")
	if status := cli.Run(context.Background(), []string{"version"}); status != 0 {
		t.Fatalf("status = %d", status)
	}
	if loader.request.Command != "" {
		t.Fatalf("version loaded configuration: %#v", loader.request)
	}
	if !reflect.DeepEqual(operations.calls, []string{"version"}) {
		t.Fatalf("calls = %v", operations.calls)
	}
	if !strings.Contains(stdout.String(), "Hermes Box test") {
		t.Fatalf("output = %q", stdout.String())
	}
}

func TestMutationLockClassifiesOnlyContentionAsBusy(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		code string
	}{
		{name: "contention", err: &box.BusyError{Name: "main"}, code: "busy"},
		{name: "filesystem failure", err: errors.New("lock directory is read-only"), code: "external_failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cli, _, _, _, locker, stdout, _ := newTestCLI()
			locker.err = test.err
			if status := cli.Run(context.Background(), []string{"--json", "start"}); status != 1 {
				t.Fatalf("status = %d", status)
			}
			if !strings.Contains(stdout.String(), `"code":"`+test.code+`"`) {
				t.Fatalf("output = %s", stdout.String())
			}
		})
	}
}

func TestStartRecoversButNeverAppliesDesiredLock(t *testing.T) {
	cli, _, operations, _, locker, _, _ := newTestCLI()
	if status := cli.Run(context.Background(), []string{"--quiet", "start"}); status != 0 {
		t.Fatalf("status = %d", status)
	}
	want := []string{"resume-host-operation", "preflight:start", "ownership", "start-vm", "recover", "start-services", "health"}
	if !reflect.DeepEqual(operations.calls, want) {
		t.Fatalf("calls = %v, want %v", operations.calls, want)
	}
	if !reflect.DeepEqual(locker.commands, []string{"start"}) {
		t.Fatalf("locks = %v", locker.commands)
	}
}

func TestCreateCleansOnlyResourcesCreatedByInvocationOnFailure(t *testing.T) {
	cli, _, operations, _, _, _, _ := newTestCLI()
	operations.ownership.Exists = false
	operations.errAt["apply:all"] = errors.New("install failed")
	if status := cli.Run(context.Background(), []string{"create"}); status != 1 {
		t.Fatalf("status = %d", status)
	}
	want := []string{"resume-host-operation", "preflight:create", "ownership", "create-infrastructure", "apply:all", "cleanup-create"}
	if !reflect.DeepEqual(operations.calls, want) {
		t.Fatalf("calls = %v, want %v", operations.calls, want)
	}
}

func TestCreatePrintsExactHandoffAfterVerifiedBackup(t *testing.T) {
	cli, _, operations, backups, _, stdout, _ := newTestCLI()
	operations.ownership.Exists = false
	if status := cli.Run(context.Background(), []string{"create"}); status != 0 {
		t.Fatalf("status = %d", status)
	}
	if !strings.HasPrefix(stdout.String(), firstRunHandoff) {
		t.Fatalf("output did not begin with handoff:\n%s", stdout.String())
	}
	if !reflect.DeepEqual(backups.calls, []string{"initial"}) {
		t.Fatalf("backup calls = %v", backups.calls)
	}
}

func TestCreateKeepsHealthyBoxAfterVerifiedBackupWhenCompletionFails(t *testing.T) {
	cli, _, operations, backups, _, _, _ := newTestCLI()
	operations.ownership.Exists = false
	operations.errAt["complete-create"] = errors.New("journal directory fsync failed")
	if status := cli.Run(context.Background(), []string{"create"}); status != 1 {
		t.Fatalf("status = %d", status)
	}
	if !reflect.DeepEqual(backups.calls, []string{"initial"}) {
		t.Fatalf("backup calls = %v", backups.calls)
	}
	if contains(operations.calls, "cleanup-create") {
		t.Fatalf("healthy recoverable box was torn down: %v", operations.calls)
	}
}

func TestDestroyAbortsWhenFinalBackupFails(t *testing.T) {
	cli, _, operations, backups, _, _, stderr := newTestCLI()
	backups.err = errors.New("cannot verify")
	if status := cli.Run(context.Background(), []string{"destroy"}); status != 1 {
		t.Fatalf("status = %d", status)
	}
	if contains(operations.calls, "remove-all") {
		t.Fatal("destroy removed resources after backup failure")
	}
	if !strings.Contains(stderr.String(), "refusing to destroy") {
		t.Fatalf("stderr = %s", stderr.String())
	}
}

func TestDestroyForceIsExplicitBackupEscapeHatch(t *testing.T) {
	cli, _, operations, backups, _, _, _ := newTestCLI()
	backups.err = errors.New("cannot verify")
	if status := cli.Run(context.Background(), []string{"--quiet", "destroy", "--force"}); status != 0 {
		t.Fatalf("status = %d", status)
	}
	if len(backups.calls) != 0 || !contains(operations.calls, "remove-all") {
		t.Fatalf("backup calls = %v, operation calls = %v", backups.calls, operations.calls)
	}
}

func TestUnownedResourceIsNeverAdopted(t *testing.T) {
	cli, _, operations, _, _, _, _ := newTestCLI()
	operations.ownership.Owned = false
	if status := cli.Run(context.Background(), []string{"start"}); status != 1 {
		t.Fatalf("status = %d", status)
	}
	if contains(operations.calls, "start-vm") {
		t.Fatal("unowned VM was started")
	}
}

func TestExecPreservesArgumentsAndRemoteStatus(t *testing.T) {
	cli, _, operations, _, _, _, _ := newTestCLI()
	operations.commandStatus = 37
	operations.errAt["exec:printf,%s,a b"] = errors.New("exit status 37")
	status := cli.Run(context.Background(), []string{"exec", "--", "printf", "%s", "a b"})
	if status != 37 {
		t.Fatalf("status = %d", status)
	}
	if !contains(operations.calls, "exec:printf,%s,a b") {
		t.Fatalf("calls = %v", operations.calls)
	}
}

func TestJSONSuccessIsOneStableEnvelope(t *testing.T) {
	cli, _, _, _, _, stdout, _ := newTestCLI()
	if status := cli.Run(context.Background(), []string{"--json", "status"}); status != 0 {
		t.Fatalf("status = %d", status)
	}
	output := stdout.String()
	for _, expected := range []string{`"schema":1`, `"ok":true`, `"command":"status"`, `"box":"main"`, `"result"`} {
		if !strings.Contains(output, expected) {
			t.Fatalf("JSON missing %s: %s", expected, output)
		}
	}
	if strings.Count(strings.TrimSpace(output), "\n") != 0 {
		t.Fatalf("expected one JSON line: %q", output)
	}
}

func TestJSONLogsUseEmptyArrayWhenThereIsNoOutput(t *testing.T) {
	cli, _, operations, _, _, stdout, _ := newTestCLI()
	empty := ""
	operations.logsOutput = &empty
	if status := cli.Run(context.Background(), []string{"--json", "logs", "recovery"}); status != 0 {
		t.Fatalf("status = %d", status)
	}
	var envelope struct {
		Result struct {
			Lines []string `json:"lines"`
		} `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Result.Lines == nil || len(envelope.Result.Lines) != 0 {
		t.Fatalf("empty logs = %#v, want non-nil empty array", envelope.Result.Lines)
	}
}

func TestOperationalJSONSchemasMatchGoldenResults(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		configure func(*fakeOperations)
		want      string
	}{
		{
			name: "status", args: []string{"--json", "status"},
			configure: func(operations *fakeOperations) {
				operations.status = Status{
					State: "running", Healthy: true, SetupRequired: []string{"codex"},
					Components: map[string]any{"codex": map[string]any{
						"desired": "2", "applied": "1", "running": "1", "previous": nil, "state": "drifted",
					}},
					Storage: map[string]any{"data": "/data"}, Ports: map[string]any{"executor": 4788},
					LastBackup: map[string]any{"archive": "backup.age"}, Updates: []any{},
				}
			},
			want: "{\"box\":\"main\",\"command\":\"status\",\"ok\":true,\"result\":{\"state\":\"running\",\"healthy\":true,\"setup_required\":[\"codex\"],\"components\":{\"codex\":{\"applied\":\"1\",\"desired\":\"2\",\"previous\":null,\"running\":\"1\",\"state\":\"drifted\"}},\"storage\":{\"data\":\"/data\"},\"ports\":{\"executor\":4788},\"last_backup\":{\"archive\":\"backup.age\"},\"updates\":[]},\"schema\":1}\n",
		},
		{
			name: "update", args: []string{"--json", "update", "codex"},
			configure: func(operations *fakeOperations) {
				operations.applyResult = map[string]any{
					"changed": []string{"codex"},
					"components": map[string]any{"codex": map[string]any{
						"desired": "2", "applied": "2", "running": "2", "previous": "1", "state": "healthy",
					}},
				}
			},
			want: "{\"box\":\"main\",\"command\":\"update\",\"ok\":true,\"result\":{\"changed\":[\"codex\"],\"components\":{\"codex\":{\"applied\":\"2\",\"desired\":\"2\",\"previous\":\"1\",\"running\":\"2\",\"state\":\"healthy\"}},\"failed\":null},\"schema\":1}\n",
		},
		{
			name: "rollback", args: []string{"--json", "rollback", "codex"},
			configure: func(operations *fakeOperations) {
				operations.rollbackResult = map[string]any{"component": "codex", "previous": "2", "current": "1", "desired": "2"}
			},
			want: "{\"box\":\"main\",\"command\":\"rollback\",\"ok\":true,\"result\":{\"component\":\"codex\",\"current\":\"1\",\"desired\":\"2\",\"previous\":\"2\"},\"schema\":1}\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cli, _, operations, _, _, stdout, _ := newTestCLI()
			test.configure(operations)
			if status := cli.Run(context.Background(), test.args); status != 0 {
				t.Fatalf("status = %d", status)
			}
			if got := stdout.String(); got != test.want {
				t.Fatalf("JSON result mismatch\ngot:  %s\nwant: %s", got, test.want)
			}
		})
	}
}

func TestHumanStatusIsConciseOperatorText(t *testing.T) {
	cli, _, operations, _, _, stdout, _ := newTestCLI()
	operations.status.SetupRequired = []string{"codex", "executor"}
	if status := cli.Run(context.Background(), []string{"status"}); status != 0 {
		t.Fatalf("status = %d", status)
	}
	want := "Box main: running\nSetup required: codex, executor\n"
	if stdout.String() != want {
		t.Fatalf("output = %q, want %q", stdout.String(), want)
	}
}

func TestHumanStatusSurfacesReviewedLockDrift(t *testing.T) {
	cli, _, operations, _, _, stdout, _ := newTestCLI()
	operations.status.Components = map[string]any{
		"codex": map[string]any{"desired": "2", "applied": "1", "state": "drifted"},
		"node":  map[string]any{"desired": "24", "applied": "24", "state": "healthy"},
	}
	if status := cli.Run(context.Background(), []string{"status"}); status != 0 {
		t.Fatalf("status = %d", status)
	}
	if got := stdout.String(); !strings.Contains(got, "Reviewed lock drift: codex 1 -> 2\n") {
		t.Fatalf("output = %q", got)
	}
}

func TestHumanStatusCheckPrintsUnqualifiedUpstreamCandidatesAndFailures(t *testing.T) {
	cli, _, operations, _, _, stdout, _ := newTestCLI()
	operations.status.Updates = []any{
		map[string]any{"component": "codex", "current": "1", "candidate": "2", "qualified": false},
		map[string]any{"component": "claude", "current": "1", "candidate": "", "qualified": false, "error": "registry unavailable"},
	}
	if status := cli.Run(context.Background(), []string{"status", "--check"}); status != 0 {
		t.Fatalf("status = %d", status)
	}
	for _, expected := range []string{
		"Upstream candidates (review and qualification required): codex 2\n",
		"Upstream checks unavailable: claude: registry unavailable\n",
	} {
		if !strings.Contains(stdout.String(), expected) {
			t.Fatalf("output %q does not contain %q", stdout.String(), expected)
		}
	}
}

func TestStoppedComponentStatusUsesDurableAppliedLockWithoutGuest(t *testing.T) {
	def := Definition{Name: "main", Home: t.TempDir()}
	desired := validClosureLock()
	applied := desired
	desired.Codex.Version = "2"
	def.Bundle.Lock = desired
	if err := writeLock(hostAppliedLockPath(def), applied); err != nil {
		t.Fatal(err)
	}
	components := stoppedComponentStatus(def)
	codex := components["codex"].(map[string]any)
	if codex["desired"] != "2" || codex["applied"] != "1" || codex["running"] != "" || codex["state"] != "drifted" {
		t.Fatalf("stopped Codex status = %#v", codex)
	}
	node := components["node"].(map[string]any)
	if node["state"] != "healthy" {
		t.Fatalf("stopped Node status = %#v", node)
	}
}

func TestStoppedStatusReportsLocalDriftAndChecksUpstreamWithoutStartingGuest(t *testing.T) {
	stoppedVM := []byte("{\"name\":\"main\",\"status\":\"Stopped\",\"arch\":\"aarch64\",\"vmType\":\"vz\"}\n")
	disk := []byte("{\"name\":\"main-data\",\"size\":53687091200,\"format\":\"raw\",\"dir\":\"/tmp/main-data\",\"instance\":\"main\"}\n")
	runner := &scriptedLimaRunner{results: []scriptedLimaResult{
		{result: lima.Result{Stdout: stoppedVM}}, {result: lima.Result{Stdout: disk}},
	}}
	def, _ := boundDefinition(t, runner)
	desired := validClosureLock()
	applied := desired
	desired.Codex.Version = "2"
	def.Bundle.Lock = desired
	if err := writeLock(hostAppliedLockPath(def), applied); err != nil {
		t.Fatal(err)
	}
	discovered := []any{map[string]any{"component": "codex", "candidate": "3", "qualified": false}}
	operations := &defaultOperations{
		limaRunner: runner,
		releaseDiscovery: func(context.Context, Definition) []any {
			return discovered
		},
	}
	status, err := operations.Status(context.Background(), def, true)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "stopped" || !reflect.DeepEqual(status.Updates, discovered) {
		t.Fatalf("stopped status = %#v", status)
	}
	codex := status.Components["codex"].(map[string]any)
	if codex["desired"] != "2" || codex["applied"] != "1" || codex["state"] != "drifted" {
		t.Fatalf("stopped Codex status = %#v", codex)
	}
	if len(runner.invocations) != 2 {
		t.Fatalf("stopped status unexpectedly contacted guest: %#v", runner.invocations)
	}
}

func TestJSONFailureUsesStableCodeAndExitTwoForUsage(t *testing.T) {
	cli, _, _, _, _, stdout, _ := newTestCLI()
	if status := cli.Run(context.Background(), []string{"--json", "update", "ubuntu"}); status != 2 {
		t.Fatalf("status = %d", status)
	}
	if !strings.Contains(stdout.String(), `"code":"invalid_input"`) {
		t.Fatalf("output = %s", stdout.String())
	}
}

func TestRestoreRefusesExistingDestination(t *testing.T) {
	cli, _, operations, _, _, _, _ := newTestCLI()
	status := cli.Run(context.Background(), []string{"restore", "backup.age", "--identity", "key.txt"})
	if status != 1 {
		t.Fatalf("status = %d", status)
	}
	if contains(operations.calls, "restore") {
		t.Fatal("restore replaced an existing destination")
	}
}

func TestRestoreDispatchesWithoutAdjacentRepositoryLock(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "hermes-box.yaml")
	configData := `schema: 1
name: restored
vm:
  cpus: 4
  memory: 8GiB
  root_disk: 30GiB
  data_disk: 50GiB
ports:
  executor: 4789
backup:
  keep: 5
`
	if err := os.WriteFile(configPath, []byte(configData), 0o600); err != nil {
		t.Fatal(err)
	}
	operations := &fakeOperations{
		ownership: Ownership{}, health: Health{Healthy: true, Components: map[string]any{}}, errAt: map[string]error{},
	}
	backups := &fakeBackups{}
	locker := &fakeLocker{}
	var stdout, stderr bytes.Buffer
	cli := New(Dependencies{Loader: &defaultLoader{}, Operations: operations, Backups: backups, Locker: locker}, strings.NewReader(""), &stdout, &stderr, nil)
	backupPath := filepath.Join(directory, "backup.tar.zst.age")
	if err := os.WriteFile(backupPath, []byte("verified by fake restore"), 0o600); err != nil {
		t.Fatal(err)
	}
	status := cli.Run(context.Background(), []string{"--config", configPath, "--quiet", "restore", backupPath, "--identity", "key.txt"})
	if status != 0 {
		t.Fatalf("status = %d, stderr = %s", status, stderr.String())
	}
	if !contains(operations.calls, "restore") {
		t.Fatalf("restore was not dispatched: %v", operations.calls)
	}
}

func TestRestoreJSONUsesStructuredBackupResult(t *testing.T) {
	cli, _, operations, _, _, stdout, _ := newTestCLI()
	operations.ownership = Ownership{}
	path := filepath.Join(t.TempDir(), "backup.tar.zst.age")
	if err := os.WriteFile(path, []byte("verified by fake restore"), 0o600); err != nil {
		t.Fatal(err)
	}
	if status := cli.Run(context.Background(), []string{"--json", "restore", path, "--identity", "key.txt"}); status != 0 {
		t.Fatalf("status = %d", status)
	}
	if !strings.Contains(stdout.String(), `"backup":{"archive":`) || strings.Contains(stdout.String(), `"backup":"`) {
		t.Fatalf("restore backup was not structured: %s", stdout.String())
	}
}

func TestRebuildRecoversPreviousRootAfterDesiredApplyFails(t *testing.T) {
	cli, _, operations, _, _, _, _ := newTestCLI()
	operations.errAt["apply:all"] = errors.New("bad desired release")
	if status := cli.Run(context.Background(), []string{"rebuild"}); status != 1 {
		t.Fatalf("status = %d", status)
	}
	wantTail := []string{"remove-vm:true", "recreate-vm", "apply:all", "recover-rebuild", "complete-rebuild"}
	if len(operations.calls) < len(wantTail) || !reflect.DeepEqual(operations.calls[len(operations.calls)-len(wantTail):], wantTail) {
		t.Fatalf("calls = %v", operations.calls)
	}
}

func TestRebuildRecoveryUsesFreshContextAfterDestructiveBoundary(t *testing.T) {
	cli, _, operations, _, _, _, _ := newTestCLI()
	operations.errAt["apply:all"] = context.Canceled
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if status := cli.Run(ctx, []string{"rebuild"}); status != 1 {
		t.Fatalf("status = %d", status)
	}
	if operations.recoveryCtxErr != nil {
		t.Fatalf("rebuild recovery inherited canceled request context: %v", operations.recoveryCtxErr)
	}
}

func TestRebuildRefusesDestructionWithoutFreshRecoveryBackup(t *testing.T) {
	cli, _, operations, backups, _, _, _ := newTestCLI()
	backups.err = errors.New("fresh backup failed")
	backups.latest = &BackupResult{Archive: "older.age", Envelope: "older.json"}
	if status := cli.Run(context.Background(), []string{"rebuild"}); status != 1 {
		t.Fatalf("status = %d", status)
	}
	if contains(operations.calls, "stop-vm") || contains(operations.calls, "remove-vm:true") {
		t.Fatalf("rebuild mutated the VM without a fresh recovery backup: %v", operations.calls)
	}
}

func TestRebuildPersistsRecoveryJournalBeforeStoppingOrDeletingVM(t *testing.T) {
	cli, _, operations, _, _, _, _ := newTestCLI()
	operations.errAt["prepare-rebuild-recovery"] = errors.New("journal fsync failed")
	if status := cli.Run(context.Background(), []string{"rebuild"}); status != 1 {
		t.Fatalf("status = %d", status)
	}
	if !contains(operations.calls, "prepare-rebuild-recovery") {
		t.Fatalf("rebuild did not persist recovery state: %v", operations.calls)
	}
	if contains(operations.calls, "stop-services") || contains(operations.calls, "stop-vm") || contains(operations.calls, "remove-vm:true") {
		t.Fatalf("rebuild mutated the VM after journal persistence failed: %v", operations.calls)
	}
}

func TestBrokenGuestRebuildUsesVerifiedHostBackupBeforeMutation(t *testing.T) {
	cli, _, operations, backups, _, _, _ := newTestCLI()
	operations.errAt["recover"] = errors.New("guest SSH unavailable")
	backups.latest = &BackupResult{Archive: "verified.age", Envelope: "verified.json", ArchiveSHA256: "abc"}
	if status := cli.Run(context.Background(), []string{"--quiet", "rebuild"}); status != 0 {
		t.Fatalf("status = %d", status)
	}
	if contains(operations.calls, "capture-recovery") || contains(operations.calls, "stop-services") || contains(operations.calls, "sync-data") {
		t.Fatalf("host-only recovery attempted guest-only preparation: %v", operations.calls)
	}
	if !contains(operations.calls, "stop-vm") || !contains(operations.calls, "remove-vm:true") {
		t.Fatalf("rebuild did not continue after verified host recovery selection: %v", operations.calls)
	}
	if !contains(operations.calls, "restore-rebuild-data") {
		t.Fatalf("host-only rebuild did not replace potentially half-swapped data from its verified backup: %v", operations.calls)
	}
}

func TestBrokenGuestRebuildRefusesMutationWithoutVerifiedHostBackup(t *testing.T) {
	cli, _, operations, backups, _, _, _ := newTestCLI()
	operations.errAt["recover"] = errors.New("guest SSH unavailable")
	backups.latestErr = errors.New("no valid backup exists")
	if status := cli.Run(context.Background(), []string{"--quiet", "rebuild"}); status != 1 {
		t.Fatalf("status = %d", status)
	}
	if contains(operations.calls, "stop-vm") || contains(operations.calls, "remove-vm:true") {
		t.Fatalf("rebuild mutated resources without verified host recovery: %v", operations.calls)
	}
}

func TestDestroyForceReportsLatestBackupWarningInHumanAndJSONModes(t *testing.T) {
	for _, jsonMode := range []bool{false, true} {
		t.Run(boolString(jsonMode), func(t *testing.T) {
			cli, _, _, backups, _, stdout, stderr := newTestCLI()
			backups.latest = &BackupResult{Archive: "verified.age", Envelope: "verified.json", ArchiveSHA256: "abc"}
			args := []string{"destroy", "--force"}
			if jsonMode {
				args = append([]string{"--json"}, args...)
			}
			if status := cli.Run(context.Background(), args); status != 0 {
				t.Fatalf("status = %d", status)
			}
			if !strings.Contains(stderr.String(), "latest verified backup verified.age") {
				t.Fatalf("stderr = %q", stderr.String())
			}
			if jsonMode && !strings.Contains(stdout.String(), `"warning":"forced removal will rely on latest verified backup verified.age"`) {
				t.Fatalf("JSON output = %s", stdout.String())
			}
		})
	}
}

func TestDestroyForceBypassesBrokenGuestAndReportsNoRecoveryBackup(t *testing.T) {
	cli, _, operations, backups, _, stdout, stderr := newTestCLI()
	operations.errAt["recover"] = errors.New("guest SSH unavailable")
	backups.latestErr = errors.New("no valid backup exists")
	if status := cli.Run(context.Background(), []string{"--json", "destroy", "--force"}); status != 0 {
		t.Fatalf("status = %d", status)
	}
	if contains(operations.calls, "recover") {
		t.Fatalf("forced destroy depended on the broken guest: %v", operations.calls)
	}
	if !contains(operations.calls, "remove-all") {
		t.Fatalf("forced destroy did not remove owned resources: %v", operations.calls)
	}
	for _, output := range []string{stdout.String(), stderr.String()} {
		if !strings.Contains(output, "no valid recovery backup") {
			t.Fatalf("missing no-backup warning: %q", output)
		}
	}
}

func TestGhosttyEnvironmentForwardsOnlyValidatedTerminalMetadata(t *testing.T) {
	values := map[string]string{
		"COLORTERM": "truecolor", "TERM_PROGRAM": "ghostty", "TERM_PROGRAM_VERSION": "1.2.3",
		"AWS_SECRET_ACCESS_KEY": "must-not-forward", "TERM": "host-controlled",
	}
	got := ghosttyEnvironment(func(name string) string { return values[name] })
	want := []string{"COLORTERM=truecolor", "TERM_PROGRAM=ghostty", "TERM_PROGRAM_VERSION=1.2.3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("environment = %v, want %v", got, want)
	}
	values["TERM_PROGRAM_VERSION"] = "bad value; touch /tmp/no"
	got = ghosttyEnvironment(func(name string) string { return values[name] })
	if contains(got, "TERM_PROGRAM_VERSION="+values["TERM_PROGRAM_VERSION"]) {
		t.Fatalf("unsafe terminal metadata was forwarded: %v", got)
	}
}

func TestSSHDispatchesDirectlyToBlessedTmSession(t *testing.T) {
	t.Setenv("COLORTERM", "truecolor")
	t.Setenv("TERM_PROGRAM", "ghostty")
	t.Setenv("TERM_PROGRAM_VERSION", "1.2.3")
	runner := &captureRunner{}
	operations := &defaultOperations{runner: runner}
	status, err := operations.SSH(context.Background(), Definition{Name: "main", Home: t.TempDir()}, strings.NewReader(""), io.Discard, io.Discard)
	if err != nil || status != 0 {
		t.Fatalf("SSH status=%d error=%v", status, err)
	}
	want := []string{
		"shell", "--workdir", "/workspace", "main", "--", "sudo", "-u", "agent", "-H", "env",
		"TERM=xterm-ghostty", "COLORTERM=truecolor", "TERM_PROGRAM=ghostty", "TERM_PROGRAM_VERSION=1.2.3",
		"/usr/local/bin/tm",
	}
	if runner.spec.Name != "limactl" || !reflect.DeepEqual(runner.spec.Args, want) {
		t.Fatalf("SSH process = %s %#v, want limactl %#v", runner.spec.Name, runner.spec.Args, want)
	}
}

func TestRecoveryLogTargetIsTruthfulAndLegacySystemTargetIsRejected(t *testing.T) {
	target, lines, follow, err := parseLogs([]string{"recovery", "-n", "25"})
	if err != nil || target != "recovery" || lines != 25 || follow {
		t.Fatalf("parse recovery logs = target %q lines %d follow %t error %v", target, lines, follow, err)
	}
	if _, _, _, err := parseLogs([]string{"system"}); err == nil {
		t.Fatal("legacy system target was accepted despite not providing system logs")
	}
	runner := &captureRunner{}
	operations := &defaultOperations{runner: runner}
	if err := operations.Logs(context.Background(), Definition{Name: "main", Home: t.TempDir()}, "recovery", 25, false, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	want := []string{"shell", "main", "--", "sudo", "journalctl", "-u", "hermes-box-recover.service", "-n", "25", "--no-pager"}
	if runner.spec.Name != "limactl" || !reflect.DeepEqual(runner.spec.Args, want) {
		t.Fatalf("recovery log process = %s %#v, want limactl %#v", runner.spec.Name, runner.spec.Args, want)
	}
}

func TestSetupExecutorKeepsTokenOutOfProcessArguments(t *testing.T) {
	runner := &secretRunner{}
	operations := &defaultOperations{runner: runner, stdout: io.Discard, stderr: io.Discard}
	secret := "super-secret-token-value"
	if _, err := operations.SetupExecutor(context.Background(), Definition{Name: "main", Home: t.TempDir()}, strings.NewReader(secret+"\n"), true); err != nil {
		t.Fatal(err)
	}
	if len(runner.runs) != 1 || runner.runs[0].Name != "limactl" {
		t.Fatalf("processes = %#v", runner.runs)
	}
	for _, argument := range runner.runs[0].Args {
		if strings.Contains(argument, secret) {
			t.Fatalf("secret appeared in process argument: %q", argument)
		}
	}
	data, err := io.ReadAll(runner.runs[0].Stdin)
	if err != nil || string(data) != secret+"\n" {
		t.Fatalf("secret was not transferred over stdin: %q, %v", data, err)
	}
}

func TestSetupExecutorNoEchoFailureIsFailClosed(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "token")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	runner := &secretRunner{disableErr: errors.New("not a terminal")}
	operations := &defaultOperations{runner: runner, stdout: io.Discard, stderr: io.Discard}
	if _, err := operations.readExecutorToken(context.Background(), file, false); err == nil || !strings.Contains(err.Error(), "disable terminal echo") {
		t.Fatalf("prompt error = %v", err)
	}
	for _, call := range runner.runs {
		if call.Name == "limactl" {
			t.Fatal("token setup continued after no-echo failure")
		}
	}
}

func TestSetupExecutorRejectsHeaderInjectionToken(t *testing.T) {
	operations := &defaultOperations{runner: &secretRunner{}, stdout: io.Discard, stderr: io.Discard}
	if _, err := operations.readExecutorToken(context.Background(), strings.NewReader("token\rInjected: yes\n"), true); err == nil {
		t.Fatal("Executor token containing a header injection control was accepted")
	}
}

func TestSetupExecutorRestoresTerminalAfterReadFailure(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "token")
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	runner := &secretRunner{}
	operations := &defaultOperations{runner: runner, stdout: io.Discard, stderr: io.Discard}
	if _, err := operations.readExecutorToken(context.Background(), file, false); err == nil {
		t.Fatal("closed terminal input unexpectedly succeeded")
	}
	if len(runner.runs) != 2 || !reflect.DeepEqual(runner.runs[0].Args, []string{"-echo"}) || !reflect.DeepEqual(runner.runs[1].Args, []string{"deadbeef:1"}) {
		t.Fatalf("terminal calls = %#v", runner.runs)
	}
}

func TestDefaultUpdateStopsBeforeMutationWhenGuestStagingFails(t *testing.T) {
	runner := &scriptedProcessRunner{runErrors: []error{errors.New("guest staging unavailable")}}
	operations := &defaultOperations{runner: runner, stdout: io.Discard, stderr: io.Discard}
	_, err := operations.Apply(context.Background(), Definition{Name: "main", Home: t.TempDir()}, "codex")
	if err == nil || !strings.Contains(err.Error(), "guest staging unavailable") {
		t.Fatalf("update error = %v", err)
	}
	if len(runner.runs) != 1 || len(runner.outputs) != 0 {
		t.Fatalf("update continued after staging failure: runs=%d outputs=%d", len(runner.runs), len(runner.outputs))
	}
}

func TestDefaultFailedUpdateKeepsServicesStoppedAndJournalDurableWhenSnapshotRestoreFails(t *testing.T) {
	runner := &scriptedProcessRunner{}
	def := Definition{Name: "main", Home: filepath.Join(t.TempDir(), "state"), ConfigDir: t.TempDir()}
	directory := filepath.Join(def.Home, "backups", def.Name, "transactions", "codex")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	lock, err := encodeLock(validClosureLock())
	if err != nil {
		t.Fatal(err)
	}
	if err := writeTransactionSnapshot(filepath.Join(directory, "latest.json"), transactionSnapshot{
		Backup: BackupResult{Archive: "missing.age", Envelope: "missing.json", ArchiveSHA256: "abc"}, AppliedLock: lock,
	}); err != nil {
		t.Fatal(err)
	}
	store, err := box.NewStore(def.Home)
	if err != nil {
		t.Fatal(err)
	}
	journal := box.Journal{
		Schema: 1, Operation: "update", Phase: "prepared", StartedAt: time.Unix(1, 0),
		Update: &box.UpdateRecovery{Component: "codex", Snapshot: filepath.Join(directory, "latest.json")},
	}
	if err := store.SaveJournal(def.Name, journal); err != nil {
		t.Fatal(err)
	}
	operations := &defaultOperations{runner: runner, keys: keychain.NewMemoryStore(), stderr: io.Discard}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := operations.completeHostUpdateRecovery(canceled, def, journal); err == nil {
		t.Fatal("failed update recovery unexpectedly succeeded")
	}
	if len(runner.runs) != 1 {
		t.Fatalf("services were restarted before recovery confirmation: %#v", runner.runs)
	}
	if runner.runContextErr[0] != nil {
		t.Fatalf("failed-update recovery inherited canceled request context: %v", runner.runContextErr[0])
	}
	if !contains(runner.runs[0].Args, "stop") {
		t.Fatalf("recovery did not stop services first: %#v", runner.runs)
	}
	if _, found, err := store.LoadJournal(def.Name); err != nil || !found {
		t.Fatalf("update journal was lost after failed recovery: found=%t err=%v", found, err)
	}
}

func TestTransactionRecoveryAcceptsLostRecoverResponseOnlyAfterStateConfirmation(t *testing.T) {
	lock := validClosureLock()
	encoded, err := encodeLock(lock)
	if err != nil {
		t.Fatal(err)
	}
	clean := []byte(`{"schema":1,"ok":true,"result":{"applied":{"schema":1,"components":{"codex":"1"}},"releases":{"schema":1,"components":{}}}}`)
	runner := &scriptedProcessRunner{outputResults: []processResult{
		{err: errors.New("SSH response lost")},
		{output: clean},
	}}
	def := Definition{Name: "main", Home: t.TempDir()}
	operations := &defaultOperations{runner: runner, stderr: io.Discard}
	status, err := operations.confirmTransactionActivation(context.Background(), def, "codex", transactionSnapshot{AppliedLock: encoded}, lock)
	if err != nil {
		t.Fatal(err)
	}
	if status.Applied.Components[component.Codex] != "1" {
		t.Fatalf("confirmed status = %#v", status)
	}
	if len(runner.runs) != 2 {
		t.Fatalf("guest calls = %d, want recover plus status confirmation", len(runner.runs))
	}
	if _, err := config.LoadLock(hostAppliedLockPath(def)); err != nil {
		t.Fatalf("confirmed recovery did not publish host applied lock: %v", err)
	}
}

func TestTransactionRecoveryAcceptsLostRollbackResponseOnlyAfterStateConfirmation(t *testing.T) {
	lock := validClosureLock()
	encoded, err := encodeLock(lock)
	if err != nil {
		t.Fatal(err)
	}
	candidate := []byte(`{"schema":1,"ok":true,"result":{"applied":{"schema":1,"components":{"codex":"2"}},"releases":{"schema":1,"components":{}}}}`)
	restored := []byte(`{"schema":1,"ok":true,"result":{"applied":{"schema":1,"components":{"codex":"1"}},"releases":{"schema":1,"components":{}}}}`)
	runner := &scriptedProcessRunner{outputResults: []processResult{
		{output: candidate},
		{output: candidate},
		{err: errors.New("rollback response lost")},
		{output: restored},
	}}
	def := Definition{Name: "main", Home: t.TempDir()}
	operations := &defaultOperations{runner: runner, stderr: io.Discard}
	status, err := operations.confirmTransactionActivation(context.Background(), def, "codex", transactionSnapshot{AppliedLock: encoded}, lock)
	if err != nil {
		t.Fatal(err)
	}
	if status.Applied.Components[component.Codex] != "1" {
		t.Fatalf("confirmed status = %#v", status)
	}
	request, err := io.ReadAll(runner.runs[2].Stdin)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(request, []byte(`"operation":"rollback"`)) {
		t.Fatalf("third call was not rollback: %s", request)
	}
}

func TestInterruptedRollbackKeepsServicesStoppedAndJournalDurableWhenRestoreFails(t *testing.T) {
	home := filepath.Join(t.TempDir(), "state")
	configDir := t.TempDir()
	def := Definition{Name: "main", Home: home, ConfigDir: configDir}
	directory := filepath.Join(home, "backups", "main", "transactions", "codex")
	previousPath := filepath.Join(directory, "rollback-previous.json")
	currentPath := filepath.Join(directory, "rollback-current.json")
	encoded, err := encodeLock(validClosureLock())
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{previousPath, currentPath} {
		if err := writeTransactionSnapshot(path, transactionSnapshot{
			Backup: BackupResult{Archive: "missing.age", Envelope: "missing.json", ArchiveSHA256: "abc"}, AppliedLock: encoded,
		}); err != nil {
			t.Fatal(err)
		}
	}
	store, err := box.NewStore(home)
	if err != nil {
		t.Fatal(err)
	}
	journal := box.Journal{
		Schema: 1, Operation: "rollback", Phase: "prepared", StartedAt: time.Unix(1, 0),
		Rollback: &box.RollbackRecovery{Component: "codex", PreviousSnapshot: previousPath, CurrentSnapshot: currentPath},
	}
	if err := store.SaveJournal("main", journal); err != nil {
		t.Fatal(err)
	}
	runner := &scriptedProcessRunner{}
	operations := &defaultOperations{runner: runner, keys: keychain.NewMemoryStore(), stderr: io.Discard}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := operations.completeHostRollback(canceled, def, journal); err == nil {
		t.Fatal("interrupted rollback unexpectedly succeeded")
	}
	if len(runner.runs) != 1 || !contains(runner.runs[0].Args, "stop") {
		t.Fatalf("services were restarted before rollback recovery completed: %#v", runner.runs)
	}
	if runner.runContextErr[0] != nil {
		t.Fatalf("rollback recovery inherited canceled request context: %v", runner.runContextErr[0])
	}
	if _, found, err := store.LoadJournal("main"); err != nil || !found {
		t.Fatalf("rollback journal was lost after failed recovery: found=%t err=%v", found, err)
	}
}

func TestDefaultRecoverSurfacesGuestRecoveryFailure(t *testing.T) {
	status := []byte(`{"schema":1,"ok":true,"result":{"applied":{"schema":1,"components":{}},"releases":{"schema":1,"components":{}}}}`)
	runner := &scriptedProcessRunner{outputResults: []processResult{
		{output: status}, {err: errors.New("guest recovery failed")},
	}}
	operations := &defaultOperations{runner: runner, stderr: io.Discard}
	err := operations.Recover(context.Background(), Definition{Name: "main", Home: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "guest recovery failed") {
		t.Fatalf("recover error = %v", err)
	}
	if len(runner.runs) != 2 {
		t.Fatalf("recover process calls = %d", len(runner.runs))
	}
}

func TestDefaultRecoverRepairsRestoreJournalBeforeRebuildCaptureCanContinue(t *testing.T) {
	pending := []byte(`{"schema":1,"ok":true,"result":{"applied":{"schema":1,"components":{}},"releases":{"schema":1,"components":{}},"restore_pending":{"component":"codex","committed":false}}}`)
	clean := []byte(`{"schema":1,"ok":true,"result":{"applied":{"schema":1,"components":{}},"releases":{"schema":1,"components":{}}}}`)
	runner := &scriptedProcessRunner{outputResults: []processResult{{output: pending}, {output: clean}}}
	operations := &defaultOperations{runner: runner, stderr: io.Discard}
	if err := operations.Recover(context.Background(), Definition{Name: "main", Home: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if len(runner.runs) != 2 {
		t.Fatalf("guest recovery calls = %d", len(runner.runs))
	}
	requestData, err := io.ReadAll(runner.runs[1].Stdin)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(requestData, []byte(`"operation":"recover"`)) {
		t.Fatalf("second request did not recover restore journal: %s", requestData)
	}
}

func TestDefaultRecoverRejectsUnresolvedRestoreJournal(t *testing.T) {
	pending := []byte(`{"schema":1,"ok":true,"result":{"applied":{"schema":1,"components":{}},"releases":{"schema":1,"components":{}},"restore_pending":{"component":"codex","committed":false}}}`)
	runner := &scriptedProcessRunner{outputResults: []processResult{{output: pending}, {output: pending}}}
	operations := &defaultOperations{runner: runner, stderr: io.Discard}
	err := operations.Recover(context.Background(), Definition{Name: "main", Home: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "unresolved") {
		t.Fatalf("recover error = %v", err)
	}
}

func TestGuestResponseDecoderRejectsUnknownSchemaAndOversize(t *testing.T) {
	for _, data := range [][]byte{
		[]byte(`{"schema":2,"ok":true,"result":{}}`),
		[]byte(`{"schema":1,"ok":true,"result":{},"unknown":true}`),
		[]byte(`{"schema":1,"ok":true,"result":{}} {}`),
	} {
		if _, err := decodeGuestResponse(data); err == nil {
			t.Fatalf("invalid response accepted: %s", data)
		}
	}
	buffer := &boundedProtocolBuffer{}
	written, err := buffer.Write(make([]byte, maximumGuestResponseBytes+1))
	if err == nil || !buffer.exceeded {
		t.Fatal("oversized guest response was not rejected")
	}
	if written != maximumGuestResponseBytes || buffer.buffer.Len() != maximumGuestResponseBytes {
		t.Fatalf("partial write = %d bytes, buffer = %d, want %d", written, buffer.buffer.Len(), maximumGuestResponseBytes)
	}
}

func TestDefaultRollbackDoesNotStopServicesWhenSnapshotCannotStart(t *testing.T) {
	status := []byte(`{"schema":1,"ok":true,"result":{"applied":{"schema":1,"components":{"codex":"2"}},"releases":{"schema":1,"components":{"codex":{"current":"2","previous":"1"}}}}}`)
	runner := &scriptedProcessRunner{outputResults: []processResult{{output: status}}}
	def := Definition{Name: "main", Home: t.TempDir()}
	directory := filepath.Join(def.Home, "backups", def.Name, "transactions", "codex")
	lock, encodeErr := encodeLock(validClosureLock())
	if encodeErr != nil {
		t.Fatal(encodeErr)
	}
	if err := writeTransactionSnapshot(filepath.Join(directory, "latest.json"), transactionSnapshot{
		Backup: BackupResult{Archive: "missing.age", Envelope: "missing.json", ArchiveSHA256: "abc"}, AppliedLock: lock,
	}); err != nil {
		t.Fatal(err)
	}
	operations := &defaultOperations{runner: runner, stderr: io.Discard}
	_, err := operations.Rollback(context.Background(), def, "codex")
	if err == nil || !strings.Contains(err.Error(), "Keychain") {
		t.Fatalf("rollback error = %v", err)
	}
	for _, call := range runner.runs {
		if contains(call.Args, "systemctl") && contains(call.Args, "stop") {
			t.Fatalf("rollback stopped services without a snapshot: %#v", runner.runs)
		}
	}
}

func TestSetupExecutorFailureDoesNotExposeToken(t *testing.T) {
	secret := "never-log-this-token"
	runner := &scriptedProcessRunner{runErrors: []error{errors.New("guest setup failed")}}
	var stderr bytes.Buffer
	operations := &defaultOperations{runner: runner, stdout: io.Discard, stderr: &stderr}
	_, err := operations.SetupExecutor(context.Background(), Definition{Name: "main", Home: t.TempDir()}, strings.NewReader(secret+"\n"), true)
	if err == nil {
		t.Fatal("setup unexpectedly succeeded")
	}
	combined := err.Error() + stderr.String()
	for _, call := range runner.runs {
		combined += strings.Join(call.Args, " ")
	}
	if strings.Contains(combined, secret) {
		t.Fatalf("secret appeared in diagnostics or argv: %q", combined)
	}
}

func TestDefaultExecUsesGuestExactArgvProtocol(t *testing.T) {
	runner := &captureRunner{}
	operations := &defaultOperations{runner: runner, stdout: io.Discard, stderr: io.Discard}
	var stdout bytes.Buffer
	status, err := operations.Exec(context.Background(), Definition{Name: "main", Home: t.TempDir()}, []string{"printf", "%s", "a b; still-one-arg"}, nil, &stdout, io.Discard)
	if err != nil || status != 0 || stdout.String() != "ok" {
		t.Fatalf("exec = status %d, stdout %q, error %v", status, stdout.String(), err)
	}
	if got := strings.Join(runner.spec.Args, " "); got != "shell main -- sudo /usr/local/libexec/hermes-box-guest" {
		t.Fatalf("limactl argv = %q", got)
	}
	requestData, err := io.ReadAll(runner.spec.Stdin)
	if err != nil {
		t.Fatal(err)
	}
	var request guestupdate.Request
	if err := json.Unmarshal(requestData, &request); err != nil {
		t.Fatal(err)
	}
	want := []string{"printf", "%s", "a b; still-one-arg"}
	if !reflect.DeepEqual(request.Argv, want) {
		t.Fatalf("guest argv = %v, want %v", request.Argv, want)
	}
}

func TestComponentStatusUsesGuestActivationsAndPreviousRelease(t *testing.T) {
	def := Definition{}
	def.Bundle.Lock.Codex.Version = "desired"
	status := guestupdate.Status{
		Applied: guestupdate.AppliedState{Components: map[component.Name]string{component.Codex: "current"}},
		Releases: guestupdate.ReleasesState{Components: map[component.Name]guestupdate.ReleaseMetadata{
			component.Codex: {Current: "current", Previous: "previous"},
		}},
	}
	observations := map[component.Name]componentObservation{
		component.Codex: {running: "codex-cli 1.2.3"},
	}
	components := componentStatus(def, status, observations)
	codex := components["codex"].(map[string]any)
	if codex["applied"] != "current" || codex["running"] != "codex-cli 1.2.3" || codex["previous"] != "previous" || codex["state"] != "drifted" {
		t.Fatalf("codex status = %#v", codex)
	}
	observations[component.Codex] = componentObservation{err: errors.New("wrong symlink")}
	codex = componentStatus(def, status, observations)["codex"].(map[string]any)
	if codex["state"] != "failed" {
		t.Fatalf("failed activation state = %#v", codex)
	}
}

func TestDoctorEmitsChecksAndReturnsNonzeroWhenUnhealthy(t *testing.T) {
	cli, _, operations, _, _, stdout, _ := newTestCLI()
	operations.errAt["doctor"] = errors.New("network unavailable")
	if status := cli.Run(context.Background(), []string{"--json", "doctor"}); status != 1 {
		t.Fatalf("status = %d", status)
	}
	var envelope struct {
		OK     bool `json:"ok"`
		Result struct {
			Healthy bool             `json:"healthy"`
			Checks  []map[string]any `json:"checks"`
		} `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if !envelope.OK || envelope.Result.Healthy || len(envelope.Result.Checks) != 1 {
		t.Fatalf("doctor envelope = %#v", envelope)
	}
}

func TestFailedRebuildCandidateIsStoppedRemovedAndVerifiedAbsent(t *testing.T) {
	present := []byte("{\"name\":\"main\",\"status\":\"Stopped\",\"arch\":\"aarch64\",\"vmType\":\"vz\"}\n")
	disk := []byte("{\"name\":\"main-data\",\"size\":53687091200,\"format\":\"raw\",\"dir\":\"/tmp/main-data\",\"instance\":\"main\"}\n")
	runner := &scriptedLimaRunner{results: []scriptedLimaResult{
		{result: lima.Result{Stdout: present}}, {result: lima.Result{Stdout: disk}},
		{result: lima.Result{Stdout: present}}, {}, {result: lima.Result{}},
	}}
	def, _ := boundDefinition(t, runner)
	operations := &defaultOperations{limaRunner: runner}
	if err := operations.removeFailedRebuildCandidate(context.Background(), def); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"list", "--format", "json", "main"},
		{"disk", "list", "--json", "main-data"},
		{"list", "--format", "json", "main"},
		{"delete", "main", "--tty=false"},
		{"list", "--format", "json", "main"},
	}
	if len(runner.invocations) != len(want) {
		t.Fatalf("invocations = %v", runner.invocations)
	}
	for index := range want {
		if !reflect.DeepEqual(runner.invocations[index].Args, want[index]) {
			t.Fatalf("invocation %d = %v, want %v", index, runner.invocations[index].Args, want[index])
		}
	}
}

func TestFailedRebuildCandidateRemovalErrorIsNotIgnored(t *testing.T) {
	present := []byte("{\"name\":\"main\",\"status\":\"Stopped\",\"arch\":\"aarch64\",\"vmType\":\"vz\"}\n")
	disk := []byte("{\"name\":\"main-data\",\"size\":53687091200,\"format\":\"raw\",\"dir\":\"/tmp/main-data\",\"instance\":\"main\"}\n")
	runner := &scriptedLimaRunner{results: []scriptedLimaResult{
		{result: lima.Result{Stdout: present}}, {result: lima.Result{Stdout: disk}},
		{result: lima.Result{Stdout: present}},
		{err: errors.New("delete denied")},
	}}
	def, _ := boundDefinition(t, runner)
	operations := &defaultOperations{limaRunner: runner}
	err := operations.removeFailedRebuildCandidate(context.Background(), def)
	if err == nil || !strings.Contains(err.Error(), "remove failed rebuild candidate") || !strings.Contains(err.Error(), "delete denied") {
		t.Fatalf("error = %v", err)
	}
	if len(runner.invocations) != 4 {
		t.Fatalf("invocations after delete failure = %v", runner.invocations)
	}
}

func TestFailedRebuildCandidateMustBeAbsentBeforeRecoveryRecreate(t *testing.T) {
	present := []byte("{\"name\":\"main\",\"status\":\"Stopped\",\"arch\":\"aarch64\",\"vmType\":\"vz\"}\n")
	disk := []byte("{\"name\":\"main-data\",\"size\":53687091200,\"format\":\"raw\",\"dir\":\"/tmp/main-data\",\"instance\":\"main\"}\n")
	runner := &scriptedLimaRunner{results: []scriptedLimaResult{
		{result: lima.Result{Stdout: present}}, {result: lima.Result{Stdout: disk}},
		{result: lima.Result{Stdout: present}}, {}, {result: lima.Result{Stdout: present}},
	}}
	def, _ := boundDefinition(t, runner)
	operations := &defaultOperations{limaRunner: runner}
	err := operations.removeFailedRebuildCandidate(context.Background(), def)
	if err == nil || !strings.Contains(err.Error(), "still exists after removal") {
		t.Fatalf("error = %v", err)
	}
}

func TestCleanupCreateRetainsOwnershipStateUntilResourcesAreConfirmedAbsent(t *testing.T) {
	presentVM := []byte("{\"name\":\"main\",\"status\":\"Stopped\",\"arch\":\"aarch64\",\"vmType\":\"vz\"}\n")
	presentDisk := []byte("{\"name\":\"main-data\",\"status\":\"InUse\",\"size\":53687091200,\"format\":\"raw\",\"dir\":\"/tmp/main-data\",\"instance\":\"main\"}\n")
	runner := &scriptedLimaRunner{results: []scriptedLimaResult{
		{result: lima.Result{Stdout: presentVM}}, {result: lima.Result{Stdout: presentDisk}},
		{}, {}, {}, {result: lima.Result{Stdout: presentVM}},
	}}
	def, store := boundDefinition(t, runner)
	operations := &defaultOperations{limaRunner: runner}
	if err := operations.CleanupCreate(context.Background(), def); err == nil || !strings.Contains(err.Error(), "still exists") {
		t.Fatalf("cleanup error = %v", err)
	}
	if _, found, err := store.LoadMetadata(def.Name); err != nil || !found {
		t.Fatalf("metadata was removed before absence proof: found=%t err=%v", found, err)
	}
	if _, found, err := store.LoadJournal(def.Name); err != nil || !found {
		t.Fatalf("journal was removed before absence proof: found=%t err=%v", found, err)
	}
}

func TestCleanupCreateRemovesStaleOwnershipAfterJournaledResourcesAreProvenAbsent(t *testing.T) {
	runner := &scriptedLimaRunner{results: []scriptedLimaResult{{result: lima.Result{}}, {result: lima.Result{}}}}
	def, store := boundDefinition(t, runner)
	operations := &defaultOperations{limaRunner: runner}
	if err := operations.CleanupCreate(context.Background(), def); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.LoadMetadata(def.Name); err != nil || found {
		t.Fatalf("stale metadata remains: found=%t err=%v", found, err)
	}
	if _, found, err := store.LoadJournal(def.Name); err != nil || found {
		t.Fatalf("stale journal remains: found=%t err=%v", found, err)
	}
	if len(runner.invocations) != 2 {
		t.Fatalf("absence proof calls = %#v", runner.invocations)
	}
}

func TestResumeCreateCleansJournalWrittenBeforeMetadataIntent(t *testing.T) {
	home := filepath.Join(t.TempDir(), "state")
	store, err := box.NewStore(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveJournal("main", box.Journal{
		Schema: 1, Operation: "create", Phase: "incomplete", StartedAt: time.Unix(1, 0),
	}); err != nil {
		t.Fatal(err)
	}
	def := Definition{Name: "main", Home: home, ConfigDir: t.TempDir()}
	operations := &defaultOperations{limaRunner: &scriptedLimaRunner{}}
	if err := operations.ResumeInterruptedMutation(context.Background(), def); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.LoadJournal("main"); err != nil || found {
		t.Fatalf("pre-metadata journal remains: found=%t err=%v", found, err)
	}
}

func TestResumeCreateFinishesCleanupAfterMetadataRemovalCrash(t *testing.T) {
	home := filepath.Join(t.TempDir(), "state")
	store, err := box.NewStore(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveJournal("main", box.Journal{
		Schema: 1, Operation: "create", Phase: "incomplete", Resources: []string{"main", "main-data"}, StartedAt: time.Unix(1, 0),
	}); err != nil {
		t.Fatal(err)
	}
	runner := &scriptedLimaRunner{results: []scriptedLimaResult{{result: lima.Result{}}, {result: lima.Result{}}}}
	def := Definition{Name: "main", Home: home, ConfigDir: t.TempDir()}
	operations := &defaultOperations{limaRunner: runner}
	if err := operations.ResumeInterruptedMutation(context.Background(), def); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.LoadJournal("main"); err != nil || found {
		t.Fatalf("cleanup journal remains after retry: found=%t err=%v", found, err)
	}
}

func TestCleanupCreateRemovesJournaledDiskCreatedBeforeDirectoryBinding(t *testing.T) {
	home := filepath.Join(t.TempDir(), "state")
	configDir := t.TempDir()
	definition := []byte("vmType: vz\narch: aarch64\n")
	disk := []byte("{\"name\":\"main-data\",\"size\":53687091200,\"format\":\"raw\",\"dir\":\"/tmp/main-data\"}\n")
	runner := &scriptedLimaRunner{results: []scriptedLimaResult{
		{result: lima.Result{}}, {result: lima.Result{Stdout: disk}}, {}, {result: lima.Result{}},
	}}
	client, err := lima.New(filepath.Join(home, "lima"), runner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.SaveDefinition("main", definition); err != nil {
		t.Fatal(err)
	}
	store, err := box.NewStore(home)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := box.NewMetadata("main", configDir, box.OwnershipBinding{
		DefinitionSHA256: definitionDigest(definition), DataDiskSize: 50 << 30, DataOwnershipMarker: strings.Repeat("a", 64),
	}, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateMetadata(metadata); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveJournal("main", box.Journal{
		Schema: 1, Operation: "create", Phase: "incomplete", Resources: []string{"main-data"}, StartedAt: time.Unix(1, 0),
	}); err != nil {
		t.Fatal(err)
	}
	operations := &defaultOperations{limaRunner: runner}
	if err := operations.CleanupCreate(context.Background(), Definition{Name: "main", Home: home, ConfigDir: configDir}); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.LoadMetadata("main"); err != nil || found {
		t.Fatalf("metadata remains after exact journaled disk cleanup: found=%t err=%v", found, err)
	}
	if len(runner.invocations) != 4 || !reflect.DeepEqual(runner.invocations[2].Args, []string{"disk", "delete", "main-data", "--tty=false"}) {
		t.Fatalf("Lima cleanup calls = %#v", runner.invocations)
	}
}

func TestDestroyRetainsOwnershipStateUntilDiskIsConfirmedAbsent(t *testing.T) {
	presentVM := []byte("{\"name\":\"main\",\"status\":\"Stopped\",\"arch\":\"aarch64\",\"vmType\":\"vz\"}\n")
	presentDisk := []byte("{\"name\":\"main-data\",\"status\":\"InUse\",\"size\":53687091200,\"format\":\"raw\",\"dir\":\"/tmp/main-data\",\"instance\":\"main\"}\n")
	runner := &scriptedLimaRunner{results: []scriptedLimaResult{
		{result: lima.Result{Stdout: presentVM}}, {result: lima.Result{Stdout: presentDisk}},
		{}, {}, {}, {}, {result: lima.Result{Stdout: presentDisk}},
	}}
	def, store := boundDefinition(t, runner)
	operations := &defaultOperations{limaRunner: runner}
	if err := operations.RemoveAll(context.Background(), def); err == nil || !strings.Contains(err.Error(), "still exists") {
		t.Fatalf("destroy error = %v", err)
	}
	if _, found, err := store.LoadMetadata(def.Name); err != nil || !found {
		t.Fatalf("metadata was removed before disk absence proof: found=%t err=%v", found, err)
	}
	if _, found, err := store.LoadJournal(def.Name); err != nil || !found {
		t.Fatalf("journal was removed before disk absence proof: found=%t err=%v", found, err)
	}
}

func TestOwnershipRejectsSameNameReplacementPlatform(t *testing.T) {
	replacement := []byte("{\"name\":\"main\",\"status\":\"Stopped\",\"arch\":\"x86_64\",\"vmType\":\"qemu\"}\n")
	disk := []byte("{\"name\":\"main-data\",\"size\":53687091200,\"format\":\"raw\",\"dir\":\"/tmp/main-data\",\"instance\":\"main\"}\n")
	runner := &scriptedLimaRunner{results: []scriptedLimaResult{{result: lima.Result{Stdout: replacement}}, {result: lima.Result{Stdout: disk}}}}
	def, _ := boundDefinition(t, runner)
	operations := &defaultOperations{limaRunner: runner}
	if _, err := operations.Ownership(context.Background(), def); err == nil || !strings.Contains(err.Error(), "unexpected platform") {
		t.Fatalf("replacement ownership error = %v", err)
	}
}

func TestHostOwnershipRemainsProvableWhenRunningGuestIsUnreachable(t *testing.T) {
	present := []byte("{\"name\":\"main\",\"status\":\"Running\",\"arch\":\"aarch64\",\"vmType\":\"vz\"}\n")
	disk := []byte("{\"name\":\"main-data\",\"size\":53687091200,\"format\":\"raw\",\"dir\":\"/tmp/main-data\",\"instance\":\"main\"}\n")
	runner := &scriptedLimaRunner{results: []scriptedLimaResult{{result: lima.Result{Stdout: present}}, {result: lima.Result{Stdout: disk}}}}
	def, _ := boundDefinition(t, runner)
	operations := &defaultOperations{limaRunner: runner, runner: &scriptedProcessRunner{runErrors: []error{errors.New("guest SSH unavailable")}}}
	ownership, err := operations.Ownership(context.Background(), def)
	if err != nil || !ownership.Owned || !ownership.Running {
		t.Fatalf("ownership = %#v, err = %v", ownership, err)
	}
}

func boundDefinition(t *testing.T, runner *scriptedLimaRunner) (Definition, *box.Store) {
	t.Helper()
	home := filepath.Join(t.TempDir(), "state")
	configDir := t.TempDir()
	def := Definition{Name: "main", Home: home, ConfigDir: configDir}
	client, err := lima.New(filepath.Join(home, "lima"), runner)
	if err != nil {
		t.Fatal(err)
	}
	definition := []byte("vmType: vz\narch: aarch64\n")
	if _, err := client.SaveDefinition(def.Name, definition); err != nil {
		t.Fatal(err)
	}
	store, err := box.NewStore(home)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := box.NewMetadata(def.Name, configDir, box.OwnershipBinding{
		DefinitionSHA256: definitionDigest(definition), DataDiskSize: 50 << 30,
		DataOwnershipMarker: strings.Repeat("a", 64),
	}, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateMetadata(metadata); err != nil {
		t.Fatal(err)
	}
	if err := store.BindDataDisk(def.Name, configDir, "/tmp/main-data"); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveJournal(def.Name, box.Journal{
		Schema: 1, Operation: "create", Phase: "incomplete",
		Resources: []string{"main", "main-data"}, StartedAt: time.Unix(1, 0),
	}); err != nil {
		t.Fatal(err)
	}
	return def, store
}

func TestDoctorRepairCommandsPreserveSelectedHomeAndConfig(t *testing.T) {
	got := operatorCommand(Definition{Home: "/tmp/box home", ConfigPath: "/tmp/davis's box/hermes-box.yaml"})
	want := `HERMES_BOX_HOME='/tmp/box home' hermes-box --config '/tmp/davis'"'"'s box/hermes-box.yaml'`
	if got != want {
		t.Fatalf("operator command = %q, want %q", got, want)
	}
}

func TestDefaultLockerUsesResolvedHermesBoxHome(t *testing.T) {
	home := filepath.Join(t.TempDir(), "custom-state")
	release, err := (defaultLocker{}).Acquire(context.Background(), Definition{Name: "main", Home: home}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "locks", "main.lock")); err != nil {
		t.Fatalf("resolved-home lock was not created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".hermes-box", "locks", "main.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("locker created an unexpected nested default-home path: %v", err)
	}
}

func TestGuestAppliedLockRejectsUnknownFields(t *testing.T) {
	def := Definition{Name: "main", Home: t.TempDir()}
	encoded, err := encodeLock(validClosureLock())
	if err != nil {
		t.Fatal(err)
	}
	if err := syncHostAppliedLock(def, encoded+"unexpected_field: true\n"); err == nil || !strings.Contains(err.Error(), "field unexpected_field not found") {
		t.Fatalf("strict guest applied-lock error = %v", err)
	}
	if _, err := os.Stat(hostAppliedLockPath(def)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid guest lock was published: %v", err)
	}
}

func TestHelpCommandDocumentsItself(t *testing.T) {
	cli, _, _, _, _, stdout, _ := newTestCLI()
	if status := cli.Run(context.Background(), []string{"help", "help"}); status != 0 {
		t.Fatalf("status = %d", status)
	}
	if got := stdout.String(); got != "Usage: hermes-box help [COMMAND]\n" {
		t.Fatalf("help help output = %q", got)
	}
}

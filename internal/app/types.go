package app

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/davis7dotsh/hermes-box/internal/config"
)

const Schema = 1

type Definition struct {
	Name       string
	ConfigPath string
	ConfigDir  string
	LockPath   string
	Home       string
	Bundle     config.Bundle
}

type LoadRequest struct {
	ConfigPath string
	Home       string
	Environ    []string
	Command    string
}

type Loader interface {
	Load(context.Context, LoadRequest) (Definition, error)
}

type Ownership struct {
	Exists  bool
	Owned   bool
	Running bool
}

type Health struct {
	Healthy       bool           `json:"healthy"`
	SetupRequired []string       `json:"setup_required"`
	Components    map[string]any `json:"components"`
	Storage       map[string]any `json:"storage"`
	Ports         map[string]any `json:"ports"`
}

type Status struct {
	State         string         `json:"state"`
	Healthy       bool           `json:"healthy"`
	SetupRequired []string       `json:"setup_required"`
	Components    map[string]any `json:"components"`
	Storage       map[string]any `json:"storage"`
	Ports         map[string]any `json:"ports"`
	LastBackup    any            `json:"last_backup"`
	Updates       []any          `json:"updates"`
}

type BackupResult struct {
	Archive       string `json:"archive"`
	Envelope      string `json:"envelope"`
	ArchiveSHA256 string `json:"archive_sha256"`
}

type RecoveryState struct {
	AppliedLock string
	Artifacts   []string
	Temporary   bool
}

type VersionResult struct {
	CLI          string `json:"cli"`
	Lima         string `json:"lima"`
	ConfigSchema int    `json:"config_schema"`
	LockSchema   int    `json:"lock_schema"`
}

// Operations is the host/guest boundary. Implementations own concrete Lima and
// guest-helper invocation; App owns public command ordering and safety policy.
type Operations interface {
	Preflight(context.Context, Definition, string) error
	ResumeInterruptedMutation(context.Context, Definition) error
	Ownership(context.Context, Definition) (Ownership, error)
	CreateInfrastructure(context.Context, Definition) error
	CompleteCreate(context.Context, Definition) error
	RecreateVM(context.Context, Definition) error
	CleanupCreate(context.Context, Definition) error
	StartVM(context.Context, Definition) error
	StopVM(context.Context, Definition) error
	RemoveVM(context.Context, Definition, bool) error
	RemoveAll(context.Context, Definition) error
	Recover(context.Context, Definition) error
	Apply(context.Context, Definition, string) (map[string]any, error)
	Rollback(context.Context, Definition, string) (map[string]any, error)
	StartServices(context.Context, Definition) error
	StopServices(context.Context, Definition) error
	SyncData(context.Context, Definition) error
	Health(context.Context, Definition) (Health, error)
	Status(context.Context, Definition, bool) (Status, error)
	SSH(context.Context, Definition, io.Reader, io.Writer, io.Writer) (int, error)
	Exec(context.Context, Definition, []string, io.Reader, io.Writer, io.Writer) (int, error)
	Logs(context.Context, Definition, string, int, bool, io.Writer, io.Writer) error
	OpenExecutor(context.Context, Definition) (string, error)
	SetupExecutor(context.Context, Definition, io.Reader, bool) ([]string, error)
	Doctor(context.Context, Definition) ([]map[string]any, error)
	ExportKey(context.Context, Definition, string) (string, error)
	CaptureRecoveryState(context.Context, Definition) (RecoveryState, error)
	PrepareRebuildRecovery(context.Context, Definition, RecoveryState, BackupResult) (RecoveryState, error)
	CompleteRebuild(context.Context, Definition) error
	Restore(context.Context, Definition, string, string, string) error
	RestoreRebuildData(context.Context, Definition, BackupResult) error
	RecoverRebuild(context.Context, Definition, RecoveryState, BackupResult) error
	Version(context.Context, Definition) (VersionResult, error)
}

type Backups interface {
	Create(context.Context, Definition, string) (BackupResult, error)
	LatestVerified(context.Context, Definition) (*BackupResult, error)
}

type Locker interface {
	Acquire(context.Context, Definition, string) (func() error, error)
}

type Clock interface {
	Now() time.Time
}

type Dependencies struct {
	Loader     Loader
	Operations Operations
	Backups    Backups
	Locker     Locker
	Clock      Clock
}

type Error struct {
	Code     string         `json:"code"`
	Message  string         `json:"message"`
	Recovery string         `json:"recovery,omitempty"`
	Details  map[string]any `json:"details,omitempty"`
	Status   int            `json:"-"`
	Cause    error          `json:"-"`
}

func (e *Error) Error() string { return e.Message }

func (e *Error) Unwrap() error { return e.Cause }

func apiError(code, message string, status int, cause error) *Error {
	return &Error{Code: code, Message: message, Status: status, Cause: cause}
}

func classify(err error) *Error {
	if err == nil {
		return nil
	}
	var api *Error
	if errors.As(err, &api) {
		return api
	}
	return apiError("external_failed", err.Error(), 1, err)
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

package box

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

type LockOwner struct {
	PID       int       `json:"pid"`
	Command   string    `json:"command"`
	StartedAt time.Time `json:"started_at"`
}

type OperationLock struct {
	file *os.File
}

type BusyError struct {
	Name  string
	Owner LockOwner
}

func (e *BusyError) Error() string {
	if e.Owner.PID == 0 {
		return fmt.Sprintf("box %q is busy", e.Name)
	}
	return fmt.Sprintf("box %q is busy with %q (pid %d, started %s)", e.Name, e.Owner.Command, e.Owner.PID, e.Owner.StartedAt.Format(time.RFC3339))
}

func (s *Store) Acquire(name, command string) (*OperationLock, error) {
	if !namePattern.MatchString(name) || command == "" {
		return nil, errors.New("box name and command are required for an operation lock")
	}
	path := filepath.Join(s.Home, "locks", name+".lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open operation lock: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("secure operation lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		owner := readLockOwner(file)
		_ = file.Close()
		return nil, &BusyError{Name: name, Owner: owner}
	}
	owner := LockOwner{PID: os.Getpid(), Command: command, StartedAt: time.Now().UTC()}
	data, err := json.Marshal(owner)
	if err != nil {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
		return nil, err
	}
	if err := file.Truncate(0); err != nil {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
		return nil, fmt.Errorf("truncate operation lock: %w", err)
	}
	if _, err := file.WriteAt(append(data, '\n'), 0); err != nil {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
		return nil, fmt.Errorf("write operation lock: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
		return nil, fmt.Errorf("sync operation lock: %w", err)
	}
	return &OperationLock{file: file}, nil
}

func (l *OperationLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	l.file = nil
	return errors.Join(unlockErr, closeErr)
}

func readLockOwner(file *os.File) LockOwner {
	var owner LockOwner
	if _, err := file.Seek(0, 0); err != nil {
		return owner
	}
	_ = json.NewDecoder(file).Decode(&owner)
	return owner
}

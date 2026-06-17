package app

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/davis7dotsh/hermes-box/internal/config"
	"github.com/davis7dotsh/hermes-box/internal/process"
)

func TestWithLockReusesUnlockedLockFile(t *testing.T) {
	root := t.TempDir()
	cfg := config.Config{DataDir: root}
	application := New(root, cfg, process.OSRunner{}, io.Discard, io.Discard)
	if err := application.prepareDirs(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "state", "operation.lock"), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}

	called := false
	if err := application.withLock(func() error {
		called = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("locked function was not called")
	}
}

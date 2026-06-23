package keychain

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"filippo.io/age"
)

func TestMemoryStoreCopiesSecrets(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	secret := []byte("secret")
	if err := store.Put("main", secret); err != nil {
		t.Fatal(err)
	}
	secret[0] = 'X'
	got, err := store.Get("main")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "secret" {
		t.Fatalf("got %q", got)
	}
	got[0] = 'Y'
	again, _ := store.Get("main")
	if string(again) != "secret" {
		t.Fatal("Get returned mutable store backing memory")
	}
}

func TestLoadOrCreateIdentityIsStable(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	first, created, err := LoadOrCreateIdentity(store, "main")
	if err != nil || !created {
		t.Fatalf("first load: created=%v err=%v", created, err)
	}
	second, created, err := LoadOrCreateIdentity(store, "main")
	if err != nil || created {
		t.Fatalf("second load: created=%v err=%v", created, err)
	}
	if first.String() != second.String() {
		t.Fatal("identity changed")
	}
}

func TestLoadIdentityNeverCreates(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	if _, err := LoadIdentity(store, "main"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing identity returned %v", err)
	}
	if len(store.secrets) != 0 {
		t.Fatal("load-only identity lookup mutated the store")
	}
	created, _, err := LoadOrCreateIdentity(store, "main")
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadIdentity(store, "main")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.String() != created.String() {
		t.Fatal("load-only lookup returned a different identity")
	}
}

func TestLoadIdentityRejectsMalformedSecret(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	if err := store.Put("main", []byte("not-an-age-identity")); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadIdentity(store, "main"); err == nil {
		t.Fatal("expected malformed stored identity rejection")
	}
}

func TestExportIdentity(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	identity, _, err := LoadOrCreateIdentity(store, "main")
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "nested", "identity.txt")
	if err := ExportIdentity(store, "main", destination); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := age.ParseX25519Identity(string(data[:len(data)-1]))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.String() != identity.String() {
		t.Fatal("exported wrong identity")
	}
	info, _ := os.Stat(destination)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", info.Mode().Perm())
	}
	if err := ExportIdentity(store, "main", destination); err == nil {
		t.Fatal("expected refusal to overwrite identity")
	}
}

func TestMemoryStoreNotFound(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	if _, err := store.Get("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
	if err := store.Delete("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestExportRejectsMalformedStoredIdentity(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	if err := store.Put("main", []byte("not-an-age-identity")); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "identity")
	if err := ExportIdentity(store, "main", destination); err == nil {
		t.Fatal("expected malformed identity rejection")
	}
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		t.Fatal("malformed identity created export file")
	}
}

func TestConcurrentLoadOrCreateReturnsOneIdentity(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	const workers = 32
	identities := make(chan string, workers)
	errors := make(chan error, workers)
	var wait sync.WaitGroup
	for i := 0; i < workers; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			identity, _, err := LoadOrCreateIdentity(store, "main")
			if err != nil {
				errors <- err
				return
			}
			identities <- identity.String()
		}()
	}
	wait.Wait()
	close(identities)
	close(errors)
	for err := range errors {
		t.Fatal(err)
	}
	var first string
	for identity := range identities {
		if first == "" {
			first = identity
		}
		if identity != first {
			t.Fatal("concurrent creators returned different identities")
		}
	}
}

func TestIdentityAccountScopesConfigurationOwnership(t *testing.T) {
	t.Parallel()
	first := IdentityAccount("/one/repo", "main")
	second := IdentityAccount("/two/repo", "main")
	if first == second || first != IdentityAccount("/one/repo", "main") {
		t.Fatal("identity account is not stable and ownership-scoped")
	}
}

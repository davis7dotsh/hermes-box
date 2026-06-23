package box

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMetadataOwnershipAndExactNames(t *testing.T) {
	home := filepath.Join(t.TempDir(), "state")
	store, err := NewStore(home)
	if err != nil {
		t.Fatal(err)
	}
	configDir := t.TempDir()
	metadata, err := NewMetadata("main", configDir, testOwnershipBinding(), time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	if metadata.VM != "main" || metadata.DataDisk != "main-data" {
		t.Fatalf("names = %q, %q", metadata.VM, metadata.DataDisk)
	}
	if metadata.VMType != "vz" || metadata.DataDiskFormat != "raw" {
		t.Fatalf("VZ disk identity = vmType %q, format %q", metadata.VMType, metadata.DataDiskFormat)
	}
	if err := store.CreateMetadata(metadata); err != nil {
		t.Fatal(err)
	}
	if _, err := store.VerifyOwnership("main", configDir); err != nil {
		t.Fatal(err)
	}
	if _, err := store.VerifyOwnership("main", t.TempDir()); err == nil {
		t.Fatal("different configuration directory adopted the box")
	}
	if err := store.CreateMetadata(metadata); err == nil {
		t.Fatal("duplicate metadata creation succeeded")
	}
}

func TestJournalIsAtomicAndStrict(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	want := Journal{Schema: 1, Operation: "create", Phase: "incomplete", Resources: []string{"main-data"}, StartedAt: time.Unix(1, 0)}
	if err := store.SaveJournal("main", want); err != nil {
		t.Fatal(err)
	}
	got, found, err := store.LoadJournal("main")
	if err != nil || !found || got.Phase != want.Phase {
		t.Fatalf("journal = %#v, found = %t, err = %v", got, found, err)
	}
	if err := store.ClearJournal("main"); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.LoadJournal("main"); err != nil || found {
		t.Fatalf("journal found after clear: %t, %v", found, err)
	}
	entries, err := os.ReadDir(filepath.Join(store.Home, "boxes"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name()[0] == '.' {
			t.Fatalf("temporary file leaked: %s", entry.Name())
		}
	}
}

func TestRebuildJournalRequiresCompleteDurableRecoveryState(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	incomplete := Journal{Schema: 1, Operation: "rebuild", Phase: "prepared", StartedAt: time.Unix(1, 0)}
	if err := store.SaveJournal("main", incomplete); err == nil {
		t.Fatal("incomplete rebuild journal was accepted")
	}
	root := t.TempDir()
	complete := incomplete
	complete.Recovery = &RebuildRecovery{
		BackupArchive: filepath.Join(root, "backup.age"), BackupEnvelope: filepath.Join(root, "backup.json"),
		BackupSHA256: strings.Repeat("a", 64), AppliedLock: filepath.Join(root, "applied.lock"),
		Artifacts: []JournalArtifact{{Path: filepath.Join(root, "artifact"), SHA256: strings.Repeat("b", 64)}},
	}
	if err := store.SaveJournal("main", complete); err != nil {
		t.Fatal(err)
	}
	loaded, found, err := store.LoadJournal("main")
	if err != nil || !found || loaded.Recovery.AppliedLock != complete.Recovery.AppliedLock {
		t.Fatalf("loaded rebuild journal = %#v, found=%t, err=%v", loaded, found, err)
	}
}

func TestRollbackJournalRequiresTwoDistinctDurableSnapshots(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	journal := Journal{
		Schema: 1, Operation: "rollback", Phase: "prepared", StartedAt: time.Unix(1, 0),
		Rollback: &RollbackRecovery{Component: "codex", PreviousSnapshot: filepath.Join(root, "previous.json"), CurrentSnapshot: filepath.Join(root, "current.json")},
	}
	if err := store.SaveJournal("main", journal); err != nil {
		t.Fatal(err)
	}
	journal.Rollback.CurrentSnapshot = journal.Rollback.PreviousSnapshot
	if err := store.SaveJournal("main", journal); err == nil {
		t.Fatal("rollback journal accepted one mutable snapshot path for both states")
	}
}

func TestUpdateJournalRequiresDurableComponentSnapshot(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	journal := Journal{
		Schema: 1, Operation: "update", Phase: "prepared", StartedAt: time.Unix(1, 0),
		Update: &UpdateRecovery{Component: "codex", Snapshot: filepath.Join(t.TempDir(), "latest.json")},
	}
	if err := store.SaveJournal("main", journal); err != nil {
		t.Fatal(err)
	}
	journal.Update.Snapshot = "relative.json"
	if err := store.SaveJournal("main", journal); err == nil {
		t.Fatal("update journal accepted a relative recovery snapshot")
	}
}

func TestOperationLockReportsOwner(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Acquire("main", "create")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	_, err = store.Acquire("main", "destroy")
	var busy *BusyError
	if !errors.As(err, &busy) || busy.Owner.Command != "create" {
		t.Fatalf("busy error = %#v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := store.Acquire("main", "destroy")
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestResolveHome(t *testing.T) {
	userHome := t.TempDir()
	got, err := ResolveHome(nil, userHome)
	if err != nil || got != filepath.Join(userHome, ".hermes-box") {
		t.Fatalf("home = %q, err = %v", got, err)
	}
	if _, err := ResolveHome([]string{"HERMES_BOX_HOME=relative"}, userHome); err == nil {
		t.Fatal("relative HERMES_BOX_HOME accepted")
	}
}

func TestRejectsPathLikeBoxName(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.LoadMetadata("../other"); err == nil {
		t.Fatal("path-like box name accepted")
	}
	if _, err := store.Acquire("../other", "create"); err == nil {
		t.Fatal("path-like lock name accepted")
	}
	if _, err := NewMetadata("../other", t.TempDir(), testOwnershipBinding(), time.Now()); err == nil {
		t.Fatal("path-like metadata name accepted")
	}
}

func testOwnershipBinding() OwnershipBinding {
	return OwnershipBinding{
		DefinitionSHA256: strings.Repeat("a", 64), DataDiskSize: 50 << 30,
		DataOwnershipMarker: strings.Repeat("b", 64),
	}
}

func TestNewStoreDoesNotChangeUnsafeDirectoryPermissions(t *testing.T) {
	home := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(home); err == nil {
		t.Fatal("public state directory accepted")
	}
	info, err := os.Stat(home)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("state directory permissions changed to %o", info.Mode().Perm())
	}
}

package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/davis7dotsh/hermes-box/internal/backup"
	"github.com/davis7dotsh/hermes-box/internal/keychain"
)

func TestPublishedBackupPresenceRetainsNewIdentity(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "before publication", err: errors.New("stream failed")},
		{name: "publication cleaned", err: &backup.PublicationError{Err: errors.New("sync failed")}},
		{name: "archive remains", err: &backup.PublicationError{ArchivePresent: true, Err: errors.New("cleanup failed")}, want: true},
		{name: "envelope remains", err: &backup.PublicationError{EnvelopePresent: true, Err: errors.New("cleanup failed")}, want: true},
		{name: "wrapped publication", err: errors.Join(errors.New("producer failed"), &backup.PublicationError{ArchivePresent: true, Err: errors.New("cleanup failed")}), want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := publishedBackupPresent(test.err); got != test.want {
				t.Fatalf("publishedBackupPresent() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestRetentionWarningReturnsVerifiedBackupAndContinues(t *testing.T) {
	result := backup.Result{Archive: "new.age", Envelope: "new.json", ArchiveSHA256: "abc"}
	var stderr bytes.Buffer
	mapped, retain, err := mapBackupCreateResult(result, &backup.RetentionWarning{
		Archive: result.Archive, Envelope: result.Envelope, Err: errors.New("old backup is locked"),
	}, &stderr)
	if err != nil || !retain {
		t.Fatalf("mapped warning = %#v, retain = %t, err = %v", mapped, retain, err)
	}
	if mapped.Archive != result.Archive || mapped.Envelope != result.Envelope || mapped.ArchiveSHA256 != result.ArchiveSHA256 {
		t.Fatalf("verified result was lost: %#v", mapped)
	}
	if !strings.Contains(stderr.String(), "retention pruning failed") {
		t.Fatalf("warning was not surfaced: %q", stderr.String())
	}
}

func TestBackupErrorWithPublishedResultRetainsIdentityAndResult(t *testing.T) {
	result := backup.Result{Archive: "new.age", Envelope: "new.json", ArchiveSHA256: "abc"}
	wantErr := errors.New("post-publication bookkeeping failed")
	mapped, retain, err := mapBackupCreateResult(result, wantErr, nil)
	if !errors.Is(err, wantErr) || !retain || mapped.Archive != result.Archive || mapped.Envelope != result.Envelope {
		t.Fatalf("mapped = %#v, retain = %t, err = %v", mapped, retain, err)
	}
}

func TestLatestVerifiedNeverCreatesMissingIdentity(t *testing.T) {
	keys := keychain.NewMemoryStore()
	def := Definition{Name: "main", ConfigDir: t.TempDir(), Home: t.TempDir()}
	backups := &defaultBackups{operations: &defaultOperations{keys: keys}}
	if _, err := backups.LatestVerified(context.Background(), def); !errors.Is(err, keychain.ErrNotFound) {
		t.Fatalf("LatestVerified error = %v, want keychain.ErrNotFound", err)
	}
	if _, err := keys.Get(identityAccount(def)); !errors.Is(err, keychain.ErrNotFound) {
		t.Fatalf("read-only backup lookup created an identity: %v", err)
	}
}

func TestLatestVerifiedReportsMissingBackupWithExistingIdentity(t *testing.T) {
	keys := keychain.NewMemoryStore()
	def := Definition{Name: "main", ConfigDir: t.TempDir(), Home: t.TempDir()}
	if _, _, err := keychain.LoadOrCreateIdentity(keys, identityAccount(def)); err != nil {
		t.Fatal(err)
	}
	backups := &defaultBackups{operations: &defaultOperations{keys: keys}}
	if _, err := backups.LatestVerified(context.Background(), def); err == nil || err.Error() != "no valid backup exists" {
		t.Fatalf("LatestVerified error = %v", err)
	}
}

func TestLatestVerifiedSameTimestampUsesDeterministicPathTieBreak(t *testing.T) {
	created := time.Unix(1, 123).UTC()
	if !newerVerifiedBackup(created, "/backups/main/b.envelope.json", created, "/backups/main/a.envelope.json") {
		t.Fatal("same-timestamp later path did not win deterministic tie-break")
	}
	if newerVerifiedBackup(created, "/backups/main/a.envelope.json", created, "/backups/main/b.envelope.json") {
		t.Fatal("same-timestamp earlier path replaced deterministic winner")
	}
}

func TestTransactionSnapshotPublishesBackupAndAppliedLockAsOneRecord(t *testing.T) {
	def := Definition{Name: "main", Home: t.TempDir()}
	lockPath := filepath.Join(t.TempDir(), "applied.lock")
	lock, err := encodeLock(validClosureLock())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, []byte(lock), 0o600); err != nil {
		t.Fatal(err)
	}
	result := BackupResult{Archive: "before.age", Envelope: "before.json", ArchiveSHA256: "abc"}
	if err := saveTransactionSnapshot(def, "codex", result, lockPath); err != nil {
		t.Fatal(err)
	}
	recordPath := filepath.Join(def.Home, "backups", def.Name, "transactions", "codex", "latest.json")
	record, err := loadTransactionSnapshot(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	if record.Backup != result || record.AppliedLock != lock {
		t.Fatalf("snapshot record = %#v", record)
	}
	if err := os.WriteFile(lockPath, []byte("schema: invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := saveTransactionSnapshot(def, "codex", BackupResult{Archive: "bad", Envelope: "bad", ArchiveSHA256: "bad"}, lockPath); err == nil {
		t.Fatal("invalid replacement lock was published")
	}
	unchanged, err := loadTransactionSnapshot(recordPath)
	if err != nil || unchanged.Backup != result {
		t.Fatalf("previous atomic snapshot was lost: %#v, %v", unchanged, err)
	}
}

func TestWriteTransactionSnapshotDurablyPublishesOnlyFinalRecord(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "transactions")
	path := filepath.Join(directory, "latest.json")
	lock, err := encodeLock(validClosureLock())
	if err != nil {
		t.Fatal(err)
	}
	first := transactionSnapshot{
		Backup: BackupResult{Archive: "first.age", Envelope: "first.json", ArchiveSHA256: "first-sha"}, AppliedLock: lock,
	}
	second := transactionSnapshot{
		Backup: BackupResult{Archive: "second.age", Envelope: "second.json", ArchiveSHA256: "second-sha"}, AppliedLock: lock,
	}
	if err := writeTransactionSnapshot(path, first); err != nil {
		t.Fatal(err)
	}
	if err := writeTransactionSnapshot(path, second); err != nil {
		t.Fatal(err)
	}
	published, err := loadTransactionSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	if published != second {
		t.Fatalf("published snapshot = %#v, want %#v", published, second)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("snapshot mode = %o, want 600", info.Mode().Perm())
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "latest.json" {
		t.Fatalf("atomic publication leaked temporary state: %#v", entries)
	}
}

func TestTransactionRestoreNeverCreatesMissingIdentity(t *testing.T) {
	keys := keychain.NewMemoryStore()
	def := Definition{Name: "main", ConfigDir: t.TempDir(), Home: t.TempDir()}
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
	operations := &defaultOperations{keys: keys}
	if err := operations.restoreTransactionSnapshot(context.Background(), def, "codex"); !errors.Is(err, keychain.ErrNotFound) {
		t.Fatalf("restoreTransactionSnapshot error = %v, want keychain.ErrNotFound", err)
	}
	if _, err := keys.Get(identityAccount(def)); !errors.Is(err, keychain.ErrNotFound) {
		t.Fatalf("rollback path created an identity: %v", err)
	}
}

func TestTransactionRestoreRejectsArchiveDifferentFromRecordedSnapshot(t *testing.T) {
	record := transactionSnapshot{Backup: BackupResult{ArchiveSHA256: "recorded"}}
	if err := verifyTransactionSnapshotArchive(record, backup.Envelope{ArchiveSHA256: "replacement"}); err == nil || !strings.Contains(err.Error(), "checksum changed") {
		t.Fatalf("checksum binding error = %v", err)
	}
	if err := verifyTransactionSnapshotArchive(record, backup.Envelope{ArchiveSHA256: "recorded"}); err != nil {
		t.Fatalf("matching recorded checksum rejected: %v", err)
	}
}

func TestRebuildRecoveryNeverCreatesMissingIdentity(t *testing.T) {
	keys := keychain.NewMemoryStore()
	def := Definition{Name: "main", ConfigDir: t.TempDir(), Home: t.TempDir()}
	operations := &defaultOperations{keys: keys}
	err := operations.RecoverRebuild(context.Background(), def, RecoveryState{}, BackupResult{Archive: "missing.age", Envelope: "missing.json"})
	if !errors.Is(err, keychain.ErrNotFound) {
		t.Fatalf("RecoverRebuild error = %v, want keychain.ErrNotFound", err)
	}
	if _, err := keys.Get(identityAccount(def)); !errors.Is(err, keychain.ErrNotFound) {
		t.Fatalf("rebuild rollback created an identity: %v", err)
	}
}

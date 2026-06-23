package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/klauspost/compress/zstd"
	"golang.org/x/sys/unix"
)

func TestCreateVerifyAndRestore(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	data := filepath.Join(root, "data")
	writeTestFile(t, filepath.Join(data, "home", "agent", "workspace", "note.txt"), "hello")
	writeTestFile(t, filepath.Join(data, "executor", "db.sqlite"), "database")
	if err := os.Symlink("note.txt", filepath.Join(data, "home", "agent", "workspace", "latest")); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(root, "applied.lock")
	writeTestFile(t, lock, "schema: 1\n")
	artifactPath := filepath.Join(root, "artifact")
	writeTestFile(t, artifactPath, "binary")
	artifactHash, _ := fileSHA256(artifactPath)
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	backupDir := filepath.Join(root, "backups")
	result, err := Create(context.Background(), Source{DataTar: guestTarFromDir(t, data), WaitDataTar: successfulWait, AppliedLock: lock, Artifacts: []Artifact{{Name: "tool", SHA256: artifactHash, Path: artifactPath}}}, CreateOptions{
		Directory: backupDir, Box: "main", Label: "configured", Keep: 2,
		Recipient: identity.Recipient(), Identity: identity,
		ValidateClosure: allowClosure,
		Now:             func() time.Time { return time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := Verify(context.Background(), result.Archive, result.Envelope, identity, allowClosure)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(bundle.Root, "data", "cache")); !os.IsNotExist(err) {
		t.Fatal("cache directory was included")
	}
	var verifiedData bytes.Buffer
	if err := WriteDataTar(context.Background(), bundle.Root, bundle.Manifest, &verifiedData); err != nil {
		t.Fatal(err)
	}
	if got := readTarEntries(t, verifiedData.Bytes())["data/home/agent/workspace/latest"].Linkname; got != "note.txt" {
		t.Fatalf("symlink target got %q", got)
	}
	bundle.Cleanup()
	destination := filepath.Join(root, "restored")
	manifest, err := Restore(context.Background(), result.Archive, result.Envelope, identity, allowClosure, destination)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Box != "main" {
		t.Fatalf("box %q", manifest.Box)
	}
	var restoredData bytes.Buffer
	if err := WriteDataTar(context.Background(), destination, manifest, &restoredData); err != nil {
		t.Fatal(err)
	}
	if contents := secretBody(t, restoredData.Bytes(), "data/home/agent/workspace/note.txt"); string(contents) != "hello" {
		t.Fatalf("restored contents %q", contents)
	}
	if _, err := Restore(context.Background(), result.Archive, result.Envelope, identity, allowClosure, destination); err == nil {
		t.Fatal("expected restore to reject existing destination")
	}
}

func TestGuestTarPreservesLinuxMetadataRoundTrip(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	lock := filepath.Join(root, "lock")
	writeTestFile(t, lock, "lock")
	input := guestTarBytes(t, []tarInput{
		{Header: tar.Header{Name: "data", Typeflag: tar.TypeDir, Mode: 0o710, Uid: 55, Gid: 66, Uname: "root-owner", Gname: "root-group"}},
		{Header: tar.Header{Name: "data/home", Typeflag: tar.TypeDir, Mode: 0o750, Uid: 1000, Gid: 1000, Uname: "agent", Gname: "agent"}},
		{Header: tar.Header{Name: "data/home/agent", Typeflag: tar.TypeDir, Mode: 0o700, Uid: 1000, Gid: 1000, Uname: "agent", Gname: "agent"}},
		{Header: tar.Header{Name: "data/home/agent/secret", Typeflag: tar.TypeReg, Mode: 0o640, Uid: 1234, Gid: 2345, Uname: "worker", Gname: "private"}, Body: []byte("secret")},
		{Header: tar.Header{Name: "data/home/agent/Foo", Typeflag: tar.TypeReg, Mode: 0o600, Uid: 1000, Gid: 1000}, Body: []byte("upper")},
		{Header: tar.Header{Name: "data/home/agent/foo", Typeflag: tar.TypeReg, Mode: 0o600, Uid: 1000, Gid: 1000}, Body: []byte("lower")},
		{Header: tar.Header{Name: "data/home/agent/é", Typeflag: tar.TypeReg, Mode: 0o600, Uid: 1000, Gid: 1000}, Body: []byte("composed")},
		{Header: tar.Header{Name: "data/home/agent/é", Typeflag: tar.TypeReg, Mode: 0o600, Uid: 1000, Gid: 1000}, Body: []byte("decomposed")},
		{Header: tar.Header{Name: "data/home/agent/readonly", Typeflag: tar.TypeDir, Mode: 0o500, Uid: 1000, Gid: 1000}},
		{Header: tar.Header{Name: "data/home/agent/readonly/child", Typeflag: tar.TypeReg, Mode: 0o400, Uid: 1000, Gid: 1000}, Body: []byte("child")},
		{Header: tar.Header{Name: "data/home/agent/current", Typeflag: tar.TypeSymlink, Mode: 0o777, Uid: 3456, Gid: 4567, Uname: "linker", Gname: "links", Linkname: "secret"}},
		{Header: tar.Header{Name: "data/home/agent/.codex", Typeflag: tar.TypeDir, Mode: 0o700, Uid: 1000, Gid: 1000, PAXRecords: map[string]string{"HERMESBOX.absent": "1"}}},
		{Header: tar.Header{Name: "data/executor", Typeflag: tar.TypeDir, Mode: 0o770, Uid: 2000, Gid: 3000, Uname: "executor", Gname: "executor"}},
	})
	identity, _ := age.GenerateX25519Identity()
	result, err := Create(context.Background(), Source{DataTar: io.NopCloser(bytes.NewReader(input)), WaitDataTar: successfulWait, AppliedLock: lock}, CreateOptions{Directory: filepath.Join(root, "backups"), Box: "main", Label: "metadata", Keep: 1, Recipient: identity.Recipient(), Identity: identity, ValidateClosure: allowClosure})
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := Verify(context.Background(), result.Archive, result.Envelope, identity, allowClosure)
	if err != nil {
		t.Fatal(err)
	}
	defer bundle.Cleanup()
	var output bytes.Buffer
	if err := WriteDataTar(context.Background(), bundle.Root, bundle.Manifest, &output); err != nil {
		t.Fatal(err)
	}
	entries := readTarEntries(t, output.Bytes())
	dataRoot := entries["data"]
	if dataRoot.Uid != 55 || dataRoot.Gid != 66 || dataRoot.Mode != 0o710 || dataRoot.Uname != "root-owner" || dataRoot.Gname != "root-group" {
		t.Fatalf("data-root metadata was not preserved: %+v", dataRoot)
	}
	secret := entries["data/home/agent/secret"]
	if secret.Uid != 1234 || secret.Gid != 2345 || secret.Mode != 0o640 || secret.Uname != "worker" || secret.Gname != "private" || string(secretBody(t, output.Bytes(), "data/home/agent/secret")) != "secret" {
		t.Fatalf("file metadata was not preserved: %+v", secret)
	}
	link := entries["data/home/agent/current"]
	if link.Uid != 3456 || link.Gid != 4567 || link.Linkname != "secret" || link.Typeflag != tar.TypeSymlink {
		t.Fatalf("symlink metadata was not preserved: %+v", link)
	}
	executor := entries["data/executor"]
	if executor.Uid != 2000 || executor.Gid != 3000 || executor.Mode != 0o770 {
		t.Fatalf("directory metadata was not preserved: %+v", executor)
	}
	absent := entries["data/home/agent/.codex"]
	if absent.Typeflag != tar.TypeDir || absent.PAXRecords["HERMESBOX.absent"] != "1" {
		t.Fatalf("absent-scope marker was not preserved: %+v", absent)
	}
	if string(secretBody(t, output.Bytes(), "data/home/agent/Foo")) != "upper" || string(secretBody(t, output.Bytes(), "data/home/agent/foo")) != "lower" {
		t.Fatal("case-distinct Linux paths did not survive host staging")
	}
	if string(secretBody(t, output.Bytes(), "data/home/agent/é")) != "composed" || string(secretBody(t, output.Bytes(), "data/home/agent/é")) != "decomposed" {
		t.Fatal("Unicode-distinct Linux paths did not survive host staging")
	}
	if entries["data/home/agent/readonly"].Mode != 0o500 || string(secretBody(t, output.Bytes(), "data/home/agent/readonly/child")) != "child" {
		t.Fatal("read-only directory with child did not round trip")
	}
}

func TestGuestTarRejectsUnsafeEntries(t *testing.T) {
	t.Parallel()
	base := []tarInput{
		{Header: tar.Header{Name: "data/home", Typeflag: tar.TypeDir, Mode: 0o700, Uid: 1000, Gid: 1000}},
		{Header: tar.Header{Name: "data/home/agent", Typeflag: tar.TypeDir, Mode: 0o700, Uid: 1000, Gid: 1000}},
		{Header: tar.Header{Name: "data/executor", Typeflag: tar.TypeDir, Mode: 0o700, Uid: 1000, Gid: 1000}},
	}
	tests := []struct {
		name    string
		entries []tarInput
	}{
		{name: "traversal", entries: []tarInput{{Header: tar.Header{Name: "data/../escape", Typeflag: tar.TypeReg, Mode: 0o600}, Body: []byte("x")}}},
		{name: "absolute", entries: []tarInput{{Header: tar.Header{Name: "/data/home/escape", Typeflag: tar.TypeReg, Mode: 0o600}, Body: []byte("x")}}},
		{name: "special file", entries: []tarInput{{Header: tar.Header{Name: "data/home/fifo", Typeflag: tar.TypeFifo, Mode: 0o600}}}},
		{name: "hard link", entries: []tarInput{{Header: tar.Header{Name: "data/home/hard", Typeflag: tar.TypeLink, Mode: 0o600, Linkname: "data/home/target"}}}},
		{name: "escaping symlink", entries: []tarInput{{Header: tar.Header{Name: "data/home/link", Typeflag: tar.TypeSymlink, Mode: 0o777, Linkname: "../../outside"}}}},
		{name: "cache", entries: []tarInput{{Header: tar.Header{Name: "data/cache/file", Typeflag: tar.TypeReg, Mode: 0o600}, Body: []byte("x")}}},
		{name: "duplicate", entries: []tarInput{{Header: tar.Header{Name: "data/home", Typeflag: tar.TypeDir, Mode: 0o700}}}},
		{name: "invalid absent value", entries: []tarInput{{Header: tar.Header{Name: "data/home/absent", Typeflag: tar.TypeDir, Mode: 0o700, PAXRecords: map[string]string{"HERMESBOX.absent": "yes"}}}}},
		{name: "absent regular file", entries: []tarInput{{Header: tar.Header{Name: "data/home/absent", Typeflag: tar.TypeReg, Mode: 0o600, PAXRecords: map[string]string{"HERMESBOX.absent": "1"}}}}},
		{name: "unknown marker", entries: []tarInput{{Header: tar.Header{Name: "data/home/absent", Typeflag: tar.TypeDir, Mode: 0o700, PAXRecords: map[string]string{"HERMESBOX.unknown": "1"}}}}},
		{name: "required root absent", entries: []tarInput{{Header: tar.Header{Name: "data", Typeflag: tar.TypeDir, Mode: 0o700, PAXRecords: map[string]string{"HERMESBOX.absent": "1"}}}}},
		{name: "absent path with child", entries: []tarInput{
			{Header: tar.Header{Name: "data/home/absent", Typeflag: tar.TypeDir, Mode: 0o700, PAXRecords: map[string]string{"HERMESBOX.absent": "1"}}},
			{Header: tar.Header{Name: "data/home/absent/file", Typeflag: tar.TypeReg, Mode: 0o600}, Body: []byte("x")},
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := guestTarBytes(t, append(append([]tarInput(nil), base...), test.entries...))
			if _, err := stageGuestTar(context.Background(), bytes.NewReader(input), t.TempDir(), []string{"executor", "home/agent"}); err == nil {
				t.Fatal("expected guest tar rejection")
			}
		})
	}
}

func TestScopedGuestTarEnforcesExactRequestedPaths(t *testing.T) {
	t.Parallel()
	scope := "home/agent/.claude"
	valid := guestTarBytes(t, []tarInput{
		{Header: tar.Header{Name: "data/" + scope, Typeflag: tar.TypeDir, Mode: 0o700, Uid: 1000, Gid: 1000}},
		{Header: tar.Header{Name: "data/" + scope + "/settings.json", Typeflag: tar.TypeReg, Mode: 0o600, Uid: 1000, Gid: 1000}, Body: []byte("{}")},
	})
	entries, err := stageGuestTar(context.Background(), bytes.NewReader(valid), t.TempDir(), []string{scope})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("got %d staged entries, want scope, file, and synthetic data root", len(entries))
	}
	absent := guestTarBytes(t, []tarInput{{Header: tar.Header{Name: "data/" + scope, Typeflag: tar.TypeDir, Mode: 0o700, Uid: 1000, Gid: 1000, PAXRecords: map[string]string{"HERMESBOX.absent": "1"}}}})
	absentEntries, err := stageGuestTar(context.Background(), bytes.NewReader(absent), t.TempDir(), []string{scope})
	if err != nil {
		t.Fatal(err)
	}
	foundAbsent := false
	for _, entry := range absentEntries {
		if entry.Entry.Path == "data/"+scope {
			foundAbsent = entry.Entry.Absent
		}
	}
	if !foundAbsent {
		t.Fatal("scoped absent marker was not retained")
	}
	scopedManifest := Manifest{DataPaths: []string{scope}}
	for _, entry := range absentEntries {
		scopedManifest.Entries = append(scopedManifest.Entries, entry.Entry)
	}
	var scopedOutput bytes.Buffer
	if err := WriteDataTar(context.Background(), t.TempDir(), scopedManifest, &scopedOutput); err != nil {
		t.Fatal(err)
	}
	scopedHeaders := readTarEntries(t, scopedOutput.Bytes())
	if _, ok := scopedHeaders["data"]; ok {
		t.Fatal("scoped restore exported structural data-root metadata")
	}
	if scopedHeaders["data/"+scope].PAXRecords["HERMESBOX.absent"] != "1" {
		t.Fatal("scoped absent marker was not exported")
	}
	tests := []struct {
		name   string
		input  []byte
		scopes []string
	}{
		{name: "missing requested scope", input: guestTarBytes(t, nil), scopes: []string{scope}},
		{name: "cross scope extra", input: guestTarBytes(t, append([]tarInput{
			{Header: tar.Header{Name: "data/" + scope, Typeflag: tar.TypeDir, Mode: 0o700}},
		}, tarInput{Header: tar.Header{Name: "data/executor", Typeflag: tar.TypeDir, Mode: 0o700}})), scopes: []string{scope}},
		{name: "cross scope symlink", input: guestTarBytes(t, []tarInput{
			{Header: tar.Header{Name: "data/" + scope, Typeflag: tar.TypeDir, Mode: 0o700}},
			{Header: tar.Header{Name: "data/" + scope + "/executor", Typeflag: tar.TypeSymlink, Mode: 0o777, Linkname: "../../../executor"}},
		}), scopes: []string{scope}},
		{name: "entry beneath symlink", input: guestTarBytes(t, []tarInput{
			{Header: tar.Header{Name: "data/" + scope, Typeflag: tar.TypeDir, Mode: 0o700}},
			{Header: tar.Header{Name: "data/" + scope + "/current", Typeflag: tar.TypeSymlink, Mode: 0o777, Linkname: "."}},
			{Header: tar.Header{Name: "data/" + scope + "/current/settings.json", Typeflag: tar.TypeReg, Mode: 0o600}, Body: []byte("{}")},
		}), scopes: []string{scope}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := stageGuestTar(context.Background(), bytes.NewReader(test.input), t.TempDir(), test.scopes); err == nil {
				t.Fatal("expected scoped guest tar rejection")
			}
		})
	}
	if _, err := normalizeDataPaths([]string{"home", "home/agent/.claude"}); err == nil {
		t.Fatal("expected overlapping scope rejection")
	}
}

func TestCreateRejectsUnsafeSources(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	data := filepath.Join(root, "data")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../outside", filepath.Join(data, "escape")); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(root, "lock")
	writeTestFile(t, lock, "lock")
	identity, _ := age.GenerateX25519Identity()
	_, err := Create(context.Background(), Source{DataTar: guestTarFromDir(t, data), WaitDataTar: successfulWait, AppliedLock: lock}, CreateOptions{Directory: filepath.Join(root, "backups"), Box: "main", Label: "bad", Keep: 1, Recipient: identity.Recipient(), Identity: identity, ValidateClosure: allowClosure})
	if err == nil {
		t.Fatal("expected unsafe symlink rejection")
	}
}

func TestCreateRejectsSpecialFilesAndArtifactSymlinks(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	data := filepath.Join(root, "data")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(data, "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(root, "lock")
	writeTestFile(t, lock, "lock")
	identity, _ := age.GenerateX25519Identity()
	if _, err := Create(context.Background(), Source{DataTar: guestTarFromDir(t, data), WaitDataTar: successfulWait, AppliedLock: lock}, CreateOptions{Directory: filepath.Join(root, "backups"), Box: "main", Label: "bad", Keep: 1, Recipient: identity.Recipient(), Identity: identity, ValidateClosure: allowClosure}); err == nil {
		t.Fatal("expected special file rejection")
	}
	if err := os.Remove(filepath.Join(data, "fifo")); err != nil {
		t.Fatal(err)
	}
	realArtifact := filepath.Join(root, "real")
	writeTestFile(t, realArtifact, "artifact")
	digest, _ := fileSHA256(realArtifact)
	symlink := filepath.Join(root, "artifact-link")
	if err := os.Symlink(realArtifact, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := Create(context.Background(), Source{DataTar: guestTarFromDir(t, data), WaitDataTar: successfulWait, AppliedLock: lock, Artifacts: []Artifact{{Name: "bad", SHA256: digest, Path: symlink}}}, CreateOptions{Directory: filepath.Join(root, "backups"), Box: "main", Label: "bad", Keep: 1, Recipient: identity.Recipient(), Identity: identity, ValidateClosure: allowClosure}); err == nil {
		t.Fatal("expected artifact symlink rejection")
	}
}

func TestVerifyRejectsTraversalBeforeWriting(t *testing.T) {
	t.Parallel()
	identity, _ := age.GenerateX25519Identity()
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "bad.tar.zst.age")
	manifest := Manifest{Schema: 1, Format: Format, CreatedAt: time.Now(), Box: "main", AppliedLockSHA256: stringsOf('a', 64), Entries: []Entry{{Path: "../escape", Type: "file", Mode: 0o600, SHA256: stringsOf('b', 64)}}}
	writeRawArchive(t, archivePath, identity.Recipient(), manifest, "../escape", []byte{})
	hash, _ := fileSHA256(archivePath)
	envelopePath := filepath.Join(dir, "bad.envelope.json")
	data, _ := json.Marshal(Envelope{Schema: 1, Format: EnvelopeFormat, Archive: filepath.Base(archivePath), ArchiveSHA256: hash, RecipientFingerprint: Fingerprint(identity.Recipient())})
	if err := os.WriteFile(envelopePath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(context.Background(), archivePath, envelopePath, identity, allowClosure); err == nil {
		t.Fatal("expected traversal rejection")
	}
	destination := filepath.Join(dir, "restore")
	if _, err := Restore(context.Background(), archivePath, envelopePath, identity, allowClosure, destination); err == nil {
		t.Fatal("expected restore traversal rejection")
	}
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		t.Fatal("restore created destination before complete verification")
	}
}

func TestCreateCancellationLeavesNoPartial(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	data := filepath.Join(root, "data")
	writeTestFile(t, filepath.Join(data, "large"), stringsOf('x', 1<<20))
	lock := filepath.Join(root, "lock")
	writeTestFile(t, lock, "lock")
	identity, _ := age.GenerateX25519Identity()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dir := filepath.Join(root, "backups")
	if _, err := Create(ctx, Source{DataTar: guestTarFromDir(t, data), WaitDataTar: successfulWait, AppliedLock: lock}, CreateOptions{Directory: dir, Box: "main", Label: "cancel", Keep: 1, Recipient: identity.Recipient(), Identity: identity, ValidateClosure: allowClosure}); err == nil {
		t.Fatal("expected cancellation")
	}
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("partial outputs remain: %v", entries)
	}
}

func TestCreateNeverExposesPlaintextGuestStaging(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	lock := filepath.Join(root, "lock")
	writeTestFile(t, lock, "lock")
	identity, _ := age.GenerateX25519Identity()
	input := guestTarBytes(t, []tarInput{
		{Header: tar.Header{Name: "data/home/agent", Typeflag: tar.TypeDir, Mode: 0o700}},
		{Header: tar.Header{Name: "data/home/agent/large", Typeflag: tar.TypeReg, Mode: 0o600}, Body: bytes.Repeat([]byte("secret"), 1<<18)},
		{Header: tar.Header{Name: "data/executor", Typeflag: tar.TypeDir, Mode: 0o700}},
	})
	stream := newGatedReadCloser(input, 32<<10)
	directory := filepath.Join(root, "backups")
	done := make(chan error, 1)
	go func() {
		_, err := Create(context.Background(), Source{DataTar: stream, WaitDataTar: successfulWait, AppliedLock: lock}, CreateOptions{Directory: directory, Box: "main", Label: "stream", Keep: 1, Recipient: identity.Recipient(), Identity: identity, ValidateClosure: allowClosure})
		done <- err
	}()
	<-stream.blocked
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("guest bytes became addressable while backup streamed: %v", entries)
	}
	close(stream.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	entries, err = os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			t.Fatalf("private backup staging remained after success: %s", entry.Name())
		}
	}
}

func TestCreateVerificationDoesNotExtractGuestPayloads(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	data := filepath.Join(root, "data")
	writeTestFile(t, filepath.Join(data, "home", "agent", "secret"), stringsOf('s', 2<<20))
	lock := filepath.Join(root, "lock")
	writeTestFile(t, lock, "lock")
	artifactPath := filepath.Join(root, "artifact")
	writeTestFile(t, artifactPath, "artifact")
	artifactHash, _ := fileSHA256(artifactPath)
	identity, _ := age.GenerateX25519Identity()
	verificationCalls := 0
	validator := func(_ []byte, artifacts []Artifact) error {
		if len(artifacts) != 1 {
			return fmt.Errorf("closure received %d artifacts", len(artifacts))
		}
		if !strings.Contains(artifacts[0].Path, ".hermes-box-verified-backup-") {
			return nil
		}
		verificationCalls++
		stagingRoot := filepath.Dir(filepath.Dir(filepath.Dir(artifacts[0].Path)))
		if _, err := os.Lstat(filepath.Join(stagingRoot, "payloads")); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("plaintext guest payload staging exists during backup verification: %v", err)
		}
		return nil
	}
	_, err := Create(context.Background(), Source{DataTar: guestTarFromDir(t, data), WaitDataTar: successfulWait, AppliedLock: lock, Artifacts: []Artifact{{Name: "artifact", SHA256: artifactHash, Path: artifactPath}}}, CreateOptions{Directory: filepath.Join(root, "backups"), Box: "main", Label: "stream", Keep: 1, Recipient: identity.Recipient(), Identity: identity, ValidateClosure: validator})
	if err != nil {
		t.Fatal(err)
	}
	if verificationCalls != 2 {
		t.Fatalf("saw %d streaming verification calls, want publication and retention verification", verificationCalls)
	}
}

func TestCreateSameSecondUsesDeterministicSequence(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	data := filepath.Join(root, "data")
	writeTestFile(t, filepath.Join(data, "home", "agent", "file"), "data")
	lock := filepath.Join(root, "lock")
	writeTestFile(t, lock, "lock")
	identity, _ := age.GenerateX25519Identity()
	directory := filepath.Join(root, "backups")
	now := func() time.Time { return time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC) }
	first, err := Create(context.Background(), Source{DataTar: guestTarFromDir(t, data), WaitDataTar: successfulWait, AppliedLock: lock}, CreateOptions{Directory: directory, Box: "main", Label: "same", Keep: 2, Recipient: identity.Recipient(), Identity: identity, ValidateClosure: allowClosure, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Create(context.Background(), Source{DataTar: guestTarFromDir(t, data), WaitDataTar: successfulWait, AppliedLock: lock}, CreateOptions{Directory: directory, Box: "main", Label: "same", Keep: 2, Recipient: identity.Recipient(), Identity: identity, ValidateClosure: allowClosure, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if first.Archive == second.Archive || !strings.HasSuffix(second.Archive, "-0001.tar.zst.age") {
		t.Fatalf("same-second names are not deterministically ordered: %s then %s", first.Archive, second.Archive)
	}
}

func TestCreateRequiresSuccessfulGuestProducer(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	data := filepath.Join(root, "data")
	writeTestFile(t, filepath.Join(data, "home", "agent", "file"), "data")
	lock := filepath.Join(root, "lock")
	writeTestFile(t, lock, "lock")
	identity, _ := age.GenerateX25519Identity()
	producerErr := errors.New("guest tar traversal failed after writing end blocks")
	_, err := Create(context.Background(), Source{DataTar: guestTarFromDir(t, data), WaitDataTar: func() error { return producerErr }, AppliedLock: lock}, CreateOptions{Directory: filepath.Join(root, "backups"), Box: "main", Label: "partial", Keep: 1, Recipient: identity.Recipient(), Identity: identity, ValidateClosure: allowClosure})
	if !errors.Is(err, producerErr) {
		t.Fatalf("expected producer error, got %v", err)
	}
	archives, _ := filepath.Glob(filepath.Join(root, "backups", "*.age"))
	if len(archives) != 0 {
		t.Fatalf("producer failure published archives: %v", archives)
	}
}

func TestEarlyFailureClosesAndWaitsForGuestProducer(t *testing.T) {
	t.Parallel()
	stream := newBlockingReadCloser()
	waited := make(chan struct{})
	_, err := Create(context.Background(), Source{DataTar: stream, WaitDataTar: func() error { close(waited); return nil }}, CreateOptions{})
	if err == nil {
		t.Fatal("expected option validation failure")
	}
	select {
	case <-stream.closed:
	default:
		t.Fatal("early failure did not close guest producer")
	}
	select {
	case <-waited:
	default:
		t.Fatal("early failure did not wait for guest producer")
	}
}

func TestCreateCancellationClosesBlockedGuestStream(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	lock := filepath.Join(root, "lock")
	writeTestFile(t, lock, "lock")
	identity, _ := age.GenerateX25519Identity()
	stream := newBlockingReadCloser()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := Create(ctx, Source{DataTar: stream, WaitDataTar: successfulWait, AppliedLock: lock}, CreateOptions{Directory: filepath.Join(root, "backups"), Box: "main", Label: "blocked", Keep: 1, Recipient: identity.Recipient(), Identity: identity, ValidateClosure: allowClosure})
		done <- err
	}()
	<-stream.started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context cancellation, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not close blocked guest stream")
	}
}

func TestRetentionKeepsNewestVerifiedBackup(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	data := filepath.Join(root, "data")
	writeTestFile(t, filepath.Join(data, "home", "agent", "file"), "data")
	lock := filepath.Join(root, "lock")
	writeTestFile(t, lock, "lock")
	identity, _ := age.GenerateX25519Identity()
	dir := filepath.Join(root, "backups")
	for i := 0; i < 3; i++ {
		_, err := Create(context.Background(), Source{DataTar: guestTarFromDir(t, data), WaitDataTar: successfulWait, AppliedLock: lock}, CreateOptions{Directory: dir, Box: "main", Label: string(rune('a' + i)), Keep: 2, Recipient: identity.Recipient(), Identity: identity, ValidateClosure: allowClosure, Now: func() time.Time { return time.Date(2026, 6, 22, 12, 0, i, 0, time.UTC) }})
		if err != nil {
			t.Fatal(err)
		}
	}
	envelopes, _ := filepath.Glob(filepath.Join(dir, "*.envelope.json"))
	archives, _ := filepath.Glob(filepath.Join(dir, "*.tar.zst.age"))
	if len(envelopes) != 2 || len(archives) != 2 {
		t.Fatalf("got %d envelopes and %d archives", len(envelopes), len(archives))
	}
}

func TestRetentionFailureKeepsJustPublishedVerifiedPair(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	data := filepath.Join(root, "data")
	writeTestFile(t, filepath.Join(data, "home", "agent", "file"), "data")
	lock := filepath.Join(root, "lock")
	writeTestFile(t, lock, "lock")
	identity, _ := age.GenerateX25519Identity()
	validationCount := 0
	retentionFailure := errors.New("retention verification failed")
	validator := func(_ []byte, _ []Artifact) error {
		validationCount++
		if validationCount == 3 {
			return retentionFailure
		}
		return nil
	}
	dir := filepath.Join(root, "backups")
	result, err := Create(context.Background(), Source{DataTar: guestTarFromDir(t, data), WaitDataTar: successfulWait, AppliedLock: lock}, CreateOptions{Directory: dir, Box: "main", Label: "retention", Keep: 1, Recipient: identity.Recipient(), Identity: identity, ValidateClosure: validator})
	if !errors.Is(err, retentionFailure) {
		t.Fatalf("expected retention failure, got %v", err)
	}
	var warning *RetentionWarning
	if !errors.As(err, &warning) {
		t.Fatalf("expected retention warning, got %T", err)
	}
	var publicationError *PublicationError
	if errors.As(err, &publicationError) {
		t.Fatalf("retention warning was misreported as publication failure: %+v", publicationError)
	}
	if result.Archive == "" || result.Envelope == "" || warning.Archive != result.Archive || warning.Envelope != result.Envelope {
		t.Fatalf("warning did not preserve the published result: result=%+v warning=%+v", result, warning)
	}
	if bundle, verifyErr := Verify(context.Background(), result.Archive, result.Envelope, identity, allowClosure); verifyErr != nil {
		t.Fatalf("published backup is not recovery-valid: %v", verifyErr)
	} else {
		bundle.Cleanup()
	}
	archives, _ := filepath.Glob(filepath.Join(dir, "*.tar.zst.age"))
	envelopes, _ := filepath.Glob(filepath.Join(dir, "*.envelope.json"))
	if len(archives) != 1 || len(envelopes) != 1 {
		t.Fatalf("retention warning lost published pair: archive=%v envelope=%v", archives, envelopes)
	}
}

func TestRetentionPruningFailurePreservesNewVerifiedPairAtEveryFilesystemStep(t *testing.T) {
	steps := []string{"remove-envelope", "sync-envelope", "remove-archive", "sync-archive"}
	for failureStep := range steps {
		failureStep := failureStep
		t.Run(steps[failureStep], func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			data := filepath.Join(root, "data")
			writeTestFile(t, filepath.Join(data, "home", "agent", "file"), "data")
			lock := filepath.Join(root, "lock")
			writeTestFile(t, lock, "lock")
			identity, err := age.GenerateX25519Identity()
			if err != nil {
				t.Fatal(err)
			}
			directory := filepath.Join(root, "backups")
			createBackup := func(second int, label string) Result {
				t.Helper()
				result, err := Create(context.Background(), Source{DataTar: guestTarFromDir(t, data), WaitDataTar: successfulWait, AppliedLock: lock}, CreateOptions{
					Directory: directory, Box: "main", Label: label, Keep: 5,
					Recipient: identity.Recipient(), Identity: identity, ValidateClosure: allowClosure,
					Now: func() time.Time { return time.Date(2026, 6, 22, 12, 0, second, 0, time.UTC) },
				})
				if err != nil {
					t.Fatal(err)
				}
				return result
			}
			createBackup(1, "old")
			injected := errors.New("injected retention filesystem failure")
			var calls []string
			step := 0
			operations := retentionFileOps{
				remove: func(path string) error {
					kind := "remove-archive"
					if strings.HasSuffix(path, ".envelope.json") {
						kind = "remove-envelope"
					}
					calls = append(calls, kind)
					if step == failureStep {
						return injected
					}
					step++
					return os.Remove(path)
				},
				syncDirectory: func(path string) error {
					kind := "sync-archive"
					if step == 1 {
						kind = "sync-envelope"
					}
					calls = append(calls, kind)
					if step == failureStep {
						return injected
					}
					step++
					return syncDirectory(path)
				},
			}
			current, err := create(context.Background(), Source{DataTar: guestTarFromDir(t, data), WaitDataTar: successfulWait, AppliedLock: lock}, CreateOptions{
				Directory: directory, Box: "main", Label: "current", Keep: 1,
				Recipient: identity.Recipient(), Identity: identity, ValidateClosure: allowClosure,
				Now: func() time.Time { return time.Date(2026, 6, 22, 12, 0, 2, 0, time.UTC) },
			}, operations)
			if !errors.Is(err, injected) {
				t.Fatalf("retention error = %v, want injected failure", err)
			}
			var warning *RetentionWarning
			if !errors.As(err, &warning) || warning.Archive != current.Archive || warning.Envelope != current.Envelope {
				t.Fatalf("retention failure did not return the published result: result=%+v error=%v", current, err)
			}
			wantCalls := steps[:failureStep+1]
			if strings.Join(calls, ",") != strings.Join(wantCalls, ",") {
				t.Fatalf("filesystem calls = %v, want %v", calls, wantCalls)
			}
			bundle, err := Verify(context.Background(), current.Archive, current.Envelope, identity, allowClosure)
			if err != nil {
				t.Fatalf("new backup became invalid after %s failed: %v", steps[failureStep], err)
			}
			bundle.Cleanup()
			valid := countVerifiedBackups(t, directory, identity)
			if valid < 1 {
				t.Fatalf("retention failure left %d valid backups", valid)
			}
		})
	}
}

func TestRetentionPruningTreatsMissingOldFilesAsIdempotent(t *testing.T) {
	for _, missing := range []string{"envelope", "archive"} {
		missing := missing
		t.Run(missing, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			data := filepath.Join(root, "data")
			writeTestFile(t, filepath.Join(data, "home", "agent", "file"), "data")
			lock := filepath.Join(root, "lock")
			writeTestFile(t, lock, "lock")
			identity, err := age.GenerateX25519Identity()
			if err != nil {
				t.Fatal(err)
			}
			directory := filepath.Join(root, "backups")
			old, err := Create(context.Background(), Source{DataTar: guestTarFromDir(t, data), WaitDataTar: successfulWait, AppliedLock: lock}, CreateOptions{
				Directory: directory, Box: "main", Label: "old", Keep: 5,
				Recipient: identity.Recipient(), Identity: identity, ValidateClosure: allowClosure,
				Now: func() time.Time { return time.Date(2026, 6, 22, 12, 0, 1, 0, time.UTC) },
			})
			if err != nil {
				t.Fatal(err)
			}
			var calls []string
			operations := retentionFileOps{
				remove: func(path string) error {
					kind := "archive"
					if strings.HasSuffix(path, ".envelope.json") {
						kind = "envelope"
					}
					calls = append(calls, "remove-"+kind)
					if err := os.Remove(path); err != nil {
						return err
					}
					if kind == missing {
						return &os.PathError{Op: "remove", Path: path, Err: os.ErrNotExist}
					}
					return nil
				},
				syncDirectory: func(path string) error {
					if len(calls) == 1 {
						calls = append(calls, "sync-envelope")
					} else {
						calls = append(calls, "sync-archive")
					}
					return syncDirectory(path)
				},
			}
			current, err := create(context.Background(), Source{DataTar: guestTarFromDir(t, data), WaitDataTar: successfulWait, AppliedLock: lock}, CreateOptions{
				Directory: directory, Box: "main", Label: "current", Keep: 1,
				Recipient: identity.Recipient(), Identity: identity, ValidateClosure: allowClosure,
				Now: func() time.Time { return time.Date(2026, 6, 22, 12, 0, 2, 0, time.UTC) },
			}, operations)
			if err != nil {
				t.Fatalf("missing old %s should be idempotent: %v", missing, err)
			}
			wantCalls := []string{"remove-envelope", "sync-envelope", "remove-archive", "sync-archive"}
			if strings.Join(calls, ",") != strings.Join(wantCalls, ",") {
				t.Fatalf("filesystem calls = %v, want %v", calls, wantCalls)
			}
			if pathExists(old.Archive) || pathExists(old.Envelope) {
				t.Fatalf("old pair remains after idempotent prune: %+v", old)
			}
			bundle, err := Verify(context.Background(), current.Archive, current.Envelope, identity, allowClosure)
			if err != nil {
				t.Fatalf("current backup became invalid: %v", err)
			}
			bundle.Cleanup()
		})
	}
}

func TestRetentionDoesNotPruneWhenCurrentPairCannotBeVerified(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	data := filepath.Join(root, "data")
	writeTestFile(t, filepath.Join(data, "home", "agent", "file"), "data")
	lock := filepath.Join(root, "lock")
	writeTestFile(t, lock, "lock")
	identity, _ := age.GenerateX25519Identity()
	directory := filepath.Join(root, "backups")
	createBackup := func(second int, label string) Result {
		t.Helper()
		result, err := Create(context.Background(), Source{DataTar: guestTarFromDir(t, data), WaitDataTar: successfulWait, AppliedLock: lock}, CreateOptions{Directory: directory, Box: "main", Label: label, Keep: 5, Recipient: identity.Recipient(), Identity: identity, ValidateClosure: allowClosure, Now: func() time.Time { return time.Date(2026, 6, 22, 12, 0, second, 0, time.UTC) }})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	old := createBackup(1, "old")
	current := createBackup(2, "current")
	if err := os.Remove(current.Envelope); err != nil {
		t.Fatal(err)
	}
	operations := retentionFileOps{remove: os.Remove, syncDirectory: syncDirectory}
	if err := enforceRetention(context.Background(), directory, 1, current.Archive, identity, allowClosure, operations); err == nil {
		t.Fatal("expected missing-current verification failure")
	}
	bundle, err := Verify(context.Background(), old.Archive, old.Envelope, identity, allowClosure)
	if err != nil {
		t.Fatalf("older recovery backup was pruned before current verification: %v", err)
	}
	bundle.Cleanup()
}

func TestPublicationCleanupFailureReportsRemainingState(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	archive := filepath.Join(dir, "archive.tar.zst.age")
	envelope := filepath.Join(dir, "archive.envelope.json")
	if err := os.Mkdir(archive, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(archive, "child"), "not removable as a file")
	writeTestFile(t, envelope, "envelope")
	err := failPublished(dir, archive, envelope, errors.New("post-publication failure"))
	var publicationError *PublicationError
	if !errors.As(err, &publicationError) {
		t.Fatalf("expected publication error, got %T", err)
	}
	if !publicationError.ArchivePresent || publicationError.EnvelopePresent {
		t.Fatalf("unexpected reported state: %+v", publicationError)
	}
}

func TestRetentionIgnoresTamperedEnvelopeReferences(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	data := filepath.Join(root, "data")
	writeTestFile(t, filepath.Join(data, "home", "agent", "file"), "data")
	lock := filepath.Join(root, "lock")
	writeTestFile(t, lock, "lock")
	identity, _ := age.GenerateX25519Identity()
	dir := filepath.Join(root, "backups")
	create := func(second int, label string, keep int) Result {
		result, err := Create(context.Background(), Source{DataTar: guestTarFromDir(t, data), WaitDataTar: successfulWait, AppliedLock: lock}, CreateOptions{Directory: dir, Box: "main", Label: label, Keep: keep, Recipient: identity.Recipient(), Identity: identity, ValidateClosure: allowClosure, Now: func() time.Time { return time.Date(2026, 6, 22, 12, 0, second, 0, time.UTC) }})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	create(1, "one", 5)
	second := create(2, "two", 5)
	tampered := Envelope{Schema: 1, Format: EnvelopeFormat, Archive: filepath.Base(second.Archive), ArchiveSHA256: second.ArchiveSHA256, RecipientFingerprint: Fingerprint(identity.Recipient())}
	tamperedData, _ := json.Marshal(tampered)
	if err := os.WriteFile(filepath.Join(dir, "00000000-000000-tampered.envelope.json"), tamperedData, 0o600); err != nil {
		t.Fatal(err)
	}
	create(3, "three", 3)
	if _, err := os.Stat(second.Archive); err != nil {
		t.Fatalf("tampered envelope caused valid archive deletion: %v", err)
	}
	validArchives, _ := filepath.Glob(filepath.Join(dir, "2026*.tar.zst.age"))
	if len(validArchives) != 3 {
		t.Fatalf("got %d valid archives, want 3", len(validArchives))
	}
}

func TestVerifyRequiresExactArtifactClosure(t *testing.T) {
	t.Parallel()
	identity, _ := age.GenerateX25519Identity()
	lockHash := sha256.Sum256([]byte("lock"))
	digestA := stringsOf('a', 64)
	digestB := stringsOf('b', 64)
	tests := []struct {
		name      string
		artifacts []Artifact
		entry     Entry
	}{
		{name: "unlisted", entry: Entry{Path: "artifacts/sha256/" + digestA, Type: "file", Mode: 0o600, SHA256: digestA}},
		{name: "path content mismatch", artifacts: []Artifact{{Name: "tool", SHA256: digestA}}, entry: Entry{Path: "artifacts/sha256/" + digestA, Type: "file", Mode: 0o600, SHA256: digestB}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			archivePath := filepath.Join(dir, "bad.tar.zst.age")
			manifest := Manifest{Schema: 1, Format: Format, CreatedAt: time.Now(), Box: "main", AppliedLockSHA256: hex.EncodeToString(lockHash[:]), Artifacts: test.artifacts, Entries: []Entry{{Path: "applied.lock", Type: "file", Mode: 0o600, SHA256: hex.EncodeToString(lockHash[:]), Size: 4}, {Path: "data", Type: "dir", Mode: 0o700}, test.entry}}
			writeRawArchive(t, archivePath, identity.Recipient(), manifest, "applied.lock", []byte("lock"))
			hash, _ := fileSHA256(archivePath)
			envelopePath := filepath.Join(dir, "bad.envelope.json")
			envelopeData, _ := json.Marshal(Envelope{Schema: 1, Format: EnvelopeFormat, Archive: filepath.Base(archivePath), ArchiveSHA256: hash, RecipientFingerprint: Fingerprint(identity.Recipient())})
			if err := os.WriteFile(envelopePath, envelopeData, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Verify(context.Background(), archivePath, envelopePath, identity, allowClosure); err == nil {
				t.Fatal("expected artifact closure rejection")
			}
		})
	}
}

func TestClosureValidatorIsRequiredAndAuthoritative(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	data := filepath.Join(root, "data")
	writeTestFile(t, filepath.Join(data, "file"), "data")
	lock := filepath.Join(root, "lock")
	writeTestFile(t, lock, "lock")
	identity, _ := age.GenerateX25519Identity()
	options := CreateOptions{Directory: filepath.Join(root, "backups"), Box: "main", Label: "test", Keep: 1, Recipient: identity.Recipient(), Identity: identity}
	if _, err := Create(context.Background(), Source{DataTar: guestTarFromDir(t, data), WaitDataTar: successfulWait, AppliedLock: lock}, options); err == nil {
		t.Fatal("expected missing closure validator rejection")
	}
	rejected := errors.New("incomplete lock closure")
	options.ValidateClosure = func(lock []byte, artifacts []Artifact) error {
		if string(lock) != "lock" || len(artifacts) != 0 {
			t.Fatalf("validator received lock %q and artifacts %v", lock, artifacts)
		}
		return rejected
	}
	if _, err := Create(context.Background(), Source{DataTar: guestTarFromDir(t, data), WaitDataTar: successfulWait, AppliedLock: lock}, options); !errors.Is(err, rejected) {
		t.Fatalf("expected validator failure, got %v", err)
	}
}

func TestVerifyRejectsMissingDataDirectory(t *testing.T) {
	t.Parallel()
	identity, _ := age.GenerateX25519Identity()
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "bad.tar.zst.age")
	lockHash := sha256.Sum256([]byte("lock"))
	manifest := Manifest{Schema: 1, Format: Format, CreatedAt: time.Now(), Box: "main", AppliedLockSHA256: hex.EncodeToString(lockHash[:]), Entries: []Entry{{Path: "applied.lock", Type: "file", Mode: 0o600, SHA256: hex.EncodeToString(lockHash[:]), Size: 4}}}
	writeRawArchive(t, archivePath, identity.Recipient(), manifest, "applied.lock", []byte("lock"))
	hash, _ := fileSHA256(archivePath)
	envelopePath := filepath.Join(dir, "bad.envelope.json")
	envelopeData, _ := json.Marshal(Envelope{Schema: 1, Format: EnvelopeFormat, Archive: filepath.Base(archivePath), ArchiveSHA256: hash, RecipientFingerprint: Fingerprint(identity.Recipient())})
	if err := os.WriteFile(envelopePath, envelopeData, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(context.Background(), archivePath, envelopePath, identity, allowClosure); err == nil {
		t.Fatal("expected missing durable data rejection")
	}
}

func TestEncryptedArchiveSnapshotCannotBeMutatedThroughSource(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "archive")
	writeTestFile(t, path, "covered")
	file, hash, err := snapshotEncryptedArchive(context.Background(), path, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	source, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.WriteString("swapped"); err != nil {
		source.Close()
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256([]byte("covered"))
	if hash != hex.EncodeToString(want[:]) {
		t.Fatal("private archive snapshot hash changed")
	}
	contents, err := io.ReadAll(file)
	if err != nil || string(contents) != "covered" {
		t.Fatalf("descriptor contents %q err %v", contents, err)
	}
}

func TestPublishDirectoryExclusiveDoesNotReplace(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	source := filepath.Join(root, "source")
	destination := filepath.Join(root, "destination")
	writeTestFile(t, filepath.Join(source, "value"), "new")
	writeTestFile(t, filepath.Join(destination, "value"), "old")
	if err := publishDirectoryExclusive(source, destination); err == nil {
		t.Fatal("expected exclusive publication failure")
	}
	contents, err := os.ReadFile(filepath.Join(destination, "value"))
	if err != nil || string(contents) != "old" {
		t.Fatal("exclusive publication replaced destination")
	}
}

func TestRestoreRemovesPublishedDestinationWhenParentSyncFails(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	data := filepath.Join(root, "data")
	writeTestFile(t, filepath.Join(data, "home", "agent", "file"), "secret")
	lock := filepath.Join(root, "lock")
	writeTestFile(t, lock, "lock")
	identity, _ := age.GenerateX25519Identity()
	created, err := Create(context.Background(), Source{DataTar: guestTarFromDir(t, data), WaitDataTar: successfulWait, AppliedLock: lock}, CreateOptions{Directory: filepath.Join(root, "backups"), Box: "main", Label: "restore", Keep: 1, Recipient: identity.Recipient(), Identity: identity, ValidateClosure: allowClosure})
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, "restore", "bundle")
	injected := errors.New("injected parent fsync failure")
	syncCalls := 0
	_, err = restore(context.Background(), created.Archive, created.Envelope, identity, allowClosure, destination, restoreFileOps{
		syncTree:         syncTree,
		publishDirectory: publishDirectoryExclusive,
		syncDirectory: func(path string) error {
			syncCalls++
			if syncCalls == 1 {
				return injected
			}
			return syncDirectory(path)
		},
		removeAll: os.RemoveAll,
	})
	if !errors.Is(err, injected) {
		t.Fatalf("restore error = %v, want injected sync failure", err)
	}
	if _, statErr := os.Lstat(destination); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("failed restore left published destination: %v", statErr)
	}
	staged, globErr := filepath.Glob(filepath.Join(filepath.Dir(destination), ".hermes-box-verified-backup-*"))
	if globErr != nil || len(staged) != 0 {
		t.Fatalf("failed restore left staging paths %v (err %v)", staged, globErr)
	}
}

func TestRestoreCleansVerificationStageOnSyncAndPublishFailure(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	data := filepath.Join(root, "data")
	writeTestFile(t, filepath.Join(data, "home", "agent", "file"), "secret")
	lock := filepath.Join(root, "lock")
	writeTestFile(t, lock, "lock")
	identity, _ := age.GenerateX25519Identity()
	created, err := Create(context.Background(), Source{DataTar: guestTarFromDir(t, data), WaitDataTar: successfulWait, AppliedLock: lock}, CreateOptions{Directory: filepath.Join(root, "backups"), Box: "main", Label: "restore", Keep: 1, Recipient: identity.Recipient(), Identity: identity, ValidateClosure: allowClosure})
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"sync", "publish"} {
		phase := phase
		t.Run(phase, func(t *testing.T) {
			parent := filepath.Join(root, phase)
			destination := filepath.Join(parent, "bundle")
			injected := errors.New("injected " + phase + " failure")
			operations := restoreFileOps{syncTree: syncTree, publishDirectory: publishDirectoryExclusive, syncDirectory: syncDirectory, removeAll: os.RemoveAll}
			if phase == "sync" {
				operations.syncTree = func(string) error { return injected }
			} else {
				operations.publishDirectory = func(string, string) error { return injected }
			}
			_, err := restore(context.Background(), created.Archive, created.Envelope, identity, allowClosure, destination, operations)
			if !errors.Is(err, injected) {
				t.Fatalf("restore error = %v, want injected failure", err)
			}
			staged, globErr := filepath.Glob(filepath.Join(parent, ".hermes-box-verified-backup-*"))
			if globErr != nil || len(staged) != 0 {
				t.Fatalf("restore left staging paths %v (err %v)", staged, globErr)
			}
			if _, statErr := os.Lstat(destination); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("failed restore created destination: %v", statErr)
			}
		})
	}
}

func allowClosure(_ []byte, _ []Artifact) error { return nil }

func countVerifiedBackups(t *testing.T, directory string, identity age.Identity) int {
	t.Helper()
	envelopes, err := filepath.Glob(filepath.Join(directory, "*.envelope.json"))
	if err != nil {
		t.Fatal(err)
	}
	valid := 0
	for _, envelopePath := range envelopes {
		envelope, err := readEnvelope(envelopePath)
		if err != nil {
			continue
		}
		archivePath := filepath.Join(directory, envelope.Archive)
		bundle, err := Verify(context.Background(), archivePath, envelopePath, identity, allowClosure)
		if err != nil {
			continue
		}
		bundle.Cleanup()
		valid++
	}
	return valid
}

func writeTestFile(t *testing.T, destination, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func stringsOf(value byte, count int) string {
	return string(bytes.Repeat([]byte{value}, count))
}

type tarInput struct {
	Header tar.Header
	Body   []byte
}

type blockingReadCloser struct {
	started     chan struct{}
	closed      chan struct{}
	startedOnce sync.Once
	closedOnce  sync.Once
}

type gatedReadCloser struct {
	data      []byte
	offset    int
	blockAt   int
	blocked   chan struct{}
	release   chan struct{}
	blockOnce sync.Once
}

func newGatedReadCloser(data []byte, blockAt int) *gatedReadCloser {
	return &gatedReadCloser{data: data, blockAt: blockAt, blocked: make(chan struct{}), release: make(chan struct{})}
}

func (r *gatedReadCloser) Read(buffer []byte) (int, error) {
	if r.offset >= r.blockAt {
		r.blockOnce.Do(func() {
			close(r.blocked)
			<-r.release
		})
	}
	if r.offset == len(r.data) {
		return 0, io.EOF
	}
	n := copy(buffer, r.data[r.offset:])
	r.offset += n
	return n, nil
}

func (r *gatedReadCloser) Close() error { return nil }

func newBlockingReadCloser() *blockingReadCloser {
	return &blockingReadCloser{started: make(chan struct{}), closed: make(chan struct{})}
}

func (r *blockingReadCloser) Read([]byte) (int, error) {
	r.startedOnce.Do(func() { close(r.started) })
	<-r.closed
	return 0, os.ErrClosed
}

func (r *blockingReadCloser) Close() error {
	r.closedOnce.Do(func() { close(r.closed) })
	return nil
}

func guestTarBytes(t *testing.T, entries []tarInput) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	for _, entry := range entries {
		header := entry.Header
		if header.Typeflag == tar.TypeReg || header.Typeflag == tar.TypeRegA {
			header.Size = int64(len(entry.Body))
		}
		if err := writer.WriteHeader(&header); err != nil {
			t.Fatal(err)
		}
		if len(entry.Body) > 0 {
			if _, err := writer.Write(entry.Body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func readTarEntries(t *testing.T, data []byte) map[string]tar.Header {
	t.Helper()
	entries := make(map[string]tar.Header)
	reader := tar.NewReader(bytes.NewReader(data))
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return entries
		}
		if err != nil {
			t.Fatal(err)
		}
		entries[header.Name] = *header
	}
}

func secretBody(t *testing.T, data []byte, name string) []byte {
	t.Helper()
	reader := tar.NewReader(bytes.NewReader(data))
	for {
		header, err := reader.Next()
		if err != nil {
			t.Fatalf("find %s: %v", name, err)
		}
		if header.Name == name {
			body, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			return body
		}
	}
}

func successfulWait() error { return nil }

func guestTarFromDir(t *testing.T, dataRoot string) io.ReadCloser {
	t.Helper()
	for _, directory := range []string{"home/agent", "executor"} {
		if err := os.MkdirAll(filepath.Join(dataRoot, directory), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	var buffer bytes.Buffer
	archive := tar.NewWriter(&buffer)
	err := filepath.WalkDir(dataRoot, func(filePath string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if filePath == dataRoot {
			return nil
		}
		relative, err := filepath.Rel(dataRoot, filePath)
		if err != nil {
			return err
		}
		info, err := os.Lstat(filePath)
		if err != nil {
			return err
		}
		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			link, err = os.Readlink(filePath)
			if err != nil {
				return err
			}
		}
		header, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		header.Name = "data/" + filepath.ToSlash(relative)
		header.Uid, header.Gid = 1000, 1000
		header.Uname, header.Gname = "agent", "agent"
		if err := archive.WriteHeader(header); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		file, err := os.Open(filePath)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(archive, file)
		closeErr := file.Close()
		return errors.Join(copyErr, closeErr)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	return io.NopCloser(bytes.NewReader(buffer.Bytes()))
}

func writeRawArchive(t *testing.T, destination string, recipient age.Recipient, manifest Manifest, name string, contents []byte) {
	t.Helper()
	file, err := os.Create(destination)
	if err != nil {
		t.Fatal(err)
	}
	ageWriter, _ := age.Encrypt(file, recipient)
	zstdWriter, _ := zstd.NewWriter(ageWriter)
	tarWriter := tar.NewWriter(zstdWriter)
	manifestData, _ := json.Marshal(manifest)
	if err := writeBytes(tarWriter, "manifest.json", 0o600, manifestData); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(contents))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write(contents); err != nil {
		t.Fatal(err)
	}
	tarWriter.Close()
	zstdWriter.Close()
	ageWriter.Close()
	file.Close()
}

func TestFileSHA256(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "file")
	writeTestFile(t, path, "abc")
	got, err := fileSHA256(path)
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256([]byte("abc"))
	if got != hex.EncodeToString(want[:]) {
		t.Fatalf("got %s", got)
	}
}

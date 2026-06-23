package backup

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"filippo.io/age"
	"github.com/klauspost/compress/zstd"
	"golang.org/x/sys/unix"
)

const (
	Format         = "hermes-box-recovery-v1"
	EnvelopeFormat = "hermes-box-recovery-v1-envelope"
)

var safeLabel = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

type Entry struct {
	Path       string `json:"path"`
	Type       string `json:"type"`
	Mode       int64  `json:"mode"`
	UID        int    `json:"uid"`
	GID        int    `json:"gid"`
	Uname      string `json:"uname,omitempty"`
	Gname      string `json:"gname,omitempty"`
	Absent     bool   `json:"absent,omitempty"`
	Structural bool   `json:"structural,omitempty"`
	Size       int64  `json:"size,omitempty"`
	SHA256     string `json:"sha256,omitempty"`
	LinkTarget string `json:"link_target,omitempty"`
}

type Artifact struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Path   string `json:"-"`
}

type Manifest struct {
	Schema            int        `json:"schema"`
	Format            string     `json:"format"`
	CreatedAt         time.Time  `json:"created_at"`
	Box               string     `json:"box"`
	AppliedLockSHA256 string     `json:"applied_lock_sha256"`
	DataPaths         []string   `json:"data_paths"`
	Artifacts         []Artifact `json:"artifacts"`
	ExcludedPaths     []string   `json:"excluded_paths"`
	Entries           []Entry    `json:"entries"`
}

type Envelope struct {
	Schema               int    `json:"schema"`
	Format               string `json:"format"`
	Archive              string `json:"archive"`
	ArchiveSHA256        string `json:"archive_sha256"`
	RecipientFingerprint string `json:"recipient_fingerprint"`
}

type Source struct {
	DataTar io.ReadCloser
	// WaitDataTar reports the guest producer's final status after tar EOF.
	WaitDataTar func() error
	// DataPaths are normalized paths relative to /data for scoped transaction
	// snapshots. Empty means the full home and executor recovery set.
	DataPaths   []string
	AppliedLock string
	Artifacts   []Artifact
}

// ClosureValidator binds the archived install artifacts to the applied lock.
// A backup is never considered recovery-valid without this lock-aware check.
type ClosureValidator func(appliedLock []byte, artifacts []Artifact) error

type CreateOptions struct {
	Directory            string
	Box                  string
	Label                string
	Keep                 int
	Recipient            age.Recipient
	Identity             age.Identity
	RecipientFingerprint string
	ValidateClosure      ClosureValidator
	Now                  func() time.Time
}

type Result struct {
	Archive       string
	Envelope      string
	ArchiveSHA256 string
}

// RetentionWarning reports that a backup was published and verified, but one
// or more older backups could not be pruned. The Result returned alongside
// this warning remains recovery-valid and must not be discarded.
type RetentionWarning struct {
	Archive  string
	Envelope string
	Err      error
}

func (e *RetentionWarning) Error() string {
	return fmt.Sprintf("backup published and verified, but retention pruning failed: %v", e.Err)
}

func (e *RetentionWarning) Unwrap() error { return e.Err }

// PublicationError reports the exact on-disk state after a failure that
// happened after backup publication began. Create removes the new pair before
// returning whenever possible; these fields make a cleanup failure explicit.
type PublicationError struct {
	Archive         string
	Envelope        string
	ArchivePresent  bool
	EnvelopePresent bool
	Err             error
}

func (e *PublicationError) Error() string {
	return fmt.Sprintf("backup publication failed (archive_present=%t envelope_present=%t): %v", e.ArchivePresent, e.EnvelopePresent, e.Err)
}

func (e *PublicationError) Unwrap() error { return e.Err }

type VerifiedBundle struct {
	Root     string
	Manifest Manifest
	Envelope Envelope
}

func (b *VerifiedBundle) Cleanup() error {
	if b.Root == "" {
		return nil
	}
	err := os.RemoveAll(b.Root)
	b.Root = ""
	return err
}

func Create(ctx context.Context, source Source, options CreateOptions) (result Result, resultErr error) {
	return create(ctx, source, options, retentionFileOps{remove: os.Remove, syncDirectory: syncDirectory})
}

func create(ctx context.Context, source Source, options CreateOptions, retentionOps retentionFileOps) (result Result, resultErr error) {
	var finalizeOnce sync.Once
	var streamErr error
	stopClose := func() bool { return true }
	if source.DataTar != nil {
		stopClose = context.AfterFunc(ctx, func() { _ = source.DataTar.Close() })
	}
	finalizeStream := func() error {
		finalizeOnce.Do(func() {
			stopClose()
			streamErr = source.DataTar.Close()
			if source.WaitDataTar != nil {
				streamErr = errors.Join(streamErr, source.WaitDataTar())
			}
		})
		return streamErr
	}
	if source.DataTar != nil {
		defer func() { resultErr = errors.Join(resultErr, finalizeStream()) }()
	}
	if options.Directory == "" || options.Box == "" || options.Recipient == nil || options.Identity == nil || options.ValidateClosure == nil {
		return Result{}, errors.New("backup directory, box, recipient, verification identity, and lock closure validator are required")
	}
	if !safeLabel.MatchString(options.Label) {
		return Result{}, errors.New("backup label must contain only letters, numbers, dot, underscore, or dash")
	}
	if options.Keep < 1 {
		return Result{}, errors.New("backup retention must be at least one")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.RecipientFingerprint == "" {
		options.RecipientFingerprint = Fingerprint(options.Recipient)
	}
	if source.DataTar == nil || source.WaitDataTar == nil {
		return Result{}, errors.New("closeable guest data tar stream and producer wait function are required")
	}
	lockData, err := readRegularBounded(source.AppliedLock, 16<<20)
	if err != nil {
		return Result{}, fmt.Errorf("read applied lock: %w", err)
	}
	if err := options.ValidateClosure(lockData, source.Artifacts); err != nil {
		return Result{}, fmt.Errorf("validate applied-lock artifact closure: %w", err)
	}
	if err := os.MkdirAll(options.Directory, 0o700); err != nil {
		return Result{}, err
	}
	dataPaths, err := normalizeDataPaths(source.DataPaths)
	if err != nil {
		return Result{}, err
	}
	spool, dataEntries, buildErr := spoolGuestTar(ctx, source.DataTar, options.Directory, dataPaths)
	producerErr := finalizeStream()
	if buildErr != nil || producerErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Result{}, ctxErr
		}
		return Result{}, errors.Join(buildErr, producerErr)
	}
	defer spool.Close()
	manifest, files, err := buildManifest(ctx, source, options.Box, options.Now().UTC(), dataPaths, dataEntries)
	if err != nil {
		return Result{}, err
	}
	base, err := availableBackupBase(options.Directory, manifest.CreatedAt, options.Label)
	if err != nil {
		return Result{}, err
	}
	archivePath := filepath.Join(options.Directory, base+".tar.zst.age")
	envelopePath := filepath.Join(options.Directory, base+".envelope.json")

	tmp, err := os.CreateTemp(options.Directory, ".backup-*.partial")
	if err != nil {
		return Result{}, err
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpPath)
	}
	defer cleanup()

	ageWriter, err := age.Encrypt(tmp, options.Recipient)
	if err != nil {
		return Result{}, err
	}
	zstdWriter, err := zstd.NewWriter(ageWriter)
	if err != nil {
		ageWriter.Close()
		return Result{}, err
	}
	tarWriter := tar.NewWriter(zstdWriter)
	manifestData, err := json.Marshal(manifest)
	if err == nil {
		err = writeBytes(tarWriter, "manifest.json", 0o600, manifestData)
	}
	if err == nil {
		err = writeFiles(ctx, tarWriter, files)
	}
	if err == nil {
		err = writeGuestSpool(ctx, tarWriter, spool, dataEntries)
	}
	if closeErr := tarWriter.Close(); err == nil {
		err = closeErr
	}
	if closeErr := zstdWriter.Close(); err == nil {
		err = closeErr
	}
	if closeErr := ageWriter.Close(); err == nil {
		err = closeErr
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return Result{}, err
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return Result{}, err
	}
	if err := publishExclusive(tmpPath, archivePath); err != nil {
		return Result{}, err
	}
	archiveHash, err := fileSHA256Context(ctx, archivePath)
	if err != nil {
		return Result{}, failPublished(options.Directory, archivePath, envelopePath, err)
	}
	envelope := Envelope{Schema: 1, Format: EnvelopeFormat, Archive: filepath.Base(archivePath), ArchiveSHA256: archiveHash, RecipientFingerprint: options.RecipientFingerprint}
	if err := writeJSONAtomic(envelopePath, envelope); err != nil {
		return Result{}, failPublished(options.Directory, archivePath, envelopePath, err)
	}
	if err := syncDirectory(options.Directory); err != nil {
		return Result{}, failPublished(options.Directory, archivePath, envelopePath, err)
	}
	bundle, err := verifyWithoutData(ctx, archivePath, envelopePath, options.Identity, options.ValidateClosure)
	if err != nil {
		return Result{}, failPublished(options.Directory, archivePath, envelopePath, fmt.Errorf("verify new backup: %w", err))
	}
	bundle.Cleanup()
	result = Result{Archive: archivePath, Envelope: envelopePath, ArchiveSHA256: archiveHash}
	if err := enforceRetention(ctx, options.Directory, options.Keep, archivePath, options.Identity, options.ValidateClosure, retentionOps); err != nil {
		return result, &RetentionWarning{Archive: archivePath, Envelope: envelopePath, Err: err}
	}
	return result, nil
}

func availableBackupBase(directory string, createdAt time.Time, label string) (string, error) {
	prefix := createdAt.Format("20060102-150405") + "-" + label
	for sequence := 0; sequence < 10_000; sequence++ {
		base := prefix
		if sequence > 0 {
			base = fmt.Sprintf("%s-%04d", prefix, sequence)
		}
		available := true
		for _, suffix := range []string{".tar.zst.age", ".envelope.json"} {
			_, err := os.Lstat(filepath.Join(directory, base+suffix))
			switch {
			case err == nil:
				available = false
			case errors.Is(err, os.ErrNotExist):
			default:
				return "", err
			}
		}
		if available {
			return base, nil
		}
	}
	return "", errors.New("backup timestamp and label sequence is exhausted")
}

func failPublished(directory, archivePath, envelopePath string, cause error) error {
	cleanupErr := error(nil)
	for _, output := range []string{archivePath, envelopePath} {
		if err := os.Remove(output); err != nil && !errors.Is(err, os.ErrNotExist) {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("remove %s: %w", output, err))
		}
	}
	if err := syncDirectory(directory); err != nil {
		cleanupErr = errors.Join(cleanupErr, fmt.Errorf("sync backup directory after cleanup: %w", err))
	}
	return &PublicationError{
		Archive:         archivePath,
		Envelope:        envelopePath,
		ArchivePresent:  pathExists(archivePath),
		EnvelopePresent: pathExists(envelopePath),
		Err:             errors.Join(cause, cleanupErr),
	}
}

func pathExists(value string) bool {
	_, err := os.Lstat(value)
	return err == nil
}

type sourceFile struct {
	Entry Entry
	Path  string
}

func buildManifest(ctx context.Context, source Source, box string, createdAt time.Time, dataPaths []string, dataEntries []sourceFile) (Manifest, []sourceFile, error) {
	if source.DataTar == nil || source.WaitDataTar == nil || source.AppliedLock == "" {
		return Manifest{}, nil, errors.New("guest data tar stream and applied lock are required")
	}
	lockHash, err := regularFileSHA256Context(ctx, source.AppliedLock)
	if err != nil {
		return Manifest{}, nil, fmt.Errorf("hash applied lock: %w", err)
	}
	files := []sourceFile{{Entry: Entry{Path: "applied.lock", Type: "file", Mode: 0o600, UID: 0, GID: 0}, Path: source.AppliedLock}}
	files = append(files, dataEntries...)
	artifacts := append([]Artifact(nil), source.Artifacts...)
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].Name < artifacts[j].Name })
	seenArtifactNames := make(map[string]bool)
	seenArtifactDigests := make(map[string]bool)
	for i := range artifacts {
		artifact := &artifacts[i]
		if !validDigest(artifact.SHA256) || artifact.Name == "" || seenArtifactNames[artifact.Name] {
			return Manifest{}, nil, fmt.Errorf("invalid or duplicate artifact %q", artifact.Name)
		}
		actual, err := regularFileSHA256Context(ctx, artifact.Path)
		if err != nil || actual != artifact.SHA256 {
			return Manifest{}, nil, fmt.Errorf("artifact %q failed SHA-256 verification", artifact.Name)
		}
		seenArtifactNames[artifact.Name] = true
		if !seenArtifactDigests[artifact.SHA256] {
			seenArtifactDigests[artifact.SHA256] = true
			files = append(files, sourceFile{Entry: Entry{Path: "artifacts/sha256/" + artifact.SHA256, Type: "file", Mode: 0o600, UID: 0, GID: 0}, Path: artifact.Path})
		}
	}
	for i := range files {
		if files[i].Entry.Type == "file" && files[i].Path != guestStreamSource {
			info, err := os.Lstat(files[i].Path)
			if err != nil || !info.Mode().IsRegular() {
				return Manifest{}, nil, fmt.Errorf("backup source %q is not a regular file", files[i].Path)
			}
			files[i].Entry.Size = info.Size()
			files[i].Entry.SHA256, err = regularFileSHA256Context(ctx, files[i].Path)
			if err != nil {
				return Manifest{}, nil, err
			}
		}
	}
	entries := make([]Entry, len(files))
	for i := range files {
		entries[i] = files[i].Entry
	}
	return Manifest{Schema: 1, Format: Format, CreatedAt: createdAt, Box: box, AppliedLockSHA256: lockHash, DataPaths: dataPaths, Artifacts: artifacts, ExcludedPaths: []string{"cache"}, Entries: entries}, files, nil
}

func writeFiles(ctx context.Context, writer *tar.Writer, files []sourceFile) error {
	for _, item := range files {
		if item.Path == guestStreamSource || item.Path == "" {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := writeEntryHeader(writer, item.Entry); err != nil {
			return err
		}
		if item.Entry.Type != "file" {
			continue
		}
		file, err := openRegularNoFollow(item.Path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(writer, &contextReader{ctx: ctx, reader: file})
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func writeEntryHeader(writer *tar.Writer, entry Entry) error {
	header := &tar.Header{Name: entry.Path, Mode: entry.Mode, Uid: entry.UID, Gid: entry.GID, Uname: entry.Uname, Gname: entry.Gname, Size: entry.Size}
	if entry.Absent {
		header.PAXRecords = map[string]string{"HERMESBOX.absent": "1"}
	}
	switch entry.Type {
	case "dir":
		header.Typeflag = tar.TypeDir
	case "symlink":
		header.Typeflag = tar.TypeSymlink
		header.Linkname = entry.LinkTarget
	case "file":
		header.Typeflag = tar.TypeReg
	default:
		return fmt.Errorf("unsupported archive entry type %q", entry.Type)
	}
	return writer.WriteHeader(header)
}

func writeBytes(writer *tar.Writer, name string, mode int64, data []byte) error {
	if err := writer.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	_, err := writer.Write(data)
	return err
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}

func fileSHA256(filePath string) (string, error) {
	return fileSHA256Context(context.Background(), filePath)
}

func fileSHA256Context(ctx context.Context, filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, &contextReader{ctx: ctx, reader: file}); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func regularFileSHA256Context(ctx context.Context, filePath string) (string, error) {
	file, err := openRegularNoFollow(filePath)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, &contextReader{ctx: ctx, reader: file}); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func readRegularBounded(path string, limit int64) ([]byte, error) {
	file, err := openRegularNoFollow(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("regular file exceeds size limit")
	}
	return data, nil
}

func openRegularNoFollow(filePath string) (*os.File, error) {
	fd, err := unix.Open(filePath, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), filePath)
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		return nil, fmt.Errorf("%s is not a regular file", filePath)
	}
	return file, nil
}

func Fingerprint(recipient age.Recipient) string {
	hash := sha256.Sum256([]byte(fmt.Sprint(recipient)))
	return hex.EncodeToString(hash[:])
}

func validDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && strings.ToLower(value) == value
}

func safeArchivePath(value string) bool {
	return value != "" && value != "." && !strings.HasPrefix(value, "/") && path.Clean(value) == value && value != ".." && !strings.HasPrefix(value, "../")
}

func safeDataSymlink(name, target string) bool {
	if !strings.HasPrefix(name, "data/") || target == "" || strings.HasPrefix(target, "/") {
		return false
	}
	resolved := path.Clean(path.Join(path.Dir(name), filepath.ToSlash(target)))
	return resolved == "data" || strings.HasPrefix(resolved, "data/")
}

func writeJSONAtomic(destination string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(destination), ".envelope-*.partial")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return publishExclusive(tmpPath, destination)
}

func publishExclusive(source, destination string) error {
	file, err := os.Open(source)
	if err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Link(source, destination); err != nil {
		return err
	}
	_ = os.Remove(source)
	return nil
}

type retentionFileOps struct {
	remove        func(string) error
	syncDirectory func(string) error
}

func enforceRetention(ctx context.Context, directory string, keep int, current string, identity age.Identity, validateClosure ClosureValidator, operations retentionFileOps) error {
	matches, err := filepath.Glob(filepath.Join(directory, "*.envelope.json"))
	if err != nil {
		return err
	}
	type pair struct {
		archive  string
		envelope string
		created  time.Time
	}
	verified := make([]pair, 0, len(matches))
	currentVerified := false
	for _, envelopePath := range matches {
		stem := strings.TrimSuffix(filepath.Base(envelopePath), ".envelope.json")
		archivePath := filepath.Join(directory, stem+".tar.zst.age")
		envelope, err := readEnvelope(envelopePath)
		if err != nil || envelope.Archive != filepath.Base(archivePath) {
			continue
		}
		bundle, err := verifyWithoutData(ctx, archivePath, envelopePath, identity, validateClosure)
		if err != nil {
			if archivePath == current {
				return fmt.Errorf("verify current backup during retention: %w", err)
			}
			continue
		}
		verified = append(verified, pair{archive: archivePath, envelope: envelopePath, created: bundle.Manifest.CreatedAt})
		currentVerified = currentVerified || archivePath == current
		bundle.Cleanup()
	}
	if !currentVerified {
		return errors.New("newly published backup was not present and verified during retention")
	}
	sort.Slice(verified, func(i, j int) bool {
		if verified[i].created.Equal(verified[j].created) {
			return verified[i].archive < verified[j].archive
		}
		return verified[i].created.Before(verified[j].created)
	})
	for len(verified) > keep {
		remove := 0
		if verified[remove].archive == current {
			remove = 1
			if remove >= len(verified) {
				break
			}
		}
		candidate := verified[remove]
		if err := pruneBackupPair(directory, candidate.archive, candidate.envelope, operations); err != nil {
			return fmt.Errorf("prune backup %s: %w", filepath.Base(candidate.archive), err)
		}
		verified = append(verified[:remove], verified[remove+1:]...)
	}
	return nil
}

func pruneBackupPair(directory, archive, envelope string, operations retentionFileOps) error {
	if err := operations.remove(envelope); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove envelope: %w", err)
	}
	if err := operations.syncDirectory(directory); err != nil {
		return fmt.Errorf("sync after envelope removal: %w", err)
	}
	if err := operations.remove(archive); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove archive: %w", err)
	}
	if err := operations.syncDirectory(directory); err != nil {
		return fmt.Errorf("sync after archive removal: %w", err)
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

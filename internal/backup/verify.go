package backup

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"

	"filippo.io/age"
	"github.com/klauspost/compress/zstd"
	"golang.org/x/sys/unix"
)

func Verify(ctx context.Context, archivePath, envelopePath string, identity age.Identity, validateClosure ClosureValidator) (*VerifiedBundle, error) {
	return verifyAt(ctx, archivePath, envelopePath, identity, validateClosure, "", true)
}

func verifyWithoutData(ctx context.Context, archivePath, envelopePath string, identity age.Identity, validateClosure ClosureValidator) (*VerifiedBundle, error) {
	return verifyAt(ctx, archivePath, envelopePath, identity, validateClosure, "", false)
}

func verifyAt(ctx context.Context, archivePath, envelopePath string, identity age.Identity, validateClosure ClosureValidator, stagingParent string, extractData bool) (*VerifiedBundle, error) {
	if identity == nil || validateClosure == nil {
		return nil, errors.New("backup identity and lock closure validator are required")
	}
	envelope, err := readEnvelope(envelopePath)
	if err != nil {
		return nil, err
	}
	if filepath.Base(archivePath) != envelope.Archive {
		return nil, errors.New("archive name does not match envelope")
	}
	archive, actualHash, err := snapshotEncryptedArchive(ctx, archivePath, stagingParent)
	if err != nil {
		return nil, err
	}
	defer archive.Close()
	if actualHash != envelope.ArchiveSHA256 {
		return nil, fmt.Errorf("encrypted archive SHA-256 mismatch: expected %s, got %s", envelope.ArchiveSHA256, actualHash)
	}
	if x25519, ok := identity.(*age.X25519Identity); ok && Fingerprint(x25519.Recipient()) != envelope.RecipientFingerprint {
		return nil, errors.New("backup identity does not match envelope recipient fingerprint")
	}
	decrypted, err := age.Decrypt(&contextReader{ctx: ctx, reader: archive}, identity)
	if err != nil {
		return nil, fmt.Errorf("decrypt backup: %w", err)
	}
	decompressed, err := zstd.NewReader(decrypted)
	if err != nil {
		return nil, err
	}
	defer decompressed.Close()
	reader := tar.NewReader(decompressed)
	header, err := reader.Next()
	if err != nil || header.Name != "manifest.json" || header.Typeflag != tar.TypeReg || header.Size > 16<<20 {
		return nil, errors.New("backup must begin with a bounded regular manifest.json")
	}
	manifestData, err := io.ReadAll(io.LimitReader(reader, header.Size+1))
	if err != nil || int64(len(manifestData)) != header.Size {
		return nil, errors.New("read backup manifest")
	}
	var manifest Manifest
	decoder := json.NewDecoder(strings.NewReader(string(manifestData)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return nil, fmt.Errorf("parse backup manifest: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, fmt.Errorf("parse backup manifest: %w", err)
	}
	if manifest.Schema != 1 || manifest.Format != Format || manifest.Box == "" || !validDigest(manifest.AppliedLockSHA256) {
		return nil, errors.New("unsupported or invalid backup manifest")
	}
	if len(manifest.DataPaths) == 0 {
		return nil, errors.New("backup manifest has no durable data scopes")
	}
	dataPaths, err := normalizeDataPaths(manifest.DataPaths)
	if err != nil || len(dataPaths) != len(manifest.DataPaths) {
		return nil, errors.New("backup manifest has invalid durable data scopes")
	}
	for index := range dataPaths {
		if dataPaths[index] != manifest.DataPaths[index] {
			return nil, errors.New("backup manifest durable data scopes are not canonical")
		}
	}
	if len(manifest.Entries) > 200_000 {
		return nil, errors.New("backup manifest has too many entries")
	}
	expected := make(map[string]Entry, len(manifest.Entries))
	var totalSize int64
	var extractedSize int64
	for _, entry := range manifest.Entries {
		if !validManifestEntry(entry) {
			return nil, fmt.Errorf("invalid manifest entry %q", entry.Path)
		}
		if _, exists := expected[entry.Path]; exists {
			return nil, fmt.Errorf("duplicate manifest entry %q", entry.Path)
		}
		expected[entry.Path] = entry
		if entry.Size > (1<<40)-totalSize {
			return nil, errors.New("backup manifest exceeds the 1 TiB safety limit")
		}
		totalSize += entry.Size
		if extractData || !strings.HasPrefix(entry.Path, "data/") {
			extractedSize += entry.Size
		}
	}
	if entry, ok := expected["applied.lock"]; !ok || entry.Type != "file" {
		return nil, errors.New("backup manifest is missing applied.lock")
	}
	if entry, ok := expected["data"]; !ok || entry.Type != "dir" {
		return nil, errors.New("backup manifest is missing its durable data directory")
	}
	if !fullDataPaths(dataPaths) && !expected["data"].Structural {
		return nil, errors.New("scoped backup data root must be structural")
	}
	for _, scope := range dataPaths {
		required := "data/" + scope
		entry, ok := expected[required]
		if !ok || entry.Type != "dir" || entry.Structural || (fullDataPaths(dataPaths) && entry.Absent) {
			return nil, fmt.Errorf("backup manifest is missing required durable scope %s", required)
		}
	}
	for entryPath := range expected {
		if entryPath == "data" || !strings.HasPrefix(entryPath, "data/") {
			continue
		}
		for ancestor := filepath.ToSlash(filepath.Dir(entryPath)); ancestor != "."; ancestor = filepath.ToSlash(filepath.Dir(ancestor)) {
			if expected[ancestor].Type == "symlink" {
				return nil, fmt.Errorf("backup manifest entry %s is beneath symlink ancestor %s", entryPath, ancestor)
			}
		}
		relative := strings.TrimPrefix(entryPath, "data/")
		if !withinDataPaths(relative, dataPaths) && !(expected[entryPath].Type == "dir" && dataPathAncestor(relative, dataPaths)) {
			return nil, fmt.Errorf("backup manifest data entry %s is outside declared scopes", entryPath)
		}
		if dataPathAncestor(relative, dataPaths) && !expected[entryPath].Structural {
			return nil, fmt.Errorf("backup manifest ancestor %s must be structural", entryPath)
		}
		if expected[entryPath].Type == "symlink" && !fullDataPaths(dataPaths) && !symlinkWithinDataPaths(entryPath, expected[entryPath].LinkTarget, dataPaths) {
			return nil, fmt.Errorf("backup manifest symlink %s escapes declared scopes", entryPath)
		}
	}
	for _, entry := range manifest.Entries {
		if !entry.Absent {
			continue
		}
		prefix := entry.Path + "/"
		for other := range expected {
			if strings.HasPrefix(other, prefix) {
				return nil, fmt.Errorf("absent backup path %s contains entry %s", entry.Path, other)
			}
		}
	}
	artifactNames := make(map[string]bool)
	artifactDigests := make(map[string]bool)
	for _, artifact := range manifest.Artifacts {
		if artifact.Name == "" || !validDigest(artifact.SHA256) || artifactNames[artifact.Name] {
			return nil, fmt.Errorf("invalid or duplicate manifest artifact %q", artifact.Name)
		}
		artifactNames[artifact.Name] = true
		artifactDigests[artifact.SHA256] = true
	}
	for digest := range artifactDigests {
		if _, ok := expected["artifacts/sha256/"+digest]; !ok {
			return nil, fmt.Errorf("backup manifest is missing artifact %s", digest)
		}
	}
	for _, entry := range manifest.Entries {
		if !strings.HasPrefix(entry.Path, "artifacts/sha256/") {
			continue
		}
		digest := strings.TrimPrefix(entry.Path, "artifacts/sha256/")
		if !artifactDigests[digest] {
			return nil, fmt.Errorf("backup manifest contains unlisted artifact %s", digest)
		}
		if entry.Type != "file" || entry.SHA256 != digest {
			return nil, fmt.Errorf("artifact path %s does not match its content digest", entry.Path)
		}
	}
	staging, err := os.MkdirTemp(stagingParent, ".hermes-box-verified-backup-*")
	if err != nil {
		return nil, err
	}
	bundle := &VerifiedBundle{Root: staging, Manifest: manifest, Envelope: envelope}
	fail := func(err error) (*VerifiedBundle, error) {
		bundle.Cleanup()
		return nil, err
	}
	if err := requireFreeSpace(staging, extractedSize); err != nil {
		return fail(err)
	}
	seen := make(map[string]bool, len(expected))
	for {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fail(err)
		}
		if !safeArchivePath(header.Name) || header.Name == "manifest.json" {
			return fail(fmt.Errorf("unsafe or duplicate archive path %q", header.Name))
		}
		entry, ok := expected[header.Name]
		if !ok || seen[header.Name] || !headerMatches(entry, header) {
			return fail(fmt.Errorf("archive entry %q does not match manifest", header.Name))
		}
		seen[header.Name] = true
		if entry.Type == "symlink" || entry.Type == "dir" {
			continue
		}
		if strings.HasPrefix(entry.Path, "data/") && !extractData {
			hashWriter := newSHA256Writer(io.Discard)
			written, copyErr := io.Copy(hashWriter, &contextReader{ctx: ctx, reader: io.LimitReader(reader, entry.Size+1)})
			if copyErr != nil || written != entry.Size || hashWriter.Sum() != entry.SHA256 {
				return fail(fmt.Errorf("archive entry %q failed size or SHA-256 verification", entry.Path))
			}
			continue
		}
		destination := ""
		if strings.HasPrefix(entry.Path, "data/") {
			destination = dataPayloadPath(staging, entry)
		} else {
			destination, err = safeDestination(staging, entry.Path)
			if err != nil {
				return fail(err)
			}
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
			return fail(err)
		}
		file, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, os.FileMode(entry.Mode))
		if err != nil {
			return fail(err)
		}
		hashWriter := newSHA256Writer(file)
		written, copyErr := io.Copy(hashWriter, &contextReader{ctx: ctx, reader: io.LimitReader(reader, entry.Size+1)})
		closeErr := file.Close()
		if copyErr != nil || closeErr != nil || written != entry.Size || hashWriter.Sum() != entry.SHA256 {
			return fail(fmt.Errorf("archive entry %q failed size or SHA-256 verification", entry.Path))
		}
		if !strings.HasPrefix(entry.Path, "data/") {
			if err := os.Chmod(destination, os.FileMode(entry.Mode)); err != nil {
				return fail(err)
			}
		}
	}
	if len(seen) != len(expected) {
		return fail(errors.New("archive is missing manifest entries"))
	}
	lockHash, err := fileSHA256Context(ctx, filepath.Join(staging, "applied.lock"))
	if err != nil || lockHash != manifest.AppliedLockSHA256 {
		return fail(errors.New("applied lock does not match manifest"))
	}
	lockData, err := readRegularBounded(filepath.Join(staging, "applied.lock"), 16<<20)
	if err != nil {
		return fail(err)
	}
	closure := append([]Artifact(nil), manifest.Artifacts...)
	for i := range closure {
		closure[i].Path = filepath.Join(staging, "artifacts", "sha256", closure[i].SHA256)
	}
	if err := validateClosure(lockData, closure); err != nil {
		return fail(fmt.Errorf("validate applied-lock artifact closure: %w", err))
	}
	for _, artifact := range manifest.Artifacts {
		artifactHash, err := fileSHA256Context(ctx, filepath.Join(staging, "artifacts", "sha256", artifact.SHA256))
		if err != nil || artifactHash != artifact.SHA256 {
			return fail(fmt.Errorf("artifact %q is missing or corrupt", artifact.Name))
		}
	}
	return bundle, nil
}

func requireFreeSpace(path string, required int64) error {
	var stats unix.Statfs_t
	if err := unix.Statfs(path, &stats); err != nil {
		return err
	}
	available := uint64(stats.Bavail) * uint64(stats.Bsize)
	if uint64(required) > available {
		return fmt.Errorf("backup requires %d bytes but staging filesystem has %d available", required, available)
	}
	return nil
}

func readEnvelope(envelopePath string) (Envelope, error) {
	data, err := readBoundedFile(envelopePath, 1<<20)
	if err != nil {
		return Envelope{}, err
	}
	var envelope Envelope
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return Envelope{}, err
	}
	if err := requireJSONEOF(decoder); err != nil {
		return Envelope{}, err
	}
	if envelope.Schema != 1 || envelope.Format != EnvelopeFormat || !safeArchivePath(envelope.Archive) || !validDigest(envelope.ArchiveSHA256) || !validDigest(envelope.RecipientFingerprint) {
		return Envelope{}, errors.New("unsupported or invalid backup envelope")
	}
	return envelope, nil
}

func readBoundedFile(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("file exceeds size limit")
	}
	return data, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}

func validManifestEntry(entry Entry) bool {
	if !safeArchivePath(entry.Path) || !allowedBundlePath(entry.Path) || entry.Path == "manifest.json" || entry.Mode < 0 || entry.Mode > 0o7777 || entry.UID < 0 || entry.UID > 65535 || entry.GID < 0 || entry.GID > 65535 || len(entry.Uname) > 255 || len(entry.Gname) > 255 || strings.ContainsRune(entry.Uname, '\x00') || strings.ContainsRune(entry.Gname, '\x00') {
		return false
	}
	if entry.Absent && entry.Path == "data" {
		return false
	}
	if entry.Structural && (entry.Type != "dir" || entry.Absent) {
		return false
	}
	switch entry.Type {
	case "dir":
		return entry.Size == 0 && entry.SHA256 == "" && entry.LinkTarget == ""
	case "file":
		return !entry.Absent && entry.Size >= 0 && validDigest(entry.SHA256) && entry.LinkTarget == ""
	case "symlink":
		return !entry.Absent && entry.Size == 0 && entry.SHA256 == "" && safeDataSymlink(entry.Path, entry.LinkTarget)
	default:
		return false
	}
}

func snapshotEncryptedArchive(ctx context.Context, sourcePath, stagingParent string) (*os.File, string, error) {
	source, err := openRegularNoFollow(sourcePath)
	if err != nil {
		return nil, "", err
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return nil, "", err
	}
	if info.Size() > 1<<40 {
		return nil, "", errors.New("encrypted backup exceeds the 1 TiB safety limit")
	}
	snapshot, err := os.CreateTemp(stagingParent, ".hermes-box-encrypted-*")
	if err != nil {
		return nil, "", err
	}
	fail := func(err error) (*os.File, string, error) {
		snapshot.Close()
		return nil, "", err
	}
	if err := snapshot.Chmod(0o600); err != nil {
		return fail(err)
	}
	if err := requireFreeSpace(snapshot.Name(), info.Size()); err != nil {
		return fail(err)
	}
	// Unlink immediately so no other process can address or mutate the private
	// ciphertext snapshot while it is verified and decrypted.
	if err := os.Remove(snapshot.Name()); err != nil {
		return fail(err)
	}
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(snapshot, hash), &contextReader{ctx: ctx, reader: io.LimitReader(source, info.Size()+1)})
	if err != nil {
		return fail(err)
	}
	if written != info.Size() {
		return fail(errors.New("encrypted backup changed size while it was snapshotted"))
	}
	if _, err := snapshot.Seek(0, io.SeekStart); err != nil {
		return fail(err)
	}
	return snapshot, fmt.Sprintf("%x", hash.Sum(nil)), nil
}

func allowedBundlePath(value string) bool {
	if value == "data/cache" || strings.HasPrefix(value, "data/cache/") {
		return false
	}
	if value == "applied.lock" || value == "data" || strings.HasPrefix(value, "data/") {
		return true
	}
	if !strings.HasPrefix(value, "artifacts/sha256/") {
		return false
	}
	return validDigest(strings.TrimPrefix(value, "artifacts/sha256/"))
}

func headerMatches(entry Entry, header *tar.Header) bool {
	if header.Mode != entry.Mode || header.Uid != entry.UID || header.Gid != entry.GID || header.Uname != entry.Uname || header.Gname != entry.Gname || header.Size != entry.Size || !validHermesMarkers(header, entry) {
		return false
	}
	switch entry.Type {
	case "dir":
		return header.Typeflag == tar.TypeDir
	case "file":
		return header.Typeflag == tar.TypeReg || header.Typeflag == tar.TypeRegA
	case "symlink":
		return header.Typeflag == tar.TypeSymlink && header.Linkname == entry.LinkTarget
	default:
		return false
	}
}

func validHermesMarkers(header *tar.Header, entry Entry) bool {
	found := false
	for key, value := range header.PAXRecords {
		if !strings.HasPrefix(key, "HERMESBOX.") {
			continue
		}
		if key != "HERMESBOX.absent" || value != "1" || entry.Type != "dir" {
			return false
		}
		found = true
	}
	return found == entry.Absent
}

func safeDestination(root, archivePath string) (string, error) {
	if !safeArchivePath(archivePath) {
		return "", errors.New("unsafe archive path")
	}
	destination := filepath.Join(root, filepath.FromSlash(archivePath))
	rel, err := filepath.Rel(root, destination)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("archive path escapes staging directory")
	}
	for parent := filepath.Dir(destination); parent != root && parent != "."; parent = filepath.Dir(parent) {
		info, err := os.Lstat(parent)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("archive path traverses a symlink")
		}
	}
	return destination, nil
}

type sha256Writer struct {
	writer io.Writer
	hash   hash.Hash
}

func newSHA256Writer(writer io.Writer) *sha256Writer {
	return &sha256Writer{writer: writer, hash: sha256.New()}
}

func (w *sha256Writer) Write(data []byte) (int, error) {
	n, err := w.writer.Write(data)
	if n > 0 {
		_, _ = w.hash.Write(data[:n])
	}
	return n, err
}

func (w *sha256Writer) Sum() string {
	return fmt.Sprintf("%x", w.hash.Sum(nil))
}

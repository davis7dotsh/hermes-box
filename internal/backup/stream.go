package backup

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"filippo.io/age"
)

const (
	maxGuestEntries = 200_000
	maxGuestBytes   = int64(1 << 40)
)

const guestStreamSource = "encrypted-guest-stream"

type guestEntrySink func(*Entry, io.Reader) error

// stageGuestTar is retained as a validation helper for focused tests. Backup
// creation uses spoolGuestTar, which sends the same validated bytes directly
// into an anonymous encrypted spool instead of materializing plaintext files.
func stageGuestTar(ctx context.Context, input io.Reader, _ string, dataPaths []string) ([]sourceFile, error) {
	return scanGuestTar(ctx, input, dataPaths, func(entry *Entry, body io.Reader) error {
		if body == nil {
			return nil
		}
		hash := sha256.New()
		written, err := io.Copy(hash, body)
		if err != nil || written != entry.Size {
			return errors.New("guest data tar entry has invalid content size")
		}
		entry.SHA256 = hex.EncodeToString(hash.Sum(nil))
		return nil
	})
}

func scanGuestTar(ctx context.Context, input io.Reader, dataPaths []string, sink guestEntrySink) ([]sourceFile, error) {
	reader := tar.NewReader(&contextReader{ctx: ctx, reader: input})
	seen := make(map[string]bool)
	types := make(map[string]string)
	files := make([]sourceFile, 0)
	var total int64
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			return nil, fmt.Errorf("read guest data tar: %w", err)
		}
		if len(files) >= maxGuestEntries {
			return nil, errors.New("guest data tar has too many entries")
		}
		name, err := canonicalGuestPath(header)
		if err != nil {
			return nil, err
		}
		if seen[name] {
			return nil, fmt.Errorf("duplicate guest data tar entry %q", name)
		}
		seen[name] = true
		if name == "data/cache" || strings.HasPrefix(name, "data/cache/") {
			return nil, fmt.Errorf("guest data tar contains excluded cache path %q", name)
		}
		if header.Uid < 0 || header.Uid > 65535 || header.Gid < 0 || header.Gid > 65535 {
			return nil, fmt.Errorf("guest data tar ownership is invalid for %q", name)
		}
		if header.Mode < 0 || header.Mode&^int64(0o7777) != 0 {
			return nil, fmt.Errorf("guest data tar mode is invalid for %q", name)
		}
		if len(header.Uname) > 255 || len(header.Gname) > 255 || strings.ContainsRune(header.Uname, '\x00') || strings.ContainsRune(header.Gname, '\x00') {
			return nil, fmt.Errorf("guest data tar owner name is invalid for %q", name)
		}
		item := sourceFile{Entry: Entry{Path: name, Mode: header.Mode, UID: header.Uid, GID: header.Gid, Uname: header.Uname, Gname: header.Gname}, Path: guestStreamSource}
		for key, value := range header.PAXRecords {
			if key == "HERMESBOX.absent" {
				if value != "1" || header.Typeflag != tar.TypeDir {
					return nil, fmt.Errorf("guest data tar has invalid absent marker for %q", name)
				}
				item.Entry.Absent = true
			} else if strings.HasPrefix(key, "HERMESBOX.") {
				return nil, fmt.Errorf("guest data tar has unknown Hermes Box marker %q", key)
			}
		}
		if item.Entry.Absent && (name == "data" || (fullDataPaths(dataPaths) && isScopeRoot(name, dataPaths))) {
			return nil, fmt.Errorf("guest data tar cannot mark required root %q absent", name)
		}
		switch header.Typeflag {
		case tar.TypeDir:
			item.Entry.Type = "dir"
		case tar.TypeReg, tar.TypeRegA:
			if header.Size < 0 || header.Size > maxGuestBytes-total {
				return nil, errors.New("guest data tar exceeds the 1 TiB safety limit")
			}
			total += header.Size
			item.Entry.Type = "file"
			item.Entry.Size = header.Size
		case tar.TypeSymlink:
			if len(header.Linkname) > 4096 || strings.ContainsRune(header.Linkname, '\x00') || !safeDataSymlink(name, header.Linkname) {
				return nil, fmt.Errorf("guest data tar has unsafe symlink %q", name)
			}
			if !fullDataPaths(dataPaths) && !symlinkWithinDataPaths(name, header.Linkname, dataPaths) {
				return nil, fmt.Errorf("guest data tar symlink %q escapes requested scopes", name)
			}
			item.Entry.Type = "symlink"
			item.Entry.LinkTarget = header.Linkname
		default:
			return nil, fmt.Errorf("guest data tar contains unsupported entry type %d for %q", header.Typeflag, name)
		}
		var body io.Reader
		if item.Entry.Type == "file" {
			body = &contextReader{ctx: ctx, reader: io.LimitReader(reader, header.Size)}
		}
		if err := sink(&item.Entry, body); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			return nil, fmt.Errorf("stream guest data tar entry %q: %w", name, err)
		}
		if item.Entry.Type == "file" && !validDigest(item.Entry.SHA256) {
			return nil, fmt.Errorf("guest data tar entry %q was not hashed", name)
		}
		files = append(files, item)
		types[name] = item.Entry.Type
	}
	for _, scope := range dataPaths {
		root := "data/" + scope
		if types[root] != "dir" {
			return nil, fmt.Errorf("guest data tar is missing requested scope %s", root)
		}
	}
	for name := range seen {
		for ancestor := path.Dir(name); ancestor != "."; ancestor = path.Dir(ancestor) {
			if types[ancestor] == "symlink" {
				return nil, fmt.Errorf("guest data tar entry %q is beneath symlink ancestor %q", name, ancestor)
			}
		}
		if name == "data" {
			continue
		}
		relative := strings.TrimPrefix(name, "data/")
		if !withinDataPaths(relative, dataPaths) && !(types[name] == "dir" && dataPathAncestor(relative, dataPaths)) {
			return nil, fmt.Errorf("guest data tar entry %q is outside requested scopes", name)
		}
	}
	if !seen["data"] {
		files = append(files, sourceFile{Entry: Entry{Path: "data", Type: "dir", Mode: 0o755, UID: 0, GID: 0, Structural: true}})
	}
	for index := range files {
		entry := &files[index].Entry
		if entry.Path == "data" {
			entry.Structural = entry.Structural || !fullDataPaths(dataPaths)
			continue
		}
		relative := strings.TrimPrefix(entry.Path, "data/")
		if entry.Type == "dir" && dataPathAncestor(relative, dataPaths) {
			entry.Structural = true
		}
	}
	for _, candidate := range files {
		if !candidate.Entry.Absent {
			continue
		}
		prefix := candidate.Entry.Path + "/"
		for _, other := range files {
			if strings.HasPrefix(other.Entry.Path, prefix) {
				return nil, fmt.Errorf("absent guest data path %q contains entry %q", candidate.Entry.Path, other.Entry.Path)
			}
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Entry.Path < files[j].Entry.Path })
	return files, nil
}

type guestSpool struct {
	file     *os.File
	identity age.Identity
}

func (s *guestSpool) Close() error {
	if s == nil || s.file == nil {
		return nil
	}
	err := s.file.Close()
	s.file = nil
	return err
}

// spoolGuestTar validates the guest stream while writing only age-encrypted
// bytes to an immediately unlinked temporary file. The ephemeral decryption
// identity exists only in memory for the duration of Create. Cancellation or
// process exit therefore cannot leave an addressable plaintext guest tree.
func spoolGuestTar(ctx context.Context, input io.Reader, directory string, dataPaths []string) (*guestSpool, []sourceFile, error) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		return nil, nil, err
	}
	file, err := os.CreateTemp(directory, ".guest-data-encrypted-*")
	if err != nil {
		return nil, nil, err
	}
	fail := func(cause error) (*guestSpool, []sourceFile, error) {
		return nil, nil, errors.Join(cause, file.Close())
	}
	if err := file.Chmod(0o600); err != nil {
		return fail(err)
	}
	if err := os.Remove(file.Name()); err != nil {
		return fail(err)
	}
	encrypted, err := age.Encrypt(file, identity.Recipient())
	if err != nil {
		return fail(err)
	}
	writer := tar.NewWriter(encrypted)
	entries, scanErr := scanGuestTar(ctx, input, dataPaths, func(entry *Entry, body io.Reader) error {
		header := tar.Header{
			Name: entry.Path, Mode: entry.Mode, Uid: entry.UID, Gid: entry.GID,
			Uname: entry.Uname, Gname: entry.Gname, Size: entry.Size,
		}
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
			return fmt.Errorf("unsupported guest spool entry type %q", entry.Type)
		}
		if err := writer.WriteHeader(&header); err != nil {
			return err
		}
		if body == nil {
			return nil
		}
		hash := sha256.New()
		written, err := io.Copy(io.MultiWriter(writer, hash), body)
		if err != nil || written != entry.Size {
			return errors.Join(errors.New("guest data tar entry has invalid content size"), err)
		}
		entry.SHA256 = hex.EncodeToString(hash.Sum(nil))
		return nil
	})
	closeErr := errors.Join(writer.Close(), encrypted.Close())
	if scanErr != nil || closeErr != nil {
		return fail(errors.Join(scanErr, closeErr))
	}
	if err := file.Sync(); err != nil {
		return fail(err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return fail(err)
	}
	return &guestSpool{file: file, identity: identity}, entries, nil
}

func writeGuestSpool(ctx context.Context, writer *tar.Writer, spool *guestSpool, entries []sourceFile) error {
	for _, item := range entries {
		if item.Path != "" {
			continue
		}
		if err := writeEntryHeader(writer, item.Entry); err != nil {
			return err
		}
	}
	if _, err := spool.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	decrypted, err := age.Decrypt(&contextReader{ctx: ctx, reader: spool.file}, spool.identity)
	if err != nil {
		return fmt.Errorf("decrypt private guest spool: %w", err)
	}
	reader := tar.NewReader(decrypted)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read private guest spool: %w", err)
		}
		if err := writer.WriteHeader(header); err != nil {
			return err
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		written, err := io.Copy(writer, &contextReader{ctx: ctx, reader: reader})
		if err != nil || written != header.Size {
			return errors.Join(errors.New("private guest spool entry changed size"), err)
		}
	}
}

func symlinkWithinDataPaths(name, target string, dataPaths []string) bool {
	resolved := path.Clean(path.Join(path.Dir(name), filepath.ToSlash(target)))
	if !strings.HasPrefix(resolved, "data/") {
		return false
	}
	return withinDataPaths(strings.TrimPrefix(resolved, "data/"), dataPaths)
}

func normalizeDataPaths(values []string) ([]string, error) {
	if len(values) == 0 {
		return []string{"executor", "home/agent"}, nil
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || strings.HasPrefix(value, "/") || strings.ContainsRune(value, '\x00') || strings.HasPrefix(value, "data/") {
			return nil, fmt.Errorf("invalid data scope %q", value)
		}
		clean := path.Clean(value)
		if clean != value || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || clean == "cache" || strings.HasPrefix(clean, "cache/") {
			return nil, fmt.Errorf("invalid data scope %q", value)
		}
		result = append(result, clean)
	}
	sort.Strings(result)
	for index, value := range result {
		if index > 0 && (value == result[index-1] || strings.HasPrefix(value, result[index-1]+"/")) {
			return nil, fmt.Errorf("overlapping data scope %q", value)
		}
	}
	return result, nil
}

func withinDataPaths(relative string, dataPaths []string) bool {
	for _, scope := range dataPaths {
		if relative == scope || strings.HasPrefix(relative, scope+"/") {
			return true
		}
	}
	return false
}

func dataPathAncestor(relative string, dataPaths []string) bool {
	for _, scope := range dataPaths {
		if strings.HasPrefix(scope, relative+"/") {
			return true
		}
	}
	return false
}

func isScopeRoot(name string, dataPaths []string) bool {
	for _, scope := range dataPaths {
		if name == "data/"+scope {
			return true
		}
	}
	return false
}

func fullDataPaths(dataPaths []string) bool {
	return len(dataPaths) == 2 && dataPaths[0] == "executor" && dataPaths[1] == "home/agent"
}

func canonicalGuestPath(header *tar.Header) (string, error) {
	if header == nil || header.Name == "" || len(header.Name) > 4096 || strings.ContainsRune(header.Name, '\x00') || strings.HasPrefix(header.Name, "/") {
		return "", errors.New("guest data tar has an invalid path")
	}
	clean := path.Clean(header.Name)
	if clean != header.Name && !(header.Typeflag == tar.TypeDir && header.Name == clean+"/") {
		return "", fmt.Errorf("guest data tar path is not canonical: %q", header.Name)
	}
	if clean != "data" && !strings.HasPrefix(clean, "data/") {
		return "", fmt.Errorf("guest data tar path is outside data: %q", header.Name)
	}
	if !safeArchivePath(clean) {
		return "", fmt.Errorf("guest data tar path is unsafe: %q", header.Name)
	}
	return clean, nil
}

// WriteDataTar reconstructs the verified guest data stream with the original
// Linux UID, GID, mode, owner names, and symlink targets from the manifest.
func WriteDataTar(ctx context.Context, bundleRoot string, manifest Manifest, output io.Writer) error {
	dataPaths, err := normalizeDataPaths(manifest.DataPaths)
	if err != nil || len(dataPaths) != len(manifest.DataPaths) {
		return errors.New("manifest has invalid durable data scopes")
	}
	for index := range dataPaths {
		if dataPaths[index] != manifest.DataPaths[index] {
			return errors.New("manifest durable data scopes are not canonical")
		}
	}
	writer := tar.NewWriter(output)
	var result error
	for _, entry := range manifest.Entries {
		if entry.Path != "data" && !strings.HasPrefix(entry.Path, "data/") {
			continue
		}
		if entry.Structural {
			continue
		}
		relative := strings.TrimPrefix(entry.Path, "data/")
		if entry.Path != "data" && !withinDataPaths(relative, dataPaths) && !(entry.Type == "dir" && dataPathAncestor(relative, dataPaths)) {
			result = fmt.Errorf("verified data entry %q is outside declared scopes", entry.Path)
			break
		}
		if !validManifestEntry(entry) {
			result = fmt.Errorf("invalid verified data entry %q", entry.Path)
			break
		}
		if err := ctx.Err(); err != nil {
			result = err
			break
		}
		header := &tar.Header{Name: entry.Path, Mode: entry.Mode, Uid: entry.UID, Gid: entry.GID, Uname: entry.Uname, Gname: entry.Gname, Size: entry.Size}
		if entry.Absent {
			header.PAXRecords = map[string]string{"HERMESBOX.absent": "1"}
		}
		switch entry.Type {
		case "dir":
			header.Typeflag = tar.TypeDir
		case "file":
			header.Typeflag = tar.TypeReg
		case "symlink":
			header.Typeflag = tar.TypeSymlink
			header.Linkname = entry.LinkTarget
		default:
			result = fmt.Errorf("unsupported verified data entry type %q", entry.Type)
		}
		if result != nil {
			break
		}
		if err := writer.WriteHeader(header); err != nil {
			result = err
			break
		}
		if entry.Type != "file" {
			continue
		}
		filePath := dataPayloadPath(bundleRoot, entry)
		file, err := openRegularNoFollow(filePath)
		if err != nil {
			result = err
			break
		}
		hash := sha256.New()
		written, copyErr := io.Copy(writer, io.TeeReader(&contextReader{ctx: ctx, reader: file}, hash))
		closeErr := file.Close()
		if copyErr != nil || closeErr != nil || written != entry.Size || hex.EncodeToString(hash.Sum(nil)) != entry.SHA256 {
			if ctxErr := ctx.Err(); ctxErr != nil {
				result = ctxErr
				break
			}
			result = fmt.Errorf("verified data entry %q changed before streaming", entry.Path)
			break
		}
	}
	return errors.Join(result, writer.Close())
}

func dataPayloadPath(root string, entry Entry) string {
	digest := sha256.Sum256([]byte(entry.Path + "\x00" + entry.SHA256))
	return filepath.Join(root, "payloads", hex.EncodeToString(digest[:]))
}

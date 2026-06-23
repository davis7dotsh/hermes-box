package artifacts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
)

var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

const defaultMaxArtifactBytes int64 = 32 << 30

type Reference struct {
	Name   string
	URL    string
	SHA256 string
	// ExpectedSize is optional. When non-zero, the response must match exactly.
	ExpectedSize int64
}

type Artifact struct {
	Reference
	Path string
	Size int64
}

type Set map[string]Artifact

type Store struct {
	Root   string
	Client *http.Client
	// MaxBytes defaults to 32 GiB and is always enforced, including for chunked responses.
	MaxBytes int64
}

func (s Store) Materialize(ctx context.Context, refs []Reference) (Set, error) {
	if s.Root == "" {
		return nil, errors.New("artifact cache root is required")
	}
	result := make(Set, len(refs))
	for _, ref := range refs {
		if _, exists := result[ref.Name]; exists {
			return nil, fmt.Errorf("duplicate artifact name %q", ref.Name)
		}
		artifact, err := s.Fetch(ctx, ref)
		if err != nil {
			return nil, fmt.Errorf("materialize %s: %w", ref.Name, err)
		}
		result[ref.Name] = artifact
	}
	return result, nil
}

func (s Store) Fetch(ctx context.Context, ref Reference) (Artifact, error) {
	if s.Root == "" {
		return Artifact{}, errors.New("artifact cache root is required")
	}
	if ref.Name == "" || ref.URL == "" || !digestPattern.MatchString(ref.SHA256) || ref.ExpectedSize < 0 {
		return Artifact{}, errors.New("artifact name, URL, and lowercase SHA-256 are required")
	}
	dir := filepath.Join(s.Root, "sha256", ref.SHA256[:2])
	path := filepath.Join(dir, ref.SHA256)
	if info, err := verifyFileContext(ctx, path, ref.SHA256); err == nil {
		if ref.ExpectedSize > 0 && info.Size() != ref.ExpectedSize {
			return Artifact{}, fmt.Errorf("cached artifact size mismatch: expected %d, got %d", ref.ExpectedSize, info.Size())
		}
		return Artifact{Reference: ref, Path: path, Size: info.Size()}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Artifact{}, ctxErr
		}
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return Artifact{}, fmt.Errorf("remove corrupt cache entry: %w", removeErr)
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Artifact{}, err
	}
	tmp, err := os.CreateTemp(dir, ".partial-*")
	if err != nil {
		return Artifact{}, err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ref.URL, nil)
	if err != nil {
		tmp.Close()
		return Artifact{}, err
	}
	client := s.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		tmp.Close()
		return Artifact{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		tmp.Close()
		return Artifact{}, fmt.Errorf("download returned %s", resp.Status)
	}
	limit := s.MaxBytes
	if limit <= 0 {
		limit = defaultMaxArtifactBytes
	}
	if ref.ExpectedSize > limit {
		tmp.Close()
		return Artifact{}, fmt.Errorf("expected artifact size %d exceeds limit %d", ref.ExpectedSize, limit)
	}
	if resp.ContentLength > limit {
		tmp.Close()
		return Artifact{}, fmt.Errorf("artifact Content-Length %d exceeds limit %d", resp.ContentLength, limit)
	}
	if ref.ExpectedSize > 0 && resp.ContentLength >= 0 && resp.ContentLength != ref.ExpectedSize {
		tmp.Close()
		return Artifact{}, fmt.Errorf("artifact Content-Length mismatch: expected %d, got %d", ref.ExpectedSize, resp.ContentLength)
	}
	hash := sha256.New()
	size, copyErr := io.Copy(io.MultiWriter(tmp, hash), io.LimitReader(resp.Body, limit+1))
	closeErr := tmp.Close()
	if copyErr != nil {
		return Artifact{}, copyErr
	}
	if closeErr != nil {
		return Artifact{}, closeErr
	}
	if size > limit {
		return Artifact{}, fmt.Errorf("artifact exceeds limit of %d bytes", limit)
	}
	if ref.ExpectedSize > 0 && size != ref.ExpectedSize {
		return Artifact{}, fmt.Errorf("artifact size mismatch: expected %d, got %d", ref.ExpectedSize, size)
	}
	actual := hex.EncodeToString(hash.Sum(nil))
	if actual != ref.SHA256 {
		return Artifact{}, fmt.Errorf("SHA-256 mismatch: expected %s, got %s", ref.SHA256, actual)
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return Artifact{}, err
	}
	if err := syncFile(tmpPath); err != nil {
		return Artifact{}, err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return Artifact{}, err
	}
	if err := syncDirectory(dir); err != nil {
		return Artifact{}, err
	}
	info, err := verifyFileContext(ctx, path, ref.SHA256)
	if err != nil {
		return Artifact{}, err
	}
	return Artifact{Reference: ref, Path: path, Size: sizeOr(info.Size(), size)}, nil
}

func syncFile(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func VerifyClosure(set Set, refs []Reference) error {
	if len(set) != len(refs) {
		return fmt.Errorf("artifact closure incomplete: have %d, need %d", len(set), len(refs))
	}
	for _, ref := range refs {
		artifact, ok := set[ref.Name]
		if !ok || artifact.SHA256 != ref.SHA256 {
			return fmt.Errorf("artifact closure missing %q at %s", ref.Name, ref.SHA256)
		}
		info, err := verifyFile(artifact.Path, ref.SHA256)
		if err != nil {
			return fmt.Errorf("verify artifact %q: %w", ref.Name, err)
		}
		if ref.ExpectedSize > 0 && info.Size() != ref.ExpectedSize {
			return fmt.Errorf("verify artifact %q: expected size %d, got %d", ref.Name, ref.ExpectedSize, info.Size())
		}
	}
	return nil
}

func verifyFile(path, expected string) (os.FileInfo, error) {
	return verifyFileContext(context.Background(), path, expected)
}

func verifyFileContext(ctx context.Context, path, expected string) (os.FileInfo, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, &contextReader{ctx: ctx, reader: file}); err != nil {
		return nil, err
	}
	actual := hex.EncodeToString(hash.Sum(nil))
	if actual != expected {
		return nil, fmt.Errorf("SHA-256 mismatch: expected %s, got %s", expected, actual)
	}
	return info, nil
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

func sizeOr(statSize, copied int64) int64 {
	if statSize != 0 {
		return statSize
	}
	return copied
}

package artifacts

import (
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

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

type OCIArtifact struct {
	Reference        string
	RuntimeReference string
	Path             string
	Size             int64
	IndexDigest      string
	ChildDigest      string
	ArchiveSHA256    string
}

type ociAttestation struct {
	Schema        int    `json:"schema"`
	IndexDigest   string `json:"index_digest"`
	ChildDigest   string `json:"child_digest"`
	OS            string `json:"os"`
	Architecture  string `json:"architecture"`
	RuntimeTag    string `json:"runtime_tag"`
	ArchiveSHA256 string `json:"archive_sha256"`
}

func SelectLinuxARM64(manifest *v1.IndexManifest, expectedChild string) (v1.Descriptor, error) {
	if manifest == nil {
		return v1.Descriptor{}, errors.New("OCI index manifest is required")
	}
	foundPlatform := false
	for _, descriptor := range manifest.Manifests {
		platform := descriptor.Platform
		if platform != nil && platform.OS == "linux" && platform.Architecture == "arm64" {
			foundPlatform = true
			if descriptor.Digest.String() == expectedChild {
				return descriptor, nil
			}
		}
	}
	if foundPlatform {
		return v1.Descriptor{}, fmt.Errorf("OCI index does not map expected digest %s to linux/arm64", expectedChild)
	}
	return v1.Descriptor{}, errors.New("OCI index has no linux/arm64 image")
}

func (s Store) MaterializeOCI(ctx context.Context, imageRef, expectedIndex, expectedChild string, options ...remote.Option) (OCIArtifact, error) {
	if s.Root == "" {
		return OCIArtifact{}, errors.New("artifact cache root is required")
	}
	indexName, err := ociDigestName(expectedIndex)
	if err != nil {
		return OCIArtifact{}, fmt.Errorf("invalid OCI index digest: %w", err)
	}
	childName, err := ociDigestName(expectedChild)
	if err != nil {
		return OCIArtifact{}, fmt.Errorf("invalid OCI child digest: %w", err)
	}
	runtimeTag, immutableIndex, err := parseOCIReference(imageRef, expectedIndex)
	if err != nil {
		return OCIArtifact{}, fmt.Errorf("invalid OCI image reference: %w", err)
	}
	dir := filepath.Join(s.Root, "oci", indexName)
	archivePath := filepath.Join(dir, childName+".tar")
	attestationPath := filepath.Join(dir, childName+".json")
	limit := s.MaxBytes
	if limit <= 0 {
		limit = defaultMaxArtifactBytes
	}
	if artifact, cacheErr := loadVerifiedOCI(imageRef, runtimeTag, archivePath, attestationPath, expectedIndex, expectedChild, limit); cacheErr == nil {
		return artifact, nil
	} else {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return OCIArtifact{}, ctxErr
		}
		if err := removeOCIPair(archivePath, attestationPath); err != nil {
			return OCIArtifact{}, fmt.Errorf("remove invalid OCI cache entry: %w", err)
		}
	}
	descriptor, err := remote.Get(immutableIndex, append(options, remote.WithContext(ctx))...)
	if err != nil {
		return OCIArtifact{}, err
	}
	if descriptor.Digest.String() != expectedIndex {
		return OCIArtifact{}, fmt.Errorf("OCI index digest mismatch: expected %s, got %s", expectedIndex, descriptor.Digest)
	}
	index, err := descriptor.ImageIndex()
	if err != nil {
		return OCIArtifact{}, fmt.Errorf("read OCI index: %w", err)
	}
	manifest, err := index.IndexManifest()
	if err != nil {
		return OCIArtifact{}, err
	}
	child, err := SelectLinuxARM64(manifest, expectedChild)
	if err != nil {
		return OCIArtifact{}, err
	}
	image, err := index.Image(child.Digest)
	if err != nil {
		return OCIArtifact{}, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return OCIArtifact{}, err
	}
	tmp, err := os.CreateTemp(dir, ".partial-*.tar")
	if err != nil {
		return OCIArtifact{}, err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return OCIArtifact{}, err
	}
	limited := &limitWriter{writer: tmp, remaining: limit}
	writeErr := tarball.Write(runtimeTag, image, limited)
	closeErr := tmp.Close()
	if writeErr != nil {
		return OCIArtifact{}, writeErr
	}
	if closeErr != nil {
		return OCIArtifact{}, closeErr
	}
	if limited.exceeded {
		return OCIArtifact{}, fmt.Errorf("OCI archive exceeds limit of %d bytes", limit)
	}
	if err := syncFile(tmpPath); err != nil {
		return OCIArtifact{}, err
	}
	if err := verifyImageTar(tmpPath, runtimeTag, expectedChild); err != nil {
		return OCIArtifact{}, err
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return OCIArtifact{}, err
	}
	if err := os.Rename(tmpPath, archivePath); err != nil {
		return OCIArtifact{}, err
	}
	info, err := os.Stat(archivePath)
	if err != nil {
		return OCIArtifact{}, err
	}
	archiveHash, err := digestFile(archivePath)
	if err != nil {
		return OCIArtifact{}, err
	}
	attestation := ociAttestation{
		Schema: 1, IndexDigest: expectedIndex, ChildDigest: expectedChild,
		OS: "linux", Architecture: "arm64", RuntimeTag: runtimeTag.String(), ArchiveSHA256: archiveHash,
	}
	if err := writeOCIAttestation(attestationPath, attestation); err != nil {
		_ = removeOCIPair(archivePath, attestationPath)
		return OCIArtifact{}, err
	}
	if err := syncDirectory(dir); err != nil {
		_ = removeOCIPair(archivePath, attestationPath)
		return OCIArtifact{}, err
	}
	return OCIArtifact{
		Reference: imageRef, RuntimeReference: runtimeTag.String(), Path: archivePath, Size: info.Size(),
		IndexDigest: expectedIndex, ChildDigest: expectedChild, ArchiveSHA256: archiveHash,
	}, nil
}

func parseOCIReference(value, expectedIndex string) (name.Tag, name.Digest, error) {
	tagValue, embeddedDigest, ok := strings.Cut(value, "@")
	if !ok || strings.Contains(embeddedDigest, "@") {
		return name.Tag{}, name.Digest{}, errors.New("must use an explicit repo:tag@sha256:index-digest reference")
	}
	if embeddedDigest != expectedIndex {
		return name.Tag{}, name.Digest{}, fmt.Errorf("embedded index digest %s does not match expected %s", embeddedDigest, expectedIndex)
	}
	runtimeTag, err := name.NewTag(tagValue, name.StrictValidation)
	if err != nil {
		return name.Tag{}, name.Digest{}, errors.New("must contain a fully qualified explicit repo:tag")
	}
	return runtimeTag, runtimeTag.Context().Digest(expectedIndex), nil
}

func ociDigestName(value string) (string, error) {
	name := strings.TrimPrefix(value, "sha256:")
	if value != "sha256:"+name || !digestPattern.MatchString(name) {
		return "", errors.New("must be a lowercase sha256 digest")
	}
	return name, nil
}

func loadVerifiedOCI(reference string, runtimeTag name.Tag, archivePath, attestationPath, expectedIndex, expectedChild string, limit int64) (OCIArtifact, error) {
	attestationFile, err := openRegular(attestationPath)
	if err != nil {
		return OCIArtifact{}, err
	}
	defer attestationFile.Close()
	attestationInfo, err := attestationFile.Stat()
	if err != nil {
		return OCIArtifact{}, err
	}
	if attestationInfo.Size() < 1 || attestationInfo.Size() > 1<<20 {
		return OCIArtifact{}, errors.New("OCI cache attestation exceeds its size limit")
	}
	var attestation ociAttestation
	decoder := json.NewDecoder(attestationFile)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&attestation); err != nil {
		return OCIArtifact{}, fmt.Errorf("read OCI cache attestation: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return OCIArtifact{}, errors.New("OCI cache attestation has trailing data")
	}
	if attestation.Schema != 1 || attestation.IndexDigest != expectedIndex || attestation.ChildDigest != expectedChild || attestation.OS != "linux" || attestation.Architecture != "arm64" || attestation.RuntimeTag != runtimeTag.String() || !digestPattern.MatchString(attestation.ArchiveSHA256) {
		return OCIArtifact{}, errors.New("OCI cache attestation does not match requested index, child, and runtime tag")
	}
	info, err := os.Lstat(archivePath)
	if err != nil {
		return OCIArtifact{}, err
	}
	if !info.Mode().IsRegular() {
		return OCIArtifact{}, errors.New("OCI cache archive is not a regular file")
	}
	if info.Size() > limit {
		return OCIArtifact{}, fmt.Errorf("cached OCI archive exceeds limit of %d bytes", limit)
	}
	archiveHash, err := digestFile(archivePath)
	if err != nil {
		return OCIArtifact{}, err
	}
	if archiveHash != attestation.ArchiveSHA256 {
		return OCIArtifact{}, errors.New("OCI cache archive does not match its attestation")
	}
	if err := verifyImageTar(archivePath, runtimeTag, expectedChild); err != nil {
		return OCIArtifact{}, err
	}
	return OCIArtifact{
		Reference: reference, RuntimeReference: runtimeTag.String(), Path: archivePath, Size: info.Size(),
		IndexDigest: expectedIndex, ChildDigest: expectedChild, ArchiveSHA256: archiveHash,
	}, nil
}

func openRegular(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("OCI cache attestation is not a regular file")
	}
	return os.Open(path)
}

func writeOCIAttestation(destination string, attestation ociAttestation) error {
	data, err := json.Marshal(attestation)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(destination), ".attestation-*.partial")
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
	return os.Rename(tmpPath, destination)
}

func removeOCIPair(archivePath, attestationPath string) error {
	var result error
	for _, path := range []string{archivePath, attestationPath} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, err)
		}
	}
	return result
}

type limitWriter struct {
	writer    io.Writer
	remaining int64
	exceeded  bool
}

func (w *limitWriter) Write(data []byte) (int, error) {
	if int64(len(data)) > w.remaining {
		w.exceeded = true
		return 0, fmt.Errorf("artifact exceeds configured byte limit")
	}
	n, err := w.writer.Write(data)
	w.remaining -= int64(n)
	return n, err
}

func digestFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func verifyImageTar(path string, runtimeTag name.Tag, expected string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("OCI cache entry is not a regular file")
	}
	manifest, err := tarball.LoadManifest(func() (io.ReadCloser, error) { return os.Open(path) })
	if err != nil {
		return err
	}
	if len(manifest) != 1 || len(manifest[0].RepoTags) != 1 || manifest[0].RepoTags[0] != runtimeTag.String() {
		return fmt.Errorf("OCI archive must contain exactly runtime tag %s", runtimeTag)
	}
	image, err := tarball.ImageFromPath(path, &runtimeTag)
	if err != nil {
		return err
	}
	digest, err := image.Digest()
	if err != nil {
		return err
	}
	if digest.String() != expected {
		return fmt.Errorf("OCI child digest mismatch: expected %s, got %s", expected, digest)
	}
	return nil
}

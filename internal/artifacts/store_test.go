package artifacts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

func TestStoreMaterializeAndReverify(t *testing.T) {
	t.Parallel()
	payload := []byte("immutable artifact")
	digest := sha256.Sum256(payload)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests++
		writer.Write(payload)
	}))
	defer server.Close()
	store := Store{Root: t.TempDir()}
	ref := Reference{Name: "tool", URL: server.URL, SHA256: hex.EncodeToString(digest[:])}
	first, err := store.Fetch(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(first.Path, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := store.Fetch(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if requests != 2 {
		t.Fatalf("expected corrupt cache entry to be fetched again, got %d requests", requests)
	}
	if err := VerifyClosure(Set{"tool": second}, []Reference{ref}); err != nil {
		t.Fatal(err)
	}
}

func TestStoreRejectsChecksumMismatchAndPartialFile(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Write([]byte("wrong"))
	}))
	defer server.Close()
	store := Store{Root: t.TempDir()}
	_, err := store.Fetch(context.Background(), Reference{Name: "tool", URL: server.URL, SHA256: strings.Repeat("0", 64)})
	if err == nil {
		t.Fatal("expected checksum mismatch")
	}
	partials, globErr := filepath.Glob(filepath.Join(store.Root, "sha256", "00", ".partial-*"))
	if globErr != nil || len(partials) != 0 {
		t.Fatalf("partial files remain: %v, err %v", partials, globErr)
	}
}

func TestSelectLinuxARM64(t *testing.T) {
	t.Parallel()
	digest, err := v1.NewHash("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	manifest := &v1.IndexManifest{Manifests: []v1.Descriptor{{
		MediaType: types.OCIManifestSchema1,
		Digest:    digest,
		Platform:  &v1.Platform{OS: "linux", Architecture: "arm64"},
	}}}
	descriptor, err := SelectLinuxARM64(manifest, digest.String())
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.Digest != digest {
		t.Fatalf("selected %s, want %s", descriptor.Digest, digest)
	}
	if _, err := SelectLinuxARM64(manifest, "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"); err == nil {
		t.Fatal("expected child digest mismatch")
	}
}

func TestFetchRequiresRootAndClosureIsExact(t *testing.T) {
	t.Parallel()
	if _, err := (Store{}).Fetch(context.Background(), Reference{}); err == nil {
		t.Fatal("expected empty cache root rejection")
	}
	if err := VerifyClosure(Set{}, []Reference{{Name: "missing", SHA256: strings.Repeat("0", 64)}}); err == nil {
		t.Fatal("expected incomplete closure rejection")
	}
}

func TestCancelledCacheVerificationPreservesEntry(t *testing.T) {
	t.Parallel()
	payload := []byte("cached")
	digest := sha256.Sum256(payload)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) { writer.Write(payload) }))
	defer server.Close()
	store := Store{Root: t.TempDir()}
	ref := Reference{Name: "tool", URL: server.URL, SHA256: hex.EncodeToString(digest[:])}
	artifact, err := store.Fetch(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Fetch(ctx, ref); err == nil {
		t.Fatal("expected cancellation")
	}
	if _, err := os.Stat(artifact.Path); err != nil {
		t.Fatalf("cancellation removed valid cache entry: %v", err)
	}
}

func TestFetchEnforcesSizeForChunkedResponses(t *testing.T) {
	t.Parallel()
	payload := []byte("too large")
	digest := sha256.Sum256(payload)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		flusher := writer.(http.Flusher)
		for _, value := range payload {
			writer.Write([]byte{value})
			flusher.Flush()
		}
	}))
	defer server.Close()
	store := Store{Root: t.TempDir(), MaxBytes: 4}
	_, err := store.Fetch(context.Background(), Reference{Name: "tool", URL: server.URL, SHA256: hex.EncodeToString(digest[:])})
	if err == nil || !strings.Contains(err.Error(), "exceeds limit") {
		t.Fatalf("expected size limit failure, got %v", err)
	}
	partials, _ := filepath.Glob(filepath.Join(store.Root, "sha256", "*", ".partial-*"))
	if len(partials) != 0 {
		t.Fatalf("partial files remain: %v", partials)
	}
}

func TestMaterializeOCIReusesVerifiedCacheOffline(t *testing.T) {
	t.Parallel()
	store, reference, indexDigest, childDigest, transport, closeRegistry := testOCIRegistry(t)
	first, err := store.MaterializeOCI(context.Background(), reference, indexDigest, childDigest, remote.WithTransport(transport))
	if err != nil {
		t.Fatal(err)
	}
	closeRegistry()
	second, err := store.MaterializeOCI(context.Background(), reference, indexDigest, childDigest)
	if err != nil {
		t.Fatalf("verified OCI cache was not usable offline: %v", err)
	}
	if second.Path != first.Path || second.ArchiveSHA256 != first.ArchiveSHA256 || second.IndexDigest != indexDigest || second.ChildDigest != childDigest {
		t.Fatalf("offline artifact does not match verified cache: first=%+v second=%+v", first, second)
	}
	runtimeTag := strings.Split(reference, "@")[0]
	if first.RuntimeReference != runtimeTag || second.RuntimeReference != runtimeTag {
		t.Fatalf("runtime references = %q and %q, want %q", first.RuntimeReference, second.RuntimeReference, runtimeTag)
	}
	manifest, err := tarball.LoadManifest(func() (io.ReadCloser, error) { return os.Open(first.Path) })
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest) != 1 || len(manifest[0].RepoTags) != 1 || manifest[0].RepoTags[0] != runtimeTag {
		t.Fatalf("archive RepoTags = %#v, want [%q]", manifest, runtimeTag)
	}
}

func TestMaterializeOCIRequiresExplicitTagAtIndexDigest(t *testing.T) {
	t.Parallel()
	indexDigest := "sha256:" + strings.Repeat("a", 64)
	childDigest := "sha256:" + strings.Repeat("b", 64)
	store := Store{Root: t.TempDir()}
	for _, reference := range []string{
		"registry.example/executor:v1",
		"registry.example/executor@" + indexDigest,
		"registry.example/executor:v1@sha256:" + strings.Repeat("c", 64),
	} {
		if _, err := store.MaterializeOCI(context.Background(), reference, indexDigest, childDigest); err == nil {
			t.Fatalf("reference %q was accepted", reference)
		}
	}
}

func TestMaterializeOCIRejectsReviewedChildDigestMismatch(t *testing.T) {
	t.Parallel()
	store, reference, indexDigest, _, transport, closeRegistry := testOCIRegistry(t)
	defer closeRegistry()
	wrongChild := "sha256:" + strings.Repeat("f", 64)
	if _, err := store.MaterializeOCI(context.Background(), reference, indexDigest, wrongChild, remote.WithTransport(transport)); err == nil || !strings.Contains(err.Error(), "does not map expected digest") {
		t.Fatalf("reviewed child mismatch error = %v", err)
	}
}

func TestMaterializeOCIRejectsTamperedCacheOffline(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		tamper func(t *testing.T, artifact OCIArtifact, attestation string)
	}{
		{
			name: "archive",
			tamper: func(t *testing.T, artifact OCIArtifact, _ string) {
				t.Helper()
				if err := os.WriteFile(artifact.Path, []byte("tampered"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "attestation",
			tamper: func(t *testing.T, _ OCIArtifact, attestation string) {
				t.Helper()
				data, err := os.ReadFile(attestation)
				if err != nil {
					t.Fatal(err)
				}
				tampered := strings.Replace(string(data), `"architecture":"arm64"`, `"architecture":"amd64"`, 1)
				if tampered == string(data) {
					t.Fatal("attestation did not contain expected platform")
				}
				if err := os.WriteFile(attestation, []byte(tampered), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "runtime tag attestation",
			tamper: func(t *testing.T, artifact OCIArtifact, attestation string) {
				t.Helper()
				data, err := os.ReadFile(attestation)
				if err != nil {
					t.Fatal(err)
				}
				tampered := strings.Replace(string(data), artifact.RuntimeReference, "registry.invalid/executor:tampered", 1)
				if tampered == string(data) {
					t.Fatal("attestation did not contain expected runtime tag")
				}
				if err := os.WriteFile(attestation, []byte(tampered), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, reference, indexDigest, childDigest, transport, closeRegistry := testOCIRegistry(t)
			artifact, err := store.MaterializeOCI(context.Background(), reference, indexDigest, childDigest, remote.WithTransport(transport))
			if err != nil {
				t.Fatal(err)
			}
			attestation := strings.TrimSuffix(artifact.Path, ".tar") + ".json"
			test.tamper(t, artifact, attestation)
			closeRegistry()
			if _, err := store.MaterializeOCI(context.Background(), reference, indexDigest, childDigest); err == nil {
				t.Fatal("tampered OCI cache was accepted offline")
			}
		})
	}
}

func testOCIRegistry(t *testing.T) (Store, string, string, string, http.RoundTripper, func()) {
	t.Helper()
	server := httptest.NewTLSServer(registry.New())
	host := strings.TrimPrefix(server.URL, "https://")
	tag, err := name.NewTag(host + "/executor:latest")
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	image, err := random.Image(1024, 1)
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	childDigest, err := image.Digest()
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	index := mutate.AppendManifests(empty.Index, mutate.IndexAddendum{
		Add: image,
		Descriptor: v1.Descriptor{
			Digest:   childDigest,
			Platform: &v1.Platform{OS: "linux", Architecture: "arm64"},
		},
	})
	indexDigest, err := index.Digest()
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	transport := server.Client().Transport
	if err := remote.WriteIndex(tag, index, remote.WithTransport(transport)); err != nil {
		server.Close()
		t.Fatal(err)
	}
	return Store{Root: t.TempDir()}, tag.String() + "@" + indexDigest.String(), indexDigest.String(), childDigest.String(), transport, server.Close
}

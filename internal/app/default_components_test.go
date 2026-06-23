package app

import (
	"context"
	"crypto/sha512"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetchSRIEnforcesBoundForChunkedClaudeTarball(t *testing.T) {
	payload := []byte("too-large")
	digest := sha512.Sum512(payload)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		flusher := writer.(http.Flusher)
		for _, value := range payload {
			_, _ = writer.Write([]byte{value})
			flusher.Flush()
		}
	}))
	defer server.Close()
	_, err := fetchSRIWithClient(
		context.Background(), t.TempDir(), server.URL,
		"sha512-"+base64.StdEncoding.EncodeToString(digest[:]), server.Client(), 4,
	)
	if err == nil || !strings.Contains(err.Error(), "exceeds limit") {
		t.Fatalf("bounded Claude fetch error = %v", err)
	}
}

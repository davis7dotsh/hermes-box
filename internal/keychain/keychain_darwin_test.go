//go:build darwin && cgo

package keychain

import "testing"

func TestDarwinNewValidatesWithoutAccessingKeychain(t *testing.T) {
	t.Parallel()
	if _, err := New(""); err == nil {
		t.Fatal("expected empty service rejection")
	}
	store, err := New("dev.hermes-box.ci")
	if err != nil {
		t.Fatal(err)
	}
	if got := string(store.service); got != "dev.hermes-box.ci" {
		t.Fatalf("service = %q", got)
	}
}

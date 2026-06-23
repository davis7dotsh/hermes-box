//go:build !darwin || !cgo

package keychain

import (
	"errors"
	"testing"
)

func TestUnsupportedStoreFailsClosed(t *testing.T) {
	t.Parallel()
	store := &KeychainStore{}
	_, getErr := store.Get("main")
	putErr := store.Put("main", []byte("secret"))
	_, createErr := store.Create("main", []byte("secret"))
	deleteErr := store.Delete("main")
	for operation, err := range map[string]error{
		"get": getErr, "put": putErr, "create": createErr, "delete": deleteErr,
	} {
		if !errors.Is(err, ErrUnavailable) {
			t.Errorf("%s error = %v, want ErrUnavailable", operation, err)
		}
		if errors.Is(err, ErrNotFound) {
			t.Errorf("%s reported unavailable Keychain as a missing item", operation)
		}
	}
}

func TestUnsupportedNewReportsUnavailable(t *testing.T) {
	t.Parallel()
	if _, err := New("hermes-box"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("New error = %v, want ErrUnavailable", err)
	}
}

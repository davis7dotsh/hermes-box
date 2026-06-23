package app

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/davis7dotsh/hermes-box/internal/keychain"
)

func TestNewDefaultReturnsKeychainInitializationError(t *testing.T) {
	want := errors.New("keychain initialization failed")
	cli, err := newDefault(strings.NewReader(""), io.Discard, io.Discard, nil, func(string) (keychain.Store, error) {
		return nil, want
	})
	if cli != nil || !errors.Is(err, want) || !strings.Contains(err.Error(), "initialize backup keychain") {
		t.Fatalf("NewDefault result = %#v, error = %v", cli, err)
	}
}

func TestNewDefaultAllowsTextCommandsOnUnsupportedContributorHost(t *testing.T) {
	var stdout bytes.Buffer
	cli, err := newDefault(strings.NewReader(""), &stdout, io.Discard, nil, func(string) (keychain.Store, error) {
		return nil, keychain.ErrUnavailable
	})
	if err != nil {
		// On the supported Darwin host an unavailable Keychain is correctly fatal.
		return
	}
	if status := cli.Run(t.Context(), []string{"help"}); status != 0 || stdout.Len() == 0 {
		t.Fatalf("unsupported contributor help status = %d, output = %q", status, stdout.String())
	}
}

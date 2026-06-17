package app

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadSecretMappings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret-env.txt")
	content := `
# Host values are referenced, not copied here.
OPENAI_API_KEY=OPENAI_API_KEY
TELEGRAM_BOT_TOKEN = HERMES_BOX_TELEGRAM_BOT_TOKEN # comment
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	mappings, err := readSecretMappings(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(mappings) != 2 {
		t.Fatalf("len(mappings) = %d", len(mappings))
	}
	if mappings[1] != "TELEGRAM_BOT_TOKEN=HERMES_BOX_TELEGRAM_BOT_TOKEN" {
		t.Fatalf("mapping = %q", mappings[1])
	}
}

func TestReadSecretMappingsRejectsInvalidNames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret-env.txt")
	if err := os.WriteFile(path, []byte("OPENAI_API_KEY=$(cat secret)\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readSecretMappings(path); err == nil {
		t.Fatal("readSecretMappings accepted shell syntax")
	}
}

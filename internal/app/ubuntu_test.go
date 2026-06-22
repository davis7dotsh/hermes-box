package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSmolfileUsesUbuntuImage(t *testing.T) {
	content, err := os.ReadFile(filepath.Join("..", "..", "Smolfile"))
	if err != nil {
		t.Fatal(err)
	}

	var imageAssignments []string
	for _, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "image = ") {
			imageAssignments = append(imageAssignments, line)
		}
	}

	want := `image = "` + ubuntuImage + `"`
	if len(imageAssignments) != 1 || imageAssignments[0] != want {
		t.Fatalf("Smolfile image assignments = %q, want [%q]", imageAssignments, want)
	}
}

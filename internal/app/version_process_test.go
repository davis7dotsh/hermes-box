package app

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersionProcessDoesNotRequireConfigOrLock(t *testing.T) {
	directory := t.TempDir()
	bin := filepath.Join(directory, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	limactl := filepath.Join(bin, "limactl")
	if err := os.WriteFile(limactl, []byte("#!/bin/sh\nprintf 'limactl version 2.1.3\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=TestVersionHelperProcess", "--", "version")
	command.Dir = directory
	command.Env = append(os.Environ(), "HERMES_BOX_VERSION_HELPER=1", "HOME="+directory, "PATH="+bin+":"+os.Getenv("PATH"))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("version process: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "Hermes Box v2-dev (Lima 2.1.3, config schema 1, lock schema 1)") {
		t.Fatalf("version output = %q", output)
	}
}

func TestVersionJSONProcessReportsUnavailableLimaAndSucceeds(t *testing.T) {
	directory := t.TempDir()
	bin := filepath.Join(directory, "empty-bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=TestVersionHelperProcess", "--", "--json", "version")
	command.Dir = directory
	command.Env = append(os.Environ(), "HERMES_BOX_VERSION_HELPER=1", "HOME="+directory, "PATH="+bin)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("JSON version process: %v\n%s", err, output)
	}
	var envelope struct {
		OK     bool `json:"ok"`
		Result struct {
			CLI          string `json:"cli"`
			Lima         string `json:"lima"`
			ConfigSchema int    `json:"config_schema"`
			LockSchema   int    `json:"lock_schema"`
		} `json:"result"`
	}
	if err := json.Unmarshal(output, &envelope); err != nil {
		t.Fatalf("decode JSON version output: %v; output %q", err, output)
	}
	if !envelope.OK || envelope.Result.CLI != "v2-dev" || envelope.Result.Lima != "unavailable" ||
		envelope.Result.ConfigSchema != 1 || envelope.Result.LockSchema != 1 {
		t.Fatalf("version envelope = %#v", envelope)
	}
}

func TestVersionHelperProcess(t *testing.T) {
	if os.Getenv("HERMES_BOX_VERSION_HELPER") != "1" {
		return
	}
	separator := 0
	for index, value := range os.Args {
		if value == "--" {
			separator = index + 1
			break
		}
	}
	cli := NewDefault(io.Reader(os.Stdin), os.Stdout, os.Stderr, os.Environ())
	os.Exit(cli.Run(context.Background(), os.Args[separator:]))
}

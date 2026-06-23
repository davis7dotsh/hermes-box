package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"

	"github.com/davis7dotsh/hermes-box/internal/config"
)

func main() {
	if len(os.Args) == 6 && os.Args[1] == "--assert-status" {
		assertStatus(os.Args[2], os.Args[3], os.Args[4], os.Args[5], os.Stdin)
		return
	}
	if len(os.Args) == 4 && os.Args[1] == "--lifecycle" {
		validateLifecycle(os.Args[2], os.Args[3])
		return
	}
	if len(os.Args) < 2 || len(os.Args) > 3 {
		fmt.Fprintln(os.Stderr, "usage: validate-lock PATH [ARTIFACT_DIR] | --lifecycle BASELINE_LOCK CANDIDATE_LOCK | --assert-status DESIRED_LOCK APPLIED_LOCK COMPONENT PREVIOUS_LOCK_OR_NONE")
		os.Exit(2)
	}
	lock, err := config.LoadLock(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if len(os.Args) == 3 {
		checks := []struct {
			name     string
			expected string
		}{
			{artifactName(lock.Ubuntu.Provisioner), lock.Ubuntu.ProvisionerSHA256},
			{artifactName(lock.Hermes.Archive), lock.Hermes.SHA256},
			{artifactName(lock.Hermes.WheelsArchive), lock.Hermes.WheelsSHA256},
		}
		for _, check := range checks {
			actual, hashErr := fileSHA256(filepath.Join(os.Args[2], check.name))
			if hashErr != nil {
				fmt.Fprintln(os.Stderr, hashErr)
				os.Exit(1)
			}
			if actual != check.expected {
				fmt.Fprintf(os.Stderr, "%s sha256 %s does not match lock %s\n", check.name, actual, check.expected)
				os.Exit(1)
			}
		}
	}
}

type statusEnvelope struct {
	Schema int  `json:"schema"`
	OK     bool `json:"ok"`
	Result struct {
		Components map[string]struct {
			Desired  string  `json:"desired"`
			Applied  string  `json:"applied"`
			Previous *string `json:"previous"`
			State    string  `json:"state"`
		} `json:"components"`
	} `json:"result"`
}

func assertStatus(desiredPath, appliedPath, selected, previousPath string, input io.Reader) {
	desired, err := config.LoadLock(desiredPath)
	if err != nil {
		fail(err)
	}
	applied, err := config.LoadLock(appliedPath)
	if err != nil {
		fail(err)
	}
	var previous *config.Lock
	if previousPath != "none" {
		lock, loadErr := config.LoadLock(previousPath)
		if loadErr != nil {
			fail(loadErr)
		}
		previous = &lock
	}
	var envelope statusEnvelope
	decoder := json.NewDecoder(io.LimitReader(input, 1<<20))
	if err := decoder.Decode(&envelope); err != nil {
		fail(fmt.Errorf("decode status JSON: %w", err))
	}
	if envelope.Schema != 1 || !envelope.OK {
		fail(fmt.Errorf("status JSON is not a successful schema-1 envelope"))
	}
	components := []string{selected}
	if selected == "all" {
		components = []string{"node", "uv", "claude", "codex", "hermes", "executor"}
	}
	for _, name := range components {
		status, ok := envelope.Result.Components[name]
		if !ok {
			fail(fmt.Errorf("status JSON omitted component %s", name))
		}
		expectedDesired := componentPin(desired, name)
		expectedApplied := componentPin(applied, name)
		if status.Desired != expectedDesired || status.Applied != expectedApplied {
			fail(fmt.Errorf("%s status pins: desired=%q applied=%q, expected desired=%q applied=%q", name, status.Desired, status.Applied, expectedDesired, expectedApplied))
		}
		if previous == nil {
			if status.Previous != nil {
				fail(fmt.Errorf("%s unexpectedly retained previous pin %q", name, *status.Previous))
			}
		} else {
			expectedPrevious := componentPin(*previous, name)
			if status.Previous == nil || *status.Previous != expectedPrevious {
				fail(fmt.Errorf("%s previous pin does not match %q", name, expectedPrevious))
			}
		}
		expectedState := "healthy"
		if expectedDesired != expectedApplied {
			expectedState = "drifted"
		}
		if status.State != expectedState {
			fail(fmt.Errorf("%s state=%q, expected %q", name, status.State, expectedState))
		}
	}
}

func componentPin(lock config.Lock, name string) string {
	switch name {
	case "node":
		return lock.Tooling.Node.Version
	case "uv":
		return lock.Tooling.UV.Version
	case "claude":
		return lock.Claude.Version
	case "codex":
		return lock.Codex.Version
	case "hermes":
		return lock.Hermes.Commit
	case "executor":
		return lock.Executor.LinuxARM64Digest
	default:
		fail(fmt.Errorf("unknown lifecycle component %q", name))
		return ""
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

func validateLifecycle(baselinePath, candidatePath string) {
	baseline, err := config.LoadLock(baselinePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	candidate, err := config.LoadLock(candidatePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := validateLifecyclePair(baseline, candidate); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func validateLifecyclePair(baseline, candidate config.Lock) error {
	if baseline.Host != candidate.Host || baseline.Ubuntu != candidate.Ubuntu {
		return fmt.Errorf("lifecycle component matrix requires identical host and Ubuntu platform locks")
	}
	differences := []struct {
		name    string
		changed bool
	}{
		{"node", baseline.Tooling.Node != candidate.Tooling.Node},
		{"uv", baseline.Tooling.UV != candidate.Tooling.UV},
		{"claude", baseline.Claude != candidate.Claude},
		{"codex", baseline.Codex != candidate.Codex},
		{"hermes", baseline.Hermes != candidate.Hermes},
		{"executor", baseline.Executor != candidate.Executor},
	}
	for _, difference := range differences {
		if !difference.changed {
			return fmt.Errorf("lifecycle candidate does not change %s", difference.name)
		}
	}
	return nil
}

func artifactName(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	return path.Base(parsed.Path)
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

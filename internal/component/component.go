package component

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

type Name string

const (
	Node     Name = "node"
	UV       Name = "uv"
	Claude   Name = "claude"
	Codex    Name = "codex"
	Hermes   Name = "hermes"
	Executor Name = "executor"
)

var dependencyOrder = []Name{Node, UV, Claude, Codex, Hermes, Executor}

var dependencies = map[Name][]Name{
	Claude: {Node},
	Hermes: {Node, UV},
}

var pinPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+:@-]{0,255}$`)

// executorImagePattern deliberately accepts a smaller subset of the OCI
// reference grammar than Podman. The value is written to an EnvironmentFile,
// so accepting whitespace, shell metacharacters, uppercase repository names,
// or a non-canonical digest would turn a reviewed image reference into guest
// configuration syntax.
var executorImagePattern = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?::[0-9]+)?(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)+(?::[A-Za-z0-9_][A-Za-z0-9._-]{0,127})?@sha256:[a-f0-9]{64}$`)

// Spec is the complete guest-side installation contract for reviewed raw
// artifacts. The guest chooses the installer and validation suite by Name.
// Kind "tree" exists only for root-prefixed unit-test fixtures.
type Spec struct {
	Name             Name             `json:"name"`
	Pin              string           `json:"pin"`
	Kind             string           `json:"kind"`
	Artifact         string           `json:"artifact,omitempty"`
	SHA256           string           `json:"sha256,omitempty"`
	Image            string           `json:"image,omitempty"`
	ImageIndexDigest string           `json:"image_index_digest,omitempty"`
	ImageChildDigest string           `json:"image_child_digest,omitempty"`
	Inputs           map[string]Input `json:"inputs,omitempty"`
	UVLockSHA256     string           `json:"uv_lock_sha256,omitempty"`
}

type Input struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

func (s Spec) Validate() error {
	if !Known(s.Name) {
		return fmt.Errorf("unknown component %q", s.Name)
	}
	if !ValidPin(s.Pin) {
		return fmt.Errorf("invalid %s pin %q", s.Name, s.Pin)
	}
	if s.Kind != "" && s.Kind != "tree" && s.Kind != "container" {
		return fmt.Errorf("%s kind is reserved for test fixture trees", s.Name)
	}
	if s.Kind == "container" {
		if s.Name != Executor || !ValidExecutorImage(s.Image) {
			return fmt.Errorf("container artifacts require an immutable executor image digest")
		}
		if s.Artifact == "" || s.SHA256 == "" {
			return fmt.Errorf("executor requires a verified OCI archive")
		}
		for label, digest := range map[string]string{"index": s.ImageIndexDigest, "linux/arm64 child": s.ImageChildDigest} {
			if !regexp.MustCompile(`^sha256:[a-f0-9]{64}$`).MatchString(digest) {
				return fmt.Errorf("executor %s digest is required", label)
			}
		}
		if !strings.HasSuffix(s.Image, "@"+s.ImageChildDigest) {
			return fmt.Errorf("executor runtime image must use the reviewed linux/arm64 child digest")
		}
	} else if s.Artifact == "" {
		return fmt.Errorf("%s artifact is required", s.Name)
	}
	if !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(s.SHA256) {
		return fmt.Errorf("%s sha256 is required", s.Name)
	}
	if s.Name == Hermes {
		if len(s.Inputs) != 2 {
			return fmt.Errorf("hermes requires exactly python and wheels inputs")
		}
		for _, key := range []string{"python", "wheels"} {
			input, ok := s.Inputs[key]
			if !ok || input.Path == "" || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(input.SHA256) {
				return fmt.Errorf("hermes requires verified %s input", key)
			}
		}
		if !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(s.UVLockSHA256) {
			return fmt.Errorf("hermes uv_lock_sha256 is required")
		}
	} else if len(s.Inputs) != 0 || s.UVLockSHA256 != "" {
		return fmt.Errorf("%s does not accept Hermes installation inputs", s.Name)
	}
	if s.Name == Executor && s.Kind != "container" {
		return fmt.Errorf("executor requires kind container")
	}
	if s.Name != Executor && s.Kind == "container" {
		return fmt.Errorf("only executor accepts kind container")
	}
	return nil
}

func ValidPin(pin string) bool {
	return pinPattern.MatchString(pin) && !strings.Contains(pin, "..")
}

func ValidExecutorImage(image string) bool {
	return executorImagePattern.MatchString(image)
}

func SnapshotPaths(name Name) []string {
	switch name {
	case Claude:
		return []string{"home/agent/.claude"}
	case Codex:
		return []string{"home/agent/.codex"}
	case Hermes, UV:
		return []string{"home/agent/.hermes"}
	case Node:
		return []string{"home/agent/.claude", "home/agent/.hermes"}
	case Executor:
		return []string{"executor"}
	default:
		return nil
	}
}

func Known(name Name) bool {
	for _, candidate := range dependencyOrder {
		if candidate == name {
			return true
		}
	}
	return false
}

func Dependencies(name Name) []Name {
	return append([]Name(nil), dependencies[name]...)
}

// Sort validates a component set, rejects duplicates, and returns it in the
// only supported transaction order.
func Sort(specs []Spec) ([]Spec, error) {
	byName := make(map[Name]Spec, len(specs))
	for _, spec := range specs {
		if err := spec.Validate(); err != nil {
			return nil, err
		}
		if _, exists := byName[spec.Name]; exists {
			return nil, fmt.Errorf("duplicate component %q", spec.Name)
		}
		byName[spec.Name] = spec
	}
	ordered := make([]Spec, 0, len(specs))
	for _, name := range dependencyOrder {
		if spec, ok := byName[name]; ok {
			ordered = append(ordered, spec)
		}
	}
	return ordered, nil
}

func Names() []Name {
	return append([]Name(nil), dependencyOrder...)
}

func SortedNames(values map[Name]string) []Name {
	names := make([]Name, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })
	return names
}

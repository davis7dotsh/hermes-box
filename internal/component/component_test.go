package component

import (
	"strings"
	"testing"
)

func TestSortUsesDependencyOrder(t *testing.T) {
	t.Parallel()
	specs, err := Sort([]Spec{
		{Name: Hermes, Pin: "abc", Kind: "tree", Artifact: "/tmp/hermes", SHA256: strings.Repeat("a", 64), Inputs: map[string]Input{"python": {Path: "/tmp/python", SHA256: strings.Repeat("d", 64)}, "wheels": {Path: "/tmp/wheels", SHA256: strings.Repeat("e", 64)}}, UVLockSHA256: strings.Repeat("f", 64)},
		{Name: Node, Pin: "24.0.0", Kind: "tree", Artifact: "/tmp/node", SHA256: strings.Repeat("b", 64)},
		{Name: UV, Pin: "0.1.0", Artifact: "/tmp/uv", SHA256: strings.Repeat("c", 64)},
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []Name{Node, UV, Hermes} {
		if specs[i].Name != want {
			t.Fatalf("spec %d = %q, want %q", i, specs[i].Name, want)
		}
	}
}

func TestSpecRejectsMutableContainerImage(t *testing.T) {
	t.Parallel()
	err := (Spec{Name: Executor, Pin: "1", Kind: "container", Artifact: "/tmp/e.tar", SHA256: strings.Repeat("a", 64), Image: "example/executor:latest"}).Validate()
	if err == nil {
		t.Fatal("expected mutable image to be rejected")
	}
}

func TestSpecRejectsExecutorEnvironmentFileInjection(t *testing.T) {
	t.Parallel()
	digest := "sha256:" + strings.Repeat("b", 64)
	base := Spec{
		Name: Executor, Pin: "1", Kind: "container", Artifact: "/tmp/e.tar",
		SHA256: strings.Repeat("a", 64), ImageIndexDigest: "sha256:" + strings.Repeat("c", 64),
		ImageChildDigest: digest,
	}
	for _, image := range []string{
		"example/executor@" + digest + "\nINJECTED=value",
		"example/Executor@" + digest,
		"example/executor@" + digest + " ",
		"example/executor:latest",
	} {
		candidate := base
		candidate.Image = image
		if err := candidate.Validate(); err == nil {
			t.Fatalf("unsafe executor image %q was accepted", image)
		}
	}
}

func TestSnapshotScopesAreComponentSpecific(t *testing.T) {
	t.Parallel()
	if got := SnapshotPaths(Node); len(got) != 2 || got[0] != "home/agent/.claude" || got[1] != "home/agent/.hermes" {
		t.Fatalf("node snapshot paths = %#v", got)
	}
	if got := SnapshotPaths(Executor); len(got) != 1 || got[0] != "executor" {
		t.Fatalf("executor snapshot paths = %#v", got)
	}
}

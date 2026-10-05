package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestTeardownVolumeHelperFeedsNativeDeletionLoop(t *testing.T) {
	root := testkit.TempDir(t)
	helper, err := filepath.Abs("../../scripts/lib/teardown-plan.py")
	if err != nil {
		t.Fatal(err)
	}
	resources := []ports.TeardownResource{
		{Kind: "volume", Pool: "approved-pool", Name: "yard-srv", Binding: strings.Repeat("a", 64)},
		{Kind: "volume", Pool: "other-pool", Name: "second", Binding: strings.Repeat("b", 64)},
		{Kind: "profile", Name: "default", Binding: strings.Repeat("c", 64)},
	}
	payload, err := json.Marshal(resources)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("python3", helper, "volumes")
	command.Env = append(os.Environ(), "SUBYARD_TEARDOWN_INVENTORY="+string(payload))
	output, err := command.CombinedOutput()
	if err != nil || string(output) != "approved-pool\tyard-srv\nother-pool\tsecond\n" {
		t.Fatalf("native no-argument volumes command: %q, %v", output, err)
	}
	// Exercise the shipped loop, rather than a second implementation of its
	// argument parsing. The fixture Incus deletes real file-backed volumes.
	script, err := os.ReadFile("../../scripts/teardown-physical.sh")
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(script), "    while IFS=$'\\t' read -r approved_pool approved_volume; do")
	endText := "done < <(python3 \"$TEARDOWN_GUARD\" volumes)"
	end := strings.Index(string(script), endText)
	if start < 0 || end < start {
		t.Fatal("native volume deletion loop not found")
	}
	for _, path := range []string{"approved-pool/yard-srv", "other-pool/second", "approved-pool/unapproved"} {
		directory := filepath.Join(root, filepath.Dir(path))
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(directory, 0700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, filepath.Join(root, path), []byte("native volume"), 0600)
	}
	setup := `set -euo pipefail
PROJ=(--project fixture)
guard_resource() { [ "$1" = volume ]; }
ok() { :; }
die() { exit 9; }
incus() {
  [ "$1" = storage ] && [ "$2" = volume ] && [ "$3" = delete ]
  rm -- "$FIXTURE_ROOT/$4/$5"
}
`
	command = exec.Command("bash", "-c", setup+string(script)[start:end+len(endText)])
	command.Env = append(os.Environ(), "SUBYARD_TEARDOWN_INVENTORY="+string(payload), "TEARDOWN_GUARD="+helper, "FIXTURE_ROOT="+root)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("native deletion loop: %s, %v", output, err)
	}
	for _, path := range []string{"approved-pool/yard-srv", "other-pool/second"} {
		if _, err := os.Stat(filepath.Join(root, path)); !os.IsNotExist(err) {
			t.Fatalf("approved volume remains: %s (%v)", path, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "approved-pool/unapproved")); err != nil {
		t.Fatalf("unapproved volume was not retained: %v", err)
	}
}

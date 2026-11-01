package cli

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/releaseowner"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestNativeOwnerObservationPreservesPreparedHandles(t *testing.T) {
	for _, state := range []string{"absent", "current", "legacy-directory", "ambiguous"} {
		t.Run(state, func(t *testing.T) {
			c, home := nativeActivationSourceFixture(t)
			tool := testkit.TempDir(t)
			log := filepath.Join(tool, "calls")
			projects := "[]"
			if state == "legacy-directory" {
				projects = `[{"name":"subyard-e2e-yard"}]`
			}
			script := "#!/bin/sh\nset -eu\nprintf '%s\\n' \"$*\" >>'" + log + "'\ncase \"$*\" in\n 'project list --format=json') echo '" + projects + "';;\n 'project get subyard-e2e-yard features.images') echo false;;\n 'list yard-e2e-yard --project subyard-e2e-yard --format=json') echo '[{\"name\":\"yard-e2e-yard\",\"status\":\"STOPPED\"}]';;\n 'config get yard-e2e-yard user.subyard.desired_power --project subyard-e2e-yard') echo running;;\n *) exit 97;;\nesac\n"
			testkit.WriteFile(t, filepath.Join(tool, "incus"), []byte(script), 0700)
			t.Setenv("PATH", tool+":"+os.Getenv("PATH"))
			c.cli.baseEnv["PATH"] = os.Getenv("PATH")
			if state != "absent" {
				yard := "e2e-yard"
				if state == "current" {
					yard = "test-yard"
				}
				if err := os.MkdirAll(filepath.Join(home, "yards", yard), 0o700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, filepath.Join(home, "yards", yard, "config.env"), []byte("YARD_TEMPLATE=test-vms\nSSH_PORT=2223\n"), 0600)
				if state == "ambiguous" {
					if err := os.MkdirAll(filepath.Join(home, "yards/test-yard"), 0o700); err != nil {
						t.Fatal(err)
					}
					testkit.WriteFile(t, filepath.Join(home, "yards/test-yard/config.env"), []byte("YARD_TEMPLATE=test-vms\nSSH_PORT=2223\n"), 0600)
				}
			}
			original := &preparedOwnerChildren{}
			c.ownerChildren = original
			complete := &releaseowner.CompleteSourceReference{Transaction: "original", JournalDigest: strings.Repeat("a", 64), MetadataBinding: strings.Repeat("b", 64)}
			c.completeSource = complete
			c.publicationPlans = map[string]string{"retained": "original-generation"}
			before, err := operationReplayFiles(c.context.RepositoryRoot)
			if err != nil {
				t.Fatal(err)
			}
			observed, err := c.CaptureOwnerObservation(context.Background())
			if state == "ambiguous" {
				if err == nil {
					t.Fatal("ambiguous registration accepted")
				}
			} else if err != nil || observed.State != state || observed.PreparedRecipe != "" {
				t.Fatalf("observation state=%s err=%v", observed.State, err)
			}
			after, e := operationReplayFiles(c.context.RepositoryRoot)
			if e != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("observation changed source/state bytes", e)
			}
			if c.ownerChildren != original || c.completeSource != complete || len(c.publicationPlans) != 1 {
				t.Fatal("observation replaced prepared session handles")
			}
			if generation, ok := c.publicationPlans["retained"]; !ok || generation != "original-generation" {
				t.Fatal("publication preparation replaced")
			}
			if len(c.prepared) != 0 || len(c.activation) != 0 {
				t.Fatal("observation prepared children")
			}
			calls, _ := os.ReadFile(log)
			for _, line := range strings.Split(strings.TrimSpace(string(calls)), "\n") {
				if line != "" && !strings.HasPrefix(line, "project list ") && !strings.HasPrefix(line, "project get ") && !strings.HasPrefix(line, "list yard-") && !strings.HasPrefix(line, "config get ") {
					t.Fatal("unexpected Incus effect/prepare", line)
				}
			}
		})
	}
}

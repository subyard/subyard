//go:build linux

package reconcileruntime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestObserveProfileRuntimeExecutesPinnedCandidateAfterPathReplacement(t *testing.T) {
	for _, process := range []string{"self", fmt.Sprint(os.Getpid())} {
		t.Run(process, func(t *testing.T) {
			parent := testkit.TempDir(t)
			root := filepath.Join(parent, "release")
			runtime := profileRuntimeFixtureAt(t, root, "#!/bin/sh\nprintf '%s\\n' '{\"state\":\"absent\",\"actual\":\"\",\"desired\":\"\"}'\n")
			pin, err := os.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			defer pin.Close()
			runtime.RepositoryRoot = fmt.Sprintf("/proc/%s/fd/%d", process, pin.Fd())
			runtime.Profiles, err = profile.Load(runtime.RepositoryRoot)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(root, root+"-retained"); err != nil {
				t.Fatal(err)
			}
			// A replacement at the original pathname must never execute.
			profileRuntimeFixtureAt(t, root, "#!/bin/sh\nexit 91\n")
			observations, err := runtime.ObserveProfileRuntimes(context.Background())
			if err != nil || observations["synthetic-runtime"].State != ports.RuntimeStateAbsent {
				t.Fatalf("pinned runtime observation = %#v, %v", observations, err)
			}
		})
	}
}

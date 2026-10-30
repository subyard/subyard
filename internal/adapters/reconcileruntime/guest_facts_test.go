package reconcileruntime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestGuestFactsUsesEngineVersionAndOwnEffectiveSelection(t *testing.T) {
	root := testkit.TempDir(t)
	runtime := Runtime{RepositoryRoot: root, Yard: domain.Context{YardName: "own", IncusProject: "own-project", YardInstanceName: "own-yard"}, Environment: []string{"YARD_VERSION=0.22.0-exact", "ENVIRONMENT_PROFILES="}}
	for _, profiles := range []string{"", "synthetic another", "fixture_profile fixture--profile fixture- 9"} {
		runtime.Environment[1] = "ENVIRONMENT_PROFILES=" + profiles
		var snapshot struct {
			Version  string
			Yard     map[string]string
			Profiles []string `json:"selected_profiles"`
		}
		if err := json.Unmarshal(runtime.guestRuntimeFacts(), &snapshot); err != nil {
			t.Fatal(err)
		}
		if snapshot.Version != "0.22.0-exact" || snapshot.Yard["name"] != "own" || snapshot.Yard["project"] != "own-project" || snapshot.Yard["instance"] != "own-yard" || snapshot.Profiles == nil || strings.Join(snapshot.Profiles, " ") != strings.Join(strings.Fields(profiles), " ") {
			t.Fatalf("wrong snapshot: %#v", snapshot)
		}
	}
	testkit.WriteFile(t, filepath.Join(root, "refresh-installed-guest-runtime.sh"), []byte("#!/bin/sh\nprintf '%s' \"$SUBYARD_GUEST_RUNTIME_FACTS\"\n"), 0o755)
	runtime.Environment = append(runtime.Environment, "SUBYARD_GUEST_RUNTIME_FACTS=foreign")
	// The physical launch boundary overwrites caller-supplied facts from its typed snapshot.
	if err := os.MkdirAll(filepath.Join(root, filepath.Dir(guestFactsSource)), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, guestFactsSource), []byte("fixture"), 0o600)
	var output strings.Builder
	if err := runtime.runPath(context.Background(), &output, filepath.Join(root, "refresh-installed-guest-runtime.sh")); err != nil {
		t.Fatal(err)
	}
	if output.String() != string(runtime.guestRuntimeFacts()) {
		t.Fatalf("unbound facts: %q", output.String())
	}
}

func TestGuestFactsReadinessDistinguishesSnapshotDriftAndExecutionFailure(t *testing.T) {
	runtime := Runtime{RepositoryRoot: "../../..", Yard: domain.Context{IncusProject: "own-project", YardInstanceName: "own-yard"}}
	for _, exit := range []int{0, 1, 10, 2, 127} {
		incus := &testkit.Incus{ExecSteps: []testkit.IncusExecStep{{Result: ports.InstanceExecResult{ExitCode: exit}}}}
		runtime.Executor = incus
		ready, err := runtime.guestFactsConverged(context.Background())
		if ready != (exit == 0) || (err != nil) != (exit != 0 && exit != 1 && exit != 10) {
			t.Fatalf("exit=%d ready=%v err=%v", exit, ready, err)
		}
		call := incus.ExecCalls[0]
		if call.Project != "own-project" || call.Name != "own-yard" || string(call.Request.Stdin) != string(runtime.guestRuntimeFacts()) {
			t.Fatalf("wrong guest facts binding: %#v", call)
		}
	}
}

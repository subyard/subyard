package reconcileruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/incusclient"
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
	for _, exit := range []int{0, 1, 10, 11, 2, 127} {
		t.Run(fmt.Sprint(exit), func(t *testing.T) {
			server, err := testkit.NewIncusServer(testkit.TempDir(t))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = server.Close() })
			server.QueueExec(testkit.IncusServerExecStep{ExitCode: exit})
			runtime.Executor = incusclient.New(server.SocketPath)
			ready, err := runtime.guestFactsConverged(context.Background())
			if ready != (exit == 0 || exit == 11) || (err != nil) != (exit != 0 && exit != 1 && exit != 10 && exit != 11) {
				t.Fatalf("exit=%d ready=%v err=%v", exit, ready, err)
			}
			call := server.ExecCalls()[0]
			if call.Project != "own-project" || call.Name != "own-yard" || string(call.Stdin) != string(runtime.guestRuntimeFacts()) {
				t.Fatalf("wrong guest facts binding: %#v", call)
			}
		})
	}
}

func TestGuestFactsReadinessPreservesExecutionErrors(t *testing.T) {
	transportErr := errors.New("transport disconnected")
	for _, scenario := range []struct {
		name string
		exit int
		err  error
	}{
		{"transport", 0, transportErr},
		{"transport-missing", 1, transportErr},
		{"transport-drift", 10, transportErr},
		{"transport-profiles", 11, transportErr},
		{"mismatched-certified-exit", 11, &ports.GuestExitError{Code: 10}},
		{"unexpected-exit", 2, transportErr},
		{"canceled-drift", 10, fmt.Errorf("wait: %w", context.Canceled)},
		{"canceled-profiles", 11, fmt.Errorf("wait: %w", context.Canceled)},
		{"deadline-profiles", 11, fmt.Errorf("wait: %w", context.DeadlineExceeded)},
		{"deadline-missing", 1, fmt.Errorf("wait: %w", context.DeadlineExceeded)},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			runtime := Runtime{RepositoryRoot: "../../..", Executor: &testkit.Incus{ExecSteps: []testkit.IncusExecStep{{Result: ports.InstanceExecResult{ExitCode: scenario.exit}, Err: scenario.err}}}}
			ready, err := runtime.guestFactsConverged(context.Background())
			if ready || !errors.Is(err, scenario.err) {
				t.Fatalf("ready=%v err=%v; want execution error %v", ready, err, scenario.err)
			}
		})
	}
}

func TestGuestFactsProfilesPendingIsReadyWithoutHidingOtherDrift(t *testing.T) {
	for _, exit := range []int{0, 1, 10, 11, 12} {
		t.Run(fmt.Sprint(exit), func(t *testing.T) {
			var nativeErr error
			if exit != 0 {
				nativeErr = &ports.GuestExitError{Code: exit}
			}
			guest := &testkit.Incus{ExecSteps: []testkit.IncusExecStep{{Result: ports.InstanceExecResult{ExitCode: exit}, Err: nativeErr}}}
			runtime := Runtime{RepositoryRoot: "../../..", Executor: guest}
			ready, err := runtime.guestFactsConverged(context.Background())
			if ready != (exit == 0 || exit == 11) || (err != nil) != (exit == 12) {
				t.Fatalf("exit=%d ready=%v err=%v", exit, ready, err)
			}
		})
	}
}

func TestInstalledGuestFactsSelectionPendingKeepsReadinessAndNativeBinding(t *testing.T) {
	previewLoopbackFixture(t)
	root := testkit.TempDir(t)
	program, err := os.ReadFile(filepath.Join("../../..", guestFactsSource))
	if err != nil {
		t.Fatal(err)
	}
	for name, payload := range map[string][]byte{
		guestFactsSource:                             program,
		"config/preview/subyard-preview":             []byte("preview"),
		"config/agents/ccusage/provision.sh":         []byte("usage"),
		"scripts/refresh-installed-guest-runtime.sh": []byte("refresh"),
	} {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, path, payload, 0o600)
	}
	guest := &testkit.Incus{Instances: map[string]ports.InstanceInfo{"fixture/yard": {
		Status: "Running", Config: map[string]string{"user.subyard.preview_sha256": "owned"},
	}}}
	runtime := Runtime{RepositoryRoot: root, Incus: guest, Executor: guest,
		Yard: domain.Context{YardName: "synthetic", IncusProject: "fixture", YardInstanceName: "yard"},
	}
	observe := func(selection string, exit int) ports.RuntimeObservation {
		t.Helper()
		runtime.Environment = []string{"ALLOWS_CODING_TOOLS=false", "YARD_VERSION=0.29.0-test", "ENVIRONMENT_PROFILES=" + selection}
		var nativeErr error
		if exit != 0 {
			nativeErr = &ports.GuestExitError{Code: exit}
		}
		guest.ExecSteps = []testkit.IncusExecStep{
			{Result: ports.InstanceExecResult{Stdout: []byte("unchanged guest file metadata")}},
			{}, // Preview remains current; the last result is the facts check.
			{Result: ports.InstanceExecResult{ExitCode: exit}, Err: nativeErr},
		}
		observation, err := runtime.ObserveInstalledRuntime(context.Background(), "core-guest-runtime")
		if err != nil {
			t.Fatal(err)
		}
		return observation
	}
	before, after := observe("synthetic", 0), observe("", 11)
	if before.State != ports.RuntimeStateCurrent || after.State != ports.RuntimeStateCurrent ||
		after.Actual != after.Desired || before.Desired == after.Desired {
		t.Fatalf("selection readiness or native consent binding lost: before=%+v after=%+v", before, after)
	}
	for _, exit := range []int{1, 10} {
		if observed := observe("", exit); observed.State != ports.RuntimeStateStale || observed.Actual == observed.Desired {
			t.Fatalf("facts exit %d suppressed real drift: %+v", exit, observed)
		}
	}
}

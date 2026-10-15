package reconcileruntime

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestInstalledRuntimeAbsentAndStoppedSkipGuestAndKeysBootstrap(t *testing.T) {
	for _, state := range []string{"absent", "Stopped", "Running"} {
		t.Run(state, func(t *testing.T) {
			root := testkit.TempDir(t)
			incus := &testkit.Incus{Instances: map[string]ports.InstanceInfo{}}
			if state != "absent" {
				incus.Instances["p/i"] = ports.InstanceInfo{Status: state}
			}
			runtime := Runtime{Incus: incus, Executor: incus, Yard: domain.Context{IncusProject: "p", YardInstanceName: "i", Paths: domain.RuntimePaths{DataHome: root, OperatorHome: root}}}
			guest, err := runtime.ObserveInstalledRuntime(context.Background(), "core-guest-runtime")
			wanted := ports.RuntimeStateAbsent
			if state == "Stopped" {
				wanted = ports.RuntimeStateDeferred
			}
			if err != nil || guest.State != wanted || len(incus.ExecCalls) != 0 {
				t.Fatalf("unexpected installation effects: %#v %v calls=%#v", guest, err, incus.ExecCalls)
			}
			if err := runtime.ApplyInstalledRuntime(context.Background(), "core-guest-runtime", "test", testRuntimeOldDigest, testRuntimeNewDigest); !errors.Is(err, domain.ErrPlanStale) {
				t.Fatalf("unavailable apply = %v", err)
			}
		})
	}
}

func TestInstalledGuestMarkerWithMissingHelperIsStaleAndRejectsChangedAssessment(t *testing.T) {
	previewLoopbackFixture(t)
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	private := testkit.TempDir(t)
	incus := &testkit.Incus{Instances: map[string]ports.InstanceInfo{"p/i": {Status: "Running", Config: map[string]string{"user.subyard.preview_sha256": "owned"}}}}
	prepare := func(payload string) {
		incus.ExecSteps = []testkit.IncusExecStep{
			{Result: ports.InstanceExecResult{Stdout: []byte(payload)}},
			{Result: ports.InstanceExecResult{ExitCode: 1}, Err: errors.New("fixture helper missing")},
		}
	}
	runtime := Runtime{RepositoryRoot: root, Incus: incus, Executor: incus, Environment: []string{"ALLOWS_CODING_TOOLS=false"}, Yard: domain.Context{IncusProject: "p", YardInstanceName: "i", Paths: domain.RuntimePaths{DataHome: private, OperatorHome: private}}}
	prepare("managed helper absent\n")
	guest, err := runtime.ObserveInstalledRuntime(context.Background(), "core-guest-runtime")
	if err != nil || guest.State != ports.RuntimeStateStale {
		t.Fatalf("missing owned helper hidden: %#v %v", guest, err)
	}
	prepare("changed managed helper\n")
	if err := runtime.ApplyInstalledRuntime(context.Background(), "core-guest-runtime", "op", guest.Actual, guest.Desired); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("changed helper authorized: %v", err)
	}
	if len(incus.ExecCalls) != 4 {
		t.Fatalf("stale apply performed extra effects: %v", incus.ExecCalls)
	}
	if !strings.Contains(incus.ExecCalls[0].Request.Command[3], "stat") {
		t.Fatal("actual identity did not inspect installed artifacts")
	}
}

func TestInstalledGuestCCUsageDriftRefreshAndNativeFailures(t *testing.T) {
	previewLoopbackFixture(t)
	root := testkit.TempDir(t)
	marker := filepath.Join(root, "refreshed")
	for name, payload := range map[string]string{
		"config/preview/subyard-preview":             "preview",
		"config/agents/ccusage/provision.sh":         "usage",
		"scripts/refresh-installed-guest-runtime.sh": "#!/bin/sh\nset -eu\n[ \"$1\" = --yes ]\nprintf refreshed >> \"$REFRESH_MARKER\"\n",
	} {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, path, []byte(payload), 0o755)
	}
	incus := &testkit.Incus{Instances: map[string]ports.InstanceInfo{"p/i": {Status: "Running", Config: map[string]string{"user.subyard.ccusage_version": "owned"}}}}
	policy := &networkPolicyFixture{}
	runtime := Runtime{RepositoryRoot: root, Incus: incus, Executor: incus, NetworkPolicy: policy,
		Environment: []string{"ALLOWS_CODING_TOOLS=true", "REFRESH_MARKER=" + marker},
		Yard:        domain.Context{IncusProject: "p", YardInstanceName: "i"}}
	steps := func(files string, result ports.InstanceExecResult, err error) []testkit.IncusExecStep {
		return []testkit.IncusExecStep{
			{Result: ports.InstanceExecResult{Stdout: []byte(files)}},
			{}, // preview is current; only the usage check reports drift
			{Result: result, Err: err},
		}
	}
	drift := func(files string) []testkit.IncusExecStep {
		return steps(files, ports.InstanceExecResult{Stdout: []byte("ccusage-stale\n")}, nil)
	}
	current := ports.InstanceExecResult{Stdout: []byte("ccusage-current\n")}
	ctx := context.Background()
	incus.ExecSteps = drift("legacy binary without receipt")
	before, err := runtime.ObserveInstalledRuntime(ctx, "core-guest-runtime")
	if err != nil || before.State != ports.RuntimeStateStale {
		t.Fatalf("completed usage drift unavailable: %#v %v", before, err)
	}
	check := incus.ExecCalls[len(incus.ExecCalls)-1].Request.Command
	for _, code := range []int{0, 1, 2, 127} {
		command := exec.Command(check[0], check[1:]...)
		command.Stdin = strings.NewReader("test \"$1\" = --check || exit 99\nprintf 'inner output\\n'\nexit " + strconv.Itoa(code) + "\n")
		output, err := command.Output()
		expected := "ccusage-current\n"
		if code == 1 {
			expected = "ccusage-stale\n"
		}
		if code <= 1 {
			if err != nil || string(output) != expected {
				t.Fatalf("completed check %d: %q %v", code, output, err)
			}
		} else {
			var failure *exec.ExitError
			if !errors.As(err, &failure) || failure.ExitCode() != code || len(output) != 0 {
				t.Fatalf("failed check %d: %q %v", code, output, err)
			}
		}
	}
	incus.ExecSteps = drift("changed legacy binary")
	if err := runtime.ApplyInstalledRuntime(ctx, "core-guest-runtime", "op", before.Actual, before.Desired); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("changed usage assessment authorized: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) || len(policy.ensured) != 0 {
		t.Fatal("stale assessment refreshed helpers or networking")
	}
	incus.ExecSteps = append(drift("legacy binary without receipt"), steps("verified binary and receipt", current, nil)...)
	if err := runtime.ApplyInstalledRuntime(ctx, "core-guest-runtime", "op", before.Actual, before.Desired); err != nil {
		t.Fatal(err)
	}
	incus.ExecSteps = steps("verified binary and receipt", current, nil)
	after, err := runtime.ObserveInstalledRuntime(ctx, "core-guest-runtime")
	if err != nil || after.State != ports.RuntimeStateCurrent || after.Desired != before.Desired {
		t.Fatalf("usage refresh not verified: %#v %v", after, err)
	}
	incus.ExecSteps = steps("verified binary and receipt", current, nil)
	if err := runtime.ApplyInstalledRuntime(ctx, "core-guest-runtime", "op", before.Actual, before.Desired); err != nil {
		t.Fatal(err)
	}
	if payload, err := os.ReadFile(marker); err != nil || string(payload) != "refreshed" {
		t.Fatalf("refresh was skipped or repeated: %q %v", payload, err)
	}
	for _, scenario := range []struct {
		name string
		code int
		err  error
		out  string
	}{
		{"transport", 0, errors.New("transport unavailable"), ""},
		{"operation-error-with-exit-one", 1, errors.New("wait for instance command failed"), "ccusage-stale\n"},
		{"other-exit", 2, errors.New("instance command exited with status 2"), ""},
		{"nonzero-without-error", 2, nil, "ccusage-stale\n"},
		{"missing-result", 0, nil, ""},
		{"unexpected-output", 0, nil, "unexpected\nccusage-stale\n"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			incus.ExecSteps = steps("files", ports.InstanceExecResult{ExitCode: scenario.code, Stdout: []byte(scenario.out)}, scenario.err)
			if _, err := runtime.ObserveInstalledRuntime(ctx, "core-guest-runtime"); err == nil {
				t.Fatal("failed usage observation reported readiness")
			}
		})
	}
}

func TestInstalledKeyLeavesPreserveAbsentToolsAndTimer(t *testing.T) {
	root := testkit.TempDir(t)
	runtime := Runtime{Yard: domain.Context{Paths: domain.RuntimePaths{DataHome: root, OperatorHome: root}}}
	if len(runtime.installedKeyLeaves()) != 0 {
		t.Fatal("empty installation acquired leaves")
	}
	tool := filepath.Join(root, "tools/bin/age")
	if err := os.MkdirAll(filepath.Dir(tool), 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, tool, []byte("public tool"), 0755)
	if wanted := []string{"install-key-tools.sh"}; !reflect.DeepEqual(runtime.installedKeyLeaves(), wanted) {
		t.Fatalf("tools bootstrap timer: %v", runtime.installedKeyLeaves())
	}
	unit := filepath.Join(root, ".config/systemd/user/subyard-keys-sync.timer")
	if err := os.MkdirAll(filepath.Dir(unit), 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, unit, []byte("timer"), 0644)
	if wanted := []string{"install-key-tools.sh", "install-keys-auto-sync.sh"}; !reflect.DeepEqual(runtime.installedKeyLeaves(), wanted) {
		t.Fatalf("installed leaves = %v", runtime.installedKeyLeaves())
	}
}

func TestInstalledRuntimeFileIdentityBindsAtomicReplacement(t *testing.T) {
	path := filepath.Join(testkit.TempDir(t), "helper")
	testkit.WriteFile(t, path, []byte("public executable"), 0755)
	original, err := installedRuntimeFile(path)
	if err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, path, []byte("changed executable"), 0755)
	changed, err := installedRuntimeFile(path)
	if err != nil || installedRuntimeDigest(original) == installedRuntimeDigest(changed) {
		t.Fatalf("changed public helper not bound: %v", err)
	}
}

func TestInstalledHostKeyRuntimeDoesNotRequireIncus(t *testing.T) {
	root := testkit.TempDir(t)
	for _, path := range []string{"scripts/install-key-tools.sh", "scripts/install-keys-auto-sync.sh", "config/systemd/subyard-keys-sync.service.in", "config/systemd/subyard-keys-sync.timer.in"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, path)), 0700); err != nil {
			t.Fatal(err)
		}
		testkit.WriteFile(t, filepath.Join(root, path), []byte("#!/bin/sh\nexit 1\n"), 0700)
	}
	if err := os.MkdirAll(filepath.Join(root, "tools/bin"), 0700); err != nil {
		t.Fatal(err)
	}
	runtime := Runtime{RepositoryRoot: root, Yard: domain.Context{Paths: domain.RuntimePaths{DataHome: root, OperatorHome: root}}}
	presence, err := runtime.InstalledRuntimePresence(context.Background(), "key-tools-runtime")
	if err != nil || !presence.KeyTools || presence.KeyTimer {
		t.Fatalf("host applicability needs guest: %#v %v", presence, err)
	}
	state, err := runtime.ObserveInstalledRuntime(context.Background(), "key-tools-runtime")
	if err != nil || state.State != ports.RuntimeStateStale {
		t.Fatalf("host tools skipped without guest: %#v %v", state, err)
	}
}

func TestInstalledStoppedGuestPresenceIsConservativeAndBound(t *testing.T) {
	incus := &testkit.Incus{Instances: map[string]ports.InstanceInfo{"p/i": {Status: "Stopped"}}}
	runtime := Runtime{Incus: incus, Yard: domain.Context{IncusProject: "p", YardInstanceName: "i"}}
	before, err := runtime.InstalledRuntimePresence(context.Background(), "core-guest-runtime")
	if err != nil || !before.Preview || !before.CCUsage || len(incus.ExecCalls) != 0 {
		t.Fatalf("stopped unknown markers treated as absent or woken: %#v %v", before, err)
	}
	incus.Instances["p/i"] = ports.InstanceInfo{Status: "Stopped", Config: map[string]string{"user.subyard.preview_sha256": "changed-owned-marker"}}
	after, err := runtime.InstalledRuntimePresence(context.Background(), "core-guest-runtime")
	if err != nil || before.Actual == after.Actual {
		t.Fatalf("stopped marker metadata not bound: %v", err)
	}
}

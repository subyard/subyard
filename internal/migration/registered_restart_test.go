package migration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
	"github.com/Subyard/Subyard/internal/yardnetwork"
)

type restartNative struct {
	*testkit.Incus
	root string
}

func (native *restartNative) Instance(ctx context.Context, project, name string) (ports.InstanceInfo, error) {
	if err := ctx.Err(); err != nil {
		return ports.InstanceInfo{}, err
	}
	instance, err := native.Incus.Instance(ctx, project, name)
	if err != nil {
		return instance, err
	}
	status, err := os.ReadFile(filepath.Join(native.root, "status"))
	instance.Status = strings.TrimSpace(string(status))
	return instance, err
}
func (native *restartNative) ReconcileState(ctx context.Context, project, name, _, _, _ string) (ports.ReconcileState, error) {
	instance, err := native.Instance(ctx, project, name)
	return ports.ReconcileState{InstanceFound: err == nil, Instance: instance}, err
}
func (native *restartNative) Exec(_ context.Context, _, _ string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	action := request.Command[len(request.Command)-1]
	if action == "apply" {
		if err := os.WriteFile(filepath.Join(native.root, "policy"), []byte("installed"), 0600); err != nil {
			return ports.InstanceExecResult{}, err
		}
	}
	if action == "check" {
		if _, err := os.Stat(filepath.Join(native.root, "policy")); errors.Is(err, os.ErrNotExist) {
			return ports.InstanceExecResult{ExitCode: 1}, &ports.GuestExitError{Code: 1}
		}
		if _, err := os.Stat(filepath.Join(native.root, "memory")); err == nil {
			return ports.InstanceExecResult{ExitCode: 10}, &ports.GuestExitError{Code: 10}
		}
	}
	return ports.InstanceExecResult{}, nil
}
func (native *restartNative) AdoptRegisteredInstance(_ context.Context, before ports.InstanceInfo, values map[string]string) error {
	current := native.Instances[before.Project+"/"+before.Name]
	for key, value := range values {
		current.Config[key] = value
		current.LocalConfig[key] = value
	}
	native.Instances[before.Project+"/"+before.Name] = current
	return nil
}

type restartNetwork struct{}

func (restartNetwork) Check(context.Context, yardnetwork.Yard) error  { return nil }
func (restartNetwork) Ensure(context.Context, yardnetwork.Yard) error { return nil }
func (restartNetwork) WithStart(_ context.Context, _ yardnetwork.Yard, start func() error) error {
	return start()
}

func writeConvergedHostNetwork(t *testing.T, root string) {
	t.Helper()
	testkit.WriteFile(t, filepath.Join(root, "scripts/06-network.sh"), []byte("#!/bin/sh\nexit 0\n"), 0700)
}

func TestGuestTmpPreparesHostBeforeStopping(t *testing.T) {
	for _, test := range []struct {
		name    string
		repair  int
		stopped bool
	}{{"repaired", 0, true}, {"refused", 3, false}} {
		t.Run(test.name, func(t *testing.T) {
			root := testkit.TempDir(t)
			if err := os.Mkdir(filepath.Join(root, "scripts"), 0700); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, filepath.Join(root, "scripts/guest-disk-tmp.sh"), []byte("trusted policy\n"), 0600)
			testkit.WriteFile(t, filepath.Join(root, "status"), []byte("running"), 0600)
			testkit.WriteFile(t, filepath.Join(root, "memory"), []byte("tmpfs"), 0600)
			testkit.WriteFile(t, filepath.Join(root, "scripts/06-network.sh"), fmt.Appendf(nil, `#!/bin/sh
printf 'network %%s\n' "$1" >> %[1]q/calls
[ "$1" != --check ] || exit 1
exit %[2]d
`, root, test.repair), 0700)
			testkit.WriteFile(t, filepath.Join(root, "scripts/lifecycle-guard.sh"), fmt.Appendf(nil, `#!/bin/sh
printf 'lifecycle %%s\n' "$1" >> %[1]q/calls
case "$1" in
 stop) printf "physical stop\n" >&2; printf stopped > %[1]q/status ;;
 start) printf running > %[1]q/status; rm -f %[1]q/memory ;;
esac
`, root), 0700)
			configuration := map[string]string{"volatile.uuid": "uuid-original", "user.subyard.desired_power": "running", "user.subyard.managed": "true", "user.subyard.initialized": "true"}
			native := &restartNative{Incus: &testkit.Incus{Instances: map[string]ports.InstanceInfo{"fixture/yard": {Project: "fixture", Name: "yard", Config: configuration, LocalConfig: configuration}}}, root: root}
			var warnings bytes.Buffer
			runtime := reconcileruntime.Runtime{Stderr: &warnings, RepositoryRoot: root, Incus: native, Executor: native, NetworkPolicy: restartNetwork{}, Yard: domain.Context{YardName: "default", IncusProject: "fixture", YardInstanceName: "yard"}}
			err := CompleteRegisteredDiskTmp(context.Background(), runtime)
			calls, _ := os.ReadFile(filepath.Join(root, "calls"))
			if !test.stopped {
				if err == nil {
					t.Fatal("host refusal succeeded")
				}
				if status, _ := os.ReadFile(filepath.Join(root, "status")); string(status) != "running" || string(calls) != "network --check\nnetwork --yes\n" {
					t.Fatalf("refused host repair touched the yard: %s %s", status, calls)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			disclosure, stop := strings.Index(warnings.String(), DiskTmpDisclosure("default", true)), strings.Index(warnings.String(), "physical stop")
			if disclosure < 0 || stop <= disclosure {
				t.Fatal("caller lost disclosure before stop", warnings.String())
			}
			if string(calls) != "network --check\nnetwork --yes\nlifecycle stop\nlifecycle start\n" {
				t.Fatalf("calls:\n%s", calls)
			}
		})
	}
}

func TestGuestTmpStoppedYardIsUntouched(t *testing.T) {
	config := map[string]string{"volatile.uuid": "original", "user.subyard.desired_power": "stopped"}
	native := &testkit.Incus{Instances: map[string]ports.InstanceInfo{"fixture/yard": {Project: "fixture", Name: "yard", Status: "Stopped", Config: config, LocalConfig: config}}}
	runtime := reconcileruntime.Runtime{Incus: native, Yard: domain.Context{IncusProject: "fixture", YardInstanceName: "yard"}}
	if err := CompleteRegisteredDiskTmp(context.Background(), runtime); err != nil {
		t.Fatal(err)
	}
	if native.Instances["fixture/yard"].Config["user.subyard.disk_tmp"] != "" {
		t.Fatal("stopped yard marked complete without boot")
	}
}

func TestGuestTmpDisclosureFitsReleasedResult(t *testing.T) {
	// Resource IDs cap a wire-visible yard at 123 bytes (the 'yard.' prefix
	// consumes five). This also covers Incus's shorter native name limit.
	for _, running := range []bool{false, true} {
		text := DiskTmpDisclosure(strings.Repeat("a", 123), running)
		if len(text) > 256 {
			t.Fatalf("frozen Result overflow: %d", len(text))
		}
		if running && (!strings.Contains(text, "sessions") || !strings.Contains(text, "connections") || !strings.Contains(text, "persistent data")) {
			t.Fatal(text)
		}
	}
}

// A failed yard completes its authorized row, not its native policy. A fresh
// update sees the absent marker again and leaves completed yards alone.
func TestGuestTmpFailureKeepsPendingMarkerWithoutStoppingLaterRows(t *testing.T) {
	var warnings bytes.Buffer
	owners := []*RegisteredYardMigration{}
	natives := []*registeredNativeFixture{}
	desired := []releasetransition.Fingerprint{}
	for index := 0; index < 3; index++ {
		_, owner, native, _ := pairedRegisteredOwners(t)
		original := owner.LoadTargets
		owner.LoadTargets = func(ctx context.Context, view releasetransition.V2SettingsSnapshotView) ([]RegisteredYardTarget, error) {
			targets, err := original(ctx, view)
			for i := range targets {
				targets[i].Runtime.Stderr = &warnings
			}
			return targets, err
		}
		before, err := owner.Observe(context.Background(), releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{})
		if err != nil {
			t.Fatal(err)
		}
		desired = append(desired, before.Desired)
		native.failTmp = index == 1
		owners = append(owners, owner)
		natives = append(natives, native)
	}
	for index, owner := range owners {
		if err := owner.Reconcile(context.Background(), releasetransition.ReleaseLinks{}); err != nil {
			t.Fatal(err)
		}
		after, err := owner.Observe(context.Background(), releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{})
		if err != nil || !after.Converged || after.Desired != desired[index] {
			t.Fatalf("row %d not verified: %+v %v", index, after, err)
		}
		if (natives[index].Instances["fixture/yard"].Config["user.subyard.disk_tmp"] == "v1") != (index != 1) {
			t.Fatalf("row %d marker lies", index)
		}
	}
	if !strings.Contains(warnings.String(), "yard default: disk-/tmp migration pending") {
		t.Fatal("missing named warning", warnings.String())
	}
	for index, owner := range owners {
		owner.attempted = false // the next command's fresh owner
		natives[index].failTmp = false
		before, err := owner.Observe(context.Background(), releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{})
		if err != nil {
			t.Fatal(err)
		}
		calls := len(natives[index].ExecCalls)
		if before.Converged == (index == 1) {
			t.Fatalf("fresh row %d lost marker-derived work", index)
		}
		if err := owner.Reconcile(context.Background(), releasetransition.ReleaseLinks{}); err != nil {
			t.Fatal(err)
		}
		if natives[index].Instances["fixture/yard"].Config["user.subyard.disk_tmp"] != "v1" || (index != 1 && len(natives[index].ExecCalls) != calls) {
			t.Fatalf("retry repeated finished row %d", index)
		}
	}
}

func TestGuestTmpNewRunningYardRequiresFreshPlan(t *testing.T) {
	_, owner, native, _ := pairedRegisteredOwners(t)
	instance := native.Instances["fixture/yard"]
	instance.Status = "Stopped"
	native.Instances["fixture/yard"], native.Reconcile.Instance = instance, instance
	if _, err := owner.Observe(context.Background(), releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{}); err != nil {
		t.Fatal(err)
	}
	instance.Status = "Running"
	native.Instances["fixture/yard"], native.Reconcile.Instance = instance, instance
	if err := owner.Reconcile(context.Background(), releasetransition.ReleaseLinks{}); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("new restart escaped plan: %v", err)
	}
	if len(native.ExecCalls) != 0 || instance.Config["user.subyard.disk_tmp"] != "" {
		t.Fatal("stale plan mutated guest")
	}
}

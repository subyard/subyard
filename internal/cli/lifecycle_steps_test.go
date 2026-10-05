package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/command"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
	"golang.org/x/sys/unix"
)

func TestStopCapturedNoOpRejectsNewWork(t *testing.T) {
	program, loaded, _, _ := credentialCLIFixture(t, strings.NewReader(""), &testkit.Prompt{}, false)
	incus := &testkit.Incus{Instances: map[string]ports.InstanceInfo{loaded.Context.IncusProject + "/" + loaded.Context.YardInstanceName: {Name: loaded.Context.YardInstanceName, Project: loaded.Context.IncusProject, Status: "Stopped"}}}
	program.options.Incus = incus
	execution, err := prepareLifecycleExecution(command.Definition{Name: "stop", Handler: "@lifecycle"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := program.observeLifecycleExecution(context.Background(), loaded.Context, execution); err != nil {
		t.Fatal(err)
	}
	if execution.changed || execution.steps(loaded.Context)[0].Decision != domain.StepSkip {
		t.Fatal("stopped instance did not project skip")
	}
	instance := incus.Instances[loaded.Context.IncusProject+"/"+loaded.Context.YardInstanceName]
	instance.Status = "Running"
	incus.Instances[loaded.Context.IncusProject+"/"+loaded.Context.YardInstanceName] = instance
	if err := program.observeLifecycleExecution(context.Background(), loaded.Context, execution); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("stop expanded from skip: %v", err)
	}
}

func TestTeardownSnapshotRejectsNewResourceAndArtifactChanges(t *testing.T) {
	resource := ports.TeardownResource{Kind: "instance", Name: "approved", Binding: strings.Repeat("a", 64)}
	approved := &teardownSnapshot{Resources: []ports.TeardownResource{resource}, Artifacts: []teardownArtifact{{Path: "owned", Binding: "before"}}}
	converged := &teardownSnapshot{Artifacts: []teardownArtifact{{Path: "owned"}}}
	if err := checkTeardownSnapshot(approved, converged); err != nil {
		t.Fatal(err)
	}
	newTarget := &teardownSnapshot{Resources: []ports.TeardownResource{resource, {Kind: "instance", Name: "new", Binding: strings.Repeat("b", 64)}}, Artifacts: approved.Artifacts}
	if err := checkTeardownSnapshot(approved, newTarget); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("new resource accepted: %v", err)
	}
	altered := &teardownSnapshot{Resources: approved.Resources, Artifacts: []teardownArtifact{{Path: "owned", Binding: "after"}}}
	if err := checkTeardownSnapshot(approved, altered); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("artifact drift accepted: %v", err)
	}
}

func TestTeardownArtifactReplacementRejectsPreservedMetadata(t *testing.T) {
	for _, kind := range []string{"file", "symlink", "contents"} {
		t.Run(kind, func(t *testing.T) {
			root := testkit.TempDir(t)
			path := filepath.Join(root, "owned")
			if kind == "symlink" {
				if err := os.Symlink("before", path); err != nil {
					t.Fatal(err)
				}
			} else {
				testkit.WriteFile(t, path, []byte("before"), 0600)
			}
			before, err := teardownArtifactBinding(path)
			if err != nil {
				t.Fatal(err)
			}
			info, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			// Filesystems may share ctime ticks across immediate writes.
			time.Sleep(20 * time.Millisecond)
			if kind == "symlink" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("after!", path); err != nil {
					t.Fatal(err)
				}
				stamp := unix.NsecToTimespec(info.ModTime().UnixNano())
				if err := unix.UtimesNanoAt(unix.AT_FDCWD, path, []unix.Timespec{stamp, stamp}, unix.AT_SYMLINK_NOFOLLOW); err != nil {
					t.Fatal(err)
				}
			} else {
				if kind == "file" {
					replacement := filepath.Join(root, "replacement")
					testkit.WriteFile(t, replacement, []byte("after!"), 0600)
					if err := os.Rename(replacement, path); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(path, []byte("after!"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
					t.Fatal(err)
				}
			}
			after, err := teardownArtifactBinding(path)
			if err != nil {
				t.Fatal(err)
			}
			approved := &teardownSnapshot{Artifacts: []teardownArtifact{{Path: path, Binding: before}}}
			current := &teardownSnapshot{Artifacts: []teardownArtifact{{Path: path, Binding: after}}}
			if err := checkTeardownSnapshot(approved, current); !errors.Is(err, domain.ErrPlanStale) {
				t.Fatalf("%s drift with preserved size and permissions accepted: %v", kind, err)
			}
		})
	}
}

func TestTeardownAndResetRejectArtifactReplacementBeforeApply(t *testing.T) {
	for _, reset := range []bool{false, true} {
		t.Run(map[bool]string{false: "teardown", true: "reset"}[reset], func(t *testing.T) {
			root, environment, _ := nativeFixture(t)
			program, err := New(Options{
				RepositoryRoot: root, Program: "yard", Environment: environment,
				WorkingDir: root, Incus: lifecycleIncus(), NetworkPolicy: allowTestNetworkPolicy(),
			})
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := program.loadContext("default")
			if err != nil {
				t.Fatal(err)
			}
			loaded.Context.Paths.StateDir = testkit.TempDir(t)
			path := filepath.Join(loaded.Context.Paths.StateDir, "owned.json")
			testkit.WriteFile(t, path, []byte("before"), 0600)
			execution := &teardownExecution{}
			var baseline *resetTeardownBaseline
			if reset {
				baseline, err = program.prepareResetTeardownBaseline(context.Background(), loaded)
				if err != nil {
					t.Fatal(err)
				}
				init := &initExecution{loaded: loaded, mode: initReset, resetBaseline: baseline, teardownResources: baseline.resources()}
				init.rebuildPlatform(program)
				runtime := init.platform.(reconcileruntime.Runtime)
				if operationStateDigest(runtime.TeardownArtifacts) != operationStateDigest(baseline.artifacts()) {
					t.Fatal("reset platform rebuild lost approved artifact scope")
				}
				execution = baseline.execution
			} else if err := program.observeTeardownExecution(context.Background(), loaded, execution); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			replacement := filepath.Join(testkit.TempDir(t), "replacement")
			testkit.WriteFile(t, replacement, []byte("after!"), 0600)
			if err := os.Chtimes(replacement, info.ModTime(), info.ModTime()); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(replacement, path); err != nil {
				t.Fatal(err)
			}
			if reset {
				err = baseline.check(context.Background(), program)
			} else {
				err = program.observeTeardownExecution(context.Background(), loaded, execution)
			}
			if !errors.Is(err, domain.ErrPlanStale) {
				t.Fatalf("replacement accepted before destructive apply: %v", err)
			}
		})
	}
}

func TestTeardownPhysicalStaleRefusalRetainsErrorType(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	command := exec.Command("sh", "-c", "exit 75")
	exitErr := command.Run()
	if exitErr == nil {
		t.Fatal("stale fixture did not fail")
	}
	runner := &testkit.ScriptedAdapter{Steps: []testkit.AdapterStep{{Err: exitErr}}}
	incus := lifecycleIncus()
	incus.Reconcile.InstanceFound = true
	program, err := New(Options{
		RepositoryRoot: root, Program: "yard", Environment: environment,
		WorkingDir: root, Incus: incus, AdapterRunner: runner, NetworkPolicy: allowTestNetworkPolicy(),
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	_, err = program.executeTeardown(context.Background(), &application.Orchestrator{Runner: runner, Clock: wallClock{}}, loaded,
		domain.OperationPlan{OperationID: "stale-teardown", Confirmed: true}, &teardownExecution{}, io.Discard)
	if !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("physical drift lost plan_stale type: %v", err)
	}
}

func TestTestVMNoOpRejectsNewAllocationBeforeWorker(t *testing.T) {
	loaded := config.Loaded{Context: domain.Context{NestedE2EVMs: true, AccessKind: domain.AccessLocal, YardKind: domain.YardContainer, IncusProject: "subyard", YardInstanceName: "yard"}}
	available := `{"schema_version":1,"status":"ok","pool":{"schema_version":2,"resource_type":"agent-e2e","resource_id":"test-vms","slots":[{"slot_id":"slot-001","resource_generation":7,"lease_epoch":0,"state":"available"}]}}`
	probe := &testVMStatusProbe{output: []byte(available)}
	program := &CLI{options: Options{Incus: &testkit.Incus{Instances: map[string]ports.InstanceInfo{"subyard/yard": {Name: "yard", Project: "subyard", Status: "Running"}}}, ProjectData: probe}}
	execution, err := program.prepareTestVMExecution(context.Background(), loaded, []string{"revoke", "--slot", "1"})
	if err != nil {
		t.Fatal(err)
	}
	if !execution.noOp || execution.steps()[0].Decision != domain.StepSkip {
		t.Fatal("available slot did not project skip")
	}
	probe.output = []byte(strings.Replace(strings.Replace(available, `"state":"available"`, `"state":"held"`, 1), `"lease_epoch":0`, `"lease_epoch":1`, 1))
	_, err = program.executeTestVMs(context.Background(), nil, loaded, domain.OperationPlan{OperationID: "no-op-slot"}, execution, io.Discard)
	if !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("new allocation accepted by no-op: %v", err)
	}
}

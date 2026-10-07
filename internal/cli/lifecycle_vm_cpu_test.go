package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/testkit"
)

type lifecycleCPUFixture struct {
	*testkit.Incus
	t       *testing.T
	ready   bool
	err     error
	checks  int
	project string
}

func (fixture *lifecycleCPUFixture) VMCPUConverged(_ context.Context, project, instance string, apply bool) (bool, error) {
	fixture.checks++
	expectedProject := fixture.project
	if expectedProject == "" {
		expectedProject = "synthetic"
	}
	if project != expectedProject || instance != "yard" || apply {
		fixture.t.Fatal("privilege assessment did not use read-only target readiness")
	}
	return fixture.ready, fixture.err
}

func lifecycleVMCPUCommandFixture(t *testing.T, status string, ready bool) (*CLI, config.Loaded, *lifecycleCPUFixture, *testkit.ScriptedAdapter, string) {
	t.Helper()
	root, environment, _ := nativeFixture(t)
	log := filepath.Join(root, "sudo.log")
	testkit.WriteFile(t, filepath.Join(root, "sudo"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$SUDO_LOG\"\nexit 1\n"), 0o700)
	incus := lifecycleIncus()
	instance := incus.Instances["subyard/yard"]
	instance.Status, instance.Type = status, domain.YardVM
	instance.Config["user.subyard.vm_cpu_weight"] = "250"
	incus.Instances["subyard/yard"] = instance
	fixture := &lifecycleCPUFixture{Incus: incus, t: t, ready: ready, project: "subyard"}
	runner := &testkit.ScriptedAdapter{}
	program, err := New(Options{RepositoryRoot: root, WorkingDir: root, Program: "yard",
		Environment: append(environment, "PATH="+root, "SUDO_LOG="+log),
		Incus:       fixture, AdapterRunner: runner, NetworkPolicy: allowTestNetworkPolicy()})
	if err != nil {
		t.Fatal(err)
	}
	program.effectiveUID = func() int { return 1000 }
	loaded, err := program.loadContext("default")
	if err != nil {
		t.Fatal(err)
	}
	return program, loaded, fixture, runner, log
}

func TestLifecycleVMCPUPlanReadOnly(t *testing.T) {
	failure := errors.New("invalid owned scheduling metadata")
	for _, scenario := range []struct {
		name, status                 string
		ready, noWeight, noScheduler bool
		failure                      error
		decision                     domain.StepDecision
	}{
		{name: "running drift", status: "Running", decision: domain.StepApply},
		{name: "running current", status: "Running", ready: true, decision: domain.StepSkip},
		{name: "stopped", status: "Stopped", noScheduler: true, decision: domain.StepConditional},
		{name: "unset", status: "Running", noWeight: true, noScheduler: true},
		{name: "invalid", status: "Running", failure: failure},
		{name: "unavailable", status: "Running", noScheduler: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			program, loaded, fixture, runner, log := lifecycleVMCPUCommandFixture(t, scenario.status, scenario.ready)
			fixture.err = scenario.failure
			if scenario.noWeight {
				delete(fixture.Instances["subyard/yard"].Config, "user.subyard.vm_cpu_weight")
			}
			if scenario.noScheduler {
				program.options.Incus = fixture.Incus
			}
			definition, _ := program.manifest.Lookup("start")
			prepared, err := program.prepareCommand(context.Background(), prepareCommandRequest{Loaded: loaded, Definition: definition, ReadOnly: true})
			if scenario.failure != nil {
				if !errors.Is(err, scenario.failure) {
					t.Fatalf("lost readiness cause: %v", err)
				}
			} else if scenario.name == "unavailable" {
				if err == nil {
					t.Fatal("weighted running start accepted unavailable scheduler")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				defer prepared.Close()
				steps := prepared.Plan.Steps
				if !prepared.stepsComplete || len(steps) != map[bool]int{true: 1, false: 2}[scenario.noWeight] {
					t.Fatalf("incomplete lifecycle preview: %#v", steps)
				}
				if !scenario.noWeight && (steps[1].ID != "vm-cpu" || steps[1].Decision != scenario.decision ||
					steps[1].Target != "incus/subyard/instance/yard" || steps[1].Desired != "cpu.weight=250" ||
					len(steps[1].DependsOn) != 1 || steps[1].DependsOn[0] != "power") {
					t.Fatalf("CPU preview lost approved effect: %#v", steps)
				}
				if scenario.status == "Running" && steps[0].Decision != domain.StepSkip {
					t.Fatalf("running preview requires power work: %#v", steps)
				}
			}
			if _, err := os.Stat(log); !errors.Is(err, os.ErrNotExist) || len(runner.Requests) != 0 ||
				len(fixture.ConfigUpdates) != 0 || len(fixture.PowerUpdates) != 0 {
				t.Fatal("read-only CPU planning performed work")
			}
		})
	}
}

func TestLifecycleVMCPUPlanRejectsNewWorkBeforeExecution(t *testing.T) {
	for _, drift := range []string{"scheduler", "weight", "power"} {
		t.Run(drift, func(t *testing.T) {
			program, loaded, fixture, runner, log := lifecycleVMCPUCommandFixture(t, "Running", true)
			definition, _ := program.manifest.Lookup("start")
			prepared, err := program.prepareCommand(context.Background(), prepareCommandRequest{Loaded: loaded, Definition: definition, ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			defer prepared.Close()
			switch drift {
			case "scheduler":
				fixture.ready = false
			case "weight":
				fixture.Instances["subyard/yard"].Config["user.subyard.vm_cpu_weight"] = "300"
			case "power":
				instance := fixture.Instances["subyard/yard"]
				instance.Status = "Stopped"
				fixture.Instances["subyard/yard"] = instance
			}
			orchestrator := program.operationOrchestrator(prepared.Plan.OperationID, loaded, nil, &definition)
			// Production privilege preparation must remain behind the stale guard.
			program.options.AdapterRunner = nil
			var diagnostics bytes.Buffer
			_, err = prepared.Execute(context.Background(), orchestrator, &diagnostics)
			if !errors.Is(err, domain.ErrPlanStale) || diagnostics.Len() != 0 {
				t.Fatalf("new %s work accepted: err=%v diagnostics=%q", drift, err, diagnostics.String())
			}
			if _, err := os.Stat(log); !errors.Is(err, os.ErrNotExist) || len(runner.Requests) != 0 ||
				len(fixture.ConfigUpdates) != 0 || len(fixture.PowerUpdates) != 0 {
				t.Fatal("stale CPU plan performed work")
			}
		})
	}
}

func TestLifecycleVMCPUPlanAllowsApprovedConvergence(t *testing.T) {
	for _, status := range []string{"Running", "Stopped"} {
		t.Run(status, func(t *testing.T) {
			program, loaded, fixture, _, _ := lifecycleVMCPUCommandFixture(t, status, false)
			definition, _ := program.manifest.Lookup("start")
			execution, err := prepareLifecycleExecution(definition, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := program.observeLifecycleExecution(context.Background(), loaded.Context, execution); err != nil {
				t.Fatal(err)
			}
			instance := fixture.Instances["subyard/yard"]
			instance.Status = "Running"
			fixture.Instances["subyard/yard"] = instance
			// A conditional stopped assessment permits the captured scheduler effect.
			if status == "Stopped" {
				if err := program.observeLifecycleExecution(context.Background(), loaded.Context, execution); err != nil {
					t.Fatalf("approved conditional effect rejected: %v", err)
				}
			}
			fixture.ready = true
			if err := program.observeLifecycleExecution(context.Background(), loaded.Context, execution); err != nil {
				t.Fatalf("approved convergence rejected: %v", err)
			}
			if execution.steps(loaded.Context)[1].Decision != domain.StepSkip {
				t.Fatal("convergence did not remove CPU work")
			}
			if err := program.prepareLifecycleVMCPUPrivileges(context.Background(), io.Discard, loaded.Context, execution); err != nil {
				t.Fatalf("converged plan requested privileges: %v", err)
			}
		})
	}
}

func TestLifecycleVMCPUPrivilegesRejectNewDrift(t *testing.T) {
	program, loaded, fixture, _, log := lifecycleVMCPUCommandFixture(t, "Running", true)
	definition, _ := program.manifest.Lookup("start")
	execution, err := prepareLifecycleExecution(definition, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := program.observeLifecycleExecution(context.Background(), loaded.Context, execution); err != nil {
		t.Fatal(err)
	}
	fixture.ready = false
	var diagnostics bytes.Buffer
	err = program.prepareLifecycleVMCPUPrivileges(context.Background(), &diagnostics, loaded.Context, execution)
	if !errors.Is(err, domain.ErrPlanStale) || diagnostics.Len() != 0 {
		t.Fatalf("privilege preparation authorized new drift: err=%v diagnostics=%q", err, diagnostics.String())
	}
	if _, err := os.Stat(log); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("new drift requested sudo")
	}
}

func TestLifecycleVMCPUPrivilegesSkipConvergedStart(t *testing.T) {
	failure := errors.New("invalid owned scheduling metadata")
	for _, scenario := range []struct {
		name                          string
		changed, ready, authorization bool
		failure                       error
	}{
		{name: "converged", ready: true},
		{name: "drift", authorization: true},
		{name: "stopped", changed: true, authorization: true},
		{name: "invalid", failure: failure},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			root := testkit.TempDir(t)
			log := filepath.Join(root, "sudo.log")
			testkit.WriteFile(t, filepath.Join(root, "sudo"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$SUDO_LOG\"\nexit 1\n"), 0o700)
			t.Setenv("PATH", root)
			fixture := &lifecycleCPUFixture{Incus: &testkit.Incus{}, t: t, ready: scenario.ready, err: scenario.failure}
			program := &CLI{options: Options{Incus: fixture}, env: map[string]string{"PATH": root, "SUDO_LOG": log},
				effectiveUID: func() int { return 1000 }, operatorTerminal: func() bool { return false }}
			var diagnostics bytes.Buffer
			err := program.prepareLifecycleVMCPUPrivileges(context.Background(), &diagnostics,
				domain.Context{IncusProject: "synthetic", YardInstanceName: "yard"},
				&lifecycleExecution{action: "start", changed: scenario.changed})
			_, statErr := os.Stat(log)
			if (statErr == nil) != scenario.authorization || (err != nil) != (scenario.authorization || scenario.failure != nil) {
				t.Fatalf("authorization=%t err=%v diagnostic=%q", statErr == nil, err, diagnostics.String())
			}
			if scenario.failure != nil && !errors.Is(err, scenario.failure) {
				t.Fatalf("lost readiness cause: %v", err)
			}
			if fixture.checks != map[bool]int{true: 0, false: 1}[scenario.changed] {
				t.Fatalf("readiness checks=%d", fixture.checks)
			}
			if diagnostics.Len() != 0 {
				t.Fatalf("no-op or failed assessment emitted warning: %q", diagnostics.String())
			}
		})
	}
}

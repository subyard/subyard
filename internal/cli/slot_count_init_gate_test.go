package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

type slotCountRepairPlatformFixture struct {
	*sampleRepairPlatformFixture
	broker ports.RuntimeObservation
}

func (fixture *slotCountRepairPlatformFixture) ObserveTestVMSlotCountRepair(context.Context) (ports.RuntimeObservation, error) {
	return fixture.broker, nil
}

func TestSlotCountInitRepairUsesCompletedGateAndBindsBackend(t *testing.T) {
	sourceRoot := repositoryRoot(t)
	for _, scenario := range []string{"repair", "unselected", "other backend drift", "config apply", "converged race", "changed actual", "changed desired"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newConfigApplyRepairFixture(t, false)
			root := fixture.cli.options.RepositoryRoot
			bin := filepath.Join(root, "mock-bin")
			for _, directory := range []string{
				filepath.Join(root, "config", "yards", "profiles"),
				filepath.Join(fixture.cli.env["SUBYARD_CONFIG_HOME"], "yards", "test-yard"), bin,
			} {
				if err := os.MkdirAll(directory, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			profile, err := os.ReadFile(filepath.Join(sourceRoot, "config", "yards", "profiles", "test-vms.env"))
			if err != nil {
				t.Fatal(err)
			}
			writeCLIFile(t, filepath.Join(root, "config", "yards", "profiles", "test-vms.env"), string(profile), 0o600)
			writeCLIFile(t, filepath.Join(fixture.cli.env["SUBYARD_CONFIG_HOME"], "yards", "test-yard", "config.env"),
				"YARD_TEMPLATE=test-vms\nSSH_PORT=2224\nE2E_VM_SLOT_COUNT=4\nCODING_TOOL_INTEGRATIONS=''\n", 0o600)
			writeCLIFile(t, filepath.Join(bin, "incus"), `#!/bin/sh
set -eu
case "$*" in
  "list --all-projects --format=json")
    printf '%s\n' '[]' ;;
  "project list --format=json")
    printf '%s\n' '[{"name":"subyard-test-yard"}]' ;;
  "project get subyard-test-yard features.images")
    printf '%s\n' true ;;
  "list yard-test-yard --project subyard-test-yard --format=json")
    printf '%s\n' '[{"name":"yard-test-yard","status":"RUNNING","config":{"user.subyard.desired_power":"running"}}]' ;;
  "exec yard-test-yard --project subyard-test-yard -- systemctl is-active subyard-test-vms-broker.service")
    printf '%s\n' active ;;
  *) exit 2 ;;
esac
`, 0o700)
			t.Setenv("PATH", bin)
			platform := &slotCountRepairPlatformFixture{
				sampleRepairPlatformFixture: &sampleRepairPlatformFixture{initPlatformFixture: newInitPlatformFixture()},
				broker: ports.RuntimeObservation{State: ports.RuntimeStateStale,
					Actual: strings.Repeat("a", 64), Desired: strings.Repeat("b", 64)},
			}
			for stage := range platform.converged {
				platform.converged[stage] = true
			}
			platform.converged[ports.ReconcileStageTestVMs] = false
			fixture.cli.options.InitPlatform = platform
			fake := fixture.cli.options.Executor.(*testkit.Incus)
			fake.ExecSteps = nil
			loaded, err := fixture.cli.resolveReleaseTransitionContext("test-yard", fixture.cli.env["SUBYARD_CONFIG_HOME"])
			if err != nil {
				t.Fatal(err)
			}
			targets, err := fixture.cli.localConfigTargets(loaded, true)
			if err != nil {
				t.Fatal(err)
			}
			for range 3 {
				for _, target := range targets {
					appendHashSteps(t, fake, target.Loaded)
				}
			}
			if scenario == "other backend drift" {
				platform.broker = ports.RuntimeObservation{State: ports.RuntimeStateAbsent}
			}
			yard := "test-yard"
			if scenario == "unselected" {
				yard = "default"
			}
			permit, err := fixture.cli.prepareProfileInitRepair(context.Background(), yard, nil, fixture.outcome)
			if scenario == "config apply" {
				permit, err = fixture.cli.prepareConfigApplyRepair(context.Background(), yard, false, fixture.outcome)
			}
			admitted := scenario != "unselected" && scenario != "other backend drift" && scenario != "config apply"
			if err != nil || (permit != nil) != admitted {
				t.Fatalf("slot-count repair admission: permit=%#v err=%v", permit, err)
			}
			if !admitted {
				return
			}
			if len(platform.applied) != 0 {
				t.Fatal("repair assessment applied a stage")
			}
			switch scenario {
			case "converged race":
				platform.broker.State, platform.broker.Actual = ports.RuntimeStateCurrent, platform.broker.Desired
				platform.converged[ports.ReconcileStageTestVMs] = true
				writeCLIFile(t, fixture.readyMarker, "ready\n", 0o600)
			case "changed actual":
				platform.broker.Actual = strings.Repeat("c", 64)
			case "changed desired":
				platform.broker.Desired = strings.Repeat("c", 64)
			}
			unlock, err := fixture.cli.lockConfigApplyRepair(context.Background(), permit)
			if scenario == "changed actual" || scenario == "changed desired" {
				if err == nil {
					unlock()
					t.Fatal("changed backend contract admitted after confirmation")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
			platform.converged[ports.ReconcileStageTestVMs] = true
			writeCLIFile(t, fixture.readyMarker, "ready\n", 0o600)
			if err := fixture.cli.finishConfigApplyRepair(context.Background(), permit); err != nil {
				t.Fatal(err)
			}
		})
	}
}

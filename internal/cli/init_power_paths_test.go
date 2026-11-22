package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/Subyard/Subyard/internal/ports"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestInitPersistsCommandPowerPathsForReadiness(t *testing.T) {
	program, _, home, server := completedRuntimeSourceAuthoringFixture(t, false, nil)
	testkit.WriteFile(t, filepath.Join(home, "host-id"), []byte("yard\n"), 0600)
	// A stopped yard needs no guest provisioning; this regression owns host paths.
	server.SetInstance("subyard", "yard", map[string]any{"name": "yard", "type": "container", "status": "Stopped", "config": map[string]string{"volatile.uuid": "source-authoring-default", "user.subyard.managed": "true"}, "expanded_config": map[string]string{"volatile.uuid": "source-authoring-default", "user.subyard.managed": "true"}})

	helper, unit := filepath.Join(home, "custom-power"), filepath.Join(home, "custom-power.service")
	options := program.options
	options.Environment = append(options.Environment, "SUBYARD_POWER_RECONCILER_PATH="+helper, "SUBYARD_POWER_UNIT_PATH="+unit)
	options.Arguments = []string{"init", "--yes"}
	platform := sourceAuthoringPlatformFixture(t, options.InitPlatform)
	for stage := range platform.converged {
		platform.converged[stage] = true
	}
	var output bytes.Buffer
	options.Stdout, options.Stderr = &output, &output
	program = mustNew(t, options)
	if code := program.Run(context.Background()); code != 0 {
		t.Fatalf("init: code=%d output=%s", code, &output)
	}
	assignments, err := config.ParsePersistentAssignments(filepath.Join(home, "config.env"), mustReadConfigFixture(t, filepath.Join(home, "config.env")))
	if err != nil {
		t.Fatal(err)
	}
	persisted := map[string]string{}
	for _, assignment := range assignments {
		persisted[assignment.Name] = assignment.Value
	}
	if persisted["SUBYARD_POWER_RECONCILER_PATH"] != helper || persisted["SUBYARD_POWER_UNIT_PATH"] != unit {
		t.Fatal("init did not persist effective power paths")
	}
	// A fresh release assessment discards the command overrides. It must still
	// resolve the paths init used from host settings, and report ready.
	operation := program.releaseActivationOperation()
	loaded, err := operation.resolveReleaseTransitionContext(context.Background(), "default", home)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Environment["SUBYARD_POWER_RECONCILER_PATH"] != helper || loaded.Environment["SUBYARD_POWER_UNIT_PATH"] != unit {
		t.Fatal("fresh readiness lost the persisted paths")
	}
	outcome, _, err := program.inspectMutationGateInspection(context.Background(), "default")
	if err != nil || outcome != nil {
		t.Fatalf("readiness after init: %#v %v", outcome, err)
	}
	// Prove the real readiness producer inspects the persisted location.
	testkit.WriteFile(t, unit, []byte("stale unit\n"), 0600)
	outcome, _, err = program.inspectMutationGateInspection(context.Background(), "default")
	if err != nil || outcome == nil || outcome.Status != releasetransition.StatusMigrationRequired {
		t.Fatalf("stale persisted power path: %#v %v", outcome, err)
	}
}

func TestInitCommandPowerPathsDesiredState(t *testing.T) {
	for _, scenario := range []string{"helper only", "unit only", "late failure", "no override", "configs"} {
		t.Run(scenario, func(t *testing.T) {
			cliFixture := newFixture(t, withWorkingDir(""))
			root, environment := cliFixture.Root, cliFixture.Environment
			home := environmentValue(environment, "SUBYARD_CONFIG_HOME")
			path := filepath.Join(home, "config.env")
			if err := os.MkdirAll(home, 0700); err != nil {
				t.Fatal(err)
			}
			before := []byte("# preserve unrelated settings\nSSH_PORT=2222\n")
			testkit.WriteFile(t, path, before, 0600)
			name := "SUBYARD_POWER_RECONCILER_PATH"
			if scenario == "unit only" {
				name = "SUBYARD_POWER_UNIT_PATH"
			}
			value := filepath.Join(home, "custom power path")
			if scenario != "no override" {
				environment = append(environment, name+"="+value)
			}
			platform := newInitPlatformFixture()
			if scenario == "late failure" {
				platform.converged[ports.ReconcileStageIncus] = true
				platform.applyErr = errors.New("synthetic init failure")
			}
			var output bytes.Buffer
			arguments := []string{"init", "--yes"}
			if scenario == "configs" {
				arguments = append(arguments, "--configs")
			}
			program := cliFixture.New(Options{RepositoryRoot: root, Environment: environment, InitPlatform: platform, Arguments: arguments, Stdout: &output, Stderr: &output})
			code := program.Run(context.Background())
			if scenario == "late failure" {
				if code == 0 {
					t.Fatal("init unexpectedly succeeded")
				}
			} else if code != 0 {
				t.Fatalf("init: %d %s", code, &output)
			}
			after := mustReadConfigFixture(t, path)
			if scenario != "no override" && scenario != "configs" && (!strings.Contains(output.String(), "persist effective command power paths") || !strings.Contains(output.String(), "retain them if later initialization fails")) {
				t.Fatalf("missing plan consequence: %s", &output)
			}
			if scenario == "configs" || scenario == "no override" {
				if !bytes.Equal(after, before) {
					t.Fatal("init changed host settings")
				}
				return
			}
			wantAssignment := fmt.Sprintf("%s=%q", name, value)
			otherName := "SUBYARD_POWER_UNIT_PATH"
			if name == otherName {
				otherName = "SUBYARD_POWER_RECONCILER_PATH"
			}
			if !strings.Contains(output.String(), wantAssignment) || strings.Contains(output.String(), otherName+"=") {
				t.Fatalf("plan must describe only the single persisted override %s: %s", wantAssignment, &output)
			}
			if !bytes.HasPrefix(after, before) {
				t.Fatal("init changed unrelated settings")
			}
			operation := program.releaseActivationOperation()
			loaded, err := operation.resolveReleaseTransitionContext(context.Background(), "default", home)
			if err != nil || loaded.Environment[name] != value {
				t.Fatalf("persisted path did not round trip: %v", err)
			}
			snapshot, err := config.ReadPersistentFileSnapshot(home, path)
			if err != nil {
				t.Fatal(err)
			}
			platform.applyErr = nil
			repeatOptions := program.options
			if scenario == "late failure" {
				repeatOptions.Environment = nil
				for _, entry := range program.options.Environment {
					if !strings.HasPrefix(entry, name+"=") {
						repeatOptions.Environment = append(repeatOptions.Environment, entry)
					}
				}
			}
			repeat := cliFixture.New(repeatOptions)
			if code := repeat.Run(context.Background()); code != 0 {
				t.Fatalf("repeat init: %d %s", code, &output)
			}
			repeated, err := config.ReadPersistentFileSnapshot(home, path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, repeated.Content) || snapshot.Identity != repeated.Identity {
				t.Fatal("repeat init rewrote host settings")
			}
		})
	}
}

func TestInitCommandPowerPathsRefuseConcurrentEdit(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(map[bool]string{false: "changed bytes", true: "changed identity"}[replace], func(t *testing.T) {
			cliFixture := newFixture(t, withWorkingDir(""))
			root, environment := cliFixture.Root, cliFixture.Environment
			home := environmentValue(environment, "SUBYARD_CONFIG_HOME")
			path := filepath.Join(home, "config.env")
			if err := os.MkdirAll(home, 0700); err != nil {
				t.Fatal(err)
			}
			before := []byte("SSH_PORT=2222\n")
			testkit.WriteFile(t, path, before, 0600)
			environment = append(environment, "SUBYARD_POWER_UNIT_PATH="+filepath.Join(home, "custom.service"))
			program := cliFixture.New(Options{RepositoryRoot: root, Environment: environment})
			loaded, err := program.loadContext("default")
			if err != nil {
				t.Fatal(err)
			}
			plan, err := program.catalog.PlanCommandPowerPathsWrite(loaded, program.baseEnv)
			if err != nil {
				t.Fatal(err)
			}
			if plan == nil {
				t.Fatal("missing prepared write")
			}
			concurrent := []byte("SSH_PORT=2244\n")
			if replace {
				concurrent = before
				replacement := path + ".replacement"
				testkit.WriteFile(t, replacement, concurrent, 0600)
				if err := os.Rename(replacement, path); err != nil {
					t.Fatal(err)
				}
			} else {
				testkit.WriteFile(t, path, concurrent, 0600)
			}
			if err := plan.Apply(); !errors.Is(err, config.ErrPersistentTargetStale) {
				t.Fatalf("CAS: %v", err)
			}
			if !bytes.Equal(mustReadConfigFixture(t, path), concurrent) {
				t.Fatal("concurrent edit overwritten")
			}
		})
	}
}

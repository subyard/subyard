package cli

import (
	"bytes"
	"context"
	"errors"
	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/Subyard/Subyard/internal/migration"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
	"maps"
)

func TestRegisteredNestedBrokerRecognizesOwnedHostMemory(t *testing.T) {
	for _, scenario := range []string{"selected", "selected-omitted-mode", "unselected", "foreign", "writable", "inherited", "divergent", "selected-vm", "char-mode", "char-option", "char-source", "char-missing", "char-inherited", "char-divergent"} {
		t.Run(scenario, func(t *testing.T) {
			_, execution, fake := adoptionFixture(t)
			runtime := execution.platform.(reconcileruntime.Runtime)
			runtime.Yard.NestedE2EVMs = scenario != "unselected"
			runtime.Environment = append(runtime.Environment, "NESTED_E2E_VMS=1")
			instance := &fake.Reconcile.Instance
			fake.Reconcile.ProjectConfig["restricted.containers.interception"] = "allow"
			for _, key := range []string{"security.syscalls.intercept.bpf", "security.syscalls.intercept.bpf.devices"} {
				instance.Config[key], instance.LocalConfig[key] = "true", "true"
			}
			for name, source := range map[string]string{"e2e-vsock": "/dev/vsock", "e2e-vhost-vsock": "/dev/vhost-vsock", "e2e-tun": "/dev/net/tun"} {
				device := map[string]string{"type": "unix-char", "source": source, "path": source, "mode": "0660"}
				if scenario == "selected-omitted-mode" {
					delete(device, "mode")
				}
				instance.LocalDevices[name], instance.Devices[name] = device, maps.Clone(device)
			}
			instance.Config["user.subyard.host_memory"], instance.LocalConfig["user.subyard.host_memory"] = "v1", "v1"
			device := map[string]string{"type": "disk", "source": "/proc/meminfo", "path": "/var/lib/subyard/host-meminfo", "readonly": "true"}
			instance.LocalDevices["subyard-host-memory"], instance.Devices["subyard-host-memory"] = device, maps.Clone(device)
			switch scenario {
			case "foreign":
				instance.Config["user.subyard.host_memory"], instance.LocalConfig["user.subyard.host_memory"] = "v2", "v2"
			case "writable":
				delete(device, "readonly")
				delete(instance.Devices["subyard-host-memory"], "readonly")
			case "inherited":
				delete(instance.LocalDevices, "subyard-host-memory")
			case "divergent":
				instance.Devices["subyard-host-memory"]["source"] = "/foreign"
			case "selected-vm":
				canonicalAdoptionVMFixture(&runtime, instance)
			case "char-mode":
				instance.LocalDevices["e2e-vsock"]["mode"], instance.Devices["e2e-vsock"]["mode"] = "0666", "0666"
			case "char-option":
				instance.LocalDevices["e2e-vsock"]["uid"], instance.Devices["e2e-vsock"]["uid"] = "1000", "1000"
			case "char-source":
				instance.LocalDevices["e2e-tun"]["source"], instance.Devices["e2e-tun"]["source"] = "/foreign", "/foreign"
			case "char-missing":
				delete(instance.LocalDevices, "e2e-vhost-vsock")
				delete(instance.Devices, "e2e-vhost-vsock")
			case "char-inherited":
				delete(instance.LocalDevices, "e2e-vhost-vsock")
			case "char-divergent":
				instance.Devices["e2e-vhost-vsock"]["mode"] = "0666"
			}
			fake.Instances = map[string]ports.InstanceInfo{runtime.Yard.IncusProject + "/" + runtime.Yard.YardInstanceName: *instance}
			loaded := execution.loaded
			loaded.Context = runtime.Yard
			owner := &migration.RegisteredYardMigration{Yard: loaded.Context.YardName, ConfigHome: loaded.Context.Paths.ConfigHome, LoadTargets: func(context.Context, releasetransition.V2SettingsSnapshotView) ([]migration.RegisteredYardTarget, error) {
				return []migration.RegisteredYardTarget{{Loaded: loaded, Runtime: runtime}}, nil
			}}
			before := operationStateDigest(*instance)
			_, err := owner.Observe(context.Background(), releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{})
			// Devices are taken as found, including memory ones in any state. Only the
			// effective-device consistency check and nested privileges still refuse here.
			admitted := scenario != "unselected" && scenario != "inherited" && scenario != "divergent" && scenario != "char-inherited" && scenario != "char-divergent"
			if (err == nil) != admitted {
				t.Fatalf("registered nested broker assessment: %v", err)
			}
			if fake.writes != 0 || operationStateDigest(*instance) != before {
				t.Fatal("inspection mutated native state")
			}
			if admitted {
				if _, _, err := runtime.InspectLegacyContinuation(context.Background()); err == nil {
					t.Fatal("strict continuation admitted nested broker")
				}
			}
		})
	}
}

func TestPlainInitFinishesCurrentMigrationsBeforeNormalPlan(t *testing.T) {
	for _, entry := range []string{"ordinary init", "restore prior admission"} {
		t.Run(entry, func(t *testing.T) {
			cliFixture, root, environment, marker := newMigrateCLIFixture(t, true)
			platform := newInitPlatformFixture()
			for stage := range platform.converged {
				platform.converged[stage] = true
			}
			var output bytes.Buffer
			var err error
			program := cliFixture.New(Options{RepositoryRoot: root, Program: "yard", Arguments: []string{"init", "--yes"}, Environment: environment, Stdout: &output, Stderr: &output, InitPlatform: platform})
			loaded, loadErr := program.resolveReleaseTransitionContext(context.Background(), "default", program.env["SUBYARD_CONFIG_HOME"])
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			seedOwnerHostID(t, program.env["SUBYARD_CONFIG_HOME"])
			program.options.Incus = &testkit.Incus{Instances: map[string]ports.InstanceInfo{loaded.Context.IncusProject + "/" + loaded.Context.YardInstanceName: {Name: loaded.Context.YardInstanceName, Project: loaded.Context.IncusProject, Config: map[string]string{"user.subyard.managed": "true"}}}}
			var code int
			if entry == "restore prior admission" {
				previous := &gateAdmission{kind: admissionRead}
				program.gateAdmitted = previous
				code, err = program.finishRegisteredInitMigrations(context.Background(), "default", []string{"--yes"}, true)
				if err != nil || program.gateAdmitted != previous {
					t.Fatalf("compatibility migration leaked admission: got=%+v want=%+v err=%v", program.gateAdmitted, previous, err)
				}
			} else {
				migrated, initialized := false, false
				gateBindObserver = func(prepared *preparedCommand) {
					admission := prepared.gateAdmission()
					switch prepared.Definition.Handler {
					case "@current-migration":
						migrated = admission != nil && admission.kind == admissionExempt
					case "@init":
						initialized = migrated && admission != nil && admission.kind == admissionReady && admission.request.handler == "@init"
					}
				}
				t.Cleanup(func() { gateBindObserver = nil })
				code = program.Run(context.Background())
				if !migrated || !initialized {
					t.Fatalf("migration/init did not bind their own admissions: migrated=%t initialized=%t output=%s", migrated, initialized, output.String())
				}
			}
			if _, err := os.Stat(marker); err != nil {
				t.Fatalf("plain init did not execute existing migration: code=%d output=%s err=%v", code, output.String(), err)
			}
			if code != 0 {
				t.Fatalf("normal init did not complete after migration: code=%d output=%s", code, output.String())
			}
		})
	}
}

func TestPlainInitMigrationProbePropagatesMissingNativeSocket(t *testing.T) {
	for _, absent := range []bool{false, true} {
		t.Run(strconv.FormatBool(absent), func(t *testing.T) {
			cliFixture, root, environment, marker := newMigrateCLIFixture(t, true)
			var output bytes.Buffer
			program := cliFixture.New(Options{RepositoryRoot: root, Program: "yard", Environment: environment, Stdout: &output, Stderr: &output})
			cause := error(&os.PathError{Op: "dial", Path: "/private/socket", Err: os.ErrNotExist})
			if absent {
				cause = ports.ErrInstanceNotFound
			}
			seedOwnerHostID(t, program.env["SUBYARD_CONFIG_HOME"])
			program.options.Incus = &testkit.Incus{Err: cause}
			code, err := program.finishRegisteredInitMigrations(context.Background(), "default", nil, true)
			if absent {
				if err != nil || code != -1 {
					t.Fatal("known missing yard did not use fresh init", code, err)
				}
			} else if !errors.Is(err, cause) {
				t.Fatal("native socket failure was treated as a missing yard", code, err)
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed probe ran migration", err)
			}
		})
	}
}

func TestRegisteredMaintenanceUsesPreparedCLIContext(t *testing.T) {
	cliFixture := newFixture(t, withWorkingDir(""))
	root, environment := cliFixture.Root, cliFixture.Environment
	var output bytes.Buffer
	cli := cliFixture.New(Options{RepositoryRoot: root, Environment: environment, Stdout: &output, Stderr: &output})
	loaded, err := cli.resolveReleaseTransitionContext(context.Background(), "default", cli.env["SUBYARD_CONFIG_HOME"])
	if err != nil {
		t.Fatal(err)
	}
	runtime := cli.initPlatform(loaded, nil).(reconcileruntime.Runtime)
	helper, err := os.ReadFile(filepath.Join(repositoryRoot(t), "scripts/lib/engine-context.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "scripts/lib"), 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, "scripts/lib/engine-context.sh"), helper, 0600)
	testkit.WriteFile(t, filepath.Join(root, "scripts/lifecycle-guard.sh"), []byte(`#!/bin/bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
. "$SCRIPT_DIR/lib/engine-context.sh"
subyard_require_engine_context
[ "$1" = stop ] && [ "$#" = 1 ]
: > "$SUBYARD_HOME/physical-called"
`), 0700)
	marker := filepath.Join(loaded.Context.Paths.DataHome, "physical-called")
	if err := os.MkdirAll(loaded.Context.Paths.DataHome, 0700); err != nil {
		t.Fatal(err)
	}
	if err := runtime.RunMigrationPhysicalLifecycle(context.Background(), "stop"); err != nil {
		t.Fatal("prepared CLI context rejected", err, output.String())
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("physical boundary did not run", err)
	}
}

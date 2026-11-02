package cli

import (
	"bytes"
	"context"
	"errors"
	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/uninstall"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/migration"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
	"golang.org/x/sys/unix"
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

func TestMigrationPromptDoesNotConsumeLaterConsent(t *testing.T) {
	input := strings.NewReader("yes\nno\n")
	prompt := streamPrompt{input: input, output: &bytes.Buffer{}, interactive: func() bool { return true }}
	request := domain.ConfirmationRequest{Summary: "initial migration", Default: domain.ConfirmationDefaultNo}
	if accepted, err := prompt.Confirm(context.Background(), request); err != nil || !accepted {
		t.Fatalf("initial %v %v", accepted, err)
	}
	request.Summary = "separate restart consent"
	if accepted, err := prompt.Confirm(context.Background(), request); accepted || err != domain.ErrOperationDeclined {
		t.Fatalf("late %v %v", accepted, err)
	}
}

func TestPlainInitFinishesCurrentMigrationsBeforeNormalPlan(t *testing.T) {
	root, environment, marker := migrateCLIFixture(t, true)
	platform := newInitPlatformFixture()
	for stage := range platform.converged {
		platform.converged[stage] = true
	}
	var output bytes.Buffer
	program, err := New(Options{RepositoryRoot: root, Program: "yard", Arguments: []string{"init", "--yes"}, Environment: environment, Stdout: &output, Stderr: &output, InitPlatform: platform})
	if err != nil {
		t.Fatal(err)
	}
	loaded, loadErr := program.resolveReleaseTransitionContext("default", program.env["SUBYARD_CONFIG_HOME"])
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	program.options.Incus = &testkit.Incus{Instances: map[string]ports.InstanceInfo{loaded.Context.IncusProject + "/" + loaded.Context.YardInstanceName: {Name: loaded.Context.YardInstanceName, Project: loaded.Context.IncusProject, Config: map[string]string{"user.subyard.managed": "true"}}}}
	code := program.Run(context.Background())
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("plain init did not execute existing migration: code=%d output=%s err=%v", code, output.String(), err)
	}
	if code != 0 {
		t.Fatalf("normal init did not complete after migration: code=%d output=%s", code, output.String())
	}
}

func TestPlainInitMigrationProbePropagatesMissingNativeSocket(t *testing.T) {
	for _, absent := range []bool{false, true} {
		t.Run(strconv.FormatBool(absent), func(t *testing.T) {
			root, environment, marker := migrateCLIFixture(t, true)
			var output bytes.Buffer
			program, err := New(Options{RepositoryRoot: root, Program: "yard", Environment: environment, Stdout: &output, Stderr: &output})
			if err != nil {
				t.Fatal(err)
			}
			cause := error(&os.PathError{Op: "dial", Path: "/private/socket", Err: os.ErrNotExist})
			if absent {
				cause = ports.ErrInstanceNotFound
			}
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

func TestLateMigrationPromptUsesSharedTerminalOnlyWithoutProvidedFD(t *testing.T) {
	if os.Getenv("SUBYARD_TEST_MIGRATION_INPUT_REUSE") == "" {
		child := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$")
		child.Env = append(os.Environ(), "SUBYARD_TEST_MIGRATION_INPUT_REUSE=1")
		if output, err := child.CombinedOutput(); err != nil {
			t.Fatalf("isolated input ownership: %v\n%s", err, output)
		}
		return
	}
	t.Setenv("SUBYARD_MIGRATION_OPERATOR_INPUT_FD", "")
	if err := os.Unsetenv("SUBYARD_MIGRATION_OPERATOR_INPUT_FD"); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	calls := 0
	cli := &CLI{options: Options{Stderr: &output}, openTerminal: func() (*os.File, error) { calls++; return nil, os.ErrNotExist }}
	t.Cleanup(func() {
		if cli.migrationOperatorInput != nil {
			_ = cli.migrationOperatorInput.Close()
		}
	})
	prompt := migrationLatePrompt{cli: cli}
	request := domain.ConfirmationRequest{Summary: "late restart", Default: domain.ConfirmationDefaultNo}
	if yes, err := prompt.Confirm(context.Background(), request); yes || !errors.Is(err, domain.ErrConfirmationRequired) || calls != 1 {
		t.Fatalf("no tty: %t %v %d", yes, err, calls)
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	_ = writer.Close()
	sourceFD := int(reader.Fd())
	// Exec transfers a distinct descriptor; the receiver owns its only wrapper.
	inheritedFD, err := unix.FcntlInt(reader.Fd(), unix.F_DUPFD_CLOEXEC, 3)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cli.migrationOperatorInput == nil {
			_ = unix.Close(inheritedFD)
		}
	})
	t.Setenv("SUBYARD_MIGRATION_OPERATOR_INPUT_FD", strconv.Itoa(inheritedFD))
	if yes, err := prompt.Confirm(context.Background(), request); yes || !errors.Is(err, domain.ErrConfirmationRequired) || calls != 1 {
		t.Fatalf("provided EOF fell back: %t %v %d", yes, err, calls)
	}
	// Reuse the caller's slot before closing the actual prompt-owned wrapper.
	// This exercises its finalizer's close path without depending on GC timing.
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	replacement, err := unix.Open("/dev/null", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	if replacement != sourceFD {
		err = unix.Dup3(replacement, sourceFD, unix.O_CLOEXEC)
		_ = unix.Close(replacement)
		if err != nil {
			t.Fatal(err)
		}
	}
	closeErr := cli.migrationOperatorInput.Close()
	var info unix.Stat_t
	statErr := unix.Fstat(sourceFD, &info)
	_ = unix.Close(sourceFD)
	if closeErr != nil || statErr != nil {
		t.Fatalf("prompt close affected reused caller descriptor: close=%v stat=%v", closeErr, statErr)
	}
}

type maintenanceCommandObserver struct {
	ports.NativeOperationObserver
	starts int
	source string
}

func (o *maintenanceCommandObserver) CommandStarted(_ string, source string) (uint64, error) {
	o.source = source
	o.starts++
	return 1, nil
}
func (*maintenanceCommandObserver) CommandTerminal(uint64) error { return nil }
func (*maintenanceCommandObserver) Uncertain()                   {}

func TestRegisteredMaintenanceUsesPreparedCLIContextAndOriginalOwner(t *testing.T) {
	root, environment, _ := nativeFixture(t)
	var output bytes.Buffer
	cli, err := New(Options{RepositoryRoot: root, Environment: environment, Stdout: &output, Stderr: &output})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := cli.resolveReleaseTransitionContext("default", cli.env["SUBYARD_CONFIG_HOME"])
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
[ "$SUBYARD_NATIVE_OWNER_FD3" = 1 ]
: > "$SUBYARD_HOME/physical-called"
`), 0700)
	observer := &maintenanceCommandObserver{}
	ctx := ports.WithNativeOwnerHome(ports.WithNativeInvocationKey(ports.WithNativeOperationObserver(context.Background(), observer), "prepared-maintenance"), loaded.Context.Paths.DataHome)
	marker := filepath.Join(loaded.Context.Paths.DataHome, "physical-called")
	if err := os.MkdirAll(loaded.Context.Paths.DataHome, 0700); err != nil {
		t.Fatal(err)
	}
	if err := runtime.RunMigrationPhysicalLifecycle(ctx, "stop"); err == nil || observer.starts != 0 {
		t.Fatal("missing original owner admitted", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("missing owner had effects", err)
	}
	_, closeOwner, err := uninstall.OwnerLock(context.Background(), loaded.Context.Paths.DataHome, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeOwner()
	if err := runtime.RunMigrationPhysicalLifecycle(ctx, "stop"); err != nil {
		t.Fatal("prepared CLI context rejected", err, output.String())
	}
	if _, err := os.Stat(marker); err != nil || observer.starts != 1 {
		t.Fatal("physical boundary did not run", err, observer.starts)
	}
}

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/application"
	"github.com/Subyard/Subyard/internal/configsync"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/testkit"
)

type installedIncusAccess struct {
	*testkit.Incus
	marker      string
	unavailable bool
}

func (fixture *installedIncusAccess) Server(context.Context) (ports.ServerInfo, error) {
	if _, err := os.Stat(fixture.marker); err != nil || fixture.unavailable {
		return ports.ServerInfo{}, errors.New("operator socket access is not active")
	}
	return ports.ServerInfo{Version: "6.0.6"}, nil
}

// Execute the real retained init adapter. Only the privileged installer/socket
// are replaced; a dispatcher or sg invocation would fail this contract.
func TestIncusInstallationRetainsPreparedCommandEnvironment(t *testing.T) {
	for _, scenario := range []string{"access restored", "access unavailable", "resource continuation"} {
		t.Run(scenario, func(t *testing.T) {
			root, environment, _ := nativeFixture(t)
			if scenario == "resource continuation" {
				root, environment, _ = bootstrapCommandFixture(t)
			}
			values := environmentMap(environment)
			delete(values, "SSH_PORT")
			values["DEV_UID"] = "2001"
			values["CAPTURE"] = filepath.Join(root, "installed")
			bin := filepath.Join(root, "bin")
			if err := os.MkdirAll(bin, 0700); err != nil {
				t.Fatal(err)
			}
			values["PATH"] = bin + ":" + os.Getenv("PATH")
			for _, name := range []string{"sg", "dispatcher"} {
				testkit.WriteFile(t, filepath.Join(bin, name), []byte("#!/bin/sh\nprintf forbidden > \"$CAPTURE.forbidden\"\nexit 75\n"), 0700)
			}
			testkit.WriteFile(t, filepath.Join(root, "config/subyard.env"), []byte("SSH_PORT=2222\n"), 0600)
			path := filepath.Join(root, "state/yards/demo.env")
			if scenario == "resource continuation" {
				path = filepath.Join(root, "state/yards/demo/config.env")
			}
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, path, []byte("SSH_PORT=2233\nDEV_UID=3001\nCODING_TOOL_INTEGRATIONS=\n"), 0600)
			testkit.WriteFile(t, filepath.Join(root, "scripts/01-install-incus.sh"), []byte("#!/bin/sh\n[ \"$SSH_PORT\" = 2233 ] && [ \"$DEV_UID\" = 2001 ] && [ \"$1\" = --yes ] || exit 72\nprintf installed > \"$CAPTURE\"\n"), 0700)
			native := &installedIncusAccess{Incus: &testkit.Incus{}, marker: values["CAPTURE"], unavailable: scenario == "access unavailable"}
			options := Options{RepositoryRoot: root, DispatcherPath: filepath.Join(bin, "dispatcher"), Program: "yard", Environment: environmentList(values, nil), Incus: native}
			if scenario == "resource continuation" {
				options.InitPlatform = newInitPlatformFixture()
			}
			program, err := New(options)
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := program.resolveContextWithYardSettings("demo", "")
			if err != nil {
				t.Fatal(err)
			}
			platform := program.initPlatform(loaded, nil)
			arguments := []string{"run", "--label=two words '$HOME'", "--", "--yes"}
			if scenario == "resource continuation" {
				definition, _ := program.resources.Lookup("demo")
				bootstrap, err := program.prepareResourceBootstrap(context.Background(), loaded, definition, arguments)
				if err != nil || bootstrap == nil || bootstrap.init == nil {
					t.Fatalf("prepare resource bootstrap: %v", err)
				}
				program.options.InitPlatform = nil
				bootstrap.init.rebuildPlatform(program)
				platform = bootstrap.init.platform
			}
			beforePID := os.Getpid()
			operation := program.ensureOperationID()
			err = platform.ApplyStage(context.Background(), ports.ReconcileStageIncus)
			if scenario == "access unavailable" {
				if err == nil || !strings.Contains(err.Error(), "new assessment") {
					t.Fatalf("unavailable access did not require explicit recovery: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if os.Getpid() != beforePID || program.ensureOperationID() != operation {
				t.Fatal("installer replaced approved parent operation")
			}
			if _, err := os.Stat(values["CAPTURE"]); err != nil {
				t.Fatal("installer did not receive retained context")
			}
			if _, err := os.Stat(values["CAPTURE"] + ".forbidden"); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("installer re-entered a dispatcher or group session")
			}
			if loaded.Environment["DEV_UID"] != "2001" || program.baseEnv["DEV_UID"] != "2001" {
				t.Fatal("installer lost caller override")
			}
			if scenario == "resource continuation" {
				runtime := platform.(reconcileruntime.Runtime)
				if runtime.ResourceCommand != "demo" || !slices.Equal(runtime.ResourceArguments, arguments) {
					t.Fatal("installer changed retained resource arguments")
				}
			}
			yards, err := program.powerYardContexts(loaded)
			if err != nil || len(yards) != 2 || yards[0].SSHPort != 2222 || yards[1].SSHPort != 2233 {
				t.Fatalf("installer polluted independent yard ports: %v", err)
			}
		})
	}
}

func TestIncusPrerequisiteRejectsInstallerDriftBeforeOwnerPublication(t *testing.T) {
	for _, changed := range []string{"unchanged", "configuration", "hook source", "project records"} {
		t.Run(changed, func(t *testing.T) {
			root, environment, _ := nativeFixture(t)
			marker := filepath.Join(root, "installed")
			later := filepath.Join(root, "later")
			projectDir := filepath.Join(root, "state", "projects")
			if err := os.MkdirAll(projectDir, 0700); err != nil {
				t.Fatal(err)
			}
			mutation := "printf '\nSSH_PORT=2223\n' >> '" + filepath.Join(root, "config", "subyard.env") + "'"
			if changed == "unchanged" {
				mutation = ":"
			}
			if changed == "hook source" {
				mutation = "printf '# drift\n' >> '" + filepath.Join(root, "config", "projects-changed.sh") + "'"
			}
			if changed == "project records" {
				// An exact native record appears during the approved dependency.
				mutation = "printf '%s' '{\"schema\":1,\"projectId\":\"project-new\",\"name\":\"new\",\"mode\":\"sync\",\"hostPath\":\"/tmp/new\",\"yardPath\":\"/srv/workspaces/project-new/src\",\"sshHost\":\"yard\",\"target\":\"yard\"}' > '" + filepath.Join(projectDir, "project-new.json") + "'\nchmod 600 '" + filepath.Join(projectDir, "project-new.json") + "'"
			}
			testkit.WriteFile(t, filepath.Join(root, "config", "projects-changed.sh"), []byte("#!/bin/sh\nexit 0\n"), 0700)
			testkit.WriteFile(t, filepath.Join(root, "scripts", "01-install-incus.sh"), []byte("#!/bin/sh\nset -eu\n"+mutation+"\nprintf installed > '"+marker+"'\n"), 0700)
			testkit.WriteFile(t, filepath.Join(root, "scripts", "02-create-project.sh"), []byte("#!/bin/sh\nprintf later > '"+later+"'\nexit 69\n"), 0700)
			native := &installedIncusAccess{Incus: &testkit.Incus{Reconcile: ports.ReconcileState{HostPoolFound: true, HostNetworkFound: true}}, marker: marker}
			program, err := New(Options{RepositoryRoot: root, Environment: environment, Incus: native})
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := program.resolveContextWithYardSettings("", "")
			if err != nil {
				t.Fatal(err)
			}
			runtime := program.initPlatform(loaded, nil).(reconcileruntime.Runtime)
			runtimePlan, err := runtime.PrepareProfileRuntimes(context.Background(), true, false)
			if err != nil {
				t.Fatal(err)
			}
			runtime.RuntimePlan = runtimePlan
			hooks, err := runtime.PrepareProjectHooks(context.Background(), true)
			if err != nil {
				t.Fatal(err)
			}
			baseline, err := program.captureOwnerInputs(loaded, "", nil)
			if err != nil {
				t.Fatal(err)
			}
			execution := &initExecution{loaded: loaded, platform: runtime, inputBaseline: baseline, runtimePlan: runtimePlan, hookPlan: hooks, hookProjects: []domain.ProjectRecord{}}
			execution.hostID, execution.hostIDPending, err = configsync.ResolveHostID(loaded.Context.Paths.ConfigHome, loaded.Environment)
			if err != nil {
				t.Fatal(err)
			}
			for _, stage := range application.InitStages(loaded.Context) {
				execution.approvedPlan.Steps = append(execution.approvedPlan.Steps, application.ReconcileStep{Stage: stage, Conditional: stage.ID != ports.ReconcileStageIncus})
			}
			execution.plan = execution.approvedPlan
			// Exercise the same authorization transition as prepared init execution.
			if err := program.prepareSudoPrivileges(context.Background(), io.Discard, 0, "init"); err != nil {
				t.Fatal(err)
			}
			execution.rebuildPlatform(program)
			attached := execution.platform.(reconcileruntime.Runtime)
			// The synthetic installer provides both server access and native ACL tools.
			attached.ACLToolsAvailable = func() (bool, error) {
				_, err := os.Stat(marker)
				if errors.Is(err, os.ErrNotExist) {
					return false, nil
				}
				return err == nil, err
			}
			execution.platform = attached
			if attached.RuntimePlan != runtimePlan || attached.HookPlan != hooks {
				t.Fatal("native rebuild discarded approved plans")
			}
			beforeEnv, afterEnv := environmentMap(runtime.Environment), environmentMap(attached.Environment)
			delete(afterEnv, "SUBYARD_SUDO_PREAUTHORIZED")
			if operationStateDigest(beforeEnv) != operationStateDigest(afterEnv) {
				t.Fatal("authorization rebuilt business inputs rather than changing only its internal marker")
			}
			steps := execution.operationSteps()
			if len(steps) < 2 || steps[0].ID != "init.stage.incus" || steps[1].DependsOn[0] != steps[0].ID {
				t.Fatal("public steps omit Incus-before-publication dependency")
			}
			err = execution.run(context.Background(), program, io.Discard)
			if changed == "unchanged" {
				if err == nil || !strings.Contains(err.Error(), `apply init stage "project"`) || !strings.Contains(err.Error(), "exit status 69") {
					t.Fatalf("authorized parent did not reach later native stage: %v", err)
				}
				for _, path := range []string{marker, later, filepath.Join(loaded.Context.Paths.ConfigHome, "host-id")} {
					if _, err := os.Stat(path); err != nil {
						t.Fatal("authorized parent did not complete approved prerequisite/publication")
					}
				}
				if !slices.Contains(attached.Environment, "SUBYARD_SUDO_PREAUTHORIZED=1") {
					t.Fatal("real authorization transition was omitted")
				}
				return
			}
			if !errors.Is(err, domain.ErrPlanStale) {
				t.Fatalf("installer %s drift was accepted: %v", changed, err)
			}
			for _, path := range []string{later, filepath.Join(loaded.Context.Paths.ConfigHome, "host-id")} {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("stale prerequisite published %s: %v", filepath.Base(path), err)
				}
			}
			if _, err := os.Stat(marker); err != nil {
				t.Fatal(fmt.Errorf("approved installer did not execute: %w; refusal: %v", err, execution.checkBeforeInitWrites(context.Background(), program)))
			}
		})
	}
}

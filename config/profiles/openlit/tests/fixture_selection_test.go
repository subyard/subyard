package tests

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/testkit"
)

// All non-package observations are already converged. Command lookup and the
// declared installation check execute locally, with no native CLI in PATH.
type configOnlyFixtureExecutor struct{ path, check string }

func (fixture configOnlyFixtureExecutor) Exec(ctx context.Context, _, _ string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	arguments := request.Command
	if len(arguments) > 0 && arguments[0] == "python3" {
		return ports.InstanceExecResult{Stdout: []byte(`{"fingerprint":"` + strings.Repeat("a", 64) + `","changed":false}`)}, nil
	}
	if len(arguments) == 1 && (arguments[0] == fixture.check || arguments[0] == "true") ||
		len(arguments) == 5 && arguments[0] == "sh" && arguments[1] == "-c" {
		executable := arguments[0]
		if executable == fixture.check {
			executable = filepath.Join(fixture.path, executable)
		}
		command := exec.CommandContext(ctx, executable, arguments[1:]...)
		command.Env = []string{"PATH=" + fixture.path}
		output, err := command.Output()
		result := ports.InstanceExecResult{Stdout: output}
		if err != nil {
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				return result, err
			}
			result.ExitCode = exit.ExitCode()
		}
		return result, err
	}
	return ports.InstanceExecResult{Stdout: []byte("changed=0\n")}, nil
}

func TestOpenLITConfigurationOnlyFixtureDoesNotClaimNativePackageReadiness(t *testing.T) {
	root := testkit.TempDir(t)
	if err := os.Mkdir(filepath.Join(root, "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, "config", "projects-changed.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o644)
	provision, check := filepath.Join(root, "provision.sh"), filepath.Join(root, "native-check")
	testkit.WriteFile(t, provision, []byte("#!/bin/sh\nexit 0\n"), 0o600)
	testkit.WriteFile(t, check, []byte("#!/bin/sh\nexit 1\n"), 0o700)
	// The same ordinary persistent overrides used before physical init must
	// retain both selected configurations while changing only package probes.
	configHome := testkit.TempDir(t)
	yardRoot := filepath.Join(configHome, "yards", "fixture")
	if err := os.MkdirAll(yardRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(yardRoot, "config.env"), []byte("CODING_TOOL_INTEGRATIONS='claude codex'\nAGENT_codex_PROVISION="+provision+"\nAGENT_codex_COMMAND=true\nAGENT_codex_CHECK=true\n"), 0o600)
	loaded, err := config.Load(config.LoadOptions{
		RepositoryRoot: filepath.Clean(filepath.Join("..", "..", "..", "..")),
		OperatorHome:   testkit.TempDir(t), YardName: "fixture", DisablePrivate: true,
		Environment: map[string]string{"SUBYARD_CONFIG_HOME": configHome},
	})
	if err != nil || loaded.Environment["CODING_TOOL_INTEGRATIONS"] != "claude codex" ||
		loaded.Environment["AGENT_codex_PROVISION"] != provision || loaded.Environment["AGENT_codex_COMMAND"] != "true" || loaded.Environment["AGENT_codex_CHECK"] != "true" {
		t.Fatalf("configuration-only selection was not resolved: %v", err)
	}
	runtime := reconcileruntime.Runtime{
		RepositoryRoot: root, Integrations: []profile.IntegrationDefinition{},
		Incus:    &testkit.Incus{Reconcile: ports.ReconcileState{InstanceFound: true, Instance: ports.InstanceInfo{Status: "Running"}}},
		Executor: configOnlyFixtureExecutor{path: root, check: "native-check"},
		Yard:     domain.Context{IncusProject: "fixture", YardInstanceName: "fixture", DevUser: "dev", DevUID: os.Getuid()},
	}
	for _, scenario := range []struct {
		name, command, check string
		changed              bool
	}{
		{"missing native command", "absent-native-codex", "true", true},
		{"failed native package check", "true", "native-check", true},
		{"declared configuration-only probes", "true", "true", false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			runtime.Environment = []string{"CODING_TOOL_INTEGRATIONS=codex", "AGENT_codex_PROVISION=" + provision,
				"AGENT_codex_COMMAND=" + scenario.command, "AGENT_codex_CHECK=" + scenario.check}
			plan, err := runtime.IntegrationPlan(context.Background())
			if err != nil || plan.Changed != scenario.changed {
				t.Fatalf("installation plan changed=%t, want %t, err=%v", plan.Changed, scenario.changed, err)
			}
		})
	}
}

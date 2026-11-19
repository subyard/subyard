package reconcileruntime

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/configmaterial"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestIntegrationPlanReportsStructuredConfigAndBubblewrapRefusals(t *testing.T) {
	runtime, definition, _, _ := integrationSourceFixture(t)
	path := filepath.Join(definition.Root, "config.toml")
	testkit.WriteFile(t, path, []byte("managed = 'new'\n"), 0644)
	definition.Artifacts = []profile.IntegrationArtifact{{ID: "config", Kind: "config", Source: "config.toml", Destination: ".sample/config.toml", Format: "toml"}}
	runtime.Integrations = []profile.IntegrationDefinition{definition}
	runtime.Environment = []string{"CODING_TOOL_INTEGRATIONS=sample", "AGENT_sample_CONFIG=" + path}
	runtime.Yard = domain.Context{YardName: "sample", IncusProject: "test", YardInstanceName: "yard", DevUser: "dev", DevUID: os.Getuid()}
	// Substrate, inventory, structured config, project hook probe, services.
	incus := runningIncus(5)
	incus.ExecSteps[1].Result.Stdout = []byte(`{"fingerprint":"` + strings.Repeat("a", 64) + `"}`)
	incus.ExecSteps[2].Result = ports.InstanceExecResult{ExitCode: 2, Stderr: []byte("config materialization: invalid current TOML\n")}
	incus.ExecSteps[2].Err = &ports.GuestExitError{Code: 2}
	const policyRefusal = bubblewrapRefusalText
	incus.ExecSteps[4].Result = ports.InstanceExecResult{ExitCode: 1, Stderr: []byte(policyRefusal + "\n")}
	runtime.Incus, runtime.Executor = incus, incus
	_, err := runtime.IntegrationPlan(context.Background())
	var refusals *domain.Refusals
	if !errors.As(err, &refusals) {
		t.Fatalf("expected both independent refusals: %v", err)
	}
	causes := refusals.Unwrap()
	if len(causes) != 2 || causes[0].Error() != policyRefusal || !strings.Contains(causes[1].Error(), "/home/dev/.sample/config.toml") {
		t.Fatalf("expected both refusals in stable order: %v", err)
	}
}

func TestUnrecordedStructuredConfigDiagnosticPreservesOwnershipRefusal(t *testing.T) {
	for _, kind := range []string{"completed", "exit-only", "edited", "invalid-json", "invalid-toml", "transport", "unknown-stderr"} {
		t.Run(kind, func(t *testing.T) {
			root := testkit.TempDir(t)
			home := filepath.Join(root, "home")
			path := "/home/dev/.fixture/config.toml"
			physicalPath := filepath.Join(home, ".fixture/config.toml")
			format, desired := "toml", []byte("managed = 'new'\n")
			incus := runningIncus(1)
			step := &incus.ExecSteps[0]
			step.Result.ExitCode = 2
			step.Result.Stderr = []byte("config materialization: adoption ownership evidence unavailable\n")
			step.Err = &ports.GuestExitError{Code: 2}
			wantDetail := "recognized legacy template"
			switch kind {
			case "exit-only":
				step.Err = nil
			case "transport":
				step.Err = errors.New("PRIVATE transport failure")
			case "unknown-stderr":
				step.Result.Stderr = []byte("PRIVATE guest content")
			case "edited":
				wantDetail = "owned fields or protected evidence changed"
				step.Result.Stderr = []byte("config materialization: adoption owned fields or evidence changed\n")
			case "invalid-json", "invalid-toml":
				wantDetail = "invalid current " + strings.ToUpper(strings.TrimPrefix(kind, "invalid-"))
				if kind == "invalid-json" {
					format, desired = "json", []byte(`{"managed":true}`)
				}
				if err := os.MkdirAll(filepath.Dir(physicalPath), 0755); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, physicalPath, []byte("[broken"), 0644)
				request, err := configmaterial.Request(format, configmaterial.ModeAssessAdopt, "dev", path, os.Getuid(), desired)
				if err != nil {
					t.Fatal(err)
				}
				request.Command[3] = strings.ReplaceAll(request.Command[3], "/var/lib/subyard/config-materialization", filepath.Join(root, "absent-state"))
				request.Command[6], request.Command[9] = physicalPath, home
				command := exec.Command(request.Command[0], request.Command[1:]...)
				command.Stdin = bytes.NewReader(request.Stdin)
				output, err := command.CombinedOutput()
				if err == nil || command.ProcessState.ExitCode() != 2 {
					t.Fatal("real guest did not refuse invalid unowned document", err, string(output))
				}
				step.Result.Stderr = output
			}
			runtime := Runtime{Executor: incus, Yard: domain.Context{
				DevUser: "dev", DevUID: 1000, YardName: "sample", IncusProject: "sample-project", YardInstanceName: "sample-instance",
			}}
			_, err := runtime.observeIntegrationConfig(context.Background(), format, configmaterial.ModeAssessAdopt, path, desired)
			if err == nil || strings.Contains(err.Error(), "PRIVATE") {
				t.Fatal("ownership refusal lost or leaked private data")
			}
			var diagnostic interface{ ActivationDiagnostic() (string, string) }
			public := errors.As(err, &diagnostic)
			if public != (kind != "transport" && kind != "unknown-stderr") {
				t.Fatal("uncertain or arbitrary native error became a public ownership diagnosis")
			}
			// Only a refusal the guest program returned reports ownership state.
			if IntegrationOwnershipConflict(err) != (kind != "transport") {
				t.Fatalf("ownership conflict classification for %s: %v", kind, err)
			}
			var pending interface{ PendingIntegrationConfiguration() bool }
			isPending := errors.As(err, &pending) && pending.PendingIntegrationConfiguration()
			if isPending != (kind == "completed" || kind == "exit-only" || kind == "edited") {
				t.Fatal("unavailable or malformed evidence became pending configuration")
			}
			if public {
				message, retry := diagnostic.ActivationDiagnostic()
				if !strings.Contains(message, wantDetail) || !strings.Contains(message, path) ||
					!strings.Contains(retry, "'"+path+"'") ||
					(strings.HasPrefix(kind, "invalid-") && !strings.Contains(retry, "'mv' '-T' '--backup=numbered'")) ||
					(!strings.HasPrefix(kind, "invalid-") && !strings.Contains(retry, "'sha256sum'")) {
					t.Fatalf("unactionable ownership diagnostic: %s; %s", message, retry)
				}
			}
		})
	}
}

func TestLifecycleDiagnosticsDoNotRenderNativeOutput(t *testing.T) {
	runtime, definition, _, _ := integrationSourceFixture(t)
	runtime.Yard.YardName = "sample-yard"
	var rendered bytes.Buffer
	runtime.Stderr = &rendered
	for _, phase := range []string{"inspect", "apply", "verify"} {
		failure := runtime.integrationDiagnostic(errors.New("PRIVATE=value /private/native"), definition, phase, "foreign-ownership")
		var diagnostic interface{ ActivationDiagnostic() (string, string) }
		if !errors.As(failure, &diagnostic) {
			t.Fatal("missing release diagnostic")
		}
		message, retry := diagnostic.ActivationDiagnostic()
		if !strings.Contains(message, "sample "+phase+" in yard sample-yard") || retry != "yard -Y sample-yard integration status sample --json" || strings.Contains(message+retry+failure.Error(), "PRIVATE") {
			t.Fatalf("unsafe/incomplete diagnostic: %s / %s", message, retry)
		}
	}
	stale := runtime.integrationDiagnostic(errors.New("native detail"), definition, "apply", "observation-changed")
	if !errors.Is(stale, domain.ErrPlanStale) {
		t.Fatal("stale guard classification lost")
	}
}

func assertIntegrationDiagnostic(t *testing.T, err error, phase, reason string) {
	t.Helper()
	var diagnostic interface{ ActivationDiagnostic() (string, string) }
	if !errors.As(err, &diagnostic) {
		t.Fatalf("missing public integration diagnostic: %v", err)
	}
	message, retry := diagnostic.ActivationDiagnostic()
	if !strings.Contains(message, "integration sample "+phase+" in yard sample-yard") || !strings.Contains(message, lifecycleReasons[reason]) || retry != "yard -Y sample-yard integration status sample --json" {
		t.Fatalf("diagnostic = %q / %q", message, retry)
	}
}

func TestIntegrationScopeMissingNativeProducesEarlyDiagnostic(t *testing.T) {
	for _, mode := range []os.FileMode{0, 0644} {
		runtime, definition, _, _ := integrationSourceFixture(t)
		runtime.Yard.YardName = "sample-yard"
		definition.Native = []profile.Native{{Package: "cmd/producer", Path: "bin/producer"}}
		runtime.Integrations = []profile.IntegrationDefinition{definition}
		if mode != 0 {
			if err := os.Mkdir(filepath.Join(definition.Root, "bin"), 0755); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, filepath.Join(definition.Root, "bin/producer"), []byte("private helper"), mode)
		}
		_, _, err := runtime.IntegrationScope()
		assertIntegrationDiagnostic(t, err, "inspect", "native-unavailable")
		if strings.Contains(err.Error(), definition.Root) || strings.Contains(err.Error(), "private helper") {
			t.Fatal("early diagnostic exposed private data")
		}
	}
}

func TestApplyIntegrationsRejectsIdentifiedDriftBeforeEffects(t *testing.T) {
	for _, change := range []string{"helper-bytes", "helper-unavailable"} {
		t.Run(change, func(t *testing.T) {
			runtime, definition, _, _ := integrationSourceFixture(t)
			for _, directory := range []string{"bin", "config"} {
				if err := os.MkdirAll(filepath.Join(definition.Root, directory), 0755); err != nil {
					t.Fatal(err)
				}
			}
			testkit.WriteFile(t, filepath.Join(definition.Root, "config/projects-changed.sh"), []byte("#!/bin/sh\nexit 0\n"), 0644)
			native := filepath.Join(definition.Root, "bin/producer")
			testkit.WriteFile(t, native, []byte("native-v1"), 0755)
			definition.Native = []profile.Native{{Package: "cmd/producer", Path: "bin/producer"}}
			runtime.Integrations = []profile.IntegrationDefinition{definition}
			runtime.RepositoryRoot = definition.Root
			runtime.Yard = domain.Context{YardName: "sample-yard", IncusProject: "test", YardInstanceName: "yard", DevUser: "dev", DevUID: os.Getuid()}
			incus := &testkit.Incus{Reconcile: ports.ReconcileState{InstanceFound: true, Instance: ports.InstanceInfo{Status: "Running", Config: map[string]string{}, Devices: map[string]map[string]string{}}}}
			executor := &retryableIntegrationExecutor{}
			runtime.Incus, runtime.Executor = incus, executor
			plan, err := runtime.IntegrationPlan(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if change == "helper-bytes" {
				testkit.WriteFile(t, native, []byte("native-v2"), 0755)
			} else if change == "helper-unavailable" {
				testkit.WriteFile(t, native, []byte("native-v1"), 0644)
			}
			before, err := os.ReadFile(filepath.Join(definition.Root, "state"))
			if err != nil {
				t.Fatal(err)
			}
			executor.requests = nil
			err = runtime.ApplyIntegrations(context.Background(), plan)
			if change != "helper-unavailable" && !errors.Is(err, domain.ErrPlanStale) {
				t.Fatalf("drift lost stale guard: %v", err)
			}
			reason := "observation-changed"
			if change == "helper-unavailable" {
				reason = "native-unavailable"
			}
			assertIntegrationDiagnostic(t, err, "apply", reason)
			after, readErr := os.ReadFile(filepath.Join(definition.Root, "state"))
			if readErr != nil || !bytes.Equal(before, after) || executor.pending || executor.commits != 0 || executor.hookAttempts != 0 || len(incus.ConfigUpdates) != 0 {
				t.Fatal("stale apply performed effects")
			}
			for _, request := range executor.requests {
				if len(request.Command) >= 5 && request.Command[0] == "python3" && request.Command[4] != "observe" || strings.Contains(strings.Join(request.Command, " "), "reconcile-integrations.sh") {
					t.Fatal("stale apply crossed inventory/reconcile effect boundary")
				}
			}
		})
	}
}

func TestIntegrationDiagnosticPreservesExistingSafeCause(t *testing.T) {
	runtime, definition, _, _ := integrationSourceFixture(t)
	cause := integrationOwnershipError{message: "known ownership conflict", retry: "yard integration status --json"}
	if err := runtime.integrationDiagnostic(cause, definition, "inspect", "native-unavailable"); err != cause {
		t.Fatal("existing safe ownership diagnostic replaced")
	}
	err := runtime.integrationPlanDiagnostic(domain.ErrPlanStale, IntegrationPlan{}, IntegrationPlan{})
	if !errors.Is(err, domain.ErrPlanStale) || !strings.HasPrefix(err.Error(), "integration lifecycle apply:") {
		t.Fatalf("unidentified drift fallback = %v", err)
	}
	runtime.Yard.YardName = "sample-yard"
	err = runtime.integrationPlanDiagnostic(domain.ErrPlanStale, IntegrationPlan{}, IntegrationPlan{})
	var diagnostic interface{ ActivationDiagnostic() (string, string) }
	if !errors.As(err, &diagnostic) {
		t.Fatal("unidentified drift lacks safe diagnostic")
	}
	_, retry := diagnostic.ActivationDiagnostic()
	if retry != "yard -Y sample-yard integration status --json" {
		t.Fatalf("unidentified component inspection lost its yard: %s", retry)
	}
}

func integrationSourceFixture(t *testing.T) (Runtime, profile.IntegrationDefinition, string, string) {
	t.Helper()
	root := testkit.TempDir(t)
	if err := os.Mkdir(filepath.Join(root, "config"), 0700); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, filepath.Join(root, "config/projects-changed.sh"), []byte("#!/bin/sh\nexit 0\n"), 0755)
	testkit.WriteFile(t, filepath.Join(root, "integration.json"), []byte(`{"schema_version":1}`), 0644)
	testkit.WriteFile(t, filepath.Join(root, "state"), []byte("unchanged"), 0600)
	definition := profile.IntegrationDefinition{Name: "sample", Root: root}
	return Runtime{RepositoryRoot: root, Integrations: []profile.IntegrationDefinition{definition}}, definition, "", ""
}

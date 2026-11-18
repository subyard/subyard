package reconcileruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/configmaterial"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestOwnershipRecoveryPreservesEveryConflict(t *testing.T) {
	root := testkit.TempDir(t)
	testkit.WriteFile(t, filepath.Join(root, "incus"), []byte("#!/bin/sh\nshift 5\nexec \"$@\"\n"), 0700)
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	path := filepath.Join(root, strings.Repeat("x", 249)+"&copy")
	runtime := Runtime{Yard: domain.Context{YardInstanceName: "yard", IncusProject: "subyard"}}
	command := runtime.preserveOwnedArtifactCommand(path)
	for _, content := range []string{"first conflict", "second conflict"} {
		testkit.WriteFile(t, path, []byte(content), 0600)
		if output, err := exec.Command("sh", "-c", command).CombinedOutput(); err != nil {
			t.Fatalf("preserve artifact: %v\n%s", err, output)
		}
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("conflicting artifact was left in place: %v", err)
		}
	}
	backups, err := filepath.Glob(filepath.Join(root, ".subyard-preserved-*", filepath.Base(path)))
	if err != nil || len(backups) != 2 {
		t.Fatalf("preservation destinations: %v / %v", backups, err)
	}
	var saved []string
	for _, backup := range backups {
		payload, err := os.ReadFile(backup)
		if err != nil {
			t.Fatal(err)
		}
		saved = append(saved, string(payload))
	}
	slices.Sort(saved)
	if !slices.Equal(saved, []string{"first conflict", "second conflict"}) {
		t.Fatalf("preserved data: %q", saved)
	}
}

func TestIntegrationPlanReportsStructuredConfigAndBubblewrapRefusals(t *testing.T) {
	runtime, definition, _, _ := integrationSourceFixture(t)
	path := filepath.Join(definition.Root, "config.toml")
	testkit.WriteFile(t, path, []byte("managed = 'new'\n"), 0644)
	definition.Artifacts = []profile.IntegrationArtifact{{ID: "config", Kind: "config", Source: "config.toml", Destination: ".sample/config.toml", Format: "toml"}}
	runtime.Integrations = []profile.IntegrationDefinition{definition}
	runtime.Environment = []string{"CODING_TOOL_INTEGRATIONS=sample", "AGENT_sample_CONFIG=" + path}
	runtime.Yard = domain.Context{YardName: "sample", IncusProject: "test", YardInstanceName: "yard", DevUser: "dev", DevUID: os.Getuid()}
	incus := runningIncus(5)
	incus.ExecSteps[1].Result.Stdout = []byte(`{"fingerprint":"` + strings.Repeat("a", 64) + `"}`)
	incus.ExecSteps[2].Result = ports.InstanceExecResult{ExitCode: 2, Stderr: []byte("config materialization: invalid current TOML\n")}
	incus.ExecSteps[2].Err = &ports.GuestExitError{Code: 2}
	incus.ExecSteps[4].Result = ports.InstanceExecResult{ExitCode: 1, Stderr: []byte("Bubblewrap AppArmor: private-native-policy-path\n")}
	runtime.Incus, runtime.Executor = incus, incus
	_, err := runtime.IntegrationPlan(context.Background())
	var refusals *domain.Refusals
	if !errors.As(err, &refusals) {
		t.Fatalf("expected both independent refusals: %v", err)
	}
	causes := refusals.Unwrap()
	if len(causes) != 2 || !strings.HasPrefix(causes[0].Error(), bubblewrapPolicyRefusal) || strings.Contains(err.Error(), "private-native-policy-path") || !strings.Contains(causes[1].Error(), "/home/dev/.sample/config.toml") {
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
					(strings.HasPrefix(kind, "invalid-") && (!strings.Contains(retry, "--backup=numbered") || !strings.Contains(retry, ".subyard-invalid"))) ||
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
	cause := integrationOwnershipFailure("known ownership conflict", "yard integration status --json", true)
	err := runtime.integrationDiagnostic(cause, definition, "inspect", "native-unavailable")
	var public interface{ ActivationDiagnostic() (string, string) }
	var pending interface{ PendingIntegrationConfiguration() bool }
	if !errors.As(err, &public) || !errors.Is(err, cause) || !IntegrationOwnershipConflict(err) ||
		!errors.As(err, &pending) || !pending.PendingIntegrationConfiguration() {
		t.Fatalf("scope lost typed cause or classifications: %v", err)
	}
	message, retry := public.ActivationDiagnostic()
	if message != "integration sample inspect: "+cause.Message || retry != cause.Recovery {
		t.Fatalf("diagnostic = %q / %q", message, retry)
	}
	err = runtime.IntegrationPlanDiagnostic(domain.ErrPlanStale, IntegrationPlan{}, IntegrationPlan{}, false)
	if !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("unidentified drift fallback = %v", err)
	}
	check := integrationFact{Key: "check:fixture-check", Desired: "ready", Observed: "false",
		item: runtime.integrationRead("inspect", "package check", "sample")}
	approved := IntegrationPlan{Fingerprint: "approved", Scope: "scope", Changed: true, facts: []integrationFact{check}}
	check.Observed, check.AtDesired = "true", true
	fresh := IntegrationPlan{Fingerprint: "fresh", Scope: "scope", facts: []integrationFact{check}}
	if err := CheckIntegrationPlan(approved, fresh); err != nil {
		t.Fatalf("projected guard rejected an improvement: %v", err)
	}
	err = runtime.IntegrationPlanDiagnostic(domain.ErrPlanStale, approved, fresh, true)
	finding, ok := domain.PublicFinding(err)
	if !ok || finding.Owner != "sample" || !strings.Contains(finding.Message, "package check") || !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("exact guard lost the owner of a now-ready check: %v", err)
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

type applyDiagnosticExecutor struct {
	retryableIntegrationExecutor
	cut             string
	applyStarted    bool
	persistApplied  bool
	persistReads    int
	verifiedReads   int
	captureAttempts int
}

func (fixture *applyDiagnosticExecutor) Exec(ctx context.Context, project, instance string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	args := request.Command
	point := ""
	if len(args) >= 5 && args[0] == "python3" {
		switch args[3] {
		case IntegrationInventoryReader():
			point = "inventory " + args[4]
			if args[4] == "capture-settings" {
				fixture.captureAttempts++
			}
			if args[4] == "settings" {
				return ports.InstanceExecResult{Stdout: []byte(`{}`)}, nil
			}
			if args[4] == "observe" && fixture.commits != 0 {
				point = "verify"
				fixture.verifiedReads++
			}
			if point == "verify" && (fixture.cut == "verify inventory drift" || fixture.cut == "verify inventory drift with check") {
				payload, _ := json.Marshal(integrationObservation{Fingerprint: strings.Repeat("a", 64), Changed: true,
					Facts: []integrationFact{{Key: "file:/home/dev/.codex/AGENTS.md", Desired: "present", Observed: "missing"}}})
				return ports.InstanceExecResult{Stdout: payload}, nil
			}
			if point == "inventory apply" && (fixture.cut == "inventory conflict" || fixture.cut == "inventory interrupted") {
				report := []byte(`{"reason":"owned artifact drift","path":"/home/dev/.codex/AGENTS.md","detail":"private-native-stderr"}`)
				if fixture.cut == "inventory interrupted" {
					return ports.InstanceExecResult{Stderr: report}, context.Canceled
				}
				return ports.InstanceExecResult{ExitCode: 2, Stderr: report}, nil
			}
			if (fixture.cut == "retire structured config" || fixture.cut == "retire undeclared structured config") && args[4] == "observe" {
				destination := "/home/dev/settings.json"
				if fixture.cut == "retire undeclared structured config" {
					destination = "/home/dev/private-native-retired.json"
				}
				payload, _ := json.Marshal(integrationObservation{Fingerprint: strings.Repeat("a", 64), Changed: true,
					Retired: []integrationArtifact{{ID: "sample", Kind: "structured", Format: "json", Path: destination}}})
				return ports.InstanceExecResult{Stdout: payload}, nil
			}
		case integrationPersistProgram:
			if args[4] == "observe" {
				fixture.persistReads++
				if fixture.applyStarted && (fixture.cut == "persist observe" || fixture.cut == "source and persist") {
					return ports.InstanceExecResult{}, errors.New("private-native-error")
				}
				observed := map[string]any{"fingerprint": strings.Repeat("b", 64)}
				if !fixture.persistApplied || fixture.cut == "persist verify drift" || fixture.cut == "persist capture drift" && fixture.verifiedReads != 0 {
					observed["entries"] = []map[string]any{{"source": "/home/dev/.memory", "ops": []string{"finish"}}}
				}
				payload, _ := json.Marshal(observed)
				return ports.InstanceExecResult{Stdout: payload}, nil
			}
			point = "persist"
			fixture.persistApplied = true
			if fixture.cut == "persist progress" {
				return ports.InstanceExecResult{Stdout: []byte(`{"results":[{"source":"/home/dev/.memory","retained":["private-native-stderr"],"not_transferred":["private-native-file"]}]}`)}, nil
			}
			if fixture.cut != point {
				return ports.InstanceExecResult{Stdout: []byte("{\"results\":[]}")}, nil
			}
		default:
			if args[4] == configmaterial.ModeObserve {
				if fixture.commits != 0 && fixture.cut == "structured verify" {
					return ports.InstanceExecResult{ExitCode: 2, Stdout: []byte("private-native-stdout"),
						Stderr: []byte("config materialization: invalid current JSON\n")}, &ports.GuestExitError{Code: 2}
				}
				if fixture.applyStarted && fixture.cut == "structured observe" {
					return ports.InstanceExecResult{Stdout: []byte("private-native-stdout"), Stderr: []byte("private-native-stderr")}, errors.New("private-native-error")
				}
				return ports.InstanceExecResult{Stdout: []byte(`{"converged":true,"fingerprint":"` + strings.Repeat("c", 64) + `"}`)}, nil
			}
			if args[4] == configmaterial.ModeApply && (fixture.cut == "structured apply" || fixture.cut == "structured apply long") {
				return ports.InstanceExecResult{ExitCode: 2, Stderr: []byte("config materialization: unowned-invalid-destination-v1 JSON; private-native-document")}, nil
			}
			if args[4] == configmaterial.ModeAssessRetire {
				return ports.InstanceExecResult{Stdout: []byte(`{"converged":false,"fingerprint":"` + strings.Repeat("c", 64) + `"}`)}, nil
			}
			if args[4] == configmaterial.ModeRetire {
				if fixture.cut == "retire undeclared structured config" {
					return ports.InstanceExecResult{ExitCode: 2, Stderr: []byte("config materialization: invalid current JSON")}, &ports.GuestExitError{Code: 2}
				}
				point = "retire structured config"
			}
		}
	}
	if len(args) == 4 && args[3] == "--observe" {
		point = "hooks run"
	}
	if (fixture.cut == "hooks release" || fixture.cut == "profile hook release") && len(args) > 2 && strings.Contains(args[2], "exec timeout") {
		if request.Environment["SUBYARD_PROJECT_HOOK_REPORT"] != "1" {
			return ports.InstanceExecResult{ExitCode: 1}, nil
		}
		return ports.InstanceExecResult{ExitCode: 1, Stdout: []byte("subyard-project-hook-failed:/usr/local/bin/fixture-hook\nsubyard-project-hook-failed:/usr/local/libexec/subyard/projects-changed.d/fixture-profile\nsubyard-project-hook-failed:/private-native-hook\n")}, nil
	}
	if fixture.applyStarted && fixture.cut == "stale package check" &&
		(slices.Equal(args, []string{"private-native-check"}) || len(args) > 3 && strings.Contains(args[3], "dispatcher=$3")) {
		return ports.InstanceExecResult{ExitCode: 1}, &ports.GuestExitError{Code: 1}
	}
	if point == "inventory apply" && fixture.cut == "persist progress" {
		return ports.InstanceExecResult{}, errors.New("private-native-error")
	}
	if point == "inventory observe" && !fixture.applyStarted {
		point = ""
	}
	if len(args) > 3 && strings.Contains(args[3], "temporary=$(mktemp") {
		point = "configs"
	}
	if fixture.commits != 0 {
		switch {
		case len(args) == 5 && args[4] == "private-native-command":
			point = "verify command"
		case len(args) == 1 && args[0] == "private-native-check":
			point = "verify check"
			if fixture.verifiedReads == 2 {
				switch fixture.cut {
				case "capture check exit":
					point = "capture check"
				case "capture check transport":
					return ports.InstanceExecResult{ExitCode: 1}, errors.New("private-native-error")
				}
			}
		case len(args) > 3 && strings.Contains(args[3], "dispatcher=$3"):
			point = "verify project hooks"
		case len(args) > 5 && args[5] == "observe" && strings.Contains(args[3], "mode=$1"):
			point = "verify services"
		}
	}
	if point != "" && fixture.cut == point+" exit" {
		result := ports.InstanceExecResult{ExitCode: 1, Stdout: []byte("private-native-stdout"), Stderr: []byte("private-native-stderr")}
		if point == "verify command" {
			return result, nil
		}
		return result, &ports.GuestExitError{Code: 1}
	}
	if fixture.cut == "verify independent refusals" {
		switch point {
		case "verify", "verify check", "verify project hooks":
			return ports.InstanceExecResult{ExitCode: 1}, errors.New("private-native-error")
		case "verify services":
			return ports.InstanceExecResult{ExitCode: 1, Stderr: []byte("subyard-integration-conflict:ccusage\nprivate-native-stderr")}, &ports.GuestExitError{Code: 1}
		}
	}
	if point == fixture.cut || point == "verify check" && (fixture.cut == "verify check init" || fixture.cut == "verify inventory drift with check") {
		return ports.InstanceExecResult{Stdout: []byte("private-native-stdout"), Stderr: []byte("private-native-stderr")}, errors.New("private-native-error")
	}
	return fixture.retryableIntegrationExecutor.Exec(ctx, project, instance, request)
}

func TestIntegrationApplySubstepDiagnostics(t *testing.T) {
	for _, test := range []struct{ cut, message string }{
		{"persist", "integration sample apply persist failed"},
		{"persist observe", "integration sample apply observe persist failed"},
		{"source and persist", "integration claude apply observe config source failed"},
		{"persist progress", "integration apply inventory failed"},
		{"inventory observe", "integration apply observe inventory failed"},
		{"inventory apply", "integration apply inventory failed"},
		{"inventory interrupted", "integration apply inventory failed"},
		{"inventory conflict", "integration codex apply inventory: integration ownership conflict in yard sample"},
		{"inventory commit", "integration apply inventory commit failed"},
		{"inventory capture-settings", "integration apply inventory capture settings failed"},
		{"configs", "integration codex apply configs failed"},
		{"config package source", "integration codex apply configs failed"},
		{"same-owner config sources", "integration codex apply configs failed"},
		{"long config sources", "integration codex apply observe config source failed"},
		{"capture source", "integration codex apply inventory capture settings failed"},
		{"stale package check", "integration second apply observe package check failed"},
		{"plain config sources", "integration codex apply observe config source failed"},
		{"script", "integration sample apply script failed"},
		{"script init", "integration sample apply script failed"},
		{"hooks run", "integration apply hooks run failed"},
		{"hooks release", "integration sample apply hooks run failed"},
		{"profile hook release", "integration fixture-profile apply hooks run failed"},
		{"hooks capture", "integration sample apply hooks capture failed"},
		{"retire undeclared structured config", "integration sample apply retire structured config failed"},
		{"retire structured config", "integration sample apply retire structured config failed"},
		{"verify", "integration apply verify inventory failed"},
		{"verify command", "integration second apply verify command failed"},
		{"verify check", "integration second apply verify package check failed"},
		{"verify command exit", "integration second apply verify command failed"},
		{"verify check exit", "integration second apply verify package check failed"},
		{"verify check init", "integration second apply verify package check failed"},
		{"capture check exit", "integration second apply verify package check failed"},
		{"capture check transport", "integration second apply verify package check failed"},
		{"verify inventory drift", "integration codex apply verify inventory failed"},
		{"verify inventory drift with check", "integration codex apply verify inventory failed"},
		{"verify independent refusals", "integration second apply verify package check failed"},
		{"structured observe", "integration codex apply observe structured config failed"},
		{"structured verify", "integration codex apply verify structured config: integration ownership conflict in yard sample"},
		{"structured apply long", "unowned invalid configuration preserved"},
		{"structured apply", "unowned invalid configuration preserved"},
		{"persist approval", "integration sample apply persist approval: persist transfer state in yard sample changed after the init plan was confirmed; nothing was transferred"},
		{"persist verify drift", "integration sample apply verify persist failed"},
		{"persist capture drift", "integration sample apply verify persist failed"},
	} {
		t.Run(test.cut, func(t *testing.T) {
			root := testkit.TempDir(t)
			for _, dir := range []string{"config", "scripts/lib"} {
				if err := os.MkdirAll(filepath.Join(root, dir), 0700); err != nil {
					t.Fatal(err)
				}
			}
			testkit.WriteFile(t, filepath.Join(root, "config/projects-changed.sh"), []byte("#!/bin/sh\nexit 0\n"), 0755)
			testkit.WriteFile(t, filepath.Join(root, "integration.json"), []byte("{}"), 0600)
			script := []byte("#!/bin/sh\nexit 0\n")
			executor := &applyDiagnosticExecutor{cut: test.cut}
			executor.serviceChanged = true
			var output bytes.Buffer
			runtime := Runtime{RepositoryRoot: root, Executor: executor, PersistFinishOnly: true, Stderr: &output, Stdout: &output,
				Incus:        &testkit.Incus{Reconcile: ports.ReconcileState{InstanceFound: true, Instance: ports.InstanceInfo{Status: "Running"}}},
				Yard:         domain.Context{YardName: "sample", DevUser: "dev", DevUID: 1000},
				Integrations: []profile.IntegrationDefinition{{Name: "sample", Root: root}}}
			configDestination := "/home/dev/.codex/settings.json"
			switch test.cut {
			case "retire structured config":
				runtime.Environment = []string{"AGENT_sample_CONFIG_DEST=settings.json"}
			case "profile hook release":
				runtime.Profiles = []profile.Definition{{Name: "fixture-profile", Runtime: &profile.RuntimeHook{
					ProjectsChangedHooks: []string{"/usr/local/libexec/subyard/projects-changed.d/fixture-profile"},
				}}}
				runtime.Environment = []string{"ENVIRONMENT_PROFILES=fixture-profile"}
			case "persist", "persist observe", "persist approval", "persist verify drift", "persist capture drift", "persist progress":
				runtime.Environment = []string{"AGENT_sample_PERSIST=.memory:/mnt/host/agent-sessions/sample/memory"}
			case "verify command", "verify check", "verify command exit", "verify check exit", "verify independent refusals", "verify check init", "capture check exit", "capture check transport", "verify inventory drift with check", "stale package check":
				secondRoot := filepath.Join(root, "second")
				if err := os.Mkdir(secondRoot, 0700); err != nil {
					t.Fatal(err)
				}
				testkit.WriteFile(t, filepath.Join(secondRoot, "integration.json"), []byte("{}"), 0600)
				runtime.Integrations = append(runtime.Integrations, profile.IntegrationDefinition{Name: "second", Root: secondRoot})
				provision := filepath.Join(root, "provision.sh")
				testkit.WriteFile(t, provision, []byte("#!/bin/sh\nexit 0\n"), 0700)
				runtime.Environment = []string{"CODING_TOOL_INTEGRATIONS=sample second",
					"AGENT_sample_PROVISION=" + provision, "AGENT_sample_COMMAND=first-command", "AGENT_sample_CHECK=first-check",
					"AGENT_second_PROVISION=" + provision, "AGENT_second_COMMAND=private-native-command", "AGENT_second_CHECK=private-native-check"}
			case "long config sources":
				bindInstructionTestPackage(t, &runtime, root, "codex")
				source := filepath.Join(root, "config/agents/codex/config")
				directory := strings.Repeat("d", 190) + "/" + strings.Repeat("e", 80)
				runtime.Environment = []string{"CODING_TOOL_INTEGRATIONS=codex",
					"AGENT_codex_CONFIG=" + source, "AGENT_codex_CONFIG_DEST=" + directory + "/settings.json",
					"AGENT_codex_RULES=" + source, "AGENT_codex_RULES_DEST=" + directory + "/rules.json"}
				runtime.SourceReader = func(asset config.MaterializedAsset) ([]byte, error) {
					if executor.applyStarted && asset.Source == source {
						return nil, errors.New("private-native-error")
					}
					return asset.ReadSource()
				}
			case "structured observe", "structured apply", "structured apply long", "structured verify":
				switch test.cut {
				case "structured apply":
					configDestination = "/home/dev/.codex/settings&notes.json"
				case "structured apply long":
					configDestination = "/home/dev/.codex/" + strings.Repeat("d", 142) + "&notes.json"
				}
				runtime.Environment = []string{"CODING_TOOL_INTEGRATIONS=codex",
					"AGENT_codex_CONFIG=" + filepath.Join(root, "config/agents/codex/config"),
					"AGENT_codex_CONFIG_DEST=" + strings.TrimPrefix(configDestination, "/home/dev/")}
				bindInstructionTestPackage(t, &runtime, root, "codex")
			case "hooks release":
				runtime.Environment = []string{"CODING_TOOL_INTEGRATIONS=sample", "AGENT_sample_PROJECTS_CHANGED=fixture-hook"}
				executor.observeHooks = func() hookObservation {
					return hookObservation{Projects: strings.Repeat("a", 64), Wiring: strings.Repeat("b", 64), Resolved: map[string]string{"fixture-hook": "/usr/local/bin/fixture-hook"}}
				}
			}
			switch test.cut {
			case "configs", "config package source", "same-owner config sources", "plain config sources", "source and persist", "inventory conflict", "inventory interrupted", "verify inventory drift", "verify inventory drift with check", "capture source":
				path := filepath.Join(root, "codex-instructions.md")
				testkit.WriteFile(t, path, []byte("instructions\n"), 0600)
				selected := runtime.environmentDefault("CODING_TOOL_INTEGRATIONS", "")
				if test.cut == "plain config sources" || test.cut == "source and persist" {
					claude := filepath.Join(root, "claude-instructions.md")
					testkit.WriteFile(t, claude, []byte("instructions\n"), 0600)
					runtime.Environment = append(runtime.Environment, "HOST_CLAUDE_MD="+claude)
					bindInstructionTestPackage(t, &runtime, root, "claude")
					selected = "claude"
				}
				runtime.Environment = append(runtime.Environment, "CODING_TOOL_INTEGRATIONS="+strings.TrimSpace(selected+" codex"), "HOST_CODEX_AGENTS_MD="+path)
				bindInstructionTestPackage(t, &runtime, root, "codex")
				if test.cut == "source and persist" {
					runtime.Environment = append(runtime.Environment, "AGENT_codex_PERSIST=.memory:/mnt/host/agent-sessions/codex/memory")
				}
				captureReads := 0
				runtime.SourceReader = func(asset config.MaterializedAsset) ([]byte, error) {
					switch {
					case test.cut == "plain config sources" && executor.applyStarted && strings.HasSuffix(asset.Source, "-instructions.md"):
						return nil, errors.New("private-native-error")
					case test.cut == "source and persist" && executor.applyStarted && strings.HasSuffix(asset.Source, "claude-instructions.md"):
						return nil, errors.New("private-native-error")
					case test.cut == "same-owner config sources" && executor.pending && asset.Source == path:
						return nil, errors.New("private-native-error")
					case (test.cut == "config package source" || test.cut == "same-owner config sources") && executor.pending && strings.HasSuffix(asset.Source, "/codex/integration.json"):
						return nil, errors.New("private-native-error")
					case test.cut == "capture source" && asset.Source == path && executor.verifiedReads == 2:
						captureReads++
						if captureReads == 2 {
							return []byte("changed source"), nil
						}
					}
					return asset.ReadSource()
				}
			case "script", "script init", "hooks capture":
				provision := filepath.Join(root, "provision.sh")
				testkit.WriteFile(t, provision, []byte("#!/bin/sh\nexit 0\n"), 0700)
				runtime.Environment = []string{"PATH=" + root + ":/usr/bin:/bin", "CODING_TOOL_INTEGRATIONS=sample",
					"AGENT_sample_PROVISION=" + provision, "AGENT_sample_COMMAND=true",
					"INCUS_PROJECT=sample", "YARD_INSTANCE_NAME=sample", "DEV_USER=dev"}
				if test.cut == "script" || test.cut == "script init" {
					var err error
					script, err = os.ReadFile("../../../scripts/reconcile-integrations.sh")
					if err != nil {
						t.Fatal(err)
					}
					for name, content := range map[string]string{
						"lib/runtime.sh": "", "lib/native-installed-stage.sh": "",
						"lib/engine-context.sh": "subyard_require_engine_context() { :; }\n",
						"lib/ui.sh":             "die() { echo \"$*\" >&2; exit 1; }; info() { :; }\n",
						"lib-power.sh":          "power_state() { echo RUNNING; }\n",
					} {
						testkit.WriteFile(t, filepath.Join(root, "scripts", name), []byte(content), 0600)
					}
					testkit.WriteFile(t, filepath.Join(root, "incus"), []byte("#!/bin/sh\necho private-native-stdout\necho private-native-stderr >&2\nexit 1\n"), 0700)
				} else {
					runtime.Environment = append(runtime.Environment, "AGENT_sample_PROJECTS_CHANGED=fixture-hook")
					executor.observeHooks = func() hookObservation {
						return hookObservation{Projects: strings.Repeat("a", 64), Wiring: strings.Repeat("b", 64)}
					}
				}
			}
			testkit.WriteFile(t, filepath.Join(root, "scripts/reconcile-integrations.sh"), script, 0755)
			if test.cut == "hooks run" || test.cut == "hooks capture" {
				scope, _, err := runtime.IntegrationScope()
				if err != nil {
					t.Fatal(err)
				}
				runtime.HookPlan = &ProjectHookPlan{integrationScope: scope}
			}
			ctx := context.Background()
			plan, err := runtime.IntegrationPlan(ctx)
			if err != nil || !plan.Changed {
				t.Fatalf("fixture plan: changed=%t, %v", plan.Changed, err)
			}
			if test.cut == "persist verify drift" {
				observed, _, err := runtime.observeIntegrationsPhase(ctx, "verify")
				if err != nil || !observed.Changed || observed.Fingerprint != plan.Fingerprint || observed.StateBinding() != plan.StateBinding() {
					t.Fatal("observation phase changed the plan binding or turned expected drift into a refusal")
				}
			}
			if test.cut == "verify check init" || test.cut == "script init" || test.cut == "persist approval" {
				runtime.PersistFinishOnly = false
			}
			if test.cut == "persist approval" {
				oldFingerprint := strings.Repeat("a", 64)
				runtime.ApprovedPersist = &oldFingerprint
			}
			executor.applyStarted = true
			persistReads := executor.persistReads
			err = runtime.ApplyIntegrations(ctx, plan)
			if test.cut == "source and persist" || test.cut == "same-owner config sources" {
				var aggregate *domain.Refusals
				if !errors.As(err, &aggregate) || len(aggregate.Unwrap()) != 2 || strings.Contains(err.Error(), "private-native") {
					t.Fatalf("expected both public findings: %v", err)
				}
				first, _ := domain.PublicFinding(aggregate.Unwrap()[0])
				second, _ := domain.PublicFinding(aggregate.Unwrap()[1])
				if test.cut == "source and persist" {
					if executor.persistReads != persistReads+1 || first.Owner != "claude" || second.Owner != "codex" {
						t.Fatalf("independent persist read lost: %v", err)
					}
				} else if first.Owner != "codex" || second.Owner != "codex" || first.Key() == second.Key() ||
					!strings.Contains(err.Error(), "integration.json") || !strings.Contains(err.Error(), "AGENTS.md") {
					t.Fatalf("same-owner inputs lost: %v", err)
				}
				return
			}
			if test.cut == "long config sources" || test.cut == "stale package check" {
				leaves := domain.RefusalCauses(err)
				if len(leaves) != 2 {
					t.Fatalf("expected two independent findings: %v", err)
				}
				first, firstPublic := domain.PublicFinding(leaves[0])
				second, secondPublic := domain.PublicFinding(leaves[1])
				if !firstPublic || !secondPublic || first.Key() == second.Key() ||
					!strings.Contains(err.Error(), test.message) ||
					test.cut == "stale package check" && (!errors.Is(err, domain.ErrPlanStale) || !strings.Contains(err.Error(), "apply observe project hooks failed")) {
					t.Fatalf("finding identities or attribution lost: %v", err)
				}
				return
			}
			if test.cut == "verify independent refusals" {
				var findings *domain.Refusals
				if !errors.As(err, &findings) || len(findings.Unwrap()) != 4 {
					t.Fatalf("expected four independent verification refusals: %v", err)
				}
				for _, want := range []string{"integration apply verify inventory failed in yard sample", "integration apply verify project hooks failed in yard sample", test.message + " in yard sample", "ownership cannot be verified"} {
					if !strings.Contains(findings.Error(), want) {
						t.Errorf("missing verification refusal %q in %v", want, err)
					}
				}
				if strings.Contains(err.Error()+output.String(), "private-native") {
					t.Fatal("verification leaked native output")
				}
				return
			}
			if test.cut == "plain config sources" || test.cut == "verify inventory drift with check" {
				other := "integration claude apply observe config source failed"
				if test.cut == "verify inventory drift with check" {
					other = "integration second apply verify package check failed"
				}
				var findings *domain.Refusals
				if !errors.As(err, &findings) || len(findings.Unwrap()) != 2 ||
					!strings.Contains(err.Error(), test.message) || !strings.Contains(err.Error(), other) ||
					strings.Contains(err.Error()+output.String(), "private-native") {
					t.Fatalf("expected two public findings: %v", err)
				}
				return
			}
			var public interface{ ActivationDiagnostic() (string, string) }
			if !errors.As(err, &public) {
				t.Fatalf("missing public diagnostic: %v", err)
			}
			message, retry := public.ActivationDiagnostic()
			wantMessage, wantRetry := test.message+" in yard sample", "run yard migrate"
			switch test.cut {
			case "structured apply", "structured apply long":
				wantMessage = fmt.Sprintf("integration codex apply configs: %s. Move it aside first, then run yard migrate. Destination: %q", test.message, configDestination)
				wantRetry = "run " + runtime.preserveInvalidConfigCommand(configDestination)
				if err := (releasetransition.Blocker{Code: releasetransition.CodeVerificationFailed, Message: message, Retry: retry}).Validate(); err != nil {
					t.Fatalf("release protocol rejected the recovery: %v", err)
				}
			case "persist", "persist observe":
				wantMessage += ": persist transfer evidence is unavailable; data was preserved"
			case "persist progress":
				if !strings.Contains(output.String(), "paths retained") || !strings.Contains(output.String(), "entries not transferred") {
					t.Fatalf("safe persist progress absent: %s", output.String())
				}
			case "configs", "capture source":
				wantMessage += ` at "/home/dev/.codex/AGENTS.md"`
			case "config package source":
				wantMessage += ` at "integration.json"`
			case "hooks release":
				wantMessage += ` at "fixture-hook"`
			case "profile hook release":
				wantMessage += ` at "/usr/local/libexec/subyard/projects-changed.d/fixture-profile"`
			case "inventory interrupted":
				if IntegrationOwnershipConflict(err) || strings.Contains(retry, "mv") {
					t.Fatalf("interrupted inventory treated as proven ownership drift: %s / %s", message, retry)
				}
			case "inventory conflict":
				wantMessage = test.message + `: owned artifact drift at "/home/dev/.codex/AGENTS.md"; managed artifacts were preserved`
				wantRetry = runtime.preserveOwnedArtifactCommand("/home/dev/.codex/AGENTS.md")
			case "retire structured config":
				wantMessage += ` at "/home/dev/settings.json"`
			case "verify inventory drift":
				wantMessage += ` at "/home/dev/.codex/AGENTS.md": native lifecycle did not converge`
			case "structured verify":
				wantMessage = test.message + ` at "/home/dev/.codex/settings.json": invalid current JSON; file preserved. Compare with the integration's shipped legacy templates before retrying`
				wantRetry = "'incus' 'exec' '' '--project' '' '--' 'sha256sum' '--' '/home/dev/.codex/settings.json'"
				if !IntegrationOwnershipConflict(err) {
					t.Fatalf("scoped recovery lost ownership conflict: %v", err)
				}
			case "verify check init", "script init":
				wantRetry = "run yard init"
			case "verify command exit", "verify check exit", "capture check exit":
				wantMessage += ": native lifecycle did not converge"
			case "persist verify drift", "persist capture drift":
				wantMessage += " at \"/home/dev/.memory\": native lifecycle did not converge"
			case "structured observe":
				wantMessage += " at \"/home/dev/.codex/settings.json\""
			case "persist approval":
				wantMessage = test.message
				wantRetry = "'yard' '-Y' 'sample' 'init'"
				if executor.persistApplied || !errors.Is(err, domain.ErrPlanStale) || !strings.Contains(err.Error(), wantMessage) || !strings.Contains(err.Error(), wantRetry) {
					t.Fatalf("init lost the stale approval refusal or executed the transfer: %v", err)
				}
			}
			wantStale := test.cut == "persist capture drift" || test.cut == "capture check exit" || test.cut == "persist approval" || test.cut == "hooks capture" || test.cut == "capture source" || test.cut == "stale package check"
			if errors.Is(err, domain.ErrPlanStale) != wantStale {
				t.Fatalf("stale classification = %t, want %t: %v", errors.Is(err, domain.ErrPlanStale), wantStale, err)
			}
			if test.cut != "inventory capture-settings" && executor.captureAttempts != 0 {
				t.Fatal("failed verification reached settings capture")
			}
			if message != wantMessage || retry != wantRetry ||
				strings.Contains(err.Error()+output.String(), "private-native") {
				t.Fatalf("diagnostic = %q / %q; private output leaked = %t", message, retry, strings.Contains(err.Error()+output.String(), "private-native"))
			}
		})
	}
}

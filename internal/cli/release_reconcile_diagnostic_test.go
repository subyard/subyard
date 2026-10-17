package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/adapters/releaseruntime"
	"github.com/Subyard/Subyard/internal/config"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ownerroute"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/shellquote"
	"github.com/Subyard/Subyard/internal/testkit"
)

func TestMaterializedConfigReconcileIdentifiesFailedYard(t *testing.T) {
	for _, test := range []struct {
		name           string
		cause          error
		message, retry string
	}{
		{"raw", errors.New("private transport detail"), "yard default: cannot reconcile integrations for release activation (stage=observe-fresh projected=false)", "run yard -Y default integration status"},
		{"stale", domain.ErrPlanStale, "yard default: cannot reconcile integrations for release activation (stage=observe-fresh projected=false)", "run yard -Y default integration status"},
		{"public", fmt.Errorf("private wrapper: %w", publicReconcileTestError{}), "known integration ownership conflict", "run yard -Y default integration status"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, _, configHome, environment := configCommandFixture(t)
			fake := &testkit.Incus{Err: test.cause}
			var stdout, stderr bytes.Buffer
			program, err := New(Options{RepositoryRoot: root, Environment: environment,
				Incus: fake, Executor: fake, Stdout: &stdout, Stderr: &stderr})
			if err != nil {
				t.Fatal(err)
			}
			reconciler := &materializedConfigActivationReconciler{
				cli: program, yard: "default", configHome: configHome,
				integrationPlans: map[string]reconcileruntime.IntegrationPlan{"default": {}},
			}
			err = reconciler.Reconcile(context.Background(), releasetransition.ReleaseLinks{})
			var diagnostic interface{ ActivationDiagnostic() (string, string) }
			if !errors.Is(err, test.cause) || !errors.As(err, &diagnostic) {
				t.Fatalf("integration error lost its cause or public context: %v", err)
			}
			message, retry := diagnostic.ActivationDiagnostic()
			if message != test.message || retry != test.retry {
				t.Fatalf("integration diagnostic = %q / %q", message, retry)
			}
			if stdout.Len() != 0 || stderr.Len() != 0 {
				t.Fatal("reconciler printed private failure details")
			}
		})
	}
}

type activationIntegrationExecutor struct{}

func (activationIntegrationExecutor) Exec(_ context.Context, _, _ string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	if request.Command[0] == "python3" {
		return ports.InstanceExecResult{Stdout: []byte(`{"fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","changed":false}`)}, nil
	}
	if strings.Contains(strings.Join(request.Command, " "), "mode=$1") {
		return ports.InstanceExecResult{Stdout: []byte("changed=0\n")}, nil
	}
	return ports.InstanceExecResult{}, nil
}

func TestIntegrationActivationRetainsApprovalAcrossSiblingDashboardPublication(t *testing.T) {
	for _, scenario := range []string{"authorized route", "projected route", "unexpected route", "foreign baseline", "foreign device", "partial tuple", "mount drift", "identity drift", "unrelated config", "desired scope", "changed desired port", "inventory drift", "sibling not ready"} {
		t.Run(scenario, func(t *testing.T) {
			root, _, _, environment := configCommandFixture(t)
			loaded := loadConfigCommandContext(t, root, environment, "default")
			loaded.Integrations = config.IntegrationSelection{Present: true}
			profileRoot := testkit.TempDir(t)
			handler := filepath.Join(profileRoot, "runtime.sh")
			testkit.WriteFile(t, handler, []byte("#!/bin/sh\nprintf '{\"state\":\"current\",\"actual\":\""+strings.Repeat("b", 64)+"\",\"desired\":\""+strings.Repeat("b", 64)+"\"}\\n'\n"), 0700)
			definition := profile.Definition{Name: "sample", Root: profileRoot,
				Runtime:   &profile.RuntimeHook{ActivationID: "sample-runtime", Handler: "runtime.sh"},
				Dashboard: &profile.IntegrationDashboard{GuestPort: 3000, PortSetting: "SAMPLE_PORT", ContainerRoute: &profile.IntegrationRoute{MarkerKey: "user.subyard.sample", DeviceName: "sample-dashboard"}}}
			old := ownerroute.Device("127.0.0.1", "18080", 3000)
			if scenario == "foreign baseline" {
				old["connect"] = "tcp:127.0.0.1:39999"
			}
			incus := &testkit.Incus{Reconcile: ports.ReconcileState{InstanceFound: true, Instance: ports.InstanceInfo{
				Name: "yard", Project: "subyard", Type: domain.YardContainer, Status: "Running",
				Config: map[string]string{"user.subyard.sample": "v1:18080"}, LocalConfig: map[string]string{"user.subyard.sample": "v1:18080"},
				Devices: map[string]map[string]string{"sample-dashboard": old}, LocalDevices: map[string]map[string]string{"sample-dashboard": old}}}}
			runtime := reconcileruntime.Runtime{RepositoryRoot: root, Incus: incus, Executor: activationIntegrationExecutor{},
				Integrations: []profile.IntegrationDefinition{}, Profiles: []profile.Definition{definition},
				Environment: []string{"ENVIRONMENT_PROFILES=sample", "SAMPLE_PORT=18081"}, Yard: loaded.Context}
			approved, err := runtime.IntegrationPlan(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			originalBinding := approved.StateBinding()
			instance := &incus.Reconcile.Instance
			instance.Config["user.subyard.sample"], instance.LocalConfig["user.subyard.sample"] = "v1:18081", "v1:18081"
			instance.Devices["sample-dashboard"] = ownerroute.Device("127.0.0.1", "18081", 3000)
			instance.LocalDevices["sample-dashboard"] = ownerroute.Device("127.0.0.1", "18081", 3000)
			switch scenario {
			case "unexpected route":
				instance.Devices["sample-dashboard"]["listen"] = "tcp:127.0.0.1:18082"
			case "foreign device":
				instance.Devices["sample-dashboard"]["connect"] = "tcp:127.0.0.1:39999"
			case "partial tuple":
				instance.LocalConfig["user.subyard.sample"] = "v1:18080"
			case "mount drift":
				instance.Devices["foreign"] = map[string]string{"type": "disk", "source": "/", "path": "/host"}
			case "identity drift":
				instance.Name = "foreign"
			case "unrelated config":
				instance.Config["security.privileged"] = "true"
			case "desired scope":
				runtime.Yard.DevUID++
			case "changed desired port":
				runtime.Environment = []string{"ENVIRONMENT_PROFILES=sample", "SAMPLE_PORT=18082"}
			case "inventory drift":
				runtime.Executor = changedActivationInventory{}
			case "sibling not ready":
				testkit.WriteFile(t, handler, []byte("#!/bin/sh\nprintf '{\"state\":\"stale\",\"actual\":\""+strings.Repeat("a", 64)+"\",\"desired\":\""+strings.Repeat("b", 64)+"\"}\\n'\n"), 0700)
			}
			incus.Instances = map[string]ports.InstanceInfo{loaded.Context.IncusProject + "/" + loaded.Context.YardInstanceName: *instance}
			fresh, freshErr := runtime.IntegrationPlan(context.Background())
			if freshErr != nil || reconcileruntime.CheckIntegrationPlan(approved, fresh) == nil {
				t.Fatalf("ordinary integration guard admitted sibling or unrelated drift: %v", freshErr)
			}
			program, err := New(Options{RepositoryRoot: root, Environment: environment, InitPlatform: runtime})
			if err != nil {
				t.Fatal(err)
			}
			reconciler := &materializedConfigActivationReconciler{projectedIntegrationPlans: scenario == "projected route", integrationPlans: map[string]reconcileruntime.IntegrationPlan{"default": approved}}
			err = reconciler.reconcileIntegrationTarget(context.Background(), program, configTarget{Name: "default", Loaded: loaded})
			if scenario == "authorized route" || scenario == "projected route" {
				if err != nil {
					t.Fatalf("authorized sibling publication invalidated integrations: %v", err)
				}
			} else if !errors.Is(err, domain.ErrPlanStale) {
				t.Fatalf("unexpected change accepted or misclassified: %v", err)
			}
			if approved.StateBinding() != originalBinding {
				t.Fatal("release comparison changed the original approval")
			}
		})
	}
}

type changedActivationInventory struct{ activationIntegrationExecutor }

func (changedActivationInventory) Exec(ctx context.Context, project, instance string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	result, err := (activationIntegrationExecutor{}).Exec(ctx, project, instance, request)
	result.Stdout = bytes.ReplaceAll(result.Stdout, []byte(strings.Repeat("a", 64)), []byte(strings.Repeat("c", 64)))
	return result, err
}

func TestIntegrationActivationFailureBoundaries(t *testing.T) {
	for _, stage := range []string{"lock", "legacy-adoption", "observe-fresh", "compare-projected", "compare-exact", "apply"} {
		t.Run(stage, func(t *testing.T) {
			root, _, configHome, environment := configCommandFixture(t)
			loaded := loadConfigCommandContext(t, root, environment, "default")
			loaded.Integrations = config.IntegrationSelection{Present: true}
			incus := &testkit.Incus{Reconcile: ports.ReconcileState{InstanceFound: true, Instance: ports.InstanceInfo{Status: "Running"}}}
			runtime := reconcileruntime.Runtime{RepositoryRoot: root, Incus: incus, Executor: activationIntegrationExecutor{},
				Integrations: []profile.IntegrationDefinition{}, Environment: []string{}, Yard: loaded.Context}
			approved, err := runtime.IntegrationPlan(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			projected := stage != "compare-exact" && stage != "legacy-adoption"
			switch stage {
			case "lock":
				writeConfigCommandFile(t, filepath.Join(configHome, "generated/integration-locks/default.lock"), "private-sentinel", 0o644)
			case "legacy-adoption":
				loaded.Integrations.AllowsCodingTools = true
				runtime.Executor = &testkit.Incus{Err: errors.New("private-sentinel")}
			case "observe-fresh":
				incus.Err = errors.New("private-sentinel")
			case "compare-projected", "compare-exact":
				approved.Fingerprint = "private-sentinel"
			case "apply":
				// Fail the apply observation after a successful fresh comparison.
				runtime.Executor = &activationApplyFailureExecutor{activationIntegrationExecutor: activationIntegrationExecutor{}}
			}
			program, err := New(Options{RepositoryRoot: root, Environment: environment, InitPlatform: runtime})
			if err != nil {
				t.Fatal(err)
			}
			reconciler := &materializedConfigActivationReconciler{projectedIntegrationPlans: projected,
				integrationPlans: map[string]reconcileruntime.IntegrationPlan{"default": approved}}
			err = reconciler.reconcileIntegrationTarget(context.Background(), program, configTarget{Name: "default", Loaded: loaded})
			if err == nil {
				t.Fatal("injected failure was accepted")
			}
			boundary := materializedConfigActivationError{cause: err, yard: "default", phase: "integrations", action: "reconcile", command: "integration status"}
			message, _ := boundary.ActivationDiagnostic()
			if !strings.Contains(message, "stage="+stage+" projected=") || strings.Contains(message, "private-sentinel") || len(message) > 512 {
				t.Fatalf("unsafe or unattributed failure: %s", message)
			}
			comparison := strings.HasPrefix(stage, "compare-")
			if strings.Contains(message, "fingerprint_equal=false scope_equal=true approved_changed=false fresh_changed=false plan_stale=true") != comparison || errors.Is(err, domain.ErrPlanStale) != comparison {
				t.Fatalf("comparison facts or stale identity lost: %s", message)
			}
		})
	}
}

type activationApplyFailureExecutor struct {
	activationIntegrationExecutor
	observations int
}

func (fixture *activationApplyFailureExecutor) Exec(ctx context.Context, project, instance string, request ports.InstanceExecRequest) (ports.InstanceExecResult, error) {
	if request.Command[0] == "python3" {
		fixture.observations++
		if fixture.observations == 2 {
			return ports.InstanceExecResult{}, errors.New("private-sentinel")
		}
	}
	return fixture.activationIntegrationExecutor.Exec(ctx, project, instance, request)
}

type publicReconcileTestError struct{}

func (publicReconcileTestError) Error() string { return "private cause" }
func (publicReconcileTestError) ActivationDiagnostic() (string, string) {
	return "known integration ownership conflict", "run yard -Y default integration status"
}

// Direct integration status retains the full public hint outside the frozen
// release protocol, including independently parseable cleanup command lines.
func TestIntegrationStatusRendersOwnedCleanupCommands(t *testing.T) {
	command := []string{"/tmp/candidate with 'quotes'/bin/yard-engine", "-Y", "example", "integration", "cleanup", "fixture"}
	apply := shellquote.Command(command)
	check := shellquote.Command(append(command, "--check"))
	detail := "known integration ownership conflict\n" + check + "\nThen run with confirmation:\n" + apply
	var stdout bytes.Buffer
	program := &CLI{options: Options{Stdout: &stdout}}
	program.printIntegrationStatus(integrationStatus{Yard: "example", ID: "fixture", Observed: "conflict", Detail: detail}, false)
	lines := strings.Split(stdout.String(), "\n")
	for _, wanted := range []string{check, apply} {
		count := 0
		for _, line := range lines {
			if line == wanted {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("activation lost separate cleanup argv %q: %q", wanted, stdout.String())
		}
	}
	if strings.Contains(stdout.String(), "private") || strings.Contains(stdout.String(), "--yes") {
		t.Fatalf("unsafe activation output: %q", stdout.String())
	}
}

func TestActivationProjectsNativeOwnershipExitError(t *testing.T) {
	root, _, _, _ := configCommandFixture(t)
	fake := &testkit.Incus{Reconcile: ports.ReconcileState{InstanceFound: true, Instance: ports.InstanceInfo{Status: "Running"}}, ExecSteps: []testkit.IncusExecStep{
		{},
		{Result: ports.InstanceExecResult{Stdout: []byte(`{"fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","changed":false}`)}},
		{},
		{Result: ports.InstanceExecResult{ExitCode: 1, Stderr: []byte("private guest detail\nsubyard-integration-conflict:ccusage\n")}, Err: errors.New("private guest exit error")},
	}}
	runtime := reconcileruntime.Runtime{RepositoryRoot: root, Incus: fake, Executor: fake, Integrations: []profile.IntegrationDefinition{}, Yard: domain.Context{YardName: "example"}}
	_, cause := runtime.IntegrationPlan(context.Background())
	if cause == nil {
		t.Fatal("native ownership conflict was accepted")
	}
	boundary := materializedConfigActivationError{cause: cause, yard: "example", phase: "integration plan", action: "inspect", command: "integration status"}
	message, retry := boundary.ActivationDiagnostic()
	var stdout bytes.Buffer
	program := &CLI{options: Options{Stdout: &stdout}}
	program.printUpdateResult(&releaseExecution{prepared: releaseruntime.Prepared{Action: "update.activate"}, outcome: &releasetransition.Outcome{Status: releasetransition.StatusOperatorActionRequired, Message: message, Retry: retry}}, false)
	if !strings.Contains(stdout.String(), "utility preserved") || strings.Contains(stdout.String(), "private") || strings.Contains(stdout.String(), "cannot inspect") {
		t.Fatalf("native public diagnostic was lost or unsafe: %q", stdout.String())
	}
}

func TestMaterializedConfigObservationPreservesEarlyNativeDiagnostic(t *testing.T) {
	for _, mode := range []os.FileMode{0, 0644} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			root, environment, _ := nativeFixture(t)
			configHome := filepath.Join(root, "state")
			packageRoot := filepath.Join(root, "config/agents/sample")
			if err := os.MkdirAll(filepath.Join(packageRoot, "bin"), 0755); err != nil {
				t.Fatal(err)
			}
			testkit.WriteFile(t, filepath.Join(packageRoot, "integration.json"), []byte(`{"schema_version":1,"command":"sample","native":[{"package":"cmd/producer","path":"bin/producer"}]}`), 0644)
			if mode != 0 {
				testkit.WriteFile(t, filepath.Join(packageRoot, "bin/producer"), []byte("private helper"), mode)
			}
			writeConfigCommandFile(t, filepath.Join(configHome, "config.env"), "CODING_TOOL_INTEGRATIONS='sample'\n")
			incus := &testkit.Incus{Instances: map[string]ports.InstanceInfo{}}
			var stdout, stderr bytes.Buffer
			program, err := New(Options{RepositoryRoot: root, Environment: environment, Incus: incus, Executor: incus, Stdout: &stdout, Stderr: &stderr})
			if err != nil {
				t.Fatal(err)
			}
			reconciler := &materializedConfigActivationReconciler{cli: program, yard: "default", configHome: configHome, scopeResolved: true}
			observed, err := reconciler.Observe(context.Background(), releasetransition.ReleasePair{}, releasetransition.ReleaseLinks{})
			var diagnostic interface{ ActivationDiagnostic() (string, string) }
			if !errors.As(err, &diagnostic) || observed.Converged {
				t.Fatalf("early native failure lost: %v", err)
			}
			message, retry := diagnostic.ActivationDiagnostic()
			if !strings.Contains(message, "integration sample inspect in yard default") || !strings.Contains(message, "native dependency or observation unavailable") || retry != "yard -Y default integration status sample --json" || strings.Contains(message, root) || strings.Contains(message, "private helper") {
				t.Fatalf("early release diagnostic = %q / %q", message, retry)
			}
			if stdout.Len() != 0 || stderr.Len() != 0 || len(incus.ExecCalls) != 0 || len(incus.ConfigUpdates) != 0 || len(incus.PowerUpdates) != 0 {
				t.Fatal("early release failure printed private details or performed effects")
			}
		})
	}
}

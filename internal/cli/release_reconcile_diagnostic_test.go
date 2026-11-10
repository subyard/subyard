package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
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
	"github.com/Subyard/Subyard/internal/resource"
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

// A plan approved before native migrations must survive exactly what those verified
// migrations and their restart published, and nothing else.
func TestIntegrationActivationExplainsOnlyVerifiedNativeMigrations(t *testing.T) {
	const (
		guestStep      = "complete-registered-guest-disk-tmp-v3"
		registeredStep = "register-existing-local-yards-v3"
	)
	device := map[string]string{"type": "proxy", "bind": "host", "listen": "tcp:127.0.0.1:32000", "connect": "tcp:127.0.0.1:9119"}
	receiptKey, receipt := "user.subyard.resource.sample", (resource.ProxyContract{}).OwnershipValue(device)
	both := func(instance *ports.InstanceInfo, key, value string) {
		instance.Config[key], instance.LocalConfig[key] = value, value
	}
	restart := func(instance *ports.InstanceInfo) {
		both(instance, "volatile.last_state.power", "RUNNING")
		both(instance, "volatile.last_state.ready", "false")
		both(instance, "volatile.eth0.host_name", "vethAFTER")
		both(instance, "volatile.sample.last_state.created", "true")
	}
	marker := func(instance *ports.InstanceInfo) { both(instance, "user.subyard.disk_tmp", "v1") }
	restartVM := func(instance *ports.InstanceInfo) {
		restart(instance)
		both(instance, "volatile.vm.rtc_adjustment", "-2")
		both(instance, "volatile.vm.needs_reset", "true")
	}
	for _, test := range []struct {
		name    string
		steps   map[string]string
		yard    string
		goal    string
		change  func(*ports.InstanceInfo)
		allowed bool
		vm      bool
	}{
		{name: "marker and restart", steps: map[string]string{guestStep: "verified"}, change: func(i *ports.InstanceInfo) { marker(i); restart(i) }, allowed: true},
		{name: "VM marker and restart", vm: true, steps: map[string]string{guestStep: "verified"}, change: func(i *ports.InstanceInfo) { marker(i); restartVM(i) }, allowed: true},
		{name: "VM foreign reset value", vm: true, steps: map[string]string{guestStep: "verified"}, change: func(i *ports.InstanceInfo) { marker(i); both(i, "volatile.vm.needs_reset", "false") }},
		{name: "container reset output", steps: map[string]string{guestStep: "verified"}, change: func(i *ports.InstanceInfo) { marker(i); both(i, "volatile.vm.needs_reset", "true") }},
		{name: "restart outputs need no evidence", change: restart, allowed: true},
		{name: "receipt and marker", steps: map[string]string{registeredStep: "verified", guestStep: "verified"}, change: func(i *ports.InstanceInfo) { marker(i); both(i, receiptKey, receipt) }, allowed: true},
		{name: "marker without journal", change: marker},
		{name: "marker step unverified", steps: map[string]string{guestStep: "evidence"}, change: marker},
		{name: "marker of another yard", steps: map[string]string{guestStep: "verified"}, yard: "other", change: marker},
		{name: "marker of another goal", steps: map[string]string{guestStep: "verified"}, goal: "9.9.9-other", change: marker},
		{name: "marker needs its own step", steps: map[string]string{registeredStep: "verified"}, change: marker},
		{name: "receipt needs its own step", steps: map[string]string{guestStep: "verified"}, change: func(i *ports.InstanceInfo) { both(i, receiptKey, receipt) }},
		{name: "foreign marker", steps: map[string]string{guestStep: "verified"}, change: func(i *ports.InstanceInfo) { both(i, "user.subyard.disk_tmp", "v2") }},
		{name: "local marker only", steps: map[string]string{guestStep: "verified"}, change: func(i *ports.InstanceInfo) { i.LocalConfig["user.subyard.disk_tmp"] = "v1" }},
		{name: "foreign receipt", steps: map[string]string{registeredStep: "verified"}, change: func(i *ports.InstanceInfo) { both(i, receiptKey, "v1:foreign") }},
		{name: "unrelated config beside marker", steps: map[string]string{guestStep: "verified"}, change: func(i *ports.InstanceInfo) {
			marker(i)
			i.Config["security.privileged"] = "true"
		}},
		{name: "stable device identity beside marker", steps: map[string]string{guestStep: "verified"}, change: func(i *ports.InstanceInfo) {
			marker(i)
			both(i, "volatile.sample.hwaddr", "00:16:3e:00:00:01")
		}},
		{name: "device beside marker", steps: map[string]string{guestStep: "verified"}, change: func(i *ports.InstanceInfo) {
			marker(i)
			i.Devices["foreign"] = map[string]string{"type": "disk", "source": "/", "path": "/host"}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, _, _, environment := configCommandFixture(t)
			loaded := loadConfigCommandContext(t, root, environment, "default")
			loaded.Integrations = config.IntegrationSelection{Present: true}
			incus := &testkit.Incus{Reconcile: ports.ReconcileState{InstanceFound: true, Instance: ports.InstanceInfo{
				Name: "yard", Project: "subyard", Type: domain.YardContainer, Status: "Running",
				Config: map[string]string{"volatile.sample.last_state.created": "false", "volatile.eth0.host_name": "vethBEFORE"}, LocalConfig: map[string]string{"volatile.sample.last_state.created": "false", "volatile.eth0.host_name": "vethBEFORE"},
				Devices: map[string]map[string]string{"sample": device}, LocalDevices: map[string]map[string]string{"sample": device}}}}
			instance := &incus.Reconcile.Instance
			if test.vm {
				loaded.Context.YardKind, instance.Type = domain.YardVM, domain.YardVM
			}
			key := loaded.Context.IncusProject + "/" + loaded.Context.YardInstanceName
			incus.Instances = map[string]ports.InstanceInfo{key: *instance}
			runtime := reconcileruntime.Runtime{RepositoryRoot: root, Incus: incus, Executor: activationIntegrationExecutor{},
				Integrations: []profile.IntegrationDefinition{}, Environment: []string{}, Yard: loaded.Context}
			approved, err := runtime.IntegrationPlan(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			binding := approved.StateBinding()
			test.change(instance)
			incus.Instances[key] = *instance

			configHome := testkit.TempDir(t)
			goal := releasetransition.Goal{Target: "1.2.3-owner", Direction: releasetransition.DirectionActivateTarget}
			artifact := releasetransition.Fingerprint(strings.Repeat("a", 64))
			if test.steps != nil {
				journalGoal := goal
				if test.goal != "" {
					journalGoal.Target = releasetransition.ReleaseID(test.goal)
				}
				yard := test.yard
				if yard == "" {
					yard = "default"
				}
				writeNativeMigrationJournal(t, root, configHome, journalGoal, artifact, yard, test.steps)
			}
			program, err := New(Options{RepositoryRoot: root, Environment: environment, InitPlatform: runtime})
			if err != nil {
				t.Fatal(err)
			}
			reconciler := &materializedConfigActivationReconciler{configHome: configHome, goal: goal, artifactDigest: artifact,
				integrationPlans: map[string]reconcileruntime.IntegrationPlan{"default": approved}}
			err = reconciler.reconcileIntegrationTarget(context.Background(), program, configTarget{Name: "default", Loaded: loaded})
			if test.allowed && err != nil {
				t.Fatalf("verified migration invalidated the approved plan: %v", err)
			}
			if !test.allowed && !errors.Is(err, domain.ErrPlanStale) {
				t.Fatalf("unexplained change accepted or misclassified: %v", err)
			}
			if approved.StateBinding() != binding {
				t.Fatal("comparison changed the original approval")
			}
		})
	}
}

func writeNativeMigrationJournal(t *testing.T, root, configHome string, goal releasetransition.Goal, artifact releasetransition.Fingerprint, yard string, checkpoints map[string]string) {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join(root, "config/release-transition.json"))
	if err != nil {
		t.Fatal(err)
	}
	catalog := releasetransition.BuiltinCapabilityCatalog()
	_, registryDigest, err := releasetransition.ParseRegistryV2(payload, catalog)
	if err != nil {
		t.Fatal(err)
	}
	digest := releasetransition.Fingerprint(strings.Repeat("c", 64))
	journal := releasetransition.JournalRecord{
		SchemaVersion: 2, Transaction: "tx-0123456789abcdef", Goal: goal,
		Releases:          releasetransition.ReleasePair{From: goal.Target, Target: goal.Target},
		AuthorizationPlan: releasetransition.PlanToken("plan-v1-" + string(digest)),
		ResumePlan:        releasetransition.PlanToken("resume-v1-" + strings.Repeat("b", 64)),
		ArtifactDigest:    artifact, RegistryDigest: registryDigest, CatalogDigest: catalog.Digest(),
		ObservationScope: digest, AuthorizationDigest: digest,
		Checkpoint: releasetransition.JournalReconciling, Steps: []releasetransition.JournalStep{},
	}
	type intentStep struct {
		ID        string                        `json:"id"`
		Migration string                        `json:"migration"`
		Resource  string                        `json:"resource"`
		Decision  releasetransition.Decision    `json:"decision"`
		Expected  releasetransition.Fingerprint `json:"expected"`
		Desired   releasetransition.Fingerprint `json:"desired"`
	}
	intents := []intentStep{}
	migrations := slices.Sorted(maps.Keys(checkpoints))
	for index, migration := range migrations {
		step := releasetransition.JournalStep{ID: fmt.Sprintf("step-%d", index), Migration: migration, Resource: "yard." + yard,
			Decision: releasetransition.DecisionTransform, Expected: digest, Desired: digest, Checkpoint: releasetransition.StepCheckpoint(checkpoints[migration])}
		evidence := releasetransition.EvidenceRecord{SchemaVersion: 2, Transaction: journal.Transaction, Releases: journal.Releases, Step: step.ID,
			Expected: digest, Desired: digest, Observed: digest, Checkpoint: releasetransition.EvidenceCaptured}
		if step.Checkpoint == releasetransition.StepVerified {
			evidence.Checkpoint = releasetransition.EvidenceVerified
		} else {
			journal.Checkpoint = releasetransition.JournalMigrating
		}
		step.Evidence = &evidence
		journal.Steps = append(journal.Steps, step)
		intents = append(intents, intentStep{step.ID, step.Migration, step.Resource, step.Decision, step.Expected, step.Desired})
	}
	intent, err := json.Marshal(struct {
		AuthorizationPlan releasetransition.PlanToken   `json:"authorizationPlan"`
		ResumePlan        releasetransition.PlanToken   `json:"resumePlan"`
		ObservationScope  releasetransition.Fingerprint `json:"observationScope"`
		Steps             []intentStep                  `json:"steps"`
	}{journal.AuthorizationPlan, journal.ResumePlan, journal.ObservationScope, intents})
	if err != nil {
		t.Fatal(err)
	}
	journal.IntentDigest = releasetransition.Fingerprint(fmt.Sprintf("%x", sha256.Sum256(intent)))
	encoded, err := releasetransition.MarshalJournal(journal)
	if err != nil {
		t.Fatal(err)
	}
	writeReleaseTransitionTestFile(t, filepath.Join(configHome, "release-transition/v2/journal.json"), encoded, 0o600)
}

type changedActivationInventory struct{}

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
			instance := ports.InstanceInfo{Project: loaded.Context.IncusProject, Name: loaded.Context.YardInstanceName, Status: "Running"}
			incus := &testkit.Incus{Reconcile: ports.ReconcileState{InstanceFound: true, Instance: instance}, Instances: map[string]ports.InstanceInfo{instance.Project + "/" + instance.Name: instance}}
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

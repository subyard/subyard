package reconcileruntime

import (
	"bytes"
	"context"
	"errors"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/testkit"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLifecycleFailureRecordIsClosedAndPhaseAware(t *testing.T) {
	for _, test := range []struct{ output, phase, wantPhase, wantReason string }{
		{"private native output\n" + lifecycleDiagnosticPrefix + "inspect foreign-ownership\n", "inspect", "inspect", "foreign-ownership"},
		{lifecycleDiagnosticPrefix + "inspect invalid-schema\n", "verify", "verify", "invalid-schema"},
		{lifecycleDiagnosticPrefix + "verify native-unavailable\n", "apply", "verify", "native-unavailable"},
		{lifecycleDiagnosticPrefix + "apply observation-changed\n", "apply", "apply", "observation-changed"},
		{"historical arbitrary stderr", "inspect", "inspect", "operation-failed"},
		{lifecycleDiagnosticPrefix + "inspect invalid-schema\n" + lifecycleDiagnosticPrefix + "inspect foreign-ownership\n", "inspect", "inspect", "operation-failed"},
		{lifecycleDiagnosticPrefix + "inspect invalid-schema\n" + lifecycleDiagnosticPrefix + "inspect invalid-schema\n", "inspect", "inspect", "operation-failed"},
		{lifecycleDiagnosticPrefix + "inspect arbitrary-private-value\n", "inspect", "inspect", "operation-failed"},
		{lifecycleDiagnosticPrefix + "apply foreign-ownership\n", "inspect", "inspect", "operation-failed"},
		{lifecycleDiagnosticPrefix + "inspect invalid-schema extra\n", "inspect", "inspect", "operation-failed"},
	} {
		phase, reason := lifecycleFailure([]byte(test.output), test.phase)
		if phase != test.wantPhase || reason != test.wantReason {
			t.Fatalf("failure decoding = %s/%s, want %s/%s", phase, reason, test.wantPhase, test.wantReason)
		}
	}
}

func TestLifecycleDiagnosticsDoNotRenderNativeOutput(t *testing.T) {
	runtime, definition, _, _ := lifecycleFixture(t)
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
	testkit.WriteFile(t, filepath.Join(definition.Root, "hook.sh"), []byte("#!/bin/sh\nprintf '%s\\n' 'PRIVATE=value' '"+lifecycleDiagnosticPrefix+"inspect foreign-ownership' >&2\nexit 1\n"), 0755)
	_, err := runtime.observeIntegrationLifecycle(context.Background(), definition)
	if err == nil || strings.Contains(err.Error(), "PRIVATE") || rendered.Len() != 0 {
		t.Fatalf("private handler output rendered: %v", err)
	}
	var conflict interface{ ActivationConflict() bool }
	if !errors.As(err, &conflict) || !conflict.ActivationConflict() {
		t.Fatal("ownership conflict classification lost")
	}
	stale := runtime.integrationDiagnostic(errors.New("native detail"), definition, "apply", "observation-changed")
	if !errors.Is(stale, domain.ErrPlanStale) {
		t.Fatal("stale guard classification lost")
	}
}

func TestLifecycleNativeBytesInvalidatePlanBeforeEffects(t *testing.T) {
	runtime, definition, actual, desired := lifecycleFixture(t)
	definition.Native = []profile.Native{{Package: "cmd/producer", Path: "bin/producer"}}
	native := filepath.Join(definition.Root, "bin", "producer")
	if err := os.MkdirAll(filepath.Dir(native), 0755); err != nil {
		t.Fatal(err)
	}
	testkit.WriteFile(t, native, []byte("native-v1"), 0755)
	runtime.Integrations = []profile.IntegrationDefinition{definition}
	sources, err := runtime.integrationSourceBinding()
	if err != nil {
		t.Fatal(err)
	}
	plan := IntegrationPlan{sources: sources, facts: []integrationFact{{Key: "lifecycle:sample", Observed: actual, Desired: desired}}}
	testkit.WriteFile(t, native, []byte("native-v2"), 0755)
	if err := runtime.applyIntegrationLifecycles(context.Background(), plan); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("native change = %v", err)
	}
	state, err := os.ReadFile(filepath.Join(definition.Root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(state, []byte("stale")) {
		t.Fatal("native drift allowed effects")
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
		runtime, definition, _, _ := lifecycleFixture(t)
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
	for _, change := range []string{"protected-observation", "helper-bytes", "helper-unavailable"} {
		t.Run(change, func(t *testing.T) {
			runtime, definition, _, desired := lifecycleFixture(t)
			for _, directory := range []string{"bin", "config"} {
				if err := os.Mkdir(filepath.Join(definition.Root, directory), 0755); err != nil {
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
			} else {
				testkit.WriteFile(t, filepath.Join(definition.Root, "state"), []byte(`{"state":"stale","actual":"`+strings.Repeat("c", 64)+`","desired":"`+desired+`"}`), 0600)
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
			if change == "helper-bytes" {
				assertIntegrationDiagnostic(t, runtime.applyIntegrationLifecycles(context.Background(), plan), "apply", "observation-changed")
			}
		})
	}
}

func TestIntegrationDiagnosticPreservesExistingSafeCause(t *testing.T) {
	runtime, definition, _, _ := lifecycleFixture(t)
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

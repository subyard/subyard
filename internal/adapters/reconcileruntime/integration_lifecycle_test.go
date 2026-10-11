package reconcileruntime

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/testkit"
)

func lifecycleFixture(t *testing.T) (Runtime, profile.IntegrationDefinition, string, string) {
	t.Helper()
	root := testkit.TempDir(t)
	actual := strings.Repeat("a", 64)
	desired := strings.Repeat("b", 64)
	testkit.WriteFile(t, filepath.Join(root, "integration.json"), []byte(`{"schema_version":1}`), 0644)
	testkit.WriteFile(t, filepath.Join(root, "hook.sh"), []byte(`#!/bin/sh
case "$1" in
observe) cat "$STATE" ;;
apply) [ "$3" = "$ACTUAL" ] && [ "$4" = "$DESIRED" ] || exit 75; cp "$NEXT" "$STATE"; cat "$STATE" ;;
*) exit 1 ;;
esac
`), 0755)
	testkit.WriteFile(t, filepath.Join(root, "state"), []byte(`{"state":"stale","actual":"`+actual+`","desired":"`+desired+`"}`), 0600)
	testkit.WriteFile(t, filepath.Join(root, "next"), []byte(`{"state":"current","actual":"`+desired+`","desired":"`+desired+`"}`), 0600)
	definition := profile.IntegrationDefinition{Name: "sample", Root: root, Lifecycle: &profile.IntegrationLifecycle{Handler: "hook.sh"}}
	runtime := Runtime{Integrations: []profile.IntegrationDefinition{definition}, Environment: []string{"PATH=/usr/bin:/bin", "STATE=" + filepath.Join(root, "state"), "NEXT=" + filepath.Join(root, "next"), "ACTUAL=" + actual, "DESIRED=" + desired, "SUBYARD_OPERATION_ID=sample-operation", "CODING_TOOL_INTEGRATIONS=sample"}}
	return runtime, definition, actual, desired
}

func TestIntegrationLifecycleExactApplyAndVerify(t *testing.T) {
	runtime, definition, actual, desired := lifecycleFixture(t)
	observation, err := runtime.observeIntegrationLifecycle(context.Background(), definition)
	if err != nil || observation.State != ports.RuntimeStateStale {
		t.Fatalf("observe = %+v, %v", observation, err)
	}
	sources, err := runtime.integrationSourceBinding()
	if err != nil {
		t.Fatal(err)
	}
	plan := IntegrationPlan{sources: sources, facts: []integrationFact{{Key: "lifecycle:sample", Observed: actual, Desired: desired}}}
	if err := runtime.applyIntegrationLifecycles(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	observation, err = runtime.observeIntegrationLifecycle(context.Background(), definition)
	if err != nil || observation.State != ports.RuntimeStateCurrent {
		t.Fatalf("verify = %+v, %v", observation, err)
	}
}
func TestIntegrationLifecycleRejectsSourceAndReportDrift(t *testing.T) {
	runtime, definition, actual, desired := lifecycleFixture(t)
	sources, _ := runtime.integrationSourceBinding()
	plan := IntegrationPlan{sources: sources, facts: []integrationFact{{Key: "lifecycle:sample", Observed: actual, Desired: desired}}}
	testkit.WriteFile(t, filepath.Join(definition.Root, "integration.json"), []byte(`{"schema_version":2}`), 0644)
	if err := runtime.applyIntegrationLifecycles(context.Background(), plan); !errors.Is(err, domain.ErrPlanStale) {
		t.Fatalf("source drift = %v", err)
	}
	for _, report := range []string{`{"state":"absent","actual":"","desired":""}`, `{"state":"current","actual":"bad","desired":"bad"}`, `{"state":"stale","actual":"` + actual + `","desired":"` + desired + `","extra":true}`} {
		testkit.WriteFile(t, filepath.Join(definition.Root, "state"), []byte(report), 0600)
		if _, err := runtime.observeIntegrationLifecycle(context.Background(), definition); err == nil {
			t.Fatal("malformed or selected-absent report accepted")
		}
	}
}

func TestIntegrationLifecycleRejectsStaleWithEqualFingerprints(t *testing.T) {
	runtime, definition, actual, _ := lifecycleFixture(t)
	testkit.WriteFile(t, filepath.Join(definition.Root, "state"), []byte(`{"state":"stale","actual":"`+actual+`","desired":"`+actual+`"}`), 0600)
	if _, err := runtime.observeIntegrationLifecycle(context.Background(), definition); err == nil {
		t.Fatal("stale report with equal fingerprints accepted")
	}
}

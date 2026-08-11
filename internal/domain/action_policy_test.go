package domain

import (
	"errors"
	"slices"
	"testing"
)

func TestActionRegistryAssessesRegisteredMutation(t *testing.T) {
	registry, err := NewActionRegistry([]ActionDefinition{{
		Action:   "config-sync",
		Summary:  "Update configuration",
		Effect:   ActionMutation,
		Impacts:  []ActionImpact{ImpactPersistentData},
		Recovery: RecoveryRecreatable,
	}})
	if err != nil {
		t.Fatal(err)
	}

	assessment, err := registry.Assess("config-sync", ActionDelta{
		Changed:      true,
		Consequences: []string{"Update the tracked configuration."},
	})
	if err != nil {
		t.Fatal(err)
	}
	if assessment.Action != "config-sync" || assessment.Effect != ActionMutation ||
		!assessment.Changed || assessment.Recovery != RecoveryRecreatable ||
		len(assessment.Impacts) != 1 || assessment.Impacts[0] != ImpactPersistentData ||
		len(assessment.Consequences) != 1 || assessment.Consequences[0] != "Update the tracked configuration." {
		t.Fatalf("assessment = %#v", assessment)
	}
}

func TestActionRegistryResolvesConfirmationPolicy(t *testing.T) {
	registry, err := NewActionRegistry([]ActionDefinition{
		{Action: "status", Summary: "Show status", Effect: ActionRead, Recovery: RecoveryNotNeeded},
		{Action: "terminal", Summary: "Open terminal", Effect: ActionSession, Recovery: RecoveryNotNeeded},
		{Action: "inspect", Summary: "Inspect logs", Effect: ActionBoundedWrite, Recovery: RecoveryNotNeeded},
		{Action: "restart", Summary: "Restart runtime", Effect: ActionMutation, Impacts: []ActionImpact{ImpactYardRuntime}, Recovery: RecoveryReversible},
		{Action: "sync", Summary: "Synchronize configuration", Effect: ActionMutation, Impacts: []ActionImpact{ImpactPersistentData}, Recovery: RecoveryRecreatable},
		{Action: "remove", Summary: "Remove project", Effect: ActionDestruction, Impacts: []ActionImpact{ImpactPersistentData}, Recovery: RecoveryIrreversible},
	})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		action     ActionID
		delta      ActionDelta
		wantPolicy ActionConfirmationPolicy
		wantPrompt *ConfirmationRequest
	}{
		{
			name:       "unchanged mutation",
			action:     "sync",
			delta:      ActionDelta{},
			wantPolicy: ActionConfirmationNever,
		},
		{
			name:       "read",
			action:     "status",
			delta:      ActionDelta{Changed: true},
			wantPolicy: ActionConfirmationNever,
		},
		{
			name:       "session",
			action:     "terminal",
			delta:      ActionDelta{Changed: true},
			wantPolicy: ActionConfirmationNever,
		},
		{
			name:       "bounded write",
			action:     "inspect",
			delta:      ActionDelta{Changed: true},
			wantPolicy: ActionConfirmationNever,
		},
		{
			name:       "reversible mutation",
			action:     "restart",
			delta:      ActionDelta{Changed: true, Consequences: []string{"Restart the runtime."}},
			wantPolicy: ActionConfirmationPromptDefaultYes,
			wantPrompt: &ConfirmationRequest{
				Summary: "Restart runtime", Consequences: []string{"Restart the runtime."}, Default: ConfirmationDefaultYes,
			},
		},
		{
			name:       "recreatable mutation",
			action:     "sync",
			delta:      ActionDelta{Changed: true, Consequences: []string{"Synchronize configuration."}},
			wantPolicy: ActionConfirmationPromptDefaultYes,
			wantPrompt: &ConfirmationRequest{
				Summary: "Synchronize configuration", Consequences: []string{"Synchronize configuration."}, Default: ConfirmationDefaultYes,
			},
		},
		{
			name:       "irreversible destruction",
			action:     "remove",
			delta:      ActionDelta{Changed: true, Consequences: []string{"Permanently remove project data."}},
			wantPolicy: ActionConfirmationPromptDefaultNo,
			wantPrompt: &ConfirmationRequest{
				Summary: "Remove project", Consequences: []string{"Permanently remove project data."}, Default: ConfirmationDefaultNo,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assessment, err := registry.Assess(test.action, test.delta)
			if err != nil {
				t.Fatal(err)
			}
			policy, prompt, err := registry.Resolve(assessment)
			if err != nil {
				t.Fatal(err)
			}
			if policy != test.wantPolicy {
				t.Fatalf("policy = %q, want %q", policy, test.wantPolicy)
			}
			if test.wantPrompt == nil {
				if prompt != nil {
					t.Fatalf("prompt = %#v, want nil", prompt)
				}
				return
			}
			if prompt == nil || prompt.Summary != test.wantPrompt.Summary || prompt.Default != test.wantPrompt.Default ||
				len(prompt.Consequences) != 1 || prompt.Consequences[0] != test.wantPrompt.Consequences[0] {
				t.Fatalf("prompt = %#v, want %#v", prompt, test.wantPrompt)
			}
		})
	}
}

func TestActionRegistryRejectsInvalidDefinitionsWithStableClassification(t *testing.T) {
	tests := []struct {
		name        string
		definitions []ActionDefinition
	}{
		{
			name: "duplicate action",
			definitions: []ActionDefinition{
				{Action: "sync", Summary: "Synchronize", Effect: ActionMutation, Impacts: []ActionImpact{ImpactPersistentData}, Recovery: RecoveryRecreatable},
				{Action: "sync", Summary: "Synchronize again", Effect: ActionMutation, Impacts: []ActionImpact{ImpactPersistentData}, Recovery: RecoveryRecreatable},
			},
		},
		{name: "missing action", definitions: []ActionDefinition{{Summary: "Synchronize", Effect: ActionMutation, Impacts: []ActionImpact{ImpactPersistentData}, Recovery: RecoveryRecreatable}}},
		{name: "unsafe action", definitions: []ActionDefinition{{Action: "sync/action", Summary: "Synchronize", Effect: ActionMutation, Impacts: []ActionImpact{ImpactPersistentData}, Recovery: RecoveryRecreatable}}},
		{name: "missing summary", definitions: []ActionDefinition{{Action: "sync", Effect: ActionMutation, Impacts: []ActionImpact{ImpactPersistentData}, Recovery: RecoveryRecreatable}}},
		{name: "unknown effect", definitions: []ActionDefinition{{Action: "sync", Summary: "Synchronize", Effect: ActionEffect("mutate"), Impacts: []ActionImpact{ImpactPersistentData}, Recovery: RecoveryRecreatable}}},
		{name: "unknown impact", definitions: []ActionDefinition{{Action: "sync", Summary: "Synchronize", Effect: ActionMutation, Impacts: []ActionImpact{"unknown"}, Recovery: RecoveryRecreatable}}},
		{name: "unknown recovery", definitions: []ActionDefinition{{Action: "sync", Summary: "Synchronize", Effect: ActionMutation, Impacts: []ActionImpact{ImpactPersistentData}, Recovery: "unknown"}}},
		{name: "read recovery contradiction", definitions: []ActionDefinition{{Action: "status", Summary: "Show status", Effect: ActionRead, Recovery: RecoveryReversible}}},
		{name: "mutation missing impact", definitions: []ActionDefinition{{Action: "sync", Summary: "Synchronize", Effect: ActionMutation, Recovery: RecoveryRecreatable}}},
		{name: "destruction recovery contradiction", definitions: []ActionDefinition{{Action: "remove", Summary: "Remove", Effect: ActionDestruction, Impacts: []ActionImpact{ImpactPersistentData}, Recovery: RecoveryNotNeeded}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewActionRegistry(test.definitions)
			assertActionPolicyInvalid(t, err)
		})
	}
}

func TestActionRegistryRejectsInvalidDeltasAndTamperedAssessments(t *testing.T) {
	registry, err := NewActionRegistry([]ActionDefinition{
		{Action: "sync", Summary: "Synchronize configuration", Effect: ActionMutation, Impacts: []ActionImpact{ImpactPersistentData}, Recovery: RecoveryRecreatable},
		{Action: "remove", Summary: "Remove project", Effect: ActionDestruction, Impacts: []ActionImpact{ImpactPersistentData}, Recovery: RecoveryIrreversible},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = registry.Assess("missing", ActionDelta{Changed: true, Consequences: []string{"Change something."}})
	assertActionPolicyInvalid(t, err)
	_, err = registry.Assess("sync", ActionDelta{Changed: true})
	assertActionPolicyInvalid(t, err)
	_, err = registry.Assess("sync", ActionDelta{Changed: true, Consequences: []string{"\x1b[2J"}})
	assertActionPolicyInvalid(t, err)

	assessment, err := registry.Assess("sync", ActionDelta{Changed: true, Consequences: []string{"Synchronize configuration."}})
	if err != nil {
		t.Fatal(err)
	}
	assessment.Effect = ActionRead
	_, _, err = registry.Resolve(assessment)
	assertActionPolicyInvalid(t, err)

	assessment, err = registry.Assess("sync", ActionDelta{Changed: true, Consequences: []string{"Synchronize configuration."}})
	if err != nil {
		t.Fatal(err)
	}
	assessment.Impacts = []ActionImpact{ImpactHostOS}
	_, _, err = registry.Resolve(assessment)
	assertActionPolicyInvalid(t, err)

	assessment, err = registry.Assess("remove", ActionDelta{Changed: true, Consequences: []string{"Permanently remove project data."}})
	if err != nil {
		t.Fatal(err)
	}
	assessment.Recovery = RecoveryReversible
	_, _, err = registry.Resolve(assessment)
	assertActionPolicyInvalid(t, err)
}

func TestActionRegistryNormalizesAndDefensivelyCopiesImpacts(t *testing.T) {
	impacts := []ActionImpact{ImpactPersistentData, ImpactHostOS, ImpactPersistentData}
	registry, err := NewActionRegistry([]ActionDefinition{{
		Action: "sync", Summary: "Synchronize configuration", Effect: ActionMutation,
		Impacts: impacts, Recovery: RecoveryReversible,
	}})
	if err != nil {
		t.Fatal(err)
	}
	impacts[0] = ImpactAccess

	first, err := registry.Assess("sync", ActionDelta{Changed: true, Consequences: []string{"Synchronize configuration."}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(first.Impacts, []ActionImpact{ImpactHostOS, ImpactPersistentData}) {
		t.Fatalf("impacts = %#v", first.Impacts)
	}
	first.Impacts[0] = ImpactAccess
	second, err := registry.Assess("sync", ActionDelta{Changed: true, Consequences: []string{"Synchronize configuration."}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(second.Impacts, []ActionImpact{ImpactHostOS, ImpactPersistentData}) {
		t.Fatalf("registry mutation leaked into assessment: %#v", second.Impacts)
	}
}

func TestActionRegistryAllowsNoOpWithoutConsequences(t *testing.T) {
	registry, err := NewActionRegistry([]ActionDefinition{{
		Action: "sync", Summary: "Synchronize configuration", Effect: ActionMutation,
		Impacts: []ActionImpact{ImpactPersistentData}, Recovery: RecoveryRecreatable,
	}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = registry.Assess("missing", ActionDelta{})
	assertActionPolicyInvalid(t, err)

	assessment, err := registry.Assess("sync", ActionDelta{})
	if err != nil {
		t.Fatal(err)
	}
	tampered := assessment
	tampered.Effect = ActionRead
	_, _, err = registry.Resolve(tampered)
	assertActionPolicyInvalid(t, err)

	policy, prompt, err := registry.Resolve(assessment)
	if err != nil || policy != ActionConfirmationNever || prompt != nil {
		t.Fatalf("no-op resolution = policy %q, prompt %#v, error %v", policy, prompt, err)
	}
}

func TestActionRegistryWithReturnsIndependentCombinedRegistry(t *testing.T) {
	base, err := NewActionRegistry([]ActionDefinition{{
		Action: "core.status", Summary: "Read core status", Effect: ActionRead,
		Recovery: RecoveryNotNeeded,
	}})
	if err != nil {
		t.Fatal(err)
	}
	combined, err := base.With([]ActionDefinition{{
		Action: "resource.fixture.up", Summary: "Start fixture resource", Effect: ActionMutation,
		Impacts: []ActionImpact{ImpactYardRuntime}, Recovery: RecoveryReversible,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := base.Assess("resource.fixture.up", ActionDelta{}); !errors.Is(err, ErrActionPolicyInvalid) {
		t.Fatalf("base registry was mutated: %v", err)
	}
	if _, err := combined.Assess("core.status", ActionDelta{}); err != nil {
		t.Fatalf("combined registry lost base action: %v", err)
	}
	if _, err := combined.Assess("resource.fixture.up", ActionDelta{
		Changed: true, Consequences: []string{"start fixture runtime"},
	}); err != nil {
		t.Fatalf("combined registry lost added action: %v", err)
	}
	if _, err := base.With([]ActionDefinition{{
		Action: "core.status", Summary: "Duplicate", Effect: ActionRead, Recovery: RecoveryNotNeeded,
	}}); !errors.Is(err, ErrActionPolicyInvalid) {
		t.Fatalf("duplicate extension error = %v", err)
	}
}

func assertActionPolicyInvalid(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, ErrActionPolicyInvalid) || ActionPolicyErrorClass(err) != ActionPolicyInvalid {
		t.Fatalf("error = %v, class = %q", err, ActionPolicyErrorClass(err))
	}
}

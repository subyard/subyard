package application

import (
	"reflect"
	"testing"

	"github.com/Subyard/Subyard/internal/domain"
)

func TestCoreActionRegistryClassifiesRemoteVariants(t *testing.T) {
	registry, err := NewCoreActionRegistry()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name       string
		action     domain.ActionID
		delta      domain.ActionDelta
		assessment domain.ActionAssessment
		policy     domain.ActionConfirmationPolicy
		request    *domain.ConfirmationRequest
	}{
		{
			name: "list", action: "remote.list",
			assessment: domain.ActionAssessment{
				Action: "remote.list", Effect: domain.ActionRead, Changed: false,
				Recovery: domain.RecoveryNotNeeded, Consequences: []string{},
			},
			policy: domain.ActionConfirmationNever,
		},
		{
			name: "add", action: "remote.add",
			delta: domain.ActionDelta{Changed: true, Consequences: []string{"register remote yard demo on owner"}},
			assessment: domain.ActionAssessment{
				Action: "remote.add", Effect: domain.ActionMutation, Changed: true,
				Impacts: []domain.ActionImpact{
					domain.ImpactAccess, domain.ImpactExternalSystem, domain.ImpactLocalMetadata, domain.ImpactTrust,
				},
				Recovery:     domain.RecoveryReversible,
				Consequences: []string{"register remote yard demo on owner"},
			},
			policy: domain.ActionConfirmationPromptDefaultYes,
			request: &domain.ConfirmationRequest{
				Summary: "Register remote yard", Consequences: []string{"register remote yard demo on owner"},
				Default: domain.ConfirmationDefaultYes,
			},
		},
		{
			name: "repair key", action: "remote.repair-key",
			delta: domain.ActionDelta{Changed: true, Consequences: []string{"replace trust pin"}},
			assessment: domain.ActionAssessment{
				Action: "remote.repair-key", Effect: domain.ActionMutation, Changed: true,
				Impacts:  []domain.ActionImpact{domain.ImpactLocalMetadata, domain.ImpactTrust},
				Recovery: domain.RecoveryReversible, Consequences: []string{"replace trust pin"},
			},
			policy: domain.ActionConfirmationPromptDefaultYes,
			request: &domain.ConfirmationRequest{
				Summary: "Repair remote yard key", Consequences: []string{"replace trust pin"},
				Default: domain.ConfirmationDefaultYes,
			},
		},
		{
			name: "remove", action: "remote.remove",
			delta: domain.ActionDelta{Changed: true, Consequences: []string{"remove remote registration"}},
			assessment: domain.ActionAssessment{
				Action: "remote.remove", Effect: domain.ActionMutation, Changed: true,
				Impacts:  []domain.ActionImpact{domain.ImpactAccess, domain.ImpactLocalMetadata, domain.ImpactTrust},
				Recovery: domain.RecoveryReversible, Consequences: []string{"remove remote registration"},
			},
			policy: domain.ActionConfirmationPromptDefaultYes,
			request: &domain.ConfirmationRequest{
				Summary: "Remove remote yard registration", Consequences: []string{"remove remote registration"},
				Default: domain.ConfirmationDefaultYes,
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			assessment, err := registry.Assess(test.action, test.delta)
			if err != nil || !reflect.DeepEqual(assessment, test.assessment) {
				t.Fatalf("assessment=%#v err=%v", assessment, err)
			}
			policy, request, err := registry.Resolve(assessment)
			if err != nil || policy != test.policy || !equalConfirmationRequest(request, test.request) {
				t.Fatalf("policy=%q request=%#v err=%v", policy, request, err)
			}
		})
	}
}

func TestCoreActionRegistryClassifiesTestVMAndProjectRemovalVariants(t *testing.T) {
	registry, err := NewCoreActionRegistry()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		action   domain.ActionID
		effect   domain.ActionEffect
		impacts  []domain.ActionImpact
		recovery domain.RecoveryClass
		policy   domain.ActionConfirmationPolicy
		default_ domain.ConfirmationDefault
	}{
		{action: "test-vms.logs", effect: domain.ActionRead, recovery: domain.RecoveryNotNeeded, policy: domain.ActionConfirmationNever},
		{action: "test-vms.status", effect: domain.ActionRead, recovery: domain.RecoveryNotNeeded, policy: domain.ActionConfirmationNever},
		{
			action: "test-vms.revoke", effect: domain.ActionMutation,
			impacts:  []domain.ActionImpact{domain.ImpactSharedWorkload, domain.ImpactYardRuntime},
			recovery: domain.RecoveryRecreatable, policy: domain.ActionConfirmationPromptDefaultYes,
			default_: domain.ConfirmationDefaultYes,
		},
		{
			action: "test-vms.recover", effect: domain.ActionMutation,
			impacts:  []domain.ActionImpact{domain.ImpactSharedWorkload, domain.ImpactYardRuntime},
			recovery: domain.RecoveryRecreatable, policy: domain.ActionConfirmationPromptDefaultYes,
			default_: domain.ConfirmationDefaultYes,
		},
		{
			action: "project.remove-soft", effect: domain.ActionMutation,
			impacts:  []domain.ActionImpact{domain.ImpactLocalMetadata, domain.ImpactYardRuntime},
			recovery: domain.RecoveryReversible, policy: domain.ActionConfirmationPromptDefaultYes,
			default_: domain.ConfirmationDefaultYes,
		},
		{
			action: "project.bind-detach", effect: domain.ActionMutation,
			impacts:  []domain.ActionImpact{domain.ImpactLocalMetadata, domain.ImpactYardRuntime},
			recovery: domain.RecoveryReversible, policy: domain.ActionConfirmationPromptDefaultYes,
			default_: domain.ConfirmationDefaultYes,
		},
		{
			action: "project.remove-workspace", effect: domain.ActionDestruction,
			impacts: []domain.ActionImpact{
				domain.ImpactLocalMetadata, domain.ImpactPersistentData, domain.ImpactYardRuntime,
			},
			recovery: domain.RecoveryIrreversible, policy: domain.ActionConfirmationPromptDefaultNo,
			default_: domain.ConfirmationDefaultNo,
		},
	} {
		t.Run(string(test.action), func(t *testing.T) {
			delta := domain.ActionDelta{}
			if test.effect != domain.ActionRead {
				delta.Changed = true
				delta.Consequences = []string{"apply the assessed change"}
			}
			assessment, assessErr := registry.Assess(test.action, delta)
			if assessErr != nil {
				t.Fatal(assessErr)
			}
			if assessment.Effect != test.effect || assessment.Recovery != test.recovery ||
				!reflect.DeepEqual(assessment.Impacts, test.impacts) {
				t.Fatalf("assessment=%#v", assessment)
			}
			policy, request, resolveErr := registry.Resolve(assessment)
			if resolveErr != nil || policy != test.policy {
				t.Fatalf("policy=%q request=%#v err=%v", policy, request, resolveErr)
			}
			if test.policy == domain.ActionConfirmationNever {
				if request != nil {
					t.Fatalf("read action returned request %#v", request)
				}
				return
			}
			if request == nil || request.Default != test.default_ {
				t.Fatalf("request=%#v", request)
			}
		})
	}
}

func TestCoreActionRegistryClassifiesRemainingPublicCommandVariants(t *testing.T) {
	registry, err := NewCoreActionRegistry()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		action   domain.ActionID
		effect   domain.ActionEffect
		recovery domain.RecoveryClass
		policy   domain.ActionConfirmationPolicy
		default_ domain.ConfirmationDefault
	}{
		{action: "yard.init.reconcile", effect: domain.ActionMutation, recovery: domain.RecoveryRecreatable, policy: domain.ActionConfirmationPromptDefaultYes, default_: domain.ConfirmationDefaultYes},
		{action: "yard.init.configs", effect: domain.ActionMutation, recovery: domain.RecoveryReversible, policy: domain.ActionConfirmationPromptDefaultYes, default_: domain.ConfirmationDefaultYes},
		{action: "yard.init.reset", effect: domain.ActionDestruction, recovery: domain.RecoveryIrreversible, policy: domain.ActionConfirmationPromptDefaultNo, default_: domain.ConfirmationDefaultNo},
		{action: "yard.provision", effect: domain.ActionMutation, recovery: domain.RecoveryRecreatable, policy: domain.ActionConfirmationPromptDefaultYes, default_: domain.ConfirmationDefaultYes},
		{action: "yard.stop", effect: domain.ActionMutation, recovery: domain.RecoveryReversible, policy: domain.ActionConfirmationPromptDefaultYes, default_: domain.ConfirmationDefaultYes},
		{action: "yard.stop-force", effect: domain.ActionMutation, recovery: domain.RecoveryReversible, policy: domain.ActionConfirmationPromptDefaultYes, default_: domain.ConfirmationDefaultYes},
		{action: "yard.teardown.keep-data", effect: domain.ActionDestruction, recovery: domain.RecoveryRecreatable, policy: domain.ActionConfirmationPromptDefaultYes, default_: domain.ConfirmationDefaultYes},
		{action: "yard.teardown.purge", effect: domain.ActionDestruction, recovery: domain.RecoveryIrreversible, policy: domain.ActionConfirmationPromptDefaultNo, default_: domain.ConfirmationDefaultNo},
		{action: "project.sync", effect: domain.ActionMutation, recovery: domain.RecoveryReversible, policy: domain.ActionConfirmationPromptDefaultYes, default_: domain.ConfirmationDefaultYes},
		{action: "project.bind", effect: domain.ActionMutation, recovery: domain.RecoveryReversible, policy: domain.ActionConfirmationPromptDefaultYes, default_: domain.ConfirmationDefaultYes},
		{action: "project.clone", effect: domain.ActionMutation, recovery: domain.RecoveryRecreatable, policy: domain.ActionConfirmationPromptDefaultYes, default_: domain.ConfirmationDefaultYes},
		{action: "project.export-patch", effect: domain.ActionBoundedWrite, recovery: domain.RecoveryNotNeeded, policy: domain.ActionConfirmationNever},
		{action: "project.environment.up", effect: domain.ActionMutation, recovery: domain.RecoveryRecreatable, policy: domain.ActionConfirmationPromptDefaultYes, default_: domain.ConfirmationDefaultYes},
		{action: "project.environment.rebuild", effect: domain.ActionMutation, recovery: domain.RecoveryRecreatable, policy: domain.ActionConfirmationPromptDefaultYes, default_: domain.ConfirmationDefaultYes},
		{action: "project.environment.down", effect: domain.ActionMutation, recovery: domain.RecoveryReversible, policy: domain.ActionConfirmationPromptDefaultYes, default_: domain.ConfirmationDefaultYes},
	} {
		t.Run(string(test.action), func(t *testing.T) {
			assessment, err := registry.Assess(test.action, domain.ActionDelta{
				Changed: true, Consequences: []string{"apply the assessed command action"},
			})
			if err != nil || assessment.Effect != test.effect || assessment.Recovery != test.recovery {
				t.Fatalf("assessment=%#v err=%v", assessment, err)
			}
			policy, request, err := registry.Resolve(assessment)
			if err != nil || policy != test.policy {
				t.Fatalf("policy=%q request=%#v err=%v", policy, request, err)
			}
			if test.policy == domain.ActionConfirmationNever {
				if request != nil {
					t.Fatalf("unexpected request=%#v", request)
				}
				return
			}
			if request == nil || request.Default != test.default_ {
				t.Fatalf("request=%#v", request)
			}
		})
	}
}

func TestCoreActionRegistryClassifiesConfigAuthoringVariants(t *testing.T) {
	registry, err := NewCoreActionRegistry()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		action   domain.ActionID
		summary  string
		impact   domain.ActionImpact
		recovery domain.RecoveryClass
	}{
		{action: "config.set", summary: "Set persistent configuration", impact: domain.ImpactLocalMetadata, recovery: domain.RecoveryReversible},
		{action: "config.unset", summary: "Unset persistent configuration", impact: domain.ImpactLocalMetadata, recovery: domain.RecoveryReversible},
		{action: "config.import", summary: "Import persistent configuration file", impact: domain.ImpactLocalMetadata, recovery: domain.RecoveryReversible},
		{action: "config.edit", summary: "Edit persistent configuration file", impact: domain.ImpactLocalMetadata, recovery: domain.RecoveryReversible},
		{action: "config.apply", summary: "Apply Subyard file settings", impact: domain.ImpactYardRuntime, recovery: domain.RecoveryRecreatable},
	} {
		t.Run(string(test.action), func(t *testing.T) {
			assessment, err := registry.Assess(test.action, domain.ActionDelta{
				Changed: true, Consequences: []string{"apply the assessed change"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if assessment.Effect != domain.ActionMutation || assessment.Recovery != test.recovery ||
				!reflect.DeepEqual(assessment.Impacts, []domain.ActionImpact{test.impact}) {
				t.Fatalf("assessment=%#v", assessment)
			}
			policy, request, err := registry.Resolve(assessment)
			if err != nil || policy != domain.ActionConfirmationPromptDefaultYes || request == nil ||
				request.Summary != test.summary || request.Default != domain.ConfirmationDefaultYes {
				t.Fatalf("policy=%q request=%#v err=%v", policy, request, err)
			}
		})
	}
}

func TestCoreActionRegistryClassifiesConfigSyncVariants(t *testing.T) {
	registry, err := NewCoreActionRegistry()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		action  domain.ActionID
		summary string
		impacts []domain.ActionImpact
	}{
		{
			action: "config.sync", summary: "Synchronize persistent configuration",
			impacts: []domain.ActionImpact{domain.ImpactLocalMetadata},
		},
		{
			action: "config.sync.connect", summary: "Connect versioned configuration source",
			impacts: []domain.ActionImpact{domain.ImpactExternalSystem, domain.ImpactLocalMetadata},
		},
		{
			action: "config.sync.pull", summary: "Pull versioned configuration",
			impacts: []domain.ActionImpact{domain.ImpactLocalMetadata},
		},
		{
			action: "config.sync.push", summary: "Push versioned configuration",
			impacts: []domain.ActionImpact{domain.ImpactExternalSystem, domain.ImpactLocalMetadata},
		},
	} {
		t.Run(string(test.action), func(t *testing.T) {
			changed, err := registry.Assess(test.action, domain.ActionDelta{
				Changed: true, Consequences: []string{"apply the assessed sync change"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if changed.Effect != domain.ActionMutation || changed.Recovery != domain.RecoveryReversible ||
				!reflect.DeepEqual(changed.Impacts, test.impacts) {
				t.Fatalf("changed assessment=%#v", changed)
			}
			policy, request, err := registry.Resolve(changed)
			if err != nil || policy != domain.ActionConfirmationPromptDefaultYes || request == nil ||
				request.Summary != test.summary || request.Default != domain.ConfirmationDefaultYes {
				t.Fatalf("changed policy=%q request=%#v err=%v", policy, request, err)
			}

			unchanged, err := registry.Assess(test.action, domain.ActionDelta{Changed: false})
			if err != nil {
				t.Fatal(err)
			}
			policy, request, err = registry.Resolve(unchanged)
			if err != nil || policy != domain.ActionConfirmationNever || request != nil {
				t.Fatalf("unchanged policy=%q request=%#v err=%v", policy, request, err)
			}
		})
	}
}

func TestCoreActionRegistryClassifiesUpdateVariants(t *testing.T) {
	registry, err := NewCoreActionRegistry()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		action   domain.ActionID
		effect   domain.ActionEffect
		impacts  []domain.ActionImpact
		recovery domain.RecoveryClass
		policy   domain.ActionConfirmationPolicy
	}{
		{action: "update.help", effect: domain.ActionRead, recovery: domain.RecoveryNotNeeded, policy: domain.ActionConfirmationNever},
		{action: "update.check", effect: domain.ActionBoundedWrite, recovery: domain.RecoveryNotNeeded, policy: domain.ActionConfirmationNever},
		{action: "update.activate", effect: domain.ActionMutation, impacts: []domain.ActionImpact{domain.ImpactLocalMetadata, domain.ImpactYardRuntime}, recovery: domain.RecoveryReversible, policy: domain.ActionConfirmationPromptDefaultYes},
		{action: "update.rollback", effect: domain.ActionMutation, impacts: []domain.ActionImpact{domain.ImpactLocalMetadata, domain.ImpactYardRuntime}, recovery: domain.RecoveryReversible, policy: domain.ActionConfirmationPromptDefaultYes},
	} {
		t.Run(string(test.action), func(t *testing.T) {
			assessment, err := registry.Assess(test.action, domain.ActionDelta{
				Changed: true, Consequences: []string{"apply release action"},
			})
			if err != nil || assessment.Effect != test.effect || assessment.Recovery != test.recovery ||
				!reflect.DeepEqual(assessment.Impacts, test.impacts) {
				t.Fatalf("assessment=%#v err=%v", assessment, err)
			}
			policy, request, err := registry.Resolve(assessment)
			if err != nil || policy != test.policy {
				t.Fatalf("policy=%q request=%#v err=%v", policy, request, err)
			}
			if test.policy == domain.ActionConfirmationNever && request != nil {
				t.Fatalf("unexpected request=%#v", request)
			}
			if test.policy == domain.ActionConfirmationPromptDefaultYes &&
				(request == nil || request.Default != domain.ConfirmationDefaultYes) {
				t.Fatalf("request=%#v", request)
			}
		})
	}
}

func TestCoreActionRegistryClassifiesEveryCredentialAction(t *testing.T) {
	registry, err := NewCoreActionRegistry()
	if err != nil {
		t.Fatal(err)
	}
	readActions := []domain.ActionID{
		"keys.help", "keys.list", "keys.status", "keys.history", "keys.check-exclusive",
		"keys.import-dry-run", "keys.auto-sync-status", "keys.exchange.identity",
		"keys.exchange.bare-path",
	}
	for _, action := range readActions {
		assessment, err := registry.Assess(action, domain.ActionDelta{})
		if err != nil {
			t.Fatalf("%s: %v", action, err)
		}
		policy, request, err := registry.Resolve(assessment)
		if err != nil || assessment.Effect != domain.ActionRead ||
			assessment.Recovery != domain.RecoveryNotNeeded || len(assessment.Impacts) != 0 ||
			policy != domain.ActionConfirmationNever || request != nil {
			t.Fatalf("%s assessment=%#v policy=%q request=%#v err=%v",
				action, assessment, policy, request, err)
		}
	}

	for _, test := range []struct {
		action   domain.ActionID
		effect   domain.ActionEffect
		impacts  []domain.ActionImpact
		recovery domain.RecoveryClass
		policy   domain.ActionConfirmationPolicy
		def      domain.ConfirmationDefault
	}{
		{action: "keys.add", effect: domain.ActionMutation, impacts: credentialLedgerImpacts(), recovery: domain.RecoveryReversible, policy: domain.ActionConfirmationPromptDefaultYes, def: domain.ConfirmationDefaultYes},
		{action: "keys.import", effect: domain.ActionMutation, impacts: credentialLedgerImpacts(), recovery: domain.RecoveryReversible, policy: domain.ActionConfirmationPromptDefaultYes, def: domain.ConfirmationDefaultYes},
		{action: "keys.rotate", effect: domain.ActionMutation, impacts: credentialLedgerImpacts(), recovery: domain.RecoveryReversible, policy: domain.ActionConfirmationPromptDefaultYes, def: domain.ConfirmationDefaultYes},
		{action: "keys.rollback", effect: domain.ActionMutation, impacts: credentialLedgerImpacts(), recovery: domain.RecoveryReversible, policy: domain.ActionConfirmationPromptDefaultYes, def: domain.ConfirmationDefaultYes},
		{action: "keys.resolve-choose", effect: domain.ActionMutation, impacts: credentialLedgerImpacts(), recovery: domain.RecoveryReversible, policy: domain.ActionConfirmationPromptDefaultYes, def: domain.ConfirmationDefaultYes},
		{action: "keys.resolve-rotate", effect: domain.ActionMutation, impacts: credentialLedgerImpacts(), recovery: domain.RecoveryReversible, policy: domain.ActionConfirmationPromptDefaultYes, def: domain.ConfirmationDefaultYes},
		{action: "keys.materialize", effect: domain.ActionMutation, impacts: []domain.ActionImpact{domain.ImpactAccess, domain.ImpactLocalMetadata, domain.ImpactYardRuntime}, recovery: domain.RecoveryRecreatable, policy: domain.ActionConfirmationPromptDefaultYes, def: domain.ConfirmationDefaultYes},
		{action: "keys.sync", effect: domain.ActionMutation, impacts: []domain.ActionImpact{domain.ImpactExternalSystem, domain.ImpactLocalMetadata, domain.ImpactPersistentData, domain.ImpactSecurity, domain.ImpactTrust}, recovery: domain.RecoveryReversible, policy: domain.ActionConfirmationPromptDefaultYes, def: domain.ConfirmationDefaultYes},
		{action: "keys.auto-sync-pause", effect: domain.ActionMutation, impacts: []domain.ActionImpact{domain.ImpactExternalSystem, domain.ImpactLocalMetadata, domain.ImpactTrust}, recovery: domain.RecoveryReversible, policy: domain.ActionConfirmationPromptDefaultYes, def: domain.ConfirmationDefaultYes},
		{action: "keys.auto-sync-resume", effect: domain.ActionMutation, impacts: []domain.ActionImpact{domain.ImpactExternalSystem, domain.ImpactLocalMetadata, domain.ImpactTrust}, recovery: domain.RecoveryReversible, policy: domain.ActionConfirmationPromptDefaultYes, def: domain.ConfirmationDefaultYes},
		{action: "keys.trust", effect: domain.ActionMutation, impacts: credentialPeerImpacts(), recovery: domain.RecoveryReversible, policy: domain.ActionConfirmationPromptDefaultYes, def: domain.ConfirmationDefaultYes},
		{action: "keys.untrust", effect: domain.ActionMutation, impacts: credentialPeerImpacts(), recovery: domain.RecoveryReversible, policy: domain.ActionConfirmationPromptDefaultYes, def: domain.ConfirmationDefaultYes},
		{action: "keys.move", effect: domain.ActionMutation, impacts: []domain.ActionImpact{domain.ImpactAccess, domain.ImpactExternalSystem, domain.ImpactPersistentData, domain.ImpactSecurity, domain.ImpactTrust, domain.ImpactYardRuntime}, recovery: domain.RecoveryReversible, policy: domain.ActionConfirmationPromptDefaultYes, def: domain.ConfirmationDefaultYes},
		{action: "keys.revoke", effect: domain.ActionDestruction, impacts: []domain.ActionImpact{domain.ImpactAccess, domain.ImpactSecurity, domain.ImpactYardRuntime}, recovery: domain.RecoveryRecreatable, policy: domain.ActionConfirmationPromptDefaultYes, def: domain.ConfirmationDefaultYes},
		{action: "keys.delete-tombstone", effect: domain.ActionDestruction, impacts: []domain.ActionImpact{domain.ImpactAccess, domain.ImpactPersistentData, domain.ImpactSecurity}, recovery: domain.RecoveryIrreversible, policy: domain.ActionConfirmationPromptDefaultNo, def: domain.ConfirmationDefaultNo},
		{action: "keys.exchange.trust-import", effect: domain.ActionMutation, impacts: credentialPeerImpacts(), recovery: domain.RecoveryReversible, policy: domain.ActionConfirmationPromptDefaultYes, def: domain.ConfirmationDefaultYes},
		{action: "keys.exchange.untrust-import", effect: domain.ActionMutation, impacts: credentialPeerImpacts(), recovery: domain.RecoveryReversible, policy: domain.ActionConfirmationPromptDefaultYes, def: domain.ConfirmationDefaultYes},
		{action: "keys.exchange.refresh", effect: domain.ActionMutation, impacts: []domain.ActionImpact{domain.ImpactExternalSystem, domain.ImpactLocalMetadata, domain.ImpactPersistentData, domain.ImpactSecurity, domain.ImpactYardRuntime}, recovery: domain.RecoveryReversible, policy: domain.ActionConfirmationPromptDefaultYes, def: domain.ConfirmationDefaultYes},
		{action: "keys.auto-worker", effect: domain.ActionMutation, impacts: []domain.ActionImpact{domain.ImpactExternalSystem, domain.ImpactLocalMetadata, domain.ImpactPersistentData, domain.ImpactSecurity, domain.ImpactTrust}, recovery: domain.RecoveryReversible, policy: domain.ActionConfirmationPromptDefaultYes, def: domain.ConfirmationDefaultYes},
		{action: "keys.init-store", effect: domain.ActionMutation, impacts: []domain.ActionImpact{domain.ImpactLocalMetadata, domain.ImpactPersistentData, domain.ImpactSecurity}, recovery: domain.RecoveryRecreatable, policy: domain.ActionConfirmationPromptDefaultYes, def: domain.ConfirmationDefaultYes},
	} {
		t.Run(string(test.action), func(t *testing.T) {
			assessment, err := registry.Assess(test.action, domain.ActionDelta{
				Changed: true, Consequences: []string{"apply credential action"},
			})
			if err != nil || assessment.Effect != test.effect || assessment.Recovery != test.recovery ||
				!reflect.DeepEqual(assessment.Impacts, test.impacts) {
				t.Fatalf("assessment=%#v err=%v", assessment, err)
			}
			policy, request, err := registry.Resolve(assessment)
			if err != nil || policy != test.policy || request == nil || request.Default != test.def {
				t.Fatalf("policy=%q request=%#v err=%v", policy, request, err)
			}
			unchanged, err := registry.Assess(test.action, domain.ActionDelta{})
			if err != nil {
				t.Fatal(err)
			}
			policy, request, err = registry.Resolve(unchanged)
			if err != nil || policy != domain.ActionConfirmationNever || request != nil {
				t.Fatalf("no-op policy=%q request=%#v err=%v", policy, request, err)
			}
		})
	}
}

func credentialLedgerImpacts() []domain.ActionImpact {
	return []domain.ActionImpact{
		domain.ImpactAccess, domain.ImpactLocalMetadata, domain.ImpactPersistentData, domain.ImpactSecurity,
	}
}

func credentialPeerImpacts() []domain.ActionImpact {
	return []domain.ActionImpact{
		domain.ImpactAccess, domain.ImpactExternalSystem, domain.ImpactPersistentData,
		domain.ImpactSecurity, domain.ImpactTrust,
	}
}

func equalConfirmationRequest(got, want *domain.ConfirmationRequest) bool {
	if got == nil || want == nil {
		return got == want
	}
	return got.Summary == want.Summary && got.Default == want.Default &&
		len(got.Consequences) == len(want.Consequences) &&
		got.Consequences[0] == want.Consequences[0]
}

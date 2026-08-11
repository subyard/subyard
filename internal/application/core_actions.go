package application

import "github.com/Subyard/Subyard/internal/domain"

func NewCoreActionRegistry() (*domain.ActionRegistry, error) {
	definitions := []domain.ActionDefinition{
		{
			Action: "remote.list", Summary: "List remote yards", Effect: domain.ActionRead,
			Recovery: domain.RecoveryNotNeeded,
		},
		{
			Action: "remote.add", Summary: "Register remote yard", Effect: domain.ActionMutation,
			Impacts: []domain.ActionImpact{
				domain.ImpactLocalMetadata, domain.ImpactTrust, domain.ImpactAccess, domain.ImpactExternalSystem,
			},
			Recovery: domain.RecoveryReversible,
		},
		{
			Action: "remote.repair-key", Summary: "Repair remote yard key", Effect: domain.ActionMutation,
			Impacts:  []domain.ActionImpact{domain.ImpactLocalMetadata, domain.ImpactTrust},
			Recovery: domain.RecoveryReversible,
		},
		{
			Action: "remote.remove", Summary: "Remove remote yard registration", Effect: domain.ActionMutation,
			Impacts:  []domain.ActionImpact{domain.ImpactLocalMetadata, domain.ImpactTrust, domain.ImpactAccess},
			Recovery: domain.RecoveryReversible,
		},
		{
			Action: "config.set", Summary: "Set persistent configuration", Effect: domain.ActionMutation,
			Impacts:  []domain.ActionImpact{domain.ImpactLocalMetadata},
			Recovery: domain.RecoveryReversible,
		},
		{
			Action: "config.unset", Summary: "Unset persistent configuration", Effect: domain.ActionMutation,
			Impacts:  []domain.ActionImpact{domain.ImpactLocalMetadata},
			Recovery: domain.RecoveryReversible,
		},
		{
			Action: "config.import", Summary: "Import persistent configuration file", Effect: domain.ActionMutation,
			Impacts:  []domain.ActionImpact{domain.ImpactLocalMetadata},
			Recovery: domain.RecoveryReversible,
		},
		{
			Action: "config.edit", Summary: "Edit persistent configuration file", Effect: domain.ActionMutation,
			Impacts:  []domain.ActionImpact{domain.ImpactLocalMetadata},
			Recovery: domain.RecoveryReversible,
		},
		{
			Action: "config.apply", Summary: "Apply Subyard file settings", Effect: domain.ActionMutation,
			Impacts:  []domain.ActionImpact{domain.ImpactYardRuntime},
			Recovery: domain.RecoveryRecreatable,
		},
		{
			Action: "config.sync", Summary: "Synchronize persistent configuration", Effect: domain.ActionMutation,
			Impacts:  []domain.ActionImpact{domain.ImpactLocalMetadata},
			Recovery: domain.RecoveryReversible,
		},
		{
			Action: "config.sync.connect", Summary: "Connect versioned configuration source", Effect: domain.ActionMutation,
			Impacts:  []domain.ActionImpact{domain.ImpactLocalMetadata, domain.ImpactExternalSystem},
			Recovery: domain.RecoveryReversible,
		},
		{
			Action: "config.sync.pull", Summary: "Pull versioned configuration", Effect: domain.ActionMutation,
			Impacts:  []domain.ActionImpact{domain.ImpactLocalMetadata},
			Recovery: domain.RecoveryReversible,
		},
		{
			Action: "config.sync.push", Summary: "Push versioned configuration", Effect: domain.ActionMutation,
			Impacts:  []domain.ActionImpact{domain.ImpactLocalMetadata, domain.ImpactExternalSystem},
			Recovery: domain.RecoveryReversible,
		},
		{
			Action: "update.help", Summary: "Show update help", Effect: domain.ActionRead,
			Recovery: domain.RecoveryNotNeeded,
		},
		{
			Action: "update.check", Summary: "Check Subyard release", Effect: domain.ActionBoundedWrite,
			Recovery: domain.RecoveryNotNeeded,
		},
		{
			Action: "update.activate", Summary: "Activate Subyard release", Effect: domain.ActionMutation,
			Impacts:  []domain.ActionImpact{domain.ImpactLocalMetadata, domain.ImpactYardRuntime},
			Recovery: domain.RecoveryReversible,
		},
		{
			Action: "update.rollback", Summary: "Roll back Subyard release", Effect: domain.ActionMutation,
			Impacts:  []domain.ActionImpact{domain.ImpactLocalMetadata, domain.ImpactYardRuntime},
			Recovery: domain.RecoveryReversible,
		},
		{
			Action: "test-vms.logs", Summary: "Show test VM broker logs", Effect: domain.ActionRead,
			Recovery: domain.RecoveryNotNeeded,
		},
		{
			Action: "test-vms.status", Summary: "Show test VM lease status", Effect: domain.ActionRead,
			Recovery: domain.RecoveryNotNeeded,
		},
		{
			Action: "test-vms.revoke", Summary: "Revoke test VM lease slot", Effect: domain.ActionMutation,
			Impacts:  []domain.ActionImpact{domain.ImpactYardRuntime, domain.ImpactSharedWorkload},
			Recovery: domain.RecoveryRecreatable,
		},
		{
			Action: "test-vms.recover", Summary: "Recover test VM lease slot", Effect: domain.ActionMutation,
			Impacts:  []domain.ActionImpact{domain.ImpactYardRuntime, domain.ImpactSharedWorkload},
			Recovery: domain.RecoveryRecreatable,
		},
		{
			Action: "yard.init.reconcile", Summary: "Reconcile yard", Effect: domain.ActionMutation,
			Impacts: []domain.ActionImpact{
				domain.ImpactHostIncus, domain.ImpactHostNetwork, domain.ImpactHostOS,
				domain.ImpactLocalMetadata, domain.ImpactPersistentData, domain.ImpactYardRuntime,
			},
			Recovery: domain.RecoveryRecreatable,
		},
		{
			Action: "yard.init.configs", Summary: "Refresh yard agent configuration", Effect: domain.ActionMutation,
			Impacts:  []domain.ActionImpact{domain.ImpactYardRuntime},
			Recovery: domain.RecoveryReversible,
		},
		{
			Action: "yard.init.reset", Summary: "Reset yard and its persistent data", Effect: domain.ActionDestruction,
			Impacts: []domain.ActionImpact{
				domain.ImpactHostIncus, domain.ImpactHostNetwork, domain.ImpactHostOS,
				domain.ImpactLocalMetadata, domain.ImpactPersistentData, domain.ImpactYardRuntime,
			},
			Recovery: domain.RecoveryIrreversible,
		},
		{
			Action: "yard.provision", Summary: "Provision yard profiles", Effect: domain.ActionMutation,
			Impacts:  []domain.ActionImpact{domain.ImpactYardRuntime},
			Recovery: domain.RecoveryRecreatable,
		},
		{
			Action: "yard.stop", Summary: "Stop yard", Effect: domain.ActionMutation,
			Impacts:  []domain.ActionImpact{domain.ImpactAccess, domain.ImpactYardRuntime},
			Recovery: domain.RecoveryReversible,
		},
		{
			Action: "yard.stop-force", Summary: "Force stop yard", Effect: domain.ActionMutation,
			Impacts: []domain.ActionImpact{
				domain.ImpactAccess, domain.ImpactSharedWorkload, domain.ImpactYardRuntime,
			},
			Recovery: domain.RecoveryReversible,
		},
		{
			Action: "yard.teardown.keep-data", Summary: "Delete yard runtime and keep persistent data",
			Effect: domain.ActionDestruction,
			Impacts: []domain.ActionImpact{
				domain.ImpactAccess, domain.ImpactHostIncus, domain.ImpactLocalMetadata,
				domain.ImpactYardRuntime,
			},
			Recovery: domain.RecoveryRecreatable,
		},
		{
			Action: "yard.teardown.purge", Summary: "Permanently delete yard and persistent data",
			Effect: domain.ActionDestruction,
			Impacts: []domain.ActionImpact{
				domain.ImpactAccess, domain.ImpactHostIncus, domain.ImpactHostNetwork,
				domain.ImpactLocalMetadata, domain.ImpactPersistentData, domain.ImpactYardRuntime,
			},
			Recovery: domain.RecoveryIrreversible,
		},
		{
			Action: "project.sync", Summary: "Synchronize project into yard", Effect: domain.ActionMutation,
			Impacts: []domain.ActionImpact{
				domain.ImpactLocalMetadata, domain.ImpactPersistentData, domain.ImpactYardRuntime,
			},
			Recovery: domain.RecoveryReversible,
		},
		{
			Action: "project.bind", Summary: "Bind host project into yard", Effect: domain.ActionMutation,
			Impacts: []domain.ActionImpact{
				domain.ImpactAccess, domain.ImpactHostIncus, domain.ImpactLocalMetadata,
				domain.ImpactYardRuntime,
			},
			Recovery: domain.RecoveryReversible,
		},
		{
			Action: "project.clone", Summary: "Clone project into yard", Effect: domain.ActionMutation,
			Impacts: []domain.ActionImpact{
				domain.ImpactExternalSystem, domain.ImpactLocalMetadata,
				domain.ImpactPersistentData, domain.ImpactYardRuntime,
			},
			Recovery: domain.RecoveryRecreatable,
		},
		{
			Action: "project.export-patch", Summary: "Export project patch", Effect: domain.ActionBoundedWrite,
			Recovery: domain.RecoveryNotNeeded,
		},
		{
			Action: "project.environment.up", Summary: "Start project environment", Effect: domain.ActionMutation,
			Impacts:  []domain.ActionImpact{domain.ImpactPersistentData, domain.ImpactSharedWorkload, domain.ImpactYardRuntime},
			Recovery: domain.RecoveryRecreatable,
		},
		{
			Action: "project.environment.rebuild", Summary: "Rebuild project environment", Effect: domain.ActionMutation,
			Impacts:  []domain.ActionImpact{domain.ImpactPersistentData, domain.ImpactSharedWorkload, domain.ImpactYardRuntime},
			Recovery: domain.RecoveryRecreatable,
		},
		{
			Action: "project.environment.down", Summary: "Stop project environment", Effect: domain.ActionMutation,
			Impacts:  []domain.ActionImpact{domain.ImpactSharedWorkload, domain.ImpactYardRuntime},
			Recovery: domain.RecoveryReversible,
		},
		{
			Action: "project.remove-soft", Summary: "Remove project registration and keep workspace",
			Effect:   domain.ActionMutation,
			Impacts:  []domain.ActionImpact{domain.ImpactLocalMetadata, domain.ImpactYardRuntime},
			Recovery: domain.RecoveryReversible,
		},
		{
			Action: "project.bind-detach", Summary: "Detach bound project", Effect: domain.ActionMutation,
			Impacts:  []domain.ActionImpact{domain.ImpactLocalMetadata, domain.ImpactYardRuntime},
			Recovery: domain.RecoveryReversible,
		},
		{
			Action: "project.remove-workspace", Summary: "Permanently remove project workspace",
			Effect: domain.ActionDestruction,
			Impacts: []domain.ActionImpact{
				domain.ImpactLocalMetadata, domain.ImpactYardRuntime, domain.ImpactPersistentData,
			},
			Recovery: domain.RecoveryIrreversible,
		},
	}
	definitions = append(definitions, credentialActionDefinitions()...)
	return domain.NewActionRegistry(definitions)
}

func credentialActionDefinitions() []domain.ActionDefinition {
	read := func(action domain.ActionID, summary string) domain.ActionDefinition {
		return domain.ActionDefinition{
			Action: action, Summary: summary, Effect: domain.ActionRead,
			Recovery: domain.RecoveryNotNeeded,
		}
	}
	mutation := func(
		action domain.ActionID,
		summary string,
		impacts []domain.ActionImpact,
		recovery domain.RecoveryClass,
	) domain.ActionDefinition {
		return domain.ActionDefinition{
			Action: action, Summary: summary, Effect: domain.ActionMutation,
			Impacts: impacts, Recovery: recovery,
		}
	}
	ledger := []domain.ActionImpact{
		domain.ImpactAccess, domain.ImpactLocalMetadata,
		domain.ImpactPersistentData, domain.ImpactSecurity,
	}
	peers := []domain.ActionImpact{
		domain.ImpactAccess, domain.ImpactExternalSystem, domain.ImpactPersistentData,
		domain.ImpactSecurity, domain.ImpactTrust,
	}
	return []domain.ActionDefinition{
		read("keys.help", "Show credential help"),
		read("keys.list", "List credentials"),
		read("keys.status", "Show credential status"),
		read("keys.history", "Show credential history"),
		read("keys.check-exclusive", "Check exclusive credential assignment"),
		read("keys.import-dry-run", "Inspect credential import"),
		read("keys.auto-sync-status", "Show automatic credential synchronization status"),
		read("keys.exchange.identity", "Show credential exchange identity"),
		read("keys.exchange.bare-path", "Show shared credential ledger path"),
		mutation("keys.add", "Add encrypted credential", ledger, domain.RecoveryReversible),
		mutation("keys.import", "Import encrypted credential", ledger, domain.RecoveryReversible),
		mutation("keys.rotate", "Rotate encrypted credential", ledger, domain.RecoveryReversible),
		mutation("keys.rollback", "Roll back encrypted credential", ledger, domain.RecoveryReversible),
		mutation("keys.resolve-choose", "Resolve credential using an existing revision", ledger, domain.RecoveryReversible),
		mutation("keys.resolve-rotate", "Resolve credential using a replacement value", ledger, domain.RecoveryReversible),
		mutation("keys.materialize", "Materialize credential consumers", []domain.ActionImpact{
			domain.ImpactAccess, domain.ImpactLocalMetadata, domain.ImpactYardRuntime,
		}, domain.RecoveryRecreatable),
		mutation("keys.sync", "Synchronize credential ledgers", []domain.ActionImpact{
			domain.ImpactExternalSystem, domain.ImpactLocalMetadata, domain.ImpactPersistentData,
			domain.ImpactSecurity, domain.ImpactTrust,
		}, domain.RecoveryReversible),
		mutation("keys.auto-sync-pause", "Pause automatic credential synchronization", []domain.ActionImpact{
			domain.ImpactExternalSystem, domain.ImpactLocalMetadata, domain.ImpactTrust,
		}, domain.RecoveryReversible),
		mutation("keys.auto-sync-resume", "Resume automatic credential synchronization", []domain.ActionImpact{
			domain.ImpactExternalSystem, domain.ImpactLocalMetadata, domain.ImpactTrust,
		}, domain.RecoveryReversible),
		mutation("keys.trust", "Trust credential peer", peers, domain.RecoveryReversible),
		mutation("keys.untrust", "Remove credential peer trust", peers, domain.RecoveryReversible),
		mutation("keys.move", "Move exclusive credential assignment", []domain.ActionImpact{
			domain.ImpactAccess, domain.ImpactExternalSystem, domain.ImpactPersistentData,
			domain.ImpactSecurity, domain.ImpactTrust, domain.ImpactYardRuntime,
		}, domain.RecoveryReversible),
		{
			Action: "keys.revoke", Summary: "Revoke encrypted credential", Effect: domain.ActionDestruction,
			Impacts: []domain.ActionImpact{
				domain.ImpactAccess, domain.ImpactSecurity, domain.ImpactYardRuntime,
			}, Recovery: domain.RecoveryRecreatable,
		},
		{
			Action: "keys.delete-tombstone", Summary: "Permanently delete encrypted credential", Effect: domain.ActionDestruction,
			Impacts: []domain.ActionImpact{
				domain.ImpactAccess, domain.ImpactPersistentData, domain.ImpactSecurity,
			}, Recovery: domain.RecoveryIrreversible,
		},
		mutation("keys.exchange.trust-import", "Accept reciprocal credential trust", peers, domain.RecoveryReversible),
		mutation("keys.exchange.untrust-import", "Remove reciprocal credential trust", peers, domain.RecoveryReversible),
		mutation("keys.exchange.refresh", "Refresh exchanged credentials", []domain.ActionImpact{
			domain.ImpactExternalSystem, domain.ImpactLocalMetadata, domain.ImpactPersistentData,
			domain.ImpactSecurity, domain.ImpactYardRuntime,
		}, domain.RecoveryReversible),
		mutation("keys.auto-worker", "Synchronize due credential peers", []domain.ActionImpact{
			domain.ImpactExternalSystem, domain.ImpactLocalMetadata, domain.ImpactPersistentData,
			domain.ImpactSecurity, domain.ImpactTrust,
		}, domain.RecoveryReversible),
		mutation("keys.init-store", "Initialize encrypted credential store", []domain.ActionImpact{
			domain.ImpactLocalMetadata, domain.ImpactPersistentData, domain.ImpactSecurity,
		}, domain.RecoveryRecreatable),
	}
}

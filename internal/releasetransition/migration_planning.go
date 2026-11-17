package releasetransition

import "context"

func inspectCanonicalSettingsMigration(transition *V2Transition, _ context.Context, _ MigrationDefinitionV2, view V2SettingsSnapshotView, _ ReleaseLinks) (settingsV2Plan, error) {
	capability := newTestVMSettingsV2Capability(transition.options.ConfigHome, transition.options.InheritedSettingIDs, view)
	capability.candidateConfigDir = transition.options.CandidateConfigDir
	return capability.Inspect()
}
func inspectLocalSSHMigration(transition *V2Transition, _ context.Context, _ MigrationDefinitionV2, view V2SettingsSnapshotView, _ ReleaseLinks) (settingsV2Plan, error) {
	return transition.inspectLocalSSHPorts(view)
}
func inspectRegisteredMigrationSettings(transition *V2Transition, ctx context.Context, migration MigrationDefinitionV2, view V2SettingsSnapshotView, links ReleaseLinks) (settingsV2Plan, error) {
	return transition.inspectRegisteredSettings(ctx, migration, view, links)
}
func planNativeMigration(transition *V2Transition, ctx context.Context, observation *v2Observation, migration MigrationDefinitionV2, view V2SettingsSnapshotView) (V2SettingsSnapshotView, error) {
	return transition.appendNativeMigration(ctx, observation, migration, view)
}

func planPublicationBoundaryMigration(transition *V2Transition, ctx context.Context, observation *v2Observation, migration MigrationDefinitionV2, view V2SettingsSnapshotView) (V2SettingsSnapshotView, error) {
	// Canonical power and owning receipt values retain their existing
	// representation. The captured/applied/verified ledger transform
	// publishes the reader boundary before runtime activation: epoch-1
	// owners may not be activated against detached listener state.
	if migration.Domain != "power-metadata" || migration.FromEpoch != 1 || migration.ToEpoch != 2 {
		return view, invalid("owner publication migration requires power-metadata 1 to 2")
	}
	observation.decisions = append(observation.decisions, RedactedDecision{
		Resource: "power-metadata", Scope: "power-metadata", Decision: DecisionTransform,
		Result: "optional-publication-reader-v1",
	})
	observation.activationConsequences = append(observation.activationConsequences,
		"preserve exact optional publication state; rollback to an epoch-1 power reader becomes unavailable")

	return view, nil
}

func planLocalSSHMigration(transition *V2Transition, ctx context.Context, observation *v2Observation, migration MigrationDefinitionV2, view V2SettingsSnapshotView) (V2SettingsSnapshotView, error) {
	plan, inspectErr := transition.inspectLocalSSHPorts(view)
	if inspectErr != nil {
		return view, inspectErr
	}
	observation.decisions = append(observation.decisions, plan.Decisions...)
	observation.blockers = append(observation.blockers, plan.Blockers...)
	for index := range plan.Files {
		file := plan.Files[index]
		if err := file.validate(); err != nil {
			return view, err
		}
		intent := settingsIntent(migration, file)
		observation.intents = append(observation.intents, intent)
		observation.observations = append(observation.observations, ResourceObservation{Resource: "yard." + file.Yard, Class: "local-ssh-port-settings-v1", Fingerprint: file.ExpectedFingerprint})
		observation.work = append(observation.work, settingsMigrationWork(intent, migration, file))
	}
	view = settingsViewAfterPlan(transition.options.ConfigHome, view, plan.Files)

	return view, nil
}

func planCanonicalSettingsMigration(transition *V2Transition, ctx context.Context, observation *v2Observation, migration MigrationDefinitionV2, view V2SettingsSnapshotView) (V2SettingsSnapshotView, error) {
	plan, inspectErr := inspectCanonicalSettingsMigration(transition, ctx, migration, view, observation.links)
	if inspectErr != nil {
		return view, inspectErr
	}
	observation.decisions = append(observation.decisions, plan.Decisions...)
	observation.blockers = append(observation.blockers, plan.Blockers...)
	for index := range plan.Files {
		file := plan.Files[index]
		if err := file.validate(); err != nil {
			return view, err
		}
		intent := settingsIntent(migration, file)
		observation.intents = append(observation.intents, intent)
		observation.observations = append(observation.observations, ResourceObservation{
			Resource: "yard." + file.Yard, Class: "yard-settings-file-v1",
			Fingerprint: file.ExpectedFingerprint,
		})
		observation.work = append(observation.work, settingsMigrationWork(intent, migration, file))
	}
	view = settingsViewAfterPlan(
		transition.options.ConfigHome, view, plan.Files,
	)

	return view, nil
}

func planOwnerRegistrationMigration(transition *V2Transition, ctx context.Context, observation *v2Observation, migration MigrationDefinitionV2, view V2SettingsSnapshotView) (V2SettingsSnapshotView, error) {
	plan, inspectErr := transition.inspectUnjournaledOwnerRegistration(ctx, observation, view)
	if inspectErr != nil {
		if blocker, ok := ownerRegistrationDiagnosticBlocker(inspectErr, ctx); ok {
			observation.blockers = append(observation.blockers, blocker)
		} else {
			return view, inspectErr
		}
	}
	if plan != nil {
		intent := PlannerStepIntent{
			ID:        migration.ID + "." + ownerRegistrationResource,
			Migration: migration.ID, Resource: ownerRegistrationResource,
			Decision: DecisionCanonicalize,
			Expected: plan.ExpectedFingerprint, Desired: plan.DesiredFingerprint,
		}
		observation.decisions = append(observation.decisions, RedactedDecision{
			Resource: ownerRegistrationResource, Scope: "owner-registration",
			Decision: DecisionCanonicalize, Result: "test-yard",
		})
		observation.intents = append(observation.intents, intent)
		observation.observations = append(observation.observations, ResourceObservation{
			Resource: "owner-registration." + ownerRegistrationResource,
			Class:    "test-yard-owner-v1", Fingerprint: plan.ExpectedFingerprint,
		})
		observation.work = append(observation.work, ownerMigrationWork(intent, migration, *plan))
	}

	return view, nil
}

func verifyCanonicalSettingsMigration(transition *V2Transition, ctx context.Context) error {
	plan, err := inspectCanonicalSettingsMigration(transition, ctx, MigrationDefinitionV2{}, nil, ReleaseLinks{})
	if err != nil {
		return err
	}
	if len(plan.Blockers) != 0 || len(plan.Files) != 0 {
		return invalid("canonical settings no longer satisfy the rewind-only migration predicate")
	}
	return nil
}

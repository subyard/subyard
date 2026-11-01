package releasetransition

import (
	"context"
)

const registeredYardsMigration = "registered-yards-v2-to-v3"
const guestDiskTmpMigration = "registered-guest-disk-tmp-v2-to-v3"

// These are two fixed recipes in the existing migration journal. Endpoint
// imports use its existing settings work before native metadata effects. Their
// owners observe only protected registered local yards; activation remains a
// separate native observation after their verified ledger steps.
func nativeMigration(m MigrationDefinitionV2) bool {
	return m.FromEpoch == 2 && m.ToEpoch == 3 &&
		(m.Kind == registeredYardsMigration && m.Domain == "owner-registration" ||
			m.Kind == guestDiskTmpMigration && m.Domain == "power-metadata")
}

func (t *V2Transition) nativeOwners(m MigrationDefinitionV2) []V2ActivationReconciler {
	if !nativeMigration(m) {
		return nil
	}
	if m.Kind == registeredYardsMigration {
		return t.options.RegisteredYards
	}
	return t.options.GuestDiskTmp
}
func (t *V2Transition) nativeOwner(m MigrationDefinitionV2, resource string) V2ActivationReconciler {
	for _, owner := range t.nativeOwners(m) {
		if !nilV2Adapter(owner) && owner.ID() == resource {
			return owner
		}
	}
	return nil
}

func nativeMigrationWork(intent PlannerStepIntent, m MigrationDefinitionV2) v2Work {
	return v2Work{intent: intent, migration: cloneWorkMigration(m), recipe: &nativeWorkRecipe{}}
}

func (t *V2Transition) nativeFingerprint(ctx context.Context, work v2Work, links ReleaseLinks, views ...V2SettingsSnapshotView) (Fingerprint, error) {
	owner := t.nativeOwner(work.migration, work.intent.Resource)
	if nilV2Adapter(owner) {
		return "", invalid("registered yard migration owner is unavailable")
	}
	var view V2SettingsSnapshotView
	if len(views) > 0 {
		view = views[0]
	}
	observed, err := observeRegisteredOwner(ctx, owner, t.options.Releases, links, view)
	if err != nil {
		return "", err
	}
	if err := validateActivationObservation(owner.ID(), observed); err != nil {
		return "", err
	}
	if observed.Desired != work.intent.Desired {
		return "", invalid("registered yard migration desired state changed")
	}
	return observed.Actual, nil
}

func (t *V2Transition) appendNativeMigration(ctx context.Context, observation *v2Observation, m MigrationDefinitionV2, view V2SettingsSnapshotView) (V2SettingsSnapshotView, error) {
	plan, err := t.inspectRegisteredSettings(ctx, m, view, observation.links)
	if err != nil {
		return view, err
	}
	for _, file := range plan.Files {
		intent := settingsIntent(m, file)
		observation.intents = append(observation.intents, intent)
		observation.observations = append(observation.observations, ResourceObservation{Resource: intent.Resource, Class: m.Kind, Fingerprint: intent.Expected})
		observation.work = append(observation.work, settingsMigrationWork(intent, m, file))
	}
	view = settingsViewAfterPlan(t.options.ConfigHome, view, plan.Files)
	for _, owner := range t.nativeOwners(m) {
		if nilV2Adapter(owner) {
			return view, invalid("registered yard migration owner is unavailable")
		}
		observed, err := observeRegisteredOwner(ctx, owner, t.options.Releases, observation.links, view)
		if err != nil {
			return view, err
		}
		if err := validateActivationObservation(owner.ID(), observed); err != nil {
			return view, err
		}
		observation.activationConsequences = append(observation.activationConsequences, observed.Consequences...)
		if observed.Converged {
			continue
		}
		intent := PlannerStepIntent{ID: m.ID + "." + owner.ID(), Migration: m.ID,
			Resource: owner.ID(), Decision: DecisionTransform,
			Expected: observed.Actual, Desired: observed.Desired}
		observation.intents = append(observation.intents, intent)
		observation.observations = append(observation.observations, ResourceObservation{Resource: intent.Resource, Class: m.Kind, Fingerprint: observed.Actual})
		observation.work = append(observation.work, nativeMigrationWork(intent, m))
	}
	return view, nil
}

package releasetransition

import (
	"context"
	"errors"

	"github.com/Subyard/Subyard/internal/domain"
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

// VerifiedNativeMigrationKinds reports which native migrations this journal proves
// were authorized and verified for a yard. It confers no other authority.
func VerifiedNativeMigrationKinds(registry RegistryV2, journal JournalRecord, yard string) (registered, guestDiskTmp bool) {
	for _, step := range journal.Steps {
		if step.Resource != "yard."+yard || step.Checkpoint != StepVerified {
			continue
		}
		for _, migration := range registry.Migrations {
			if migration.ID == step.Migration && nativeMigration(migration) {
				registered = registered || migration.Kind == registeredYardsMigration
				guestDiskTmp = guestDiskTmp || migration.Kind == guestDiskTmpMigration
			}
		}
	}
	return registered, guestDiskTmp
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

func laterNativeRow(steps []JournalStep, row JournalStep) bool {
	later := false
	for _, step := range steps {
		if later && step.Resource == row.Resource {
			return true
		}
		later = later || step.ID == row.ID
	}
	return false
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
	var refusals, blocking []error
	for _, owner := range t.nativeOwners(m) {
		if nilV2Adapter(owner) {
			return view, invalid("registered yard migration owner is unavailable")
		}
		observed, err := observeRegisteredOwner(ctx, owner, t.options.Releases, observation.links, view)
		if err != nil {
			if nativeMigrationRefusal(err) {
				blocking = append(blocking, err)
			} else {
				refusals = append(refusals, err)
			}
			continue
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
	// An inspection failure ends the observation, so every refusal must travel with it.
	if len(refusals) != 0 {
		return view, domain.CollectRefusals(append(refusals, blocking...)...)
	}
	if err := domain.CollectRefusals(blocking...); err != nil {
		observation.blockers = append(observation.blockers, nativeRefusalBlocker("migration."+m.ID, err))
	}
	return view, nil
}

// nativeMigrationRefusal recognizes an owner's explicit verdict about existing state.
// Other failures stay observation errors; a verdict is not a missing dependency.
func nativeMigrationRefusal(err error) bool {
	var refusal interface{ MigrationRefusal() bool }
	return errors.As(err, &refusal) && refusal.MigrationRefusal()
}

func nativeRefusalBlocker(resource string, err error) Blocker {
	message, retry := "", "run yard update --check"
	for _, cause := range refusalCauses(err) {
		text, action := publicActivationDiagnostic(cause, cause.Error(), retry)
		retry = action
		if message != "" {
			message += "; "
		}
		message += text
	}
	return Blocker{Code: CodePreconditionBlocked, Resource: resource, Message: boundedFailureText(message, maxDiagnosticText), Retry: retry}
}

func refusalCauses(err error) []error {
	var refusals *domain.Refusals
	if errors.As(err, &refusals) {
		return refusals.Unwrap()
	}
	return []error{err}
}

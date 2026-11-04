package releasetransition

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

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

// V2ResumeIdentityObserver names what a finished row must still apply to. Unlike
// the row's fingerprints, it survives later rows and activation.
type V2ResumeIdentityObserver interface {
	ResumeIdentity(context.Context, V2SettingsSnapshotView) (Fingerprint, error)
}

func (t *V2Transition) nativeResumeIdentity(ctx context.Context, work v2Work, view V2SettingsSnapshotView) (string, bool, error) {
	observer, ok := t.nativeOwner(work.migration, work.intent.Resource).(V2ResumeIdentityObserver)
	if !ok {
		return "", false, nil
	}
	identity, err := observer.ResumeIdentity(ctx, view)
	if err != nil {
		return "", false, err
	}
	if err := validateFingerprint(identity, "native resume identity"); err != nil {
		return "", false, err
	}
	return string(identity) + "\n", true, nil
}

// pinNativeIdentity is a sibling record: released readers refuse recovery data on native rows.
func (t *V2Transition) pinNativeIdentity(ctx context.Context, journal JournalRecord, step JournalStep, work v2Work) error {
	if work.kind() != v2NativeWork {
		return nil
	}
	identity, ok, err := t.nativeResumeIdentity(ctx, work, nil)
	if err != nil || !ok {
		return err
	}
	return t.store.CreateNativeIdentity(journal.Transaction, step.ID, []byte(identity))
}

func (t *V2Transition) nativeIdentityReplaced(ctx context.Context, journal JournalRecord, step JournalStep, work v2Work, view V2SettingsSnapshotView) (bool, error) {
	pinned, err := t.store.ReadNativeIdentity(journal.Transaction, step.ID)
	if err != nil || !pinned.Exists {
		return false, err
	}
	identity, ok, err := t.nativeResumeIdentity(ctx, work, view)
	if err != nil || !ok {
		return false, err
	}
	return identity != string(pinned.Payload), nil
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
		return "", scopedNativeRefusals(owner.ID(), err)
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
	if len(plan.Blockers) != 0 {
		observation.blockers = append(observation.blockers, plan.Blockers...)
		return view, nil
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
			err = scopedNativeRefusals(owner.ID(), err)
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
		if m.Kind == guestDiskTmpMigration {
			for _, text := range observed.Consequences {
				decision := DecisionTransform
				if observed.Converged {
					decision = DecisionPreserve
				}
				observation.decisions = append(observation.decisions, RedactedDecision{Resource: owner.ID(), Scope: "guest-disk-tmp", Decision: decision, Result: text})
			}
		}
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
	// Preserve verdicts even when an independent dependency failed beside them.
	if err := domain.CollectRefusals(append(refusals, blocking...)...); err != nil {
		if !hasMigrationRefusal(err) {
			return view, err
		}
		observation.blockers = append(observation.blockers, nativeRefusalBlocker("migration."+m.ID, err, ctx))
	}
	return view, nil
}

// nativeMigrationRefusal recognizes an owner's explicit verdict about existing state.
// Other failures stay observation errors; a verdict is not a missing dependency.
func nativeMigrationRefusal(err error) bool {
	if err == nil {
		return false
	}
	for _, cause := range refusalCauses(err) {
		var refusal interface{ MigrationRefusal() bool }
		if !errors.As(cause, &refusal) || !refusal.MigrationRefusal() {
			return false
		}
	}
	return true
}

func hasMigrationRefusal(err error) bool {
	for _, cause := range refusalCauses(err) {
		var refusal interface{ MigrationRefusal() bool }
		if errors.As(cause, &refusal) && refusal.MigrationRefusal() {
			return true
		}
	}
	return false
}

func nativeRefusalBlocker(resource string, err error, contexts ...context.Context) Blocker {
	if len(contexts) > 0 {
		recordMigrationDiagnostics(contexts[0], err)
	}
	type group struct {
		yard, reason string
		names        []string
		causes       map[string]bool
	}
	groups := map[string]*group{}
	messages, actions := []string{}, []string{}
	classes := []migrationReasonClass{}
	otherMessages := []string{}
	for _, cause := range refusalCauses(err) {
		message, action := publicActivationDiagnostic(cause, "registered yard state cannot be assessed safely", "run yard update --check")
		var dependency *nativeDependencyError
		if errors.As(cause, &dependency) {
			message, action = publicActivationDiagnostic(dependency.cause, "yard "+dependency.yard+": migration observation dependency unavailable", "install a compatible release, then run yard update --check")
			prefix := "yard " + dependency.yard + ": "
			if !strings.HasPrefix(message, prefix) {
				message = prefix + message
			}
		}
		actions = append(actions, action)
		var details interface {
			MigrationRefusalDetails() (string, string, []string)
		}
		if errors.As(cause, &details) {
			yard, reason, names := details.MigrationRefusalDetails()
			if len(yard) > 63 || !domain.SafeName(yard) || !utf8.ValidString(reason) || validateText(reason, "migration refusal reason", maxDiagnosticText, true) != nil {
				messages = append(messages, "registered yard state cannot be assessed safely")
				continue
			}
			for index, name := range names {
				if len(name) > 96 || (!domain.SafeID(name) && name != "unrecognized name") {
					names[index] = "unrecognized name"
				}
			}
			key := yard + "\x00" + reason
			if groups[key] == nil {
				groups[key] = &group{yard: yard, reason: reason, causes: map[string]bool{}}
			}
			groups[key].names = append(groups[key].names, names...)
			groups[key].causes[cause.Error()] = true
		} else {
			if !slices.Contains(messages, message) {
				otherMessages = append(otherMessages, message)
			}
			messages = append(messages, message)
		}
	}
	for _, finding := range groups {
		slices.Sort(finding.names)
		finding.names = slices.Compact(finding.names)
		message := "yard " + finding.yard + ": " + finding.reason
		count := max(len(finding.causes), len(finding.names))
		if len(finding.names) > 0 {
			message += fmt.Sprintf(" (%d: %s)", count, strings.Join(finding.names, ", "))
		} else if count > 1 {
			message += fmt.Sprintf(" (%d distinct findings)", count)
		}
		classes = append(classes, migrationReasonClass{yard: finding.yard, reason: finding.reason, count: count})
		messages = append(messages, message)
	}
	slices.Sort(messages)
	messages = slices.Compact(messages)
	slices.Sort(actions)
	actions = slices.Compact(actions)
	message := strings.Join(messages, "; ")
	if len(message) > maxDiagnosticText {
		for _, text := range otherMessages {
			yard, reason := "unrecognized", text
			if prefix, rest, ok := strings.Cut(text, ": "); ok {
				if value, ok := strings.CutPrefix(prefix, "yard "); ok && len(value) <= 63 && domain.SafeName(value) {
					yard, reason = value, rest
				}
			}
			classes = append(classes, migrationReasonClass{yard: yard, reason: reason, count: 1})
		}
		message = compactMigrationSummary(classes)
	}
	retry := strings.Join(actions, ", then ")
	if len(retry) > maxDiagnosticText {
		retry = "inspect all reported yard refusals, then run yard update --check --json"
	}
	code := CodePreconditionBlocked
	if !nativeMigrationRefusal(err) {
		code = CodeDependencyUnavailable
		install := "install a compatible release, then run yard update --check"
		if !strings.Contains(retry, install) {
			retry = install + ", then " + retry
			if len(retry) > maxDiagnosticText {
				retry = install + ", then inspect the verdict repairs in stderr of yard update --check"
			}
		}
	}
	return Blocker{Code: code, Resource: resource, Message: message, Retry: retry}
}

// A settings-CAS recheck may refuse after authorization. This public cause is
// recoverable when the transient refusal is gone on the next observation.
type migrationBlockerError struct{ Blocker }

func (failure migrationBlockerError) Error() string { return failure.Message }
func (failure migrationBlockerError) ActivationDiagnostic() (string, string) {
	return failure.Message, failure.Retry
}

func refusalCauses(err error) []error {
	if _, public := err.(interface{ ActivationDiagnostic() (string, string) }); public {
		return []error{err}
	}
	var refusals *domain.Refusals
	if errors.As(err, &refusals) {
		var causes []error
		for _, cause := range refusals.Unwrap() {
			causes = append(causes, refusalCauses(cause)...)
		}
		return causes
	}
	return []error{err}
}

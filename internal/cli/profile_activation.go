package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/releasetransition"
)

// Refresh only installed profile files; the profile adapter owns their contract.
// The transition owns confirmation, recovery, and verification across local yards.
type profileRuntimeActivationReconciler struct {
	cli      *CLI
	request  releasetransition.ProcessRequest
	id       string
	recovery map[string]ports.RuntimeObservation
	// applied caches whether this process repairs a completed release. Convergence
	// writes an unfinished journal before its effects, which must not switch the
	// effects to configuration its approved plan did not observe.
	applied *bool
}

func (reconciler *profileRuntimeActivationReconciler) appliedConfiguration() bool {
	if reconciler.applied == nil {
		applied := completedReleaseConfiguration(reconciler.request)
		reconciler.applied = &applied
	}
	return *reconciler.applied
}

// installedReleaseRuntime observes and repairs a core runtime on its installed
// configuration projection.
type installedReleaseRuntime interface {
	ObserveInstalledReleaseRuntime(context.Context, string) (ports.RuntimeObservation, error)
	ApplyInstalledReleaseRuntime(context.Context, string, string, string, string) error
}

// observeTarget is the stage observation of one yard. A completed-release repair
// observes the installed configuration; other processes observe desired intent.
func (reconciler *profileRuntimeActivationReconciler) observeTarget(ctx context.Context, operation *CLI, target configTarget, applied bool) (ports.RuntimeObservation, error) {
	if applied && !coreInstalledRuntimeID(reconciler.id) {
		var err error
		target, err = operation.appliedIntegrationTarget(ctx, target)
		if err != nil {
			return ports.RuntimeObservation{}, err
		}
	}
	platform := profileActivationPlatform(operation, target)
	if observer, ok := platform.(interface {
		ObserveInstalledReleaseRuntime(context.Context, string) (ports.RuntimeObservation, error)
	}); ok && applied && coreInstalledRuntimeID(reconciler.id) {
		return observer.ObserveInstalledReleaseRuntime(ctx, reconciler.id)
	}
	return observeProfileRuntime(ctx, platform, reconciler.id)
}

func (cli *CLI) profileActivationReconciler(request releasetransition.ProcessRequest, id string) releasetransition.V2ActivationReconciler {
	return &profileRuntimeActivationReconciler{cli: cli, request: request, id: id}
}

func (reconciler *profileRuntimeActivationReconciler) ID() string { return reconciler.id }

func (reconciler *profileRuntimeActivationReconciler) ConcurrentActivationObservation() bool {
	// Native profile handlers have isolated processes and captured output. Shared
	// injected platforms/transports have no independence or concurrency contract.
	return !coreInstalledRuntimeID(reconciler.id) && reconciler.cli.options.InitPlatform == nil &&
		reconciler.cli.options.Incus == nil && reconciler.cli.options.Executor == nil &&
		slices.Contains(reconciler.cli.profileRuntimeIDs(), reconciler.id)
}

func (reconciler *profileRuntimeActivationReconciler) failure(yard, phase string, cause error) error {
	owner := "profile " + reconciler.id + " runtime"
	if coreInstalledRuntimeID(reconciler.id) {
		owner = reconciler.id
	}
	return materializedConfigActivationError{cause, yard, owner, "status", phase, reconciler.request.ConfigHome}
}

func (reconciler *profileRuntimeActivationReconciler) targets(ctx context.Context) (*CLI, []configTarget, error) {
	operation := reconciler.cli.releaseActivationOperation()
	// Host-wide inventory must not require the caller's yard to exist.
	loaded, err := operation.resolveReleaseTransitionContext("default", reconciler.request.ConfigHome)
	if err != nil {
		return nil, nil, reconciler.failure("default", "inspect", err)
	}
	targets, err := operation.releaseConfigTargets(ctx, loaded, true)
	if err != nil {
		return operation, nil, err
	}
	// Only installed local yard runtimes belong to release activation. A verified
	// absent local installation has none; raw runtime observation still reports
	// unavailable transport so init can retain its approved prerequisite slots.
	incus, _ := operation.statusPorts()
	if probe, ok := incus.(interface{ LocalInstallationAbsent() bool }); reconciler.id != "key-tools-runtime" && ok && probe.LocalInstallationAbsent() {
		targets = nil
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].Name < targets[j].Name })
	return operation, targets, nil
}

func (reconciler *profileRuntimeActivationReconciler) sourceMigrationsComplete() (bool, error) {
	store, err := releasetransition.NewPOSIXV2Store(reconciler.request.ConfigHome)
	if err != nil {
		return false, err
	}
	return releaseSourceMigrationsComplete(reconciler.cli.options.RepositoryRoot, store)
}

func (reconciler *profileRuntimeActivationReconciler) desired(
	releases releasetransition.ReleasePair,
) (releasetransition.Fingerprint, error) {
	return activationStageFingerprint(struct {
		ID       string                        `json:"id"`
		Target   releasetransition.ReleaseID   `json:"target"`
		Artifact releasetransition.Fingerprint `json:"artifact"`
	}{ID: reconciler.ID(), Target: releases.Target, Artifact: reconciler.request.ArtifactDigest})
}

func profileActivationPlatform(operation *CLI, target configTarget) ports.InitPlatform {
	return operation.initPlatformWithDispatcher(target.Loaded, nil,
		filepath.Join(operation.options.RepositoryRoot, "bin", "yard-engine"))
}

func (reconciler *profileRuntimeActivationReconciler) Observe(
	ctx context.Context, releases releasetransition.ReleasePair, _ releasetransition.ReleaseLinks,
) (releasetransition.V2ActivationObservation, error) {
	desiredDigest, err := reconciler.desired(releases)
	if err != nil {
		return releasetransition.V2ActivationObservation{}, err
	}
	complete, err := reconciler.sourceMigrationsComplete()
	if err != nil {
		return releasetransition.V2ActivationObservation{}, err
	}
	if !complete {
		actual, err := activationStageFingerprint(struct {
			ID    string `json:"id"`
			State string `json:"state"`
		}{ID: reconciler.ID(), State: "source-migrations-pending"})
		return releasetransition.V2ActivationObservation{
			Actual: actual, Desired: desiredDigest, Converged: false,
		}, err
	}
	operation, targets, err := reconciler.targets(ctx)
	if err != nil {
		return releasetransition.V2ActivationObservation{}, reconciler.failure("default", "inspect", err)
	}
	deferred, err := operation.deferredDiskTmpTargets(ctx, targets, reconciler.id == "core-guest-runtime" || slices.Contains(reconciler.cli.profileRuntimeIDs(), reconciler.id))
	if err != nil {
		return releasetransition.V2ActivationObservation{}, reconciler.failure("default", "inspect", err)
	}
	warnings := []string{}
	if reconciler.id == "core-guest-runtime" {
		warnings = diskTmpWarnings(deferred, targets)
	}
	type observation struct {
		Yard, Project, Instance, State, Digest, Desired string
	}
	actual := make([]observation, 0, len(targets))
	converged := true
	disclosed, order := map[string][]string{}, []string{}
	for _, target := range targets {
		if _, ok := deferred[target.Name]; ok {
			continue
		}
		state, err := reconciler.observeTarget(ctx, operation, target, reconciler.appliedConfiguration())
		if err != nil {
			return releasetransition.V2ActivationObservation{}, reconciler.failure(target.Name, "inspect", err)
		}
		kind := string(state.State)
		if kind == "current" || kind == "stale" {
			kind = "installed"
		}
		entry := observation{Yard: target.Name, Project: target.Loaded.Context.IncusProject,
			Instance: target.Loaded.Context.YardInstanceName, State: kind, Digest: state.Actual, Desired: state.Desired}
		actual = append(actual, entry)
		converged = converged && state.State != "stale"
		if state.State == "stale" && state.Consequence != "" {
			if _, seen := disclosed[state.Consequence]; !seen {
				order = append(order, state.Consequence)
			}
			disclosed[state.Consequence] = append(disclosed[state.Consequence], target.Name)
		}
	}
	actualDigest := desiredDigest
	if !converged {
		actualDigest, err = activationStageFingerprint(actual)
	}
	return releasetransition.V2ActivationObservation{Actual: actualDigest, Desired: desiredDigest, Converged: converged, Consequences: runtimeConsequences(order, disclosed), Warnings: warnings}, err
}

// runtimeConsequences states each owner consequence once with its yards, within the
// release plan's diagnostic text bound.
func runtimeConsequences(order []string, yards map[string][]string) []string {
	result := make([]string, 0, len(order))
	for _, consequence := range order {
		names := yards[consequence]
		prefix := "yard " + names[0]
		if len(names) > 1 {
			prefix = "yards " + strings.Join(names, ", ")
		}
		if len(prefix)+len(consequence) > 480 {
			prefix = fmt.Sprintf("%d yards", len(names))
		}
		result = append(result, prefix+": "+consequence)
	}
	return result
}

// A completed release retains its installed configuration until an ordinary
// operation applies newly authored intent. Unfinished transitions still assess
// their complete desired activation contract.
//
// Release convergence observes the same way: its pre-effect recheck must
// reproduce the plan its inspection approved. Reconcile still applies the
// desired contract, and ordinary repairs that apply new intent carry no mode.
func completedReleaseConfiguration(request releasetransition.ProcessRequest) bool {
	if request.Mode != releasetransition.ProcessInspect && request.Mode != releasetransition.ProcessConverge {
		return false
	}
	store, err := releasetransition.NewPOSIXV2Store(request.ConfigHome)
	if err != nil {
		return false
	}
	snapshot, err := store.ReadCurrentJournal()
	if err != nil || !snapshot.Exists {
		return false
	}
	journal, err := releasetransition.ParseJournal(snapshot.Payload)
	return err == nil && journal.Checkpoint == releasetransition.JournalComplete &&
		journal.Goal.Target == request.Target && journal.Goal.Direction == request.Direction && journal.ArtifactDigest == request.ArtifactDigest &&
		journal.RegistryDigest == request.RegistryDigest
}

func (reconciler *profileRuntimeActivationReconciler) Reconcile(ctx context.Context, _ releasetransition.ReleaseLinks) error {
	complete, err := reconciler.sourceMigrationsComplete()
	if err != nil {
		return err
	}
	if !complete {
		return fmt.Errorf("profile runtime refresh requires completed source migrations")
	}
	operation, targets, err := reconciler.targets(ctx)
	if err != nil {
		return err
	}
	deferred, err := operation.deferredDiskTmpTargets(ctx, targets, reconciler.id == "core-guest-runtime" || slices.Contains(reconciler.cli.profileRuntimeIDs(), reconciler.id))
	if err != nil {
		return err
	}
	if reconciler.recovery != nil && len(targets) != len(reconciler.recovery) {
		return domain.ErrPlanStale
	}
	for _, target := range targets {
		if _, ok := deferred[target.Name]; ok {
			continue
		}
		if reconciler.appliedConfiguration() {
			// The approved plan observed the installed configuration. A yard current
			// there receives no effect; a stale yard is repaired to that installed
			// configuration with this release's own values. Intent awaiting init stays
			// pending for init.
			installed, err := reconciler.observeTarget(ctx, operation, target, true)
			if err != nil {
				return reconciler.failure(target.Name, "inspect", err)
			}
			if installed.State != ports.RuntimeStateStale {
				continue
			}
			// An owner-service profile already matching desired intent has only a
			// lagging applied record, left by profile enable or disable before they
			// recorded it. Record the converged selection instead of reverting it.
			if _, owner := reconciler.ownerServiceProfile(); owner {
				desired, err := observeProfileRuntime(ctx, profileActivationPlatform(operation, target), reconciler.id)
				if err != nil {
					return reconciler.failure(target.Name, "inspect", err)
				}
				if desired.State == ports.RuntimeStateCurrent {
					if err := reconciler.recordAppliedProfile(ctx, profileActivationPlatform(operation, target), target); err != nil {
						return reconciler.failure(target.Name, "apply", err)
					}
					continue
				}
			}
			if !coreInstalledRuntimeID(reconciler.id) {
				target, err = operation.appliedIntegrationTarget(ctx, target)
				if err != nil {
					return reconciler.failure(target.Name, "inspect", err)
				}
			}
		}
		platform := profileActivationPlatform(operation, target)
		observe := func() (ports.RuntimeObservation, error) { return observeProfileRuntime(ctx, platform, reconciler.id) }
		apply := platform.ApplyProfileRuntime
		if coreInstalledRuntimeID(reconciler.id) {
			apply = platform.ApplyInstalledRuntime
			// A core repair applies the same installed projection its plan observed.
			if release, ok := platform.(installedReleaseRuntime); ok && reconciler.appliedConfiguration() {
				observe = func() (ports.RuntimeObservation, error) {
					return release.ObserveInstalledReleaseRuntime(ctx, reconciler.id)
				}
				apply = release.ApplyInstalledReleaseRuntime
			}
		}
		state, err := observe()
		if err != nil {
			return reconciler.failure(target.Name, "inspect", err)
		}
		if reconciler.recovery != nil {
			if _, exists := reconciler.recovery[target.Name]; !exists {
				return domain.ErrPlanStale
			}
		}
		if approved, exists := reconciler.recovery[target.Name]; exists && state != approved &&
			!(state.State == ports.RuntimeStateCurrent && state.Desired == approved.Desired) {
			return reconciler.failure(target.Name, "apply", domain.ErrPlanStale)
		}
		if state.State != "stale" {
			// Absent and deferred yards have no running guest whose selection could be recorded.
			if state.State == ports.RuntimeStateCurrent {
				if err := reconciler.recordAppliedProfile(ctx, platform, target); err != nil {
					return reconciler.failure(target.Name, "apply", err)
				}
			}
			continue
		}
		if err := apply(ctx, reconciler.id, operation.ensureOperationID(), state.Actual, state.Desired); err != nil {
			return reconciler.failure(target.Name, "apply", err)
		}
		state, err = observe()
		if err != nil {
			return reconciler.failure(target.Name, "verify", err)
		}
		if state.State != "current" {
			return reconciler.failure(target.Name, "verify", fmt.Errorf("profile runtime did not converge"))
		}
		if err := reconciler.recordAppliedProfile(ctx, platform, target); err != nil {
			return reconciler.failure(target.Name, "apply", err)
		}
	}
	return nil
}

// recordAppliedProfile keeps the applied selection of an owner-service profile
// equal to its converged desired runtime. Profile enable and disable apply only
// that runtime, so a completed release must not keep observing the old selection.
// Profiles that init provisions keep their selection with init.
func (reconciler *profileRuntimeActivationReconciler) recordAppliedProfile(ctx context.Context, platform ports.InitPlatform, target configTarget) error {
	definition, owner := reconciler.ownerServiceProfile()
	recorder, ok := platform.(profileSelectionRecorder)
	if !owner || !ok {
		return nil
	}
	err := recorder.RecordAppliedProfile(ctx, definition.Name, definition.Selected(target.Loaded.Context.YardName, target.Loaded.Environment))
	if errors.Is(err, reconcileruntime.ErrGuestFactsUnfinished) {
		// The core guest refresh of this transition rewrites unfinished facts with
		// the whole desired selection; a release retry must still reach it.
		return nil
	}
	return err
}

// ownerServiceProfile is the owner-service profile whose runtime this reconciler owns.
func (reconciler *profileRuntimeActivationReconciler) ownerServiceProfile() (profile.Definition, bool) {
	if coreInstalledRuntimeID(reconciler.id) {
		return profile.Definition{}, false
	}
	for _, definition := range reconciler.cli.catalog.Profiles() {
		if definition.Runtime != nil && definition.Runtime.ActivationID == reconciler.id && definition.OwnerService != "" {
			return definition, true
		}
	}
	return profile.Definition{}, false
}

func observeProfileRuntime(ctx context.Context, platform ports.InitPlatform, id string) (ports.RuntimeObservation, error) {
	if coreInstalledRuntimeID(id) {
		return platform.ObserveInstalledRuntime(ctx, id)
	}
	if observer, ok := platform.(interface {
		ObserveProfileRuntime(context.Context, string) (ports.RuntimeObservation, error)
	}); ok {
		return observer.ObserveProfileRuntime(ctx, id)
	}
	observations, err := platform.ObserveProfileRuntimes(ctx)
	if err != nil {
		return ports.RuntimeObservation{}, err
	}
	observation, exists := observations[id]
	if !exists {
		return ports.RuntimeObservation{}, fmt.Errorf("profile runtime %s observation is missing", id)
	}
	return observation, nil
}

func coreInstalledRuntimeID(id string) bool {
	return id == "core-guest-runtime" || id == "key-tools-runtime"
}

// Runtime hooks own guarded, repeatable convergence from partial delivery. Bind
// a fresh native plan to each yard's current CAS contract before new consent.
func (reconciler *profileRuntimeActivationReconciler) PrepareActivationRecovery(ctx context.Context, releases releasetransition.ReleasePair, links releasetransition.ReleaseLinks) (releasetransition.Fingerprint, error) {
	complete, err := reconciler.sourceMigrationsComplete()
	if err != nil {
		return "", err
	}
	if !complete {
		return "", fmt.Errorf("runtime recovery requires completed source migrations")
	}
	operation, targets, err := reconciler.targets(ctx)
	if err != nil {
		return "", err
	}
	type entry struct {
		Yard, Project, Instance string
		Runtime                 ports.RuntimeObservation
	}
	var entries []entry
	reconciler.recovery = make(map[string]ports.RuntimeObservation, len(targets))
	for _, target := range targets {
		state, err := observeProfileRuntime(ctx, profileActivationPlatform(operation, target), reconciler.id)
		if err != nil {
			return "", reconciler.failure(target.Name, "inspect", err)
		}
		entries = append(entries, entry{target.Name, target.Loaded.Context.IncusProject, target.Loaded.Context.YardInstanceName, state})
		reconciler.recovery[target.Name] = state
	}
	return activationStageFingerprint(struct {
		ID       string
		Releases releasetransition.ReleasePair
		Links    releasetransition.ReleaseLinks
		Targets  []entry
	}{reconciler.id, releases, links, entries})
}
func (cli *CLI) profileRuntimeIDs() []string {
	var result []string
	for _, definition := range cli.catalog.Profiles() {
		if definition.Runtime != nil {
			result = append(result, definition.Runtime.ActivationID)
		}
	}
	sort.Strings(result)
	return result
}

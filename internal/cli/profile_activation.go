package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/Subyard/Subyard/internal/adapters/reconcileruntime"
	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/profile"
	"github.com/Subyard/Subyard/internal/releasetransition"
	"github.com/Subyard/Subyard/internal/shellquote"
)

// Refresh only installed profile files; the profile adapter owns their contract.
// The transition owns confirmation, recovery, and verification across local yards.
type profileRuntimeActivationReconciler struct {
	cli      *CLI
	request  releasetransition.ProcessRequest
	id       string
	recovery map[string]ports.RuntimeObservation
}

func (reconciler *profileRuntimeActivationReconciler) observeTarget(ctx context.Context, operation *CLI, target configTarget) (ports.RuntimeObservation, error) {
	if reconciler.id != "key-tools-runtime" {
		if err := operation.checkNativeOwner(ctx, target.Loaded); err != nil {
			return ports.RuntimeObservation{}, err
		}
	}
	return observeProfileRuntime(ctx, profileActivationPlatform(operation, target), reconciler.id)
}

func (cli *CLI) profileActivationReconciler(request releasetransition.ProcessRequest, id string) releasetransition.V2ActivationReconciler {
	return &profileRuntimeActivationReconciler{cli: cli.releaseActivationOperation(request), request: request, id: id}
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
	return materializedConfigActivationError{cause, yard, owner, "config status", phase, reconciler.request.ConfigHome}
}

func (reconciler *profileRuntimeActivationReconciler) targets(ctx context.Context) (*CLI, []configTarget, error) {
	operation := reconciler.cli.releaseActivationOperation()
	// Host-wide inventory must not require the caller's yard to exist.
	_, targets, err := operation.releaseActivationTargets(ctx, "default", reconciler.request.ConfigHome, true, nil)
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
	// A stale yard whose recorded yard-scoped failure still matches is
	// converged with a pending finding; any other stale yard is unconverged.
	var pendingYards []releasetransition.V2PendingConfiguration
	pendingInputs := false
	var diagnostics []releasetransition.RedactedDecision
	disclosed, order := map[string][]string{}, []string{}
	var failures []error
	for _, target := range targets {
		if _, ok := deferred[target.Name]; ok {
			continue
		}
		state, err := reconciler.observeTarget(ctx, operation, target)
		if err != nil {
			failures = append(failures, reconciler.failure(target.Name, "inspect", err))
			continue
		}
		if state.State == ports.RuntimeStateStale {
			pending, err := reconciler.recordedPending(target, state)
			if err != nil {
				failures = append(failures, reconciler.failure(target.Name, "inspect", err))
				continue
			}
			if pending != nil {
				pendingYards = append(pendingYards, pending.finding())
				actual = append(actual, observation{Yard: target.Name, Project: target.Loaded.Context.IncusProject,
					Instance: target.Loaded.Context.YardInstanceName, State: "pending-configuration", Digest: state.Actual, Desired: state.Desired})
				continue
			}
		}
		kind := string(state.State)
		if kind == "current" || kind == "stale" {
			kind = "installed"
		}
		entry := observation{Yard: target.Name, Project: target.Loaded.Context.IncusProject,
			Instance: target.Loaded.Context.YardInstanceName, State: kind, Digest: state.Actual, Desired: state.Desired}
		actual = append(actual, entry)
		converged = converged && state.State != "stale" && state.State != ports.RuntimeStatePending
		if state.State == ports.RuntimeStatePending {
			pendingInputs = true
			if state.Diagnostics != nil {
				for _, detail := range *state.Diagnostics {
					diagnostics = append(diagnostics, releasetransition.RedactedDecision{Resource: detail.Parameter, Scope: "yard." + target.Name, Decision: releasetransition.DecisionPreserve, Result: detail.Installed + " -> " + detail.Desired})
				}
			}
		}
		if state.State == "stale" && state.Consequence != "" {
			if _, seen := disclosed[state.Consequence]; !seen {
				order = append(order, state.Consequence)
			}
			disclosed[state.Consequence] = append(disclosed[state.Consequence], target.Name)
		}
	}
	if err := domain.CollectRefusals(failures...); err != nil {
		return releasetransition.V2ActivationObservation{}, err
	}
	actualDigest := desiredDigest
	if !converged {
		actualDigest, err = activationStageFingerprint(actual)
	}
	return releasetransition.V2ActivationObservation{Actual: actualDigest, Desired: desiredDigest, Converged: converged, PendingInputs: pendingInputs, Diagnostics: diagnostics, Consequences: runtimeConsequences(order, disclosed), Warnings: warnings, Pending: pendingYards}, err
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
	// A typed yard-scoped failure is recorded and does not stop the other
	// yards; observation then reports it as a pending finding. Any other
	// failure stops as before.
	for _, target := range targets {
		if _, ok := deferred[target.Name]; ok {
			continue
		}
		platform := profileActivationPlatform(operation, target)
		observe := func() (ports.RuntimeObservation, error) { return observeProfileRuntime(ctx, platform, reconciler.id) }
		apply := platform.ApplyProfileRuntime
		if coreInstalledRuntimeID(reconciler.id) {
			apply = platform.ApplyInstalledRuntime

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
		if approved, exists := reconciler.recovery[target.Name]; exists && !reflect.DeepEqual(state, approved) &&
			!(state.State == ports.RuntimeStateCurrent && state.Desired == approved.Desired) &&
			!(approved.State == ports.RuntimeStatePending && state.State == ports.RuntimeStateStale && state.Desired == approved.Desired) {
			return reconciler.failure(target.Name, "apply", domain.ErrPlanStale)
		}
		if state.State == ports.RuntimeStatePending {
			return reconciler.failure(target.Name, "apply", reconcileruntime.PendingProfileRuntimeError(target.Name, reconciler.id, state))
		}
		if state.State != "stale" {
			if state.State == ports.RuntimeStateCurrent || state.State == ports.RuntimeStateAbsent {
				if err := reconciler.retirePending(target); err != nil {
					return reconciler.failure(target.Name, "apply", err)
				}
			}
			// Absent and deferred yards have no running guest whose selection could be recorded.
			if state.State == ports.RuntimeStateCurrent {
				if err := reconciler.recordAppliedProfile(ctx, platform, target); err != nil {
					return reconciler.failure(target.Name, "apply", err)
				}
			}
			continue
		}
		// A recorded failure waits for yard init; repair does not repeat it.
		pending, err := reconciler.recordedPending(target, state)
		if err != nil {
			return reconciler.failure(target.Name, "apply", err)
		}
		if pending != nil {
			continue
		}
		// A record that no longer matches describes an earlier state; this apply
		// replaces it with its own result.
		if err := reconciler.retirePending(target); err != nil {
			return reconciler.failure(target.Name, "apply", err)
		}
		if err := apply(ctx, reconciler.id, operation.ensureOperationID(), state.Actual, state.Desired); err != nil {
			var yardFailure *reconcileruntime.ProfileRuntimeYardFailure
			if !errors.As(err, &yardFailure) || yardFailure.Yard != target.Name {
				return reconciler.failure(target.Name, "apply", err)
			}
			if recordErr := reconciler.recordPending(target, observe, yardFailure); recordErr != nil {
				return reconciler.failure(target.Name, "apply", err)
			}
			continue
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

// runtimePendingConfiguration is a recorded yard-scoped runtime failure that
// still matches its yard. Its only next step is plain init of that yard.
type runtimePendingConfiguration struct{ record activationPendingRecord }

func (pending runtimePendingConfiguration) finding() releasetransition.V2PendingConfiguration {
	return releasetransition.V2PendingConfiguration{Message: pending.record.Message, Retry: shellquote.Command([]string{"yard", "-Y", pending.record.Yard, "init"})}
}

// pendingFacts are the exact facts a pending record binds for one yard.
func (reconciler *profileRuntimeActivationReconciler) pendingFacts(target configTarget, state ports.RuntimeObservation) (activationPendingRecord, error) {
	definition, ok := reconciler.declaredYardScope()
	if !ok {
		return activationPendingRecord{}, errors.New("profile runtime does not declare a yard failure scope")
	}
	digest, err := profileRuntimePackageDigest(definition)
	if err != nil {
		return activationPendingRecord{}, err
	}
	yard := target.Name
	return activationPendingRecord{
		SchemaVersion: activationPendingSchema, Yard: yard, Project: target.Loaded.Context.IncusProject,
		Instance: target.Loaded.Context.YardInstanceName, Owner: reconciler.id, Actual: state.Actual, Desired: state.Desired,
		Inputs: digest,
		// Each yard's finding is its own readiness blocker, so this bounded
		// message and its init step never depend on the number of yards.
		Message: fmt.Sprintf("yard %[1]s: profile %[2]s runtime failed (diagnose: yard -Y %[1]s profile list); "+
			"profile not applied; fix it and run yard -Y %[1]s init, or run yard -Y %[1]s profile disable %[2]s then init", yard, definition.Name),
	}, nil
}

// profileRuntimePackageDigest binds a record to the effective handler: the
// declared wrapper and every resource of its profile package that it may
// execute, so a release changing any of them invalidates older records.
func profileRuntimePackageDigest(definition profile.Definition) (string, error) {
	handler, err := definition.ExecutablePath(definition.Runtime.Handler)
	if err != nil {
		return "", err
	}
	root := strings.TrimSuffix(handler, string(filepath.Separator)+filepath.Clean(definition.Runtime.Handler))
	hash := sha256.New()
	entries := 0
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entries++; entries > 4096 {
			return errors.New("profile package exceeds its digest bound")
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		switch {
		case entry.IsDir():
			if path != root && entry.Name() == "tests" {
				return filepath.SkipDir
			}
			fmt.Fprintf(hash, "d %q\n", relative)
		case entry.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			fmt.Fprintf(hash, "l %q %q\n", relative, target)
		case entry.Type().IsRegular():
			info, err := entry.Info()
			if err != nil {
				return err
			}
			payload, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			fmt.Fprintf(hash, "f %q %o %x\n", relative, info.Mode().Perm(), sha256.Sum256(payload))
		default:
			return fmt.Errorf("profile package entry %q has an unsupported type", relative)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// recordedPending returns the pending finding when a record still matches the
// observed facts of a declared owner. Undeclared owners never have one.
func (reconciler *profileRuntimeActivationReconciler) recordedPending(target configTarget, state ports.RuntimeObservation) (*runtimePendingConfiguration, error) {
	if _, ok := reconciler.declaredYardScope(); !ok || state.State != ports.RuntimeStateStale {
		return nil, nil
	}
	facts, err := reconciler.pendingFacts(target, state)
	if err != nil {
		return nil, err
	}
	record, found, err := pendingActivation(reconciler.request.ConfigHome, facts)
	if err != nil || !found {
		return nil, err
	}
	return &runtimePendingConfiguration{record}, nil
}

// recordPending binds a typed failure to the state it left.
func (reconciler *profileRuntimeActivationReconciler) recordPending(target configTarget, observe func() (ports.RuntimeObservation, error), failure *reconcileruntime.ProfileRuntimeYardFailure) error {
	state, err := observe()
	if err != nil {
		return err
	}
	if state.State != ports.RuntimeStateStale {
		return fmt.Errorf("profile runtime %s is %s after its yard-scoped failure", failure.ActivationID, state.State)
	}
	record, err := reconciler.pendingFacts(target, state)
	if err != nil {
		return err
	}
	return writeActivationPendingRecord(reconciler.request.ConfigHome, record)
}

// retirePending removes this owner's record for a yard whose runtime converged
// or is about to be applied again.
func (reconciler *profileRuntimeActivationReconciler) retirePending(target configTarget) error {
	if _, ok := reconciler.declaredYardScope(); !ok {
		return nil
	}
	return removeActivationPendingRecord(reconciler.request.ConfigHome, target.Name, reconciler.id)
}

// yardScopedRuntimeIDs are the runtime owners that may hold pending records.
func (cli *CLI) yardScopedRuntimeIDs() []string {
	var ids []string
	for _, definition := range cli.catalog.Profiles() {
		if definition.Runtime != nil && definition.Runtime.FailureScope == profile.FailureScopeYard {
			ids = append(ids, definition.Runtime.ActivationID)
		}
	}
	sort.Strings(ids)
	return ids
}

// declaredYardScope is the profile whose runtime this owner applies when it
// declares the yard failure scope. Core runtimes never declare one.
func (reconciler *profileRuntimeActivationReconciler) declaredYardScope() (profile.Definition, bool) {
	if coreInstalledRuntimeID(reconciler.id) {
		return profile.Definition{}, false
	}
	for _, definition := range reconciler.cli.catalog.Profiles() {
		if definition.Runtime != nil && definition.Runtime.ActivationID == reconciler.id {
			return definition, definition.Runtime.FailureScope == profile.FailureScopeYard
		}
	}
	return profile.Definition{}, false
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
		state, err := reconciler.observeTarget(ctx, operation, target)
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

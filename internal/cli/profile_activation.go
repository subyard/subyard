package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/Subyard/Subyard/internal/domain"
	"github.com/Subyard/Subyard/internal/ports"
	"github.com/Subyard/Subyard/internal/releasetransition"
)

// Refresh only installed profile files; the profile adapter owns their contract.
// The transition owns confirmation, recovery, and verification across local yards.
type profileRuntimeActivationReconciler struct {
	cli      *CLI
	request  releasetransition.ProcessRequest
	id       string
	recovery map[string]ports.RuntimeObservation
}

func (cli *CLI) profileActivationReconciler(request releasetransition.ProcessRequest, id string) releasetransition.V2ActivationReconciler {
	return &profileRuntimeActivationReconciler{cli: cli, request: request, id: id}
}

func (reconciler *profileRuntimeActivationReconciler) ID() string { return reconciler.id }

func (reconciler *profileRuntimeActivationReconciler) failure(yard, phase string, cause error) error {
	owner := "profile " + reconciler.id + " runtime"
	if coreInstalledRuntimeID(reconciler.id) {
		owner = reconciler.id
	}
	return materializedConfigActivationError{cause, yard, owner, "status", phase, reconciler.request.ConfigHome}
}

func (reconciler *profileRuntimeActivationReconciler) targets() (*CLI, []configTarget, error) {
	operation := reconciler.cli.releaseActivationOperation()
	// Host-wide inventory must not require the caller's yard to exist.
	loaded, err := operation.resolveReleaseTransitionContext("default", reconciler.request.ConfigHome)
	if err != nil {
		return nil, nil, reconciler.failure("default", "inspect", err)
	}
	targets, err := operation.localConfigTargets(loaded, true)
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
	operation, targets, err := reconciler.targets()
	if err != nil {
		return releasetransition.V2ActivationObservation{}, reconciler.failure("default", "inspect", err)
	}
	type observation struct {
		Yard, Project, Instance, State, Digest, Desired string
	}
	actual := make([]observation, 0, len(targets))
	converged := true
	for _, target := range targets {
		state, err := observeProfileRuntime(ctx, profileActivationPlatform(operation, target), reconciler.id)
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
	}
	actualDigest := desiredDigest
	if !converged {
		actualDigest, err = activationStageFingerprint(actual)
	}
	return releasetransition.V2ActivationObservation{Actual: actualDigest, Desired: desiredDigest, Converged: converged}, err
}

func (reconciler *profileRuntimeActivationReconciler) Reconcile(ctx context.Context, _ releasetransition.ReleaseLinks) error {
	complete, err := reconciler.sourceMigrationsComplete()
	if err != nil {
		return err
	}
	if !complete {
		return fmt.Errorf("profile runtime refresh requires completed source migrations")
	}
	operation, targets, err := reconciler.targets()
	if err != nil {
		return err
	}
	if reconciler.recovery != nil && len(targets) != len(reconciler.recovery) {
		return domain.ErrPlanStale
	}
	for _, target := range targets {
		platform := profileActivationPlatform(operation, target)
		state, err := observeProfileRuntime(ctx, platform, reconciler.id)
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
			continue
		}
		apply := platform.ApplyProfileRuntime
		if coreInstalledRuntimeID(reconciler.id) {
			apply = platform.ApplyInstalledRuntime
		}
		if err := apply(ctx, reconciler.id, operation.ensureOperationID(), state.Actual, state.Desired); err != nil {
			return reconciler.failure(target.Name, "apply", err)
		}
		state, err = observeProfileRuntime(ctx, platform, reconciler.id)
		if err != nil {
			return reconciler.failure(target.Name, "verify", err)
		}
		if state.State != "current" {
			return reconciler.failure(target.Name, "verify", fmt.Errorf("profile runtime did not converge"))
		}
	}
	return nil
}

func observeProfileRuntime(ctx context.Context, platform ports.InitPlatform, id string) (ports.RuntimeObservation, error) {
	if coreInstalledRuntimeID(id) {
		return platform.ObserveInstalledRuntime(ctx, id)
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
	operation, targets, err := reconciler.targets()
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
